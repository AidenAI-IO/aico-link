package agent

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/BurntSushi/toml"
)

func TestVoiceProviderRecordsUseCanonicalTypeTOML(t *testing.T) {
	cfg := writeVoiceProviderConfig(t, `
[voice_settings.classic.tts.providers.voice]
type = "fish-audio"
provider = "minimax"
api_key = "sk-v"

[voice_settings.classic.stt.providers.speech]
type = "openai-whisper"
provider = "tencent-asr"
api_key = "sk-s"

[voice_settings.classic.tts]
provider = "voice"

[voice_settings.classic.stt]
provider = "speech"

[model_settings.model]
provider = "openai"
model = "gpt-4o"
api_key = "sk-x"
`)
	if cfg.TTS.Provider != "fish-audio" || cfg.STT.Provider != "openai-whisper" {
		t.Fatalf("canonical types did not win: tts=%q stt=%q", cfg.TTS.Provider, cfg.STT.Provider)
	}

	var encoded bytes.Buffer
	if err := toml.NewEncoder(&encoded).Encode(Config{
		TTSProviders: map[string]TTSProvider{"voice": {Type: "fish-audio"}},
		STTProviders: map[string]STTProvider{"speech": {Type: "openai-whisper"}},
	}); err != nil {
		t.Fatalf("encode voice provider records: %v", err)
	}
	output := encoded.String()
	if !strings.Contains(output, `type = "fish-audio"`) || !strings.Contains(output, `type = "openai-whisper"`) {
		t.Errorf("canonical type fields missing:\n%s", output)
	}
}

func TestLegacyVoiceProviderRecordFieldStillLoads(t *testing.T) {
	cfg := writeVoiceProviderConfig(t, `
[voice_settings.classic.tts.providers.voice]
provider = "fish-audio"

[voice_settings.classic.stt.providers.speech]
provider = "openai-whisper"

[voice_settings.classic.tts]
provider = "voice"

[voice_settings.classic.stt]
provider = "speech"

[model_settings.model]
provider = "openai"
model = "gpt-4o"
api_key = "sk-x"
`)
	if cfg.TTS.Provider != "fish-audio" || cfg.STT.Provider != "openai-whisper" {
		t.Fatalf("legacy provider fields did not load: tts=%q stt=%q", cfg.TTS.Provider, cfg.STT.Provider)
	}
}

// writeVoiceProviderConfig writes a config file and loads it through the runtime
// path, which is where reference resolution and legacy migration run.
func writeVoiceProviderConfig(t *testing.T, body string) Config {
	t.Helper()
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	return cfg
}

// A [tts] provider that names a [tts_providers] record resolves to that record's
// type and inherits its fields, mirroring resolveModelProvider for [model].
func TestTTSProviderReferenceResolves(t *testing.T) {
	cfg := writeVoiceProviderConfig(t, `
[voice_settings.classic.tts.providers.fish-main]
type = "fish-audio"
api_key = "sk-fish"
reference_id = "ref-123"

[voice_settings.classic.tts]
provider = "fish-main"
speed = 1.2
`)

	if cfg.TTS.Provider != "fish-audio" {
		t.Errorf("tts.provider = %q, want %q", cfg.TTS.Provider, "fish-audio")
	}
	if cfg.TTS.APIKey != "sk-fish" {
		t.Errorf("tts.api_key = %q, want %q", cfg.TTS.APIKey, "sk-fish")
	}
	if cfg.TTS.ReferenceID != "ref-123" {
		t.Errorf("tts.reference_id = %q, want %q", cfg.TTS.ReferenceID, "ref-123")
	}
	if cfg.TTS.Speed != 1.2 {
		t.Errorf("tts.speed = %v, want 1.2", cfg.TTS.Speed)
	}
}

// Two records of the same provider type must stay independently addressable --
// this is the whole point of named records over the old per-type credentials.
func TestTTSTwoRecordsOfSameType(t *testing.T) {
	cfg := writeVoiceProviderConfig(t, `
[voice_settings.classic.tts.providers.minimax-main]
type = "minimax"
api_key = "sk-aaa"
voice_id = "male-qn-qingse"

[voice_settings.classic.tts.providers.minimax-alt]
type = "minimax"
api_key = "sk-bbb"
voice_id = "female-shaonv"

[voice_settings.classic.tts]
provider = "minimax-alt"
`)

	if cfg.TTS.Provider != "minimax" {
		t.Errorf("tts.provider = %q, want %q", cfg.TTS.Provider, "minimax")
	}
	if cfg.TTS.APIKey != "sk-bbb" {
		t.Errorf("tts.api_key = %q, want %q (picked the wrong record)", cfg.TTS.APIKey, "sk-bbb")
	}
	if cfg.TTS.VoiceID != "female-shaonv" {
		t.Errorf("tts.voice_id = %q, want %q", cfg.TTS.VoiceID, "female-shaonv")
	}
	if len(cfg.TTSProviders) != 2 {
		t.Errorf("len(tts_providers) = %d, want 2", len(cfg.TTSProviders))
	}
}

