package identity

import (
	"os"
	"testing"
)

func TestStoreUsesOwnerOnlyPermissions(t *testing.T) {
	store := NewStore(t.TempDir())
	want := Identity{NodeID: 7, NodeName: "edge-1", Credential: "secret"}
	if err := store.Save(want); err != nil {
		t.Fatalf("Save: %v", err)
	}
	info, err := os.Stat(store.Path())
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("identity mode = %o, want 600", got)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got != want {
		t.Fatalf("identity = %+v, want %+v", got, want)
	}
}

func TestLoadRejectsBroadPermissions(t *testing.T) {
	store := NewStore(t.TempDir())
	if err := store.Save(Identity{NodeID: 7, Credential: "secret"}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if err := os.Chmod(store.Path(), 0o644); err != nil {
		t.Fatalf("Chmod: %v", err)
	}
	if _, err := store.Load(); err == nil {
		t.Fatal("Load accepted a group/world-readable identity")
	}
}
