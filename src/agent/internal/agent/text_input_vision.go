package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aiden-agent/internal/agent/model"
	"aiden-agent/internal/logging"

	"github.com/tmc/langchaingo/llms"
)

type textInputScreenAnalysisRequest struct {
	Platform               string
	TargetText             string
	MatchTextSuffix        bool
	CandidateTargetText    string
	CandidateCommittedText string
	Focus                  focusPointArgs
	Segments               []string
}

type textInputCandidateActionKind string

const (
	textInputCandidateActionNone   textInputCandidateActionKind = "none"
	textInputCandidateActionSelect textInputCandidateActionKind = "select"
	textInputCandidateActionExpand textInputCandidateActionKind = "expand"
	textInputCandidateActionUp     textInputCandidateActionKind = "up"
)

type textInputCandidateAction struct {
	Action        textInputCandidateActionKind `json:"action"`
	Offset        int                          `json:"offset,omitempty"`
	Text          string                       `json:"text,omitempty"`
	CompletesPart bool                         `json:"completes_part,omitempty"`
}

type textInputScreenAnalysis struct {
	ObservedMode       textInputMode `json:"observed_mode"`
	FieldText          string        `json:"field_text"`
	TargetMatched      bool          `json:"target_matched"`
	CompositionPending bool          `json:"composition_pending"`
	WrongIMESuspected  bool          `json:"wrong_ime_suspected"`
	SuggestSwitchIME   bool          `json:"suggest_switch_ime"`
}

type textInputVision interface {
	AnalyzeScreen(ctx context.Context, screenshot screenshotResult, req textInputScreenAnalysisRequest) (textInputScreenAnalysis, error)
}

type textInputCandidateVision interface {
	DecideCandidateAction(ctx context.Context, screenshot screenshotResult, req textInputScreenAnalysisRequest) (textInputCandidateAction, error)
}

type textInputProbeAnalysis struct {
	Mode                    textInputMode
	TypedAVisible           bool
	InlinePreeditVisible    bool
	CandidatePopupVisible   bool
	CJKCandidateVisible     bool
	OnscreenKeyboardVisible bool
	Evidence                string
}

type textInputProbeVision interface {
	ProbeInputMode(ctx context.Context, screenshot screenshotResult, platform string, focus focusPointArgs) (textInputProbeAnalysis, error)
}

type textInputProbeCleanupVision interface {
	VerifyProbeCleanup(ctx context.Context, before, after screenshotResult, platform string, focus focusPointArgs) (bool, error)
}

type llmTextInputVision struct {
	models model.Model
}

func newLLMTextInputVision(models model.Model) textInputVision {
	if models == nil {
		return nil
	}
	return &llmTextInputVision{models: models}
}

