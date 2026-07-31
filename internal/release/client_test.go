package release

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestURLPinsRepositoryAndReleaseShape(t *testing.T) {
	client, err := NewClient("https://github.com", false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	got, err := client.URL("qqqasdwx/xui-agent", "v1.2.3", "manifest.json")
	if err != nil {
		t.Fatalf("URL: %v", err)
	}
	want := "https://github.com/qqqasdwx/xui-agent/releases/download/v1.2.3/manifest.json"
	if got != want {
		t.Fatalf("URL = %q, want %q", got, want)
	}
	latest, err := client.URL("qqqasdwx/Xray-core", "", "xray-manifest.json")
	if err != nil {
		t.Fatalf("latest URL: %v", err)
	}
	if latest != "https://github.com/qqqasdwx/Xray-core/releases/latest/download/xray-manifest.json" {
		t.Fatalf("latest URL = %q", latest)
	}
}

func TestClientAllowsSlowReleaseDownloadsWithinCommandLifetime(t *testing.T) {
	client, err := NewClient("https://github.com", false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if client.httpClient.Timeout != 10*time.Minute {
		t.Fatalf("HTTP timeout = %s, want 10m", client.httpClient.Timeout)
	}
}

func TestURLRejectsUnstructuredValues(t *testing.T) {
	client, err := NewClient("https://github.com", false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	for _, test := range []struct{ repository, version, asset string }{
		{"other", "v1", "manifest.json"},
		{"owner/repo/extra", "v1", "manifest.json"},
		{"owner/repo", "../v1", "manifest.json"},
		{"owner/repo", "v1", "../manifest.json"},
	} {
		if _, err := client.URL(test.repository, test.version, test.asset); err == nil {
			t.Fatalf("URL(%q, %q, %q) succeeded", test.repository, test.version, test.asset)
		}
	}
}

func TestDownloadAllowsSecureReleaseRedirectQuery(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/release":
			http.Redirect(response, request, "/asset?token=signed", http.StatusFound)
		case "/asset":
			if request.URL.Query().Get("token") != "signed" {
				http.Error(response, "missing token", http.StatusForbidden)
				return
			}
			_, _ = response.Write([]byte("artifact"))
		default:
			http.NotFound(response, request)
		}
	}))
	defer server.Close()

	client, err := NewClient(server.URL, true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	raw, err := client.Download(context.Background(), server.URL+"/release", 64)
	if err != nil {
		t.Fatalf("Download: %v", err)
	}
	if string(raw) != "artifact" {
		t.Fatalf("Download=%q, want artifact", raw)
	}
}

func TestDownloadToStreamsAndEnforcesLimit(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		_, _ = response.Write([]byte("artifact"))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	var output bytes.Buffer
	written, err := client.DownloadTo(context.Background(), server.URL+"/release", 8, &output)
	if err != nil || written != 8 || output.String() != "artifact" {
		t.Fatalf("DownloadTo written=%d output=%q err=%v", written, output.String(), err)
	}
	output.Reset()
	written, err = client.DownloadTo(context.Background(), server.URL+"/release", 7, &output)
	if err == nil || written != 8 {
		t.Fatalf("oversized DownloadTo written=%d err=%v", written, err)
	}
}

func TestDownloadRetriesTransientRequestFailure(t *testing.T) {
	client, err := NewClient("https://github.com", false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.retryDelay = func(int) time.Duration { return 0 }
	attempts := 0
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		if attempts == 1 {
			return nil, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("artifact")),
			Request:    request,
		}, nil
	})

	raw, err := client.Download(context.Background(), "https://github.com/release", 64)
	if err != nil || string(raw) != "artifact" || attempts != 2 {
		t.Fatalf("Download attempts=%d raw=%q err=%v", attempts, raw, err)
	}
}

func TestDownloadRetriesTransientHTTPStatus(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		attempts++
		if attempts == 1 {
			http.Error(response, "temporary", http.StatusServiceUnavailable)
			return
		}
		_, _ = response.Write([]byte("artifact"))
	}))
	defer server.Close()

	client, err := NewClient(server.URL, true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.retryDelay = func(int) time.Duration { return 0 }
	raw, err := client.Download(context.Background(), server.URL+"/release", 64)
	if err != nil || string(raw) != "artifact" || attempts != 2 {
		t.Fatalf("Download attempts=%d raw=%q err=%v", attempts, raw, err)
	}
}

func TestDownloadRetriesPartialInMemoryResponse(t *testing.T) {
	client, err := NewClient("https://github.com", false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.retryDelay = func(int) time.Duration { return 0 }
	attempts := 0
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		body := io.Reader(strings.NewReader("artifact"))
		if attempts == 1 {
			body = io.MultiReader(strings.NewReader("partial"), unexpectedEOFReader{})
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(body),
			Request:    request,
		}, nil
	})

	raw, err := client.Download(context.Background(), "https://github.com/release", 64)
	if err != nil || string(raw) != "artifact" || attempts != 2 {
		t.Fatalf("Download attempts=%d raw=%q err=%v", attempts, raw, err)
	}
}

func TestDownloadDoesNotRetryPermanentHTTPStatus(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, _ *http.Request) {
		attempts++
		http.Error(response, "missing", http.StatusNotFound)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.retryDelay = func(int) time.Duration { return 0 }
	_, err = client.Download(context.Background(), server.URL+"/release", 64)
	if err == nil || attempts != 1 {
		t.Fatalf("Download attempts=%d err=%v", attempts, err)
	}
}

func TestDownloadDoesNotRetryAfterWritingResponseBody(t *testing.T) {
	client, err := NewClient("https://github.com", false)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	client.retryDelay = func(int) time.Duration { return 0 }
	attempts := 0
	client.httpClient.Transport = roundTripFunc(func(request *http.Request) (*http.Response, error) {
		attempts++
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     make(http.Header),
			Body:       io.NopCloser(io.MultiReader(strings.NewReader("partial"), unexpectedEOFReader{})),
			Request:    request,
		}, nil
	})

	var output bytes.Buffer
	written, err := client.DownloadTo(context.Background(), "https://github.com/release", 64, &output)
	if !errors.Is(err, io.ErrUnexpectedEOF) || attempts != 1 || written != int64(len("partial")) || output.String() != "partial" {
		t.Fatalf("DownloadTo attempts=%d written=%d output=%q err=%v", attempts, written, output.String(), err)
	}
}

func TestDownloadRejectsQueryInInitialURL(t *testing.T) {
	client, err := NewClient("http://example.test", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Download(context.Background(), "http://example.test/release?token=center", 64); err == nil {
		t.Fatal("initial release URL with query was accepted")
	}
}

func TestDownloadRejectsRedirectCredentials(t *testing.T) {
	attempts := 0
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		attempts++
		response.Header().Set("Location", fmt.Sprintf("http://user:password@%s/asset", request.Host))
		response.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	client, err := NewClient(server.URL, true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Download(context.Background(), server.URL, 64); err == nil {
		t.Fatal("release redirect with credentials was accepted")
	}
	if attempts != 1 {
		t.Fatalf("unsafe redirect was retried %d times", attempts)
	}
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}

type unexpectedEOFReader struct{}

func (unexpectedEOFReader) Read([]byte) (int, error) {
	return 0, io.ErrUnexpectedEOF
}
