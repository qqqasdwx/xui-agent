package xrayruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
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

type preflightFailureController struct {
	restarts int
}

type rollbackFailureController struct {
	directory string
	restarts  int
}

func (*preflightFailureController) Preflight() error {
	return errors.New("synthetic corrupt pid")
}

func (c *preflightFailureController) RestartAndWait(context.Context) error {
	c.restarts++
	return nil
}

func (*preflightFailureController) StopAndWait(context.Context) error { return nil }

func (c *rollbackFailureController) RestartAndWait(context.Context) error {
	c.restarts++
	if c.restarts == 1 {
		return nil
	}
	return errors.New("synthetic restart failure")
}

func (*rollbackFailureController) StopAndWait(context.Context) error { return nil }

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

func TestManagerRejectsAppliedMetadataThatDiffersFromTarget(t *testing.T) {
	controller := &recordingController{}
	manager, state := newRuntimeManager(t, controller)
	controller.directory = Directory(state)
	request := runtimeRequest(1, `{"version":1}`)
	if result, err := manager.Apply(context.Background(), request); err != nil || !result.Success() {
		t.Fatalf("Apply result=%+v err=%v", result, err)
	}
	applied, err := manager.loadApplied()
	if err != nil {
		t.Fatalf("loadApplied: %v", err)
	}
	applied.ConfigVersion = 2
	if err := writeJSONAtomic(manager.appliedPath(), applied, configFileMode); err != nil {
		t.Fatalf("write applied: %v", err)
	}
	if _, err := manager.Current(); err == nil {
		t.Fatal("applied state was accepted with metadata that differs from its immutable target")
	}
}

func TestManagerRejectsPendingMetadataThatDiffersFromTarget(t *testing.T) {
	controller := &recordingController{}
	manager, state := newRuntimeManager(t, controller)
	controller.directory = Directory(state)
	request := runtimeRequest(1, `{"version":1}`)
	target, err := manager.installVersion(request, false)
	if err != nil {
		t.Fatalf("installVersion: %v", err)
	}
	pending := pendingState{
		CommandTarget: target, ConfigVersion: 2, ConfigDigest: request.ConfigDigest, StartedAt: 1,
	}
	if err := writeJSONAtomic(manager.pendingPath(), pending, configFileMode); err != nil {
		t.Fatalf("write pending: %v", err)
	}
	if _, err := manager.loadPending(); err == nil {
		t.Fatal("pending state was accepted with metadata that differs from its immutable target")
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
	if err != nil || result.Success() || result.Status != StatusApplyFailed || !result.RolledBack || result.ErrorCode != ErrorCodeActivationFailed || result.RecoveryStatus != RecoveryStatusRolledBack {
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
	secondTarget, err := manager.installVersion(second, false)
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

func TestManagerRecoveryAtDurableBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name           string
		switchCurrent  bool
		persistApplied bool
		wantVersion    uint64
		wantRestarts   int
		wantFailed     bool
	}{
		{name: "pending before switch", wantVersion: 1, wantRestarts: 2, wantFailed: true},
		{name: "current switched before applied", switchCurrent: true, wantVersion: 1, wantRestarts: 2, wantFailed: true},
		{name: "applied before pending removal", switchCurrent: true, persistApplied: true, wantVersion: 2, wantRestarts: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
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
			secondTarget, err := manager.installVersion(second, false)
			if err != nil {
				t.Fatalf("install second: %v", err)
			}
			pending := pendingState{
				CommandTarget: secondTarget, PreviousTarget: firstTarget,
				ConfigVersion: second.ConfigVersion, ConfigDigest: second.ConfigDigest, StartedAt: 1,
			}
			if err := writeJSONAtomic(manager.pendingPath(), pending, configFileMode); err != nil {
				t.Fatalf("write pending: %v", err)
			}
			if tc.switchCurrent {
				if err := atomicSymlink(secondTarget, manager.currentPath()); err != nil {
					t.Fatalf("switch current: %v", err)
				}
			}
			if tc.persistApplied {
				if err := writeJSONAtomic(manager.appliedPath(), AppliedState{
					ConfigVersion: 2, ConfigDigest: second.ConfigDigest, Target: secondTarget, AppliedAt: 2,
				}, configFileMode); err != nil {
					t.Fatalf("write applied: %v", err)
				}
			}

			if err := manager.Recover(context.Background()); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			current, err := manager.Current()
			if err != nil || current.ConfigVersion != tc.wantVersion {
				t.Fatalf("current=%+v err=%v, want v%d", current, err, tc.wantVersion)
			}
			if controller.restarts != tc.wantRestarts {
				t.Fatalf("restarts=%d, want %d", controller.restarts, tc.wantRestarts)
			}
			if _, err := os.Stat(manager.pendingPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pending remains: %v", err)
			}
			_, failedErr := os.Stat(manager.failedPath())
			if tc.wantFailed && failedErr != nil {
				t.Fatalf("failed state missing: %v", failedErr)
			}
			if !tc.wantFailed && !errors.Is(failedErr, os.ErrNotExist) {
				t.Fatalf("unexpected failed state: %v", failedErr)
			}
		})
	}
}

