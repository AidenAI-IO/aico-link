package main

import (
	"encoding/json"
	"strings"
	"testing"

	"aico-agent/internal/agent"
)

// Voice provider records must reach the wire, but their credentials are
// write-only. The browser receives configured-state flags and non-secret fields;
// submitted api_key/secret fields are still accepted by the decoder.
func TestConfigWire_VoiceProvidersRoundTrip(t *testing.T) {
	// has_api_key reports a $VAR reference only when the variable is set.
	t.Setenv("MINIMAX_KEY", "sk-from-env")
	t.Setenv("SPEKO_KEY", "sk-from-env")
	cfg := agent.Config{
		TTSProviders: map[string]agent.TTSProvider{
			"minimax-main": {Type: "minimax", APIKey: "sk-aaa", VoiceID: "male-qn-qingse", Emotion: "happy"},
			"fish":         {Type: "fish-audio", APIKey: "sk-ccc", ReferenceID: "ref-abc"},
			"env-based":    {Type: "minimax", APIKey: "$MINIMAX_KEY"},
		},
		STTProviders: map[string]agent.STTProvider{
			"tencent": {Type: "tencent-asr", AppID: "123", SecretID: "AKID", SecretKey: "sec", Region: "ap-shanghai"},
			"whisper": {Type: "openai-whisper", APIKey: "sk-w", BaseURL: "https://api.openai.com/v1", Model: "whisper-1"},
		},
		VoiceModelProviders: map[string]agent.VoiceModelProvider{
			"qwen-main":    {Type: "qwen", APIKey: "qwen-secret", Model: "qwen-realtime", Voice: "longanqian"},
			"openai-proxy": {Type: "openai", APIKey: "openai-secret", Endpoint: "wss://gateway.example/realtime", RealtimeProtocol: "legacy"},
			"speko-main":   {Type: "speko", APIKey: "$SPEKO_KEY", UpstreamProvider: "xai", Model: "grok-voice-latest", Voice: "eve"},
			"vertex":       {Type: "gemini", APIKey: "$VERTEX_TOKEN", AuthMode: "vertex", ProjectID: "project-1", Location: "us-central1"},
		},
		TTS:        agent.TTSConfig{Provider: "minimax-main", Speed: 1.2},
		STT:        agent.STTConfig{Provider: "tencent", Language: "zh"},
		VoiceModel: agent.VoiceModelConfig{Provider: "speko-main"},
		Model:      agent.ModelConfig{Provider: "openai", Model: "gpt-4o"},
	}

	dto := webConfigDTOFromAgentConfig(cfg)
	if len(dto.TTSProviders) != 3 {
		t.Fatalf("dto.TTSProviders = %#v, want 3 entries", dto.TTSProviders)
	}
	if len(dto.STTProviders) != 2 {
		t.Fatalf("dto.STTProviders = %#v, want 2 entries", dto.STTProviders)
	}
	if len(dto.VoiceModelProviders) != 4 {
		t.Fatalf("dto.VoiceModelProviders = %#v, want 4 entries", dto.VoiceModelProviders)
	}
	if got := dto.TTSProviders["fish"]; got.Type != "fish-audio" || got.ReferenceID != "ref-abc" {
		t.Errorf("dto.TTSProviders[fish] = %#v", got)
	}
	if got := dto.TTSProviders["env-based"]; got.APIKey != "" || !got.HasAPIKey {
		t.Errorf("environment credential was not redacted: %#v", got)
	}
	if got := dto.STTProviders["tencent"]; got.AppID != "123" || got.SecretID != "" ||
		!got.HasSecretID || got.SecretKey != "" || !got.HasSecretKey {
		t.Errorf("dto.STTProviders[tencent] = %#v", got)
	}
	if got := dto.VoiceModelProviders["speko-main"]; got.APIKey != "" || !got.HasAPIKey || got.UpstreamProvider != "xai" || got.Model != "grok-voice-latest" {
		t.Errorf("dto.VoiceModelProviders[speko-main] = %#v", got)
	}
	if got := dto.VoiceModelProviders["openai-proxy"]; got.RealtimeProtocol != "legacy" || got.Endpoint != "wss://gateway.example/realtime" {
		t.Errorf("dto.VoiceModelProviders[openai-proxy] = %#v", got)
	}
	if got := dto.VoiceModelProviders["vertex"]; got.AuthMode != "vertex" || got.ProjectID != "project-1" || got.Location != "us-central1" {
		t.Errorf("dto.VoiceModelProviders[vertex] = %#v", got)
	}

	back := dto.ToAgentConfig()
	if got := back.TTSProviders["fish"]; got.Type != "fish-audio" || got.ReferenceID != "ref-abc" || got.APIKey != hasAPIKeyPlaceholder {
		t.Errorf("redacted tts_providers conversion = %#v", back.TTSProviders)
	}
	if got := back.STTProviders["tencent"]; got.Type != "tencent-asr" || got.AppID != "123" ||
		got.SecretID != hasAPIKeyPlaceholder || got.SecretKey != hasAPIKeyPlaceholder {
		t.Errorf("redacted stt_providers conversion = %#v", back.STTProviders)
	}
	if got := back.VoiceModelProviders["speko-main"]; got.Type != "speko" || got.APIKey != hasAPIKeyPlaceholder || got.UpstreamProvider != "xai" {
		t.Errorf("redacted voice_model_providers conversion = %#v", back.VoiceModelProviders)
	}
	if got := back.VoiceModelProviders["openai-proxy"]; got.RealtimeProtocol != "legacy" || got.Endpoint != "wss://gateway.example/realtime" {
		t.Errorf("OpenAI compatibility fields conversion = %#v", got)
	}
	if got := back.VoiceModelProviders["vertex"]; got.AuthMode != "vertex" || got.ProjectID != "project-1" || got.Location != "us-central1" {
		t.Errorf("Vertex voice_model_providers conversion = %#v", back.VoiceModelProviders)
	}
	// The reference itself must not be resolved on the wire: the config page
	// edits the reference, so it has to come back as the name it wrote.
	if back.TTS.Provider != "minimax-main" {
		t.Errorf("tts.provider = %q, want the unresolved ref %q", back.TTS.Provider, "minimax-main")
	}
	if back.STT.Provider != "tencent" {
		t.Errorf("stt.provider = %q, want the unresolved ref %q", back.STT.Provider, "tencent")
	}
	if back.VoiceModel.Provider != "speko-main" {
		t.Errorf("voice_model.provider = %q, want the unresolved ref %q", back.VoiceModel.Provider, "speko-main")
	}
}

