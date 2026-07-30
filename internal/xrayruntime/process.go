package xrayruntime

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/xraybinary"
)

const processStopTimeout = 10 * time.Second

func RunManagedProcess(ctx context.Context, stateDirectory, binaryPath string) error {
	binaryPath = xraybinary.ActivePath(stateDirectory, binaryPath)
	target, err := os.Readlink(CurrentConfigPath(stateDirectory))
	if err != nil {
		return fmt.Errorf("read current Xray config: %w", err)
	}
	if err := validateTarget(target); err != nil {
		return err
	}
	configPath := filepath.Join(Directory(stateDirectory), target)
	info, err := os.Stat(configPath)
	if err != nil {
		return fmt.Errorf("inspect current Xray config: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != configFileMode {
		return errors.New("current Xray config is not a secure regular file")
	}
	binaryInfo, err := os.Stat(binaryPath)
	if err != nil {
		return fmt.Errorf("inspect Xray binary: %w", err)
	}
	if !binaryInfo.Mode().IsRegular() || binaryInfo.Mode().Perm()&0o111 == 0 {
		return errors.New("Xray binary is not executable")
	}

	command := exec.Command(binaryPath, "run", "-config", configPath)
	command.Dir = filepath.Dir(binaryPath)
	command.Env = append(os.Environ(), "XRAY_LOCATION_ASSET="+command.Dir)
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return fmt.Errorf("start Xray: %w", err)
	}
	pidPath := PIDPath(stateDirectory)
	if err := writeAtomic(pidPath, []byte(fmt.Sprintf("%d\n", command.Process.Pid)), configFileMode); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return fmt.Errorf("write Xray pid: %w", err)
	}
	defer removeAndSync(pidPath)

	done := make(chan error, 1)
	go func() { done <- command.Wait() }()
	select {
	case err := <-done:
		if err == nil {
			return errors.New("Xray exited unexpectedly")
		}
		return fmt.Errorf("Xray exited: %w", err)
	case <-ctx.Done():
		_ = command.Process.Signal(syscall.SIGTERM)
		select {
		case <-done:
			return nil
		case <-time.After(processStopTimeout):
			_ = command.Process.Kill()
			<-done
			return errors.New("Xray required a forced stop")
		}
	}
}
