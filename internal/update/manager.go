package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/release"
)

const (
	maxManifestBytes      = 1 << 20
	maxArchiveBytes       = 128 << 20
	maxBinaryBytes        = 64 << 20
	maxRuntimeAssetBytes  = 1 << 20
	retainedVersions      = 3
	pendingFilename       = "update-pending.json"
	failedFilename        = "update-failed.json"
	ManifestSchemaVersion = 3
	releaseBaseURL        = "https://github.com"
	releaseRepository     = "qqqasdwx/xui-agent"
	manifestAssetName     = "manifest.json"
)

var runtimeAssetNames = []string{
	"uninstall.sh",
	"xui-agent-launcher",
	"xui-agent-xray.path",
	"xui-agent-xray.service",
	"xui-agent.service",
}

var ErrRestartRequired = errors.New("agent restart required to activate the update")

type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	SchemaVersion       int        `json:"schemaVersion"`
	Version             string     `json:"version"`
	PublishedAt         string     `json:"publishedAt"`
	RuntimeAssetsSHA256 string     `json:"runtimeAssetsSha256"`
	Artifacts           []Artifact `json:"artifacts"`
}

type Request struct {
	CommandID string
	Version   string
}

type Pending struct {
	CommandID      string `json:"commandId"`
	PreviousTarget string `json:"previousTarget"`
	TargetTarget   string `json:"targetTarget"`
	TargetVersion  string `json:"targetVersion"`
	StartedAt      int64  `json:"startedAt"`
}

type Manager struct {
	stateDirectory      string
	installedAssetsPath string
	releases            *release.Client
}

func NewManager(stateDirectory, installedAssetsPath string) (*Manager, error) {
	releases, err := release.NewClient(releaseBaseURL, false)
	if err != nil {
		return nil, err
	}
	return &Manager{
		stateDirectory:      stateDirectory,
		installedAssetsPath: installedAssetsPath,
		releases:            releases,
	}, nil
}

func (m *Manager) Enabled() bool {
	return m != nil && m.releases != nil
}

func (m *Manager) Pending() (*Pending, error) {
	return m.readState(m.pendingPath(), "pending")
}

func (m *Manager) Failed() (*Pending, error) {
	return m.readState(filepath.Join(m.stateDirectory, failedFilename), "failed")
}

func (m *Manager) readState(path, state string) (*Pending, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var pending Pending
	if err := json.Unmarshal(raw, &pending); err != nil {
		return nil, fmt.Errorf("decode %s update: %w", state, err)
	}
	if pending.CommandID == "" || pending.PreviousTarget == "" || pending.TargetTarget == "" {
		return nil, fmt.Errorf("%s update is incomplete", state)
	}
	return &pending, nil
}

func (m *Manager) Confirm(version string) error {
	pending, err := m.Pending()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if pending.TargetVersion != version {
		return fmt.Errorf("running version %q does not match pending version %q", version, pending.TargetVersion)
	}
	if err := os.Remove(m.pendingPath()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("confirm update: %w", err)
	}
	return syncDirectory(m.stateDirectory)
}

