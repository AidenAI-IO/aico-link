package agent

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"aiden-agent/internal/decision"
	"aiden-agent/internal/logging"
)

// decisionAdvisor routes fixed-choice workflow decisions to the optional
// decision model ([model_settings.decision]). It reads the config through a
// closure on every decision, so enabling it or switching mode applies to the
// next decision without rebuilding the tools. A nil advisor, or a disabled
// config, leaves every decision to the main model exactly as before.
type decisionAdvisor struct {
	mu     sync.Mutex
	cfg    func() DecisionModelConfig
	key    string
	client *decision.Client
}

// setConfig installs the live config source. The runtime sets it once it
// exists, after the tools that hold this advisor were built.
func (a *decisionAdvisor) setConfig(cfg func() DecisionModelConfig) {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.cfg = cfg
	a.mu.Unlock()
}

// settings returns the live config and a client for it, or ok=false when the
// decision model is off.
func (a *decisionAdvisor) settings() (DecisionModelConfig, *decision.Client, bool) {
	if a == nil {
		return DecisionModelConfig{}, nil, false
	}
	// Read the config outside a.mu: the source takes the runtime config lock,
	// which config apply may hold while it calls setConfig.
	a.mu.Lock()
	source := a.cfg
	a.mu.Unlock()
	if source == nil {
		return DecisionModelConfig{}, nil, false
	}
	cfg := source()
	if !cfg.On() {
		return cfg, nil, false
	}
	key := strings.Join([]string{cfg.ProviderOrDefault(), cfg.APIKey, cfg.BaseURLOrDefault(), cfg.ModelOrDefault()}, "\x00")
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client == nil || a.key != key {
		a.client = newDecisionClient(cfg)
		a.key = key
	}
	return cfg, a.client, true
}

// newDecisionClient builds a client for cfg. The board reaches the API through
// its Wi-Fi proxy, which the agent receives as proxy environment variables.
func newDecisionClient(cfg DecisionModelConfig) *decision.Client {
	httpClient := newProxyHTTPClient(ProxyConfig{})
	httpClient.Timeout = decision.DefaultTimeout
	return &decision.Client{
		Provider: cfg.ProviderOrDefault(),
		BaseURL:  cfg.BaseURLOrDefault(),
		APIKey:   cfg.APIKey,
		Model:    cfg.ModelOrDefault(),
		HTTP:     httpClient,
	}
}

// decisionOutcome is the decision model's answer to one workflow decision.
type decisionOutcome[T any] struct {
	Value T
	// Confident is true when the answer clears min_confidence and may replace
	// the main model's decision in active mode.
	Confident  bool
	Confidence float64
}

// decisionPoint describes one workflow decision for decide.
type decisionPoint[T any] struct {
	op string
	// llm is the existing main-model decision.
	llm func(ctx context.Context) (T, error)
	// ask poses the same decision to the decision model.
	ask func(ctx context.Context, client *decision.Client, minConfidence float64) (decisionOutcome[T], error)
	// summary renders a decision compactly for the comparison log.
	summary func(T) string
	// agree compares the two decisions; nil compares summaries.
	agree func(jev, llm T) bool
	// merge, when set, lets an active decision model correct only part of the
	// main model's answer: both are awaited and a confident decision-model
	// answer is merged into the main model's. Used where the main model's
	// answer also carries text the decision model cannot produce.
	merge func(jev, llm T) T
}

// decide runs one decision. Without a decision model it is the main model's
// decision. Otherwise both models are asked at once (see decideRace, or
// decideMerge for a point that only corrects part of the main model's
// answer). usedLLM reports whether the main model's answer was used.
func decide[T any](ctx context.Context, a *decisionAdvisor, p decisionPoint[T]) (value T, usedLLM bool, err error) {
	cfg, client, ok := a.settings()
	if !ok {
		value, err = p.llm(ctx)
		return value, true, err
	}
	if p.merge != nil {
		return decideMerge(ctx, p, cfg, client)
	}
	return decideRace(ctx, p, cfg, client)
}

