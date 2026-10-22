package agent

import (
	"aiden-agent/internal/agent/mnk"
	"aiden-agent/internal/agent/screen"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestResolvePointerPositionNormalized(t *testing.T) {
	x, y, err := resolvePointerPosition(nil, 500, 250)
	if err != nil {
		t.Fatalf("resolvePointerPosition returned error: %v", err)
	}
	if x != 16384 {
		t.Fatalf("x = %d, want 16384", x)
	}
	if y != 8192 {
		t.Fatalf("y = %d, want 8192", y)
	}
}

func TestToolSetUpdateDeviceEnvironmentTracksPhoneScreenInfo(t *testing.T) {
	tools := NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{})
	env := &PhoneEnvironment{
		Screen: screen.PhoneScreenInfo{
			WidthPixels:        intPtr(1080),
			HeightPixels:       intPtr(1920),
			NativeWidthPixels:  intPtr(1080),
			NativeHeightPixels: intPtr(1920),
		},
	}

	tools.UpdateDeviceEnvironment(env)

	screen := tools.screen.PhoneScreenInfo()
	if screen.WidthPixels == nil || screen.HeightPixels == nil {
		t.Fatalf("screen info = %+v, want width/height pixels", screen)
	}
	if *screen.WidthPixels != 1080 || *screen.HeightPixels != 1920 {
		t.Fatalf("screen pixels = %v x %v, want 1080 x 1920", *screen.WidthPixels, *screen.HeightPixels)
	}

	tools.UpdateDeviceEnvironment(nil)

	screen = tools.screen.PhoneScreenInfo()
	if screen.WidthPixels != nil || screen.HeightPixels != nil || screen.NativeWidthPixels != nil || screen.NativeHeightPixels != nil || screen.Width != nil || screen.Height != nil {
		t.Fatalf("screen info should be cleared, got %+v", screen)
	}
}

func TestBuiltinToolSetWiresConfiguredKeyboardLayout(t *testing.T) {
	tools := NewBuiltinToolSet(HIDConfig{KeyboardLayout: mnk.KeyboardLayoutAZERTY}, AudioConfig{}, SearchConfig{}, ProxyConfig{})
	keyboardText, ok := tools.textInputHW.keyboardText.(*KeyboardTextTool)
	if !ok {
		t.Fatalf("keyboardText = %T, want *KeyboardTextTool", tools.textInputHW.keyboardText)
	}
	if got := keyboardText.keyboardLayout; got != mnk.KeyboardLayoutAZERTY {
		t.Fatalf("keyboard_text layout = %q, want %q", got, mnk.KeyboardLayoutAZERTY)
	}
	if _, ok := tools.textInputHW.keyboardTap.(*KeyboardTapTool); !ok {
		t.Fatalf("keyboardTap = %T, want *KeyboardTapTool", tools.textInputHW.keyboardTap)
	}
}

func TestBuiltinToolSetRegistersExpectedTools(t *testing.T) {
	tools := NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{})
	want := []string{
		"audio_volume",
		"keyboard_tap",
		"mouse_move",
		"mouse_scroll",
		"quick_action",
		"request_user_action",
		"screenshot",
		"shell",
		"touch_gesture",
		"wait_for_stable_screen",
		"weather",
		"web_scraper",
		"web_search",
		"wikipedia",
	}

	if got := tools.Names(); !slices.Equal(got, want) {
		t.Fatalf("built-in tools = %v, want %v", got, want)
	}
}

func TestHIDToolsExposeStructuredSchemas(t *testing.T) {
	for name, tool := range map[string]structuredInputTool{
		"keyboard_tap":  &KeyboardTapTool{},
		"keyboard_text": &KeyboardTextTool{},
		"mouse_move":    &MouseMoveTool{},
		"mouse_scroll":  &MouseScrollTool{},
		"touch_gesture": &TouchGestureTool{},
	} {
		schema := tool.ArgsSchema()
		props, ok := schema["properties"].(map[string]any)
		if !ok || len(props) == 0 {
			t.Fatalf("%s missing schema properties: %#v", name, schema)
		}
	}
}

func TestTouchGestureRejectsRemovedDragType(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"drag","start":{"x":500,"y":500},"end":{"x":500.001,"y":500.001}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, `unsupported gesture type: "drag"`) {
		t.Fatalf("Call output = %q, want removed drag rejection", out)
	}
	if reports := readMouseReports(t, dev, path); len(reports) != 0 {
		t.Fatalf("removed drag wrote %d HID reports", len(reports))
	}
}

func TestTouchGestureTouchscreenPrimesMappingBeforeNormalizedInput(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	screenState := &screen.ScreenState{}
	primeCalls := 0
	tool := testTouchGestureTool(t, testMNKOpts{
		screenState: screenState,
		pointer:     dev,
		touchscreen: true,
		primeScreenMapping: func(context.Context) error {
			primeCalls++
			screenState.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 711, Y: 0, Width: 497, Height: 1080, Valid: true})
			return nil
		},
	})

	out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":931,"y":83}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}
	if primeCalls != 1 {
		t.Fatalf("prime calls = %d, want 1", primeCalls)
	}

	reports := readTouchscreenReports(t, dev, path)
	if len(reports) != 1+touchReleaseReportCount {
		t.Fatalf("len(reports) = %d, want %d", len(reports), 1+touchReleaseReportCount)
	}
	expectedX := scalePixelToAbsolute(711+(931.0/1000.0)*496, 1920)
	expectedY := scalePixelToAbsolute((83.0/1000.0)*1079, 1080)
	if reports[0].x != uint16(expectedX) || reports[0].y != uint16(expectedY) {
		t.Fatalf("first report = (%d,%d), want (%d,%d)", reports[0].x, reports[0].y, expectedX, expectedY)
	}
	fallbackX, fallbackY := normalizedToAbsolutePoint(931, 83)
	if reports[0].x == uint16(fallbackX) && reports[0].y == uint16(fallbackY) {
		t.Fatalf("first report used fallback coordinates (%d,%d)", reports[0].x, reports[0].y)
	}
}

func TestTouchGestureTouchscreenDoesNotWriteWhenMappingPrimeFails(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{
		screenState: &screen.ScreenState{},
		pointer:     dev,
		touchscreen: true,
		primeScreenMapping: func(context.Context) error {
			return errors.New("frame service recovering")
		},
	})

	out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":931,"y":83}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, "touchscreen mapping unavailable") {
		t.Fatalf("Call output = %q, want mapping error", out)
	}
	if reports := readTouchscreenReports(t, dev, path); len(reports) != 0 {
		t.Fatalf("len(reports) = %d, want no HID writes", len(reports))
	}
}

func TestTouchGestureTouchscreenKeepsFreshFullFrameMappingWhenPrimeWouldFail(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	screenState := &screen.ScreenState{}
	screenState.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{})
	primeCalls := 0
	tool := testTouchGestureTool(t, testMNKOpts{
		screenState: screenState,
		pointer:     dev,
		touchscreen: true,
		primeScreenMapping: func(context.Context) error {
			primeCalls++
			return errors.New("frame service unavailable")
		},
	})

	out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":931,"y":83}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}
	if primeCalls != 0 {
		t.Fatalf("prime calls = %d, want 0", primeCalls)
	}

	reports := readTouchscreenReports(t, dev, path)
	if len(reports) != 1+touchReleaseReportCount {
		t.Fatalf("len(reports) = %d, want %d", len(reports), 1+touchReleaseReportCount)
	}
	expectedX := scalePixelToAbsolute((931.0/1000.0)*1919, 1920)
	expectedY := scalePixelToAbsolute((83.0/1000.0)*1079, 1080)
	if reports[0].x != uint16(expectedX) || reports[0].y != uint16(expectedY) {
		t.Fatalf("first report = (%d,%d), want (%d,%d)", reports[0].x, reports[0].y, expectedX, expectedY)
	}
}

func TestTouchGestureSchemaDoesNotExposeWheelMetadata(t *testing.T) {
	schema := (&TouchGestureTool{}).ArgsSchema()
	props := schema["properties"].(map[string]any)
	if _, ok := props["wheel"]; ok {
		t.Fatalf("touch_gesture must remain generic and must not expose wheel metadata: %#v", props)
	}
}

func TestTouchGestureDescriptionRequiresInternalStableWaitThenReleaseDragFlow(t *testing.T) {
	description := (&TouchGestureTool{}).Description()
	for _, want := range []string{
		"Atomic actions are a low-frequency advanced option",
		"Do not use actions for ordinary taps, long presses, swipes, scrolling, or moving draggable UI targets",
		"Decide whether the requested target is draggable before choosing a gesture form",
		"When moving an app icon, card, widget, list item, or any other draggable UI target, never use actions",
		"call drag_start with the target's current point",
		"drag_start internally waits for the screen to stabilize before returning its final screenshot",
		"do not call wait_for_stable_screen separately in the normal drag flow",
		"Confirm screen_stable=true",
		"call drag_release with it",
		"Never determine or guess the destination from an intermediate or screen_stable=false result",
		"When drag_start returns screen_stable=false, it has automatically moved back to the original point and released the contact",
		"retry the complete drag flow instead of calling drag_release",
		"drag_start presses for 500ms, then moves 200 normalized units at 500 normalized units per second (a 400ms interpolated move)",
		"Never use the removed drag type",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("touch_gesture description missing %q: %s", want, description)
		}
	}
}

func TestTouchGestureSchemaKeepsAtomicActionsExceptional(t *testing.T) {
	schema := (&TouchGestureTool{}).ArgsSchema()
	description, _ := schema["description"].(string)
	for _, want := range []string{
		"Use type for normal interaction",
		"Reserve actions for uninterrupted custom contact timing",
		"Never use actions to move a draggable target",
		"let its internal stability wait finish",
		"returned screen_stable=true screenshot",
		"A screen_stable=false drag_start automatically returns to its original point and releases",
		"Do not call wait_for_stable_screen separately in the normal drag flow",
	} {
		if !strings.Contains(description, want) {
			t.Fatalf("touch_gesture schema description missing %q: %s", want, description)
		}
	}

	properties := schema["properties"].(map[string]any)
	actions := properties["actions"].(map[string]any)
	actionsDescription, _ := actions["description"].(string)
	for _, want := range []string{"Low-frequency advanced touch program", "Never use actions for a draggable UI target", "internally waits for screen stability", "returned screen_stable=true screenshot"} {
		if !strings.Contains(actionsDescription, want) {
			t.Fatalf("touch_gesture actions description missing %q: %s", want, actionsDescription)
		}
	}
	if strings.Contains(actionsDescription, "Preferred atomic") {
		t.Fatalf("touch_gesture actions must not be described as preferred: %s", actionsDescription)
	}

	point := properties["point"].(map[string]any)
	pointDescription, _ := point["description"].(string)
	if !strings.Contains(pointDescription, "screen_stable=true screenshot returned by drag_start after its internal stability wait") {
		t.Fatalf("touch_gesture point description must require a stable drag destination: %s", pointDescription)
	}
}

func TestKeyboardTapSchemaRequiresKeysArray(t *testing.T) {
	schema := (&KeyboardTapTool{}).ArgsSchema()
	props := schema["properties"].(map[string]any)
	keys := props["keys"].(map[string]any)
	if keys["type"] != "array" {
		t.Fatalf("keys schema type = %#v, want array", keys["type"])
	}
	items := keys["items"].(map[string]any)
	if items["type"] != "string" {
		t.Fatalf("keys items type = %#v, want string", items["type"])
	}
}

func TestTouchGestureSchemaRequiresNamedPointCoordinates(t *testing.T) {
	schema := (&TouchGestureTool{}).ArgsSchema()
	props := schema["properties"].(map[string]any)
	point := props["point"].(map[string]any)
	if point["type"] != "object" {
		t.Fatalf("point type = %#v, want object", point["type"])
	}
	pointProps := point["properties"].(map[string]any)
	if _, ok := pointProps["x"]; !ok {
		t.Fatalf("point schema missing x: %#v", pointProps)
	}
	if _, ok := pointProps["y"]; !ok {
		t.Fatalf("point schema missing y: %#v", pointProps)
	}
}

