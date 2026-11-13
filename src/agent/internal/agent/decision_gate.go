package agent

import (
	"context"
	"encoding/base64"
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

// stepGateTimeout bounds one step-gate request. The batch waits on it between
// two steps, and a request that fails or times out leaves the batch unchecked,
// so it is kept well below decision.DefaultTimeout.
var stepGateTimeout = 4 * time.Second

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
	ctx, cancel := context.WithTimeout(ctx, stepGateTimeout)
	defer cancel()
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

// compareGate asks whether the controls described by expect are the same in
// after as in before: still there, uncovered and in place. It answers like
// gate: only a confident "yes" passes, and a failed request is unchecked.
func (a *decisionAdvisor) compareGate(ctx context.Context, before, after []byte, expect string) stepGateResult {
	cfg, client, ok := a.settings()
	if !ok {
		return stepGateResult{}
	}
	provider := cfg.ProviderOrDefault()
	started := time.Now()
	ctx, cancel := context.WithTimeout(ctx, stepGateTimeout)
	defer cancel()
	beforeImage, err := decision.PrepareJPEG(base64.StdEncoding.EncodeToString(before))
	if err != nil {
		return stepGateResult{}
	}
	afterImage, err := decision.PrepareJPEG(base64.StdEncoding.EncodeToString(after))
	if err != nil {
		return stepGateResult{}
	}
	resp, err := client.Ask(ctx, decision.Request{
		State: map[string]any{"before": beforeImage.Ref, "screen": afterImage.Ref, "controls": expect},
		Assets: map[string]decision.Asset{
			beforeImage.Ref: beforeImage.Asset,
			afterImage.Ref:  afterImage.Asset,
		},
		Questions: map[string]decision.Question{
			"met": {Type: decision.TypeNoul, Instructions: "`before` is the screen when a series of taps was planned and `screen` is the screen now, partway through. " +
				"Are the controls named in `controls` the same controls in `screen` as in `before`: same label or icon, same function, still visible, uncovered and in place? " +
				"Dimming, a highlight or a nudge of a pixel or two keeps them the same. A different label or icon at a point, a new page, or a dialog, menu or sheet over a point means no."},
		},
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
	logDecision(DecisionOpStepGate, provider, fmt.Sprintf("same_controls=%v sure=%v controls=%q", met, sure, expect), p, "", "", ms, 0, fallback)
	return result
}
