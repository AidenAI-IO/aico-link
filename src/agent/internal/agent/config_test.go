package agent

import (
	"fmt"
	"math"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConfigValidateAcceptsSTT(t *testing.T) {
	cfg := Config{
		Model:     ModelConfig{Provider: "fake"},
		TTS:       TTSConfig{Provider: "minimax-cn"},
		STT:       STTConfig{Provider: "openai-whisper"},
		InputMode: " stt ",
	}

	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got := cfg.InputModeOrDefault(); got != "stt" {
		t.Fatalf("InputModeOrDefault() = %q, want stt", got)
	}
}

func TestConfigValidateAcceptsRealtimeWithoutSTTProvider(t *testing.T) {
	cfg := Config{
		Model:      ModelConfig{Provider: "fake"},
		VoiceModel: VoiceModelConfig{APIKey: "voice-secret"},
		InputMode:  " realtime ",
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got := cfg.InputModeOrDefault(); got != "realtime" {
		t.Fatalf("InputModeOrDefault() = %q, want realtime", got)
	}
}

func TestConfigValidateRejectsRealtimeWithoutVoiceModelCredential(t *testing.T) {
	err := (Config{Model: ModelConfig{Provider: "fake"}, InputMode: "realtime"}).Validate()
	if err == nil || !strings.Contains(err.Error(), "voice_model.api_key is required") {
		t.Fatalf("Validate() error = %v, want missing voice model credential", err)
	}
}

func TestVoiceModelConfigRejectsNonLoopbackPlaintextEndpoint(t *testing.T) {
	for _, endpoint := range []string{"ws://example.com/realtime", "http://localhost/realtime", "not-a-url"} {
		if err := (VoiceModelConfig{APIKey: "key", Endpoint: endpoint}).Validate(); err == nil {
			t.Errorf("Validate(%q) succeeded, want error", endpoint)
		}
	}
	for _, endpoint := range []string{"wss://example.com/realtime", "ws://localhost/realtime", "ws://127.0.0.1:3000/realtime", "ws://[::1]:3000/realtime"} {
		if err := (VoiceModelConfig{APIKey: "key", Endpoint: endpoint}).Validate(); err != nil {
			t.Errorf("Validate(%q) error = %v", endpoint, err)
		}
	}
}

func TestVoiceModelConfigDoesNotStrictlyValidateRealtimeProtocolAtBoot(t *testing.T) {
	for _, cfg := range []VoiceModelConfig{
		{Provider: "openai", APIKey: "key", RealtimeProtocol: "v2"},
		{Provider: "gemini", APIKey: "key", RealtimeProtocol: "legacy"},
	} {
		if err := cfg.Validate(); err != nil {
			t.Fatalf("voice protocol compatibility should be validated on config save, not boot: %v", err)
		}
	}
}

func TestVoiceModelConfigValidatesRealtimeProvider(t *testing.T) {
	if err := (VoiceModelConfig{Provider: "unknown", APIKey: "key"}).Validate(); err == nil {
		t.Fatal("expected unsupported realtime provider error")
	}
	if err := (VoiceModelConfig{Provider: "speko", APIKey: "key"}).Validate(); err == nil || !strings.Contains(err.Error(), "automatic routing is disabled") {
		t.Fatalf("Speko auto-routing config accepted: %v", err)
	}
	if err := (VoiceModelConfig{Provider: "speko", UpstreamProvider: "xai", Model: "grok-voice-latest", APIKey: "key", BaseURL: "http://localhost:8080"}).Validate(); err != nil {
		t.Fatalf("valid Speko config rejected: %v", err)
	}
	if err := (VoiceModelConfig{Provider: "speko", UpstreamProvider: "openai", Model: "gpt-realtime", APIKey: "key"}).Validate(); err == nil {
		t.Fatal("Speko OpenAI route must be rejected")
	}
	if err := (VoiceModelConfig{Provider: "speko", UpstreamProvider: "inworld", Model: "model", APIKey: "key"}).Validate(); err == nil || !strings.Contains(err.Error(), "unsupported provider") {
		t.Fatalf("unsupported Speko upstream accepted: %v", err)
	}
	if err := (VoiceModelConfig{Provider: "speko", UpstreamProvider: "gemini", Model: "gemini-live", APIKey: "key"}).Validate(); err != nil {
		t.Fatalf("Gemini Speko alias rejected: %v", err)
	}
	if err := (VoiceModelConfig{Provider: "speko", UpstreamProvider: "google", APIKey: "key"}).Validate(); err == nil || !strings.Contains(err.Error(), "are required") {
		t.Fatalf("partial Speko routing config accepted: %v", err)
	}
	for _, provider := range []string{"openai", "gemini", "xai"} {
		t.Run(provider, func(t *testing.T) {
			if err := (VoiceModelConfig{Provider: provider, APIKey: "key"}).Validate(); err != nil {
				t.Fatalf("valid native realtime config rejected: %v", err)
			}
		})
	}
}

func TestVoiceModelConfigValidatesGeminiVertexAuth(t *testing.T) {
	valid := VoiceModelConfig{Provider: "gemini", APIKey: "oauth-token", AuthMode: "vertex", ProjectID: "project-1", Location: "us-central1"}
	if err := valid.Validate(); err != nil {
		t.Fatalf("valid Vertex config rejected: %v", err)
	}
	for _, config := range []VoiceModelConfig{
		{Provider: "gemini", APIKey: "oauth-token", AuthMode: "vertex", Location: "us-central1"},
		{Provider: "gemini", APIKey: "oauth-token", AuthMode: "vertex", ProjectID: "project-1"},
		{Provider: "gemini", APIKey: "key", AuthMode: "unknown"},
	} {
		if err := config.Validate(); err == nil {
			t.Fatalf("invalid Vertex config accepted: %+v", config)
		}
	}
}

func TestVoiceModelConfigProviderFieldsLoad(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(`[voice_settings.mode]
input_mode = "realtime"

[voice_settings.realtime]
provider = "speko"
upstream_provider = "xai"
agent_id = "agent-1"
api_key = "secret"
model = "grok-voice-latest"
base_url = "https://api.speko.dev"
`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.VoiceModel.Provider != "speko" || cfg.VoiceModel.UpstreamProvider != "xai" || cfg.VoiceModel.AgentID != "agent-1" || cfg.VoiceModel.BaseURL != "https://api.speko.dev" {
		t.Fatalf("voice model = %+v", cfg.VoiceModel)
	}
}

func TestSpekoVoiceModelDoesNotInheritQwenDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(`[voice_settings.mode]
input_mode = "realtime"

[voice_settings.realtime]
provider = "speko"
api_key = "secret"
upstream_provider = "google"
model = "gemini-live"
turn_detection = "smart_turn"
turn_detection_threshold = 0.25
turn_detection_silence_ms = 900
`), 0o600); err != nil {
		t.Fatal(err)
	}

	runtime, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.VoiceModel.Model != "gemini-live" || runtime.VoiceModel.Voice != "" || runtime.VoiceModel.Region != "" || runtime.VoiceModel.TurnDetection != "" || runtime.VoiceModel.TurnDetectionThreshold != nil || runtime.VoiceModel.TurnDetectionSilenceMs != 0 {
		t.Fatalf("runtime Speko inherited Qwen defaults: %+v", runtime.VoiceModel)
	}

	resolved, err := LoadResolvedConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	record := resolved.VoiceModelProviders["speko"]
	if record.Model != "gemini-live" || record.Voice != "" || record.Region != "" || record.TurnDetection != "" || record.TurnDetectionThreshold != nil || record.TurnDetectionSilenceMs != 0 || resolved.VoiceModel.Model != "" {
		t.Fatalf("resolved Speko inherited Qwen defaults: selector=%+v record=%+v", resolved.VoiceModel, record)
	}
}

func TestVoiceModelProviderRecordsPreserveSettingsAcrossSwitches(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(`[voice_settings.mode]
input_mode = "realtime"

[voice_settings.realtime.providers.qwen-main]
type = "qwen"
api_key = "qwen-secret"
model = "qwen-realtime"
voice = "longanqian"
region = "cn-beijing"

[voice_settings.realtime.providers.speko-main]
type = "speko"
api_key = "speko-secret"
upstream_provider = "xai"
model = "grok-voice-latest"
voice = "eve"
base_url = "https://api.speko.dev"

[voice_settings.realtime.providers.openai-gateway]
type = "openai"
api_key = "openai-secret"
model = "gpt-realtime-2"
endpoint = "wss://gateway.example/v1/realtime"
realtime_protocol = "legacy"
voice = "alloy"

[voice_settings.realtime]
provider = "speko-main"
`), 0o600); err != nil {
		t.Fatal(err)
	}

	resolved, err := LoadResolvedConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.VoiceModel.Provider != "speko-main" {
		t.Fatalf("resolved voice_model.provider = %q", resolved.VoiceModel.Provider)
	}
	if got := resolved.VoiceModelProviders["qwen-main"]; got.APIKey != "qwen-secret" || got.Model != "qwen-realtime" || got.Voice != "longanqian" {
		t.Fatalf("saved Qwen record changed: %+v", got)
	}
	if got := resolved.VoiceModelProviders["speko-main"]; got.APIKey != "speko-secret" || got.Model != "grok-voice-latest" || got.Voice != "eve" {
		t.Fatalf("saved Speko record changed: %+v", got)
	}
	if got := resolved.VoiceModelProviders["openai-gateway"]; got.RealtimeProtocol != "legacy" || got.Endpoint != "wss://gateway.example/v1/realtime" {
		t.Fatalf("saved OpenAI compatibility record changed: %+v", got)
	}

	runtime, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := runtime.VoiceModel; got.Provider != "speko" || got.APIKey != "speko-secret" || got.Model != "grok-voice-latest" || got.Voice != "eve" || got.UpstreamProvider != "xai" {
		t.Fatalf("runtime did not resolve selected Speko record: %+v", got)
	}
}

func TestConfigValidateRejectsRemovedAudioMode(t *testing.T) {
	cfg := Config{
		Model:     ModelConfig{Provider: "fake"},
		TTS:       TTSConfig{Provider: "minimax-cn"},
		InputMode: "audio",
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected invalid input_mode error for removed audio mode")
	}
	if !strings.Contains(err.Error(), "text and audio modes have been removed") {
		t.Fatalf("unexpected error: %v", err)
	}
}

