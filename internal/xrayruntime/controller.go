package xrayruntime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/xraybinary"
)

const (
	defaultRestartTimeout = 20 * time.Second
	defaultStablePeriod   = 2 * time.Second
	defaultPollPeriod     = 100 * time.Millisecond
)

type ProcessController struct {
	stateDirectory string
	binaryPath     string
	restartTimeout time.Duration
	stablePeriod   time.Duration
	pollPeriod     time.Duration
}

func NewProcessController(stateDirectory, binaryPath string) *ProcessController {
	return &ProcessController{
		stateDirectory: stateDirectory,
		binaryPath:     binaryPath,
		restartTimeout: defaultRestartTimeout,
		stablePeriod:   defaultStablePeriod,
		pollPeriod:     defaultPollPeriod,
	}
}

func (c *ProcessController) Preflight() error {
	_, err := c.runningPID()
	return err
}

func (c *ProcessController) RestartAndWait(ctx context.Context) error {
	oldPID, err := c.runningPID()
	if err != nil {
		return err
	}
	if err := writeAtomic(RestartPath(c.stateDirectory), []byte(strconv.FormatInt(time.Now().UnixNano(), 10)+"\n"), configFileMode); err != nil {
		return fmt.Errorf("notify Xray supervisor: %w", err)
	}
	if oldPID > 0 {
		if err := signalProcess(oldPID, syscall.SIGTERM); err != nil {
			return fmt.Errorf("stop current Xray process: %w", err)
		}
	}
	return c.waitForReplacement(ctx, oldPID)
}

func (c *ProcessController) StopAndWait(ctx context.Context) error {
	pid, err := c.runningPID()
	if err != nil {
		return err
	}
	if pid == 0 {
		return nil
	}
	if err := signalProcess(pid, syscall.SIGTERM); err != nil {
		return fmt.Errorf("stop failed Xray process: %w", err)
	}
	checkCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	ticker := time.NewTicker(c.poll())
	defer ticker.Stop()
	for {
		running, err := c.runningPID()
		if err != nil {
			return err
		}
		if running == 0 {
			return nil
		}
		select {
		case <-checkCtx.Done():
			return errors.New("Xray process did not stop before the health deadline")
		case <-ticker.C:
		}
	}
}

func (c *ProcessController) waitForReplacement(ctx context.Context, oldPID int) error {
	checkCtx, cancel := context.WithTimeout(ctx, c.timeout())
	defer cancel()
	ticker := time.NewTicker(c.poll())
	defer ticker.Stop()
	var candidatePID int
	var stableSince time.Time
	for {
		pid, err := c.runningPID()
		if err != nil {
			return err
		}
		if pid > 0 && pid != oldPID {
			if pid != candidatePID {
				candidatePID = pid
				stableSince = time.Now()
			} else if time.Since(stableSince) >= c.stability() {
				return nil
			}
		} else {
			candidatePID = 0
			stableSince = time.Time{}
		}
		select {
		case <-checkCtx.Done():
			return errors.New("Xray process did not become stable before the health deadline")
		case <-ticker.C:
		}
	}
}

func (c *ProcessController) runningPID() (int, error) {
	raw, err := os.ReadFile(PIDPath(c.stateDirectory))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		return 0, fmt.Errorf("read Xray pid: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		return 0, errors.New("Xray pid file is invalid")
	}
	if err := syscall.Kill(pid, 0); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return 0, nil
		}
		if !errors.Is(err, syscall.EPERM) {
			return 0, err
		}
	}
	want, err := filepath.EvalSymlinks(xraybinary.ActivePath(c.stateDirectory, c.binaryPath))
	if err != nil {
		return 0, fmt.Errorf("resolve Xray binary: %w", err)
	}
	got, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return 0, nil
		}
		if !errors.Is(err, os.ErrPermission) {
			return 0, fmt.Errorf("inspect Xray process: %w", err)
		}
		matches, commandErr := processCommandMatches(pid, want)
		if commandErr != nil {
			if errors.Is(commandErr, os.ErrProcessDone) {
				return 0, nil
			}
			return 0, fmt.Errorf("inspect Xray process command: %w", commandErr)
		}
		if !matches {
			return 0, errors.New("Xray pid file points to a different executable")
		}
		return pid, nil
	}
	got = strings.TrimSuffix(got, " (deleted)")
	if filepath.Clean(got) != filepath.Clean(want) {
		return 0, errors.New("Xray pid file points to a different executable")
	}
	return pid, nil
}

func processCommandMatches(pid int, want string) (bool, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	command, _, _ := bytes.Cut(raw, []byte{0})
	if len(command) == 0 {
		return false, os.ErrProcessDone
	}
	resolved, err := filepath.EvalSymlinks(string(command))
	if err != nil {
		return false, err
	}
	return filepath.Clean(resolved) == filepath.Clean(want), nil
}

func signalProcess(pid int, signal syscall.Signal) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(signal); err != nil && !errors.Is(err, os.ErrProcessDone) && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (c *ProcessController) timeout() time.Duration {
	if c.restartTimeout <= 0 {
		return defaultRestartTimeout
	}
	return c.restartTimeout
}

func (c *ProcessController) stability() time.Duration {
	if c.stablePeriod <= 0 {
		return defaultStablePeriod
	}
	return c.stablePeriod
}

func (c *ProcessController) poll() time.Duration {
	if c.pollPeriod <= 0 {
		return defaultPollPeriod
	}
	return c.pollPeriod
}