// A bare provider type in [tts] keeps working: devices in the field have flat
// configs and must not need a migration to keep speaking.
func TestTTSBareProviderTypeStillWorks(t *testing.T) {
	cfg := writeVoiceProviderConfig(t, `
[voice_settings.classic.tts]
provider = "minimax-cn"
api_key = "sk-flat"
voice_id = "male-qn-qingse"
`)

	if cfg.TTS.Provider != "minimax-cn" {
		t.Errorf("tts.provider = %q, want %q", cfg.TTS.Provider, "minimax-cn")
	}
	if cfg.TTS.APIKey != "sk-flat" {
		t.Errorf("tts.api_key = %q, want %q", cfg.TTS.APIKey, "sk-flat")
	}
}

// STT records carry the Tencent credential set, which is the largest per-provider
// field group and the clearest case for moving fields off the flat section.
func TestSTTProviderReferenceResolves(t *testing.T) {
	cfg := writeVoiceProviderConfig(t, `
[voice_settings.classic.stt.providers.tencent-main]
type = "tencent-asr"
app_id = "1234"
secret_id = "AKID-xxx"
secret_key = "secret-yyy"
region = "ap-shanghai"

[voice_settings.classic.stt]
provider = "tencent-main"
language = "zh"
`)

	if cfg.STT.Provider != "tencent-asr" {
		t.Errorf("stt.provider = %q, want %q", cfg.STT.Provider, "tencent-asr")
	}
	if cfg.STT.AppID != "1234" {
		t.Errorf("stt.app_id = %q, want %q", cfg.STT.AppID, "1234")
	}
	if cfg.STT.SecretID != "AKID-xxx" {
		t.Errorf("stt.secret_id = %q, want %q", cfg.STT.SecretID, "AKID-xxx")
	}
	if cfg.STT.SecretKey != "secret-yyy" {
		t.Errorf("stt.secret_key = %q, want %q", cfg.STT.SecretKey, "secret-yyy")
	}
	if cfg.STT.Region != "ap-shanghai" {
		t.Errorf("stt.region = %q, want %q", cfg.STT.Region, "ap-shanghai")
	}
	if cfg.STT.Language != "zh" {
		t.Errorf("stt.language = %q, want %q", cfg.STT.Language, "zh")
	}
}

func TestSTTBareProviderTypeStillWorks(t *testing.T) {
	cfg := writeVoiceProviderConfig(t, `
[voice_settings.classic.stt]
provider = "openai-whisper"
api_key = "sk-whisper"
base_url = "https://api.openai.com/v1"
`)

	if cfg.STT.Provider != "openai-whisper" {
		t.Errorf("stt.provider = %q, want %q", cfg.STT.Provider, "openai-whisper")
	}
	if cfg.STT.APIKey != "sk-whisper" {
		t.Errorf("stt.api_key = %q, want %q", cfg.STT.APIKey, "sk-whisper")
	}
}

// A field set directly on [tts] wins over the record, matching how [model]
// overrides an inherited [model_providers.*] value.
func TestVoiceFlatFieldOverridesRecord(t *testing.T) {
	cfg := writeVoiceProviderConfig(t, `
[voice_settings.classic.tts.providers.minimax-main]
type = "minimax"
api_key = "sk-record"
voice_id = "male-qn-qingse"

[voice_settings.classic.tts]
provider = "minimax-main"
api_key = "sk-override"
`)

	if cfg.TTS.APIKey != "sk-override" {
		t.Errorf("tts.api_key = %q, want %q", cfg.TTS.APIKey, "sk-override")
	}
	if cfg.TTS.VoiceID != "male-qn-qingse" {
		t.Errorf("tts.voice_id = %q, want the inherited %q", cfg.TTS.VoiceID, "male-qn-qingse")
	}
}

func TestVoiceProviderAPIKeyEnvironmentReferencesResolve(t *testing.T) {
	t.Setenv("AICO_TEST_TTS_KEY", "sk-from-env")
	t.Setenv("AICO_TEST_STT_KEY", "sk-stt-env")

	cfg := writeVoiceProviderConfig(t, `
[voice_settings.classic.tts.providers.minimax-main]
type = "minimax"
api_key = "$AICO_TEST_TTS_KEY"

[voice_settings.classic.tts]
provider = "minimax-main"

[voice_settings.classic.stt.providers.whisper]
type = "openai-whisper"
api_key = "$AICO_TEST_STT_KEY"

[voice_settings.classic.stt]
provider = "whisper"
`)

	if cfg.TTS.APIKey != "sk-from-env" {
		t.Errorf("tts.api_key = %q, want %q from environment reference", cfg.TTS.APIKey, "sk-from-env")
	}
	if cfg.STT.APIKey != "sk-stt-env" {
		t.Errorf("stt.api_key = %q, want %q from environment reference", cfg.STT.APIKey, "sk-stt-env")
	}
}
