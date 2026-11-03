package agent

import (
	"strings"

	"aico-agent/internal/agent/model"
)

// ModelDisplayInfo represents UI display information for a model.
// The ID serves as both the model identifier and the display name to reduce
// cognitive overhead. Only descriptions are localized.
type ModelDisplayInfo struct {
	ID           string            `json:"id"`           // Model ID, also used as display name
	Descriptions map[string]string `json:"descriptions"` // locale -> description (localeEnglishUS, localeSimplifiedChinese)
	Recommended  bool              `json:"recommended"`  // Whether this model is recommended for the provider
}

// GetDescription returns the localized description for the given locale.
// Falls back to English if the requested locale is not available.
func (m ModelDisplayInfo) GetDescription(locale string) string {
	// Direct match
	if desc, ok := m.Descriptions[locale]; ok && desc != "" {
		return desc
	}
	// Fallback to English
	if desc, ok := m.Descriptions[localeEnglishUS]; ok {
		return desc
	}
	return ""
}

// LocalizedModelInfo represents a model's display information after localization.
type LocalizedModelInfo struct {
	ID          string           `json:"id"`
	Description string           `json:"description"`
	Recommended bool             `json:"recommended"`
	Spec        *model.ModelSpec `json:"spec,omitempty"`
}

// Localized returns the model information localized for the given locale.
func (m ModelDisplayInfo) Localized(locale string) LocalizedModelInfo {
	var spec *model.ModelSpec
	if resolved, ok := LookupModelSpec("", m.ID); ok {
		resolved.Provider = ""
		resolved.Name = m.ID
		spec = &resolved
	}
	return LocalizedModelInfo{
		ID:          m.ID,
		Description: m.GetDescription(locale),
		Recommended: m.Recommended,
		Spec:        spec,
	}
}

