package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const downloadTimeout = 10 * time.Minute

type Client struct {
	allowInsecure bool
	httpClient    *http.Client
	baseURL       string
}

func NewClient(baseURL string, allowInsecure bool) (*Client, error) {
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return nil, errors.New("release base URL is invalid")
	}
	client := &Client{allowInsecure: allowInsecure, baseURL: strings.TrimSuffix(baseURL, "/")}
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
	if _, err := c.DownloadTo(ctx, rawURL, limit, &output); err != nil {
		return nil, err
	}
	return output.Bytes(), nil
}

func (c *Client) DownloadTo(ctx context.Context, rawURL string, limit int64, destination io.Writer) (int64, error) {
	if c == nil || limit <= 0 || destination == nil {
		return 0, errors.New("release download is not configured")
	}
	u, err := url.Parse(rawURL)
	if err != nil || c.validateInitialURL(u) != nil {
		return 0, errors.New("release URL must be absolute HTTPS without credentials, query, or fragment")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return 0, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	written, err := io.Copy(destination, io.LimitReader(response.Body, limit+1))
	if err != nil {
		return written, err
	}
	if written > limit {
		return written, errors.New("release download exceeds the size limit")
	}
	return written, nil
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
