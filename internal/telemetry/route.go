package telemetry

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/qqqasdwx/xui-agent/internal/eventspool"
	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

const maxRouteWebhookBytes = 64 << 10

type RouteServer struct {
	address  string
	token    string
	spool    *eventspool.Store
	listener net.Listener
}

type xrayRoutePayload struct {
	Email          *string `json:"email"`
	Level          *uint32 `json:"level"`
	Protocol       *string `json:"protocol"`
	Network        *string `json:"network"`
	Source         *string `json:"source"`
	Destination    *string `json:"destination"`
	OriginalTarget *string `json:"originalTarget"`
	RouteTarget    *string `json:"routeTarget"`
	InboundTag     *string `json:"inboundTag"`
	InboundName    *string `json:"inboundName"`
	InboundLocal   *string `json:"inboundLocal"`
	OutboundTag    *string `json:"outboundTag"`
	Timestamp      int64   `json:"ts"`
}

func NewRouteServer(address, credential string, spool *eventspool.Store) *RouteServer {
	return &RouteServer{address: address, token: RouteWebhookToken(credential), spool: spool}
}

func RouteWebhookToken(credential string) string {
	credentialDigest := sha256.Sum256([]byte(credential))
	h := sha256.New()
	_, _ = h.Write([]byte("xui-route-webhook-v1"))
	_, _ = h.Write([]byte{0})
	_, _ = h.Write([]byte(hex.EncodeToString(credentialDigest[:])))
	return hex.EncodeToString(h.Sum(nil))
}

func (s *RouteServer) Run(ctx context.Context) error {
	if s.listener == nil {
		if err := s.Open(); err != nil {
			return err
		}
	}
	server := &http.Server{
		Handler:           http.HandlerFunc(s.handle),
		ReadHeaderTimeout: 2 * time.Second,
		ReadTimeout:       2 * time.Second,
		WriteTimeout:      2 * time.Second,
		IdleTimeout:       15 * time.Second,
	}
	done := make(chan error, 1)
	go func() {
		err := server.Serve(s.listener)
		if errors.Is(err, http.ErrServerClosed) {
			err = nil
		}
		done <- err
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return err
		}
		return <-done
	case err := <-done:
		return err
	}
}

func (s *RouteServer) Open() error {
	if s.listener != nil {
		return nil
	}
	listener, err := net.Listen("tcp", s.address)
	if err != nil {
		return fmt.Errorf("listen for route webhook: %w", err)
	}
	s.listener = listener
	return nil
}

func (s *RouteServer) handle(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/xray-route" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	presented := r.Header.Get(v1.RouteWebhookTokenHeader)
	if len(presented) != len(s.token) || subtle.ConstantTimeCompare([]byte(presented), []byte(s.token)) != 1 {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxRouteWebhookBytes))
	decoder.DisallowUnknownFields()
	var raw xrayRoutePayload
	if err := decoder.Decode(&raw); err != nil {
		w.WriteHeader(routeDecodeStatus(err))
		return
	}
	var extra any
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	event, observedAt, err := normalizeRoute(raw)
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	if _, err := s.spool.Enqueue(v1.EventKindRoute, observedAt, event); err != nil {
		if errors.Is(err, eventspool.ErrFull) {
			_ = s.spool.RecordDrop(v1.EventKindRoute, time.Now())
			w.WriteHeader(http.StatusInsufficientStorage)
			return
		}
		_ = s.spool.RecordError("route_spool_failed", time.Now())
		slog.Warn("route event persistence failed", "reason", "route_spool_failed")
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func normalizeRoute(raw xrayRoutePayload) (v1.RouteEvent, time.Time, error) {
	if raw.Timestamp <= 0 {
		return v1.RouteEvent{}, time.Time{}, errors.New("route timestamp is required")
	}
	event := v1.RouteEvent{}
	var err error
	if event.Email, err = cleanOptional(raw.Email, 254); err != nil {
		return v1.RouteEvent{}, time.Time{}, err
	}
	if event.Protocol, err = cleanOptional(raw.Protocol, 32); err != nil {
		return v1.RouteEvent{}, time.Time{}, err
	}
	if event.Network, err = cleanOptional(raw.Network, 8); err != nil {
		return v1.RouteEvent{}, time.Time{}, err
	}
	if event.Network != "" && event.Network != "tcp" && event.Network != "udp" {
		return v1.RouteEvent{}, time.Time{}, errors.New("invalid route network")
	}
	if event.OutboundTag, err = cleanOptional(raw.OutboundTag, 128); err != nil {
		return v1.RouteEvent{}, time.Time{}, err
	}
	if event.DestinationHost, event.DestinationPort, err = splitOptionalTarget(raw.Destination); err != nil {
		return v1.RouteEvent{}, time.Time{}, err
	}
	if event.OriginalHost, event.OriginalPort, err = splitOptionalTarget(raw.OriginalTarget); err != nil {
		return v1.RouteEvent{}, time.Time{}, err
	}
	if event.RouteHost, event.RoutePort, err = splitOptionalTarget(raw.RouteTarget); err != nil {
		return v1.RouteEvent{}, time.Time{}, err
	}
	return event, time.Unix(raw.Timestamp, 0), nil
}

func cleanOptional(value *string, max int) (string, error) {
	if value == nil {
		return "", nil
	}
	return cleanToken(*value, max, true)
}

func splitOptionalTarget(value *string) (string, uint16, error) {
	if value == nil || strings.TrimSpace(*value) == "" {
		return "", 0, nil
	}
	return splitXrayTarget(*value)
}

func routeDecodeStatus(err error) int {
	var maxErr *http.MaxBytesError
	if errors.As(err, &maxErr) {
		return http.StatusRequestEntityTooLarge
	}
	return http.StatusBadRequest
}
