package agent

import (
	"aiden-agent/internal/logging"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"

	langtools "github.com/tmc/langchaingo/tools"
)

type textInputHardwareDeps struct {
	pointerMode  string
	deviceTypeFn func() string
	touchGesture langtools.Tool
	keyboardTap  langtools.Tool
	keyboardText langtools.Tool
	quickAction  langtools.Tool
	screenshot   langtools.Tool
}

func normalizeTextInputPlatform(platform string) string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "ios", "iphone", "ipad", "ipados":
		return "ios"
	case "android":
		return "android"
	case "mac", "macos", "darwin":
		return "mac"
	default:
		return strings.ToLower(strings.TrimSpace(platform))
	}
}

func textInputPlatformFromDeviceType(deviceType string) string {
	normalizedDeviceType, ok := normalizeDeviceType(deviceType)
	if !ok {
		return ""
	}
	return normalizeTextInputPlatform(deviceTypePlatform(normalizedDeviceType))
}

func (d textInputHardwareDeps) platform() string {
	if d.deviceTypeFn != nil {
		if platform := textInputPlatformFromDeviceType(d.deviceTypeFn()); platform != "" {
			return platform
		}
	}
	return textInputPlatformFromDeviceType(defaultDeviceType)
}

func textInputHardwarePlatform(hw *textInputHardwareDeps) string {
	if hw == nil {
		return textInputPlatformFromDeviceType(defaultDeviceType)
	}
	return hw.platform()
}

type textInputEngine struct {
	hw     textInputHardwareDeps
	vision textInputVision
	sleep  func(context.Context, time.Duration) error
}

type textInputArgs struct {
	Text             string         `json:"text"`
	Focus            focusPointArgs `json:"focus"`
	CurrentIMEPart   string         `json:"-"`
	VerifyTextSuffix bool           `json:"-"`
}

type textInputResult struct {
	OK                bool   `json:"ok"`
	Committed         bool   `json:"committed"`
	FieldText         string `json:"field_text,omitempty"`
	WrongIMESuspected bool   `json:"wrong_ime_suspected,omitempty"`
	Reason            string `json:"reason,omitempty"`
	BackendFailure    bool   `json:"backend_failure,omitempty"`
}

type keyboardExecutionFailure struct{ error }

func (e keyboardExecutionFailure) Unwrap() error { return e.error }

func markKeyboardExecutionFailure(err error) error {
	var toolErr *ToolError
	if errors.As(err, &toolErr) && toolErr.Code == CodeToolExecutionFailed {
		return keyboardExecutionFailure{err}
	}
	return err
}

func isKeyboardExecutionFailure(err error) bool {
	var failure keyboardExecutionFailure
	return errors.As(err, &failure)
}

func callKeyboardInputTool(ctx context.Context, tool langtools.Tool, input string) (string, error) {
	out, err := callTextInputTool(ctx, tool, input)
	return out, markKeyboardExecutionFailure(err)
}

func newTextInputEngine(hw textInputHardwareDeps, vision textInputVision) *textInputEngine {
	return &textInputEngine{hw: hw, vision: vision}
}

func newTextInputEngineWithSleep(hw textInputHardwareDeps, vision textInputVision, sleep func(context.Context, time.Duration) error) *textInputEngine {
	engine := newTextInputEngine(hw, vision)
	engine.sleep = sleep
	return engine
}

func (e *textInputEngine) sleepFor(ctx context.Context, delay time.Duration) error {
	sleep := sleepWithContext
	if e != nil && e.sleep != nil {
		sleep = e.sleep
	}
	return sleep(ctx, delay)
}