func (v *llmTextInputVision) AnalyzeScreen(ctx context.Context, screenshot screenshotResult, req textInputScreenAnalysisRequest) (textInputScreenAnalysis, error) {
	prompt := buildTextInputAnalysisPrompt(req)
	raw, err := v.visionDecisionJSON(ctx, "screen_analysis", prompt, []string{"observed_mode", "target_matched", "composition_pending"}, screenshot)
	if err != nil {
		return textInputScreenAnalysis{}, err
	}
	var parsed struct {
		ObservedMode       string `json:"observed_mode"`
		FieldText          string `json:"field_text"`
		TargetMatched      bool   `json:"target_matched"`
		CompositionPending bool   `json:"composition_pending"`
		WrongIMESuspected  bool   `json:"wrong_ime_suspected"`
		SuggestSwitchIME   bool   `json:"suggest_switch_ime"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return textInputScreenAnalysis{}, fmt.Errorf("parse screen analysis: %w", err)
	}
	mode, err := parseObservedTextInputMode(parsed.ObservedMode)
	if err != nil {
		mode = textInputModeUnknown
	}
	return textInputScreenAnalysis{
		ObservedMode:       mode,
		FieldText:          strings.TrimSpace(parsed.FieldText),
		TargetMatched:      parsed.TargetMatched,
		CompositionPending: parsed.CompositionPending,
		WrongIMESuspected:  parsed.WrongIMESuspected,
		SuggestSwitchIME:   parsed.SuggestSwitchIME,
	}, nil
}

func (v *llmTextInputVision) DecideCandidateAction(ctx context.Context, screenshot screenshotResult, req textInputScreenAnalysisRequest) (textInputCandidateAction, error) {
	raw, err := v.visionDecisionJSON(ctx, "candidate_action", buildTextInputCandidateActionPrompt(req), []string{"action"}, screenshot)
	if err != nil {
		return textInputCandidateAction{}, err
	}
	var action textInputCandidateAction
	if err := json.Unmarshal([]byte(raw), &action); err != nil {
		return textInputCandidateAction{}, fmt.Errorf("parse candidate action: %w", err)
	}
	action.Action = textInputCandidateActionKind(strings.ToLower(strings.TrimSpace(string(action.Action))))
	switch action.Action {
	case textInputCandidateActionSelect:
		action.Text = strings.TrimSpace(action.Text)
	case textInputCandidateActionExpand, textInputCandidateActionUp, textInputCandidateActionNone:
		action.Offset = 0
		action.Text = ""
		action.CompletesPart = false
	default:
		action = textInputCandidateAction{Action: textInputCandidateActionNone}
	}
	return action, nil
}

func (v *llmTextInputVision) ProbeInputMode(ctx context.Context, screenshot screenshotResult, platform string, focus focusPointArgs) (textInputProbeAnalysis, error) {
	prompt := buildTextInputProbePrompt(platform, focus)
	raw, err := v.visionJSON(ctx, "ime_probe", prompt, screenshot)
	if err != nil {
		return textInputProbeAnalysis{}, err
	}
	var parsed struct {
		Mode                    string `json:"mode"`
		TypedAVisible           bool   `json:"typed_a_visible"`
		InlinePreeditVisible    bool   `json:"inline_preedit_visible"`
		CandidatePopupVisible   bool   `json:"candidate_popup_visible"`
		CJKCandidateVisible     bool   `json:"cjk_candidate_visible"`
		OnscreenKeyboardVisible bool   `json:"onscreen_keyboard_visible"`
		Evidence                string `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return textInputProbeAnalysis{}, fmt.Errorf("parse input mode probe: %w", err)
	}
	mode, err := parseObservedTextInputMode(parsed.Mode)
	if err != nil {
		return textInputProbeAnalysis{}, fmt.Errorf("parse input mode probe: %w", err)
	}
	// Objective composition evidence wins over a contradictory mode label. This
	// prevents a cursor beside "a" from being mistaken for committed English
	// when a desktop-style CJK candidate popup is visibly active below it.
	if parsed.InlinePreeditVisible || (parsed.CandidatePopupVisible && parsed.CJKCandidateVisible) {
		mode = textInputModeComposition
	}
	return textInputProbeAnalysis{
		Mode:                    mode,
		TypedAVisible:           parsed.TypedAVisible,
		InlinePreeditVisible:    parsed.InlinePreeditVisible,
		CandidatePopupVisible:   parsed.CandidatePopupVisible,
		CJKCandidateVisible:     parsed.CJKCandidateVisible,
		OnscreenKeyboardVisible: parsed.OnscreenKeyboardVisible,
		Evidence:                strings.TrimSpace(parsed.Evidence),
	}, nil
}