func llmSummary[T any](p decisionPoint[T], value T, err error) string {
	if err != nil {
		return "error"
	}
	return p.summary(value)
}

func agreement[T any](p decisionPoint[T], jev decisionOutcome[T], jevErr error, llm T, llmErr error) string {
	if jevErr != nil || llmErr != nil {
		return "n/a"
	}
	if p.agree != nil {
		return fmt.Sprint(p.agree(jev.Value, llm))
	}
	return fmt.Sprint(p.summary(jev.Value) == p.summary(llm))
}

// logDecision writes one comparable line per decision: the decision model's
// answer (dm) next to the main model's (llm). Grep the Agent log for
// "[decision]" to read agreement, confidence and latency by op.
func logDecision(op, provider, dm string, confidence float64, llm, agree string, dmMS, llmMS int64, fallback string) {
	logging.Infof("agent", "decision", "op=%s provider=%s dm={%s} conf=%.2f llm={%s} agree=%s dm_ms=%d llm_ms=%d fallback=%q",
		op, provider, dm, confidence, llm, agree, dmMS, llmMS, fallback)
}

// screenshotAsset prepares a screenshot for the decision model.
func screenshotAsset(shot screenshotResult) (decision.Image, error) {
	if shot.Data == "" {
		return decision.Image{}, fmt.Errorf("screenshot has no image data")
	}
	return decision.PrepareJPEG(shot.Data)
}

// askOne sends questions about one screenshot. The image is the state under
// the key "screen", so instructions refer to it as `screen`.
func askOne(ctx context.Context, client *decision.Client, shot screenshotResult, extra map[string]any, questions map[string]decision.Question) (decision.Response, error) {
	img, err := screenshotAsset(shot)
	if err != nil {
		return decision.Response{}, err
	}
	state := map[string]any{"screen": img.Ref}
	for key, value := range extra {
		state[key] = value
	}
	return client.Ask(ctx, decision.Request{
		State:     state,
		Assets:    map[string]decision.Asset{img.Ref: img.Asset},
		Questions: questions,
	})
}

// pointsAgree reports whether two normalized 0-1000 tap points hit the same
// target. Icons and menu actions are rarely closer than this. A decision model
// without box answers (OpenAI) gives no point; then only presence is compared.
func pointsAgree(jev, llm *focusPointArgs) bool {
	if jev == nil {
		return true
	}
	if llm == nil {
		return false
	}
	a, b := jev, llm
	dx, dy := a.X-b.X, a.Y-b.Y
	return dx*dx+dy*dy <= 60*60
}

func decisionPointSummary(p *focusPointArgs) string {
	if p == nil {
		return "-"
	}
	return fmt.Sprintf("%.0f,%.0f", p.X, p.Y)
}

// boxPoint converts a box answer to a tap point when it is usable.
func boxPoint(answer decision.Answer) *focusPointArgs {
	if answer.Box == nil || !answer.Box.Valid() {
		return nil
	}
	x, y := answer.Box.Center1000()
	return &focusPointArgs{X: x, Y: y}
}

