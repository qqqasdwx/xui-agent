package v1

import "encoding/json"

const (
	EventVersion             = "1"
	EventVersionHeader       = "X-XUI-Agent-Event-Version"
	RouteWebhookTokenHeader  = "X-XUI-Webhook-Token"
	EventKindAccess          = "access"
	EventKindRoute           = "route"
	EventKindHeartbeat       = "heartbeat"
	EventKindTrafficSnapshot = "traffic_snapshot"
	EventKindOnlineSnapshot  = "online_snapshot"
	MaxEventBatchEvents      = 500
	MaxEventCompressedBytes  = 1 << 20
	MaxEventExpandedBytes    = 4 << 20
	MaxEventBytes            = 256 << 10
)

type Event struct {
	Sequence   uint64          `json:"sequence"`
	EventID    string          `json:"eventId"`
	Kind       string          `json:"kind"`
	ObservedAt int64           `json:"observedAt"`
	Payload    json.RawMessage `json:"payload"`
}

type EventBatch struct {
	Version       string  `json:"version"`
	StreamID      string  `json:"streamId"`
	FirstSequence uint64  `json:"firstSequence"`
	LastSequence  uint64  `json:"lastSequence"`
	Events        []Event `json:"events"`
}

type EventBatchAck struct {
	Version                   string `json:"version"`
	StreamID                  string `json:"streamId"`
	HighestContiguousSequence uint64 `json:"highestContiguousSequence"`
}

type AccessEvent struct {
	Email       string `json:"email"`
	SourceIP    string `json:"sourceIp"`
	SourcePort  uint16 `json:"sourcePort"`
	Network     string `json:"network"`
	TargetHost  string `json:"targetHost"`
	TargetPort  uint16 `json:"targetPort"`
	OutboundTag string `json:"outboundTag"`
}

type RouteEvent struct {
	Email           string `json:"email,omitempty"`
	Protocol        string `json:"protocol,omitempty"`
	Network         string `json:"network,omitempty"`
	DestinationHost string `json:"destinationHost,omitempty"`
	DestinationPort uint16 `json:"destinationPort,omitempty"`
	OriginalHost    string `json:"originalHost,omitempty"`
	OriginalPort    uint16 `json:"originalPort,omitempty"`
	RouteHost       string `json:"routeHost,omitempty"`
	RoutePort       uint16 `json:"routePort,omitempty"`
	OutboundTag     string `json:"outboundTag,omitempty"`
}

type HeartbeatEvent struct {
	AgentStartedAt    int64  `json:"agentStartedAt"`
	XrayRunning       bool   `json:"xrayRunning"`
	XrayStartedAt     int64  `json:"xrayStartedAt"`
	XrayConfigVersion uint64 `json:"xrayConfigVersion"`
	QueueEvents       uint64 `json:"queueEvents"`
	QueueBytes        uint64 `json:"queueBytes"`
	OldestQueuedAt    int64  `json:"oldestQueuedAt"`
	DroppedRoute      uint64 `json:"droppedRoute"`
	DroppedTelemetry  uint64 `json:"droppedTelemetry"`
	LastDropAt        int64  `json:"lastDropAt"`
}

type TrafficSnapshotEvent struct {
	RuntimeID string        `json:"runtimeId"`
	Users     []TrafficUser `json:"users"`
}

type TrafficUser struct {
	Email        string `json:"email"`
	UpBytes      uint64 `json:"upBytes"`
	DownBytes    uint64 `json:"downBytes"`
	CounterEpoch uint64 `json:"counterEpoch"`
}

type OnlineSnapshotEvent struct {
	RuntimeID string       `json:"runtimeId"`
	Users     []OnlineUser `json:"users"`
}

type OnlineUser struct {
	Email      string `json:"email"`
	IPCount    uint32 `json:"ipCount"`
	LastSeenAt int64  `json:"lastSeenAt"`
}

type EventQueueInfo struct {
	Enabled              bool   `json:"enabled"`
	StreamID             string `json:"streamId,omitempty"`
	PendingEvents        uint64 `json:"pendingEvents"`
	PendingBytes         uint64 `json:"pendingBytes"`
	HighestAckedSequence uint64 `json:"highestAckedSequence"`
	OldestQueuedAt       int64  `json:"oldestQueuedAt"`
	LastReceivedAt       int64  `json:"lastReceivedAt"`
	LastAccessAt         int64  `json:"lastAccessAt"`
	LastRouteAt          int64  `json:"lastRouteAt"`
	LastTrafficAt        int64  `json:"lastTrafficAt"`
	LastOnlineAt         int64  `json:"lastOnlineAt"`
	LastErrorAt          int64  `json:"lastErrorAt"`
	LastErrorCode        string `json:"lastErrorCode,omitempty"`
	DroppedRoute         uint64 `json:"droppedRoute"`
	DroppedTelemetry     uint64 `json:"droppedTelemetry"`
	LastDropAt           int64  `json:"lastDropAt"`
}
