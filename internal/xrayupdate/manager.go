package xrayupdate

import (
	"archive/zip"
	"bytes"
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
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/release"
	"github.com/qqqasdwx/xui-agent/internal/xrayruntime"
)

const (
	StatusInstalled     = "installed"
	StatusInstallFailed = "install_failed"

	ErrorCodeValidationFailed  = "validation_failed"
	ErrorCodePreparationFailed = "preparation_failed"
	ErrorCodeActivationFailed  = "activation_failed"
	ErrorCodePersistenceFailed = "persistence_failed"
	ErrorCodeRecoveryFailed    = "recovery_failed"
	ErrorCodeUnclassified      = "unclassified"

	RecoveryStatusNotRequired = "not_required"
	RecoveryStatusRolledBack  = "rolled_back"
	RecoveryStatusFailed      = "failed"
	RecoveryStatusUnknown     = "unknown"

	directoryName      = "xray-runtime"
	downloadsName      = ".downloads"
	downloadPattern    = "xray-release-*"
	legacyTempPattern  = ".xui-agent-xray-release-*"
	versionsName       = "versions"
	currentName        = "current"
	previousName       = "previous"
	appliedName        = "applied.json"
	pendingName        = "update-pending.json"
	failedName         = "update-failed.json"
	bundleStateName    = "bundle.json"
	stateDirectoryMode = 0o700
	stateFileMode      = 0o600
	retainedVersions   = 3
	maxStateBytes      = 64 << 10
	validationTimeout  = 15 * time.Second
	recoveryTimeout    = 30 * time.Second
	releaseBaseURL     = "https://github.com"
	releaseRepository  = "qqqasdwx/Xray-core"
	manifestAssetName  = "xray-manifest.json"
)

var errAppliedStateCorrupt = errors.New("applied Xray binary state is corrupt")

type Request struct {
	CommandID string
	Version   string
}

type Result struct {
	Version        string `json:"version"`
	Status         string `json:"status"`
	Error          string `json:"error,omitempty"`
	ErrorCode      string `json:"errorCode,omitempty"`
	RecoveryStatus string `json:"recoveryStatus,omitempty"`
	AppliedAt      int64  `json:"appliedAt,omitempty"`
	RolledBack     bool   `json:"rolledBack,omitempty"`
}

func (r Result) Success() bool { return r.Status == StatusInstalled }

type AppliedState struct {
	Version       string `json:"version"`
	XrayVersion   string `json:"xrayVersion"`
	ArchiveDigest string `json:"archiveDigest"`
	Target        string `json:"target"`
	AppliedAt     int64  `json:"appliedAt"`
}

type pendingState struct {
	CommandID       string        `json:"commandId"`
	PreviousTarget  string        `json:"previousTarget,omitempty"`
	PreviousApplied *AppliedState `json:"previousApplied,omitempty"`
	Target          string        `json:"target"`
	Version         string        `json:"version"`
	XrayVersion     string        `json:"xrayVersion"`
	ArchiveDigest   string        `json:"archiveDigest"`
	StartedAt       int64         `json:"startedAt"`
}

type bundleState struct {
	Version       string            `json:"version"`
	XrayVersion   string            `json:"xrayVersion"`
	ArchiveDigest string            `json:"archiveDigest"`
	Files         map[string]string `json:"files"`
}

type Controller interface {
	RestartAndWait(context.Context) error
	StopAndWait(context.Context) error
}

type preflightController interface {
	Preflight() error
}

type Runner interface {
	ValidateVersion(context.Context, string, string) error
	ValidateConfig(context.Context, string, string) error
}

type ExecRunner struct{}

func (ExecRunner) ValidateVersion(ctx context.Context, binaryPath, version string) error {
	checkCtx, cancel := context.WithTimeout(ctx, validationTimeout)
	defer cancel()
	output := &limitedBuffer{limit: 32 << 10}
	command := exec.CommandContext(checkCtx, binaryPath, "version")
	command.Dir = filepath.Dir(binaryPath)
	command.Env = append(os.Environ(), "XRAY_LOCATION_ASSET="+command.Dir)
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
			return errors.New("Xray version check timed out")
		}
		return fmt.Errorf("run Xray version check: %s", cleanError("", err))
	}
	fields := strings.Fields(output.String())
	if len(fields) < 2 || fields[0] != "Xray" || strings.TrimPrefix(fields[1], "v") != strings.TrimPrefix(version, "v") {
		return errors.New("Xray candidate reports a different version")
	}
	return nil
}

func (ExecRunner) ValidateConfig(ctx context.Context, binaryPath, configPath string) error {
	checkCtx, cancel := context.WithTimeout(ctx, validationTimeout)
	defer cancel()
	output := &limitedBuffer{limit: 32 << 10}
	command := exec.CommandContext(checkCtx, binaryPath, "run", "-test", "-config", configPath)
	command.Dir = filepath.Dir(binaryPath)
	command.Env = append(os.Environ(), "XRAY_LOCATION_ASSET="+command.Dir)
	command.Stdout = output
	command.Stderr = output
	if err := command.Run(); err != nil {
		if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
			return errors.New("Xray candidate config validation timed out")
		}
		return fmt.Errorf("validate current config with Xray candidate: %s", cleanError("", err))
	}
	return nil
}

