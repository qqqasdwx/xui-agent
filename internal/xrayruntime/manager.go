package xrayruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/xrayconfig"
)

const (
	StatusApplied     = "applied"
	StatusApplyFailed = "apply_failed"

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

	directoryName      = "xray-config"
	versionsName       = "versions"
	currentName        = "current.json"
	previousName       = "previous.json"
	appliedName        = "applied.json"
	pendingName        = "apply-pending.json"
	failedName         = "apply-failed.json"
	pidName            = "xray.pid"
	restartName        = "restart"
	maxStateBytes      = 64 << 10
	configFileMode     = 0o600
	stateDirectoryMode = 0o700
	rollbackTimeout    = 30 * time.Second
	retainedVersions   = 5
)

type Request struct {
	ConfigVersion uint64
	ConfigDigest  string
	Config        json.RawMessage
}

type Result struct {
	ConfigVersion  uint64 `json:"configVersion"`
	ConfigDigest   string `json:"configDigest"`
	Status         string `json:"status"`
	Error          string `json:"error,omitempty"`
	ErrorCode      string `json:"errorCode,omitempty"`
	RecoveryStatus string `json:"recoveryStatus,omitempty"`
	AppliedAt      int64  `json:"appliedAt,omitempty"`
	RolledBack     bool   `json:"rolledBack,omitempty"`
}

func (r Result) Success() bool {
	return r.Status == StatusApplied
}

type AppliedState struct {
	ConfigVersion uint64 `json:"configVersion"`
	ConfigDigest  string `json:"configDigest"`
	Target        string `json:"target"`
	AppliedAt     int64  `json:"appliedAt"`
}

type pendingState struct {
	CommandTarget   string        `json:"commandTarget"`
	PreviousTarget  string        `json:"previousTarget,omitempty"`
	PreviousApplied *AppliedState `json:"previousApplied,omitempty"`
	ConfigVersion   uint64        `json:"configVersion"`
	ConfigDigest    string        `json:"configDigest"`
	StartedAt       int64         `json:"startedAt"`
}

type Controller interface {
	RestartAndWait(context.Context) error
	StopAndWait(context.Context) error
}

type preflightController interface {
	Preflight() error
}

type Manager struct {
	directory  string
	validator  *xrayconfig.Manager
	controller Controller
	writeState func(string, any, os.FileMode) error
	mu         sync.Mutex
}

func NewManager(stateDirectory string, validator *xrayconfig.Manager, controller Controller) *Manager {
	return &Manager{
		directory:  filepath.Join(stateDirectory, directoryName),
		validator:  validator,
		controller: controller,
	}
}

func (m *Manager) Enabled() bool {
	return m != nil && m.validator != nil && m.validator.Enabled() && m.controller != nil
}

