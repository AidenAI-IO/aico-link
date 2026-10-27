package volcengine

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aico-agent/internal/agent/tts"
)

func TestVolcengineStreamsTextAndAudio(t *testing.T) {
	var sawAPIKey, sawResourceID atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Api-Key") == "test-key" {
			sawAPIKey.Store(true)
		}
		if r.Header.Get("X-Api-Resource-Id") == "seed-tts-2.0" {
			sawResourceID.Store(true)
		}
		conn, err := websocket.Upgrade(w, r, nil, 1024, 1024)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read start connection: %v", err)
			return
		}
		msg, err := parseServerFrame(raw)
		if err != nil || msg.event != eventStartConnection {
			t.Errorf("start connection = %#v err=%v", msg, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventConnectionStarted, "conn-1", "", []byte("{}")))

		_, raw, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read start session: %v", err)
			return
		}
		msg, err = parseServerFrame(raw)
		if err != nil || msg.event != eventStartSession || !strings.Contains(string(msg.payload), "test-speaker") {
			t.Errorf("start session = %#v payload=%s err=%v", msg, msg.payload, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventSessionStarted, "", msg.sessionID, []byte(`{"status_code":20000000,"message":"ok"}`)))

		_, raw, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read task request: %v", err)
			return
		}
		msg, err = parseServerFrame(raw)
		if err != nil || msg.event != eventTaskRequest || !strings.Contains(string(msg.payload), "hello") {
			t.Errorf("task request = %#v payload=%s err=%v", msg, msg.payload, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerAudioFrame(eventTTSResponse, msg.sessionID, []byte{1, 2, 3, 4}))

		_, raw, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read finish session: %v", err)
			return
		}
		msg, err = parseServerFrame(raw)
		if err != nil || msg.event != eventFinishSession {
			t.Errorf("finish session = %#v err=%v", msg, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventSessionFinished, "", msg.sessionID, []byte(`{"status_code":20000000,"message":"ok"}`)))

		_, raw, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read finish connection: %v", err)
			return
		}
		msg, err = parseServerFrame(raw)
		if err != nil || msg.event != eventFinishConnection {
			t.Errorf("finish connection = %#v err=%v", msg, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventConnectionFinished, "conn-1", "", []byte("{}")))
	}))
	defer server.Close()

	provider, err := New(tts.ProviderConfig{Provider: ProviderName, APIKey: "test-key", Voice: "test-speaker", Extra: map[string]any{"model": "seed-tts-2.0"}, Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	sink := &recordingSink{format: tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}}
	session, err := provider.BeginStream(context.Background(), sink)
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	if err := session.WriteText("hello"); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !sawAPIKey.Load() || !sawResourceID.Load() {
		t.Fatalf("headers saw api_key=%v resource_id=%v", sawAPIKey.Load(), sawResourceID.Load())
	}
	if string(sink.pcm) != string([]byte{1, 2, 3, 4}) || !sink.drained {
		t.Fatalf("sink pcm=%v drained=%v", sink.pcm, sink.drained)
	}
}

func TestVolcengineBeginStreamHonorsContextWhileWaitingForHandshake(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, nil, 1024, 1024)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()
		<-r.Context().Done()
	}))
	defer server.Close()
	defer server.CloseClientConnections()

	provider, err := New(tts.ProviderConfig{Provider: ProviderName, APIKey: "test-key", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := provider.BeginStream(ctx, &recordingSink{format: tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}})
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("BeginStream() error = nil, want context cancellation")
		}
	case <-time.After(500 * time.Millisecond):
		server.CloseClientConnections()
		t.Fatal("BeginStream() blocked after context cancellation")
	}
}

func TestVolcengineCloseReturnsWhenServerNeverFinishesSession(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, nil, 1024, 1024)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		_, raw, err := conn.ReadMessage()
		if err != nil {
			return
		}
		msg, err := parseServerFrame(raw)
		if err != nil || msg.event != eventStartConnection {
			t.Errorf("start connection = %#v err=%v", msg, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventConnectionStarted, "conn-1", "", []byte("{}")))

		_, raw, err = conn.ReadMessage()
		if err != nil {
			return
		}
		msg, err = parseServerFrame(raw)
		if err != nil || msg.event != eventStartSession {
			t.Errorf("start session = %#v err=%v", msg, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventSessionStarted, "", msg.sessionID, []byte(`{"status_code":20000000,"message":"ok"}`)))

		_, raw, err = conn.ReadMessage()
		if err != nil {
			return
		}
		msg, err = parseServerFrame(raw)
		if err != nil || msg.event != eventFinishSession {
			t.Errorf("finish session = %#v err=%v", msg, err)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	defer server.CloseClientConnections()

	provider, err := New(tts.ProviderConfig{Provider: ProviderName, APIKey: "test-key", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	session, err := provider.BeginStream(ctx, &recordingSink{format: tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}})
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- session.Close() }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		server.CloseClientConnections()
		t.Fatal("Close() blocked waiting for session_finished")
	}
}