// RunSegmented enters mixed text without relying on a clipboard route. The
// caller keeps the keyboard isolated for this entire operation. The engine
// probes the active input mode once, maintains that state while switching
// between direct and composition parts, and never uses pointer input.
func (e *textInputEngine) RunSegmented(ctx context.Context, args textInputArgs) (textInputResult, error) {
	if e == nil || e.vision == nil {
		return textInputResult{}, fmt.Errorf("text input engine not configured")
	}
	if strings.TrimSpace(args.Text) == "" {
		return textInputResult{Reason: "text is required"}, nil
	}
	initialChunks, err := splitTextInputChunks(args.Text)
	if err != nil {
		return textInputResult{Reason: err.Error()}, nil
	}
	platform := e.hw.platform()
	type compositionPlanResult struct {
		chunks   []textInputChunk
		segments [][]string
		err      error
	}
	planCtx, cancelPlanning := context.WithCancel(ctx)
	defer cancelPlanning()
	planResultCh := make(chan compositionPlanResult, 1)
	go func() {
		chunks, partitionErr := e.partitionIMEChunks(planCtx, initialChunks)
		if partitionErr != nil {
			planResultCh <- compositionPlanResult{err: partitionErr}
			return
		}
		segments, planErr := e.planCompositionSegmentsForChunks(planCtx, chunks)
		planResultCh <- compositionPlanResult{chunks: chunks, segments: segments, err: planErr}
	}()
	currentMode, _, err := e.probeTextInputMode(ctx, platform, args.Focus)
	if err != nil {
		cancelPlanning()
		return textInputResult{Reason: err.Error(), BackendFailure: isKeyboardExecutionFailure(err)}, nil
	}

	planResult := <-planResultCh
	if planResult.err != nil {
		return textInputResult{Reason: planResult.err.Error()}, nil
	}
	chunks := planResult.chunks
	compositionSegments := planResult.segments
	lastFieldText := ""
	for index, chunk := range chunks {
		if chunk.ascii {
			targetMode := textInputModeASCII
			if chunk.text != chunk.input {
				// Full-width punctuation is sent through an ASCII HID key, but the
				// active IME is what turns that key into the requested target rune.
				targetMode = textInputModeComposition
			}
			currentMode, err = e.ensureTextInputMode(ctx, platform, currentMode, targetMode)
			if err != nil {
				return textInputResult{Reason: err.Error(), BackendFailure: isKeyboardExecutionFailure(err)}, nil
			}
			if err := e.typeASCII(ctx, chunk.input); err != nil {
				return textInputResult{Reason: err.Error(), BackendFailure: isKeyboardExecutionFailure(err)}, nil
			}
			continue
		}
		currentMode, err = e.ensureTextInputMode(ctx, platform, currentMode, textInputModeComposition)
		if err != nil {
			return textInputResult{Reason: err.Error(), BackendFailure: isKeyboardExecutionFailure(err)}, nil
		}
		if err := e.sleepFor(ctx, textInputCompositionReadyDelay); err != nil {
			return textInputResult{Reason: err.Error()}, nil
		}
		partialArgs := args
		partialArgs.Text = chunk.text
		partialArgs.CurrentIMEPart = chunk.text
		partialArgs.VerifyTextSuffix = true
		committed, fieldText, wrongIME, _, err := e.typeCompositionWithCandidateSelection(ctx, platform, partialArgs, compositionSegments[index])
		if err != nil || !committed {
			reason := "IME run was not verified"
			if err != nil {
				reason = err.Error()
			} else if wrongIME {
				reason = "wrong IME suspected"
			}
			return textInputResult{FieldText: fieldText, WrongIMESuspected: wrongIME, Reason: reason, BackendFailure: isKeyboardExecutionFailure(err)}, nil
		}
		lastFieldText = fieldText
	}
	result := textInputResult{OK: true, Committed: true, FieldText: lastFieldText}
	return result, nil
}