func (m *Manager) Apply(ctx context.Context, request Request) (Result, error) {
	if !m.Enabled() {
		return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, errors.New("managed Xray runtime is not configured"))
	}
	request.ConfigDigest = strings.ToLower(strings.TrimSpace(request.ConfigDigest))
	validated, err := m.validator.Validate(ctx, xrayconfig.Request{
		ConfigVersion: request.ConfigVersion,
		ConfigDigest:  request.ConfigDigest,
		Config:        request.Config,
	})
	if err != nil {
		return Result{}, newApplyError(ErrorCodeValidationFailed, RecoveryStatusNotRequired, err)
	}
	if !validated.Success() {
		return Result{
			ConfigVersion:  validated.ConfigVersion,
			ConfigDigest:   validated.ConfigDigest,
			Status:         StatusApplyFailed,
			Error:          validated.Error,
			ErrorCode:      ErrorCodeValidationFailed,
			RecoveryStatus: RecoveryStatusNotRequired,
		}, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureDirectory(); err != nil {
		return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, err)
	}
	repair := false
	var previousTarget string
	var previousApplied *AppliedState
	applied, err := m.loadApplied()
	if err == nil {
		switch {
		case request.ConfigVersion < applied.ConfigVersion:
			return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
				fmt.Errorf("config version %d is older than applied version %d", request.ConfigVersion, applied.ConfigVersion))
		case request.ConfigVersion == applied.ConfigVersion && request.ConfigDigest != applied.ConfigDigest:
			return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
				fmt.Errorf("applied config version %d has a different digest", request.ConfigVersion))
		case request.ConfigVersion == applied.ConfigVersion:
			if err := m.verifyApplied(applied); err == nil {
				if err := m.confirmApplied(applied); err != nil {
					return Result{}, newApplyError(ErrorCodePersistenceFailed, RecoveryStatusNotRequired,
						fmt.Errorf("confirm applied Xray config state: %w", err))
				}
				if err := m.pruneVersions(); err != nil {
					return Result{}, newApplyError(ErrorCodePersistenceFailed, RecoveryStatusNotRequired,
						fmt.Errorf("garbage collect Xray config versions: %w", err))
				}
				return resultFromApplied(applied), nil
			}
			repair = true
		case request.ConfigVersion > applied.ConfigVersion:
			if err := m.verifyConfigTarget(applied.Target, applied.ConfigDigest); err != nil {
				return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
					fmt.Errorf("verify rollback Xray config: %w", err))
			}
			previousTarget = applied.Target
			previousApplied = applied
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
			fmt.Errorf("load applied Xray config state: %w", err))
	}
	if applied == nil {
		if _, err := os.Lstat(m.currentPath()); err == nil {
			return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
				errors.New("current Xray config exists without applied state"))
		} else if !errors.Is(err, os.ErrNotExist) {
			return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
				fmt.Errorf("inspect untracked current Xray config: %w", err))
		}
	}
	if repair {
		previousTarget, err = m.previousTarget()
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
				fmt.Errorf("read repair rollback Xray config target: %w", err))
		}
		if previousTarget != "" {
			digest, err := configTargetDigest(previousTarget)
			if err != nil {
				return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
					fmt.Errorf("read repair rollback Xray config digest: %w", err))
			}
			if err := m.verifyConfigTarget(previousTarget, digest); err != nil {
				return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
					fmt.Errorf("verify repair rollback Xray config: %w", err))
			}
			previousApplied, err = m.appliedStateForTarget(previousTarget)
			if err != nil {
				return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
					fmt.Errorf("read repair rollback Xray config state: %w", err))
			}
		}
	}
	if controller, ok := m.controller.(preflightController); ok {
		if err := controller.Preflight(); err != nil {
			return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
				fmt.Errorf("preflight managed Xray process: %w", err))
		}
	}
	if _, err := m.loadPending(); err == nil {
		return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusUnknown,
			errors.New("another Xray config apply is pending"))
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusUnknown,
			fmt.Errorf("load pending Xray config before apply: %w", err))
	}
	if err := m.pruneVersions(); err != nil {
		return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
			fmt.Errorf("garbage collect Xray config versions: %w", err))
	}

	target, err := m.installVersion(request, repair)
	if err != nil {
		return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired, err)
	}
	pending := pendingState{
		CommandTarget:   target,
		PreviousTarget:  previousTarget,
		PreviousApplied: previousApplied,
		ConfigVersion:   request.ConfigVersion,
		ConfigDigest:    request.ConfigDigest,
		StartedAt:       time.Now().Unix(),
	}
	if err := m.persistState(m.pendingPath(), pending, configFileMode); err != nil {
		return Result{}, newApplyError(ErrorCodePreparationFailed, RecoveryStatusNotRequired,
			fmt.Errorf("persist pending Xray config: %w", err))
	}
	if err := atomicSymlink(target, m.currentPath()); err != nil {
		recoveryStatus := RecoveryStatusNotRequired
		failure := fmt.Sprintf("switch current Xray config: %v", err)
		if cleanupErr := removeAndSync(m.pendingPath()); cleanupErr != nil {
			recoveryStatus = RecoveryStatusUnknown
			failure = fmt.Sprintf("%s; clear pending state: %v", failure, cleanupErr)
		}
		return Result{}, newApplyError(ErrorCodePreparationFailed, recoveryStatus, errors.New(cleanError(failure)))
	}
	if err := syncDirectory(m.directory); err != nil {
		failure := cleanError(fmt.Sprintf("sync current Xray config: %v", err))
		return m.rollbackResult(ctx, pending, failure, ErrorCodePersistenceFailed)
	}

	if err := m.controller.RestartAndWait(ctx); err != nil {
		failure := cleanError(fmt.Sprintf("activate Xray config: %v", err))
		return m.rollbackResult(ctx, pending, failure, ErrorCodeActivationFailed)
	}

	if previousTarget != "" {
		if err := atomicSymlink(previousTarget, m.previousPath()); err != nil {
			failure := cleanError(fmt.Sprintf("record previous Xray config: %v", err))
			return m.rollbackResult(ctx, pending, failure, ErrorCodePersistenceFailed)
		}
	}
	next := AppliedState{
		ConfigVersion: request.ConfigVersion,
		ConfigDigest:  request.ConfigDigest,
		Target:        target,
		AppliedAt:     time.Now().Unix(),
	}
	if err := m.persistState(m.appliedPath(), next, configFileMode); err != nil {
		failure := cleanError(fmt.Sprintf("persist applied Xray config: %v", err))
		return m.rollbackResult(ctx, pending, failure, ErrorCodePersistenceFailed)
	}
	if err := removeAndSync(m.pendingPath()); err != nil {
		return Result{}, newApplyError(ErrorCodePersistenceFailed, RecoveryStatusUnknown,
			fmt.Errorf("confirm applied Xray config: %w", err))
	}
	if err := removeAndSync(m.failedPath()); err != nil {
		return Result{}, newApplyError(ErrorCodePersistenceFailed, RecoveryStatusUnknown,
			fmt.Errorf("clear failed Xray config state: %w", err))
	}
	if err := m.pruneVersions(); err != nil {
		return Result{}, newApplyError(ErrorCodePersistenceFailed, RecoveryStatusNotRequired,
			fmt.Errorf("garbage collect Xray config versions: %w", err))
	}
	return resultFromApplied(&next), nil
}

