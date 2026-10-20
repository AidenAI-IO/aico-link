package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigReadsGroupedTOML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	const source = `
[basic_settings.device]
device_type = "Android"

[basic_settings.device.hid]
keyboard_layout = "azerty"

[conversation_settings.agent]
max_iterations = 12

[model_settings.providers.local]
type = "openai"
api_key = "secret"

[model_settings.model]
provider = "local"
model = "gpt-test"

[voice_settings.mode]
input_mode = "stt"

[voice_settings.classic.stt.providers.local]
type = "openai-whisper"
model = "whisper-1"

[voice_settings.classic.stt]
provider = "local"

[voice_settings.classic.tts.providers.local]
type = "minimax-cn"

[voice_settings.classic.tts]
provider = "local"

[memory_settings.notification]
retention_days = 21
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Device.DeviceType != "Android" || cfg.HID.KeyboardLayout != "azerty" {
		t.Fatalf("device mapping = %#v / %#v", cfg.Device, cfg.HID)
	}
	if cfg.MaxIterations != 12 {
		t.Fatalf("max_iterations = %d, want 12", cfg.MaxIterations)
	}
	if cfg.Model.Provider != "local" || cfg.ModelProviders["local"].Type != "openai" {
		t.Fatalf("model mapping = %#v / %#v", cfg.Model, cfg.ModelProviders)
	}
	if cfg.InputMode != "stt" || cfg.STT.Provider != "local" || cfg.STTProviders["local"].Type != "openai-whisper" {
		t.Fatalf("voice mapping = mode %q stt %#v providers %#v", cfg.InputMode, cfg.STT, cfg.STTProviders)
	}
	if cfg.VoiceNotifications.RetentionDays != 21 {
		t.Fatalf("notification retention days = %d, want 21", cfg.VoiceNotifications.RetentionDays)
	}
}

func TestLoadConfigBasicKeyboardLayoutOverridesAdvancedHID(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	const source = `
[basic_settings.device.hid]
keyboard_layout = "azerty"

[advanced_settings.hardware.hid]
keyboard_layout = "qwertz"
input_backend = "adb"

[model_settings.model]
provider = "fake"
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.HID.KeyboardLayout != "azerty" {
		t.Fatalf("HID.KeyboardLayout = %q, want canonical Basic Settings value", cfg.HID.KeyboardLayout)
	}
	if cfg.HID.InputBackend != "adb" {
		t.Fatalf("HID.InputBackend = %q, want Advanced Settings value", cfg.HID.InputBackend)
	}
}

func TestLoadConfigReadsGroupedLegacyFieldAliases(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	const source = `
[model_settings.model]
provider = "fake"
max_tokens = 777

[voice_settings.classic.audio]
playback_backend = "local"
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Model.MaxResponseTokens != 777 {
		t.Fatalf("MaxResponseTokens = %d, want 777", cfg.Model.MaxResponseTokens)
	}
	if cfg.Audio.Backend != AudioBackendLocal {
		t.Fatalf("Audio.Backend = %q, want local", cfg.Audio.Backend)
	}
}

func TestLoadConfigReadsTapWakeupEnabledFromClassicRuntime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	const source = `
[model_settings.model]
provider = "fake"

[voice_settings.classic.runtime]
tap_wakeup_enabled = false
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.TapWakeupEnabledOrDefault() {
		t.Fatal("TapWakeupEnabledOrDefault() = true, want false from classic.runtime")
	}
}

func TestLoadConfigMapsGroupedLogRawHTTPToModelConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	const source = `
[model_settings.model]
provider = "fake"

[advanced_settings.log]
log_raw_http = false
`
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	cfg, err := LoadConfig(path)
	if err != nil {
		t.Fatalf("LoadConfig() error = %v", err)
	}
	if cfg.Model.LogRawHTTP {
		t.Fatal("Model.LogRawHTTP = true, want grouped advanced_settings.log value false")
	}
}

func TestLoadConfigRejectsLegacyFlatSections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte("[model]\nprovider = \"fake\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), `unsupported top-level TOML key "model"`) {
		t.Fatalf("LoadConfig() error = %v, want grouped-schema error", err)
	}
}

func TestLoadConfigRejectsLegacyRootFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte("locale = \"en-US\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err := LoadConfig(path)
	if err == nil || !strings.Contains(err.Error(), `unsupported top-level TOML key "locale"`) {
		t.Fatalf("LoadConfig() error = %v, want grouped-schema error", err)
	}
}
