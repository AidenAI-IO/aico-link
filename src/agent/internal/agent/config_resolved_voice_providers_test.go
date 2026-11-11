package agent

import (
	"os"
	"path/filepath"
	"testing"
)

// LoadResolvedConfig backs `agent config --format=json`, which is what the config
// page reads through. It ran neither migration nor resolution, so for a flat
// config it emitted fields and no records: the page showed a provider select
// with no card, leaving the key invisible and un-editable.
//
// Migration has to run here, not only in LoadRuntimeConfig, because both GET and
// POST read the stored config through this CLI path.
func TestLoadResolvedConfigMigratesFlatVoiceCredentials(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	body := `
[model_settings.model]
provider = "openai"
model = "gpt-4o"
api_key = "sk-model"

[voice_settings.classic.tts]
provider = "minimax-cn"
api_key = "sk-minimax"
voice_id = "male-qn-qingse"
speed = 1.3

[voice_settings.classic.stt]
provider = "tencent-asr"
app_id = "1234"
secret_key = "secret-yyy"
language = "zh"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadResolvedConfig(path)
	if err != nil {
		t.Fatalf("LoadResolvedConfig: %v", err)
	}

	// The flat credential set becomes a record too.
	active, ok := cfg.TTSProviders["minimax-cn"]
	if !ok {
		t.Fatalf("expected a minimax-cn record, got %v", recordNames(cfg.TTSProviders))
	}
	if active.APIKey != "sk-minimax" {
		t.Errorf("minimax-cn record api_key = %q, want %q", active.APIKey, "sk-minimax")
	}
	if active.VoiceID != "male-qn-qingse" {
		t.Errorf("minimax-cn record voice_id = %q, want %q", active.VoiceID, "male-qn-qingse")
	}

	stt, ok := cfg.STTProviders["tencent-asr"]
	if !ok {
		t.Fatalf("expected a tencent-asr record, got %v", sttRecordNames(cfg.STTProviders))
	}
	if stt.AppID != "1234" || stt.SecretKey != "secret-yyy" {
		t.Errorf("tencent record = %+v, want the flat credentials", stt)
	}

	// The reference stays unresolved: the config page edits the reference, so it
	// must come back as a name, not as the resolved provider type.
	if cfg.TTS.Provider != "minimax-cn" {
		t.Errorf("tts.provider = %q, want the ref %q", cfg.TTS.Provider, "minimax-cn")
	}

	// Exactly one editor per credential. Leaving the flat copy in place would
	// give the page two fields for one key that disagree the moment either is
	// edited, and would write both shapes back to agent.toml.
	if cfg.TTS.APIKey != "" {
		t.Errorf("tts.api_key = %q, want cleared after migrating onto the record", cfg.TTS.APIKey)
	}
	if cfg.TTS.VoiceID != "" {
		t.Errorf("tts.voice_id = %q, want cleared", cfg.TTS.VoiceID)
	}
	if cfg.STT.AppID != "" || cfg.STT.SecretKey != "" {
		t.Errorf("flat stt credentials not cleared: %+v", cfg.STT)
	}

	// The global settings stay flat.
	if cfg.TTS.Speed != 1.3 {
		t.Errorf("tts.speed = %v, want 1.3 to stay global", cfg.TTS.Speed)
	}
	if cfg.STT.Language != "zh" {
		t.Errorf("stt.language = %q, want %q to stay global", cfg.STT.Language, "zh")
	}
}

func TestLoadRuntimeConfigKeepsBackendAndResolvesBenchmarkForegroundProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	body := `
[model_settings.model]
provider = "openai"
model = "fixed-backend-model"
api_key = "backend-key"

[voice_settings.mode]
input_mode = "realtime"

[voice_settings.realtime]
provider = "benchmark-foreground"
use_backend_agent = true

[voice_settings.realtime.providers.benchmark-foreground]
type = "openai"
model = "realtime-model"
api_key = "realtime-key"
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	if cfg.Model.Provider != "openai" || cfg.Model.Model != "fixed-backend-model" || cfg.Model.APIKey != "backend-key" {
		t.Fatalf("backend model changed: %+v", cfg.Model)
	}
	if cfg.InputModeOrDefault() != "realtime" || !cfg.VoiceModel.UseBackendAgent || cfg.VoiceModel.Provider != "openai" || cfg.VoiceModel.ActiveProviderRecord != "benchmark-foreground" {
		t.Fatalf("realtime delegation was not resolved: mode=%s voice=%+v", cfg.InputModeOrDefault(), cfg.VoiceModel)
	}
	record, ok := cfg.VoiceModelProviders["benchmark-foreground"]
	if !ok || record.Type != "openai" || record.Model != "realtime-model" || record.APIKey != "realtime-key" {
		t.Fatalf("foreground provider record = %+v, present=%v", record, ok)
	}
}