type Manager struct {
	stateDirectory string
	directory      string
	releases       *release.Client
	controller     Controller
	runner         Runner
	writeState     func(string, any, os.FileMode) error
	legacyTempDir  string
	mu             sync.Mutex
}

func NewManager(stateDirectory string, controller Controller, runner Runner) (*Manager, error) {
	releases, err := release.NewClient(releaseBaseURL, false)
	if err != nil {
		return nil, err
	}
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Manager{
		stateDirectory: stateDirectory,
		directory:      Directory(stateDirectory),
		releases:       releases,
		controller:     controller,
		runner:         runner,
		legacyTempDir:  os.TempDir(),
	}, nil
}

func (m *Manager) Enabled() bool {
	return m != nil && m.releases != nil && m.controller != nil && m.runner != nil
}

func (m *Manager) Apply(ctx context.Context, request Request) (Result, error) {
	if !m.Enabled() {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, errors.New("Xray release updates are not configured"))
	}
	if request.CommandID == "" || len(request.CommandID) > 256 {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, errors.New("Xray update command id is invalid"))
	}
	if request.Version != "" {
		if err := validateVersion(request.Version); err != nil {
			return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, err)
		}
	}
	manifestURL, err := m.releases.URL(releaseRepository, request.Version, manifestAssetName)
	if err != nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, err)
	}
	manifestRaw, err := m.releases.Download(ctx, manifestURL, maxManifestBytes)
	if err != nil {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, err)
	}
	manifest, err := decodeManifest(manifestRaw)
	if err != nil {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, err)
	}
	if request.Version != "" && request.Version != manifest.Version {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired,
			fmt.Errorf("Xray manifest version %q does not match requested version %q", manifest.Version, request.Version))
	}
	artifact, err := selectArtifact(manifest)
	if err != nil {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, err)
	}
	archiveURL, err := m.releases.URL(releaseRepository, manifest.Version, releaseAssetName(artifact.Arch))
	if err != nil {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, err)
	}
	downloadDirectory, err := m.ensureDownloadDirectory()
	if err != nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, fmt.Errorf("prepare Xray release download directory: %w", err))
	}
	archive, err := os.CreateTemp(downloadDirectory, downloadPattern)
	if err != nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, fmt.Errorf("create Xray release temporary file: %w", err))
	}
	archivePath := archive.Name()
	defer os.Remove(archivePath)
	defer archive.Close()
	if err := archive.Chmod(stateFileMode); err != nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, fmt.Errorf("secure Xray release temporary file: %w", err))
	}
	hasher := sha256.New()
	size, err := m.releases.DownloadTo(ctx, archiveURL, artifact.Size, io.MultiWriter(archive, hasher))
	if err != nil {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, fmt.Errorf("download Xray release archive: %w", err))
	}
	if size != artifact.Size {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, errors.New("Xray release archive size does not match the manifest"))
	}
	archiveDigest := hex.EncodeToString(hasher.Sum(nil))
	if archiveDigest != artifact.SHA256 {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, errors.New("Xray release archive digest does not match the manifest"))
	}
	if err := archive.Sync(); err != nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, fmt.Errorf("sync Xray release temporary file: %w", err))
	}
	return m.installArchive(ctx, request.CommandID, manifest.Version, manifest.XrayVersion, archiveDigest, archive)
}

func (m *Manager) InstallLocal(ctx context.Context, commandID, version string, archive []byte) (Result, error) {
	if commandID == "" || len(commandID) > 256 {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, errors.New("Xray update command id is invalid"))
	}
	if err := validateVersion(version); err != nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, err)
	}
	if len(archive) == 0 || len(archive) > maxArchiveBytes {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, errors.New("Xray release archive size is invalid"))
	}
	digest := sha256.Sum256(archive)
	return m.install(ctx, commandID, version, version, hex.EncodeToString(digest[:]), archive)
}

func (m *Manager) install(ctx context.Context, commandID, version, xrayVersion, archiveDigest string, archive []byte) (Result, error) {
	candidate, err := extractBundle(archive)
	if err != nil {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, err)
	}
	return m.installPrepared(ctx, commandID, version, xrayVersion, archiveDigest, func() (string, error) {
		return m.installBundle(version, xrayVersion, archiveDigest, candidate)
	})
}

