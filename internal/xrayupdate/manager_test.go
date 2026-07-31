package xrayupdate

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/release"
	"github.com/qqqasdwx/xui-agent/internal/xrayruntime"
)

type recordingRunner struct {
	versions     []string
	configChecks int
}

func (r *recordingRunner) ValidateVersion(_ context.Context, _ string, version string) error {
	r.versions = append(r.versions, version)
	return nil
}

func (r *recordingRunner) ValidateConfig(_ context.Context, _, configPath string) error {
	r.configChecks++
	if _, err := os.Stat(configPath); err != nil {
		return err
	}
	return nil
}

type recordingController struct {
	directory             string
	failTarget            string
	allowBootstrapRestart bool
	preflights            int
	stops                 int
	restarts              int
}

func (c *recordingController) Preflight() error {
	c.preflights++
	return nil
}

func (c *recordingController) StopAndWait(context.Context) error {
	c.stops++
	return nil
}

func (c *recordingController) RestartAndWait(context.Context) error {
	c.restarts++
	target, err := os.Readlink(filepath.Join(c.directory, currentName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) && c.allowBootstrapRestart {
			return nil
		}
		return err
	}
	if c.failTarget != "" && strings.Contains(target, c.failTarget) {
		return errors.New("synthetic Xray startup failure")
	}
	return nil
}

func testArchive(t *testing.T, marker string) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, entry := range []struct {
		name string
		raw  []byte
		mode os.FileMode
	}{
		{name: "xray", raw: []byte("xray-" + marker), mode: 0o755},
		{name: "geoip.dat", raw: []byte("geoip-" + marker), mode: 0o644},
		{name: "geosite.dat", raw: []byte("geosite-" + marker), mode: 0o644},
	} {
		header := &zip.FileHeader{Name: entry.name, Method: zip.Deflate}
		header.SetMode(entry.mode)
		stream, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatalf("create %s: %v", entry.name, err)
		}
		if _, err := stream.Write(entry.raw); err != nil {
			t.Fatalf("write %s: %v", entry.name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close archive: %v", err)
	}
	return output.Bytes()
}

func newTestManager(t *testing.T, state string, controller Controller, runner *recordingRunner) *Manager {
	t.Helper()
	manager, err := NewManager(state, controller, runner)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager
}

func installVersion(t *testing.T, manager *Manager, version string) Result {
	t.Helper()
	result, err := manager.InstallLocal(context.Background(), "command-"+version, version, testArchive(t, version))
	if err != nil || !result.Success() {
		t.Fatalf("install %s result=%+v err=%v", version, result, err)
	}
	return result
}

func writeAppliedConfig(t *testing.T, state string) {
	t.Helper()
	raw := []byte(`{"inbounds":[]}`)
	digest := sha256.Sum256(raw)
	target := filepath.Join("versions", "00000000000000000001-"+hex.EncodeToString(digest[:])+".json")
	directory := xrayruntime.Directory(state)
	if err := os.MkdirAll(filepath.Join(directory, "versions"), 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, target), raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.Symlink(target, xrayruntime.CurrentConfigPath(state)); err != nil {
		t.Fatalf("link config: %v", err)
	}
	applied := xrayruntime.AppliedState{ConfigVersion: 1, ConfigDigest: hex.EncodeToString(digest[:]), Target: target, AppliedAt: 1}
	encoded, err := json.Marshal(applied)
	if err != nil {
		t.Fatalf("marshal applied config: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, appliedName), append(encoded, '\n'), 0o600); err != nil {
		t.Fatalf("write applied config: %v", err)
	}
}

func TestManagerDownloadsChecksummedBundleFromPinnedReleaseShape(t *testing.T) {
	downloadTemp := t.TempDir()
	t.Setenv("TMPDIR", downloadTemp)
	arch := runtime.GOARCH
	if arch == "arm" {
		arch = "armv7"
	}
	archive := testArchive(t, "github-release")
	digest := sha256.Sum256(archive)
	manifest := Manifest{
		SchemaVersion: ManifestSchemaVersion,
		Version:       "v26.7.28-xui.1",
		XrayVersion:   "26.7.28",
		PublishedAt:   "2026-07-30T12:00:00Z",
		Artifacts: []Artifact{{
			OS: runtime.GOOS, Arch: arch, SHA256: hex.EncodeToString(digest[:]), Size: int64(len(archive)),
		}},
	}
	manifestRaw, err := json.Marshal(manifest)
	if err != nil {
		t.Fatalf("marshal manifest: %v", err)
	}
	assets := map[string][]byte{
		"/qqqasdwx/Xray-core/releases/download/v26.7.28-xui.1/xray-manifest.json":        manifestRaw,
		"/qqqasdwx/Xray-core/releases/download/v26.7.28-xui.1/" + releaseAssetName(arch): archive,
	}
	server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
		raw, ok := assets[request.URL.Path]
		if !ok {
			http.NotFound(response, request)
			return
		}
		_, _ = response.Write(raw)
	}))
	defer server.Close()

	state := t.TempDir()
	manager := newTestManager(t, state, &recordingController{directory: Directory(state)}, &recordingRunner{})
	manager.releases, err = release.NewClient(server.URL, true)
	if err != nil {
		t.Fatalf("release.NewClient: %v", err)
	}
	result, err := manager.Apply(context.Background(), Request{CommandID: "release-command", Version: manifest.Version})
	if err != nil || !result.Success() || result.Version != manifest.Version {
		t.Fatalf("Apply result=%+v err=%v", result, err)
	}
	entries, err := os.ReadDir(downloadTemp)
	if err != nil {
		t.Fatalf("read download temp: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("Xray release temporary files were not cleaned: %v", entries)
	}
}