func (m *Manager) Apply(ctx context.Context, request Request) (string, error) {
	if !m.Enabled() {
		return "", errors.New("release updates are not configured")
	}
	if request.CommandID == "" {
		return "", errors.New("update command id is required")
	}
	if request.Version != "" {
		if err := validateVersion(request.Version); err != nil {
			return "", err
		}
	}
	manifestURL, err := m.releases.URL(releaseRepository, request.Version, manifestAssetName)
	if err != nil {
		return "", err
	}
	manifestRaw, err := m.releases.Download(ctx, manifestURL, maxManifestBytes)
	if err != nil {
		return "", fmt.Errorf("download update manifest: %w", err)
	}
	manifest, err := decodeManifest(manifestRaw)
	if err != nil {
		return "", err
	}
	if request.Version != "" && manifest.Version != request.Version {
		return "", fmt.Errorf("manifest version %q does not match requested version %q", manifest.Version, request.Version)
	}
	if err := m.verifyInstalledRuntimeAssets(manifest.RuntimeAssetsSHA256); err != nil {
		return "", err
	}
	request.Version = manifest.Version
	artifact, err := selectArtifact(manifest)
	if err != nil {
		return "", err
	}
	archiveName := "xui-agent-linux-" + artifact.Arch + ".tar.gz"
	archiveURL, err := m.releases.URL(releaseRepository, manifest.Version, archiveName)
	if err != nil {
		return "", err
	}
	archive, err := m.releases.Download(ctx, archiveURL, maxArchiveBytes)
	if err != nil {
		return "", fmt.Errorf("download update archive: %w", err)
	}
	if artifact.Size <= 0 || int64(len(archive)) != artifact.Size {
		return "", errors.New("update archive size does not match the manifest")
	}
	digest := sha256.Sum256(archive)
	if !strings.EqualFold(hex.EncodeToString(digest[:]), artifact.SHA256) {
		return "", errors.New("update archive checksum does not match the manifest")
	}
	binary, runtimeAssetsDigest, err := extractRelease(archive)
	if err != nil {
		return "", err
	}
	if runtimeAssetsDigest != manifest.RuntimeAssetsSHA256 {
		return "", errors.New("update archive runtime assets do not match the manifest")
	}
	return m.InstallLocal(ctx, request, binary)
}

// InstallLocal prepares an already verified local binary for activation.
// Root-owned release installers use this entry point after verifying the release
// archive, while the Manager retains ownership of the durable update state.
func (m *Manager) InstallLocal(ctx context.Context, request Request, binary []byte) (string, error) {
	if request.CommandID == "" || len(request.CommandID) > 256 {
		return "", errors.New("update command id is invalid")
	}
	if err := validateVersion(request.Version); err != nil {
		return "", err
	}
	if len(binary) == 0 || len(binary) > maxBinaryBytes {
		return "", errors.New("candidate binary size is invalid")
	}
	return m.install(ctx, request, binary)
}

func (m *Manager) verifyInstalledRuntimeAssets(want string) error {
	if m.installedAssetsPath == "" {
		return errors.New("runtime assets path is not configured; run the release installer")
	}
	raw, err := os.ReadFile(m.installedAssetsPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return errors.New("runtime assets are not registered; run the release installer")
		}
		return fmt.Errorf("read installed runtime assets: %w", err)
	}
	got := strings.ToLower(strings.TrimSpace(string(raw)))
	if len(got) != sha256.Size*2 || got != want {
		return errors.New("installed runtime assets do not match the update; run the release installer")
	}
	return nil
}

