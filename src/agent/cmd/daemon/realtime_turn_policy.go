package main

import (
	"strings"
	"unicode"
)

// realtimeEmptyTurnGuard suppresses the provider response that follows an empty
// or locally-handled user transcript. Server VAD may still create a response;
// the session loop cancels or drops that output instead of playing it.
type realtimeEmptyTurnGuard struct {
	suppressResponse bool
}

func (g *realtimeEmptyTurnGuard) suppress() {
	if g == nil {
		return
	}
	g.suppressResponse = true
}

func (g *realtimeEmptyTurnGuard) clear() {
	if g == nil {
		return
	}
	g.suppressResponse = false
}

func (g *realtimeEmptyTurnGuard) shouldSuppress() bool {
	return g != nil && g.suppressResponse
}

// realtimeUserTranscriptIsEmpty reports whether a completed user transcript
// carries no usable speech content for response admission.
func realtimeUserTranscriptIsEmpty(text string) bool {
	return strings.TrimSpace(text) == ""
}

// realtimeConversationExitIntent reports whether a completed user transcript is
// a short conversation-control request to end the voice session. Matching is
// intentionally conservative: only normalized whole-utterance phrases qualify,
// so ordinary requests that merely mention standby stay with the model.
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
	"没事了":                  {},
	"没有了":                  {},
	"不用了":                  {},
	"先这样":                  {},
	"goodbye":              {},
	"bye":                  {},
	"stoplistening":        {},
	"stoptheservice":       {},
	"goidle":               {},
	"standby":              {},
	"gotosleep":            {},
	"sleep":                {},
	"waitforthenextwakeup": {},
}
