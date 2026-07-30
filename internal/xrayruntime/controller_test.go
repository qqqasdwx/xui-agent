package xrayruntime

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessControllerRestartsManagedExecutable(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(Directory(state), stateDirectoryMode); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}
	binary := runtimeHelperBinary(t)
	oldProcess := startRuntimeHelper(t, binary)
	defer func() {
		if oldProcess.ProcessState == nil {
			_ = oldProcess.Process.Kill()
			_ = oldProcess.Wait()
		}
	}()
	if err := writeAtomic(PIDPath(state), []byte(fmt.Sprintf("%d\n", oldProcess.Process.Pid)), configFileMode); err != nil {
		t.Fatalf("write old pid: %v", err)
	}

	controller := NewProcessController(state, binary)
	controller.restartTimeout = 3 * time.Second
	controller.stablePeriod = 50 * time.Millisecond
	controller.pollPeriod = 10 * time.Millisecond

	replacement := make(chan *exec.Cmd, 1)
	workerError := make(chan error, 1)
	go func() {
		deadline := time.Now().Add(2 * time.Second)
		for {
			if _, err := os.Stat(RestartPath(state)); err == nil {
				break
			} else if !os.IsNotExist(err) {
				workerError <- err
				return
			}
			if time.Now().After(deadline) {
				workerError <- fmt.Errorf("restart notification was not written")
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		if err := oldProcess.Wait(); err != nil {
			if exit, ok := err.(*exec.ExitError); !ok || exit.ProcessState.Sys().(syscall.WaitStatus).Signal() != syscall.SIGTERM {
				workerError <- fmt.Errorf("old helper exit: %w", err)
				return
			}
		}
		next := startRuntimeHelperCommand(binary)
		if err := next.Start(); err != nil {
			workerError <- err
			return
		}
		if err := writeAtomic(PIDPath(state), []byte(fmt.Sprintf("%d\n", next.Process.Pid)), configFileMode); err != nil {
			_ = next.Process.Kill()
			_ = next.Wait()
			workerError <- err
			return
		}
		replacement <- next
	}()

	if err := controller.RestartAndWait(context.Background()); err != nil {
		t.Fatalf("RestartAndWait: %v", err)
	}
	select {
	case err := <-workerError:
		t.Fatalf("restart worker: %v", err)
	case next := <-replacement:
		defer func() {
			_ = next.Process.Signal(syscall.SIGTERM)
			_ = next.Wait()
		}()
		pid, err := controller.runningPID()
		if err != nil || pid != next.Process.Pid {
			t.Fatalf("running pid=%d err=%v, want %d", pid, err, next.Process.Pid)
		}
	case <-time.After(time.Second):
		t.Fatal("replacement helper was not reported")
	}
}

func TestProcessControllerRejectsPIDForDifferentExecutable(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(Directory(state), stateDirectoryMode); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}
	if err := writeAtomic(PIDPath(state), []byte(fmt.Sprintf("%d\n", os.Getpid())), configFileMode); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	controller := NewProcessController(state, "/bin/sh")
	if _, err := controller.runningPID(); err == nil {
		t.Fatal("pid for a different executable was accepted")
	}
}

func TestProcessControllerPreflightRejectsCorruptPID(t *testing.T) {
	state := t.TempDir()
	if err := os.MkdirAll(Directory(state), stateDirectoryMode); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}
	if err := writeAtomic(PIDPath(state), []byte("not-a-pid\n"), configFileMode); err != nil {
		t.Fatalf("write corrupt pid: %v", err)
	}
	controller := NewProcessController(state, "/bin/sh")
	if err := controller.Preflight(); err == nil || !strings.Contains(err.Error(), "pid file is invalid") {
		t.Fatalf("Preflight error = %v", err)
	}
}

func TestProcessCommandMatchesManagedExecutable(t *testing.T) {
	binary := runtimeHelperBinary(t)
	process := startRuntimeHelper(t, binary)
	defer func() {
		_ = process.Process.Kill()
		_ = process.Wait()
	}()
	matches, err := processCommandMatches(process.Process.Pid, binary)
	if err != nil || !matches {
		t.Fatalf("processCommandMatches=%v err=%v", matches, err)
	}
	matches, err = processCommandMatches(process.Process.Pid, "/bin/sh")
	if err != nil || matches {
		t.Fatalf("different command processCommandMatches=%v err=%v", matches, err)
	}
}

func runtimeHelperBinary(t *testing.T) string {
	t.Helper()
	binary, err := exec.LookPath("sleep")
	if err != nil {
		t.Fatalf("find runtime helper executable: %v", err)
	}
	binary, err = filepath.EvalSymlinks(binary)
	if err != nil {
		t.Fatalf("resolve runtime helper executable: %v", err)
	}
	return binary
}

func startRuntimeHelper(t *testing.T, binary string) *exec.Cmd {
	t.Helper()
	command := startRuntimeHelperCommand(binary)
	if err := command.Start(); err != nil {
		t.Fatalf("start runtime helper: %v", err)
	}
	deadline := time.Now().Add(2 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		matches, err := processCommandMatches(command.Process.Pid, binary)
		if err == nil && matches {
			return command
		}
		lastErr = err
		time.Sleep(time.Millisecond)
	}
	_ = command.Process.Kill()
	_ = command.Wait()
	t.Fatalf("runtime helper did not become ready: %v", lastErr)
	return nil
}

func startRuntimeHelperCommand(binary string) *exec.Cmd {
	return exec.Command(binary, "300")
}