func (m *Manager) install(ctx context.Context, request Request, binary []byte) (string, error) {
	if _, err := m.Pending(); err == nil {
		return "", errors.New("another agent update is pending")
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", fmt.Errorf("read pending update state: %w", err)
	}
	if err := m.pruneVersions(); err != nil {
		return "", fmt.Errorf("garbage collect agent versions: %w", err)
	}
	versionsDirectory := filepath.Join(m.stateDirectory, "versions")
	binaryDigest := sha256.Sum256(binary)
	releaseDirectory := request.Version + "-" + hex.EncodeToString(binaryDigest[:])
	targetDirectory := filepath.Join(versionsDirectory, releaseDirectory)
	if err := os.MkdirAll(targetDirectory, 0o700); err != nil {
		return "", fmt.Errorf("create version directory: %w", err)
	}
	temporary, err := os.CreateTemp(targetDirectory, ".xui-agent-*")
	if err != nil {
		return "", fmt.Errorf("create candidate binary: %w", err)
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o755); err != nil {
		temporary.Close()
		return "", err
	}
	if _, err := temporary.Write(binary); err != nil {
		temporary.Close()
		return "", fmt.Errorf("write candidate binary: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return "", fmt.Errorf("sync candidate binary: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	output, err := exec.CommandContext(checkCtx, temporaryPath, "run", "-version").CombinedOutput()
	cancel()
	if err != nil {
		return "", fmt.Errorf("validate candidate binary: %w", err)
	}
	if string(output) != request.Version+"\n" {
		return "", errors.New("candidate binary reports a different version")
	}
	targetBinary := filepath.Join(targetDirectory, "xui-agent")
	if err := os.Rename(temporaryPath, targetBinary); err != nil {
		return "", fmt.Errorf("activate candidate file: %w", err)
	}
	if err := syncDirectory(targetDirectory); err != nil {
		return "", err
	}

	currentPath := filepath.Join(m.stateDirectory, "current")
	previousTarget, err := os.Readlink(currentPath)
	if err != nil {
		return "", fmt.Errorf("read current agent target: %w", err)
	}
	resolvedCurrent, err := filepath.EvalSymlinks(currentPath)
	if err != nil {
		return "", fmt.Errorf("resolve current agent target: %w", err)
	}
	managedRelative, err := filepath.Rel(versionsDirectory, resolvedCurrent)
	if err != nil || managedRelative == ".." || strings.HasPrefix(managedRelative, ".."+string(filepath.Separator)) {
		return "", errors.New("current agent target is outside the managed versions directory")
	}
	targetTarget, err := filepath.Rel(m.stateDirectory, targetBinary)
	if err != nil {
		return "", err
	}
	if previousTarget == targetTarget {
		return request.Version, nil
	}
	if err := atomicSymlink(previousTarget, filepath.Join(m.stateDirectory, "previous")); err != nil {
		return "", fmt.Errorf("record previous agent target: %w", err)
	}
	pending := Pending{
		CommandID:      request.CommandID,
		PreviousTarget: previousTarget,
		TargetTarget:   targetTarget,
		TargetVersion:  request.Version,
		StartedAt:      time.Now().Unix(),
	}
	_ = os.Remove(filepath.Join(m.stateDirectory, failedFilename))
	if err := writeJSONAtomic(m.pendingPath(), pending, 0o600); err != nil {
		return "", err
	}
	if err := atomicSymlink(targetTarget, currentPath); err != nil {
		_ = os.Remove(m.pendingPath())
		return "", fmt.Errorf("switch current agent target: %w", err)
	}
	if err := syncDirectory(m.stateDirectory); err != nil {
		return "", err
	}
	return request.Version, nil
}

func (m *Manager) pruneVersions() error {
	directory := filepath.Join(m.stateDirectory, "versions")
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	protected := make(map[string]bool)
	for _, name := range []string{"current", "previous"} {
		target, err := os.Readlink(filepath.Join(m.stateDirectory, name))
		if err == nil {
			versionDirectory, err := agentVersionDirectory(target)
			if err != nil {
				return err
			}
			protected[versionDirectory] = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	type installedVersion struct {
		name    string
		modTime time.Time
	}
	versions := make([]installedVersion, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "" || entry.Name() == "." || entry.Name() == ".." {
			continue
		}
		children, err := os.ReadDir(filepath.Join(directory, entry.Name()))
		if err != nil {
			return err
		}
		if len(children) != 1 || children[0].Name() != "xui-agent" || children[0].Type()&os.ModeSymlink != 0 {
			continue
		}
		binaryInfo, err := children[0].Info()
		if err != nil {
			return err
		}
		if !binaryInfo.Mode().IsRegular() || binaryInfo.Mode().Perm()&0o111 == 0 {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		versions = append(versions, installedVersion{name: entry.Name(), modTime: info.ModTime()})
	}
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].modTime.Equal(versions[j].modTime) {
			return versions[i].name > versions[j].name
		}
		return versions[i].modTime.After(versions[j].modTime)
	})
	removed := false
	for index, version := range versions {
		if index < retainedVersions || protected[version.name] {
			continue
		}
		versionDirectory := filepath.Join(directory, version.name)
		if err := os.Remove(filepath.Join(versionDirectory, "xui-agent")); err != nil {
			return err
		}
		if err := os.Remove(versionDirectory); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return syncDirectory(directory)
	}
	return nil
}

func agentVersionDirectory(target string) (string, error) {
	if target == "" || filepath.IsAbs(target) || filepath.Clean(target) != target {
		return "", errors.New("agent version target is invalid")
	}
	parts := strings.Split(target, string(filepath.Separator))
	if len(parts) != 3 || parts[0] != "versions" || parts[1] == "" || parts[2] != "xui-agent" {
		return "", errors.New("agent version target is outside the versions directory")
	}
	return parts[1], nil
}

func decodeManifest(raw []byte) (Manifest, error) {
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, fmt.Errorf("decode update manifest: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return Manifest{}, errors.New("update manifest contains trailing data")
	}
	if manifest.SchemaVersion != ManifestSchemaVersion || manifest.Version == "" || len(manifest.Artifacts) == 0 {
		return Manifest{}, errors.New("update manifest is incomplete or unsupported")
	}
	manifest.RuntimeAssetsSHA256 = strings.ToLower(strings.TrimSpace(manifest.RuntimeAssetsSHA256))
	if len(manifest.RuntimeAssetsSHA256) != sha256.Size*2 {
		return Manifest{}, errors.New("update manifest runtime assets digest is invalid")
	}
	if _, err := hex.DecodeString(manifest.RuntimeAssetsSHA256); err != nil {
		return Manifest{}, errors.New("update manifest runtime assets digest is invalid")
	}
	if err := validateVersion(manifest.Version); err != nil {
		return Manifest{}, err
	}
	return manifest, nil
}

func selectArtifact(manifest Manifest) (Artifact, error) {
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	for _, artifact := range manifest.Artifacts {
		if artifact.OS == runtime.GOOS && artifact.Arch == arch {
			if len(artifact.SHA256) != sha256.Size*2 || artifact.Size <= 0 {
				return Artifact{}, errors.New("update artifact metadata is invalid")
			}
			return artifact, nil
		}
	}
	return Artifact{}, fmt.Errorf("manifest has no artifact for %s/%s", runtime.GOOS, arch)
}

func RuntimeAssetsDigest(archive []byte) (string, error) {
	_, digest, err := extractRelease(archive)
	return digest, err
}

func extractRelease(archive []byte) ([]byte, string, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, "", fmt.Errorf("open update archive: %w", err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var binary []byte
	runtimeAssets := make(map[string][]byte, len(runtimeAssetNames))
	expected := make(map[string]struct{}, len(runtimeAssetNames)+1)
	expected["xui-agent"] = struct{}{}
	for _, name := range runtimeAssetNames {
		expected[name] = struct{}{}
	}
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, "", fmt.Errorf("read update archive: %w", err)
		}
		if _, ok := expected[header.Name]; !ok {
			return nil, "", fmt.Errorf("update archive contains unexpected entry %q", header.Name)
		}
		limit := int64(maxRuntimeAssetBytes)
		if header.Name == "xui-agent" {
			limit = maxBinaryBytes
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > limit {
			return nil, "", fmt.Errorf("update archive has an invalid %s entry", header.Name)
		}
		content, readErr := io.ReadAll(io.LimitReader(reader, limit+1))
		if readErr != nil || int64(len(content)) != header.Size {
			return nil, "", fmt.Errorf("read %s from update archive", header.Name)
		}
		if header.Name == "xui-agent" {
			if binary != nil {
				return nil, "", errors.New("update archive has duplicate xui-agent entries")
			}
			binary = content
		} else {
			if _, exists := runtimeAssets[header.Name]; exists {
				return nil, "", fmt.Errorf("update archive has duplicate %s entries", header.Name)
			}
			runtimeAssets[header.Name] = content
		}
	}
	if binary == nil {
		return nil, "", errors.New("update archive does not contain xui-agent")
	}
	hasher := sha256.New()
	for _, name := range runtimeAssetNames {
		content, ok := runtimeAssets[name]
		if !ok {
			return nil, "", fmt.Errorf("update archive does not contain %s", name)
		}
		digest := sha256.Sum256(content)
		_, _ = fmt.Fprintf(hasher, "%x  %s\n", digest, name)
	}
	return binary, hex.EncodeToString(hasher.Sum(nil)), nil
}

func validateVersion(version string) error {
	if version == "" || version == "." || version == ".." || len(version) > 128 {
		return errors.New("update version is invalid")
	}
	for _, r := range version {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return errors.New("update version is invalid")
	}
	return nil
}

func atomicSymlink(target, path string) error {
	temporary := fmt.Sprintf("%s.tmp-%d", path, time.Now().UnixNano())
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	defer os.Remove(temporary)
	return os.Rename(temporary, path)
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	raw, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".update-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(raw); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func (m *Manager) pendingPath() string {
	return filepath.Join(m.stateDirectory, pendingFilename)
}
