package agent

import (
	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/agent/screen"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultTapHoldMs is the dwell between a touch press and release so iOS
	// registers a tap rather than dropping the sub-millisecond event or
	// treating it as a long-press.
	defaultTapHoldMs = 60

	defaultSwipeDurationMs = 700
	defaultSwipeSteps      = 24

	// defaultCursorSettleMs is the dwell between positioning the HID absolute
	// cursor and pressing a button at that position. iOS HID cursor mode
	// smoothly animates the cursor toward the target; if the press lands while
	// the cursor is still in flight the input is interpreted as a drag from
	// the previous cursor position rather than a tap, which triggers
	// long-press menus or off-target activations.
	defaultCursorSettleMs = 80

	// screenDimensionsStaleAfter bounds how long cached screenshot dimensions
	// are trusted for pixel-coordinate resolution. Past this age the cache may
	// not match the current HDMI capture resolution.
	screenDimensionsStaleAfter = 30 * time.Second

	// touchReleaseReportCount repeats the button-up/touch-up report for touch-like
	// gestures. On phone USB HID hosts a single final release report can be missed
	// or coalesced with the final move, leaving the UI in a pressed/dragged state.
	// Repeating the release is harmless for normal hosts and gives the target
	// several polling frames to observe button-up.
	touchReleaseReportCount   = 3
	touchReleaseReportDelayMs = 15

	// defaultKeyboardTapHoldMs matches example_usb_hid's default tap duration.
	// Zero hold often fails on macOS/iOS hosts that miss sub-frame modifier chords.
	defaultKeyboardTapHoldMs = 50
)

// pointerController sends hid.usb1 reports in absolute mouse or touchscreen mode.
type pointerController struct {
	dev                  *mnk.HIDDevice
	state                *mnk.PointerState
	touchscreen          bool
	iosKeyboardIsolation *iosKeyboardIsolationController
}

func newPointerController(hid HIDConfig) *pointerController {
	return &pointerController{
		dev:         mnk.NewHIDDevice(hid.MouseDeviceOrDefault()),
		state:       &mnk.PointerState{},
		touchscreen: hid.PointerTouchscreen(),
	}
}

// KeyboardTapTool sends a key press then release via HID.
type KeyboardTapTool struct {
	mnkProvider  mnk.Provider
	deviceTypeFn func() string
}

func (t *KeyboardTapTool) Name() string { return "keyboard_tap" }

func (t *KeyboardTapTool) SetDeviceTypeFunc(fn func() string) {
	if t != nil {
		t.deviceTypeFn = fn
	}
}

func (t *KeyboardTapTool) platform() string {
	if t != nil && t.deviceTypeFn != nil {
		if deviceType, ok := normalizeDeviceType(t.deviceTypeFn()); ok {
			return normalizeAgentToolPlatform(deviceTypePlatform(deviceType))
		}
	}
	return ""
}

func (t *KeyboardTapTool) fullAndroidExtensionKeysVisible() bool {
	platform := t.platform()
	return platform == "" || platform == "android"
}

func (t *KeyboardTapTool) extensionKeyDescription() string {
	if t.fullAndroidExtensionKeysVisible() {
		return " Android device_type also supports single-key Android KEYCODE_* aliases through the extension keyboard device; they cannot be combined with modifiers/chords."
	}
	return " This non-Android device_type exposes only the absolute pointer-mode KEYCODE subset through the extension keyboard device: " + mnk.AbsolutePointerModeExtensionKeyList + "."
}

func (t *KeyboardTapTool) Description() string {
	description := `Press and release literal keyboard keys (e.g. {"keys":["enter"]}). Use for simple keys such as enter, escape, tab, or arrows; for exact physical chords explicitly requested by the user; and for app-specific shortcuts not represented by quick_action. Do not use keyboard_tap to enter text by spelling words, sentences, pinyin, romanization, or other sequential characters one key at a time. For text entry in an editable field, use enter_text. If enter_text fails or is unavailable, do not fall back to per-character keyboard_tap. A single letter or digit tap is appropriate only when the user explicitly requests that literal physical key, not as part of text entry. For cataloged semantic actions—including copy, paste, cut, select_all, delete_backward, delete_forward, undo, redo, find, send, back, home, app switching, and browser actions—you MUST use quick_action and let runtime select the device-specific binding from global device_type state. A ctrl/meta chord fallback is allowed only after a quick_action result in the current run explicitly reports the action as reserved/unavailable before executing a binding. Do not infer unavailability from another tool's failure. Never replay an active quick_action binding as a raw chord after failure or no visible effect.`
	if t.fullAndroidExtensionKeysVisible() {
		description += ` On Android device_type, single-key Android KEYCODE_* aliases are available through the Android extension keyboard device.`
	} else {
		description += ` On this non-Android device_type, only media, volume, screenshot, and brightness KEYCODE_* aliases are available through the extension keyboard device.`
	}
	return description
}