func TestLoadResolvedConfigMigratesLegacyQwenTurnDetectionToProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	body := `
[voice_settings.realtime]
provider = "qwen"
api_key = "qwen-secret"
turn_detection = "smart_turn"
turn_detection_threshold = 0.35
turn_detection_silence_ms = 900
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	resolved, err := LoadResolvedConfig(path)
	if err != nil {
		t.Fatalf("LoadResolvedConfig: %v", err)
	}
	record, ok := resolved.VoiceModelProviders["qwen"]
	if !ok {
		t.Fatalf("Qwen provider record was not created: %+v", resolved.VoiceModelProviders)
	}
	if record.TurnDetection != "smart_turn" || record.TurnDetectionThreshold == nil || *record.TurnDetectionThreshold != 0.35 || record.TurnDetectionSilenceMs != 900 {
		t.Fatalf("Qwen turn detection was not migrated: %+v", record)
	}
	if resolved.VoiceModel.TurnDetection != "" || resolved.VoiceModel.TurnDetectionThreshold != nil || resolved.VoiceModel.TurnDetectionSilenceMs != 0 {
		t.Fatalf("legacy flat turn detection fields were not cleared: %+v", resolved.VoiceModel)
	}

	runtime, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig: %v", err)
	}
	if runtime.VoiceModel.Provider != "qwen" || runtime.VoiceModel.TurnDetection != "smart_turn" || runtime.VoiceModel.TurnDetectionThreshold == nil || *runtime.VoiceModel.TurnDetectionThreshold != 0.35 || runtime.VoiceModel.TurnDetectionSilenceMs != 900 {
		t.Fatalf("runtime Qwen turn detection was not resolved: %+v", runtime.VoiceModel)
	}
}

// A config that never configured voice must not gain a record. DefaultConfig
// carries a TTS provider, so migrating without a metadata gate would mint a
// minimax-cn card for every device that only ever set up a model.
func TestLoadResolvedConfigInventsNoVoiceRecord(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	body := `
[model_settings.model]
provider = "openai"
model = "gpt-4o"
api_key = "sk-model"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadResolvedConfig(path)
	if err != nil {
		t.Fatalf("LoadResolvedConfig: %v", err)
	}

	if len(cfg.TTSProviders) != 0 {
		t.Errorf("len(tts_providers) = %d, want 0: %v", len(cfg.TTSProviders), recordNames(cfg.TTSProviders))
	}
	if len(cfg.STTProviders) != 0 {
		t.Errorf("len(stt_providers) = %d, want 0: %v", len(cfg.STTProviders), sttRecordNames(cfg.STTProviders))
	}
}

// A config already using records passes through untouched.
func TestLoadResolvedConfigLeavesRecordsAlone(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	body := `
[model_settings.model]
provider = "openai"
model = "gpt-4o"
api_key = "sk-model"

[voice_settings.classic.tts.providers.fish-main]
type = "fish-audio"
api_key = "sk-fish"

[voice_settings.classic.tts]
provider = "fish-main"
speed = 1.0
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadResolvedConfig(path)
	if err != nil {
		t.Fatalf("LoadResolvedConfig: %v", err)
	}

	if len(cfg.TTSProviders) != 1 {
		t.Errorf("len(tts_providers) = %d, want 1: %v", len(cfg.TTSProviders), recordNames(cfg.TTSProviders))
	}
	if cfg.TTSProviders["fish-main"].APIKey != "sk-fish" {
		t.Errorf("record api_key = %q, want %q", cfg.TTSProviders["fish-main"].APIKey, "sk-fish")
	}
	if cfg.TTS.Provider != "fish-main" {
		t.Errorf("tts.provider = %q, want the ref %q", cfg.TTS.Provider, "fish-main")
	}
}

func TestLoadResolvedConfigMigratesMixedVoiceCredentialsToReferencedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	body := `
[model_settings.model]
provider = "openai"
model = "gpt-4o"
api_key = "sk-model"

[voice_settings.classic.tts.providers.voice]
type = "minimax-cn"
api_key = "$TTS_API_KEY"
voice_id = "record-voice"
emotion = "record-emotion"

[voice_settings.classic.stt.providers.speech]
type = "tencent-asr"
api_key = "$STT_API_KEY"
model = "record-model"
app_id = "record-app"
secret_id = "record-id"
secret_key = "$STT_SECRET_KEY"

[voice_settings.classic.tts]
provider = "voice"
api_key = "flat-tts-key"
voice_id = "flat-voice"

[voice_settings.classic.stt]
provider = "speech"
api_key = "flat-stt-key"
app_id = "flat-app"
secret_id = ""
secret_key = "flat-secret-key"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadResolvedConfig(path)
	if err != nil {
		t.Fatalf("LoadResolvedConfig: %v", err)
	}

	if got := cfg.TTSProviders["voice"].APIKey; got != "flat-tts-key" {
		t.Errorf("tts record api_key = %q, want flat override", got)
	}
	if got := cfg.TTSProviders["voice"].VoiceID; got != "flat-voice" {
		t.Errorf("tts record voice_id = %q, want flat override", got)
	}
	if got := cfg.TTSProviders["voice"].Emotion; got != "record-emotion" {
		t.Errorf("tts record emotion = %q, want existing value rather than editor default", got)
	}
	stt := cfg.STTProviders["speech"]
	if stt.APIKey != "flat-stt-key" {
		t.Errorf("stt record api_key = %q, want flat override", stt.APIKey)
	}
	if stt.SecretID != "record-id" {
		t.Errorf("stt record secret_id = %q, want existing value preserved by empty flat field", stt.SecretID)
	}
	if stt.SecretKey != "flat-secret-key" {
		t.Errorf("stt record secret_key = %q, want flat override", stt.SecretKey)
	}
	if stt.Model != "record-model" {
		t.Errorf("stt record model = %q, want existing value rather than editor default", stt.Model)
	}
	if stt.AppID != "flat-app" {
		t.Errorf("stt record app_id = %q, want flat override", stt.AppID)
	}
	if cfg.TTS.APIKey != "" {
		t.Errorf("tts.api_key = %q, want cleared", cfg.TTS.APIKey)
	}
	if cfg.TTS.Model != "" || cfg.TTS.VoiceID != "" || cfg.TTS.Emotion != "" || cfg.TTS.ReferenceID != "" {
		t.Errorf("flat tts provider fields not cleared: %+v", cfg.TTS)
	}
	if cfg.STT.APIKey != "" || cfg.STT.Model != "" || cfg.STT.AppID != "" ||
		cfg.STT.SecretID != "" || cfg.STT.SecretKey != "" {
		t.Errorf("flat stt credentials not cleared: %+v", cfg.STT)
	}
}

