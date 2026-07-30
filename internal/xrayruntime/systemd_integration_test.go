//go:build integration

package xrayruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/xrayconfig"
)

func TestManagedRuntimeWithSystemd(t *testing.T) {
	if os.Getenv("XUI_AGENT_SYSTEMD_INTEGRATION") != "1" {
		t.Skip("set XUI_AGENT_SYSTEMD_INTEGRATION=1 in the isolated systemd harness")
	}
	state := requireIntegrationEnv(t, "XUI_AGENT_INTEGRATION_STATE")
	binary := requireIntegrationEnv(t, "XUI_AGENT_INTEGRATION_XRAY")
	validator := xrayconfig.NewManager(state, binary, nil)
	controller := NewProcessController(state, binary)
	controller.restartTimeout = 10 * time.Second
	controller.stablePeriod = 500 * time.Millisecond
	manager := NewManager(state, validator, controller)

	first := integrationRequest(1, `{"marker":"v1"}`)
	result, err := manager.Apply(context.Background(), first)
	if err != nil || !result.Success() {
		t.Fatalf("apply v1 result=%+v err=%v", result, err)
	}
	firstPID := readIntegrationPID(t, state)
	duplicate, err := manager.Apply(context.Background(), first)
	if err != nil || !duplicate.Success() || readIntegrationPID(t, state) != firstPID {
		t.Fatalf("duplicate apply result=%+v err=%v", duplicate, err)
	}

	second := integrationRequest(2, `{"marker":"v2"}`)
	result, err = manager.Apply(context.Background(), second)
	if err != nil || !result.Success() {
		t.Fatalf("apply v2 result=%+v err=%v", result, err)
	}
	secondPID := readIntegrationPID(t, state)
	if secondPID == firstPID {
		t.Fatal("v2 did not replace the v1 Xray process")
	}
	secondTarget, err := os.Readlink(CurrentConfigPath(state))
	if err != nil {
		t.Fatalf("read v2 target: %v", err)
	}

	failed := integrationRequest(3, `{"marker":"v3","failStart":true}`)
	result, err = manager.Apply(context.Background(), failed)
	if err != nil || result.Success() || !result.RolledBack {
		t.Fatalf("failed v3 result=%+v err=%v", result, err)
	}
	current, err := manager.Current()
	if err != nil || current.ConfigVersion != 2 || current.Target != secondTarget {
		t.Fatalf("state after failed v3=%+v err=%v", current, err)
	}
	if readIntegrationPID(t, state) == secondPID {
		t.Fatal("rollback did not restart the v2 Xray process")
	}

	fourth := integrationRequest(4, `{"marker":"v4"}`)
	fourthTarget, err := manager.installVersion(fourth)
	if err != nil {
		t.Fatalf("install v4: %v", err)
	}
	pending := pendingState{
		CommandTarget: fourthTarget, PreviousTarget: secondTarget,
		ConfigVersion: fourth.ConfigVersion, ConfigDigest: fourth.ConfigDigest,
		StartedAt: time.Now().Unix(),
	}
	if err := writeJSONAtomic(manager.pendingPath(), pending, configFileMode); err != nil {
		t.Fatalf("persist simulated v4 pending state: %v", err)
	}
	if err := atomicSymlink(fourthTarget, manager.currentPath()); err != nil {
		t.Fatalf("switch simulated v4: %v", err)
	}
	if err := controller.RestartAndWait(context.Background()); err != nil {
		t.Fatalf("start simulated unconfirmed v4: %v", err)
	}
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("recover unconfirmed v4: %v", err)
	}
	current, err = manager.Current()
	if err != nil || current.ConfigVersion != 2 || current.Target != secondTarget {
		t.Fatalf("state after v4 recovery=%+v err=%v", current, err)
	}
}

func integrationRequest(version uint64, raw string) Request {
	digest := sha256.Sum256([]byte(raw))
	return Request{
		ConfigVersion: version,
		ConfigDigest:  hex.EncodeToString(digest[:]),
		Config:        json.RawMessage(raw),
	}
}

func readIntegrationPID(t *testing.T, state string) int {
	t.Helper()
	raw, err := os.ReadFile(PIDPath(state))
	if err != nil {
		t.Fatalf("read managed Xray pid: %v", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid <= 0 {
		t.Fatalf("invalid managed Xray pid %q: %v", raw, err)
	}
	return pid
}

func requireIntegrationEnv(t *testing.T, name string) string {
	t.Helper()
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" || !filepath.IsAbs(value) {
		t.Fatalf("%s must be an absolute path", name)
	}
	return value
}