func (t *KeyboardTapTool) ArgsSchema() map[string]any {
	keysDescription := "Literal keys pressed simultaneously, e.g. [\"enter\"] or [\"shift\",\"tab\"]. Cataloged semantic actions MUST use quick_action. Raw ctrl/meta chords are allowed only for explicitly requested physical input, uncataloged app-specific shortcuts, or after a quick_action result in the current run explicitly reports the matching action as reserved/unavailable before execution. Do not infer unavailability or replay an active binding after failure or no visible effect. " +
		"Standard boot-keyboard keys: a-z, 0-9, f1-f12, enter, escape, backspace, tab, space, delete, arrows, home, end, pageup/down, insert, printscreen; modifiers ctrl, shift, alt, meta/super/win/cmd; modifier-only taps allowed. " +
		"backspace is backward-delete before the cursor; delete is forward-delete after the cursor. For semantic deletion, use quick_action delete_backward/delete_forward unless the user explicitly requests the literal physical key or the evidence-gated fallback above applies."
	keysDescription += t.extensionKeyDescription()
	keysSchema := stringArrayArgSchema(keysDescription, []string{"enter"}, []string{"shift", "tab"})
	keysSchema["minItems"] = 1
	keysSchema["maxItems"] = 6

	return objectArgsSchema(map[string]any{
		"keys": keysSchema,
	}, "keys")
}

func (t *KeyboardTapTool) Call(ctx context.Context, input string) (string, error) {
	adapter := mnk.NewKeyboardTapToolAdapter(t.mnkProvider)
	output, err := adapter.Call(ctx, input)
	return mapMNKAdapterResult(ctx, output, err)
}

// KeyboardTextTool types a string character by character via HID.
type KeyboardTextTool struct {
	textProvider         mnk.TextProvider
	dev                  *mnk.HIDDevice
	adb                  *ADBInputController
	keyboardLayout       string
	iosKeyboardIsolation *iosKeyboardIsolationController
}

// KeyboardText serves the provider endpoint through the same layout-aware
// primitive used by enter_text, without focusing or observing the device.
func (t *KeyboardTextTool) KeyboardText(ctx context.Context, text string) error {
	input, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	ctx, _ = WithToolError(ctx)
	if _, err := t.Call(ctx, string(input)); err != nil {
		return err
	}
	if toolErr := ToolErrorFromContext(ctx); toolErr != nil {
		return fmt.Errorf("keyboard input failed: %s", toolErr.Message)
	}
	return nil
}

func (t *KeyboardTextTool) Name() string { return "keyboard_text" }

func (t *KeyboardTextTool) Description() string {
	return `Configured-layout ASCII text input only via USB HID physical keyboard (not the on-screen soft keyboard). ` +
		`The physical keyboard layout comes from hid.keyboard_layout (qwerty, azerty, or qwertz). ` +
		`Allowed characters: a-z, A-Z, 0-9, space, and common ASCII punctuation. ` +
		`For model/tool calls, pass JSON only, for example {"text":"App Store"}; do not pass a bare string. ` +
		`Do NOT pass non-ASCII text, emoji, or spaced romanization — use enter_text for input box entry. ` +
		`Do not transliterate Chinese/CJK targets to pinyin or guessed ASCII keywords; if enter_text is unavailable, report the blocker instead. ` +
		`If a Chinese IME is active but the target text is English, switch to the English/Latin keyboard first, commonly with the globe/input-method key; do not leave English text in Chinese IME preedit/candidate state. ` +
		`Do not use keyboard_text for picker/wheel values, even if tapping the selected row appears to expose edit mode; use touch_gesture swipes and verify each returned screenshot instead. ` +
		`keyboard_text remains for simple standalone ASCII typing outside the enter_text workflow. ` +
		`Bare plain text is accepted only as a legacy compatibility fallback.`
}

func (t *KeyboardTextTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"text": stringArgSchema("ASCII text to type using the configured physical keyboard layout."),
	}, "text")
}

func keyboardTextUsesModifier(layout, text string) bool {
	for _, ch := range text {
		stroke, ok := mnk.KeyboardLayoutKeyStroke(layout, byte(ch))
		if ok && stroke.Modifier != 0 {
			return true
		}
	}
	return false
}

