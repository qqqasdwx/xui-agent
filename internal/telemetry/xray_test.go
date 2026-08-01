package telemetry

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

func TestParseTrafficAndOnlineStats(t *testing.T) {
	traffic, err := parseTrafficStats([]byte(`{"stat":[{"name":"user>>>alice>>>traffic>>>uplink","value":"12"},{"name":"user>>>alice>>>traffic>>>downlink","value":34},{"name":"inbound>>>x>>>traffic>>>uplink","value":"99"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if traffic["alice"].up != 12 || traffic["alice"].down != 34 || len(traffic) != 1 {
		t.Fatalf("traffic = %+v", traffic)
	}
	online, err := parseOnlineStats([]byte(`{"users":[{"email":"alice","ips":[{"ip":"192.0.2.1","lastSeen":"10"},{"ip":"198.51.100.2","lastSeen":20}]}]}`))
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(online)
	if len(online) != 1 || online[0].IPCount != 2 || online[0].LastSeenAt != 20 {
		t.Fatalf("online = %s", encoded)
	}
	if string(encoded) == "" || containsAny(string(encoded), "192.0.2.1", "198.51.100.2") {
		t.Fatalf("online snapshot retained IP values: %s", encoded)
	}
}

func TestTrafficCounterEpochChangesOnResetAndRuntime(t *testing.T) {
	store := testRouteStore(t)
	responses := [][]byte{
		[]byte(`{"stat":[{"name":"user>>>alice>>>traffic>>>uplink","value":"100"},{"name":"user>>>alice>>>traffic>>>downlink","value":"200"}]}`),
		[]byte(`{"stat":[{"name":"user>>>alice>>>traffic>>>uplink","value":"50"},{"name":"user>>>alice>>>traffic>>>downlink","value":"250"}]}`),
		[]byte(`{"stat":[{"name":"user>>>alice>>>traffic>>>uplink","value":"10"},{"name":"user>>>alice>>>traffic>>>downlink","value":"20"}]}`),
	}
	collector := NewXrayCollector("xray", "127.0.0.1:1", time.Minute, store, func(context.Context) v1.XrayInfo { return v1.XrayInfo{} })
	collector.run = func(context.Context, string, ...string) ([]byte, error) {
		if len(responses) == 0 {
			return nil, errors.New("unexpected call")
		}
		raw := responses[0]
		responses = responses[1:]
		return raw, nil
	}
	if err := collector.collectTraffic(t.Context(), "runtime-1", time.Unix(1, 0)); err != nil {
		t.Fatal(err)
	}
	if err := collector.collectTraffic(t.Context(), "runtime-1", time.Unix(2, 0)); err != nil {
		t.Fatal(err)
	}
	counter, _, _ := store.TrafficCounter("alice")
	if counter.CounterEpoch != 1 || counter.UpBytes != 50 {
		t.Fatalf("reset counter = %+v", counter)
	}
	if err := collector.collectTraffic(t.Context(), "runtime-2", time.Unix(3, 0)); err != nil {
		t.Fatal(err)
	}
	counter, _, _ = store.TrafficCounter("alice")
	if counter.CounterEpoch != 0 || counter.RuntimeID != "runtime-2" {
		t.Fatalf("new runtime counter = %+v", counter)
	}
}

func containsAny(value string, needles ...string) bool {
	for _, needle := range needles {
		if len(needle) > 0 && strings.Contains(value, needle) {
			return true
		}
	}
	return false
}
