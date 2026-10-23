package fishaudio

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/vmihailenco/msgpack/v5"

	"aico-agent/internal/agent/tts"
)

func TestFishAudioUsesDefaultReferenceIDWhenEmpty(t *testing.T) {
	startMsg := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("model") != "s2-pro" {
			t.Errorf("model header = %q, want s2-pro", r.Header.Get("model"))
		}
		conn, err := websocket.Upgrade(w, r, nil, 1024, 1024)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		_, raw, err := conn.ReadMessage()
		if err != nil {
			t.Errorf("read start: %v", err)
			return
		}
		var msg map[string]any
		if err := msgpack.Unmarshal(raw, &msg); err != nil {
			t.Errorf("unmarshal start: %v", err)
			return
		}
		startMsg <- msg
		finish, _ := msgpack.Marshal(map[string]any{"event": "finish"})
		_ = conn.WriteMessage(websocket.BinaryMessage, finish)
	}))
	defer server.Close()

	provider, err := New(tts.ProviderConfig{
		APIKey:   "test-key",
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		Extra:    map[string]any{"reference_id": ""},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer provider.Close()

	session, err := provider.BeginStream(context.Background(), noopSink{format: tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}})
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	defer session.Close()

	msg := <-startMsg
	request, ok := msg["request"].(map[string]any)
	if !ok {
		t.Fatalf("request = %#v, want map", msg["request"])
	}
	if refID, ok := request["reference_id"].(string); !ok || refID != tts.DefaultFishAudioReferenceID {
		t.Fatalf("reference_id = %#v, want default %q", request["reference_id"], tts.DefaultFishAudioReferenceID)
	}
	if request["text"] != "" {
		t.Fatalf("text = %#v, want empty string", request["text"])
	}
	if request["latency"] != "balanced" {
		t.Fatalf("latency = %#v, want balanced", request["latency"])
	}
	if _, ok := request["channels"]; ok {
		t.Fatalf("channels should not be sent in Fish Audio TTSRequest: %#v", request)
	}
	if _, ok := request["speed"]; ok {
		t.Fatalf("top-level speed should not be sent in Fish Audio TTSRequest: %#v", request)
	}
}

func TestFishAudioCloseReturnsWhenServerNeverFinishes(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, nil, 1024, 1024)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	defer server.CloseClientConnections()

	provider, err := New(tts.ProviderConfig{
		APIKey:   "test-key",
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		Extra:    map[string]any{"reference_id": "test-ref-id"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer provider.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	session, err := provider.BeginStream(ctx, noopSink{format: tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}})
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}

	done := make(chan error, 1)
	go func() { done <- session.Close() }()
	select {
	case <-done:
	case <-time.After(500 * time.Millisecond):
		server.CloseClientConnections()
		t.Fatal("Close() blocked waiting for server finish")
	}
}

func TestFishAudioCloseDoesNotUseConnectTimeoutForPlaybackDrain(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := websocket.Upgrade(w, r, nil, 1024, 1024)
		if err != nil {
			t.Errorf("upgrade: %v", err)
			return
		}
		defer conn.Close()

		for {
			_, raw, err := conn.ReadMessage()
			if err != nil {
				t.Errorf("read message: %v", err)
				return
			}
			var msg map[string]any
			if err := msgpack.Unmarshal(raw, &msg); err != nil {
				t.Errorf("unmarshal: %v", err)
				return
			}
			if event, _ := msg["event"].(string); event == "stop" {
				finish, _ := msgpack.Marshal(map[string]any{"event": "finish"})
				_ = conn.WriteMessage(websocket.BinaryMessage, finish)
				return
			}
		}
	}))
	defer server.Close()

	provider, err := New(tts.ProviderConfig{
		APIKey:   "test-key",
		Endpoint: "ws" + strings.TrimPrefix(server.URL, "http"),
		Extra:    map[string]any{"reference_id": "test-ref-id"},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer provider.Close()

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

func TestFishAudioSynthesisTimeoutExceedsConnectTimeout(t *testing.T) {
	if synthesisTimeout <= connectTimeout {
		t.Fatalf("synthesisTimeout = %s, want greater than connectTimeout %s", synthesisTimeout, connectTimeout)
	}
}

// TestFishAudioReferenceIDResolution verifies that an empty reference_id uses
// the built-in demo voice while voice_id remains ignored. This prevents
// cross-provider parameter pollution without making Config Web placeholders
// lie about the effective runtime value.
func TestFishAudioReferenceIDResolution(t *testing.T) {
	tests := []struct {
		name          string
		cfg           tts.ProviderConfig
		wantReference string
	}{
		{
			name: "missing reference_id",
			cfg: tts.ProviderConfig{
				APIKey: "test-key",
			},
			wantReference: tts.DefaultFishAudioReferenceID,
		},
		{
			name: "voice_id without reference_id still uses default",
			cfg: tts.ProviderConfig{
				APIKey: "test-key",
				Voice:  "some-voice-id", // This should NOT be used
			},
			wantReference: tts.DefaultFishAudioReferenceID,
		},
		{
			name: "reference_id in Extra should work",
			cfg: tts.ProviderConfig{
				APIKey: "test-key",
				Extra:  map[string]any{"reference_id": "valid-ref-id"},
			},
			wantReference: "valid-ref-id",
		},
		{
			name: "reference_id takes precedence over Voice",
			cfg: tts.ProviderConfig{
				APIKey: "test-key",
				Voice:  "ignored-voice-id",
				Extra:  map[string]any{"reference_id": "valid-ref-id"},
			},
			wantReference: "valid-ref-id",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			provider, err := New(tt.cfg)
			if err != nil {
				t.Fatalf("New() unexpected error = %v", err)
			}
			adapter := provider.(*Adapter)
			if adapter.referenceID != tt.wantReference {
				t.Fatalf("referenceID = %q, want %q", adapter.referenceID, tt.wantReference)
			}
		})
	}
}

type noopSink struct{ format tts.AudioFormat }

func (s noopSink) Format() tts.AudioFormat { return s.format }
func (noopSink) WritePCM([]byte) error     { return nil }
func (noopSink) Drain(context.Context) error {
	return nil
}
func (noopSink) Stop() error { return nil }

type deadlineCheckingSink struct {
	format  tts.AudioFormat
	drained bool
}

func (s *deadlineCheckingSink) Format() tts.AudioFormat { return s.format }
func (s *deadlineCheckingSink) WritePCM([]byte) error   { return nil }
func (s *deadlineCheckingSink) Drain(ctx context.Context) error {
	s.drained = true
	if deadline, ok := ctx.Deadline(); ok && time.Until(deadline) < 20*time.Second {
		return fmt.Errorf("drain deadline too short: %s", time.Until(deadline))
	}
	return nil
}
func (s *deadlineCheckingSink) Stop() error { return nil }