func TestConfigWire_VoiceProviderRecordsUseCanonicalType(t *testing.T) {
	payload := `{
		"tts_providers":{"voice":{"type":"fish-audio","api_key":"sk-v"}},
		"stt_providers":{"speech":{"type":"openai-whisper","api_key":"sk-s"}},
		"tts":{"provider":"voice"},
		"stt":{"provider":"speech"},
		"model":{"provider":"openai","model":"gpt-4o","api_key":"sk-x"},
		"search":{"provider":"duckduckgo"},
		"agent":{},
		"hid":{"pointer_mode":"absolute"}
	}`
	var dto webConfigDTO
	if err := json.Unmarshal([]byte(payload), &dto); err != nil {
		t.Fatalf("unmarshal canonical voice records: %v", err)
	}
	encoded, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal canonical voice records: %v", err)
	}
	output := string(encoded)
	if !strings.Contains(output, `"tts_providers":{"voice":{"type":"fish-audio"`) ||
		!strings.Contains(output, `"stt_providers":{"speech":{"type":"openai-whisper"`) {
		t.Errorf("canonical voice provider types missing: %s", output)
	}
	if strings.Contains(output, `"provider":"minimax"`) || strings.Contains(output, `"provider":"tencent-asr"`) {
		t.Errorf("legacy voice provider fields leaked or overrode type: %s", output)
	}

	result := checkWire(t, payload)
	if !result.Valid {
		t.Fatalf("canonical voice records rejected: %+v", result.Errors)
	}
}