func TestRecoverRestoresPreviousStateWhenAppliedCandidateLosesCurrentLink(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	manager := NewManager(state, validator, controller)
	first := runtimeRequest(1, `{"version":1}`)
	if result, err := manager.Apply(context.Background(), first); err != nil || !result.Success() {
		t.Fatalf("apply first result=%+v err=%v", result, err)
	}
	previous, err := manager.Current()
	if err != nil {
		t.Fatalf("load first state: %v", err)
	}
	second := runtimeRequest(2, `{"version":2}`)
	target, err := manager.installVersion(second, false)
	if err != nil {
		t.Fatalf("install second: %v", err)
	}
	pending := pendingState{
		CommandTarget: target, PreviousTarget: previous.Target, PreviousApplied: previous,
		ConfigVersion: second.ConfigVersion, ConfigDigest: second.ConfigDigest, StartedAt: 1,
	}
	if err := writeJSONAtomic(manager.pendingPath(), pending, configFileMode); err != nil {
		t.Fatalf("write pending: %v", err)
	}
	if err := writeJSONAtomic(manager.appliedPath(), AppliedState{
		ConfigVersion: second.ConfigVersion, ConfigDigest: second.ConfigDigest, Target: target, AppliedAt: 2,
	}, configFileMode); err != nil {
		t.Fatalf("write visible candidate applied state: %v", err)
	}
	if err := os.Remove(manager.currentPath()); err != nil {
		t.Fatalf("remove current link: %v", err)
	}

	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	restored, err := manager.Current()
	if err != nil || restored.ConfigVersion != previous.ConfigVersion || restored.Target != previous.Target {
		t.Fatalf("restored=%+v err=%v, want %+v", restored, err, previous)
	}
}

func TestRollbackRemovesVisibleInitialAppliedConfigState(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	manager := NewManager(state, validator, controller)
	request := runtimeRequest(1, `{"version":1}`)
	target, err := manager.installVersion(request, false)
	if err != nil {
		t.Fatalf("install candidate: %v", err)
	}
	pending := pendingState{
		CommandTarget: target, ConfigVersion: request.ConfigVersion,
		ConfigDigest: request.ConfigDigest, StartedAt: 1,
	}
	if err := atomicSymlink(target, manager.currentPath()); err != nil {
		t.Fatalf("select candidate: %v", err)
	}
	if err := writeJSONAtomic(manager.appliedPath(), AppliedState{
		ConfigVersion: request.ConfigVersion, ConfigDigest: request.ConfigDigest, Target: target, AppliedAt: 1,
	}, configFileMode); err != nil {
		t.Fatalf("write visible candidate applied state: %v", err)
	}

	if err := manager.rollback(context.Background(), pending, "synthetic persistence failure", ErrorCodePersistenceFailed); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Lstat(manager.currentPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate current remains: %v", err)
	}
	if _, err := os.Stat(manager.appliedPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate applied state remains: %v", err)
	}
	if controller.stops != 1 {
		t.Fatalf("stop count = %d, want 1", controller.stops)
	}
}

func TestManagerRejectsMissingRollbackTargetBeforeSwitch(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	manager := NewManager(state, validator, controller)
	first := runtimeRequest(1, `{"version":1}`)
	if result, err := manager.Apply(context.Background(), first); err != nil || !result.Success() {
		t.Fatalf("apply first result=%+v err=%v", result, err)
	}
	firstTarget, _ := os.Readlink(CurrentConfigPath(state))
	if err := os.Remove(filepath.Join(Directory(state), firstTarget)); err != nil {
		t.Fatalf("remove rollback target: %v", err)
	}

	if _, err := manager.Apply(context.Background(), runtimeRequest(2, `{"version":2}`)); err == nil || !strings.Contains(err.Error(), "verify rollback") {
		t.Fatalf("missing rollback error = %v", err)
	}
	if current, err := os.Readlink(CurrentConfigPath(state)); err != nil || current != firstTarget {
		t.Fatalf("current=%q err=%v, want unchanged %q", current, err, firstTarget)
	}
	if controller.restarts != 1 {
		t.Fatalf("restarts=%d, want only initial apply", controller.restarts)
	}
	if _, err := os.Stat(manager.pendingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending state was created: %v", err)
	}
}

