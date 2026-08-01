package eventspool

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	bolt "go.etcd.io/bbolt"

	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

const (
	defaultMaxBytes  = 256 << 20
	defaultMaxEvents = 100000
)

var ErrFull = errors.New("event spool capacity reached")

var (
	bucketMeta        = []byte("meta")
	bucketEvents      = []byte("events")
	bucketCheckpoints = []byte("checkpoints")
	bucketCounters    = []byte("traffic-counters")
	keyStreamID       = []byte("stream-id")
	keyNextSequence   = []byte("next-sequence")
	keyAckedSequence  = []byte("acked-sequence")
	keyPendingBytes   = []byte("pending-bytes")
	keyPendingEvents  = []byte("pending-events")
	keyDroppedRoute   = []byte("dropped-route")
	keyDroppedOther   = []byte("dropped-telemetry")
	keyLastDropAt     = []byte("last-drop-at")
	keyLastAckAt      = []byte("last-ack-at")
	keyLastErrorAt    = []byte("last-error-at")
	keyLastErrorCode  = []byte("last-error-code")
	keyLastAccessAt   = []byte("last-access-at")
	keyLastRouteAt    = []byte("last-route-at")
	keyLastTrafficAt  = []byte("last-traffic-at")
	keyLastOnlineAt   = []byte("last-online-at")
)

type Config struct {
	Path      string
	NodeID    int
	MaxBytes  uint64
	MaxEvents uint64
}

type Store struct {
	db        *bolt.DB
	nodeID    int
	maxBytes  uint64
	maxEvents uint64
}

type AccessCheckpoint struct {
	Device       uint64 `json:"device"`
	Inode        uint64 `json:"inode"`
	Offset       int64  `json:"offset"`
	AnchorOffset int64  `json:"anchorOffset"`
	AnchorSHA256 string `json:"anchorSha256"`
	Initialized  bool   `json:"initialized"`
}

type TrafficCounter struct {
	RuntimeID    string `json:"runtimeId"`
	UpBytes      uint64 `json:"upBytes"`
	DownBytes    uint64 `json:"downBytes"`
	CounterEpoch uint64 `json:"counterEpoch"`
}

func Open(cfg Config) (*Store, error) {
	if cfg.NodeID <= 0 {
		return nil, errors.New("event spool node id must be positive")
	}
	if !filepath.IsAbs(cfg.Path) {
		return nil, errors.New("event spool path must be absolute")
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = defaultMaxBytes
	}
	if cfg.MaxEvents == 0 {
		cfg.MaxEvents = defaultMaxEvents
	}
	if err := os.MkdirAll(filepath.Dir(cfg.Path), 0o700); err != nil {
		return nil, fmt.Errorf("create event spool directory: %w", err)
	}
	db, err := bolt.Open(cfg.Path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open event spool: %w", err)
	}
	store := &Store{db: db, nodeID: cfg.NodeID, maxBytes: cfg.MaxBytes, maxEvents: cfg.MaxEvents}
	if err := store.initialize(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

func (s *Store) initialize() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists(bucketMeta)
		if err != nil {
			return err
		}
		for _, name := range [][]byte{bucketEvents, bucketCheckpoints, bucketCounters} {
			if _, err := tx.CreateBucketIfNotExists(name); err != nil {
				return err
			}
		}
		if len(meta.Get(keyStreamID)) == 0 {
			streamID, err := randomStreamID()
			if err != nil {
				return err
			}
			if err := meta.Put(keyStreamID, []byte(streamID)); err != nil {
				return err
			}
		}
		if readUint64(meta.Get(keyNextSequence)) == 0 {
			return putUint64(meta, keyNextSequence, 1)
		}
		return nil
	})
}

func randomStreamID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("generate event stream id: %w", err)
	}
	return hex.EncodeToString(raw), nil
}

func (s *Store) Enqueue(kind string, observedAt time.Time, payload any) (v1.Event, error) {
	return s.enqueue(kind, observedAt, payload, nil)
}

