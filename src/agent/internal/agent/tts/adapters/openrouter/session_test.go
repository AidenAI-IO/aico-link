package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"aico-agent/internal/agent/tts"
)

func TestLastSentenceBoundaryHandlesUTF8Punctuation(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{name: "none", text: "没有结束符", want: -1},
		{name: "ascii", text: "English. remainder", want: len("English")},
		{name: "period", text: "第一句。后续", want: len("第一句")},
		{name: "exclamation", text: "第一句！后续", want: len("第一句")},
		{name: "question", text: "第一句？后续", want: len("第一句")},
		{name: "semicolon", text: "第一句；后续", want: len("第一句")},
		{name: "last boundary", text: "第一句。第二句！后续", want: len("第一句。第二句")},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := lastSentenceBoundary(tt.text); got != tt.want {
				t.Fatalf("lastSentenceBoundary(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

func TestSessionSentenceBuffering(t *testing.T) {
	tests := []struct {
		name       string
		fragments  []string
		flush      bool
		wantInputs []string
	}{
		{name: "empty text", fragments: []string{""}},
		{name: "whitespace only", fragments: []string{" \t", "\n"}},
		{name: "punctuation only", fragments: []string{"。！？"}, wantInputs: []string{"。！？"}},
		{name: "consecutive punctuation", fragments: []string{"你好！？继续"}, wantInputs: []string{"你好！？"}},
		{name: "ends at boundary", fragments: []string{"完整句。"}, wantInputs: []string{"完整句。"}},
		{name: "mixed punctuation", fragments: []string{"One.二！Three?四；尾巴"}, wantInputs: []string{"One.二！Three?四；"}},
		{name: "single character and punctuation", fragments: []string{"啊。"}, wantInputs: []string{"啊。"}},
		{name: "multiple fragments without boundary", fragments: []string{"多次", "写入", "仍无边界"}, flush: true, wantInputs: []string{"多次写入仍无边界"}},
		{
			name:       "complete sentences and remainder",
			fragments:  []string{"第一句", "。第二句！第三句？第四句；余下"},
			flush:      true,
			wantInputs: []string{"第一句。第二句！第三句？第四句；", "余下"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			recorder := newOpenRouterRequestRecorder(t, http.StatusOK)
			defer recorder.Close()

			session, sink := newOpenRouterTestSession(t, recorder.URL)
			for i, fragment := range tt.fragments {
				if err := session.WriteText(fragment); err != nil {
					t.Fatalf("WriteText(fragment %d) error = %v", i, err)
				}
			}
			if tt.flush {
				if err := session.Flush(); err != nil {
					t.Fatalf("Flush() error = %v", err)
				}
			}

			assertRecordedInputs(t, recorder.Inputs(), tt.wantInputs)

			if err := session.Close(); err != nil {
				t.Fatalf("Close() error = %v", err)
			}
			if !sink.isDrained() {
				t.Fatal("Close() did not drain the sink")
			}
		})
	}
}

func TestSessionWriteFailureClosesSessionAndPreservesFirstError(t *testing.T) {
	recorder := newOpenRouterRequestRecorder(t, http.StatusInternalServerError)
	defer recorder.Close()

	session, sink := newOpenRouterTestSession(t, recorder.URL)
	firstErr := session.WriteText("第一句。")
	if firstErr == nil || !strings.Contains(firstErr.Error(), "API error 500") {
		t.Fatalf("WriteText(first) error = %v, want API error 500", firstErr)
	}

	secondErr := session.WriteText("第二句。")
	if !errors.Is(secondErr, firstErr) {
		t.Fatalf("WriteText(second) error = %v, want first error %v", secondErr, firstErr)
	}
	assertRecordedInputs(t, recorder.Inputs(), []string{"第一句。"})

	if closeErr := session.Close(); !errors.Is(closeErr, firstErr) {
		t.Fatalf("Close() error = %v, want first error %v", closeErr, firstErr)
	}
	if !sink.isDrained() {
		t.Fatal("Close() did not drain the sink after WriteText failure")
	}
}

func TestRuneEndIndexRejectsInvalidUTF8(t *testing.T) {
	_, err := runeEndIndex(string([]byte{0xff}), 0)
	if err == nil {
		t.Fatal("runeEndIndex() error = nil, want invalid UTF-8 error")
	}
}

func TestSessionSynthesizeUpToAcceptsZeroIndex(t *testing.T) {
	session := &session{textBuffer: bytes.NewBufferString("pending")}
	if err := session.synthesizeUpTo(0); err != nil {
		t.Fatalf("synthesizeUpTo(0) error = %v", err)
	}
	if got := session.textBuffer.String(); got != "pending" {
		t.Fatalf("buffer after synthesizeUpTo(0) = %q, want %q", got, "pending")
	}
}

type openRouterRequestRecorder struct {
	*httptest.Server

	mu     sync.Mutex
	inputs []string
}

func newOpenRouterRequestRecorder(t *testing.T, status int) *openRouterRequestRecorder {
	t.Helper()
	recorder := &openRouterRequestRecorder{}
	recorder.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request speechRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		recorder.mu.Lock()
		recorder.inputs = append(recorder.inputs, request.Input)
		recorder.mu.Unlock()
		if status != http.StatusOK {
			http.Error(w, "synthesis failed", status)
			return
		}
		_, _ = w.Write([]byte{0, 1, 2, 3})
	}))
	return recorder
}

func (r *openRouterRequestRecorder) Inputs() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.inputs...)
}

func newOpenRouterTestSession(t *testing.T, endpoint string) (tts.StreamSession, *openRouterTestSink) {
	t.Helper()
	provider, err := New(tts.ProviderConfig{APIKey: "test-key", Endpoint: endpoint})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() {
		if err := provider.Close(); err != nil {
			t.Errorf("provider.Close() error = %v", err)
		}
	})

	sink := &openRouterTestSink{}
	stream, err := provider.BeginStream(context.Background(), sink)
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	return stream, sink
}

func assertRecordedInputs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("request count = %d, want %d; inputs = %#v", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("request %d input = %q, want %q", i, got[i], want[i])
		}
	}
}

type openRouterTestSink struct {
	mu      sync.Mutex
	pcm     []byte
	drained bool
}

func (s *openRouterTestSink) Format() tts.AudioFormat {
	return tts.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16}
}

func (s *openRouterTestSink) WritePCM(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pcm = append(s.pcm, data...)
	return nil
}

func (s *openRouterTestSink) Drain(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drained = true
	return nil
}

func (s *openRouterTestSink) isDrained() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.drained
}

func (s *openRouterTestSink) Stop() error { return nil }
