package agent

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/decision"
)

// DecisionOpTurnScreenshot is asked once per user turn from the utterance
// alone; see decideTurn.
const DecisionOpTurnScreenshot = "turn_screenshot"

// turnDecisionTimeout bounds the decision request made for a user turn. The
// screenshot is captured meanwhile, so this only delays the turn when the
// decision model is slower than the capture.
// A variable so tests can shorten it.
var turnDecisionTimeout = 4 * time.Second

// turnDecision is the decision model's reading of one user utterance.
type turnDecision struct {
	// SkipScreenshot is true only for a confident "the screen is not needed".
	SkipScreenshot bool
}

// decideTurn asks the decision model whether answering utterance needs the
// phone screen. It returns the zero value, which keeps the screenshot, when
// the decision model is off, the request fails, or the answer is unsure: the
// main model can still take a screenshot itself.
//
// previous is the exchange before utterance (see previousTurnText); a
// follow-up such as "now press equals" only reads as needing the screen next
// to the turn that opened the app.
func decideTurn(ctx context.Context, decider *decisionAdvisor, utterance, previous string) turnDecision {
	utterance = strings.TrimSpace(utterance)
	if utterance == "" {
		return turnDecision{}
	}
	cfg, client, ok := decider.settings()
	if !ok {
		return turnDecision{}
	}
	state := map[string]any{"utterance": utterance}
	screenContext := ""
	if previous != "" {
		state["previous_turn"] = previous
		screenContext = "`previous_turn` is the exchange just before it; read `utterance` as its follow-up. "
	}
	questions := map[string]decision.Question{
		// Telling Jev the assistant can see the screen made it unsure about
		// questions on remembered facts; describing both sides of the line
		// lets it answer those confidently (board run, 2026-11-11).
		"screen": {Type: decision.TypeNoul, Instructions: "`utterance` is what the user just said to a voice assistant. " + screenContext + "Would the assistant need to look at the phone's current screen to respond? " +
			"Yes when the request operates the phone or an app (opening, tapping, typing, sending, reading notifications on screen), continues an operation the previous turn started, or refers to something shown on it (this, that button, what is on screen, the result). " +
			"No when it can be answered by talking alone: general knowledge, weather, time, arithmetic, translation of text the user spoke, chat, jokes, or facts about the user and their family that the assistant may have remembered."},
	}

	started := time.Now()
	resp, err := client.Ask(ctx, decision.Request{State: state, Questions: questions})
	ms := time.Since(started).Milliseconds()
	provider := cfg.ProviderOrDefault()
	if err != nil {
		logDecision(DecisionOpTurnScreenshot, provider, "", 0, "", "", ms, 0, "error: "+err.Error())
		return turnDecision{}
	}
	var result turnDecision
	if need, sure, p, err := noulAnswer(resp, "screen", cfg.MinConfidenceOrDefault()); err == nil {
		result.SkipScreenshot = sure && !need
		fallback := ""
		if !sure {
			fallback = "low_confidence"
		}
		logDecision(DecisionOpTurnScreenshot, provider, fmt.Sprintf("needs_screen=%v sure=%v", need, sure), p, "", "", ms, 0, fallback)
	}
	return result
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
	previous := previousTurnText(r.contextManager)
	done := make(chan turnDecision, 1)
	ctx, cancel := context.WithTimeout(context.Background(), turnDecisionTimeout)
	go func() {
		defer cancel()
		done <- decideTurn(ctx, decider, utterance, previous)
	}()
	var once sync.Once
	var result turnDecision
	return func() turnDecision {
		once.Do(func() {
			select {
			case result = <-done:
			case <-ctx.Done():
			}
		})
		return result
	}
}

// previousTurnMessages bounds how far back previousTurnText looks; a turn that
// operated the phone is a few dozen messages at most.
const previousTurnMessages = 60

// previousTurnRunes bounds each text part of previousTurnText.
const previousTurnRunes = 300

// previousTurnText describes the last exchange in the conversation for the
// turn decision: the user's request, the tools the assistant called for it,
// and its final reply. It is empty for a new conversation.
func previousTurnText(manager *contextmanager.ContextManager) string {
	window := manager.MessageListWindow(func(_ string, view []messages.Message) (int, int) {
		return len(view) - previousTurnMessages, len(view)
	})
	var request, reply string
	var tools []string
	for i := len(window.Messages) - 1; i >= 0; i-- {
		message := window.Messages[i]
		switch message.Role {
		case messages.MessageRoleUser:
			request = strings.TrimSpace(message.Content)
		case messages.MessageRoleAssistant, messages.MessageRoleToolCall:
			if reply == "" && message.Role == messages.MessageRoleAssistant {
				reply = strings.TrimSpace(message.Content)
			}
			for j := len(message.ToolCalls) - 1; j >= 0; j-- {
				if name := message.ToolCalls[j].Name; name != "" && !slices.Contains(tools, name) {
					tools = append(tools, name)
				}
			}
		}
		if request != "" {
			break
		}
	}
	if request == "" {
		return ""
	}
	slices.Reverse(tools)
	var b strings.Builder
	fmt.Fprintf(&b, "user: %s\n", truncateToolResultRunes(request, previousTurnRunes))
	if len(tools) > 0 {
		fmt.Fprintf(&b, "assistant used tools: %s\n", strings.Join(tools, ", "))
	}
	if reply != "" {
		fmt.Fprintf(&b, "assistant: %s\n", truncateToolResultRunes(reply, previousTurnRunes))
	}
	return b.String()
}
