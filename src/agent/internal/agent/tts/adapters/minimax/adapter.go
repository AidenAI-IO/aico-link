// Package minimax implements TTS adapters for Minimax.
//
// Minimax does not support true incremental text streaming: the WebSocket
// `task_continue` event must contain a complete text fragment, not partial
// tokens. This adapter buffers WriteText calls internally until a sentence
// boundary is reached, then performs a full task_start/task_continue/task_finish
// cycle for that sentence.
//
// Supported provider names are "minimax" for the global endpoint and
// "minimax-cn" for the mainland China endpoint. The adapter opens a WebSocket
// connection per sentence because the server closes after task_finish.
package minimax

import (
	"fmt"
	"strings"

	"aico-agent/internal/agent/tts"
)

func init() {
	tts.Register(ProviderName, NewWebSocket)
	tts.Register(ProviderNameCN, NewWebSocket)
}

const (
	ProviderName   = "minimax"
	ProviderNameCN = "minimax-cn"

	defaultModel      = "speech-2.8-hd"
	defaultSampleRate = 16000
	defaultChannels   = 1
	defaultBitWidth   = 16
	defaultVoiceID    = "male-qn-qingse"
	defaultEmotion    = "happy"
)

// commonConfig holds the fields shared by HTTP and WebSocket adapters.
type commonConfig struct {
	provider string
	apiKey   string
	model    string
	voiceID  string
	emotion  string
	speed    float64
	proxy    tts.ProxyConfig
	endpoint string
}

func parseConfig(cfg tts.ProviderConfig, defaultEndpoint string) commonConfig {
	provider := normalizeProvider(cfg.Provider)
	voiceID := cfg.Voice
	if voiceID == "" {
		voiceID = defaultVoiceID
	}
	emotion := defaultEmotion
	if e, ok := cfg.Extra["emotion"].(string); ok && e != "" {
		emotion = e
	}
	model := defaultModel
	if m, ok := cfg.Extra["model"].(string); ok && m != "" {
		model = m
	}
	speed := cfg.SpeedRatio
	if speed == 0 {
		speed = 1.0
	}
	endpoint := cfg.Endpoint
	if endpoint == "" {
		endpoint = defaultEndpoint
	}
	return commonConfig{
		provider: provider,
		apiKey:   cfg.APIKey,
		model:    model,
		voiceID:  voiceID,
		emotion:  emotion,
		speed:    speed,
		proxy:    cfg.Proxy,
		endpoint: endpoint,
	}
}

func normalizeProvider(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case ProviderName:
		return ProviderName
	case ProviderNameCN, "":
		return ProviderNameCN
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

func defaultEndpointForProvider(provider string) (string, error) {
	switch normalizeProvider(provider) {
	case ProviderName:
		return wsEndpoint, nil
	case ProviderNameCN:
		return wsEndpointCN, nil
	default:
		return "", fmt.Errorf("unsupported minimax provider: %s", provider)
	}
}

func capabilities() tts.Capabilities {
	return tts.Capabilities{
		SupportsContextContinuation: false,
		SupportedSampleRates:        []int{8000, 16000, 22050, 24000, 32000, 44100},
		RegionRestricted:            false,
		TextGranularity:             tts.TextGranularitySentence,
		SupportsIncrementalAudio:    true,
		SupportsFlush:               true,
	}
}
