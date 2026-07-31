package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/qqqasdwx/xui-agent/internal/config"
	"github.com/qqqasdwx/xui-agent/internal/identity"
	"github.com/qqqasdwx/xui-agent/internal/status"
	updatepkg "github.com/qqqasdwx/xui-agent/internal/update"
	"github.com/qqqasdwx/xui-agent/internal/xrayconfig"
	"github.com/qqqasdwx/xui-agent/internal/xrayruntime"
	"github.com/qqqasdwx/xui-agent/internal/xrayupdate"
	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

const (
	enrollmentTokenEnv  = "XUI_AGENT_ENROLLMENT_TOKEN"
	maxResponseBytes    = 64 << 10
	maxMessageBytes     = v1.MaxMessageBytes
	updateHealthTimeout = 2 * time.Minute
)

type Client struct {
	cfg                   config.Config
	version               string
	store                 *identity.Store
	collector             *status.Collector
	updater               *updatepkg.Manager
	configValidator       *xrayconfig.Manager
	configApplier         *xrayruntime.Manager
	xrayUpdater           *xrayupdate.Manager
	httpClient            *http.Client
	wsDialer              *websocket.Dialer
	startedAt             time.Time
	validatePendingUpdate bool
}

type LocalStatus struct {
	Enrolled bool         `json:"enrolled"`
	NodeID   int          `json:"nodeId,omitempty"`
	NodeName string       `json:"nodeName,omitempty"`
	Agent    v1.Heartbeat `json:"agent"`
}

func NewClient(cfg config.Config, version string) (*Client, error) {
	tlsConfig, err := tlsConfigFor(cfg)
	if err != nil {
		return nil, err
	}
	startedAt := time.Now()
	updater, err := updatepkg.NewManager(cfg.StateDirectory, cfg.Update.RuntimeAssetsPath)
	if err != nil {
		return nil, err
	}
	validatePendingUpdate := false
	if pending, pendingErr := updater.Pending(); pendingErr == nil {
		if pending.TargetVersion != version {
			return nil, fmt.Errorf("pending update targets %q but running version is %q", pending.TargetVersion, version)
		}
		validatePendingUpdate = true
	} else if !errors.Is(pendingErr, os.ErrNotExist) {
		return nil, pendingErr
	}
	transport := &http.Transport{TLSClientConfig: tlsConfig}
	validator := xrayconfig.NewManager(cfg.StateDirectory, cfg.Xray.BinaryPath, nil)
	var applier *xrayruntime.Manager
	var xrayUpdater *xrayupdate.Manager
	if cfg.Xray.Managed() {
		validator = xrayconfig.NewManagedManager(cfg.StateDirectory, cfg.Xray.BinaryPath, nil)
		controller := xrayruntime.NewProcessController(cfg.StateDirectory, cfg.Xray.BinaryPath)
		applier = xrayruntime.NewManager(
			cfg.StateDirectory,
			validator,
			controller,
		)
		xrayUpdater, err = xrayupdate.NewManager(cfg.StateDirectory, controller, nil)
		if err != nil {
			return nil, err
		}
	}
	return &Client{
		cfg:             cfg,
		version:         version,
		store:           identity.NewStore(cfg.StateDirectory),
		collector:       status.NewCollector(cfg.Xray, cfg.StateDirectory, startedAt),
		updater:         updater,
		configValidator: validator,
		configApplier:   applier,
		xrayUpdater:     xrayUpdater,
		httpClient: &http.Client{
			Transport: transport,
			Timeout:   15 * time.Second,
		},
		wsDialer: &websocket.Dialer{
			TLSClientConfig:  tlsConfig,
			HandshakeTimeout: 15 * time.Second,
		},
		startedAt:             startedAt,
		validatePendingUpdate: validatePendingUpdate,
	}, nil
}

