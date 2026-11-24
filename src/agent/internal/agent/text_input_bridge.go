package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

const (
	textViaBridgePostWriteDelay = 2 * time.Second
	textViaBridgePostPasteDelay = 350 * time.Millisecond
	textViaBridgeMenuDelay      = 450 * time.Millisecond
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
	TextMayBeInserted bool
	Attempted         bool
	Committed         bool
	FieldText         string
	Err               error
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

func (t *textInputBridge) runClipboardFirstResult(ctx context.Context, args textInputArgs, verify bool) (textInputResult, bool) {
	platform := t.platform()
	if strings.TrimSpace(args.Text) == "" {
		return textInputResult{Reason: "text is required"}, false
	}
	bridgeResult := t.runClipboardFirstFlow(ctx, platform, args, verify)
	if !bridgeResult.Attempted {
		if bridgeResult.Err != nil {
			return textViaBridgeResultToTextInputResult(bridgeResult), true
		}
		return textInputResult{Reason: "reliable bridge clipboard path unavailable"}, false
	}
	// For explicit app typing, success means dispatch, not a verified field.
	if !verify && bridgeResult.TextMayBeInserted && bridgeResult.Err == nil {
		return textInputResult{OK: true, TextMayBeInserted: true}, true
	}
	return textViaBridgeResultToTextInputResult(bridgeResult), true
}

func (t *textInputBridge) runClipboardFirstFlow(ctx context.Context, platform string, args textInputArgs, verify bool) textViaBridgeResult {
	bridge := t.currentBridge()
	if bridge == nil {
		return textViaBridgeResult{Err: fmt.Errorf("phone bridge is not configured")}
	}
	status := bridge.getStatus()
	switch strings.ToLower(strings.TrimSpace(platform)) {
	case "ios":
		if bridge.ClipboardRecentlyEquals(args.Text, preparedClipboardMaxAge) {
			return t.finishClipboardPaste(ctx, platform, args, verify)
		}
		if phoneBridgeCanUsePiPBackground(status, "clipboard_write") {
			return t.runClipboardWritePasteFlow(ctx, platform, args, verify)
		}
		if args.Focus != nil && (phoneBridgeCanRestoreFromReturnEntry(status) || phoneBridgeReadyForCommand(status)) {
			return t.runLegacyBridgeFlow(ctx, platform, args, verify)
		}
		return textViaBridgeResult{}
	case "android":
		if bridge.ClipboardRecentlyEquals(args.Text, preparedClipboardMaxAge) {
			return t.finishClipboardPaste(ctx, platform, args, verify)
		}
		return t.runClipboardWritePasteFlow(ctx, platform, args, verify)
	default:
		return textViaBridgeResult{}
	}
}

func textViaBridgeResultToTextInputResult(bridgeResult textViaBridgeResult) textInputResult {
	result := textInputResult{TextMayBeInserted: bridgeResult.TextMayBeInserted}
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

func (t *textInputBridge) finishClipboardPaste(ctx context.Context, platform string, args textInputArgs, verify bool) textViaBridgeResult {
	engine := newTextInputEngineWithSleep(*t.hw, t.vision, t.sleep)
	result := t.focusPaste(ctx, engine, platform, args, verify)
	result.Attempted = true
	return result
}

func (t *textInputBridge) runClipboardWritePasteFlow(ctx context.Context, platform string, args textInputArgs, verify bool) textViaBridgeResult {
	bridge := t.currentBridge()
	if bridge == nil {
		return textViaBridgeResult{Attempted: true, Err: fmt.Errorf("phone bridge is not configured")}
	}
	if err := t.writeClipboard(ctx, bridge, args.Text); err != nil {
		return textViaBridgeResult{Attempted: true, Err: err}
	}
	return t.finishClipboardPaste(ctx, platform, args, verify)
}

func (t *textInputBridge) runLegacyBridgeFlow(ctx context.Context, platform string, args textInputArgs, verify bool) textViaBridgeResult {
	bridge := t.currentBridge()
	if bridge == nil {
		return textViaBridgeResult{Attempted: true, Err: fmt.Errorf("phone bridge is not configured")}
	}
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
	return t.finishClipboardPaste(ctx, platform, args, verify)
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

func (t *textInputBridge) focusPaste(ctx context.Context, engine *textInputEngine, platform string, args textInputArgs, verify bool) textViaBridgeResult {
	if err := engine.applyFocus(ctx, args.Focus); err != nil {
		return textViaBridgeResult{Err: err}
	}
	if verify && !args.ReplaceField {
		before, err := engine.captureScreenshot(ctx)
		if err != nil {
			return textViaBridgeResult{Err: fmt.Errorf("capture field before paste: %w", err)}
		}
		args.InsertionBefore = &before
	}
	mayHavePasted, pasteErr := t.pasteClipboard(ctx, platform)
	result := textViaBridgeResult{TextMayBeInserted: mayHavePasted, Err: pasteErr}
	if !verify || !mayHavePasted {
		return result
	}
	// A dispatch error, blank OCR result, or failed verification does not prove
	// the paste had no effect. Observe it, but do not send a second paste.
	observe := func() bool {
		if err := t.sleepAfterPaste(ctx); err != nil {
			result.Err = err
			return false
		}
		analysis, _, err := engine.analyzeScreen(ctx, platform, args, nil)
		if err != nil {
			result.Err = err
			return false
		}
		result.Committed, result.FieldText = evaluateFieldCommit(analysis)
		if result.Committed {
			result.Err = nil
		}
		return true
	}
	if !observe() || result.Committed || result.FieldText != "" {
		return result
	}
	// Allowing the existing paste permission prompt can complete the original
	// paste. Verify again after dismissing it, without replaying input.
	popup, err := t.handleBlockingPopup(ctx, engine, platform)
	if err != nil {
		result.Err = err
	} else if popup.blocking() {
		result.Err = fmt.Errorf("%s dialog is blocking input: %s", popup.Kind, popup.Summary)
	} else if popup.Handled {
		observe()
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

// pasteClipboard reports whether a paste might have reached the field, even
// when the tool returned an error. Only a definite pre-dispatch rejection can
// fall back to another shortcut.
func (t *textInputBridge) pasteClipboard(ctx context.Context, platform string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if t == nil || t.hw == nil {
		return false, fmt.Errorf("paste input tools are not configured")
	}
	if t.hw.quickAction != nil {
		if _, err := t.callQuickAction(ctx, "paste"); err == nil || !pasteNotDispatched(err) {
			return true, err
		}
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if t.hw.keyboardTap == nil {
		return false, fmt.Errorf("keyboard_tap is not configured")
	}
	err := t.keyboardTap(ctx, keyboardPasteKeys(platform))
	return !pasteNotDispatched(err), err
}

func pasteNotDispatched(err error) bool {
	var te *ToolError
	if !errors.As(err, &te) {
		return false
	}
	return actionNotDispatched(te) || te.Code == CodeQuickActionReserved || te.Code == CodeQuickActionUnsupportedPlatform
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