type recordingADBRunner struct {
	commands [][]string
	timeouts []time.Duration
	ctxErrs  []error
	handler  func(args []string) (string, error)
}

func (r *recordingADBRunner) run(ctx context.Context, _ string, args ...string) ([]byte, []byte, error) {
	copied := append([]string(nil), args...)
	r.commands = append(r.commands, copied)
	r.ctxErrs = append(r.ctxErrs, ctx.Err())
	if deadline, ok := ctx.Deadline(); ok {
		r.timeouts = append(r.timeouts, time.Until(deadline))
	} else {
		r.timeouts = append(r.timeouts, 0)
	}
	if r.handler == nil {
		return nil, nil, nil
	}
	out, err := r.handler(copied)
	if err != nil {
		return []byte(out), nil, err
	}
	return []byte(out), nil, nil
}

func newTestADBInputController(t *testing.T, screenState *screen.ScreenState, runner *recordingADBRunner) *ADBInputController {
	t.Helper()
	t.Setenv("AIDEN_ADB_PATH", "/fake/adb")
	t.Setenv("AIDEN_ADB_SERIAL", "serial123")
	if screenState == nil {
		screenState = &screen.ScreenState{}
	}
	return &ADBInputController{
		screen: screenState,
		client: NewADBScreenClient(),
		runADB: runner.run,
	}
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSliceMatrixEqual(a, b [][]string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !stringSlicesEqual(a[i], b[i]) {
			return false
		}
	}
	return true
}

func TestADBTouchGestureTapAutoRejectsOutOfRangeCoordinates(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdatePhoneScreenInfo(screen.PhoneScreenInfo{WidthPixels: intPtr(1080), HeightPixels: intPtr(2400)})
	runner := &recordingADBRunner{}
	tool := testTouchGestureTool(t, testMNKOpts{screenState: screenState, adbRunner: runner})

	out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":1500,"y":500}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, "normalized 0-1000 scale") {
		t.Fatalf("Call output = %q, want normalized coordinate error", out)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("adb commands = %#v, want no command for out-of-range normalized coordinates", runner.commands)
	}
}

func TestPointerResolversRejectNonFiniteCoordinates(t *testing.T) {
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, _, err := resolvePointerPositionForSurface(nil, false, value, 500); err == nil || !strings.Contains(err.Error(), "finite") {
			t.Fatalf("resolvePointerPositionForSurface(%v, 500) error = %v, want finite-coordinate error", value, err)
		}
		controller := &ADBInputController{}
		if _, err := controller.ResolvePosition(context.Background(), value, 500); err == nil || !strings.Contains(err.Error(), "finite") {
			t.Fatalf("ResolvePosition(%v, 500) error = %v, want finite-coordinate error", value, err)
		}
	}
}

func TestADBTouchGestureSwipeUsesContinuousMotion(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdatePhoneScreenInfo(screen.PhoneScreenInfo{WidthPixels: intPtr(1001), HeightPixels: intPtr(1001)})
	runner := &recordingADBRunner{}
	tool := testTouchGestureTool(t, testMNKOpts{screenState: screenState, adbRunner: runner})

	out, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":100,"y":900},"end":{"x":900,"y":100}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	if len(runner.commands) != 2 {
		t.Fatalf("adb commands = %#v, want discovery and one motion program", runner.commands)
	}
	script := runner.commands[1][len(runner.commands[1])-1]
	for _, want := range []string{"input touchscreen motionevent DOWN 100 900", "input touchscreen motionevent MOVE", "input touchscreen motionevent UP 900 100"} {
		if !strings.Contains(script, want) {
			t.Fatalf("motion script missing %q: %s", want, script)
		}
	}
}

func TestADBTouchGestureSwipeRejectsSameResolvedPoint(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdatePhoneScreenInfo(screen.PhoneScreenInfo{WidthPixels: intPtr(1001), HeightPixels: intPtr(1001)})
	runner := &recordingADBRunner{}
	tool := testTouchGestureTool(t, testMNKOpts{screenState: screenState, adbRunner: runner})
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{"type":"swipe","start":{"x":500,"y":500},"end":{"x":500,"y":500}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, "swipe requires distinct start and end points") {
		t.Fatalf("Call output = %q, want same-point error", out)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeInvalidArguments || got.Message != out {
		t.Fatalf("ToolError = %+v, want invalid_arguments with output message", got)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("adb commands = %#v, want no swipe command for identical points", runner.commands)
	}
}

func TestADBTouchGestureLongPressExtendsCommandTimeout(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdatePhoneScreenInfo(screen.PhoneScreenInfo{WidthPixels: intPtr(1001), HeightPixels: intPtr(1001)})
	runner := &recordingADBRunner{}
	tool := testTouchGestureTool(t, testMNKOpts{screenState: screenState, adbRunner: runner})

	out, err := tool.Call(context.Background(), `{"type":"long_press","point":{"x":50,"y":50},"hold_ms":9000}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	want := []string{"-s", "serial123", "shell", "input", "swipe", "50", "50", "50", "50", "9000"}
	if len(runner.commands) != 1 || !stringSlicesEqual(runner.commands[0], want) {
		t.Fatalf("adb commands = %#v, want %#v", runner.commands, want)
	}
	if len(runner.timeouts) != 1 {
		t.Fatalf("adb timeouts = %#v, want one timeout", runner.timeouts)
	}
	if runner.timeouts[0] < 11*time.Second {
		t.Fatalf("adb timeout = %v, want at least 11s for 9s gesture", runner.timeouts[0])
	}
}

func TestADBKeyboardTapAndroidAliasesAlignWithBridge(t *testing.T) {
	tests := []struct {
		name string
		keys []string
		want string
	}{
		{name: "home", keys: []string{"home"}, want: "KEYCODE_MOVE_HOME"},
		{name: "keycode home", keys: []string{"KEYCODE_HOME"}, want: "KEYCODE_HOME"},
		{name: "escape", keys: []string{"escape"}, want: "KEYCODE_ESCAPE"},
		{name: "return", keys: []string{"return"}, want: "KEYCODE_ENTER"},
		{name: "delete backward", keys: []string{"delete_backward"}, want: "KEYCODE_DEL"},
		{name: "app switch", keys: []string{"keycode_app_switch"}, want: "KEYCODE_APP_SWITCH"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runner := &recordingADBRunner{}
			tool := testKeyboardTapTool(t, testMNKOpts{adbRunner: runner})

			payload, err := json.Marshal(map[string]any{"keys": tt.keys})
			if err != nil {
				t.Fatal(err)
			}
			out, err := tool.Call(context.Background(), string(payload))
			if err != nil {
				t.Fatalf("Call returned error: %v", err)
			}
			if out != "ok" {
				t.Fatalf("Call output = %q, want ok", out)
			}
			want := []string{"-s", "serial123", "shell", "input", "keyevent", tt.want}
			if len(runner.commands) != 1 || !stringSlicesEqual(runner.commands[0], want) {
				t.Fatalf("adb commands = %#v, want %#v", runner.commands, want)
			}
		})
	}
}

func TestADBKeyboardTapUsesKeyCombinationForChords(t *testing.T) {
	runner := &recordingADBRunner{handler: func(args []string) (string, error) {
		if strings.Join(args, " ") == "-s serial123 shell getprop ro.build.version.sdk" {
			return "31", nil
		}
		return "", nil
	}}
	tool := testKeyboardTapTool(t, testMNKOpts{adbRunner: runner})

	out, err := tool.Call(context.Background(), `{"keys":["ctrl","c"]}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	want := [][]string{
		{"-s", "serial123", "shell", "getprop", "ro.build.version.sdk"},
		{"-s", "serial123", "shell", "input", "keycombination", "-t", "50", "KEYCODE_CTRL_LEFT", "KEYCODE_C"},
	}
	if !stringSliceMatrixEqual(runner.commands, want) {
		t.Fatalf("adb commands = %#v, want %#v", runner.commands, want)
	}
}

func TestADBKeyboardTextUsesADBKeyboardBroadcastAndRestoresIME(t *testing.T) {
	origSleep := sleepMs
	sleepMs = func(int) {}
	defer func() { sleepMs = origSleep }()

	runner := &recordingADBRunner{handler: func(args []string) (string, error) {
		if strings.Join(args, " ") == "-s serial123 shell settings get secure default_input_method" {
			return "com.example/.Keyboard", nil
		}
		return "", nil
	}}
	tool := &KeyboardTextTool{adb: newTestADBInputController(t, nil, runner)}

	out, err := tool.Call(context.Background(), `{"text":"Hello!"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	encoded := base64.StdEncoding.EncodeToString([]byte("Hello!"))
	want := [][]string{
		{"-s", "serial123", "shell", "settings", "get", "secure", "default_input_method"},
		{"-s", "serial123", "shell", "ime", "set", adbKeyboardIME},
		{"-s", "serial123", "shell", "am", "broadcast", "-a", "ADB_INPUT_B64", "--es", "msg", encoded},
		{"-s", "serial123", "shell", "ime", "set", "com.example/.Keyboard"},
	}
	if !stringSliceMatrixEqual(runner.commands, want) {
		t.Fatalf("adb commands = %#v, want %#v", runner.commands, want)
	}
}

func TestADBKeyboardTextRestoresIMEAfterCallerContextCanceled(t *testing.T) {
	origSleep := sleepMs
	sleepMs = func(int) {}
	defer func() { sleepMs = origSleep }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	runner := &recordingADBRunner{handler: func(args []string) (string, error) {
		switch strings.Join(args, " ") {
		case "-s serial123 shell settings get secure default_input_method":
			return "com.example/.Keyboard", nil
		case "-s serial123 shell am broadcast -a ADB_INPUT_B64 --es msg " + base64.StdEncoding.EncodeToString([]byte("Hello")):
			cancel()
		}
		return "", nil
	}}
	tool := &KeyboardTextTool{adb: newTestADBInputController(t, nil, runner)}

	out, err := tool.Call(ctx, `{"text":"Hello"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	wantRestore := []string{"-s", "serial123", "shell", "ime", "set", "com.example/.Keyboard"}
	if len(runner.commands) != 4 || !stringSlicesEqual(runner.commands[3], wantRestore) {
		t.Fatalf("adb commands = %#v, want final restore command %#v", runner.commands, wantRestore)
	}
	if runner.ctxErrs[3] != nil {
		t.Fatalf("restore ctx err = %v, want independent uncanceled context", runner.ctxErrs[3])
	}
}

func TestADBKeyboardTextFallsBackToKeyEventsWhenADBKeyboardUnavailable(t *testing.T) {
	runner := &recordingADBRunner{handler: func(args []string) (string, error) {
		if strings.Join(args, " ") == "-s serial123 shell settings get secure default_input_method" {
			return "com.example/.Keyboard", nil
		}
		if strings.Join(args, " ") == "-s serial123 shell ime set "+adbKeyboardIME {
			return "Unknown input method", errors.New("exit status 1")
		}
		return "", nil
	}}
	tool := &KeyboardTextTool{adb: newTestADBInputController(t, nil, runner)}

	out, err := tool.Call(context.Background(), `{"text":"A z!"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	want := [][]string{
		{"-s", "serial123", "shell", "settings", "get", "secure", "default_input_method"},
		{"-s", "serial123", "shell", "ime", "set", adbKeyboardIME},
		{"-s", "serial123", "shell", "input", "keycombination", "-t", "50", "KEYCODE_SHIFT_LEFT", "KEYCODE_A"},
		{"-s", "serial123", "shell", "input", "keyevent", "KEYCODE_SPACE"},
		{"-s", "serial123", "shell", "input", "keyevent", "KEYCODE_Z"},
		{"-s", "serial123", "shell", "input", "keycombination", "-t", "50", "KEYCODE_SHIFT_LEFT", "KEYCODE_1"},
	}
	if !stringSliceMatrixEqual(runner.commands, want) {
		t.Fatalf("adb commands = %#v, want %#v", runner.commands, want)
	}
}

func TestADBMouseMoveRejectsUnsupportedAfterCoordinateValidation(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdatePhoneScreenInfo(screen.PhoneScreenInfo{WidthPixels: intPtr(1080), HeightPixels: intPtr(2400)})
	runner := &recordingADBRunner{}
	tool := testMouseMoveTool(t, testMNKOpts{screenState: screenState, adbRunner: runner})
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{"x":500,"y":250}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, "adb mouse_move is unsupported") {
		t.Fatalf("Call output = %q, want unsupported adb mouse_move error", out)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeModuleUnavailable || got.Message != out {
		t.Fatalf("ToolError = %+v, want module_unavailable with output message", got)
	}
	if len(runner.commands) != 0 {
		t.Fatalf("adb commands = %#v, want no command for unsupported mouse_move", runner.commands)
	}
}

func TestADBMouseScrollUsesSwipeApproximation(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdatePhoneScreenInfo(screen.PhoneScreenInfo{WidthPixels: intPtr(1001), HeightPixels: intPtr(1001)})
	runner := &recordingADBRunner{}
	tool := testMouseScrollTool(t, testMNKOpts{screenState: screenState, adbRunner: runner})

	out, err := tool.Call(context.Background(), `{"delta":-3}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}
	want := []string{"-s", "serial123", "shell", "input", "swipe", "500", "750", "500", "250", "650"}
	if len(runner.commands) != 1 || !stringSlicesEqual(runner.commands[0], want) {
		t.Fatalf("adb commands = %#v, want %#v", runner.commands, want)
	}
}

func TestParseADBWMSizePrefersOverrideSize(t *testing.T) {
	size, ok := parseADBWMSize("Physical size: 1080x2400\nOverride size: 720x1600\n")
	if !ok {
		t.Fatal("parseADBWMSize ok = false")
	}
	if size.width != 720 || size.height != 1600 {
		t.Fatalf("size = %+v, want 720x1600", size)
	}
}

func TestResolvePointerPositionNormalizedUsesActiveArea(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 656, Y: 0, Width: 608, Height: 1080, Valid: true})

	x, y, err := resolvePointerPosition(screenState, 0, 500)
	if err != nil {
		t.Fatalf("resolvePointerPosition returned error: %v", err)
	}
	if x != 0 {
		t.Fatalf("x = %d, want 0 at left edge of active_area", x)
	}
	wantY := scalePixelToAbsolute(float64(1079)/2, 1080)
	if y != wantY {
		t.Fatalf("y = %d, want %d", y, wantY)
	}
}

func TestResolvePointerPositionTouchscreenNormalizedUsesFrameSpaceWithinActiveArea(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 656, Y: 0, Width: 608, Height: 1080, Valid: true})

	x, y, err := resolvePointerPositionForSurface(screenState, true, 627, 180)
	if err != nil {
		t.Fatalf("resolvePointerPositionForSurface returned error: %v", err)
	}

	wantX := scalePixelToAbsolute(float64(656)+(627.0/1000.0)*float64(608-1), 1920)
	wantY := scalePixelToAbsolute((180.0/1000.0)*float64(1080-1), 1080)
	if x != wantX {
		t.Fatalf("x = %d, want %d", x, wantX)
	}
	if y != wantY {
		t.Fatalf("y = %d, want %d", y, wantY)
	}
}

