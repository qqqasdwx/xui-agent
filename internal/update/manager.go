package update

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const (
	maxManifestBytes  = 1 << 20
	maxSignatureBytes = 4 << 10
	maxArchiveBytes   = 128 << 20
	maxBinaryBytes    = 64 << 20
	pendingFilename   = "update-pending.json"
	failedFilename    = "update-failed.json"
)

var ErrRestartRequired = errors.New("agent restart required to activate the update")

type Artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type Manifest struct {
	SchemaVersion int        `json:"schemaVersion"`
	Version       string     `json:"version"`
	PublishedAt   string     `json:"publishedAt"`
	Artifacts     []Artifact `json:"artifacts"`
}

type Request struct {
	CommandID    string
	Version      string
	ManifestURL  string
	SignatureURL string
}

type Pending struct {
	CommandID      string `json:"commandId"`
	PreviousTarget string `json:"previousTarget"`
	TargetTarget   string `json:"targetTarget"`
	TargetVersion  string `json:"targetVersion"`
	StartedAt      int64  `json:"startedAt"`
}

type Manager struct {
	stateDirectory string
	publicKey      ed25519.PublicKey
	allowInsecure  bool
	httpClient     *http.Client
}

func NewManager(stateDirectory, publicKey string, allowInsecure bool) (*Manager, error) {
	m := &Manager{
		stateDirectory: stateDirectory,
		allowInsecure:  allowInsecure,
		httpClient: &http.Client{
			Timeout: 2 * time.Minute,
			CheckRedirect: func(request *http.Request, _ []*http.Request) error {
				if request.URL.User != nil {
					return errors.New("update redirect contains credentials")
				}
				if request.URL.Scheme == "https" || (allowInsecure && request.URL.Scheme == "http") {
					return nil
				}
				return errors.New("update redirect must use HTTPS")
			},
		},
	}
	if publicKey == "" {
		return m, nil
	}
	raw, err := base64.StdEncoding.DecodeString(publicKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, errors.New("update public key is invalid")
	}
	m.publicKey = ed25519.PublicKey(raw)
	return m, nil
}

func (m *Manager) Enabled() bool {
	return len(m.publicKey) == ed25519.PublicKeySize
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
		return "", errors.New("signed updates are not configured")
	}
	if request.CommandID == "" {
		return "", errors.New("update command id is required")
	}
	if request.Version != "" {
		if err := validateVersion(request.Version); err != nil {
			return "", err
		}
	}
	manifestRaw, err := m.download(ctx, request.ManifestURL, maxManifestBytes)
	if err != nil {
		return "", fmt.Errorf("download update manifest: %w", err)
	}
	signatureRaw, err := m.download(ctx, request.SignatureURL, maxSignatureBytes)
	if err != nil {
		return "", fmt.Errorf("download update signature: %w", err)
	}
	signature, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(signatureRaw)))
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(m.publicKey, manifestRaw, signature) {
		return "", errors.New("update manifest signature verification failed")
	}
	manifest, err := decodeManifest(manifestRaw)
	if err != nil {
		return "", err
	}
	if request.Version != "" && manifest.Version != request.Version {
		return "", fmt.Errorf("manifest version %q does not match requested version %q", manifest.Version, request.Version)
	}
	request.Version = manifest.Version
	artifact, err := selectArtifact(manifest)
	if err != nil {
		return "", err
	}
	archive, err := m.download(ctx, artifact.URL, maxArchiveBytes)
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
	binary, err := extractBinary(archive)
	if err != nil {
		return "", err
	}
	return m.install(ctx, request, binary)
}

func (m *Manager) install(ctx context.Context, request Request, binary []byte) (string, error) {
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
	output, err := exec.CommandContext(checkCtx, temporaryPath, "version").CombinedOutput()
	cancel()
	if err != nil {
		return "", fmt.Errorf("validate candidate binary: %w", err)
	}
	if !strings.HasPrefix(strings.TrimSpace(string(output)), "xui-agent "+request.Version+" ") {
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

func (m *Manager) download(ctx context.Context, rawURL string, limit int64) ([]byte, error) {
	u, err := url.Parse(rawURL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("update URL must be an absolute URL without credentials, query, or fragment")
	}
	if u.Scheme != "https" && !(m.allowInsecure && u.Scheme == "http") {
		return nil, errors.New("update URL must use HTTPS")
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	response, err := m.httpClient.Do(request)
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
		return nil, errors.New("download exceeds the size limit")
	}
	return raw, nil
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
	if manifest.SchemaVersion != 1 || manifest.Version == "" || len(manifest.Artifacts) == 0 {
		return Manifest{}, errors.New("update manifest is incomplete or unsupported")
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

func extractBinary(archive []byte) ([]byte, error) {
	gz, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open update archive: %w", err)
	}
	defer gz.Close()
	reader := tar.NewReader(gz)
	var binary []byte
	for {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read update archive: %w", err)
		}
		if header.Name != "xui-agent" {
			continue
		}
		if header.Typeflag != tar.TypeReg || header.Size <= 0 || header.Size > maxBinaryBytes || binary != nil {
			return nil, errors.New("update archive has an invalid xui-agent entry")
		}
		binary, err = io.ReadAll(io.LimitReader(reader, maxBinaryBytes+1))
		if err != nil || int64(len(binary)) != header.Size {
			return nil, errors.New("read xui-agent from update archive")
		}
	}
	if binary == nil {
		return nil, errors.New("update archive does not contain xui-agent")
	}
	return binary, nil
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