func TestVolcengineCloseDoesNotUseConnectTimeoutForPlaybackDrain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, nil, 1024, 1024)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read start connection: %v", err)
			return
		}
		msg, err := parseServerFrame(raw)
		if err != nil || msg.event != eventStartConnection {
			t.Errorf("start connection = %#v err=%v", msg, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventConnectionStarted, "conn-1", "", []byte("{}")))

		_, raw, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read start session: %v", err)
			return
		}
		msg, err = parseServerFrame(raw)
		if err != nil || msg.event != eventStartSession {
			t.Errorf("start session = %#v err=%v", msg, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventSessionStarted, "", msg.sessionID, []byte(`{"status_code":20000000,"message":"ok"}`)))

		_, raw, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read finish session: %v", err)
			return
		}
		msg, err = parseServerFrame(raw)
		if err != nil || msg.event != eventFinishSession {
			t.Errorf("finish session = %#v err=%v", msg, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventSessionFinished, "", msg.sessionID, []byte(`{"status_code":20000000,"message":"ok"}`)))

		_, raw, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read finish connection: %v", err)
			return
		}
		msg, err = parseServerFrame(raw)
		if err != nil || msg.event != eventFinishConnection {
			t.Errorf("finish connection = %#v err=%v", msg, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventConnectionFinished, "conn-1", "", []byte("{}")))
	}))
	defer server.Close()

	provider, err := New(tts.ProviderConfig{Provider: ProviderName, APIKey: "test-key", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	sink := &deadlineCheckingSink{format: tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}}
	session, err := provider.BeginStream(context.Background(), sink)
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !sink.drained {
		t.Fatal("sink was not drained")
	}
}

func TestVolcengineSynthesisTimeoutExceedsConnectTimeout(t *testing.T) {
	if synthesisTimeout <= connectTimeout {
		t.Fatalf("synthesisTimeout = %s, want greater than connectTimeout %s", synthesisTimeout, connectTimeout)
	}
}

func TestVolcengineCloseIgnoresConnectionFinishedTimeoutAfterDrain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, nil, 1024, 1024)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read start connection: %v", err)
			return
		}
		msg, err := parseServerFrame(raw)
		if err != nil || msg.event != eventStartConnection {
			t.Errorf("start connection = %#v err=%v", msg, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventConnectionStarted, "conn-1", "", []byte("{}")))

		_, raw, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read start session: %v", err)
			return
		}
		msg, err = parseServerFrame(raw)
		if err != nil || msg.event != eventStartSession {
			t.Errorf("start session = %#v err=%v", msg, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventSessionStarted, "", msg.sessionID, []byte(`{"status_code":20000000,"message":"ok"}`)))

		_, raw, err = conn.ReadMessage()
		if err != nil {
			t.Errorf("read finish session: %v", err)
			return
		}
		msg, err = parseServerFrame(raw)
		if err != nil || msg.event != eventFinishSession {
			t.Errorf("finish session = %#v err=%v", msg, err)
			return
		}
		_ = conn.WriteMessage(websocket.BinaryMessage, encodeServerJSONFrame(eventSessionFinished, "", msg.sessionID, []byte(`{"status_code":20000000,"message":"ok"}`)))

		_, raw, err = conn.ReadMessage()
		if err != nil {
			return
		}
		msg, err = parseServerFrame(raw)
		if err != nil || msg.event != eventFinishConnection {
			t.Errorf("finish connection = %#v err=%v", msg, err)
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	defer server.CloseClientConnections()

	provider, err := New(tts.ProviderConfig{Provider: ProviderName, APIKey: "test-key", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	sink := &recordingSink{format: tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}}
	session, err := provider.BeginStream(ctx, sink)
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if !sink.drained {
		t.Fatal("sink was not drained")
	}
}

type recordingSink struct {
	format  tts.AudioFormat
	pcm     []byte
	drained bool
}

func (s *recordingSink) Format() tts.AudioFormat { return s.format }

func (s *recordingSink) WritePCM(data []byte) error {
	s.pcm = append(s.pcm, data...)
	return nil
}

func (s *recordingSink) Drain(context.Context) error {
	s.drained = true
	return nil
}

func (s *recordingSink) Stop() error { return nil }

type deadlineCheckingSink struct {
	format  tts.AudioFormat
	drained bool
}

func (s *deadlineCheckingSink) Format() tts.AudioFormat { return s.format }

func (s *deadlineCheckingSink) WritePCM([]byte) error { return nil }

func (s *deadlineCheckingSink) Drain(ctx context.Context) error {
	s.drained = true
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 20*time.Second {
		return fmt.Errorf("drain deadline too short: %s", time.Until(deadline))
	}
	return nil
}

func (s *deadlineCheckingSink) Stop() error { return nil }
