package telemetry

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/eventspool"
	"github.com/qqqasdwx/xui-agent/internal/identity"
	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

func TestUploaderDeletesOnlyAcknowledgedEvents(t *testing.T) {
	store := testRouteStore(t)
	for i := 0; i < 2; i++ {
		if _, err := store.Enqueue(v1.EventKindHeartbeat, time.Unix(int64(i+1), 0), v1.HeartbeatEvent{}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer credential" || r.Header.Get(v1.EventVersionHeader) != v1.EventVersion {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		defer reader.Close()
		var batch v1.EventBatch
		if err := json.NewDecoder(reader).Decode(&batch); err != nil {
			t.Error(err)
			return
		}
		_ = json.NewEncoder(w).Encode(v1.EventBatchAck{
			Version: v1.EventVersion, StreamID: batch.StreamID, HighestContiguousSequence: batch.FirstSequence,
		})
	}))
	defer server.Close()
	uploader := NewUploader(server.URL, identity.Identity{NodeID: 3, Credential: "credential"}, server.Client(), store)
	had, err := uploader.uploadOnce(t.Context())
	if err != nil || !had {
		t.Fatalf("upload = %v, %v", had, err)
	}
	info, _ := store.Info()
	if info.PendingEvents != 1 || info.HighestAckedSequence != 1 {
		t.Fatalf("info = %+v", info)
	}
}

func TestUploaderRejectsAcknowledgementBeyondUploadedBatch(t *testing.T) {
	store, err := eventspool.Open(eventspool.Config{
		Path: filepath.Join(t.TempDir(), "events.db"), NodeID: 3, MaxBytes: 1 << 20, MaxEvents: 600,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for i := 0; i < v1.MaxEventBatchEvents+1; i++ {
		if _, err := store.Enqueue(v1.EventKindHeartbeat, time.Unix(int64(i+1), 0), v1.HeartbeatEvent{}); err != nil {
			t.Fatal(err)
		}
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		defer reader.Close()
		var batch v1.EventBatch
		if err := json.NewDecoder(reader).Decode(&batch); err != nil {
			t.Error(err)
			return
		}
		_ = json.NewEncoder(w).Encode(v1.EventBatchAck{
			Version: v1.EventVersion, StreamID: batch.StreamID,
			HighestContiguousSequence: batch.LastSequence + 1,
		})
	}))
	defer server.Close()
	uploader := NewUploader(server.URL, identity.Identity{NodeID: 3, Credential: "credential"}, server.Client(), store)
	if _, err := uploader.uploadOnce(t.Context()); err == nil {
		t.Fatal("acknowledgement beyond the uploaded batch was accepted")
	}
	info, err := store.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.PendingEvents != v1.MaxEventBatchEvents+1 || info.HighestAckedSequence != 0 {
		t.Fatalf("events were deleted after invalid acknowledgement: %+v", info)
	}
}

func TestUploaderRejectsTrailingAcknowledgementJSON(t *testing.T) {
	store := testRouteStore(t)
	if _, err := store.Enqueue(v1.EventKindHeartbeat, time.Unix(1, 0), v1.HeartbeatEvent{}); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reader, err := gzip.NewReader(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		defer reader.Close()
		var batch v1.EventBatch
		if err := json.NewDecoder(reader).Decode(&batch); err != nil {
			t.Error(err)
			return
		}
		_, _ = fmt.Fprintf(w, `{"version":"%s","streamId":"%s","highestContiguousSequence":1}{}`, v1.EventVersion, batch.StreamID)
	}))
	defer server.Close()
	uploader := NewUploader(server.URL, identity.Identity{NodeID: 3, Credential: "credential"}, server.Client(), store)
	if _, err := uploader.uploadOnce(t.Context()); err == nil {
		t.Fatal("acknowledgement with trailing JSON was accepted")
	}
	info, err := store.Info()
	if err != nil {
		t.Fatal(err)
	}
	if info.PendingEvents != 1 || info.HighestAckedSequence != 0 {
		t.Fatalf("event was deleted after invalid acknowledgement: %+v", info)
	}
}
