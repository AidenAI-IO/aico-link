package agent

import (
	"strings"

	"aico-agent/internal/agent/model"
)

// modelSpecRegistry covers the chat models referenced from this repo (overlay
// agent.toml, configuration docs, common OpenRouter routes used in dev/staging)
// plus the current mainstream multimodal + tool-calling models. Numbers reflect
// public model cards; when in doubt prefer the smaller advertised window so we
// err on the side of triggering compression earlier instead of overflowing the
// prompt. Models are keyed by both their provider-prefixed canonical id and the
// bare model name so configs that drop the provider prefix (e.g. the openai
// provider pointed at a proxy with model = "gpt-5.4") still resolve.
//
// Only models that support both image input and tool/function calling are
// listed here, since those are the capabilities the agent relies on.
var modelSpecRegistry = map[string]model.ModelSpec{
	// OpenAI GPT-5.x family (vision + tool calling). ContextWindow is the total
	// advertised window; compaction logic uses (ContextWindow - MaxOutput) for
	// the actual input budget.
	"openai/gpt-5.5":      {ContextWindow: 1_050_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"none", "low", "medium", "high", "xhigh"}, true)},
	"gpt-5.5":             {ContextWindow: 1_050_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"none", "low", "medium", "high", "xhigh"}, true)},
	"openai/gpt-5.5-pro":  {ContextWindow: 1_050_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"medium", "high", "xhigh"}, true)},
	"gpt-5.5-pro":         {ContextWindow: 1_050_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"medium", "high", "xhigh"}, true)},
	"openai/gpt-5.4":      {ContextWindow: 1_050_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"none", "low", "medium", "high", "xhigh"}, true)},
	"gpt-5.4":             {ContextWindow: 1_050_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"none", "low", "medium", "high", "xhigh"}, true)},
	"openai/gpt-5.4-mini": {ContextWindow: 400_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"none", "low", "medium", "high", "xhigh"}, true)},
	"gpt-5.4-mini":        {ContextWindow: 400_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"none", "low", "medium", "high", "xhigh"}, true)},
	"openai/gpt-5.4-nano": {ContextWindow: 400_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"none", "low", "medium", "high", "xhigh"}, true)},
	"gpt-5.4-nano":        {ContextWindow: 400_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"none", "low", "medium", "high", "xhigh"}, true)},

	// Anthropic Claude family (vision + tool calling).
	"anthropic/claude-fable-5":  {ContextWindow: 1_000_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"low", "medium", "high", "xhigh", "max"}, true)},
	"claude-fable-5":            {ContextWindow: 1_000_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"low", "medium", "high", "xhigh", "max"}, true)},
	"anthropic/claude-opus-5":   {ContextWindow: 1_000_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"low", "medium", "high", "xhigh", "max"}, true)},
	"claude-opus-5":             {ContextWindow: 1_000_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"low", "medium", "high", "xhigh", "max"}, true)},
	"anthropic/claude-opus-4-8": {ContextWindow: 1_000_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"low", "medium", "high", "xhigh", "max"}, true)},
	"claude-opus-4-8":           {ContextWindow: 1_000_000, MaxOutput: 128_000, Reasoning: effortReasoning([]string{"low", "medium", "high", "xhigh", "max"}, true)},
	// Sonnet 4.6 exposes adaptive effort as the primary control and retains
	// the exact budget_tokens override for callers that need a hard ceiling.
	"anthropic/claude-sonnet-4-6": {ContextWindow: 1_000_000, MaxOutput: 128_000, Reasoning: effortBudgetReasoning([]string{"low", "medium", "high", "max"}, 1024, 0, true)},
	"claude-sonnet-4-6":           {ContextWindow: 1_000_000, MaxOutput: 128_000, Reasoning: effortBudgetReasoning([]string{"low", "medium", "high", "max"}, 1024, 0, true)},
	"anthropic/claude-haiku-4-5":  {ContextWindow: 200_000, MaxOutput: 64_000, Reasoning: budgetReasoning(1024, 0, true)},
	"claude-haiku-4-5":            {ContextWindow: 200_000, MaxOutput: 64_000, Reasoning: budgetReasoning(1024, 0, true)},

	// Google Gemini family (vision + tool calling). Context windows are per
	// official model cards. The effort lists mirror the thinking_level values
	// each model accepts: Gemini 3.8, 3.7, and the 2.5 models take
	// low/medium/high; 3.6 and 3.5 Flash additionally take minimal. No Gemini
	// model here can disable thinking, so none set CanDisable. Pin low on the
	// current Flash models to keep voice interactions responsive.
	//
	// The Gemini 3 models pin DefaultTemperature to their documented default of
	// 1.0. Google strongly recommends leaving temperature there for all Gemini 3
	// models, warning that setting it below 1.0 may cause looping or degraded
	// performance on complex reasoning and math tasks. Without this pin the
	// global defaultModelTemperature (0.2) applies, so every request would ship
	// the exact value Google advises against. The 2.5 models are deliberately
	// left unpinned: their default is per-model metadata reported by models.get
	// rather than a documented constant, and Google's migration note treats an
	// explicit low temperature on 2.5 as a legitimate prior pattern. An explicit
	// model.temperature still wins for every entry.
	"google/gemini-3.8-flash": {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultTemperature: floatPtr(1), DefaultReasoningEffort: stringPtr("low"), Reasoning: effortReasoning([]string{"low", "medium", "high"}, false)},
	"gemini-3.8-flash":        {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultTemperature: floatPtr(1), DefaultReasoningEffort: stringPtr("low"), Reasoning: effortReasoning([]string{"low", "medium", "high"}, false)},
	"google/gemini-3.7-flash": {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultTemperature: floatPtr(1), DefaultReasoningEffort: stringPtr("low"), Reasoning: effortReasoning([]string{"low", "medium", "high"}, false)},
	"gemini-3.7-flash":        {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultTemperature: floatPtr(1), DefaultReasoningEffort: stringPtr("low"), Reasoning: effortReasoning([]string{"low", "medium", "high"}, false)},
	"google/gemini-3.6-flash": {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultTemperature: floatPtr(1), Reasoning: effortReasoning([]string{"minimal", "low", "medium", "high"}, false)},
	"gemini-3.6-flash":        {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultTemperature: floatPtr(1), Reasoning: effortReasoning([]string{"minimal", "low", "medium", "high"}, false)},
	"google/gemini-3.5-flash": {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultTemperature: floatPtr(1), Reasoning: effortReasoning([]string{"minimal", "low", "medium", "high"}, false)},
	"gemini-3.5-flash":        {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultTemperature: floatPtr(1), Reasoning: effortReasoning([]string{"minimal", "low", "medium", "high"}, false)},
	"google/gemini-3.5-pro":   {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultTemperature: floatPtr(1)},
	"gemini-3.5-pro":          {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultTemperature: floatPtr(1)},
	"google/gemini-2.5-flash": {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultReasoningEffort: stringPtr("low"), Reasoning: effortReasoning([]string{"low", "medium", "high"}, false)},
	"gemini-2.5-flash":        {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultReasoningEffort: stringPtr("low"), Reasoning: effortReasoning([]string{"low", "medium", "high"}, false)},
	"google/gemini-2.5-pro":   {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultReasoningEffort: stringPtr("low"), Reasoning: effortReasoning([]string{"low", "medium", "high"}, false)},
	"gemini-2.5-pro":          {ContextWindow: 1_048_576, MaxOutput: 65_536, DefaultReasoningEffort: stringPtr("low"), Reasoning: effortReasoning([]string{"low", "medium", "high"}, false)},

	// Moonshot Kimi K3 (vision + tool calling). Reached via the Moonshot
	// OpenAI-compatible endpoint (bare model name) or the OpenRouter route.
	// K3 only accepts temperature=1, so pin it as the default; a user-set
	// model.temperature still overrides. K3 is forced-reasoning and defaults to
	// "max" effort, which stalls streaming for several seconds before any content
	// arrives; pin "low" as the default to keep voice interactions responsive. A
	// user-set model.reasoning_effort still overrides.
	"moonshotai/kimi-k3": {ContextWindow: 1_048_576, MaxOutput: 131_072, DefaultTemperature: floatPtr(1), DefaultReasoningEffort: stringPtr("low"), Reasoning: effortReasoning([]string{"low", "high", "max"}, false)},
	"kimi-k3":            {ContextWindow: 1_048_576, MaxOutput: 131_072, DefaultTemperature: floatPtr(1), DefaultReasoningEffort: stringPtr("low"), Reasoning: effortReasoning([]string{"low", "high", "max"}, false)},

	// ByteDance Doubao Seed 2.1 Pro on Volcengine Ark (vision + tool calling),
	// reached through Ark's OpenAI-compatible endpoint. The window and output cap
	// come from the published model directory listings (256K context / 128K
	// output); the Ark console is the authority, so set context_window and
	// model_max_output_tokens explicitly if a dated release differs. Ark supports
	// reasoning_effort minimal/low/medium/high and treats an omitted value as
	// "high", which delays the first streamed token by seconds; pin "low" so
	// voice interactions stay responsive. A user-set model.reasoning_effort still
	// overrides. Ark places no constraint on temperature, so no default is set.
	// Other dated releases need their own entry, keyed by the exact Ark model id.
	"doubao-seed-2-1-pro-260628": {ContextWindow: 262_144, MaxOutput: 131_072, DefaultReasoningEffort: stringPtr("low"), Reasoning: effortReasoning([]string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}, true)},

	// DeepSeek Flash supports vision and tool calling.
	// Published limits: https://api-docs.deepseek.com/quick_start/pricing
	// (2026-09-14). Use a conservative 1,000,000 tokens for the advertised 1M
	// window. Default to non-thinking mode for fast device interactions.
	"deepseek-flash": {ContextWindow: 1_000_000, MaxOutput: 393_216, DefaultReasoningEffort: stringPtr("none"), Reasoning: effortReasoning([]string{"none", "low", "high", "max"}, true)},
	// Pro is text-only. Restore alongside its display preset after verifying vision support and limits.
	// "deepseek-v4-pro": {ContextWindow: 1_000_000, MaxOutput: 393_216, DefaultReasoningEffort: stringPtr("none"), Reasoning: effortReasoning([]string{"none", "low", "high", "max"}, true)},

	// Qwen3.8-27B on OpenRouter (vision + tool calling). Context window is
	// 256K per the Qwen3 published model card. Default reasoning_effort is
	// "medium" because Qwen3.8 includes native reasoning and performs best
	// with medium-level reasoning effort out of the box.
	"qwen/qwen3.8-27b": {ContextWindow: 262_144, MaxOutput: 32_768, DefaultReasoningEffort: stringPtr("medium"), Reasoning: effortReasoning([]string{"none", "low", "medium", "high"}, true)},
	"qwen3.8-27b":      {ContextWindow: 262_144, MaxOutput: 32_768, DefaultReasoningEffort: stringPtr("medium"), Reasoning: effortReasoning([]string{"none", "low", "medium", "high"}, true)},

	// Existing entries retained for back-compat with dev/staging configs.
	"anthropic/claude-3.5-sonnet":  {ContextWindow: 200_000, MaxOutput: 8_192, Reasoning: unsupportedReasoning()},
	"anthropic/claude-3.7-sonnet":  {ContextWindow: 200_000, MaxOutput: 8_192, Reasoning: budgetReasoning(1024, 0, true)},
	"bytedance-seed/seed-2.0-lite": {ContextWindow: 128_000, MaxOutput: 8_192},
	"openai/gpt-4o":                {ContextWindow: 128_000, MaxOutput: 16_384, Reasoning: unsupportedReasoning()},
	"openai/gpt-4o-mini":           {ContextWindow: 128_000, MaxOutput: 16_384, Reasoning: unsupportedReasoning()},
}