func (s *Store) EnqueueWithCheckpoint(kind string, observedAt time.Time, payload any, checkpointKey string, checkpoint AccessCheckpoint) (v1.Event, error) {
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return v1.Event{}, fmt.Errorf("encode access checkpoint: %w", err)
	}
	return s.enqueue(kind, observedAt, payload, func(tx *bolt.Tx) error {
		if checkpointKey == "" {
			return errors.New("access checkpoint key is required")
		}
		return tx.Bucket(bucketCheckpoints).Put([]byte(checkpointKey), raw)
	})
}

func (s *Store) EnqueueTrafficSnapshot(observedAt time.Time, payload v1.TrafficSnapshotEvent, counters map[string]TrafficCounter) (v1.Event, error) {
	encoded := make(map[string][]byte, len(counters))
	for email, counter := range counters {
		raw, err := json.Marshal(counter)
		if err != nil {
			return v1.Event{}, err
		}
		encoded[email] = raw
	}
	return s.enqueue(v1.EventKindTrafficSnapshot, observedAt, payload, func(tx *bolt.Tx) error {
		bucket := tx.Bucket(bucketCounters)
		for email, raw := range encoded {
			if err := bucket.Put([]byte(email), raw); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) enqueue(kind string, observedAt time.Time, payload any, sideEffect func(*bolt.Tx) error) (v1.Event, error) {
	if !validKind(kind) {
		return v1.Event{}, fmt.Errorf("unsupported event kind %q", kind)
	}
	if observedAt.IsZero() || observedAt.UnixMilli() <= 0 {
		return v1.Event{}, errors.New("event observed time is required")
	}
	payloadRaw, err := json.Marshal(payload)
	if err != nil {
		return v1.Event{}, fmt.Errorf("encode %s event payload: %w", kind, err)
	}
	if len(payloadRaw) == 0 || len(payloadRaw) > v1.MaxEventBytes {
		return v1.Event{}, errors.New("event payload exceeds the size limit")
	}
	var event v1.Event
	err = s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		sequence := readUint64(meta.Get(keyNextSequence))
		streamID := string(meta.Get(keyStreamID))
		event = v1.Event{
			Sequence: sequence, Kind: kind, ObservedAt: observedAt.UnixMilli(), Payload: payloadRaw,
		}
		event.EventID = eventID(s.nodeID, streamID, event)
		encoded, err := json.Marshal(event)
		if err != nil {
			return err
		}
		if len(encoded) > v1.MaxEventBytes {
			return errors.New("event exceeds the size limit")
		}
		pendingBytes := readUint64(meta.Get(keyPendingBytes))
		pendingEvents := readUint64(meta.Get(keyPendingEvents))
		if uint64(len(encoded))+pendingBytes > s.maxBytes || pendingEvents >= s.maxEvents {
			return ErrFull
		}
		if err := tx.Bucket(bucketEvents).Put(sequenceKey(sequence), encoded); err != nil {
			return err
		}
		if sideEffect != nil {
			if err := sideEffect(tx); err != nil {
				return err
			}
		}
		if err := putUint64(meta, keyNextSequence, sequence+1); err != nil {
			return err
		}
		if err := putUint64(meta, keyPendingBytes, pendingBytes+uint64(len(encoded))); err != nil {
			return err
		}
		if err := putUint64(meta, keyPendingEvents, pendingEvents+1); err != nil {
			return err
		}
		if key := kindTimestampKey(kind); key != nil {
			return putInt64(meta, key, observedAt.UnixMilli())
		}
		return nil
	})
	if err != nil {
		return v1.Event{}, err
	}
	return event, nil
}

func (s *Store) SetCheckpoint(key string, checkpoint AccessCheckpoint) error {
	if strings.TrimSpace(key) == "" {
		return errors.New("access checkpoint key is required")
	}
	raw, err := json.Marshal(checkpoint)
	if err != nil {
		return err
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		return tx.Bucket(bucketCheckpoints).Put([]byte(key), raw)
	})
}