func (m *Manager) Recover(ctx context.Context) error {
	if !m.Enabled() {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()

	pending, err := m.loadPending()
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("load pending Xray config: %w", err)
	}
	applied, appliedErr := m.loadApplied()
	if appliedErr == nil && applied.Target == pending.CommandTarget && applied.ConfigVersion == pending.ConfigVersion && applied.ConfigDigest == pending.ConfigDigest {
		if err := m.verifyApplied(applied); err == nil {
			return m.confirmApplied(applied)
		}
	}
	if appliedErr != nil && !errors.Is(appliedErr, os.ErrNotExist) {
		return fmt.Errorf("load applied Xray config during recovery: %w", appliedErr)
	}
	_, err = m.rollbackResult(ctx, *pending, "unconfirmed Xray config was rolled back after agent restart", ErrorCodePreparationFailed)
	return err
}

func (m *Manager) confirmApplied(applied *AppliedState) error {
	pending, err := m.loadPending()
	if err == nil {
		if pending.CommandTarget != applied.Target || pending.ConfigVersion != applied.ConfigVersion || pending.ConfigDigest != applied.ConfigDigest {
			return errors.New("pending Xray config does not match applied state")
		}
		if err := removeAndSync(m.pendingPath()); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return removeAndSync(m.failedPath())
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
		return nil, err
	}
	return applied, nil
}

