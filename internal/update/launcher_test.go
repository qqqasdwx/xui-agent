package update

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
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
	failed, err := NewManager(state, "", false, "")
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