func TestManagerRejectsSymlinkedInstalledBundleFile(t *testing.T) {
	state := t.TempDir()
	manager := newTestManager(t, state, &recordingController{directory: Directory(state)}, &recordingRunner{})
	installVersion(t, manager, "v1.0.0")
	applied, err := manager.Current()
	if err != nil {
		t.Fatalf("Current before corruption: %v", err)
	}
	filePath := filepath.Join(manager.directory, applied.Target, "geosite.dat")
	if err := os.Remove(filePath); err != nil {
		t.Fatalf("remove installed file: %v", err)
	}
	if err := os.Symlink("geoip.dat", filePath); err != nil {
		t.Fatalf("replace installed file with symlink: %v", err)
	}
	if _, err := manager.Current(); err == nil || !strings.Contains(err.Error(), "unsafe permissions or type") {
		t.Fatalf("Current with symlink error = %v", err)
	}
}

func TestManagerInstallsCompleteBundleBeforeConfigExists(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	runner := &recordingRunner{}
	manager := newTestManager(t, state, controller, runner)
	installVersion(t, manager, "v1.0.0")

	applied, err := manager.Current()
	if err != nil || applied.Version != "v1.0.0" {
		t.Fatalf("Current=%+v err=%v", applied, err)
	}
	for _, name := range requiredBundleFiles {
		if _, err := os.Stat(filepath.Join(Directory(state), applied.Target, name)); err != nil {
			t.Fatalf("bundle file %s: %v", name, err)
		}
	}
	if controller.stops != 0 || controller.restarts != 0 || runner.configChecks != 0 {
		t.Fatalf("unexpected process activity: controller=%+v config_checks=%d", controller, runner.configChecks)
	}
}

func TestManagerSeparatesReleaseTagFromReportedXrayVersion(t *testing.T) {
	state := t.TempDir()
	runner := &recordingRunner{}
	manager := newTestManager(t, state, &recordingController{directory: Directory(state)}, runner)
	archive := testArchive(t, "fork-build")
	digest := sha256.Sum256(archive)
	result, err := manager.install(
		context.Background(), "command-fork", "v26.7.28-xui.1", "26.7.28",
		hex.EncodeToString(digest[:]), archive,
	)
	if err != nil || !result.Success() || result.Version != "v26.7.28-xui.1" {
		t.Fatalf("install result=%+v err=%v", result, err)
	}
	if len(runner.versions) != 1 || runner.versions[0] != "26.7.28" {
		t.Fatalf("validated versions=%v, want runtime version 26.7.28", runner.versions)
	}
	applied, err := manager.Current()
	if err != nil || applied.Version != "v26.7.28-xui.1" || applied.XrayVersion != "26.7.28" {
		t.Fatalf("applied=%+v err=%v", applied, err)
	}
}

