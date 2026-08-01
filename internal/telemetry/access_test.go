package telemetry

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

const accessLine = "2026/08/01 10:20:30.123456 from 192.0.2.10:43123 accepted tcp:Example.COM:443 [in-443 -> direct] email: alice@example.com\n"

func TestParseAccessLineNormalizesAndDropsInternalAPI(t *testing.T) {
	parsed, keep, err := parseAccessLine(accessLine, time.UTC)
	if err != nil || !keep {
		t.Fatalf("parse = %+v, keep=%v, err=%v", parsed, keep, err)
	}
	if parsed.payload.SourceIP != "192.0.2.10" || parsed.payload.TargetHost != "example.com" || parsed.payload.Network != "tcp" {
		t.Fatalf("payload = %+v", parsed.payload)
	}
	_, keep, err = parseAccessLine("2026/08/01 10:20:30 from 127.0.0.1:1234 accepted tcp:127.0.0.1:62789 [api -> api] email: internal", time.UTC)
	if err != nil || keep {
		t.Fatalf("internal API keep=%v err=%v", keep, err)
	}
	_, keep, err = parseAccessLine("2026/08/01 10:20:30 from 127.0.0.1:1234 accepted tcp:127.0.0.1:62789 [api -> api]", time.UTC)
	if err != nil || keep {
		t.Fatalf("internal API without email keep=%v err=%v", keep, err)
	}
	_, keep, err = parseAccessLine("2026/08/01 10:20:30 from 192.0.2.10:43123 accepted tcp:example.com:443 [in-443 -> direct]", time.UTC)
	if err == nil || keep {
		t.Fatalf("business access without email keep=%v err=%v", keep, err)
	}
}

func TestAccessCollectorStartsAtEndAndKeepsPartialLine(t *testing.T) {
	store := testRouteStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, []byte(accessLine), 0o600); err != nil {
		t.Fatal(err)
	}
	collector, err := NewAccessCollector(path, time.Second, "UTC", store)
	if err != nil {
		t.Fatal(err)
	}
	if err := collector.collectOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	batch, _ := store.Batch(10, 1<<20)
	if len(batch.Events) != 0 {
		t.Fatal("historical access line was replayed on first collection")
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	half := len(accessLine) / 2
	if _, err := file.WriteString(accessLine[:half]); err != nil {
		t.Fatal(err)
	}
	_ = file.Close()
	if err := collector.collectOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	batch, _ = store.Batch(10, 1<<20)
	if len(batch.Events) != 0 {
		t.Fatal("partial access line was committed")
	}
	file, _ = os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = file.WriteString(accessLine[half:])
	_ = file.Close()
	if err := collector.collectOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	batch, _ = store.Batch(10, 1<<20)
	if len(batch.Events) != 1 {
		t.Fatalf("events = %d", len(batch.Events))
	}
	var payload v1.AccessEvent
	if err := json.Unmarshal(batch.Events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Email != "alice@example.com" {
		t.Fatalf("payload = %+v", payload)
	}
}

func TestAccessCollectorHandlesRenameAndCopyTruncate(t *testing.T) {
	store := testRouteStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "access.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	collector, _ := NewAccessCollector(path, time.Second, "UTC", store)
	if err := collector.collectOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(accessLine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collector.collectOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(path, path+".1"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(accessLine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collector.collectOnce(t.Context()); err != nil {
		t.Fatal(err)
	}

	// Rebuild the same inode and immediately regrow beyond the old offset. The
	// checkpoint anchor, rather than size alone, must detect the truncation.
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_TRUNC, 0)
	if err != nil {
		t.Fatal(err)
	}
	rebuilt := strings.ReplaceAll(accessLine, "alice@example.com", "bob@example.com")
	_, _ = file.WriteString(rebuilt + rebuilt)
	_ = file.Close()
	if err := collector.collectOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	batch, err := store.Batch(10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 4 {
		t.Fatalf("events after rename and copy-truncate = %d, want 4", len(batch.Events))
	}
}

func TestAccessCollectorBoundsAndSkipsCompleteOversizeLine(t *testing.T) {
	store := testRouteStore(t)
	path := filepath.Join(t.TempDir(), "access.log")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	collector, err := NewAccessCollector(path, time.Second, "UTC", store)
	if err != nil {
		t.Fatal(err)
	}
	if err := collector.collectOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	oversize := strings.Repeat("x", maxAccessLineBytes+1024) + "\n"
	if err := os.WriteFile(path, []byte(oversize+accessLine), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := collector.collectOnce(t.Context()); err != nil {
		t.Fatal(err)
	}
	batch, err := store.Batch(10, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if len(batch.Events) != 1 {
		t.Fatalf("events after oversize line = %d, want 1", len(batch.Events))
	}
	checkpoint, err := store.Checkpoint(accessCheckpointKey)
	if err != nil {
		t.Fatal(err)
	}
	if checkpoint.Offset != int64(len(oversize)+len(accessLine)) {
		t.Fatalf("checkpoint offset = %d, want %d", checkpoint.Offset, len(oversize)+len(accessLine))
	}
}

func TestReadBoundedAccessLineKeepsIncompleteOversizeUncommitted(t *testing.T) {
	reader := bufio.NewReaderSize(strings.NewReader(strings.Repeat("x", maxAccessLineBytes+1024)), 1024)
	line, err := readBoundedAccessLine(reader)
	if err != nil {
		t.Fatal(err)
	}
	if line.complete || !line.tooLong || len(line.data) != 0 || len(line.anchor) > 64 {
		t.Fatalf("unexpected bounded line state: %+v", line)
	}
}
