package xrayconfig

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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/xraybinary"
)

const (
	MaxConfigBytes    = 4 << 20
	validationTimeout = 15 * time.Second
	maxErrorBytes     = 2048
	StatusValidated   = "validated"
	StatusFailed      = "validation_failed"
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
	ValidatedAt   int64  `json:"validatedAt"`
}

func (r Result) Success() bool {
	return r.Status == StatusValidated
}

type Runner interface {
	Validate(context.Context, string, string) error
}

type ExecRunner struct {
	Timeout time.Duration
}

func (r ExecRunner) Validate(ctx context.Context, binaryPath, configPath string) error {
	timeout := r.Timeout
	if timeout <= 0 {
		timeout = validationTimeout
	}
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	command := exec.CommandContext(checkCtx, binaryPath, "run", "-test", "-config", configPath)
	command.Dir = filepath.Dir(binaryPath)
	err := command.Run()
	if errors.Is(checkCtx.Err(), context.DeadlineExceeded) {
		return errors.New("xray config validation timed out")
	}
	if err == nil {
		return nil
	}
	return fmt.Errorf("xray config validation failed: %s", cleanError(err.Error()))
}

type Manager struct {
	directory          string
	binaryPath         string
	binaryPathResolver func() string
	runner             Runner
	mu                 sync.Mutex
}

func NewManagedManager(stateDirectory, bootstrapPath string, runner Runner) *Manager {
	manager := NewManager(stateDirectory, bootstrapPath, runner)
	manager.binaryPathResolver = func() string {
		return xraybinary.ActivePath(stateDirectory, bootstrapPath)
	}
	return manager
}

func NewManager(stateDirectory, binaryPath string, runner Runner) *Manager {
	if runner == nil {
		runner = ExecRunner{}
	}
	return &Manager{
		directory:  filepath.Join(stateDirectory, "xray-config"),
		binaryPath: strings.TrimSpace(binaryPath),
		runner:     runner,
	}
}

func (m *Manager) Enabled() bool {
	return m != nil && m.binaryPath != ""
}

func (m *Manager) Validate(ctx context.Context, request Request) (Result, error) {
	if m == nil || !m.Enabled() {
		return Result{}, errors.New("xray binary path is not configured")
	}
	request.ConfigDigest = strings.ToLower(strings.TrimSpace(request.ConfigDigest))
	if err := validateRequest(request); err != nil {
		return Result{}, err
	}

	m.mu.Lock()
	defer m.mu.Unlock()

	previous, err := m.loadResult()
	if err == nil {
		switch {
		case request.ConfigVersion < previous.ConfigVersion:
			return Result{}, fmt.Errorf("config version %d is older than local version %d", request.ConfigVersion, previous.ConfigVersion)
		case request.ConfigVersion == previous.ConfigVersion && request.ConfigDigest != previous.ConfigDigest:
			return Result{}, fmt.Errorf("config version %d has a different digest", request.ConfigVersion)
		case request.ConfigVersion == previous.ConfigVersion:
			return previous, nil
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Result{}, fmt.Errorf("load config validation state: %w", err)
	}

	if err := os.MkdirAll(m.directory, 0o700); err != nil {
		return Result{}, fmt.Errorf("create xray config state directory: %w", err)
	}
	if err := os.Chmod(m.directory, 0o700); err != nil {
		return Result{}, fmt.Errorf("secure xray config state directory: %w", err)
	}
	candidatePath := filepath.Join(m.directory, "candidate.json")
	if err := writeAtomic(candidatePath, request.Config, 0o600); err != nil {
		return Result{}, fmt.Errorf("write candidate xray config: %w", err)
	}

	result := Result{
		ConfigVersion: request.ConfigVersion,
		ConfigDigest:  request.ConfigDigest,
		Status:        StatusValidated,
		ValidatedAt:   time.Now().Unix(),
	}
	binaryPath := m.binaryPath
	if m.binaryPathResolver != nil {
		binaryPath = m.binaryPathResolver()
	}
	if err := m.runner.Validate(ctx, binaryPath, candidatePath); err != nil {
		result.Status = StatusFailed
		result.Error = cleanError(err.Error())
	}
	if err := m.saveResult(result); err != nil {
		return Result{}, fmt.Errorf("persist config validation state: %w", err)
	}
	return result, nil
}

func validateRequest(request Request) error {
	if request.ConfigVersion == 0 {
		return errors.New("config version must be greater than zero")
	}
	if len(request.Config) == 0 || len(request.Config) > MaxConfigBytes {
		return fmt.Errorf("config must be between 1 and %d bytes", MaxConfigBytes)
	}
	if !json.Valid(request.Config) {
		return errors.New("config is not valid JSON")
	}
	decoded, err := hex.DecodeString(request.ConfigDigest)
	if err != nil || len(decoded) != sha256.Size {
		return errors.New("config digest must be a SHA-256 hex digest")
	}
	actual := sha256.Sum256(request.Config)
	if !bytes.Equal(actual[:], decoded) {
		return errors.New("config digest does not match the received config")
	}
	return nil
}

func (m *Manager) loadResult() (Result, error) {
	raw, err := os.ReadFile(filepath.Join(m.directory, "validation.json"))
	if err != nil {
		return Result{}, err
	}
	var result Result
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return Result{}, err
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Result{}, errors.New("multiple JSON values")
		}
		return Result{}, err
	}
	if result.ConfigVersion == 0 || result.ConfigDigest == "" || (result.Status != StatusValidated && result.Status != StatusFailed) {
		return Result{}, errors.New("invalid config validation state")
	}
	return result, nil
}

func (m *Manager) saveResult(result Result) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return writeAtomic(filepath.Join(m.directory, "validation.json"), raw, 0o600)
}

func writeAtomic(path string, content []byte, mode os.FileMode) error {
	directory := filepath.Dir(path)
	temporary, err := os.CreateTemp(directory, ".xui-agent-*")
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
	dir, err := os.Open(directory)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func cleanError(message string) string {
	message = strings.TrimSpace(message)
	if len(message) > maxErrorBytes {
		message = message[:maxErrorBytes]
	}
	return message
}
