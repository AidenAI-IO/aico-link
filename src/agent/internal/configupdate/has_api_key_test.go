package configupdate

import (
	"testing"

	"aico-agent/internal/agent"
)

// The shipped agent.toml references "$OPENAI_API_KEY"-style variables that a
// device never sets. Onboarding and Config Web read has_api_key to decide
// whether a key is filled in, so an unset reference must report false.
func TestHasAPIKeyReflectsResolvedCredential(t *testing.T) {
	t.Setenv("AIDEN_TEST_SET_KEY", "sk-from-env")
	t.Setenv("AIDEN_TEST_EMPTY_KEY", "")
	cases := []struct {
		name   string
		apiKey string
		want   bool
	}{
		{"literal key", "sk-literal", true},
		{"empty", "", false},
		{"unset environment reference", "$AIDEN_TEST_UNSET_KEY", false},
		{"empty environment reference", "$AIDEN_TEST_EMPTY_KEY", false},
		{"set environment reference", "$AIDEN_TEST_SET_KEY", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := agent.Config{
				ModelProviders:      map[string]agent.ModelProvider{"m": {Type: "openai", APIKey: tc.apiKey}},
				TTSProviders:        map[string]agent.TTSProvider{"t": {Type: "minimax-cn", APIKey: tc.apiKey}},
				STTProviders:        map[string]agent.STTProvider{"s": {Type: "openai-whisper", APIKey: tc.apiKey}},
				VoiceModelProviders: map[string]agent.VoiceModelProvider{"v": {Type: "qwen", APIKey: tc.apiKey}},
			}
			dto := FromAgentConfig(cfg)
			got := map[string]bool{
				"model_providers":       dto.ModelProviders["m"].HasAPIKey,
				"tts_providers":         dto.TTSProviders["t"].HasAPIKey,
				"stt_providers":         dto.STTProviders["s"].HasAPIKey,
				"voice_model_providers": dto.VoiceModelProviders["v"].HasAPIKey,
			}
			for section, has := range got {
				if has != tc.want {
					t.Errorf("%s has_api_key = %v, want %v", section, has, tc.want)
				}
			}
		})
	}
}
