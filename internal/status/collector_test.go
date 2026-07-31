package status

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
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

type failingRuntimeController struct{ restarts int }

func (c *failingRuntimeController) RestartAndWait(context.Context) error {
	c.restarts++
	if c.restarts == 1 {
		return fmt.Errorf("synthetic activation failure")
	}
	return nil
}
func (*failingRuntimeController) StopAndWait(context.Context) error { return nil }

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

func TestParseProcessStartTicksHandlesParenthesesInCommand(t *testing.T) {
	raw := "123 (xray ) worker) S 1 2 3 4 5 6 7 8 9 10 11 12 13 14 15 16 17 18 123456 0\n"
	startTicks, err := parseProcessStartTicks(raw)
	if err != nil {
		t.Fatalf("parseProcessStartTicks: %v", err)
	}
	if startTicks != 123456 {
		t.Fatalf("start ticks=%d, want 123456", startTicks)
	}
}

func TestParseBootTime(t *testing.T) {
	bootTime, err := parseBootTime("cpu 1 2 3 4\nbtime 1700000000\nprocesses 1\n")
	if err != nil {
		t.Fatalf("parseBootTime: %v", err)
	}
	if bootTime != 1700000000 {
		t.Fatalf("boot time=%d, want 1700000000", bootTime)
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

	failedRaw := json.RawMessage(`{"inbounds":[],"failed":true}`)
	failedDigest := sha256.Sum256(failedRaw)
	failing := xrayruntime.NewManager(state, validator, &failingRuntimeController{})
	failed, err := failing.Apply(context.Background(), xrayruntime.Request{
		ConfigVersion: 10, ConfigDigest: hex.EncodeToString(failedDigest[:]), Config: failedRaw,
	})
	if err != nil || failed.Success() || failed.RecoveryStatus != xrayruntime.RecoveryStatusRolledBack {
		t.Fatalf("failed managed config result=%+v err=%v", failed, err)
	}
	status = collector.collectXray(context.Background())
	if status.RecoveryStatus != xrayruntime.RecoveryStatusRolledBack || status.RecoveryErrorCode != xrayruntime.ErrorCodeActivationFailed {
		t.Fatalf("managed recovery status=%+v", status)
	}
}

func TestCollectorReportsCorruptAppliedBinaryState(t *testing.T) {
	state := t.TempDir()
	runtimeDirectory := filepath.Join(state, "xray-runtime")
	if err := os.MkdirAll(runtimeDirectory, 0o700); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}
	applied := `{"version":"v1","xrayVersion":"v1","archiveDigest":"` + strings.Repeat("a", 64) + `","target":"versions/v1","appliedAt":1}`
	if err := os.WriteFile(filepath.Join(runtimeDirectory, "applied.json"), []byte(applied), 0o600); err != nil {
		t.Fatalf("write applied state: %v", err)
	}

	collector := NewCollector(config.XrayConfig{
		Mode:       config.XrayModeManaged,
		BinaryPath: "/opt/xray/bootstrap",
	}, state, time.Now())
	status := collector.collectXray(context.Background())
	if !strings.Contains(status.Error, "applied Xray binary state is corrupt") {
		t.Fatalf("managed binary error = %q", status.Error)
	}
}

func TestCollectorRefreshesVersionAfterManagedBinarySwitch(t *testing.T) {
	root := t.TempDir()
	first := filepath.Join(root, "versions", "v1")
	second := filepath.Join(root, "versions", "v2")
	for path, version := range map[string]string{first: "Xray v1", second: "Xray v2"} {
		if err := os.MkdirAll(path, 0o700); err != nil {
			t.Fatalf("create version directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(path, "xray"), []byte("#!/bin/sh\necho '"+version+"'\n"), 0o700); err != nil {
			t.Fatalf("write fake Xray: %v", err)
		}
	}
	current := filepath.Join(root, "current")
	if err := os.Symlink(first, current); err != nil {
		t.Fatalf("link first version: %v", err)
	}
	collector := NewCollector(config.XrayConfig{BinaryPath: filepath.Join(current, "xray")}, root, time.Now())
	if got := collector.xrayVersion(context.Background(), collector.binaryPath()); got != "Xray v1" {
		t.Fatalf("initial version = %q, want Xray v1", got)
	}
	if err := os.Remove(current); err != nil {
		t.Fatalf("remove current link: %v", err)
	}
	if err := os.Symlink(second, current); err != nil {
		t.Fatalf("link second version: %v", err)
	}
	if got := collector.xrayVersion(context.Background(), collector.binaryPath()); got != "Xray v2" {
		t.Fatalf("version after switch = %q, want Xray v2", got)
	}
}