func (m *Manager) rollback(ctx context.Context, pending pendingState, failure, errorCode string) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	if pending.PreviousTarget == "" {
		if err := removeAndSync(m.currentPath()); err != nil {
			return fmt.Errorf("remove failed initial Xray config: %w", err)
		}
		if err := removeAndSync(m.appliedPath()); err != nil {
			return fmt.Errorf("remove failed initial Xray config state: %w", err)
		}
		if err := m.controller.StopAndWait(rollbackCtx); err != nil {
			return err
		}
	} else {
		previousApplied, err := m.rollbackAppliedState(pending)
		if err != nil {
			return fmt.Errorf("invalid rollback target: %w", err)
		}
		if err := atomicSymlink(pending.PreviousTarget, m.currentPath()); err != nil {
			return fmt.Errorf("restore previous Xray config: %w", err)
		}
		if err := m.persistState(m.appliedPath(), previousApplied, configFileMode); err != nil {
			return fmt.Errorf("restore previous Xray config state: %w", err)
		}
		if err := syncDirectory(m.directory); err != nil {
			return err
		}
		if err := m.controller.RestartAndWait(rollbackCtx); err != nil {
			return err
		}
	}
	failed := Result{
		ConfigVersion:  pending.ConfigVersion,
		ConfigDigest:   pending.ConfigDigest,
		Status:         StatusApplyFailed,
		Error:          cleanError(failure),
		ErrorCode:      errorCode,
		RecoveryStatus: RecoveryStatusRolledBack,
		RolledBack:     true,
	}
	if err := m.persistState(m.failedPath(), failed, configFileMode); err != nil {
		return err
	}
	return removeAndSync(m.pendingPath())
}

func (m *Manager) rollbackResult(ctx context.Context, pending pendingState, failure, errorCode string) (Result, error) {
	if err := m.rollback(ctx, pending, failure, errorCode); err != nil {
		combined := cleanError(fmt.Sprintf("%s; rollback failed: %v", failure, err))
		failed := Result{
			ConfigVersion: pending.ConfigVersion, ConfigDigest: pending.ConfigDigest,
			Status: StatusApplyFailed, Error: combined, ErrorCode: errorCode,
			RecoveryStatus: RecoveryStatusFailed,
		}
		if persistErr := m.persistState(m.failedPath(), failed, configFileMode); persistErr != nil {
			combined = cleanError(fmt.Sprintf("%s; persist recovery failure: %v", combined, persistErr))
		}
		return Result{}, newApplyError(ErrorCodeRecoveryFailed, RecoveryStatusFailed, errors.New(combined))
	}
	return Result{
		ConfigVersion:  pending.ConfigVersion,
		ConfigDigest:   pending.ConfigDigest,
		Status:         StatusApplyFailed,
		Error:          failure,
		ErrorCode:      errorCode,
		RecoveryStatus: RecoveryStatusRolledBack,
		RolledBack:     true,
	}, nil
}

