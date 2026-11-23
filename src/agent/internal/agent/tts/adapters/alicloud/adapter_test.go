package alicloud

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"aico-agent/internal/agent/tts"
	"aico-agent/internal/providerretry"
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

func TestAbortWhileReaderReceivesAudio(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"type": "session.created"})
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		for {
			if err := conn.WriteJSON(map[string]any{"type": "response.audio.delta", "delta": "AQI="}); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	provider, err := New(tts.ProviderConfig{APIKey: "k", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")})
	if err != nil {
		t.Fatal(err)
	}
	defer provider.Close()
	for i := 0; i < 20; i++ {
		sink := &blockingPCMSink{started: make(chan struct{}), release: make(chan struct{})}
		stream, err := provider.BeginStream(context.Background(), sink)
		if err != nil {
			t.Fatal(err)
		}
		select {
		case <-sink.started:
		case <-time.After(2 * time.Second):
			_ = sink.Stop()
			_ = stream.(*session).Abort()
			t.Fatal("reader never submitted PCM")
		}
		done := make(chan error, 1)
		go func() { done <- stream.(*session).Abort() }()
		// Abort calls Stop, which releases the in-flight PCM write.
		select {
		case err := <-done:
			if err != nil {
				t.Fatalf("Abort: %v", err)
			}
		case <-time.After(2 * time.Second):
			_ = sink.Stop()
			t.Fatal("Abort blocked during PCM delivery")
		}
	}
}

// Stop releases an in-flight PCM write so Abort must overlap the reader.
type blockingPCMSink struct {
	started, release    chan struct{}
	startOnce, stopOnce sync.Once
}

func (*blockingPCMSink) Format() tts.AudioFormat {
	return tts.AudioFormat{SampleRate: defaultSampleRate, Channels: 1, BitWidth: 16}
}
func (s *blockingPCMSink) WritePCM([]byte) error {
	s.startOnce.Do(func() { close(s.started) })
	select {
	case <-s.release:
		return nil
	case <-time.After(2 * time.Second):
		return fmt.Errorf("PCM write was not stopped")
	}
}
func (*blockingPCMSink) Drain(context.Context) error { return nil }
func (s *blockingPCMSink) Stop() error               { s.stopOnce.Do(func() { close(s.release) }); return nil }

func TestReaderRejectionEndsFlushAndClose(t *testing.T) {
	for _, pending := range []bool{false, true} {
		for _, code := range []int{401, 429, 503} {
			t.Run(fmt.Sprintf("pending=%v/status=%d", pending, code), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					_ = conn.WriteJSON(map[string]any{"type": "session.created"})
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					if pending {
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
					}
					_ = conn.WriteJSON(map[string]any{"type": "error", "error": map[string]any{"code": code, "message": "rejected"}})
					for {
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
					}
				}))
				defer server.Close()
				provider, err := New(tts.ProviderConfig{APIKey: "k", Endpoint: "ws" + strings.TrimPrefix(server.URL, "http")})
				if err != nil {
					t.Fatal(err)
				}
				defer provider.Close()
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				stream, err := provider.BeginStream(ctx, &recordingSink{})
				if err != nil {
					t.Fatal(err)
				}
				raw := stream.(*session)
				defer func() { cancel(); _ = raw.Abort() }()
				if pending {
					if err := stream.Flush(); err != nil {
						var status *providerretry.StatusError
						if !errors.As(err, &status) || status.Status != code {
							t.Fatalf("initial Flush: %v", err)
						}
					}
				}
				select {
				case <-raw.readDone:
				case <-time.After(time.Second):
					t.Fatal("reader did not report rejection")
				}
				if code == 401 {
					cancel() // A known rejection must survive subsequent cancellation.
				}
				for _, op := range []func() error{stream.Flush, stream.Flush, func() error { return stream.WriteText("hello") }, stream.Close} {
					done := make(chan error, 1)
					go func() { done <- op() }()
					select {
					case err := <-done:
						var status *providerretry.StatusError
						if !errors.As(err, &status) || status.Status != code {
							t.Fatalf("error=%v, want status %d", err, code)
						}
					case <-time.After(time.Second):
						t.Fatal("operation blocked after terminal reader error")
					}
				}
			})
		}
	}
}