func TestNormalizedCoordinatesPreserveNearlyFullAxisOffsetForAbsolutePointer(t *testing.T) {
	screenState := &screen.ScreenState{}
	screenState.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 711, Y: 28, Width: 498, Height: 1052, Valid: true})
	x, y, err := resolvePointerPositionForSurface(screenState, false, 500, 220)
	if err != nil {
		t.Fatalf("resolvePointerPositionForSurface returned error: %v", err)
	}
	localX := 0.5 * float64(498-1)
	localY := 0.22 * float64(1052-1)
	wantX := scalePixelToAbsolute(localX, 498)
	wantY := scalePixelToAbsolute(28+localY, 1080)
	if x != wantX || y != wantY {
		t.Fatalf("normalized point = (%d,%d), want (%d,%d)", x, y, wantX, wantY)
	}
}

func TestPointerCoordinateRejectsNonFiniteStringValues(t *testing.T) {
	for _, input := range []string{`"NaN"`, `"Inf"`, `"-Inf"`, `"Infinity"`} {
		var coordinate pointerCoordinate
		err := json.Unmarshal([]byte(input), &coordinate)
		if err == nil {
			t.Fatalf("UnmarshalJSON(%s) succeeded, want finite number error", input)
		}
		if !strings.Contains(err.Error(), "coordinate must be a finite number") {
			t.Fatalf("UnmarshalJSON(%s) error = %v, want finite number error", input, err)
		}
	}
}

func testPointerController(dev *HIDDevice, state *pointerState) *pointerController {
	return &pointerController{dev: dev, state: state}
}

func testTouchscreenPointerController(dev *HIDDevice, state *pointerState) *pointerController {
	return &pointerController{dev: dev, state: state, touchscreen: true}
}

type touchscreenReport struct {
	flags     uint8
	contactID uint8
	x         uint16
	y         uint16
}

func readTouchscreenReports(t *testing.T, dev *HIDDevice, path string) []touchscreenReport {
	t.Helper()

	dev.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data)%6 != 0 {
		t.Fatalf("touchscreen report data length = %d, want multiple of 6", len(data))
	}

	reports := make([]touchscreenReport, 0, len(data)/6)
	for i := 0; i < len(data); i += 6 {
		reports = append(reports, touchscreenReport{
			flags:     data[i],
			contactID: data[i+1],
			x:         binary.LittleEndian.Uint16(data[i+2 : i+4]),
			y:         binary.LittleEndian.Uint16(data[i+4 : i+6]),
		})
	}
	return reports
}

func TestTouchGestureSwipeWritesDragSequence(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":100,"y":900},"end":{"x":900,"y":100}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	reports := readMouseReports(t, dev, path)
	if len(reports) <= 2+defaultSwipeSteps+touchReleaseReportCount {
		t.Fatalf("len(reports) = %d, want provider default sequence", len(reports))
	}
	if reports[0].buttons != 0x00 {
		t.Fatalf("pre-move buttons = %d, want 0", reports[0].buttons)
	}
	if reports[0].x != 3277 || reports[0].y != 29490 {
		t.Fatalf("pre-move point = (%d,%d), want (3277,29490)", reports[0].x, reports[0].y)
	}
	if reports[1].buttons != 0x01 {
		t.Fatalf("press buttons = %d, want 1", reports[1].buttons)
	}
	if reports[1].x != 3277 || reports[1].y != 29490 {
		t.Fatalf("press point = (%d,%d), want (3277,29490)", reports[1].x, reports[1].y)
	}
	finalMove := len(reports) - touchReleaseReportCount - 1
	if reports[finalMove].x != 29490 || reports[finalMove].y != 3277 || reports[finalMove].buttons != 0x01 {
		t.Fatalf("final move = (%d,%d,%d), want (29490,3277,1)", reports[finalMove].x, reports[finalMove].y, reports[finalMove].buttons)
	}
	for i := finalMove + 1; i < len(reports); i++ {
		if reports[i].x != 29490 || reports[i].y != 3277 || reports[i].buttons != 0 {
			t.Fatalf("release report %d = (%d,%d,%d), want (29490,3277,0)", i, reports[i].x, reports[i].y, reports[i].buttons)
		}
	}
}

func TestSwipeDirectionUsesSpeedAndDuration(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":500,"y":800},"direction":"up","speed":2500,"duration_ms":300}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	reports := readMouseReports(t, dev, path)
	if len(reports) <= 2+defaultSwipeSteps+touchReleaseReportCount {
		t.Fatalf("len(reports) = %d, want provider default sequence", len(reports))
	}
	if reports[0].y != 26214 {
		t.Fatalf("swipe start y = %d, want 26214", reports[0].y)
	}
	if reports[len(reports)-touchReleaseReportCount-1].y != 1638 {
		t.Fatalf("swipe end y = %d, want 1638", reports[len(reports)-touchReleaseReportCount-1].y)
	}
}

func TestSwipeSpeedControlsCalculatedDuration(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":500,"y":600},"end":{"x":500,"y":400},"speed":1000}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	reports := readMouseReports(t, dev, path)
	if len(reports) <= 2+defaultSwipeSteps+touchReleaseReportCount {
		t.Fatalf("len(reports) = %d, want provider default sequence", len(reports))
	}
	if reports[0].y != 19660 {
		t.Fatalf("start y = %d, want 19660", reports[0].y)
	}
	if reports[len(reports)-touchReleaseReportCount-1].y != 13107 {
		t.Fatalf("end y = %d, want 13107", reports[len(reports)-touchReleaseReportCount-1].y)
	}
}

func TestSwipeDirectionDefaultsToEdgeAndSlowsBeforeRelease(t *testing.T) {
	dev, w := newTimedHIDDevice()
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":750,"y":500},"direction":"left"}`)
	if err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want ok", out)
	}

	times := w.writeTimes()
	if len(times) <= 2+defaultSwipeSteps+touchReleaseReportCount {
		t.Fatalf("len(times) = %d, want provider default sequence", len(times))
	}
	firstRelease := len(times) - touchReleaseReportCount
	releaseDelay := times[firstRelease].Sub(times[1+defaultSwipeSteps])
	if releaseDelay < 95*time.Millisecond || releaseDelay > 250*time.Millisecond {
		t.Fatalf("direction-form swipe slow tail duration = %v, want about 100ms", releaseDelay)
	}
}

func TestSwipeRejectsRetiredGestureTypes(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"swipe_up","start":{"x":500,"y":800}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, `unsupported gesture type: "swipe_up"`) {
		t.Fatalf("Call output = %q, want retired gesture type error", out)
	}

	reports := readMouseReports(t, dev, path)
	if len(reports) != 0 {
		t.Fatalf("len(reports) = %d, want no HID writes", len(reports))
	}
}

func TestMouseMoveRejectsCoordinatesOutsideNormalizedRange(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testMouseMoveTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"x":2000,"y":3000}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, "normalized 0-1000 scale") {
		t.Fatalf("Call output = %q, want normalized range error", out)
	}

	reports := readMouseReports(t, dev, path)
	if len(reports) != 0 {
		t.Fatalf("len(reports) = %d, want 0", len(reports))
	}
}

func TestTouchGestureTapAcceptsStringCoordinates(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":"500","y":"250"}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	reports := readMouseReports(t, dev, path)
	if len(reports) != 2+touchReleaseReportCount {
		t.Fatalf("len(reports) = %d, want %d (pre-move, press, repeated release)", len(reports), 2+touchReleaseReportCount)
	}
	if reports[0].x != 16384 || reports[0].y != 8192 {
		t.Fatalf("pre-move point = (%d,%d), want (16384,8192)", reports[0].x, reports[0].y)
	}
}

func TestTouchscreenTapWritesTouchDownAndUp(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev, touchscreen: true})

	out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":500,"y":250}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	reports := readTouchscreenReports(t, dev, path)
	if len(reports) != 1+touchReleaseReportCount {
		t.Fatalf("len(reports) = %d, want %d (down, repeated up)", len(reports), 1+touchReleaseReportCount)
	}
	if reports[0].flags != 0x03 || reports[0].contactID != 1 || reports[0].x != 16384 || reports[0].y != 8192 {
		t.Fatalf("down report = %+v, want touch at (16384,8192)", reports[0])
	}
	for i := 1; i < len(reports); i++ {
		if reports[i].flags != 0x00 || reports[i].contactID != 1 || reports[i].x != 16384 || reports[i].y != 8192 {
			t.Fatalf("up report %d = %+v, want release at (16384,8192)", i, reports[i])
		}
	}
}

