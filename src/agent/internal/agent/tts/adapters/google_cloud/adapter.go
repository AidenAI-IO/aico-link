// Package google_cloud implements a TTS adapter for Google Cloud Text-to-Speech API.
//
// Uses the REST API with API Key authentication.
// Text is buffered until a sentence boundary before synthesis, matching the
// pattern used by the minimax adapter.
package google_cloud

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"aiden-agent/internal/agent/tts"
)

const (
	ProviderName    = "google-cloud"
	defaultEndpoint = "https://texttospeech.googleapis.com/v1/text:synthesize"
	defaultVoice    = "en-US-Neural2-C"
)

func init() {
	tts.Register(ProviderName, New)
}

// Adapter is the Google Cloud TTS provider.
type Adapter struct {
	endpoint   string
	voice      string
	language   string
	speed      float64
	apiKey     string
	httpClient *http.Client
}

var _ tts.TTSProvider = (*Adapter)(nil)

// New constructs a Google Cloud TTS provider from the unified config.
func New(cfg tts.ProviderConfig) (tts.TTSProvider, error) {
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}

	voice := cfg.Voice
	if voice == "" {
		voice = defaultVoice
	}

	speed := cfg.SpeedRatio
	if speed == 0 {
		speed = 1.0
	}

	adapter := &Adapter{
		endpoint:   endpoint,
		voice:      voice,
		language:   extractLanguageFromVoice(voice),
		speed:      speed,
		httpClient: &http.Client{Transport: cfg.Proxy.Transport()},
	}

	if cfg.APIKey == "" {
		return nil, fmt.Errorf("google-cloud TTS: api_key is required")
	}

	adapter.apiKey = cfg.APIKey
	return adapter, nil
}

func (a *Adapter) Name() string { return ProviderName }

func (a *Adapter) Capabilities() tts.Capabilities {
	return tts.Capabilities{
		SupportsContextContinuation: false,
		SupportedSampleRates:        []int{24000},
		RegionRestricted:            false,
	}
}

func (a *Adapter) Close() error { return nil }

func (a *Adapter) BeginStream(ctx context.Context, sink tts.AudioSink) (tts.StreamSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	return &session{
		ctx:        ctx,
		adapter:    a,
		sink:       sink,
		textBuffer: new(bytes.Buffer),
	}, nil
}

func extractLanguageFromVoice(voice string) string {
	// Voice format: "en-US-Neural2-C" -> "en-US"
	parts := strings.Split(voice, "-")
	if len(parts) >= 2 {
		return parts[0] + "-" + parts[1]
	}
	return "en-US"
}

// --- Session implementation ---

type session struct {
	ctx        context.Context
	adapter    *Adapter
	sink       tts.AudioSink
	textBuffer *bytes.Buffer
	errMu      sync.Mutex
	lastErr    error
}

func (s *session) WriteText(text string) error {
	s.textBuffer.WriteString(text)
	return s.flushIfNeeded()
}

func (s *session) Flush() error {
	if s.textBuffer.Len() == 0 {
		return nil
	}
	text := s.textBuffer.String()
	s.textBuffer.Reset()
	return s.synthesize(text)
}

func (s *session) Close() error {
	if err := s.Flush(); err != nil {
		return err
	}
	if err := s.sink.Drain(s.ctx); err != nil {
		s.recordErr(err)
	}
	return s.Err()
}

func (s *session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.lastErr
}

func (s *session) recordErr(err error) {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.lastErr == nil {
		s.lastErr = err
	}
}

func (s *session) flushIfNeeded() error {
	text := s.textBuffer.String()
	idx := findSentenceBoundary(text)
	if idx < 0 {
		return nil
	}
	toSynth := text[:idx]
	s.textBuffer.Reset()
	s.textBuffer.WriteString(text[idx:])
	return s.synthesize(toSynth)
}

func (s *session) synthesize(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}

	reqBody := googleCloudTTSRequest{
		Input: googleCloudTTSInput{
			Text: text,
		},
		Voice: googleCloudTTSVoice{
			LanguageCode: s.adapter.language,
			Name:         s.adapter.voice,
		},
		AudioConfig: googleCloudTTSAudioConfig{
			AudioEncoding:   "LINEAR16",
			SampleRateHertz: 24000,
			SpeakingRate:    s.adapter.speed,
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		s.recordErr(fmt.Errorf("marshal request: %w", err))
		return err
	}

	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", s.adapter.endpoint, bytes.NewReader(jsonData))
	if err != nil {
		s.recordErr(fmt.Errorf("create request: %w", err))
		return err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Goog-Api-Key", s.adapter.apiKey)

	resp, err := s.adapter.httpClient.Do(req)
	if err != nil {
		s.recordErr(fmt.Errorf("send request: %w", err))
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		err := fmt.Errorf("API error %d: %s", resp.StatusCode, string(body))
		s.recordErr(err)
		return err
	}

	var result googleCloudTTSResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		s.recordErr(fmt.Errorf("decode response: %w", err))
		return err
	}

	// Decode base64 audio and send to sink
	pcmData, err := base64.StdEncoding.DecodeString(result.AudioContent)
	if err != nil {
		s.recordErr(fmt.Errorf("decode audio: %w", err))
		return err
	}

	if err := s.sink.WritePCM(pcmData); err != nil {
		s.recordErr(fmt.Errorf("write to sink: %w", err))
		return err
	}

	return nil
}

// findSentenceBoundary returns the index after a sentence-ending punctuation,
// or -1 if no boundary is found.
func findSentenceBoundary(text string) int {
	sentenceEndings := []rune{'.', '!', '?', '。', '！', '？', '\n'}
	for i, r := range text {
		for _, ending := range sentenceEndings {
			if r == ending {
				return i + len(string(r))
			}
		}
	}
	return -1
}

// --- Google Cloud TTS protocol message types ---

type googleCloudTTSRequest struct {
	Input       googleCloudTTSInput       `json:"input"`
	Voice       googleCloudTTSVoice       `json:"voice"`
	AudioConfig googleCloudTTSAudioConfig `json:"audioConfig"`
}

type googleCloudTTSInput struct {
	Text string `json:"text"`
}

type googleCloudTTSVoice struct {
	LanguageCode string `json:"languageCode"`
	Name         string `json:"name"`
}

type googleCloudTTSAudioConfig struct {
	AudioEncoding   string  `json:"audioEncoding"`
	SampleRateHertz int     `json:"sampleRateHertz"`
	SpeakingRate    float64 `json:"speakingRate"`
}

type googleCloudTTSResponse struct {
	AudioContent string `json:"audioContent"` // base64 encoded
}
