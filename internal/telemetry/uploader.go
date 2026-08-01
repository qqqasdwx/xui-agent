package telemetry

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/eventspool"
	"github.com/qqqasdwx/xui-agent/internal/identity"
	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

const maxUploadResponseBytes = 64 << 10

type Uploader struct {
	endpoint string
	identity identity.Identity
	client   *http.Client
	spool    *eventspool.Store
}

func NewUploader(endpoint string, id identity.Identity, client *http.Client, spool *eventspool.Store) *Uploader {
	return &Uploader{endpoint: endpoint, identity: id, client: client, spool: spool}
}

func (u *Uploader) Run(ctx context.Context) {
	backoff := time.Second
	for {
		if err := ctx.Err(); err != nil {
			return
		}
		hadEvents, err := u.uploadOnce(ctx)
		if err == nil {
			backoff = time.Second
			delay := time.Second
			if hadEvents {
				delay = 100 * time.Millisecond
			}
			if !waitContext(ctx, delay) {
				return
			}
			continue
		}
		_ = u.spool.RecordError(uploadErrorCode(err), time.Now())
		if !waitContext(ctx, backoff) {
			return
		}
		backoff *= 2
		if backoff > time.Minute {
			backoff = time.Minute
		}
	}
}

func (u *Uploader) uploadOnce(ctx context.Context) (bool, error) {
	batch, err := u.spool.Batch(v1.MaxEventBatchEvents, 512<<10)
	if err != nil {
		return false, err
	}
	if len(batch.Events) == 0 {
		return false, nil
	}
	raw, err := json.Marshal(batch)
	if err != nil {
		return true, err
	}
	var compressed bytes.Buffer
	writer, _ := gzip.NewWriterLevel(&compressed, gzip.BestSpeed)
	if _, err := writer.Write(raw); err != nil {
		return true, err
	}
	if err := writer.Close(); err != nil {
		return true, err
	}
	if compressed.Len() > v1.MaxEventCompressedBytes {
		return true, errors.New("compressed event batch exceeds limit")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u.endpoint, &compressed)
	if err != nil {
		return true, err
	}
	req.Header.Set("Authorization", "Bearer "+u.identity.Credential)
	req.Header.Set("X-XUI-Agent-Node-ID", strconv.Itoa(u.identity.NodeID))
	req.Header.Set(v1.EventVersionHeader, v1.EventVersion)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	resp, err := u.client.Do(req)
	if err != nil {
		return true, errors.New("event endpoint unavailable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxUploadResponseBytes+1))
	if err != nil {
		return true, errors.New("event acknowledgement read failed")
	}
	if len(body) > maxUploadResponseBytes {
		return true, errors.New("event acknowledgement exceeds limit")
	}
	if resp.StatusCode != http.StatusOK {
		return true, fmt.Errorf("event endpoint returned HTTP %d", resp.StatusCode)
	}
	var ack v1.EventBatchAck
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ack); err != nil {
		return true, errors.New("invalid event acknowledgement")
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return true, errors.New("invalid event acknowledgement")
	}
	if ack.Version != v1.EventVersion || ack.StreamID != batch.StreamID {
		return true, errors.New("event acknowledgement contract mismatch")
	}
	if ack.HighestContiguousSequence < batch.FirstSequence {
		return true, errors.New("event acknowledgement did not reach the first batch sequence")
	}
	if ack.HighestContiguousSequence > batch.LastSequence {
		return true, errors.New("event acknowledgement exceeds the uploaded batch")
	}
	if err := u.spool.Acknowledge(ack.StreamID, ack.HighestContiguousSequence, time.Now()); err != nil {
		return true, err
	}
	return true, nil
}

func waitContext(ctx context.Context, duration time.Duration) bool {
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func uploadErrorCode(err error) string {
	if err == nil {
		return ""
	}
	return "event_upload_failed"
}
