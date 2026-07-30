package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qqqasdwx/xui-agent/internal/config"
	"github.com/qqqasdwx/xui-agent/internal/identity"
	updatepkg "github.com/qqqasdwx/xui-agent/internal/update"
	"github.com/qqqasdwx/xui-agent/internal/xrayconfig"
	"github.com/qqqasdwx/xui-agent/internal/xrayruntime"
	"github.com/qqqasdwx/xui-agent/internal/xrayupdate"
	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

func TestClientEnrollsAndCompletesHeartbeatRoundTrip(t *testing.T) {
	t.Setenv(enrollmentTokenEnv, "one-time-token")
	heartbeatReceived := make(chan v1.Heartbeat, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

	var server *httptest.Server
	mux := http.NewServeMux()
	mux.HandleFunc("/base/agent/v1/enroll", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method", http.StatusMethodNotAllowed)
			return
		}
		var request v1.EnrollRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, "decode", http.StatusBadRequest)
			return
		}
		if request.Token != "one-time-token" || request.ProtocolVersion != v1.Version {
			http.Error(w, "credentials", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(v1.EnrollResponse{NodeID: 17, NodeName: "edge-17", Credential: "node-credential"})
	})
	mux.HandleFunc("/base/agent/v1/connect", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer node-credential" || r.Header.Get("X-XUI-Agent-Node-ID") != "17" {
			http.Error(w, "credentials", http.StatusUnauthorized)
			return
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		now := time.Now()
		hello, _ := v1.NewEnvelope(v1.MessageHelloAck, "hello", v1.HelloAck{
			SessionID:               "session-17",
			HeartbeatIntervalSecond: 5,
			ServerTime:              now.UnixMilli(),
		}, now)
		if err := conn.WriteJSON(hello); err != nil {
			return
		}
		var envelope v1.Envelope
		if err := conn.ReadJSON(&envelope); err != nil {
			return
		}
		heartbeat, err := v1.DecodePayload[v1.Heartbeat](envelope)
		if err != nil {
			return
		}
		ack, _ := v1.NewEnvelope(v1.MessageHeartbeatAck, "ack", v1.HeartbeatAck{
			MessageID:  envelope.ID,
			ServerTime: time.Now().UnixMilli(),
		}, time.Now())
		if err := conn.WriteJSON(ack); err != nil {
			return
		}
		heartbeatReceived <- heartbeat
		<-r.Context().Done()
	})
	server = httptest.NewServer(mux)
	defer server.Close()

	stateDir := t.TempDir()
	client, err := NewClient(config.Config{
		ServerURL:      server.URL + "/base",
		StateDirectory: stateDir,
		AllowInsecure:  true,
	}, "test-version")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()

	select {
	case heartbeat := <-heartbeatReceived:
		if heartbeat.AgentVersion != "test-version" || heartbeat.ProtocolVersion != v1.Version {
			t.Fatalf("unexpected heartbeat: %+v", heartbeat)
		}
		cancel()
	case <-time.After(3 * time.Second):
		cancel()
		t.Fatal("agent did not complete the first heartbeat")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not stop promptly after context cancellation")
	}

	stored, err := identity.NewStore(stateDir).Load()
	if err != nil {
		t.Fatalf("load stored identity: %v", err)
	}
	if stored.NodeID != 17 || stored.Credential != "node-credential" {
		t.Fatalf("unexpected stored identity: %+v", stored)
	}
}

type configValidationRunner struct {
	calls int
}

func (r *configValidationRunner) Validate(_ context.Context, _, configPath string) error {
	r.calls++
	raw, err := os.ReadFile(configPath)
	if err != nil {
		return err
	}
	if string(raw) != `{"inbounds":[]}` {
		return errors.New("unexpected config")
	}
	return nil
}

