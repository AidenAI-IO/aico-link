package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"aico-agent/internal/agent"
)

// These tests exercise the real config_web <-> agent wire contract: the JSON
// shape defined by webConfigDTO (snake_case keys, agent settings nested under
// "agent", search reporting only has_api_key). The PascalCase fixtures in
// config_commands_test.go validate Config.Validate() directly and never touch
// this decode path, so a contract drift between the config page and the Go
// decoder would pass there while silently accepting every invalid config in
// production. checkConfig() is the guard against that.

func checkWire(t *testing.T, payload string) ValidationResult {
	t.Helper()
	result, err := checkConfig(strings.NewReader(payload))
	if err != nil {
		t.Fatalf("checkConfig returned decode error for valid JSON: %v", err)
	}
	return result
}

// TestConfigCheck_WireFormatContract is the regression test for the bug where
// agent.Config (TOML-tagged only) was decoded straight from the snake_case /
// nested wire format, dropping every field and validating an empty config.
// A minimal valid config in real wire format must pass, and invalid values in
// their real wire positions must be rejected.
func TestConfigCheck_WireFormatContract(t *testing.T) {
	t.Run("minimal valid config passes", func(t *testing.T) {
		payload := `{
			"model":{"provider":"openai","model":"gpt-4"},
			"search":{"provider":"duckduckgo"},
			"agent":{},
			"device":{"device_type":"iOS"}
		}`
		result := checkWire(t, payload)
		if !result.Valid {
			t.Fatalf("expected valid=true, got errors: %+v", result.Errors)
		}
	})

	// The core regression: these invalid values live in their real wire
	// positions (nested under "agent" / "device" / "hid", snake_case). Before the DTO fix
	// every one of these was silently dropped and the config validated as valid.
	invalidCases := []struct {
		name        string
		payload     string
		wantInField string // substring expected in the offending field/message
	}{
		{
			name: "invalid device_type nested under device",
			payload: `{"model":{"provider":"openai","model":"gpt-4"},
				"search":{"provider":"duckduckgo"},
				"device":{"device_type":"blackberry"},"agent":{}}`,
			wantInField: "device_type",
		},
		{
			name: "invalid keyboard_layout nested under hid",
			payload: `{"model":{"provider":"openai","model":"gpt-4"},
				"search":{"provider":"duckduckgo"},
				"hid":{"keyboard_layout":"dvorak"},"agent":{}}`,
			wantInField: "keyboard_layout",
		},
		{
			name: "vad_speech_threshold out of range nested under agent",
			payload: `{"model":{"provider":"openai","model":"gpt-4"},
				"search":{"provider":"duckduckgo"},
				"agent":{"vad_speech_threshold":1.5}}`,
			wantInField: "vad_speech_threshold",
		},
		{
			name: "max_iterations below -1 nested under agent",
			payload: `{"model":{"provider":"openai","model":"gpt-4"},
				"search":{"provider":"duckduckgo"},
				"agent":{"max_iterations":-5}}`,
			wantInField: "max_iterations",
		},
		{
			name: "invalid input_mode nested under agent",
			payload: `{"model":{"provider":"openai","model":"gpt-4"},
				"search":{"provider":"duckduckgo"},
				"agent":{"input_mode":"bogus"}}`,
			wantInField: "input_mode",
		},
		{
			name: "invalid locale nested under agent",
			payload: `{"model":{"provider":"openai","model":"gpt-4"},
				"search":{"provider":"duckduckgo"},
				"agent":{"locale":"fr-FR"}}`,
			wantInField: "locale",
		},
		{
			name: "missing model provider",
			payload: `{"model":{"model":"gpt-4"},
				"search":{"provider":"duckduckgo"},"agent":{}}`,
			wantInField: "model.provider",
		},
		{
			name: "invalid search provider",
			payload: `{"model":{"provider":"openai","model":"gpt-4"},
				"search":{"provider":"google"},"agent":{}}`,
			wantInField: "search.provider",
		},
		{
			name: "invalid audio playback backend",
			payload: `{"model":{"provider":"openai","model":"gpt-4"},
				"search":{"provider":"duckduckgo"},
				"audio":{"backend":"speaker"},"agent":{}}`,
			wantInField: "audio.backend",
		},
	}

	for _, tc := range invalidCases {
		t.Run(tc.name, func(t *testing.T) {
			result := checkWire(t, tc.payload)
			if result.Valid {
				t.Fatalf("expected valid=false for %s, but config was accepted", tc.name)
			}
			joined := result.Errors[0].Field + " " + result.Errors[0].Message
			if !strings.Contains(joined, tc.wantInField) {
				t.Errorf("expected error to reference %q, got field=%q message=%q",
					tc.wantInField, result.Errors[0].Field, result.Errors[0].Message)
			}
		})
	}
}

