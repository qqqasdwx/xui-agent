package telemetry

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/identity"
	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

func TestRuntimeWaitsForWorkersBeforeClose(t *testing.T) {
	spool := testRouteStore(t)
	state := func(context.Context) v1.XrayInfo { return v1.XrayInfo{} }
	access, err := NewAccessCollector(filepath.Join(t.TempDir(), "missing", "access.log"), time.Hour, "UTC", spool)
	if err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{
		spool: spool, access: access,
		route: NewRouteServer("127.0.0.1:0", "credential", spool),
		xray:  NewXrayCollector("xray", "127.0.0.1:1", time.Hour, spool, state),
		uploader: NewUploader("http://127.0.0.1:1/agent/v1/events",
			identity.Identity{NodeID: 3, Credential: "credential"}, &http.Client{Timeout: 100 * time.Millisecond}, spool),
		state: state, startedAt: time.Now(), heartbeat: time.Hour,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- runtime.Run(ctx) }()
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runtime did not stop all workers")
	}
	if err := runtime.Close(); err != nil {
		t.Fatal(err)
	}
}
