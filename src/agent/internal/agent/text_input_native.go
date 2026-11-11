package agent

import (
	"context"
	"fmt"
	"time"

	"aico-agent/internal/agent/mnk"
)

func (e *textInputEngine) runNativeTextInsertion(ctx context.Context, args textInputArgs) (textInputResult, bool) {
	if e.hw.textInsertion == nil {
		return textInputResult{}, false
	}
	supported, err := e.hw.textInsertion.SupportsTextInsertion(ctx)
	if err != nil {
		return textInputResult{Reason: err.Error(), BackendFailure: true}, true
	}
	if !supported {
		return textInputResult{}, false
	}
	if err := mnk.ValidateInsertionText(args.Text); err != nil {
		return textInputResult{Reason: err.Error()}, true
	}
	if e.vision == nil {
		return textInputResult{Reason: "native text verification is not configured"}, true
	}
	if err := e.applyFocus(ctx, args.Focus); err != nil {
		return textInputFailure(err), true
	}
	// Never fall back or replay after insertion: a timeout or permission failure
	// can occur after partial delivery. The caller must inspect the field first.
	if err := e.hw.textInsertion.InsertText(ctx, args.Text, args.ReplaceField); err != nil {
		return textInputResult{Reason: fmt.Sprintf("native insertion may be partial: %v", err), BackendFailure: true, TextMayBeInserted: true}, true
	}
	if err := e.sleepFor(ctx, 350*time.Millisecond); err != nil {
		return textInputResult{Reason: err.Error(), TextMayBeInserted: true}, true
	}
	args.VerifyTextSuffix = !args.ReplaceField
	analysis, _, err := e.analyzeScreen(ctx, e.hw.platform(), args, nil)
	if err != nil {
		return textInputResult{Reason: fmt.Sprintf("text was sent but verification failed: %v", err), TextMayBeInserted: true}, true
	}
	committed, fieldText := evaluateFieldCommit(analysis)
	committed = committed && !analysis.CompositionPending
	return textInputResult{OK: committed, Committed: committed, FieldText: fieldText,
		TextMayBeInserted: true, Reason: "native text was sent; inspect the field before retrying"}, true
}