func TestTouchscreenTapUsesFrameSpaceForActiveArea(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	screenState := &screen.ScreenState{}
	screenState.UpdateActiveArea(1920, 1080, screen.ScreenActiveArea{X: 656, Y: 0, Width: 608, Height: 1080, Valid: true})
	tool := testTouchGestureTool(t, testMNKOpts{screenState: screenState, pointer: dev, touchscreen: true})

	out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":627,"y":180}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	reports := readTouchscreenReports(t, dev, path)
	if len(reports) != 1+touchReleaseReportCount {
		t.Fatalf("len(reports) = %d, want %d (down, repeated up)", len(reports), 1+touchReleaseReportCount)
	}
	wantX := uint16(scalePixelToAbsolute(float64(656)+(627.0/1000.0)*float64(608-1), 1920))
	wantY := uint16(scalePixelToAbsolute((180.0/1000.0)*float64(1080-1), 1080))
	if reports[0].flags != 0x03 || reports[0].contactID != 1 || reports[0].x != wantX || reports[0].y != wantY {
		t.Fatalf("down report = %+v, want touch at (%d,%d)", reports[0], wantX, wantY)
	}
	for i := 1; i < len(reports); i++ {
		if reports[i].flags != 0x00 || reports[i].contactID != 1 || reports[i].x != wantX || reports[i].y != wantY {
			t.Fatalf("up report %d = %+v, want release at (%d,%d)", i, reports[i], wantX, wantY)
		}
	}
}

func TestTouchscreenSwipeWritesTouchSequence(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev, touchscreen: true})

	out, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":200,"y":500},"end":{"x":800,"y":500}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call output = %q, want ok", out)
	}

	reports := readTouchscreenReports(t, dev, path)
	if len(reports) <= 1+defaultSwipeSteps+touchReleaseReportCount {
		t.Fatalf("len(reports) = %d, want additional low-speed reports", len(reports))
	}
	if reports[0].flags != 0x03 || reports[0].x != 6553 {
		t.Fatalf("down report = %+v, want start touch", reports[0])
	}
	finalMove := len(reports) - touchReleaseReportCount - 1
	if reports[finalMove].flags != 0x03 || reports[finalMove].x != 26214 {
		t.Fatalf("final move = %+v, want end while touching", reports[finalMove])
	}
	last := reports[len(reports)-1]
	if last.flags != 0x00 || last.x != 26214 {
		t.Fatalf("last release = %+v, want release at end", last)
	}
}

func TestKeyboardTextAcceptsBareTextFallback(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := &KeyboardTextTool{dev: dev}

	out, err := tool.Call(context.Background(), `App Store`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected output: %q", out)
	}

	dev.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != len("App Store")*16 {
		t.Fatalf("expected 2 keyboard reports per ASCII character, got %d bytes", len(data))
	}
}

func TestKeyboardTextUsesConfiguredAZERTYLayout(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := &KeyboardTextTool{dev: dev, keyboardLayout: mnk.KeyboardLayoutAZERTY}

	out, err := tool.Call(context.Background(), `{"text":"shape"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected output: %q", out)
	}

	dev.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	wantUsages := []byte{0x16, 0x0b, 0x14, 0x13, 0x08}
	if len(data) != len(wantUsages)*16 {
		t.Fatalf("report bytes = %d, want %d", len(data), len(wantUsages)*16)
	}
	for i, want := range wantUsages {
		press := data[i*16 : i*16+8]
		if press[0] != 0 || press[2] != want {
			t.Fatalf("press report %d = %v, want modifier 0 and usage 0x%02x", i, press, want)
		}
	}
}

func TestKeyboardTextUsesConfiguredQWERTZLayout(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := &KeyboardTextTool{dev: dev, keyboardLayout: mnk.KeyboardLayoutQWERTZ}

	out, err := tool.Call(context.Background(), `{"text":"yz"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected output: %q", out)
	}

	dev.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	wantUsages := []byte{0x1d, 0x1c}
	if len(data) != len(wantUsages)*16 {
		t.Fatalf("report bytes = %d, want %d", len(data), len(wantUsages)*16)
	}
	for i, want := range wantUsages {
		press := data[i*16 : i*16+8]
		if press[0] != 0 || press[2] != want {
			t.Fatalf("press report %d = %v, want modifier 0 and usage 0x%02x", i, press, want)
		}
	}
}

func TestKeyboardTextRejectsQWERTZDeadKeysWithoutPartialTyping(t *testing.T) {
	for _, deadKey := range []string{"^", "`"} {
		t.Run(deadKey, func(t *testing.T) {
			dev, path := newTestHIDDevice(t)
			tool := &KeyboardTextTool{dev: dev, keyboardLayout: mnk.KeyboardLayoutQWERTZ}

			out, err := tool.Call(context.Background(), fmt.Sprintf(`{"text":"a%sb"}`, deadKey))
			if err != nil {
				t.Fatalf("Call returned error: %v", err)
			}
			if !strings.Contains(out, fmt.Sprintf(`unsupported characters: %q`, deadKey)) {
				t.Fatalf("unexpected output: %q", out)
			}

			dev.Close()
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile: %v", err)
			}
			if len(data) != 0 {
				t.Fatalf("dead key must not type a partial prefix, got %d bytes", len(data))
			}
		})
	}
}

func TestKeyboardTextRejectsUnsupportedCharactersWithoutPartialTyping(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := &KeyboardTextTool{dev: dev}

	out, err := tool.Call(context.Background(), `{"text":"A™中文B"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, `unsupported characters: "™中文"`) {
		t.Fatalf("unexpected output: %q", out)
	}

	dev.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("unsupported text must not type a partial prefix, got %d bytes", len(data))
	}
}

func TestPostActionScreenshotToolReturnsScreenshotJSON(t *testing.T) {
	action := &stubTool{name: "keyboard_tap", output: "ok"}
	screenshot := &stubTool{
		name:   "screenshot",
		output: `{"width":320,"height":240,"format":"jpeg","size":4,"data":"ZmFrZQ=="}`,
	}
	tool := newPostActionScreenshotTool(action, screenshot, 0)

	out, err := tool.Call(context.Background(), `{"keys":["enter"]}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out == "ok" {
		t.Fatalf("Call output = %q, want screenshot JSON", out)
	}

	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not valid post-action screenshot JSON: %v", err)
	}
	if result.ActionOutput != "ok" {
		t.Fatalf("ActionOutput = %q, want ok", result.ActionOutput)
	}
	if result.Width != 320 || result.Height != 240 || result.Format != "jpeg" || result.Data != "ZmFrZQ==" {
		t.Fatalf("unexpected screenshot result: %#v", result)
	}
	if len(action.inputs) != 1 || action.inputs[0] != `{"keys":["enter"]}` {
		t.Fatalf("action inputs = %#v", action.inputs)
	}
	if len(screenshot.inputs) != 2 || screenshot.inputs[0] != "{}" || screenshot.inputs[1] != "{}" {
		t.Fatalf("screenshot inputs = %#v, want pre-action and final captures", screenshot.inputs)
	}
	visual, ok := tool.(visualObservationTool)
	if !ok || !visual.ReturnsVisualObservation() {
		t.Fatalf("post-action tool must be a visual observation tool")
	}
}

func TestPostActionScreenshotToolComparesPreActionAndFinalScreenshots(t *testing.T) {
	before := terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{})
	tests := []struct {
		name    string
		after   string
		changed bool
	}{
		{
			name:    "unchanged",
			after:   terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{}),
			changed: false,
		},
		{
			name:    "top status area only",
			after:   terminationPolicyScreenshotObservation(t, 200, 400, image.Rect(0, 0, 200, 32)),
			changed: false,
		},
		{
			name:    "meaningful structural change",
			after:   terminationPolicyScreenshotObservation(t, 200, 400, image.Rect(20, 100, 120, 200)),
			changed: true,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			events := make([]string, 0, 3)
			screenshot := &stubTool{name: "screenshot"}
			screenshot.callFn = func(context.Context, string) (string, error) {
				if len(screenshot.inputs) == 1 {
					events = append(events, "baseline")
					return before, nil
				}
				events = append(events, "final")
				return test.after, nil
			}
			action := &stubTool{name: "touch_gesture"}
			action.callFn = func(context.Context, string) (string, error) {
				events = append(events, "action")
				return "ok", nil
			}
			tool := newPostActionScreenshotTool(action, screenshot, 0)

			out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":500,"y":500}}`)
			if err != nil {
				t.Fatalf("Call returned error: %v", err)
			}
			var result postActionScreenshotResult
			if err := json.Unmarshal([]byte(out), &result); err != nil {
				t.Fatalf("unmarshal result: %v", err)
			}
			if result.ScreenChanged == nil || *result.ScreenChanged != test.changed {
				t.Fatalf("ScreenChanged = %#v, want %v", result.ScreenChanged, test.changed)
			}
			if !slices.Equal(events, []string{"baseline", "action", "final"}) {
				t.Fatalf("call order = %#v, want baseline, action, final", events)
			}
		})
	}
}

func TestPostActionScreenshotToolContinuesWhenBaselineCaptureFails(t *testing.T) {
	final := terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{})
	screenshot := &stubTool{name: "screenshot"}
	screenshot.callFn = func(context.Context, string) (string, error) {
		if len(screenshot.inputs) == 1 {
			return "", errors.New("baseline unavailable")
		}
		return final, nil
	}
	action := &stubTool{name: "touch_gesture", output: "ok"}
	tool := newPostActionScreenshotTool(action, screenshot, 0)

	out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":500,"y":500}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	if len(action.inputs) != 1 || len(screenshot.inputs) != 2 {
		t.Fatalf("calls after baseline failure: action=%#v screenshot=%#v", action.inputs, screenshot.inputs)
	}
	if result.ScreenChanged != nil {
		t.Fatalf("ScreenChanged = %#v, want omitted without a baseline", result.ScreenChanged)
	}
	if result.Data == "" {
		t.Fatal("final screenshot was not returned")
	}
}

func TestPostActionScreenshotToolFallsBackScreenshotWhenScreenUnstable(t *testing.T) {
	action := &stubTool{name: "touch_gesture", output: "ok"}
	waitStable := &stubTool{
		name:   "wait_for_stable_screen",
		output: `{"ok":true,"stable":false,"elapsed_ms":3001,"screen_changed":true,"last_diff":18.5}`,
	}
	unchanged := terminationPolicyScreenshotObservation(t, 320, 240, image.Rectangle{})
	screenshot := &stubTool{name: "screenshot", output: unchanged}
	tool := newPostActionStableScreenshotTool(action, waitStable, screenshot, 0, ScreenStableDefaults{TimeoutMs: 3000, StableMs: 500})

	out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":500,"y":500}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if strings.HasPrefix(out, "error:") {
		t.Fatalf("Call output = %q, want success with fallback screenshot", out)
	}

	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not valid post-action screenshot JSON: %v", err)
	}
	if result.ActionOutput != "ok" {
		t.Fatalf("ActionOutput = %q, want ok", result.ActionOutput)
	}
	if result.ScreenStable == nil || *result.ScreenStable {
		t.Fatalf("ScreenStable = %#v, want false", result.ScreenStable)
	}
	if result.ScreenChanged == nil || *result.ScreenChanged {
		t.Fatalf("ScreenChanged = %#v, want false from unchanged pre-action/final screenshots despite wait motion", result.ScreenChanged)
	}
	if result.StableWaitMs == nil || *result.StableWaitMs != 3001 {
		t.Fatalf("StableWaitMs = %#v, want 3001", result.StableWaitMs)
	}
	if result.LastDiff == nil || *result.LastDiff != 18.5 {
		t.Fatalf("LastDiff = %#v, want 18.5", result.LastDiff)
	}
	var expected screenshotResult
	if err := json.Unmarshal([]byte(unchanged), &expected); err != nil {
		t.Fatalf("unmarshal expected screenshot: %v", err)
	}
	raw, err := base64.StdEncoding.DecodeString(expected.Data)
	if err != nil {
		t.Fatalf("decode expected screenshot: %v", err)
	}
	marked, err := drawTouchGesturePostMarker(raw, touchGesturePostMarkerInfo{Type: "tap", X: 500, Y: 500})
	if err != nil {
		t.Fatalf("mark expected screenshot: %v", err)
	}
	if result.Data != base64.StdEncoding.EncodeToString(marked) {
		t.Fatalf("screenshot data does not contain the requested touch marker")
	}
	if len(waitStable.inputs) != 1 || waitStable.inputs[0] != `{"timeout_ms":3000,"stable_ms":500,"diff_threshold":6}` {
		t.Fatalf("wait stable inputs = %#v", waitStable.inputs)
	}
	if len(screenshot.inputs) != 2 {
		t.Fatalf("screenshot should still be called, got inputs %#v", screenshot.inputs)
	}
}

