package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"aiden-agent/internal/agent"
	"aiden-agent/internal/configupdate"
)

func TestRunConfigUpdateIODelegatesToService(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte("[basic_settings.device.hid]\nkeyboard_layout = \"qwerty\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := runConfigUpdateIO(
		[]string{"--config", path, "--stdin", "--format=json"},
		strings.NewReader(`{"config":{"hid":{"keyboard_layout":"azerty"}}}`),
		&stdout,
		&stderr,
	)
	if exitCode != 0 || stderr.Len() != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	var result configupdate.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if !result.OK || !result.RebootRequired || strings.Join(result.ChangedPaths, ",") != "basic_settings.device.hid.keyboard_layout" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunConfigUpdateIOEncodesInvalidArguments(t *testing.T) {
	var stdout, stderr bytes.Buffer
	exitCode := runConfigUpdateIO(nil, strings.NewReader("{}"), &stdout, &stderr)
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1", exitCode)
	}
	var result configupdate.Result
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("decode result: %v", err)
	}
	if result.OK || result.ErrorKind != configupdate.ErrorKindInvalidRequest {
		t.Fatalf("result = %+v", result)
	}
}

func TestRunConfigUpdateIOHelpExitsSuccessfully(t *testing.T) {
	var stdout, stderr bytes.Buffer
	if exitCode := runConfigUpdateIO([]string{"-h"}, strings.NewReader("{}"), &stdout, &stderr); exitCode != 0 {
		t.Fatalf("exit code = %d, want 0; stderr=%q", exitCode, stderr.String())
	}
	if stdout.Len() != 0 || !strings.Contains(stderr.String(), "Usage of config-update") {
		t.Fatalf("stdout=%q stderr=%q, want help on stderr only", stdout.String(), stderr.String())
	}
}

func TestExecuteConfigTestUsesModelRuntime(t *testing.T) {
	values, err := json.Marshal(modelDTO{Provider: "fake"})
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	result := executeConfigTest(context.Background(), agent.Config{
		Model: agent.ModelConfig{Provider: "fake", Responses: []string{"hello"}},
	}, configTestInput{Section: "model", Values: values}, "model")
	if !result.OK || len(result.Results) != 1 || result.Results[0].Check != "provider_request" {
		t.Fatalf("result = %+v", result)
	}
}

func TestExecuteConfigTestAsksDecisionModelWithSavedKey(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer ts-saved" {
			http.Error(w, "bad key", http.StatusUnauthorized)
			return
		}
		_, _ = io.WriteString(w, `{"answers":{"check":{"type":"noul","noul":0.97}}}`)
	}))
	defer server.Close()
	cfg := agent.Config{Decision: agent.DecisionModelConfig{APIKey: "ts-saved", BaseURL: server.URL}}

	// The page sends what it shows: has_api_key, never the saved key.
	values := []byte(`{"provider":"typesafe","has_api_key":true,"base_url":"` + server.URL + `","model":"jev-preview"}`)
	result := executeConfigTest(context.Background(), cfg, configTestInput{Section: "decision", Values: values}, "decision")
	if !result.OK || result.Results[0].Check != "provider_request" {
		t.Fatalf("result = %+v", result)
	}

	values = []byte(`{"provider":"typesafe","api_key":"ts-wrong","base_url":"` + server.URL + `"}`)
	result = executeConfigTest(context.Background(), cfg, configTestInput{Section: "decision", Values: values}, "decision")
	if result.OK || !strings.Contains(result.Results[0].Detail, "401") {
		t.Fatalf("wrong key result = %+v", result)
	}
}

func TestModelDTOProviderTestRequestPreservesSamplingPresenceFromJSON(t *testing.T) {
	tests := []struct {
		name                string
		payload             string
		wantTemperature     *float64
		wantReasoningEffort string
	}{
		{
			name:    "omitted fields remain unset",
			payload: `{"provider":"openai","model":"kimi-k3"}`,
		},
		{
			name:    "null temperature remains unset",
			payload: `{"provider":"openai","model":"kimi-k3","temperature":null}`,
		},
		{
			name:                "explicit zero remains present",
			payload:             `{"provider":"openai","model":"kimi-k3","temperature":0,"reasoning_effort":"none"}`,
			wantTemperature:     testFloat64Ptr(0),
			wantReasoningEffort: "none",
		},
		{
			name:                "explicit nonzero values are preserved",
			payload:             `{"provider":"openai","model":"kimi-k3","temperature":0.7,"reasoning_effort":"medium"}`,
			wantTemperature:     testFloat64Ptr(0.7),
			wantReasoningEffort: "medium",
		},
		{
			name:    "explicit empty reasoning remains auto",
			payload: `{"provider":"openai","model":"kimi-k3","reasoning_effort":""}`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var dto modelDTO
			if err := json.Unmarshal([]byte(tt.payload), &dto); err != nil {
				t.Fatalf("unmarshal model values: %v", err)
			}
			req := dto.ProviderTestRequest()
			if tt.wantTemperature == nil {
				if req.Temperature != nil {
					t.Fatalf("temperature request = %v, want unset", req.Temperature)
				}
			} else if req.Temperature == nil || *req.Temperature != *tt.wantTemperature {
				t.Fatalf("temperature request = %v, want %v", req.Temperature, *tt.wantTemperature)
			}
			if req.ReasoningEffort != tt.wantReasoningEffort {
				t.Fatalf("reasoning request = %q, want %q", req.ReasoningEffort, tt.wantReasoningEffort)
			}
		})
	}
}

func testFloat64Ptr(value float64) *float64 {
	return &value
}

