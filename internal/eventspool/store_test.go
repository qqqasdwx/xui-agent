package eventspool

import (
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

func openTestStore(t *testing.T, maxBytes, maxEvents uint64) *Store {
	t.Helper()
	store, err := Open(Config{
		Path: filepath.Join(t.TempDir(), "events.db"), NodeID: 7,
		MaxBytes: maxBytes, MaxEvents: maxEvents,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func TestEventAndCheckpointCommitAtomically(t *testing.T) {
	store := openTestStore(t, 1<<20, 100)
	now := time.Unix(100, 123000000)
	checkpoint := AccessCheckpoint{Device: 1, Inode: 2, Offset: 45, Initialized: true}
	event, err := store.EnqueueWithCheckpoint(v1.EventKindAccess, now, v1.AccessEvent{
		Email: "alice", SourceIP: "192.0.2.1", SourcePort: 1234, Network: "tcp",
		TargetHost: "example.com", TargetPort: 443, OutboundTag: "direct",
	}, "access", checkpoint)
	if err != nil {
		t.Fatal(err)
	}
	if event.Sequence != 1 || len(event.EventID) != 64 {
		t.Fatalf("event = %+v", event)
	}
	got, err := store.Checkpoint("access")
	if err != nil || got != checkpoint {
		t.Fatalf("checkpoint = %+v, %v", got, err)
	}
	batch, err := store.Batch(500, 1<<20)
	if err != nil {
		t.Fatal(err)
	}
	if batch.FirstSequence != 1 || batch.LastSequence != 1 || len(batch.Events) != 1 {
		t.Fatalf("batch = %+v", batch)
	}
	if err := store.Acknowledge(batch.StreamID, 1, now.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	info, err := store.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.PendingEvents != 0 || info.PendingBytes != 0 || info.HighestAckedSequence != 1 {
		t.Fatalf("info = %+v", info)
	}
}

func TestEventIDFixedVectorMatchesCenter(t *testing.T) {
	payload := v1.AccessEvent{
		Email: "alice", SourceIP: "192.0.2.1", SourcePort: 1234, Network: "tcp",
		TargetHost: "example.com", TargetPort: 443, OutboundTag: "direct",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	event := v1.Event{Sequence: 1, Kind: v1.EventKindAccess, ObservedAt: 100123, Payload: raw}
	const streamID = "0123456789abcdef0123456789abcdef"
	const want = "c1b7e65d2c534da0269a2f0b1f17b9a33ca3bda8a86ffe95d7e60390980186cf"
	if got := eventID(7, streamID, event); got != want {
		t.Fatalf("event ID = %q, want %q", got, want)
	}
}

func TestStreamAndSequenceSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "events.db")
	store, err := Open(Config{Path: path, NodeID: 8, MaxBytes: 1 << 20, MaxEvents: 100})
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.Enqueue(v1.EventKindHeartbeat, time.Unix(200, 0), v1.HeartbeatEvent{})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := store.Info()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(Config{Path: path, NodeID: 8, MaxBytes: 1 << 20, MaxEvents: 100})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	second, err := store.Enqueue(v1.EventKindHeartbeat, time.Unix(201, 0), v1.HeartbeatEvent{})
	if err != nil {
		t.Fatal(err)
	}
	after, _ := store.Info()
	if first.Sequence != 1 || second.Sequence != 2 || before.StreamID == "" || before.StreamID != after.StreamID {
		t.Fatalf("before=%+v after=%+v sequences=%d/%d", before, after, first.Sequence, second.Sequence)
	}
}

func TestCapacityDoesNotAdvanceCheckpoint(t *testing.T) {
	store := openTestStore(t, 1<<20, 1)
	if _, err := store.Enqueue(v1.EventKindHeartbeat, time.Unix(300, 0), v1.HeartbeatEvent{}); err != nil {
		t.Fatal(err)
	}
	checkpoint := AccessCheckpoint{Device: 1, Inode: 2, Offset: 99, Initialized: true}
	if _, err := store.EnqueueWithCheckpoint(v1.EventKindAccess, time.Unix(301, 0), v1.AccessEvent{}, "access", checkpoint); !errors.Is(err, ErrFull) {
		t.Fatalf("enqueue error = %v", err)
	}
	if _, err := store.Checkpoint("access"); err == nil {
		t.Fatal("checkpoint advanced when event enqueue failed")
	}
}

func TestRejectsAcknowledgementPastAssignedSequence(t *testing.T) {
	store := openTestStore(t, 1<<20, 10)
	info, err := store.Info()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Acknowledge(info.StreamID, 1, time.Now()); err == nil {
		t.Fatal("acknowledgement past the assigned range succeeded")
	}
}