func (e *textInputEngine) partitionIMEChunks(ctx context.Context, chunks []textInputChunk) ([]textInputChunk, error) {
	partitioner, ok := e.vision.(textInputCompositionPartitioner)
	type partResult struct {
		index int
		parts []string
		err   error
	}
	partitioned := make([][]string, len(chunks))
	results := make(chan partResult, len(chunks))
	semaphore := make(chan struct{}, textInputPlanConcurrency)
	partitionCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := 0
	for index, chunk := range chunks {
		if chunk.ascii || len([]rune(chunk.text)) < textInputIMEPartitionMinRunes {
			continue
		}
		if !ok {
			return nil, fmt.Errorf("IME partitioner is not configured for %q", chunk.text)
		}
		jobs++
		go func(index int, target string) {
			select {
			case semaphore <- struct{}{}:
			case <-partitionCtx.Done():
				results <- partResult{index: index, err: partitionCtx.Err()}
				return
			}
			defer func() { <-semaphore }()
			parts, err := partitioner.PartitionComposition(partitionCtx, target)
			if err == nil {
				_, err = validateTextInputPartition(target, parts)
			}
			results <- partResult{index: index, parts: parts, err: err}
		}(index, chunk.text)
	}
	var firstErr error
	for completed := 0; completed < jobs; completed++ {
		part := <-results
		if part.err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("partition IME part: %w", part.err)
				cancel()
			}
			continue
		}
		partitioned[part.index] = part.parts
	}
	if firstErr != nil {
		return nil, firstErr
	}
	result := make([]textInputChunk, 0, len(chunks)+jobs)
	for index, chunk := range chunks {
		if len(partitioned[index]) == 0 {
			result = append(result, chunk)
			continue
		}
		for _, part := range partitioned[index] {
			result = append(result, textInputChunk{text: part})
		}
	}
	return result, nil
}

func validateTextInputPartition(target string, parts []string) ([]string, error) {
	raw, err := json.Marshal(struct {
		Parts []string `json:"parts"`
	}{Parts: parts})
	if err != nil {
		return nil, err
	}
	return parseTextInputPartition(target, string(raw))
}

func (e *textInputEngine) planCompositionSegmentsForChunks(ctx context.Context, chunks []textInputChunk) ([][]string, error) {
	segments := make([][]string, len(chunks))
	type partResult struct {
		index    int
		segments []string
		err      error
	}
	jobs := 0
	results := make(chan partResult, len(chunks))
	semaphore := make(chan struct{}, textInputPlanConcurrency)
	planCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	for index, chunk := range chunks {
		if chunk.ascii {
			continue
		}
		jobs++
		go func(index int, target string) {
			select {
			case semaphore <- struct{}{}:
			case <-planCtx.Done():
				results <- partResult{index: index, err: planCtx.Err()}
				return
			}
			defer func() { <-semaphore }()
			planned, planErr := e.compositionSegmentsForText(planCtx, target, nil)
			results <- partResult{index: index, segments: planned, err: planErr}
		}(index, chunk.text)
	}
	var firstErr error
	for completed := 0; completed < jobs; completed++ {
		result := <-results
		if result.err != nil {
			if firstErr == nil {
				firstErr = result.err
				cancel()
			}
			continue
		}
		segments[result.index] = result.segments
	}
	if firstErr != nil {
		return nil, firstErr
	}
	return segments, nil
}