func TestExecuteConfigTestUsesSTTRuntimeWithoutAudio(t *testing.T) {
	values, err := json.Marshal(sttDTO{Provider: "qwen-main", Language: "zh"})
	if err != nil {
		t.Fatalf("marshal values: %v", err)
	}
	result := executeConfigTest(context.Background(), agent.Config{
		STTProviders: map[string]agent.STTProvider{
			"qwen-main": {Type: "qwen-asr", APIKey: "test-key"},
		},
	}, configTestInput{Section: "stt", Values: values}, "stt")
	if !result.OK || len(result.Results) != 1 || result.Results[0].Check != "provider_config" {
		t.Fatalf("result = %+v", result)
	}
}

func TestConfigCheck_ValidConfig(t *testing.T) {
	validConfig := agent.Config{
		Model: agent.ModelConfig{
			Provider: "openai",
			Model:    "gpt-4",
		},
		Search: agent.SearchConfig{
			Provider: "duckduckgo",
		},
	}

	configJSON, err := json.Marshal(validConfig)
	if err != nil {
		t.Fatalf("failed to marshal config: %v", err)
	}

	var output bytes.Buffer
	result := ValidationResult{}

	// Manually test validation
	if err := validConfig.Validate(); err != nil {
		t.Errorf("valid config should not have errors: %v", err)
	}

	// Test parseValidationErrors with nil
	errors := parseValidationErrors(nil)
	if len(errors) != 0 {
		t.Errorf("expected 0 errors for nil error, got %d", len(errors))
	}

	// Verify JSON serialization
	encoder := json.NewEncoder(&output)
	result.Valid = true
	result.Errors = []ValidationError{}
	if err := encoder.Encode(result); err != nil {
		t.Fatalf("failed to encode result: %v", err)
	}

	var decoded ValidationResult
	if err := json.Unmarshal(output.Bytes(), &decoded); err != nil {
		t.Fatalf("failed to decode result: %v", err)
	}

	if !decoded.Valid {
		t.Errorf("expected valid=true, got valid=false")
	}

	if len(decoded.Errors) != 0 {
		t.Errorf("expected 0 errors, got %d", len(decoded.Errors))
	}

	t.Logf("Valid config JSON: %s", string(configJSON))
}

func TestResolvedWebConfigDTO_MissingFileUsesDefaults(t *testing.T) {
	dto, err := resolvedWebConfigDTO(filepath.Join(t.TempDir(), "agent.toml"))
	if err != nil {
		t.Fatalf("resolvedWebConfigDTO() error = %v", err)
	}
	if dto.HID.FrameSocket != agent.DefaultConfig().HID.FrameSocket {
		t.Fatalf("HID frame_socket = %q, want %q",
			dto.HID.FrameSocket, agent.DefaultConfig().HID.FrameSocket)
	}
	if dto.HID.KeyboardLayout != agent.DefaultConfig().HID.KeyboardLayout {
		t.Fatalf("HID keyboard_layout = %q, want %q",
			dto.HID.KeyboardLayout, agent.DefaultConfig().HID.KeyboardLayout)
	}
	if dto.Log.LLMHTTPRetentionDays != agent.DefaultConfig().Log.LLMHTTPRetentionDaysOrDefault() {
		t.Fatalf("log.llm_http_retention_days = %d, want %d",
			dto.Log.LLMHTTPRetentionDays, agent.DefaultConfig().Log.LLMHTTPRetentionDaysOrDefault())
	}
	if dto.Agent.Prompt != "" {
		t.Fatalf("prompt = %q, want empty wire value for a config that sets no prompt", dto.Agent.Prompt)
	}
	if err := requireNoLegacyInstructionKeys(dto); err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal resolved config: %v", err)
	}
	result, err := checkConfig(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("checkConfig(resolved config) decode error: %v", err)
	}
	if !result.Valid {
		t.Fatalf("resolved default config is invalid: %+v", result.Errors)
	}
}

func TestResolvedWebConfigDTOReadsInvalidConfigForRecovery(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte("[voice_settings.mode]\ninput_mode = \"realtime\"\n[model_settings.model]\nprovider = \"fake\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	dto, err := resolvedWebConfigDTO(path)
	if err != nil {
		t.Fatalf("resolvedWebConfigDTO() error = %v", err)
	}
	if dto.Agent.InputMode != "realtime" {
		t.Fatalf("agent.input_mode = %q, want realtime", dto.Agent.InputMode)
	}
}

// requireNoLegacyInstructionKeys fails when the resolved wire config exposes a
// custom_instruction (or legacy instruction) field. The built-in Agent
// instruction is runtime content and has no editable wire field.
func requireNoLegacyInstructionKeys(dto webConfigDTO) error {
	encoded, err := json.Marshal(dto)
	if err != nil {
		return fmt.Errorf("marshal resolved config: %w", err)
	}
	var sections map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &sections); err != nil {
		return fmt.Errorf("decode resolved config: %w", err)
	}
	var agentSection map[string]json.RawMessage
	if err := json.Unmarshal(sections["agent"], &agentSection); err != nil {
		return fmt.Errorf("decode agent section: %w", err)
	}
	for _, key := range []string{"custom_instruction", "instruction"} {
		if _, ok := agentSection[key]; ok {
			return fmt.Errorf("resolved wire config exposes removed field agent.%s: %s", key, encoded)
		}
	}
	return nil
}

