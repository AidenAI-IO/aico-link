package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
	received := make(chan string, 2)
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
		received <- audio
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
	first, second := <-received, <-received
	if first == "" || first != second {
		t.Fatalf("audio replay=%q, %q", first, second)
	}
}

func TestTencentASRRetriesServiceUnavailable(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) == 1 {
			_, _ = io.WriteString(w, `{"Response":{"Error":{"Code":"ServiceUnavailable","Message":"busy"}}}`)
		} else {
			_, _ = io.WriteString(w, `{"Response":{"Result":"hello"}}`)
		}
	}))
	defer server.Close()
	client := NewTencentASRSTT("id", "key", "", "", "", "", server.Client())
	client.apiURL = server.URL
	text, err := client.TranscribeWAV([]byte("wav"))
	if err != nil || text != "hello" || calls.Load() != 2 {
		t.Fatalf("text=%q calls=%d error=%v", text, calls.Load(), err)
	}
}

func TestTencentASRRetriesOnlyTransientInitialServerErrors(t *testing.T) {
	for _, code := range []int{5000, 5001, 5002, 4001, 4008} {
		t.Run(fmt.Sprint(code), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				n := calls.Add(1)
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				reply := code
				if n > 1 {
					reply = 0
				}
				_ = conn.WriteJSON(map[string]any{"code": reply, "message": "setup"})
				_, _, _ = conn.ReadMessage()
			}))
			defer server.Close()
			client := NewTencentASRSTT("id", "key", "app", "", "", "")
			client.realtimeURL = "ws" + strings.TrimPrefix(server.URL, "http")
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			uploader, err := client.NewStreamingUploader(ctx, STTStreamConfig{SampleRate: 16000, Channels: 1, BitWidth: 16})
			if code >= 5000 {
				if err != nil || calls.Load() != 2 {
					t.Fatalf("calls=%d error=%v", calls.Load(), err)
				}
				_ = uploader.Close()
			} else if err == nil || calls.Load() != 1 {
				t.Fatalf("calls=%d error=%v", calls.Load(), err)
			}
		})
	}
}
