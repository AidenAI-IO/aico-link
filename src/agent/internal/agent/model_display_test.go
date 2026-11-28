package agent

import (
	"strings"
	"testing"
)

func TestModelCatalogLoadsAndHasInvariants(t *testing.T) {
	catalog, err := loadModelCatalog()
	if err != nil {
		t.Fatalf("loadModelCatalog: %v", err)
	}
	required := []string{"openai", "anthropic", "openrouter", "kimi", "kimi-cn", "volcengine", "deepseek", "gemini", "ollama"}
	for _, provider := range required {
		entry, ok := catalog[provider]
		if !ok {
			t.Fatalf("missing provider %q", provider)
		}
		if entry.Default == "" {
			t.Fatalf("provider %q missing default", provider)
		}
		ids := make(map[string]bool, len(entry.Models))
		hasRecommended := false
		for _, model := range entry.Models {
			if model.ID == "" {
				t.Fatalf("provider %q has empty model id", provider)
			}
			if ids[model.ID] {
				t.Fatalf("provider %q duplicate model %q", provider, model.ID)
			}
			ids[model.ID] = true
			if model.Recommended {
				hasRecommended = true
			}
			if model.GetDescription(localeEnglishUS) == "" {
				t.Fatalf("provider %q model %q missing English description", provider, model.ID)
			}
		}
		if !hasRecommended {
			t.Fatalf("provider %q has no recommended model", provider)
		}
		if !ids[entry.Default] {
			t.Fatalf("provider %q default %q not in models", provider, entry.Default)
		}
	}
}

func TestGetDisplayModelsOrdersDefaultFirst(t *testing.T) {
	models := GetDisplayModelsForProvider("openrouter")
	if len(models) == 0 {
		t.Fatal("openrouter catalog is empty")
	}
	want := GetDefaultModelForProvider("openrouter")
	if want == "" {
		t.Fatal("openrouter default is empty")
	}
	if models[0].ID != want {
		t.Fatalf("first model = %q, want default %q", models[0].ID, want)
	}
	if !models[0].Recommended {
		t.Fatalf("default model %q should be recommended", models[0].ID)
	}
}

func TestGetDisplayModelsForProviderUnknown(t *testing.T) {
	if models := GetDisplayModelsForProvider("unknown-provider"); len(models) != 0 {
		t.Fatalf("unknown provider models = %#v, want empty", models)
	}
	if models := GetDisplayModelsForProvider(""); len(models) != 0 {
		t.Fatalf("empty provider models = %#v, want empty", models)
	}
}

func TestGetDisplayModelsForProviderCaseInsensitive(t *testing.T) {
	lower := GetDisplayModelsForProvider("openai")
	upper := GetDisplayModelsForProvider("OPENAI")
	if len(lower) == 0 || len(lower) != len(upper) {
		t.Fatalf("case mismatch: lower=%d upper=%d", len(lower), len(upper))
	}
	if lower[0].ID != upper[0].ID {
		t.Fatalf("first id mismatch: %q vs %q", lower[0].ID, upper[0].ID)
	}
}

func TestAnthropicDisplayModelsUseBareIDs(t *testing.T) {
	for _, model := range GetDisplayModelsForProvider("anthropic") {
		if strings.Contains(model.ID, "/") {
			t.Fatalf("anthropic model id %q should be a bare Messages API id", model.ID)
		}
	}
}

func TestOpenRouterDefaultIsFactorySeed(t *testing.T) {
	if got := GetDefaultModelForProvider("openrouter"); got != "bytedance-seed/seed-2.0-lite" {
		t.Fatalf("openrouter default = %q, want bytedance-seed/seed-2.0-lite", got)
	}
}

func TestModelDisplayInfo_GetDescription(t *testing.T) {
	model := ModelDisplayInfo{
		ID: "test-model",
		Descriptions: map[string]string{
			localeEnglishUS:         "English description",
			localeSimplifiedChinese: "中文描述",
		},
	}
	if got := model.GetDescription(localeSimplifiedChinese); got != "中文描述" {
		t.Fatalf("zh description = %q", got)
	}
	if got := model.GetDescription("fr-FR"); got != "English description" {
		t.Fatalf("fallback description = %q", got)
	}
}

func TestModelDisplayInfo_Localized(t *testing.T) {
	model := ModelDisplayInfo{
		ID: "test-model",
		Descriptions: map[string]string{
			localeEnglishUS:         "English description",
			localeSimplifiedChinese: "中文描述",
		},
		Recommended: true,
	}
	localized := model.Localized(localeSimplifiedChinese)
	if localized.ID != "test-model" || localized.Description != "中文描述" || !localized.Recommended {
		t.Fatalf("localized = %#v", localized)
	}
}

func TestGetLocalizedModelsForProvider(t *testing.T) {
	models := GetLocalizedModelsForProvider("openai", localeSimplifiedChinese)
	if len(models) == 0 {
		t.Fatal("expected openai models")
	}
	for _, m := range models {
		if m.Description == "" {
			t.Fatalf("model %q missing localized description", m.ID)
		}
	}
}

func TestLocalizedProviderModelsIncludeProviderScopedReasoningCapability(t *testing.T) {
	models := GetLocalizedModelsForProvider("openai", localeEnglishUS)
	for _, item := range models {
		if item.ID != "gpt-4o" {
			continue
		}
		if item.Spec == nil || item.Spec.Reasoning == nil {
			t.Fatalf("gpt-4o spec = %#v, want explicit unsupported reasoning capability", item.Spec)
		}
		if item.Spec.Reasoning.Supported {
			t.Fatalf("gpt-4o reasoning = %#v, want unsupported", item.Spec.Reasoning)
		}
		return
	}
	t.Fatal("openai display models do not include gpt-4o")
}

func TestParseModelCatalogRejectsBadDefault(t *testing.T) {
	_, err := parseModelCatalogYAML([]byte(`
providers:
  demo:
    default: missing-model
    models:
      - id: real-model
        recommended: true
        descriptions:
          en-US: "x"
`))
	if err == nil {
		t.Fatal("expected error for missing default")
	}
}

func TestEachProviderHasRecommendedModel(t *testing.T) {
	catalog, err := loadModelCatalog()
	if err != nil {
		t.Fatal(err)
	}
	for provider, entry := range catalog {
		hasRecommended := false
		for _, m := range entry.Models {
			if m.Recommended {
				hasRecommended = true
				break
			}
		}
		if !hasRecommended {
			t.Errorf("provider %q has no recommended model", provider)
		}
	}
}