func (m *Manager) installArchive(ctx context.Context, commandID, version, xrayVersion, archiveDigest string, archive *os.File) (Result, error) {
	if archive == nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, errors.New("Xray release temporary file is unavailable"))
	}
	info, err := archive.Stat()
	if err != nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, fmt.Errorf("inspect Xray release temporary file: %w", err))
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxArchiveBytes {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, errors.New("Xray release archive is not a secure regular file"))
	}
	reader, err := zip.NewReader(archive, info.Size())
	if err != nil {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, fmt.Errorf("open Xray release archive: %w", err))
	}
	entries, err := validateBundleArchive(reader)
	if err != nil {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, err)
	}
	return m.installPrepared(ctx, commandID, version, xrayVersion, archiveDigest, func() (string, error) {
		return m.installBundleArchive(version, xrayVersion, archiveDigest, entries)
	})
}

func (m *Manager) installPrepared(
	ctx context.Context,
	commandID, version, xrayVersion, archiveDigest string,
	installCandidate func() (string, error),
) (Result, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ensureDirectory(); err != nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, err)
	}
	if _, err := m.loadPending(); err == nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusUnknown, errors.New("another Xray binary update is pending"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusUnknown, fmt.Errorf("load pending Xray binary update: %w", err))
	}
	if err := m.pruneVersions(); err != nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, fmt.Errorf("garbage collect Xray versions: %w", err))
	}
	target, err := installCandidate()
	if err != nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, err)
	}
	candidateBinary := filepath.Join(m.directory, target, "xray")
	if err := m.runner.ValidateVersion(ctx, candidateBinary, xrayVersion); err != nil {
		return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, err)
	}

	var previousTarget string
	var previousApplied *AppliedState
	applied, err := m.loadApplied()
	if err == nil {
		if err := m.verifyApplied(applied); err != nil {
			return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, err)
		}
		if applied.Version == version && applied.XrayVersion == xrayVersion && applied.ArchiveDigest == archiveDigest && applied.Target == target {
			if err := removeAndSync(m.failedPath()); err != nil {
				return Result{}, newUpdateError(ErrorCodePersistenceFailed, RecoveryStatusNotRequired, fmt.Errorf("clear failed Xray binary update: %w", err))
			}
			if err := m.pruneVersions(); err != nil {
				return Result{}, newUpdateError(ErrorCodePersistenceFailed, RecoveryStatusNotRequired, fmt.Errorf("garbage collect Xray versions: %w", err))
			}
			return resultFromApplied(applied), nil
		}
		previousTarget = applied.Target
		previousApplied = applied
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, fmt.Errorf("load applied Xray binary state: %w", err))
	} else if _, linkErr := os.Lstat(m.currentPath()); linkErr == nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, errors.New("current Xray binary exists without applied state"))
	} else if !errors.Is(linkErr, os.ErrNotExist) {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, linkErr)
	}

	configPath, hasConfig, err := m.appliedConfigPath()
	if err != nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, err)
	}
	if hasConfig {
		if err := m.runner.ValidateConfig(ctx, candidateBinary, configPath); err != nil {
			return Result{}, newUpdateError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, err)
		}
		if controller, ok := m.controller.(preflightController); ok {
			if err := controller.Preflight(); err != nil {
				return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, fmt.Errorf("preflight managed Xray process: %w", err))
			}
		}
	}
	pending := pendingState{
		CommandID: commandID, PreviousTarget: previousTarget, PreviousApplied: previousApplied, Target: target,
		Version: version, XrayVersion: xrayVersion, ArchiveDigest: archiveDigest, StartedAt: time.Now().Unix(),
	}
	if err := m.persistState(m.pendingPath(), pending, stateFileMode); err != nil {
		return Result{}, newUpdateError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, fmt.Errorf("persist pending Xray binary update: %w", err))
	}
	if hasConfig {
		if err := m.controller.StopAndWait(ctx); err != nil {
			return m.rollbackResult(ctx, pending, configPath, cleanError("stop current Xray before binary update", err), ErrorCodeActivationFailed)
		}
	}
	if err := atomicSymlink(target, m.currentPath()); err != nil {
		return m.rollbackResult(ctx, pending, configPath, cleanError("switch current Xray binary", err), ErrorCodePersistenceFailed)
	}
	if hasConfig {
		if err := m.controller.RestartAndWait(ctx); err != nil {
			return m.rollbackResult(ctx, pending, configPath, cleanError("start updated Xray binary", err), ErrorCodeActivationFailed)
		}
	}
	if previousTarget != "" {
		if err := atomicSymlink(previousTarget, m.previousPath()); err != nil {
			return m.rollbackResult(ctx, pending, configPath, cleanError("record previous Xray binary", err), ErrorCodePersistenceFailed)
		}
	}
	next := &AppliedState{Version: version, XrayVersion: xrayVersion, ArchiveDigest: archiveDigest, Target: target, AppliedAt: time.Now().Unix()}
	if err := m.persistState(m.appliedPath(), next, stateFileMode); err != nil {
		return m.rollbackResult(ctx, pending, configPath, cleanError("persist applied Xray binary", err), ErrorCodePersistenceFailed)
	}
	if err := removeAndSync(m.pendingPath()); err != nil {
		return Result{}, newUpdateError(ErrorCodePersistenceFailed, RecoveryStatusUnknown, fmt.Errorf("confirm Xray binary update: %w", err))
	}
	if err := removeAndSync(m.failedPath()); err != nil {
		return Result{}, newUpdateError(ErrorCodePersistenceFailed, RecoveryStatusUnknown, fmt.Errorf("clear failed Xray binary update: %w", err))
	}
	if err := m.pruneVersions(); err != nil {
		return Result{}, newUpdateError(ErrorCodePersistenceFailed, RecoveryStatusNotRequired, fmt.Errorf("garbage collect Xray versions: %w", err))
	}
	return resultFromApplied(next), nil
}