func TestPostActionScreenshotFailureMarksActionAsCompleted(t *testing.T) {
	action := &stubTool{name: "touch_gesture", output: "ok"}
	baseline := terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{})
	screenshot := &stubTool{name: "screenshot"}
	screenshot.callFn = func(context.Context, string) (string, error) {
		if len(screenshot.inputs) == 1 {
			return baseline, nil
		}
		return "", errors.New("capture unavailable")
	}
	tool := newPostActionScreenshotTool(action, screenshot, 0)
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	toolErr := ToolErrorFromContext(ctx)
	if toolErr == nil || toolErr.Message != out {
		t.Fatalf("ToolError = %#v, output=%q", toolErr, out)
	}
	if completed, _ := toolErr.Details[postActionCompletedDetail].(bool); !completed {
		t.Fatalf("post-action error details = %#v, want action_completed=true", toolErr.Details)
	}
	if len(action.inputs) != 1 || len(screenshot.inputs) != 2 {
		t.Fatalf("final failure call counts: action=%#v screenshot=%#v", action.inputs, screenshot.inputs)
	}
}

func TestPostActionScreenshotToolOmitsLastDiffWhenStableWaitOmitsIt(t *testing.T) {
	action := &stubTool{name: "touch_gesture", output: "ok"}
	waitStable := &stubTool{
		name:   "wait_for_stable_screen",
		output: `{"ok":true,"stable":true,"elapsed_ms":600,"screen_changed":false}`,
	}
	screenshot := &stubTool{name: "screenshot", output: terminationPolicyScreenshotObservation(t, 320, 240, image.Rectangle{})}
	tool := newPostActionStableScreenshotTool(action, waitStable, screenshot, 0, ScreenStableDefaults{})

	out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":500,"y":500}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}

	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output is not valid post-action screenshot JSON: %v", err)
	}
	if result.LastDiff != nil {
		t.Fatalf("LastDiff = %#v, want omitted", result.LastDiff)
	}
	if result.ScreenChanged == nil || *result.ScreenChanged {
		t.Fatalf("ScreenChanged = %#v, want false", result.ScreenChanged)
	}
}

func TestPostActionScreenshotToolSkipsScreenshotOnActionErrorOutput(t *testing.T) {
	action := &stubTool{name: "touch_gesture", output: "error: invalid input"}
	screenshot := &stubTool{
		name:   "screenshot",
		output: `{"width":320,"height":240,"format":"jpeg","size":4,"data":"ZmFrZQ=="}`,
	}
	tool := newPostActionScreenshotTool(action, screenshot, 0)
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "invalid input" {
		t.Fatalf("Call output = %q, want structured error message", out)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeToolExecutionFailed || got.Message != out {
		t.Fatalf("ToolError = %+v, want tool_execution_failed with output message", got)
	}
	if len(screenshot.inputs) != 1 {
		t.Fatalf("only the pre-action baseline should be captured on action error, got inputs %#v", screenshot.inputs)
	}
}

func TestPostActionScreenshotToolSkipsScreenshotOnStructuredActionError(t *testing.T) {
	toolErr := NewToolError(CodeInvalidArguments, "invalid touch gesture")
	action := &contextToolErrorStub{name: "touch_gesture", toolErr: toolErr}
	screenshot := &stubTool{
		name:   "screenshot",
		output: `{"width":320,"height":240,"format":"jpeg","size":4,"data":"ZmFrZQ=="}`,
	}
	tool := newPostActionScreenshotTool(action, screenshot, 0)
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != toolErr.Message {
		t.Fatalf("Call output = %q, want structured error message %q", out, toolErr.Message)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeInvalidArguments {
		t.Fatalf("context ToolError = %+v, want invalid_arguments", got)
	}
	if len(screenshot.inputs) != 1 {
		t.Fatalf("only the pre-action baseline should be captured on structured action error, got inputs %#v", screenshot.inputs)
	}
}

type contextToolErrorStub struct {
	name    string
	toolErr *ToolError
}

func (t *contextToolErrorStub) Name() string { return t.name }

func (t *contextToolErrorStub) Description() string { return "structured error stub" }

func (t *contextToolErrorStub) Call(ctx context.Context, input string) (string, error) {
	SetToolError(ctx, t.toolErr)
	return toolErrorString(t.toolErr), nil
}

func TestHIDDeviceWriteRetriesAfterEndpointShutdown(t *testing.T) {
	first := &fakeHIDWriteCloser{writeErr: syscall.ESHUTDOWN}
	second := &fakeHIDWriteCloser{}
	openCount := 0

	dev := &HIDDevice{
		path: "fake-hid",
		open: func(string) (io.WriteCloser, error) {
			openCount++
			if openCount == 1 {
				return first, nil
			}
			return second, nil
		},
	}

	if err := dev.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if openCount != 2 {
		t.Fatalf("openCount = %d, want 2", openCount)
	}
	if !first.closed {
		t.Fatalf("expected first writer to be closed after retryable failure")
	}
	if second.writeCount != 1 {
		t.Fatalf("second writer writeCount = %d, want 1", second.writeCount)
	}
}

func TestHIDDeviceWriteReopensWhenDeviceNodeRecreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidg0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create test hid device: %v", err)
	}
	dev := NewHIDDevice(path)
	dev.refreshState = ""

	if err := dev.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("first Write returned error: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove test hid device: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("recreate test hid device: %v", err)
	}

	if err := dev.Write([]byte{4, 5, 6}); err != nil {
		t.Fatalf("second Write returned error: %v", err)
	}
	dev.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recreated test hid device: %v", err)
	}
	if string(data) != string([]byte{4, 5, 6}) {
		t.Fatalf("recreated device data = %v, want only second report", data)
	}
}

func TestHIDDeviceWriteReopensAfterWatchdogRefreshState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hidg0")
	statePath := filepath.Join(dir, "aiden_usb_ecm_watchdog.state")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create test hid device: %v", err)
	}
	dev := NewHIDDevice(path)
	dev.refreshState = statePath

	if err := dev.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("first Write returned error: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("last_refresh_result=ok\n"), 0o600); err != nil {
		t.Fatalf("write watchdog state: %v", err)
	}
	refreshTime := dev.openedAt.Add(time.Second)
	if err := os.Chtimes(statePath, refreshTime, refreshTime); err != nil {
		t.Fatalf("set watchdog state mtime: %v", err)
	}

	if err := dev.Write([]byte{4, 5, 6}); err != nil {
		t.Fatalf("second Write returned error: %v", err)
	}
	dev.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read test hid device: %v", err)
	}
	if string(data) != string([]byte{4, 5, 6}) {
		t.Fatalf("device data = %v, want second report after reopen", data)
	}
}

func TestTriggerUSBCompositeRefreshUsesConfiguredCommand(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "usb-refresh")
	argsPath := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$AIDEN_USB_REFRESH_TEST_ARGS\"\n"
	if err := os.WriteFile(commandPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write refresh command: %v", err)
	}
	t.Setenv("AIDEN_USB_COMPOSITE_REFRESH_COMMAND", commandPath)
	t.Setenv("AIDEN_USB_REFRESH_TEST_ARGS", argsPath)

	if err := triggerUSBCompositeRefresh(""); err != nil {
		t.Fatalf("triggerUSBCompositeRefresh returned error: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read refresh arguments: %v", err)
	}
	if string(args) != "refresh\n" {
		t.Fatalf("refresh arguments = %q, want %q", args, "refresh\\n")
	}
}

