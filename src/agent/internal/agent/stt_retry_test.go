package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gorilla/websocket"
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
	synctest.Test(t, func(t *testing.T) {
		var calls atomic.Int32
		httpClient := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls.Add(1)
			return &http.Response{StatusCode: 503, Header: http.Header{"Retry-After": {"3600"}}, Body: http.NoBody, Request: req}, nil
		})}
		client := NewOpenAIWhisperSTT("k", "m", "http://stt.test", "", httpClient)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		done := make(chan error, 1)
		go func() { _, err := client.TranscribeWAVWithContext(ctx, []byte("wav")); done <- err }()
		// The transport is synchronous; the only remaining blocking operation
		// is the retry timer. Wait until the client has reached it.
		synctest.Wait()
		select {
		case err := <-done:
			t.Fatalf("returned before cancellation: %v", err)
		default:
		}
		cancel()
		if err := <-done; !errors.Is(err, context.Canceled) || calls.Load() != 1 {
			t.Fatalf("err=%v calls=%d", err, calls.Load())
		}
	})
}

func TestDashScopeSTTRetriesCompleteBufferedWAV(t *testing.T) {
	var calls atomic.Int32
	upgrader := websocket.Upgrader{}
	received := make(chan []byte, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := calls.Add(1)
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_ = conn.WriteJSON(map[string]any{"type": "session.created"})
		var audio []byte
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var event struct{ Type, Audio string }
			_ = json.Unmarshal(body, &event)
			if event.Type == "input_audio_buffer.append" {
				chunk, err := base64.StdEncoding.DecodeString(event.Audio)
				if err != nil {
					t.Errorf("decode PCM: %v", err)
					return
				}
				audio = append(audio, chunk...)
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
	samples := make([]int16, 3207)
	for i := range samples {
		samples[i] = int16(i*7 + 1)
	}
	wav := pcm16MonoToWAV(samples, 16000)
	var expected bytes.Buffer
	if err := binary.Write(&expected, binary.LittleEndian, samples); err != nil {
		t.Fatal(err)
	}
	text, err := client.TranscribeWAV(wav)
	if err != nil || text != "hello" || calls.Load() != 2 {
		t.Fatalf("text=%q err=%v calls=%d", text, err, calls.Load())
	}
	first, second := <-received, <-received
	for i, audio := range [][]byte{first, second} {
		if !bytes.Equal(audio, expected.Bytes()) {
			t.Fatalf("attempt %d PCM mismatch: got %d bytes, want %d", i+1, len(audio), expected.Len())
		}
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
