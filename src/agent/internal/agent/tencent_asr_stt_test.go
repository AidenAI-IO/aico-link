package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"aico-agent/internal/stream"
	"github.com/gorilla/websocket"
)

func TestTencentASRSTTTranscribeWAV(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("method = %s, want POST", r.Method)
		}
		if got := r.Header.Get("X-TC-Action"); got != tencentASRAction {
			t.Fatalf("X-TC-Action = %q, want %q", got, tencentASRAction)
		}
		if got := r.Header.Get("Authorization"); !strings.HasPrefix(got, tencentASRAlgorithm) {
			t.Fatalf("Authorization = %q, want TC3 prefix", got)
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll() error = %v", err)
		}
		var req tencentASRRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatalf("Unmarshal() error = %v", err)
		}
		if req.EngSerViceType != "16k_zh" || req.VoiceFormat != "wav" || req.Data == "" {
			t.Fatalf("unexpected request: %+v", req)
		}

		_ = json.NewEncoder(w).Encode(map[string]any{
			"Response": map[string]string{"Result": "hello"},
		})
	}))
	defer server.Close()

	client := NewTencentASRSTT("test-id", "test-key", "", "ap-shanghai", "16k_zh", "", server.Client())
	client.apiURL = server.URL

	text, err := client.TranscribeWAV([]byte("RIFFxxxxWAVEfmt "))
	if err != nil {
		t.Fatalf("TranscribeWAV() error = %v", err)
	}
	if text != "hello" {
		t.Fatalf("text = %q, want hello", text)
	}
}

func TestNewSTTClientTencentASRProviderNames(t *testing.T) {
	for _, provider := range []string{"tencent-asr", "tencent_asr", "tencent"} {
		t.Run(provider, func(t *testing.T) {
			cfg := Config{
				STT: STTConfig{
					Provider:        provider,
					SecretID:        "id",
					SecretKey:       "key",
					Region:          "ap-shanghai",
					EngineModelType: "16k_zh",
				},
			}
			client, err := NewSTTClientFromConfig(cfg)
			if err != nil {
				t.Fatalf("NewSTTClientFromConfig() error = %v", err)
			}
			if _, ok := client.(*TencentASRSTT); !ok {
				t.Fatalf("client type = %T, want *TencentASRSTT", client)
			}
		})
	}
}

func TestBuildTencentTC3AuthorizationDeterministic(t *testing.T) {
	auth, err := buildTencentTC3Authorization("AKIDTEST", "SECRET", `{"EngSerViceType":"16k_zh"}`, 1700000000)
	if err != nil {
		t.Fatalf("buildTencentTC3Authorization() error = %v", err)
	}
	if !strings.Contains(auth, "Credential=AKIDTEST/2023-11-14/asr/tc3_request") {
		t.Fatalf("authorization missing credential scope: %q", auth)
	}
	if !strings.Contains(auth, "Signature=") {
		t.Fatalf("authorization missing signature: %q", auth)
	}
}

func TestTencentASRSTTRequiresCredentials(t *testing.T) {
	client := NewTencentASRSTT("", "", "", "", "", "")
	_, err := client.TranscribeWAV([]byte("wav"))
	if err == nil || !strings.Contains(err.Error(), "secret_id") {
		t.Fatalf("TranscribeWAV() error = %v, want missing credentials", err)
	}
}

func TestTencentASRSTTStreamingCapabilitiesRequireAppID(t *testing.T) {
	client := NewTencentASRSTT("id", "key", "", "", "", "")
	if client.Capabilities().SupportsStreamingUpload {
		t.Fatal("expected realtime upload to stay disabled without app_id")
	}

	client = NewTencentASRSTT("id", "key", "app-1", "ap-shanghai", "16k_zh", "")
	if !client.Capabilities().SupportsStreamingUpload {
		t.Fatal("expected realtime upload to be enabled when app_id is configured")
	}
}

