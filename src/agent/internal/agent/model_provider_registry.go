package agent

import (
	"fmt"
	"net/http"
	"strings"

	"aico-agent/internal/agent/model"
	"github.com/tmc/langchaingo/llms"
	fakellm "github.com/tmc/langchaingo/llms/fake"
	"github.com/tmc/langchaingo/llms/ollama"
)

type ModelBuildContext struct {
	HTTPClient        *http.Client
	OllamaHTTPClient  *http.Client
	RawHTTPLogger     RawHTTPLogger
	SessionIDProvider func() string
	PromptCachePolicy PromptCachePolicy
	ModelSpec         func() model.ModelSpec
}

type ModelProviderBuilder func(ModelBuildContext, ModelConfig) (llms.Model, error)

type modelProviderDefinition struct {
	providerType                   string
	allowsCustomBaseURL            bool
	usesProviderTemperatureDefault bool
	supportsResponses              bool
	supportsResponsesStateful      bool
	supportsInteractions           bool
	supportsInteractionsStateful   bool
	// chatCompletionsUnsupported marks a provider that has no usable
	// OpenAI-compatible /chat/completions transport, so an explicit
	// api_mode=chat_completions must be rejected instead of silently building a
	// client that fails at request time.
	chatCompletionsUnsupported bool
	hiddenFromConfigUI         bool
	build                      ModelProviderBuilder
}

var modelProviderDefinitions = []modelProviderDefinition{
	{
		providerType:      "openrouter",
		supportsResponses: true,
		build:             buildOpenRouterModel,
	},
	{
		providerType:              "openai",
		allowsCustomBaseURL:       true,
		supportsResponses:         true,
		supportsResponsesStateful: true,
		build: func(ctx ModelBuildContext, cfg ModelConfig) (llms.Model, error) {
			return buildOpenAICompatibleModel(ctx, cfg, "https://api.openai.com/v1", responsesDialectOpenAI), nil
		},
	},
	{
		providerType:        "anthropic",
		allowsCustomBaseURL: true,
		build:               buildAnthropicModel,
	},
	{
		providerType: "kimi",
		build: func(ctx ModelBuildContext, cfg ModelConfig) (llms.Model, error) {
			return buildKimiModel(ctx, cfg, moonshotGlobalBaseURL)
		},
	},
	{
		providerType: "kimi-cn",
		build: func(ctx ModelBuildContext, cfg ModelConfig) (llms.Model, error) {
			return buildKimiModel(ctx, cfg, moonshotCNBaseURL)
		},
	},
	{
		providerType:              "volcengine",
		supportsResponses:         true,
		supportsResponsesStateful: true,
		build: func(ctx ModelBuildContext, cfg ModelConfig) (llms.Model, error) {
			return buildOpenAICompatibleModel(ctx, cfg, arkBeijingBaseURL, responsesDialectVolcengine), nil
		},
	},
	{
		providerType:      "deepseek",
		supportsResponses: true,
		build:             buildDeepSeekModel,
	},
	{
		providerType:        "ollama",
		allowsCustomBaseURL: true,
		build:               buildOllamaModel,
	},
	{
		providerType:                   "gemini",
		allowsCustomBaseURL:            true,
		usesProviderTemperatureDefault: true,
		supportsInteractions:           true,
		supportsInteractionsStateful:   true,
		chatCompletionsUnsupported:     true,
		build:                          buildGeminiModel,
	},
	{
		providerType:              "fake",
		supportsResponses:         true,
		supportsResponsesStateful: true,
		hiddenFromConfigUI:        true,
		build: func(_ ModelBuildContext, cfg ModelConfig) (llms.Model, error) {
			return fakellm.NewFakeLLM(cfg.Responses), nil
		},
	},
}

