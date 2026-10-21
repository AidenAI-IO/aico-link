package agent

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDecisionModelConfigDefaults(t *testing.T) {
	var cfg DecisionModelConfig
	if cfg.Enabled() {
		t.Fatal("decision model must be off without an API key")
	}
	if !(DecisionModelConfig{APIKey: "ts-secret"}).Enabled() {
		t.Fatal("an API key alone must turn the decision model on")
	}
	if got := cfg.ProviderOrDefault(); got != DecisionProviderTypeSafe {
		t.Fatalf("ProviderOrDefault() = %q", got)
	}
	if got := cfg.BaseURLOrDefault(); got != defaultTypeSafeBaseURL {
		t.Fatalf("BaseURLOrDefault() = %q", got)
	}
	if got := cfg.ModelOrDefault(); got != defaultTypeSafeModel {
		t.Fatalf("ModelOrDefault() = %q", got)
	}
	if got := cfg.MinConfidenceOrDefault(); got != defaultDecisionMinConfidence {
		t.Fatalf("MinConfidenceOrDefault() = %v", got)
	}
}

func TestConfigValidateDecisionModel(t *testing.T) {
	tests := []struct {
		name     string
		decision DecisionModelConfig
		field    string
	}{
		{name: "off without key", decision: DecisionModelConfig{}},
		{name: "on with key", decision: DecisionModelConfig{APIKey: "ts-secret"}},
		{name: "unknown provider", decision: DecisionModelConfig{Provider: "other"}, field: "decision.provider"},
		{name: "openai provider", decision: DecisionModelConfig{Provider: "OpenAI", APIKey: "sk"}},
		{name: "confidence above one", decision: DecisionModelConfig{MinConfidence: 1.5}, field: "decision.min_confidence"},
		{name: "negative confidence", decision: DecisionModelConfig{MinConfidence: -0.1}, field: "decision.min_confidence"},
		{name: "NaN confidence", decision: DecisionModelConfig{MinConfidence: math.NaN()}, field: "decision.min_confidence"},
		{name: "relative base URL", decision: DecisionModelConfig{BaseURL: "api.typesafe.ai"}, field: "decision.base_url"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Config{Model: ModelConfig{Provider: "fake"}, Decision: tt.decision}
			err := cfg.Validate()
			if tt.field == "" {
				if err != nil {
					t.Fatalf("Validate() error = %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected %s error", tt.field)
			}
			errs := ParseConfigValidationErrors(err)
			if len(errs) != 1 || errs[0].Field != tt.field {
				t.Fatalf("validation errors = %+v, want field %s", errs, tt.field)
			}
		})
	}
}

func TestLoadConfigReadsDecisionModelFromModelSettings(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	source := strings.Join([]string{
		"[model_settings.model]",
		`provider = "fake"`,
		"",
		"[model_settings.decision]",
		"# enabled is no longer read; a leftover key must still load.",
		"enabled = true",
		`api_key = "ts-secret"`,
		"min_confidence = 0.65",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadResolvedConfig(path)
	if err != nil {
		t.Fatalf("LoadResolvedConfig() error = %v", err)
	}
	if !cfg.Decision.Enabled() || cfg.Decision.APIKey != "ts-secret" || cfg.Decision.MinConfidenceOrDefault() != 0.65 {
		t.Fatalf("decision config = %+v", cfg.Decision)
	}
	if cfg.Decision.ModelOrDefault() != defaultTypeSafeModel {
		t.Fatalf("decision model = %q", cfg.Decision.ModelOrDefault())
	}
}

func TestDecisionModelDefaultsFollowProvider(t *testing.T) {
	cfg := DecisionModelConfig{Provider: DecisionProviderOpenAI}
	if cfg.BaseURLOrDefault() != defaultOpenAIBaseURL || cfg.ModelOrDefault() != defaultOpenAIModel {
		t.Fatalf("openai defaults = %s %s", cfg.BaseURLOrDefault(), cfg.ModelOrDefault())
	}
	cfg.BaseURL, cfg.Model = "https://proxy.example/v1", "gpt-6-luna-2026-10-06"
	if cfg.BaseURLOrDefault() != cfg.BaseURL || cfg.ModelOrDefault() != cfg.Model {
		t.Fatal("explicit base_url/model must win over provider defaults")
	}
}
