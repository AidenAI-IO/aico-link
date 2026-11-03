package agent

import "testing"

// Runtime switching must read the target provider's own credentials instead of
// falling back to the active provider's key.
func TestBuildTTSProviderConfigForUsesRecords(t *testing.T) {
	cfg := Config{
		TTSProviders: map[string]TTSProvider{
			"minimax-main": {Type: "minimax", APIKey: "sk-minimax", VoiceID: "male-qn-qingse"},
			"fish-main":    {Type: "fish-audio", APIKey: "sk-fish", ReferenceID: "ref-abc"},
		},
		TTS: TTSConfig{Provider: "minimax", APIKey: "sk-minimax", VoiceID: "male-qn-qingse"},
	}

	// Switching to fish-audio must pick up the fish record, not the active
	// minimax key.
	got := buildTTSProviderConfigFor(cfg, "fish-audio")
	if got.Provider != "fish-audio" {
		t.Errorf("provider = %q, want %q", got.Provider, "fish-audio")
	}
	if got.APIKey != "sk-fish" {
		t.Errorf("api_key = %q, want %q (fell back to the active provider's key)", got.APIKey, "sk-fish")
	}
	if ref, _ := got.Extra["reference_id"].(string); ref != "ref-abc" {
		t.Errorf("reference_id = %q, want %q", ref, "ref-abc")
	}
	// Fish Audio does not use voice, and must not inherit minimax's.
	if got.Voice != "" {
		t.Errorf("voice = %q, want empty for fish-audio", got.Voice)
	}
}

// A record can also be addressed by name, which is what the config page passes.
func TestBuildTTSProviderConfigForAcceptsRecordName(t *testing.T) {
	cfg := Config{
		TTSProviders: map[string]TTSProvider{
			"fish-main": {Type: "fish-audio", APIKey: "sk-fish", ReferenceID: "ref-abc"},
		},
		TTS: TTSConfig{Provider: "fish-main"},
	}

	got := buildTTSProviderConfigFor(cfg, "fish-main")
	if got.Provider != "fish-audio" {
		t.Errorf("provider = %q, want the record's type %q", got.Provider, "fish-audio")
	}
	if got.APIKey != "sk-fish" {
		t.Errorf("api_key = %q, want %q", got.APIKey, "sk-fish")
	}
}

func TestBuildTTSProviderConfigForResolvesAPIKeyEnvironmentReference(t *testing.T) {
	t.Setenv("AICO_TEST_SWITCH_KEY", "sk-env-switch")
	cfg := Config{
		TTSProviders: map[string]TTSProvider{
			"fish-main": {Type: "fish-audio", APIKey: "$AICO_TEST_SWITCH_KEY"},
		},
	}

	got := buildTTSProviderConfigFor(cfg, "fish-audio")
	if got.APIKey != "sk-env-switch" {
		t.Errorf("api_key = %q, want %q from environment reference", got.APIKey, "sk-env-switch")
	}
}

func TestBuildTTSProviderConfigForDoesNotFallbackWhenRecordEnvironmentReferenceIsUnavailable(t *testing.T) {
	t.Setenv("AICO_TEST_MISSING_SWITCH_KEY", "")
	cfg := Config{
		TTSProviders: map[string]TTSProvider{
			"fish-main": {Type: "fish-audio", APIKey: "$AICO_TEST_MISSING_SWITCH_KEY"},
		},
		TTS: TTSConfig{Provider: "minimax", APIKey: "active-provider-secret"},
	}

	got := buildTTSProviderConfigFor(cfg, "fish-main")
	if got.APIKey != "" {
		t.Fatalf("api_key = %q, want empty resolved record credential without active-provider fallback", got.APIKey)
	}
}

func TestBuildTTSProviderConfigForFallsBackWhenRecordCredentialIsBlank(t *testing.T) {
	cfg := Config{
		TTSProviders: map[string]TTSProvider{
			"fish-main": {Type: "fish-audio"},
		},
		TTS: TTSConfig{Provider: "minimax", APIKey: "active-provider-secret"},
	}

	got := buildTTSProviderConfigFor(cfg, "fish-main")
	if got.APIKey != "active-provider-secret" {
		t.Fatalf("api_key = %q, want blank record to use the flat TTS fallback", got.APIKey)
	}
}
