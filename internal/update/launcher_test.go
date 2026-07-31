package update

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLauncherRollsBackAFailedPendingVersion(t *testing.T) {
	state := t.TempDir()
	oldDirectory := filepath.Join(state, "versions", "old")
	newDirectory := filepath.Join(state, "versions", "new")
	if err := os.MkdirAll(oldDirectory, 0o700); err != nil {
		t.Fatalf("create old directory: %v", err)
	}
	if err := os.MkdirAll(newDirectory, 0o700); err != nil {
		t.Fatalf("create new directory: %v", err)
	}
	oldBinary := filepath.Join(oldDirectory, "xui-agent")
	newBinary := filepath.Join(newDirectory, "xui-agent")
	if err := os.WriteFile(oldBinary, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatalf("write old binary: %v", err)
	}
	if err := os.WriteFile(newBinary, []byte("#!/bin/sh\nexit 1\n"), 0o755); err != nil {
		t.Fatalf("write new binary: %v", err)
	}
	if err := os.Symlink("versions/new/xui-agent", filepath.Join(state, "current")); err != nil {
		t.Fatalf("create current: %v", err)
	}
	if err := os.Symlink("versions/old/xui-agent", filepath.Join(state, "previous")); err != nil {
		t.Fatalf("create previous: %v", err)
	}
	pending := Pending{
		CommandID: "command", PreviousTarget: "versions/old/xui-agent",
		TargetTarget: "versions/new/xui-agent", TargetVersion: "new",
	}
	if err := writeJSONAtomic(filepath.Join(state, pendingFilename), pending, 0o600); err != nil {
		t.Fatalf("write pending state: %v", err)
	}
	launcher := filepath.Join("..", "..", "deploy", "xui-agent-launcher")
	command := exec.Command("sh", launcher)
	command.Env = append(os.Environ(), "XUI_AGENT_STATE_DIRECTORY="+state, "XUI_AGENT_CONFIG_PATH=/unused")
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("launcher: %v\n%s", err, output)
	}
	current, err := os.Readlink(filepath.Join(state, "current"))
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	if current != "versions/old/xui-agent" {
		t.Fatalf("current target = %q, want previous version", current)
	}
	failed, err := NewManager(state, "")
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	failedState, err := failed.Failed()
	if err != nil || failedState.CommandID != "command" {
		t.Fatalf("failed state = %+v, err=%v", failedState, err)
	}
	if _, err := os.Stat(filepath.Join(state, pendingFilename)); !os.IsNotExist(err) {
		t.Fatalf("pending state still exists: %v", err)
	}
}

func TestLauncherForwardsTerminationToAgent(t *testing.T) {
	state := t.TempDir()
	versionDirectory := filepath.Join(state, "versions", "current")
	if err := os.MkdirAll(versionDirectory, 0o700); err != nil {
		t.Fatalf("create version directory: %v", err)
	}
	childPIDFile := filepath.Join(state, "child.pid")
	terminatedFile := filepath.Join(state, "terminated")
	binary := filepath.Join(versionDirectory, "xui-agent")
	fakeAgent := `#!/bin/sh
printf '%s\n' "$$" > "$XUI_AGENT_CHILD_PID_FILE"
trap 'printf terminated > "$XUI_AGENT_TERMINATED_FILE"; exit 0' TERM
while :; do sleep 1; done
`
	if err := os.WriteFile(binary, []byte(fakeAgent), 0o755); err != nil {
		t.Fatalf("write fake agent: %v", err)
	}
	if err := os.Symlink("versions/current/xui-agent", filepath.Join(state, "current")); err != nil {
		t.Fatalf("create current link: %v", err)
	}

	launcher := filepath.Join("..", "..", "deploy", "xui-agent-launcher")
	command := exec.Command("sh", launcher)
	command.Env = append(os.Environ(),
		"XUI_AGENT_STATE_DIRECTORY="+state,
		"XUI_AGENT_CONFIG_PATH=/unused",
		"XUI_AGENT_CHILD_PID_FILE="+childPIDFile,
		"XUI_AGENT_TERMINATED_FILE="+terminatedFile,
	)
	if err := command.Start(); err != nil {
		t.Fatalf("start launcher: %v", err)
	}
	launcherExited := false
	childPID := 0
	t.Cleanup(func() {
		if !launcherExited {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		if childPID > 0 {
			_ = syscall.Kill(childPID, syscall.SIGKILL)
		}
	})

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(childPIDFile)
		if err == nil {
			childPID, err = strconv.Atoi(strings.TrimSpace(string(raw)))
			if err != nil {
				t.Fatalf("parse child PID: %v", err)
			}
			break
		}
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("read child PID: %v", err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if childPID == 0 {
		t.Fatal("launcher did not start the agent")
	}

	if err := command.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("terminate launcher: %v", err)
	}
	waitResult := make(chan error, 1)
	go func() { waitResult <- command.Wait() }()
	select {
	case err := <-waitResult:
		launcherExited = true
		if err != nil {
			t.Fatalf("launcher exit: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("launcher did not exit after SIGTERM")
	}
	if _, err := os.Stat(terminatedFile); err != nil {
		t.Fatalf("agent did not receive SIGTERM: %v", err)
	}
	if err := syscall.Kill(childPID, 0); !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("agent process %d still exists after launcher exit: %v", childPID, err)
	}
}