// TestConfigCheck_WireSearchHasAPIKey verifies the has_api_key contract: the
// web UI never echoes the stored secret, so a provider that requires a key
// must pass when has_api_key=true and fail when it is false/absent.
func TestConfigCheck_WireSearchHasAPIKey(t *testing.T) {
	for _, provider := range []string{"brave", "tavily"} {
		t.Run(provider+" with has_api_key=true passes", func(t *testing.T) {
			payload := `{"model":{"provider":"openai","model":"gpt-4"},
				"search":{"provider":"` + provider + `","has_api_key":true},"agent":{}}`
			result := checkWire(t, payload)
			if !result.Valid {
				t.Fatalf("expected valid=true, got errors: %+v", result.Errors)
			}
		})

		t.Run(provider+" with has_api_key=false fails", func(t *testing.T) {
			payload := `{"model":{"provider":"openai","model":"gpt-4"},
				"search":{"provider":"` + provider + `","has_api_key":false},"agent":{}}`
			result := checkWire(t, payload)
			if result.Valid {
				t.Fatalf("expected valid=false when %s key absent, but config was accepted", provider)
			}
		})
	}
}

// TestConfigCheck_WirePromptRoundTrip covers the only editable prompt
// field: prompt reaches agent.Config and survives the DTO round trip,
// while the removed custom_instruction has no wire field at all.
func TestConfigCheck_WirePromptRoundTrip(t *testing.T) {
	dto := webConfigDTO{
		Model:  modelDTO{Provider: "openai", Model: "gpt-4"},
		Search: searchDTO{Provider: "duckduckgo"},
		Agent:  agentDTO{Prompt: "Use a deployment-specific persona."},
	}
	cfg := dto.ToAgentConfig()
	if cfg.Prompt != "Use a deployment-specific persona." {
		t.Fatalf("Prompt = %q, want the wire value", cfg.Prompt)
	}
	if got := webConfigDTOFromAgentConfig(cfg).Agent.Prompt; got != "Use a deployment-specific persona." {
		t.Fatalf("round-trip prompt = %q, want the wire value", got)
	}

	encoded, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal DTO: %v", err)
	}
	var sections map[string]map[string]json.RawMessage
	if err := json.Unmarshal(encoded, &sections); err != nil {
		t.Fatalf("decode DTO: %v", err)
	}
	if _, ok := sections["agent"]["custom_instruction"]; ok {
		t.Fatalf("wire config still exposes custom_instruction: %s", encoded)
	}
}

func TestConfigCheck_WireLocaleMapsToAgentConfig(t *testing.T) {
	dto := webConfigDTO{
		Model:  modelDTO{Provider: "openai", Model: "gpt-4"},
		Search: searchDTO{Provider: "duckduckgo"},
		Agent:  agentDTO{Locale: "en-US"},
	}
	cfg := dto.ToAgentConfig()
	if cfg.Locale != "en-US" {
		t.Fatalf("Locale = %q, want en-US", cfg.Locale)
	}
	if got := webConfigDTOFromAgentConfig(cfg).Agent.Locale; got != "en-US" {
		t.Fatalf("round-trip locale = %q, want en-US", got)
	}
}

