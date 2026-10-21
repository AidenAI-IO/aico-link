package agent

import (
	"errors"
	"fmt"
	"math"
	"net/url"
	"strings"
)

const (
	// DecisionProviderTypeSafe is TypeSafe's System One API (Jev).
	DecisionProviderTypeSafe = "typesafe"
	// DecisionProviderOpenAI is OpenAI's Decisions API (GPT-6 Luna). It has
	// no box questions, so tap-point decisions stay with the main model.
	DecisionProviderOpenAI = "openai"

	defaultTypeSafeBaseURL = "https://api.typesafe.ai/v1"
	// jev-preview tracks Jev 1.14, the first release that answers box questions.
	defaultTypeSafeModel = "jev-preview"
	defaultOpenAIBaseURL = "https://api.openai.com/v1"
	defaultOpenAIModel   = "gpt-6-luna"

	defaultDecisionMinConfidence = 0.8
)

// Decision ops name the workflow decisions in the "[decision]" log lines. Each
// is one whose confident decision-model answers matched the main model in
// on-device runs (2026-10, iOS and Android). App-open confirmation trusts only
// "opened" (see appOpenDecision): Jev was confidently wrong saying "not
// opened" on app-switch transitions. App search trusts only "found" (see
// appSearchDecision). Decisions that did not gain from the decision model
// (IME candidate choice, dialog checks, loop-guard advice, memory retention)
// are not asked at all.
const (
	DecisionOpIMEProbe           = "ime_probe"
	DecisionOpProbeCleanup       = "probe_cleanup"
	DecisionOpScreenAnalysis     = "screen_analysis"
	DecisionOpAppSearch          = "app_search"
	DecisionOpAppOpen            = "app_open_confirmation"
	DecisionOpAndroidSearchEntry = "android_search_entry"
	DecisionOpPasteMenu          = "paste_menu"
)

// DecisionModelConfig configures the optional decision model (TypeSafe Jev or
// OpenAI Luna): a classifier that answers fixed-choice questions (choice,
// yes/no, score) inside deterministic workflows, such as reading the input
// method state or confirming that an app opened. It is off by default and
// needs an API key to be turned on; when off, or when an answer falls below
// MinConfidence, callers keep using the main model.
type DecisionModelConfig struct {
	Enabled       bool    `toml:"enabled"`
	Provider      string  `toml:"provider,omitempty"`
	APIKey        string  `toml:"api_key,omitempty"`
	BaseURL       string  `toml:"base_url,omitempty"`
	Model         string  `toml:"model,omitempty"`
	MinConfidence float64 `toml:"min_confidence,omitempty"`
}

// On reports whether decisions go to the decision model: it is enabled and
// has an API key.
func (c DecisionModelConfig) On() bool {
	return c.Enabled && strings.TrimSpace(c.APIKey) != ""
}

func (c DecisionModelConfig) ProviderOrDefault() string {
	provider := strings.ToLower(strings.TrimSpace(c.Provider))
	if provider == "" {
		return DecisionProviderTypeSafe
	}
	return provider
}

func (c DecisionModelConfig) BaseURLOrDefault() string {
	if baseURL := strings.TrimSpace(c.BaseURL); baseURL != "" {
		return baseURL
	}
	if c.ProviderOrDefault() == DecisionProviderOpenAI {
		return defaultOpenAIBaseURL
	}
	return defaultTypeSafeBaseURL
}

func (c DecisionModelConfig) ModelOrDefault() string {
	if model := strings.TrimSpace(c.Model); model != "" {
		return model
	}
	if c.ProviderOrDefault() == DecisionProviderOpenAI {
		return defaultOpenAIModel
	}
	return defaultTypeSafeModel
}

// MinConfidenceOrDefault returns the confidence below which a decision is
// handed back to the main model. Zero selects the default.
func (c DecisionModelConfig) MinConfidenceOrDefault() float64 {
	if c.MinConfidence == 0 {
		return defaultDecisionMinConfidence
	}
	return c.MinConfidence
}

func (c DecisionModelConfig) validate() error {
	if provider := c.ProviderOrDefault(); provider != DecisionProviderTypeSafe && provider != DecisionProviderOpenAI {
		return fmt.Errorf("invalid decision.provider: %s (expected %s or %s)", c.Provider, DecisionProviderTypeSafe, DecisionProviderOpenAI)
	}
	if math.IsNaN(c.MinConfidence) || c.MinConfidence < 0 || c.MinConfidence > 1 {
		return fmt.Errorf("decision.min_confidence must be between 0 and 1, got %g", c.MinConfidence)
	}
	if baseURL := strings.TrimSpace(c.BaseURL); baseURL != "" && !isDecisionBaseURL(baseURL) {
		return fmt.Errorf("decision.base_url must be an http(s) URL, got %q", c.BaseURL)
	}
	if c.Enabled && strings.TrimSpace(c.APIKey) == "" {
		return errors.New("decision.api_key is required when decision.enabled=true")
	}
	return nil
}

func isDecisionBaseURL(value string) bool {
	parsed, err := url.Parse(value)
	return err == nil && (parsed.Scheme == "http" || parsed.Scheme == "https") && parsed.Host != ""
}
