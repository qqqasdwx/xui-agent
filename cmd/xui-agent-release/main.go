package main

import (
	"crypto/sha256"
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

func main() {
	dist := flag.String("dist", "dist", "release asset directory")
	version := flag.String("version", "", "release version")
	publishedAt := flag.String("published-at", "", "RFC3339 release timestamp")
	flag.Parse()
	if err := createManifest(*dist, *version, *publishedAt); err != nil {
		fmt.Fprintln(os.Stderr, "xui-agent-release:", err)
		os.Exit(1)
	}
}

func createManifest(dist, version, publishedAt string) error {
	if version == "" || strings.ContainsAny(version, "/\\") {
		return errors.New("version is required and must not contain path separators")
	}
	if publishedAt == "" {
		publishedAt = time.Now().UTC().Format(time.RFC3339)
	} else if _, err := time.Parse(time.RFC3339, publishedAt); err != nil {
		return errors.New("published-at must be RFC3339")
	}
	manifest := updatepkg.Manifest{SchemaVersion: updatepkg.ManifestSchemaVersion, Version: version, PublishedAt: publishedAt}
	for _, arch := range []string{"amd64"} {
		name := "xui-agent-linux-" + arch + ".tar.gz"
		path := filepath.Join(dist, name)
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		digest := sha256.Sum256(raw)
		runtimeAssetsDigest, err := updatepkg.RuntimeAssetsDigest(raw)
		if err != nil {
			return fmt.Errorf("inspect runtime assets in %s: %w", path, err)
		}
		if manifest.RuntimeAssetsSHA256 == "" {
			manifest.RuntimeAssetsSHA256 = runtimeAssetsDigest
		} else if manifest.RuntimeAssetsSHA256 != runtimeAssetsDigest {
			return errors.New("release archives contain different runtime assets")
		}
		manifest.Artifacts = append(manifest.Artifacts, updatepkg.Artifact{
			OS:     "linux",
			Arch:   arch,
			SHA256: hex.EncodeToString(digest[:]),
			Size:   int64(len(raw)),
		})
	}
	manifestRaw, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return err
	}
	manifestRaw = append(manifestRaw, '\n')
	if err := os.WriteFile(filepath.Join(dist, "manifest.json"), manifestRaw, 0o644); err != nil {
		return err
	}
	return nil
}
