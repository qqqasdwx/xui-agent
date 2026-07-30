package xrayconfig

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
)

type recordingRunner struct {
	calls int
	err   error
}

type pathRecordingRunner struct {
	paths []string
}

func (r *pathRecordingRunner) Validate(_ context.Context, binaryPath, _ string) error {
	r.paths = append(r.paths, binaryPath)
	return nil
}

func (r *recordingRunner) Validate(_ context.Context, binaryPath, configPath string) error {
	r.calls++
	if binaryPath != "/opt/xray/xray" {
		return errors.New("unexpected binary path")
	}
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	if string(raw) != `{"inbounds":[]}` {
		return errors.New("unexpected candidate content")
	}
	return r.err
}

func request(version uint64, config string) Request {
	sum := sha256.Sum256([]byte(config))
	return Request{ConfigVersion: version, ConfigDigest: hex.EncodeToString(sum[:]), Config: json.RawMessage(config)}
}

func TestManagerValidatesAndPersistsCandidate(t *testing.T) {
	directory := t.TempDir()
	runner := &recordingRunner{}
	manager := NewManager(directory, "/opt/xray/xray", runner)

	result, err := manager.Validate(context.Background(), request(2, `{"inbounds":[]}`))
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if !result.Success() || result.ConfigVersion != 2 || runner.calls != 1 {
		t.Fatalf("unexpected result=%+v calls=%d", result, runner.calls)
	}
	for _, name := range []string{"candidate.json", "validation.json"} {
		info, err := os.Stat(filepath.Join(directory, "xray-config", name))
		if err != nil {
			t.Fatalf("stat %s: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", name, info.Mode().Perm())
		}
	}

	duplicate, err := manager.Validate(context.Background(), request(2, `{"inbounds":[]}`))
	if err != nil || !duplicate.Success() || runner.calls != 1 {
		t.Fatalf("duplicate result=%+v err=%v calls=%d", duplicate, err, runner.calls)
	}
}

func TestManagedManagerSwitchesFromBootstrapToSelectedRuntime(t *testing.T) {
	state := t.TempDir()
	bootstrap := "/opt/xray/bootstrap"
	runner := &pathRecordingRunner{}
	manager := NewManagedManager(state, bootstrap, runner)
	if _, err := manager.Validate(context.Background(), request(1, `{"inbounds":[]}`)); err != nil {
		t.Fatalf("validate with bootstrap: %v", err)
	}

	runtimeDirectory := filepath.Join(state, "xray-runtime")
	if err := os.MkdirAll(filepath.Join(runtimeDirectory, "versions", "v1"), 0o700); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}
	if err := os.Symlink("versions/v1", filepath.Join(runtimeDirectory, "current")); err != nil {
		t.Fatalf("select managed runtime: %v", err)
	}
	if _, err := manager.Validate(context.Background(), request(2, `{"inbounds":[]}`)); err != nil {
		t.Fatalf("validate with managed runtime: %v", err)
	}

	wantManaged := filepath.Join(runtimeDirectory, "current", "xray")
	if len(runner.paths) != 2 || runner.paths[0] != bootstrap || runner.paths[1] != wantManaged {
		t.Fatalf("validation paths = %v, want [%s %s]", runner.paths, bootstrap, wantManaged)
	}
}

func TestManagerRejectsOldOrConflictingVersions(t *testing.T) {
	runner := &recordingRunner{}
	manager := NewManager(t.TempDir(), "/opt/xray/xray", runner)
	if _, err := manager.Validate(context.Background(), request(3, `{"inbounds":[]}`)); err != nil {
		t.Fatalf("initial Validate: %v", err)
	}
	if _, err := manager.Validate(context.Background(), request(2, `{"inbounds":[]}`)); err == nil || !strings.Contains(err.Error(), "older") {
		t.Fatalf("old version error = %v", err)
	}
	if _, err := manager.Validate(context.Background(), request(3, `{"inbounds":[{}]}`)); err == nil || !strings.Contains(err.Error(), "different digest") {
		t.Fatalf("conflicting digest error = %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}

func TestManagerPersistsFailedValidationIdempotently(t *testing.T) {
	runner := &recordingRunner{err: errors.New("synthetic validation failure")}
	directory := t.TempDir()
	manager := NewManager(directory, "/opt/xray/xray", runner)
	want := request(4, `{"inbounds":[]}`)
	result, err := manager.Validate(context.Background(), want)
	if err != nil {
		t.Fatalf("Validate: %v", err)
	}
	if result.Success() || result.Status != StatusFailed || !strings.Contains(result.Error, "synthetic") {
		t.Fatalf("unexpected failed result: %+v", result)
	}

	reloaded := NewManager(directory, "/opt/xray/xray", &recordingRunner{})
	duplicate, err := reloaded.Validate(context.Background(), want)
	if err != nil || duplicate.Status != StatusFailed || duplicate.Error != result.Error {
		t.Fatalf("reloaded duplicate=%+v err=%v", duplicate, err)
	}
}

func TestManagerRejectsInvalidDigestBeforeWriting(t *testing.T) {
	directory := t.TempDir()
	manager := NewManager(directory, "/opt/xray/xray", &recordingRunner{})
	bad := request(1, `{"inbounds":[]}`)
	bad.ConfigDigest = strings.Repeat("0", 64)
	if _, err := manager.Validate(context.Background(), bad); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("digest error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(directory, "xray-config")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("state directory exists after rejected payload: %v", err)
	}
}

func TestExecRunnerInvokesXrayConfigTest(t *testing.T) {
	directory := t.TempDir()
	binary := filepath.Join(directory, "xray")
	script := "#!/bin/sh\n[ \"$1\" = run ] && [ \"$2\" = -test ] && [ \"$3\" = -config ] && [ -f \"$4\" ]\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake xray: %v", err)
	}
	configPath := filepath.Join(directory, "candidate.json")
	if err := os.WriteFile(configPath, []byte(`{"inbounds":[]}`), 0o600); err != nil {
		t.Fatalf("write candidate: %v", err)
	}
	if err := (ExecRunner{}).Validate(context.Background(), binary, configPath); err != nil {
		t.Fatalf("ExecRunner.Validate: %v", err)
	}
}