func (m *Manager) Recover(ctx context.Context) error {
	if m == nil || m.controller == nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.cleanupStaleDownloads(); err != nil {
		return fmt.Errorf("clean stale Xray release downloads: %w", err)
	}
	pending, err := m.loadPending()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load pending Xray binary update: %w", err)
	}
	applied, appliedErr := m.loadApplied()
	if appliedErr == nil && applied.Target == pending.Target && applied.Version == pending.Version && applied.XrayVersion == pending.XrayVersion && applied.ArchiveDigest == pending.ArchiveDigest {
		if err := m.verifyApplied(applied); err == nil {
			if err := removeAndSync(m.pendingPath()); err != nil {
				return err
			}
			return removeAndSync(m.failedPath())
		}
	}
	if appliedErr != nil && !errors.Is(appliedErr, os.ErrNotExist) {
		return fmt.Errorf("load applied Xray binary state during recovery: %w", appliedErr)
	}
	configPath, _, err := m.appliedConfigPath()
	if err != nil {
		return err
	}
	if err := m.rollback(ctx, *pending, configPath, "unconfirmed Xray binary update was rolled back after agent restart", ErrorCodePreparationFailed); err != nil {
		combined := cleanError("recover Xray binary update", err)
		_ = m.persistState(m.failedPath(), Result{
			Version: pending.Version, Status: StatusInstallFailed, Error: combined,
			ErrorCode: ErrorCodePreparationFailed, RecoveryStatus: RecoveryStatusFailed,
		}, stateFileMode)
		return newUpdateError(ErrorCodeRecoveryFailed, RecoveryStatusFailed, errors.New(combined))
	}
	return nil
}

func (m *Manager) rollbackResult(ctx context.Context, pending pendingState, configPath, failure, errorCode string) (Result, error) {
	if err := m.rollback(ctx, pending, configPath, failure, errorCode); err != nil {
		combined := cleanError(failure+"; rollback failed", err)
		failed := Result{
			Version: pending.Version, Status: StatusInstallFailed, Error: combined,
			ErrorCode: errorCode, RecoveryStatus: RecoveryStatusFailed,
		}
		_ = m.persistState(m.failedPath(), failed, stateFileMode)
		return Result{}, newUpdateError(ErrorCodeRecoveryFailed, RecoveryStatusFailed, errors.New(combined))
	}
	return Result{
		Version: pending.Version, Status: StatusInstallFailed, Error: failure,
		ErrorCode: errorCode, RecoveryStatus: RecoveryStatusRolledBack, RolledBack: true,
	}, nil
}

func (m *Manager) rollback(ctx context.Context, pending pendingState, configPath, failure, errorCode string) error {
	recoveryCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), recoveryTimeout)
	defer cancel()
	if configPath != "" {
		if err := m.prepareRollbackController(pending); err != nil {
			return fmt.Errorf("prepare Xray binary rollback: %w", err)
		}
		if err := m.controller.StopAndWait(recoveryCtx); err != nil {
			return fmt.Errorf("stop failed Xray binary: %w", err)
		}
	}
	if pending.PreviousTarget == "" {
		if err := removeAndSync(m.currentPath()); err != nil {
			return fmt.Errorf("remove failed initial Xray binary: %w", err)
		}
		if err := removeAndSync(m.appliedPath()); err != nil {
			return fmt.Errorf("remove failed initial Xray binary state: %w", err)
		}
	} else {
		previousApplied, err := m.rollbackAppliedState(pending)
		if err != nil {
			if deselectErr := removeAndSync(m.currentPath()); deselectErr != nil {
				return fmt.Errorf("verify previous Xray binary: %w; deselect failed candidate: %v", err, deselectErr)
			}
			return fmt.Errorf("verify previous Xray binary: %w", err)
		}
		if err := atomicSymlink(pending.PreviousTarget, m.currentPath()); err != nil {
			return fmt.Errorf("restore previous Xray binary: %w", err)
		}
		if err := m.persistState(m.appliedPath(), previousApplied, stateFileMode); err != nil {
			return fmt.Errorf("restore previous Xray binary state: %w", err)
		}
	}
	if configPath != "" {
		if err := m.controller.RestartAndWait(recoveryCtx); err != nil {
			if pending.PreviousTarget == "" {
				return fmt.Errorf("restart bootstrap Xray binary: %w", err)
			}
			return fmt.Errorf("restart previous Xray binary: %w", err)
		}
	}
	failed := Result{
		Version: pending.Version, Status: StatusInstallFailed, Error: failure,
		ErrorCode: errorCode, RecoveryStatus: RecoveryStatusRolledBack, RolledBack: true,
	}
	if err := m.persistState(m.failedPath(), failed, stateFileMode); err != nil {
		return err
	}
	return removeAndSync(m.pendingPath())
}

