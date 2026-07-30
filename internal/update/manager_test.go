package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func testArchive(t *testing.T, version string) []byte {
	t.Helper()
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	binary := testBinary(version)
	if err := tw.WriteHeader(&tar.Header{Name: "xui-agent", Mode: 0o755, Size: int64(len(binary)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatalf("write tar header: %v", err)
	}
	if _, err := tw.Write(binary); err != nil {
		t.Fatalf("write tar body: %v", err)
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return output.Bytes()
}

func testBinary(version string) []byte {
	return []byte("#!/bin/sh\necho 'xui-agent " + version + " (test)'\n")
}

func releaseTarget(version string, binary []byte) string {
	digest := sha256.Sum256(binary)
	return filepath.Join("versions", version+"-"+hex.EncodeToString(digest[:]), "xui-agent")
}

func setupManagedState(t *testing.T) string {
	t.Helper()
	state := t.TempDir()
	bootstrap := filepath.Join(state, "versions", "bootstrap")
	if err := os.MkdirAll(bootstrap, 0o700); err != nil {
		t.Fatalf("create bootstrap: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bootstrap, "xui-agent"), []byte("old"), 0o755); err != nil {
		t.Fatalf("write bootstrap: %v", err)
	}
	if err := os.Symlink("versions/bootstrap/xui-agent", filepath.Join(state, "current")); err != nil {
		t.Fatalf("create current symlink: %v", err)
	}
	return state
}

func TestManagerAppliesSignedArchiveAndConfirmsAfterHealth(t *testing.T) {
	publicKey, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	archive := testArchive(t, "v1.2.3")
	digest := sha256.Sum256(archive)

	assets := map[string][]byte{"/archive": archive}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		value, ok := assets[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(value)
	}))
	defer server.Close()

	manifest := Manifest{
		SchemaVersion: 1,
		Version:       "v1.2.3",
		PublishedAt:   "2026-07-29T00:00:00Z",
		Artifacts: []Artifact{{
			OS: "linux", Arch: runtimeArch(), URL: server.URL + "/archive",
			SHA256: hex.EncodeToString(digest[:]), Size: int64(len(archive)),
		}},
	}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	assets["/manifest"] = manifestRaw
	assets["/signature"] = []byte(base64.StdEncoding.EncodeToString(ed25519.Sign(privateKey, manifestRaw)))

	state := setupManagedState(t)
	manager, err := NewManager(state, base64.StdEncoding.EncodeToString(publicKey), true)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	version, err := manager.Apply(context.Background(), Request{
		CommandID: "command-1", Version: "v1.2.3",
		ManifestURL: server.URL + "/manifest", SignatureURL: server.URL + "/signature",
	})
	if err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if version != "v1.2.3" {
		t.Fatalf("version = %q", version)
	}
	current, err := os.Readlink(filepath.Join(state, "current"))
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if current != releaseTarget("v1.2.3", testBinary("v1.2.3")) {
		t.Fatalf("current target = %q", current)
	}
	pending, err := manager.Pending()
	if err != nil {
		t.Fatalf("Pending: %v", err)
	}
	if pending.PreviousTarget != "versions/bootstrap/xui-agent" || pending.TargetVersion != "v1.2.3" {
		t.Fatalf("unexpected pending state: %+v", pending)
	}
	if err := manager.Confirm("v1.2.3"); err != nil {
		t.Fatalf("Confirm: %v", err)
	}
	if _, err := manager.Pending(); !os.IsNotExist(err) {
		t.Fatalf("pending update still exists after confirmation: %v", err)
	}
}

func TestManagerPreservesRollbackTargetWhenVersionContentChanges(t *testing.T) {
	state := setupManagedState(t)
	manager := &Manager{stateDirectory: state}

	first := testBinary("v1.2.3")
	if _, err := manager.install(context.Background(), Request{CommandID: "first", Version: "v1.2.3"}, first); err != nil {
		t.Fatalf("install first build: %v", err)
	}
	if err := manager.Confirm("v1.2.3"); err != nil {
		t.Fatalf("confirm first build: %v", err)
	}
	firstTarget, err := os.Readlink(filepath.Join(state, "current"))
	if err != nil {
		t.Fatalf("read first target: %v", err)
	}

	second := []byte("#!/bin/sh\necho 'xui-agent v1.2.3 (rebuilt)'\n")
	if _, err := manager.install(context.Background(), Request{CommandID: "second", Version: "v1.2.3"}, second); err != nil {
		t.Fatalf("install rebuilt version: %v", err)
	}
	secondTarget, err := os.Readlink(filepath.Join(state, "current"))
	if err != nil {
		t.Fatalf("read second target: %v", err)
	}
	if firstTarget == secondTarget {
		t.Fatalf("rebuilt version reused the active target %q", firstTarget)
	}
	previous, err := os.Readlink(filepath.Join(state, "previous"))
	if err != nil {
		t.Fatalf("read rollback target: %v", err)
	}
	if previous != firstTarget {
		t.Fatalf("rollback target = %q, want %q", previous, firstTarget)
	}
	firstOnDisk, err := os.ReadFile(filepath.Join(state, firstTarget))
	if err != nil {
		t.Fatalf("read preserved first binary: %v", err)
	}
	if !bytes.Equal(firstOnDisk, first) {
		t.Fatal("rebuilt version overwrote the rollback binary")
	}
}

func TestManagerRejectsInvalidSignatureWithoutSwitching(t *testing.T) {
	publicKey, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	assets := map[string][]byte{
		"/manifest":  []byte(`{"schemaVersion":1,"version":"v1","publishedAt":"now","artifacts":[]}`),
		"/signature": []byte(base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))),
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(assets[r.URL.Path]) }))
	defer server.Close()
	state := setupManagedState(t)
	manager, err := NewManager(state, base64.StdEncoding.EncodeToString(publicKey), true)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	_, err = manager.Apply(context.Background(), Request{
		CommandID: "command-bad", Version: "v1",
		ManifestURL: server.URL + "/manifest", SignatureURL: server.URL + "/signature",
	})
	if err == nil {
		t.Fatal("invalid manifest signature was accepted")
	}
	current, readErr := os.Readlink(filepath.Join(state, "current"))
	if readErr != nil || current != "versions/bootstrap/xui-agent" {
		t.Fatalf("current changed after rejected update: target=%q err=%v", current, readErr)
	}
}

func TestValidateVersionRejectsPathAliases(t *testing.T) {
	for _, version := range []string{"", ".", "..", "v1/next", `v1\next`} {
		if err := validateVersion(version); err == nil {
			t.Fatalf("validateVersion(%q) succeeded", version)
		}
	}
	for _, version := range []string{"v1.2.3", "release_candidate-1"} {
		if err := validateVersion(version); err != nil {
			t.Fatalf("validateVersion(%q): %v", version, err)
		}
	}
}

func runtimeArch() string {
	if runtime.GOARCH == "arm" {
		return "armv7"
	}
	return runtime.GOARCH
}
