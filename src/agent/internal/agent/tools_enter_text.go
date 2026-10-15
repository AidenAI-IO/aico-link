package agent

import (
	"aiden-agent/internal/logging"
	"context"
	"encoding/json"
	"strings"
	"time"
)

// EnterTextTool is the single public text-entry tool. It prefers Phone Bridge
// clipboard paste when that route is currently usable, then falls back to a
// local mixed ASCII/IME entry strategy.
type EnterTextTool struct {
	engine                          *textInputEngine
	bridgeTool                      *textInputBridge
	deviceTypeFn                    func() string
	iosKeyboardIsolation            *iosKeyboardIsolationController
	allowIOSKeyboardIsolationBypass bool
}

// enterTextArgs is the public tool payload. textInputArgs adds only the
// internal state needed while processing an IME part.
type enterTextArgs struct {
	Text  string          `json:"text"`
	Focus *focusPointArgs `json:"focus"`
}

type enterTextToolResult struct {
	OK         bool   `json:"ok"`
	Suggestion string `json:"suggestion,omitempty"`
}

func (a enterTextArgs) toEngineArgs() textInputArgs {
	return textInputArgs{
		Text:  a.Text,
		Focus: *a.Focus,
	}
}

func (t *EnterTextTool) Name() string { return "enter_text" }

func (t *EnterTextTool) SetDeviceTypeFunc(fn func() string) {
	if t == nil {
		return
	}
	t.deviceTypeFn = fn
	if t.engine != nil {
		t.engine.hw.deviceTypeFn = fn
	}
	if t.bridgeTool != nil {
		t.bridgeTool.SetDeviceTypeFunc(fn)
	}
}

func (t *EnterTextTool) platform() string {
	if t == nil {
		return textInputPlatformFromDeviceType(defaultDeviceType)
	}
	if t.deviceTypeFn != nil {
		if platform := textInputPlatformFromDeviceType(t.deviceTypeFn()); platform != "" {
			return platform
		}
	}
	if t.engine != nil {
		return t.engine.hw.platform()
	}
	if t.bridgeTool != nil {
		return t.bridgeTool.platform()
	}
	return textInputPlatformFromDeviceType(defaultDeviceType)
}

func (t *EnterTextTool) Description() string {
	return `Enter exact text into a visible, focused input field or composer. ` +
		`First prefers the Phone Bridge clipboard route when it is currently usable, including long, multiline, CJK, and non-ASCII text; it pastes and verifies the complete target. ` +
		`If Bridge is unavailable or its restore/paste attempt fails, the field must already be focused for the local HID/IME fallback. The local path holds the pointer-free keyboard isolation profile for the whole operation, probes the current ENG/IME state with a temporary "a", and maintains that state while entering parts in order. ` +
		`ASCII parts are typed directly in ENG mode without a vision verification step. IME parts are typed in IME mode and use vision-guided candidate selection until the part is committed. The local path never uses mouse input. ` +
		`Precondition: the latest screenshot must clearly show the editable field or composer, and focus coordinates must identify that field for bridge paste and visual analysis; never use a guessed blank-space coordinate. ` +
		`Provide the exact original text only: this tool detects ASCII and IME parts and derives required IME keystrokes internally. ` +
		`The result contains only ok, plus a next-step suggestion when ok is false.`
}

func (t *EnterTextTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"text":  stringArgSchema("Exact text that must appear in the field when done."),
		"focus": focusPointArgSchema("Coordinates inside a clearly visible editable field or composer."),
	}, "text", "focus")
}

func (t *EnterTextTool) Call(ctx context.Context, input string) (string, error) {
	return t.enterTextInner(ctx, input, false)
}