func TestTencentASRSTTStreamingUploaderFinalizesTranscript(t *testing.T) {
	upgrader := websocket.Upgrader{}
	var mu sync.Mutex
	var gotBinary []byte
	var gotEnd string
	var gotPath string
	var gotQuery url.Values

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		gotPath = r.URL.Path
		gotQuery = r.URL.Query()
		mu.Unlock()
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("Upgrade() error = %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{
			"code":     0,
			"message":  "success",
			"voice_id": "voice-1",
		}); err != nil {
			t.Fatalf("WriteJSON(handshake) error = %v", err)
		}

		msgType, payload, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage(binary) error = %v", err)
		}
		if msgType != websocket.BinaryMessage {
			t.Fatalf("message type = %d, want binary", msgType)
		}
		mu.Lock()
		gotBinary = append([]byte(nil), payload...)
		mu.Unlock()

		msgType, payload, err = conn.ReadMessage()
		if err != nil {
			t.Fatalf("ReadMessage(end) error = %v", err)
		}
		if msgType != websocket.TextMessage {
			t.Fatalf("message type = %d, want text", msgType)
		}
		mu.Lock()
		gotEnd = string(payload)
		mu.Unlock()

		if err := conn.WriteJSON(map[string]any{
			"code":     0,
			"message":  "success",
			"voice_id": "voice-1",
			"result": map[string]any{
				"slice_type":     1,
				"index":          0,
				"voice_text_str": "h",
			},
		}); err != nil {
			t.Fatalf("WriteJSON(partial) error = %v", err)
		}
		if err := conn.WriteJSON(map[string]any{
			"code":     0,
			"message":  "success",
			"voice_id": "voice-1",
			"result": map[string]any{
				"slice_type":     2,
				"index":          0,
				"voice_text_str": "hello",
			},
		}); err != nil {
			t.Fatalf("WriteJSON(stable) error = %v", err)
		}
		if err := conn.WriteJSON(map[string]any{
			"code":     0,
			"message":  "success",
			"voice_id": "voice-1",
			"final":    1,
		}); err != nil {
			t.Fatalf("WriteJSON(final) error = %v", err)
		}
	}))
	defer server.Close()

	client := NewTencentASRSTT("test-id", "test-key", "app-1", "ap-shanghai", "16k_zh", "")
	client.realtimeURL = "ws" + strings.TrimPrefix(server.URL, "http")

	uploader, err := client.NewStreamingUploader(context.Background(), STTStreamConfig{
		SampleRate: 16000,
		Channels:   1,
		BitWidth:   16,
	})
	if err != nil {
		t.Fatalf("NewStreamingUploader() error = %v", err)
	}
	if err := uploader.UploadPCM(append(make([]byte, 100), make([]byte, 6300)...)); err != nil {
		t.Fatalf("UploadPCM() error = %v", err)
	}
	transcript, err := uploader.Finalize()
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}

	if transcript != "hello" {
		t.Fatalf("transcript = %q, want hello", transcript)
	}
	mu.Lock()
	gotPathCopy := gotPath
	gotQueryCopy := gotQuery
	gotBinaryCopy := append([]byte(nil), gotBinary...)
	gotEndCopy := gotEnd
	mu.Unlock()
	if gotPathCopy != "/asr/v2/app-1" {
		t.Fatalf("path = %q, want /asr/v2/app-1", gotPathCopy)
	}
	if gotQueryCopy.Get("engine_model_type") != "16k_zh" {
		t.Fatalf("engine_model_type = %q, want 16k_zh", gotQueryCopy.Get("engine_model_type"))
	}
	if gotQueryCopy.Get("voice_format") != "1" {
		t.Fatalf("voice_format = %q, want 1", gotQueryCopy.Get("voice_format"))
	}
	if gotQueryCopy.Get("signature") == "" {
		t.Fatal("expected signature query parameter")
	}
	if len(gotBinaryCopy) != 6400 {
		t.Fatalf("binary payload len = %d, want 6400", len(gotBinaryCopy))
	}
	if gotEndCopy != `{"type":"end"}` {
		t.Fatalf("end payload = %q, want end message", gotEndCopy)
	}

	eventSource, ok := uploader.(STTTranscriptEventSource)
	if !ok {
		t.Fatal("streaming uploader does not expose transcript events")
	}
	var events []stream.TranscriptEvent
	for event := range eventSource.TranscriptEvents() {
		events = append(events, event)
	}
	if len(events) != 3 {
		t.Fatalf("transcript events = %#v, want hypothesis, final, commit", events)
	}
	if events[0].Kind != stream.TranscriptHypothesis || events[0].Segment.SegmentID != "tencent-0" || events[0].Segment.Revision != 1 || events[0].Segment.Text != "h" {
		t.Fatalf("hypothesis event = %#v", events[0])
	}
	if events[1].Kind != stream.TranscriptSegmentFinal || events[1].Segment.Revision != 2 || events[1].Segment.Text != "hello" {
		t.Fatalf("segment final event = %#v", events[1])
	}
	if events[2].Kind != stream.TranscriptTurnCommit || events[2].Text != "hello" || len(events[2].CommitSegments) != 1 {
		t.Fatalf("turn commit event = %#v", events[2])
	}
}

func TestTencentASRSTTStreamingUploaderFinalizeTimesOut(t *testing.T) {
	oldTimeout := tencentRealtimeASRConnTimeout
	tencentRealtimeASRConnTimeout = 50 * time.Millisecond
	t.Cleanup(func() {
		tencentRealtimeASRConnTimeout = oldTimeout
	})

	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Fatalf("Upgrade() error = %v", err)
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{
			"code":     0,
			"message":  "success",
			"voice_id": "voice-1",
		}); err != nil {
			t.Fatalf("WriteJSON(handshake) error = %v", err)
		}

		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}

		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	client := NewTencentASRSTT("test-id", "test-key", "app-1", "ap-shanghai", "16k_zh", "")
	client.realtimeURL = "ws" + strings.TrimPrefix(server.URL, "http")

	uploader, err := client.NewStreamingUploader(context.Background(), STTStreamConfig{
		SampleRate: 16000,
		Channels:   1,
		BitWidth:   16,
	})
	if err != nil {
		t.Fatalf("NewStreamingUploader() error = %v", err)
	}
	if err := uploader.UploadPCM([]byte{1, 2, 3, 4}); err != nil {
		t.Fatalf("UploadPCM() error = %v", err)
	}

	start := time.Now()
	_, err = uploader.Finalize()
	if err == nil {
		t.Fatal("expected finalize timeout error")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("Finalize() blocked too long: %s", time.Since(start))
	}
}