func (e *textInputEngine) probeTextInputMode(ctx context.Context, platform string, focus focusPointArgs) (mode textInputMode, vlmCalls int, err error) {
	undoKeys, err := textInputKeyboardKeysForUndo(platform)
	if err != nil {
		return textInputModeUnknown, vlmCalls, err
	}
	var probeBeforeScreenshot screenshotResult
	cleanupVision, cleanupSupported := e.vision.(textInputProbeCleanupVision)
	if cleanupSupported {
		probeBeforeScreenshot, err = e.captureScreenshot(ctx)
		if err != nil {
			logging.Warnf("agent", "text_input", "probe cleanup baseline screenshot failed: %v", err)
			cleanupSupported = false
		}
	}
	if err = e.typeASCIIChunk(ctx, "a"); err != nil {
		return textInputModeUnknown, vlmCalls, fmt.Errorf("input mode probe: type a: %w", err)
	}
	defer func() {
		// Send undo keys
		undoErr := e.tapKeys(ctx, undoKeys)
		if undoErr == nil {
			undoErr = e.sleepFor(ctx, textInputKeystrokeGap)
		}
		if undoErr != nil {
			err = errors.Join(err, undoErr)
			return
		}

		// Secondary verification is optional so probe-only vision
		// implementations can still complete mode analysis.
		if !cleanupSupported {
			return
		}

		// Wait for undo to take effect
		if waitErr := e.sleepFor(ctx, textInputProbeSettleDelay); waitErr != nil {
			err = errors.Join(err, waitErr)
			return
		}

		// Capture screenshot for verification
		verifyShot, captureErr := e.captureScreenshot(ctx)
		if captureErr != nil {
			logging.Errorf("agent", "text_input", "probe cleanup verification screenshot failed: %v", captureErr)
			err = errors.Join(err, fmt.Errorf("probe cleanup verification screenshot: %w", captureErr))
			return
		}

		// Ask LLM to verify if probe character is still visible
		probeStillVisible, verifyErr := cleanupVision.VerifyProbeCleanup(ctx, probeBeforeScreenshot, verifyShot, platform, focus)
		vlmCalls++
		if verifyErr != nil {
			logging.Errorf("agent", "text_input", "probe cleanup verification failed: %v", verifyErr)
			err = errors.Join(err, fmt.Errorf("probe cleanup verification: %w", verifyErr))
			return
		}

		if probeStillVisible {
			logging.Warnf("agent", "text_input", "probe character still visible after undo, sending backspace")
			if backspaceErr := e.tapKeys(ctx, []string{"backspace"}); backspaceErr != nil {
				err = errors.Join(err, fmt.Errorf("probe cleanup backspace: %w", backspaceErr))
				return
			}
			if waitErr := e.sleepFor(ctx, textInputKeystrokeGap); waitErr != nil {
				err = errors.Join(err, waitErr)
			}
		} else {
			logging.Infof("agent", "text_input", "probe character successfully removed by undo")
		}
	}()
	if err = e.sleepFor(ctx, textInputProbeSettleDelay); err != nil {
		return textInputModeUnknown, vlmCalls, err
	}
	shot, captureErr := e.captureScreenshot(ctx)
	if captureErr != nil {
		return textInputModeUnknown, vlmCalls, captureErr
	}
	probeVision, ok := e.vision.(textInputProbeVision)
	if !ok {
		return textInputModeUnknown, vlmCalls, fmt.Errorf("input mode probe vision is not configured")
	}
	analysis, analyzeErr := probeVision.ProbeInputMode(ctx, shot, platform, focus)
	vlmCalls++
	if analyzeErr != nil {
		return textInputModeUnknown, vlmCalls, analyzeErr
	}
	mode = analysis.Mode
	if mode != textInputModeASCII && mode != textInputModeComposition {
		return textInputModeUnknown, vlmCalls, fmt.Errorf("input mode probe returned %q", mode)
	}
	return mode, vlmCalls, nil
}

func (e *textInputEngine) ensureTextInputMode(ctx context.Context, platform string, current, target textInputMode) (textInputMode, error) {
	if current == target {
		return current, nil
	}
	if current != textInputModeASCII && current != textInputModeComposition {
		return current, fmt.Errorf("cannot switch from unknown input mode %q", current)
	}
	if _, err := e.cycleIME(ctx, platform); err != nil {
		return current, err
	}
	return target, nil
}

type textInputChunk struct {
	// text is the exact target text verified on screen. input is what the HID
	// keyboard sends; they differ for IME-aware full-width punctuation.
	text  string
	input string
	ascii bool
	space bool
}

