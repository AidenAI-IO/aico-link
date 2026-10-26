package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	textViaBridgePostWriteDelay = 2 * time.Second
	textViaBridgePostPasteDelay = 350 * time.Millisecond
	textViaBridgeMenuDelay      = 450 * time.Millisecond
	textViaBridgePasteAttempts  = 2
	preparedClipboardMaxAge     = 5 * time.Minute
	textViaBridgeLongPressMS    = 900
)

type bridgeSearchResult struct {
	Found    bool            `json:"found"`
	TapPoint *focusPointArgs `json:"tap_point"`
	Label    string          `json:"label,omitempty"`
}

type bridgeAppOpenResult struct {
	Opened bool   `json:"opened"`
	Reason string `json:"reason,omitempty"`
}

type pasteMenuResult struct {
	Found    bool            `json:"found"`
	TapPoint *focusPointArgs `json:"tap_point"`
	Label    string          `json:"label,omitempty"`
}

type textViaBridgeResult struct {
	Attempted bool
	Committed bool
	FieldText string
	Err       error
}

type textInputBridge struct {
	hw               *textInputHardwareDeps
	vision           textInputVision
	bridgeFn         func() *PhoneBridge
	restorer         *PhoneBridgeRestorer
	deviceTypeFn     func() string
	clipboardWriteFn func(context.Context, *PhoneBridge, string) error
	findPasteMenuFn  func(context.Context, screenshotResult, string) (pasteMenuResult, error)
	// findPopupFn replaces the blocking-dialog check in tests.
	findPopupFn func(context.Context, screenshotResult, string) (popupCheck, error)
	sleep       func(context.Context, time.Duration) error
}

func (t *textInputBridge) SetDeviceTypeFunc(fn func() string) {
	if t == nil {
		return
	}
	t.deviceTypeFn = fn
	if t.hw != nil {
		t.hw.deviceTypeFn = fn
	}
}

func (t *textInputBridge) platform() string {
	if t == nil {
		return textInputPlatformFromDeviceType(defaultDeviceType)
	}
	if t.deviceTypeFn != nil {
		if platform := textInputPlatformFromDeviceType(t.deviceTypeFn()); platform != "" {
			return platform
		}
	}
	return textInputHardwarePlatform(t.hw)
}

func (t *textInputBridge) runClipboardFirstResult(ctx context.Context, args textInputArgs) (textInputResult, bool) {
	platform := t.platform()
	if strings.TrimSpace(args.Text) == "" {
		return textInputResult{Reason: "text is required"}, false
	}
	bridgeResult := t.runClipboardFirstFlow(ctx, platform, args)
	if !bridgeResult.Attempted {
		if bridgeResult.Err != nil {
			return textViaBridgeResultToTextInputResult(bridgeResult), true
		}
		return textInputResult{Reason: "reliable bridge clipboard path unavailable"}, false
	}
	return textViaBridgeResultToTextInputResult(bridgeResult), true
}

func (t *textInputBridge) runClipboardFirstFlow(ctx context.Context, platform string, args textInputArgs) textViaBridgeResult {
	bridge := t.currentBridge()
	if bridge == nil {
		return textViaBridgeResult{Err: fmt.Errorf("phone bridge is not configured")}
	}
	status := bridge.getStatus()
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "ios":
		if bridge.ClipboardRecentlyEquals(args.Text, preparedClipboardMaxAge) {
			return t.runPreparedClipboardPasteFlow(ctx, platform, args)
		}
		if phoneBridgeCanUsePiPBackground(status, "clipboard_write") {
			return t.runClipboardWritePasteFlow(ctx, platform, args)
		}
		if phoneBridgeCanRestoreFromReturnEntry(status) || phoneBridgeReadyForCommand(status) {
			return t.runLegacyBridgeFlow(ctx, platform, args)
		}
		return textViaBridgeResult{}
	case "android":
		if bridge.ClipboardRecentlyEquals(args.Text, preparedClipboardMaxAge) {
			return t.runPreparedClipboardPasteFlow(ctx, platform, args)
		}
		return t.runClipboardWritePasteFlow(ctx, platform, args)
	default:
		return textViaBridgeResult{}
	}
}

func textViaBridgeResultToTextInputResult(bridgeResult textViaBridgeResult) textInputResult {
	result := textInputResult{}
	result.Committed = bridgeResult.Committed
	result.FieldText = bridgeResult.FieldText
	result.OK = bridgeResult.Committed
	if bridgeResult.Err != nil {
		result.Reason = bridgeResult.Err.Error()
		return result
	}
	if !bridgeResult.Committed {
		result.Reason = "bridge clipboard input not verified in field"
		return result
	}
	return result
}