func TestExecuteConfigValidationCommand(t *testing.T) {
	raw := json.RawMessage(`{"inbounds":[]}`)
	digest := sha256.Sum256(raw)
	now := time.Now()
	command, err := v1.NewCommand("config-1", v1.CommandValidateConfig, v1.ValidateConfigCommand{
		ConfigVersion: 7,
		ConfigDigest:  hex.EncodeToString(digest[:]),
		Config:        raw,
	}, now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	runner := &configValidationRunner{}
	client := &Client{configValidator: xrayconfig.NewManager(t.TempDir(), "/opt/xray/xray", runner)}
	result, restart := client.executeCommand(context.Background(), command)
	if restart || !result.Success || result.Status != xrayconfig.StatusValidated || result.ConfigVersion != 7 || result.ConfigDigest != hex.EncodeToString(digest[:]) {
		t.Fatalf("executeCommand restart=%v result=%+v", restart, result)
	}
	if runner.calls != 1 {
		t.Fatalf("runner calls = %d, want 1", runner.calls)
	}
}

type successfulApplyController struct{}

func (successfulApplyController) RestartAndWait(context.Context) error { return nil }
func (successfulApplyController) StopAndWait(context.Context) error    { return nil }

func TestExecuteConfigApplyCommand(t *testing.T) {
	raw := json.RawMessage(`{"inbounds":[]}`)
	digest := sha256.Sum256(raw)
	now := time.Now()
	command, err := v1.NewCommand("apply-1", v1.CommandApplyConfig, v1.ApplyConfigCommand{
		ConfigVersion: 8,
		ConfigDigest:  hex.EncodeToString(digest[:]),
		Config:        raw,
	}, now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	state := t.TempDir()
	validator := xrayconfig.NewManager(state, "/opt/xray/xray", &configValidationRunner{})
	client := &Client{
		configValidator: validator,
		configApplier:   xrayruntime.NewManager(state, validator, successfulApplyController{}),
	}
	result, restart := client.executeCommand(context.Background(), command)
	if restart || !result.Success || result.Status != xrayruntime.StatusApplied || result.ConfigVersion != 8 || result.ConfigDigest != hex.EncodeToString(digest[:]) {
		t.Fatalf("executeCommand restart=%v result=%+v", restart, result)
	}
}

func TestExpiredXrayUpdateReturnsTypedFailure(t *testing.T) {
	now := time.Now()
	command, err := v1.NewCommand("xray-expired", v1.CommandXrayUpdate, v1.XrayUpdateCommand{
		Version: "v26.7.28", ManifestURL: "https://example.com/xray-manifest.json", SignatureURL: "https://example.com/xray-manifest.sig",
	}, now.Add(-2*time.Minute), now.Add(-time.Minute))
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	result, restart := (&Client{}).executeCommand(context.Background(), command)
	if restart || result.Success || result.Status != xrayupdate.StatusInstallFailed || result.Version != "v26.7.28" || result.ErrorCode != "command_expired" || result.RecoveryStatus != xrayupdate.RecoveryStatusNotRequired {
		t.Fatalf("executeCommand restart=%v result=%+v", restart, result)
	}
}

func TestConfigApplyCapabilityRequiresManagedMode(t *testing.T) {
	base := config.Config{
		ServerURL:      "http://127.0.0.1:2053",
		StateDirectory: t.TempDir(),
		AllowInsecure:  true,
		Xray: config.XrayConfig{
			Mode:       config.XrayModeObserve,
			BinaryPath: "/opt/xray/xray",
			ConfigPath: "/opt/xray/config.json",
		},
	}
	observing, err := NewClient(base, "test")
	if err != nil {
		t.Fatalf("NewClient observe: %v", err)
	}
	if slices.Contains(observing.capabilities(), v1.CapabilityConfigApply) {
		t.Fatal("observe mode advertised config apply")
	}
	base.StateDirectory = t.TempDir()
	base.Xray.Mode = config.XrayModeManaged
	base.Xray.ConfigPath = ""
	managed, err := NewClient(base, "test")
	if err != nil {
		t.Fatalf("NewClient managed: %v", err)
	}
	if !slices.Contains(managed.capabilities(), v1.CapabilityConfigApply) {
		t.Fatal("managed mode did not advertise config apply")
	}
}

func TestXrayUpdateCapabilityAllowsManagedBootstrapBinary(t *testing.T) {
	base := config.Config{
		ServerURL:      "http://127.0.0.1:2053",
		StateDirectory: t.TempDir(),
		AllowInsecure:  true,
		Xray: config.XrayConfig{
			Mode:       config.XrayModeManaged,
			BinaryPath: "/opt/xray/bootstrap",
		},
		Update: config.UpdateConfig{
			PublicKey: "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=",
		},
	}
	managed, err := NewClient(base, "test")
	if err != nil {
		t.Fatalf("NewClient managed bootstrap: %v", err)
	}
	if !slices.Contains(managed.capabilities(), v1.CapabilityXrayUpdate) {
		t.Fatal("managed bootstrap binary did not advertise Xray update")
	}

	base.StateDirectory = t.TempDir()
	base.Xray.Mode = config.XrayModeObserve
	base.Xray.ConfigPath = "/opt/xray/config.json"
	observing, err := NewClient(base, "test")
	if err != nil {
		t.Fatalf("NewClient observe: %v", err)
	}
	if slices.Contains(observing.capabilities(), v1.CapabilityXrayUpdate) {
		t.Fatal("observe mode advertised Xray update")
	}
}

func TestClientReceivesLargeConfigValidationCommand(t *testing.T) {
	stateDirectory := t.TempDir()
	binary := filepath.Join(t.TempDir(), "xray")
	script := "#!/bin/sh\n[ \"$1\" = run ] && [ \"$2\" = -test ] && [ \"$3\" = -config ] && [ -f \"$4\" ]\n"
	if err := os.WriteFile(binary, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake xray: %v", err)
	}
	configRaw, err := json.Marshal(map[string]any{
		"inbounds": []any{},
		"padding":  strings.Repeat("a", 128<<10),
	})
	if err != nil {
		t.Fatalf("marshal config: %v", err)
	}
	digest := sha256.Sum256(configRaw)
	now := time.Now()
	command, err := v1.NewCommand("config-large", v1.CommandValidateConfig, v1.ValidateConfigCommand{
		ConfigVersion: 9,
		ConfigDigest:  hex.EncodeToString(digest[:]),
		Config:        configRaw,
	}, now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}

	resultReceived := make(chan v1.CommandResult, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		hello, _ := v1.NewEnvelope(v1.MessageHelloAck, "hello", v1.HelloAck{
			SessionID: "session", HeartbeatIntervalSecond: 5, ServerTime: time.Now().UnixMilli(),
		}, time.Now())
		if err := conn.WriteJSON(hello); err != nil {
			return
		}
		var heartbeatEnvelope v1.Envelope
		if err := conn.ReadJSON(&heartbeatEnvelope); err != nil {
			return
		}
		heartbeat, err := v1.DecodePayload[v1.Heartbeat](heartbeatEnvelope)
		if err != nil || !slices.Contains(heartbeat.Capabilities, v1.CapabilityConfigValidate) {
			return
		}
		ack, _ := v1.NewEnvelope(v1.MessageHeartbeatAck, "heartbeat-ack", v1.HeartbeatAck{
			MessageID: heartbeatEnvelope.ID, ServerTime: time.Now().UnixMilli(), Command: &command,
		}, time.Now())
		if err := conn.WriteJSON(ack); err != nil {
			return
		}
		var resultEnvelope v1.Envelope
		if err := conn.ReadJSON(&resultEnvelope); err != nil {
			return
		}
		result, err := v1.DecodePayload[v1.CommandResult](resultEnvelope)
		if err != nil {
			return
		}
		resultAck, _ := v1.NewEnvelope(v1.MessageCommandResultAck, "result-ack", v1.CommandResultAck{CommandID: result.CommandID}, time.Now())
		if err := conn.WriteJSON(resultAck); err != nil {
			return
		}
		resultReceived <- result
		<-r.Context().Done()
	}))
	defer server.Close()

	if err := identity.NewStore(stateDirectory).Save(identity.Identity{NodeID: 9, NodeName: "edge-9", Credential: "credential"}); err != nil {
		t.Fatalf("save identity: %v", err)
	}
	client, err := NewClient(config.Config{
		ServerURL: server.URL, StateDirectory: stateDirectory, AllowInsecure: true,
		Xray: config.XrayConfig{BinaryPath: binary},
	}, "test-version")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- client.Run(ctx) }()
	select {
	case result := <-resultReceived:
		if !result.Success || result.Status != xrayconfig.StatusValidated || result.ConfigVersion != 9 || result.ConfigDigest != hex.EncodeToString(digest[:]) {
			t.Fatalf("unexpected command result: %+v", result)
		}
		cancel()
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("large config validation command did not complete")
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("agent did not stop after validation")
	}
}

