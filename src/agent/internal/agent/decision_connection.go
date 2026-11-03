package agent

import (
	"context"
	"errors"
	"strings"
	"time"

	"aico-agent/internal/decision"
)

// DecisionModelTestResult reports one decision-model connection test.
type DecisionModelTestResult struct {
	Provider string
	Model    string
	Latency  time.Duration
}

// RunDecisionModelTest sends one small text question with the candidate
// settings, the way the Agent asks workflow decisions, and reports whether a
// well-formed answer came back. An empty candidate key uses the saved one, as
// the settings page never receives the saved key; it is only sent to the
// saved provider and Base URL, so a request cannot point it elsewhere. It
// works with the switch off, so a key can be checked before turning the model
// on.
func RunDecisionModelTest(ctx context.Context, cfg Config, candidate DecisionModelConfig) (DecisionModelTestResult, error) {
	result := DecisionModelTestResult{Provider: candidate.ProviderOrDefault(), Model: candidate.ModelOrDefault()}
	if strings.TrimSpace(candidate.APIKey) == "" {
		if candidate.ProviderOrDefault() != cfg.Decision.ProviderOrDefault() ||
			candidate.BaseURLOrDefault() != cfg.Decision.BaseURLOrDefault() {
			return result, errors.New("decision.api_key is not set for this provider and base URL")
		}
		candidate.APIKey = cfg.Decision.APIKey
	}
	if strings.TrimSpace(candidate.APIKey) == "" {
		return result, errors.New("decision.api_key is not set")
	}
	if err := candidate.validate(); err != nil {
		return result, err
	}
	started := time.Now()
	resp, err := newDecisionClient(candidate).Ask(ctx, decision.Request{
		State: map[string]any{"statement": "The phone screen shows a calculator."},
		Questions: map[string]decision.Question{
			"check": {Type: decision.TypeNoul, Instructions: "Does `statement` mention a calculator?"},
		},
	})
	result.Latency = time.Since(started)
	if err != nil {
		return result, err
	}
	if _, err := resp.Answer("check", decision.TypeNoul); err != nil {
		return result, err
	}
	return result, nil
}
