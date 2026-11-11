package agent

import (
	"context"
	"fmt"
	"time"

	"aico-agent/internal/decision"
)

// stepGateResult is the decision model's check of a batch step's expected
// screen state.
type stepGateResult struct {
	// Checked is false when no check ran: the decision model is off, or the
	// screenshot or the request failed. The batch then runs as it would
	// without the decision model.
	Checked bool
	// Pass is true only for a confident "yes".
	Pass       bool
	Confidence float64
}

// gate asks whether shot shows the state a batch step expects. Unlike decide,
// no main-model answer exists to fall back on mid-batch, so only a confident
// "yes" lets the batch continue; "no" and an unsure answer both stop it and
// hand control back to the main model, which costs at most one extra turn.
// A failed request carries no information about the screen, so it is treated
// like a disabled decision model.
func (a *decisionAdvisor) gate(ctx context.Context, shot screenshotResult, expect string) stepGateResult {
	cfg, client, ok := a.settings()
	if !ok {
		return stepGateResult{}
	}
	provider := cfg.ProviderOrDefault()
	started := time.Now()
	resp, err := askOne(ctx, client, shot, map[string]any{"expected": expect}, map[string]decision.Question{
		"met": {Type: decision.TypeNoul, Instructions: "`screen` was captured right after an automation step that should leave the device showing the state described by `expected`. " +
			"Does `screen` show that state? A transition that has not finished, a different screen, or a dialog covering it means no."},
	})
	ms := time.Since(started).Milliseconds()
	if err != nil {
		logDecision(DecisionOpStepGate, provider, "", 0, "", "", ms, 0, "error: "+err.Error())
		return stepGateResult{}
	}
	met, sure, p, err := noulAnswer(resp, "met", cfg.MinConfidenceOrDefault())
	if err != nil {
		logDecision(DecisionOpStepGate, provider, "", 0, "", "", ms, 0, "error: "+err.Error())
		return stepGateResult{}
	}
	result := stepGateResult{Checked: true, Pass: met && sure, Confidence: p}
	fallback := ""
	if !result.Pass {
		fallback = "stop"
	}
	logDecision(DecisionOpStepGate, provider, fmt.Sprintf("met=%v sure=%v expected=%q", met, sure, expect), p, "", "", ms, 0, fallback)
	return result
}
