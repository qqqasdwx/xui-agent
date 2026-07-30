package xraybinary

import (
	"os"
	"path/filepath"
	"testing"
)

func TestActivePathUsesBootstrapUntilManagedRuntimeIsSelected(t *testing.T) {
	state := t.TempDir()
	bootstrap := "/opt/xray/bootstrap"
	if got := ActivePath(state, bootstrap); got != bootstrap {
		t.Fatalf("ActivePath before bootstrap = %q, want %q", got, bootstrap)
	}

	directory := filepath.Join(state, runtimeDirectory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}
	if err := os.Symlink("versions/v1", filepath.Join(directory, currentName)); err != nil {
		t.Fatalf("create current link: %v", err)
	}
	if got := ActivePath(state, bootstrap); got != ManagedPath(state) {
		t.Fatalf("ActivePath with current = %q, want managed path", got)
	}
}

func TestActivePathDoesNotMaskMissingAppliedRuntime(t *testing.T) {
	state := t.TempDir()
	directory := filepath.Join(state, runtimeDirectory)
	if err := os.MkdirAll(directory, 0o700); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(directory, appliedName), []byte("{}\n"), 0o600); err != nil {
		t.Fatalf("write applied marker: %v", err)
	}
	if got := ActivePath(state, "/opt/xray/bootstrap"); got != ManagedPath(state) {
		t.Fatalf("ActivePath with applied marker = %q, want managed path", got)
	}
}