func TestConfigCheck_WireModelReasoningControlsRoundTrip(t *testing.T) {
	cfg := agent.Config{Model: agent.ModelConfig{
		Provider:              "openai",
		Model:                 "gpt-4",
		ReasoningEffort:       "high",
		ReasoningBudgetTokens: 4096,
	}}
	dto := webConfigDTOFromAgentConfig(cfg)
	if dto.Model.ReasoningEffort != "high" {
		t.Fatalf("DTO reasoning_effort = %q, want high", dto.Model.ReasoningEffort)
	}
	back := dto.ToAgentConfig()
	if back.Model.ReasoningEffort != "high" {
		t.Fatalf("round-trip reasoning_effort = %q, want high", back.Model.ReasoningEffort)
	}
	if back.Model.ReasoningBudgetTokens != 4096 {
		t.Fatalf("round-trip reasoning_budget_tokens = %d, want 4096", back.Model.ReasoningBudgetTokens)
	}
	wire, err := json.Marshal(dto)
	if err != nil {
		t.Fatalf("marshal DTO: %v", err)
	}
	if !strings.Contains(string(wire), `"reasoning_budget_tokens":4096`) {
		t.Fatalf("wire config missing canonical reasoning budget: %s", wire)
	}
}

// TestConfigCheck_WireTelemetryNested verifies telemetry validation runs
// against the nested "telemetry" wire object.
func TestConfigCheck_WireTelemetryNested(t *testing.T) {
	payload := `{"model":{"provider":"openai","model":"gpt-4"},
		"search":{"provider":"duckduckgo"},
		"telemetry":{"enabled":true},"agent":{}}`
	result := checkWire(t, payload)
	if result.Valid {
		t.Fatal("expected valid=false when telemetry.enabled=true but base_url missing")
	}
	if !strings.Contains(result.Errors[0].Field, "telemetry") {
		t.Errorf("expected telemetry field, got %q", result.Errors[0].Field)
	}
}

// TestConfigCheck_WireLiveActivityNested verifies the local-only live_activity
// setting is accepted through the nested wire object used by config_web.
func TestConfigCheck_WireLiveActivityNested(t *testing.T) {
	payload := `{"model":{"provider":"openai","model":"gpt-4"},
		"benchmark":{"api_key":"sk-judge"},
		"search":{"provider":"duckduckgo"},
		"live_activity":{"enabled":true},"agent":{}}`
	result := checkWire(t, payload)
	if !result.Valid {
		t.Fatalf("expected valid=true, got errors: %+v", result.Errors)
	}
}

// TestConfigCheck_InvalidJSON ensures malformed input is reported as a decode
// error rather than silently validating a zero-value config.
func TestConfigCheck_InvalidJSON(t *testing.T) {
	_, err := checkConfig(strings.NewReader("not json"))
	if err == nil {
		t.Fatal("expected decode error for malformed JSON, got nil")
	}
}

func TestConfigCheckPath_ValidatesGroupedTOMLWithoutRejectingUnknownNestedFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	content := `[basic_settings.language_timezone]
locale = "en-US"
future_plugin_flag = true

[basic_settings.device]
backend = "hdmi"
future_device_option = "keep me"
`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	result := checkConfigPath(path)
	if !result.Valid {
		t.Fatalf("expected valid config, got errors: %+v", result.Errors)
	}
}

