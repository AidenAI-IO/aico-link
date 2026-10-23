package agent

import (
	"aico-agent/internal/logging"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
)

type textInputMetricsContextKey struct{}

type textInputMetrics struct {
	characters atomic.Int64
	vllmCalls  atomic.Int64
}

func withTextInputMetrics(ctx context.Context) (context.Context, *textInputMetrics) {
	metrics := &textInputMetrics{}
	return context.WithValue(ctx, textInputMetricsContextKey{}, metrics), metrics
}

func textInputMetricsFromContext(ctx context.Context) *textInputMetrics {
	if ctx == nil {
		return nil
	}
	metrics, _ := ctx.Value(textInputMetricsContextKey{}).(*textInputMetrics)
	return metrics
}

func beginTextInputVLLMCall(ctx context.Context, operation string) func(error) {
	metrics := textInputMetricsFromContext(ctx)
	if metrics == nil {
		return func(error) {}
	}
	call := metrics.vllmCalls.Add(1)
	started := time.Now()
	logging.Debugf("agent", "text_input", "vllm begin call=%d operation=%s", call, operation)
	return func(err error) {
		duration := time.Since(started)
		if err != nil {
			logging.Debugf("agent", "text_input", "vllm end call=%d operation=%s duration=%s ok=false err=%q", call, operation, duration, err)
			return
		}
		logging.Debugf("agent", "text_input", "vllm end call=%d operation=%s duration=%s ok=true", call, operation, duration)
	}
}

func textInputDurationPerCharacter(duration time.Duration, characters int64) time.Duration {
	if characters <= 0 {
		return 0
	}
	return duration / time.Duration(characters)
}

type textInputMode string

const (
	textInputModeASCII       textInputMode = "ascii"
	textInputModeComposition textInputMode = "composition"
	textInputModeUnknown     textInputMode = "unknown"

	textInputKeystrokeGap           = 60 * time.Millisecond
	textInputFocusRestoreDelay      = time.Second
	textInputLocalIMESettleDelay    = 300 * time.Millisecond
	textInputCandidateSettleDelay   = textInputLocalIMESettleDelay
	textInputInitialCandidateDelay  = textInputLocalIMESettleDelay
	textInputIMESwitchSettleDelay   = textInputLocalIMESettleDelay
	textInputClearBackspaceRepeats  = 32
	textInputClearBackspaceFallback = 16
	textInputCandidatePageMax       = 5
	textInputCandidateActionMax     = 20
	textInputCandidateMoveMax       = 20
	textInputVisionParseAttempts    = 3
	textInputPlanAttempts           = 3
	textInputModelMaxTokens         = 4096
	textInputVisionMaxTokens        = textInputModelMaxTokens
	textInputPlanMaxTokens          = textInputModelMaxTokens
	textInputPlanConcurrency        = 4
	textInputIMEPartitionMinRunes   = 5
	textInputIMEPartitionMaxRunes   = 6
)

var textInputCompositionReadyDelay = textInputLocalIMESettleDelay
var textInputProbeSettleDelay = textInputLocalIMESettleDelay

func containsHanRunes(text string) bool {
	for _, r := range text {
		if unicode.Is(unicode.Han, r) {
			return true
		}
	}
	return false
}

func compositionSegmentsForText(text string, segments []string) ([]string, error) {
	segs := normalizeCompositionSegments(segments)
	if len(segs) == 0 {
		return nil, fmt.Errorf("composition input requires segments (IME romanization syllables); none provided for %q", text)
	}
	return segs, nil
}

// textInputCompositionPlanner derives the ASCII keystrokes required by the
// active IME for a non-ASCII target. It is intentionally internal to the text
// entry workflow so the main agent never has to construct pinyin/IME segments.
type textInputCompositionPlanner interface {
	PlanComposition(ctx context.Context, text string) ([]string, error)
}

// textInputCompositionPartitioner splits a long IME run into smaller semantic
// units before romanization planning and candidate selection.
type textInputCompositionPartitioner interface {
	PartitionComposition(ctx context.Context, text string) ([]string, error)
}