func splitTextInputChunks(text string) ([]textInputChunk, error) {
	var chunks []textInputChunk
	for _, r := range text {
		if r == ' ' {
			chunks = append(chunks, textInputChunk{text: " ", input: " ", ascii: true, space: true})
			continue
		}
		input, ascii := directTextInputForRune(r)
		if r <= 0x7f && !ascii {
			return nil, fmt.Errorf("unsupported ASCII character %q for keyboard input", r)
		}
		// Keep punctuation isolated so it is sent in its original position and
		// cannot be absorbed into an adjacent direct-input or IME run.
		if isTextInputPunctuation(r) {
			chunks = append(chunks, textInputChunk{text: string(r), input: input, ascii: ascii})
			continue
		}
		if len(chunks) == 0 || chunks[len(chunks)-1].space || textInputChunkIsPunctuation(chunks[len(chunks)-1]) || chunks[len(chunks)-1].ascii != ascii {
			chunks = append(chunks, textInputChunk{ascii: ascii})
		}
		chunks[len(chunks)-1].text += string(r)
		chunks[len(chunks)-1].input += input
	}
	return chunks, nil
}

func isTextInputPunctuation(r rune) bool {
	if r <= 0x7f {
		return r != ' ' && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	}
	return unicode.IsPunct(r)
}

func textInputChunkIsPunctuation(chunk textInputChunk) bool {
	var last rune
	count := 0
	for _, r := range chunk.text {
		last = r
		count++
	}
	return count == 1 && isTextInputPunctuation(last)
}

// directTextInputForRune maps full-width punctuation to the equivalent HID
// key. With a CJK IME active, these keys produce the target punctuation while
// avoiding an unnecessary pinyin/candidate-selection cycle.
func directTextInputForRune(r rune) (string, bool) {
	if r <= 0x7f {
		_, ok := keyboardLayoutKeyStroke(defaultKeyboardLayout, byte(r))
		return string(r), ok
	}
	if input, ok := map[rune]string{
		'，': ",", '。': ".", '！': "!", '？': "?", '：': ":", '；': ";",
		'（': "(", '）': ")", '【': "[", '】': "]", '「': "[", '」': "]",
		'“': `"`, '”': `"`, '‘': "'", '’': "'",
	}[r]; ok {
		return input, true
	}
	return "", false
}

