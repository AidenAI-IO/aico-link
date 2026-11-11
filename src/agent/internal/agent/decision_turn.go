package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/decision"
)

// Decision ops asked once per user turn from the utterance alone; see
// decideTurn.
const (
	DecisionOpTurnScreenshot = "turn_screenshot"
	DecisionOpMemoryRecall   = "memory_recall"
)

// turnRecallCandidates bounds how many saved memories one turn asks about, so
// the decision request stays small. The store returns them by priority.
const turnRecallCandidates = 20

// turnRecallMax bounds how many memories one turn attaches.
const turnRecallMax = 5

// turnDecisionTimeout bounds the decision request made for a user turn. The
// screenshot is captured meanwhile, so this only delays the turn when the
// decision model is slower than the capture.
const turnDecisionTimeout = 4 * time.Second

// turnDecision is the decision model's reading of one user utterance.
type turnDecision struct {
	// SkipScreenshot is true only for a confident "the screen is not needed".
	SkipScreenshot bool
	// Recalled are the saved memories the decision model confidently judged
	// relevant, in the store's priority order.
	Recalled []MemoryResult
}

// decideTurn asks the decision model, in one request, whether answering
// utterance needs the phone screen and which saved memories bear on it. It
// returns the zero value, which keeps the screenshot and recalls nothing, when
// the decision model is off, the request fails, or an answer is unsure: the
// main model can still take a screenshot or call recall_memory itself.
func decideTurn(ctx context.Context, decider *decisionAdvisor, utterance string, candidates []MemoryResult) turnDecision {
	utterance = strings.TrimSpace(utterance)
	if utterance == "" {
		return turnDecision{}
	}
	cfg, client, ok := decider.settings()
	if !ok {
		return turnDecision{}
	}
	if len(candidates) > turnRecallCandidates {
		candidates = candidates[:turnRecallCandidates]
	}
	state := map[string]any{"utterance": utterance}
	questions := map[string]decision.Question{
		// Telling Jev the assistant can see the screen made it unsure about
		// questions on remembered facts; describing both sides of the line
		// lets it answer those confidently (board run, 2026-11-11).
		"screen": {Type: decision.TypeNoul, Instructions: "`utterance` is what the user just said to a voice assistant. Would the assistant need to look at the phone's current screen to respond? " +
			"Yes when the request operates the phone or an app (opening, tapping, typing, sending, reading notifications on screen) or refers to something shown on it (this, that button, what is on screen). " +
			"No when it can be answered by talking alone: general knowledge, weather, time, arithmetic, translation of text the user spoke, chat, jokes, or facts about the user and their family that the assistant may have remembered."},
	}
	for i, memory := range candidates {
		key := fmt.Sprintf("memory_%d", i)
		state[key] = memoryDecisionText(memory)
		questions["recall_"+key] = decision.Question{Type: decision.TypeNoul, Instructions: fmt.Sprintf(
			"`%s` is something the user told the assistant earlier. Does it bear on how to respond to `utterance`, for example a preference, a fact about the user or the people they mention, or the usual way they want this done?", key)}
	}

	started := time.Now()
	resp, err := client.Ask(ctx, decision.Request{State: state, Questions: questions})
	ms := time.Since(started).Milliseconds()
	provider := cfg.ProviderOrDefault()
	minConfidence := cfg.MinConfidenceOrDefault()
	if err != nil {
		logDecision(DecisionOpTurnScreenshot, provider, "", 0, "", "", ms, 0, "error: "+err.Error())
		return turnDecision{}
	}

	var result turnDecision
	if need, sure, p, err := noulAnswer(resp, "screen", minConfidence); err == nil {
		result.SkipScreenshot = sure && !need
		fallback := ""
		if !sure {
			fallback = "low_confidence"
		}
		logDecision(DecisionOpTurnScreenshot, provider, fmt.Sprintf("needs_screen=%v sure=%v", need, sure), p, "", "", ms, 0, fallback)
	}
	ids := make([]string, 0, turnRecallMax)
	for i, memory := range candidates {
		if len(result.Recalled) >= turnRecallMax {
			break
		}
		relevant, sure, _, err := noulAnswer(resp, fmt.Sprintf("recall_memory_%d", i), minConfidence)
		if err == nil && sure && relevant {
			result.Recalled = append(result.Recalled, memory)
			ids = append(ids, memory.ID)
		}
	}
	if len(candidates) > 0 {
		logDecision(DecisionOpMemoryRecall, provider, fmt.Sprintf("recalled=%d/%d ids=%s", len(ids), len(candidates), strings.Join(ids, ",")), 0, "", "", ms, 0, "")
	}
	return result
}

// memoryDecisionText is the text of a memory as the decision model sees it.
func memoryDecisionText(memory MemoryResult) string {
	parts := make([]string, 0, 3)
	for _, part := range []string{memory.Title, memory.Summary, memory.Content} {
		part = strings.TrimSpace(part)
		if part != "" && !slices.Contains(parts, part) {
			parts = append(parts, part)
		}
	}
	return truncateToolResultRunes(strings.Join(parts, " — "), 600)
}

// recalledMemoriesText formats automatically recalled memories for the state
// message that precedes the user turn.
func recalledMemoriesText(memories []MemoryResult) string {
	if len(memories) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Saved memories that may bear on this request (recalled automatically; call recall_memory for others):\n")
	for _, memory := range memories {
		fmt.Fprintf(&b, "- [%s] %s\n", memory.Type, memoryDecisionText(memory))
	}
	return b.String()
}

// startTurnDecision starts decideTurn for a user utterance and returns a
// function that waits for its result; calling it again returns the same
// result. Without a decision model the result is immediate and empty.
func (r *Runtime) startTurnDecision(utterance string) func() turnDecision {
	tools := r.toolSnapshot()
	var decider *decisionAdvisor
	if tools != nil {
		decider = tools.decider
	}
	if _, _, ok := decider.settings(); !ok || strings.TrimSpace(utterance) == "" {
		return func() turnDecision { return turnDecision{} }
	}
	store := r.longTermMemoryStore()
	done := make(chan turnDecision, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), turnDecisionTimeout)
		defer cancel()
		var candidates []MemoryResult
		if store != nil {
			// A browse returns the highest-priority memories and, by design,
			// never a sensitive one; recall_memory still finds those by topic.
			candidates, _ = store.Search(ctx, MemoryQuery{Limit: turnRecallCandidates})
		}
		done <- decideTurn(ctx, decider, utterance, candidates)
	}()
	var once sync.Once
	var result turnDecision
	return func() turnDecision {
		once.Do(func() { result = <-done })
		return result
	}
}

// longTermMemoryStore returns the runtime's long-term memory store, if any.
func (r *Runtime) longTermMemoryStore() *LongTermMemoryStore {
	if r == nil || r.memories == nil {
		return nil
	}
	r.memories.mu.Lock()
	defer r.memories.mu.Unlock()
	return r.memories.longTerm
}
