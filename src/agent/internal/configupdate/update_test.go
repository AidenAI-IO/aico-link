package configupdate

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aiden-agent/internal/agent"
)

func TestUpdateConfigFilePreservesCommentsAndUnknownFields(t *testing.T) {
	source := `[basic_settings.language_timezone]
locale = "en-US" # locale comment

[basic_settings.device]
device_type = "iOS"

[model_settings.providers.openai-main]
type = "openai"
api_key = "test-key"

[model_settings.model]
provider = "openai-main"
model = "gpt-5.5"
responses = ["text", "audio"]

[basic_settings.device.hid]
keyboard_layout = "qwerty" # keep this comment
future_key = "preserve me"
`
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := NewService().Update(path, []byte(`{"config":{"hid":{"keyboard_layout":"azerty"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || len(result.ChangedPaths) != 1 || !result.RebootRequired {
		t.Fatalf("unexpected result: %+v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `keyboard_layout = "azerty" # keep this comment`) ||
		!strings.Contains(string(got), `future_key = "preserve me"`) ||
		!strings.Contains(string(got), `responses = ["text", "audio"]`) {
		t.Fatalf("unrelated TOML content was lost:\n%s", got)
	}
}

func TestUpdateConfigFileRepairsInvalidInputMode(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	source := `[voice_settings.mode]
input_mode = "realtime"

[voice_settings.providers.voice_model]
provider = "gemini"
api_key = "key"

[model_settings.model]
provider = "fake"
`
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}

	result, err := NewService().Update(path, []byte(`{"config":{"agent":{"input_mode":"stt"},"stt":{"provider":"openai-whisper"},"tts":{"provider":"minimax-cn"},"stt_providers":{"openai-whisper":{"type":"openai-whisper","api_key":"key"}},"tts_providers":{"minimax-cn":{"type":"minimax-cn","api_key":"key"}}}}`))
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !result.OK || !strings.Contains(strings.Join(result.ChangedPaths, ","), "voice_settings.mode.input_mode") {
		t.Fatalf("result = %+v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `input_mode = "stt"`) {
		t.Fatalf("input_mode was not repaired:\n%s", got)
	}
}

func TestUpdateConfigFileEnablesRealtimeWithProviderAtomically(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	source := `[voice_settings.mode]
input_mode = "stt"

[voice_settings.classic.stt]
provider = "speech"

[voice_settings.classic.tts]
provider = "voice"

[voice_settings.classic.stt.providers.speech]
type = "openai-whisper"
api_key = "stt-secret"

[voice_settings.classic.tts.providers.voice]
type = "alicloud"
api_key = "tts-secret"
`
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}

	result, err := NewService().Update(path, []byte(`{"config":{"agent":{"input_mode":"realtime"},"voice_model":{"provider":"qwen-main"},"voice_model_providers":{"qwen-main":{"type":"qwen","api_key":"realtime-secret","model":"qwen-audio-3.0-realtime-plus","voice":"longanqian"}}}}`))
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !result.OK || result.Config.Agent.InputMode != "realtime" || result.Config.VoiceModel.Provider != "qwen-main" {
		t.Fatalf("result = %+v", result)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		`input_mode = "realtime"`,
		`[voice_settings.realtime]`,
		`provider = "qwen-main"`,
		`[voice_settings.realtime.providers.qwen-main]`,
		`api_key = "realtime-secret"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("atomic realtime update lost %q:\n%s", want, text)
		}
	}
}

func TestUpdateConfigFileWritesGroupedLogRawHTTP(t *testing.T) {
	source := `[model_settings.model]
provider = "openai"
model = "gpt-5.5"

[advanced_settings.log]
log_raw_http = false
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}

	result, err := NewService().Update(path, []byte(`{"config":{"model":{"log_raw_http":true}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Config.Model.LogRawHTTP {
		t.Fatal("resolved model.log_raw_http = false, want true")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "[advanced_settings.log]") || !strings.Contains(string(got), "log_raw_http = true") {
		t.Fatalf("advanced_settings.log.log_raw_http was not updated:\n%s", got)
	}
	modelSection := string(got)
	if start := strings.Index(modelSection, "[model_settings.model]"); start >= 0 {
		modelSection = modelSection[start+len("[model_settings.model]"):]
		if next := strings.Index(modelSection, "\n["); next >= 0 {
			modelSection = modelSection[:next]
		}
	}
	if strings.Contains(modelSection, "log_raw_http") {
		t.Fatalf("log_raw_http remained in model section:\n%s", got)
	}
}

func TestUpdateConfigFileWritesIndependentContextPruneThreshold(t *testing.T) {
	source := `[model_settings.model]
provider = "openai"
model = "gpt-5.5"
responses_compact_threshold = 32000
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}

	// Deliberately not the default fraction: a patch equal to the resolved
	// current value is filtered as a no-op and never reaches the file.
	result, err := NewService().Update(path, []byte(`{"config":{"agent":{"context_prune_threshold":0.4}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.Agent.ContextPruneThreshold != 0.4 {
		t.Fatalf("resolved context_prune_threshold = %g, want 0.4", result.Config.Agent.ContextPruneThreshold)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "context_prune_threshold = 0.4") {
		t.Fatalf("context_prune_threshold was not updated:\n%s", got)
	}
	if !strings.Contains(string(got), "responses_compact_threshold = 32000") {
		t.Fatalf("provider compaction threshold changed while updating prune threshold:\n%s", got)
	}

	result, err = NewService().Update(path, []byte(`{"config":{"agent":{"context_prune_threshold":0}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.Agent.ContextPruneThreshold != 0 {
		t.Fatalf("resolved context_prune_threshold = %g, want 0 automatic mode", result.Config.Agent.ContextPruneThreshold)
	}
	got, err = os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "context_prune_threshold = 0") {
		t.Fatalf("context_prune_threshold was not reset to automatic mode:\n%s", got)
	}
	if !strings.Contains(string(got), "responses_compact_threshold = 32000") {
		t.Fatalf("provider compaction threshold changed while resetting prune threshold:\n%s", got)
	}
}

func TestUpdateConfigFileAddsProviderToInlineTable(t *testing.T) {
	source := `model_settings.providers = { old = { type = "openai" } }

[model_settings.model]
provider = "old"
model = "gpt-5.5"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}

	result, err := NewService().Update(path, []byte(`{"config":{"model_providers":{"new":{"type":"ollama"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.ChangedPaths, ",") != "model_settings.providers.new.type" {
		t.Fatalf("changed paths = %v", result.ChangedPaths)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `new = { type = "ollama" }`) ||
		strings.Contains(string(got), "[model_settings.providers.new]") {
		t.Fatalf("provider was not added inside the inline table:\n%s", got)
	}
}

func TestUpdateConfigFileSupportsQuotedProviderNamesAcrossOperations(t *testing.T) {
	source := `[model_settings.providers."open.router"]
type = "openai"
api_key = "model-secret"
base_url = "https://old.example"

[model_settings.model]
provider = "open.router"
model = "gpt-5.5"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}

	patches := []string{
		`{"config":{"model_providers":{"open.router":{"base_url":"https://new.example"}}}}`,
		`{"config":{"model_providers":{"new.provider":{"type":"ollama"}}}}`,
		`{"config":{"model_providers":{"open.router":null,"renamed.provider":{"type":"openai","base_url":"https://new.example"}},"_provider_renames":{"model_providers":{"renamed.provider":"open.router"}},"model":{"provider":"renamed.provider"}}}`,
		`{"config":{"model_providers":{"new.provider":null}}}`,
	}
	for _, patch := range patches {
		if _, err := NewService().Update(path, []byte(patch)); err != nil {
			t.Fatalf("Update(%s) error = %v", patch, err)
		}
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		`api_key = "model-secret"`,
		`base_url = "https://new.example"`,
		`provider = "renamed.provider"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("quoted provider operation lost %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "open.router") || strings.Contains(text, "new.provider") {
		t.Fatalf("deleted or renamed quoted provider remains:\n%s", text)
	}
	loaded, err := agent.LoadResolvedConfig(path)
	if err != nil {
		t.Fatalf("load updated quoted provider config: %v", err)
	}
	provider, ok := loaded.ModelProviders["renamed.provider"]
	if !ok || provider.APIKey != "model-secret" || provider.BaseURL != "https://new.example" {
		t.Fatalf("resolved quoted provider = %+v, exists = %v", provider, ok)
	}
}

func TestUpdateConfigFileRebootUsesEffectiveHIDConfig(t *testing.T) {
	tests := []struct {
		name          string
		deviceType    string
		wantCanonical string
		wantReboot    bool
	}{
		{name: "canonical spelling only", deviceType: "ios", wantCanonical: "iOS", wantReboot: false},
		{name: "same pointer mode", deviceType: "macOS", wantCanonical: "macOS", wantReboot: false},
		{name: "different pointer mode", deviceType: "Android", wantCanonical: "Android", wantReboot: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.toml")
			if err := os.WriteFile(path, []byte("[basic_settings.device]\ndevice_type = \"iOS\"\n"), 0o640); err != nil {
				t.Fatal(err)
			}
			patch := []byte(fmt.Sprintf(`{"config":{"device":{"device_type":%q}}}`, tt.deviceType))
			result, err := NewService().Update(path, patch)
			if err != nil {
				t.Fatal(err)
			}
			if result.RebootRequired != tt.wantReboot {
				t.Fatalf("reboot required = %v, want %v", result.RebootRequired, tt.wantReboot)
			}
			if result.Config.Device.DeviceType != tt.wantCanonical {
				t.Fatalf("resolved device type = %q, want %q", result.Config.Device.DeviceType, tt.wantCanonical)
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(string(got), fmt.Sprintf("device_type = %q", tt.deviceType)) {
				t.Fatalf("save did not preserve submitted spelling:\n%s", got)
			}
		})
	}
}

func TestConfigUpdateErrorsExposeStableKinds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if _, err := NewService().Update(path, []byte("not json")); err == nil {
		t.Fatal("invalid patch error = nil")
	} else if got := ErrorKind(err); got != ErrorKindInvalidRequest {
		t.Fatalf("invalid error kind = %q", got)
	}

	missingParent := filepath.Join(t.TempDir(), "missing", "agent.toml")
	if _, err := NewService().Update(missingParent, []byte(`{"config":{}}`)); err == nil {
		t.Fatal("missing parent error = nil")
	} else if got := ErrorKind(err); got != ErrorKindInternal {
		t.Fatalf("internal error kind = %q", got)
	}
}

// A file that selects Classic mode (input_mode=stt) with a TTS provider but no STT
// provider is exactly the recovery state the portal highlights. The editor view
// used to show the DefaultConfig() STT provider for it and the no-op filter
// compared against that view, so re-selecting the provider the page offered wrote
// nothing and the flag could never be cleared. The patch has to reach the file.
func TestUpdateConfigFileWritesProviderTheFileNeverDeclared(t *testing.T) {
	source := `[model_settings.model]
provider = "fake"

[voice_settings.mode]
input_mode = "stt"

[voice_settings.classic.tts]
provider = "minimax-cn"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}

	result, err := NewService().Update(path, []byte(`{"config":{"stt":{"provider":"openai-whisper"}}}`))
	if err != nil {
		t.Fatalf("Update() error = %v", err)
	}
	if !result.OK || strings.Join(result.ChangedPaths, ",") != "voice_settings.classic.stt.provider" {
		t.Fatalf("result = %+v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `provider = "openai-whisper"`) {
		t.Fatalf("stt provider was not written:\n%s", got)
	}
	if _, err := agent.LoadResolvedConfig(path); err != nil {
		t.Fatalf("repaired config is still rejected: %v", err)
	}
}

func TestUpdateConfigFileEmptyPatchDoesNotRewrite(t *testing.T) {
	source := []byte("[basic_settings.language_timezone]\nlocale = \"en-US\"\n[basic_settings.device.hid]\nkeyboard_layout = \"qwerty\"\n")
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, source, 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := NewService().Update(path, []byte(`{"config":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ChangedPaths) != 0 {
		t.Fatalf("changed paths = %v", result.ChangedPaths)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(source) {
		t.Fatalf("empty patch changed file")
	}
}

func TestConfigPatchOperationsUseRecordDeletesAndExplicitZero(t *testing.T) {
	ops, err := configPatchOperations(map[string]json.RawMessage{
		"tts_providers": json.RawMessage(`{"old":null,"new":{"type":"fish-audio"}}`),
		"audio_archive": json.RawMessage(`{"max_files":0,"enabled":false}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ops) != 4 {
		t.Fatalf("operations = %+v", ops)
	}
	var sawDelete, sawZero, sawFalse bool
	for _, op := range ops {
		if op.DeleteTable && strings.Join(op.Path, ".") == "voice_settings.classic.tts.providers.old" {
			sawDelete = true
		}
		if op.Value == int64(0) {
			sawZero = true
		}
		if op.Value == false {
			sawFalse = true
		}
	}
	if !sawDelete || !sawZero || !sawFalse {
		t.Fatalf("record delete/explicit values missing: %+v", ops)
	}
}

func TestUpdateVoiceModelProviderSelectionPreservesOtherRecords(t *testing.T) {
	source := `[voice_settings.mode]
input_mode = "realtime"

[voice_settings.realtime.providers.qwen-main]
type = "qwen"
api_key = "qwen-secret"
model = "qwen-realtime"
voice = "longanqian"

[voice_settings.realtime.providers.speko-main]
type = "speko"
api_key = "speko-secret"
upstream_provider = "xai"
model = "grok-voice-latest"
voice = "eve"

[voice_settings.realtime]
provider = "qwen-main"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}

	result, err := NewService().Update(path, []byte(`{"config":{"voice_model":{"provider":"speko-main"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.VoiceModel.Provider != "speko-main" {
		t.Fatalf("voice_model.provider = %q", result.Config.VoiceModel.Provider)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		`[voice_settings.realtime.providers.qwen-main]`, `api_key = "qwen-secret"`, `model = "qwen-realtime"`,
		`[voice_settings.realtime.providers.speko-main]`, `api_key = "speko-secret"`, `model = "grok-voice-latest"`,
		`provider = "speko-main"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("provider switch lost %q:\n%s", want, text)
		}
	}
}

func TestUpdateMigratesLegacyFlatVoiceModelWithoutLosingCredential(t *testing.T) {
	source := `[voice_settings.mode]
input_mode = "realtime"

[voice_settings.realtime]
provider = "openai"
api_key = "openai-secret"
model = "gpt-realtime-2"
voice = "alloy"
endpoint = "wss://gateway.example/v1/realtime"
realtime_protocol = "legacy"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}

	if _, err := NewService().Update(path, []byte(`{"config":{"agent":{"locale":"zh-CN"}}}`)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		`[voice_settings.realtime.providers.openai]`, `type = "openai"`, `api_key = "openai-secret"`,
		`model = "gpt-realtime-2"`, `voice = "alloy"`, `endpoint = "wss://gateway.example/v1/realtime"`,
		`realtime_protocol = "legacy"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("legacy voice model migration lost %q:\n%s", want, text)
		}
	}
	start := strings.Index(text, "[voice_settings.realtime]")
	if start < 0 {
		t.Fatalf("missing [voice_settings.realtime] after migration:\n%s", text)
	}
	voiceModelTable := text[start:]
	if next := strings.Index(voiceModelTable[1:], "\n["); next >= 0 {
		voiceModelTable = voiceModelTable[:next+1]
	}
	for _, legacy := range []string{"api_key =", "model =", "voice =", "endpoint =", "realtime_protocol ="} {
		if strings.Contains(voiceModelTable, legacy) {
			t.Fatalf("legacy field %q remains in [voice_settings.realtime]:\n%s", legacy, text)
		}
	}
}

func TestUpdateMigratesLegacyQwenTurnDetectionToProviderRecord(t *testing.T) {
	source := `[voice_settings.realtime]
provider = "qwen"
api_key = "qwen-secret"
turn_detection = "smart_turn"
turn_detection_threshold = 0.35
turn_detection_silence_ms = 900
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}

	if _, err := NewService().Update(path, []byte(`{"config":{"agent":{"locale":"zh-CN"}}}`)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		`[voice_settings.realtime.providers.qwen]`, `turn_detection = "smart_turn"`,
		`turn_detection_threshold = 0.35`, `turn_detection_silence_ms = 900`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("legacy Qwen migration lost %q:\n%s", want, text)
		}
	}
	start := strings.Index(text, "[voice_settings.realtime]")
	if start < 0 {
		t.Fatalf("missing [voice_settings.realtime] after migration:\n%s", text)
	}
	voiceModelTable := text[start:]
	if next := strings.Index(voiceModelTable[1:], "\n["); next >= 0 {
		voiceModelTable = voiceModelTable[:next+1]
	}
	if strings.Contains(voiceModelTable, "turn_detection") {
		t.Fatalf("legacy turn detection remains in [voice_settings.realtime]:\n%s", text)
	}
}

func TestUpdateWritesQwenProviderTurnDetectionNumbers(t *testing.T) {
	source := `[voice_settings.realtime.providers.qwen-main]
type = "qwen"

[voice_settings.realtime]
provider = "qwen-main"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}

	result, err := NewService().Update(path, []byte(`{"config":{"voice_model_providers":{"qwen-main":{"type":"qwen","turn_detection":"smart_turn","turn_detection_threshold":0.45,"turn_detection_silence_ms":850}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	record := result.Config.VoiceModelProviders["qwen-main"]
	if record.TurnDetection != "smart_turn" || record.TurnDetectionThreshold == nil || *record.TurnDetectionThreshold != 0.45 || record.TurnDetectionSilenceMs != 850 {
		t.Fatalf("saved Qwen provider = %+v", record)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{`turn_detection = "smart_turn"`, `turn_detection_threshold = 0.45`, `turn_detection_silence_ms = 850`} {
		if !strings.Contains(text, want) {
			t.Fatalf("Qwen provider update lost %q:\n%s", want, text)
		}
	}
}

func TestUpdateConfigFileRenamesProviderWithoutLosingCredentials(t *testing.T) {
	source := `[model_settings.providers.old]
type = "openai"
api_key = "model-secret"

[voice_settings.classic.tts.providers.old]
type = "fish-audio"
api_key = "tts-secret"

[voice_settings.classic.stt.providers.old]
type = "tencent-asr"
api_key = "stt-secret"
secret_id = "stt-id"
secret_key = "stt-key"

[model_settings.model]
provider = "old"
model = "gpt-5.5"

[voice_settings.classic.tts]
provider = "old"

[voice_settings.classic.stt]
provider = "old"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	patch := []byte(`{"config":{"model_providers":{"old":null,"new":{"type":"openai"}},"tts_providers":{"old":null,"new":{"type":"fish-audio"}},"stt_providers":{"old":null,"new":{"type":"tencent-asr"}},"_provider_renames":{"model_providers":{"new":"old"},"tts_providers":{"new":"old"},"stt_providers":{"new":"old"}},"model":{"provider":"new"},"tts":{"provider":"new"},"stt":{"provider":"new"}}}`)
	if _, err := NewService().Update(path, patch); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		"[model_settings.providers.new]", "api_key = \"model-secret\"",
		"[voice_settings.classic.tts.providers.new]", "api_key = \"tts-secret\"",
		"[voice_settings.classic.stt.providers.new]", "api_key = \"stt-secret\"",
		"secret_id = \"stt-id\"", "secret_key = \"stt-key\"",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("renamed provider lost %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "[model_settings.providers.old]") ||
		strings.Contains(text, "[voice_settings.classic.tts.providers.old]") ||
		strings.Contains(text, "[voice_settings.classic.stt.providers.old]") {
		t.Fatalf("old provider records were not removed:\n%s", text)
	}
}

func TestUpdateConfigFileRenamesDottedKeyProviderWithoutLeavingSource(t *testing.T) {
	source := `model_settings.providers.old.type = "openai"
model_settings.providers.old.api_key = "model-secret"
model_settings.model.provider = "old"
model_settings.model.model = "gpt-5.5"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	patch := []byte(`{"config":{"model_providers":{"old":null,"new":{"type":"openai"}},"_provider_renames":{"model_providers":{"new":"old"}},"model":{"provider":"new"}}}`)
	if _, err := NewService().Update(path, patch); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "model_settings.providers.old.") {
		t.Fatalf("old dotted-key provider was not removed:\n%s", text)
	}
	if !strings.Contains(text, `api_key = "model-secret"`) {
		t.Fatalf("renamed provider lost credential:\n%s", text)
	}
}

func TestUpdateConfigFilePreservesMaskedProviderCredential(t *testing.T) {
	source := `[model_settings.providers.openai]
type = "openai"
api_key = "model-secret"

[model_settings.model]
provider = "openai"
model = "gpt-5.5"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	patch := []byte(`{"config":{"model_providers":{"openai":{"type":"openai","api_key":"mode***cret"}}}}`)
	if _, err := NewService().Update(path, patch); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `api_key = "model-secret"`) || strings.Contains(string(got), "mode***cret") {
		t.Fatalf("masked credential was not preserved:\n%s", got)
	}
}

func TestUpdateConfigFilePreservesLiteralProviderCredentialSyntaxWhenEmpty(t *testing.T) {
	source := `[model_settings.providers.openai]
type = 'openai'
api_key = 'sk-secret-value-1234'

[model_settings.model]
provider = "openai"
model = "gpt-5.5"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := NewService().Update(path, []byte(`{"config":{"model_providers":{"openai":{"type":"openai","api_key":"","base_url":"https://x.test"}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.ChangedPaths, ",") != "model_settings.providers.openai.base_url" {
		t.Fatalf("changed paths = %v", result.ChangedPaths)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if !strings.Contains(text, "api_key = 'sk-secret-value-1234'") ||
		!strings.Contains(text, `base_url = "https://x.test"`) {
		t.Fatalf("provider credential syntax was not preserved:\n%s", text)
	}
}

func TestUpdateConfigFileAcceptsRedactedResolvedConfigPayload(t *testing.T) {
	source := `[model_settings.providers.openai]
type = "openai"
api_key = "model-secret"

[voice_settings.classic.tts.providers.voice]
type = "fish-audio"
api_key = "tts-secret"

[voice_settings.classic.stt.providers.tencent]
type = "tencent-asr"
api_key = "stt-secret"
secret_id = "secret-id"
secret_key = "secret-key"

[model_settings.model]
provider = "openai"
model = "gpt-5.5"

[voice_settings.classic.tts]
provider = "voice"

[voice_settings.classic.stt]
provider = "tencent"
language = "zh"

[storage_settings.storage]
monitor_enabled = true
root_path = "/custom/root"
check_interval_seconds = 123
warning_threshold_mb = 81
critical_threshold_mb = 21
emergency_threshold_mb = 9
recovery_hysteresis_mb = 4

[storage_settings.storage.degraded_mode]
disable_llm_http_log = false
disable_audio_archive = true
disable_session_archive = false
max_agent_log_mb = 3

[storage_settings.storage.cleanup]
enabled = false
llm_http_log_retention_days = [8, 4]
audio_archive_retention_days = [20, 2]
session_archive_retention_days = [14]
cleanup_retry_interval_seconds = 42
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	cfg, err := agent.LoadResolvedConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(map[string]any{"config": FromAgentConfig(cfg)})
	if err != nil {
		t.Fatal(err)
	}
	result, err := NewService().Update(path, payload)
	if err != nil {
		t.Fatalf("redacted resolved config was rejected: %v", err)
	}
	if len(result.ChangedPaths) != 0 {
		t.Fatalf("unchanged resolved config reported changes: %v", result.ChangedPaths)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != source {
		t.Fatalf("unchanged resolved config rewrote TOML:\n%s", got)
	}
	text := string(got)
	for _, want := range []string{
		`api_key = "model-secret"`,
		`api_key = "tts-secret"`,
		`api_key = "stt-secret"`,
		`secret_id = "secret-id"`,
		`secret_key = "secret-key"`,
		`root_path = "/custom/root"`,
		`max_agent_log_mb = 3`,
		`cleanup_retry_interval_seconds = 42`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("resolved config update lost %q:\n%s", want, text)
		}
	}
}

func TestUpdateConfigFileRejectsChangedReadOnlyCredentialStatus(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte("[conversation_settings.search]\nprovider = \"duckduckgo\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := NewService().Update(path, []byte(`{"config":{"search":{"has_api_key":true}}}`))
	if err == nil || !strings.Contains(err.Error(), "read-only status field") {
		t.Fatalf("changed read-only status error = %v", err)
	}
}

func TestUpdateConfigFileRejectsChangedDerivedPointerMode(t *testing.T) {
	source := "[basic_settings.device]\ndevice_type = \"iOS\"\n"
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	_, err := NewService().Update(path, []byte(`{"config":{"hid":{"pointer_mode":"touchscreen"}}}`))
	if err == nil || !strings.Contains(err.Error(), "hid.pointer_mode is a read-only derived field") {
		t.Fatalf("changed derived field error = %v", err)
	}
	got, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(got) != source {
		t.Fatalf("rejected derived field changed config:\n%s", got)
	}
}

func TestUpdateConfigFileUpdatesNestedStorageConfig(t *testing.T) {
	source := `[storage_settings.storage]
monitor_enabled = true
root_path = "/userdata"
check_interval_seconds = 300
warning_threshold_mb = 50
critical_threshold_mb = 10
emergency_threshold_mb = 5
recovery_hysteresis_mb = 5

[storage_settings.storage.degraded_mode]
max_agent_log_mb = 1 # keep comment

[storage_settings.storage.cleanup]
enabled = true
llm_http_log_retention_days = [7, 3, 1, 0]
cleanup_retry_interval_seconds = 60
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := NewService().Update(path, []byte(`{"config":{"storage":{"degraded_mode":{"max_agent_log_mb":4},"cleanup":{"llm_http_log_retention_days":[5,1]}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.ChangedPaths, ",") != "storage_settings.storage.cleanup.llm_http_log_retention_days,storage_settings.storage.degraded_mode.max_agent_log_mb" {
		t.Fatalf("changed paths = %v", result.ChangedPaths)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "max_agent_log_mb = 4 # keep comment") ||
		!strings.Contains(string(got), "llm_http_log_retention_days = [5, 1]") {
		t.Fatalf("nested storage fields were not updated losslessly:\n%s", got)
	}
}

func TestUpdateConfigFileRejectsMissingProviderRenameSource(t *testing.T) {
	source := `[model_settings.providers.current]
type = "openai"

[model_settings.model]
provider = "current"
model = "gpt-5.5"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	patch := []byte(`{"config":{"model_providers":{"missing":null,"new":{"type":"openai"}},"_provider_renames":{"model_providers":{"new":"missing"}}}}`)
	if _, err := NewService().Update(path, patch); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("NewService().Update() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != source {
		t.Fatalf("rejected rename changed config:\n%s", got)
	}
}

func TestUpdateConfigFileRejectsExistingProviderRenameTarget(t *testing.T) {
	source := `[model_settings.providers.old]
type = "openai"
api_key = "old-secret"

[model_settings.providers.existing]
type = "ollama"
base_url = "http://target"
api_key = "target-secret"

[model_settings.model]
provider = "old"
model = "gpt-5.5"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	patch := []byte(`{"config":{"model_providers":{"old":null,"existing":{"type":"openai"}},"_provider_renames":{"model_providers":{"existing":"old"}},"model":{"provider":"existing"}}}`)
	if _, err := NewService().Update(path, patch); err == nil || !strings.Contains(err.Error(), "target model_providers.existing already exists") {
		t.Fatalf("NewService().Update() error = %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != source {
		t.Fatalf("rejected rename changed config:\n%s", got)
	}
}

func TestUpdateConfigFileCreatesMissingConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	result, err := NewService().Update(path, []byte(`{"config":{"agent":{"locale":"zh-CN"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if !result.OK || strings.Join(result.ChangedPaths, ",") != "basic_settings.language_timezone.locale" {
		t.Fatalf("unexpected result: %+v", result)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `locale = "zh-CN"`) {
		t.Fatalf("missing config was not created:\n%s", got)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("created config mode = %o, want 640", info.Mode().Perm())
	}
}

func TestUpdateConfigFileWritesTimezoneToLanguageTimezoneGroup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte("[basic_settings.language_timezone]\nlocale = \"en-US\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := NewService().Update(path, []byte(`{"config":{"agent":{"timezone":"Asia/Shanghai"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.ChangedPaths, ",") != "basic_settings.language_timezone.timezone" {
		t.Fatalf("changed paths = %v", result.ChangedPaths)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), `timezone = "Asia/Shanghai"`) {
		t.Fatalf("timezone was not written to grouped TOML:\n%s", got)
	}
}

func TestTimezoneSurvivesConfigDTORoundTrip(t *testing.T) {
	wire := FromAgentConfig(agent.Config{Timezone: "Europe/London"})
	if wire.Agent.Timezone != "Europe/London" {
		t.Fatalf("wire timezone = %q", wire.Agent.Timezone)
	}
	if got := wire.ToAgentConfig().Timezone; got != "Europe/London" {
		t.Fatalf("round-trip timezone = %q", got)
	}
}

func TestConfigPatchOperationsRejectsSectionNull(t *testing.T) {
	for _, section := range []string{"hid", "agent", "model_providers"} {
		t.Run(section, func(t *testing.T) {
			_, err := configPatchOperations(map[string]json.RawMessage{section: json.RawMessage("null")})
			if err == nil || !strings.Contains(err.Error(), section+" must be an object") {
				t.Fatalf("configPatchOperations() error = %v", err)
			}
		})
	}
}

func TestUpdateConfigFileAcceptsScalarMapEntries(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	patch := []byte(`{"config":{"voice_notifications":{"expiration":{"code_ttl_seconds":{"network":123}}}}}`)
	if _, err := NewService().Update(path, patch); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "[memory_settings.notification.expiration.code_ttl_seconds]") ||
		!strings.Contains(string(got), "network = 123") {
		t.Fatalf("scalar map entry was not written:\n%s", got)
	}
}

func TestUpdateConfigFileWritesNotificationRetentionUnderMemorySettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := NewService().Update(path, []byte(`{"config":{"voice_notifications":{"retention_days":21}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.ChangedPaths, ",") != "memory_settings.notification.retention_days" {
		t.Fatalf("changed paths = %v", result.ChangedPaths)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "[memory_settings.notification]") ||
		!strings.Contains(string(got), "retention_days = 21") {
		t.Fatalf("notification retention was not written to Memory Settings:\n%s", got)
	}
}

func TestUpdateConfigFileWritesLogLevelUnderAdvancedSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := NewService().Update(path, []byte(`{"config":{"log":{"level":"warn"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.ChangedPaths, ",") != "advanced_settings.log.level" {
		t.Fatalf("changed paths = %v", result.ChangedPaths)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "[advanced_settings.log]") ||
		!strings.Contains(string(got), `level = "warn"`) {
		t.Fatalf("log level was not written to Advanced Settings:\n%s", got)
	}
}

func TestUpdateConfigFileRejectsNonObjectPatch(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	for _, patch := range []string{"null", `[]`, `{"config":null}`} {
		if _, err := NewService().Update(path, []byte(patch)); err == nil {
			t.Fatalf("NewService().Update(%s) error = nil", patch)
		}
	}
}

func TestUpdateConfigFileRejectsMalformedProviderRenames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, nil, 0o640); err != nil {
		t.Fatal(err)
	}
	for _, renames := range []string{
		"null",
		`{"model_providers":null}`,
		`{"model_providers":{"one":"old","two":"old"}}`,
	} {
		patch := `{"config":{"_provider_renames":` + renames + `}}`
		if _, err := NewService().Update(path, []byte(patch)); err == nil {
			t.Fatalf("NewService().Update(%s) error = nil", patch)
		}
	}
}

func TestValidateWebConfigPatchReportsScalarTypeErrors(t *testing.T) {
	tests := []struct {
		patch map[string]json.RawMessage
		want  string
	}{
		{map[string]json.RawMessage{"device": json.RawMessage(`{"backend":7}`)}, "device.backend: expected string"},
		{map[string]json.RawMessage{"live_activity": json.RawMessage(`{"enabled":"yes"}`)}, "live_activity.enabled: expected bool"},
		{map[string]json.RawMessage{"telemetry": json.RawMessage(`{"tags":"alpha"}`)}, "telemetry.tags: expected array"},
		{map[string]json.RawMessage{"tts": json.RawMessage(`{"speed":"fast"}`)}, "tts.speed: expected number"},
	}
	for _, tt := range tests {
		if err := validateWebConfigPatch(tt.patch); err == nil || !strings.Contains(err.Error(), tt.want) {
			t.Fatalf("validateWebConfigPatch() error = %v, want %q", err, tt.want)
		}
	}
}

func TestUpdateConfigFileRejectsOutOfRangeNumbersWithoutChangingFile(t *testing.T) {
	source := []byte("[model_settings.model]\nprovider = \"openai\"\nmodel = \"gpt-5.5\"\n\n[voice_settings.classic.audio_archive]\nmax_files = 5\n\n[voice_settings.classic.tts]\nspeed = 1\n")
	tests := []struct {
		name  string
		patch string
	}{
		{name: "integer overflow", patch: `{"config":{"audio_archive":{"max_files":999999999999999999999}}}`},
		{name: "float overflow", patch: `{"config":{"tts":{"speed":1e999}}}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.toml")
			if err := os.WriteFile(path, source, 0o640); err != nil {
				t.Fatal(err)
			}
			if _, err := NewService().Update(path, []byte(tt.patch)); err == nil {
				t.Fatal("NewService().Update() error = nil")
			}
			got, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, source) {
				t.Fatalf("rejected numeric patch changed config:\n%s", got)
			}
		})
	}
}

func TestUpdateConfigFileUpdatesInlineTable(t *testing.T) {
	source := []byte("advanced_settings.hardware.hid = { keyboard_layout = \"qwerty\" } # keep\n\n[model_settings.model]\nprovider = \"openai\"\nmodel = \"gpt-5.5\"\n")
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, source, 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := NewService().Update(path, []byte(`{"config":{"hid":{"keyboard_device":"/dev/hidg9"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.ChangedPaths, ",") != "advanced_settings.hardware.hid.keyboard_device" {
		t.Fatalf("changed paths = %v", result.ChangedPaths)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := "advanced_settings.hardware.hid = { keyboard_layout = \"qwerty\", keyboard_device = \"/dev/hidg9\" } # keep\n"
	if !strings.Contains(string(got), want) || strings.Contains(string(got), "\n[advanced_settings.hardware.hid]\n") {
		t.Fatalf("inline table was not updated in place:\n%s", got)
	}
}

func TestUpdateConfigFileProviderEditsRemoveLegacyFlatOverrides(t *testing.T) {
	source := `[model_settings.providers.primary]
type = "openai"
api_key = "record-model-key"

[voice_settings.classic.tts.providers.voice]
type = "fish-audio"
api_key = "record-tts-key"
voice_id = "record-voice"

[voice_settings.classic.stt.providers.speech]
type = "tencent-asr"
api_key = "record-stt-key"
secret_key = "record-stt-secret"

[model_settings.model]
provider = "primary"
model = "gpt-5.5"
api_key = "legacy-model-key"

[voice_settings.classic.tts]
provider = "voice"
api_key = "legacy-tts-key"
voice_id = "legacy-voice"

[voice_settings.classic.stt]
provider = "speech"
api_key = "legacy-stt-key"
secret_key = "legacy-stt-secret"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	patch := []byte(`{"config":{"model_providers":{"primary":{"type":"openai","api_key":"new-model-key"}},"tts_providers":{"voice":{"type":"fish-audio","api_key":"new-tts-key","voice_id":"new-voice"}},"stt_providers":{"speech":{"type":"tencent-asr","api_key":"new-stt-key","secret_key":"new-stt-secret"}}}}`)
	result, err := NewService().Update(path, patch)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ChangedPaths) == 0 {
		t.Fatal("provider edit reported no changed paths")
	}

	runtime, err := agent.LoadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Model.APIKey != "new-model-key" {
		t.Errorf("runtime model api_key = %q, want new provider key", runtime.Model.APIKey)
	}
	if runtime.TTS.APIKey != "new-tts-key" || runtime.TTS.VoiceID != "new-voice" {
		t.Errorf("runtime tts fields = %+v, want new provider values", runtime.TTS)
	}
	if runtime.STT.APIKey != "new-stt-key" || runtime.STT.SecretKey != "new-stt-secret" {
		t.Errorf("runtime stt fields = %+v, want new provider values", runtime.STT)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, section := range []string{
		"model_settings.model",
		"voice_settings.classic.tts",
		"voice_settings.classic.stt",
	} {
		sectionText := tomlTestSection(string(got), section)
		if sectionText == "" {
			t.Fatalf("updated config is missing [%s]:\n%s", section, got)
		}
		if strings.Contains(sectionText, "api_key") || strings.Contains(sectionText, "voice_id") ||
			strings.Contains(sectionText, "secret_key") {
			t.Errorf("legacy fields remain in [%s]:\n%s", section, sectionText)
		}
	}
}

func TestUpdateConfigFileProviderSwitchesIgnoreLegacyFlatCredentials(t *testing.T) {
	source := `[model_settings.providers.old-model]
type = "openai"
api_key = "old-model-record"

[model_settings.providers.new-model]
type = "openai"
api_key = "new-model-key"

[voice_settings.classic.tts.providers.old-voice]
type = "fish-audio"
api_key = "old-tts-record"

[voice_settings.classic.tts.providers.new-voice]
type = "fish-audio"
api_key = "new-tts-key"

[voice_settings.classic.stt.providers.old-speech]
type = "openai-whisper"
api_key = "old-stt-record"

[voice_settings.classic.stt.providers.new-speech]
type = "openai-whisper"
api_key = "new-stt-key"

[model_settings.model]
provider = "old-model"
model = "gpt-5.5"
api_key = "legacy-model-key"

[voice_settings.classic.tts]
provider = "old-voice"
api_key = "legacy-tts-key"

[voice_settings.classic.stt]
provider = "old-speech"
api_key = "legacy-stt-key"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	patch := []byte(`{"config":{"model":{"provider":"new-model"},"tts":{"provider":"new-voice"},"stt":{"provider":"new-speech"}}}`)
	if _, err := NewService().Update(path, patch); err != nil {
		t.Fatal(err)
	}

	runtime, err := agent.LoadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Model.APIKey != "new-model-key" || runtime.TTS.APIKey != "new-tts-key" ||
		runtime.STT.APIKey != "new-stt-key" {
		t.Fatalf("switched runtime kept a legacy key: model=%q tts=%q stt=%q",
			runtime.Model.APIKey, runtime.TTS.APIKey, runtime.STT.APIKey)
	}
	if runtime.ModelProviders["old-model"].APIKey != "legacy-model-key" ||
		runtime.TTSProviders["old-voice"].APIKey != "legacy-tts-key" ||
		runtime.STTProviders["old-speech"].APIKey != "legacy-stt-key" {
		t.Fatal("legacy credentials were not preserved on their original provider records")
	}
}

func TestUpdateConfigFileChangedSavePersistsLegacyCredentialsOnlyInRecords(t *testing.T) {
	source := `[model_settings.model]
provider = "openai"
model = "gpt-5.5"
api_key = "legacy-model-key"

[voice_settings.classic.tts]
provider = "fish-audio"
api_key = "legacy-tts-key"
reference_id = "legacy-reference"

[voice_settings.classic.stt]
provider = "openai-whisper"
api_key = "legacy-stt-key"
model = "whisper-1"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := NewService().Update(path, []byte(`{"config":{"agent":{"max_iterations":7}}}`)); err != nil {
		t.Fatal(err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		"[model_settings.providers.openai]", `api_key = "legacy-model-key"`,
		"[voice_settings.classic.tts.providers.fish-audio]", `api_key = "legacy-tts-key"`, `reference_id = "legacy-reference"`,
		"[voice_settings.classic.stt.providers.openai-whisper]", `api_key = "legacy-stt-key"`, `model = "whisper-1"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("migrated config missing %q:\n%s", want, text)
		}
	}
	for _, section := range []string{
		"model_settings.model",
		"voice_settings.classic.tts",
		"voice_settings.classic.stt",
	} {
		sectionText := tomlTestSection(text, section)
		if sectionText == "" {
			t.Fatalf("migrated config is missing [%s]:\n%s", section, text)
		}
		if strings.Contains(sectionText, "api_key") || strings.Contains(sectionText, "reference_id") {
			t.Errorf("legacy fields remain in [%s]:\n%s", section, sectionText)
		}
	}
}

func TestUpdateConfigFileNoopDoesNotPersistLegacyProviderMigration(t *testing.T) {
	source := []byte(`[model_settings.model]
provider = "openai"
model = "gpt-5.5"
api_key = "legacy-model-key"

[voice_settings.classic.tts]
provider = "fish-audio"
api_key = "legacy-tts-key"
`)
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, source, 0o640); err != nil {
		t.Fatal(err)
	}
	result, err := NewService().Update(path, []byte(`{"config":{}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ChangedPaths) != 0 {
		t.Fatalf("changed paths = %v", result.ChangedPaths)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, source) {
		t.Fatalf("no-op save persisted a legacy migration:\n%s", got)
	}
}

func tomlTestSection(text, name string) string {
	start := strings.Index(text, "["+name+"]\n")
	if start < 0 {
		return ""
	}
	rest := text[start+1:]
	if end := strings.Index(rest, "\n["); end >= 0 {
		return text[start : start+1+end]
	}
	return text[start:]
}

func TestUpdateRejectsObsoleteRequestFields(t *testing.T) {
	for _, patch := range []string{
		`{"model":{"api_key":"secret"}}`,
		`{"tts":{"api_key":"secret"}}`,
		`{"stt":{"secret_key":"secret"}}`,
		`{"voice_model":{"api_key":"secret"}}`,
		`{"voice_model":{"instructions":"ignored"}}`,
		`{"model":{"base_url":"https://example.com"}}`,
		`{"agent":{"default_platform":"ios"}}`,
		`{"agent":{"instruction":"ignored"}}`,
		`{"agent":{"custom_instruction":"ignored"}}`,
		`{"agent":{"additional_prompt":"ignored"}}`,
		`{"tts_providers":{"voice":{"provider":"fish-audio"}}}`,
		`{"audio":{"playback_backend":"alsa"}}`,
	} {
		t.Run(patch, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.toml")
			source := []byte("# unchanged\n")
			if err := os.WriteFile(path, source, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := NewService().Update(path, []byte(`{"config":`+patch+`}`)); err == nil || ErrorKind(err) != ErrorKindInvalidRequest {
				t.Fatalf("obsolete request accepted: %v", err)
			}
			if got, _ := os.ReadFile(path); string(got) != string(source) {
				t.Fatal("rejected request changed file")
			}
		})
	}
}

func TestUpdateRequiresCanonicalEnvelope(t *testing.T) {
	for _, body := range []string{`{}`, `{"agent":{"locale":"zh-CN"}}`, `{"config":{},"apply_wifi":false}`, `{"config":null}`} {
		if _, err := NewService().Update(filepath.Join(t.TempDir(), "agent.toml"), []byte(body)); err == nil || ErrorKind(err) != ErrorKindInvalidRequest {
			t.Errorf("accepted %s: %v", body, err)
		}
	}
}

func TestResolvedWebConfigOmitsLegacyModelCredential(t *testing.T) {
	cfg := agent.DefaultConfig()
	cfg.Model.APIKey = "top-secret"
	encoded, err := json.Marshal(FromAgentConfig(cfg))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "top-secret") || strings.Contains(string(encoded), `"api_key"`) {
		t.Fatalf("resolved web config exposed legacy model credential: %s", encoded)
	}
}

// TestResolvedWebConfigOmitsCustomInstruction covers the removed
// custom_instruction setting: the wire DTO has no such field even when the
// runtime config carries a non-default Instruction, and prompt is
// the only prompt field that round trips.
func TestResolvedWebConfigOmitsCustomInstruction(t *testing.T) {
	cfg := agent.DefaultConfig()
	cfg.Instruction = "Deployment persona."
	cfg.Prompt = "Always answer in bullet points."

	dto := FromAgentConfig(cfg)
	encoded, err := json.Marshal(dto)
	if err != nil {
		t.Fatal(err)
	}
	var sections map[string]map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &sections); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"custom_instruction", "instruction"} {
		if _, ok := sections["agent"][key]; ok {
			t.Fatalf("resolved web config exposed agent.%s: %s", key, encoded)
		}
	}
	if got := dto.ToAgentConfig().Prompt; got != cfg.Prompt {
		t.Fatalf("round-trip prompt = %q, want %q", got, cfg.Prompt)
	}
}

// TestUpdateIgnoresLegacyCustomInstructionInFile proves that a leftover
// custom_instruction line is inert: config web applies targeted document
// operations, so an unrelated save leaves the legacy key untouched instead of
// rewriting it from the DTO, and the loader still resolves the built-in
// instruction.
func TestUpdateIgnoresLegacyCustomInstructionInFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	source := `[conversation_settings.agent]
custom_instruction = "Deployment persona."

[model_settings.model]
provider = "fake"
`
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}

	result, err := NewService().Update(path, []byte(`{"config":{"agent":{"prompt":"Be concise."}}}`))
	if err != nil {
		t.Fatalf("update with a legacy custom_instruction present failed: %v", err)
	}
	updated, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(updated), "custom_instruction") != 1 {
		t.Fatalf("update rewrote the legacy custom_instruction line:\n%s", updated)
	}
	if !strings.Contains(string(updated), `prompt = "Be concise."`) {
		t.Fatalf("update did not persist prompt:\n%s", updated)
	}
	if result.Config.Agent.Prompt != "Be concise." {
		t.Fatalf("result prompt = %q, want Be concise.", result.Config.Agent.Prompt)
	}

	cfg, err := agent.LoadResolvedConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Instruction != agent.DefaultConfig().Instruction {
		t.Fatalf("Instruction = %q, want built-in default because the legacy key is ignored", cfg.Instruction)
	}
	if cfg.Prompt != "Be concise." {
		t.Fatalf("Prompt = %q, want Be concise.", cfg.Prompt)
	}
}

func TestVoiceModelConfigRoundTripPreservesSettingsAndCredentialPresence(t *testing.T) {
	emotion := true
	threshold := 0.72
	want := agent.Config{VoiceModel: agent.VoiceModelConfig{
		Provider:               "speko",
		UpstreamProvider:       "xai",
		AgentID:                "agent-1",
		APIKey:                 "voice-secret",
		Model:                  "qwen-audio-3.0-realtime-plus",
		WorkspaceID:            "workspace-1",
		Region:                 "cn-beijing",
		AuthMode:               "vertex",
		ProjectID:              "project-1",
		Location:               "us-central1",
		Endpoint:               "wss://voice.example.test/realtime",
		BaseURL:                "https://api.speko.dev",
		RealtimeProtocol:       "legacy",
		Voice:                  "longanqian",
		EnableSpeechEmotion:    &emotion,
		InputAudioFormat:       "pcm16",
		OutputAudioFormat:      "pcm16",
		TurnDetection:          "smart_turn",
		TurnDetectionThreshold: &threshold,
		TurnDetectionSilenceMs: 900,
		UseBackendAgent:        true,
	}}
	dto := FromAgentConfig(want)
	if dto.VoiceModel.APIKey != "" || !dto.VoiceModel.HasAPIKey {
		t.Fatalf("voice credential was not redacted correctly: %+v", dto.VoiceModel)
	}
	got := dto.ToAgentConfig().VoiceModel
	if got.APIKey != hasAPIKeyPlaceholder {
		t.Fatalf("round-trip API key = %q, want placeholder", got.APIKey)
	}
	got.APIKey = want.VoiceModel.APIKey
	if !reflect.DeepEqual(got, want.VoiceModel) {
		t.Fatalf("voice model round-trip = %+v, want %+v", got, want.VoiceModel)
	}
}

func TestVoiceModelProviderSettingsRoundTrip(t *testing.T) {
	threshold := 0.4
	want := map[string]agent.VoiceModelProvider{
		"qwen-main": {
			Type: "qwen", TurnDetection: "smart_turn",
			TurnDetectionThreshold: &threshold, TurnDetectionSilenceMs: 875,
		},
		"gemini-main": {Type: "gemini", ThinkingLevel: "HIGH"},
	}
	dto := FromAgentConfig(agent.Config{
		VoiceModelProviders: want,
	})
	got := dto.ToAgentConfig().VoiceModelProviders
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("voice model provider round-trip = %+v, want %+v", got, want)
	}
}

func TestUpdateConfigFileWritesResponsesContextFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	source := "[model_settings.model]\nprovider = \"volcengine\"\nmodel = \"doubao-seed-2-1-pro\"\napi_mode = \"responses_stateful\"\n"
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	patch := []byte(`{"config":{"model":{"responses_context_management":"ark_context_edit","responses_context_edit_trigger":10,"responses_context_edit_keep":3,"responses_context_edit_clear_thinking":true,"responses_include":["reasoning.encrypted_content"]}}}`)
	result, err := NewService().Update(path, patch)
	if err != nil {
		t.Fatal(err)
	}
	if result.Config.Model.ResponsesContextManagement != "ark_context_edit" ||
		result.Config.Model.ResponsesContextEditTrigger != 10 ||
		result.Config.Model.ResponsesContextEditKeep != 3 ||
		!result.Config.Model.ResponsesContextEditClearThinking ||
		len(result.Config.Model.ResponsesInclude) != 1 {
		t.Fatalf("unexpected resolved Responses config: %+v", result.Config.Model)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	for _, want := range []string{
		`responses_context_management = "ark_context_edit"`,
		`responses_context_edit_trigger = 10`,
		`responses_context_edit_keep = 3`,
		`responses_context_edit_clear_thinking = true`,
		`responses_include = ['reasoning.encrypted_content']`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("updated config missing %q:\n%s", want, text)
		}
	}
}

func TestPlanReportsApplyLevelWithoutWriting(t *testing.T) {
	source := `[basic_settings.device]
device_type = "iOS"

[basic_settings.device.hid]
keyboard_layout = "qwerty"
`
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(source), 0o640); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		patch string
		want  agent.ApplyLevel
	}{
		{`{"config":{"device":{"device_type":"macOS"}}}`, agent.ApplyLive},
		{`{"config":{"device":{"device_type":"Android"}}}`, agent.ApplyReboot},
		{`{"config":{"hid":{"keyboard_layout":"azerty"}}}`, agent.ApplyReboot},
		{`{"config":{}}`, agent.ApplyLive},
	}
	for _, tt := range tests {
		result, err := NewService().Plan(path, []byte(tt.patch))
		if err != nil {
			t.Fatalf("Plan(%s) error = %v", tt.patch, err)
		}
		if result.Apply != tt.want || result.Persisted || result.RebootRequired != (tt.want == agent.ApplyReboot) {
			t.Fatalf("Plan(%s) = %+v, want apply %q", tt.patch, result, tt.want)
		}
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != source {
		t.Fatalf("Plan must not write the config:\n%s", got)
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("Plan left files behind: %v", entries)
	}
	if _, err := NewService().Plan(path, []byte(`{"config":{"hid":{"keyboard_layout":"dvorak"}}}`)); err == nil {
		t.Fatal("Plan must reject a patch Update would reject")
	}
}

func TestUpdateDefaultQwenProviderPersistsType(t *testing.T) {
	for _, record := range []string{`{"type":"qwen","api_key":"test-key"}`, `{"api_key":"test-key"}`} {
		t.Run(record, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "agent.toml")
			if err := os.WriteFile(path, []byte(""), 0600); err != nil {
				t.Fatal(err)
			}
			patch := []byte(`{"config":{"voice_model_providers":{"qwen":` + record + `}}}`)
			if _, err := NewService().Plan(path, patch); err != nil {
				t.Fatal(err)
			}
			before, _ := os.ReadFile(path)
			if len(before) != 0 {
				t.Fatal("plan changed config")
			}
			if _, err := NewService().Update(path, patch); err != nil {
				t.Fatal(err)
			}
			cfg, err := agent.LoadResolvedConfig(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.VoiceModelProviders["qwen"].Type != "qwen" || cfg.VoiceModelProviders["qwen"].APIKey != "test-key" {
				t.Fatal("provider did not round trip")
			}
		})
	}
}