func (t *textInputBridge) runPreparedClipboardPasteFlow(ctx context.Context, platform string, args textInputArgs) textViaBridgeResult {
	engine := newTextInputEngineWithSleep(*t.hw, t.vision, t.sleep)
	result := t.focusPasteVerify(ctx, engine, platform, args)
	result.Attempted = true
	return result
}

func (t *textInputBridge) runClipboardWritePasteFlow(ctx context.Context, platform string, args textInputArgs) textViaBridgeResult {
	bridge := t.currentBridge()
	if bridge == nil {
		return textViaBridgeResult{Attempted: true, Err: fmt.Errorf("phone bridge is not configured")}
	}
	if err := t.writeClipboard(ctx, bridge, args.Text); err != nil {
		return textViaBridgeResult{Attempted: true, Err: err}
	}
	engine := newTextInputEngineWithSleep(*t.hw, t.vision, t.sleep)
	result := t.focusPasteVerify(ctx, engine, platform, args)
	result.Attempted = true
	return result
}

func (t *textInputBridge) runLegacyBridgeFlow(ctx context.Context, platform string, args textInputArgs) textViaBridgeResult {
	bridge := t.currentBridge()
	if bridge == nil {
		return textViaBridgeResult{Attempted: true, Err: fmt.Errorf("phone bridge is not configured")}
	}
	engine := newTextInputEngineWithSleep(*t.hw, t.vision, t.sleep)
	if _, err := ensurePhoneBridgeReadyForCommand(ctx, bridge, t.restorer, "clipboard_write"); err != nil {
		return textViaBridgeResult{Attempted: true, Err: err}
	}
	bridge = t.currentBridge()
	if bridge == nil || !bridge.Connected() {
		return textViaBridgeResult{Attempted: true, Err: fmt.Errorf("phone bridge did not connect")}
	}
	if err := t.writeClipboard(ctx, bridge, args.Text); err != nil {
		return textViaBridgeResult{Attempted: true, Err: err}
	}
	if err := t.sleepAfterClipboardWrite(ctx); err != nil {
		return textViaBridgeResult{Attempted: true, Err: err}
	}
	if _, err := t.callQuickAction(ctx, "app_switch_back"); err != nil {
		return textViaBridgeResult{Attempted: true, Err: err}
	}
	result := t.focusPasteVerify(ctx, engine, platform, args)
	result.Attempted = true
	return result
}

func (t *textInputBridge) canUseClipboardFirst(platform string, text string) bool {
	bridge := t.currentBridge()
	if bridge == nil {
		return false
	}
	if bridge.ClipboardRecentlyEquals(text, preparedClipboardMaxAge) {
		return true
	}
	status := bridge.getStatus()
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "ios":
		return phoneBridgeCanUsePiPBackground(status, "clipboard_write") ||
			phoneBridgeCanRestoreFromReturnEntry(status) ||
			phoneBridgeReadyForCommand(status)
	case "android":
		return phoneBridgeReadyForCommand(status, "clipboard_write") || phoneBridgeCanUseFGSBackground(status, "clipboard_write")
	default:
		return false
	}
}

func (t *textInputBridge) focusPasteVerify(ctx context.Context, engine *textInputEngine, platform string, args textInputArgs) textViaBridgeResult {
	var result textViaBridgeResult
	// settled waits for the paste to land, then records what the field holds.
	// It reports true once the result is final: an error, a verified commit,
	// or any text in the field.
	settled := func() bool {
		if err := t.sleepAfterPaste(ctx); err != nil {
			result.Err = err
			return true
		}
		analysis, _, err := engine.analyzeScreen(ctx, platform, args, nil)
		if err != nil {
			result.Err = err
			return true
		}
		if committed, committedFieldText := evaluateFieldCommit(analysis); committed {
			result.Committed = true
			result.FieldText = committedFieldText
			return true
		}
		result.FieldText = analysis.FieldText
		return strings.TrimSpace(analysis.FieldText) != ""
	}
	for attempt := 1; attempt <= textViaBridgePasteAttempts; attempt++ {
		if err := engine.applyFocus(ctx, args.Focus); err != nil {
			result.Err = err
			return result
		}
		if attempt == 1 {
			_, _, _ = t.pasteClipboard(ctx, platform)
		} else {
			if err := t.pasteViaContextMenu(ctx, engine, platform, args.Focus); err != nil {
				result.Err = err
				return result
			}
		}
		if settled() {
			return result
		}
		// Nothing landed in the field. A dialog can hold the paste back, most
		// often iOS "Allow Paste" when the user skipped the onboarding
		// setting. Safe dialogs are handled here; any other dialog is reported
		// instead of being pasted over again.
		popup, err := t.handleBlockingPopup(ctx, engine, platform)
		if err != nil {
			result.Err = err
			return result
		}
		if popup.blocking() {
			result.Err = fmt.Errorf("%s dialog is blocking input: %s", popup.Kind, popup.Summary)
			return result
		}
		if popup.Handled && settled() {
			return result
		}
	}
	return result
}

