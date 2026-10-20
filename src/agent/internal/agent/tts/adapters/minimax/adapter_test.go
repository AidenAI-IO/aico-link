package minimax

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aiden-agent/internal/agent/tts"
)

func TestWebSocketRequestsSinkSampleRate(t *testing.T) {
	sampleRates := make(chan int, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{"event": "connected_success"}); err != nil {
			t.Errorf("write connected_success: %v", err)
			return
		}

		var start map[string]any
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read task_start: %v", err)
			return
		}
		audioSetting, ok := start["audio_setting"].(map[string]any)
		if !ok {
			t.Errorf("audio_setting = %#v", start["audio_setting"])
			return
		}
		sampleRates <- int(audioSetting["sample_rate"].(float64))
		if err := conn.WriteJSON(map[string]any{"event": "task_started"}); err != nil {
			t.Errorf("write task_started: %v", err)
			return
		}

		var cont map[string]any
		if err := conn.ReadJSON(&cont); err != nil {
			t.Errorf("read task_continue: %v", err)
			return
		}

		var finish map[string]any
		if err := conn.ReadJSON(&finish); err != nil {
			t.Errorf("read task_finish: %v", err)
			return
		}
		if err := conn.WriteJSON(map[string]any{"is_final": true}); err != nil {
			t.Errorf("write final audio response: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{"event": "task_finished"})
	}))
	defer server.Close()

	provider, err := NewWebSocket(tts.ProviderConfig{
		APIKey:   "test-key",
		Endpoint: "ws" + server.URL[len("http"):],
	})
	if err != nil {
		t.Fatalf("NewWebSocket() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	session, err := provider.BeginStream(ctx, noopSink{format: tts.AudioFormat{SampleRate: 32000, Channels: 1, BitWidth: 16}})
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	if err := session.WriteText("hello"); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	select {
	case got := <-sampleRates:
		if got != 32000 {
			t.Fatalf("sample_rate = %d, want sink sample rate 32000", got)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for sample_rate from websocket server")
	}
}

func TestWebSocketSendsTaskFinishBeforeWaitingForAudio(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		_ = conn.WriteJSON(map[string]any{"event": "connected_success"})
		var start map[string]any
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read task_start: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{"event": "task_started"})
		var cont map[string]any
		if err := conn.ReadJSON(&cont); err != nil {
			t.Errorf("read task_continue: %v", err)
			return
		}
		var finish map[string]any
		if err := conn.ReadJSON(&finish); err != nil {
			t.Errorf("read task_finish: %v", err)
			return
		}
		if event, _ := finish["event"].(string); event != "task_finish" {
			t.Errorf("finish event = %q, want task_finish", event)
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"data":     map[string]any{"audio": "0001"},
			"is_final": true,
		})
		_ = conn.WriteJSON(map[string]any{"event": "task_finished"})
	}))
	defer server.Close()

	provider, err := NewWebSocket(tts.ProviderConfig{APIKey: "test-key", Endpoint: "ws" + server.URL[len("http"):]})
	if err != nil {
		t.Fatalf("NewWebSocket() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sink := &recordingSink{format: tts.AudioFormat{SampleRate: 32000, Channels: 1, BitWidth: 16}}
	session, err := provider.BeginStream(ctx, sink)
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	if err := session.WriteText("test passed."); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if string(sink.data) != "\x00\x01" {
		t.Fatalf("sink data = %v, want [0 1]", sink.data)
	}
}

func TestWebSocketTreatsNormalCloseAfterAudioAsSuccess(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		_ = conn.WriteJSON(map[string]any{"event": "connected_success"})
		var start map[string]any
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read task_start: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{"event": "task_started"})
		var cont map[string]any
		if err := conn.ReadJSON(&cont); err != nil {
			t.Errorf("read task_continue: %v", err)
			return
		}
		var finish map[string]any
		if err := conn.ReadJSON(&finish); err != nil {
			t.Errorf("read task_finish: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"data":     map[string]any{"audio": "0001"},
			"is_final": true,
		})
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(time.Second),
		)
	}))
	defer server.Close()

	provider, err := NewWebSocket(tts.ProviderConfig{APIKey: "test-key", Endpoint: "ws" + server.URL[len("http"):]})
	if err != nil {
		t.Fatalf("NewWebSocket() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	sink := &recordingSink{format: tts.AudioFormat{SampleRate: 32000, Channels: 1, BitWidth: 16}}
	session, err := provider.BeginStream(ctx, sink)
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	if err := session.WriteText("test passed."); err != nil {
		t.Fatalf("WriteText() error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	if string(sink.data) != "\x00\x01" {
		t.Fatalf("sink data = %v, want [0 1]", sink.data)
	}
}

func TestWebSocketDialerUsesConfiguredProxy(t *testing.T) {
	dialer, err := websocketDialerForConfig(commonConfig{
		proxy: tts.ProxyConfig{AllProxy: "http://127.0.0.1:7890"},
	})
	if err != nil {
		t.Fatalf("websocketDialerForConfig() error = %v", err)
	}
	if dialer.Proxy == nil {
		t.Fatal("expected websocket dialer proxy to be configured")
	}

	proxyURL, err := dialer.Proxy(&http.Request{URL: mustURL(t, "https://api.minimaxi.com/ws/v1/t2a_v2")})
	if err != nil {
		t.Fatalf("Proxy() error = %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "http://127.0.0.1:7890" {
		t.Fatalf("proxy URL = %v, want http://127.0.0.1:7890", proxyURL)
	}
}

func TestWebSocketDialerFallsBackToHTTPProxy(t *testing.T) {
	dialer, err := websocketDialerForConfig(commonConfig{
		proxy: tts.ProxyConfig{HTTPProxy: "http://127.0.0.1:7890"},
	})
	if err != nil {
		t.Fatalf("websocketDialerForConfig() error = %v", err)
	}
	if dialer.Proxy == nil {
		t.Fatal("expected websocket dialer proxy to be configured")
	}

	proxyURL, err := dialer.Proxy(&http.Request{URL: mustURL(t, "wss://api.minimaxi.com/ws/v1/t2a_v2")})
	if err != nil {
		t.Fatalf("Proxy() error = %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "http://127.0.0.1:7890" {
		t.Fatalf("proxy URL = %v, want http://127.0.0.1:7890", proxyURL)
	}
}

func TestWebSocketDialerHonorsIPv6NoProxy(t *testing.T) {
	dialer, err := websocketDialerForConfig(commonConfig{
		proxy: tts.ProxyConfig{AllProxy: "http://127.0.0.1:7890", NoProxy: "::1"},
	})
	if err != nil {
		t.Fatalf("websocketDialerForConfig() error = %v", err)
	}

	proxyURL, err := dialer.Proxy(&http.Request{URL: mustURL(t, "ws://[::1]:8080/ws")})
	if err != nil {
		t.Fatalf("Proxy() error = %v", err)
	}
	if proxyURL != nil {
		t.Fatalf("proxy URL = %v, want direct connection for NO_PROXY ::1", proxyURL)
	}
}

func TestWebSocketRejectsInvalidAudioHex(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{"event": "connected_success"}); err != nil {
			t.Errorf("write connected_success: %v", err)
			return
		}
		var start map[string]any
		if err := conn.ReadJSON(&start); err != nil {
			t.Errorf("read task_start: %v", err)
			return
		}
		if err := conn.WriteJSON(map[string]any{"event": "task_started"}); err != nil {
			t.Errorf("write task_started: %v", err)
			return
		}
		var cont map[string]any
		if err := conn.ReadJSON(&cont); err != nil {
			t.Errorf("read task_continue: %v", err)
			return
		}
		var finish map[string]any
		if err := conn.ReadJSON(&finish); err != nil {
			t.Errorf("read task_finish: %v", err)
			return
		}
		_ = conn.WriteJSON(map[string]any{"data": map[string]any{"audio": "zz"}, "is_final": true})
	}))
	defer server.Close()

	provider, err := NewWebSocket(tts.ProviderConfig{APIKey: "test-key", Endpoint: "ws" + server.URL[len("http"):]})
	if err != nil {
		t.Fatalf("NewWebSocket() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	session, err := provider.BeginStream(ctx, noopSink{format: tts.AudioFormat{SampleRate: 32000, Channels: 1, BitWidth: 16}})
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	if err := session.WriteText("this sentence is long enough."); err == nil || !strings.Contains(err.Error(), "decode audio hex") {
		t.Fatalf("WriteText() error = %v, want decode audio hex", err)
	}
}

func TestWebSocketReadAudioHonorsContextCancellation(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		_ = conn.WriteJSON(map[string]any{"event": "connected_success"})
		var start map[string]any
		if err := conn.ReadJSON(&start); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"event": "task_started"})
		var cont map[string]any
		if err := conn.ReadJSON(&cont); err != nil {
			return
		}
		var finish map[string]any
		if err := conn.ReadJSON(&finish); err != nil {
			return
		}
		<-r.Context().Done()
	}))
	defer server.Close()
	defer server.CloseClientConnections()

	provider, err := NewWebSocket(tts.ProviderConfig{APIKey: "test-key", Endpoint: "ws" + server.URL[len("http"):]})
	if err != nil {
		t.Fatalf("NewWebSocket() error = %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	session, err := provider.BeginStream(ctx, noopSink{format: tts.AudioFormat{SampleRate: 32000, Channels: 1, BitWidth: 16}})
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- session.WriteText("this sentence is long enough.") }()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("WriteText() error = nil, want context cancellation")
		}
	case <-time.After(500 * time.Millisecond):
		server.CloseClientConnections()
		t.Fatal("WriteText() blocked after context cancellation")
	}
}

func mustURL(t *testing.T, raw string) *url.URL {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse URL: %v", err)
	}
	return u
}

type noopSink struct{ format tts.AudioFormat }

func (s noopSink) Format() tts.AudioFormat { return s.format }
func (s noopSink) WritePCM([]byte) error   { return nil }
func (s noopSink) Drain(context.Context) error {
	return nil
}
func (s noopSink) Stop() error { return nil }

type recordingSink struct {
	format tts.AudioFormat
	data   []byte
}

func (s *recordingSink) Format() tts.AudioFormat { return s.format }
func (s *recordingSink) WritePCM(data []byte) error {
	s.data = append(s.data, data...)
	return nil
}
func (s *recordingSink) Drain(context.Context) error { return nil }
func (s *recordingSink) Stop() error                 { return nil }
