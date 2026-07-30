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
	"strings"
	"sync"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/xrayconfig"
)

const (
	StatusApplied     = "applied"
	StatusApplyFailed = "apply_failed"

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
)

type Request struct {
	ConfigVersion uint64
	ConfigDigest  string
	Config        json.RawMessage
}

type Result struct {
	ConfigVersion uint64 `json:"configVersion"`
	ConfigDigest  string `json:"configDigest"`
	Status        string `json:"status"`
	Error         string `json:"error,omitempty"`
	AppliedAt     int64  `json:"appliedAt,omitempty"`
	RolledBack    bool   `json:"rolledBack,omitempty"`
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
	CommandTarget  string `json:"commandTarget"`
	PreviousTarget string `json:"previousTarget,omitempty"`
	ConfigVersion  uint64 `json:"configVersion"`
	ConfigDigest   string `json:"configDigest"`
	StartedAt      int64  `json:"startedAt"`
}

type Controller interface {
	RestartAndWait(context.Context) error
	StopAndWait(context.Context) error
}

type Manager struct {
	directory  string
	validator  *xrayconfig.Manager
	controller Controller
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
		return Result{}, errors.New("managed Xray runtime is not configured")
	}
	request.ConfigDigest = strings.ToLower(strings.TrimSpace(request.ConfigDigest))
	validated, err := m.validator.Validate(ctx, xrayconfig.Request{
		ConfigVersion: request.ConfigVersion,
		ConfigDigest:  request.ConfigDigest,
		Config:        request.Config,
	})
	if err != nil {
		return Result{}, err
	}
	if !validated.Success() {
		return Result{
			ConfigVersion: validated.ConfigVersion,
			ConfigDigest:  validated.ConfigDigest,
			Status:        StatusApplyFailed,
			Error:         validated.Error,
		}, nil
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	if err := m.ensureDirectory(); err != nil {
		return Result{}, err
	}
	repair := false
	applied, err := m.loadApplied()
	if err == nil {
		switch {
		case request.ConfigVersion < applied.ConfigVersion:
			return Result{}, fmt.Errorf("config version %d is older than applied version %d", request.ConfigVersion, applied.ConfigVersion)
		case request.ConfigVersion == applied.ConfigVersion && request.ConfigDigest != applied.ConfigDigest:
			return Result{}, fmt.Errorf("applied config version %d has a different digest", request.ConfigVersion)
		case request.ConfigVersion == applied.ConfigVersion:
			if err := m.verifyApplied(applied); err == nil {
				return resultFromApplied(applied), nil
			}
			repair = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("load applied Xray config state: %w", err)
	}

	target, err := m.installVersion(request, repair)
	if err != nil {
		return Result{}, err
	}
	var previousTarget string
	if repair {
		previousTarget, err = m.previousTarget()
	} else {
		previousTarget, err = m.currentTarget()
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("read previous Xray config target: %w", err)
	}
	if applied != nil && previousTarget == "" && !repair {
		return Result{}, errors.New("applied Xray config has no current target")
	}
	pending := pendingState{
		CommandTarget:  target,
		PreviousTarget: previousTarget,
		ConfigVersion:  request.ConfigVersion,
		ConfigDigest:   request.ConfigDigest,
		StartedAt:      time.Now().Unix(),
	}
	if err := writeJSONAtomic(m.pendingPath(), pending, configFileMode); err != nil {
		return Result{}, fmt.Errorf("persist pending Xray config: %w", err)
	}
	if err := atomicSymlink(target, m.currentPath()); err != nil {
		_ = removeAndSync(m.pendingPath())
		return Result{}, fmt.Errorf("switch current Xray config: %w", err)
	}
	if err := syncDirectory(m.directory); err != nil {
		return Result{}, fmt.Errorf("sync current Xray config: %w", err)
	}

	if err := m.controller.RestartAndWait(ctx); err != nil {
		failure := cleanError(fmt.Sprintf("activate Xray config: %v", err))
		if rollbackErr := m.rollback(ctx, pending, failure); rollbackErr != nil {
			return Result{}, fmt.Errorf("%s; rollback failed: %w", failure, rollbackErr)
		}
		return Result{
			ConfigVersion: request.ConfigVersion,
			ConfigDigest:  request.ConfigDigest,
			Status:        StatusApplyFailed,
			Error:         failure,
			RolledBack:    true,
		}, nil
	}

	if previousTarget != "" {
		if err := atomicSymlink(previousTarget, m.previousPath()); err != nil {
			return Result{}, fmt.Errorf("record previous Xray config: %w", err)
		}
	}
	next := AppliedState{
		ConfigVersion: request.ConfigVersion,
		ConfigDigest:  request.ConfigDigest,
		Target:        target,
		AppliedAt:     time.Now().Unix(),
	}
	if err := writeJSONAtomic(m.appliedPath(), next, configFileMode); err != nil {
		return Result{}, fmt.Errorf("persist applied Xray config: %w", err)
	}
	if err := removeAndSync(m.pendingPath()); err != nil {
		return Result{}, fmt.Errorf("confirm applied Xray config: %w", err)
	}
	if err := removeAndSync(m.failedPath()); err != nil {
		return Result{}, fmt.Errorf("clear failed Xray config state: %w", err)
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
		if err := m.verifyApplied(applied); err != nil {
			return err
		}
		return removeAndSync(m.pendingPath())
	}
	if appliedErr != nil && !errors.Is(appliedErr, os.ErrNotExist) {
		return fmt.Errorf("load applied Xray config during recovery: %w", appliedErr)
	}
	return m.rollback(ctx, *pending, "unconfirmed Xray config was rolled back after agent restart")
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

func (m *Manager) rollback(ctx context.Context, pending pendingState, failure string) error {
	rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rollbackTimeout)
	defer cancel()

	if pending.PreviousTarget == "" {
		if err := removeAndSync(m.currentPath()); err != nil {
			return fmt.Errorf("remove failed initial Xray config: %w", err)
		}
		if err := m.controller.StopAndWait(rollbackCtx); err != nil {
			return err
		}
	} else {
		if err := validateTarget(pending.PreviousTarget); err != nil {
			return fmt.Errorf("invalid rollback target: %w", err)
		}
		if err := atomicSymlink(pending.PreviousTarget, m.currentPath()); err != nil {
			return fmt.Errorf("restore previous Xray config: %w", err)
		}
		if err := syncDirectory(m.directory); err != nil {
			return err
		}
		if err := m.controller.RestartAndWait(rollbackCtx); err != nil {
			return err
		}
	}
	failed := Result{
		ConfigVersion: pending.ConfigVersion,
		ConfigDigest:  pending.ConfigDigest,
		Status:        StatusApplyFailed,
		Error:         cleanError(failure),
		RolledBack:    true,
	}
	if err := writeJSONAtomic(m.failedPath(), failed, configFileMode); err != nil {
		return err
	}
	return removeAndSync(m.pendingPath())
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
	if applied == nil {
		return errors.New("applied Xray config state is missing")
	}
	current, err := m.currentTarget()
	if err != nil {
		return fmt.Errorf("read applied Xray config target: %w", err)
	}
	if current != applied.Target {
		return errors.New("current Xray config target differs from applied state")
	}
	raw, err := os.ReadFile(filepath.Join(m.directory, applied.Target))
	if err != nil {
		return fmt.Errorf("read applied Xray config: %w", err)
	}
	info, err := os.Lstat(filepath.Join(m.directory, applied.Target))
	if err != nil {
		return fmt.Errorf("inspect applied Xray config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != configFileMode {
		return errors.New("current Xray config has unsafe permissions or file type")
	}
	digest := sha256.Sum256(raw)
	if hex.EncodeToString(digest[:]) != applied.ConfigDigest {
		return errors.New("current Xray config digest differs from applied state")
	}
	return nil
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
	if applied.ConfigVersion == 0 || len(applied.ConfigDigest) != sha256.Size*2 || applied.AppliedAt <= 0 {
		return nil, errors.New("applied Xray config state is incomplete")
	}
	if err := validateTarget(applied.Target); err != nil {
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
	if pending.PreviousTarget != "" {
		if err := validateTarget(pending.PreviousTarget); err != nil {
			return nil, err
		}
	}
	return &pending, nil
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

func (m *Manager) currentPath() string  { return filepath.Join(m.directory, currentName) }
func (m *Manager) previousPath() string { return filepath.Join(m.directory, previousName) }
func (m *Manager) appliedPath() string  { return filepath.Join(m.directory, appliedName) }
func (m *Manager) pendingPath() string  { return filepath.Join(m.directory, pendingName) }
func (m *Manager) failedPath() string   { return filepath.Join(m.directory, failedName) }
