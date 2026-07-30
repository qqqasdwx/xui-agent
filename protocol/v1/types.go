package v1

import (
	"encoding/json"
	"fmt"
	"time"
)

const (
	Version                  = "1"
	CapabilityObserve        = "observe"
	CapabilitySelfUpdate     = "self_update"
	CapabilityConfigValidate = "config_validate"
	MessageHelloAck          = "hello_ack"
	MessageHeartbeat         = "heartbeat"
	MessageHeartbeatAck      = "heartbeat_ack"
	MessageCommandResult     = "command_result"
	MessageCommandResultAck  = "command_result_ack"
	MessageProtocolError     = "error"
	CommandAgentUpdate       = "agent_update"
	CommandValidateConfig    = "validate_config"
	DefaultHeartbeatPeriod   = 30 * time.Second
	MaxConfigBytes           = 4 << 20
	MaxMessageBytes          = 8 << 20
)

type EnrollRequest struct {
	Token           string   `json:"token"`
	ProtocolVersion string   `json:"protocolVersion"`
	AgentVersion    string   `json:"agentVersion"`
	Hostname        string   `json:"hostname"`
	OS              string   `json:"os"`
	Arch            string   `json:"arch"`
	Capabilities    []string `json:"capabilities"`
}

type EnrollResponse struct {
	NodeID     int    `json:"nodeId"`
	NodeName   string `json:"nodeName"`
	Credential string `json:"credential"`
}

type ErrorResponse struct {
	Error string `json:"error"`
}

type Envelope struct {
	Version string          `json:"version"`
	Type    string          `json:"type"`
	ID      string          `json:"id"`
	SentAt  int64           `json:"sentAt"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func NewEnvelope(messageType, id string, payload any, now time.Time) (Envelope, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, fmt.Errorf("marshal %s payload: %w", messageType, err)
	}
	return Envelope{
		Version: Version,
		Type:    messageType,
		ID:      id,
		SentAt:  now.UnixMilli(),
		Payload: raw,
	}, nil
}

func DecodePayload[T any](envelope Envelope) (T, error) {
	var payload T
	if err := json.Unmarshal(envelope.Payload, &payload); err != nil {
		return payload, fmt.Errorf("decode %s payload: %w", envelope.Type, err)
	}
	return payload, nil
}

type HelloAck struct {
	SessionID               string `json:"sessionId"`
	HeartbeatIntervalSecond int    `json:"heartbeatIntervalSeconds"`
	ServerTime              int64  `json:"serverTime"`
}

type Heartbeat struct {
	ProtocolVersion string     `json:"protocolVersion"`
	AgentVersion    string     `json:"agentVersion"`
	Hostname        string     `json:"hostname"`
	OS              string     `json:"os"`
	Arch            string     `json:"arch"`
	AgentStartedAt  int64      `json:"agentStartedAt"`
	ClockUnixMilli  int64      `json:"clockUnixMilli"`
	System          SystemInfo `json:"system"`
	Xray            XrayInfo   `json:"xray"`
	Capabilities    []string   `json:"capabilities"`
}

type SystemInfo struct {
	UptimeSeconds uint64 `json:"uptimeSeconds"`
	MemoryTotal   uint64 `json:"memoryTotal"`
	MemoryUsed    uint64 `json:"memoryUsed"`
	DiskTotal     uint64 `json:"diskTotal"`
	DiskAvailable uint64 `json:"diskAvailable"`
}

type XrayInfo struct {
	Present      bool   `json:"present"`
	Version      string `json:"version"`
	Running      bool   `json:"running"`
	StartedAt    int64  `json:"startedAt"`
	ConfigDigest string `json:"configDigest"`
	Error        string `json:"error,omitempty"`
}

type HeartbeatAck struct {
	MessageID  string   `json:"messageId"`
	ServerTime int64    `json:"serverTime"`
	Command    *Command `json:"command,omitempty"`
}

type Command struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`
	IssuedAt  int64           `json:"issuedAt"`
	ExpiresAt int64           `json:"expiresAt"`
	Payload   json.RawMessage `json:"payload"`
}

func NewCommand(id, commandType string, payload any, issuedAt, expiresAt time.Time) (Command, error) {
	raw, err := json.Marshal(payload)
	if err != nil {
		return Command{}, fmt.Errorf("marshal %s command payload: %w", commandType, err)
	}
	return Command{ID: id, Type: commandType, IssuedAt: issuedAt.Unix(), ExpiresAt: expiresAt.Unix(), Payload: raw}, nil
}

func DecodeCommandPayload[T any](command Command) (T, error) {
	var payload T
	if err := json.Unmarshal(command.Payload, &payload); err != nil {
		return payload, fmt.Errorf("decode %s command payload: %w", command.Type, err)
	}
	return payload, nil
}

type AgentUpdateCommand struct {
	Version      string `json:"version"`
	ManifestURL  string `json:"manifestUrl"`
	SignatureURL string `json:"signatureUrl"`
}

type ValidateConfigCommand struct {
	ConfigVersion uint64          `json:"configVersion"`
	ConfigDigest  string          `json:"configDigest"`
	Config        json.RawMessage `json:"config"`
}

type CommandResult struct {
	CommandID     string `json:"commandId"`
	Success       bool   `json:"success"`
	Status        string `json:"status"`
	Version       string `json:"version,omitempty"`
	ConfigVersion uint64 `json:"configVersion,omitempty"`
	ConfigDigest  string `json:"configDigest,omitempty"`
	Error         string `json:"error,omitempty"`
}

type CommandResultAck struct {
	CommandID string `json:"commandId"`
}

type ProtocolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