func (m *Manager) prepareRollbackController(pending pendingState) error {
	applied, err := m.loadApplied()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if applied.Target != pending.Target || applied.Version != pending.Version || applied.XrayVersion != pending.XrayVersion || applied.ArchiveDigest != pending.ArchiveDigest {
		return nil
	}
	if _, err := os.Lstat(m.currentPath()); err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := m.verifyAppliedTarget(applied); err != nil {
		return err
	}
	return atomicSymlink(pending.Target, m.currentPath())
}

func (m *Manager) Current() (*AppliedState, error) {
	if m == nil {
		return nil, os.ErrNotExist
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	applied, err := m.loadApplied()
	if err != nil {
		return nil, err
	}
	if err := m.verifyApplied(applied); err != nil {
		return nil, fmt.Errorf("%w: %v", errAppliedStateCorrupt, err)
	}
	return applied, nil
}

func (m *Manager) appliedConfigPath() (string, bool, error) {
	applied, err := xrayruntime.LoadAppliedState(m.stateDirectory)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("load applied Xray config for binary update: %w", err)
	}
	return filepath.Join(xrayruntime.Directory(m.stateDirectory), applied.Target), true, nil
}

func (m *Manager) installBundle(version, xrayVersion, archiveDigest string, candidate bundle) (string, error) {
	return m.installBundleFiles(version, xrayVersion, archiveDigest, func(directory string) (map[string]string, error) {
		digests := make(map[string]string, len(requiredBundleFiles))
		for _, fileName := range requiredBundleFiles {
			raw := candidate.files[fileName]
			mode := os.FileMode(stateFileMode)
			if fileName == "xray" {
				mode = 0o700
			}
			if err := writeAtomic(filepath.Join(directory, fileName), raw, mode); err != nil {
				return nil, err
			}
			digest := sha256.Sum256(raw)
			digests[fileName] = hex.EncodeToString(digest[:])
		}
		return digests, nil
	})
}

func (m *Manager) installBundleArchive(version, xrayVersion, archiveDigest string, entries []*zip.File) (string, error) {
	return m.installBundleFiles(version, xrayVersion, archiveDigest, func(directory string) (map[string]string, error) {
		digests := make(map[string]string, len(requiredBundleFiles))
		for _, entry := range entries {
			limit := bundleEntryLimit(entry.Name)
			if !isRequiredBundleFile(entry.Name) {
				if _, err := copyBundleEntry(entry, io.Discard, limit); err != nil {
					return nil, err
				}
				continue
			}
			mode := os.FileMode(stateFileMode)
			if entry.Name == "xray" {
				mode = 0o700
			}
			digest, err := writeBundleEntry(filepath.Join(directory, entry.Name), entry, limit, mode)
			if err != nil {
				return nil, err
			}
			digests[entry.Name] = digest
		}
		return digests, nil
	})
}