func TestManagerRejectsExistingBundleWithDifferentRuntimeVersion(t *testing.T) {
	state := t.TempDir()
	manager := newTestManager(t, state, &recordingController{directory: Directory(state)}, &recordingRunner{})
	archive := testArchive(t, "fork-build")
	digest := sha256.Sum256(archive)
	archiveDigest := hex.EncodeToString(digest[:])
	if _, err := manager.install(context.Background(), "command-first", "v26.7.28-xui.1", "26.7.28", archiveDigest, archive); err != nil {
		t.Fatalf("first install: %v", err)
	}
	if _, err := manager.install(context.Background(), "command-conflict", "v26.7.28-xui.1", "26.7.29", archiveDigest, archive); err == nil {
		t.Fatal("existing bundle was accepted with different runtime version metadata")
	}
}

func TestManagerRejectsAppliedMetadataThatDiffersFromBundle(t *testing.T) {
	state := t.TempDir()
	manager := newTestManager(t, state, &recordingController{directory: Directory(state)}, &recordingRunner{})
	installVersion(t, manager, "v1.0.0")
	applied, err := manager.loadApplied()
	if err != nil {
		t.Fatalf("loadApplied: %v", err)
	}
	applied.XrayVersion = "v2.0.0"
	if err := writeJSONAtomic(manager.appliedPath(), applied, stateFileMode); err != nil {
		t.Fatalf("write applied: %v", err)
	}
	if _, err := manager.Current(); err == nil {
		t.Fatal("applied state was accepted with metadata that differs from the installed bundle")
	}
}

func TestManagerValidatesConfigThenSwitchesAndRestarts(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	runner := &recordingRunner{}
	manager := newTestManager(t, state, controller, runner)
	installVersion(t, manager, "v1.0.0")
	first, err := manager.Current()
	if err != nil {
		t.Fatalf("Current first: %v", err)
	}
	writeAppliedConfig(t, state)
	installVersion(t, manager, "v1.1.0")

	current, err := manager.Current()
	if err != nil || current.Version != "v1.1.0" {
		t.Fatalf("Current=%+v err=%v", current, err)
	}
	previous, err := os.Readlink(manager.previousPath())
	if err != nil || previous != first.Target {
		t.Fatalf("previous=%q err=%v want=%q", previous, err, first.Target)
	}
	if runner.configChecks != 1 || controller.preflights != 1 || controller.stops != 1 || controller.restarts != 1 {
		t.Fatalf("runner=%+v controller=%+v", runner, controller)
	}
}

func TestManagerRestoresPreviousBundleWhenUpdatedXrayFails(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	runner := &recordingRunner{}
	manager := newTestManager(t, state, controller, runner)
	installVersion(t, manager, "v1.0.0")
	writeAppliedConfig(t, state)
	controller.failTarget = "v1.1.0-"
	result, err := manager.InstallLocal(context.Background(), "command-v1.1.0", "v1.1.0", testArchive(t, "v1.1.0"))
	if err != nil || result.Success() || !result.RolledBack || result.RecoveryStatus != RecoveryStatusRolledBack {
		t.Fatalf("failed update result=%+v err=%v", result, err)
	}
	current, err := manager.Current()
	if err != nil || current.Version != "v1.0.0" {
		t.Fatalf("Current after rollback=%+v err=%v", current, err)
	}
	if controller.restarts != 2 || controller.stops != 2 {
		t.Fatalf("controller=%+v", controller)
	}
}

func TestManagerRestartsBootstrapXrayWhenFirstBundleFails(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{
		directory:             Directory(state),
		failTarget:            "v1.0.0-",
		allowBootstrapRestart: true,
	}
	manager := newTestManager(t, state, controller, &recordingRunner{})
	writeAppliedConfig(t, state)

	result, err := manager.InstallLocal(context.Background(), "command-v1.0.0", "v1.0.0", testArchive(t, "v1.0.0"))
	if err != nil || result.Success() || !result.RolledBack || result.RecoveryStatus != RecoveryStatusRolledBack {
		t.Fatalf("failed bootstrap result=%+v err=%v", result, err)
	}
	if controller.stops != 2 || controller.restarts != 2 {
		t.Fatalf("controller=%+v, want candidate attempt and bootstrap recovery", controller)
	}
	if _, err := os.Lstat(manager.currentPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed candidate remained selected: %v", err)
	}
	if _, err := manager.Current(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed candidate was recorded as applied: %v", err)
	}
}