func (e *textInputEngine) compositionSegmentsForText(ctx context.Context, text string, override []string) ([]string, error) {
	if segments := normalizeCompositionSegments(override); len(segments) > 0 {
		return segments, nil
	}
	planner, ok := e.vision.(textInputCompositionPlanner)
	if !ok {
		return nil, fmt.Errorf("IME segment planner is not configured for %q", text)
	}
	segments, err := planner.PlanComposition(ctx, text)
	if err != nil {
		return nil, fmt.Errorf("plan IME segments: %w", err)
	}
	segments = normalizePlannedCompositionSegments(segments)
	if len(segments) == 0 {
		return nil, fmt.Errorf("IME segment planner returned no input for %q", text)
	}
	for _, segment := range segments {
		for _, r := range segment {
			if r > 0x7f {
				return nil, fmt.Errorf("IME segment planner returned non-ASCII input %q", segment)
			}
			if _, ok := keyboardLayoutKeyStroke(defaultKeyboardLayout, byte(r)); !ok {
				return nil, fmt.Errorf("IME segment planner returned unsupported key %q", r)
			}
		}
	}
	return segments, nil
}

// normalizePlannedCompositionSegments tolerates an LLM returning a whole
// pinyin phrase (for example "ni hao") in one JSON array entry. keyboard_text
// correctly rejects that form, so split it into individual HID-safe chunks
// before any keyboard call.
func normalizePlannedCompositionSegments(segments []string) []string {
	out := make([]string, 0, len(segments))
	for _, segment := range segments {
		out = append(out, strings.Fields(segment)...)
	}
	return out
}

func fieldTextExactlyMatches(fieldText, targetText string) bool {
	return strings.TrimSpace(fieldText) == strings.TrimSpace(targetText)
}

func evaluateFieldCommit(analysis textInputScreenAnalysis) (committed bool, fieldText string) {
	fieldText = strings.TrimSpace(analysis.FieldText)
	// field_text is a diagnostic transcription, not a reliable code-point-level
	// representation of the screenshot. target_matched already means that the
	// vision model found the target in the committed field, so do not override
	// that decision with a second OCR/string or composition flag check.
	return analysis.TargetMatched, fieldText
}

func asciiPartNeedsEnter(analysis textInputScreenAnalysis) bool {
	return analysis.CompositionPending ||
		analysis.ObservedMode == textInputModeComposition
}

func imeCandidateStateActive(analysis textInputScreenAnalysis) bool {
	return analysis.CompositionPending
}

func shouldSuspectWrongIME(analysis textInputScreenAnalysis, fieldText string, segments []string, requiredMode textInputMode) bool {
	if analysis.WrongIMESuspected || analysis.SuggestSwitchIME {
		return true
	}
	if requiredMode == textInputModeASCII {
		// ASCII text but IME is active (composition pending or candidates visible)
		if analysis.CompositionPending {
			return true
		}
		if analysis.ObservedMode == textInputModeComposition {
			return true
		}
		return false
	}
	// Composition mode checks below
	if analysis.CompositionPending {
		return false
	}
	if isRomanizationOnlyField(fieldText, segments) {
		return true
	}
	return analysis.ObservedMode == textInputModeASCII && !containsHanRunes(fieldText)
}

func normalizeCompositionSegments(segments []string) []string {
	out := make([]string, 0, len(segments))
	for _, segment := range segments {
		segment = strings.TrimSpace(segment)
		if segment == "" {
			continue
		}
		out = append(out, segment)
	}
	return out
}

func isRomanizationOnlyField(fieldText string, segments []string) bool {
	fieldText = strings.TrimSpace(fieldText)
	if fieldText == "" || containsHanRunes(fieldText) {
		return false
	}
	normalized := normalizeRomanizationField(fieldText)
	joined := strings.ToLower(strings.Join(segments, ""))
	if joined != "" && (normalized == joined || strings.Contains(normalized, joined)) {
		return true
	}
	return looksLikeSpacedRomanizationBlob(fieldText)
}