func TestConfigCheckPath_RejectsInvalidTOML(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte("locale = [\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	result := checkConfigPath(path)
	if result.Valid {
		t.Fatal("expected malformed TOML to be rejected")
	}
	if len(result.Errors) == 0 || !strings.Contains(result.Errors[0].Message, "TOML") {
		t.Fatalf("expected TOML decode error, got: %+v", result.Errors)
	}
}

// TestConfigWire_ProvidersRoundTrip guards the sync point that broke the
// provider feature end to end: webConfigDTO had no top-level "model_providers" field,
// so `agent config --format=json` never emitted the section. config_web serves
// GET /api/config straight from that output, which made the page report zero
// providers and made every save of an unrelated section erase them from
// agent.toml. Validation was skipped for the same reason.
func TestConfigWire_ProvidersRoundTrip(t *testing.T) {
	t.Run("providers reach the DTO with write-only credentials", func(t *testing.T) {
		// has_api_key reports a $VAR reference only when the variable is set.
		t.Setenv("OPENAI_KEY", "sk-from-env")
		cfg := agent.Config{
			ModelProviders: map[string]agent.ModelProvider{
				"my-openai": {Type: "openai", APIKey: "$OPENAI_KEY"},
				"my-ollama": {Type: "ollama", BaseURL: "http://127.0.0.1:11434"},
			},
			Model: agent.ModelConfig{Provider: "my-openai", Model: "gpt-4"},
		}

		dto := webConfigDTOFromAgentConfig(cfg)
		if len(dto.ModelProviders) != 2 {
			t.Fatalf("dto.ModelProviders = %#v, want 2 entries", dto.ModelProviders)
		}
		if got := dto.ModelProviders["my-openai"]; got.Type != "openai" ||
			got.APIKey != "" || !got.HasAPIKey {
			t.Errorf("dto.ModelProviders[my-openai] = %#v", got)
		}
		if got := dto.ModelProviders["my-ollama"].BaseURL; got != "http://127.0.0.1:11434" {
			t.Errorf("dto.ModelProviders[my-ollama].BaseURL = %q", got)
		}

		back := dto.ToAgentConfig()
		if got := back.ModelProviders["my-openai"]; got.Type != "openai" || got.APIKey != hasAPIKeyPlaceholder {
			t.Errorf("redacted model_providers conversion = %#v", back.ModelProviders)
		}
	})

	t.Run("wire payload marshals a providers key", func(t *testing.T) {
		dto := webConfigDTOFromAgentConfig(agent.Config{
			ModelProviders: map[string]agent.ModelProvider{"my-openai": {Type: "openai"}},
			Model:          agent.ModelConfig{Provider: "my-openai", Model: "gpt-4"},
		})
		encoded, err := json.Marshal(dto)
		if err != nil {
			t.Fatalf("marshal dto: %v", err)
		}
		if !strings.Contains(string(encoded), `"model_providers":{"my-openai":{"type":"openai"}}`) {
			t.Errorf("encoded payload missing model_providers section: %s", encoded)
		}
	})

	t.Run("unsupported provider type is rejected through the wire decoder", func(t *testing.T) {
		payload := `{
			"model_providers":{"zz":{"type":"nonsense"}},
			"model":{"provider":"zz","model":"gpt-4"},
			"search":{"provider":"duckduckgo"},
			"agent":{},
			"hid":{"pointer_mode":"absolute"}
		}`
		result := checkWire(t, payload)
		if result.Valid {
			t.Fatal("expected an unsupported provider type to be rejected")
		}
		if len(result.Errors) == 0 || !strings.Contains(result.Errors[0].Message, "model_providers.zz") {
			t.Fatalf("expected model_providers.zz error, got: %+v", result.Errors)
		}
	})

	t.Run("provider entry without a type is rejected", func(t *testing.T) {
		payload := `{
			"model_providers":{"empty-one":{"api_key":"k"}},
			"model":{"provider":"empty-one","model":"gpt-4"},
			"search":{"provider":"duckduckgo"},
			"agent":{},
			"hid":{"pointer_mode":"absolute"}
		}`
		result := checkWire(t, payload)
		if result.Valid {
			t.Fatal("expected a provider without a type to be rejected")
		}
	})

	t.Run("valid named providers pass", func(t *testing.T) {
		payload := `{
			"model_providers":{
				"my-openai":{"type":"openai","api_key":"sk-x"},
				"my-ollama":{"type":"ollama","base_url":"http://127.0.0.1:11434"}
			},
			"model":{"provider":"my-openai","model":"gpt-4"},
			"search":{"provider":"duckduckgo"},
			"agent":{},
			"hid":{"pointer_mode":"absolute"}
		}`
		result := checkWire(t, payload)
		if !result.Valid {
			t.Fatalf("expected valid=true, got errors: %+v", result.Errors)
		}
	})
}

func TestWebConfigDTOStorageRoundTrip(t *testing.T) {
	cfg := agent.Config{
		Storage: agent.StorageConfig{
			MonitorEnabled:       true,
			MountPoint:           "/custom/mount",
			Device:               "mmcblk9",
			MinCardFreeMB:        77,
			MigrateStartFreePct:  11,
			MigrateStopFreePct:   61,
			RootPath:             "/custom/root",
			CheckIntervalSeconds: 123,
			WarningThresholdMB:   81,
			CriticalThresholdMB:  21,
			EmergencyThresholdMB: 9,
			RecoveryHysteresisMB: 4,
			DegradedMode: agent.StorageDegradedModeConfig{
				DisableLLMHTTPLog:     false,
				DisableAudioArchive:   true,
				DisableSessionArchive: false,
				MaxAgentLogMB:         3,
			},
			Cleanup: agent.StorageCleanupConfig{
				Enabled:                     false,
				LLMHTTPLogRetentionDays:     []int{8, 4},
				AudioArchiveRetentionDays:   []int{20, 2},
				SessionArchiveRetentionDays: []int{14},
				CleanupRetryIntervalSeconds: 42,
			},
		},
	}
	dto := webConfigDTOFromAgentConfig(cfg)
	back := dto.ToAgentConfig()
	if !reflect.DeepEqual(back.Storage, cfg.Storage) {
		t.Fatalf("storage round-trip changed config:\n got:  %+v\n want: %+v", back.Storage, cfg.Storage)
	}
}

func TestConfigWire_ModelProviderCanonicalJSON(t *testing.T) {
	t.Run("canonical payload uses model_providers and type", func(t *testing.T) {
		payload := `{
			"model_providers":{"work":{"type":"openai","api_key":"sk-x"}},
			"model":{"provider":"work","model":"gpt-4"},
			"search":{"provider":"duckduckgo"},
			"agent":{},
			"hid":{"pointer_mode":"absolute"}
		}`
		result := checkWire(t, payload)
		if !result.Valid {
			t.Fatalf("canonical payload rejected: %+v", result.Errors)
		}
	})

	t.Run("legacy providers namespace is rejected", func(t *testing.T) {
		payload := `{
			"providers":{"work":{"provider":"openai","api_key":"sk-x"}},
			"model":{"provider":"work","model":"gpt-4"},
			"search":{"provider":"duckduckgo"},
			"agent":{},
			"hid":{"pointer_mode":"absolute"}
		}`
		if _, err := checkConfig(strings.NewReader(payload)); err == nil {
			t.Fatal("expected legacy providers namespace to fail decoding")
		} else if !strings.Contains(err.Error(), "model_providers") {
			t.Fatalf("unexpected decode error: %v", err)
		}
	})

	t.Run("legacy provider record field is rejected", func(t *testing.T) {
		payload := `{
			"model_providers":{"work":{"type":"openai","provider":"kimi","api_key":"sk-canonical"}},
			"model":{"provider":"work","model":"gpt-4"},
			"search":{"provider":"duckduckgo"},
			"agent":{},
			"hid":{"pointer_mode":"absolute"}
		}`
		if _, err := checkConfig(strings.NewReader(payload)); err == nil {
			t.Fatal("expected legacy provider field to fail decoding")
		}
	})

	t.Run("null canonical type is accepted without provider alias", func(t *testing.T) {
		var dto webConfigDTO
		if err := json.Unmarshal([]byte(`{"model_providers":{"work":{"type":null}}}`), &dto); err != nil {
			t.Fatalf("unmarshal null canonical type: %v", err)
		}
	})

	t.Run("marshaled payload only uses canonical names", func(t *testing.T) {
		dto := webConfigDTO{
			ModelProviders: map[string]modelProviderDTO{"work": {Type: "openai", APIKey: "sk-x"}},
			Model:          modelDTO{Provider: "work", Model: "gpt-4"},
		}
		encoded, err := json.Marshal(dto)
		if err != nil {
			t.Fatalf("marshal canonical payload: %v", err)
		}
		output := string(encoded)
		if !strings.Contains(output, `"model_providers":{"work":{"type":"openai"`) {
			t.Errorf("canonical provider missing from JSON: %s", output)
		}
		if strings.Contains(output, `"providers"`) || strings.Contains(output, `"provider":"openai"`) {
			t.Errorf("legacy model provider shape leaked into JSON: %s", output)
		}
	})
}

// TestWebConfigDTOTopLevelSectionsAreCovered pins the set of top-level sections
// that `agent config --format=json` emits.
//
// config_web serves this payload to the page verbatim, so a section the Go DTO
// does not emit does not exist as far as the config UI is concerned. That is
// exactly how the `providers` bug worked: the field was missing here, so the
// config page always showed zero providers AND every save of an unrelated
// section started from an empty map and erased them from agent.toml.
//
// The E2E override fixtures are hand-maintained copies of this payload, so they
// cannot catch drift on the Go side. This test can: when a section is added to
// webConfigDTO, this list must be updated, which is the prompt to update those
// fixtures in the same change.
func TestWebConfigDTOTopLevelSectionsAreCovered(t *testing.T) {
	want := []string{
		"agent",
		"audio",
		"audio_archive",
		"decision",
		"device",
		"frame_service",
		"hid",
		"live_activity",
		"log",
		"model",
		"model_providers",
		"ota",
		"quick_capture",
		"search",
		"storage",
		"stt",
		"stt_providers",
		"telemetry",
		"termination_policy",
		"tts",
		"tts_providers",
		"voice_model",
		"voice_model_providers",
		"voice_notifications",
	}

	dtoType := reflect.TypeOf(webConfigDTO{})
	got := make([]string, 0, dtoType.NumField())
	for i := 0; i < dtoType.NumField(); i++ {
		tag := dtoType.Field(i).Tag.Get("json")
		if tag == "" || tag == "-" {
			t.Errorf("field %s has no json tag; the Config Web wire contract keys off these names",
				dtoType.Field(i).Name)
			continue
		}
		got = append(got, strings.Split(tag, ",")[0])
	}

	sort.Strings(got)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("webConfigDTO top-level sections drifted.\n got: %v\nwant: %v\n"+
			"If this is intentional, update this list and the Config Web contract tests.", got, want)
	}
}

