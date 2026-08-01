package telemetry

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/config"
	"github.com/qqqasdwx/xui-agent/internal/eventspool"
	"github.com/qqqasdwx/xui-agent/internal/identity"
	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

type Runtime struct {
	spool     *eventspool.Store
	access    *AccessCollector
	route     *RouteServer
	xray      *XrayCollector
	uploader  *Uploader
	state     XrayStateFunc
	startedAt time.Time
	heartbeat time.Duration
}

func NewRuntime(cfg config.Config, id identity.Identity, client *http.Client, endpoint string, state XrayStateFunc, startedAt time.Time) (*Runtime, error) {
	if err := os.MkdirAll(filepath.Dir(cfg.Telemetry.AccessPath), 0o700); err != nil {
		return nil, fmt.Errorf("create access log directory: %w", err)
	}
	spool, err := eventspool.Open(eventspool.Config{
		Path: cfg.Telemetry.SpoolPath, NodeID: id.NodeID,
		MaxBytes: cfg.Telemetry.QueueMaxBytes, MaxEvents: cfg.Telemetry.QueueMaxEvents,
	})
	if err != nil {
		return nil, err
	}
	access, err := NewAccessCollector(cfg.Telemetry.AccessPath, time.Duration(cfg.Telemetry.PollIntervalSeconds)*time.Second, cfg.Telemetry.LogTimezone, spool)
	if err != nil {
		_ = spool.Close()
		return nil, err
	}
	route := NewRouteServer(cfg.Telemetry.RouteListen, id.Credential, spool)
	if err := route.Open(); err != nil {
		_ = spool.Close()
		return nil, err
	}
	return &Runtime{
		spool: spool, access: access,
		route:    route,
		xray:     NewXrayCollector(cfg.Xray.BinaryPath, cfg.Telemetry.XrayAPIAddress, time.Duration(cfg.Telemetry.SampleIntervalSeconds)*time.Second, spool, state),
		uploader: NewUploader(endpoint, id, client, spool), state: state, startedAt: startedAt,
		heartbeat: time.Duration(cfg.Telemetry.HeartbeatIntervalSeconds) * time.Second,
	}, nil
}

func (r *Runtime) Run(ctx context.Context) error {
	child, cancel := context.WithCancel(ctx)
	defer cancel()
	routeDone := make(chan error, 1)
	var workers sync.WaitGroup
	workers.Add(5)
	go func() {
		defer workers.Done()
		routeDone <- r.route.Run(child)
	}()
	go func() {
		defer workers.Done()
		r.access.Run(child)
	}()
	go func() {
		defer workers.Done()
		r.xray.Run(child)
	}()
	go func() {
		defer workers.Done()
		r.uploader.Run(child)
	}()
	go func() {
		defer workers.Done()
		r.runHeartbeat(child)
	}()
	select {
	case <-ctx.Done():
		cancel()
		routeErr := <-routeDone
		workers.Wait()
		if routeErr != nil && !errors.Is(routeErr, context.Canceled) {
			return routeErr
		}
		return nil
	case err := <-routeDone:
		cancel()
		workers.Wait()
		return err
	}
}

func (r *Runtime) Close() error {
	return r.spool.Close()
}

func (r *Runtime) Info() v1.EventQueueInfo {
	info, err := r.spool.Info()
	if err != nil {
		return v1.EventQueueInfo{Enabled: true, LastErrorAt: time.Now().UnixMilli(), LastErrorCode: "event_spool_status_failed"}
	}
	return info
}

func (r *Runtime) runHeartbeat(ctx context.Context) {
	r.emitHeartbeat(ctx)
	ticker := time.NewTicker(r.heartbeat)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			r.emitHeartbeat(ctx)
		}
	}
}

func (r *Runtime) emitHeartbeat(ctx context.Context) {
	now := time.Now()
	info := r.Info()
	xray := r.state(ctx)
	_, err := r.spool.Enqueue(v1.EventKindHeartbeat, now, v1.HeartbeatEvent{
		AgentStartedAt: r.startedAt.Unix(), XrayRunning: xray.Running, XrayStartedAt: xray.StartedAt,
		XrayConfigVersion: xray.ConfigVersion, QueueEvents: info.PendingEvents, QueueBytes: info.PendingBytes,
		OldestQueuedAt: info.OldestQueuedAt, DroppedRoute: info.DroppedRoute,
		DroppedTelemetry: info.DroppedTelemetry, LastDropAt: info.LastDropAt,
	})
	if errors.Is(err, eventspool.ErrFull) {
		_ = r.spool.RecordDrop(v1.EventKindHeartbeat, now)
	} else if err != nil {
		_ = r.spool.RecordError("event_heartbeat_failed", now)
	}
}
