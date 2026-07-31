package xrayupdate

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"runtime"
	"strings"
)

const (
	ManifestSchemaVersion = 2
	maxManifestBytes      = 1 << 20
	maxArchiveBytes       = 128 << 20
	maxExpandedBytes      = 128 << 20
	maxBinaryBytes        = 64 << 20
	maxAssetBytes         = 32 << 20
)

var requiredBundleFiles = []string{"xray", "geoip.dat", "geosite.dat"}

type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

func releaseAssetName(arch string) string {
	switch arch {
	case "amd64":
		return "Xray-linux-64.zip"
	case "arm64":
		return "Xray-linux-arm64-v8a.zip"
	case "armv7":
		return "Xray-linux-arm32-v7a.zip"
	default:
		return ""
	}
}

type Manifest struct {
	SchemaVersion int        `json:"schemaVersion"`
	Version       string     `json:"version"`
	XrayVersion   string     `json:"xrayVersion"`
	PublishedAt   string     `json:"publishedAt"`
	Artifacts     []Artifact `json:"artifacts"`
}

type bundle struct {
	files map[string][]byte
}

func decodeManifest(raw []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode Xray release manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("Xray release manifest contains trailing data")
	}
	if manifest.SchemaVersion != ManifestSchemaVersion || validateVersion(manifest.Version) != nil || validateVersion(manifest.XrayVersion) != nil || len(manifest.Artifacts) == 0 {
		return Manifest{}, errors.New("Xray release manifest is incomplete or unsupported")
	}
	return manifest, nil
}

func selectArtifact(manifest Manifest) (Artifact, error) {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	var selected *Artifact
	for i := range manifest.Artifacts {
		artifact := manifest.Artifacts[i]
		if artifact.OS != runtime.GOOS || artifact.Arch != arch {
			continue
		}
		if selected != nil {
			return Artifact{}, errors.New("Xray release manifest has duplicate artifacts for this platform")
		}
		artifact.SHA256 = strings.ToLower(strings.TrimSpace(artifact.SHA256))
		if artifact.Size <= 0 || artifact.Size > maxArchiveBytes || len(artifact.SHA256) != sha256.Size*2 {
			return Artifact{}, errors.New("Xray release artifact metadata is invalid")
		}
		if _, err := hex.DecodeString(artifact.SHA256); err != nil {
			return Artifact{}, errors.New("Xray release artifact digest is invalid")
		}
		selected = &artifact
	}
	if selected == nil {
		return Artifact{}, fmt.Errorf("Xray release has no artifact for %s/%s", runtime.GOOS, arch)
	}
	return *selected, nil
}

func extractBundle(archive []byte) (bundle, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return bundle{}, fmt.Errorf("open Xray release archive: %w", err)
	}
	entries, err := validateBundleArchive(reader)
	if err != nil {
		return bundle{}, err
	}
	files := make(map[string][]byte, len(requiredBundleFiles))
	for _, entry := range entries {
		limit := bundleEntryLimit(entry.Name)
		stream, err := entry.Open()
		if err != nil {
			return bundle{}, err
		}
		raw, readErr := io.ReadAll(io.LimitReader(stream, limit+1))
		closeErr := stream.Close()
		if readErr != nil {
			return bundle{}, readErr
		}
		if closeErr != nil {
			return bundle{}, closeErr
		}
		if int64(len(raw)) != int64(entry.UncompressedSize64) || int64(len(raw)) > limit {
			return bundle{}, fmt.Errorf("Xray release entry %q exceeds its size limit", entry.Name)
		}
		if isRequiredBundleFile(entry.Name) {
			files[entry.Name] = raw
		}
	}
	return bundle{files: files}, nil
}

func validateBundleArchive(reader *zip.Reader) ([]*zip.File, error) {
	if reader == nil {
		return nil, errors.New("Xray release archive is invalid")
	}
	allowed := map[string]int64{
		"xray": maxBinaryBytes, "geoip.dat": maxAssetBytes, "geosite.dat": maxAssetBytes,
		"LICENSE": 1 << 20, "README.md": 1 << 20,
	}
	var expanded int64
	seen := make(map[string]bool, len(reader.File))
	for _, entry := range reader.File {
		name := entry.Name
		limit, ok := allowed[name]
		if !ok || seen[name] || entry.FileInfo().IsDir() || !entry.Mode().IsRegular() {
			return nil, fmt.Errorf("Xray release archive contains invalid entry %q", name)
		}
		seen[name] = true
		if int64(entry.UncompressedSize64) <= 0 || int64(entry.UncompressedSize64) > limit {
			return nil, fmt.Errorf("Xray release entry %q has an invalid size", name)
		}
		expanded += int64(entry.UncompressedSize64)
		if expanded > maxExpandedBytes {
			return nil, errors.New("Xray release archive exceeds the expanded size limit")
		}
	}
	for _, name := range requiredBundleFiles {
		if !seen[name] {
			return nil, fmt.Errorf("Xray release archive is missing %s", name)
		}
	}
	return reader.File, nil
}

func bundleEntryLimit(name string) int64 {
	switch name {
	case "xray":
		return maxBinaryBytes
	case "geoip.dat", "geosite.dat":
		return maxAssetBytes
	case "LICENSE", "README.md":
		return 1 << 20
	default:
		return 0
	}
}

func isRequiredBundleFile(name string) bool {
	return name == "xray" || name == "geoip.dat" || name == "geosite.dat"
}

func validateVersion(version string) error {
	if version == "" || version == "." || version == ".." || len(version) > 128 {
		return errors.New("Xray version is invalid")
	}
	for _, character := range version {
		if (character < 'a' || character > 'z') && (character < 'A' || character > 'Z') &&
			(character < '0' || character > '9') && character != '.' && character != '_' && character != '-' {
			return errors.New("Xray version is invalid")
		}
	}
	return nil
}
