package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestValidateRequiresTLSUnlessExplicitlyAllowed(t *testing.T) {
	cfg := Config{ServerURL: "http://127.0.0.1:2053", StateDirectory: "/tmp/state"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("plain HTTP was accepted without allowInsecure")
	}
	cfg.AllowInsecure = true
	if err := cfg.Validate(); err != nil {
		t.Fatalf("explicit local HTTP config rejected: %v", err)
	}
}

func TestValidateRejectsRelativeStateAndXrayPaths(t *testing.T) {
	cfg := Config{ServerURL: "https://panel.example.com", StateDirectory: "state"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("relative stateDirectory was accepted")
	}
	cfg.StateDirectory = "/var/lib/xui-agent"
	cfg.Xray.BinaryPath = "xray"
	if err := cfg.Validate(); err == nil {
		t.Fatal("relative xray binary path was accepted")
	}
}

func TestValidateManagedXrayRequiresAgentOwnedPaths(t *testing.T) {
	cfg := Config{
		ServerURL:      "https://panel.example.com",
		StateDirectory: "/var/lib/xui-agent",
		Xray: XrayConfig{
			Mode:       XrayModeManaged,
			BinaryPath: "/usr/local/bin/xray",
		},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("managed config rejected: %v", err)
	}
	cfg.Xray.ConfigPath = "/etc/xray/config.json"
	if err := cfg.Validate(); err == nil {
		t.Fatal("managed config accepted an externally owned config path")
	}
}

func TestValidateRejectsCredentialsInServerURL(t *testing.T) {
	cfg := Config{ServerURL: "https://user:password@panel.example.com", StateDirectory: "/var/lib/xui-agent"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("serverUrl credentials were accepted")
	}
}

func TestLoadAcceptsRetiredReleasePublicKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	raw := []byte(`{"serverUrl":"https://panel.example.com","stateDirectory":"/var/lib/xui-agent","xray":{},"update":{"publicKey":"retired-value"}}`)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Update.PublicKey != "retired-value" {
		t.Fatalf("retired public key = %q", loaded.Update.PublicKey)
	}
}

func TestWriteIsAtomicAndDoesNotOverwriteByDefault(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config", "config.json")
	cfg := Config{ServerURL: "https://panel.example.com/base", StateDirectory: "/var/lib/xui-agent"}
	if err := Write(path, cfg, false); err != nil {
		t.Fatalf("Write: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o640 {
		t.Fatalf("config mode = %o, want 640", got)
	}
	loaded, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.ServerURL != cfg.ServerURL {
		t.Fatalf("server URL = %q, want %q", loaded.ServerURL, cfg.ServerURL)
	}
	if loaded.Update.RuntimeAssetsPath != DefaultRuntimeAssetsPath {
		t.Fatalf("runtime assets path = %q, want %q", loaded.Update.RuntimeAssetsPath, DefaultRuntimeAssetsPath)
	}
	if err := Write(path, cfg, false); err == nil {
		t.Fatal("Write overwrote an existing config without permission")
	}
	if err := Write(path, cfg, true); err != nil {
		t.Fatalf("Write overwrite: %v", err)
	}
}