func TestManagerReportsStorageFailureBeforeConfigSwitch(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "disk full", err: syscall.ENOSPC},
		{name: "read only filesystem", err: syscall.EROFS},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			controller := &recordingController{directory: Directory(state)}
			validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
			manager := NewManager(state, validator, controller)
			if result, err := manager.Apply(context.Background(), runtimeRequest(1, `{"version":1}`)); err != nil || !result.Success() {
				t.Fatalf("apply first result=%+v err=%v", result, err)
			}
			first, err := manager.Current()
			if err != nil {
				t.Fatalf("load first state: %v", err)
			}
			initialRestarts := controller.restarts
			manager.writeState = func(path string, value any, mode os.FileMode) error {
				if path == manager.pendingPath() {
					return tc.err
				}
				return writeJSONAtomic(path, value, mode)
			}

			if _, err := manager.Apply(context.Background(), runtimeRequest(2, `{"version":2}`)); err == nil || !errors.Is(err, tc.err) {
				t.Fatalf("storage failure = %v, want %v", err, tc.err)
			} else if code, recovery := ErrorDetails(err); code != ErrorCodePreparationFailed || recovery != RecoveryStatusNotRequired {
				t.Fatalf("storage failure details=(%q,%q)", code, recovery)
			}
			current, err := manager.Current()
			if err != nil || current.ConfigVersion != first.ConfigVersion || current.Target != first.Target {
				t.Fatalf("current=%+v err=%v, want unchanged %+v", current, err, first)
			}
			if controller.restarts != initialRestarts {
				t.Fatalf("restarts=%d, want unchanged %d", controller.restarts, initialRestarts)
			}
			if _, err := os.Stat(manager.pendingPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pending state exists after failed persistence: %v", err)
			}
		})
	}
}

func TestManagerRunsProcessPreflightBeforeInstallingVersion(t *testing.T) {
	state := t.TempDir()
	controller := &preflightFailureController{}
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	manager := NewManager(state, validator, controller)

	if _, err := manager.Apply(context.Background(), runtimeRequest(1, `{"version":1}`)); err == nil || !strings.Contains(err.Error(), "preflight managed Xray process") {
		t.Fatalf("preflight error = %v", err)
	} else if code, recovery := ErrorDetails(err); code != ErrorCodePreparationFailed || recovery != RecoveryStatusNotRequired {
		t.Fatalf("preflight details=(%q,%q)", code, recovery)
	}
	if controller.restarts != 0 {
		t.Fatalf("restart called after failed preflight")
	}
	versions := filepath.Join(Directory(state), versionsName)
	entries, err := os.ReadDir(versions)
	if err == nil && len(entries) != 0 {
		t.Fatalf("version files installed after failed preflight: %v", entries)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read versions: %v", err)
	}
}

func TestManagerPersistsReadableRollbackFailure(t *testing.T) {
	state := t.TempDir()
	controller := &rollbackFailureController{directory: Directory(state)}
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	manager := NewManager(state, validator, controller)
	if result, err := manager.Apply(context.Background(), runtimeRequest(1, `{"version":1}`)); err != nil || !result.Success() {
		t.Fatalf("apply first result=%+v err=%v", result, err)
	}

	second := runtimeRequest(2, `{"version":2}`)
	if _, err := manager.Apply(context.Background(), second); err == nil {
		t.Fatal("apply with failed rollback succeeded")
	} else if code, recovery := ErrorDetails(err); code != ErrorCodeRecoveryFailed || recovery != RecoveryStatusFailed {
		t.Fatalf("rollback failure details=(%q,%q) err=%v", code, recovery, err)
	}
	failure, err := LoadFailureState(state)
	if err != nil {
		t.Fatalf("LoadFailureState: %v", err)
	}
	if failure.ErrorCode != ErrorCodeActivationFailed || failure.RecoveryStatus != RecoveryStatusFailed || !strings.Contains(failure.Error, "rollback failed") {
		t.Fatalf("failure state=%+v", failure)
	}
}

