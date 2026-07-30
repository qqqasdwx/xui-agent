package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
	v1 "github.com/qqqasdwx/xui-agent/protocol/v1"
)

const (
	testToken      = "installer-integration-token"
	testCredential = "installer-integration-credential"
)

func main() {
	readyFile := flag.String("ready-file", "", "path used to publish the listener URL")
	flag.Parse()
	if *readyFile == "" {
		fmt.Fprintln(os.Stderr, "-ready-file is required")
		os.Exit(2)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/agent/v1/enroll", enroll)
	mux.HandleFunc("/agent/v1/connect", connect)
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	if err := os.WriteFile(*readyFile, []byte("http://"+listener.Addr().String()+"\n"), 0o600); err != nil {
		panic(err)
	}

	stopped := make(chan os.Signal, 1)
	signal.Notify(stopped, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-stopped
		_ = server.Close()
	}()
	if err := server.Serve(listener); err != nil && err != http.ErrServerClosed {
		panic(err)
	}
}

func enroll(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var request v1.EnrollRequest
	if err := json.NewDecoder(r.Body).Decode(&request); err != nil || request.Token != testToken || request.ProtocolVersion != v1.Version {
		http.Error(w, "invalid enrollment", http.StatusUnauthorized)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(v1.EnrollResponse{NodeID: 1, NodeName: "installer-test", Credential: testCredential})
}

func connect(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer "+testCredential || r.Header.Get("X-XUI-Agent-Node-ID") != "1" {
		http.Error(w, "invalid node credential", http.StatusUnauthorized)
		return
	}
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	connection, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer connection.Close()
	now := time.Now()
	hello, _ := v1.NewEnvelope(v1.MessageHelloAck, "hello", v1.HelloAck{
		SessionID:               fmt.Sprintf("installer-%d", now.UnixNano()),
		HeartbeatIntervalSecond: 5,
		ServerTime:              now.UnixMilli(),
	}, now)
	if err := connection.WriteJSON(hello); err != nil {
		return
	}
	for {
		var heartbeat v1.Envelope
		if err := connection.ReadJSON(&heartbeat); err != nil {
			return
		}
		if heartbeat.Version != v1.Version || heartbeat.Type != v1.MessageHeartbeat {
			return
		}
		ack, _ := v1.NewEnvelope(v1.MessageHeartbeatAck, "ack", v1.HeartbeatAck{
			MessageID:  heartbeat.ID,
			ServerTime: time.Now().UnixMilli(),
		}, time.Now())
		if err := connection.WriteJSON(ack); err != nil {
			return
		}
	}
}
