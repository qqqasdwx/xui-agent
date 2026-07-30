package status

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/config"
	"github.com/qqqasdwx/xui-agent/internal/xrayconfig"
	"github.com/qqqasdwx/xui-agent/internal/xrayruntime"
)

type acceptingConfigRunner struct{}

func (acceptingConfigRunner) Validate(context.Context, string, string) error { return nil }

type successfulRuntimeController struct{}

func (successfulRuntimeController) RestartAndWait(context.Context) error { return nil }
func (successfulRuntimeController) StopAndWait(context.Context) error    { return nil }

func TestProcessStatusFindsConfiguredExecutableWithoutPIDFile(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	running, startedAt, err := processStatus("", executable)
	if err != nil {
		t.Fatalf("processStatus: %v", err)
	}
	if !running {
		t.Fatal("current executable was not found in /proc")
	}
	if startedAt <= 0 || startedAt > time.Now().Unix() {
		t.Fatalf("process startedAt=%d, want a past Unix timestamp", startedAt)
	}
}

func TestProcessStatusReportsStartedAtFromPIDFile(t *testing.T) {
	executable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	pidFile := filepath.Join(t.TempDir(), "xray.pid")
	if err := os.WriteFile(pidFile, []byte(fmt.Sprintf("%d\n", os.Getpid())), 0o600); err != nil {
		t.Fatalf("write pid file: %v", err)
	}
	running, startedAt, err := processStatus(pidFile, executable)
	if err != nil {
		t.Fatalf("processStatus: %v", err)
	}
	if !running {
		t.Fatal("current process was not reported as running")
	}
	if startedAt <= 0 || startedAt > time.Now().Unix() {
		t.Fatalf("process startedAt=%d, want a past Unix timestamp", startedAt)
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

func TestCollectorReportsConfirmedManagedConfigVersion(t *testing.T) {
	state := t.TempDir()
	binary := filepath.Join(t.TempDir(), "xray")
	if err := os.WriteFile(binary, []byte("#!/bin/sh\n[ \"$1\" = version ] && echo 'Xray test'\n"), 0o700); err != nil {
		t.Fatalf("write fake Xray: %v", err)
	}
	raw := json.RawMessage(`{"inbounds":[]}`)
	digest := sha256.Sum256(raw)
	validator := xrayconfig.NewManager(state, binary, acceptingConfigRunner{})
	manager := xrayruntime.NewManager(state, validator, successfulRuntimeController{})
	result, err := manager.Apply(context.Background(), xrayruntime.Request{
		ConfigVersion: 9,
		ConfigDigest:  hex.EncodeToString(digest[:]),
		Config:        raw,
	})
	if err != nil || !result.Success() {
		t.Fatalf("apply managed config result=%+v err=%v", result, err)
	}

	collector := NewCollector(config.XrayConfig{
		Mode:       config.XrayModeManaged,
		BinaryPath: binary,
	}, state, time.Now())
	status := collector.collectXray(context.Background())
	if status.ConfigVersion != 9 || status.ConfigDigest != hex.EncodeToString(digest[:]) {
		t.Fatalf("managed config status=%+v", status)
	}
}