func (e *textInputEngine) analyzeActVerify(ctx context.Context, platform string, args textInputArgs, requiredMode textInputMode, segments []string) (committed bool, fieldText string, wrongIME bool, vlmCalls int, err error) {
	analysis, calls, err := e.analyzeScreen(ctx, platform, args, segments)
	vlmCalls += calls
	if err != nil {
		return false, "", false, vlmCalls, err
	}

	if requiredMode == textInputModeASCII && asciiPartNeedsEnter(analysis) {
		if err := e.tapKeys(ctx, []string{"enter"}); err != nil {
			return false, analysis.FieldText, false, vlmCalls, err
		}
		if err := e.sleepFor(ctx, textInputFocusRestoreDelay); err != nil {
			return false, analysis.FieldText, false, vlmCalls, err
		}
		analysis, calls, err = e.analyzeScreen(ctx, platform, args, segments)
		vlmCalls += calls
		if err != nil {
			return false, analysis.FieldText, false, vlmCalls, err
		}
		if analysis.CompositionPending {
			return false, analysis.FieldText, false, vlmCalls, nil
		}
		if committed, fieldText := evaluateFieldCommit(analysis); committed {
			return true, fieldText, false, vlmCalls, nil
		}
	}
	if requiredMode != textInputModeComposition || !imeCandidateStateActive(analysis) {
		if committed, fieldText := evaluateFieldCommit(analysis); committed {
			return true, fieldText, false, vlmCalls, nil
		}
	}

	if requiredMode == textInputModeComposition {
		candidateActions := 0
		pageAttempts := 0
		selectedCandidateText := ""
	candidateLoop:
		for candidateActions < textInputCandidateActionMax && imeCandidateStateActive(analysis) {
			action, calls, err := e.decideCandidateAction(ctx, platform, args, segments, selectedCandidateText)
			vlmCalls += calls
			if err != nil {
				return false, analysis.FieldText, false, vlmCalls, err
			}
			switch action.Action {
			case textInputCandidateActionSelect:
				if err := e.selectCandidateByKeyboard(ctx, action); err != nil {
					return false, analysis.FieldText, false, vlmCalls, err
				}
				selectedCandidateText += action.Text
				candidateActions++
				pageAttempts = 0
				// completes_part is the model's prediction, not proof that the
				// keyboard committed the candidate. Always settle and inspect the
				// field again, especially when this is the final IME part: an
				// early return here bubbles up as enter_text success without
				// verifying that the selection actually took effect.
				if err := e.sleepFor(ctx, textInputCandidateSettleDelay); err != nil {
					return false, analysis.FieldText, false, vlmCalls, err
				}
			case textInputCandidateActionExpand:
				if pageAttempts >= textInputCandidatePageMax {
					break candidateLoop
				}
				if err := e.tapKeys(ctx, []string{"down"}); err != nil {
					return false, analysis.FieldText, false, vlmCalls, err
				}
				candidateActions++
				pageAttempts++
				if err := e.sleepFor(ctx, textInputCandidateSettleDelay); err != nil {
					return false, analysis.FieldText, false, vlmCalls, err
				}
			case textInputCandidateActionUp:
				if err := e.tapKeys(ctx, []string{"up"}); err != nil {
					return false, analysis.FieldText, false, vlmCalls, err
				}
				candidateActions++
				if pageAttempts > 0 {
					pageAttempts--
				}
				if err := e.sleepFor(ctx, textInputCandidateSettleDelay); err != nil {
					return false, analysis.FieldText, false, vlmCalls, err
				}
			default:
				break candidateLoop
			}

			analysis, calls, err = e.analyzeScreen(ctx, platform, args, segments)
			vlmCalls += calls
			if err != nil {
				return false, analysis.FieldText, false, vlmCalls, err
			}
			if !imeCandidateStateActive(analysis) {
				if committed, fieldText := evaluateFieldCommit(analysis); committed {
					return true, fieldText, false, vlmCalls, nil
				}
				break
			}
		}
	}

	fieldText = analysis.FieldText
	wrongIME = shouldSuspectWrongIME(analysis, fieldText, segments, requiredMode)
	return false, fieldText, wrongIME, vlmCalls, nil
}

func (e *textInputEngine) analyzeScreen(ctx context.Context, platform string, args textInputArgs, segments []string) (analysis textInputScreenAnalysis, vlmCalls int, err error) {
	shot, err := e.captureScreenshot(ctx)
	if err != nil {
		return textInputScreenAnalysis{}, 0, err
	}
	req := textInputScreenAnalysisRequest{
		Platform:        platform,
		TargetText:      args.Text,
		MatchTextSuffix: args.VerifyTextSuffix,
		Focus:           args.Focus,
		Segments:        segments,
	}
	for attempt := 1; attempt <= textInputVisionParseAttempts; attempt++ {
		analysis, err = e.vision.AnalyzeScreen(ctx, shot, req)
		vlmCalls++
		if err == nil {
			break
		}
		var syntaxErr *json.SyntaxError
		if !errors.As(err, &syntaxErr) || attempt == textInputVisionParseAttempts {
			return textInputScreenAnalysis{}, vlmCalls, err
		}
	}
	return analysis, vlmCalls, nil
}