func tlsConfigFor(cfg config.Config) (*tls.Config, error) {
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	pin := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(cfg.ServerCertSHA256), ":", ""))
	if pin != "" {
		want, err := hex.DecodeString(pin)
		if err != nil || len(want) != sha256.Size {
			return nil, errors.New("serverCertSha256 must be a SHA-256 hex digest")
		}
		tlsConfig.VerifyConnection = func(cs tls.ConnectionState) error {
			if len(cs.PeerCertificates) == 0 {
				return errors.New("server did not present a certificate")
			}
			got := sha256.Sum256(cs.PeerCertificates[0].Raw)
			if subtle.ConstantTimeCompare(got[:], want) != 1 {
				return errors.New("server certificate fingerprint mismatch")
			}
			return nil
		}
	}
	return tlsConfig, nil
}

func (c *Client) Run(ctx context.Context) error {
	if c.xrayUpdater != nil {
		if err := c.xrayUpdater.Recover(ctx); err != nil {
			return fmt.Errorf("recover managed Xray binary: %w", err)
		}
	}
	if c.configApplier != nil {
		if err := c.configApplier.Recover(ctx); err != nil {
			return fmt.Errorf("recover managed Xray config: %w", err)
		}
	}
	id, err := c.store.Load()
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("load identity: %w", err)
		}
		token := strings.TrimSpace(os.Getenv(enrollmentTokenEnv))
		if token == "" {
			return fmt.Errorf("agent is not enrolled; set %s for the first start", enrollmentTokenEnv)
		}
		id, err = c.Enroll(ctx, token)
		if err != nil {
			return err
		}
		slog.Info("agent enrolled", "node_id", id.NodeID, "node_name", id.NodeName)
	}

	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return nil
		}
		if c.validatePendingUpdate && time.Since(c.startedAt) >= updateHealthTimeout {
			return errors.New("updated agent did not establish a healthy control session before the deadline")
		}
		stable, err := c.runSession(ctx, id)
		if ctx.Err() != nil {
			return nil
		}
		if stable {
			backoff = time.Second
		}
		if errors.Is(err, updatepkg.ErrRestartRequired) {
			return err
		}
		if pending, pendingErr := c.updater.Pending(); pendingErr == nil && pending.TargetVersion != c.version {
			return fmt.Errorf("updated agent activation was not acknowledged: %w", err)
		}
		slog.Warn("control session ended", "error", err, "retry_in", backoff)
		jitter := time.Duration(rand.Int64N(int64(backoff/2) + 1))
		timer := time.NewTimer(backoff + jitter)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		backoff *= 2
		if backoff > time.Minute {
			backoff = time.Minute
		}
	}
}

func (c *Client) Enroll(ctx context.Context, token string) (identity.Identity, error) {
	token = strings.TrimSpace(token)
	if token == "" {
		return identity.Identity{}, fmt.Errorf("set %s to a one-time enrollment token", enrollmentTokenEnv)
	}
	id, err := c.enroll(ctx, token)
	if err != nil {
		return identity.Identity{}, err
	}
	if err := c.store.Save(id); err != nil {
		return identity.Identity{}, err
	}
	_ = os.Unsetenv(enrollmentTokenEnv)
	return id, nil
}

func (c *Client) Status(ctx context.Context) (LocalStatus, error) {
	now := time.Now()
	heartbeat := c.collector.Heartbeat(ctx, c.version, now)
	heartbeat.Capabilities = c.capabilities()
	result := LocalStatus{Agent: heartbeat}
	id, err := c.store.Load()
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return result, nil
		}
		return LocalStatus{}, fmt.Errorf("load identity: %w", err)
	}
	result.Enrolled = true
	result.NodeID = id.NodeID
	result.NodeName = id.NodeName
	return result, nil
}

