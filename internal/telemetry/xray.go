package telemetry

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/eventspool"
	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

const maxXrayAPIOutputBytes = 4 << 20

type XrayStateFunc func(context.Context) v1.XrayInfo

type commandRunner func(context.Context, string, ...string) ([]byte, error)

type XrayCollector struct {
	binary   string
	address  string
	interval time.Duration
	spool    *eventspool.Store
	state    XrayStateFunc
	run      commandRunner
}

func NewXrayCollector(binary, address string, interval time.Duration, spool *eventspool.Store, state XrayStateFunc) *XrayCollector {
	return &XrayCollector{binary: binary, address: address, interval: interval, spool: spool, state: state, run: runXrayCommand}
}

func (c *XrayCollector) Run(ctx context.Context) {
	c.collect(ctx)
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.collect(ctx)
		}
	}
}

func (c *XrayCollector) collect(ctx context.Context) {
	state := c.state(ctx)
	if !state.Running || state.StartedAt <= 0 {
		return
	}
	runtimeID := strconv.FormatInt(state.StartedAt, 10)
	now := time.Now()
	if err := c.collectTraffic(ctx, runtimeID, now); err != nil {
		c.recordCollectionError(err, now)
	}
	if err := c.collectOnline(ctx, runtimeID, now); err != nil {
		c.recordCollectionError(err, now)
	}
}

func (c *XrayCollector) collectTraffic(ctx context.Context, runtimeID string, observedAt time.Time) error {
	raw, err := c.run(ctx, c.binary, "api", "statsquery", "--server="+c.address, "--timeout=3", "--pattern=user>>>")
	if err != nil {
		return fmt.Errorf("query traffic: %w", err)
	}
	current, err := parseTrafficStats(raw)
	if err != nil {
		return err
	}
	users := make([]v1.TrafficUser, 0, len(current))
	stored := make(map[string]eventspool.TrafficCounter, len(current))
	for email, values := range current {
		previous, found, err := c.spool.TrafficCounter(email)
		if err != nil {
			return err
		}
		epoch := uint64(0)
		if found && previous.RuntimeID == runtimeID {
			epoch = previous.CounterEpoch
			if values.up < previous.UpBytes || values.down < previous.DownBytes {
				epoch++
			}
		}
		users = append(users, v1.TrafficUser{
			Email: email, UpBytes: values.up, DownBytes: values.down, CounterEpoch: epoch,
		})
		stored[email] = eventspool.TrafficCounter{
			RuntimeID: runtimeID, UpBytes: values.up, DownBytes: values.down, CounterEpoch: epoch,
		}
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Email < users[j].Email })
	_, err = c.spool.EnqueueTrafficSnapshot(observedAt, v1.TrafficSnapshotEvent{RuntimeID: runtimeID, Users: users}, stored)
	return err
}

func (c *XrayCollector) collectOnline(ctx context.Context, runtimeID string, observedAt time.Time) error {
	raw, err := c.run(ctx, c.binary, "api", "statsonlineiplist", "--server="+c.address, "--timeout=3", "--all")
	if err != nil {
		return fmt.Errorf("query online users: %w", err)
	}
	users, err := parseOnlineStats(raw)
	if err != nil {
		return err
	}
	_, err = c.spool.Enqueue(v1.EventKindOnlineSnapshot, observedAt, v1.OnlineSnapshotEvent{RuntimeID: runtimeID, Users: users})
	return err
}

func (c *XrayCollector) recordCollectionError(err error, now time.Time) {
	if errors.Is(err, eventspool.ErrFull) {
		_ = c.spool.RecordDrop(v1.EventKindTrafficSnapshot, now)
		return
	}
	_ = c.spool.RecordError("xray_telemetry_failed", now)
}

func runXrayCommand(ctx context.Context, binary string, args ...string) ([]byte, error) {
	commandCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(commandCtx, binary, args...)
	output := &limitedBuffer{limit: maxXrayAPIOutputBytes}
	cmd.Stdout = output
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return nil, errors.New("Xray API command failed")
	}
	if output.exceeded {
		return nil, errors.New("Xray API response exceeds limit")
	}
	return output.Bytes(), nil
}

type limitedBuffer struct {
	bytes.Buffer
	limit    int
	exceeded bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	original := len(p)
	remaining := b.limit - b.Len()
	if remaining <= 0 {
		b.exceeded = true
		return original, nil
	}
	if len(p) > remaining {
		b.exceeded = true
		p = p[:remaining]
	}
	_, _ = b.Buffer.Write(p)
	return original, nil
}

type trafficPair struct {
	up   uint64
	down uint64
}

func parseTrafficStats(raw []byte) (map[string]trafficPair, error) {
	var response struct {
		Stats []struct {
			Name  string          `json:"name"`
			Value json.RawMessage `json:"value"`
		} `json:"stat"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return nil, errors.New("invalid Xray traffic response")
	}
	result := make(map[string]trafficPair)
	for _, stat := range response.Stats {
		direction := ""
		switch {
		case strings.HasPrefix(stat.Name, "user>>>") && strings.HasSuffix(stat.Name, ">>>traffic>>>uplink"):
			direction = "up"
		case strings.HasPrefix(stat.Name, "user>>>") && strings.HasSuffix(stat.Name, ">>>traffic>>>downlink"):
			direction = "down"
		default:
			continue
		}
		suffix := ">>>traffic>>>" + map[bool]string{true: "uplink", false: "downlink"}[direction == "up"]
		email := strings.TrimSuffix(strings.TrimPrefix(stat.Name, "user>>>"), suffix)
		email, err := cleanToken(email, 254, false)
		if err != nil || strings.Contains(email, ">>>") {
			return nil, errors.New("invalid Xray traffic user")
		}
		value, err := parseProtoUint64(stat.Value)
		if err != nil {
			return nil, errors.New("invalid Xray traffic counter")
		}
		pair := result[email]
		if direction == "up" {
			pair.up = value
		} else {
			pair.down = value
		}
		result[email] = pair
	}
	return result, nil
}

func parseOnlineStats(raw []byte) ([]v1.OnlineUser, error) {
	var response struct {
		Users []struct {
			Email string `json:"email"`
			IPs   []struct {
				IP       string          `json:"ip"`
				LastSeen json.RawMessage `json:"lastSeen"`
			} `json:"ips"`
		} `json:"users"`
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&response); err != nil {
		return nil, errors.New("invalid Xray online response")
	}
	users := make([]v1.OnlineUser, 0, len(response.Users))
	for _, rawUser := range response.Users {
		email, err := cleanToken(rawUser.Email, 254, false)
		if err != nil {
			return nil, errors.New("invalid Xray online user")
		}
		lastSeen := int64(0)
		for _, entry := range rawUser.IPs {
			value, err := parseProtoInt64(entry.LastSeen)
			if err != nil {
				return nil, errors.New("invalid Xray online timestamp")
			}
			if value > lastSeen {
				lastSeen = value
			}
		}
		users = append(users, v1.OnlineUser{Email: email, IPCount: uint32(len(rawUser.IPs)), LastSeenAt: lastSeen})
	}
	sort.Slice(users, func(i, j int) bool { return users[i].Email < users[j].Email })
	return users, nil
}

func parseProtoUint64(raw json.RawMessage) (uint64, error) {
	value := strings.Trim(string(raw), `"`)
	return strconv.ParseUint(value, 10, 64)
}

func parseProtoInt64(raw json.RawMessage) (int64, error) {
	value := strings.Trim(string(raw), `"`)
	return strconv.ParseInt(value, 10, 64)
}
