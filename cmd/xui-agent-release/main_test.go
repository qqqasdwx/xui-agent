package main

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateManifestWritesSignedFiles(t *testing.T) {
	_, privateKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	dist := t.TempDir()
	for _, arch := range []string{"amd64", "arm64", "armv7"} {
		if err := os.WriteFile(filepath.Join(dist, "xui-agent-linux-"+arch+".tar.gz"), []byte("archive-"+arch), 0o644); err != nil {
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
	encodedSignature, err := os.ReadFile(filepath.Join(dist, "manifest.sig"))
	if err != nil {
		t.Fatalf("read signature: %v", err)
	}
	signature, err := base64.StdEncoding.DecodeString(string(encodedSignature))
	if err != nil || !ed25519.Verify(privateKey.Public().(ed25519.PublicKey), manifest, signature) {
		t.Fatal("release manifest signature did not verify")
	}
}