func buildOpenAICompatibleModel(ctx ModelBuildContext, cfg ModelConfig, defaultBaseURL string, dialect responsesDialect) llms.Model {
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	apiMode := normalizeModelAPIMode(cfg.APIMode)
	if apiMode == modelAPIModeResponses || apiMode == modelAPIModeResponsesStateful {
		return newResponsesModel(baseURL, cfg.Model, resolveToken(cfg), ctx.HTTPClient, responsesModelOptions{
			rawLogger:                ctx.RawHTTPLogger,
			sessionIDProvider:        ctx.SessionIDProvider,
			reasoningEffort:          cfg.ReasoningEffort,
			temperature:              cfg.Temperature,
			providerManagedContext:   apiMode == modelAPIModeResponsesStateful,
			contextManagement:        cfg.ResponsesContextManagement,
			compactThreshold:         cfg.ResponsesCompactThreshold,
			contextEditTrigger:       cfg.ResponsesContextEditTrigger,
			contextEditKeep:          cfg.ResponsesContextEditKeep,
			contextEditClearThinking: cfg.ResponsesContextEditClearThinking,
			truncation:               cfg.ResponsesTruncation,
			include:                  cfg.ResponsesInclude,
			dialect:                  dialect,
		})
	}
	return newOpenAICompatibleModel(baseURL, cfg.Model, resolveToken(cfg), ctx.HTTPClient, openAICompatibleOptions(ctx, cfg)...)
}

func buildDeepSeekModel(ctx ModelBuildContext, cfg ModelConfig) (llms.Model, error) {
	// Default to non-thinking mode for all DeepSeek models, including custom
	// model IDs not registered in model_specs.go. The model spec registry
	// already sets this default for known models like "deepseek-flash", but
	// this fallback ensures direct ModelManager users and custom IDs (e.g.,
	// "deepseek-custom-v1") also start in non-thinking mode for fast device
	// interactions. User-provided reasoning_effort always overrides.
	cfg.ReasoningEffort = strings.ToLower(strings.TrimSpace(cfg.ReasoningEffort))
	if cfg.ReasoningEffort == "" {
		cfg.ReasoningEffort = "none"
	}
	thinkingEnabled := cfg.ReasoningEffort != "none"
	switch apiMode := normalizeModelAPIMode(cfg.APIMode); apiMode {
	case modelAPIModeResponses:
		return newResponsesModel(deepseekBaseURL, cfg.Model, resolveToken(cfg), ctx.HTTPClient, responsesModelOptions{
			rawLogger:         ctx.RawHTTPLogger,
			reasoningEffort:   cfg.ReasoningEffort,
			temperature:       cfg.Temperature,
			ignoreTemperature: thinkingEnabled,
			dialect:           responsesDialectDeepSeek,
		}), nil
	case modelAPIModeResponsesStateful:
		return nil, fmt.Errorf("model.api_mode=responses_stateful is not supported by DeepSeek; its /responses endpoint is stateless and does not support previous_response_id")
	case modelAPIModeChatCompletions:
	default:
		return nil, fmt.Errorf("invalid model.api_mode: %s", cfg.APIMode)
	}
	opts := append(openAICompatibleOptions(ctx, cfg), withOpenAICompatibleDialect(compatibleDialectDeepSeek))
	if thinkingEnabled {
		opts = append(opts, withOpenAICompatibleIgnoreTemperature())
	}
	return newOpenAICompatibleModel(deepseekBaseURL, cfg.Model, resolveToken(cfg), ctx.HTTPClient, opts...), nil
}

