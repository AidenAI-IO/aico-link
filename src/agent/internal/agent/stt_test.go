package agent

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestOpenAIWhisperSTTWithLanguage(t *testing.T) {
	wavData := []byte("RIFF wav data")
	errCh := make(chan error, 1)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(format string, args ...any) {
			errCh <- fmt.Errorf(format, args...)
			http.Error(w, "bad request", http.StatusBadRequest)
		}

		if r.Method != http.MethodPost {
			fail("method = %s, want POST", r.Method)
			return
		}
		if r.URL.Path != "/audio/transcriptions" {
			fail("path = %s, want /audio/transcriptions", r.URL.Path)
			return
		}

		if err := r.ParseMultipartForm(10 << 20); err != nil {
			fail("parse multipart form: %v", err)
			return
		}

		if model := r.FormValue("model"); model != "whisper-1" {
			fail("model = %q, want whisper-1", model)
			return
		}
		if language := r.FormValue("language"); language != "zh" {
			fail("language = %q, want zh", language)
			return
		}

		errCh <- nil
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"text":"hello world"}`))
	}))
	defer server.Close()

	client := NewOpenAIWhisperSTT("sk-test", "whisper-1", server.URL, "zh", server.Client())
	text, err := client.TranscribeWAV(wavData)
	select {
	case handlerErr := <-errCh:
		if handlerErr != nil {
			t.Fatal(handlerErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for OpenAI Whisper STT test handler")
	}
	if err != nil {
		t.Fatalf("TranscribeWAV() error = %v", err)
	}
	if text != "hello world" {
		t.Fatalf("text = %q, want hello world", text)
	}
}

func TestTencentASRLanguageMapping(t *testing.T) {
	tests := []struct {
		name            string
		language        string
		engineModelType string
		wantEngine      string
	}{
		{"zh maps to 16k_zh", "zh", "", "16k_zh"},
		{"en maps to 16k_en", "en", "", "16k_en"},
		{"language overrides explicit engine", "zh", "8k_en", "16k_zh"},
		{"unknown language uses default", "ja", "", "16k_zh"},
		{"empty language uses explicit engine", "", "8k_en", "8k_en"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := NewTencentASRSTT("id", "key", "", "ap-shanghai", tt.engineModelType, tt.language)
			if client.engineModelType != tt.wantEngine {
				t.Fatalf("engineModelType = %q, want %q", client.engineModelType, tt.wantEngine)
			}
		})
	}
}

type contextAwareSTTTestClient struct {
	inputs       [][]byte
	legacyCalls  int
	contextCalls int
}

func (c *contextAwareSTTTestClient) Capabilities() STTCapabilities { return STTCapabilities{} }
func (c *contextAwareSTTTestClient) NewStreamingUploader(context.Context, STTStreamConfig) (STTStreamUploader, error) {
	return nil, fmt.Errorf("streaming unsupported")
}
func (c *contextAwareSTTTestClient) TranscribeWAV(wavData []byte) (string, error) {
	c.legacyCalls++
	return "legacy", nil
}
func (c *contextAwareSTTTestClient) TranscribeWAVWithContext(ctx context.Context, wavData []byte) (string, error) {
	c.contextCalls++
	c.inputs = append(c.inputs, append([]byte(nil), wavData...))
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return string(wavData), nil
}

func TestTranscribeWAVWithContextPreservesLegacyClient(t *testing.T) {
	client := &stubSTTClient{}
	got, err := TranscribeWAVWithContext(context.Background(), client, []byte("wav"))
	if err != nil {
		t.Fatalf("TranscribeWAVWithContext() error = %v", err)
	}
	if got != "" {
		t.Fatalf("transcript = %q, want stub result", got)
	}
	if len(client.inputs) != 1 {
		t.Fatalf("legacy TranscribeWAV calls = %d, want 1", len(client.inputs))
	}
}

func TestTranscribeBatchUsesContextAwareProviderInOrder(t *testing.T) {
	client := &contextAwareSTTTestClient{}
	got, err := TranscribeBatch(context.Background(), client, [][]byte{[]byte("one"), []byte("two")})
	if err != nil {
		t.Fatalf("TranscribeBatch() error = %v", err)
	}
	if fmt.Sprint(got) != "[one two]" {
		t.Fatalf("transcripts = %#v, want [one two]", got)
	}
	if client.contextCalls != 2 || client.legacyCalls != 0 {
		t.Fatalf("context calls = %d, legacy calls = %d", client.contextCalls, client.legacyCalls)
	}
}

func TestTranscribeBatchStopsBeforeCanceledItem(t *testing.T) {
	client := &contextAwareSTTTestClient{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := TranscribeBatch(ctx, client, [][]byte{[]byte("one")}); err == nil {
		t.Fatal("TranscribeBatch() error = nil, want cancellation error")
	}
	if client.contextCalls != 0 {
		t.Fatalf("context calls = %d, want 0", client.contextCalls)
	}
}