func (t *KeyboardTextTool) Call(ctx context.Context, input string) (string, error) {
	text, errText := parseKeyboardTextInput(input)
	if errText != "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, errText), nil
	}

	if unsupported := unsupportedKeyboardTextRunes(t.keyboardLayout, text); len(unsupported) > 0 {
		return toolErrorResultf(
			ctx,
			CodeInvalidArguments,
			"keyboard_text supports only ASCII characters available on the configured physical keyboard layout; unsupported characters: %q. Use enter_text for this target.",
			string(unsupported),
		), nil
	}
	if looksLikeSpacedRomanizationBlob(text) {
		return toolErrorResultString(ctx, CodeInvalidArguments, "keyboard_text received spaced romanization; use enter_text instead."), nil
	}

	if t.textProvider != nil {
		if err := t.textProvider.KeyboardText(ctx, text); err != nil {
			return toolErrorResultf(ctx, CodeToolExecutionFailed, "remote keyboard input failed: %v", err), nil
		}
		return "ok", nil
	}

	if t.adb != nil {
		if err := t.adb.Text(ctx, text); err != nil {
			return toolErrorResultf(ctx, adbInputToolErrorCode(err), "%v", err), nil
		}
		return "ok", nil
	}

	typeText := func() error {
		releaseReport := make([]byte, 8)
		for _, ch := range text {
			stroke, ok := mnk.KeyboardLayoutKeyStroke(t.keyboardLayout, byte(ch))
			if !ok {
				continue
			}
			report := make([]byte, 8)
			report[0] = stroke.Modifier
			report[2] = stroke.Usage
			// Press then release immediately, same as keyboard_tap.
			if err := t.dev.Write(report); err != nil {
				return err
			}
			if err := t.dev.Write(releaseReport); err != nil {
				return err
			}
		}
		return nil
	}
	var err error
	if t.iosKeyboardIsolation != nil {
		err = t.iosKeyboardIsolation.withKeyboard(ctx, keyboardTextUsesModifier(t.keyboardLayout, text), typeText)
	} else {
		err = typeText()
	}
	if err != nil {
		return toolErrorResultf(ctx, CodeToolExecutionFailed, "%v", err), nil
	}

	return "ok", nil
}

// MouseMoveTool moves the mouse to coordinates without clicking.
type MouseMoveTool struct {
	mnkProvider mnk.Provider
}

func (t *MouseMoveTool) Name() string { return "mouse_move" }

func (t *MouseMoveTool) Description() string {
	return `Move the mouse without clicking. Use normalized coordinates (0-1000) from the latest screenshot, where (500,500) is center.`
}

func (t *MouseMoveTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"x": coordinateSchema("Normalized 0-1000 X coordinate.", 500),
		"y": coordinateSchema("Normalized 0-1000 Y coordinate.", 300),
	}, "x", "y")
}

func (t *MouseMoveTool) Call(ctx context.Context, input string) (string, error) {
	adapter := mnk.NewMouseMoveToolAdapter(t.mnkProvider)
	output, err := adapter.Call(ctx, input)
	return mapMNKAdapterResult(ctx, output, err)
}

// TouchGestureTool executes touch-like pointer gestures for mobile UI control.
type TouchGestureTool struct {
	mnkProvider        mnk.Provider
	screen             *screen.ScreenState
	touchscreen        bool
	primeScreenMapping func(context.Context) error
	deviceTypeFn       func() string
	// captureFrame backs the tap_sequence screen-change check; nil disables it.
	captureFrame frameCapturer
}

func (t *TouchGestureTool) Name() string { return "touch_gesture" }

func (t *TouchGestureTool) SetDeviceTypeFunc(fn func() string) {
	if t != nil {
		t.deviceTypeFn = fn
	}
}