func buildTextInputProbePrompt(platform string, focus focusPointArgs) string {
	return strings.TrimSpace(fmt.Sprintf(`Determine the active keyboard input mode from this screenshot immediately after the automation typed the single lowercase letter "a".
Platform: %q
Focused field location (normalized 0-1000): (%.0f, %.0f)

This is an input-mode probe only. Do not judge or transcribe the user's eventual target text.
The device may use either a desktop-style IME or an on-screen keyboard. An on-screen keyboard may be visible or completely absent; its absence is normal and is never sufficient reason to return unknown.

Return JSON only using exactly this shape:
{"typed_a_visible":true,"inline_preedit_visible":false,"candidate_popup_visible":true,"cjk_candidate_visible":true,"onscreen_keyboard_visible":false,"mode":"composition","evidence":"short visual reason"}

Classification rules:
- First inspect the focused input field, text cursor, inline preedit text, underlines/highlights, and any candidate row or popup near the cursor or input field. Use an on-screen keyboard only as optional supporting evidence when one happens to be visible.
- HIGHEST PRIORITY: if a floating candidate bar near the typed "a" contains any Han/CJK characters, mode MUST be composition. This overrides the presence of a caret, the absence of an underline, and any appearance that "a" is ordinary committed text.
- A horizontal rounded popup directly below the input containing numbered alternatives such as "1 啊", an emoji, "3 阿", "4 钢", plus a disclosure arrow is a desktop-style CJK IME candidate popup. Set candidate_popup_visible=true, cjk_candidate_visible=true, and mode=composition.
- mode=composition if the typed "a" is shown as uncommitted inline preedit text, or if any nearby IME candidate row/popup contains Chinese candidates such as "啊", "爱", "阿", or similar characters. Candidate text does not need to match "a".
- mode=ascii only if the typed "a" is directly committed as ordinary field text and there is no active inline preedit, candidate row, or candidate popup near the input position.
- mode=unknown only if the focused input position and its surrounding candidate/preedit area are obscured or too unclear to inspect.
- Never return unknown merely because an on-screen keyboard is absent.
- A visible text cursor beside "a" does not prove ASCII mode. IME preedit text can also show a cursor.
- The absence of an underline does not prove ASCII mode. Candidate popup evidence takes priority.
- Keyboard autocorrect suggestions for already committed English text are not an IME composition state.
- When a keyboard is visible, distinguish ordinary autocorrect suggestions from an active IME candidate state by also checking the focused input position.`, platform, focus.X, focus.Y))
}

