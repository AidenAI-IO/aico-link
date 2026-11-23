package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"github.com/gorilla/websocket"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestSTTRetriesBufferedAudio(t *testing.T) {
	wav := pcm16MonoToWAV([]int16{1, 2, 3, 4}, 16000)
	pcm, _, _ := extractPCMFromWAV(wav)
	for _, provider := range []string{"whisper", "openrouter", "google-cloud", "tencent"} {
		t.Run(provider, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var audio []byte
				if provider == "whisper" {
					if err := r.ParseMultipartForm(1 << 20); err != nil {
						t.Error(err)
						return
					}
					defer r.MultipartForm.RemoveAll()
					file, _, err := r.FormFile("file")
					if err != nil {
						t.Error(err)
						return
					}
					defer file.Close()
					audio, _ = io.ReadAll(file)
				} else {
					var body map[string]json.RawMessage
					_ = json.NewDecoder(r.Body).Decode(&body)
					var encoded string
					switch provider {
					case "openrouter":
						var a struct {
							Data string `json:"data"`
						}
						_ = json.Unmarshal(body["input_audio"], &a)
						encoded = a.Data
					case "google-cloud":
						var a struct {
							Content string `json:"content"`
						}
						_ = json.Unmarshal(body["audio"], &a)
						encoded = a.Content
					case "tencent":
						_ = json.Unmarshal(body["Data"], &encoded)
					}
					audio, _ = base64.StdEncoding.DecodeString(encoded)
				}
				want := wav
				if provider == "google-cloud" {
					want = pcm
				}
				if string(audio) != string(want) {
					t.Errorf("audio changed: %x", audio)
				}
				if calls.Add(1) == 1 {
					w.WriteHeader(429)
					return
				}
				switch provider {
				case "google-cloud":
					_, _ = io.WriteString(w, `{"results":[{"alternatives":[{"transcript":"hello"}]}]}`)
				case "tencent":
					_, _ = io.WriteString(w, `{"Response":{"Result":"hello"}}`)
				default:
					_, _ = io.WriteString(w, `{"text":"hello"}`)
				}
			}))
			defer server.Close()
			var client STTClient
			switch provider {
			case "whisper":
				client = NewOpenAIWhisperSTT("k", "m", server.URL, "", server.Client())
			case "openrouter":
				client = NewOpenRouterSTT("k", "m", server.URL, "", server.Client())
			case "google-cloud":
				client, _ = NewGoogleCloudSTT("k", server.URL, "en", "", server.Client())
			case "tencent":
				c := NewTencentASRSTT("id", "key", "", "", "", "", server.Client())
				c.apiURL = server.URL
				client = c
			}
			text, err := TranscribeWAVWithContext(context.Background(), client, wav)
			if err != nil || text != "hello" || calls.Load() != 2 {
				t.Fatalf("text=%q err=%v calls=%d", text, err, calls.Load())
			}
		})
	}
}

func TestSTTCancelStopsRetryAfter(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(503)
	}))
	defer server.Close()
	client := NewOpenAIWhisperSTT("k", "m", server.URL, "", server.Client())
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := client.TranscribeWAVWithContext(ctx, []byte("wav"))
	if !errors.Is(err, context.DeadlineExceeded) || calls.Load() != 1 {
		t.Fatalf("err=%v calls=%d", err, calls.Load())
	}
}

func TestDashScopeSTTRetriesCompleteBufferedWAV(t *testing.T) {
	var calls atomic.Int32
	upgrader := websocket.Upgrader{}
	var received []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"type": "session.created"})
		var audio string
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var event struct{ Type, Audio string }
			_ = json.Unmarshal(body, &event)
			if event.Type == "input_audio_buffer.append" {
				audio += event.Audio
			}
			if event.Type == "input_audio_buffer.commit" {
				break
			}
		}
		received = append(received, audio)
		if n == 1 {
			_ = conn.WriteJSON(map[string]any{"type": "error", "error": map[string]any{"code": "rate_limit_exceeded", "message": "busy"}})
			return
		}
		_ = conn.WriteJSON(map[string]any{"type": "conversation.item.input_audio_transcription.completed", "item_id": "i", "transcript": "hello"})
	}))
	defer server.Close()
	client := NewDashScopeRealtimeSTT("k", "m", "ws"+strings.TrimPrefix(server.URL, "http"), "en")
	text, err := client.TranscribeWAV(pcm16MonoToWAV([]int16{1, 2, 3}, 16000))
	if err != nil || text != "hello" || calls.Load() != 2 {
		t.Fatalf("text=%q err=%v calls=%d", text, err, calls.Load())
	}
	if len(received) != 2 || received[0] == "" || received[0] != received[1] {
		t.Fatalf("audio replay=%v", received)
	}
}