func (t *TouchGestureTool) Description() string {
	return `Perform touch/pointer gestures. Standard examples: {"type":"tap","point":{"x":500,"y":500}}; {"type":"swipe","start":{"x":500,"y":900},"direction":"up"}. Always include type for a standard gesture; swipe needs start and either end or direction in the SAME call. Provide either type or actions. Use the type form for normal interaction: tap, double_tap, long_press, swipe, drag_start, drag_release, or tap_sequence. Use tap_sequence {"type":"tap_sequence","points":[{"x":250,"y":320},{"x":750,"y":320}]} to tap several targets in order in one call when every target is visible in the latest screenshot and the taps do not change the layout, such as keypad digits, calculator keys, PIN pads, or several checkboxes; plan the full ordered list from the current screenshot, then verify the returned screenshot once. Do not use tap_sequence when a tap navigates, opens a menu or dialog, or moves the remaining targets; tap those one at a time. tap_sequence stops early when the screen layout changes between taps and reports executed and stop_reason in action_output. Prefer enter_text for text fields. Atomic actions are a low-frequency advanced option only for an uninterrupted custom contact sequence that cannot be expressed by those standard gestures. Do not use actions for ordinary taps, long presses, swipes, scrolling, or moving draggable UI targets. Atomic actions execute in order and keep the contact pressed until touch_up. Atomic move_to accepts speed in normalized coordinate units per second; duration_ms overrides speed, and omitting both preserves immediate movement. ` +
		`Base coordinates on the latest screenshot using normalized 0-1000 coordinates where (500,500) is center. For taps and long presses, aim at the visual center of the target. Point, start, and end never accept screenshot pixels: convert a target measured at (pixel_x,pixel_y) in the latest image with x=pixel_x/max(image_width-1,1)*1000 and y=pixel_y/max(image_height-1,1)*1000 before calling. The tool returns a post-action screenshot. Swipe direction names describe finger movement, not content scroll. ` +
		`Many scroll controls continue receiving the same gesture after the initial press, even when the pointer moves outside their visible bounds. For browsing, searching, or picker scrolling, use type:swipe with explicit start and end. Start inside the intended scrollable region near the appropriate edge, with a small margin chosen from the latest screenshot. The end may extend beyond that region along the scroll direction while remaining inside the screen with a margin from physical screen edges. Use the available travel for larger movements and shorter travel near the target. Inspect the returned screenshot to confirm that the intended control moved; behavior can vary by control. Leave timing at defaults; the tool handles acceleration, braking, and release. ` +
		`For swipe, provide start and either end or direction (up/down/left/right). Speed is normalized coordinate units per second and defaults to 2500; duration_ms may be supplied to override the calculated duration. For HID swipes starting outside the outer 1% screen border with hold_after_ms=0, duration_ms controls main motion with a 180ms minimum and excludes an additional 100ms low-speed release tail, in both absolute-mouse and touchscreen modes. hold_before_ms and hold_after_ms optionally dwell after press and before release, and steps controls HID interpolation (provider default 24). A direction-only swipe travels toward the corresponding screen edge when duration_ms is omitted, or travels speed*duration_ms/1000 normalized units when duration_ms is supplied. ` +
		`Decide whether the requested target is draggable before choosing a gesture form. When moving an app icon, card, widget, list item, or any other draggable UI target, never use actions; always use exactly this sequence: call drag_start with the target's current point. drag_start internally waits for the screen to stabilize before returning its final screenshot; do not call wait_for_stable_screen separately in the normal drag flow. Confirm screen_stable=true, then inspect that returned stable screenshot to identify and confirm the final destination point and call drag_release with it. Never determine or guess the destination from an intermediate or screen_stable=false result. When drag_start returns screen_stable=false, it has automatically moved back to the original point and released the contact; inspect the returned screenshot and retry the complete drag flow instead of calling drag_release. drag_start presses for 500ms, then moves 200 normalized units at 500 normalized units per second (a 400ms interpolated move) in a bounded direction to activate dragging, and keeps the contact down only when its internal wait succeeds. drag_release moves directly to the destination, holds for 200ms, and releases. Never use the removed drag type or perform unrelated input while drag_start is active. ` +
		`For a picker/wheel column, use type:"swipe" and read the selected CENTER value plus adjacent row ordering. A desired row above center needs finger-down (end.y > start.y); a row below needs finger-up (end.y < start.y). For values increasing downward, 08 to 07 needs finger-down. Choose each swipe's start INSIDE the intended column from the latest screenshot: near its upper inner edge for finger-down or lower inner edge for finger-up. The end may extend beyond the column while staying inside the screen. Keep X aligned with the column. Estimate remaining rows from the visible value step and use the last observed movement to scale travel: if 120 normalized units moved three rows and one remains, try about 40, not another 120. Use longer travel when many rows remain and one-row corrections near the target. Keep the requested target fixed; verify the centered value after every swipe, even when screen_changed=false. Reset movement estimates when the column or UI changes. Use default timing and omit hold_after_ms. Do not use atomic actions, drag_start, keyboard entry, or taps on picker rows for this interaction.`
}

