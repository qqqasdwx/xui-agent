package xrayruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/qqqasdwx/xui-agent/internal/xrayconfig"
)

type acceptingRunner struct{}

func (acceptingRunner) Validate(context.Context, string, string) error { return nil }

type recordingController struct {
	directory string
	failOn    string
	restarts  int
	stops     int
}

type canceledApplyController struct {
	directory string
	restarts  int
}

func (c *canceledApplyController) RestartAndWait(ctx context.Context) error {
	c.restarts++
	if c.restarts == 2 {
		return context.Canceled
	}
	if c.restarts == 3 && ctx.Err() != nil {
		return errors.New("rollback reused the canceled apply context")
	}
	return nil
}

func (*canceledApplyController) StopAndWait(ctx context.Context) error {
	if ctx.Err() != nil {
		return errors.New("rollback reused the canceled apply context")
	}
	return nil
}

func (c *recordingController) RestartAndWait(context.Context) error {
	c.restarts++
	target, err := os.Readlink(filepath.Join(c.directory, currentName))
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(filepath.Join(c.directory, target))
	if err != nil {
		return err
	}
	if c.failOn != "" && strings.Contains(string(raw), c.failOn) {
		return errors.New("synthetic runtime health failure")
	}
	return nil
}

func (c *recordingController) StopAndWait(context.Context) error {
	c.stops++
	return nil
}

func runtimeRequest(version uint64, raw string) Request {
	digest := sha256.Sum256([]byte(raw))
	return Request{
		ConfigVersion: version,
		ConfigDigest:  hex.EncodeToString(digest[:]),
		Config:        json.RawMessage(raw),
	}
}

func newRuntimeManager(t *testing.T, controller Controller) (*Manager, string) {
	t.Helper()
	state := t.TempDir()
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	return NewManager(state, validator, controller), state
}

func TestManagerAppliesIdempotentlyAndPreservesPreviousVersion(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	manager := NewManager(state, validator, controller)

	first := runtimeRequest(1, `{"version":1}`)
	result, err := manager.Apply(context.Background(), first)
	if err != nil || !result.Success() {
		t.Fatalf("apply first result=%+v err=%v", result, err)
	}
	firstTarget, err := os.Readlink(CurrentConfigPath(state))
	if err != nil {
		t.Fatalf("read first current target: %v", err)
	}
	duplicate, err := manager.Apply(context.Background(), first)
	if err != nil || !duplicate.Success() || controller.restarts != 1 {
		t.Fatalf("duplicate result=%+v err=%v restarts=%d", duplicate, err, controller.restarts)
	}

	second := runtimeRequest(2, `{"version":2}`)
	result, err = manager.Apply(context.Background(), second)
	if err != nil || !result.Success() {
		t.Fatalf("apply second result=%+v err=%v", result, err)
	}
	previous, err := os.Readlink(filepath.Join(Directory(state), previousName))
	if err != nil || previous != firstTarget {
		t.Fatalf("previous target=%q err=%v, want %q", previous, err, firstTarget)
	}
	current, err := manager.Current()
	if err != nil || current.ConfigVersion != 2 || current.ConfigDigest != second.ConfigDigest {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	info, err := os.Stat(filepath.Join(Directory(state), current.Target))
	if err != nil || info.Mode().Perm() != configFileMode {
		t.Fatalf("current config mode=%v err=%v", info.Mode().Perm(), err)
	}
}

func TestManagerRollsBackRuntimeHealthFailure(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state), failOn: `"fail":true`}
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	manager := NewManager(state, validator, controller)
	first := runtimeRequest(1, `{"version":1}`)
	if result, err := manager.Apply(context.Background(), first); err != nil || !result.Success() {
		t.Fatalf("apply first result=%+v err=%v", result, err)
	}
	firstTarget, _ := os.Readlink(CurrentConfigPath(state))

	failed := runtimeRequest(2, `{"version":2,"fail":true}`)
	result, err := manager.Apply(context.Background(), failed)
	if err != nil || result.Success() || result.Status != StatusApplyFailed || !result.RolledBack {
		t.Fatalf("failed apply result=%+v err=%v", result, err)
	}
	currentTarget, err := os.Readlink(CurrentConfigPath(state))
	if err != nil || currentTarget != firstTarget {
		t.Fatalf("current target=%q err=%v, want rollback %q", currentTarget, err, firstTarget)
	}
	current, err := manager.Current()
	if err != nil || current.ConfigVersion != 1 {
		t.Fatalf("applied state=%+v err=%v", current, err)
	}
	if controller.restarts != 3 {
		t.Fatalf("restart calls=%d, want initial, failed, rollback", controller.restarts)
	}
	if _, err := os.Stat(filepath.Join(Directory(state), pendingName)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending state remains after rollback: %v", err)
	}
}

