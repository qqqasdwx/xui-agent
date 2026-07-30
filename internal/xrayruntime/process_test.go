package xrayruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestRunManagedProcessStopsChildAndRemovesPID(t *testing.T) {
	state := t.TempDir()
	directory := Directory(state)
	versions := filepath.Join(directory, versionsName)
	if err := os.MkdirAll(versions, stateDirectoryMode); err != nil {
		t.Fatalf("create versions: %v", err)
	}
	config := []byte(`{"inbounds":[]}`)
	digest := sha256.Sum256(config)
	target := filepath.Join(versionsName, "00000000000000000001-"+hex.EncodeToString(digest[:])+".json")
	if err := os.WriteFile(filepath.Join(directory, target), config, configFileMode); err != nil {
		t.Fatalf("write config: %v", err)
	}
	if err := os.Symlink(target, CurrentConfigPath(state)); err != nil {
		t.Fatalf("create current link: %v", err)
	}
	binary := filepath.Join(t.TempDir(), "fake-xray")
	script := "#!/bin/sh\ntrap 'exit 0' TERM INT\nwhile :; do sleep 1; done\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake Xray: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- RunManagedProcess(ctx, state, binary) }()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(PIDPath(state)); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("managed process did not write its pid")
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunManagedProcess: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("managed process did not stop")
	}
	if _, err := os.Stat(PIDPath(state)); !os.IsNotExist(err) {
		t.Fatalf("pid file remains after stop: %v", err)
	}
}