// TestConfigInputModeDefaultContract pins the unconfigured contract: input_mode
// has no baked-in default, because both modes require providers the device may
// never have configured. The effective mode is inferred from those providers.
func TestConfigInputModeDefaultContract(t *testing.T) {
	const want = ""

	if got := DefaultConfig().InputMode; got != want {
		t.Fatalf("DefaultConfig().InputMode = %q, want %q", got, want)
	}
	if got := (Config{}).InputModeOrDefault(); got != want {
		t.Fatalf("Config{}.InputModeOrDefault() = %q, want %q", got, want)
	}

	var metaDefault any
	for _, section := range ConfigMeta().Sections {
		if section.Name != "agent" {
			continue
		}
		for _, field := range section.Fields {
			if field.Key == "input_mode" {
				metaDefault = field.Default
			}
		}
	}
	if metaDefault != want {
		t.Fatalf("ConfigMeta agent.input_mode default = %#v, want %q", metaDefault, want)
	}

	// The classic pair and a realtime credential each imply their own mode.
	classic := Config{STT: STTConfig{Provider: "openai-whisper"}, TTS: TTSConfig{Provider: "minimax-cn"}}
	if got := classic.InputModeOrDefault(); got != "stt" {
		t.Fatalf("configured classic pair InputModeOrDefault() = %q, want stt", got)
	}
	realtime := Config{VoiceModel: VoiceModelConfig{APIKey: "key"}}
	if got := realtime.InputModeOrDefault(); got != "realtime" {
		t.Fatalf("configured realtime InputModeOrDefault() = %q, want realtime", got)
	}
}

func TestLoadRuntimeConfigFallsBackFromUnconfiguredRealtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	config := `[voice_settings.mode]
input_mode = "realtime"

[model_settings.model]
provider = "fake"
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if got, want := cfg.InputModeOrDefault(), ""; got != want {
		t.Fatalf("InputModeOrDefault() = %q, want runtime fallback %q (voice unconfigured)", got, want)
	}
}

func TestHIDKeyboardLayoutDefaultsToQWERTY(t *testing.T) {
	if got := (HIDConfig{}).KeyboardLayoutOrDefault(); got != keyboardLayoutQWERTY {
		t.Fatalf("KeyboardLayoutOrDefault() = %q, want %q", got, keyboardLayoutQWERTY)
	}
	if got := DefaultConfig().HID.KeyboardLayout; got != keyboardLayoutQWERTY {
		t.Fatalf("DefaultConfig().HID.KeyboardLayout = %q, want %q", got, keyboardLayoutQWERTY)
	}
}

func TestConfigValidateKeyboardLayout(t *testing.T) {
	for _, layout := range []string{keyboardLayoutQWERTY, keyboardLayoutAZERTY, keyboardLayoutQWERTZ} {
		cfg := Config{Model: ModelConfig{Provider: "fake"}, HID: HIDConfig{KeyboardLayout: layout}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("Validate() rejected keyboard layout %q: %v", layout, err)
		}
	}

	cfg := Config{Model: ModelConfig{Provider: "fake"}, HID: HIDConfig{KeyboardLayout: "dvorak"}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "hid.keyboard_layout") {
		t.Fatalf("Validate() error = %v, want hid.keyboard_layout rejection", err)
	}
}

func TestConfigLocaleContract(t *testing.T) {
	if got := DefaultConfig().Locale; got != "en-US" {
		t.Fatalf("DefaultConfig().Locale = %q, want en-US", got)
	}
	if got := (Config{}).LocaleOrDefault(); got != "en-US" {
		t.Fatalf("Config{}.LocaleOrDefault() = %q, want en-US", got)
	}

	english := Config{Model: ModelConfig{Provider: "fake"}, Locale: "en-US"}
	if err := english.Validate(); err != nil {
		t.Fatalf("Validate(en-US) error = %v", err)
	}
	if got := english.LocaleOrDefault(); got != "en-US" {
		t.Fatalf("LocaleOrDefault() = %q, want en-US", got)
	}

	for _, locale := range []string{"fr-FR", "en-us"} {
		invalid := Config{Model: ModelConfig{Provider: "fake"}, Locale: locale}
		if err := invalid.Validate(); err == nil || !strings.Contains(err.Error(), "locale") {
			t.Fatalf("Validate(%s) error = %v, want locale rejection", locale, err)
		}
	}
}

func TestLoadRuntimeConfigParsesTerminationPolicyOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	contents := `
[model_settings.model]
provider = "fake"

[conversation_settings.termination_policy]
enabled = false
max_seconds = 12.5
repeat_action_limit = 7
same_result_limit = 8
screen_unchanged_limit = 9
soft_notice_stall_score = 10
restrict_tools_stall_score = 11
terminate_stall_score = 12
parse_failure_limit = 13
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	policy := cfg.TerminationPolicy
	if policy.Enabled == nil || *policy.Enabled {
		t.Fatalf("termination policy enabled = %#v, want false", policy.Enabled)
	}
	if policy.MaxSeconds != 12.5 || policy.RepeatActionLimit != 7 || policy.SameResultLimit != 8 ||
		policy.ScreenUnchangedLimit != 9 || policy.SoftNoticeStallScore != 10 ||
		policy.RestrictToolsStallScore != 11 || policy.TerminateStallScore != 12 ||
		policy.ParseFailureLimit != 13 {
		t.Fatalf("termination policy overrides = %#v", policy)
	}
}

func TestLoadRuntimeConfigIgnoresModelBaseURL(t *testing.T) {
	tests := []struct {
		name     string
		provider string
	}{
		{"openai", "openai"},
		{"OpenAI case insensitive", "OpenAI"},
		{"ollama", "ollama"},
		{"Ollama case insensitive", "Ollama"},
		{"anthropic", "anthropic"},
		{"Anthropic case insensitive", "Anthropic"},
		{"openrouter", "openrouter"},
		{"kimi", "kimi"},
		{"kimi-cn", "kimi-cn"},
		{"volcengine", "volcengine"},
		{"Volcengine case insensitive", "Volcengine"},
		{"fake", "fake"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.toml")
			contents := `
[model_settings.model]
provider = "` + tt.provider + `"
model = "some-model"
base_url = "https://gateway.example.com/v1"
`
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			cfg, err := LoadRuntimeConfig(path)
			if err != nil {
				t.Fatalf("LoadRuntimeConfig() error = %v", err)
			}
			if cfg.Model.BaseURL != "" {
				t.Errorf("model.base_url = %q, want empty", cfg.Model.BaseURL)
			}
		})
	}
}

func TestLoadRuntimeConfigResolvesModelTemperatureDefault(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		explSet  string // explicit temperature line, empty means unset
		want     *float64
	}{
		{
			name:  "kimi-k3 without explicit temperature pins model default",
			model: "kimi-k3",
			want:  floatPtr(1),
		},
		{
			name:    "explicit temperature overrides kimi-k3 model default",
			model:   "kimi-k3",
			explSet: "temperature = 0.5",
			want:    floatPtr(0.5),
		},
		{
			name:  "unknown model without explicit temperature uses fallback",
			model: "gpt-5.5",
			want:  floatPtr(defaultModelTemperature),
		},
		{
			name:    "explicit zero temperature is respected",
			model:   "kimi-k3",
			explSet: "temperature = 0.0",
			want:    floatPtr(0),
		},
		{
			// Google documents 1.0 as the Gemini 3 default and warns that a lower
			// value may cause looping or degraded reasoning, so the global 0.2
			// fallback must not reach these models.
			name:     "gemini 3 without explicit temperature pins documented default",
			provider: "gemini",
			model:    "gemini-3.8-flash",
			want:     floatPtr(1),
		},
		{
			name:     "explicit temperature overrides gemini 3 default",
			provider: "gemini",
			model:    "gemini-3.8-flash",
			explSet:  "temperature = 0.4",
			want:     floatPtr(0.4),
		},
		{
			name:     "gemini 2.5 without documented temperature stays unset",
			provider: "gemini",
			model:    "gemini-2.5-flash",
			want:     nil,
		},
		{
			name:     "unknown gemini model without documented temperature stays unset",
			provider: "gemini",
			model:    "gemini-future",
			want:     nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.toml")
			provider := tt.provider
			if provider == "" {
				provider = "openai"
			}
			contents := "[model_settings.model]\nprovider = \"" + provider + "\"\nmodel = \"" + tt.model + "\"\n" + tt.explSet + "\n"
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			cfg, err := LoadRuntimeConfig(path)
			if err != nil {
				t.Fatalf("LoadRuntimeConfig() error = %v", err)
			}
			if !floatPtrEqual(cfg.Model.Temperature, tt.want) {
				t.Errorf("model.temperature = %v, want %v", formatFloatPtr(cfg.Model.Temperature), formatFloatPtr(tt.want))
			}
		})
	}
}