func TestManagerRollbackSurvivesCanceledApplyContext(t *testing.T) {
	state := t.TempDir()
	controller := &canceledApplyController{directory: Directory(state)}
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	manager := NewManager(state, validator, controller)
	if result, err := manager.Apply(context.Background(), runtimeRequest(1, `{"version":1}`)); err != nil || !result.Success() {
		t.Fatalf("apply first result=%+v err=%v", result, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	result, err := manager.Apply(ctx, runtimeRequest(2, `{"version":2}`))
	if err != nil || result.Success() || !result.RolledBack {
		t.Fatalf("canceled apply result=%+v err=%v", result, err)
	}
	current, err := manager.Current()
	if err != nil || current.ConfigVersion != 1 {
		t.Fatalf("applied state=%+v err=%v", current, err)
	}
	if controller.restarts != 3 {
		t.Fatalf("restart calls=%d, want initial, failed apply, rollback", controller.restarts)
	}
}

func TestManagerStopsAfterFailedInitialApply(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state), failOn: "fail"}
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	manager := NewManager(state, validator, controller)
	result, err := manager.Apply(context.Background(), runtimeRequest(1, `{"fail":true}`))
	if err != nil || result.Success() || !result.RolledBack || controller.stops != 1 {
		t.Fatalf("initial failure result=%+v err=%v stops=%d", result, err, controller.stops)
	}
	if _, err := os.Stat(CurrentConfigPath(state)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("current config remains after initial failure: %v", err)
	}
	if _, err := manager.Current(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("applied state exists after initial failure: %v", err)
	}
}

func TestManagerRecoversUnconfirmedSwitchByRollingBack(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	manager := NewManager(state, validator, controller)
	first := runtimeRequest(1, `{"version":1}`)
	if result, err := manager.Apply(context.Background(), first); err != nil || !result.Success() {
		t.Fatalf("apply first result=%+v err=%v", result, err)
	}
	firstTarget, _ := os.Readlink(CurrentConfigPath(state))
	second := runtimeRequest(2, `{"version":2}`)
	secondTarget, err := manager.installVersion(second)
	if err != nil {
		t.Fatalf("install second: %v", err)
	}
	pending := pendingState{
		CommandTarget:  secondTarget,
		PreviousTarget: firstTarget,
		ConfigVersion:  second.ConfigVersion,
		ConfigDigest:   second.ConfigDigest,
		StartedAt:      1,
	}
	if err := writeJSONAtomic(manager.pendingPath(), pending, configFileMode); err != nil {
		t.Fatalf("write pending: %v", err)
	}
	if err := atomicSymlink(secondTarget, manager.currentPath()); err != nil {
		t.Fatalf("switch unconfirmed target: %v", err)
	}

	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	currentTarget, err := os.Readlink(CurrentConfigPath(state))
	if err != nil || currentTarget != firstTarget {
		t.Fatalf("current target=%q err=%v, want %q", currentTarget, err, firstTarget)
	}
	current, err := manager.Current()
	if err != nil || current.ConfigVersion != 1 {
		t.Fatalf("applied state=%+v err=%v", current, err)
	}
}