func TestHIDDeviceWriteReturnsNonRetryableError(t *testing.T) {
	dev := &HIDDevice{
		path: "fake-hid",
		open: func(string) (io.WriteCloser, error) {
			return &fakeHIDWriteCloser{writeErr: errors.New("permission denied")}, nil
		},
	}

	err := dev.Write([]byte{1})
	if err == nil {
		t.Fatal("expected write error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestMouseScrollToolRejectsOutOfRangeDelta(t *testing.T) {
	tool := &MouseScrollTool{}
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{"delta":128}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "delta must be between -127 and 127" {
		t.Fatalf("output = %q", out)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeInvalidArguments || got.Message != out {
		t.Fatalf("ToolError = %+v, want invalid_arguments with output message", got)
	}
}

func TestMouseScrollToolRejectsTouchscreenPointerMode(t *testing.T) {
	tool := testMouseScrollTool(t, testMNKOpts{touchscreen: true})
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{"delta":-3}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	want := "mouse_scroll is unsupported when pointer_mode is touchscreen; use touch_gesture"
	if out != want {
		t.Fatalf("output = %q, want %q", out, want)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeInvalidArguments || got.Message != want {
		t.Fatalf("ToolError = %+v, want invalid_arguments with output message", got)
	}
}

func TestMouseScrollUsesLastPointerPosition(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	provider := testMNKProvider(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})
	moveTool := &MouseMoveTool{mnkProvider: provider}
	scrollTool := &MouseScrollTool{mnkProvider: provider}

	if out, err := moveTool.Call(context.Background(), `{"x":200,"y":300}`); err != nil || out != "ok" {
		t.Fatalf("move output=%q err=%v", out, err)
	}
	if out, err := scrollTool.Call(context.Background(), `{"delta":-3}`); err != nil || out != "ok" {
		t.Fatalf("scroll output=%q err=%v", out, err)
	}

	reports := readMouseReports(t, dev, path)
	if len(reports) != 2 {
		t.Fatalf("len(reports) = %d, want 2", len(reports))
	}
	wantX, wantY := normalizedToAbsolutePoint(200, 300)
	if reports[1].buttons != 0 || int(reports[1].x) != wantX || int(reports[1].y) != wantY || reports[1].wheel != -3 {
		t.Fatalf("scroll report = (%d,%d,%d,%d), want (0,%d,%d,-3)", reports[1].buttons, reports[1].x, reports[1].y, reports[1].wheel, wantX, wantY)
	}
}

type mouseReport struct {
	buttons uint8
	x       uint16
	y       uint16
	wheel   int8
}

func newTestHIDDevice(t *testing.T) (*HIDDevice, string) {
	t.Helper()

	dir := t.TempDir()
	path := filepath.Join(dir, "hid.bin")
	f, err := os.Create(path)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	f.Close()

	return NewHIDDevice(path), path
}

func TestKeyboardTapSendsModifierOnly(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testKeyboardTapTool(t, testMNKOpts{keyboard: dev})

	out, err := tool.Call(context.Background(), `{"keys":["meta"]}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected output: %s", out)
	}

	dev.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != 16 {
		t.Fatalf("report bytes = %d, want 16 (modifier + release)", len(data))
	}

	chord := data[0:8]
	release := data[8:16]
	if chord[0] != 0x08 {
		t.Fatalf("chord modifier = 0x%02x, want 0x08", chord[0])
	}
	for i := 2; i < 8; i++ {
		if chord[i] != 0 {
			t.Fatalf("chord key slot [%d] = 0x%02x, want 0", i, chord[i])
		}
	}
	for i := range release {
		if release[i] != 0 {
			t.Fatalf("release report = %v, want all zeros", release)
		}
	}
}

func TestKeyboardTapSendsModifierChordWithHold(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testKeyboardTapTool(t, testMNKOpts{keyboard: dev})

	out, err := tool.Call(context.Background(), `{"keys":["meta","q"]}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected output: %s", out)
	}

	dev.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != 16 {
		t.Fatalf("report bytes = %d, want 16 (chord + release)", len(data))
	}

	chord := data[0:8]
	release := data[8:16]

	if chord[0] != 0x08 || chord[2] != 0x14 {
		t.Fatalf("chord report = %v, want modifier 0x08 and key q(0x14)", chord)
	}
	for i := range release {
		if release[i] != 0 {
			t.Fatalf("release report = %v, want all zeros", release)
		}
	}
}

func TestKeyboardTapUsesConfiguredAZERTYLayout(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testKeyboardTapTool(t, testMNKOpts{keyboard: dev, layout: mnk.KeyboardLayoutAZERTY})

	out, err := tool.Call(context.Background(), `{"keys":["a"]}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected output: %s", out)
	}

	dev.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != 16 {
		t.Fatalf("report bytes = %d, want 16", len(data))
	}
	if data[0] != 0 || data[2] != 0x14 {
		t.Fatalf("press report = %v, want modifier 0 and AZERTY a usage 0x14", data[:8])
	}
}

func TestKeyboardTapSupportsAndroidBackAlias(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	androidDev, androidPath := newTestHIDDevice(t)
	tool := testKeyboardTapTool(t, testMNKOpts{keyboard: dev, android: androidDev, touchscreen: true})

	out, err := tool.Call(context.Background(), `{"keys":["KEYCODE_BACK"]}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected output: %s", out)
	}

	dev.Close()
	androidDev.Close()
	if data, err := os.ReadFile(path); err != nil {
		t.Fatalf("ReadFile keyboard path: %v", err)
	} else if len(data) != 0 {
		t.Fatalf("standard keyboard path bytes = %v, want none for android extension key", data)
	}
	data, err := os.ReadFile(androidPath)
	if err != nil {
		t.Fatalf("ReadFile android path: %v", err)
	}
	if len(data) != 4 {
		t.Fatalf("report bytes = %d, want 4 (consumer usage + release)", len(data))
	}
	if got := uint16(data[0]) | uint16(data[1])<<8; got != androidExtensionUsageMap["android_back"] {
		t.Fatalf("android back usage = 0x%04x, want 0x%04x", got, androidExtensionUsageMap["android_back"])
	}
	if data[2] != 0 || data[3] != 0 {
		t.Fatalf("android back release = %v, want [0 0]", data[2:4])
	}
}

func TestKeyboardTapSupportsAndroidVolumeAlias(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	androidDev, androidPath := newTestHIDDevice(t)
	tool := testKeyboardTapTool(t, testMNKOpts{keyboard: dev, android: androidDev, touchscreen: true})

	out, err := tool.Call(context.Background(), `{"keys":["KEYCODE_VOLUME_UP"]}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected output: %s", out)
	}

	dev.Close()
	androidDev.Close()
	if data, err := os.ReadFile(path); err != nil {
		t.Fatalf("ReadFile keyboard path: %v", err)
	} else if len(data) != 0 {
		t.Fatalf("standard keyboard path bytes = %v, want none for android extension key", data)
	}
	data, err := os.ReadFile(androidPath)
	if err != nil {
		t.Fatalf("ReadFile android path: %v", err)
	}
	if len(data) != 4 {
		t.Fatalf("report bytes = %d, want 4 (consumer usage + release)", len(data))
	}
	if got := uint16(data[0]) | uint16(data[1])<<8; got != androidExtensionUsageMap["volume_up"] {
		t.Fatalf("android volume_up usage = 0x%04x, want 0x%04x", got, androidExtensionUsageMap["volume_up"])
	}
}

func TestKeyboardTapSupportsAndroidAppSwitchAlias(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	androidDev, androidPath := newTestHIDDevice(t)
	tool := testKeyboardTapTool(t, testMNKOpts{keyboard: dev, android: androidDev, touchscreen: true})

	out, err := tool.Call(context.Background(), `{"keys":["KEYCODE_APP_SWITCH"]}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if out != "ok" {
		t.Fatalf("unexpected output: %s", out)
	}

	dev.Close()
	androidDev.Close()
	if data, err := os.ReadFile(path); err != nil {
		t.Fatalf("ReadFile keyboard path: %v", err)
	} else if len(data) != 0 {
		t.Fatalf("standard keyboard path bytes = %v, want none for android extension key", data)
	}
	data, err := os.ReadFile(androidPath)
	if err != nil {
		t.Fatalf("ReadFile android path: %v", err)
	}
	if len(data) != 4 {
		t.Fatalf("report bytes = %d, want 4 (consumer usage + release)", len(data))
	}
	if got := uint16(data[0]) | uint16(data[1])<<8; got != androidExtensionUsageMap["app_switch"] {
		t.Fatalf("android app_switch usage = 0x%04x, want 0x%04x", got, androidExtensionUsageMap["app_switch"])
	}
}

func TestKeyboardTapSupportsAdditionalAndroidKeycodeAliases(t *testing.T) {
	testCases := []struct {
		input  string
		mapKey string
	}{
		{input: "KEYCODE_SLEEP", mapKey: "sleep"},
		{input: "KEYCODE_MEDIA_PLAY_PAUSE", mapKey: "media_play_pause"},
		{input: "KEYCODE_MEDIA_STOP", mapKey: "media_stop"},
		{input: "KEYCODE_MEDIA_NEXT", mapKey: "media_next"},
		{input: "KEYCODE_MEDIA_PREVIOUS", mapKey: "media_previous"},
		{input: "KEYCODE_MEDIA_REWIND", mapKey: "media_rewind"},
		{input: "KEYCODE_MEDIA_FAST_FORWARD", mapKey: "media_fast_forward"},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			dev, path := newTestHIDDevice(t)
			androidDev, androidPath := newTestHIDDevice(t)
			tool := testKeyboardTapTool(t, testMNKOpts{keyboard: dev, android: androidDev, touchscreen: true})

			out, err := tool.Call(context.Background(), fmt.Sprintf(`{"keys":["%s"]}`, tc.input))
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			if out != "ok" {
				t.Fatalf("unexpected output: %s", out)
			}

			dev.Close()
			androidDev.Close()
			if data, err := os.ReadFile(path); err != nil {
				t.Fatalf("ReadFile keyboard path: %v", err)
			} else if len(data) != 0 {
				t.Fatalf("standard keyboard path bytes = %v, want none for android extension key", data)
			}
			data, err := os.ReadFile(androidPath)
			if err != nil {
				t.Fatalf("ReadFile android path: %v", err)
			}
			if len(data) != 4 {
				t.Fatalf("report bytes = %d, want 4 (consumer usage + release)", len(data))
			}
			if got := uint16(data[0]) | uint16(data[1])<<8; got != androidExtensionUsageMap[tc.mapKey] {
				t.Fatalf("usage = 0x%04x, want 0x%04x", got, androidExtensionUsageMap[tc.mapKey])
			}
		})
	}
}

func TestKeyboardTapSupportsHIDBackedAndroidKeycodeAliases(t *testing.T) {
	testCases := []struct {
		input  string
		mapKey string
	}{
		{input: "KEYCODE_SCREENSHOT", mapKey: "screenshot"},
		{input: "KEYCODE_SETTINGS", mapKey: "settings"},
		{input: "KEYCODE_WINDOW", mapKey: "window"},
		{input: "KEYCODE_BRIGHTNESS_UP", mapKey: "brightness_up"},
		{input: "KEYCODE_BRIGHTNESS_DOWN", mapKey: "brightness_down"},
		{input: "KEYCODE_DICTATE", mapKey: "dictate"},
		{input: "KEYCODE_EMOJI_PICKER", mapKey: "emoji_picker"},
		{input: "KEYCODE_MEDIA_AUDIO_TRACK", mapKey: "media_audio_track"},
		{input: "KEYCODE_PROFILE_SWITCH", mapKey: "profile_switch"},
		{input: "KEYCODE_NEW", mapKey: "new"},
		{input: "KEYCODE_CLOSE", mapKey: "close"},
		{input: "KEYCODE_PRINT", mapKey: "print"},
		{input: "KEYCODE_LANGUAGE_SWITCH", mapKey: "language_switch"},
		{input: "KEYCODE_REFRESH", mapKey: "refresh"},
		{input: "KEYCODE_FULLSCREEN", mapKey: "fullscreen"},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			dev, path := newTestHIDDevice(t)
			androidDev, androidPath := newTestHIDDevice(t)
			tool := testKeyboardTapTool(t, testMNKOpts{keyboard: dev, android: androidDev, touchscreen: true})

			out, err := tool.Call(context.Background(), fmt.Sprintf(`{"keys":["%s"]}`, tc.input))
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			if out != "ok" {
				t.Fatalf("unexpected output: %s", out)
			}

			dev.Close()
			androidDev.Close()
			if data, err := os.ReadFile(path); err != nil {
				t.Fatalf("ReadFile keyboard path: %v", err)
			} else if len(data) != 0 {
				t.Fatalf("standard keyboard path bytes = %v, want none for android extension key", data)
			}
			data, err := os.ReadFile(androidPath)
			if err != nil {
				t.Fatalf("ReadFile android path: %v", err)
			}
			if len(data) != 4 {
				t.Fatalf("report bytes = %d, want 4 (consumer usage + release)", len(data))
			}
			if got := uint16(data[0]) | uint16(data[1])<<8; got != androidExtensionUsageMap[tc.mapKey] {
				t.Fatalf("usage = 0x%04x, want 0x%04x", got, androidExtensionUsageMap[tc.mapKey])
			}
		})
	}
}

func TestKeyboardTapKeepsLegacyHIDUsageAliases(t *testing.T) {
	testCases := []struct {
		input  string
		mapKey string
	}{
		{input: "KEY_USAGE_SCREENSHOT", mapKey: "key_usage_screenshot"},
		{input: "KEY_USAGE_SETTINGS", mapKey: "key_usage_settings"},
		{input: "KEY_USAGE_LANGUAGE_SWITCH", mapKey: "key_usage_language_switch"},
		{input: "KEY_USAGE_PRINT", mapKey: "key_usage_print"},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			dev, path := newTestHIDDevice(t)
			androidDev, androidPath := newTestHIDDevice(t)
			tool := testKeyboardTapTool(t, testMNKOpts{keyboard: dev, android: androidDev, touchscreen: true})

			out, err := tool.Call(context.Background(), fmt.Sprintf(`{"keys":[%q]}`, tc.input))
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			if out != "ok" {
				t.Fatalf("output = %q, want ok", out)
			}

			dev.Close()
			androidDev.Close()
			if data, err := os.ReadFile(path); err != nil {
				t.Fatalf("ReadFile keyboard path: %v", err)
			} else if len(data) != 0 {
				t.Fatalf("standard keyboard path bytes = %v, want none", data)
			}
			data, err := os.ReadFile(androidPath)
			if err != nil {
				t.Fatalf("ReadFile android path: %v", err)
			}
			if len(data) != 4 {
				t.Fatalf("report bytes = %d, want 4", len(data))
			}
			if got := uint16(data[0]) | uint16(data[1])<<8; got != androidExtensionUsageMap[tc.mapKey] {
				t.Fatalf("usage = 0x%04x, want 0x%04x", got, androidExtensionUsageMap[tc.mapKey])
			}
		})
	}
}

func TestKeyboardTapAbsolutePointerModeAllowsMediaKeySubset(t *testing.T) {
	testCases := []struct {
		input  string
		mapKey string
	}{
		{input: "KEYCODE_VOLUME_MUTE", mapKey: "volume_mute"},
		{input: "KEYCODE_VOLUME_UP", mapKey: "volume_up"},
		{input: "KEYCODE_VOLUME_DOWN", mapKey: "volume_down"},
		{input: "KEYCODE_MEDIA_PLAY_PAUSE", mapKey: "media_play_pause"},
		{input: "KEYCODE_MEDIA_STOP", mapKey: "media_stop"},
		{input: "KEYCODE_MEDIA_NEXT", mapKey: "media_next"},
		{input: "KEYCODE_MEDIA_PREVIOUS", mapKey: "media_previous"},
		{input: "KEYCODE_MEDIA_REWIND", mapKey: "media_rewind"},
		{input: "KEYCODE_MEDIA_FAST_FORWARD", mapKey: "media_fast_forward"},
		{input: "KEYCODE_SCREENSHOT", mapKey: "screenshot"},
		{input: "KEYCODE_BRIGHTNESS_UP", mapKey: "brightness_up"},
		{input: "KEYCODE_BRIGHTNESS_DOWN", mapKey: "brightness_down"},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			dev, path := newTestHIDDevice(t)
			androidDev, androidPath := newTestHIDDevice(t)
			tool := testKeyboardTapTool(t, testMNKOpts{keyboard: dev, android: androidDev})

			out, err := tool.Call(context.Background(), fmt.Sprintf(`{"keys":["%s"]}`, tc.input))
			if err != nil {
				t.Fatalf("Call failed: %v", err)
			}
			if out != "ok" {
				t.Fatalf("unexpected output: %s", out)
			}

			dev.Close()
			androidDev.Close()
			if data, err := os.ReadFile(path); err != nil {
				t.Fatalf("ReadFile keyboard path: %v", err)
			} else if len(data) != 0 {
				t.Fatalf("standard keyboard path bytes = %v, want none for absolute media key", data)
			}
			data, err := os.ReadFile(androidPath)
			if err != nil {
				t.Fatalf("ReadFile android path: %v", err)
			}
			if len(data) != 4 {
				t.Fatalf("report bytes = %d, want 4 (consumer usage + release)", len(data))
			}
			if got := uint16(data[0]) | uint16(data[1])<<8; got != absolutePointerModeExtensionReports[tc.mapKey] {
				t.Fatalf("report mask = 0x%04x, want 0x%04x", got, absolutePointerModeExtensionReports[tc.mapKey])
			}
		})
	}
}