func TestRollbackRemovesVisibleInitialAppliedStateBeforeBootstrapRestart(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state), allowBootstrapRestart: true}
	manager := newTestManager(t, state, controller, &recordingRunner{})
	writeAppliedConfig(t, state)

	archive := testArchive(t, "v1.0.0")
	digest := sha256.Sum256(archive)
	archiveDigest := hex.EncodeToString(digest[:])
	candidate, err := extractBundle(archive)
	if err != nil {
		t.Fatalf("extract candidate: %v", err)
	}
	target, err := manager.installBundle("v1.0.0", "v1.0.0", archiveDigest, candidate)
	if err != nil {
		t.Fatalf("install candidate: %v", err)
	}
	pending := pendingState{
		CommandID: "persistence-boundary", Target: target, Version: "v1.0.0",
		XrayVersion: "v1.0.0", ArchiveDigest: archiveDigest, StartedAt: 1,
	}
	if err := atomicSymlink(target, manager.currentPath()); err != nil {
		t.Fatalf("select candidate: %v", err)
	}
	if err := writeJSONAtomic(manager.appliedPath(), AppliedState{
		Version: "v1.0.0", XrayVersion: "v1.0.0", ArchiveDigest: archiveDigest, Target: target, AppliedAt: 1,
	}, stateFileMode); err != nil {
		t.Fatalf("write visible candidate applied state: %v", err)
	}
	if err := manager.rollback(context.Background(), pending, filepath.Join(xrayruntime.Directory(state), "current.json"), "synthetic persistence failure", ErrorCodePersistenceFailed); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if _, err := os.Lstat(manager.currentPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate current remains: %v", err)
	}
	if _, err := os.Stat(manager.appliedPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("candidate applied state remains: %v", err)
	}
	if controller.restarts != 1 {
		t.Fatalf("bootstrap restart count = %d, want 1", controller.restarts)
	}
}

func TestRollbackRestoresPreviousAppliedStateAfterVisibleCandidateWrite(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	manager := newTestManager(t, state, controller, &recordingRunner{})
	installVersion(t, manager, "v1.0.0")
	previous, err := manager.Current()
	if err != nil {
		t.Fatalf("load first state: %v", err)
	}
	writeAppliedConfig(t, state)

	archive := testArchive(t, "v1.1.0")
	digest := sha256.Sum256(archive)
	archiveDigest := hex.EncodeToString(digest[:])
	candidate, err := extractBundle(archive)
	if err != nil {
		t.Fatalf("extract candidate: %v", err)
	}
	target, err := manager.installBundle("v1.1.0", "v1.1.0", archiveDigest, candidate)
	if err != nil {
		t.Fatalf("install candidate: %v", err)
	}
	pending := pendingState{
		CommandID: "persistence-boundary", PreviousTarget: previous.Target,
		PreviousApplied: previous, Target: target, Version: "v1.1.0",
		XrayVersion: "v1.1.0", ArchiveDigest: archiveDigest, StartedAt: 1,
	}
	if err := atomicSymlink(target, manager.currentPath()); err != nil {
		t.Fatalf("select candidate: %v", err)
	}
	if err := writeJSONAtomic(manager.appliedPath(), AppliedState{
		Version: "v1.1.0", XrayVersion: "v1.1.0", ArchiveDigest: archiveDigest, Target: target, AppliedAt: 2,
	}, stateFileMode); err != nil {
		t.Fatalf("write visible candidate applied state: %v", err)
	}
	if err := manager.rollback(context.Background(), pending, filepath.Join(xrayruntime.Directory(state), "current.json"), "synthetic persistence failure", ErrorCodePersistenceFailed); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	restored, err := manager.Current()
	if err != nil || restored.Target != previous.Target || restored.Version != previous.Version || restored.ArchiveDigest != previous.ArchiveDigest {
		t.Fatalf("restored state=%+v err=%v, want %+v", restored, err, previous)
	}
}