// decideRace asks the decision model and the main model at the same time. A
// confident decision-model answer is used as soon as it arrives and the main
// model call is canceled; otherwise the main model's answer is used. An
// unconfident answer therefore never adds latency over the main model alone.
func decideRace[T any](ctx context.Context, p decisionPoint[T], cfg DecisionModelConfig, client *decision.Client) (T, bool, error) {
	minConfidence := cfg.MinConfidenceOrDefault()
	type jevResult struct {
		outcome decisionOutcome[T]
		err     error
		ms      int64
	}
	type llmResult struct {
		value T
		err   error
		ms    int64
	}
	provider := cfg.ProviderOrDefault()
	llmCtx, cancelLLM := context.WithCancel(ctx)
	started := time.Now()
	jevDone := make(chan jevResult, 1)
	llmDone := make(chan llmResult, 1)
	go func() {
		outcome, err := p.ask(ctx, client, minConfidence)
		jevDone <- jevResult{outcome: outcome, err: err, ms: time.Since(started).Milliseconds()}
	}()
	go func() {
		value, err := p.llm(llmCtx)
		llmDone <- llmResult{value: value, err: err, ms: time.Since(started).Milliseconds()}
	}()

	var jev *jevResult
	select {
	case got := <-jevDone:
		jev = &got
		if got.err == nil && got.outcome.Confident {
			cancelLLM()
			logDecision(p.op, provider, p.summary(got.outcome.Value), got.outcome.Confidence, "", "", got.ms, 0, "")
			return got.outcome.Value, false, nil
		}
	case <-ctx.Done():
		cancelLLM()
		var zero T
		return zero, false, ctx.Err()
	case llm := <-llmDone:
		// The main model answered first; its answer stands. The decision
		// model's answer is still logged for comparison once it arrives.
		cancelLLM()
		go func() {
			got := <-jevDone
			logComparison(p, provider, got.outcome, got.err, got.ms, llm.value, llm.err, llm.ms, "llm_first")
		}()
		return llm.value, true, llm.err
	}
	llm := <-llmDone
	cancelLLM()
	fallback := "low_confidence"
	if jev.err != nil {
		fallback = "error: " + jev.err.Error()
	}
	logComparison(p, provider, jev.outcome, jev.err, jev.ms, llm.value, llm.err, llm.ms, fallback)
	return llm.value, true, llm.err
}

// logComparison logs a decision answered by both models.
func logComparison[T any](p decisionPoint[T], provider string, outcome decisionOutcome[T], jevErr error, jevMS int64, value T, llmErr error, llmMS int64, fallback string) {
	jevSummary := ""
	if jevErr == nil {
		jevSummary = p.summary(outcome.Value)
	}
	agree := agreement(p, outcome, jevErr, value, llmErr)
	logDecision(p.op, provider, jevSummary, outcome.Confidence, llmSummary(p, value, llmErr), agree, jevMS, llmMS, fallback)
}

// decisionMergeGrace is how long decideMerge waits for the decision model
// after the main model has answered. Jev usually answers first; this only
// bounds a stalled endpoint.
const decisionMergeGrace = 2 * time.Second

// decideMerge asks both models at once and waits for both. The main model's
// answer is used, with a confident decision-model answer merged into it.
func decideMerge[T any](ctx context.Context, p decisionPoint[T], cfg DecisionModelConfig, client *decision.Client) (T, bool, error) {
	minConfidence := cfg.MinConfidenceOrDefault()
	type jevResult struct {
		outcome decisionOutcome[T]
		err     error
		ms      int64
	}
	started := time.Now()
	askCtx, cancelAsk := context.WithCancel(ctx)
	defer cancelAsk()
	jevDone := make(chan jevResult, 1)
	go func() {
		outcome, err := p.ask(askCtx, client, minConfidence)
		jevDone <- jevResult{outcome: outcome, err: err, ms: time.Since(started).Milliseconds()}
	}()
	value, err := p.llm(ctx)
	llmMS := time.Since(started).Milliseconds()
	if err != nil {
		return value, true, err
	}
	// The main model has answered; a slow decision model may refine that
	// answer but not hold it up for long.
	grace := time.NewTimer(decisionMergeGrace)
	defer grace.Stop()
	var jev jevResult
	select {
	case jev = <-jevDone:
	case <-ctx.Done():
		return value, true, ctx.Err()
	case <-grace.C:
		logDecision(p.op, cfg.ProviderOrDefault(), "", 0, llmSummary(p, value, nil), "n/a", time.Since(started).Milliseconds(), llmMS, "timeout")
		return value, true, nil
	}
	fallback := ""
	switch {
	case jev.err != nil:
		fallback = "error: " + jev.err.Error()
	case !jev.outcome.Confident:
		fallback = "low_confidence"
	}
	logComparison(p, cfg.ProviderOrDefault(), jev.outcome, jev.err, jev.ms, value, nil, llmMS, fallback)
	if fallback != "" {
		return value, true, nil
	}
	return p.merge(jev.outcome.Value, value), true, nil
}