func TestLoadRuntimeConfigNamedGeminiProviderLeavesUnknownTemperatureUnset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	contents := `[model_settings.model]
provider = "google-main"
model = "gemini-2.5-pro"

[model_settings.providers.google-main]
type = "gemini"
api_key = "test-key"
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if cfg.Model.Provider != "gemini" {
		t.Fatalf("model.provider = %q, want resolved gemini", cfg.Model.Provider)
	}
	if cfg.Model.Temperature != nil {
		t.Fatalf("model.temperature = %v, want nil", formatFloatPtr(cfg.Model.Temperature))
	}
}

func TestLoadResolvedConfigKeepsTemperatureUnset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	contents := "[model_settings.model]\nprovider = \"kimi\"\nmodel = \"kimi-k3\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadResolvedConfig(path)
	if err != nil {
		t.Fatalf("LoadResolvedConfig() error = %v", err)
	}
	if cfg.Model.Temperature != nil {
		t.Errorf("model.temperature = %v, want nil (LoadResolvedConfig keeps unset for editor)", formatFloatPtr(cfg.Model.Temperature))
	}
}

func TestLoadResolvedConfigRejectsDirectory(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "config.toml")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatalf("create config directory: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "agent.toml"), []byte("[model_settings.model]\nprovider = \"fake\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := LoadResolvedConfig(dir)
	if err == nil {
		t.Fatal("LoadResolvedConfig() error = nil, want directory rejection")
	}
	if !strings.Contains(err.Error(), "config path must be a regular file") {
		t.Fatalf("LoadResolvedConfig() error = %q, want clear regular-file requirement", err)
	}
}

func TestLoadResolvedConfigRejectsExistingNonTOMLFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "draft.json")
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	_, err := LoadResolvedConfig(path)
	if err == nil {
		t.Fatal("LoadResolvedConfig() error = nil, want TOML suffix rejection")
	}
	if !strings.Contains(err.Error(), "config path must end with .toml") {
		t.Fatalf("LoadResolvedConfig() error = %q, want consistent TOML suffix requirement", err)
	}
}

func TestLoadResolvedConfigAcceptsSymlinkToRegularFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "target.toml")
	if err := os.WriteFile(target, []byte("[model_settings.model]\nprovider = \"fake\"\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	path := filepath.Join(dir, "draft.toml")
	if err := os.Symlink(target, path); err != nil {
		t.Fatalf("create config symlink: %v", err)
	}

	cfg, err := LoadResolvedConfig(path)
	if err != nil {
		t.Fatalf("LoadResolvedConfig() error = %v", err)
	}
	if cfg.Model.Provider != "fake" {
		t.Fatalf("model.provider = %q, want fake", cfg.Model.Provider)
	}
}

func TestLoadResolvedConfigRejectsSpecialFile(t *testing.T) {
	dir, err := os.MkdirTemp("", "aico-config-")
	if err != nil {
		t.Fatalf("create short temp directory: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	path := filepath.Join(dir, "socket.toml")
	listener, err := net.Listen("unix", path)
	if err != nil {
		t.Fatalf("create Unix socket: %v", err)
	}
	defer listener.Close()

	_, err = LoadResolvedConfig(path)
	if err == nil {
		t.Fatal("LoadResolvedConfig() error = nil, want special-file rejection")
	}
	if !strings.Contains(err.Error(), "config path must be a regular file") {
		t.Fatalf("LoadResolvedConfig() error = %q, want clear regular-file requirement", err)
	}
}

func TestLoadResolvedConfigRejectsDanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "draft.toml")
	if err := os.Symlink(filepath.Join(dir, "missing.toml"), path); err != nil {
		t.Fatalf("create dangling config symlink: %v", err)
	}

	_, err := LoadResolvedConfig(path)
	if err == nil {
		t.Fatal("LoadResolvedConfig() error = nil, want dangling-symlink rejection")
	}
	if !strings.Contains(err.Error(), "config symlink target") {
		t.Fatalf("LoadResolvedConfig() error = %q, want clear dangling-symlink error", err)
	}
}

func TestLoadResolvedConfigMissingPathContract(t *testing.T) {
	t.Run("toml file may be missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "draft.toml")

		cfg, err := LoadResolvedConfig(path)
		if err != nil {
			t.Fatalf("LoadResolvedConfig() error = %v", err)
		}
		if cfg.HID.FrameSocket != DefaultConfig().HID.FrameSocket {
			t.Fatalf("HID frame_socket = %q, want default %q", cfg.HID.FrameSocket, DefaultConfig().HID.FrameSocket)
		}
	})

	for _, tt := range []struct {
		name string
		path func(string) string
	}{
		{
			name: "non-TOML file",
			path: func(dir string) string { return filepath.Join(dir, "draft.json") },
		},
		{
			name: "trailing slash",
			path: func(dir string) string { return filepath.Join(dir, "draft.toml") + string(os.PathSeparator) },
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			_, err := LoadResolvedConfig(tt.path(t.TempDir()))
			if err == nil {
				t.Fatal("LoadResolvedConfig() error = nil, want TOML file-path rejection")
			}
			if !strings.Contains(err.Error(), "config path must end with .toml") {
				t.Fatalf("LoadResolvedConfig() error = %q, want clear TOML file-path requirement", err)
			}
		})
	}
}

func TestLoadRuntimeConfigResolvesModelReasoningEffortDefault(t *testing.T) {
	tests := []struct {
		name     string
		provider string
		model    string
		explSet  string // explicit reasoning_effort line, empty means unset
		want     string
	}{
		{
			name:     "kimi-k3 without explicit reasoning_effort pins model default",
			provider: "openai",
			model:    "kimi-k3",
			want:     "low",
		},
		{
			name:     "explicit reasoning_effort overrides kimi-k3 model default",
			provider: "openai",
			model:    "kimi-k3",
			explSet:  `reasoning_effort = "high"`,
			want:     "high",
		},
		{
			name:     "doubao-seed-2.1-pro without explicit reasoning_effort pins model default",
			provider: "volcengine",
			model:    "doubao-seed-2-1-pro-260628",
			want:     "low",
		},
		{
			name:     "explicit minimal overrides doubao model default",
			provider: "volcengine",
			model:    "doubao-seed-2-1-pro-260628",
			explSet:  `reasoning_effort = "minimal"`,
			want:     "minimal",
		},
		{
			name:     "deepseek flash without explicit reasoning_effort disables thinking",
			provider: "deepseek",
			model:    "deepseek-flash",
			want:     "none",
		},
		{
			name:     "unknown model without explicit reasoning_effort stays auto (empty)",
			provider: "openai",
			model:    "gpt-5.5",
			want:     "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.toml")
			provider := tt.provider
			if provider == "" {
				provider = "openai"
			}
			contents := "[model_settings.model]\nprovider = \"" + provider + "\"\nmodel = \"" + tt.model + "\"\n" + tt.explSet + "\n"
			if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			cfg, err := LoadRuntimeConfig(path)
			if err != nil {
				t.Fatalf("LoadRuntimeConfig() error = %v", err)
			}
			if cfg.Model.ReasoningEffort != tt.want {
				t.Errorf("model.reasoning_effort = %q, want %q", cfg.Model.ReasoningEffort, tt.want)
			}
		})
	}
}

func TestLoadResolvedConfigKeepsReasoningEffortUnset(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	contents := "[model_settings.model]\nprovider = \"kimi\"\nmodel = \"kimi-k3\"\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadResolvedConfig(path)
	if err != nil {
		t.Fatalf("LoadResolvedConfig() error = %v", err)
	}
	if cfg.Model.ReasoningEffort != "" {
		t.Errorf("model.reasoning_effort = %q, want \"\" (LoadResolvedConfig keeps unset for editor)", cfg.Model.ReasoningEffort)
	}
}

func floatPtrEqual(a, b *float64) bool {
	if a == nil && b == nil {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return *a == *b
}

func formatFloatPtr(p *float64) string {
	if p == nil {
		return "nil"
	}
	return fmt.Sprintf("%v", *p)
}

func TestLoadRuntimeConfigParsesVoiceNotificationOverrides(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	contents := `
[model_settings.model]
provider = "fake"

[memory_settings.notification]
enabled = true
max_pending = 4

[memory_settings.notification.response_tail]
enabled = false
max_items = 1
max_text_chars = 64

[memory_settings.notification.expiration]
default_ttl_seconds = 30

[memory_settings.notification.expiration.code_ttl_seconds]
storage = 120
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	voice := cfg.VoiceNotifications
	if !voice.EnabledOrDefault() || voice.MaxPending != 4 {
		t.Fatalf("voice notification overrides = %#v", voice)
	}
	if voice.ResponseTail.EnabledOrDefault() || voice.ResponseTail.MaxItems != 1 || voice.ResponseTail.MaxTextChars != 64 {
		t.Fatalf("response tail overrides = %#v", voice.ResponseTail)
	}
	if voice.Expiration.DefaultTTLSeconds != 30 || voice.Expiration.CodeTTLSeconds["storage"] != 120 {
		t.Fatalf("expiration overrides = %#v", voice.Expiration)
	}
}