func (t *TouchGestureTool) ArgsSchema() map[string]any {
	typeDescription := "Gesture type. tap, double_tap, long_press, drag_start, and drag_release require point. swipe requires start and either end or direction. tap_sequence requires points."
	speedSchema := numberArgSchema("Swipe speed in normalized coordinate units per second. Defaults to 2500.", 2500)
	speedSchema["exclusiveMinimum"] = 0
	schema := objectArgsSchema(map[string]any{
		"actions": map[string]any{
			"type":        "array",
			"minItems":    1,
			"maxItems":    128,
			"description": "Low-frequency advanced touch program for an uninterrupted custom contact sequence that standard gesture types cannot express. Never use actions for a draggable UI target; use drag_start, which internally waits for screen stability, determine the destination from its returned screen_stable=true screenshot, then use drag_release. A screen_stable=false drag_start is automatically canceled at its original point and must be retried instead of released. Each action is one of touch_down, move_to, wait, or touch_up. move_to and touch_down require point; wait requires ms. move_to may use speed or duration_ms for interpolated movement, with duration_ms taking precedence. A program must end with touch_up.",
			"items": objectArgsSchema(map[string]any{
				"action":      stringEnumArgSchema("Atomic action to execute.", "touch_down", "move_to", "wait", "touch_up"),
				"point":       pointSchema("Normalized point for move_to or touch_down."),
				"x":           coordinateSchema("Normalized X coordinate; use point for new programs."),
				"y":           coordinateSchema("Normalized Y coordinate; use point for new programs."),
				"ms":          rangedIntegerArgSchema("Wait duration in milliseconds.", 0, 30000),
				"duration_ms": rangedIntegerArgSchema("Optional movement duration for move_to in milliseconds.", 0, 30000),
				"speed":       speedSchema,
				"button":      stringEnumArgSchema("Pointer button held during the contact.", "left", "right", "middle"),
			}, "action"),
		},
		"type": stringEnumArgSchema(typeDescription, "tap", "double_tap", "long_press", "drag_start", "drag_release", "swipe", "tap_sequence"),
		"points": map[string]any{
			"type":        "array",
			"minItems":    mnk.MinTapSequencePoints,
			"maxItems":    mnk.MaxTapSequencePoints,
			"description": "Required for tap_sequence: the targets to tap, in order, all taken from the latest screenshot. Repeat a point to tap the same target again.",
			"items":       pointSchema(`One tap target. Must be a JSON object containing both named keys "x" and "y".`),
		},
		"interval_ms":           rangedIntegerArgSchema("Optional tap_sequence delay between taps in milliseconds. Defaults to 250.", mnk.MinTapSequenceIntervalMs, mnk.MaxTapSequenceIntervalMs),
		"stop_on_screen_change": map[string]any{"type": "boolean", "description": "Optional for tap_sequence. Defaults to true: stop the remaining taps when the screen layout changes between taps."},
		"point":                 pointSchema(`Required for tap, double_tap, long_press, drag_start, and drag_release. drag_start uses the target's current point; drag_release uses the destination confirmed from the screen_stable=true screenshot returned by drag_start after its internal stability wait. Must be a JSON object containing both named keys "x" and "y"; do not use an array, bare value, or positional shorthand.`),
		"start":                 pointSchema(`Required start point for swipe. For scrolling and pickers, choose each swipe's start inside the intended visible region from the latest screenshot. Use its upper inner edge for finger-down or lower inner edge for finger-up, with a margin from the current screenshot. Must be a JSON object containing both named keys "x" and "y"; do not use an array, bare value, or positional shorthand.`),
		"end":                   pointSchema(`End point for swipe. After starting inside the intended scrollable region, the end may extend beyond that region along the scroll direction while staying inside the screen with a small margin from physical edges. Many controls continue receiving the gesture after the initial press. Use longer travel when far from the target and shorter travel for fine adjustment; verify movement in the returned screenshot. Swipe may provide direction instead of end.`),
		"direction":             stringEnumArgSchema("Swipe direction when end is omitted. The gesture starts at start and moves toward that direction.", "up", "down", "left", "right"),
		"button":                stringEnumArgSchema("Mouse button for pointer gestures.", "left", "right", "middle"),
		"hold_ms":               nonNegativeIntegerSchema("Tap or long-press hold duration in milliseconds."),
		"speed":                 speedSchema,
		"duration_ms":           rangedIntegerArgSchema("Optional swipe main-motion duration in milliseconds. With end, it overrides timing calculated from speed; with direction, speed × duration determines travel. For HID swipes starting outside the outer 1% screen border with hold_after_ms=0, duration_ms controls main motion with a 180ms minimum and excludes an additional 100ms low-speed release tail, in both absolute-mouse and touchscreen modes.", 1, mnk.MaxSwipeDurationMs),
		"hold_before_ms":        rangedIntegerArgSchema("Optional swipe dwell after pressing and before movement.", 0, mnk.MaxSwipeHoldMs),
		"hold_after_ms":         rangedIntegerArgSchema("Optional swipe dwell at the end before release.", 0, mnk.MaxSwipeHoldMs),
		"steps":                 rangedIntegerArgSchema("Optional HID interpolation step count; larger values produce smoother motion. Defaults to the provider default (24).", 1, mnk.MaxSwipeSteps),
	})
	schema["description"] = `JSON object for one standard gesture or, rarely, an advanced atomic touch program. Provide either type or actions; a call with neither is rejected. Use type for normal interaction. Reserve actions for uninterrupted custom contact timing that tap, double_tap, long_press, swipe, drag_start, or drag_release cannot express. Never use actions to move a draggable target: call drag_start at its current point, let its internal stability wait finish, determine the destination from its returned screen_stable=true screenshot, then call drag_release at the confirmed point. A screen_stable=false drag_start automatically returns to its original point and releases; inspect its screenshot and retry drag_start rather than calling drag_release. Do not call wait_for_stable_screen separately in the normal drag flow. Unknown fields are ignored. Coordinate fields point, start, and end, and each item of points, use named objects containing both x and y. tap_sequence taps points in order in one call for targets that are all visible and do not change the layout; it stops early when the layout changes. For scrolling, start inside the intended scrollable region near its appropriate edge. Many controls continue receiving the gesture after the initial press, so the explicit end may extend beyond that region while staying inside the screen with a margin from physical edges. Use the available travel for larger movements and shorten it near the target; verify the result after every swipe and leave timing at defaults. Swipe accepts either start+end or start+direction; speed defaults to 2500 normalized coordinate units per second and duration_ms overrides calculated timing. For HID swipes starting outside the outer 1% screen border with hold_after_ms=0, duration_ms controls main motion with a 180ms minimum and excludes an additional 100ms low-speed release tail, in both absolute-mouse and touchscreen modes. hold_before_ms, hold_after_ms, and steps are optional swipe timing controls.`
	schema["examples"] = []map[string]any{
		{"type": "tap", "point": map[string]any{"x": 500, "y": 500}},
		{"type": "swipe", "start": map[string]any{"x": 500, "y": 900}, "direction": "up"},
		{"type": "swipe", "start": map[string]any{"x": 500, "y": 900}, "end": map[string]any{"x": 500, "y": 300}},
		{"type": "drag_start", "point": map[string]any{"x": 400, "y": 500}},
		{"type": "drag_release", "point": map[string]any{"x": 750, "y": 500}},
		{"type": "tap_sequence", "points": []map[string]any{{"x": 250, "y": 320}, {"x": 750, "y": 320}, {"x": 750, "y": 320}}},
	}
	return schema
}

