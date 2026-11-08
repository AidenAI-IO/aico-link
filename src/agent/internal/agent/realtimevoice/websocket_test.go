package realtimevoice

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestJSONWebSocketTransportWriteHonorsCanceledContext(t *testing.T) {
	transport := writeStateTestTransport()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := transport.writeJSON(ctx, map[string]string{"type": "test"}); err != context.Canceled {
		t.Fatalf("writeJSON error = %v, want context.Canceled", err)
	}
}

func TestJSONWebSocketTransportWriteRejectsClosedSession(t *testing.T) {
	transport := writeStateTestTransport()
	close(transport.done)

	err := transport.writeJSON(context.Background(), map[string]string{"type": "test"})
	if err == nil || !strings.Contains(err.Error(), "session is closed") {
		t.Fatalf("writeJSON error = %v, want closed session", err)
	}
}

func writeStateTestTransport() *jsonWebSocketTransport {
	transport := &jsonWebSocketTransport{
		label:     "test realtime",
		done:      make(chan struct{}),
		writeGate: make(chan struct{}, 1),
	}
	transport.writeGate <- struct{}{}
	return transport
}

func TestJSONWebSocketTransportCancelBlockedWrite(t *testing.T) {
	connected := make(chan struct{})
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		close(connected)
		<-release
	}))
	defer server.Close()
	defer close(release)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	transport := newJSONWebSocketTransport(conn, "test", 1)
	defer transport.Close()
	<-connected
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- transport.writeJSON(ctx, strings.Repeat("x", 32<<20)) }()
	select {
	case err := <-done:
		t.Fatalf("write did not block: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("write error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not unblock websocket write")
	}
}

func TestDialErrorReportsRejectedUpgradeReason(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"user quota insufficient"}`, http.StatusPaymentRequired)
	}))
	defer server.Close()
	_, err := OpenAIProvider{Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")}.Open(
		context.Background(), SessionConfig{APIKey: "key"})
	if err == nil || !strings.Contains(err.Error(), "402 Payment Required") ||
		!strings.Contains(err.Error(), "user quota insufficient") || !errors.Is(err, websocket.ErrBadHandshake) {
		t.Fatalf("dial error = %v", err)
	}
}
