package main

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	updatepkg "github.com/qqqasdwx/xui-agent/internal/update"
)

const privateKeyEnvironment = "XUI_AGENT_RELEASE_PRIVATE_KEY"

func main() {
	dist := flag.String("dist", "dist", "release asset directory")
	version := flag.String("version", "", "release version")
	repository := flag.String("repository", "qqqasdwx/xui-agent", "GitHub owner/repository")
	publishedAt := flag.String("published-at", "", "RFC3339 release timestamp")
	flag.Parse()
	if err := createManifest(*dist, *version, *repository, *publishedAt, os.Getenv(privateKeyEnvironment)); err != nil {
		fmt.Fprintln(os.Stderr, "xui-agent-release:", err)
		os.Exit(1)
	}
}

func createManifest(dist, version, repository, publishedAt, encodedPrivateKey string) error {
	if version == "" || strings.ContainsAny(version, "/\\") {
		return errors.New("version is required and must not contain path separators")
	}
	if repository == "" || strings.Count(repository, "/") != 1 {
		return errors.New("repository must use owner/name form")
	}
	if publishedAt == "" {
		publishedAt = time.Now().UTC().Format(time.RFC3339)
	} else if _, err := time.Parse(time.RFC3339, publishedAt); err != nil {
		return errors.New("published-at must be RFC3339")
	}
	privateKey, err := decodePrivateKey(encodedPrivateKey)
	if err != nil {
		return err
	}

	manifest := updatepkg.Manifest{SchemaVersion: 1, Version: version, PublishedAt: publishedAt}
	for _, arch := range []string{"amd64", "arm64", "armv7"} {
		name := "xui-agent-linux-" + arch + ".tar.gz"
		path := filepath.Join(dist, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		digest := sha256.Sum256(raw)
		manifest.Artifacts = append(manifest.Artifacts, updatepkg.Artifact{
			OS:     "linux",
			Arch:   arch,
			URL:    fmt.Sprintf("https://github.com/%s/releases/download/%s/%s", repository, version, name),
			SHA256: hex.EncodeToString(digest[:]),
			Size:   int64(len(raw)),
		})
	}
	manifestRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestRaw = append(manifestRaw, '\n')
	signature := ed25519.Sign(privateKey, manifestRaw)
	if err := os.WriteFile(filepath.Join(dist, "manifest.json"), manifestRaw, 0o644); err != nil {
		return err
	}
	encodedSignature := base64.StdEncoding.EncodeToString(signature) + "\n"
	if err := os.WriteFile(filepath.Join(dist, "manifest.sig"), []byte(encodedSignature), 0o644); err != nil {
		return err
	}
	publicKey := privateKey.Public().(ed25519.PublicKey)
	fmt.Println("release public key:", base64.StdEncoding.EncodeToString(publicKey))
	return nil
}

func decodePrivateKey(encoded string) (ed25519.PrivateKey, error) {
	raw, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil {
		return nil, fmt.Errorf("decode %s: %w", privateKeyEnvironment, err)
	}
	switch len(raw) {
	case ed25519.SeedSize:
		return ed25519.NewKeyFromSeed(raw), nil
	case ed25519.PrivateKeySize:
		return ed25519.PrivateKey(raw), nil
	default:
		return nil, fmt.Errorf("%s must contain a base64 Ed25519 seed or private key", privateKeyEnvironment)
	}
}
