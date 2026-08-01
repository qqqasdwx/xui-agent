package telemetry

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/eventspool"
	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

func testRouteStore(t *testing.T) *eventspool.Store {
	t.Helper()
	store, err := eventspool.Open(eventspool.Config{
		Path: filepath.Join(t.TempDir(), "events.db"), NodeID: 3, MaxBytes: 1 << 20, MaxEvents: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestRouteWebhookAuthenticatesAndWhitelists(t *testing.T) {
	store := testRouteStore(t)
	server := NewRouteServer("127.0.0.1:0", "credential", store)
	body := `{"email":"alice","level":0,"protocol":"http1","network":"tcp","source":"192.0.2.1:1234","destination":"198.51.100.2:443","originalTarget":"tcp:198.51.100.2:443","routeTarget":"tcp:example.com:443","inboundTag":"secret-inbound","inboundName":"secret-name","inboundLocal":"127.0.0.1:8443","outboundTag":"direct","ts":1785585600}`
	req := httptest.NewRequest(http.MethodPost, "/xray-route", strings.NewReader(body))
	req.Header.Set(v1.RouteWebhookTokenHeader, server.token)
	recorder := httptest.NewRecorder()
	server.handle(recorder, req)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status = %d", recorder.Code)
	}
	batch, err := store.Batch(10, 1<<20)
	if err != nil || len(batch.Events) != 1 {
		t.Fatalf("batch = %+v, %v", batch, err)
	}
	encoded, _ := json.Marshal(batch.Events[0])
	for _, forbidden := range []string{"192.0.2.1", "secret-inbound", "secret-name", "inboundLocal", "level"} {
		if bytes.Contains(encoded, []byte(forbidden)) {
			t.Fatalf("queued route contains forbidden value %q: %s", forbidden, encoded)
		}
	}
	var event v1.RouteEvent
	if err := json.Unmarshal(batch.Events[0].Payload, &event); err != nil {
		t.Fatal(err)
	}
	if event.Protocol != "http1" || event.RouteHost != "example.com" || event.RoutePort != 443 {
		t.Fatalf("route = %+v", event)
	}
}

func TestRouteWebhookRejectsMissingTokenAndOversize(t *testing.T) {
	store := testRouteStore(t)
	server := NewRouteServer("127.0.0.1:0", "credential", store)
	req := httptest.NewRequest(http.MethodPost, "/xray-route", strings.NewReader(`{"ts":1}`))
	recorder := httptest.NewRecorder()
	server.handle(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing token status = %d", recorder.Code)
	}

	req = httptest.NewRequest(http.MethodPost, "/xray-route", strings.NewReader(`{"ts":1,"padding":"`+strings.Repeat("x", maxRouteWebhookBytes)+`"}`))
	req.Header.Set(v1.RouteWebhookTokenHeader, server.token)
	recorder = httptest.NewRecorder()
	server.handle(recorder, req)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize status = %d", recorder.Code)
	}
}

func TestRouteWebhookReportsFullSpoolWithoutReplacingEvents(t *testing.T) {
	store, err := eventspool.Open(eventspool.Config{
		Path: filepath.Join(t.TempDir(), "events.db"), NodeID: 3, MaxBytes: 1 << 20, MaxEvents: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Enqueue(v1.EventKindHeartbeat, time.Unix(1, 0), v1.HeartbeatEvent{}); err != nil {
		t.Fatal(err)
	}
	server := NewRouteServer("127.0.0.1:0", "credential", store)
	req := httptest.NewRequest(http.MethodPost, "/xray-route", strings.NewReader(`{"ts":1785585600}`))
	req.Header.Set(v1.RouteWebhookTokenHeader, server.token)
	recorder := httptest.NewRecorder()
	server.handle(recorder, req)
	if recorder.Code != http.StatusInsufficientStorage {
		t.Fatalf("full spool status = %d, want 507", recorder.Code)
	}
	info, err := store.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.PendingEvents != 1 || info.DroppedRoute != 1 {
		t.Fatalf("full spool state = %+v", info)
	}
}

func TestRouteWebhookTokenFixedVector(t *testing.T) {
	const want = "f4d42cdd03a873ce13767ffae80171267acd0b240cc7b8ed139945fc9059c2eb"
	if got := RouteWebhookToken("test-node-credential"); got != want {
		t.Fatalf("route webhook token = %q, want %q", got, want)
	}
}

func TestRouteWebhookOpenRejectsOccupiedAddress(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := NewRouteServer(listener.Addr().String(), "credential", testRouteStore(t))
	if err := server.Open(); err == nil {
		t.Fatal("occupied route webhook address was accepted")
	}
}
