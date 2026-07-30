package signedrelease

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

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

	client, err := NewClient("", true)
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

func TestDownloadRejectsQueryInInitialURL(t *testing.T) {
	client, err := NewClient("", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Download(context.Background(), "http://example.test/release?token=center", 64); err == nil {
		t.Fatal("initial release URL with query was accepted")
	}
}

func TestDownloadRejectsRedirectCredentials(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		response.Header().Set("Location", fmt.Sprintf("http://user:password@%s/asset", request.Host))
		response.WriteHeader(http.StatusFound)
	}))
	defer server.Close()

	client, err := NewClient("", true)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, err := client.Download(context.Background(), server.URL, 64); err == nil {
		t.Fatal("release redirect with credentials was accepted")
	}
}