func (t *EnterTextTool) enterTextInner(ctx context.Context, input string, disableBridge bool) (string, error) {
	started := time.Now()
	ctx, metrics := withTextInputMetrics(ctx)
	var output string
	var callErr error
	defer func() {
		duration := time.Since(started)
		characters := metrics.characters.Load()
		logging.Debugf("agent", "text_input", "enter_text end ok=%t chars=%d duration=%s time_per_char=%s vllm_calls=%d", enterTextOutputOK(output, callErr), characters, duration, textInputDurationPerCharacter(duration, characters), metrics.vllmCalls.Load())
	}()
	var controller *iosKeyboardIsolationController
	if t != nil {
		controller = t.iosKeyboardIsolation
	}
	output, callErr = withIOSKeyboardIsolationBatchCall(ctx, controller, func(batchCtx context.Context) (string, error) {
		if t == nil || t.engine == nil {
			return enterTextToolFailure(batchCtx, CodeModuleUnavailable, "Configure enter_text dependencies, then retry."), nil
		}
		var publicArgs enterTextArgs
		if err := decodeStrictJSONObject(input, &publicArgs); err != nil {
			return enterTextToolFailure(batchCtx, CodeInvalidArguments, "Call enter_text again with valid JSON containing text and focus."), nil
		}
		if publicArgs.Focus == nil {
			return enterTextToolFailure(batchCtx, CodeInvalidArguments, "Call enter_text again with valid JSON containing text and focus."), nil
		}
		metrics.characters.Store(int64(len([]rune(publicArgs.Text))))
		args := publicArgs.toEngineArgs()
		if strings.TrimSpace(args.Text) == "" {
			return enterTextToolFailure(batchCtx, CodeInvalidArguments, "Provide non-empty text, then retry enter_text."), nil
		}
		platform := t.platform()
		localController := controller
		if localController == nil {
			localController = iosKeyboardIsolationControllerFromContext(batchCtx)
		}
		if !disableBridge && t.bridgeAvailable(args) {
			bridgeResult, attempted := t.bridgeTool.runClipboardFirstResult(batchCtx, args)
			if attempted && bridgeResult.OK {
				return enterTextToolResultString(bridgeResult), nil
			}
			// A Bridge attempt is an optimization, not a hard dependency. In
			// particular, restoring the companion app through Dynamic Island can
			// fail after the route was selected. Continue with the local HID/IME
			// path so enter_text remains usable on the phone's visible UI.
		}
		if platform == "ios" && localController == nil && !t.allowIOSKeyboardIsolationBypass {
			return enterTextToolFailure(batchCtx, CodeModuleUnavailable, "Enable iOS keyboard isolation, then retry enter_text."), nil
		}
		var result textInputResult
		var err error
		runLocal := func() error {
			result, err = t.engine.RunSegmented(batchCtx, args)
			return err
		}
		if localController != nil {
			err = localController.withKeyboard(batchCtx, true, runLocal)
		} else {
			err = runLocal()
		}
		if err != nil {
			return enterTextToolFailure(batchCtx, CodeToolExecutionFailed, "Inspect the latest screen, refocus the target field, then retry enter_text."), nil
		}
		if !result.OK {
			logging.Warnf("agent", "text_input", "enter_text failed: %s", result.Reason)
			return enterTextToolFailure(batchCtx, CodeToolExecutionFailed, enterTextFailureSuggestion(result)), nil
		}
		return enterTextToolResultString(result), nil
	})
	return output, callErr
}

func enterTextOutputOK(output string, callErr error) bool {
	if callErr != nil {
		return false
	}
	var result enterTextToolResult
	if err := json.Unmarshal([]byte(output), &result); err != nil {
		return false
	}
	return result.OK
}

func enterTextToolResultString(result textInputResult) string {
	if result.OK {
		return jsonString(enterTextToolResult{OK: true})
	}
	return jsonString(enterTextToolResult{OK: false, Suggestion: enterTextFailureSuggestion(result)})
}

func enterTextToolFailure(ctx context.Context, code, suggestion string) string {
	output := jsonString(enterTextToolResult{OK: false, Suggestion: suggestion})
	SetToolError(ctx, NewToolError(code, output))
	return output
}

func enterTextFailureSuggestion(result textInputResult) string {
	if strings.Contains(strings.ToLower(result.Reason), "deadline") || strings.Contains(strings.ToLower(result.Reason), "timeout") {
		return "Retry enter_text; if it times out again, enter a shorter section at a time."
	}
	if result.BackendFailure {
		return "The input backend could not execute keyboard input. Refocusing will not fix this; report the input connection or unsupported operation."
	}
	if result.WrongIMESuspected {
		return "Switch to the intended IME, refocus the field, then retry enter_text."
	}
	if strings.TrimSpace(result.FieldText) != "" {
		return "Inspect the latest screen and correct or clear the existing field text before retrying enter_text."
	}
	return "Inspect the latest screen, refocus the target field, then retry enter_text."
}

func (t *EnterTextTool) bridgeAvailable(args textInputArgs) bool {
	if t == nil || t.bridgeTool == nil {
		return false
	}
	var hw *textInputHardwareDeps
	if t.engine != nil {
		hw = &t.engine.hw
	} else if t.bridgeTool != nil {
		hw = t.bridgeTool.hw
	}
	platform := t.platform()
	if strings.TrimSpace(platform) == "" {
		platform = textInputHardwarePlatform(hw)
	}
	return t.bridgeTool.canUseClipboardFirst(platform, args.Text)
}
