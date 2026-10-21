package agent

import (
	"sort"

	"aiden-agent/internal/agent/mnk"
	"aiden-agent/internal/agent/realtimevoice"
	"aiden-agent/internal/agent/tts"
)

// This file is the single source of truth for config field metadata consumed
// by the config web UI (via the `agent config-meta` CLI subcommand). It
// describes how each field is rendered, typed and defaulted, and the conditions
// under which it is shown. Config web derives JSON field type checks from the
// widget metadata; semantic validation still lives in Config.Validate(). The
// enums here are kept consistent with the constants used there.

// Widget identifies how the config web UI should render a field.
type Widget string

const (
	WidgetText     Widget = "text"
	WidgetTextarea Widget = "textarea"
	WidgetNumber   Widget = "number"
	WidgetBoolean  Widget = "boolean"
	WidgetSelect   Widget = "select"
	WidgetList     Widget = "list"
)

// EnumOption is a single predefined choice. Label may differ from Value (e.g.
// an empty value shown as "langfuse (default)"). Text widgets may also carry
// choices when SelectWhen conditionally renders them as a select.
type EnumOption struct {
	Value     string   `json:"value"`
	Label     string   `json:"label,omitempty"`
	Providers []string `json:"providers,omitempty"`
	// ExcludeProviders hides a choice from provider types that cannot use it.
	// It exists for values such as the empty api_mode: that value means "the
	// provider's compatible default", which providers without a compatible
	// transport (Gemini) must not be offered.
	ExcludeProviders []string `json:"excludeProviders,omitempty"`
}

// chatCompletionsUnsupportedProviderTypes lists provider types that have no
// usable OpenAI-compatible /chat/completions transport, so the empty api_mode
// value cannot stand for their default.
func chatCompletionsUnsupportedProviderTypes() []string {
	var types []string
	for _, definition := range modelProviderDefinitions {
		if definition.chatCompletionsUnsupported {
			types = append(types, definition.providerType)
		}
	}
	return types
}

func realtimeProviderEnumOptions() []EnumOption {
	descriptors := realtimevoice.ProviderDescriptors()
	options := make([]EnumOption, 0, len(descriptors))
	for _, descriptor := range descriptors {
		options = append(options, EnumOption{Value: descriptor.Name, Label: descriptor.Label})
	}
	return options
}

func realtimeProviderPlaceholders(field string) []ConditionalPlaceholder {
	descriptors := realtimevoice.ProviderDescriptors()
	placeholders := make([]ConditionalPlaceholder, 0, len(descriptors))
	for _, descriptor := range descriptors {
		value := descriptor.ModelPlaceholder
		if field == "voice" {
			value = descriptor.VoicePlaceholder
		}
		if value == "" {
			continue
		}
		placeholders = append(placeholders, ConditionalPlaceholder{
			When:  VisibleRule{All: []Condition{eq("voice_model_providers.type", descriptor.Name)}},
			Value: value,
		})
	}
	return placeholders
}

// modelTemperaturePlaceholders derives the temperature placeholder from the
// model spec registry so the editor advertises the value the runtime will
// actually resolve for the selected model. Without it the field shows the global
// defaultModelTemperature for every model, which contradicts the runtime
// wherever a spec pins its own default: the Gemini 3 models pin Google's
// documented 1.0 (Google warns a lower value may cause looping or degraded
// reasoning) and Kimi K3 pins the temperature it requires. Native Gemini models
// without a registered default get an empty placeholder because the runtime
// omits temperature and lets Google choose.
//
// This is a placeholder, not a Default, so an untouched field still saves as
// unset and keeps the resolve-at-load contract in applyModelTemperatureDefault
// instead of baking a value into agent.toml.
//
// Ids are grouped by pinned value so one condition covers every spelling the
// registry holds (bare name and provider-prefixed). The registry stays the
// single source of truth: pinning a new model in model_specs.go extends this
// automatically.
func modelTemperaturePlaceholders() []ConditionalPlaceholder {
	grouped := map[float64][]string{}
	for id, spec := range modelSpecRegistry {
		if spec.DefaultTemperature == nil {
			continue
		}
		grouped[*spec.DefaultTemperature] = append(grouped[*spec.DefaultTemperature], id)
	}
	temperatures := make([]float64, 0, len(grouped))
	for temperature := range grouped {
		temperatures = append(temperatures, temperature)
	}
	// Map iteration order is random; sort so `agent config-meta` output is stable.
	sort.Float64s(temperatures)
	providerDefaultTypes := modelProviderTypesUsingProviderTemperatureDefault()
	placeholders := make([]ConditionalPlaceholder, 0, len(grouped)+len(providerDefaultTypes))
	for _, temperature := range temperatures {
		ids := grouped[temperature]
		sort.Strings(ids)
		placeholders = append(placeholders, placeholderWhen(temperature, in("model.model", ids...)))
	}
	// Keep provider-level rules after model-specific rules so a known model
	// default wins. The provider registry owns which provider types defer.
	for _, providerType := range providerDefaultTypes {
		placeholders = append(placeholders, placeholderWhen(nil, providerTypeIs("model.provider", providerType)))
	}
	return placeholders
}

// Range describes the bounds for a numeric field. When a number field also
// carries a Range, the UI renders it as a select of discrete steps.
type Range struct {
	Min       float64 `json:"min"`
	Max       float64 `json:"max"`
	Step      float64 `json:"step"`
	Precision int     `json:"precision,omitempty"`
}

// Condition is a single predicate against another field's current value.
// Field is a dotted path like "model.provider" or "agent.input_mode".
type Condition struct {
	Field  string   `json:"field"`
	Op     string   `json:"op"`               // eq | ne | in | notIn | truthy | providerType
	Value  string   `json:"value,omitempty"`  // for eq / ne
	Values []string `json:"values,omitempty"` // for in / notIn
}

// VisibleRule is a declarative visibility expression. Exactly one of All/Any
// is populated. All = logical AND, Any = logical OR.
type VisibleRule struct {
	All []Condition `json:"all,omitempty"`
	Any []Condition `json:"any,omitempty"`
}

// ConditionalPlaceholder supplies a provider/model-specific placeholder when
// its condition matches. It documents the runtime recommendation without
// writing the example value into the user's configuration.
type ConditionalPlaceholder struct {
	When  VisibleRule `json:"when"`
	Value interface{} `json:"value"`
}