func TestManagerPrunesVersionsWithoutDeletingRollbackTargets(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	manager := NewManager(state, validator, controller)
	for version := uint64(1); version <= 8; version++ {
		request := runtimeRequest(version, fmt.Sprintf(`{"version":%d}`, version))
		if result, err := manager.Apply(context.Background(), request); err != nil || !result.Success() {
			t.Fatalf("apply v%d result=%+v err=%v", version, result, err)
		}
	}
	current, err := os.Readlink(manager.currentPath())
	if err != nil {
		t.Fatalf("read current: %v", err)
	}
	previous, err := os.Readlink(manager.previousPath())
	if err != nil {
		t.Fatalf("read previous: %v", err)
	}
	entries, err := os.ReadDir(filepath.Join(Directory(state), versionsName))
	if err != nil {
		t.Fatalf("read versions: %v", err)
	}
	if len(entries) != retainedVersions {
		t.Fatalf("version files=%d, want %d", len(entries), retainedVersions)
	}
	for _, target := range []string{current, previous} {
		if _, err := os.Stat(filepath.Join(Directory(state), target)); err != nil {
			t.Fatalf("protected target %q was deleted: %v", target, err)
		}
	}
}

func TestManagerRepairsDriftedAppliedConfigFromSameVersion(t *testing.T) {
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
	if result, err := manager.Apply(context.Background(), second); err != nil || !result.Success() {
		t.Fatalf("apply second result=%+v err=%v", result, err)
	}
	driftedTarget, _ := os.Readlink(CurrentConfigPath(state))
	if err := os.WriteFile(filepath.Join(Directory(state), driftedTarget), []byte(`{"drifted":true}`), configFileMode); err != nil {
		t.Fatalf("tamper applied config: %v", err)
	}

	repaired, err := manager.Apply(context.Background(), second)
	if err != nil || !repaired.Success() {
		t.Fatalf("repair result=%+v err=%v", repaired, err)
	}
	repairedTarget, err := os.Readlink(CurrentConfigPath(state))
	if err != nil {
		t.Fatalf("read repaired target: %v", err)
	}
	if repairedTarget == driftedTarget || !strings.Contains(repairedTarget, "-repair-") {
		t.Fatalf("repaired target=%q, want a new immutable repair target", repairedTarget)
	}
	raw, err := os.ReadFile(filepath.Join(Directory(state), repairedTarget))
	if err != nil || !bytes.Equal(raw, second.Config) {
		t.Fatalf("repaired config=%q err=%v", raw, err)
	}
	previous, err := os.Readlink(filepath.Join(Directory(state), previousName))
	if err != nil || previous != firstTarget {
		t.Fatalf("previous target=%q err=%v, want %q", previous, err, firstTarget)
	}
	current, err := manager.Current()
	if err != nil || current.ConfigVersion != second.ConfigVersion || current.ConfigDigest != second.ConfigDigest || current.Target != repairedTarget {
		t.Fatalf("current=%+v err=%v", current, err)
	}
	if controller.restarts != 3 {
		t.Fatalf("restart calls=%d, want first, second, repair", controller.restarts)
	}
	if duplicate, err := manager.Apply(context.Background(), second); err != nil || !duplicate.Success() || controller.restarts != 3 {
		t.Fatalf("duplicate after repair=%+v err=%v restarts=%d", duplicate, err, controller.restarts)
	}
}

func TestManagerRepairsAppliedConfigPermissionDrift(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	validator := xrayconfig.NewManager(state, "/opt/xray", acceptingRunner{})
	manager := NewManager(state, validator, controller)

	request := runtimeRequest(1, `{"version":1}`)
	if result, err := manager.Apply(context.Background(), request); err != nil || !result.Success() {
		t.Fatalf("apply result=%+v err=%v", result, err)
	}
	driftedTarget, _ := os.Readlink(CurrentConfigPath(state))
	if err := os.Chmod(filepath.Join(Directory(state), driftedTarget), 0o644); err != nil {
		t.Fatalf("change applied config permissions: %v", err)
	}

	repaired, err := manager.Apply(context.Background(), request)
	if err != nil || !repaired.Success() {
		t.Fatalf("repair result=%+v err=%v", repaired, err)
	}
	repairedTarget, err := os.Readlink(CurrentConfigPath(state))
	if err != nil || repairedTarget == driftedTarget || !strings.Contains(repairedTarget, "-repair-") {
		t.Fatalf("repaired target=%q err=%v", repairedTarget, err)
	}
	info, err := os.Stat(filepath.Join(Directory(state), repairedTarget))
	if err != nil {
		t.Fatalf("inspect repaired config: %v", err)
	}
	if info.Mode().Perm() != configFileMode {
		t.Fatalf("repaired mode=%v", info.Mode().Perm())
	}
}