func pointSchema(description string) map[string]any {
	schema := objectArgsSchema(map[string]any{
		"x": coordinateSchema("Normalized 0-1000 X coordinate, not a screenshot pixel X value."),
		"y": coordinateSchema("Normalized 0-1000 Y coordinate, not a screenshot pixel Y value."),
	}, "x", "y")
	schema["description"] = description + ` Coordinates always use normalized 0-1000 values, never screenshot pixels.`
	return schema
}

func (t *TouchGestureTool) Call(ctx context.Context, input string) (string, error) {
	if err := t.ensureTouchscreenMapping(ctx); err != nil {
		return toolErrorResultf(ctx, CodeToolExecutionFailed, "touchscreen mapping unavailable: %v", err), nil
	}
	adapter := mnk.NewTouchGestureToolAdapter(t.mnkProvider)
	if points, guarded := tapSequenceInput(input); guarded && t.captureFrame != nil {
		// Park the absolute-mode pointer on the first target so its hover
		// state is part of the baseline instead of a change at its old spot.
		if !t.touchscreen && t.mnkProvider != nil {
			if err := t.mnkProvider.Move(ctx, points[0].X, points[0].Y); err == nil {
				if err := sleepWithContext(ctx, tapSequencePointerParkDelay); err != nil {
					return toolErrorResultf(ctx, CodeToolExecutionFailed, "tap_sequence canceled before any tap: %v", err), nil
				}
			}
		}
		baseline, err := t.captureFrame(ctx)
		if err != nil {
			return toolErrorResultf(ctx, CodeToolExecutionFailed, "tap_sequence screen check unavailable, so no tap was sent: %v. Retry, or send the taps individually.", err), nil
		}
		adapter.WithTapSequenceGuard(newTapSequenceGuard(t.captureFrame, t.screen, baseline, points))
	}
	output, err := adapter.Call(ctx, input)
	return mapMNKAdapterResult(ctx, output, err)
}

func (t *TouchGestureTool) recoverAfterUnstableScreen(ctx context.Context, input string) error {
	var args struct {
		Type  string          `json:"type"`
		Point json.RawMessage `json:"point"`
	}
	if err := json.Unmarshal([]byte(input), &args); err != nil || !strings.EqualFold(strings.TrimSpace(args.Type), "drag_start") {
		return nil
	}
	if len(args.Point) == 0 || string(args.Point) == "null" {
		return fmt.Errorf("drag_start recovery requires the original point")
	}
	releaseInput, err := json.Marshal(struct {
		Type  string          `json:"type"`
		Point json.RawMessage `json:"point"`
	}{Type: "drag_release", Point: args.Point})
	if err != nil {
		return fmt.Errorf("encode drag_start recovery: %w", err)
	}
	_, err = mnk.NewTouchGestureToolAdapter(t.mnkProvider).Call(ctx, string(releaseInput))
	return err
}

func (t *TouchGestureTool) ensureTouchscreenMapping(ctx context.Context) error {
	if t == nil || !t.touchscreen || t.screen == nil || t.primeScreenMapping == nil {
		return nil
	}
	if t.screen.FreshActiveArea(screenDimensionsStaleAfter) {
		return nil
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("touch_gesture prime mapping before input mapping_before={%s}", t.screen.Format())
	}
	if err := t.primeScreenMapping(ctx); err != nil {
		return err
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("touch_gesture prime mapping succeeded mapping_after={%s}", t.screen.Format())
	}
	return nil
}

// MouseScrollTool sends mouse wheel events.
type MouseScrollTool struct {
	mnkProvider mnk.Provider
}

func (t *MouseScrollTool) Name() string { return "mouse_scroll" }

func (t *MouseScrollTool) Description() string {
	return `Scroll the mouse wheel. This is a wheel event, not equivalent to a mobile swipe gesture; use touch_gesture for swipes. Unsupported when pointer_mode is touchscreen.`
}

func (t *MouseScrollTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"delta": rangedIntegerArgSchema("Mouse wheel delta. Positive scrolls up, negative scrolls down.", -127, 127),
	}, "delta")
}

func (t *MouseScrollTool) Call(ctx context.Context, input string) (string, error) {
	adapter := mnk.NewMouseScrollToolAdapter(t.mnkProvider)
	output, err := adapter.Call(ctx, input)
	return mapMNKAdapterResult(ctx, output, err)
}

