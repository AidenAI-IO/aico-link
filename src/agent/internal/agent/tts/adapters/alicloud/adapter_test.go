package alicloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gorilla/websocket"

	"aico-agent/internal/agent/tts"
)

func TestSessionSupportsMultipleFlushResponses(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if err := conn.WriteJSON(map[string]any{"type": "session.created"}); err != nil {
			return
		}

		var pending strings.Builder
		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var event map[string]any
			if err := json.Unmarshal(raw, &event); err != nil {
				return
			}
			switch event["type"] {
			case "session.update":
				_ = conn.WriteJSON(map[string]any{"type": "session.updated"})
			case "input_text_buffer.append":
				text, _ := event["text"].(string)
				pending.WriteString(text)
			case "input_text_buffer.commit":
				text := pending.String()
				pending.Reset()
				_ = conn.WriteJSON(map[string]any{"type": "input_text_buffer.committed"})
				_ = conn.WriteJSON(map[string]any{"type": "response.created"})
				_ = conn.WriteJSON(map[string]any{
					"type":  "response.audio.delta",
					"delta": base64.StdEncoding.EncodeToString([]byte(text)),
				})
				_ = conn.WriteJSON(map[string]any{"type": "response.audio.done"})
				_ = conn.WriteJSON(map[string]any{"type": "response.done"})
			case "session.finish":
				_ = conn.WriteJSON(map[string]any{"type": "session.finished"})
				return
			}
		}
	}))
	defer server.Close()

	provider, err := New(tts.ProviderConfig{
		APIKey:   "test-key",
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	sink := &recordingSink{format: tts.AudioFormat{SampleRate: defaultSampleRate, Channels: 1, BitWidth: 16}}
	session, err := provider.BeginStream(context.Background(), sink)
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}

	if err := session.WriteText("tool progress"); err != nil {
		t.Fatalf("WriteText(tool) error = %v", err)
	}
	if err := session.Flush(); err != nil {
		t.Fatalf("Flush(tool) error = %v", err)
	}
	if err := session.WriteText("final answer"); err != nil {
		t.Fatalf("WriteText(final) error = %v", err)
	}
	if err := session.Flush(); err != nil {
		t.Fatalf("Flush(final) error = %v", err)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if got := sink.String(); got != "tool progressfinal answer" {
		t.Fatalf("audio payload = %q", got)
	}
}

type recordingSink struct {
	mu     sync.Mutex
	format tts.AudioFormat
	data   []byte
}

func (s *recordingSink) Format() tts.AudioFormat { return s.format }

func (s *recordingSink) WritePCM(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = append(s.data, data...)
	return nil
}

func (s *recordingSink) Drain(context.Context) error { return nil }

func (s *recordingSink) Stop() error { return nil }

func (s *recordingSink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return string(s.data)
}