func normalizeRomanizationField(fieldText string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(fieldText) {
		if r >= 'a' && r <= 'z' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func looksLikeSpacedRomanizationBlob(text string) bool {
	text = strings.TrimSpace(text)
	if text == "" || !strings.Contains(text, " ") {
		return false
	}
	parts := strings.Fields(text)
	if len(parts) < 2 {
		return false
	}
	for _, part := range parts {
		if part != strings.ToLower(part) {
			return false
		}
		for _, r := range part {
			if r < 'a' || r > 'z' {
				return false
			}
		}
	}
	return true
}

func parseObservedTextInputMode(raw string) (textInputMode, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case string(textInputModeASCII), "english", "direct", "latin":
		return textInputModeASCII, nil
	case string(textInputModeComposition), "chinese", "pinyin", "ime", "cjk", "han":
		return textInputModeComposition, nil
	case string(textInputModeUnknown), "unclear", "unsure":
		return textInputModeUnknown, nil
	case "":
		return "", fmt.Errorf("observed_mode is required")
	default:
		return "", fmt.Errorf("unsupported observed_mode %q", raw)
	}
}

func focusPointArgSchema(description string) map[string]any {
	schema := objectArgsSchema(map[string]any{
		"x": coordinateSchema("Normalized 0-1000 X coordinate.", 500),
		"y": coordinateSchema("Normalized 0-1000 Y coordinate.", 300),
	}, "x", "y")
	schema["description"] = description
	return schema
}

type focusPointArgs struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
}

func (p *focusPointArgs) UnmarshalJSON(data []byte) error {
	var raw struct {
		X *float64 `json:"x"`
		Y *float64 `json:"y"`
	}
	if err := decodeStrictJSONObject(string(data), &raw); err != nil {
		return err
	}
	if raw.X == nil || raw.Y == nil {
		return fmt.Errorf("x and y are required")
	}
	p.X = *raw.X
	p.Y = *raw.Y
	return nil
}

func textInputKeyboardKeysForIMESwitch(platform string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "android":
		return []string{"KEYCODE_LANGUAGE_SWITCH"}, nil
	case "ios":
		return []string{"capslock"}, nil
	case "mac", "macos":
		return []string{"ctrl", "space"}, nil
	case "windows":
		return []string{"alt", "shift"}, nil
	case "linux":
		return []string{"ctrl", "space"}, nil
	default:
		return nil, fmt.Errorf("unsupported platform %q for IME switch", platform)
	}
}

func textInputKeyboardKeysForSelectAll(platform string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "android":
		return []string{"ctrl", "a"}, nil
	case "ios", "mac", "macos":
		return []string{"meta", "a"}, nil
	case "windows", "linux":
		return []string{"ctrl", "a"}, nil
	default:
		return nil, fmt.Errorf("unsupported platform %q for select all", platform)
	}
}

func textInputKeyboardKeysForUndo(platform string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "android":
		return []string{"ctrl", "z"}, nil
	case "ios", "mac", "macos":
		return []string{"meta", "z"}, nil
	case "windows", "linux":
		return []string{"ctrl", "z"}, nil
	default:
		return nil, fmt.Errorf("unsupported platform %q for undo", platform)
	}
}

func interpretTextInputToolOutput(out string) error {
	out = strings.TrimSpace(out)
	if out == "" {
		return fmt.Errorf("empty tool output")
	}
	if strings.HasPrefix(out, "{") {
		var payload struct {
			OK      *bool  `json:"ok"`
			Status  string `json:"status"`
			Message string `json:"message"`
		}
		if err := json.Unmarshal([]byte(out), &payload); err == nil && payload.OK != nil && !*payload.OK {
			msg := strings.TrimSpace(payload.Message)
			if msg == "" {
				msg = strings.TrimSpace(payload.Status)
			}
			if msg == "" {
				msg = "tool returned ok=false"
			}
			return fmt.Errorf("%s", msg)
		}
	}
	return nil
}
