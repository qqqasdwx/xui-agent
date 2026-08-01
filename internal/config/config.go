package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/xraybinary"
)

const (
	XrayModeObserve          = "observe"
	XrayModeManaged          = "managed"
	DefaultRuntimeAssetsPath = "/etc/xui-agent/runtime-assets.sha256"
	DefaultEventSpoolName    = "events.db"
	DefaultAccessLogName     = "logs/access.log"
	DefaultRouteListen       = "127.0.0.1:8687"
	DefaultXrayAPIAddress    = "127.0.0.1:62789"
)

type Config struct {
	ServerURL        string          `json:"serverUrl"`
	StateDirectory   string          `json:"stateDirectory"`
	AllowInsecure    bool            `json:"allowInsecure"`
	ServerCertSHA256 string          `json:"serverCertSha256,omitempty"`
	Xray             XrayConfig      `json:"xray"`
	Update           UpdateConfig    `json:"update"`
	Telemetry        TelemetryConfig `json:"telemetry,omitempty"`
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

func ManagedXrayBinaryPath(stateDirectory string) string {
	return xraybinary.ManagedPath(stateDirectory)
}

type UpdateConfig struct {
	// PublicKey is accepted only so pre-v0.4 installations can be upgraded by
	// the release installer. Release downloads no longer use this value.
	PublicKey         string `json:"publicKey,omitempty"`
	RuntimeAssetsPath string `json:"runtimeAssetsPath,omitempty"`
}

type TelemetryConfig struct {
	Enabled                  *bool  `json:"enabled,omitempty"`
	AccessPath               string `json:"accessPath,omitempty"`
	SpoolPath                string `json:"spoolPath,omitempty"`
	RouteListen              string `json:"routeListen,omitempty"`
	XrayAPIAddress           string `json:"xrayApiAddress,omitempty"`
	LogTimezone              string `json:"logTimezone,omitempty"`
	PollIntervalSeconds      int    `json:"pollIntervalSeconds,omitempty"`
	SampleIntervalSeconds    int    `json:"sampleIntervalSeconds,omitempty"`
	HeartbeatIntervalSeconds int    `json:"heartbeatIntervalSeconds,omitempty"`
	QueueMaxBytes            uint64 `json:"queueMaxBytes,omitempty"`
	QueueMaxEvents           uint64 `json:"queueMaxEvents,omitempty"`
}

func (c Config) TelemetryEnabled() bool {
	if c.Telemetry.Enabled != nil {
		return *c.Telemetry.Enabled
	}
	return c.Xray.Managed()
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
	cfg.Telemetry = normalizeTelemetry(cfg.StateDirectory, cfg.Telemetry)
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
	if c.Update.RuntimeAssetsPath != "" && !filepath.IsAbs(c.Update.RuntimeAssetsPath) {
		return errors.New("update.runtimeAssetsPath must be an absolute path")
	}
	telemetry := normalizeTelemetry(c.StateDirectory, c.Telemetry)
	if c.TelemetryEnabled() {
		if !filepath.IsAbs(telemetry.AccessPath) || !filepath.IsAbs(telemetry.SpoolPath) {
			return errors.New("telemetry accessPath and spoolPath must be absolute paths")
		}
		if _, err := time.LoadLocation(telemetry.LogTimezone); err != nil {
			return errors.New("telemetry.logTimezone must be a valid IANA timezone or Local")
		}
		if err := validateLoopbackAddress("telemetry.routeListen", telemetry.RouteListen); err != nil {
			return err
		}
		if err := validateLoopbackAddress("telemetry.xrayApiAddress", telemetry.XrayAPIAddress); err != nil {
			return err
		}
		if telemetry.PollIntervalSeconds < 1 || telemetry.PollIntervalSeconds > 60 {
			return errors.New("telemetry.pollIntervalSeconds must be between 1 and 60")
		}
		if telemetry.SampleIntervalSeconds < 30 || telemetry.SampleIntervalSeconds > 3600 {
			return errors.New("telemetry.sampleIntervalSeconds must be between 30 and 3600")
		}
		if telemetry.HeartbeatIntervalSeconds < 30 || telemetry.HeartbeatIntervalSeconds > 3600 {
			return errors.New("telemetry.heartbeatIntervalSeconds must be between 30 and 3600")
		}
		if telemetry.QueueMaxBytes < 1<<20 || telemetry.QueueMaxBytes > 4<<30 {
			return errors.New("telemetry.queueMaxBytes must be between 1 MiB and 4 GiB")
		}
		if telemetry.QueueMaxEvents < 100 || telemetry.QueueMaxEvents > 10_000_000 {
			return errors.New("telemetry.queueMaxEvents must be between 100 and 10000000")
		}
	}
	return nil
}

func normalizeTelemetry(stateDirectory string, cfg TelemetryConfig) TelemetryConfig {
	if cfg.AccessPath == "" {
		cfg.AccessPath = filepath.Join(stateDirectory, filepath.FromSlash(DefaultAccessLogName))
	}
	if cfg.SpoolPath == "" {
		cfg.SpoolPath = filepath.Join(stateDirectory, DefaultEventSpoolName)
	}
	if cfg.RouteListen == "" {
		cfg.RouteListen = DefaultRouteListen
	}
	if cfg.XrayAPIAddress == "" {
		cfg.XrayAPIAddress = DefaultXrayAPIAddress
	}
	if cfg.LogTimezone == "" {
		cfg.LogTimezone = "Local"
	}
	if cfg.PollIntervalSeconds == 0 {
		cfg.PollIntervalSeconds = 1
	}
	if cfg.SampleIntervalSeconds == 0 {
		cfg.SampleIntervalSeconds = 300
	}
	if cfg.HeartbeatIntervalSeconds == 0 {
		cfg.HeartbeatIntervalSeconds = 300
	}
	if cfg.QueueMaxBytes == 0 {
		cfg.QueueMaxBytes = 256 << 20
	}
	if cfg.QueueMaxEvents == 0 {
		cfg.QueueMaxEvents = 100000
	}
	return cfg
}

func validateLoopbackAddress(name string, value string) error {
	host, port, err := net.SplitHostPort(value)
	if err != nil || port == "" {
		return fmt.Errorf("%s must be a loopback host and port", name)
	}
	ip := net.ParseIP(host)
	if ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("%s must bind to a loopback IP", name)
	}
	return nil
}