func TestKeyboardTapAbsolutePointerModeRejectsAndroidNavigationKeys(t *testing.T) {
	dev, _ := newTestHIDDevice(t)
	androidDev, _ := newTestHIDDevice(t)
	tool := testKeyboardTapTool(t, testMNKOpts{keyboard: dev, android: androidDev})
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{"keys":["KEYCODE_BACK"]}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, `hid.pointer_mode="touchscreen"`) || !strings.Contains(out, mnk.AbsolutePointerModeExtensionKeyList) {
		t.Fatalf("output = %q, want absolute pointer mode allow-list error", out)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeInvalidArguments || got.Message != out {
		t.Fatalf("ToolError = %+v, want invalid_arguments with output message", got)
	}
}

func TestKeyboardTapRejectsUnsupportedAndroidKeycodeAliases(t *testing.T) {
	testCases := []struct {
		input      string
		messageSub string
	}{
		{input: "KEYCODE_WAKEUP", messageSub: "Generic Desktop/System Control HID path"},
		{input: "KEYCODE_SOFT_SLEEP", messageSub: "no verified standard Consumer Control usage"},
		{input: "KEYCODE_NOTIFICATION", messageSub: "notification center has no verified standard Consumer Control usage"},
	}

	for _, tc := range testCases {
		t.Run(tc.input, func(t *testing.T) {
			ctx, _ := WithToolError(context.Background())
			tool := &KeyboardTapTool{}

			out, err := tool.Call(ctx, fmt.Sprintf(`{"keys":["%s"]}`, tc.input))
			if err != nil {
				t.Fatalf("Call returned error: %v", err)
			}
			if !strings.Contains(out, strings.ToLower(tc.input)) || !strings.Contains(out, tc.messageSub) {
				t.Fatalf("output = %q, want alias and reason substring %q", out, tc.messageSub)
			}
			if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeInvalidArguments || got.Message != out {
				t.Fatalf("ToolError = %+v, want invalid_arguments with output message", got)
			}
		})
	}
}

func TestKeyboardTapRejectsAndroidExtensionWithoutAndroidDevice(t *testing.T) {
	tool := testKeyboardTapTool(t, testMNKOpts{touchscreen: true})
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{"keys":["KEYCODE_HOME"]}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, "android extension keyboard device is not configured") || !strings.Contains(out, "hid.android_keyboard_device") {
		t.Fatalf("output = %q, want android extension device error", out)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeModuleUnavailable || got.Message != out {
		t.Fatalf("ToolError = %+v, want module_unavailable with output message", got)
	}
}

func TestKeyboardTapRejectsMixedAndroidExtensionChord(t *testing.T) {
	tool := &KeyboardTapTool{}
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{"keys":["ctrl","KEYCODE_BACK"]}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, `Android extension key "android_back" cannot be combined`) {
		t.Fatalf("output = %q, want mixed-chord error", out)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeInvalidArguments || got.Message != out {
		t.Fatalf("ToolError = %+v, want invalid_arguments with output message", got)
	}
}

func readMouseReports(t *testing.T, dev *HIDDevice, path string) []mouseReport {
	t.Helper()

	dev.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data)%6 != 0 {
		t.Fatalf("mouse report data length = %d, want multiple of 6", len(data))
	}

	reports := make([]mouseReport, 0, len(data)/6)
	for i := 0; i < len(data); i += 6 {
		reports = append(reports, mouseReport{
			buttons: data[i],
			x:       binary.LittleEndian.Uint16(data[i+1 : i+3]),
			y:       binary.LittleEndian.Uint16(data[i+3 : i+5]),
			wheel:   int8(data[i+5]),
		})
	}
	return reports
}

type fakeHIDWriteCloser struct {
	closed     bool
	writeCount int
	writeErr   error
}

func (f *fakeHIDWriteCloser) Write(p []byte) (int, error) {
	f.writeCount++
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f *fakeHIDWriteCloser) Close() error {
	f.closed = true
	return nil
}

// timestampedHIDWriter records successful write times so provider-owned tap
// and swipe timing can be asserted in tests.
type timestampedHIDWriter struct {
	mu     sync.Mutex
	closed bool
	times  []time.Time
}

func (w *timestampedHIDWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.times = append(w.times, time.Now())
	return len(p), nil
}

func (w *timestampedHIDWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func (w *timestampedHIDWriter) writeTimes() []time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()
	out := make([]time.Time, len(w.times))
	copy(out, w.times)
	return out
}

func newTimedHIDDevice() (*HIDDevice, *timestampedHIDWriter) {
	w := &timestampedHIDWriter{}
	dev := &HIDDevice{
		path: "timed-hid",
		open: func(string) (io.WriteCloser, error) {
			return w, nil
		},
	}
	return dev, w
}

func TestTapPointerHoldsBetweenPressAndRelease(t *testing.T) {
	dev, w := newTimedHIDDevice()

	pc := testPointerController(dev, &pointerState{})
	if err := tapPointer(pc, 100, 200, 0x01); err != nil {
		t.Fatalf("tapPointer error: %v", err)
	}

	times := w.writeTimes()
	if len(times) != 2+touchReleaseReportCount {
		t.Fatalf("len(times) = %d, want %d (pre-move, press, repeated release)", len(times), 2+touchReleaseReportCount)
	}
	gap := times[2].Sub(times[1])
	if gap < 50*time.Millisecond {
		t.Fatalf("gap between press and release = %v, want >= 50ms", gap)
	}
}

func TestTapPointerSettlesCursorBeforePress(t *testing.T) {
	dev, w := newTimedHIDDevice()

	pc := testPointerController(dev, &pointerState{})
	if err := tapPointer(pc, 100, 200, 0x01); err != nil {
		t.Fatalf("tapPointer error: %v", err)
	}

	times := w.writeTimes()
	if len(times) != 2+touchReleaseReportCount {
		t.Fatalf("len(times) = %d, want %d", len(times), 2+touchReleaseReportCount)
	}
	settleGap := times[1].Sub(times[0])
	if settleGap < 60*time.Millisecond {
		t.Fatalf("pre-move to press gap = %v, want >= 60ms (cursor settle)", settleGap)
	}
}

func TestTouchGestureTapAcceptsHoldMs(t *testing.T) {
	dev, w := newTimedHIDDevice()
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"tap","point":{"x":500,"y":500},"hold_ms":150}`)
	if err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want ok", out)
	}

	times := w.writeTimes()
	if len(times) != 2+touchReleaseReportCount {
		t.Fatalf("len(times) = %d, want %d (pre-move, press, repeated release)", len(times), 2+touchReleaseReportCount)
	}
	gap := times[2].Sub(times[1])
	if gap < 130*time.Millisecond {
		t.Fatalf("press-to-release gap = %v, want >= 130ms", gap)
	}
}

func TestTouchGestureSwipeStartsMovingImmediately(t *testing.T) {
	dev, w := newTimedHIDDevice()
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":10,"y":500},"end":{"x":500,"y":500}}`)
	if err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want ok", out)
	}

	// The provider owns swipe timing, including the pre-move dwell.
	times := w.writeTimes()
	if len(times) < 4 {
		t.Fatalf("len(times) = %d, want >= 4", len(times))
	}
	gap := times[2].Sub(times[1])
	if gap > 30*time.Millisecond {
		t.Fatalf("swipe press-to-first-move gap = %v, want <= 30ms", gap)
	}
}

func TestTouchGestureSwipeDefaultsUseFastMotionAndSlowTailBeforeRelease(t *testing.T) {
	dev, w := newTimedHIDDevice()
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":100,"y":500},"end":{"x":900,"y":500}}`)
	if err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want ok", out)
	}

	// Writes: pre-move, press, 24 main motion steps, slow final motion, release.
	times := w.writeTimes()
	if len(times) <= defaultSwipeSteps+2+touchReleaseReportCount {
		t.Fatalf("len(times) = %d, want additional low-speed reports", len(times))
	}
	moveStart := 2
	firstRelease := len(times) - touchReleaseReportCount
	lastMove := 1 + defaultSwipeSteps
	moveDuration := times[lastMove].Sub(times[moveStart])
	if moveDuration < 250*time.Millisecond || moveDuration > 450*time.Millisecond {
		t.Fatalf("swipe move duration = %v, want about 300ms", moveDuration)
	}
	releaseDelay := times[firstRelease].Sub(times[lastMove])
	if releaseDelay < 95*time.Millisecond || releaseDelay > 250*time.Millisecond {
		t.Fatalf("swipe slow tail duration = %v, want about 100ms", releaseDelay)
	}
}

func TestTouchGestureSwipeRestoresTimingAndSteps(t *testing.T) {
	dev, w := newTimedHIDDevice()
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":100,"y":500},"end":{"x":900,"y":500},"duration_ms":20,"hold_before_ms":10,"hold_after_ms":10,"steps":4}`)
	if err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want ok", out)
	}

	times := w.writeTimes()
	if len(times) != 2+4+touchReleaseReportCount {
		t.Fatalf("len(times) = %d, want %d", len(times), 2+4+touchReleaseReportCount)
	}
	if gap := times[2].Sub(times[1]); gap < 7*time.Millisecond {
		t.Fatalf("press-to-first-move gap = %v, want hold_before_ms to be applied", gap)
	}
	firstRelease := len(times) - touchReleaseReportCount
	if gap := times[firstRelease].Sub(times[firstRelease-1]); gap < 7*time.Millisecond {
		t.Fatalf("final-move-to-release gap = %v, want hold_after_ms to be applied", gap)
	}
}

func TestTouchGestureExplicitBackSwipeStartsAtLeftPhysicalEdge(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":1,"y":500},"end":{"x":750,"y":500},"duration_ms":700}`)
	if err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want ok", out)
	}

	reports := readMouseReports(t, dev, path)
	if len(reports) != 2+defaultSwipeSteps+touchReleaseReportCount {
		t.Fatalf("len(reports) = %d, want provider default sequence", len(reports))
	}
	if reports[0].x > 100 {
		t.Fatalf("back start x = %d, want near left physical edge", reports[0].x)
	}
	if reports[0].y != uint16(absMouseMaxPos/2+1) {
		t.Fatalf("back start y = %d, want center", reports[0].y)
	}
	finalMove := 1 + defaultSwipeSteps
	if reports[finalMove].x < uint16(absMouseMaxPos*70/100) {
		t.Fatalf("back end x = %d, want a long swipe across the screen", reports[finalMove].x)
	}
	for i := finalMove + 1; i < len(reports); i++ {
		if reports[i].buttons != 0 {
			t.Fatalf("back release report %d buttons = %d, want 0", i-3, reports[i].buttons)
		}
	}
}