func TestRecoverRollsBackVisibleCandidateWhenCurrentLinkIsMissing(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	manager := newTestManager(t, state, controller, &recordingRunner{})
	installVersion(t, manager, "v1.0.0")
	previous, err := manager.Current()
	if err != nil {
		t.Fatalf("load first state: %v", err)
	}
	writeAppliedConfig(t, state)

	archive := testArchive(t, "v1.1.0")
	digest := sha256.Sum256(archive)
	archiveDigest := hex.EncodeToString(digest[:])
	candidate, err := extractBundle(archive)
	if err != nil {
		t.Fatalf("extract candidate: %v", err)
	}
	target, err := manager.installBundle("v1.1.0", "v1.1.0", archiveDigest, candidate)
	if err != nil {
		t.Fatalf("install candidate: %v", err)
	}
	pending := pendingState{
		CommandID: "interrupted-persistence", PreviousTarget: previous.Target,
		PreviousApplied: previous, Target: target, Version: "v1.1.0",
		XrayVersion: "v1.1.0", ArchiveDigest: archiveDigest, StartedAt: 1,
	}
	if err := writeJSONAtomic(manager.pendingPath(), pending, stateFileMode); err != nil {
		t.Fatalf("write pending: %v", err)
	}
	if err := writeJSONAtomic(manager.appliedPath(), AppliedState{
		Version: "v1.1.0", XrayVersion: "v1.1.0", ArchiveDigest: archiveDigest, Target: target, AppliedAt: 2,
	}, stateFileMode); err != nil {
		t.Fatalf("write visible candidate applied state: %v", err)
	}
	if err := os.Remove(manager.currentPath()); err != nil {
		t.Fatalf("remove current link: %v", err)
	}

	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	restored, err := manager.Current()
	if err != nil || restored.Target != previous.Target || restored.Version != previous.Version {
		t.Fatalf("restored state=%+v err=%v, want %+v", restored, err, previous)
	}
	if controller.stops != 1 || controller.restarts != 1 {
		t.Fatalf("controller=%+v, want one deterministic rollback", controller)
	}
}

func TestManagerRecoversUnconfirmedBinarySwitch(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	runner := &recordingRunner{}
	manager := newTestManager(t, state, controller, runner)
	installVersion(t, manager, "v1.0.0")
	first, err := manager.Current()
	if err != nil {
		t.Fatalf("Current first: %v", err)
	}
	writeAppliedConfig(t, state)
	archive := testArchive(t, "v1.1.0")
	digest := sha256.Sum256(archive)
	candidate, err := extractBundle(archive)
	if err != nil {
		t.Fatalf("extractBundle: %v", err)
	}
	target, err := manager.installBundle("v1.1.0", "v1.1.0", hex.EncodeToString(digest[:]), candidate)
	if err != nil {
		t.Fatalf("installBundle: %v", err)
	}
	pending := pendingState{
		CommandID: "interrupted", PreviousTarget: first.Target, Target: target,
		Version: "v1.1.0", XrayVersion: "v1.1.0", ArchiveDigest: hex.EncodeToString(digest[:]), StartedAt: 1,
	}
	if err := writeJSONAtomic(manager.pendingPath(), pending, 0o600); err != nil {
		t.Fatalf("write pending: %v", err)
	}
	if err := atomicSymlink(target, manager.currentPath()); err != nil {
		t.Fatalf("switch current: %v", err)
	}
	if err := manager.Recover(context.Background()); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	current, err := manager.Current()
	if err != nil || current.Version != "v1.0.0" {
		t.Fatalf("Current after recover=%+v err=%v", current, err)
	}
	if _, err := os.Stat(manager.pendingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending remains: %v", err)
	}
}

func TestManagerRecoveryAtDurableBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name            string
		switchCurrent   bool
		recordPrevious  bool
		persistApplied  bool
		wantVersion     string
		wantProcessWork bool
		wantFailed      bool
	}{
		{name: "pending before switch", wantVersion: "v1.0.0", wantProcessWork: true, wantFailed: true},
		{name: "current switched before previous", switchCurrent: true, wantVersion: "v1.0.0", wantProcessWork: true, wantFailed: true},
		{name: "previous recorded before applied", switchCurrent: true, recordPrevious: true, wantVersion: "v1.0.0", wantProcessWork: true, wantFailed: true},
		{name: "applied before pending removal", switchCurrent: true, recordPrevious: true, persistApplied: true, wantVersion: "v1.1.0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			controller := &recordingController{directory: Directory(state)}
			manager := newTestManager(t, state, controller, &recordingRunner{})
			installVersion(t, manager, "v1.0.0")
			previous, err := manager.Current()
			if err != nil {
				t.Fatalf("load previous state: %v", err)
			}
			writeAppliedConfig(t, state)

			archive := testArchive(t, "v1.1.0")
			digest := sha256.Sum256(archive)
			archiveDigest := hex.EncodeToString(digest[:])
			candidate, err := extractBundle(archive)
			if err != nil {
				t.Fatalf("extract candidate: %v", err)
			}
			target, err := manager.installBundle("v1.1.0", "v1.1.0", archiveDigest, candidate)
			if err != nil {
				t.Fatalf("install candidate: %v", err)
			}
			pending := pendingState{
				CommandID: "durable-boundary", PreviousTarget: previous.Target, PreviousApplied: previous,
				Target: target, Version: "v1.1.0", XrayVersion: "v1.1.0", ArchiveDigest: archiveDigest, StartedAt: 2,
			}
			if err := writeJSONAtomic(manager.pendingPath(), pending, stateFileMode); err != nil {
				t.Fatalf("write pending: %v", err)
			}
			if tc.switchCurrent {
				if err := atomicSymlink(target, manager.currentPath()); err != nil {
					t.Fatalf("switch current: %v", err)
				}
			}
			if tc.recordPrevious {
				if err := atomicSymlink(previous.Target, manager.previousPath()); err != nil {
					t.Fatalf("record previous: %v", err)
				}
			}
			if tc.persistApplied {
				if err := writeJSONAtomic(manager.appliedPath(), AppliedState{
					Version: "v1.1.0", XrayVersion: "v1.1.0", ArchiveDigest: archiveDigest, Target: target, AppliedAt: 2,
				}, stateFileMode); err != nil {
					t.Fatalf("persist candidate state: %v", err)
				}
			}

			if err := manager.Recover(context.Background()); err != nil {
				t.Fatalf("Recover: %v", err)
			}
			current, err := manager.Current()
			if err != nil || current.Version != tc.wantVersion {
				t.Fatalf("current=%+v err=%v, want %s", current, err, tc.wantVersion)
			}
			wantProcessCalls := 0
			if tc.wantProcessWork {
				wantProcessCalls = 1
			}
			if controller.stops != wantProcessCalls || controller.restarts != wantProcessCalls {
				t.Fatalf("controller=%+v, want stops/restarts=%d", controller, wantProcessCalls)
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

func TestManagerRepeatedCommandDoesNotRestartAppliedBundle(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	manager := newTestManager(t, state, controller, &recordingRunner{})
	installVersion(t, manager, "v1.0.0")
	writeAppliedConfig(t, state)
	archive := testArchive(t, "v1.1.0")
	first, err := manager.InstallLocal(context.Background(), "command-first", "v1.1.0", archive)
	if err != nil || !first.Success() {
		t.Fatalf("first install result=%+v err=%v", first, err)
	}
	stops, restarts := controller.stops, controller.restarts
	second, err := manager.InstallLocal(context.Background(), "command-retry", "v1.1.0", archive)
	if err != nil || !second.Success() {
		t.Fatalf("repeated install result=%+v err=%v", second, err)
	}
	if controller.stops != stops || controller.restarts != restarts {
		t.Fatalf("repeated command changed process counts from (%d,%d) to (%d,%d)", stops, restarts, controller.stops, controller.restarts)
	}
}

func TestManagerReportsStorageFailureBeforeBinarySwitch(t *testing.T) {
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
			manager := newTestManager(t, state, controller, &recordingRunner{})
			installVersion(t, manager, "v1.0.0")
			first, err := manager.Current()
			if err != nil {
				t.Fatalf("load first state: %v", err)
			}
			writeAppliedConfig(t, state)
			manager.writeState = func(path string, value any, mode os.FileMode) error {
				if path == manager.pendingPath() {
					return tc.err
				}
				return writeJSONAtomic(path, value, mode)
			}

			if _, err := manager.InstallLocal(context.Background(), "command-v1.1.0", "v1.1.0", testArchive(t, "v1.1.0")); err == nil || !errors.Is(err, tc.err) {
				t.Fatalf("storage failure = %v, want %v", err, tc.err)
			} else if code, recovery := ErrorDetails(err); code != ErrorCodePreparationFailed || recovery != RecoveryStatusNotRequired {
				t.Fatalf("storage failure details=(%q,%q)", code, recovery)
			}
			current, err := manager.Current()
			if err != nil || current.Target != first.Target || current.Version != first.Version {
				t.Fatalf("current=%+v err=%v, want unchanged %+v", current, err, first)
			}
			if controller.stops != 0 || controller.restarts != 0 {
				t.Fatalf("storage failure changed process state: %+v", controller)
			}
			if _, err := os.Stat(manager.pendingPath()); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pending state exists after failed persistence: %v", err)
			}
		})
	}
}