func (e *textInputEngine) decideCandidateAction(ctx context.Context, platform string, args textInputArgs, segments []string, selectedCandidateText string) (textInputCandidateAction, int, error) {
	vision, ok := e.vision.(textInputCandidateVision)
	if !ok {
		return textInputCandidateAction{}, 0, fmt.Errorf("candidate action vision is not configured")
	}
	shot, err := e.captureScreenshot(ctx)
	if err != nil {
		return textInputCandidateAction{}, 0, err
	}
	action, err := vision.DecideCandidateAction(ctx, shot, textInputScreenAnalysisRequest{
		Platform:               platform,
		TargetText:             args.Text,
		CandidateTargetText:    args.CurrentIMEPart,
		CandidateCommittedText: selectedCandidateText,
		Focus:                  args.Focus,
		Segments:               segments,
	})
	return action, 1, err
}

func (e *textInputEngine) applyFocus(ctx context.Context, focus focusPointArgs) error {
	_, err := callTextInputTool(ctx, e.hw.touchGesture, jsonString(map[string]any{
		"type":  "tap",
		"point": map[string]any{"x": focus.X, "y": focus.Y},
	}))
	if err != nil {
		return err
	}
	return e.sleepFor(ctx, textInputFocusRestoreDelay)
}

func (e *textInputEngine) tapKeys(ctx context.Context, keys []string) error {
	if e == nil || e.hw.keyboardTap == nil {
		return fmt.Errorf("keyboard_tap is not configured")
	}
	out, err := callKeyboardInputTool(ctx, e.hw.keyboardTap, jsonString(map[string]any{"keys": keys}))
	if err != nil {
		return err
	}
	return interpretTextInputToolOutput(out)
}

func (e *textInputEngine) selectCandidateByKeyboard(ctx context.Context, action textInputCandidateAction) error {
	offset := action.Offset
	if offset > textInputCandidateMoveMax || offset < -textInputCandidateMoveMax {
		return fmt.Errorf("candidate keyboard offset %d exceeds limit %d", offset, textInputCandidateMoveMax)
	}
	direction := "right"
	if offset < 0 {
		direction = "left"
		offset = -offset
	}
	for i := 0; i < offset; i++ {
		if err := e.tapKeys(ctx, []string{direction}); err != nil {
			return err
		}
		if err := e.sleepFor(ctx, textInputKeystrokeGap); err != nil {
			return err
		}
	}
	return e.tapKeys(ctx, []string{"space"})
}

func callTextInputTool(ctx context.Context, tool langtools.Tool, input string) (string, error) {
	toolCtx, _ := WithToolError(ctx)
	out, err := tool.Call(toolCtx, input)
	if err != nil {
		return out, err
	}
	if te := ToolErrorFromContext(toolCtx); te != nil {
		return out, te
	}
	return out, nil
}

func (e *textInputEngine) cycleIME(ctx context.Context, platform string) (string, error) {
	keys, err := textInputKeyboardKeysForIMESwitch(platform)
	if err != nil {
		return "", err
	}
	if err := e.tapKeys(ctx, keys); err != nil {
		return "", err
	}
	if err := e.sleepFor(ctx, textInputIMESwitchSettleDelay); err != nil {
		return "", err
	}
	return strings.Join(keys, "+"), nil
}

func (e *textInputEngine) typeASCII(ctx context.Context, text string) error {
	if !strings.Contains(text, " ") {
		return e.typeASCIIChunk(ctx, text)
	}
	parts := strings.Split(text, " ")
	for i, part := range parts {
		if part != "" {
			if err := e.typeASCIIChunk(ctx, part); err != nil {
				return err
			}
		}
		if i < len(parts)-1 {
			if err := e.tapKeys(ctx, []string{"space"}); err != nil {
				return err
			}
			if err := e.sleepFor(ctx, textInputKeystrokeGap); err != nil {
				return err
			}
		}
	}
	return nil
}

func (e *textInputEngine) typeASCIIChunk(ctx context.Context, text string) error {
	out, err := callKeyboardInputTool(ctx, e.hw.keyboardText, jsonString(map[string]string{"text": text}))
	if err != nil {
		return err
	}
	return interpretTextInputToolOutput(out)
}