func (m *Manager) installVersion(request Request, repair bool) (string, error) {
	versionsDirectory := filepath.Join(m.directory, versionsName)
	if err := os.MkdirAll(versionsDirectory, stateDirectoryMode); err != nil {
		return "", fmt.Errorf("create Xray config versions directory: %w", err)
	}
	if err := os.Chmod(versionsDirectory, stateDirectoryMode); err != nil {
		return "", fmt.Errorf("secure Xray config versions directory: %w", err)
	}
	name := fmt.Sprintf("%020d-%s.json", request.ConfigVersion, request.ConfigDigest)
	target := filepath.Join(versionsName, name)
	path := filepath.Join(m.directory, target)
	if raw, err := os.ReadFile(path); err == nil {
		digest := sha256.Sum256(raw)
		info, err := os.Lstat(path)
		if err != nil {
			return "", err
		}
		contentMatches := bytes.Equal(digest[:], mustDecodeDigest(request.ConfigDigest)) && bytes.Equal(raw, request.Config)
		permissionsMatch := info.Mode().IsRegular() && info.Mode().Perm() == configFileMode
		if contentMatches && permissionsMatch {
			return target, nil
		}
		if !repair {
			if !contentMatches {
				return "", errors.New("existing Xray config version content does not match")
			}
			return "", errors.New("existing Xray config version has unsafe permissions")
		}
		return m.writeRepairVersion(request)
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	if repair {
		return m.writeRepairVersion(request)
	}
	if err := writeAtomic(path, request.Config, configFileMode); err != nil {
		return "", fmt.Errorf("write versioned Xray config: %w", err)
	}
	return target, nil
}

func (m *Manager) writeRepairVersion(request Request) (string, error) {
	name := fmt.Sprintf("%020d-%s-repair-%d.json", request.ConfigVersion, request.ConfigDigest, time.Now().UnixNano())
	target := filepath.Join(versionsName, name)
	if err := writeAtomic(filepath.Join(m.directory, target), request.Config, configFileMode); err != nil {
		return "", fmt.Errorf("write repaired Xray config: %w", err)
	}
	return target, nil
}

func (m *Manager) verifyApplied(applied *AppliedState) error {
	current, err := m.currentTarget()
	if err != nil {
		return fmt.Errorf("read applied Xray config target: %w", err)
	}
	if current != applied.Target {
		return errors.New("current Xray config target differs from applied state")
	}
	return m.verifyAppliedTarget(applied)
}

func (m *Manager) verifyAppliedTarget(applied *AppliedState) error {
	if err := validateAppliedState(applied); err != nil {
		return err
	}
	targetVersion, targetDigest, err := configTargetMetadata(applied.Target)
	if err != nil {
		return err
	}
	if targetVersion != applied.ConfigVersion || targetDigest != applied.ConfigDigest {
		return errors.New("applied Xray config metadata differs from its immutable target")
	}
	return m.verifyConfigTarget(applied.Target, applied.ConfigDigest)
}

func (m *Manager) appliedStateForTarget(target string) (*AppliedState, error) {
	version, digest, err := configTargetMetadata(target)
	if err != nil {
		return nil, err
	}
	if err := m.verifyConfigTarget(target, digest); err != nil {
		return nil, err
	}
	info, err := os.Stat(filepath.Join(m.directory, target))
	if err != nil {
		return nil, err
	}
	appliedAt := info.ModTime().Unix()
	if appliedAt <= 0 {
		appliedAt = 1
	}
	return &AppliedState{ConfigVersion: version, ConfigDigest: digest, Target: target, AppliedAt: appliedAt}, nil
}

func (m *Manager) rollbackAppliedState(pending pendingState) (*AppliedState, error) {
	if pending.PreviousApplied != nil {
		if pending.PreviousApplied.Target != pending.PreviousTarget {
			return nil, errors.New("previous applied Xray config target does not match pending rollback target")
		}
		if err := m.verifyAppliedTarget(pending.PreviousApplied); err != nil {
			return nil, err
		}
		return pending.PreviousApplied, nil
	}
	return m.appliedStateForTarget(pending.PreviousTarget)
}

func (m *Manager) verifyConfigTarget(target, expectedDigest string) error {
	if err := validateTarget(target); err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(m.directory, target))
	if err != nil {
		return fmt.Errorf("read Xray config target: %w", err)
	}
	info, err := os.Lstat(filepath.Join(m.directory, target))
	if err != nil {
		return fmt.Errorf("inspect Xray config target: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != configFileMode {
		return errors.New("Xray config target has unsafe permissions or file type")
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != expectedDigest {
		return errors.New("Xray config target digest does not match expected state")
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
	protected, err := m.protectedTargets()
	if err != nil {
		return err
	}
	type versionFile struct {
		name    string
		modTime time.Time
	}
	versions := make([]versionFile, 0, len(entries))
	for _, entry := range entries {
		if _, err := configTargetDigest(filepath.Join(versionsName, entry.Name())); err != nil {
			continue
		}
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() {
			continue
		}
		versions = append(versions, versionFile{name: entry.Name(), modTime: info.ModTime()})
	}
	sort.Slice(versions, func(i, j int) bool {
		if versions[i].modTime.Equal(versions[j].modTime) {
			return versions[i].name > versions[j].name
		}
		return versions[i].modTime.After(versions[j].modTime)
	})
	removed := false
	for index, version := range versions {
		target := filepath.Join(versionsName, version.name)
		if index < retainedVersions || protected[target] {
			continue
		}
		if err := os.Remove(filepath.Join(directory, version.name)); err != nil {
			return err
		}
		removed = true
	}
	if removed {
		return syncDirectory(directory)
	}
	return nil
}

func (m *Manager) protectedTargets() (map[string]bool, error) {
	protected := make(map[string]bool)
	for _, path := range []string{m.currentPath(), m.previousPath()} {
		target, err := os.Readlink(path)
		if err == nil {
			if err := validateTarget(target); err != nil {
				return nil, err
			}
			protected[target] = true
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	if applied, err := m.loadApplied(); err == nil {
		protected[applied.Target] = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if pending, err := m.loadPending(); err == nil {
		protected[pending.CommandTarget] = true
		if pending.PreviousTarget != "" {
			protected[pending.PreviousTarget] = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return protected, nil
}

func (m *Manager) ensureDirectory() error {
	if err := os.MkdirAll(m.directory, stateDirectoryMode); err != nil {
		return fmt.Errorf("create Xray runtime directory: %w", err)
	}
	if err := os.Chmod(m.directory, stateDirectoryMode); err != nil {
		return fmt.Errorf("secure Xray runtime directory: %w", err)
	}
	return nil
}

func (m *Manager) loadApplied() (*AppliedState, error) {
	var applied AppliedState
	if err := readJSONFile(m.appliedPath(), &applied); err != nil {
		return nil, err
	}
	if err := validateAppliedState(&applied); err != nil {
		return nil, err
	}
	return &applied, nil
}

func (m *Manager) loadPending() (*pendingState, error) {
	var pending pendingState
	if err := readJSONFile(m.pendingPath(), &pending); err != nil {
		return nil, err
	}
	if pending.ConfigVersion == 0 || len(pending.ConfigDigest) != sha256.Size*2 || pending.StartedAt <= 0 {
		return nil, errors.New("pending Xray config state is incomplete")
	}
	if err := validateTarget(pending.CommandTarget); err != nil {
		return nil, err
	}
	targetVersion, targetDigest, err := configTargetMetadata(pending.CommandTarget)
	if err != nil {
		return nil, err
	}
	if targetVersion != pending.ConfigVersion || targetDigest != pending.ConfigDigest {
		return nil, errors.New("pending Xray config metadata differs from its immutable target")
	}
	if pending.PreviousTarget != "" {
		if err := validateTarget(pending.PreviousTarget); err != nil {
			return nil, err
		}
	}
	if pending.PreviousApplied != nil {
		if pending.PreviousTarget == "" || pending.PreviousApplied.Target != pending.PreviousTarget {
			return nil, errors.New("pending Xray config rollback state does not match its target")
		}
		if err := validateAppliedState(pending.PreviousApplied); err != nil {
			return nil, errors.New("pending Xray config rollback state is invalid")
		}
	}
	return &pending, nil
}

func validateAppliedState(applied *AppliedState) error {
	if applied == nil || applied.ConfigVersion == 0 || len(applied.ConfigDigest) != sha256.Size*2 || applied.AppliedAt <= 0 {
		return errors.New("applied Xray config state is incomplete")
	}
	if _, err := hex.DecodeString(applied.ConfigDigest); err != nil {
		return errors.New("applied Xray config digest is invalid")
	}
	return validateTarget(applied.Target)
}

func (m *Manager) currentTarget() (string, error) {
	target, err := os.Readlink(m.currentPath())
	if err != nil {
		return "", err
	}
	if err := validateTarget(target); err != nil {
		return "", err
	}
	return target, nil
}

func (m *Manager) previousTarget() (string, error) {
	target, err := os.Readlink(m.previousPath())
	if err != nil {
		return "", err
	}
	if err := validateTarget(target); err != nil {
		return "", err
	}
	return target, nil
}

func validateTarget(target string) error {
	if target == "" || filepath.IsAbs(target) || filepath.Clean(target) != target {
		return errors.New("Xray config target is invalid")
	}
	prefix := versionsName + string(filepath.Separator)
	if !strings.HasPrefix(target, prefix) || strings.Contains(strings.TrimPrefix(target, prefix), string(filepath.Separator)) {
		return errors.New("Xray config target is outside the versions directory")
	}
	return nil
}

func configTargetDigest(target string) (string, error) {
	_, digest, err := configTargetMetadata(target)
	return digest, err
}

func configTargetMetadata(target string) (uint64, string, error) {
	if err := validateTarget(target); err != nil {
		return 0, "", err
	}
	name := strings.TrimSuffix(filepath.Base(target), ".json")
	parts := strings.Split(name, "-")
	if len(parts) < 2 || len(parts[0]) != 20 || len(parts[1]) != sha256.Size*2 {
		return 0, "", errors.New("Xray config target name has no valid metadata")
	}
	version, err := strconv.ParseUint(parts[0], 10, 64)
	if err != nil || version == 0 {
		return 0, "", errors.New("Xray config target name has no valid version")
	}
	if _, err := hex.DecodeString(parts[1]); err != nil {
		return 0, "", errors.New("Xray config target name has no valid digest")
	}
	return version, strings.ToLower(parts[1]), nil
}

func readJSONFile(path string, value any) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if len(raw) == 0 || len(raw) > maxStateBytes {
		return errors.New("Xray config state has an invalid size")
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("Xray config state contains trailing data")
	}
	return nil
}

func resultFromApplied(applied *AppliedState) Result {
	return Result{
		ConfigVersion: applied.ConfigVersion,
		ConfigDigest:  applied.ConfigDigest,
		Status:        StatusApplied,
		AppliedAt:     applied.AppliedAt,
	}
}

func mustDecodeDigest(value string) []byte {
	raw, _ := hex.DecodeString(value)
	return raw
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
	temporary, err := os.CreateTemp(directory, ".xui-agent-runtime-*")
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
	return os.Rename(temporary, path)
}

func removeAndSync(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	directory := filepath.Dir(path)
	if _, err := os.Stat(directory); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return syncDirectory(directory)
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}

func cleanError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > 2048 {
		message = message[:2048]
	}
	return message
}

type applyError struct {
	code           string
	recoveryStatus string
	err            error
}

func (e *applyError) Error() string { return e.err.Error() }
func (e *applyError) Unwrap() error { return e.err }

func newApplyError(code, recoveryStatus string, err error) error {
	return &applyError{code: code, recoveryStatus: recoveryStatus, err: err}
}

func ErrorDetails(err error) (string, string) {
	var classified *applyError
	if errors.As(err, &classified) {
		return classified.code, classified.recoveryStatus
	}
	return ErrorCodeUnclassified, RecoveryStatusUnknown
}

func Directory(stateDirectory string) string {
	return filepath.Join(stateDirectory, directoryName)
}

func CurrentConfigPath(stateDirectory string) string {
	return filepath.Join(Directory(stateDirectory), currentName)
}

func PIDPath(stateDirectory string) string {
	return filepath.Join(Directory(stateDirectory), pidName)
}

func RestartPath(stateDirectory string) string {
	return filepath.Join(Directory(stateDirectory), restartName)
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
	if result.Status != StatusApplyFailed || result.ConfigVersion == 0 || len(result.ConfigDigest) != sha256.Size*2 {
		return nil, errors.New("failed Xray config state is incomplete")
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