func (v *llmTextInputVision) VerifyProbeCleanup(ctx context.Context, before, after screenshotResult, platform string, focus focusPointArgs) (bool, error) {
	prompt := buildTextInputProbeCleanupPrompt(platform, focus)
	raw, err := v.visionJSONWithScreenshots(ctx, "probe_cleanup", prompt, before, after)
	if err != nil {
		return false, err
	}
	var parsed struct {
		ProbeCharacterVisible *bool  `json:"probe_character_visible"`
		CleanupSafe           bool   `json:"cleanup_safe"`
		Evidence              string `json:"evidence"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return false, fmt.Errorf("parse probe cleanup verification: %w", err)
	}
	if parsed.ProbeCharacterVisible == nil {
		return false, fmt.Errorf("parse probe cleanup verification: missing probe_character_visible")
	}
	if *parsed.ProbeCharacterVisible && !parsed.CleanupSafe {
		return false, fmt.Errorf("probe character remains visible but cleanup is unsafe")
	}
	return *parsed.ProbeCharacterVisible, nil
}

func buildTextInputProbeCleanupPrompt(platform string, focus focusPointArgs) string {
	return strings.TrimSpace(fmt.Sprintf(`Compare these two screenshots to verify whether the probe character "a" remains in the input field after the undo operation.
The first screenshot was captured immediately before automation typed the probe character. The second screenshot was captured after automation sent undo.
Platform: %q
Focused field location (normalized 0-1000): (%.0f, %.0f)

Return JSON only using exactly this shape:
{"probe_character_visible":false,"cleanup_safe":false,"evidence":"short visual reason"}

Verification rules:
- Inspect the focused input field at the specified coordinates in both screenshots.
- Set probe_character_visible=true only if the second screenshot shows the lowercase letter "a" at the probe insertion position as committed text or uncommitted preedit text.
- Set cleanup_safe=true only if the comparison confirms that this visible "a" was injected by this probe and a single Backspace will remove that probe character without deleting pre-existing user text.
- Set both fields false when the probe character was removed by undo.
- If the before screenshot is unclear, the insertion position cannot be identified, or the identity of the visible "a" is ambiguous, set cleanup_safe=false. A destructive cleanup must never be recommended based on the after screenshot alone.
- Focus on the cursor position and immediate surrounding text; ignore any "a" characters already present in the first screenshot.`, platform, focus.X, focus.Y))
}

// PartitionComposition splits one long IME run at natural language boundaries.
// It deliberately uses a dedicated prompt and response contract instead of
// sharing the romanization planner or candidate-selection prompts.
func (v *llmTextInputVision) PartitionComposition(ctx context.Context, target string) ([]string, error) {
	if target == "" {
		return nil, fmt.Errorf("IME partition target text is required")
	}
	if len([]rune(target)) < textInputIMEPartitionMinRunes {
		return []string{target}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	prompt := buildTextInputPartitionPrompt(target)
	var lastErr error
	for attempt := 1; attempt <= textInputPlanAttempts; attempt++ {
		attemptPrompt := prompt
		if lastErr != nil {
			attemptPrompt += fmt.Sprintf("\n\nThe previous response was invalid: %s. Return the complete corrected JSON object.", lastErr)
		}
		resp, err := v.generateContent(ctx, "ime_partition", []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, "You split exact IME target text into short semantic input parts. Output JSON only."),
			llms.TextParts(llms.ChatMessageTypeHuman, attemptPrompt),
		}, llms.WithJSONMode(), llms.WithMaxTokens(textInputPlanMaxTokens))
		if err != nil {
			return nil, err
		}
		if len(resp.Choices) == 0 {
			lastErr = fmt.Errorf("empty IME partition response")
			continue
		}
		parts, parseErr := parseTextInputPartition(target, resp.Choices[0].Content)
		if parseErr == nil {
			return parts, nil
		}
		lastErr = parseErr
	}
	return nil, fmt.Errorf("IME partition remained invalid after %d attempts: %w", textInputPlanAttempts, lastErr)
}

func buildTextInputPartitionPrompt(target string) string {
	return fmt.Sprintf(`Split the following exact IME target text into short semantic words or phrases for separate IME composition and candidate selection.
Target text: %q

Return JSON only:
{"parts":["自然词语","短语"]}

Rules:
- Concatenating every string in parts must reproduce Target text exactly, byte for byte and in the original order.
- Do not add, remove, normalize, reorder, or rewrite any character.
- Each part must be non-empty and contain at most %d Unicode characters.
- Choose natural word boundaries and keep established terms, names, and grammatical units intact when they fit the length limit.
- Prefer useful multi-character words or short phrases. Use a single-character part only when it is naturally independent or required by the length limit.
- Do not output romanization, pinyin, candidate choices, punctuation explanations, or any text outside the JSON object.`, target, textInputIMEPartitionMaxRunes)
}

func parseTextInputPartition(target, raw string) ([]string, error) {
	var parsed struct {
		Parts []string `json:"parts"`
	}
	if err := json.Unmarshal([]byte(stripJSONCodeFence(raw)), &parsed); err != nil {
		return nil, fmt.Errorf("parse IME partition: %w", err)
	}
	if len(parsed.Parts) == 0 {
		return nil, fmt.Errorf("IME partition returned no parts")
	}
	var joined strings.Builder
	for index, part := range parsed.Parts {
		if part == "" {
			return nil, fmt.Errorf("IME partition part %d is empty", index)
		}
		if count := len([]rune(part)); count > textInputIMEPartitionMaxRunes {
			return nil, fmt.Errorf("IME partition part %d has %d characters, maximum is %d", index, count, textInputIMEPartitionMaxRunes)
		}
		joined.WriteString(part)
	}
	if joined.String() != target {
		return nil, fmt.Errorf("IME partition does not reconstruct the exact target")
	}
	return parsed.Parts, nil
}

// PlanComposition turns one non-ASCII text run into the ASCII keystrokes that
// should be sent to the current IME. This keeps transliteration out of the
// public tool contract; enter_text callers supply only the original text.
func (v *llmTextInputVision) PlanComposition(ctx context.Context, target string) ([]string, error) {
	target = strings.TrimSpace(target)
	if target == "" {
		return nil, fmt.Errorf("IME target text is required")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	prompt := fmt.Sprintf(`Convert each character of the following exact non-ASCII text into the ASCII keystrokes needed to enter it with a standard phone IME.
Target text: %q

Return JSON only:
{"mappings":[{"index":0,"text":"原","input":"yuan"}]}

Rules:
- Return exactly one mapping for every Unicode character in Target text.
- index is the zero-based character index in Target text; include every index exactly once.
- text must be exactly the one target character at index.
- input is that character's common context-appropriate romanization/IME input.
- input must contain only lowercase ASCII letters or digits, without spaces or tone marks.
- Mapping array order does not matter; the caller reorders it by index.
- Do not include explanations or candidate labels.`, target)
	var lastErr error
	for attempt := 1; attempt <= textInputPlanAttempts; attempt++ {
		attemptPrompt := prompt
		if lastErr != nil {
			attemptPrompt += fmt.Sprintf("\n\nThe previous response was invalid: %s. Return the complete JSON object again.", lastErr)
		}
		resp, err := v.generateContent(ctx, "ime_plan", []llms.MessageContent{
			llms.TextParts(llms.ChatMessageTypeSystem, "You plan exact IME keystrokes for text-entry automation. Output JSON only."),
			llms.TextParts(llms.ChatMessageTypeHuman, attemptPrompt),
		}, llms.WithJSONMode(), llms.WithMaxTokens(textInputPlanMaxTokens))
		if err != nil {
			return nil, err
		}
		if len(resp.Choices) == 0 {
			lastErr = fmt.Errorf("empty IME planning response")
			continue
		}
		segments, err := parseTextInputCompositionPlan(target, resp.Choices[0].Content)
		if err == nil {
			return segments, nil
		}
		lastErr = err
	}
	return nil, fmt.Errorf("IME plan remained invalid after %d attempts: %w", textInputPlanAttempts, lastErr)
}

func parseTextInputCompositionPlan(target, raw string) ([]string, error) {
	var parsed struct {
		Mappings []struct {
			Index int    `json:"index"`
			Text  string `json:"text"`
			Input string `json:"input"`
		} `json:"mappings"`
	}
	if err := json.Unmarshal([]byte(stripJSONCodeFence(raw)), &parsed); err != nil {
		return nil, fmt.Errorf("parse IME plan: %w", err)
	}
	runes := []rune(target)
	if len(parsed.Mappings) != len(runes) {
		return nil, fmt.Errorf("IME plan returned %d character mappings for %d target characters", len(parsed.Mappings), len(runes))
	}
	segments := make([]string, len(runes))
	seen := make([]bool, len(runes))
	for _, mapping := range parsed.Mappings {
		if mapping.Index < 0 || mapping.Index >= len(runes) {
			return nil, fmt.Errorf("IME plan mapping index %d is out of range", mapping.Index)
		}
		if seen[mapping.Index] {
			return nil, fmt.Errorf("IME plan contains duplicate mapping index %d", mapping.Index)
		}
		mappedRunes := []rune(mapping.Text)
		if len(mappedRunes) != 1 || mappedRunes[0] != runes[mapping.Index] {
			return nil, fmt.Errorf("IME plan mapping index %d has text %q, want %q", mapping.Index, mapping.Text, string(runes[mapping.Index]))
		}
		input := strings.TrimSpace(mapping.Input)
		if input == "" {
			return nil, fmt.Errorf("IME plan mapping index %d has empty input", mapping.Index)
		}
		for _, r := range input {
			if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
				return nil, fmt.Errorf("IME plan mapping index %d has invalid input %q", mapping.Index, input)
			}
		}
		seen[mapping.Index] = true
		segments[mapping.Index] = input
	}
	return segments, nil
}

func buildTextInputAnalysisPrompt(req textInputScreenAnalysisRequest) string {
	segments := strings.Join(req.Segments, ", ")
	if segments == "" {
		segments = "(none yet)"
	}
	phaseHint := "Typing already happened. Read ONLY the focused target input field for field_text. Do NOT copy IME candidate bar, preedit strip, keyboard suggestions, or inline composition text into field_text. Set composition_pending=true whenever the just-typed content is still in an IME preedit or candidate state and needs confirmation before more text is typed. IME candidate selection is handled separately. If field shows only latin/pinyin segments instead of target characters, set wrong_ime_suspected=true and suggest_switch_ime=true."
	verificationScope := "exact-field"
	targetMatchRule := "target_matched: directly judge whether the complete committed text visible in the focused field matches Target text. Return false for any extra committed prefix or suffix"
	if req.MatchTextSuffix {
		verificationScope = "committed-suffix"
		targetMatchRule = "target_matched: judge only whether the committed text at the END of the focused field matches Target text exactly. Ignore any committed text before that suffix because it may have existed before this operation or been entered by earlier parts. Return false if the target suffix is missing, incomplete, reordered, or followed by extra committed text"
	}
	return strings.TrimSpace(fmt.Sprintf(`Analyze this device screenshot for text-input automation.
Platform: %q
Target text: %q
Verification scope: %q
Typed segments: %s
Focus (normalized 0-1000): (%.0f, %.0f)
%s

Return JSON only:
{
  "observed_mode": "ascii|composition|unknown",
  "field_text": "exact committed text visible ONLY inside the target input field",
  "target_matched": false,
  "composition_pending": false,
  "wrong_ime_suspected": false,
  "suggest_switch_ime": false
}

Rules:
- field_text: committed text inside the target input box only; exclude IME candidate rows, preedit, and keyboard suggestion chips
- %s. Use visual meaning, not a code-point comparison of your field_text transcription. Treat visually equivalent punctuation forms such as full-width versus half-width comma as matching when the screenshot cannot reliably distinguish them
- composition_pending: true whenever the just-typed content is still in an IME preedit/candidate state and requires confirmation, including an ASCII part shown with an IME candidate box
- observed_mode: ascii=direct Latin entry with no active candidate/preedit box; composition=any active IME candidate/preedit state, including a candidate box shown for an ASCII part such as "4k60"
- wrong_ime_suspected: true when field shows raw romanization instead of target script
- suggest_switch_ime: true only when confident the wrong keyboard/IME is active`, req.Platform, req.TargetText, verificationScope, segments, req.Focus.X, req.Focus.Y, phaseHint, targetMatchRule))
}

func buildTextInputCandidateActionPrompt(req textInputScreenAnalysisRequest) string {
	candidateTarget := req.CandidateTargetText
	if candidateTarget == "" {
		candidateTarget = req.TargetText
	}
	return strings.TrimSpace(fmt.Sprintf(`Choose the single next keyboard action for the active IME candidate UI in this screenshot.
Verification target for the just-entered text: %q
Current IME part: %q
Text already selected within the current IME part: %q
Focus (normalized 0-1000): (%.0f, %.0f)

Return JSON only in one of these forms:
{"action":"select","offset":0,"text":"visible candidate text","completes_part":true}
{"action":"expand"}
{"action":"up"}
{"action":"none"}

Rules:
- The remaining target is the suffix of Current IME part after Text already selected within the current IME part
- Judge actions from the visible candidate bar or candidate list. Text shown in the field as active preedit/composition is not proof that it has been committed
- select only when a visible candidate exactly matches one or more characters at the start of the remaining target. A shorter exact prefix, including a single character, is valid
- completes_part=true only when selecting this candidate finishes the entire Current IME part and no further candidate selection is needed. Account for any portion of the current part that is already committed or represented by the active preedit in the screenshot
- completes_part=false when another candidate action will still be required after this selection, or when completion is uncertain
- Do not select a candidate based only on similar pronunciation, related meaning, or likely intent
- If no exact-prefix candidate is visible and an expand/disclosure control is available, use expand
- expand means press Down to open the candidate list or move toward a lower candidate row
- If the current highlight is on a lower row and the desired candidate is visible on a row above it, use up to press Up once toward that row
- Use up when returning toward the first candidate row after an earlier Down movement; do not use up when the highlight is already on the first row
- If no exact-prefix candidate is visible and expansion is unavailable or cannot be determined, use none
- offset is the signed number of moves from the highlighted candidate: positive=Right, negative=Left, zero=select the highlighted candidate
- text must exactly transcribe the visible candidate selected and is used only for logging
- When the visible text or its relationship to the remaining target is uncertain, use none`, req.TargetText, candidateTarget, req.CandidateCommittedText, req.Focus.X, req.Focus.Y))
}

func (v *llmTextInputVision) visionJSON(ctx context.Context, operation, prompt string, screenshot screenshotResult) (string, error) {
	return v.visionJSONWithScreenshots(ctx, operation, prompt, screenshot)
}

// visionDecisionJSON retries, with feedback, when a syntactically valid response
// omits the decision fields. Some providers answer JSON mode with a bare format
// echo such as {"type":"json_object"}, which would otherwise read as a negative
// or unknown observation.
func (v *llmTextInputVision) visionDecisionJSON(ctx context.Context, operation, prompt string, required []string, screenshots ...screenshotResult) (string, error) {
	var missing []string
	for attempt := 1; attempt <= textInputVisionParseAttempts; attempt++ {
		attemptPrompt := prompt
		if len(missing) > 0 {
			attemptPrompt += fmt.Sprintf("\n\nThe previous response was missing required fields: %s. Analyze the same screenshot and return one complete JSON object with every required field.", strings.Join(missing, ", "))
		}
		raw, err := v.visionJSONWithScreenshots(ctx, operation, attemptPrompt, screenshots...)
		if err != nil {
			return "", err
		}
		missing = missingJSONFields(raw, required)
		if len(missing) == 0 {
			return raw, nil
		}
		logging.Warnf("agent", "text_input", "vision %s response missing %s attempt=%d", operation, strings.Join(missing, ","), attempt)
	}
	return "", fmt.Errorf("vision %s response missing required fields: %s", operation, strings.Join(missing, ", "))
}

// missingJSONFields leaves syntax errors to the caller's parser.
func missingJSONFields(raw string, required []string) []string {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return nil
	}
	var missing []string
	for _, key := range required {
		if value, ok := fields[key]; !ok || strings.TrimSpace(string(value)) == "null" {
			missing = append(missing, key)
		}
	}
	return missing
}

func (v *llmTextInputVision) visionJSONWithScreenshots(ctx context.Context, operation, prompt string, screenshots ...screenshotResult) (string, error) {
	if len(screenshots) == 0 {
		return "", fmt.Errorf("screenshot data missing")
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	msgs := []llms.MessageContent{
		llms.TextParts(llms.ChatMessageTypeSystem, "You analyze device screenshots for text input automation. Output JSON only."),
	}
	parts := []llms.ContentPart{llms.TextPart(prompt)}
	for _, screenshot := range screenshots {
		if strings.TrimSpace(screenshot.Data) == "" {
			return "", fmt.Errorf("screenshot data missing")
		}
		parts = append(parts, llms.ImageURLPart("data:image/jpeg;base64,"+screenshot.Data))
	}
	msgs = append(msgs, llms.MessageContent{Role: llms.ChatMessageTypeHuman, Parts: parts})
	// Use the model's configured temperature for vision analysis. Previously
	// hardcoded to 0 for determinism, but that breaks kimi-k3 (requires temp=1)
	// and the temperature difference has minimal impact on vision text extraction.
	resp, err := v.generateContent(ctx, operation, msgs,
		llms.WithJSONMode(),
		llms.WithMaxTokens(textInputVisionMaxTokens),
	)
	if err != nil {
		return "", err
	}
	if len(resp.Choices) == 0 {
		return "", fmt.Errorf("empty vision response")
	}
	return stripJSONCodeFence(resp.Choices[0].Content), nil
}

func (v *llmTextInputVision) generateContent(ctx context.Context, operation string, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	finish := beginTextInputVLLMCall(ctx, operation)
	response, err := v.models.GenerateContent(ctx, messages, options...)
	finish(err)
	return response, err
}

func stripJSONCodeFence(raw string) string {
	raw = strings.TrimSpace(raw)
	raw = strings.TrimPrefix(raw, "```json")
	raw = strings.TrimPrefix(raw, "```")
	raw = strings.TrimSuffix(raw, "```")
	return strings.TrimSpace(raw)
}

type stubTextInputVision struct {
	analyses []textInputScreenAnalysis
	actions  []textInputCandidateAction
}

func (s *stubTextInputVision) ProbeInputMode(_ context.Context, _ screenshotResult, _ string, _ focusPointArgs) (textInputProbeAnalysis, error) {
	if len(s.analyses) == 0 {
		return textInputProbeAnalysis{Mode: textInputModeComposition, Evidence: "stub default"}, nil
	}
	out := s.analyses[0]
	s.analyses = s.analyses[1:]
	mode := out.ObservedMode
	if out.CompositionPending {
		mode = textInputModeComposition
	}
	return textInputProbeAnalysis{Mode: mode, InlinePreeditVisible: out.CompositionPending, CandidatePopupVisible: out.CompositionPending, Evidence: "stub analysis"}, nil
}

func (s *stubTextInputVision) PartitionComposition(_ context.Context, text string) ([]string, error) {
	runes := []rune(text)
	parts := make([]string, 0, (len(runes)+textInputIMEPartitionMaxRunes-1)/textInputIMEPartitionMaxRunes)
	for start := 0; start < len(runes); start += textInputIMEPartitionMaxRunes {
		end := start + textInputIMEPartitionMaxRunes
		if end > len(runes) {
			end = len(runes)
		}
		parts = append(parts, string(runes[start:end]))
	}
	return parts, nil
}

func (s *stubTextInputVision) AnalyzeScreen(_ context.Context, _ screenshotResult, req textInputScreenAnalysisRequest) (textInputScreenAnalysis, error) {
	if len(s.analyses) == 0 {
		return textInputScreenAnalysis{ObservedMode: textInputModeComposition}, nil
	}
	out := s.analyses[0]
	s.analyses = s.analyses[1:]
	// Existing unit-test fixtures predate target_matched and describe successful
	// observations through field_text. Preserve that fixture shorthand without
	// reintroducing string comparison into the production verifier.
	if !out.TargetMatched && ((req.MatchTextSuffix && strings.HasSuffix(strings.TrimSpace(out.FieldText), strings.TrimSpace(req.TargetText))) || (!req.MatchTextSuffix && fieldTextExactlyMatches(out.FieldText, req.TargetText))) {
		out.TargetMatched = true
	}
	return out, nil
}

func (s *stubTextInputVision) DecideCandidateAction(_ context.Context, _ screenshotResult, _ textInputScreenAnalysisRequest) (textInputCandidateAction, error) {
	if len(s.actions) == 0 {
		return textInputCandidateAction{Action: textInputCandidateActionNone}, nil
	}
	action := s.actions[0]
	s.actions = s.actions[1:]
	return action, nil
}

func (s *stubTextInputVision) VerifyProbeCleanup(_ context.Context, _, _ screenshotResult, _ string, _ focusPointArgs) (bool, error) {
	// Stub always returns false (probe character successfully removed)
	return false, nil
}
