package agent

import (
	"context"
	"errors"
	"strings"

	"aico-agent/internal/agent/tts"
)

const DefaultTTSPlaybackTestText = "test passed"

type TTSPlaybackTestRequest struct {
	Provider    string
	APIKey      string
	Model       string
	VoiceID     string
	ReferenceID string
	Emotion     string
	Speed       float64
	Text        string
}

type TTSPlaybackTestResult struct {
	Provider string `json:"provider"`
	Text     string `json:"text"`
	Spoke    bool   `json:"spoke"`
}

func RunTTSPlaybackTest(ctx context.Context, cfg Config, req TTSPlaybackTestRequest) (TTSPlaybackTestResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		text = DefaultTTSPlaybackTestText
	}
	applyTTSPlaybackTestRequest(&cfg, req)
	if strings.TrimSpace(cfg.TTS.Provider) == "" {
		return TTSPlaybackTestResult{Text: text}, errors.New("tts.provider is required")
	}

	provider, err := tts.New(buildTTSProviderConfig(cfg))
	if err != nil {
		return TTSPlaybackTestResult{Provider: cfg.TTS.Provider, Text: text}, err
	}
	manager := tts.NewProviderManager(provider, nil)
	defer manager.Close()

	audio := NewAudioServiceClient(cfg.Audio.SocketOrDefault())
	playback := newTTSPlaybackBackendFromConfig(cfg, audio, nil)
	spoke, err := speakWithTTSManager(ctx, manager, playback, cfg, text)
	result := TTSPlaybackTestResult{
		Provider: manager.Current(),
		Text:     text,
		Spoke:    spoke,
	}
	if err != nil {
		return result, err
	}
	if !spoke {
		return result, errors.New("TTS completed without writing audio")
	}
	return result, nil
}

func applyTTSPlaybackTestRequest(cfg *Config, req TTSPlaybackTestRequest) {
	if cfg == nil {
		return
	}
	if provider := strings.TrimSpace(req.Provider); provider != "" {
		cfg.TTS.Provider = provider
	}
	// Always apply stdin values — they represent the user's current form state.
	// Empty string means the field is hidden/cleared, which should override
	// whatever is persisted in the toml file.
	//
	// NOTE: Model, VoiceID, ReferenceID, and Emotion are unconditionally overwritten
	// (including with empty strings) to support the form-clearing use case. Provider,
	// APIKey, and Speed retain the "apply only if non-empty/positive" guard because
	// they are not expected to be cleared by the form UI. This function is exclusively
	// called from config_commands.go's handle_tts_playback_test_request with a
	// complete JSON payload from stdin.
	cfg.TTS.Model = req.Model
	cfg.TTS.VoiceID = req.VoiceID
	cfg.TTS.ReferenceID = req.ReferenceID
	cfg.TTS.Emotion = req.Emotion
	if req.APIKey != "" {
		cfg.TTS.APIKey = req.APIKey
	}
	if req.Speed > 0 {
		cfg.TTS.Speed = req.Speed
	}

	// Resolve a [tts_providers] reference last. The form posts only the
	// reference plus the global settings now that credentials live on records,
	// so without this the provider stays a record name.
	//
	// The ordering composes with the blanking above: a field the form omitted is
	// empty here, and resolution fills exactly the empty ones from the record,
	// while a value the user typed still wins.
	resolveTTSProvider(cfg)
}