func (m *Manager) installBundleFiles(
	version, xrayVersion, archiveDigest string,
	writeFiles func(string) (map[string]string, error),
) (string, error) {
	name := version + "-" + archiveDigest
	target := filepath.Join(versionsName, name)
	path := filepath.Join(m.directory, target)
	if _, err := os.Lstat(path); err == nil {
		state, err := m.loadVerifiedTarget(target)
		if err != nil {
			return "", fmt.Errorf("existing Xray version is invalid: %w", err)
		}
		if state.Version != version || state.XrayVersion != xrayVersion || state.ArchiveDigest != archiveDigest {
			return "", errors.New("existing Xray version metadata does not match the requested release")
		}
		return target, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	versionsDirectory := filepath.Join(m.directory, versionsName)
	if err := os.MkdirAll(versionsDirectory, stateDirectoryMode); err != nil {
		return "", err
	}
	temporary, err := os.MkdirTemp(versionsDirectory, ".xray-version-*")
	if err != nil {
		return "", err
	}
	defer removeBundleDirectory(temporary)
	if err := os.Chmod(temporary, stateDirectoryMode); err != nil {
		return "", err
	}
	fileDigests, err := writeFiles(temporary)
	if err != nil {
		return "", err
	}
	for _, fileName := range requiredBundleFiles {
		if len(fileDigests[fileName]) != sha256.Size*2 {
			return "", fmt.Errorf("Xray bundle digest for %s is invalid", fileName)
		}
	}
	state := bundleState{Version: version, XrayVersion: xrayVersion, ArchiveDigest: archiveDigest, Files: fileDigests}
	if err := writeJSONAtomic(filepath.Join(temporary, bundleStateName), state, stateFileMode); err != nil {
		return "", err
	}
	if err := syncDirectory(temporary); err != nil {
		return "", err
	}
	if err := os.Rename(temporary, path); err != nil {
		return "", err
	}
	if err := syncDirectory(versionsDirectory); err != nil {
		return "", err
	}
	return target, nil
}

func writeBundleEntry(path string, entry *zip.File, limit int64, mode os.FileMode) (string, error) {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return "", err
	}
	if err := file.Chmod(mode); err != nil {
		file.Close()
		return "", err
	}
	hasher := sha256.New()
	_, copyErr := copyBundleEntry(entry, io.MultiWriter(file, hasher), limit)
	if copyErr == nil {
		copyErr = file.Sync()
	}
	closeErr := file.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func copyBundleEntry(entry *zip.File, destination io.Writer, limit int64) (int64, error) {
	if entry == nil || destination == nil || limit <= 0 {
		return 0, errors.New("Xray release archive entry is invalid")
	}
	stream, err := entry.Open()
	if err != nil {
		return 0, err
	}
	written, copyErr := io.Copy(destination, io.LimitReader(stream, limit+1))
	closeErr := stream.Close()
	if copyErr != nil {
		return written, copyErr
	}
	if closeErr != nil {
		return written, closeErr
	}
	if written != int64(entry.UncompressedSize64) || written > limit {
		return written, fmt.Errorf("Xray release entry %q exceeds its size limit", entry.Name)
	}
	return written, nil
}

func (m *Manager) verifyApplied(applied *AppliedState) error {
	current, err := os.Readlink(m.currentPath())
	if err != nil {
		return err
	}
	if current != applied.Target {
		return errors.New("current Xray binary differs from applied state")
	}
	return m.verifyAppliedTarget(applied)
}

func (m *Manager) verifyAppliedTarget(applied *AppliedState) error {
	if err := validateAppliedState(applied); err != nil {
		return err
	}
	state, err := m.loadVerifiedTarget(applied.Target)
	if err != nil {
		return err
	}
	if state.Version != applied.Version || state.XrayVersion != applied.XrayVersion || state.ArchiveDigest != applied.ArchiveDigest {
		return errors.New("applied Xray binary metadata differs from the installed bundle")
	}
	return nil
}

func (m *Manager) rollbackAppliedState(pending pendingState) (*AppliedState, error) {
	if pending.PreviousApplied != nil {
		if pending.PreviousApplied.Target != pending.PreviousTarget {
			return nil, errors.New("previous applied Xray binary target does not match pending rollback target")
		}
		if err := m.verifyAppliedTarget(pending.PreviousApplied); err != nil {
			return nil, err
		}
		return pending.PreviousApplied, nil
	}
	state, err := m.loadVerifiedTarget(pending.PreviousTarget)
	if err != nil {
		return nil, err
	}
	return &AppliedState{
		Version: state.Version, XrayVersion: state.XrayVersion, ArchiveDigest: state.ArchiveDigest,
		Target: pending.PreviousTarget, AppliedAt: pending.StartedAt,
	}, nil
}

func (m *Manager) verifyTarget(target string) error {
	_, err := m.loadVerifiedTarget(target)
	return err
}

func (m *Manager) loadVerifiedTarget(target string) (*bundleState, error) {
	if err := validateTarget(target); err != nil {
		return nil, err
	}
	path := filepath.Join(m.directory, target)
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() || info.Mode().Perm() != stateDirectoryMode {
		return nil, errors.New("Xray version target is not a secure directory")
	}
	var state bundleState
	if err := readJSONFile(filepath.Join(path, bundleStateName), &state); err != nil {
		return nil, err
	}
	if validateVersion(state.Version) != nil || validateVersion(state.XrayVersion) != nil || len(state.ArchiveDigest) != sha256.Size*2 || len(state.Files) != len(requiredBundleFiles) {
		return nil, errors.New("Xray bundle state is incomplete")
	}
	if _, err := hex.DecodeString(state.ArchiveDigest); err != nil {
		return nil, errors.New("Xray bundle archive digest is invalid")
	}
	if filepath.Base(target) != state.Version+"-"+state.ArchiveDigest {
		return nil, errors.New("Xray version target does not match bundle state")
	}
	for _, fileName := range requiredBundleFiles {
		want := state.Files[fileName]
		if len(want) != sha256.Size*2 {
			return nil, fmt.Errorf("Xray bundle digest for %s is invalid", fileName)
		}
		filePath := filepath.Join(path, fileName)
		wantMode := os.FileMode(stateFileMode)
		if fileName == "xray" {
			wantMode = 0o700
		}
		got, err := hashBundleFile(filePath, bundleEntryLimit(fileName), wantMode)
		if err != nil {
			return nil, fmt.Errorf("verify Xray bundle file %s: %w", fileName, err)
		}
		if got != want {
			return nil, fmt.Errorf("Xray bundle file %s digest mismatch", fileName)
		}
	}
	return &state, nil
}

func hashBundleFile(path string, limit int64, mode os.FileMode) (string, error) {
	pathInfo, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if !pathInfo.Mode().IsRegular() || pathInfo.Mode().Perm() != mode {
		return "", errors.New("file has unsafe permissions or type")
	}
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return "", err
	}
	if !os.SameFile(pathInfo, info) || !info.Mode().IsRegular() || info.Mode().Perm() != mode || info.Size() <= 0 || info.Size() > limit {
		return "", errors.New("file has unsafe permissions, type, or size")
	}
	hasher := sha256.New()
	written, err := io.Copy(hasher, io.LimitReader(file, limit+1))
	if err != nil {
		return "", err
	}
	if written != info.Size() || written > limit {
		return "", errors.New("file exceeds its size limit")
	}
	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func (m *Manager) ensureDirectory() error {
	if err := os.MkdirAll(m.directory, stateDirectoryMode); err != nil {
		return err
	}
	return os.Chmod(m.directory, stateDirectoryMode)
}

func (m *Manager) ensureDownloadDirectory() (string, error) {
	if err := m.ensureDirectory(); err != nil {
		return "", err
	}
	directory := filepath.Join(m.directory, downloadsName)
	if err := os.MkdirAll(directory, stateDirectoryMode); err != nil {
		return "", err
	}
	info, err := os.Lstat(directory)
	if err != nil {
		return "", err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Xray release download path is not a directory")
	}
	if err := os.Chmod(directory, stateDirectoryMode); err != nil {
		return "", err
	}
	return directory, nil
}

func (m *Manager) cleanupStaleDownloads() error {
	directory, err := m.ensureDownloadDirectory()
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("stale Xray release download %q is not a regular file", entry.Name())
		}
		if err := os.Remove(filepath.Join(directory, entry.Name())); err != nil {
			return err
		}
	}
	if len(entries) > 0 {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	return m.cleanupLegacyDownloads()
}