// The editor view backs `agent config --format=json`, which is what the config
// page renders. Optional speech providers are opt-in at runtime, so a file that
// never declared one must come back empty: showing the DefaultConfig() provider
// would display (and label as required) a value Config Web itself reports as
// missing, and re-saving that displayed value could not repair the file.
func TestLoadResolvedConfigDoesNotInheritOptionalSpeechProviders(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	body := `
[model_settings.model]
provider = "openai"
model = "gpt-4o"

[voice_settings.mode]
input_mode = "stt"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadResolvedConfigForUpdate(path)
	if err != nil {
		t.Fatalf("LoadResolvedConfigForUpdate: %v", err)
	}
	if cfg.STT.Provider != "" || cfg.TTS.Provider != "" {
		t.Fatalf("editor view inherited providers the file never declared: stt=%q tts=%q", cfg.STT.Provider, cfg.TTS.Provider)
	}

	// config-check reads the same view, so it has to reject the file the page flags
	// rather than filling the gap with a default the runtime will not use.
	if _, err := LoadResolvedConfig(path); err == nil {
		t.Fatal("LoadResolvedConfig accepted declared stt mode without an stt provider")
	}
}

// The rule above must not remove a provider the file does declare.
func TestLoadResolvedConfigKeepsDeclaredSpeechProvider(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	body := `
[model_settings.model]
provider = "openai"
model = "gpt-4o"

[voice_settings.classic.stt]
provider = "tencent-asr"
app_id = "1234"
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadResolvedConfigForUpdate(path)
	if err != nil {
		t.Fatalf("LoadResolvedConfigForUpdate: %v", err)
	}
	if cfg.STT.Provider != "tencent-asr" {
		t.Fatalf("declared stt provider was dropped: %+v", cfg.STT)
	}
	// Its flat credentials become a record, as they do in LoadRuntimeConfig.
	if cfg.STTProviders["tencent-asr"].AppID != "1234" {
		t.Fatalf("declared stt credentials were dropped: %+v", cfg.STTProviders)
	}
	if cfg.TTS.Provider != "" {
		t.Fatalf("undeclared tts provider appeared: %q", cfg.TTS.Provider)
	}
}

// The opt-in rule keys off what the file declared, so it must not fire for a file
// that is not there at all. An absent agent.toml resolves to the built-in
// defaults: that is what the page renders so a save can create the file, and
// clearing the defaults instead would report a device that has simply never been
// configured as carrying an invalid configuration.
func TestLoadResolvedConfigKeepsDefaultsWhenFileIsAbsent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")

	cfg, err := LoadResolvedConfigForUpdate(path)
	if err != nil {
		t.Fatalf("LoadResolvedConfigForUpdate(absent): %v", err)
	}
	defaults := DefaultConfig()
	if cfg.TTS.Provider != defaults.TTS.Provider {
		t.Errorf("tts.provider = %q, want the built-in default %q", cfg.TTS.Provider, defaults.TTS.Provider)
	}
	if cfg.STT.Provider != defaults.STT.Provider {
		t.Errorf("stt.provider = %q, want the built-in default %q", cfg.STT.Provider, defaults.STT.Provider)
	}

	// config-check reads this same view, so the absent-file state has to pass it.
	if _, err := LoadResolvedConfig(path); err != nil {
		t.Fatalf("LoadResolvedConfig(absent) reported a device with no config file as invalid: %v", err)
	}
}
