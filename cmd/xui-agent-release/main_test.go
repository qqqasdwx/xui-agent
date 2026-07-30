package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	updatepkg "github.com/qqqasdwx/xui-agent/internal/update"
)

func TestCreateManifestWritesSignedFiles(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dist := t.TempDir()
	for _, arch := range []string{"amd64", "arm64", "armv7"} {
		if err := os.WriteFile(filepath.Join(dist, "xui-agent-linux-"+arch+".tar.gz"), releaseArchive(t, arch), 0o644); err != nil {
			t.Fatalf("write archive: %v", err)
		}
	}
	if err := createManifest(dist, "v1.2.3", "owner/repository", "2026-07-29T00:00:00Z", base64.StdEncoding.EncodeToString(privateKey)); err != nil {
		t.Fatalf("createManifest: %v", err)
	}
	for _, name := range []string{"manifest.json", "manifest.sig"} {
		if info, err := os.Stat(filepath.Join(dist, name)); err != nil || info.Size() == 0 {
			t.Fatalf("%s was not written: info=%v err=%v", name, info, err)
		}
	}
	manifest, err := os.ReadFile(filepath.Join(dist, "manifest.json"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	var decoded updatepkg.Manifest
	if err := json.Unmarshal(manifest, &decoded); err != nil {
		t.Fatalf("decode manifest: %v", err)
	}
	if decoded.SchemaVersion != updatepkg.ManifestSchemaVersion || len(decoded.RuntimeAssetsSHA256) != 64 {
		t.Fatalf("runtime assets metadata is incomplete: %+v", decoded)
	}
	for _, arch := range []string{"amd64", "arm64", "armv7"} {
		archive, err := os.ReadFile(filepath.Join(dist, "xui-agent-linux-"+arch+".tar.gz"))
		if err != nil {
			t.Fatalf("read %s archive: %v", arch, err)
		}
		digest, err := updatepkg.RuntimeAssetsDigest(archive)
		if err != nil || digest != decoded.RuntimeAssetsSHA256 {
			t.Fatalf("%s runtime assets digest = %q, err=%v", arch, digest, err)
		}
	}
	encodedSignature, err := os.ReadFile(filepath.Join(dist, "manifest.sig"))
	if err != nil {
		t.Fatalf("read signature: %v", err)
	}
	signature, err := base64.StdEncoding.DecodeString(string(encodedSignature))
	if err != nil || !ed25519.Verify(privateKey.Public().(ed25519.PublicKey), manifest, signature) {
		t.Fatal("release manifest signature did not verify")
	}
}

func releaseArchive(t *testing.T, arch string) []byte {
	t.Helper()
	entries := map[string][]byte{
		"xui-agent":              []byte("binary-" + arch),
		"uninstall.sh":           []byte("uninstall\n"),
		"xui-agent-launcher":     []byte("launcher\n"),
		"xui-agent-xray.path":    []byte("path\n"),
		"xui-agent-xray.service": []byte("xray service\n"),
		"xui-agent.service":      []byte("agent service\n"),
	}
	order := []string{"xui-agent", "uninstall.sh", "xui-agent-launcher", "xui-agent-xray.path", "xui-agent-xray.service", "xui-agent.service"}
	var output bytes.Buffer
	gz := gzip.NewWriter(&output)
	tw := tar.NewWriter(gz)
	for _, name := range order {
		content := entries[name]
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatalf("write %s header: %v", name, err)
		}
		if _, err := tw.Write(content); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatalf("close tar: %v", err)
	}
	if err := gz.Close(); err != nil {
		t.Fatalf("close gzip: %v", err)
	}
	return output.Bytes()
}
