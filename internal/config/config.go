package config

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
)

const (
	XrayModeObserve          = "observe"
	XrayModeManaged          = "managed"
	DefaultRuntimeAssetsPath = "/etc/xui-agent/runtime-assets.sha256"
)

type Config struct {
	ServerURL        string       `json:"serverUrl"`
	StateDirectory   string       `json:"stateDirectory"`
	AllowInsecure    bool         `json:"allowInsecure"`
	ServerCertSHA256 string       `json:"serverCertSha256,omitempty"`
	Xray             XrayConfig   `json:"xray"`
	Update           UpdateConfig `json:"update"`
}

type XrayConfig struct {
	Mode       string `json:"mode,omitempty"`
	BinaryPath string `json:"binaryPath"`
	ConfigPath string `json:"configPath"`
	PIDFile    string `json:"pidFile"`
}

func (c XrayConfig) Managed() bool {
	return c.Mode == XrayModeManaged
}

type UpdateConfig struct {
	PublicKey         string `json:"publicKey,omitempty"`
	RuntimeAssetsPath string `json:"runtimeAssetsPath,omitempty"`
}

func Load(path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %s: %w", path, err)
	}
	var cfg Config
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return Config{}, fmt.Errorf("decode config %s: multiple JSON values", path)
		}
		return Config{}, fmt.Errorf("decode config %s: %w", path, err)
	}
	if cfg.StateDirectory == "" {
		cfg.StateDirectory = "/var/lib/xui-agent"
	}
	if cfg.Xray.Mode == "" {
		cfg.Xray.Mode = XrayModeObserve
	}
	if cfg.Update.RuntimeAssetsPath == "" {
		cfg.Update.RuntimeAssetsPath = DefaultRuntimeAssetsPath
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func Write(path string, cfg Config, overwrite bool) error {
	if err := cfg.Validate(); err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if err := os.MkdirAll(directory, 0o750); err != nil {
		return fmt.Errorf("create config directory: %w", err)
	}
	if !overwrite {
		if _, err := os.Stat(path); err == nil {
			return fmt.Errorf("config %s already exists", path)
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect config %s: %w", path, err)
		}
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode config: %w", err)
	}
	raw = append(raw, '\n')
	tmp, err := os.CreateTemp(directory, ".config-*")
	if err != nil {
		return fmt.Errorf("create config temp file: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(0o640); err != nil {
		tmp.Close()
		return fmt.Errorf("secure config temp file: %w", err)
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return fmt.Errorf("write config: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync config: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close config: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("replace config: %w", err)
	}
	return nil
}

func (c Config) Validate() error {
	u, err := url.Parse(strings.TrimSpace(c.ServerURL))
	if err != nil || u.Host == "" {
		return errors.New("serverUrl must be an absolute URL")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return errors.New("serverUrl must not contain a query or fragment")
	}
	if u.User != nil {
		return errors.New("serverUrl must not contain credentials")
	}
	switch u.Scheme {
	case "https":
	case "http":
		if !c.AllowInsecure {
			return errors.New("plain HTTP requires allowInsecure=true")
		}
	default:
		return errors.New("serverUrl scheme must be https")
	}
	if !filepath.IsAbs(c.StateDirectory) {
		return errors.New("stateDirectory must be an absolute path")
	}
	xrayMode := c.Xray.Mode
	if xrayMode == "" {
		xrayMode = XrayModeObserve
	}
	if xrayMode != XrayModeObserve && xrayMode != XrayModeManaged {
		return errors.New("xray.mode must be observe or managed")
	}
	for name, value := range map[string]string{
		"xray.binaryPath": c.Xray.BinaryPath,
		"xray.configPath": c.Xray.ConfigPath,
		"xray.pidFile":    c.Xray.PIDFile,
	} {
		if value != "" && !filepath.IsAbs(value) {
			return fmt.Errorf("%s must be an absolute path", name)
		}
	}
	if xrayMode == XrayModeManaged {
		if c.Xray.BinaryPath == "" {
			return errors.New("xray.binaryPath is required in managed mode")
		}
		if c.Xray.ConfigPath != "" || c.Xray.PIDFile != "" {
			return errors.New("xray.configPath and xray.pidFile must be empty in managed mode")
		}
	}
	if c.Update.PublicKey != "" {
		raw, err := base64.StdEncoding.DecodeString(c.Update.PublicKey)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			return errors.New("update.publicKey must be a base64 Ed25519 public key")
		}
	}
	if c.Update.RuntimeAssetsPath != "" && !filepath.IsAbs(c.Update.RuntimeAssetsPath) {
		return errors.New("update.runtimeAssetsPath must be an absolute path")
	}
	return nil
}