func TestManagerPreflightRejectsCorruptPIDBeforeBinarySwitch(t *testing.T) {
	state := t.TempDir()
	controller := xrayruntime.NewProcessController(state, "/bin/sh")
	manager := newTestManager(t, state, controller, &recordingRunner{})
	installVersion(t, manager, "v1.0.0")
	first, err := manager.Current()
	if err != nil {
		t.Fatalf("load first state: %v", err)
	}
	writeAppliedConfig(t, state)
	if err := os.WriteFile(xrayruntime.PIDPath(state), []byte("not-a-pid\n"), stateFileMode); err != nil {
		t.Fatalf("write corrupt pid: %v", err)
	}

	if _, err := manager.InstallLocal(context.Background(), "command-v1.1.0", "v1.1.0", testArchive(t, "v1.1.0")); err == nil || !strings.Contains(err.Error(), "pid file is invalid") {
		t.Fatalf("corrupt pid error = %v", err)
	} else if code, recovery := ErrorDetails(err); code != ErrorCodePreparationFailed || recovery != RecoveryStatusNotRequired {
		t.Fatalf("corrupt pid details=(%q,%q)", code, recovery)
	}
	current, err := manager.Current()
	if err != nil || current.Target != first.Target {
		t.Fatalf("current=%+v err=%v, want unchanged %+v", current, err, first)
	}
	if _, err := os.Stat(manager.pendingPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("pending state exists after failed preflight: %v", err)
	}
}

func TestManagerRecoveryDeselectsCandidateWhenRollbackBundleIsMissing(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	manager := newTestManager(t, state, controller, &recordingRunner{})
	installVersion(t, manager, "v1.0.0")
	previous, err := manager.Current()
	if err != nil {
		t.Fatalf("load previous state: %v", err)
	}
	writeAppliedConfig(t, state)
	archive := testArchive(t, "v1.1.0")
	digest := sha256.Sum256(archive)
	archiveDigest := hex.EncodeToString(digest[:])
	candidate, err := extractBundle(archive)
	if err != nil {
		t.Fatalf("extract candidate: %v", err)
	}
	target, err := manager.installBundle("v1.1.0", "v1.1.0", archiveDigest, candidate)
	if err != nil {
		t.Fatalf("install candidate: %v", err)
	}
	pending := pendingState{
		CommandID: "missing-rollback", PreviousTarget: previous.Target, PreviousApplied: previous,
		Target: target, Version: "v1.1.0", XrayVersion: "v1.1.0", ArchiveDigest: archiveDigest, StartedAt: 2,
	}
	if err := writeJSONAtomic(manager.pendingPath(), pending, stateFileMode); err != nil {
		t.Fatalf("write pending: %v", err)
	}
	if err := atomicSymlink(target, manager.currentPath()); err != nil {
		t.Fatalf("switch current: %v", err)
	}
	if err := removeBundleDirectory(filepath.Join(manager.directory, previous.Target)); err != nil {
		t.Fatalf("remove rollback bundle: %v", err)
	}

	err = manager.Recover(context.Background())
	if err == nil || !strings.Contains(err.Error(), "verify previous Xray binary") {
		t.Fatalf("missing rollback error = %v", err)
	}
	if code, recovery := ErrorDetails(err); code != ErrorCodeRecoveryFailed || recovery != RecoveryStatusFailed {
		t.Fatalf("missing rollback details=(%q,%q)", code, recovery)
	}
	if _, err := os.Lstat(manager.currentPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed candidate remained selected: %v", err)
	}
	if _, err := os.Stat(manager.pendingPath()); err != nil {
		t.Fatalf("pending recovery point was not retained: %v", err)
	}
	failure, err := LoadFailureState(state)
	if err != nil || failure.RecoveryStatus != RecoveryStatusFailed {
		t.Fatalf("failure=%+v err=%v", failure, err)
	}
	if controller.stops != 1 || controller.restarts != 0 {
		t.Fatalf("controller=%+v, want stopped candidate without restart", controller)
	}
}

func TestManagerIdempotentInstallClearsPreviousFailure(t *testing.T) {
	state := t.TempDir()
	controller := &recordingController{directory: Directory(state)}
	manager := newTestManager(t, state, controller, &recordingRunner{})
	archive := testArchive(t, "v1.0.0")
	first, err := manager.InstallLocal(context.Background(), "command-first", "v1.0.0", archive)
	if err != nil || !first.Success() {
		t.Fatalf("first install result=%+v err=%v", first, err)
	}
	if err := writeJSONAtomic(manager.failedPath(), Result{
		Version: "v1.0.0", Status: StatusInstallFailed, Error: "old failure",
		ErrorCode: ErrorCodeActivationFailed, RecoveryStatus: RecoveryStatusRolledBack,
	}, stateFileMode); err != nil {
		t.Fatalf("write failed state: %v", err)
	}
	second, err := manager.InstallLocal(context.Background(), "command-retry", "v1.0.0", archive)
	if err != nil || !second.Success() {
		t.Fatalf("idempotent install result=%+v err=%v", second, err)
	}
	if _, err := os.Stat(manager.failedPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed state remains after idempotent success: %v", err)
	}
}

