package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const (
	downloadTimeout       = 10 * time.Minute
	maxDownloadAttempts   = 3
	initialRetryDelay     = time.Second
	maxRetryResponseBytes = 4 << 10
)

type Client struct {
	allowInsecure bool
	httpClient    *http.Client
	baseURL       string
	retryDelay    func(int) time.Duration
}

func NewClient(baseURL string, allowInsecure bool) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("release base URL is invalid")
	}
	client := &Client{
		allowInsecure: allowInsecure,
		baseURL:       strings.TrimSuffix(baseURL, "/"),
		retryDelay:    downloadRetryDelay,
	}
	if err := client.validateRedirectURL(parsed); err != nil {
		return nil, err
	}
	client.httpClient = &http.Client{
		Timeout: downloadTimeout,
		CheckRedirect: func(request *http.Request, _ []*http.Request) error {
			return client.validateRedirectURL(request.URL)
		},
	}
	return client, nil
}

func (c *Client) URL(repository, version, asset string) (string, error) {
	if c == nil || !validRepository(repository) || !validToken(asset) {
		return "", errors.New("release source is invalid")
	}
	if version == "" {
		return fmt.Sprintf("%s/%s/releases/latest/download/%s", c.baseURL, repository, asset), nil
	}
	if !validToken(version) {
		return "", errors.New("release version is invalid")
	}
	return fmt.Sprintf("%s/%s/releases/download/%s/%s", c.baseURL, repository, version, asset), nil
}

func (c *Client) Download(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	var output bytes.Buffer
	_, err := c.downloadWithRetry(ctx, rawURL, limit, func() io.Writer {
		output.Reset()
		return &output
	}, true)
	if err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (c *Client) DownloadTo(ctx context.Context, rawURL string, limit int64, destination io.Writer) (int64, error) {
	return c.downloadWithRetry(ctx, rawURL, limit, func() io.Writer { return destination }, false)
}

func (c *Client) downloadWithRetry(ctx context.Context, rawURL string, limit int64, destination func() io.Writer, retryAfterWrite bool) (int64, error) {
	if c == nil || limit <= 0 || destination == nil {
		return 0, errors.New("release download is not configured")
	}
	u, err := url.Parse(rawURL)
	if err != nil || c.validateInitialURL(u) != nil {
		return 0, errors.New("release URL must be absolute HTTPS without credentials, query, or fragment")
	}
	downloadCtx, cancel := context.WithTimeout(ctx, downloadTimeout)
	defer cancel()
	for attempt := 0; attempt < maxDownloadAttempts; attempt++ {
		written, retryable, err := c.downloadAttempt(downloadCtx, u.String(), limit, destination())
		if err == nil {
			return written, nil
		}
		if attempt+1 == maxDownloadAttempts || !retryable || (written > 0 && !retryAfterWrite) {
			return written, err
		}
		if err := c.waitBeforeRetry(downloadCtx, attempt); err != nil {
			return written, err
		}
	}
	return 0, errors.New("release download attempts exhausted")
}

func (c *Client) downloadAttempt(ctx context.Context, rawURL string, limit int64, destination io.Writer) (int64, bool, error) {
	if destination == nil {
		return 0, false, errors.New("release download is not configured")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return 0, false, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, retryableRequestError(err), err
	}
	if response.StatusCode != http.StatusOK {
		retryable := retryableStatus(response.StatusCode)
		if retryable {
			_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, maxRetryResponseBytes))
		}
		_ = response.Body.Close()
		return 0, retryable, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	written, err := io.Copy(destination, io.LimitReader(response.Body, limit+1))
	_ = response.Body.Close()
	if err != nil {
		return written, retryableRequestError(err), err
	}
	if written > limit {
		return written, false, errors.New("release download exceeds the size limit")
	}
	return written, false, nil
}

func retryableRequestError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var requestError *url.Error
	if errors.As(err, &requestError) {
		err = requestError.Err
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	var networkError net.Error
	return errors.As(err, &networkError)
}

func retryableStatus(status int) bool {
	return status == http.StatusRequestTimeout || status == http.StatusTooManyRequests || (status >= 500 && status <= 599)
}

func downloadRetryDelay(attempt int) time.Duration {
	return initialRetryDelay << attempt
}

func (c *Client) waitBeforeRetry(ctx context.Context, attempt int) error {
	delay := downloadRetryDelay(attempt)
	if c.retryDelay != nil {
		delay = c.retryDelay(attempt)
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (c *Client) validateInitialURL(u *url.URL) error {
	if err := c.validateRedirectURL(u); err != nil {
		return err
	}
	if u.RawQuery != "" {
		return errors.New("release URL must not contain a query")
	}
	return nil
}

func (c *Client) validateRedirectURL(u *url.URL) error {
	if u == nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("invalid release URL")
	}
	if u.Scheme == "https" || (c.allowInsecure && u.Scheme == "http") {
		return nil
	}
	return errors.New("release URL must use HTTPS")
}

func validRepository(value string) bool {
	parts := strings.Split(value, "/")
	return len(parts) == 2 && validToken(parts[0]) && validToken(parts[1])
}

func validToken(value string) bool {
	if value == "" || value == "." || value == ".." || len(value) > 128 {
		return false
	}
	for _, character := range value {
		if (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') || character == '.' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}
