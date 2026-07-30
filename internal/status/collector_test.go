package status

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestProcessStatusFindsConfiguredExecutableWithoutPIDFile(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	running, _, err := processStatus("", executable)
	if err != nil {
		t.Fatalf("processStatus: %v", err)
	}
	if !running {
		t.Fatal("current executable was not found in /proc")
	}
}

func TestProcessStatusRejectsPIDFileForDifferentExecutable(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "xray.pid")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	if running, _, err := processStatus(pidFile, "/bin/sh"); err == nil || running {
		t.Fatalf("processStatus running=%v err=%v, want executable mismatch", running, err)
	}
}

func TestProcessCommandMatchesConfiguredExecutable(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	want, err := filepath.EvalSymlinks(executable)
	if err != nil {
		t.Fatalf("EvalSymlinks: %v", err)
	}
	match, err := processCommandMatches("/proc/self/cmdline", want)
	if err != nil {
		t.Fatalf("processCommandMatches: %v", err)
	}
	if !match {
		t.Fatal("current process command did not match its executable")
	}
}

func TestProcessCommandRejectsDifferentExecutable(t *testing.T) {
	match, err := processCommandMatches("/proc/self/cmdline", "/bin/sh")
	if err != nil {
		t.Fatalf("processCommandMatches: %v", err)
	}
	if match {
		t.Fatal("current process unexpectedly matched /bin/sh")
	}
}