func (m *Manager) cleanupLegacyDownloads() error {
	if m.legacyTempDir == "" {
		return nil
	}
	paths, err := filepath.Glob(filepath.Join(m.legacyTempDir, legacyTempPattern))
	if err != nil {
		return err
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		stat, ok := info.Sys().(*syscall.Stat_t)
		if !ok || !info.Mode().IsRegular() || stat.Uid != uint32(os.Geteuid()) {
			continue
		}
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

func (m *Manager) loadApplied() (*AppliedState, error) {
	var state AppliedState
	if err := readJSONFile(m.appliedPath(), &state); err != nil {
		return nil, err
	}
	if err := validateAppliedState(&state); err != nil {
		return nil, err
	}
	return &state, nil
}

func (m *Manager) loadPending() (*pendingState, error) {
	var state pendingState
	if err := readJSONFile(m.pendingPath(), &state); err != nil {
		return nil, err
	}
	if state.CommandID == "" || validateVersion(state.Version) != nil || validateVersion(state.XrayVersion) != nil || len(state.ArchiveDigest) != sha256.Size*2 || validateTarget(state.Target) != nil {
		return nil, errors.New("pending Xray binary update is incomplete")
	}
	if state.PreviousTarget != "" && validateTarget(state.PreviousTarget) != nil {
		return nil, errors.New("pending Xray binary rollback target is invalid")
	}
	if state.PreviousApplied != nil {
		if state.PreviousTarget == "" || state.PreviousApplied.Target != state.PreviousTarget {
			return nil, errors.New("pending Xray binary rollback state does not match its target")
		}
		if err := validateAppliedState(state.PreviousApplied); err != nil {
			return nil, errors.New("pending Xray binary rollback state is invalid")
		}
	}
	return &state, nil
}

func validateAppliedState(state *AppliedState) error {
	if state == nil || validateVersion(state.Version) != nil || validateVersion(state.XrayVersion) != nil || len(state.ArchiveDigest) != sha256.Size*2 || validateTarget(state.Target) != nil {
		return errors.New("applied Xray binary state is incomplete")
	}
	if _, err := hex.DecodeString(state.ArchiveDigest); err != nil {
		return errors.New("applied Xray binary archive digest is invalid")
	}
	return nil
}

func (m *Manager) pruneVersions() error {
	directory := filepath.Join(m.directory, versionsName)
	entries, err := os.ReadDir(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	protected := make(map[string]bool)
	for _, path := range []string{m.currentPath(), m.previousPath()} {
		if target, err := os.Readlink(path); err == nil {
			if err := validateTarget(target); err != nil {
				return err
			}
			protected[filepath.Base(target)] = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if applied, err := m.loadApplied(); err == nil {
		protected[filepath.Base(applied.Target)] = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if pending, err := m.loadPending(); err == nil {
		protected[filepath.Base(pending.Target)] = true
		if pending.PreviousTarget != "" {
			protected[filepath.Base(pending.PreviousTarget)] = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	type installedVersion struct {
		name    string
		modTime time.Time
	}
	versions := make([]installedVersion, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		target := filepath.Join(versionsName, entry.Name())
		if err := m.verifyTarget(target); err != nil {
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
		if err := removeBundleDirectory(filepath.Join(directory, version.name)); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return syncDirectory(directory)
	}
	return nil
}

func validateTarget(target string) error {
	if target == "" || filepath.IsAbs(target) || filepath.Clean(target) != target {
		return errors.New("Xray version target is invalid")
	}
	parts := strings.Split(target, string(filepath.Separator))
	if len(parts) != 2 || parts[0] != versionsName || parts[1] == "" {
		return errors.New("Xray version target is outside the versions directory")
	}
	return nil
}

func removeBundleDirectory(path string) error {
	for _, name := range append(append([]string{}, requiredBundleFiles...), bundleStateName) {
		if err := os.Remove(filepath.Join(path, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func resultFromApplied(applied *AppliedState) Result {
	return Result{Version: applied.Version, Status: StatusInstalled, AppliedAt: applied.AppliedAt}
}

type updateError struct {
	code           string
	recoveryStatus string
	err            error
}

func (e *updateError) Error() string { return e.err.Error() }
func (e *updateError) Unwrap() error { return e.err }

func newUpdateError(code, recoveryStatus string, err error) error {
	return &updateError{code: code, recoveryStatus: recoveryStatus, err: err}
}

func ErrorDetails(err error) (string, string) {
	var classified *updateError
	if errors.As(err, &classified) {
		return classified.code, classified.recoveryStatus
	}
	return ErrorCodeUnclassified, RecoveryStatusUnknown
}

type limitedBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	written := len(p)
	remaining := b.limit - b.buffer.Len()
	if remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		_, _ = b.buffer.Write(p)
	}
	return written, nil
}

func (b *limitedBuffer) String() string { return b.buffer.String() }

func cleanError(prefix string, err error) string {
	message := strings.TrimSpace(prefix)
	if err != nil {
		if message != "" {
			message += ": "
		}
		message += strings.TrimSpace(err.Error())
	}
	if len(message) > 2048 {
		message = message[:2048]
	}
	return message
}

func readJSONFile(path string, value any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > maxStateBytes {
		return errors.New("Xray update state has an invalid size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("Xray update state contains trailing data")
	}
	return nil
}

func writeJSONAtomic(path string, value any, mode os.FileMode) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return writeAtomic(path, append(raw, '\n'), mode)
}

func (m *Manager) persistState(path string, value any, mode os.FileMode) error {
	if m.writeState != nil {
		return m.writeState(path, value, mode)
	}
	return writeJSONAtomic(path, value, mode)
}

func writeAtomic(path string, content []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, stateDirectoryMode); err != nil {
		return err
	}
	temporary, err := os.CreateTemp(directory, ".xray-update-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return err
	}
	if _, err := temporary.Write(content); err != nil {
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
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	return syncDirectory(directory)
}

func atomicSymlink(target, path string) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	temporary := fmt.Sprintf("%s.tmp-%d", path, time.Now().UnixNano())
	if err := os.Symlink(target, temporary); err != nil {
		return err
	}
	defer os.Remove(temporary)
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func removeAndSync(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func Directory(stateDirectory string) string {
	return filepath.Join(stateDirectory, directoryName)
}

func CurrentBinaryPath(stateDirectory string) string {
	return filepath.Join(Directory(stateDirectory), currentName, "xray")
}

func LoadAppliedState(stateDirectory string) (*AppliedState, error) {
	manager := &Manager{directory: Directory(stateDirectory)}
	return manager.Current()
}

func LoadFailureState(stateDirectory string) (*Result, error) {
	manager := &Manager{directory: Directory(stateDirectory)}
	var result Result
	if err := readJSONFile(manager.failedPath(), &result); err != nil {
		return nil, err
	}
	if validateVersion(result.Version) != nil || result.Status != StatusInstallFailed {
		return nil, errors.New("failed Xray binary update state is incomplete")
	}
	if result.ErrorCode == "" {
		result.ErrorCode = ErrorCodeUnclassified
	}
	if result.RecoveryStatus == "" {
		if result.RolledBack {
			result.RecoveryStatus = RecoveryStatusRolledBack
		} else {
			result.RecoveryStatus = RecoveryStatusUnknown
		}
	}
	return &result, nil
}

func (m *Manager) currentPath() string  { return filepath.Join(m.directory, currentName) }
func (m *Manager) previousPath() string { return filepath.Join(m.directory, previousName) }
func (m *Manager) appliedPath() string  { return filepath.Join(m.directory, appliedName) }
func (m *Manager) pendingPath() string  { return filepath.Join(m.directory, pendingName) }
func (m *Manager) failedPath() string   { return filepath.Join(m.directory, failedName) }