func TestConfigRejectsInvalidTerminationPolicyThresholdOrder(t *testing.T) {
	cfg := Config{
		Model: ModelConfig{Provider: "fake"},
		TerminationPolicy: TerminationPolicyConfig{
			SoftNoticeStallScore:    5,
			RestrictToolsStallScore: 4,
			TerminateStallScore:     6,
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "soft_notice < restrict_tools < terminate") {
		t.Fatalf("Validate() error = %v, want ordered termination-policy threshold error", err)
	}
}

func TestBundledSkillsDirCandidatesUseBusinessPackage(t *testing.T) {
	want := []string{"/usr/share/aico/skills"}
	if got := bundledSkillsDirCandidates(); !reflect.DeepEqual(got, want) {
		t.Fatalf("bundledSkillsDirCandidates() = %#v, want %#v", got, want)
	}
}

func TestConfigScreenshotPruningDefaultsAndOverrides(t *testing.T) {
	cfg := Config{Model: ModelConfig{Provider: "fake"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	pruning := cfg.ScreenshotPruningOrDefault()
	if pruning.KeepN != 3 || pruning.Interval != 2 {
		t.Fatalf("default screenshot pruning = %#v, want keep_n=3 interval=2", pruning)
	}

	cfg.ScreenshotKeepN = 5
	cfg.ScreenshotPruneInterval = 40
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with screenshot pruning overrides error = %v", err)
	}
	pruning = cfg.ScreenshotPruningOrDefault()
	if pruning.KeepN != 5 || pruning.Interval != 40 {
		t.Fatalf("configured screenshot pruning = %#v, want keep_n=5 interval=40", pruning)
	}
}

func TestConfigScreenStableDefaults(t *testing.T) {
	cfg := Config{
		ScreenStableTimeoutMs:     7000,
		ScreenStableMs:            800,
		ScreenStableDiffThreshold: 2.5,
	}
	defaults := cfg.ScreenStableDefaults().Resolved()
	if defaults.TimeoutMs != 7000 || defaults.StableMs != 800 || defaults.DiffThreshold != 2.5 {
		t.Fatalf("resolved defaults = %#v, want timeout=7000 stable=800 diff=2.5", defaults)
	}
}

func TestLoadConfigParsesModelSpecOverrides(t *testing.T) {
	configDir := t.TempDir()
	config := `
[conversation_settings.agent]
prompt = "test"

[model_settings.model]
provider = "openrouter"
model = "vendor/test-model"
max_response_tokens = 1000
context_window = 64000
model_max_output_tokens = 4096
`
	if err := os.WriteFile(configDir+"/agent.toml", []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigFromDir(configDir)
	if err != nil {
		t.Fatalf("LoadConfigFromDir() error = %v", err)
	}
	if cfg.Model.ContextWindow != 64_000 {
		t.Errorf("ContextWindow = %d, want 64_000", cfg.Model.ContextWindow)
	}
	if cfg.Model.MaxResponseTokens != 1_000 {
		t.Errorf("MaxResponseTokens = %d, want 1_000", cfg.Model.MaxResponseTokens)
	}
	if cfg.Model.ModelMaxOutputTokens != 4_096 {
		t.Errorf("ModelMaxOutputTokens = %d, want 4_096", cfg.Model.ModelMaxOutputTokens)
	}
}

func TestLoadConfigParsesLegacyModelMaxTokens(t *testing.T) {
	configDir := t.TempDir()
	config := `
[conversation_settings.agent]
prompt = "test"

[model_settings.model]
provider = "openrouter"
model = "vendor/test-model"
max_tokens = 777
`
	if err := os.WriteFile(configDir+"/agent.toml", []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigFromDir(configDir)
	if err != nil {
		t.Fatalf("LoadConfigFromDir() error = %v", err)
	}
	if cfg.Model.MaxResponseTokens != 777 {
		t.Errorf("MaxResponseTokens = %d, want legacy max_tokens value 777", cfg.Model.MaxResponseTokens)
	}
}

func TestLoadConfigPrefersMaxResponseTokensOverLegacyMaxTokens(t *testing.T) {
	configDir := t.TempDir()
	config := `
[conversation_settings.agent]
prompt = "test"

[model_settings.model]
provider = "openrouter"
model = "vendor/test-model"
max_response_tokens = 1000
max_tokens = 777
`
	if err := os.WriteFile(configDir+"/agent.toml", []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadConfigFromDir(configDir)
	if err != nil {
		t.Fatalf("LoadConfigFromDir() error = %v", err)
	}
	if cfg.Model.MaxResponseTokens != 1_000 {
		t.Errorf("MaxResponseTokens = %d, want canonical max_response_tokens value 1_000", cfg.Model.MaxResponseTokens)
	}
}

func TestOverrideDeviceTypeDerivesRuntimeDeviceSettings(t *testing.T) {
	tests := []struct {
		input       string
		deviceType  string
		pointerMode string
	}{
		{input: "android", deviceType: "Android", pointerMode: "touchscreen"},
		{input: "mac", deviceType: "macOS", pointerMode: "absolute"},
		{input: "windows", deviceType: "windows", pointerMode: "absolute"},
		{input: "linux", deviceType: "linux", pointerMode: "absolute"},
	}
	for _, tt := range tests {
		t.Run(tt.input, func(t *testing.T) {
			var cfg Config
			if err := cfg.OverrideDeviceType(tt.input); err != nil {
				t.Fatalf("OverrideDeviceType() error = %v", err)
			}
			if cfg.Device.DeviceType != tt.deviceType {
				t.Fatalf("device type = %q, want %q", cfg.Device.DeviceType, tt.deviceType)
			}
			if cfg.HID.PointerMode != tt.pointerMode {
				t.Fatalf("pointer mode = %q, want %q", cfg.HID.PointerMode, tt.pointerMode)
			}
		})
	}
}

func TestLoadRuntimeConfigFromDirAppliesRuntimeDefaultsWithoutActivatingSpeech(t *testing.T) {
	configDir := t.TempDir()
	config := `
[model_settings.model]
provider = "fake"
`
	if err := os.WriteFile(filepath.Join(configDir, "agent.toml"), []byte(config), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfigFromDir(configDir)
	if err != nil {
		t.Fatalf("LoadRuntimeConfigFromDir() error = %v", err)
	}

	if cfg.InputMode != defaultInputMode {
		t.Fatalf("InputMode = %q, want runtime default %q", cfg.InputMode, defaultInputMode)
	}
	if cfg.Audio.Socket != defaultAudioSocket {
		t.Fatalf("Audio.Socket = %q, want runtime default %q", cfg.Audio.Socket, defaultAudioSocket)
	}
	if cfg.HID.FrameSocket != defaultFrameServiceSocket {
		t.Fatalf("HID.FrameSocket = %q, want runtime default %q", cfg.HID.FrameSocket, defaultFrameServiceSocket)
	}
	if cfg.VoiceMaxResponseTokens != defaultVoiceMaxResponseTokens {
		t.Fatalf("VoiceMaxResponseTokens = %d, want runtime default %d",
			cfg.VoiceMaxResponseTokens, defaultVoiceMaxResponseTokens)
	}
	if !cfg.Model.LogRawHTTP {
		t.Fatal("Model.LogRawHTTP = false, want runtime default true")
	}
	if cfg.Log.LLMHTTPRetentionDays != defaultLLMHTTPLogRetentionDays {
		t.Fatalf("Log.LLMHTTPRetentionDays = %d, want runtime default %d",
			cfg.Log.LLMHTTPRetentionDays, defaultLLMHTTPLogRetentionDays)
	}
	if cfg.ScreenStableTimeoutMs != defaultStableWaitTimeoutMs {
		t.Fatalf("ScreenStableTimeoutMs = %d, want runtime default %d",
			cfg.ScreenStableTimeoutMs, defaultStableWaitTimeoutMs)
	}
	if cfg.TTS.Provider != "" {
		t.Fatalf("TTS.Provider = %q, want empty when text mode did not configure TTS", cfg.TTS.Provider)
	}
	if cfg.STT.Provider != "" {
		t.Fatalf("STT.Provider = %q, want empty when text mode did not configure STT", cfg.STT.Provider)
	}
}

func TestLoadRuntimeConfigCanDisableRawHTTPLogging(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(`
[model_settings.model]
provider = "fake"
log_raw_http = false
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if cfg.Model.LogRawHTTP {
		t.Fatal("Model.LogRawHTTP = true, want explicit false to disable raw HTTP logging")
	}
}

func TestLoadRuntimeConfigCanOverrideLLMHTTPRetentionDays(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(`
[model_settings.model]
provider = "fake"

[advanced_settings.log]
llm_http_retention_days = 14
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if cfg.Log.LLMHTTPRetentionDaysOrDefault() != 14 {
		t.Fatalf("LLMHTTPRetentionDaysOrDefault() = %d, want 14", cfg.Log.LLMHTTPRetentionDaysOrDefault())
	}
}

func TestLoadRuntimeConfigCanOverrideLogLevel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(`
[model_settings.model]
provider = "fake"

[advanced_settings.log]
level = "warn"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if cfg.Log.LevelOrDefault() != "warn" {
		t.Fatalf("Log.LevelOrDefault() = %q, want warn", cfg.Log.LevelOrDefault())
	}
}

func TestConfigRejectsInvalidLogLevel(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Log.Level = "verbose"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "log.level") {
		t.Fatalf("Validate() error = %v, want log.level rejection", err)
	}
}

func TestLoadRuntimeConfigCanOverrideContextCompactionThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(`
[conversation_settings.agent]
context_compaction_threshold = 0.6

[model_settings.model]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	got := cfg.ContextCompactionThresholdOrDefault()
	if got != 0.6 {
		t.Fatalf("ContextCompactionThresholdOrDefault() = %g, want 0.6", got)
	}
	usableInputBudget := 10_000
	trigger, enabled := conversationCompactionTrigger(usableInputBudget, got)
	if !enabled || trigger != 6_000 {
		t.Fatalf("conversationCompactionTrigger(10000, %g) = %d, %v; want 6000, true", got, trigger, enabled)
	}
}

func TestLoadRuntimeConfigMigratesLegacyAbsoluteContextPruneThreshold(t *testing.T) {
	// Devices configured before the field became a fraction hold an absolute
	// token count. TOML decodes it into the float64 field without error, so the
	// loader has to migrate it or the agent would fail Validate and not start.
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(`
[conversation_settings.agent]
context_prune_threshold = 12000

[model_settings.model]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() with legacy absolute threshold must still load, got error = %v", err)
	}
	if got := cfg.ContextPruneThresholdOrDefault(); got != defaultContextPruneThreshold {
		t.Fatalf("migrated prune threshold = %g, want default %g", got, defaultContextPruneThreshold)
	}
}

func TestLoadRuntimeConfigCanOverrideContextPruneThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	// Deliberately not defaultContextPruneThreshold: an override equal to the
	// default would pass this test even if the configured value were ignored.
	if err := os.WriteFile(path, []byte(`
[conversation_settings.agent]
context_prune_threshold = 0.4

[model_settings.model]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	got := cfg.ContextPruneThresholdOrDefault()
	if got != 0.4 {
		t.Fatalf("ContextPruneThresholdOrDefault() = %g, want 0.4", got)
	}
	trigger, target, enabled := historicalPruneBudgets(10_000, got)
	if !enabled || trigger != 4_000 {
		t.Fatalf("historicalPruneBudgets(10000, %g) = %d, %d, %v; want trigger 4000", got, trigger, target, enabled)
	}
}

func TestLoadRuntimeConfigCapsPruneThresholdAtCompactionThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(`
[conversation_settings.agent]
context_prune_threshold = 0.9
context_compaction_threshold = 0.6

[model_settings.model]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if got := cfg.ContextPruneThresholdOrDefault(); got != 0.6 {
		t.Fatalf("ContextPruneThresholdOrDefault() = %g, want capped 0.6", got)
	}
}

func TestLoadRuntimeConfigDefaultsContextCompactionThreshold(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(`
[model_settings.model]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if got := cfg.ContextCompactionThresholdOrDefault(); got != defaultContextCompactionThreshold {
		t.Fatalf("ContextCompactionThresholdOrDefault() = %g, want %g", got, defaultContextCompactionThreshold)
	}
}

func TestConfigValidateRejectsNegativeLLMHTTPRetentionDays(t *testing.T) {
	cfg := Config{
		Model: ModelConfig{Provider: "fake"},
		Log:   LogConfig{LLMHTTPRetentionDays: -1},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("Validate() error = nil, want log.llm_http_retention_days rejection")
	}
	if !strings.Contains(err.Error(), "log.llm_http_retention_days") {
		t.Fatalf("Validate() error = %v, want log.llm_http_retention_days", err)
	}
}

func TestLoadRuntimeConfigIgnoresLegacyInstructionField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(`
[conversation_settings.agent]
instruction = "legacy field should be ignored"

[model_settings.model]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if cfg.Instruction != defaultInstruction {
		t.Fatalf("Instruction = %q, want built-in default because legacy instruction is ignored", cfg.Instruction)
	}
}

// TestLoadRuntimeConfigIgnoresLegacyCustomInstruction covers the removed
// custom_instruction setting: an existing file that still carries the key must
// keep loading, but the value can no longer override the built-in instruction.
func TestLoadRuntimeConfigIgnoresLegacyCustomInstruction(t *testing.T) {
	for name, instruction := range map[string]string{
		"empty":     "",
		"non-empty": "Use a deployment-specific persona.",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent.toml")
			if err := os.WriteFile(path, []byte(`
[conversation_settings.agent]
custom_instruction = `+strconv.Quote(instruction)+`

[model_settings.model]
provider = "fake"
`), 0o644); err != nil {
				t.Fatalf("write config: %v", err)
			}

			cfg, err := LoadRuntimeConfig(path)
			if err != nil {
				t.Fatalf("LoadRuntimeConfig() error = %v", err)
			}
			if cfg.Instruction != defaultInstruction {
				t.Fatalf("Instruction = %q, want built-in default because legacy custom_instruction is ignored", cfg.Instruction)
			}

			strictCfg, err := LoadConfig(path)
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			if strictCfg.Instruction != defaultInstruction {
				t.Fatalf("strict Instruction = %q, want built-in default because legacy custom_instruction is ignored", strictCfg.Instruction)
			}
		})
	}
}

// TestLoadRuntimeConfigAppendsPrompt verifies the remaining prompt
// setting: prompt is preserved and reaches the prompt builder after
// the built-in instruction.
func TestLoadRuntimeConfigAppendsPrompt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(`
[conversation_settings.agent]
prompt = "Always answer in bullet points."

[model_settings.model]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if cfg.Prompt != "Always answer in bullet points." {
		t.Fatalf("Prompt = %q, want the configured value", cfg.Prompt)
	}
	combined := combinedAgentInstruction(AgentConfig{Instruction: cfg.Instruction, Prompt: cfg.Prompt})
	want := defaultInstruction + "\n\n" + cfg.Prompt
	if combined != want {
		t.Fatalf("combinedAgentInstruction() = %q, want built-in instruction followed by prompt", combined)
	}
}

func TestLoadRuntimeConfigIgnoresLegacyAdditionalPrompt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(`
[conversation_settings.agent]
additional_prompt = "Legacy prompt spelling."

[model_settings.model]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if cfg.Prompt != "" {
		t.Fatalf("Prompt = %q, want legacy additional_prompt to be ignored", cfg.Prompt)
	}
}

func TestLoadRuntimeConfigKeepsSpeechProvidersOptIn(t *testing.T) {
	dir := t.TempDir()

	unconfiguredPath := filepath.Join(dir, "unconfigured.toml")
	if err := os.WriteFile(unconfiguredPath, []byte(`
[model_settings.model]
provider = "fake"

[voice_settings.classic.stt]

[voice_settings.classic.tts]
`), 0o644); err != nil {
		t.Fatalf("write unconfigured config: %v", err)
	}
	unconfigured, err := LoadRuntimeConfig(unconfiguredPath)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig(unconfigured) error = %v", err)
	}
	if unconfigured.STT.Provider != "" {
		t.Fatalf("unconfigured STT.Provider = %q, want empty", unconfigured.STT.Provider)
	}
	if unconfigured.TTS.Provider != "" {
		t.Fatalf("unconfigured TTS.Provider = %q, want empty", unconfigured.TTS.Provider)
	}

	keyOnlyPath := filepath.Join(dir, "key-only.toml")
	if err := os.WriteFile(keyOnlyPath, []byte(`
[model_settings.model]
provider = "fake"

[voice_settings.classic.stt]
api_key = "stt-key"

[voice_settings.classic.tts]
api_key = "tts-key"
`), 0o644); err != nil {
		t.Fatalf("write key-only config: %v", err)
	}
	keyOnly, err := LoadRuntimeConfig(keyOnlyPath)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig(key-only) error = %v", err)
	}
	if keyOnly.STT.Provider != "" {
		t.Fatalf("key-only STT.Provider = %q, want empty", keyOnly.STT.Provider)
	}
	if keyOnly.TTS.Provider != "" {
		t.Fatalf("key-only TTS.Provider = %q, want empty", keyOnly.TTS.Provider)
	}

	explicitPath := filepath.Join(dir, "explicit.toml")
	if err := os.WriteFile(explicitPath, []byte(`
[model_settings.model]
provider = "fake"

[voice_settings.classic.stt]
provider = "openai-whisper"
api_key = "stt-key"

[voice_settings.classic.tts]
provider = "minimax-ws"
api_key = "tts-key"
`), 0o644); err != nil {
		t.Fatalf("write explicit config: %v", err)
	}
	explicit, err := LoadRuntimeConfig(explicitPath)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig(explicit) error = %v", err)
	}
	if explicit.STT.Provider != defaultSTTProvider {
		t.Fatalf("explicit STT.Provider = %q, want default %q", explicit.STT.Provider, defaultSTTProvider)
	}
	if explicit.STT.Model != defaultSTTModel {
		t.Fatalf("explicit STT.Model = %q, want default %q", explicit.STT.Model, defaultSTTModel)
	}
	if explicit.TTS.Provider != defaultTTSProvider {
		t.Fatalf("explicit TTS.Provider = %q, want default %q", explicit.TTS.Provider, defaultTTSProvider)
	}
	if explicit.TTS.VoiceID != defaultTTSVoiceID {
		t.Fatalf("explicit TTS.VoiceID = %q, want default %q", explicit.TTS.VoiceID, defaultTTSVoiceID)
	}
}

func TestLoadRuntimeConfigDoesNotCarryDefaultSpeechFieldsAcrossProviders(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(`
[model_settings.model]
provider = "fake"

[voice_settings.classic.stt]
provider = "openrouter"
api_key = "stt-key"

[voice_settings.classic.tts]
provider = "volcengine"
api_key = "tts-key"
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if cfg.STT.Model != "" {
		t.Fatalf("STT.Model = %q, want empty so provider-specific defaults can apply", cfg.STT.Model)
	}
	if cfg.TTS.VoiceID != "" {
		t.Fatalf("TTS.VoiceID = %q, want empty so provider-specific defaults can apply", cfg.TTS.VoiceID)
	}
	if cfg.TTS.Emotion != "" {
		t.Fatalf("TTS.Emotion = %q, want empty so provider-specific defaults can apply", cfg.TTS.Emotion)
	}
	if cfg.TTS.Speed != 0 {
		t.Fatalf("TTS.Speed = %v, want zero so provider-specific defaults can apply", cfg.TTS.Speed)
	}
}

func TestLoadRuntimeConfigRejectsVoiceModeWithoutExplicitSpeechProviders(t *testing.T) {
	dir := t.TempDir()

	sttPath := filepath.Join(dir, "stt.toml")
	if err := os.WriteFile(sttPath, []byte(`
[voice_settings.mode]
input_mode = "stt"

[model_settings.model]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write stt config: %v", err)
	}
	_, err := LoadRuntimeConfig(sttPath)
	if err == nil || !strings.Contains(err.Error(), "stt.provider") {
		t.Fatalf("LoadRuntimeConfig(stt) error = %v, want stt.provider validation error", err)
	}

	audioPath := filepath.Join(dir, "audio.toml")
	if err := os.WriteFile(audioPath, []byte(`
[voice_settings.mode]
input_mode = "audio"

[model_settings.model]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write audio config: %v", err)
	}
	_, err = LoadRuntimeConfig(audioPath)
	if err == nil || !strings.Contains(err.Error(), "text and audio modes have been removed") {
		t.Fatalf("LoadRuntimeConfig(audio) error = %v, want removed audio/text mode error", err)
	}

	textPath := filepath.Join(dir, "text.toml")
	if err := os.WriteFile(textPath, []byte(`
[voice_settings.mode]
input_mode = "text"

[model_settings.model]
provider = "fake"
`), 0o644); err != nil {
		t.Fatalf("write text config: %v", err)
	}
	_, err = LoadRuntimeConfig(textPath)
	if err == nil || !strings.Contains(err.Error(), "text and audio modes have been removed") {
		t.Fatalf("LoadRuntimeConfig(text) error = %v, want removed audio/text mode error", err)
	}
}

func TestConfigValidateRejectsNegativeModelSpecOverrides(t *testing.T) {
	cfg := Config{Model: ModelConfig{Provider: "fake", MaxResponseTokens: -1}}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "model.max_response_tokens") {
		t.Fatalf("expected model.max_response_tokens validation error, got %v", err)
	}

	cfg = Config{Model: ModelConfig{Provider: "fake", ContextWindow: -1}}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "model.context_window") {
		t.Fatalf("expected model.context_window validation error, got %v", err)
	}

	cfg = Config{Model: ModelConfig{Provider: "fake", ModelMaxOutputTokens: -1}}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "model.model_max_output_tokens") {
		t.Fatalf("expected model.model_max_output_tokens validation error, got %v", err)
	}

}

func TestConfigValidateReasoningBudgetAgainstResponseLimit(t *testing.T) {
	tests := []struct {
		name    string
		model   ModelConfig
		wantErr string
	}{
		{
			name:    "below provider floor",
			model:   ModelConfig{Provider: "anthropic", Model: "claude-haiku-4-5", ReasoningBudgetTokens: 512, MaxResponseTokens: 8_192},
			wantErr: "model.reasoning_budget_tokens must be 0 or >= 1024",
		},
		{
			// A response limit smaller than the exact budget is invalid because
			// reasoning tokens are included in the response limit.
			// user hits first when enabling an exact budget.
			name:    "budget exceeds response limit",
			model:   ModelConfig{Provider: "anthropic", Model: "claude-haiku-4-5", ReasoningBudgetTokens: 4_096, MaxResponseTokens: 1_000},
			wantErr: "must be less than model.max_response_tokens",
		},
		{
			name:    "budget equals response limit",
			model:   ModelConfig{Provider: "anthropic", Model: "claude-haiku-4-5", ReasoningBudgetTokens: 4_096, MaxResponseTokens: 4_096},
			wantErr: "must be less than model.max_response_tokens",
		},
		{
			name:    "budget exceeds model output capability",
			model:   ModelConfig{Provider: "anthropic", Model: "claude-haiku-4-5", ReasoningBudgetTokens: 9_000, ModelMaxOutputTokens: 8_192, MaxResponseTokens: 64_000},
			wantErr: "must be less than model.model_max_output_tokens",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := Config{Model: tt.model}.Validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want an error containing %q", err, tt.wantErr)
			}
		})
	}

	valid := Config{Model: ModelConfig{Provider: "anthropic", Model: "claude-haiku-4-5", ReasoningBudgetTokens: 4_096, MaxResponseTokens: 16_384}}
	if err := valid.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want a valid budget/limit pair accepted", err)
	}
	unset := Config{Model: ModelConfig{Provider: "fake", MaxResponseTokens: 500}}
	if err := unset.Validate(); err != nil {
		t.Fatalf("Validate() = %v, want budget 0 to stay unconstrained", err)
	}
}

func TestConfigValidateReasoningBudgetForNamedAnthropicProvider(t *testing.T) {
	cfg := Config{
		ModelProviders: map[string]ModelProvider{
			"claude": {Type: "anthropic"},
		},
		Model: ModelConfig{
			Provider:              "claude",
			Model:                 "claude-haiku-4-5",
			ReasoningBudgetTokens: 512,
			MaxResponseTokens:     8_192,
		},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "model.reasoning_budget_tokens must be 0 or >= 1024") {
		t.Fatalf("Validate() = %v, want named Anthropic provider budget validation", err)
	}
}

func TestConfigValidateRejectsOutOfRangeContextPruneThreshold(t *testing.T) {
	// NaN and the infinities are included because TOML accepts those literals
	// into a float64 field, and NaN passes every ordered comparison.
	for _, threshold := range []float64{-0.1, 1, 1.5, math.NaN(), math.Inf(1), math.Inf(-1)} {
		cfg := Config{
			Model:                 ModelConfig{Provider: "fake"},
			ContextPruneThreshold: threshold,
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "context_prune_threshold") {
			t.Fatalf("threshold %g: expected context_prune_threshold validation error, got %v", threshold, err)
		}
	}

	cfg := Config{
		Model:                 ModelConfig{Provider: "fake"},
		ContextPruneThreshold: 0,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("threshold 0 must be accepted as automatic, got %v", err)
	}
}

// TestLoadRuntimeConfigRejectsNonFiniteContextThresholds covers the decode path
// rather than a hand-built Config: TOML's nan and inf literals decode into the
// float64 fields, so a hand-edited config can reach validation with values that
// no amount of arithmetic can make sensible. Neither may be silently migrated to
// the default, and neither may reach the budget helpers.
func TestLoadRuntimeConfigRejectsNonFiniteContextThresholds(t *testing.T) {
	for _, body := range []string{
		"context_prune_threshold = nan",
		"context_prune_threshold = inf",
		"context_prune_threshold = -inf",
		"context_compaction_threshold = nan",
		"context_compaction_threshold = inf",
		"context_compaction_threshold = -inf",
	} {
		dir := t.TempDir()
		path := filepath.Join(dir, "agent.toml")
		if err := os.WriteFile(path, []byte("\n[conversation_settings.agent]\n"+body+"\n\n[model_settings.model]\nprovider = \"fake\"\n"), 0o644); err != nil {
			t.Fatalf("write config: %v", err)
		}
		if _, err := LoadRuntimeConfig(path); err == nil {
			t.Fatalf("%s: LoadRuntimeConfig() must reject a non-finite threshold", body)
		}
	}
}

func TestContextPruneThresholdOrDefaultIsCappedAtCompaction(t *testing.T) {
	if got := (Config{}).ContextPruneThresholdOrDefault(); got != defaultContextPruneThreshold {
		t.Fatalf("unset prune threshold = %g, want default %g", got, defaultContextPruneThreshold)
	}
	if got := (Config{ContextPruneThreshold: 0.5}).ContextPruneThresholdOrDefault(); got != 0.5 {
		t.Fatalf("configured prune threshold = %g, want 0.5", got)
	}

	// A prune fraction above the compaction fraction is capped, so the cheap
	// deterministic pass still runs before the LLM summary.
	cfg := Config{ContextPruneThreshold: 0.95, ContextCompactionThreshold: 0.6}
	if got := cfg.ContextPruneThresholdOrDefault(); got != 0.6 {
		t.Fatalf("prune threshold above compaction = %g, want capped 0.6", got)
	}

	// The cap also applies to the default when compaction is configured below it.
	cfg = Config{ContextCompactionThreshold: 0.5}
	if got := cfg.ContextPruneThresholdOrDefault(); got != 0.5 {
		t.Fatalf("default prune threshold with compaction 0.5 = %g, want capped 0.5", got)
	}

	if got := DefaultConfig().ContextPruneThresholdOrDefault(); got != defaultContextPruneThreshold {
		t.Fatalf("DefaultConfig prune threshold = %g, want %g", got, defaultContextPruneThreshold)
	}
}

func TestConfigValidateRejectsOutOfRangeContextCompactionThreshold(t *testing.T) {
	for _, threshold := range []float64{-0.1, 1, 1.5, math.NaN(), math.Inf(1), math.Inf(-1)} {
		cfg := Config{
			Model:                      ModelConfig{Provider: "fake"},
			ContextCompactionThreshold: threshold,
		}
		err := cfg.Validate()
		if err == nil || !strings.Contains(err.Error(), "context_compaction_threshold") {
			t.Fatalf("threshold %g: expected context_compaction_threshold validation error, got %v", threshold, err)
		}
	}

	// Zero means "use the default" and must stay valid.
	cfg := Config{
		Model:                      ModelConfig{Provider: "fake"},
		ContextCompactionThreshold: 0,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("threshold 0 must be accepted as automatic, got %v", err)
	}
}

func TestContextCompactionThresholdOrDefault(t *testing.T) {
	if got := (Config{}).ContextCompactionThresholdOrDefault(); got != defaultContextCompactionThreshold {
		t.Fatalf("unset threshold = %g, want default %g", got, defaultContextCompactionThreshold)
	}
	if got := (Config{ContextCompactionThreshold: 0.5}).ContextCompactionThresholdOrDefault(); got != 0.5 {
		t.Fatalf("configured threshold = %g, want 0.5", got)
	}
	if got := DefaultConfig().ContextCompactionThresholdOrDefault(); got != defaultContextCompactionThreshold {
		t.Fatalf("DefaultConfig threshold = %g, want %g", got, defaultContextCompactionThreshold)
	}
}

func TestConfigValidateRejectsNegativeScreenStableSettings(t *testing.T) {
	cfg := Config{
		Model:                 ModelConfig{Provider: "fake"},
		ScreenStableTimeoutMs: -1,
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "screen_stable_timeout_ms") {
		t.Fatalf("expected screen_stable_timeout_ms validation error, got %v", err)
	}

	cfg = Config{
		Model:          ModelConfig{Provider: "fake"},
		ScreenStableMs: -1,
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "screen_stable_ms") {
		t.Fatalf("expected screen_stable_ms validation error, got %v", err)
	}

	cfg = Config{
		Model:                     ModelConfig{Provider: "fake"},
		ScreenStableDiffThreshold: -0.1,
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "screen_stable_diff_threshold") {
		t.Fatalf("expected screen_stable_diff_threshold validation error, got %v", err)
	}
}

func TestSearchProviderDefaultsAndAliases(t *testing.T) {
	tests := []struct {
		provider string
		want     string
	}{
		{provider: "", want: searchProviderDuckDuckGo},
		{provider: " DuckDuckGo ", want: searchProviderDuckDuckGo},
		{provider: " Brave Search ", want: searchProviderBrave},
		{provider: "brave_search", want: searchProviderBrave},
		{provider: "brave-search", want: searchProviderBrave},
		{provider: "brave-free", want: searchProviderBrave},
		{provider: " tavily ", want: searchProviderTavily},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			got := (SearchConfig{Provider: tt.provider}).ProviderOrDefault()
			if got != tt.want {
				t.Fatalf("ProviderOrDefault() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestConfigValidateAcceptsBraveSearchWithAPIKey(t *testing.T) {
	t.Setenv(braveSearchAPIKeyEnv, "")

	cfg := Config{
		Model:  ModelConfig{Provider: "fake"},
		Search: SearchConfig{Provider: "brave", APIKey: "BSA-token"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestConfigValidateAcceptsBraveSearchWithEnvAPIKey(t *testing.T) {
	t.Setenv(braveSearchAPIKeyEnv, "BSA-env-token")

	cfg := Config{
		Model:  ModelConfig{Provider: "fake"},
		Search: SearchConfig{Provider: "brave"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
}

func TestConfigValidateRejectsBraveSearchWithoutAPIKey(t *testing.T) {
	t.Setenv(braveSearchAPIKeyEnv, "")

	cfg := Config{
		Model:  ModelConfig{Provider: "fake"},
		Search: SearchConfig{Provider: "brave"},
	}
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected missing Brave Search API key error")
	}
	if !strings.Contains(err.Error(), braveSearchAPIKeyEnv) {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfigValidateRejectsNegativeScreenshotPruning(t *testing.T) {
	cfg := Config{
		Model:           ModelConfig{Provider: "fake"},
		ScreenshotKeepN: -1,
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "screenshot_keep_n") {
		t.Fatalf("expected screenshot_keep_n validation error, got %v", err)
	}

	cfg = Config{
		Model:                   ModelConfig{Provider: "fake"},
		ScreenshotPruneInterval: -1,
	}
	err = cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "screenshot_prune_interval") {
		t.Fatalf("expected screenshot_prune_interval validation error, got %v", err)
	}
}

func TestProxyConfigFromEnvironment(t *testing.T) {
	t.Setenv("http_proxy", "http://proxy.example:18080")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("all_proxy", "")
	t.Setenv("ALL_PROXY", "")
	t.Setenv("no_proxy", "")
	t.Setenv("NO_PROXY", "")

	proxy := ProxyConfigFromEnvironment()
	if proxy.HTTPProxy != "http://proxy.example:18080" {
		t.Fatalf("HTTPProxy = %q", proxy.HTTPProxy)
	}
	if proxy.NoProxy != DefaultNoProxy {
		t.Fatalf("NoProxy = %q, want default", proxy.NoProxy)
	}
}

func TestProxyConfigFromEnvironmentPrefersUppercase(t *testing.T) {
	t.Setenv("http_proxy", "http://lower.example:18080")
	t.Setenv("HTTP_PROXY", "http://upper.example:18080")
	t.Setenv("https_proxy", "http://lower.example:18081")
	t.Setenv("HTTPS_PROXY", "http://upper.example:18081")
	t.Setenv("all_proxy", "http://lower.example:18082")
	t.Setenv("ALL_PROXY", "http://upper.example:18082")
	t.Setenv("no_proxy", "lower.example")
	t.Setenv("NO_PROXY", "upper.example")

	proxy := ProxyConfigFromEnvironment()
	if proxy.HTTPProxy != "http://upper.example:18080" {
		t.Fatalf("HTTPProxy = %q, want uppercase value", proxy.HTTPProxy)
	}
	if proxy.HTTPSProxy != "http://upper.example:18081" {
		t.Fatalf("HTTPSProxy = %q, want uppercase value", proxy.HTTPSProxy)
	}
	if proxy.AllProxy != "http://upper.example:18082" {
		t.Fatalf("AllProxy = %q, want uppercase value", proxy.AllProxy)
	}
	if proxy.NoProxy != "upper.example" {
		t.Fatalf("NoProxy = %q, want uppercase value", proxy.NoProxy)
	}
}

func TestProxyConfigFromEnvironmentPreservesRawWhitespace(t *testing.T) {
	t.Setenv("HTTP_PROXY", " http://proxy.example:18080")
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")
	t.Setenv("ALL_PROXY", "")
	t.Setenv("all_proxy", "")
	t.Setenv("NO_PROXY", " example.com ")
	t.Setenv("no_proxy", "")

	proxy := ProxyConfigFromEnvironment()
	if proxy.HTTPProxy != " http://proxy.example:18080" {
		t.Fatalf("HTTPProxy = %q, want raw env value", proxy.HTTPProxy)
	}
	if proxy.NoProxy != " example.com " {
		t.Fatalf("NoProxy = %q, want raw env value", proxy.NoProxy)
	}
}

func TestConfigVADBackendDefaultsAndValidation(t *testing.T) {
	cfg := Config{Model: ModelConfig{Provider: "fake"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() error = %v", err)
	}
	if got := cfg.VADBackendOrDefault(); got != "rknn" {
		t.Fatalf("VADBackendOrDefault() = %q, want rknn", got)
	}

	cfg.VADBackend = " cpu "
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() with cpu backend error = %v", err)
	}
	if got := cfg.VADBackendOrDefault(); got != "cpu" {
		t.Fatalf("VADBackendOrDefault() = %q, want cpu", got)
	}
	if got := DefaultVADHelperPathForBackend("cpu"); got != "/usr/lib/aico/cpu_vad" {
		t.Fatalf("DefaultVADHelperPathForBackend(cpu) = %q", got)
	}
	if got := ResolveVADHelperPath("cpu", DefaultVADHelperPath()); got != "/usr/lib/aico/cpu_vad" {
		t.Fatalf("ResolveVADHelperPath(cpu, rknn default) = %q", got)
	}
	if got := ResolveVADHelperPath("cpu", "/custom/vad"); got != "/custom/vad" {
		t.Fatalf("ResolveVADHelperPath(cpu, custom) = %q", got)
	}

	cfg.VADBackend = "npu"
	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected invalid vad_backend error")
	}
	if !strings.Contains(err.Error(), "invalid vad_backend") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestDeviceConfigBackendDefaultsToHDMI(t *testing.T) {
	cfg := Config{Model: ModelConfig{Provider: "fake"}}
	if got := cfg.Device.BackendOrDefault(); got != "hdmi" {
		t.Fatalf("BackendOrDefault() = %q, want hdmi", got)
	}
}

func TestAudioBackendDefaultsByAgentMode(t *testing.T) {
	if got := (Config{Model: ModelConfig{Provider: "fake"}}).AudioBackendOrDefault(); got != AudioBackendAudioService {
		t.Fatalf("default playback backend = %q, want audio_service", got)
	}
	if got := (Config{Model: ModelConfig{Provider: "fake"}, HID: HIDConfig{InputBackend: "adb"}}).AudioBackendOrDefault(); got != AudioBackendLocal {
		t.Fatalf("adb playback backend = %q, want local", got)
	}
	if got := (Config{Model: ModelConfig{Provider: "fake"}, EnvironmentBridge: EnvironmentBridgeConfig{Enabled: true}}).AudioBackendOrDefault(); got != AudioBackendLocal {
		t.Fatalf("environment bridge playback backend = %q, want local", got)
	}
	if got := (Config{Model: ModelConfig{Provider: "fake"}, Audio: AudioConfig{Backend: "audio-service"}, HID: HIDConfig{InputBackend: "adb"}}).AudioBackendOrDefault(); got != AudioBackendAudioService {
		t.Fatalf("explicit audio-service backend = %q, want audio_service", got)
	}
}

func TestConfigValidateRejectsUnknownAudioBackend(t *testing.T) {
	cfg := Config{
		Model: ModelConfig{Provider: "fake"},
		Audio: AudioConfig{Backend: "bogus"},
	}
	err := cfg.Validate()
	if err == nil || !strings.Contains(err.Error(), "invalid audio.backend") {
		t.Fatalf("Validate() error = %v, want invalid audio.backend", err)
	}
}

func TestLoadConfigMigratesLegacyAudioPlaybackBackend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte("[model_settings.model]\nprovider = \"fake\"\n\n[voice_settings.classic.audio]\nplayback_backend = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Audio.Backend != AudioBackendLocal {
		t.Fatalf("Audio.Backend = %q, want local", cfg.Audio.Backend)
	}
}

func TestLoadConfigPrefersNewAudioBackendOverLegacy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte("[model_settings.model]\nprovider = \"fake\"\n\n[voice_settings.classic.audio]\nbackend = \"audio_service\"\nplayback_backend = \"local\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Audio.Backend != AudioBackendAudioService {
		t.Fatalf("Audio.Backend = %q, want audio_service", cfg.Audio.Backend)
	}
}

func TestLoadConfigRejectsUnknownDeviceBackend(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	content := `
[conversation_settings.agent]
prompt = "test"

[model_settings.model]
provider = "fake"

[basic_settings.device]
backend = "bogus"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), "invalid device.backend") {
		t.Fatalf("LoadConfig() error = %v, want invalid device.backend", err)
	}
}

func TestConfigValidateRejectsInvalidVADSpeechThreshold(t *testing.T) {
	for _, threshold := range []float64{-0.1, 1.1} {
		cfg := Config{
			Model:              ModelConfig{Provider: "fake"},
			VADSpeechThreshold: threshold,
		}

		err := cfg.Validate()
		if err == nil {
			t.Fatalf("Validate() error = nil for threshold %v, want error", threshold)
		}
		if !strings.Contains(err.Error(), "vad_speech_threshold") {
			t.Fatalf("Validate() error = %v, want vad_speech_threshold", err)
		}
	}
}

func TestConfigValidateRequiresTTSForSTT(t *testing.T) {
	cfg := Config{
		Model:     ModelConfig{Provider: "fake"},
		STT:       STTConfig{Provider: "openai-whisper"},
		TTS:       TTSConfig{Provider: "   "},
		InputMode: "stt",
	}

	err := cfg.Validate()
	if err == nil {
		t.Fatal("expected missing TTS provider error")
	}
	if !strings.Contains(err.Error(), "tts.provider is required") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConfigValidateRejectsUnsupportedAudioFormatForVoiceInput(t *testing.T) {
	tests := []struct {
		name    string
		audio   AudioConfig
		wantErr string
	}{
		{
			name:    "stereo",
			audio:   AudioConfig{Channels: 2},
			wantErr: "audio.channels must be 1",
		},
		{
			name:    "eight bit",
			audio:   AudioConfig{BitWidth: 8},
			wantErr: "audio.bit_width must be 16",
		},
		{
			name:    "too low sample rate",
			audio:   AudioConfig{SampleRate: 1},
			wantErr: "audio.sample_rate must be at least 8000",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{
				Model:     ModelConfig{Provider: "fake"},
				TTS:       TTSConfig{Provider: "minimax-cn"},
				STT:       STTConfig{Provider: "openai-whisper"},
				Audio:     tt.audio,
				InputMode: "stt",
			}

			err := cfg.Validate()
			if err == nil {
				t.Fatal("expected audio format validation error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestVoiceModelConfigLoadsAndValidates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	config := `[voice_settings.mode]
input_mode = "stt"

[model_settings.model]
provider = "fake"
model = "fake"

[voice_settings.classic.stt]
provider = "openai-whisper"

[voice_settings.classic.tts]
provider = "minimax-cn"

[voice_settings.realtime]
api_key = "voice-secret"
model = "qwen-audio-3.0-realtime-plus"
workspace_id = "ws-123"
region = "cn-beijing"
voice = "longanqian"
turn_detection = "smart_turn"
turn_detection_silence_ms = 900
`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.VoiceModel.Enabled() || cfg.VoiceModel.Model != "qwen-audio-3.0-realtime-plus" || cfg.VoiceModel.TurnDetection != "smart_turn" {
		t.Fatalf("unexpected voice model config: %+v", cfg.VoiceModel)
	}
}

func TestVoiceSessionConfigDefaults(t *testing.T) {
	cfg := Config{}

	if cfg.VoiceFollowupEnabledOrDefault() {
		t.Fatal("VoiceFollowupEnabledOrDefault() = true, want false")
	}
	if cfg.VoiceFirstTurnTimeoutOrDefault() != 10*time.Second {
		t.Fatalf("VoiceFirstTurnTimeoutOrDefault() = %s, want 10s", cfg.VoiceFirstTurnTimeoutOrDefault())
	}
	if cfg.VoiceFollowupTimeoutOrDefault() != 5*time.Second {
		t.Fatalf("VoiceFollowupTimeoutOrDefault() = %s, want 5s", cfg.VoiceFollowupTimeoutOrDefault())
	}
	if !cfg.VoiceInterruptOnWakeupOrDefault() {
		t.Fatal("VoiceInterruptOnWakeupOrDefault() = false, want true")
	}
	if !cfg.VoiceStreamingTTSEnabledOrDefault() {
		t.Fatal("VoiceStreamingTTSEnabledOrDefault() = false, want true")
	}
	if !cfg.VoiceToolCallSpeechOrDefault() {
		t.Fatal("VoiceToolCallSpeechOrDefault() = false, want true")
	}
	if !cfg.VoiceProgressSpeechEnabledOrDefault() {
		t.Fatal("VoiceProgressSpeechEnabledOrDefault() = false, want true")
	}
	if cfg.VoiceMaxResponseTokensOrDefault() != 300 {
		t.Fatalf("VoiceMaxResponseTokensOrDefault() = %d, want 300", cfg.VoiceMaxResponseTokensOrDefault())
	}
}

func TestVoiceSessionConfigOverrides(t *testing.T) {
	followupEnabled := true
	interruptDisabled := false
	streamingDisabled := false
	toolSpeech := false
	progressSpeechDisabled := false
	cfg := Config{
		VoiceFollowupEnabled:       &followupEnabled,
		VoiceFirstTurnTimeoutMs:    1234,
		VoiceFollowupTimeoutMs:     5678,
		VoiceInterruptOnWakeup:     &interruptDisabled,
		VoiceStreamingTTSEnabled:   &streamingDisabled,
		VoiceToolCallSpeech:        &toolSpeech,
		VoiceProgressSpeechEnabled: &progressSpeechDisabled,
		VoiceMaxResponseTokens:     123,
	}

	if !cfg.VoiceFollowupEnabledOrDefault() {
		t.Fatal("VoiceFollowupEnabledOrDefault() = false, want true")
	}
	if cfg.VoiceFirstTurnTimeoutOrDefault() != 1234*time.Millisecond {
		t.Fatalf("VoiceFirstTurnTimeoutOrDefault() = %s, want 1234ms", cfg.VoiceFirstTurnTimeoutOrDefault())
	}
	if cfg.VoiceFollowupTimeoutOrDefault() != 5678*time.Millisecond {
		t.Fatalf("VoiceFollowupTimeoutOrDefault() = %s, want 5678ms", cfg.VoiceFollowupTimeoutOrDefault())
	}
	if cfg.VoiceInterruptOnWakeupOrDefault() {
		t.Fatal("VoiceInterruptOnWakeupOrDefault() = true, want false")
	}
	if cfg.VoiceStreamingTTSEnabledOrDefault() {
		t.Fatal("VoiceStreamingTTSEnabledOrDefault() = true, want false")
	}
	if cfg.VoiceToolCallSpeechOrDefault() {
		t.Fatal("VoiceToolCallSpeechOrDefault() = true, want false")
	}
	if cfg.VoiceProgressSpeechEnabledOrDefault() {
		t.Fatal("VoiceProgressSpeechEnabledOrDefault() = true, want false")
	}
	if cfg.VoiceMaxResponseTokensOrDefault() != 123 {
		t.Fatalf("VoiceMaxResponseTokensOrDefault() = %d, want 123", cfg.VoiceMaxResponseTokensOrDefault())
	}
}

func TestVoiceSessionConfigValidationRejectsNegativeValues(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "negative followup timeout",
			cfg: Config{
				Model:                  ModelConfig{Provider: "fake"},
				VoiceFollowupTimeoutMs: -1,
			},
			want: "voice_followup_timeout_ms must be >= 0",
		},
		{
			name: "negative first turn timeout",
			cfg: Config{
				Model:                   ModelConfig{Provider: "fake"},
				VoiceFirstTurnTimeoutMs: -1,
			},
			want: "voice_first_turn_timeout_ms must be >= 0",
		},
		{
			name: "negative max turns",
			cfg: Config{
				Model:         ModelConfig{Provider: "fake"},
				VoiceMaxTurns: -1,
			},
			want: "voice_max_turns must be >= 0",
		},
		{
			name: "negative voice max response tokens",
			cfg: Config{
				Model:                  ModelConfig{Provider: "fake"},
				VoiceMaxResponseTokens: -1,
			},
			want: "voice_max_response_tokens must be >= 0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if err == nil {
				t.Fatal("Validate() error = nil, want error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("Validate() error = %v, want contains %q", err, tt.want)
			}
		})
	}
}

func TestAudioArchiveConfigDefaults(t *testing.T) {
	configContent := `
[voice_settings.classic.audio_archive]
enabled = true
storage_path = "/userdata/audio"
`

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "agent.toml")
	if err := os.WriteFile(configFile, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	var cfg Config
	if _, err := decodeConfigFile(configFile, &cfg); err != nil {
		t.Fatal(err)
	}

	if !cfg.AudioArchive.Enabled {
		t.Error("AudioArchive.Enabled should be true")
	}
	if cfg.AudioArchive.StoragePath != "/userdata/audio" {
		t.Errorf("StoragePath: got %q, want %q", cfg.AudioArchive.StoragePath, "/userdata/audio")
	}

	// Test defaults for unspecified fields
	if cfg.AudioArchive.MaxFilesOrDefault() != 500 {
		t.Errorf("MaxFiles default: got %d, want %d", cfg.AudioArchive.MaxFilesOrDefault(), 500)
	}
	if cfg.AudioArchive.MaxSizeMBOrDefault() != 100 {
		t.Errorf("MaxSizeMB default: got %d, want %d", cfg.AudioArchive.MaxSizeMBOrDefault(), 100)
	}
}

func TestLoadRuntimeConfigEnablesAudioArchiveByDefault(t *testing.T) {
	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "agent.toml")
	if err := os.WriteFile(configFile, []byte(`[model_settings.model]
provider = "fake"
`), 0644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRuntimeConfig(configFile)
	if err != nil {
		t.Fatalf("LoadRuntimeConfig() error = %v", err)
	}
	if !cfg.AudioArchive.Enabled {
		t.Fatal("AudioArchive.Enabled = false, want true for runtime defaults")
	}
	if got, want := cfg.AudioArchive.StoragePathOrDefault(), "/userdata/audio"; got != want {
		t.Fatalf("AudioArchive.StoragePathOrDefault() = %q, want %q", got, want)
	}
}

func TestAudioArchiveStoragePathOrDefaultTrimsWhitespace(t *testing.T) {
	if got := (AudioArchiveConfig{StoragePath: "  /tmp/audio  "}).StoragePathOrDefault(); got != "/tmp/audio" {
		t.Fatalf("StoragePathOrDefault() = %q, want trimmed path", got)
	}
	if got := (AudioArchiveConfig{StoragePath: "  \t  "}).StoragePathOrDefault(); got != defaultAudioArchiveStoragePath {
		t.Fatalf("StoragePathOrDefault() = %q, want default path", got)
	}
}

func TestAudioArchiveExplicitStoragePath(t *testing.T) {
	tests := []struct {
		name        string
		storagePath string
		want        string
	}{
		{"unset", "", ""},
		{"whitespace only", "  \t ", ""},
		{"built-in default treated as unset", defaultAudioArchiveStoragePath, ""},
		{"built-in default with whitespace", "  " + defaultAudioArchiveStoragePath + " ", ""},
		{"custom path", "/mnt/pinned/audio", "/mnt/pinned/audio"},
		{"custom path trimmed", " /mnt/pinned/audio ", "/mnt/pinned/audio"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := AudioArchiveConfig{StoragePath: tt.storagePath}
			if got := cfg.ExplicitStoragePath(); got != tt.want {
				t.Fatalf("ExplicitStoragePath() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The runtime config seeds storage_path with the built-in default, so a stock
// deployment must still route audio through the storage manager.
func TestRuntimeDefaultStoragePathIsNotExplicit(t *testing.T) {
	cfg := DefaultConfig()
	if got := cfg.AudioArchive.ExplicitStoragePath(); got != "" {
		t.Fatalf("DefaultConfig().AudioArchive.ExplicitStoragePath() = %q, want empty", got)
	}
}

func TestStorageMonitorEnabledConfigKey(t *testing.T) {
	tests := []struct {
		name    string
		storage string
		want    bool
	}{
		{name: "new key disables monitor", storage: "monitor_enabled = false", want: false},
		{name: "legacy ambiguous key is ignored", storage: "enabled = false", want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			configFile := filepath.Join(t.TempDir(), "agent.toml")
			content := "[model_settings.model]\nprovider = \"fake\"\n\n[storage_settings.storage]\n" + tt.storage + "\n"
			if err := os.WriteFile(configFile, []byte(content), 0644); err != nil {
				t.Fatal(err)
			}

			cfg, err := LoadRuntimeConfig(configFile)
			if err != nil {
				t.Fatalf("LoadRuntimeConfig() error = %v", err)
			}
			if cfg.Storage.MonitorEnabled != tt.want {
				t.Fatalf("Storage.MonitorEnabled = %t, want %t", cfg.Storage.MonitorEnabled, tt.want)
			}
		})
	}
}

func TestAudioArchiveConfigExplicitValues(t *testing.T) {
	configContent := `
[voice_settings.classic.audio_archive]
enabled = false
max_files = 1000
max_size_mb = 200
storage_path = "/custom/path"
`

	tmpDir := t.TempDir()
	configFile := filepath.Join(tmpDir, "agent.toml")
	if err := os.WriteFile(configFile, []byte(configContent), 0644); err != nil {
		t.Fatal(err)
	}

	var cfg Config
	if _, err := decodeConfigFile(configFile, &cfg); err != nil {
		t.Fatal(err)
	}

	if cfg.AudioArchive.Enabled {
		t.Error("AudioArchive.Enabled should be false")
	}
	if cfg.AudioArchive.MaxFilesOrDefault() != 1000 {
		t.Errorf("MaxFiles: got %d, want %d", cfg.AudioArchive.MaxFilesOrDefault(), 1000)
	}
	if cfg.AudioArchive.MaxSizeMBOrDefault() != 200 {
		t.Errorf("MaxSizeMB: got %d, want %d", cfg.AudioArchive.MaxSizeMBOrDefault(), 200)
	}
	if cfg.AudioArchive.StoragePath != "/custom/path" {
		t.Errorf("StoragePath: got %q, want %q", cfg.AudioArchive.StoragePath, "/custom/path")
	}
}

func TestLoadConfig_LiveActivitySection(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	body := `
[model_settings.model]
provider = "openrouter"
api_key = "x"
model = "y"

[advanced_settings.runtime.live_activity]
enabled = false
relay_url = "https://relay.example.com"
relay_api_key = "relay-secret"
board_id = "board-001"
bundle_id = "com.example.aico"
environment = "sandbox"
team_id = "TEAMID1234"
key_id = "KEYID12345"
private_key_path = "/tmp/AuthKey.p8"
timeout_sec = 3
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.LiveActivity.EnabledOrDefault() {
		t.Fatal("LiveActivity.EnabledOrDefault() = true, want false")
	}
}

func TestLoadRuntimeConfigFromDirDoesNotCreateLiveActivityBoardID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	body := `
[model_settings.model]
provider = "fake"

[advanced_settings.runtime.live_activity]
enabled = true
`
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := LoadRuntimeConfigFromDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.LiveActivity.EnabledOrDefault() {
		t.Fatal("LiveActivity.EnabledOrDefault() = false, want true")
	}
	if _, err := os.Stat(filepath.Join(dir, "board_id")); !os.IsNotExist(err) {
		t.Fatalf("board_id file exists or stat failed: %v", err)
	}
}