func TestManagerPruneProtectsCurrentPreviousAndPendingTargets(t *testing.T) {
	state := t.TempDir()
	manager := newTestManager(t, state, &recordingController{directory: Directory(state)}, &recordingRunner{})
	if err := manager.ensureDirectory(); err != nil {
		t.Fatalf("ensureDirectory: %v", err)
	}
	targets := make(map[string]string)
	digests := make(map[string]string)
	baseTime := time.Unix(1_700_000_000, 0)
	for index, version := range []string{"v0", "v1", "v2", "v3", "v4", "v5", "v6"} {
		archive := testArchive(t, version)
		digest := sha256.Sum256(archive)
		digests[version] = hex.EncodeToString(digest[:])
		candidate, err := extractBundle(archive)
		if err != nil {
			t.Fatalf("extract %s: %v", version, err)
		}
		target, err := manager.installBundle(version, version, digests[version], candidate)
		if err != nil {
			t.Fatalf("install bundle %s: %v", version, err)
		}
		targets[version] = target
		stamp := baseTime.Add(time.Duration(index) * time.Second)
		if err := os.Chtimes(filepath.Join(manager.directory, target), stamp, stamp); err != nil {
			t.Fatalf("set %s time: %v", version, err)
		}
	}
	if err := os.Symlink(targets["v1"], manager.currentPath()); err != nil {
		t.Fatalf("link current: %v", err)
	}
	if err := os.Symlink(targets["v2"], manager.previousPath()); err != nil {
		t.Fatalf("link previous: %v", err)
	}
	if err := writeJSONAtomic(manager.appliedPath(), AppliedState{
		Version: "v1", XrayVersion: "v1", ArchiveDigest: digests["v1"], Target: targets["v1"], AppliedAt: 1,
	}, stateFileMode); err != nil {
		t.Fatalf("write applied: %v", err)
	}
	if err := writeJSONAtomic(manager.pendingPath(), pendingState{
		CommandID: "pending", PreviousTarget: targets["v2"], Target: targets["v3"],
		Version: "v3", XrayVersion: "v3", ArchiveDigest: digests["v3"], StartedAt: 1,
	}, stateFileMode); err != nil {
		t.Fatalf("write pending: %v", err)
	}
	if err := manager.pruneVersions(); err != nil {
		t.Fatalf("pruneVersions: %v", err)
	}
	for _, version := range []string{"v1", "v2", "v3", "v4", "v5", "v6"} {
		if _, err := os.Stat(filepath.Join(manager.directory, targets[version])); err != nil {
			t.Fatalf("required version %s was pruned: %v", version, err)
		}
	}
	if _, err := os.Stat(filepath.Join(manager.directory, targets["v0"])); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("old unprotected version was not pruned: %v", err)
	}
}

func TestExtractBundleRejectsUnexpectedEntries(t *testing.T) {
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	entry, err := writer.Create("nested/xray")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, _ = entry.Write([]byte("bad"))
	if err := writer.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := extractBundle(output.Bytes()); err == nil {
		t.Fatal("archive with nested path was accepted")
	}
}

func TestExecRunnerDoesNotExposeFailedCommandOutput(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "xray")
	secret := "client-credential-must-not-leak"
	script := "#!/bin/sh\necho '" + secret + "' >&2\nexit 23\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake xray: %v", err)
	}
	configPath := filepath.Join(directory, "config.json")
	if err := os.WriteFile(configPath, []byte(`{"inbounds":[]}`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	for _, tc := range []struct {
		name string
		run  func() error
	}{
		{name: "version", run: func() error { return (ExecRunner{}).ValidateVersion(context.Background(), binary, "v1") }},
		{name: "config", run: func() error { return (ExecRunner{}).ValidateConfig(context.Background(), binary, configPath) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil || !strings.Contains(err.Error(), "exit status 23") {
				t.Fatalf("command error = %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("command error exposed Xray output: %v", err)
			}
		})
	}
}