// FieldMeta describes a single configurable field.
type FieldMeta struct {
	Key             string                   `json:"key"`
	Label           string                   `json:"label,omitempty"`
	Help            string                   `json:"help,omitempty"`
	Placeholder     string                   `json:"placeholder,omitempty"`
	Layout          string                   `json:"layout,omitempty"`
	Widget          Widget                   `json:"widget"`
	Enum            []EnumOption             `json:"enum,omitempty"`
	Range           *Range                   `json:"range,omitempty"`
	Default         interface{}              `json:"default,omitempty"`
	PlaceholderWhen []ConditionalPlaceholder `json:"placeholderWhen,omitempty"`
	Secret          bool                     `json:"secret,omitempty"`
	Advanced        bool                     `json:"advanced,omitempty"`
	Nullable        bool                     `json:"nullable,omitempty"` // For number fields: empty input means unset (omit key), not 0
	VisibleWhen     *VisibleRule             `json:"visibleWhen,omitempty"`
	SelectWhen      *VisibleRule             `json:"selectWhen,omitempty"`
}

// SectionMeta groups fields under a UI section. Section names match the JSON
// object keys the UI reads/writes (e.g. "model", "agent").
type SectionMeta struct {
	Name   string      `json:"name"`
	Fields []FieldMeta `json:"fields"`
}

// ConfigMetadata is the top-level payload emitted by `agent config-meta`.
type ConfigMetadata struct {
	Sections []SectionMeta `json:"sections"`
}

func keyboardLayoutEnumOptions() []EnumOption {
	options := make([]EnumOption, 0, len(mnk.KeyboardLayoutDefinitions))
	for _, layout := range mnk.KeyboardLayoutDefinitions {
		options = append(options, EnumOption{Value: layout.Value, Label: layout.Label})
	}
	return options
}

// enumOptions builds plain value==label options from raw strings.
func enumOptions(values ...string) []EnumOption {
	opts := make([]EnumOption, 0, len(values))
	for _, v := range values {
		opts = append(opts, EnumOption{Value: v})
	}
	return opts
}

// eq/ne/in/notIn/truthy are small helpers for building conditions.
func eq(field, value string) Condition { return Condition{Field: field, Op: "eq", Value: value} }
func ne(field, value string) Condition { return Condition{Field: field, Op: "ne", Value: value} }
func truthy(field string) Condition    { return Condition{Field: field, Op: "truthy"} }
func providerTypeIs(field, value string) Condition {
	return Condition{Field: field, Op: "providerType", Value: value}
}
func in(field string, vs ...string) Condition {
	return Condition{Field: field, Op: "in", Values: vs}
}
func notIn(field string, vs ...string) Condition {
	return Condition{Field: field, Op: "notIn", Values: vs}
}

func all(conds ...Condition) *VisibleRule { return &VisibleRule{All: conds} }

func placeholderWhen(value interface{}, conds ...Condition) ConditionalPlaceholder {
	return ConditionalPlaceholder{When: VisibleRule{All: conds}, Value: value}
}

// withDisplayDefaults ensures every field has a stable label even when the
// form does not need custom presentation metadata. Config keys are already the
// labels used by the existing web UI, so using the key preserves that contract.
func withDisplayDefaults(metadata ConfigMetadata) ConfigMetadata {
	for sectionIndex := range metadata.Sections {
		for fieldIndex := range metadata.Sections[sectionIndex].Fields {
			field := &metadata.Sections[sectionIndex].Fields[fieldIndex]
			if field.Label == "" {
				field.Label = field.Key
			}
		}
	}
	return metadata
}

