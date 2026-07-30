package signedrelease

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const maxSignatureBytes = 4 << 10

type Client struct {
	publicKey     ed25519.PublicKey
	allowInsecure bool
	httpClient    *http.Client
}

func NewClient(encodedPublicKey string, allowInsecure bool) (*Client, error) {
	client := &Client{allowInsecure: allowInsecure}
	client.httpClient = &http.Client{
		Timeout: 2 * time.Minute,
		CheckRedirect: func(request *http.Request, _ []*http.Request) error {
			return client.validateRedirectURL(request.URL)
		},
	}
	if encodedPublicKey == "" {
		return client, nil
	}
	raw, err := base64.StdEncoding.DecodeString(encodedPublicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("release public key is invalid")
	}
	client.publicKey = ed25519.PublicKey(raw)
	return client, nil
}

func (c *Client) Enabled() bool {
	return c != nil && len(c.publicKey) == ed25519.PublicKeySize
}

func (c *Client) FetchSigned(ctx context.Context, manifestURL, signatureURL string, maxManifestBytes int64) ([]byte, error) {
	if !c.Enabled() {
		return nil, errors.New("signed releases are not configured")
	}
	manifest, err := c.Download(ctx, manifestURL, maxManifestBytes)
	if err != nil {
		return nil, fmt.Errorf("download release manifest: %w", err)
	}
	encodedSignature, err := c.Download(ctx, signatureURL, maxSignatureBytes)
	if err != nil {
		return nil, fmt.Errorf("download release signature: %w", err)
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(encodedSignature)))
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(c.publicKey, manifest, signature) {
		return nil, errors.New("release manifest signature verification failed")
	}
	return manifest, nil
}

func (c *Client) Download(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	if c == nil || limit <= 0 {
		return nil, errors.New("release download is not configured")
	}
	u, err := url.Parse(rawURL)
	if err != nil || c.validateInitialURL(u) != nil {
		return nil, errors.New("release URL must be absolute HTTPS without credentials, query, or fragment")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := c.httpClient.Do(request)
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", response.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(response.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, errors.New("release download exceeds the size limit")
	}
	return raw, nil
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
