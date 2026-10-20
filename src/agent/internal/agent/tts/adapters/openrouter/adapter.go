// Package openrouter implements a TTS adapter for OpenRouter's HTTP API.
//
// OpenRouter exposes a unified /audio/speech endpoint that routes to various
// TTS providers (OpenAI, Microsoft, Google, etc.). This adapter uses
// response_format=pcm to receive raw PCM s16le audio, avoiding the need for
// external audio decoding libraries.
//
// Text is buffered until a sentence boundary before synthesis, matching the
// pattern used by the minimax adapter.
package openrouter

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"time"

	"aiden-agent/internal/agent/tts"
)

const (
	ProviderName    = "openrouter"
	defaultEndpoint = "https://openrouter.ai/api/v1"
	defaultModel    = "google/gemini-3.1-flash-tts-preview"
	defaultVoice    = "alloy"
	requestTimeout  = 30 * time.Second
)

func init() {
	tts.Register(ProviderName, New)
}

// Adapter is the OpenRouter TTS provider.
type Adapter struct {
	apiKey     string
	model      string
	voice      string
	endpoint   string
	speed      float64
	httpClient *http.Client
}

var _ tts.TTSProvider = (*Adapter)(nil)

// New constructs an OpenRouter TTS provider from the unified config.
func New(cfg tts.ProviderConfig) (tts.TTSProvider, error) {
	if cfg.APIKey == "" {
		return nil, fmt.Errorf("openrouter: api_key is required")
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	model := defaultModel
	if m, ok := cfg.Extra["model"].(string); ok && m != "" {
		model = m
	}
	voice := cfg.Voice
	if voice == "" || !isValidOpenRouterVoice(voice) {
		voice = defaultVoiceForModel(model)
	}
	speed := cfg.SpeedRatio
	if speed == 0 {
		speed = 1.0
	}
	return &Adapter{
		apiKey:     cfg.APIKey,
		model:      model,
		voice:      voice,
		endpoint:   endpoint,
		speed:      speed,
		httpClient: &http.Client{Transport: cfg.Proxy.Transport(), Timeout: requestTimeout},
	}, nil
}

func (a *Adapter) Name() string { return ProviderName }

func (a *Adapter) Capabilities() tts.Capabilities {
	return tts.Capabilities{
		SupportsContextContinuation: false,
		SupportedSampleRates:        []int{24000},
		RegionRestricted:            true,
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
		httpClient: a.httpClient,
		textBuffer: new(bytes.Buffer),
	}, nil
}

// defaultVoiceForModel returns a known-good voice for the given model.
func defaultVoiceForModel(model string) string {
	switch {
	case strings.HasPrefix(model, "hexgrad/kokoro"):
		return "af_heart"
	case strings.HasPrefix(model, "google/"):
		return "Kore"
	case strings.Contains(model, "mai-voice"):
		return "en-US-AndrewMultilingualNeural"
	default:
		return defaultVoice // "alloy" works for OpenAI-compatible models
	}
}

// isValidOpenRouterVoice returns true if the voice is known to work with
// OpenRouter TTS models. Voices from other TTS providers (minimax, fish-audio,
// etc.) will silently fall back to the model default to avoid upstream errors.
func isValidOpenRouterVoice(voice string) bool {
	// OpenAI voices
	switch strings.ToLower(voice) {
	case "alloy", "ash", "ballad", "coral", "echo", "fable", "nova", "onyx", "sage", "shimmer":
		return true
	}
	// Google Gemini voices (capitalized)
	switch voice {
	case "Kore", "Puck", "Charon", "Fenrir", "Aoede", "Leda", "Orus", "Zephyr":
		return true
	}
	// Kokoro voices
	if strings.HasPrefix(voice, "af_") || strings.HasPrefix(voice, "am_") ||
		strings.HasPrefix(voice, "bf_") || strings.HasPrefix(voice, "bm_") {
		return true
	}
	// Microsoft Azure neural voices (contain "Neural")
	if strings.Contains(voice, "Neural") {
		return true
	}
	return false
}
