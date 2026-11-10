package main

import (
	"strings"
	"unicode"
)

// realtimeEmptyTurnGuard suppresses provider output that belongs to one empty
// or locally-handled user audio turn. It is keyed by input-turn sequence so a
// later chat, task-update, or notification CreateResponse is not canceled.
type realtimeEmptyTurnGuard struct {
	armedTurn uint64
}

func (g *realtimeEmptyTurnGuard) suppressTurn(turn uint64) {
	if g == nil || turn == 0 {
		return
	}
	g.armedTurn = turn
}

func (g *realtimeEmptyTurnGuard) clear() {
	if g == nil {
		return
	}
	g.armedTurn = 0
}

func (g *realtimeEmptyTurnGuard) shouldSuppressTurn(turn uint64) bool {
	return g != nil && g.armedTurn != 0 && turn != 0 && g.armedTurn == turn
}

// realtimeUserTranscriptIsEmpty reports whether a completed user transcript
// carries no usable speech content for response admission.
func realtimeUserTranscriptIsEmpty(text string) bool {
	return strings.TrimSpace(text) == ""
}

// realtimeConversationExitIntent reports whether a completed user transcript is
// a short conversation-control request to end the voice session. Matching is
// intentionally conservative: only normalized whole-utterance phrases qualify,
// and ambiguous answers like "不用了" stay with the model.
func realtimeConversationExitIntent(text string) bool {
	normalized := normalizeRealtimeExitTranscript(text)
	if normalized == "" {
		return false
	}
	_, ok := realtimeConversationExitPhrases[normalized]
	return ok
}

func normalizeRealtimeExitTranscript(text string) string {
	var b strings.Builder
	b.Grow(len(text))
	for _, r := range strings.ToLower(strings.TrimSpace(text)) {
		if unicode.IsSpace(r) {
			continue
		}
		if unicode.IsPunct(r) || unicode.IsSymbol(r) {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

// Keep this list short and high-precision. Broader farewell language still goes
// through the model-owned end_conversation tool.
var realtimeConversationExitPhrases = map[string]struct{}{
	"退下":                   {},
	"退下吧":                  {},
	"退一下":                  {},
	"退一下吧":                 {},
	"你退下":                  {},
	"你退下吧":                 {},
	"先退下":                  {},
	"先退下吧":                 {},
	"再见":                   {},
	"拜拜":                   {},
	"goodbye":              {},
	"bye":                  {},
	"stoplistening":        {},
	"stoptheservice":       {},
	"goidle":               {},
	"standby":              {},
	"gotosleep":            {},
	"waitforthenextwakeup": {},
}