func TestTouchGestureExplicitHomeSwipeStartsAtBottomPhysicalEdge(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":500,"y":999},"end":{"x":500,"y":180},"duration_ms":700}`)
	if err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want ok", out)
	}

	reports := readMouseReports(t, dev, path)
	if len(reports) != 2+defaultSwipeSteps+touchReleaseReportCount {
		t.Fatalf("len(reports) = %d, want provider default sequence", len(reports))
	}
	if reports[0].y < uint16(absMouseMaxPos*995/1000) {
		t.Fatalf("home start y = %d, want near bottom physical edge", reports[0].y)
	}
	if reports[0].x != uint16(absMouseMaxPos/2+1) {
		t.Fatalf("home start x = %d, want center", reports[0].x)
	}
	finalMove := 1 + defaultSwipeSteps
	if reports[finalMove].y > uint16(absMouseMaxPos/4) {
		t.Fatalf("home end y = %d, want a long upward swipe", reports[finalMove].y)
	}
	for i := finalMove + 1; i < len(reports); i++ {
		if reports[i].buttons != 0 {
			t.Fatalf("home release report %d buttons = %d, want 0", i-3, reports[i].buttons)
		}
	}
}

func TestTouchGestureSchemaRequiresNamedCoordinateObjectsAndValidExamples(t *testing.T) {
	schema := (&TouchGestureTool{}).ArgsSchema()
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", schema["properties"])
	}
	for _, removed := range []string{"distance", "anchor", "strength", "pause_ms"} {
		if _, ok := properties[removed]; ok {
			t.Fatalf("touch_gesture schema still exposes removed field %q", removed)
		}
	}
	if _, ok := properties["hold_ms"]; !ok {
		t.Fatal("touch_gesture schema must retain hold_ms for tap and long_press")
	}
	for _, added := range []string{"direction", "speed", "duration_ms", "hold_before_ms", "hold_after_ms", "steps"} {
		if _, ok := properties[added]; !ok {
			t.Fatalf("touch_gesture schema missing unified swipe field %q", added)
		}
	}
	for _, name := range []string{"point", "start", "end"} {
		coordinate, ok := properties[name].(map[string]any)
		if !ok {
			t.Fatalf("%s schema = %#v", name, properties[name])
		}
		if coordinate["type"] != "object" || coordinate["additionalProperties"] != false {
			t.Fatalf("%s schema is not a strict object: %#v", name, coordinate)
		}
		required, ok := coordinate["required"].([]string)
		if !ok || !slices.Equal(required, []string{"x", "y"}) {
			t.Fatalf("%s required = %#v, want x and y", name, coordinate["required"])
		}
	}

	examples, ok := schema["examples"].([]map[string]any)
	if !ok || len(examples) != 5 {
		t.Fatalf("schema examples = %#v, want tap, both swipe forms, and both drag phases", schema["examples"])
	}
	encoded, err := json.Marshal(examples)
	if err != nil || !json.Valid(encoded) {
		t.Fatalf("schema examples are not valid JSON: encoded=%s err=%v", encoded, err)
	}
	for _, want := range []string{
		`{"point":{"x":500,"y":500},"type":"tap"}`,
		`{"direction":"up","start":{"x":500,"y":900},"type":"swipe"}`,
		`{"end":{"x":500,"y":300},"start":{"x":500,"y":900},"type":"swipe"}`,
		`"type":"drag_start"`,
		`"type":"drag_release"`,
	} {
		if !strings.Contains(string(encoded), want) {
			t.Fatalf("schema examples missing %q: %s", want, encoded)
		}
	}
}

func TestTouchGestureSchemaExposesAtomicActions(t *testing.T) {
	schema := (&TouchGestureTool{}).ArgsSchema()
	// Anthropic rejects oneOf, allOf, and anyOf at the top level of a tool
	// input_schema, and the requirement is stated in the description instead so
	// every provider receives the same schema.
	for _, key := range []string{"oneOf", "allOf", "anyOf"} {
		if _, found := schema[key]; found {
			t.Fatalf("schema declares top-level %q, which Anthropic rejects: %#v", key, schema[key])
		}
	}
	description, _ := schema["description"].(string)
	if !strings.Contains(description, "either type or actions") {
		t.Fatalf("schema description no longer states the type-or-actions requirement: %q", description)
	}
	properties, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema properties = %#v", schema["properties"])
	}
	actions, ok := properties["actions"].(map[string]any)
	if !ok || actions["type"] != "array" || actions["minItems"] != 1 || actions["maxItems"] != 128 {
		t.Fatalf("actions schema = %#v", properties["actions"])
	}
	items, ok := actions["items"].(map[string]any)
	if !ok {
		t.Fatalf("actions items schema = %#v", actions["items"])
	}
	itemProperties, ok := items["properties"].(map[string]any)
	if !ok {
		t.Fatalf("atomic item properties = %#v", items["properties"])
	}
	action, ok := itemProperties["action"].(map[string]any)
	if !ok || !slices.Contains(action["enum"].([]string), "touch_down") || !slices.Contains(action["enum"].([]string), "touch_up") {
		t.Fatalf("atomic action enum = %#v", itemProperties["action"])
	}
	if _, ok := itemProperties["speed"].(map[string]any); !ok {
		t.Fatalf("atomic speed schema = %#v", itemProperties["speed"])
	}
}

func TestTouchGestureSchemaUsesUnifiedTypesOnEveryPlatform(t *testing.T) {
	for _, deviceType := range []string{"windows", "Android", "iOS"} {
		tool := &TouchGestureTool{}
		tool.SetDeviceTypeFunc(func() string { return deviceType })
		types := stringEnumPropertyValues(t, tool.ArgsSchema(), "type")
		for _, want := range []string{"tap", "double_tap", "long_press", "drag_start", "drag_release", "swipe"} {
			if _, ok := types[want]; !ok {
				t.Fatalf("%s touch_gesture schema missing %q: %v", deviceType, want, types)
			}
		}
		for _, notWant := range []string{"drag", "swipe_up", "swipe_down", "swipe_left", "swipe_right", "back", "home"} {
			if _, ok := types[notWant]; ok {
				t.Fatalf("%s touch_gesture schema exposed retired type %q: %v", deviceType, notWant, types)
			}
		}
	}
}

func TestTouchGestureIgnoresRetiredCoordSpaceField(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"swipe","start":{"x":1,"y":500},"end":{"x":750,"y":500},"coord_space":"normalized"}`)
	if err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want ok", out)
	}

	reports := readMouseReports(t, dev, path)
	if len(reports) == 0 {
		t.Fatal("expected gesture reports")
	}
}

func TestTouchGestureIgnoresRetiredCoordSpaceFieldInPoints(t *testing.T) {
	tests := []struct {
		name  string
		input string
	}{
		{
			name:  "point",
			input: `{"type":"tap","point":{"x":500,"y":500,"coord_space":"pixel"}}`,
		},
		{
			name:  "start",
			input: `{"type":"swipe","start":{"x":500,"y":800,"coord_space":"pixel"},"end":{"x":500,"y":200}}`,
		},
		{
			name:  "end",
			input: `{"type":"swipe","start":{"x":500,"y":800},"end":{"x":500,"y":200,"coord_space":"pixel"}}`,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dev, path := newTestHIDDevice(t)
			tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

			out, err := tool.Call(context.Background(), tt.input)
			if err != nil {
				t.Fatalf("Call error: %v", err)
			}
			if out != "ok" {
				t.Fatalf("output = %q, want ok", out)
			}
			if reports := readMouseReports(t, dev, path); len(reports) == 0 {
				t.Fatal("expected gesture reports")
			}
		})
	}
}

func TestMouseMoveIgnoresRetiredCoordSpaceField(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testMouseMoveTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})
	out, err := tool.Call(context.Background(), `{"x":500,"y":500,"coord_space":"normalized"}`)
	if err != nil {
		t.Fatalf("Call error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("output = %q, want ok", out)
	}
	if reports := readMouseReports(t, dev, path); len(reports) != 1 {
		t.Fatalf("len(reports) = %d, want one move", len(reports))
	}
}

func TestTouchGestureDragStartAndReleaseTiming(t *testing.T) {
	dev, w := newTimedHIDDevice()
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"drag_start","point":{"x":100,"y":100}}`)
	if err != nil {
		t.Fatalf("drag_start Call error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("drag_start output = %q, want ok", out)
	}

	times := w.writeTimes()
	dragStartWrites := 2 + defaultSwipeSteps // settle, press, then interpolated activation moves
	if len(times) != dragStartWrites {
		t.Fatalf("drag_start writes = %d, want %d", len(times), dragStartWrites)
	}
	gap := times[2].Sub(times[1])
	if gap < 450*time.Millisecond {
		t.Fatalf("drag_start press-to-first-activation-move gap = %v, want about 500ms", gap)
	}
	if moveDuration := times[dragStartWrites-1].Sub(times[2]); moveDuration < 390*time.Millisecond {
		t.Fatalf("drag_start activation move duration = %v, want interpolated motion over about 400ms", moveDuration)
	}

	out, err = tool.Call(context.Background(), `{"type":"drag_release","point":{"x":900,"y":900}}`)
	if err != nil {
		t.Fatalf("drag_release Call error: %v", err)
	}
	if out != "ok" {
		t.Fatalf("drag_release output = %q, want ok", out)
	}
	times = w.writeTimes()
	if len(times) != dragStartWrites+1+touchReleaseReportCount {
		t.Fatalf("total writes = %d, want direct target move plus repeated release", len(times))
	}
	firstRelease := len(times) - touchReleaseReportCount
	if gap := times[firstRelease].Sub(times[firstRelease-1]); gap < 180*time.Millisecond {
		t.Fatalf("drag_release target-to-release gap = %v, want about 200ms", gap)
	}
}

func TestDragPointerReleasesOnMoveError(t *testing.T) {
	failAfter := 3 // settle + press + first move = 3rd write fails
	writer := &countingFailWriter{failAt: failAfter}
	dev := &HIDDevice{
		path: "fail-hid",
		open: func(string) (io.WriteCloser, error) {
			return writer, nil
		},
	}

	start := resolvedPointerPoint{x: 100, y: 100}
	end := resolvedPointerPoint{x: 200, y: 200}
	pc := testPointerController(dev, &pointerState{})
	err := dragPointer(pc, start, end, 0x01, 0, 0, 0, 3)
	if err == nil {
		t.Fatal("expected error from dragPointer")
	}
	// The release report must have been attempted even though a move failed.
	if writer.writeCount < failAfter+touchReleaseReportCount {
		t.Fatalf("writeCount = %d, expected at least %d (release must be attempted after move failure)", writer.writeCount, failAfter+touchReleaseReportCount)
	}
}

type countingFailWriter struct {
	mu         sync.Mutex
	writeCount int
	failAt     int
	closed     bool
}

func (w *countingFailWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.writeCount++
	if w.writeCount == w.failAt {
		return 0, errors.New("simulated write failure")
	}
	return len(p), nil
}

func (w *countingFailWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.closed = true
	return nil
}

func TestScreenStateDimensionsWithAge(t *testing.T) {
	screen := &screen.ScreenState{}

	if _, _, _, ok := screen.DimensionsWithAge(); ok {
		t.Fatal("expected ok=false before Update")
	}

	screen.Update(800, 1600)
	w, h, age, ok := screen.DimensionsWithAge()
	if !ok {
		t.Fatal("expected ok=true after Update")
	}
	if w != 800 || h != 1600 {
		t.Fatalf("dims = %dx%d, want 800x1600", w, h)
	}
	if age > time.Second {
		t.Fatalf("fresh age = %v, want < 1s", age)
	}
}

func TestTouchGestureSwipeRejectsPointInsteadOfStartEnd(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"swipe","point":{"x":500,"y":500}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, "start is required for swipe") {
		t.Fatalf("output = %q, want swipe start error", out)
	}

	reports := readMouseReports(t, dev, path)
	if len(reports) != 0 {
		t.Fatalf("len(reports) = %d, want no HID writes", len(reports))
	}
}

func TestTouchGestureRejectsDragReleaseWithoutStart(t *testing.T) {
	dev, path := newTestHIDDevice(t)
	tool := testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev})

	out, err := tool.Call(context.Background(), `{"type":"drag_release","point":{"x":313,"y":513}}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, "drag_release requires an active drag_start") {
		t.Fatalf("output = %q, want inactive drag error", out)
	}

	reports := readMouseReports(t, dev, path)
	if len(reports) != 0 {
		t.Fatalf("len(reports) = %d, want no HID writes", len(reports))
	}
}
