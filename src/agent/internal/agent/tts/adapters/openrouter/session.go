package openrouter

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"unicode/utf8"

	"aico-agent/internal/agent/tts"
)

type session struct {
	ctx        context.Context
	adapter    *Adapter
	sink       tts.AudioSink
	httpClient *http.Client

	mu         sync.Mutex
	textBuffer *bytes.Buffer
	closed     bool
	finalized  bool
	lastErr    error
}

func (s *session) WriteText(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return s.sessionErr()
	}
	if text == "" {
		return nil
	}

	s.textBuffer.WriteString(text)

	// Find sentence boundary and synthesize only up to it, retaining remainder.
	bufferedText := s.textBuffer.String()
	if idx := lastSentenceBoundary(bufferedText); idx >= 0 {
		endIdx, err := runeEndIndex(bufferedText, idx)
		if err == nil {
			err = s.synthesizeUpTo(endIdx)
		}
		if err != nil {
			s.closed = true
			return s.recordErr(err)
		}
	}
	return nil
}

func (s *session) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return s.sessionErr()
	}
	if s.textBuffer.Len() == 0 {
		return nil
	}
	if err := s.synthesizeAndClear(); err != nil {
		s.closed = true
		return s.recordErr(err)
	}
	return nil
}

func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.finalized {
		return s.lastErr
	}
	s.closed = true
	s.finalized = true

	if s.lastErr == nil && s.textBuffer.Len() > 0 {
		if err := s.synthesizeAndClear(); err != nil {
			s.recordErr(err)
		}
	}

	if err := s.sink.Drain(s.ctx); err != nil {
		s.recordErr(err)
	}

	return s.lastErr
}

func (s *session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

// recordErr stores and returns the first session error.
// Must be called with s.mu held.
func (s *session) recordErr(err error) error {
	if err != nil && s.lastErr == nil {
		s.lastErr = err
	}
	return s.lastErr
}

// sessionErr returns the stored failure, or the generic closed-session error.
// Must be called with s.mu held.
func (s *session) sessionErr() error {
	if s.lastErr != nil {
		return s.lastErr
	}
	return tts.ErrSessionClosed
}

// ResetBuffer drops any buffered text not yet synthesized.
func (s *session) ResetBuffer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.textBuffer.Reset()
}

// synthesizeAndClear sends buffered text to OpenRouter TTS and writes PCM to sink.
// Must be called with s.mu held.
func (s *session) synthesizeAndClear() error {
	text := s.textBuffer.String()
	s.textBuffer.Reset()

	if strings.TrimSpace(text) == "" {
		return nil
	}

	return s.synthesize(text)
}

// synthesizeUpTo sends text up to the specified index and retains the remainder.
// Must be called with s.mu held.
func (s *session) synthesizeUpTo(endIdx int) error {
	text := s.textBuffer.String()
	if endIdx < 0 || endIdx > len(text) {
		return nil
	}
	// Zero is a valid end index: it selects an empty prefix and leaves the
	// entire buffer pending for a later synthesis.
	if endIdx == 0 {
		return nil
	}

	toSend := text[:endIdx]
	remainder := text[endIdx:]

	s.textBuffer.Reset()
	s.textBuffer.WriteString(remainder)

	if strings.TrimSpace(toSend) == "" {
		return nil
	}

	return s.synthesize(toSend)
}

// synthesize sends the given text to OpenRouter TTS API and writes PCM to sink.
// Must be called with s.mu held.
func (s *session) synthesize(text string) error {
	reqBody := speechRequest{
		Model:          s.adapter.model,
		Voice:          s.adapter.voice,
		Input:          text,
		ResponseFormat: "pcm",
	}
	if s.adapter.speed != 1.0 {
		reqBody.Speed = s.adapter.speed
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return s.recordErr(fmt.Errorf("marshal request: %w", err))
	}

	url := fmt.Sprintf("%s/audio/speech", s.adapter.endpoint)
	req, err := http.NewRequestWithContext(s.ctx, "POST", url, bytes.NewReader(jsonData))
	if err != nil {
		return s.recordErr(fmt.Errorf("create request: %w", err))
	}

	req.Header.Set("Authorization", "Bearer "+s.adapter.apiKey)
	req.Header.Set("Content-Type", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return s.recordErr(fmt.Errorf("send request: %w", err))
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return s.recordErr(fmt.Errorf("API error %d: %s", resp.StatusCode, string(body)))
	}

	// Response is raw PCM s16le audio stream.
	pcmData, err := io.ReadAll(resp.Body)
	if err != nil {
		return s.recordErr(fmt.Errorf("read audio: %w", err))
	}

	if len(pcmData) > 0 {
		if err := s.sink.WritePCM(pcmData); err != nil {
			return s.recordErr(fmt.Errorf("write pcm: %w", err))
		}
	}

	return nil
}

func runeEndIndex(text string, startIdx int) (int, error) {
	if startIdx < 0 || startIdx >= len(text) {
		return 0, fmt.Errorf("rune start index %d out of range", startIdx)
	}
	r, size := utf8.DecodeRuneInString(text[startIdx:])
	if r == utf8.RuneError && size == 1 {
		return 0, fmt.Errorf("invalid UTF-8 at byte %d", startIdx)
	}
	return startIdx + size, nil
}

// lastSentenceBoundary returns the byte index of the last sentence boundary
// rune in text, or -1 if none.
func lastSentenceBoundary(text string) int {
	last := -1
	for i, r := range text {
		switch r {
		case '.', '!', '?', '\n', '。', '！', '？', '；':
			last = i
		}
	}
	return last
}

type speechRequest struct {
	Model          string  `json:"model"`
	Voice          string  `json:"voice"`
	Input          string  `json:"input"`
	ResponseFormat string  `json:"response_format"`
	Speed          float64 `json:"speed,omitempty"`
}