func (c *Client) enroll(ctx context.Context, token string) (identity.Identity, error) {
	hostname, _ := os.Hostname()
	payload := v1.EnrollRequest{
		Token:           token,
		ProtocolVersion: v1.Version,
		AgentVersion:    c.version,
		Hostname:        hostname,
		OS:              runtime.GOOS,
		Arch:            runtime.GOARCH,
		Capabilities:    c.capabilities(),
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return identity.Identity{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpointURL("agent/v1/enroll"), bytes.NewReader(raw))
	if err != nil {
		return identity.Identity{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		return identity.Identity{}, fmt.Errorf("enroll request: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil {
		return identity.Identity{}, fmt.Errorf("read enroll response: %w", err)
	}
	if len(body) > maxResponseBytes {
		return identity.Identity{}, errors.New("enroll response is too large")
	}
	if resp.StatusCode != http.StatusCreated {
		var failure v1.ErrorResponse
		_ = json.Unmarshal(body, &failure)
		if failure.Error == "" {
			failure.Error = http.StatusText(resp.StatusCode)
		}
		return identity.Identity{}, fmt.Errorf("enroll failed: %s", failure.Error)
	}
	var enrolled v1.EnrollResponse
	if err := json.Unmarshal(body, &enrolled); err != nil {
		return identity.Identity{}, fmt.Errorf("decode enroll response: %w", err)
	}
	return identity.Identity{NodeID: enrolled.NodeID, NodeName: enrolled.NodeName, Credential: enrolled.Credential}, nil
}

func (c *Client) runSession(ctx context.Context, id identity.Identity) (bool, error) {
	headers := http.Header{}
	headers.Set("Authorization", "Bearer "+id.Credential)
	headers.Set("X-XUI-Agent-Node-ID", strconv.Itoa(id.NodeID))
	headers.Set("X-XUI-Agent-Protocol", v1.Version)
	conn, resp, err := c.wsDialer.DialContext(ctx, c.websocketURL("agent/v1/connect"), headers)
	if err != nil {
		if resp != nil {
			return false, fmt.Errorf("connect websocket: HTTP %d", resp.StatusCode)
		}
		return false, fmt.Errorf("connect websocket: %w", err)
	}
	defer conn.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stopClose()
	conn.SetReadLimit(maxMessageBytes)
	if err := conn.SetReadDeadline(time.Now().Add(20 * time.Second)); err != nil {
		return false, err
	}
	var helloEnvelope v1.Envelope
	if err := conn.ReadJSON(&helloEnvelope); err != nil {
		return false, fmt.Errorf("read hello: %w", err)
	}
	if helloEnvelope.Version != v1.Version || helloEnvelope.Type != v1.MessageHelloAck {
		return false, errors.New("server returned an incompatible hello")
	}
	hello, err := v1.DecodePayload[v1.HelloAck](helloEnvelope)
	if err != nil {
		return false, err
	}
	period := time.Duration(hello.HeartbeatIntervalSecond) * time.Second
	if period < 5*time.Second || period > 5*time.Minute {
		return false, errors.New("server returned an invalid heartbeat interval")
	}
	slog.Info("control session connected", "node_id", id.NodeID, "session_id", hello.SessionID)

	stable := false
	for {
		messageID, err := c.writeHeartbeat(ctx, conn)
		if err != nil {
			return stable, err
		}
		if err := conn.SetReadDeadline(time.Now().Add(3 * period)); err != nil {
			return stable, err
		}
		var envelope v1.Envelope
		if err := conn.ReadJSON(&envelope); err != nil {
			return stable, err
		}
		if envelope.Version != v1.Version {
			return stable, errors.New("received incompatible protocol version")
		}
		if envelope.Type == v1.MessageProtocolError {
			failure, decodeErr := v1.DecodePayload[v1.ProtocolError](envelope)
			if decodeErr != nil {
				return stable, decodeErr
			}
			return stable, fmt.Errorf("server rejected heartbeat: %s: %s", failure.Code, failure.Message)
		}
		if envelope.Type != v1.MessageHeartbeatAck {
			return stable, fmt.Errorf("unexpected server message %q", envelope.Type)
		}
		ack, err := v1.DecodePayload[v1.HeartbeatAck](envelope)
		if err != nil {
			return stable, err
		}
		if ack.MessageID != messageID {
			return stable, errors.New("heartbeat acknowledgement does not match the sent message")
		}
		if c.validatePendingUpdate {
			if err := c.updater.Confirm(c.version); err != nil {
				return stable, fmt.Errorf("confirm updated agent health: %w", err)
			}
			c.validatePendingUpdate = false
			slog.Info("agent update confirmed", "version", c.version)
		}
		stable = true
		if ack.Command != nil {
			result, restart := c.executeCommand(ctx, *ack.Command)
			if err := c.writeCommandResult(conn, result); err != nil {
				return stable, err
			}
			if restart {
				return stable, updatepkg.ErrRestartRequired
			}
		}

		timer := time.NewTimer(period)
		select {
		case <-ctx.Done():
			timer.Stop()
			return stable, nil
		case <-timer.C:
		}
	}
}

func (c *Client) writeHeartbeat(ctx context.Context, conn *websocket.Conn) (string, error) {
	now := time.Now()
	messageID := fmt.Sprintf("%d-%d", now.UnixMilli(), rand.Uint64())
	heartbeat := c.collector.Heartbeat(ctx, c.version, now)
	heartbeat.Capabilities = c.capabilities()
	envelope, err := v1.NewEnvelope(v1.MessageHeartbeat, messageID, heartbeat, now)
	if err != nil {
		return "", err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return "", err
	}
	if err := conn.WriteJSON(envelope); err != nil {
		return "", err
	}
	return messageID, nil
}

func (c *Client) capabilities() []string {
	capabilities := []string{v1.CapabilityObserve}
	if c.updater.Enabled() {
		capabilities = append(capabilities, v1.CapabilitySelfUpdate)
	}
	if c.configValidator.Enabled() {
		capabilities = append(capabilities, v1.CapabilityConfigValidate)
	}
	if c.configApplier != nil && c.configApplier.Enabled() {
		capabilities = append(capabilities, v1.CapabilityConfigApply)
	}
	if c.xrayUpdater != nil && c.xrayUpdater.Enabled() {
		capabilities = append(capabilities, v1.CapabilityXrayUpdate)
	}
	return capabilities
}

func (c *Client) executeCommand(ctx context.Context, command v1.Command) (v1.CommandResult, bool) {
	result := v1.CommandResult{CommandID: command.ID, Status: "failed"}
	if command.ID == "" {
		result.Error = "unsupported command"
		return result, false
	}
	now := time.Now().Unix()
	if command.ExpiresAt <= now || command.IssuedAt > now+60 {
		return rejectedCommandResult(command, "command is expired or not yet valid", "command_expired"), false
	}
	switch command.Type {
	case v1.CommandAgentUpdate:
		return c.executeAgentUpdate(ctx, command, result)
	case v1.CommandValidateConfig:
		return c.executeConfigValidation(ctx, command, result), false
	case v1.CommandApplyConfig:
		return c.executeConfigApply(ctx, command, result), false
	case v1.CommandXrayUpdate:
		return c.executeXrayUpdate(ctx, command, result), false
	default:
		result.Error = "unsupported command"
		return result, false
	}
}

func rejectedCommandResult(command v1.Command, message, errorCode string) v1.CommandResult {
	result := v1.CommandResult{CommandID: command.ID, Status: "failed", Error: message, ErrorCode: errorCode}
	switch command.Type {
	case v1.CommandValidateConfig:
		result.Status = xrayconfig.StatusFailed
		if payload, err := v1.DecodeCommandPayload[v1.ValidateConfigCommand](command); err == nil {
			result.ConfigVersion = payload.ConfigVersion
			result.ConfigDigest = payload.ConfigDigest
		}
	case v1.CommandApplyConfig:
		result.Status = xrayruntime.StatusApplyFailed
		result.RecoveryStatus = xrayruntime.RecoveryStatusNotRequired
		if payload, err := v1.DecodeCommandPayload[v1.ApplyConfigCommand](command); err == nil {
			result.ConfigVersion = payload.ConfigVersion
			result.ConfigDigest = payload.ConfigDigest
		}
	case v1.CommandXrayUpdate:
		result.Status = xrayupdate.StatusInstallFailed
		result.RecoveryStatus = xrayupdate.RecoveryStatusNotRequired
		if payload, err := v1.DecodeCommandPayload[v1.XrayUpdateCommand](command); err == nil {
			result.Version = payload.Version
		}
	}
	return result
}

func (c *Client) executeXrayUpdate(ctx context.Context, command v1.Command, result v1.CommandResult) v1.CommandResult {
	result.Status = xrayupdate.StatusInstallFailed
	if c.xrayUpdater == nil || !c.xrayUpdater.Enabled() {
		result.Error = "managed Xray updates are not configured"
		result.ErrorCode = xrayupdate.ErrorCodePreparationFailed
		result.RecoveryStatus = xrayupdate.RecoveryStatusNotRequired
		return result
	}
	payload, err := v1.DecodeCommandPayload[v1.XrayUpdateCommand](command)
	if err != nil {
		result.Error = "invalid Xray update command"
		result.ErrorCode = xrayupdate.ErrorCodePreparationFailed
		result.RecoveryStatus = xrayupdate.RecoveryStatusNotRequired
		return result
	}
	result.Version = payload.Version
	applied, err := c.xrayUpdater.Apply(ctx, xrayupdate.Request{
		CommandID: command.ID, Version: payload.Version,
	})
	if err != nil {
		result.Error = safeUpdateError(err)
		result.ErrorCode, result.RecoveryStatus = xrayupdate.ErrorDetails(err)
		return result
	}
	result.Success = applied.Success()
	result.Status = applied.Status
	result.Version = applied.Version
	result.Error = applied.Error
	result.ErrorCode = applied.ErrorCode
	result.RecoveryStatus = applied.RecoveryStatus
	return result
}

func (c *Client) executeConfigApply(ctx context.Context, command v1.Command, result v1.CommandResult) v1.CommandResult {
	result.Status = xrayruntime.StatusApplyFailed
	if c.configApplier == nil || !c.configApplier.Enabled() {
		result.Error = "managed Xray runtime is not configured"
		result.ErrorCode = xrayruntime.ErrorCodePreparationFailed
		result.RecoveryStatus = xrayruntime.RecoveryStatusNotRequired
		return result
	}
	payload, err := v1.DecodeCommandPayload[v1.ApplyConfigCommand](command)
	if err != nil {
		result.Error = "invalid config apply command"
		result.ErrorCode = xrayruntime.ErrorCodePreparationFailed
		result.RecoveryStatus = xrayruntime.RecoveryStatusNotRequired
		return result
	}
	result.ConfigVersion = payload.ConfigVersion
	result.ConfigDigest = payload.ConfigDigest
	applied, err := c.configApplier.Apply(ctx, xrayruntime.Request{
		ConfigVersion: payload.ConfigVersion,
		ConfigDigest:  payload.ConfigDigest,
		Config:        payload.Config,
	})
	if err != nil {
		result.Error = safeUpdateError(err)
		result.ErrorCode, result.RecoveryStatus = xrayruntime.ErrorDetails(err)
		return result
	}
	result.Success = applied.Success()
	result.Status = applied.Status
	result.ConfigVersion = applied.ConfigVersion
	result.ConfigDigest = applied.ConfigDigest
	result.Error = applied.Error
	result.ErrorCode = applied.ErrorCode
	result.RecoveryStatus = applied.RecoveryStatus
	return result
}

func (c *Client) executeAgentUpdate(ctx context.Context, command v1.Command, result v1.CommandResult) (v1.CommandResult, bool) {
	payload, err := v1.DecodeCommandPayload[v1.AgentUpdateCommand](command)
	if err != nil {
		result.Error = "invalid update command"
		return result, false
	}
	if pending, pendingErr := c.updater.Pending(); pendingErr == nil {
		if pending.CommandID == command.ID {
			result.Success = true
			result.Status = "restarting"
			result.Version = pending.TargetVersion
			return result, true
		}
	} else if !errors.Is(pendingErr, os.ErrNotExist) {
		result.Error = safeUpdateError(fmt.Errorf("read pending update state: %w", pendingErr))
		return result, false
	}
	if failed, failedErr := c.updater.Failed(); failedErr == nil {
		if failed.CommandID == command.ID {
			result.Error = "the previous update attempt failed health validation and was rolled back"
			return result, false
		}
	} else if !errors.Is(failedErr, os.ErrNotExist) {
		result.Error = safeUpdateError(fmt.Errorf("read failed update state: %w", failedErr))
		return result, false
	}
	version, err := c.updater.Apply(ctx, updatepkg.Request{
		CommandID: command.ID, Version: payload.Version,
	})
	if err != nil {
		result.Error = safeUpdateError(err)
		return result, false
	}
	result.Success = true
	result.Status = "restarting"
	result.Version = version
	return result, true
}

func (c *Client) executeConfigValidation(ctx context.Context, command v1.Command, result v1.CommandResult) v1.CommandResult {
	result.Status = xrayconfig.StatusFailed
	payload, err := v1.DecodeCommandPayload[v1.ValidateConfigCommand](command)
	if err != nil {
		result.Error = "invalid config validation command"
		return result
	}
	result.ConfigVersion = payload.ConfigVersion
	result.ConfigDigest = payload.ConfigDigest
	validated, err := c.configValidator.Validate(ctx, xrayconfig.Request{
		ConfigVersion: payload.ConfigVersion,
		ConfigDigest:  payload.ConfigDigest,
		Config:        payload.Config,
	})
	if err != nil {
		result.Error = safeUpdateError(err)
		return result
	}
	result.Success = validated.Success()
	result.Status = validated.Status
	result.ConfigVersion = validated.ConfigVersion
	result.ConfigDigest = validated.ConfigDigest
	result.Error = validated.Error
	return result
}

func (c *Client) writeCommandResult(conn *websocket.Conn, result v1.CommandResult) error {
	now := time.Now()
	envelope, err := v1.NewEnvelope(v1.MessageCommandResult, fmt.Sprintf("%d-%d", now.UnixMilli(), rand.Uint64()), result, now)
	if err != nil {
		return err
	}
	if err := conn.SetWriteDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	if err := conn.WriteJSON(envelope); err != nil {
		return err
	}
	if err := conn.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
		return err
	}
	var ackEnvelope v1.Envelope
	if err := conn.ReadJSON(&ackEnvelope); err != nil {
		return fmt.Errorf("read command result acknowledgement: %w", err)
	}
	if ackEnvelope.Version != v1.Version || ackEnvelope.Type != v1.MessageCommandResultAck {
		return errors.New("server returned an invalid command result acknowledgement")
	}
	ack, err := v1.DecodePayload[v1.CommandResultAck](ackEnvelope)
	if err != nil {
		return err
	}
	if ack.CommandID != result.CommandID {
		return errors.New("command result acknowledgement does not match the command")
	}
	return nil
}

func safeUpdateError(err error) string {
	message := strings.TrimSpace(err.Error())
	if len(message) > 512 {
		message = message[:512]
	}
	return message
}

func (c *Client) endpointURL(endpoint string) string {
	return strings.TrimRight(c.cfg.ServerURL, "/") + "/" + strings.TrimLeft(endpoint, "/")
}

func (c *Client) websocketURL(endpoint string) string {
	u, _ := url.Parse(c.endpointURL(endpoint))
	if u.Scheme == "https" {
		u.Scheme = "wss"
	} else {
		u.Scheme = "ws"
	}
	return u.String()
}