func TestConfigWire_LegacyVoiceProviderRecordFieldsAreRejected(t *testing.T) {
	payload := `{
		"tts_providers":{"voice":{"provider":"fish-audio","api_key":"sk-v"}},
		"stt_providers":{"speech":{"provider":"openai-whisper","api_key":"sk-s"}},
		"tts":{"provider":"voice"},
		"stt":{"provider":"speech"},
		"model":{"provider":"openai","model":"gpt-4o","api_key":"sk-x"},
		"search":{"provider":"duckduckgo"},
		"agent":{},
		"hid":{"pointer_mode":"absolute"}
	}`
	_, err := checkConfig(strings.NewReader(payload))
	if err == nil {
		t.Fatal("legacy voice records were accepted")
	}
}

func TestConfigWire_CanonicalNullVoiceTypesDoNotUseLegacyAliases(t *testing.T) {
	var dto webConfigDTO
	payload := `{
		"tts_providers":{"voice":{"type":null,"provider":"fish-audio"}},
		"stt_providers":{"speech":{"type":null,"provider":"openai-whisper"}}
	}`
	if err := json.Unmarshal([]byte(payload), &dto); err == nil {
		t.Fatal("legacy provider aliases were accepted with null type")
	}
}

// omitempty on both maps, matching providers: a config with no records omits the
// key rather than emitting {}, and consumers of the resolved config treat a
// missing key as "no records" rather than an error.
func TestConfigWire_VoiceProvidersOmittedWhenEmpty(t *testing.T) {
	cfg := agent.Config{Model: agent.ModelConfig{Provider: "openai", Model: "gpt-4o", APIKey: "sk-x"}}
	payload, err := json.Marshal(webConfigDTOFromAgentConfig(cfg))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(payload), `"tts_providers"`) {
		t.Errorf("expected tts_providers omitted when empty, got: %s", payload)
	}
	if strings.Contains(string(payload), `"stt_providers"`) {
		t.Errorf("expected stt_providers omitted when empty, got: %s", payload)
	}
	if strings.Contains(string(payload), `"voice_model_providers"`) {
		t.Errorf("expected voice_model_providers omitted when empty, got: %s", payload)
	}

	cfg.TTSProviders = map[string]agent.TTSProvider{"fish": {Type: "fish-audio"}}
	cfg.STTProviders = map[string]agent.STTProvider{"w": {Type: "openai-whisper"}}
	cfg.VoiceModelProviders = map[string]agent.VoiceModelProvider{"realtime": {Type: "qwen"}}
	payload, err = json.Marshal(webConfigDTOFromAgentConfig(cfg))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"tts_providers"`) {
		t.Errorf("expected tts_providers in payload, got: %s", payload)
	}
	if !strings.Contains(string(payload), `"stt_providers"`) {
		t.Errorf("expected stt_providers in payload, got: %s", payload)
	}
	if !strings.Contains(string(payload), `"voice_model_providers"`) {
		t.Errorf("expected voice_model_providers in payload, got: %s", payload)
	}
}

func TestConfigWire_FlatVoiceCredentialsAreWriteOnly(t *testing.T) {
	cfg := agent.Config{
		Model: agent.ModelConfig{Provider: "openai", Model: "gpt-4o"},
		TTS: agent.TTSConfig{
			Provider: "voice",
			APIKey:   "flat-tts-secret",
		},
		STT: agent.STTConfig{
			Provider:  "speech",
			APIKey:    "flat-stt-secret",
			SecretID:  "flat-secret-id",
			SecretKey: "flat-secret-key",
		},
	}
	payload, err := json.Marshal(webConfigDTOFromAgentConfig(cfg))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var root map[string]map[string]json.RawMessage
	if err := json.Unmarshal(payload, &root); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	for _, field := range []string{"api_key"} {
		if _, ok := root["tts"][field]; ok {
			t.Errorf("tts.%s must be omitted from editor output: %s", field, payload)
		}
	}
	for _, field := range []string{"api_key", "secret_id", "secret_key"} {
		if _, ok := root["stt"][field]; ok {
			t.Errorf("stt.%s must be omitted from editor output: %s", field, payload)
		}
	}

	var submitted webConfigDTO
	input := `{"tts":{"api_key":"test-tts"},"stt":{"api_key":"test-stt","secret_id":"test-id","secret_key":"test-key"}}`
	if err := json.Unmarshal([]byte(input), &submitted); err != nil {
		t.Fatalf("decode internal test input: %v", err)
	}
	if submitted.TTS.APIKey != "test-tts" || submitted.STT.APIKey != "test-stt" ||
		submitted.STT.SecretID != "test-id" || submitted.STT.SecretKey != "test-key" {
		t.Errorf("internal flat credential decoding regressed: %+v %+v", submitted.TTS, submitted.STT)
	}
}

// A save that stores a dangling reference silently loses voice on the next
// restart, so config-check must reject it while the user is still on the form.
// Boot stays lenient on purpose -- see the agent package tests.
func TestConfigWire_DanglingVoiceRefRejectedOnSave(t *testing.T) {
	payload := `{
		"tts_providers":{"fish":{"type":"fish-audio"}},
		"tts":{"provider":"typo-name"},
		"model":{"provider":"openai","model":"gpt-4o","api_key":"sk-x"},
		"search":{"provider":"duckduckgo"},
		"agent":{},
		"hid":{"pointer_mode":"absolute"}
	}`
	result := checkWire(t, payload)
	if result.Valid {
		t.Fatal("expected a dangling tts.provider reference to be rejected on save")
	}
	if len(result.Errors) == 0 || !strings.Contains(result.Errors[0].Message, "typo-name") {
		t.Errorf("error should name the dangling ref, got: %+v", result.Errors)
	}
}

func TestConfigWire_UnsupportedVoiceProviderTypeRejectedOnSave(t *testing.T) {
	// openai is an LLM type; accepting it for TTS would save a config that can
	// never build a TTS adapter.
	payload := `{
		"tts_providers":{"bad":{"type":"openai"}},
		"tts":{"provider":"bad"},
		"model":{"provider":"openai","model":"gpt-4o","api_key":"sk-x"},
		"search":{"provider":"duckduckgo"},
		"agent":{},
		"hid":{"pointer_mode":"absolute"}
	}`
	result := checkWire(t, payload)
	if result.Valid {
		t.Fatal("expected an LLM provider type to be rejected for tts_providers")
	}

	payload = `{
		"stt_providers":{"bad":{"type":"minimax"}},
		"stt":{"provider":"bad"},
		"model":{"provider":"openai","model":"gpt-4o","api_key":"sk-x"},
		"search":{"provider":"duckduckgo"},
		"agent":{},
		"hid":{"pointer_mode":"absolute"}
	}`
	result = checkWire(t, payload)
	if result.Valid {
		t.Fatal("expected a TTS provider type to be rejected for stt_providers")
	}
}

// An unreferenced record is parked user data. It must not block a save of an
// unrelated section even when this build has no adapter for its provider type.
func TestConfigWire_UnreferencedVoiceRecordSaves(t *testing.T) {
	payload := `{
		"tts_providers":{"fish":{"type":"fish-audio"},"cartesia":{"type":"cartesia"}},
		"tts":{"provider":"fish"},
		"model":{"provider":"openai","model":"gpt-4o","api_key":"sk-x"},
		"search":{"provider":"duckduckgo"},
		"agent":{},
		"hid":{"pointer_mode":"absolute"}
	}`
	result := checkWire(t, payload)
	if !result.Valid {
		t.Errorf("unreferenced cartesia record should not block a save, got: %+v", result.Errors)
	}
}

// A bare provider type in [tts] keeps saving: devices in the field have flat
// configs and the config page must not reject them.
func TestConfigWire_BareVoiceProviderTypeSaves(t *testing.T) {
	payload := `{
		"tts":{"provider":"minimax-cn"},
		"stt":{"provider":"openai-whisper"},
		"model":{"provider":"openai","model":"gpt-4o","api_key":"sk-x"},
		"search":{"provider":"duckduckgo"},
		"agent":{},
		"hid":{"pointer_mode":"absolute"}
	}`
	result := checkWire(t, payload)
	if !result.Valid {
		t.Errorf("a bare provider type should still save, got: %+v", result.Errors)
	}
}
