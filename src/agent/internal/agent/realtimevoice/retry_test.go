package realtimevoice

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"aico-agent/internal/providerretry"
	"github.com/gorilla/websocket"
)

func TestRegistryRetriesSetupButNeverLiveMedia(t *testing.T) {
	for _, failure := range []string{"upgrade", "setup", "auth", "exhausted"} {
		t.Run(failure, func(t *testing.T) {
			var attempts, media atomic.Int32
			upgrader := websocket.Upgrader{}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := attempts.Add(1)
				if failure == "auth" {
					w.WriteHeader(401)
					return
				}
				if failure == "exhausted" || (failure == "upgrade" && n == 1) {
					w.WriteHeader(429)
					return
				}
				conn, err := upgrader.Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if failure == "setup" && n == 1 {
					_ = conn.WriteJSON(map[string]any{"type": "error", "error": map[string]any{"type": "server_error", "message": "busy"}})
					return
				}
				_ = conn.WriteJSON(map[string]any{"type": "session.created", "session": map[string]any{"id": "s"}})
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				_ = conn.WriteJSON(map[string]any{"type": "session.updated", "session": map[string]any{"id": "s"}})
				for {
					_, body, err := conn.ReadMessage()
					if err != nil {
						return
					}
					var ev struct {
						Type string `json:"type"`
					}
					_ = json.Unmarshal(body, &ev)
					if ev.Type == "input_audio_buffer.append" {
						media.Add(1)
						_ = conn.WriteJSON(map[string]any{"type": "error", "error": map[string]any{"code": "rate_limit_exceeded", "message": "busy live"}})
					}
				}
			}))
			defer server.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			format := AudioFormat{Encoding: "pcm_s16le", SampleRate: 24000, Channels: 1, BitDepth: 16}
			s, err := DefaultProviderRegistry().Open(ctx, ProviderOpenAI, ProviderConfig{Endpoint: server.URL}, SessionConfig{APIKey: "k"}, DeviceMediaConfig{Input: format, Output: format})
			if failure == "auth" || failure == "exhausted" {
				want := int32(1)
				if failure == "exhausted" {
					want = 3
				}
				if err == nil || attempts.Load() != want {
					t.Fatalf("err=%v attempts=%d", err, attempts.Load())
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if err = s.SendAudio(ctx, []byte{1, 2}); err != nil {
				t.Fatal(err)
			}
			select {
			case ev := <-s.Events():
				if ev.Kind != EventError || !providerretry.Retryable(ev.Error) {
					t.Fatalf("event=%+v", ev)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			if attempts.Load() != 2 || media.Load() != 1 {
				t.Fatalf("attempts=%d media=%d", attempts.Load(), media.Load())
			}
		})
	}
}

func TestRealtimeStructuredErrorClassification(t *testing.T) {
	body := []byte(`{"type":"error","error":{"code":"rate_limit_exceeded","message":"busy"}}`)
	openai, _ := translateOpenAIEvent(body)
	qwen, _ := translateQwenEvent(body)
	xai, _ := translateXAIEventBase(body)
	gemini := (&geminiSession{}).translate([]byte(`{"error":{"code":503,"status":"UNAVAILABLE","message":"busy"}}`))[0]
	for _, ev := range []Event{openai, qwen, xai, gemini} {
		if !providerretry.Retryable(ev.Error) {
			t.Errorf("error=%v", ev.Error)
		}
	}
	idle := providerServerError(body, "session closed because no response was generated")
	if !errors.Is(idle, ErrSessionIdleTimeout) {
		t.Fatalf("idle=%v", idle)
	}
}

func TestSpekoSetupRetriesShareMintKeyAndBudget(t *testing.T) {
	var mints, upgrades atomic.Int32
	var key string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/sessions" {
			mints.Add(1)
			got := r.Header.Get("Idempotency-Key")
			if got == "" {
				t.Error("missing mint key")
			}
			if key == "" {
				key = got
			} else if key != got {
				t.Error("mint key changed across setup attempts")
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"mode": "s2s", "transport": "provider_direct", "sessionId": "s", "provider": "google", "adapter": "google.live.v1", "providerTransport": "websocket",
				"model": "gemini-3.1-flash-live-preview", "expiresAt": "2099-01-01T00:00:00Z",
				"endpoint":        "ws://" + r.Host + "/ws/google.ai.generativelanguage.v1beta.GenerativeService.BidiGenerateContent",
				"credential":      map[string]any{"kind": "bearer", "value": "token", "expiresAt": "2099-01-01T00:00:00Z"},
				"reservation":     map[string]any{"leaseExpiresAt": "2099-01-01T00:00:00Z"},
				"inputSampleRate": 16000, "outputSampleRate": 24000,
			})
			return
		}
		upgrades.Add(1)
		w.WriteHeader(503)
	}))
	defer server.Close()
	_, err := DefaultProviderRegistry().Open(context.Background(), ProviderSpeko, ProviderConfig{BaseURL: server.URL, AgentID: "a", UpstreamProvider: "google"}, SessionConfig{APIKey: "k", Model: "gemini-3.1-flash-live-preview"}, DeviceMediaConfig{})
	if err == nil || mints.Load() != 3 || upgrades.Load() != 3 {
		t.Fatalf("mints=%d upgrades=%d err=%v", mints.Load(), upgrades.Load(), err)
	}
}

func TestMossSetupFrameErrorHonorsRetryClassification(t *testing.T) {
	for _, frame := range []string{"error", "session.updated"} {
		for _, code := range []string{"rate_limit_exceeded", "invalid_api_key"} {
			t.Run(frame+"/"+code, func(t *testing.T) {
				var attempts atomic.Int32
				upgrader := websocket.Upgrader{}
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					attempts.Add(1)
					conn, err := upgrader.Upgrade(w, r, nil)
					if err != nil {
						return
					}
					defer conn.Close()
					if _, _, err := conn.ReadMessage(); err != nil {
						return
					}
					if frame == "session.updated" {
						_ = conn.WriteJSON(map[string]any{"action": "session_attached", "data": map[string]any{"session_id": "s"}})
						if _, _, err := conn.ReadMessage(); err != nil {
							return
						}
					}
					_ = conn.WriteJSON(map[string]any{"action": frame, "data": map[string]any{"error": map[string]any{"code": code, "message": "setup rejected"}}})
					_, _, _ = conn.ReadMessage() // Keep the socket alive until the client classifies the frame.
				}))
				defer server.Close()
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				_, err := DefaultProviderRegistry().Open(ctx, ProviderMoss, ProviderConfig{Endpoint: server.URL}, SessionConfig{APIKey: "k"}, DeviceMediaConfig{})
				want := int32(1)
				if code == "rate_limit_exceeded" {
					want = 3
				}
				if err == nil || errors.Is(err, context.DeadlineExceeded) || attempts.Load() != want {
					t.Fatalf("err=%v attempts=%d", err, attempts.Load())
				}
			})
		}
	}
}