// handleBlockingPopup checks for a dialog over the field after a paste had no
// effect, tapping it away when that is safe (see handlePopup).
func (t *textInputBridge) handleBlockingPopup(ctx context.Context, engine *textInputEngine, platform string) (popupCheck, error) {
	if t == nil || t.hw == nil || t.hw.touchGesture == nil {
		return popupCheck{}, nil
	}
	shot, err := engine.captureScreenshot(ctx)
	if err != nil {
		return popupCheck{}, err
	}
	popup, _, err := handlePopup(ctx, t.hw, t.vision, t.findPopupFn, shot, platform, popupMomentPaste)
	return popup, err
}

func (t *textInputBridge) pasteViaContextMenu(ctx context.Context, engine *textInputEngine, platform string, focus focusPointArgs) error {
	if t == nil || t.hw == nil || t.hw.touchGesture == nil {
		return fmt.Errorf("touch_gesture is not configured for long-press paste fallback")
	}
	_, err := callTextInputTool(ctx, t.hw.touchGesture, jsonString(map[string]any{
		"type":    "long_press",
		"point":   map[string]any{"x": focus.X, "y": focus.Y},
		"hold_ms": textViaBridgeLongPressMS,
	}))
	if err != nil {
		return fmt.Errorf("long-press focused field: %w", err)
	}
	if err := t.sleepAfterMenuOpen(ctx); err != nil {
		return err
	}
	shot, err := engine.captureScreenshot(ctx)
	if err != nil {
		return err
	}
	menu, err := t.findPasteMenuAction(ctx, shot, platform)
	if err != nil {
		return err
	}
	if !menu.Found {
		return fmt.Errorf("Paste/粘贴 menu action was not visible after long press")
	}
	if menu.TapPoint == nil {
		return fmt.Errorf("paste menu action is missing tap_point")
	}
	if _, err := callTextInputTool(ctx, t.hw.touchGesture, jsonString(map[string]any{
		"type":  "tap",
		"point": map[string]any{"x": menu.TapPoint.X, "y": menu.TapPoint.Y},
	})); err != nil {
		return fmt.Errorf("tap paste menu action: %w", err)
	}
	return nil
}

func (t *textInputBridge) findPasteMenuAction(ctx context.Context, shot screenshotResult, platform string) (pasteMenuResult, error) {
	if t != nil && t.findPasteMenuFn != nil {
		result, err := t.findPasteMenuFn(ctx, shot, platform)
		return result, err
	}
	modelVision, ok := t.vision.(*llmTextInputVision)
	if !ok || modelVision == nil {
		return pasteMenuResult{}, fmt.Errorf("paste menu vision is not configured")
	}
	llm := func(ctx context.Context) (pasteMenuResult, error) {
		return findPasteMenuActionLLM(ctx, modelVision, shot, platform)
	}
	result, _, err := decide(ctx, modelVision.decider, pasteMenuDecision(llm, shot, platform))
	return result, err
}

func findPasteMenuActionLLM(ctx context.Context, modelVision *llmTextInputVision, shot screenshotResult, platform string) (pasteMenuResult, error) {
	prompt := strings.TrimSpace(fmt.Sprintf(`Analyze this %s device screenshot after a long press inside a focused text field.
Find the visible system context-menu action that pastes the clipboard into that field. The label may be Paste, 粘贴, or another localized equivalent.
Return JSON only:
{
  "found": true,
  "tap_point": {"x": 500, "y": 700},
  "label": "Paste"
}

Rules:
- Return found=true only when a visible paste action is clearly tappable.
- Prefer the plain Paste/粘贴 action over unrelated actions such as Select, Look Up, Share, or Autofill.
- tap_point must be centered inside the visible paste action using normalized 0-1000 coordinates.
- If no paste action is visible, return {"found": false, "tap_point": {"x": 0, "y": 0}}.`, platform))
	raw, err := modelVision.visionJSON(ctx, "paste_menu", prompt, shot)
	if err != nil {
		return pasteMenuResult{}, err
	}
	var result pasteMenuResult
	if err := decodeStrictJSONObject(raw, &result); err != nil {
		return pasteMenuResult{}, fmt.Errorf("parse paste menu action: %w", err)
	}
	if result.Found && result.TapPoint == nil {
		return pasteMenuResult{}, fmt.Errorf("parse paste menu action: found result is missing tap_point")
	}
	return result, nil
}