// ConfigMeta returns the full field metadata for the config web UI. Defaults
// here are the canonical defaults for the device's agent.toml. The free-text
// prompt intentionally carries no metadata default: the built-in
// Agent instruction is runtime content, and prompt only extends it.
func ConfigMeta() ConfigMetadata {
	defaults := DefaultConfig()
	tencentSTTProviderNames := sttProviderNamesForCanonical(tencentASRProvider)
	metadata := ConfigMetadata{
		Sections: []SectionMeta{
			{
				Name: "device",
				Fields: []FieldMeta{
					{Key: "device_type", Widget: WidgetSelect,
						Help:    "Android uses HID touchscreen mode. iOS, macOS, windows, and linux use absolute pointer mode.",
						Enum:    enumOptions("iOS", "Android", "macOS", "windows", "linux"),
						Default: defaults.Device.DeviceTypeOrDefault()},
				},
			},
			{
				Name: "model",
				Fields: []FieldMeta{
					{Key: "provider", Label: "Model provider", Widget: WidgetSelect,
						Help:    "Select the configured provider record used for the main model.",
						Layout:  "wide",
						Enum:    enumOptions(modelProviderTypesForConfigUI()...),
						Default: defaults.Model.Provider},
					{Key: "model", Label: "Model", Widget: WidgetText,
						Help:    "Select or enter the model name exposed by the configured provider.",
						Default: defaults.Model.Model, Layout: "wide"},
					{Key: "api_mode", Label: "Conversation API", Widget: WidgetSelect,
						Help: "Choose the conversation wire protocol and who manages context. Local modes submit the transcript; provider modes continue from provider state.",
						Enum: []EnumOption{
							{Value: "", Label: "Chat Completions (compatible)", ExcludeProviders: chatCompletionsUnsupportedProviderTypes()},
							{Value: "responses", Label: "Responses (local context)", Providers: []string{"openai", "openrouter", "volcengine", "deepseek"}},
							{Value: "responses_stateful", Label: "Responses (provider context)", Providers: []string{"openai", "volcengine"}},
							{Value: "interactions", Label: "Interactions (local context)", Providers: []string{"gemini"}},
							{Value: "interactions_stateful", Label: "Interactions (provider context)", Providers: []string{"gemini"}},
						},
						Default: defaults.Model.APIMode, Layout: "wide"},
					{Key: "responses_context_management", Label: "Provider compaction", Widget: WidgetSelect,
						Help: "Choose provider-side context cleanup. OpenAI uses token compaction; Volcengine Ark uses tool-call and thinking edits.",
						Enum: []EnumOption{
							{Value: "", Label: "Off (recommended)"},
							{Value: "compaction", Label: "Compact at threshold", Providers: []string{"openai"}},
							{Value: "ark_context_edit", Label: "Ark context edits", Providers: []string{"volcengine"}},
						},
						VisibleWhen: all(in("model.api_mode", "responses", "responses_stateful")),
						Default:     defaults.Model.ResponsesContextManagement,
						Layout:      "wide"},
					{Key: "responses_compact_threshold", Label: "Compaction threshold (tokens)", Widget: WidgetNumber,
						Help:        "Token count that triggers provider compaction. 0 lets the provider choose.",
						VisibleWhen: all(in("model.api_mode", "responses", "responses_stateful"), eq("model.responses_context_management", "compaction")),
						Default:     defaults.Model.ResponsesCompactThreshold, Placeholder: "0 = provider default"},
					{Key: "responses_context_edit_trigger", Label: "Ark tool-call trigger", Widget: WidgetNumber,
						Help:        "After this many tool calls, Ark clears old tool inputs. 0 uses the recommended value 10.",
						VisibleWhen: all(in("model.api_mode", "responses", "responses_stateful"), eq("model.responses_context_management", "ark_context_edit")),
						Default:     defaults.Model.ResponsesContextEditTrigger, Placeholder: "10 = recommended"},
					{Key: "responses_context_edit_keep", Label: "Ark tool calls to keep", Widget: WidgetNumber,
						Help:        "Number of recent tool calls Ark keeps after cleanup. 0 uses the recommended value 3.",
						VisibleWhen: all(in("model.api_mode", "responses", "responses_stateful"), eq("model.responses_context_management", "ark_context_edit")),
						Default:     defaults.Model.ResponsesContextEditKeep, Placeholder: "3 = recommended"},
					{Key: "responses_context_edit_clear_thinking", Label: "Clear old thinking", Widget: WidgetBoolean,
						Help:        "Ask Ark to remove previous thinking turns when it applies the context edit.",
						VisibleWhen: all(in("model.api_mode", "responses", "responses_stateful"), eq("model.responses_context_management", "ark_context_edit")),
						Default:     defaults.Model.ResponsesContextEditClearThinking},
					{Key: "responses_truncation", Label: "Over-limit input", Widget: WidgetSelect,
						Help: "Fail when input is too long, or let a compatible provider discard the oldest input and continue.",
						Enum: []EnumOption{
							{Value: "", Label: "Fail with an error (default)"},
							{Value: "auto", Label: "Discard oldest input automatically", Providers: []string{"openai", "openrouter"}},
						},
						VisibleWhen: all(in("model.api_mode", "responses", "responses_stateful")),
						Default:     defaults.Model.ResponsesTruncation},
					{Key: "responses_include", Label: "Extra response fields", Widget: WidgetList, Layout: "wide",
						Help:        "Usually leave empty. Add provider-supported include values only when needed, one per line.",
						Placeholder: "reasoning.encrypted_content",
						VisibleWhen: all(in("model.api_mode", "responses", "responses_stateful")),
						Default:     defaults.Model.ResponsesInclude},
					// The effective default is model-dependent (resolved at load
					// time). Default carries the non-Gemini global fallback;
					// PlaceholderWhen overrides it for pinned models and native
					// Gemini models that defer to Google's default.
					{Key: "temperature", Label: "Temperature", Widget: WidgetNumber,
						Help:            "Controls response randomness. Leave empty to use the Agent-selected default; some models defer to their provider default. 0 is sent as an explicit value.",
						PlaceholderWhen: modelTemperaturePlaceholders(),
						Default:         defaultModelTemperature, Nullable: true},
					{Key: "max_response_tokens", Label: "Maximum response tokens", Widget: WidgetNumber,
						Help:    "Maximum number of tokens allowed in one model response.",
						Default: defaults.Model.MaxResponseTokens},
					{Key: "log_raw_http", Label: "Raw HTTP logging", Widget: WidgetBoolean,
						Help:    "Write raw model HTTP requests and responses to the Agent log directory. Enable only while troubleshooting.",
						Default: defaults.Model.LogRawHTTP},
					// Reasoning levels differ per provider: "minimal" is supported by
					// OpenRouter and Volcengine Ark; "none" is the OpenAI-style off
					// switch that Ark does not recognize. Scope each to the providers
					// that accept it so the UI cannot save a value the endpoint
					// rejects; auto plus low/medium/high stay unscoped.
					{Key: "reasoning_effort", Label: "Reasoning effort", Widget: WidgetSelect,
						Help: "Empty = auto. Options follow the selected model capability; none is shown only when the model supports disabling reasoning.",
						Enum: []EnumOption{
							{Value: "", Label: "auto (default)"},
							{Value: "minimal", Label: "minimal (no reasoning)", Providers: []string{"openrouter", "volcengine"}},
							{Value: "none", Label: "none", Providers: []string{"openrouter", "openai", "kimi", "kimi-cn", "deepseek", "ollama", "fake"}},
							{Value: "low", Label: "low"},
							{Value: "medium", Label: "medium"},
							{Value: "high", Label: "high"},
							{Value: "xhigh", Label: "xhigh", Providers: []string{"anthropic", "openrouter", "openai", "kimi", "kimi-cn", "volcengine"}},
							{Value: "max", Label: "max", Providers: []string{"anthropic", "openrouter", "openai", "kimi", "kimi-cn", "volcengine", "deepseek"}},
						},
						Default: defaults.Model.ReasoningEffort},
					{Key: "reasoning_budget_tokens", Widget: WidgetNumber,
						Label:       "Reasoning budget (tokens)",
						Placeholder: "0 = auto",
						Help:        "Optional exact reasoning budget. Shown for models that expose budget_tokens; 0 uses the model default or effort preset. Reasoning tokens are drawn from max_response_tokens, so this must be at least 1024 and smaller than that limit.",
						Default:     0},
					{Key: "context_window", Label: "Context window (tokens)", Widget: WidgetNumber, Default: defaults.Model.ContextWindow,
						Placeholder: "0 = auto", Help: "0 = auto: use provider metadata when available."},
					{Key: "model_max_output_tokens", Label: "Provider output limit (tokens)", Widget: WidgetNumber, Default: defaults.Model.ModelMaxOutputTokens,
						Placeholder: "0 = auto", Help: "0 = auto: use provider metadata when available."},
				},
			},
			// model_providers describes one [model_providers.<name>] record, the same shape
			// [tts_providers] and [stt_providers] give voice. Each holds the
			// credentials and settings for one LLM service, and [model] references
			// one by putting the name in its own provider field. Several providers
			// stay configured at once, so switching is a one-line change instead of
			// a re-entry of keys.
			//
			// Every rule keys on model_providers.type — the record's own type select.
			{
				Name: "model_providers",
				Fields: []FieldMeta{
					{Key: "type", Widget: WidgetSelect,
						Enum: enumOptions(modelProviderTypesForConfigUI()...)},
					{Key: "api_key", Widget: WidgetText, Secret: true},
					{Key: "base_url", Widget: WidgetText,
						VisibleWhen: all(in("model_providers.type", modelProviderTypesAllowingCustomBaseURLForConfigUI()...))},
				},
			},
			// [tts] keeps only the provider reference and the settings that are
			// genuinely global. Everything that stops meaning anything when the
			// provider changes lives on the record instead -- see tts_providers.
			//
			// The provider enum here is the legacy bare-type list, which is what
			// a pre-records config carries. The UI replaces these options with
			// the configured record names at runtime.
			{
				Name: "tts",
				Fields: []FieldMeta{
					{Key: "provider", Label: "Text-to-speech provider", Widget: WidgetSelect,
						Layout:  "wide",
						Enum:    enumOptions(tts.AvailableProviders()...),
						Default: defaults.TTS.Provider},
					// speed is a listening preference, not a credential: it must
					// not change when the voice changes, so it stays global.
					{Key: "speed", Label: "Speech speed", Widget: WidgetSelect, Default: defaults.TTS.Speed,
						Range: &Range{Min: 0.5, Max: 2, Step: 0.1, Precision: 1}},
				},
			},
			// tts_providers describes one [tts_providers.<name>] record. The
			// provider dialog renders straight from this, so provider knowledge
			// stays here rather than being hardcoded per dialog in JS.
			//
			// Every rule keys on tts_providers.type -- the record's own type
			// select. Keying on tts.provider would compare against a record NAME
			// and never match, which would show every field for every type.
			{
				Name: "tts_providers",
				Fields: []FieldMeta{
					{Key: "type", Widget: WidgetSelect,
						Enum: enumOptions(tts.AvailableProviders()...)},
					{Key: "api_key", Widget: WidgetText, Secret: true},
					{Key: "model", Widget: WidgetText,
						Enum: []EnumOption{
							{Value: "speech-2.8-hd", Label: "MiniMax speech-2.8-hd", Providers: []string{"minimax", "minimax-cn"}},
							{Value: "s2-pro", Label: "Fish Audio s2-pro", Providers: []string{"fish-audio"}},
							{Value: "qwen-tts-realtime", Label: "Alibaba Cloud qwen-tts-realtime", Providers: []string{"alicloud"}},
							{Value: "qwen3-tts-flash-realtime", Label: "Alibaba Cloud qwen3-tts-flash-realtime", Providers: []string{"alicloud"}},
							{Value: "seed-tts-2.0", Label: "Volcengine seed-tts-2.0", Providers: []string{"volcengine"}},
							{Value: "google/gemini-3.1-flash-tts-preview", Label: "Google Gemini TTS", Providers: []string{"openrouter"}},
							{Value: "hexgrad/kokoro-82m", Label: "Hexgrad Kokoro", Providers: []string{"openrouter"}},
							{Value: "microsoft/mai-voice-2", Label: "Microsoft MAI Voice 2", Providers: []string{"openrouter"}},
						},
						PlaceholderWhen: []ConditionalPlaceholder{
							placeholderWhen("speech-2.8-hd", in("tts_providers.type", "minimax", "minimax-cn")),
							placeholderWhen("s2-pro", in("tts_providers.type", "fish-audio")),
							placeholderWhen("qwen-tts-realtime", in("tts_providers.type", "alicloud")),
							placeholderWhen("seed-tts-2.0", in("tts_providers.type", "volcengine")),
							placeholderWhen("google/gemini-3.1-flash-tts-preview", in("tts_providers.type", "openrouter")),
						},
						SelectWhen:  all(in("tts_providers.type", "openrouter")),
						VisibleWhen: all(in("tts_providers.type", "minimax", "minimax-cn", "fish-audio", "alicloud", "volcengine", "openrouter"))},
					{Key: "voice_id", Widget: WidgetText, Default: defaults.TTS.VoiceID,
						PlaceholderWhen: []ConditionalPlaceholder{
							placeholderWhen("Kore", eq("tts_providers.type", "openrouter"), eq("tts_providers.model", "google/gemini-3.1-flash-tts-preview")),
							placeholderWhen("af_heart", eq("tts_providers.type", "openrouter"), eq("tts_providers.model", "hexgrad/kokoro-82m")),
							placeholderWhen("en-US-AndrewMultilingualNeural", eq("tts_providers.type", "openrouter"), eq("tts_providers.model", "microsoft/mai-voice-2")),
							placeholderWhen("alloy", in("tts_providers.type", "openrouter")),
							placeholderWhen("male-qn-qingse", in("tts_providers.type", "minimax", "minimax-cn")),
							placeholderWhen("Cherry", in("tts_providers.type", "alicloud")),
							placeholderWhen("zh_female_vv_uranus_bigtts", in("tts_providers.type", "volcengine")),
							placeholderWhen("en-US-Neural2-C", in("tts_providers.type", "google-cloud")),
						},
						VisibleWhen: all(in("tts_providers.type", "minimax", "minimax-cn", "alicloud", "volcengine", "openrouter", "google-cloud"))},
					{Key: "reference_id", Widget: WidgetText,
						PlaceholderWhen: []ConditionalPlaceholder{
							placeholderWhen(tts.DefaultFishAudioReferenceID, in("tts_providers.type", "fish-audio")),
						},
						VisibleWhen: all(in("tts_providers.type", "fish-audio"))},
					{Key: "emotion", Widget: WidgetText, Default: defaults.TTS.Emotion,
						PlaceholderWhen: []ConditionalPlaceholder{
							placeholderWhen("happy", in("tts_providers.type", "minimax", "minimax-cn")),
						},
						VisibleWhen: all(in("tts_providers.type", "minimax", "minimax-cn", "volcengine"))},
				},
			},
			// [stt] keeps the provider reference and language. language is a user
			// preference that holds regardless of which provider transcribes, so
			// unlike the credential set it does not belong on a record.
			{
				Name: "stt",
				Fields: []FieldMeta{
					{Key: "provider", Label: "Speech-to-text provider", Widget: WidgetSelect,
						Layout:  "wide",
						Enum:    enumOptions(sttProviderTypes()...),
						Default: defaults.STT.Provider},
					{Key: "language", Label: "Recognition language", Widget: WidgetSelect,
						Enum:    []EnumOption{{Value: "zh", Label: "中文"}, {Value: "en", Label: "English"}},
						Default: defaults.STT.Language},
				},
			},
			// stt_providers describes one [stt_providers.<name>] record. Same
			// contract as tts_providers: every rule keys on the record's own type.
			{
				Name: "stt_providers",
				Fields: []FieldMeta{
					{Key: "type", Widget: WidgetSelect,
						Enum: enumOptions(sttProviderTypes()...)},
					{Key: "api_key", Widget: WidgetText, Secret: true,
						VisibleWhen: all(in("stt_providers.type", "openai-whisper", "openrouter", "qwen-asr", "google-cloud"))},
					{Key: "model", Widget: WidgetSelect,
						Enum: []EnumOption{
							{Value: "openai/whisper-large-v3-turbo", Label: "OpenAI Whisper v3 Turbo"},
							{Value: "openai/whisper-large-v3", Label: "OpenAI Whisper v3"},
							{Value: "openai/whisper-1", Label: "OpenAI Whisper 1"},
							{Value: "openai/gpt-4o-transcribe", Label: "OpenAI GPT-4o Transcribe"},
							{Value: "openai/gpt-4o-mini-transcribe", Label: "OpenAI GPT-4o Mini Transcribe"},
							{Value: "microsoft/mai-transcribe-1.5", Label: "Microsoft MAI Transcribe"},
							{Value: "nvidia/parakeet-tdt-0.6b-v3", Label: "NVIDIA Parakeet"},
							{Value: "mistralai/voxtral-mini-transcribe", Label: "Mistral Voxtral Mini"},
							{Value: "qwen/qwen3-asr-flash-2026-02-10", Label: "Qwen3 ASR Flash"},
							{Value: "google/chirp-3", Label: "Google Chirp 3"},
						},
						Default:     defaults.STT.Model,
						VisibleWhen: all(in("stt_providers.type", "openrouter"))},
					{Key: "base_url", Widget: WidgetText,
						VisibleWhen: all(in("stt_providers.type", "openai-whisper"))},
					{Key: "app_id", Widget: WidgetText, Default: "",
						VisibleWhen: all(in("stt_providers.type", tencentSTTProviderNames...))},
					{Key: "secret_id", Widget: WidgetText, Secret: true,
						VisibleWhen: all(in("stt_providers.type", tencentSTTProviderNames...))},
					{Key: "secret_key", Widget: WidgetText, Secret: true,
						VisibleWhen: all(in("stt_providers.type", tencentSTTProviderNames...))},
					{Key: "region", Widget: WidgetText, Default: defaultTencentASRRegion,
						VisibleWhen: all(in("stt_providers.type", tencentSTTProviderNames...))},
					{Key: "engine_model_type", Widget: WidgetText,
						VisibleWhen: all(in("stt_providers.type", tencentSTTProviderNames...))},
				},
			},
			{
				Name: "audio",
				Fields: []FieldMeta{
					{Key: "socket", Widget: WidgetText, Default: defaults.Audio.Socket, Layout: "wide"},
					{Key: "sample_rate", Widget: WidgetNumber, Default: defaults.Audio.SampleRate},
					{Key: "channels", Widget: WidgetNumber, Default: defaults.Audio.Channels},
					{Key: "bit_width", Widget: WidgetNumber, Default: defaults.Audio.BitWidth},
					{Key: "backend", Widget: WidgetSelect,
						Enum:    enumOptions(AudioBackendAuto, AudioBackendAudioService, AudioBackendLocal),
						Default: defaults.Audio.Backend},
				},
			},
			{
				Name: "voice_model",
				Fields: []FieldMeta{
					{Key: "provider", Label: "Realtime Provider", Widget: WidgetSelect,
						Enum:        realtimeProviderEnumOptions(),
						Default:     defaults.VoiceModel.Provider,
						VisibleWhen: all(eq("agent.input_mode", "realtime"))},
					{Key: "use_backend_agent", Label: "Use Backend Agent", Widget: WidgetBoolean,
						Help:        "Disabled by default. Enable to use the backend agent and expose its communication tools. When disabled, the realtime model gets the runtime tools that the backend agent would otherwise use. Provider reasoning is unaffected.",
						Default:     defaults.VoiceModel.UseBackendAgent,
						Advanced:    true,
						VisibleWhen: all(eq("agent.input_mode", "realtime"))},
				},
			},
			{
				Name: "voice_model_providers",
				Fields: []FieldMeta{
					{Key: "type", Label: "Realtime Provider Type", Widget: WidgetSelect,
						Enum:    realtimeProviderEnumOptions(),
						Default: defaults.VoiceModel.Provider},
					// OpenAI is absent on purpose: Speko serves that upstream over WebRTC
					// and every adapter here speaks WebSocket, so the option could only
					// fail after spending a mint request. Select the standalone "OpenAI
					// Realtime" provider type for a direct WebSocket session instead.
					{Key: "upstream_provider", Label: "Realtime Engine", Widget: WidgetSelect,
						Enum: []EnumOption{
							{Value: "google", Label: "Google Gemini Live"},
							{Value: "xai", Label: "xAI Grok Voice"},
						},
						Help:        "Required Speko S2S engine. Select Google or xAI and set its Model. Automatic routing is disabled because it may select an unsupported WebRTC route. For OpenAI Realtime, select it as the provider type instead. No separate engine API key is needed here.",
						VisibleWhen: all(eq("voice_model_providers.type", "speko"))},
					{Key: "agent_id", Label: "Speko Agent ID", Widget: WidgetText,
						Help: "Optional Speko agent ID.", Advanced: true,
						VisibleWhen: all(eq("voice_model_providers.type", "speko"))},
					{Key: "api_key", Label: "Realtime API Key", Widget: WidgetText, Secret: true, Layout: "wide",
						Help:        "Credential for the selected realtime provider. For Gemini Vertex, provide an OAuth access token. Use a literal value or $ENV_VAR.",
						Placeholder: "$REALTIME_API_KEY"},
					{Key: "auth_mode", Label: "Gemini Authentication", Widget: WidgetSelect,
						Enum:        []EnumOption{{Value: "api_key", Label: "Gemini API key"}, {Value: "vertex", Label: "Vertex OAuth"}},
						Default:     "api_key",
						VisibleWhen: all(eq("voice_model_providers.type", "gemini"))},
					{Key: "project_id", Label: "Google Cloud Project ID", Widget: WidgetText, Layout: "wide",
						VisibleWhen: all(eq("voice_model_providers.type", "gemini"), eq("voice_model_providers.auth_mode", "vertex"))},
					{Key: "location", Label: "Vertex Location", Widget: WidgetText,
						Placeholder: "us-central1",
						VisibleWhen: all(eq("voice_model_providers.type", "gemini"), eq("voice_model_providers.auth_mode", "vertex"))},
					{Key: "model", Label: "Realtime Model", Widget: WidgetText, Default: defaults.VoiceModel.Model, Layout: "wide",
						Help:            "Realtime model ID (provider-specific).",
						PlaceholderWhen: realtimeProviderPlaceholders("model")},
					{Key: "workspace_id", Label: "DashScope Workspace ID", Widget: WidgetText,
						Advanced:    true,
						VisibleWhen: all(eq("voice_model_providers.type", "qwen"))},
					{Key: "endpoint", Label: "WebSocket Endpoint", Widget: WidgetText, Layout: "wide",
						Help:        "Optional provider WebSocket endpoint override. Leave empty to use the provider default.",
						Advanced:    true,
						VisibleWhen: all(in("voice_model_providers.type", "qwen", "openai", "gemini", "xai"))},
					{Key: "thinking_level", Label: "Thinking Level", Widget: WidgetSelect,
						Enum: []EnumOption{
							{Value: "", Label: "LOW (default)"},
							{Value: "MINIMAL", Label: "MINIMAL"},
							{Value: "LOW", Label: "LOW"},
							{Value: "MEDIUM", Label: "MEDIUM"},
							{Value: "HIGH", Label: "HIGH"},
						},
						Help:        "Thinking depth for Gemini Live Extended Thinking models. Only applies to models that support thinking; other providers ignore it.",
						Advanced:    true,
						VisibleWhen: all(eq("voice_model_providers.type", "gemini"))},
					{Key: "realtime_protocol", Label: "Realtime Protocol", Widget: WidgetSelect,
						Enum: []EnumOption{
							{Value: "", Label: "OpenAI GA (default)"},
							{Value: "legacy", Label: "Legacy / Beta-compatible"},
						},
						Help:        "Use Legacy / Beta-compatible for OpenAI-compatible gateways that reject session.output_modalities or session.audio, such as MixRoute.",
						Advanced:    true,
						VisibleWhen: all(eq("voice_model_providers.type", "openai"))},
					{Key: "base_url", Label: "Provider Base URL", Widget: WidgetText, Layout: "wide",
						Help: "Optional provider API base URL; useful for Speko-compatible deployments or tests.", Advanced: true,
						VisibleWhen: all(eq("voice_model_providers.type", "speko"))},
					{Key: "region", Label: "Region", Widget: WidgetSelect,
						Enum: []EnumOption{
							{Value: "", Label: "Automatic"},
							{Value: "cn-beijing", Label: "China (Beijing)"},
							{Value: "ap-southeast-1", Label: "Singapore"},
						},
						Default: defaults.VoiceModel.Region, Advanced: true,
						VisibleWhen: all(eq("voice_model_providers.type", "qwen"))},
					{Key: "turn_detection", Label: "Turn Detection", Widget: WidgetSelect,
						Enum: []EnumOption{
							{Value: "server_vad", Label: "Server VAD"},
							{Value: "smart_turn", Label: "Smart Turn"},
						},
						Default: defaults.VoiceModel.TurnDetection, Advanced: true,
						VisibleWhen: all(eq("voice_model_providers.type", "qwen"))},
					{Key: "turn_detection_threshold", Label: "Turn Detection Threshold", Widget: WidgetNumber,
						Help: "Optional Qwen server_vad threshold. Leave empty to use the provider default.", Placeholder: "Default: 0.5", Nullable: true, Advanced: true,
						VisibleWhen: all(eq("voice_model_providers.type", "qwen"), eq("voice_model_providers.turn_detection", "server_vad"))},
					{Key: "turn_detection_silence_ms", Label: "Turn Detection Silence (ms)", Widget: WidgetNumber,
						Help: "Optional Qwen server_vad silence duration before completing a turn. Leave empty to use the provider default.", Placeholder: "Default: 800", Advanced: true,
						VisibleWhen: all(eq("voice_model_providers.type", "qwen"), eq("voice_model_providers.turn_detection", "server_vad"))},
					{Key: "voice", Label: "Voice", Widget: WidgetText, Default: defaults.VoiceModel.Voice,
						Help:            "Realtime system voice name or voice-clone ID (provider-specific).",
						PlaceholderWhen: realtimeProviderPlaceholders("voice")},
				},
			},
			{
				Name: "audio_archive",
				Fields: []FieldMeta{
					{Key: "enabled", Widget: WidgetBoolean, Default: defaults.AudioArchive.Enabled,
						Help:        "After enabling, save STT voice recording WAV for Web UI playback; Automatically delete old files when exceeding quantity or capacity limit.",
						VisibleWhen: all(eq("agent.input_mode", "stt"))},
					{Key: "storage_path", Widget: WidgetText, Default: defaults.AudioArchive.StoragePathOrDefault(),
						Layout:      "wide",
						VisibleWhen: all(eq("agent.input_mode", "stt"), truthy("audio_archive.enabled"))},
					{Key: "max_files", Widget: WidgetNumber, Default: defaults.AudioArchive.MaxFilesOrDefault(),
						VisibleWhen: all(eq("agent.input_mode", "stt"), truthy("audio_archive.enabled"))},
					{Key: "max_size_mb", Widget: WidgetNumber, Default: defaults.AudioArchive.MaxSizeMBOrDefault(),
						VisibleWhen: all(eq("agent.input_mode", "stt"), truthy("audio_archive.enabled"))},
				},
			},
			{
				Name: "frame_service",
				Fields: []FieldMeta{
					{Key: "keep_streamon", Label: "Keep STREAMON", Widget: WidgetBoolean,
						Default: defaults.FrameService.KeepStreamOn,
						Help:    "Keep the RK628 CSI capture stream enabled between screenshots. This reduces screenshot latency but increases idle power consumption."},
				},
			},
			{
				Name: "quick_capture",
				Fields: []FieldMeta{
					{Key: "enabled", Label: "Enabled", Widget: WidgetBoolean,
						Default: defaults.QuickCapture.EnabledOrDefault(),
						Help:    "Enable GPIO-triggered Screen Memory capture. Legacy GPIO32/GPIO33 wakeup remains independent."},
					{Key: "gpio_pin", Label: "GPIO Pin", Widget: WidgetNumber,
						Default: defaults.QuickCapture.GPIOPin,
						Help:    "Falling-edge trigger pin. Supported values are 0 (disabled) and GPIO3 (physical pin 38 on Luckfox Pico Zero). GPIO32 and GPIO33 remain reserved for legacy wakeup."},
					{Key: "screen_memory_ttl", Label: "Screen Memory TTL", Widget: WidgetText,
						Default: defaults.QuickCapture.ScreenMemoryTTLOrDefault(),
						Help:    "Retention period for captured Screen Memory entries, such as 90d, or forever."},
				},
			},
			{
				Name: "voice_notifications",
				Fields: []FieldMeta{
					{Key: "retention_days", Label: "Notification memory retention (days)", Widget: WidgetNumber,
						Default: defaults.VoiceNotifications.RetentionDaysOrDefault(),
						Help:    "Number of days to keep processed notification memory during normal cleanup. Storage pressure may use shorter fallback windows."},
				},
			},
			{
				Name: "storage",
				Fields: []FieldMeta{
					{Key: "monitor_enabled", Widget: WidgetBoolean, Default: defaults.Storage.MonitorEnabled},
					{Key: "mount_point", Widget: WidgetText, Default: defaults.Storage.MountPointOrDefault()},
					{Key: "device", Widget: WidgetText, Default: defaults.Storage.DeviceOrDefault()},
					{Key: "min_card_free_mb", Widget: WidgetNumber, Default: defaults.Storage.MinCardFreeMBOrDefault()},
					{Key: "migrate_start_free_pct", Widget: WidgetNumber, Default: defaults.Storage.MigrateStartFreePct},
					{Key: "migrate_stop_free_pct", Widget: WidgetNumber, Default: defaults.Storage.MigrateStopFreePct},
				},
			},
			{
				Name: "log",
				Fields: []FieldMeta{
					{Key: "level", Label: "Log level", Widget: WidgetSelect,
						Enum: enumOptions("debug", "info", "warn", "error"),
						Help: "Minimum severity written to the Agent log.", Default: defaults.Log.LevelOrDefault()},
					{Key: "llm_http_retention_days", Label: "LLM HTTP log retention (days)", Widget: WidgetNumber,
						Help:    "Number of days to keep raw LLM HTTP log files before automatic cleanup.",
						Default: defaults.Log.LLMHTTPRetentionDaysOrDefault()},
				},
			},
			{
				Name: "ota",
				Fields: []FieldMeta{
					{Key: "github_proxy_url", Label: "GitHub Proxy URL", Widget: WidgetText, Default: "",
						Help:        "Optional proxy to accelerate GitHub downloads (e.g., https://gh-proxy.com/ or https://ghfast.top/)",
						Placeholder: "Leave empty to disable",
						Layout:      "wide"},
				},
			},
			{
				Name: "hid",
				Fields: []FieldMeta{
					{Key: "keyboard_layout", Widget: WidgetSelect,
						Help:    "How the phone interprets the USB keyboard. Keep qwerty unless typed text comes out transposed; then switch the phone input language to match, save, and reboot the board.",
						Enum:    keyboardLayoutEnumOptions(),
						Default: defaults.HID.KeyboardLayoutOrDefault()},
					{Key: "input_backend", Widget: WidgetSelect,
						Enum:    enumOptions("hid", "adb"),
						Default: defaults.HID.InputBackend},
					{Key: "keyboard_device", Widget: WidgetText, Default: defaults.HID.KeyboardDevice, Layout: "wide"},
					{Key: "mouse_device", Widget: WidgetText, Default: defaults.HID.MouseDevice, Layout: "wide"},
					{Key: "android_keyboard_device", Widget: WidgetText, Default: defaults.HID.AndroidKeyboardDevice, Layout: "wide"},
					{Key: "frame_socket", Widget: WidgetText, Default: defaults.HID.FrameSocket, Layout: "wide"},
				},
			},
			{
				Name: "search",
				Fields: []FieldMeta{
					{Key: "provider", Label: "Search provider", Widget: WidgetSelect,
						Enum:    enumOptions(searchProviderDuckDuckGo, searchProviderBrave, searchProviderTavily),
						Default: defaults.Search.ProviderOrDefault()},
					{Key: "api_key", Label: "Search API key", Widget: WidgetText, Secret: true, Layout: "wide",
						VisibleWhen: all(ne("search.provider", searchProviderDuckDuckGo))},
				},
			},
			{
				Name: "telemetry",
				Fields: []FieldMeta{
					{Key: "enabled", Widget: WidgetBoolean, Default: false},
					{Key: "provider", Widget: WidgetSelect,
						Enum:        []EnumOption{{Value: "", Label: "langfuse (default)"}, {Value: "langfuse"}},
						VisibleWhen: all(truthy("telemetry.enabled"))},
					{Key: "base_url", Widget: WidgetText, Placeholder: "http://langfuse.example.com:3000", Layout: "wide",
						VisibleWhen: all(truthy("telemetry.enabled"))},
					{Key: "public_key", Widget: WidgetText, Secret: true, Layout: "wide",
						VisibleWhen: all(truthy("telemetry.enabled"))},
					{Key: "secret_key", Widget: WidgetText, Secret: true, Layout: "wide",
						VisibleWhen: all(truthy("telemetry.enabled"))},
					{Key: "upload_screenshots", Widget: WidgetBoolean,
						VisibleWhen: all(truthy("telemetry.enabled"))},
					{Key: "upload_timeout_sec", Widget: WidgetNumber,
						VisibleWhen: all(truthy("telemetry.enabled"))},
					{Key: "max_retry", Widget: WidgetNumber,
						VisibleWhen: all(truthy("telemetry.enabled"))},
					{Key: "environment", Widget: WidgetText,
						VisibleWhen: all(truthy("telemetry.enabled"))},
					{Key: "tags", Widget: WidgetList, Layout: "wide",
						VisibleWhen: all(truthy("telemetry.enabled"))},
				},
			},
			{
				Name: "live_activity",
				Fields: []FieldMeta{
					{Key: "enabled", Widget: WidgetBoolean, Default: true},
				},
			},
			{
				// The "agent" UI section maps to the top-level Config fields.
				Name: "agent",
				Fields: []FieldMeta{
					{Key: "locale", Widget: WidgetSelect,
						Enum:    enumOptions(localeSimplifiedChinese, localeEnglishUS),
						Default: defaults.LocaleOrDefault()},
					{Key: "timezone", Label: "Time zone", Widget: WidgetSelect,
						Help:    "Time zone used for the Agent's date context and controller shell commands.",
						Enum:    enumOptions(SupportedTimezones()...),
						Default: defaults.TimezoneOrDefault()},
					{Key: "input_mode", Widget: WidgetSelect,
						Enum:    enumOptions("stt", "realtime"),
						Default: defaults.InputMode},
					{Key: "tap_wakeup_enabled", Label: "Tap wakeup", Widget: WidgetBoolean,
						Help:    "When on, the tap button can wake Aiden. Turning it off disables only that button path, not other activation such as Realtime /api/chat.",
						Default: defaults.TapWakeupEnabledOrDefault()},
					{Key: "mcp_enabled", Label: "MCP", Widget: WidgetBoolean,
						Help:    "When on, Agent tools can be used over MCP.",
						Default: defaults.MCPEnabledOrDefault()},
					{Key: "vad_backend", Widget: WidgetSelect,
						Enum:        enumOptions(defaultVADBackend, "cpu"),
						Default:     defaults.VADBackend,
						VisibleWhen: all(eq("agent.input_mode", "stt"))},
					{Key: "vad_model_path", Widget: WidgetText,
						Default:     defaults.VADModelPath,
						Layout:      "wide",
						VisibleWhen: all(eq("agent.input_mode", "stt"))},
					{Key: "vad_helper_path", Widget: WidgetText,
						Default:     defaults.VADHelperPath,
						Layout:      "wide",
						VisibleWhen: all(eq("agent.input_mode", "stt"))},
					{Key: "vad_speech_threshold", Widget: WidgetSelect,
						Default:     defaults.VADSpeechThreshold,
						Range:       &Range{Min: 0, Max: 1, Step: 0.05, Precision: 2},
						VisibleWhen: all(eq("agent.input_mode", "stt"))},
					{Key: "silence_ms", Widget: WidgetNumber, Default: defaults.SilenceMs,
						VisibleWhen: all(eq("agent.input_mode", "stt"))},
					{Key: "min_speech_ms", Widget: WidgetNumber, Default: defaults.MinSpeechMs,
						VisibleWhen: all(eq("agent.input_mode", "stt"))},
					{Key: "voice_followup_enabled", Widget: WidgetBoolean, Default: defaults.VoiceFollowupEnabledOrDefault(),
						VisibleWhen: all(eq("agent.input_mode", "stt"))},
					{Key: "voice_followup_timeout_ms", Widget: WidgetNumber, Default: defaults.VoiceFollowupTimeoutMs,
						VisibleWhen: all(eq("agent.input_mode", "stt"), truthy("agent.voice_followup_enabled"))},
					{Key: "voice_first_turn_timeout_ms", Widget: WidgetNumber, Default: defaults.VoiceFirstTurnTimeoutMs,
						VisibleWhen: all(eq("agent.input_mode", "stt"), truthy("agent.voice_followup_enabled"))},
					{Key: "voice_max_turns", Widget: WidgetNumber, Default: defaults.VoiceMaxTurns,
						VisibleWhen: all(eq("agent.input_mode", "stt"), truthy("agent.voice_followup_enabled"))},
					{Key: "voice_interrupt_on_wakeup", Widget: WidgetBoolean, Default: defaults.VoiceInterruptOnWakeupOrDefault(),
						VisibleWhen: all(eq("agent.input_mode", "stt"), truthy("agent.voice_followup_enabled"))},
					{Key: "voice_streaming_tts_enabled", Widget: WidgetBoolean, Default: defaults.VoiceStreamingTTSEnabledOrDefault(),
						VisibleWhen: all(eq("agent.input_mode", "stt"))},
					{Key: "voice_tool_call_speech", Widget: WidgetBoolean, Default: defaults.VoiceToolCallSpeechOrDefault(),
						VisibleWhen: all(eq("agent.input_mode", "stt"))},
					{Key: "voice_progress_speech_enabled", Widget: WidgetBoolean, Default: defaults.VoiceProgressSpeechEnabledOrDefault(),
						VisibleWhen: all(eq("agent.input_mode", "stt"))},
					{Key: "voice_max_response_tokens", Widget: WidgetNumber, Default: defaults.VoiceMaxResponseTokens,
						VisibleWhen: all(eq("agent.input_mode", "stt"))},
					{Key: "max_iterations", Label: "Maximum tool-call iterations", Widget: WidgetNumber,
						Help:    "Maximum number of model and tool loops for one task. -1 allows unlimited iterations.",
						Default: defaults.MaxIterations},
					{Key: "context_prune_threshold", Label: "Context prune threshold (fraction)", Widget: WidgetNumber,
						Help:    "Fraction of the usable model input budget that triggers cleanup of stale state and older tool exchanges, including during one long-running tool loop, cleaning down to 6/7 of the trigger. Must be 0 or greater than 0 and less than 1; 0 uses 0.5. Capped at context_compaction_threshold so this cheap pass runs before the conversation summary.",
						Default: defaults.ContextPruneThreshold, Placeholder: "0 = automatic (0.5)"},
					{Key: "context_compaction_threshold", Label: "Context compaction threshold (fraction)", Widget: WidgetNumber,
						Help:    "Fraction of the usable model input budget at which the conversation is summarized. Must be 0 or greater than 0 and less than 1; 0 uses 0.8.",
						Default: defaults.ContextCompactionThreshold, Placeholder: "0 = automatic (0.8)"},
					{Key: "screenshot_keep_n", Widget: WidgetNumber, Default: defaults.ScreenshotKeepN},
					{Key: "screenshot_prune_interval", Widget: WidgetNumber, Default: defaults.ScreenshotPruneInterval},
					{Key: "screen_stable_timeout_ms", Widget: WidgetNumber, Default: defaults.ScreenStableTimeoutMs},
					{Key: "screen_stable_ms", Widget: WidgetNumber, Default: defaults.ScreenStableMs},
					{Key: "screen_stable_diff_threshold", Widget: WidgetNumber, Default: defaults.ScreenStableDiffThreshold},
					{Key: "prompt", Label: "Prompt", Widget: WidgetTextarea, Layout: "wide",
						Help: "Appended after the built-in Agent instruction. Use it for device, project, or environment-specific requirements."},
				},
			},
		},
	}
	return withDisplayDefaults(metadata)
}