// TestWebConfigDTOProvidersOmittedWhenEmpty documents that `model_providers` carries
// omitempty, so a config with no providers omits the key entirely rather than
// emitting {}. Consumers of the resolved config must treat a missing key as
// "no providers" rather than as an error.
func TestWebConfigDTOProvidersOmittedWhenEmpty(t *testing.T) {
	cfg := agent.Config{
		Model: agent.ModelConfig{Provider: "openai", Model: "gpt-4o", APIKey: "sk-x"},
	}
	payload, err := json.Marshal(webConfigDTOFromAgentConfig(cfg))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(payload), `"model_providers"`) {
		t.Errorf("expected model_providers to be omitted when empty, got: %s", payload)
	}

	// And with a provider present the key must appear.
	cfg.ModelProviders = map[string]agent.ModelProvider{
		"work": {Type: "openai", APIKey: "sk-work"},
	}
	payload, err = json.Marshal(webConfigDTOFromAgentConfig(cfg))
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(payload), `"model_providers"`) {
		t.Errorf("expected model_providers in the payload, got: %s", payload)
	}
}

func TestWebConfigDTOResponsesFieldsRoundTrip(t *testing.T) {
	include := []string{"reasoning.encrypted_content", "message.output_text"}
	cfg := agent.Config{Model: agent.ModelConfig{
		Provider:                          "volcengine",
		Model:                             "doubao-seed-2-1-pro",
		APIMode:                           "responses_stateful",
		ResponsesContextManagement:        "ark_context_edit",
		ResponsesCompactThreshold:         12345,
		ResponsesContextEditTrigger:       10,
		ResponsesContextEditKeep:          3,
		ResponsesContextEditClearThinking: true,
		ResponsesTruncation:               "auto",
		ResponsesInclude:                  include,
	}}
	dto := webConfigDTOFromAgentConfig(cfg)
	if dto.Model.ResponsesContextManagement != "ark_context_edit" ||
		dto.Model.ResponsesCompactThreshold != 12345 ||
		dto.Model.ResponsesContextEditTrigger != 10 ||
		dto.Model.ResponsesContextEditKeep != 3 ||
		!dto.Model.ResponsesContextEditClearThinking ||
		dto.Model.ResponsesTruncation != "auto" ||
		!reflect.DeepEqual(dto.Model.ResponsesInclude, include) {
		t.Fatalf("Responses fields were not emitted: %+v", dto.Model)
	}
	got := dto.ToAgentConfig().Model
	if got.ResponsesContextManagement != cfg.Model.ResponsesContextManagement ||
		got.ResponsesCompactThreshold != cfg.Model.ResponsesCompactThreshold ||
		got.ResponsesContextEditTrigger != cfg.Model.ResponsesContextEditTrigger ||
		got.ResponsesContextEditKeep != cfg.Model.ResponsesContextEditKeep ||
		got.ResponsesContextEditClearThinking != cfg.Model.ResponsesContextEditClearThinking ||
		got.ResponsesTruncation != cfg.Model.ResponsesTruncation ||
		!reflect.DeepEqual(got.ResponsesInclude, include) {
		t.Fatalf("Responses fields did not round-trip: %+v", got)
	}
}