func effortReasoning(efforts []string, canDisable bool) *model.ReasoningSpec {
	return &model.ReasoningSpec{Supported: true, Mode: "effort", Efforts: efforts, CanDisable: canDisable}
}

func unsupportedReasoning() *model.ReasoningSpec {
	return &model.ReasoningSpec{Supported: false}
}

func budgetReasoning(min, max int, canDisable bool) *model.ReasoningSpec {
	return &model.ReasoningSpec{Supported: true, Mode: "budget_tokens", CanDisable: canDisable, BudgetTokensMin: min, BudgetTokensMax: max}
}

func effortBudgetReasoning(efforts []string, min, max int, canDisable bool) *model.ReasoningSpec {
	return &model.ReasoningSpec{Supported: true, Mode: "effort", Efforts: efforts, CanDisable: canDisable, BudgetTokensMin: min, BudgetTokensMax: max}
}

// floatPtr returns a pointer to v, letting map-literal ModelSpec entries set
// optional float fields like DefaultTemperature inline.
func floatPtr(v float64) *float64 {
	return &v
}

// stringPtr returns a pointer to v, letting map-literal ModelSpec entries set
// optional string fields like DefaultReasoningEffort inline.
func stringPtr(v string) *string {
	return &v
}

// boolPtr returns a pointer to v, letting request payloads set optional bool
// fields inline without temporary variables.
func boolPtr(v bool) *bool {
	return &v
}