func buildKimiModel(ctx ModelBuildContext, cfg ModelConfig, defaultBaseURL string) (llms.Model, error) {
	apiMode := normalizeModelAPIMode(cfg.APIMode)
	if apiMode == "" && strings.TrimSpace(cfg.APIMode) != "" {
		return nil, fmt.Errorf("invalid model.api_mode: %s", cfg.APIMode)
	}
	if apiMode == modelAPIModeResponses || apiMode == modelAPIModeResponsesStateful {
		return nil, fmt.Errorf("model.api_mode=%s is not supported by Moonshot Kimi; its official endpoint implements OpenAI-compatible Chat Completions, not /responses", apiMode)
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	return newOpenAICompatibleModel(baseURL, cfg.Model, resolveToken(cfg), ctx.HTTPClient, openAICompatibleOptions(ctx, cfg)...), nil
}

func buildOpenRouterModel(ctx ModelBuildContext, cfg ModelConfig) (llms.Model, error) {
	apiMode := normalizeModelAPIMode(cfg.APIMode)
	if apiMode == modelAPIModeResponsesStateful {
		return nil, fmt.Errorf("model.api_mode=responses_stateful is not supported by OpenRouter; its /responses endpoint is stateless and rejects store=true or previous_response_id")
	}
	token := resolveToken(cfg)
	if token == "" {
		if env, ok := providerAPIKeyEnv(cfg.APIKey); ok && env != "" {
			return nil, fmt.Errorf("missing the OpenRouter API key, set it in the %s environment variable", env)
		}
		return nil, fmt.Errorf("missing the OpenRouter API key, set api_key on the provider record")
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1"
	}
	opts := append(openAICompatibleOptions(ctx, cfg),
		withOpenAICompatibleSessionSticky(ctx.SessionIDProvider),
		withOpenAICompatibleRouterMetadata(),
		withOpenAICompatibleDialect(compatibleDialectOpenRouter))
	if ctx.PromptCachePolicy.UsesExplicitCacheControl() {
		opts = append(opts, withOpenAICompatibleExplicitPromptCache())
	}
	if apiMode == modelAPIModeResponses {
		return newResponsesModel(baseURL, cfg.Model, token, ctx.HTTPClient, responsesModelOptions{
			rawLogger:                ctx.RawHTTPLogger,
			sessionIDProvider:        ctx.SessionIDProvider,
			reasoningEffort:          cfg.ReasoningEffort,
			temperature:              cfg.Temperature,
			routerMetadata:           true,
			providerManagedContext:   false,
			contextManagement:        cfg.ResponsesContextManagement,
			compactThreshold:         cfg.ResponsesCompactThreshold,
			contextEditTrigger:       cfg.ResponsesContextEditTrigger,
			contextEditKeep:          cfg.ResponsesContextEditKeep,
			contextEditClearThinking: cfg.ResponsesContextEditClearThinking,
			truncation:               cfg.ResponsesTruncation,
			include:                  cfg.ResponsesInclude,
			dialect:                  responsesDialectOpenRouter,
		}), nil
	}
	return newOpenAICompatibleModel(baseURL, cfg.Model, token, ctx.HTTPClient, opts...), nil
}

func buildAnthropicModel(ctx ModelBuildContext, cfg ModelConfig) (llms.Model, error) {
	if apiMode := normalizeModelAPIMode(cfg.APIMode); apiMode == modelAPIModeResponses || apiMode == modelAPIModeResponsesStateful {
		return nil, fmt.Errorf("model.api_mode=%s requires an OpenAI-compatible /responses endpoint; configure that endpoint with provider type openai", apiMode)
	}
	token, useBearerAuth := resolveAnthropicToken(cfg.APIKey)
	if token == "" {
		if env, ok := providerAPIKeyEnv(cfg.APIKey); ok && env != "" {
			return nil, fmt.Errorf("missing the Anthropic API key, set it in the %s environment variable", env)
		}
		return nil, fmt.Errorf("missing the Anthropic API key, set api_key on the provider record, ANTHROPIC_AUTH_TOKEN, or ANTHROPIC_API_KEY")
	}
	options := buildAnthropicModelOptions(ctx, cfg)
	if ctx.ModelSpec != nil {
		options = append(options, withAnthropicModelSpecFn(ctx.ModelSpec))
	}
	if useBearerAuth {
		options = append(options, withAnthropicBearerAuth())
	}
	return newAnthropicModel(resolveAnthropicBaseURL(cfg.BaseURL), cfg.Model, token, ctx.HTTPClient, options...), nil
}

func buildOllamaModel(ctx ModelBuildContext, cfg ModelConfig) (llms.Model, error) {
	if apiMode := normalizeModelAPIMode(cfg.APIMode); apiMode == modelAPIModeResponses || apiMode == modelAPIModeResponsesStateful {
		return nil, fmt.Errorf("model.api_mode=%s requires an OpenAI-compatible /responses endpoint; configure that endpoint with provider type openai", apiMode)
	}
	options := []ollama.Option{ollama.WithModel(cfg.Model), ollama.WithHTTPClient(ctx.OllamaHTTPClient)}
	if cfg.BaseURL != "" {
		options = append(options, ollama.WithServerURL(cfg.BaseURL))
	}
	return ollama.New(options...)
}

// buildGeminiModel always uses Gemini's native Interactions API. The
// OpenAI-compatible /chat/completions endpoint cannot round-trip the
// thought_signature that Gemini 3 models require on replayed function calls, so
// multi-turn tool use fails there with HTTP 400. Google also documents
// generateContent as legacy and recommends calling the native API directly.
func buildGeminiModel(ctx ModelBuildContext, cfg ModelConfig) (llms.Model, error) {
	apiMode := resolveGeminiAPIMode(cfg.APIMode)
	if apiMode == "" {
		return nil, fmt.Errorf("model.api_mode=%s is not supported by Google Gemini; use interactions or interactions_stateful", strings.TrimSpace(cfg.APIMode))
	}
	baseURL := cfg.BaseURL
	if baseURL == "" {
		baseURL = geminiInteractionsBaseURL
	}
	return newInteractionsModel(baseURL, cfg.Model, resolveToken(cfg), ctx.HTTPClient, interactionsModelOptions{
		rawLogger:              ctx.RawHTTPLogger,
		reasoningEffort:        cfg.ReasoningEffort,
		temperature:            cfg.Temperature,
		providerManagedContext: apiMode == modelAPIModeInteractionsStateful,
	}), nil
}

// resolveGeminiAPIMode maps configuration onto the two supported native modes.
// An unset api_mode selects stateless interactions so a minimal Gemini provider
// record works without extra configuration. Any other mode returns "" because
// Gemini has no compatible transport behind this provider type.
func resolveGeminiAPIMode(configured string) string {
	if strings.TrimSpace(configured) == "" {
		return modelAPIModeInteractions
	}
	switch normalizeModelAPIMode(configured) {
	case modelAPIModeInteractions:
		return modelAPIModeInteractions
	case modelAPIModeInteractionsStateful:
		return modelAPIModeInteractionsStateful
	default:
		return ""
	}
}

func lookupModelProviderDefinition(providerType string) (modelProviderDefinition, bool) {
	providerType = strings.ToLower(strings.TrimSpace(providerType))
	for _, definition := range modelProviderDefinitions {
		if definition.providerType == providerType {
			return definition, true
		}
	}
	return modelProviderDefinition{}, false
}

func modelProviderTypes() []string {
	types := make([]string, 0, len(modelProviderDefinitions))
	for _, definition := range modelProviderDefinitions {
		types = append(types, definition.providerType)
	}
	return types
}

func modelProviderUsesProviderTemperatureDefault(providerType string) bool {
	definition, ok := lookupModelProviderDefinition(providerType)
	return ok && definition.usesProviderTemperatureDefault
}

func modelProviderTypesUsingProviderTemperatureDefault() []string {
	types := make([]string, 0, len(modelProviderDefinitions))
	for _, definition := range modelProviderDefinitions {
		if definition.usesProviderTemperatureDefault {
			types = append(types, definition.providerType)
		}
	}
	return types
}

func modelProviderTypesForConfigUI() []string {
	types := make([]string, 0, len(modelProviderDefinitions))
	for _, definition := range modelProviderDefinitions {
		if !definition.hiddenFromConfigUI {
			types = append(types, definition.providerType)
		}
	}
	return types
}

func modelProviderTypesAllowingCustomBaseURLForConfigUI() []string {
	types := make([]string, 0, len(modelProviderDefinitions))
	for _, definition := range modelProviderDefinitions {
		if !definition.hiddenFromConfigUI && definition.allowsCustomBaseURL {
			types = append(types, definition.providerType)
		}
	}
	return types
}