// mapMNKAdapterResult converts mnk adapter failures into the agent tool contract:
// structured ToolError via toolErrorResult*, with a nil Go error. This keeps
// executeToolCall / quick_action.delegate on the recoverable observation path.
func mapMNKAdapterResult(ctx context.Context, output string, err error) (string, error) {
	if err == nil {
		return output, nil
	}
	if mnkErr := mnk.AsError(err); mnkErr != nil {
		switch mnkErr.Kind {
		case mnk.ErrInvalidArguments:
			return toolErrorResultString(ctx, CodeInvalidArguments, mnkErr.Error()), nil
		case mnk.ErrModuleUnavailable:
			return toolErrorResultString(ctx, CodeModuleUnavailable, mnkErr.Error()), nil
		case mnk.ErrExecutionFailed:
			return toolErrorResultString(ctx, CodeToolExecutionFailed, mnkErr.Error()), nil
		}
	}
	return toolErrorResultf(ctx, CodeToolExecutionFailed, "%v", err), nil
}

// writeAbsMouseReport writes an absolute mouse report:
// [buttons, x_lo, x_hi, y_lo, y_hi, wheel].
func writeAbsMouseReport(dev *mnk.HIDDevice, state *mnk.PointerState, x, y int, buttons uint8, wheel int8) error {
	report, absX, absY := mnk.AbsMouseReport(x, y, buttons, wheel)
	if err := dev.Write(report); err != nil {
		return err
	}
	state.Update(absX, absY)
	return nil
}

// writeTouchscreenReport writes a single-contact touch report:
// [flags, contact_id, x_lo, x_hi, y_lo, y_hi]. flags bit0=tip switch, bit1=in range.
func writeTouchscreenReport(dev *mnk.HIDDevice, state *mnk.PointerState, x, y int, touching bool) error {
	report, absX, absY := mnk.TouchscreenReport(x, y, touching)

	path := "<nil>"
	if dev != nil {
		path = dev.Path()
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf(
			"hid touchscreen report write start path=%s requested=(%d,%d) clamped=(%d,%d) touching=%v flags=0x%02x report=% x",
			path,
			x,
			y,
			absX,
			absY,
			touching,
			report[0],
			report,
		)
	}
	err := dev.Write(report)
	if err != nil {
		if touchscreenRCADebugEnabledCached() {
			touchscreenRCALogf("hid touchscreen report write error path=%s clamped=(%d,%d) touching=%v err=%v", path, absX, absY, touching, err)
		}
		return err
	}
	state.Update(absX, absY)
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("hid touchscreen report write ok path=%s clamped=(%d,%d) touching=%v", path, absX, absY, touching)
	}
	return nil
}

func (pc *pointerController) moveTo(x, y int, buttons uint8) error {
	if pc.touchscreen {
		return writeTouchscreenReport(pc.dev, pc.state, x, y, buttons != 0)
	}
	return writeAbsMouseReport(pc.dev, pc.state, x, y, buttons, 0)
}

type pointerPoint struct {
	X pointerCoordinate `json:"x"`
	Y pointerCoordinate `json:"y"`
}

func (p *pointerPoint) UnmarshalJSON(data []byte) error {
	// Accept array format [x, y] as fallback for models that pass coordinates as arrays.
	var arr []json.RawMessage
	if json.Unmarshal(data, &arr) == nil && len(arr) == 2 {
		if err := json.Unmarshal(arr[0], &p.X); err != nil {
			return fmt.Errorf("coordinate must be a number or numeric string")
		}
		if err := json.Unmarshal(arr[1], &p.Y); err != nil {
			return fmt.Errorf("coordinate must be a number or numeric string")
		}
		return nil
	}
	// Normal object format {"x":N,"y":M}
	type plain pointerPoint
	return decodeStrictJSONObject(string(data), (*plain)(p))
}

type pointerCoordinate float64

func (c *pointerCoordinate) UnmarshalJSON(data []byte) error {
	var number float64
	if err := json.Unmarshal(data, &number); err == nil {
		return c.setFinite(number)
	}

	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		value, parseErr := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if parseErr != nil {
			return fmt.Errorf("parse coordinate %q: %w", text, parseErr)
		}
		return c.setFinite(value)
	}

	return fmt.Errorf("coordinate must be a number or numeric string")
}

func (c *pointerCoordinate) setFinite(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("coordinate must be a finite number")
	}
	*c = pointerCoordinate(value)
	return nil
}

func (c pointerCoordinate) Float64() float64 {
	return float64(c)
}

type resolvedPointerPoint struct {
	x int
	y int
}

func tapPointer(pc *pointerController, x, y int, button uint8) error {
	return tapPointerWithHold(pc, x, y, button, defaultTapHoldMs)
}