func lookupModelSpecKey(key string) (model.ModelSpec, bool) {
	if spec, ok := modelSpecRegistry[key]; ok {
		return cloneModelSpec(spec), true
	}

	// Claude 4 model IDs are also commonly written with a dotted version
	// (for example, claude-sonnet-4.6). Keep the registry canonicalized on the
	// hyphenated IDs while accepting either spelling from provider configs.
	if strings.Contains(key, "claude-") {
		if hyphenated := strings.Replace(key, "-4.", "-4-", 1); hyphenated != key {
			if spec, ok := modelSpecRegistry[hyphenated]; ok {
				return cloneModelSpec(spec), true
			}
		}
	}

	return model.ModelSpec{}, false
}

func cloneModelSpec(spec model.ModelSpec) model.ModelSpec {
	if spec.Reasoning != nil {
		reasoning := *spec.Reasoning
		reasoning.Efforts = append([]string(nil), spec.Reasoning.Efforts...)
		if reasoning.DefaultEffort == "" && spec.DefaultReasoningEffort != nil {
			reasoning.DefaultEffort = *spec.DefaultReasoningEffort
		}
		spec.Reasoning = &reasoning
	}
	return spec
}

// LookupModelSpec returns the spec for the given (provider, model). It tries
// the full canonical id first (e.g. "google/gemini-3.5-flash") and then falls
// back to the bare model name without a provider prefix so configurations that
// drop the prefix still resolve. The boolean is false when the model is
// unknown; callers must then fall back to a configured default.
func LookupModelSpec(provider, modelName string) (model.ModelSpec, bool) {
	key := strings.ToLower(strings.TrimSpace(modelName))
	if key == "" {
		return model.ModelSpec{}, false
	}
	if spec, ok := lookupModelSpecKey(key); ok {
		return scopeModelSpecToProvider(provider, spec), true
	}
	if i := strings.LastIndex(key, "/"); i >= 0 && i+1 < len(key) {
		if spec, ok := lookupModelSpecKey(key[i+1:]); ok {
			return scopeModelSpecToProvider(provider, spec), true
		}
	}
	// Provider-prefixed retry for entries registered without a provider.
	if provider != "" {
		full := strings.ToLower(strings.TrimSpace(provider)) + "/" + key
		if spec, ok := lookupModelSpecKey(full); ok {
			return scopeModelSpecToProvider(provider, spec), true
		}
	}
	return model.ModelSpec{}, false
}

func scopeModelSpecToProvider(provider string, spec model.ModelSpec) model.ModelSpec {
	if spec.Reasoning == nil || strings.TrimSpace(provider) == "" || strings.EqualFold(strings.TrimSpace(provider), "anthropic") {
		return spec
	}
	if spec.Reasoning.BudgetTokensMin == 0 && spec.Reasoning.BudgetTokensMax == 0 && spec.Reasoning.Mode != "budget_tokens" {
		return spec
	}
	if len(spec.Reasoning.Efforts) == 0 {
		spec.Reasoning = unsupportedReasoning()
		return spec
	}
	reasoning := *spec.Reasoning
	reasoning.Mode = "effort"
	reasoning.BudgetTokensMin = 0
	reasoning.BudgetTokensMax = 0
	spec.Reasoning = &reasoning
	return spec
}