func (e *textInputEngine) typeCompositionWithCandidateSelection(ctx context.Context, platform string, args textInputArgs, segments []string) (committed bool, fieldText string, wrongIME bool, vlmCalls int, err error) {
	for _, segment := range segments {
		out, err := callKeyboardInputTool(ctx, e.hw.keyboardText, jsonString(map[string]string{"text": segment}))
		if err != nil {
			return false, fieldText, false, vlmCalls, err
		}
		if err := interpretTextInputToolOutput(out); err != nil {
			return false, fieldText, false, vlmCalls, err
		}
		if err := e.sleepFor(ctx, textInputKeystrokeGap); err != nil {
			return false, fieldText, false, vlmCalls, err
		}
	}
	// Do not commit the IME's default candidate blindly. Analyze the live
	// candidate list first; analyzeActVerify will execute the model-selected
	// candidate action and verify the resulting committed field.
	if err := e.sleepFor(ctx, textInputInitialCandidateDelay); err != nil {
		return false, fieldText, false, vlmCalls, err
	}

	var calls int
	committed, fieldText, wrongIME, calls, err = e.analyzeActVerify(ctx, platform, args, textInputModeComposition, segments)
	vlmCalls += calls
	if err != nil {
		return false, fieldText, wrongIME, vlmCalls, err
	}
	return committed, fieldText, wrongIME, vlmCalls, nil
}

func (e *textInputEngine) clearField(ctx context.Context, platform string) error {
	// Take a screenshot to determine how many characters to delete
	shot, err := e.captureScreenshot(ctx)
	if err != nil {
		// Fallback: use escape to dismiss composition then moderate backspaces
		_ = e.tapKeys(ctx, []string{"escape"})
		_ = e.sleepFor(ctx, textInputKeystrokeGap)
		for i := 0; i < textInputClearBackspaceFallback; i++ {
			_ = e.tapKeys(ctx, []string{"backspace"})
			_ = e.sleepFor(ctx, textInputKeystrokeGap)
		}
		return nil
	}
	analysis, err := e.vision.AnalyzeScreen(ctx, shot, textInputScreenAnalysisRequest{
		Platform: platform,
	})
	if err != nil {
		_ = e.tapKeys(ctx, []string{"escape"})
		_ = e.sleepFor(ctx, textInputKeystrokeGap)
		for i := 0; i < textInputClearBackspaceFallback; i++ {
			_ = e.tapKeys(ctx, []string{"backspace"})
			_ = e.sleepFor(ctx, textInputKeystrokeGap)
		}
		return nil
	}

	// Dismiss any active composition in candidate bar first
	if analysis.CompositionPending {
		_ = e.tapKeys(ctx, []string{"escape"})
		_ = e.sleepFor(ctx, textInputKeystrokeGap)
	}

	// Backspace once per rune in the committed field text
	fieldLen := len([]rune(strings.TrimSpace(analysis.FieldText)))
	if fieldLen == 0 {
		return nil
	}
	for i := 0; i < fieldLen; i++ {
		if err := e.tapKeys(ctx, []string{"backspace"}); err != nil {
			return err
		}
		if err := e.sleepFor(ctx, textInputKeystrokeGap); err != nil {
			return err
		}
	}
	return nil
}

func (e *textInputEngine) captureScreenshot(ctx context.Context) (screenshotResult, error) {
	out, err := e.hw.screenshot.Call(ctx, "{}")
	if err != nil {
		return screenshotResult{}, err
	}
	var result screenshotResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return screenshotResult{}, fmt.Errorf("invalid screenshot JSON: %w", err)
	}
	if strings.TrimSpace(result.Data) == "" {
		return screenshotResult{}, fmt.Errorf("screenshot missing image data")
	}
	return result, nil
}