// TestResolvedWebConfigDTO_IgnoresLegacyInstructionKeys covers the removed
// custom_instruction setting. A file that still carries the key (the grouped
// custom_instruction or the older flat instruction) must keep loading, must not
// expose the field on the wire, and must not override the built-in instruction.
func TestResolvedWebConfigDTO_IgnoresLegacyInstructionKeys(t *testing.T) {
	for name, key := range map[string]string{
		"custom_instruction": "custom_instruction",
		"legacy instruction": "instruction",
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, "agent.toml")
			body := `
[conversation_settings.agent]
` + key + ` = "Use a deployment-specific persona."

[model_settings.model]
provider = "fake"
`
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatalf("write config: %v", err)
			}

			dto, err := resolvedWebConfigDTO(path)
			if err != nil {
				t.Fatalf("resolvedWebConfigDTO() error = %v", err)
			}
			if err := requireNoLegacyInstructionKeys(dto); err != nil {
				t.Fatal(err)
			}

			cfg, err := agent.LoadResolvedConfig(path)
			if err != nil {
				t.Fatalf("LoadResolvedConfig() error = %v", err)
			}
			if cfg.Instruction != agent.DefaultConfig().Instruction {
				t.Fatalf("Instruction = %q, want built-in default because legacy %s is ignored", cfg.Instruction, key)
			}
		})
	}
}