func (t *textInputBridge) keyboardPaste(ctx context.Context, platform string) error {
	return t.keyboardTap(ctx, keyboardPasteKeys(platform))
}

func (t *textInputBridge) pasteClipboard(ctx context.Context, platform string) (method string, fallbackReason string, err error) {
	if t != nil && t.hw != nil && t.hw.quickAction != nil {
		if _, err := t.callQuickAction(ctx, "paste"); err == nil {
			return "quick_action", "", nil
		} else {
			fallbackReason = err.Error()
		}
	}
	if err := t.keyboardPaste(ctx, platform); err != nil {
		if fallbackReason != "" {
			return "", fallbackReason, fmt.Errorf("quick_action paste failed: %s; keyboard paste failed: %w", fallbackReason, err)
		}
		return "", "", err
	}
	return "keyboard", fallbackReason, nil
}

func (t *textInputBridge) keyboardTap(ctx context.Context, keys []string) error {
	if t == nil || t.hw == nil || t.hw.keyboardTap == nil {
		return fmt.Errorf("keyboard_tap is not configured")
	}
	out, err := callTextInputTool(ctx, t.hw.keyboardTap, jsonString(map[string]any{"keys": keys}))
	if err != nil {
		return err
	}
	return interpretTextInputToolOutput(out)
}

func keyboardPasteKeys(platform string) []string {
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "android":
		return []string{"ctrl", "v"}
	default:
		return []string{"meta", "v"}
	}
}

func (t *textInputBridge) sleepAfterClipboardWrite(ctx context.Context) error {
	sleep := sleepWithContext
	if t != nil && t.sleep != nil {
		sleep = t.sleep
	}
	return sleep(ctx, textViaBridgePostWriteDelay)
}

func (t *textInputBridge) sleepAfterPaste(ctx context.Context) error {
	sleep := sleepWithContext
	if t != nil && t.sleep != nil {
		sleep = t.sleep
	}
	return sleep(ctx, textViaBridgePostPasteDelay)
}

func (t *textInputBridge) sleepAfterMenuOpen(ctx context.Context) error {
	sleep := sleepWithContext
	if t != nil && t.sleep != nil {
		sleep = t.sleep
	}
	return sleep(ctx, textViaBridgeMenuDelay)
}

func (t *textInputBridge) currentBridge() *PhoneBridge {
	if t == nil || t.bridgeFn == nil {
		return nil
	}
	return t.bridgeFn()
}

func (t *textInputBridge) callQuickAction(ctx context.Context, action string) (string, error) {
	// Quick actions are an internal optimization for bridge paste. Isolate
	// their structured error from the parent enter_text call so a failed
	// Dynamic Island/paste route can still complete through local HID input.
	out, err := callTextInputTool(ctx, t.hw.quickAction, jsonString(map[string]any{"action": action}))
	if err != nil {
		return out, err
	}
	if err := interpretTextInputToolOutput(out); err != nil {
		return out, err
	}
	return out, nil
}

func (t *textInputBridge) writeClipboard(ctx context.Context, bridge *PhoneBridge, text string) error {
	if t != nil && t.clipboardWriteFn != nil {
		return t.clipboardWriteFn(ctx, bridge, text)
	}
	clipboard := NewClipboardTool(bridge, nil)
	// Isolate nested ClipboardTool SetToolError so a bridge failure cannot
	// poison the parent enter_text / search_launch_app observation.
	toolCtx, _ := WithToolError(ctx)
	clipOut, clipErr := clipboard.Call(toolCtx, jsonString(map[string]any{"action": "write", "text": text}))
	if clipErr != nil {
		return clipErr
	}
	if te := ToolErrorFromContext(toolCtx); te != nil {
		return te
	}
	return interpretTextInputToolOutput(clipOut)
}