func (s *Store) Checkpoint(key string) (AccessCheckpoint, error) {
	var result AccessCheckpoint
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketCheckpoints).Get([]byte(key))
		if len(raw) == 0 {
			return os.ErrNotExist
		}
		return json.Unmarshal(raw, &result)
	})
	return result, err
}

func (s *Store) Batch(maxEvents int, maxBytes int) (v1.EventBatch, error) {
	if maxEvents <= 0 || maxEvents > v1.MaxEventBatchEvents {
		maxEvents = v1.MaxEventBatchEvents
	}
	if maxBytes <= 0 || maxBytes > v1.MaxEventExpandedBytes {
		maxBytes = v1.MaxEventExpandedBytes
	}
	batch := v1.EventBatch{Version: v1.EventVersion}
	err := s.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		batch.StreamID = string(meta.Get(keyStreamID))
		cursor := tx.Bucket(bucketEvents).Cursor()
		total := 0
		for key, value := cursor.First(); key != nil && len(batch.Events) < maxEvents; key, value = cursor.Next() {
			if total+len(value) > maxBytes && len(batch.Events) > 0 {
				break
			}
			var event v1.Event
			if err := json.Unmarshal(value, &event); err != nil {
				return fmt.Errorf("decode queued event %d: %w", readUint64(key), err)
			}
			batch.Events = append(batch.Events, event)
			total += len(value)
		}
		return nil
	})
	if err != nil {
		return v1.EventBatch{}, err
	}
	if len(batch.Events) > 0 {
		batch.FirstSequence = batch.Events[0].Sequence
		batch.LastSequence = batch.Events[len(batch.Events)-1].Sequence
	}
	return batch, nil
}

func (s *Store) Acknowledge(streamID string, sequence uint64, now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if streamID != string(meta.Get(keyStreamID)) {
			return errors.New("event acknowledgement stream does not match")
		}
		next := readUint64(meta.Get(keyNextSequence))
		if sequence >= next {
			return errors.New("event acknowledgement exceeds the last assigned sequence")
		}
		acked := readUint64(meta.Get(keyAckedSequence))
		if sequence < acked {
			return nil
		}
		pendingBytes := readUint64(meta.Get(keyPendingBytes))
		pendingEvents := readUint64(meta.Get(keyPendingEvents))
		bucket := tx.Bucket(bucketEvents)
		cursor := bucket.Cursor()
		for key, value := cursor.First(); key != nil && readUint64(key) <= sequence; key, value = cursor.Next() {
			if uint64(len(value)) > pendingBytes || pendingEvents == 0 {
				return errors.New("event spool accounting is inconsistent")
			}
			pendingBytes -= uint64(len(value))
			pendingEvents--
			if err := cursor.Delete(); err != nil {
				return err
			}
		}
		if err := putUint64(meta, keyAckedSequence, sequence); err != nil {
			return err
		}
		if err := putUint64(meta, keyPendingBytes, pendingBytes); err != nil {
			return err
		}
		if err := putUint64(meta, keyPendingEvents, pendingEvents); err != nil {
			return err
		}
		if err := putInt64(meta, keyLastAckAt, now.UnixMilli()); err != nil {
			return err
		}
		return meta.Delete(keyLastErrorCode)
	})
}

func (s *Store) RecordDrop(kind string, now time.Time) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		key := keyDroppedOther
		if kind == v1.EventKindRoute {
			key = keyDroppedRoute
		}
		if err := putUint64(meta, key, readUint64(meta.Get(key))+1); err != nil {
			return err
		}
		return putInt64(meta, keyLastDropAt, now.UnixMilli())
	})
}