func tapPointerWithHold(pc *pointerController, x, y int, button uint8, holdMs int) error {
	touchscreenRCALogf("tapPointerWithHold start pointer_mode=%s absolute=(%d,%d) button=0x%02x hold_ms=%d", touchscreenRCAPointerMode(pc), x, y, button, holdMs)
	if err := settlePointer(pc, x, y); err != nil {
		return err
	}
	if err := pressPointer(pc, x, y, button); err != nil {
		return err
	}
	sleepMs(holdMs)
	err := releasePointerRepeated(pc, x, y, touchReleaseReportCount, touchReleaseReportDelayMs)
	if err != nil {
		touchscreenRCALogf("tapPointerWithHold error pointer_mode=%s absolute=(%d,%d) err=%v", touchscreenRCAPointerMode(pc), x, y, err)
		return err
	}
	touchscreenRCALogf("tapPointerWithHold completed pointer_mode=%s absolute=(%d,%d)", touchscreenRCAPointerMode(pc), x, y)
	return nil
}

func settlePointer(pc *pointerController, x, y int) error {
	if pc.touchscreen {
		return nil
	}
	if err := movePointer(pc, x, y, 0); err != nil {
		return err
	}
	sleepMs(defaultCursorSettleMs)
	return nil
}

func pressPointer(pc *pointerController, x, y int, button uint8) error {
	return movePointer(pc, x, y, button)
}

func releasePointer(pc *pointerController, x, y int) error {
	return movePointer(pc, x, y, 0)
}

func releasePointerRepeated(pc *pointerController, x, y int, count int, delayMs int) error {
	if count < 1 {
		count = 1
	}

	var firstErr error
	released := false
	for i := 0; i < count; i++ {
		if err := releasePointer(pc, x, y); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			released = true
		}
		if i+1 < count {
			sleepMs(delayMs)
		}
	}
	if released {
		return nil
	}
	return firstErr
}

func movePointer(pc *pointerController, x, y int, buttons uint8) error {
	return pc.moveTo(x, y, buttons)
}

func dragPointer(pc *pointerController, start, end resolvedPointerPoint, button uint8, durationMs, holdBeforeMs, holdAfterMs, steps int) (dragErr error) {
	if steps < 1 {
		steps = 1
	}
	if durationMs < 0 {
		durationMs = 0
	}
	if holdBeforeMs < 0 {
		holdBeforeMs = 0
	}
	if holdAfterMs < 0 {
		holdAfterMs = 0
	}

	touchscreenRCALogf(
		"dragPointer start pointer_mode=%s start_abs=(%d,%d) end_abs=(%d,%d) button=0x%02x duration_ms=%d hold_before_ms=%d hold_after_ms=%d steps=%d",
		touchscreenRCAPointerMode(pc),
		start.x,
		start.y,
		end.x,
		end.y,
		button,
		durationMs,
		holdBeforeMs,
		holdAfterMs,
		steps,
	)
	if err := settlePointer(pc, start.x, start.y); err != nil {
		return err
	}
	if err := pressPointer(pc, start.x, start.y, button); err != nil {
		return err
	}

	defer func() {
		relErr := releasePointerRepeated(pc, end.x, end.y, touchReleaseReportCount, touchReleaseReportDelayMs)
		if dragErr == nil {
			dragErr = relErr
		}
	}()

	sleepMs(holdBeforeMs)

	stepDelay := 0
	if steps > 0 {
		stepDelay = durationMs / steps
	}
	for i := 1; i <= steps; i++ {
		progress := float64(i) / float64(steps)
		x := interpolateInt(start.x, end.x, progress)
		y := interpolateInt(start.y, end.y, progress)
		if err := movePointer(pc, x, y, button); err != nil {
			return err
		}
		if i < steps {
			sleepMs(stepDelay)
		}
	}

	sleepMs(holdAfterMs)
	touchscreenRCALogf("dragPointer completed pointer_mode=%s start_abs=(%d,%d) end_abs=(%d,%d)", touchscreenRCAPointerMode(pc), start.x, start.y, end.x, end.y)
	return nil
}

func interpolateInt(start, end int, progress float64) int {
	return int(math.Round(float64(start) + (float64(end-start) * progress)))
}

var sleepMs = realSleepMs

func realSleepMs(ms int) {
	if ms <= 0 {
		return
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

func mouseButtonByte(button string) uint8 {
	switch strings.ToLower(strings.TrimSpace(button)) {
	case "right":
		return 0x02
	case "middle":
		return 0x04
	default:
		return 0x01 // left
	}
}

func parseKeyboardTextInput(input string) (string, string) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", "text is required"
	}

	if strings.HasPrefix(trimmed, "{") {
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return "", fmt.Sprintf("invalid input: %v", err)
		}
		if args.Text == "" {
			return "", "text is required"
		}
		return args.Text, ""
	}

	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if err := json.Unmarshal([]byte(trimmed), &text); err != nil {
			return "", fmt.Sprintf("invalid input: %v", err)
		}
		if text == "" {
			return "", "text is required"
		}
		return text, ""
	}

	return trimmed, ""
}

func unsupportedKeyboardTextRunes(layout, text string) []rune {
	unsupported := make([]rune, 0)
	for _, ch := range text {
		if ch > 0x7F {
			unsupported = append(unsupported, ch)
			continue
		}
		if _, ok := mnk.KeyboardLayoutKeyStroke(layout, byte(ch)); !ok {
			unsupported = append(unsupported, ch)
		}
	}
	return unsupported
}