func TestResolvedWebConfigDTO_OverlaysCurrentConfig(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(path, []byte(`
	[voice_settings.classic.runtime]
voice_followup_enabled = true

[model_settings.model]
provider = "openai"
model = "gpt-4o-mini"

[advanced_settings.hardware.hid]
pointer_mode = "touchscreen"
keyboard_layout = "azerty"

[advanced_settings.log]
llm_http_retention_days = 14
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	dto, err := resolvedWebConfigDTO(path)
	if err != nil {
		t.Fatalf("resolvedWebConfigDTO() error = %v", err)
	}
	if dto.Model.Provider != "openai" || dto.Model.Model != "gpt-4o-mini" {
		t.Fatalf("model overlay = provider %q model %q", dto.Model.Provider, dto.Model.Model)
	}
	if dto.Device.DeviceType != "iOS" {
		t.Fatalf("device.device_type = %q, want iOS (default when unset)", dto.Device.DeviceType)
	}
	if dto.HID.PointerMode != "absolute" {
		t.Fatalf("hid.pointer_mode = %q, want absolute (derived from iOS device_type)", dto.HID.PointerMode)
	}
	if dto.HID.KeyboardLayout != "azerty" {
		t.Fatalf("hid.keyboard_layout = %q, want azerty", dto.HID.KeyboardLayout)
	}
	if !dto.Agent.VoiceFollowupEnabled {
		t.Fatal("agent.voice_followup_enabled = false, want true from current config")
	}
	if dto.HID.KeyboardDevice != agent.DefaultConfig().HID.KeyboardDevice {
		t.Fatalf("hid.keyboard_device = %q, want default %q",
			dto.HID.KeyboardDevice, agent.DefaultConfig().HID.KeyboardDevice)
	}
	if dto.Audio.Socket != agent.DefaultConfig().Audio.Socket {
		t.Fatalf("audio.socket = %q, want default %q",
			dto.Audio.Socket, agent.DefaultConfig().Audio.Socket)
	}
	if dto.Audio.Backend != agent.AudioBackendAuto {
		t.Fatalf("audio.backend = %q, want auto", dto.Audio.Backend)
	}
	if dto.Log.LLMHTTPRetentionDays != 14 {
		t.Fatalf("log.llm_http_retention_days = %d, want 14", dto.Log.LLMHTTPRetentionDays)
	}
	if !dto.AudioArchive.Enabled || dto.AudioArchive.StoragePath != agent.DefaultConfig().AudioArchive.StoragePath {
		t.Fatalf("audio_archive defaults = %+v, want enabled default storage", dto.AudioArchive)
	}
	if !dto.QuickCapture.Enabled || dto.QuickCapture.GPIOPin != agent.DefaultConfig().QuickCapture.GPIOPin ||
		dto.QuickCapture.ScreenMemoryTTL != agent.DefaultScreenMemoryTTL {
		t.Fatalf("quick_capture defaults = %+v", dto.QuickCapture)
	}
}

func TestWebConfigDTOFromAgentConfig_UsesRuntimeDefaults(t *testing.T) {
	defaults := webConfigDTOFromAgentConfig(agent.Config{})
	if defaults.Search.Provider != "duckduckgo" {
		t.Fatalf("search provider = %q, want duckduckgo", defaults.Search.Provider)
	}
	if defaults.Audio.Socket == "" || defaults.Audio.SampleRate == 0 ||
		defaults.Audio.Channels == 0 || defaults.Audio.BitWidth == 0 ||
		defaults.Audio.Backend == "" {
		t.Fatalf("audio defaults were not populated: %+v", defaults.Audio)
	}
	if defaults.AudioArchive.Enabled || defaults.AudioArchive.StoragePath == "" ||
		defaults.AudioArchive.MaxFiles == 0 || defaults.AudioArchive.MaxSizeMB == 0 {
		t.Fatalf("audio archive zero config conversion = %+v, want disabled with path and retention defaults", defaults.AudioArchive)
	}
	if defaults.Device.DeviceType == "" {
		t.Fatalf("device defaults were not populated: %+v", defaults.Device)
	}
	if defaults.HID.FrameSocket == "" || defaults.HID.KeyboardDevice == "" ||
		defaults.HID.MouseDevice == "" || defaults.HID.AndroidKeyboardDevice == "" ||
		defaults.HID.PointerMode == "" || defaults.HID.KeyboardLayout == "" {
		t.Fatalf("hid defaults were not populated: %+v", defaults.HID)
	}
	if defaults.Log.LLMHTTPRetentionDays != agent.DefaultConfig().Log.LLMHTTPRetentionDaysOrDefault() {
		t.Fatalf("log defaults were not populated: %+v", defaults.Log)
	}
	if defaults.Agent.InputMode != "" {
		t.Fatalf("agent input mode default = %q, want empty (unconfigured)", defaults.Agent.InputMode)
	}
	if defaults.Agent.VoiceFollowupTimeoutMs == 0 ||
		defaults.Agent.VoiceFirstTurnTimeoutMs == 0 ||
		defaults.Agent.VoiceMaxResponseTokens == 0 {
		t.Fatalf("voice defaults were not populated: %+v", defaults.Agent)
	}
}

func TestWebConfigDTOFromAgentConfigDoesNotInferAudioArchiveEnabled(t *testing.T) {
	roundTrip := webConfigDTOFromAgentConfig(agent.Config{AudioArchive: agent.AudioArchiveConfig{Enabled: false}})
	if roundTrip.AudioArchive.Enabled {
		t.Fatal("AudioArchive.Enabled = true, want explicit disabled zero-value config to stay disabled")
	}
}

func TestWebConfigDTOMapsTTSReferenceID(t *testing.T) {
	const referenceID = "fish-reference-id"
	dto := webConfigDTO{TTS: ttsDTO{Provider: "fish-audio", ReferenceID: referenceID}}
	if got := dto.ToAgentConfig().TTS.ReferenceID; got != referenceID {
		t.Fatalf("TTS.ReferenceID = %q, want %q", got, referenceID)
	}
	if got := webConfigDTOFromAgentConfig(agent.Config{TTS: agent.TTSConfig{Provider: "fish-audio", ReferenceID: referenceID}}).TTS.ReferenceID; got != referenceID {
		t.Fatalf("round-trip TTS.ReferenceID = %q, want %q", got, referenceID)
	}
}

func TestWebConfigDTOFromAgentConfig_RedactsSearchAPIKey(t *testing.T) {
	dto := webConfigDTOFromAgentConfig(agent.Config{
		Search: agent.SearchConfig{
			Provider: "brave",
			APIKey:   "search-test-key",
		},
	})
	if dto.Search.APIKey != "" {
		t.Fatalf("search api key was exposed in DTO: %q", dto.Search.APIKey)
	}
	if !dto.Search.HasAPIKey {
		t.Fatal("search has_api_key = false, want true for stored API key")
	}
}

func TestWebConfigDTOFromAgentConfig_RedactsProviderCredentials(t *testing.T) {
	dto := webConfigDTOFromAgentConfig(agent.Config{
		ModelProviders: map[string]agent.ModelProvider{
			"model-main": {Type: "openai", APIKey: "model-secret"},
		},
		TTSProviders: map[string]agent.TTSProvider{
			"tts-main": {Type: "fish-audio", APIKey: "tts-secret"},
		},
		STTProviders: map[string]agent.STTProvider{
			"stt-main": {
				Type: "tencent-asr", APIKey: "stt-secret",
				SecretID: "secret-id", SecretKey: "secret-key",
			},
		},
	})

	if got := dto.ModelProviders["model-main"]; got.APIKey != "" || !got.HasAPIKey {
		t.Fatalf("model provider credential DTO = %+v, want redacted configured state", got)
	}
	if got := dto.TTSProviders["tts-main"]; got.APIKey != "" || !got.HasAPIKey {
		t.Fatalf("TTS provider credential DTO = %+v, want redacted configured state", got)
	}
	if got := dto.STTProviders["stt-main"]; got.APIKey != "" || !got.HasAPIKey ||
		got.SecretID != "" || !got.HasSecretID || got.SecretKey != "" || !got.HasSecretKey {
		t.Fatalf("STT provider credential DTO = %+v, want redacted configured state", got)
	}

	data, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal config DTO: %v", err)
	}
	for _, secret := range []string{"model-secret", "tts-secret", "stt-secret", "secret-id", "secret-key"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("provider secret %q leaked in JSON: %s", secret, data)
		}
	}
}

func TestWebConfigDTOMapsAudioArchive(t *testing.T) {
	dto := webConfigDTO{
		AudioArchive: audioArchiveDTO{
			Enabled:     false,
			MaxFiles:    42,
			MaxSizeMB:   17,
			StoragePath: "/tmp/audio-archive",
		},
	}
	cfg := dto.ToAgentConfig()
	if cfg.AudioArchive.Enabled {
		t.Fatal("AudioArchive.Enabled = true, want false")
	}
	if cfg.AudioArchive.MaxFiles != 42 || cfg.AudioArchive.MaxSizeMB != 17 || cfg.AudioArchive.StoragePath != "/tmp/audio-archive" {
		t.Fatalf("AudioArchive = %+v, want DTO values", cfg.AudioArchive)
	}

	roundTrip := webConfigDTOFromAgentConfig(agent.Config{AudioArchive: cfg.AudioArchive})
	if roundTrip.AudioArchive.Enabled || roundTrip.AudioArchive.MaxFiles != 42 ||
		roundTrip.AudioArchive.MaxSizeMB != 17 || roundTrip.AudioArchive.StoragePath != "/tmp/audio-archive" {
		t.Fatalf("round-trip AudioArchive = %+v, want DTO values", roundTrip.AudioArchive)
	}
}

func TestWebConfigDTOMapsAudioBackend(t *testing.T) {
	dto := webConfigDTO{
		Audio: audioDTO{
			Socket:     "/tmp/audio.sock",
			SampleRate: 24000,
			Channels:   1,
			BitWidth:   16,
			Backend:    agent.AudioBackendLocal,
		},
	}
	cfg := dto.ToAgentConfig()
	if cfg.Audio.Backend != agent.AudioBackendLocal {
		t.Fatalf("Audio.Backend = %q, want local", cfg.Audio.Backend)
	}
	roundTrip := webConfigDTOFromAgentConfig(agent.Config{Audio: cfg.Audio})
	if roundTrip.Audio.Backend != agent.AudioBackendLocal {
		t.Fatalf("round-trip audio.backend = %q, want local", roundTrip.Audio.Backend)
	}

	autoDTO := webConfigDTO{
		Audio: audioDTO{
			Socket:     "/tmp/audio.sock",
			SampleRate: 24000,
			Channels:   1,
			BitWidth:   16,
			Backend:    agent.AudioBackendAuto,
		},
	}
	autoCfg := autoDTO.ToAgentConfig()
	if autoCfg.Audio.Backend != agent.AudioBackendAuto {
		t.Fatalf("auto Audio.Backend = %q, want auto", autoCfg.Audio.Backend)
	}
	autoRoundTrip := webConfigDTOFromAgentConfig(agent.Config{
		Audio: autoCfg.Audio,
		HID:   agent.HIDConfig{InputBackend: "adb"},
	})
	if autoRoundTrip.Audio.Backend != agent.AudioBackendAuto {
		t.Fatalf("auto round-trip audio.backend = %q, want auto", autoRoundTrip.Audio.Backend)
	}
}

func TestWebConfigDTOMapsQuickCapture(t *testing.T) {
	dto := webConfigDTO{QuickCapture: quickCaptureDTO{
		Enabled:         false,
		GPIOPin:         3,
		ScreenMemoryTTL: "14d",
	}}
	cfg := dto.ToAgentConfig()
	if cfg.QuickCapture.EnabledOrDefault() || cfg.QuickCapture.GPIOPin != 3 || cfg.QuickCapture.ScreenMemoryTTL != "14d" {
		t.Fatalf("QuickCapture = %+v, want DTO values", cfg.QuickCapture)
	}

	roundTrip := webConfigDTOFromAgentConfig(agent.Config{QuickCapture: cfg.QuickCapture})
	if roundTrip.QuickCapture != dto.QuickCapture {
		t.Fatalf("round-trip QuickCapture = %+v, want %+v", roundTrip.QuickCapture, dto.QuickCapture)
	}
}

func TestWebConfigDTOMapsVoiceNotifications(t *testing.T) {
	enabled := false
	tailEnabled := false
	voiceNotifications := agent.VoiceNotificationsConfig{
		Enabled:       &enabled,
		MaxPending:    6,
		RetentionDays: 14,
		ResponseTail: agent.VoiceNotificationResponseTailConfig{
			Enabled:      &tailEnabled,
			MaxItems:     1,
			MaxTextChars: 72,
		},
		Expiration: agent.VoiceNotificationExpirationConfig{
			DefaultTTLSeconds: 120,
			CodeTTLSeconds: map[string]int{
				"storage": 900,
			},
		},
	}

	dto := webConfigDTOFromAgentConfig(agent.Config{VoiceNotifications: voiceNotifications})
	cfg := dto.ToAgentConfig()
	if !reflect.DeepEqual(cfg.VoiceNotifications, voiceNotifications) {
		t.Fatalf("VoiceNotifications = %#v, want %#v", cfg.VoiceNotifications, voiceNotifications)
	}
}

func TestWebConfigDTOMapsLog(t *testing.T) {
	dto := webConfigDTO{
		Model: modelDTO{Provider: "fake"},
		Log:   logDTO{LLMHTTPRetentionDays: 21},
	}

	cfg := dto.ToAgentConfig()
	if cfg.Log.LLMHTTPRetentionDays != 21 {
		t.Fatalf("Log.LLMHTTPRetentionDays = %d, want 21", cfg.Log.LLMHTTPRetentionDays)
	}

	roundTrip := webConfigDTOFromAgentConfig(agent.Config{Log: agent.LogConfig{LLMHTTPRetentionDays: 21}})
	if roundTrip.Log.LLMHTTPRetentionDays != 21 {
		t.Fatalf("round-trip log.llm_http_retention_days = %d, want 21", roundTrip.Log.LLMHTTPRetentionDays)
	}
}

func TestWebConfigDTOMapsTerminationPolicy(t *testing.T) {
	enabled := false
	policy := agent.TerminationPolicyConfig{
		Enabled:                 &enabled,
		MaxSeconds:              12.5,
		RepeatActionLimit:       7,
		SameResultLimit:         8,
		ScreenUnchangedLimit:    9,
		SoftNoticeStallScore:    10,
		RestrictToolsStallScore: 11,
		TerminateStallScore:     12,
		ParseFailureLimit:       13,
	}
	dto := webConfigDTO{TerminationPolicy: policy}
	if got := dto.ToAgentConfig().TerminationPolicy; !reflect.DeepEqual(got, policy) {
		t.Fatalf("TerminationPolicy = %#v, want %#v", got, policy)
	}
	if got := webConfigDTOFromAgentConfig(agent.Config{TerminationPolicy: policy}).TerminationPolicy; !reflect.DeepEqual(got, policy) {
		t.Fatalf("round-trip TerminationPolicy = %#v, want %#v", got, policy)
	}
}

func TestWebConfigDTOMapsSTTLanguage(t *testing.T) {
	dto := webConfigDTO{
		STT: sttDTO{
			Provider: "openai-whisper",
			Language: "en",
			AppID:    "12345",
		},
	}

	cfg := dto.ToAgentConfig()
	if cfg.STT.Language != "en" {
		t.Fatalf("STT.Language = %q, want en", cfg.STT.Language)
	}
	if cfg.STT.AppID != "12345" {
		t.Fatalf("STT.AppID = %q, want 12345", cfg.STT.AppID)
	}

	roundTrip := webConfigDTOFromAgentConfig(agent.Config{
		STT: agent.STTConfig{
			Provider: "openai-whisper",
			Language: "zh",
			AppID:    "67890",
		},
	})
	if roundTrip.STT.Language != "zh" {
		t.Fatalf("round-trip stt.language = %q, want zh", roundTrip.STT.Language)
	}
	if roundTrip.STT.AppID != "67890" {
		t.Fatalf("round-trip stt.app_id = %q, want 67890", roundTrip.STT.AppID)
	}
}

func TestConfigCheck_InvalidSearchProvider(t *testing.T) {
	invalidConfig := agent.Config{
		Model: agent.ModelConfig{
			Provider: "openai",
			Model:    "gpt-4",
		},
		Search: agent.SearchConfig{
			Provider: "google", // Invalid provider
		},
	}

	err := invalidConfig.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid search provider, got nil")
	}

	errors := parseValidationErrors(err)
	if len(errors) == 0 {
		t.Fatal("expected at least one validation error")
	}

	// Check that the error mentions the search provider
	errorMsg := errors[0].Message
	if !strings.Contains(strings.ToLower(errorMsg), "search") &&
		!strings.Contains(strings.ToLower(errorMsg), "provider") {
		t.Errorf("expected error message to mention search provider, got: %s", errorMsg)
	}

	// Check that field is extracted
	if errors[0].Field != "search.provider" && errors[0].Field != "" {
		t.Logf("field extraction: got %q, expected %q", errors[0].Field, "search.provider")
	}

	t.Logf("Invalid provider error: %s", errorMsg)
}

func TestConfigCheck_InvalidVADThreshold(t *testing.T) {
	invalidConfig := agent.Config{
		Model: agent.ModelConfig{
			Provider: "openai",
			Model:    "gpt-4",
		},
		Search: agent.SearchConfig{
			Provider: "duckduckgo",
		},
		VADSpeechThreshold: 1.5, // Invalid: must be in [0, 1]
	}

	err := invalidConfig.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid VAD threshold, got nil")
	}

	errors := parseValidationErrors(err)
	if len(errors) == 0 {
		t.Fatal("expected at least one validation error")
	}

	errorMsg := errors[0].Message
	if !strings.Contains(errorMsg, "vad_speech_threshold") {
		t.Errorf("expected error message to mention vad_speech_threshold, got: %s", errorMsg)
	}

	t.Logf("Invalid VAD threshold error: %s", errorMsg)
}

func TestConfigCheck_MissingModelProvider(t *testing.T) {
	invalidConfig := agent.Config{
		Model: agent.ModelConfig{
			// Provider missing
			Model: "gpt-4",
		},
		Search: agent.SearchConfig{
			Provider: "duckduckgo",
		},
	}

	err := invalidConfig.Validate()
	if err == nil {
		t.Fatal("expected validation error for missing model provider, got nil")
	}

	errors := parseValidationErrors(err)
	if len(errors) == 0 {
		t.Fatal("expected at least one validation error")
	}

	errorMsg := errors[0].Message
	if !strings.Contains(errorMsg, "model.provider") {
		t.Errorf("expected error message to mention model.provider, got: %s", errorMsg)
	}

	t.Logf("Missing model provider error: %s", errorMsg)
}

func TestConfigCheck_InvalidInputMode(t *testing.T) {
	invalidConfig := agent.Config{
		Model: agent.ModelConfig{
			Provider: "openai",
			Model:    "gpt-4",
		},
		Search: agent.SearchConfig{
			Provider: "duckduckgo",
		},
		InputMode: "invalid_mode", // Invalid: must be text/audio/stt
	}

	err := invalidConfig.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid input mode, got nil")
	}

	errors := parseValidationErrors(err)
	if len(errors) == 0 {
		t.Fatal("expected at least one validation error")
	}

	errorMsg := errors[0].Message
	if !strings.Contains(errorMsg, "input_mode") {
		t.Errorf("expected error message to mention input_mode, got: %s", errorMsg)
	}

	t.Logf("Invalid input mode error: %s", errorMsg)
}

func TestConfigCheck_NegativeVoiceMaxTurns(t *testing.T) {
	invalidConfig := agent.Config{
		Model: agent.ModelConfig{
			Provider: "openai",
			Model:    "gpt-4",
		},
		Search: agent.SearchConfig{
			Provider: "duckduckgo",
		},
		VoiceMaxTurns: -5, // Invalid: must be >= 0
	}

	err := invalidConfig.Validate()
	if err == nil {
		t.Fatal("expected validation error for negative voice_max_turns, got nil")
	}

	errors := parseValidationErrors(err)
	if len(errors) == 0 {
		t.Fatal("expected at least one validation error")
	}

	errorMsg := errors[0].Message
	if !strings.Contains(errorMsg, "voice_max_turns") {
		t.Errorf("expected error message to mention voice_max_turns, got: %s", errorMsg)
	}

	t.Logf("Negative voice_max_turns error: %s", errorMsg)
}

func TestConfigCheck_InvalidMaxIterations(t *testing.T) {
	invalidConfig := agent.Config{
		Model: agent.ModelConfig{
			Provider: "openai",
			Model:    "gpt-4",
		},
		Search: agent.SearchConfig{
			Provider: "duckduckgo",
		},
		MaxIterations: -2, // Invalid: must be >= -1 (-1 means unlimited)
	}

	err := invalidConfig.Validate()
	if err == nil {
		t.Fatal("expected validation error for max_iterations < -1, got nil")
	}

	errors := parseValidationErrors(err)
	if len(errors) == 0 {
		t.Fatal("expected at least one validation error")
	}

	errorMsg := errors[0].Message
	if !strings.Contains(errorMsg, "max_iterations") {
		t.Errorf("expected error message to mention max_iterations, got: %s", errorMsg)
	}

	t.Logf("Invalid max_iterations error: %s", errorMsg)
}

func TestConfigCheck_UnlimitedMaxIterations(t *testing.T) {
	// -1 is the sentinel for "unlimited" and must be accepted.
	validConfig := agent.Config{
		Model: agent.ModelConfig{
			Provider: "openai",
			Model:    "gpt-4",
		},
		Search: agent.SearchConfig{
			Provider: "duckduckgo",
		},
		MaxIterations: -1,
	}

	if err := validConfig.Validate(); err != nil {
		t.Errorf("max_iterations=-1 (unlimited) should be valid, got: %v", err)
	}
}

func TestConfigCheck_InvalidDeviceType(t *testing.T) {
	invalidConfig := agent.Config{
		Model: agent.ModelConfig{
			Provider: "openai",
			Model:    "gpt-4",
		},
		Search: agent.SearchConfig{
			Provider: "duckduckgo",
		},
		Device: agent.DeviceConfig{
			DeviceType: "blackberry",
		},
	}

	err := invalidConfig.Validate()
	if err == nil {
		t.Fatal("expected validation error for invalid device.device_type, got nil")
	}

	errors := parseValidationErrors(err)
	if len(errors) == 0 {
		t.Fatal("expected at least one validation error")
	}

	errorMsg := errors[0].Message
	if !strings.Contains(errorMsg, "device_type") {
		t.Errorf("expected error message to mention device_type, got: %s", errorMsg)
	}

	t.Logf("Invalid device_type error: %s", errorMsg)
}

func TestConfigCheck_ValidDeviceTypes(t *testing.T) {
	for _, deviceType := range []string{"", "iOS", "Android", "macOS", "windows", "linux", "ios", "ANDROID", "darwin"} {
		validConfig := agent.Config{
			Model: agent.ModelConfig{
				Provider: "openai",
				Model:    "gpt-4",
			},
			Search: agent.SearchConfig{
				Provider: "duckduckgo",
			},
			Device: agent.DeviceConfig{
				DeviceType: deviceType,
			},
		}

		if err := validConfig.Validate(); err != nil {
			t.Errorf("device_type=%q should be valid, got: %v", deviceType, err)
		}
	}
}

func TestParseValidationErrors_ExtractsField(t *testing.T) {
	testCases := []struct {
		name          string
		errorMsg      string
		expectedField string
	}{
		{
			name:          "search provider error",
			errorMsg:      "invalid search.provider: google (expected duckduckgo, brave, or tavily)",
			expectedField: "search.provider",
		},
		{
			// Validators echo the offending value, so a value that happens to spell
			// a field name must not steal that field's highlight.
			name:          "search provider value that names another field",
			errorMsg:      "invalid search.provider: timezone (expected duckduckgo, brave, or tavily)",
			expectedField: "search.provider",
		},
		{
			name:          "api mode value that names another field",
			errorMsg:      "invalid model.api_mode: timezone (expected chat_completions, responses, or responses_stateful)",
			expectedField: "model.api_mode",
		},
		{
			name:          "invalid locale",
			errorMsg:      "invalid locale: fr-FR (expected zh-CN or en-US)",
			expectedField: "locale",
		},
		{
			name:          "unsupported timezone",
			errorMsg:      "unsupported timezone: Mars/Olympus",
			expectedField: "timezone",
		},
		{
			name:          "unloadable timezone",
			errorMsg:      "load timezone Mars/Olympus: unknown time zone Mars/Olympus",
			expectedField: "timezone",
		},
		{
			name:          "model provider error",
			errorMsg:      "model.provider is required",
			expectedField: "model.provider",
		},
		{
			name:          "model api mode error",
			errorMsg:      "invalid model.api_mode: invalid (expected chat_completions, responses, responses_stateful, interactions, or interactions_stateful)",
			expectedField: "model.api_mode",
		},
		{
			name:          "model max response tokens error",
			errorMsg:      "model.max_response_tokens must be >= 0, got -1",
			expectedField: "model.max_response_tokens",
		},
		{
			name:          "model context window error",
			errorMsg:      "model.context_window must be >= 0, got -1",
			expectedField: "model.context_window",
		},
		{
			name:          "model max output tokens error",
			errorMsg:      "model.model_max_output_tokens must be >= 0, got -1",
			expectedField: "model.model_max_output_tokens",
		},
		{
			name:          "vad threshold error",
			errorMsg:      "vad_speech_threshold must be in [0,1] when set, got 1.5",
			expectedField: "vad_speech_threshold",
		},
		{
			name:          "telemetry base_url error",
			errorMsg:      "telemetry.base_url is required when telemetry.enabled=true",
			expectedField: "telemetry.base_url",
		},
		{
			name:          "wrapped model provider error",
			errorMsg:      "model: provider is required",
			expectedField: "model.provider",
		},
		{
			name:          "realtime voice key error",
			errorMsg:      "voice_model.api_key is required when input_mode=realtime",
			expectedField: "voice_model.api_key",
		},
		{
			// The Responses API knobs are rendered fields, and each one reports its
			// own path first, so the recovery page can highlight it.
			name:          "responses knob choice error",
			errorMsg:      "invalid model.responses_context_management: bogus (expected empty, compaction, ark_context_edit, or disabled)",
			expectedField: "model.responses_context_management",
		},
		{
			name:          "responses knob range error",
			errorMsg:      "model.responses_compact_threshold must be 0 or >= 1000, got 12",
			expectedField: "model.responses_compact_threshold",
		},
		{
			name:          "responses include list error",
			errorMsg:      "model.responses_include entries must be non-empty",
			expectedField: "model.responses_include",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			// Create a dummy error from the message
			err := &validationError{msg: tc.errorMsg}
			errors := parseValidationErrors(err)
			if len(errors) == 0 {
				t.Fatal("expected at least one error")
			}

			if errors[0].Field != tc.expectedField {
				t.Errorf("expected field %q, got %q", tc.expectedField, errors[0].Field)
			}

			if errors[0].Message != tc.errorMsg {
				t.Errorf("expected message %q, got %q", tc.errorMsg, errors[0].Message)
			}
		})
	}
}

// validationError is a helper for testing
type validationError struct {
	msg string
}

func (e *validationError) Error() string {
	return e.msg
}

func TestValidationResult_JSONFormat(t *testing.T) {
	result := ValidationResult{
		Valid: false,
		Errors: []ValidationError{
			{
				Field:   "search.provider",
				Message: "invalid provider 'google'",
			},
			{
				Field:   "vad_speech_threshold",
				Message: "must be in [0,1]",
			},
		},
	}

	data, err := json.Marshal(result)
	if err != nil {
		t.Fatalf("failed to marshal result: %v", err)
	}

	var decoded ValidationResult
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("failed to unmarshal result: %v", err)
	}

	if decoded.Valid != result.Valid {
		t.Errorf("expected valid=%v, got valid=%v", result.Valid, decoded.Valid)
	}

	if len(decoded.Errors) != len(result.Errors) {
		t.Fatalf("expected %d errors, got %d", len(result.Errors), len(decoded.Errors))
	}

	for i, err := range decoded.Errors {
		if err.Field != result.Errors[i].Field {
			t.Errorf("error[%d]: expected field=%q, got field=%q", i, result.Errors[i].Field, err.Field)
		}
		if err.Message != result.Errors[i].Message {
			t.Errorf("error[%d]: expected message=%q, got message=%q", i, result.Errors[i].Message, err.Message)
		}
	}

	t.Logf("JSON output: %s", string(data))
}

// The section Test buttons are most useful while the persisted config is in
// recovery: the flagged field is what the user is testing a replacement for. So
// config-test must load the surrounding context without requiring the file to
// pass semantic validation, while still refusing a file it cannot decode.
//
// The request carries no values on purpose: the command then stops at its own
// argument check, which tells the two load outcomes apart without reaching a
// provider.
func TestRunConfigTestLoadsRecoveryConfigAndRejectsDamagedFiles(t *testing.T) {
	dir := t.TempDir()

	recovery := filepath.Join(dir, "agent.toml")
	if err := os.WriteFile(recovery, []byte(`[model_settings.model]
provider = "openai"
model = "gpt-4o"

[voice_settings.mode]
input_mode = "stt"
`), 0o640); err != nil {
		t.Fatal(err)
	}
	// The runtime rejects this file, which is what puts the portal in recovery.
	if _, err := agent.LoadRuntimeConfig(recovery); err == nil {
		t.Fatal("expected the runtime loader to reject a declared stt mode with no provider")
	}

	result, code := runConfigTestWithStdin(t, recovery, `{"section":"model"}`)
	if check := configTestCheck(result); check == "load_config" {
		t.Fatalf("config-test refused a recoverable config: %+v", result.Results)
	}
	if check := configTestCheck(result); check != "request" || code != 1 {
		t.Fatalf("config-test did not stop at its request check: check=%q code=%d", check, code)
	}

	damaged := filepath.Join(dir, "damaged.toml")
	if err := os.WriteFile(damaged, []byte("[broken"), 0o640); err != nil {
		t.Fatal(err)
	}
	result, _ = runConfigTestWithStdin(t, damaged, `{"section":"model"}`)
	if check := configTestCheck(result); check != "load_config" {
		t.Fatalf("config-test accepted a config it cannot decode: %+v", result.Results)
	}
}

func configTestCheck(result ConfigTestResult) string {
	if len(result.Results) == 0 {
		return ""
	}
	return result.Results[0].Check
}

// runConfigTestWithStdin drives the subcommand the way the config page does, and
// returns its exit code with the decoded result it printed.
func runConfigTestWithStdin(t *testing.T, configPath, request string) (ConfigTestResult, int) {
	t.Helper()
	stdinReader, stdinWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdin, originalStdout := os.Stdin, os.Stdout
	os.Stdin, os.Stdout = stdinReader, stdoutWriter
	defer func() {
		os.Stdin, os.Stdout = originalStdin, originalStdout
	}()
	go func() {
		_, _ = stdinWriter.WriteString(request)
		_ = stdinWriter.Close()
	}()
	code := runConfigTest([]string{"--stdin", "--config=" + configPath})
	_ = stdoutWriter.Close()
	output, err := io.ReadAll(stdoutReader)
	if err != nil {
		t.Fatal(err)
	}
	var result ConfigTestResult
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatalf("config-test output %q: %v", string(output), err)
	}
	return result, code
}

// The Config Web recovery portal and `agent config-check --config` read different
// views of the same file on purpose: the portal uses the runtime loader because it
// describes the state the Agent boots in, while this command is the CI and release
// gate and stays strict, so a config the runtime would silently paper over still
// fails here. They must not disagree about *which* field is wrong, or the gate's
// output sends a user to a value that is not the problem.
//
// This drives the exported command the gates run, rather than its loader step.
func TestRunConfigCheckReportsTheFieldTheRuntimeVerdictNames(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	body := `[model_settings.model]
provider = "fake"

[voice_settings.mode]
input_mode = "stt"
`
	if err := os.WriteFile(path, []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}

	// What the portal reports for this file.
	_, runtimeErr := agent.LoadRuntimeConfig(path)
	if runtimeErr == nil {
		t.Fatal("the runtime loader accepted a declared stt mode with no provider")
	}
	want := agent.ParseConfigValidationErrors(runtimeErr)
	if len(want) != 1 || want[0].Field != "stt.provider" {
		t.Fatalf("runtime verdict = %+v, want the missing stt provider flagged", want)
	}

	got := checkConfigPath(path)
	if got.Valid || len(got.Errors) != 1 || got.Errors[0].Field != want[0].Field {
		t.Fatalf("checkConfigPath = %+v, want the same field as the runtime verdict %+v", got, want)
	}

	// The gate branches on the exit code, so it has to be non-zero here.
	stdoutReader, stdoutWriter, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	originalStdout := os.Stdout
	os.Stdout = stdoutWriter
	code := RunConfigCheck([]string{"--config=" + path, "--format=json"})
	_ = stdoutWriter.Close()
	os.Stdout = originalStdout
	if _, err := io.ReadAll(stdoutReader); err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		t.Fatal("config-check exited 0 for a config the release gate must reject")
	}
}
