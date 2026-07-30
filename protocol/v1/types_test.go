package v1

import (
	"encoding/json"
	"testing"
	"time"
)

func TestHeartbeatEnvelopeRoundTrip(t *testing.T) {
	want := Heartbeat{
		ProtocolVersion: Version,
		AgentVersion:    "0.1.0",
		Hostname:        "edge-1",
		OS:              "linux",
		Arch:            "amd64",
		Capabilities:    []string{CapabilityObserve},
		Xray: XrayInfo{
			Present:      true,
			Running:      true,
			Version:      "Xray 26.7.28",
			ConfigDigest: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	envelope, err := NewEnvelope(MessageHeartbeat, "message-1", want, time.Unix(100, 0))
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	raw, err := json.Marshal(envelope)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	var decoded Envelope
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	got, err := DecodePayload[Heartbeat](decoded)
	if err != nil {
		t.Fatalf("DecodePayload: %v", err)
	}
	if got.ProtocolVersion != want.ProtocolVersion || got.Xray.ConfigDigest != want.Xray.ConfigDigest {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestAgentUpdateCommandRoundTrip(t *testing.T) {
	now := time.Unix(200, 0)
	command, err := NewCommand("command-1", CommandAgentUpdate, AgentUpdateCommand{
		Version: "v1.2.3", ManifestURL: "https://example.com/manifest.json", SignatureURL: "https://example.com/manifest.sig",
	}, now, now.Add(10*time.Minute))
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	payload, err := DecodeCommandPayload[AgentUpdateCommand](command)
	if err != nil {
		t.Fatalf("DecodeCommandPayload: %v", err)
	}
	if payload.Version != "v1.2.3" || command.ExpiresAt != now.Add(10*time.Minute).Unix() {
		t.Fatalf("unexpected command round trip: command=%+v payload=%+v", command, payload)
	}
}

func TestValidateConfigCommandAndResultRoundTrip(t *testing.T) {
	now := time.Unix(300, 0)
	command, err := NewCommand("config-1", CommandValidateConfig, ValidateConfigCommand{
		ConfigVersion: 4,
		ConfigDigest:  "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		Config:        json.RawMessage(`{"inbounds":[]}`),
	}, now, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("NewCommand: %v", err)
	}
	payload, err := DecodeCommandPayload[ValidateConfigCommand](command)
	if err != nil {
		t.Fatalf("DecodeCommandPayload: %v", err)
	}
	if payload.ConfigVersion != 4 || string(payload.Config) != `{"inbounds":[]}` {
		t.Fatalf("unexpected payload: %+v", payload)
	}
	envelope, err := NewEnvelope(MessageCommandResult, "result-1", CommandResult{
		CommandID: "config-1", Success: true, Status: "validated",
		ConfigVersion: payload.ConfigVersion, ConfigDigest: payload.ConfigDigest,
	}, now)
	if err != nil {
		t.Fatalf("NewEnvelope: %v", err)
	}
	result, err := DecodePayload[CommandResult](envelope)
	if err != nil || result.ConfigVersion != 4 || result.ConfigDigest != payload.ConfigDigest {
		t.Fatalf("unexpected result=%+v err=%v", result, err)
	}
}