func (s *Store) RecordError(code string, now time.Time) error {
	code = strings.TrimSpace(code)
	if len(code) > 64 {
		code = code[:64]
	}
	return s.db.Update(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		if err := meta.Put(keyLastErrorCode, []byte(code)); err != nil {
			return err
		}
		return putInt64(meta, keyLastErrorAt, now.UnixMilli())
	})
}

func (s *Store) Info() (v1.EventQueueInfo, error) {
	var info v1.EventQueueInfo
	err := s.db.View(func(tx *bolt.Tx) error {
		meta := tx.Bucket(bucketMeta)
		info.Enabled = true
		info.StreamID = string(meta.Get(keyStreamID))
		info.PendingEvents = readUint64(meta.Get(keyPendingEvents))
		info.PendingBytes = readUint64(meta.Get(keyPendingBytes))
		info.HighestAckedSequence = readUint64(meta.Get(keyAckedSequence))
		info.LastReceivedAt = readInt64(meta.Get(keyLastAckAt))
		info.LastErrorAt = readInt64(meta.Get(keyLastErrorAt))
		info.LastErrorCode = string(meta.Get(keyLastErrorCode))
		info.LastAccessAt = readInt64(meta.Get(keyLastAccessAt))
		info.LastRouteAt = readInt64(meta.Get(keyLastRouteAt))
		info.LastTrafficAt = readInt64(meta.Get(keyLastTrafficAt))
		info.LastOnlineAt = readInt64(meta.Get(keyLastOnlineAt))
		info.DroppedRoute = readUint64(meta.Get(keyDroppedRoute))
		info.DroppedTelemetry = readUint64(meta.Get(keyDroppedOther))
		info.LastDropAt = readInt64(meta.Get(keyLastDropAt))
		_, value := tx.Bucket(bucketEvents).Cursor().First()
		if value != nil {
			var event v1.Event
			if err := json.Unmarshal(value, &event); err != nil {
				return err
			}
			info.OldestQueuedAt = event.ObservedAt
		}
		return nil
	})
	return info, err
}

func (s *Store) TrafficCounter(email string) (TrafficCounter, bool, error) {
	var counter TrafficCounter
	found := false
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket(bucketCounters).Get([]byte(email))
		if len(raw) == 0 {
			return nil
		}
		found = true
		return json.Unmarshal(raw, &counter)
	})
	return counter, found, err
}

func validKind(kind string) bool {
	switch kind {
	case v1.EventKindAccess, v1.EventKindRoute, v1.EventKindHeartbeat, v1.EventKindTrafficSnapshot, v1.EventKindOnlineSnapshot:
		return true
	default:
		return false
	}
}

func kindTimestampKey(kind string) []byte {
	switch kind {
	case v1.EventKindAccess:
		return keyLastAccessAt
	case v1.EventKindRoute:
		return keyLastRouteAt
	case v1.EventKindTrafficSnapshot:
		return keyLastTrafficAt
	case v1.EventKindOnlineSnapshot:
		return keyLastOnlineAt
	default:
		return nil
	}
}

func eventID(nodeID int, streamID string, event v1.Event) string {
	h := sha256.New()
	for _, value := range []string{
		"xui-event-v1", strconv.Itoa(nodeID), streamID, strconv.FormatUint(event.Sequence, 10),
		event.Kind, strconv.FormatInt(event.ObservedAt, 10), string(event.Payload),
	} {
		_, _ = h.Write([]byte(value))
		_, _ = h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

func sequenceKey(sequence uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, sequence)
	return key
}

func readUint64(value []byte) uint64 {
	if len(value) != 8 {
		return 0
	}
	return binary.BigEndian.Uint64(value)
}

func readInt64(value []byte) int64 {
	return int64(readUint64(value))
}

func putUint64(bucket *bolt.Bucket, key []byte, value uint64) error {
	raw := make([]byte, 8)
	binary.BigEndian.PutUint64(raw, value)
	return bucket.Put(key, raw)
}

func putInt64(bucket *bolt.Bucket, key []byte, value int64) error {
	return putUint64(bucket, key, uint64(value))
}