// displayModelsByProvider defines the models available for each provider,
// displayed in config_web UI for user selection.
var displayModelsByProvider = map[string][]ModelDisplayInfo{
	"openai": {
		{
			ID: "gpt-5.5",
			Descriptions: map[string]string{
				localeEnglishUS:         "Latest flagship model with 1M+ context",
				localeSimplifiedChinese: "最新旗舰模型，100万+ 上下文",
			},
			Recommended: true,
		},
		{
			ID: "gpt-5.4",
			Descriptions: map[string]string{
				localeEnglishUS:         "Previous generation flagship model",
				localeSimplifiedChinese: "上一代旗舰模型",
			},
		},
		{
			ID: "gpt-5.4-mini",
			Descriptions: map[string]string{
				localeEnglishUS:         "Fast and economical small model",
				localeSimplifiedChinese: "快速且经济的小模型",
			},
		},
		{
			ID: "gpt-5.4-nano",
			Descriptions: map[string]string{
				localeEnglishUS:         "Ultra-fast nano model",
				localeSimplifiedChinese: "超快速微型模型",
			},
		},
		{
			ID: "gpt-4o",
			Descriptions: map[string]string{
				localeEnglishUS:         "Classic multimodal model",
				localeSimplifiedChinese: "经典多模态模型",
			},
		},
		{
			ID: "gpt-4o-mini",
			Descriptions: map[string]string{
				localeEnglishUS:         "Compact version of GPT-4o",
				localeSimplifiedChinese: "GPT-4o 的紧凑版本",
			},
		},
	},
	"anthropic": {
		{
			ID: "claude-fable-5",
			Descriptions: map[string]string{
				localeEnglishUS:         "Anthropic's most capable model",
				localeSimplifiedChinese: "Anthropic 最强大的模型",
			},
			Recommended: true,
		},
		{
			ID: "claude-opus-5",
			Descriptions: map[string]string{
				localeEnglishUS:         "Newest flagship for agentic coding and enterprise workflows",
				localeSimplifiedChinese: "面向智能体编程和企业工作流的最新旗舰模型",
			},
		},
		{
			ID: "claude-sonnet-4-6",
			Descriptions: map[string]string{
				localeEnglishUS:         "Balanced performance and speed",
				localeSimplifiedChinese: "性能和速度平衡",
			},
		},
		{
			ID: "claude-haiku-4-5",
			Descriptions: map[string]string{
				localeEnglishUS:         "Fast and economical Claude model",
				localeSimplifiedChinese: "快速且经济的 Claude 模型",
			},
		},
	},
	"kimi": {
		{
			ID: "kimi-k3",
			Descriptions: map[string]string{
				localeEnglishUS:         "Deep reasoning model with 1M+ tokens",
				localeSimplifiedChinese: "深度推理模型，100万+ token 上下文",
			},
			Recommended: true,
		},
	},
	"kimi-cn": {
		{
			ID: "kimi-k3",
			Descriptions: map[string]string{
				localeEnglishUS:         "Deep reasoning model with 1M+ tokens",
				localeSimplifiedChinese: "深度推理模型，100万+ token 上下文",
			},
			Recommended: true,
		},
	},
	"volcengine": {
		{
			ID: "doubao-seed-2-1-pro-260628",
			Descriptions: map[string]string{
				localeEnglishUS:         "ByteDance multimodal model",
				localeSimplifiedChinese: "字节跳动多模态模型",
			},
			Recommended: true,
		},
	},
	"deepseek": {
		{
			ID: "deepseek-flash",
			Descriptions: map[string]string{
				localeEnglishUS:         "Fast multimodal model with 1M context",
				localeSimplifiedChinese: "快速多模态模型，100万上下文",
			},
			Recommended: true,
		},
		// Pro does not support vision yet (2026-09-14). Restore this preset and
		// its modelSpecRegistry entry after verifying vision support and limits.
		// {
		// 	ID: "deepseek-v4-pro",
		// 	Descriptions: map[string]string{
		// 		localeEnglishUS:         "Text-only model; does not support screenshots",
		// 		localeSimplifiedChinese: "纯文本模型，不支持截图输入",
		// 	},
		// },
	},
	"gemini": {
		{
			ID: "gemini-3.8-flash",
			Descriptions: map[string]string{
				localeEnglishUS:         "Most intelligent Flash model for complex coding and agentic workflows, 1M context",
				localeSimplifiedChinese: "最智能的 Flash 模型，适用于复杂编程和智能体工作流，100万上下文",
			},
			Recommended: true,
		},
		{
			ID: "gemini-2.5-pro",
			Descriptions: map[string]string{
				localeEnglishUS:         "Advanced 2.5 model for complex reasoning and coding, 1M context",
				localeSimplifiedChinese: "适用于复杂推理和编程的 2.5 高级模型，100万上下文",
			},
		},
		{
			ID: "gemini-2.5-flash",
			Descriptions: map[string]string{
				localeEnglishUS:         "Best price-performance for low-latency, high-volume reasoning, 1M context",
				localeSimplifiedChinese: "最佳性价比，低延迟高吞吐推理，100万上下文",
			},
		},
	},
	"openrouter": {
		{
			ID: "anthropic/claude-fable-5",
			Descriptions: map[string]string{
				localeEnglishUS:         "Anthropic's most capable model",
				localeSimplifiedChinese: "Anthropic 最强大的模型",
			},
			Recommended: true,
		},
		{
			ID: "anthropic/claude-opus-5",
			Descriptions: map[string]string{
				localeEnglishUS:         "Newest flagship for agentic coding and enterprise workflows",
				localeSimplifiedChinese: "面向智能体编程和企业工作流的最新旗舰模型",
			},
		},
		{
			ID: "anthropic/claude-sonnet-4-6",
			Descriptions: map[string]string{
				localeEnglishUS:         "Balanced performance and speed",
				localeSimplifiedChinese: "性能和速度平衡",
			},
		},
		{
			ID: "google/gemini-3.5-pro",
			Descriptions: map[string]string{
				localeEnglishUS:         "Google's flagship model",
				localeSimplifiedChinese: "Google 旗舰模型",
			},
		},
		{
			ID: "google/gemini-3.5-flash",
			Descriptions: map[string]string{
				localeEnglishUS:         "Fast and efficient Gemini model",
				localeSimplifiedChinese: "快速高效的 Gemini 模型",
			},
		},
	},
	"ollama": {
		{
			ID: "qwen2.5:14b",
			Descriptions: map[string]string{
				localeEnglishUS:         "General Chinese model (recommended)",
				localeSimplifiedChinese: "通用中文模型（推荐）",
			},
			Recommended: true,
		},
		{
			ID: "qwen2.5:7b",
			Descriptions: map[string]string{
				localeEnglishUS:         "Lightweight Chinese model",
				localeSimplifiedChinese: "轻量级中文模型",
			},
		},
		{
			ID: "llama3.1:8b",
			Descriptions: map[string]string{
				localeEnglishUS:         "Meta's open source model",
				localeSimplifiedChinese: "Meta 开源模型",
			},
		},
		{
			ID: "llama3.1:70b",
			Descriptions: map[string]string{
				localeEnglishUS:         "Large version of Llama 3.1",
				localeSimplifiedChinese: "Llama 3.1 大型版本",
			},
		},
	},
}

// GetDisplayModelsForProvider returns the list of models available for the given provider.
// Returns an empty slice if the provider is not found.
func GetDisplayModelsForProvider(providerType string) []ModelDisplayInfo {
	normalized := strings.ToLower(strings.TrimSpace(providerType))
	if models, ok := displayModelsByProvider[normalized]; ok {
		return models
	}
	return []ModelDisplayInfo{}
}

// GetLocalizedModelsForProvider returns the localized model list for the given provider and locale.
func GetLocalizedModelsForProvider(providerType, locale string) []LocalizedModelInfo {
	models := GetDisplayModelsForProvider(providerType)
	result := make([]LocalizedModelInfo, len(models))
	for i, m := range models {
		result[i] = m.Localized(locale)
		// Some registry entries only exist under a provider-qualified key (for
		// example openai/gpt-4o). Resolve again with the selected provider so the
		// config UI receives explicit reasoning support, including "unsupported".
		if resolved, ok := LookupModelSpec(providerType, m.ID); ok {
			resolved.Provider = normalizedProviderName(providerType)
			resolved.Name = m.ID
			result[i].Spec = &resolved
		}
		if result[i].Spec != nil {
			result[i].Spec.Provider = normalizedProviderName(providerType)
			result[i].Spec.API = modelAPIEndpoint(providerType, "")
			result[i].Spec.APIShape = modelAPIShape(providerType, "")
		}
	}
	return result
}