func TestRunSessionRejectsMismatchedHeartbeatAcknowledgement(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		now := time.Now()
		hello, _ := v1.NewEnvelope(v1.MessageHelloAck, "hello", v1.HelloAck{
			SessionID:               "session",
			HeartbeatIntervalSecond: 5,
			ServerTime:              now.UnixMilli(),
		}, now)
		_ = conn.WriteJSON(hello)
		var heartbeat v1.Envelope
		if err := conn.ReadJSON(&heartbeat); err != nil {
			return
		}
		ack, _ := v1.NewEnvelope(v1.MessageHeartbeatAck, "ack", v1.HeartbeatAck{
			MessageID:  "different-message",
			ServerTime: time.Now().UnixMilli(),
		}, time.Now())
		_ = conn.WriteJSON(ack)
	}))
	defer server.Close()

	client, err := NewClient(config.Config{
		ServerURL:      server.URL,
		StateDirectory: t.TempDir(),
		AllowInsecure:  true,
	}, "test-version")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	stable, err := client.runSession(context.Background(), identity.Identity{NodeID: 1, Credential: "credential"})
	if stable || err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("runSession stable=%v err=%v, want mismatched acknowledgement error", stable, err)
	}
}

func TestExecuteCommandRejectsCorruptUpdateState(t *testing.T) {
	now := time.Now()
	command, err := v1.NewCommand("command-1", v1.CommandAgentUpdate, v1.AgentUpdateCommand{
		Version: "v1.2.3", ManifestURL: "https://example.com/manifest.json", SignatureURL: "https://example.com/manifest.sig",
	}, now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	for _, tc := range []struct {
		name     string
		filename string
		want     string
	}{
		{name: "pending", filename: "update-pending.json", want: "read pending update state"},
		{name: "failed", filename: "update-failed.json", want: "read failed update state"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			state := t.TempDir()
			if err := os.WriteFile(filepath.Join(state, tc.filename), []byte("not-json"), 0o600); err != nil {
				t.Fatalf("write corrupt state: %v", err)
			}
			manager, err := updatepkg.NewManager(state, "", false, "")
			if err != nil {
				t.Fatalf("NewManager: %v", err)
			}
			client := &Client{updater: manager}
			result, restart := client.executeCommand(context.Background(), command)
			if restart || result.Success || !strings.Contains(result.Error, tc.want) {
				t.Fatalf("executeCommand restart=%v result=%+v", restart, result)
			}
		})
	}
}
