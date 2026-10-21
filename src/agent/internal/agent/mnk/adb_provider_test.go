package mnk

import (
	"context"
	"errors"
	"math"
	"strings"
	"testing"
)

type adbTestRunner struct {
	commands [][]string
	sdk      string
}

func (r *adbTestRunner) run(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
	r.commands = append(r.commands, append([]string(nil), args...))
	joined := strings.Join(args, " ")
	switch {
	case strings.HasSuffix(joined, "shell getprop ro.build.version.sdk"):
		return []byte(r.sdk), nil, nil
	case strings.HasSuffix(joined, "shell wm size"):
		return []byte("Physical size: 1000x1000"), nil, nil
	default:
		return nil, nil, nil
	}
}

func newTestADBProvider(t *testing.T, runner *adbTestRunner) *ADBProvider {
	t.Helper()
	t.Setenv("AIDEN_ADB_PATH", "/fake/adb")
	t.Setenv("AIDEN_ADB_SERIAL", "serial-test")
	return NewADBProvider(nil, nil, runner.run)
}

func TestADBProviderKeypressGatesKeyCombinationDuration(t *testing.T) {
	for _, test := range []struct {
		name string
		sdk  string
		want []string
		err  ErrorKind
	}{
		{name: "android11", sdk: "30", want: []string{"-s", "serial-test", "shell", "input", "keycombination", "KEYCODE_CTRL_LEFT", "KEYCODE_A"}},
		{name: "android12", sdk: "31", want: []string{"-s", "serial-test", "shell", "input", "keycombination", "-t", "50", "KEYCODE_CTRL_LEFT", "KEYCODE_A"}},
		{name: "unsupported", sdk: "29", err: ErrModuleUnavailable},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &adbTestRunner{sdk: test.sdk}
			provider := newTestADBProvider(t, runner)
			err := provider.Keypress(context.Background(), []string{"ctrl", "a"})
			if test.err != "" {
				if got := AsError(err); got == nil || got.Kind != test.err {
					t.Fatalf("Keypress() error = %v, want %s", err, test.err)
				}
				return
			}
			if err != nil {
				t.Fatalf("Keypress() error = %v", err)
			}
			if len(runner.commands) != 2 {
				t.Fatalf("commands = %#v, want API query and key combination", runner.commands)
			}
			got := runner.commands[1]
			if !stringSliceEqual(got, test.want) {
				t.Fatalf("key combination = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestADBKeyboardAliasesUseKeyboardKeys(t *testing.T) {
	provider := &ADBProvider{}
	for key, want := range map[string]string{
		"esc":    "KEYCODE_ESCAPE",
		"escape": "KEYCODE_ESCAPE",
		"home":   "KEYCODE_MOVE_HOME",
	} {
		got, err := provider.resolveKeycode(key)
		if err != nil {
			t.Fatalf("resolveKeycode(%q): %v", key, err)
		}
		if got != want {
			t.Errorf("resolveKeycode(%q) = %q, want %q", key, got, want)
		}
	}
	if got, err := provider.resolveKeycode("android_home"); err != nil || got != "KEYCODE_HOME" {
		t.Errorf("resolveKeycode(android_home) = %q, %v; want KEYCODE_HOME", got, err)
	}
}

func TestADBScreenClientRejectsAmbiguousDevices(t *testing.T) {
	runner := &adbTestRunner{}
	client := NewADBScreenClient()
	provider := NewADBProvider(nil, client, func(ctx context.Context, path string, args ...string) ([]byte, []byte, error) {
		if len(args) == 1 && args[0] == "devices" {
			return []byte("List of devices attached\nfirst\tdevice\nsecond\tdevice\n"), nil, nil
		}
		return runner.run(ctx, path, args...)
	})
	t.Setenv("AIDEN_ADB_SERIAL", "")
	t.Setenv("ANDROID_SERIAL", "")
	if _, err := client.ResolveSerial(context.Background(), "/fake/adb"); err == nil || !strings.Contains(err.Error(), "multiple adb devices") {
		t.Fatalf("ResolveSerial() error = %v, want multiple-device error", err)
	}
	_ = provider
}

func TestADBProviderScrollHorizontalDirection(t *testing.T) {
	for _, test := range []struct {
		name       string
		delta      int
		wantStartX string
		wantEndX   string
	}{
		{name: "left", delta: -1, wantStartX: "400", wantEndX: "599"},
		{name: "right", delta: 1, wantStartX: "599", wantEndX: "400"},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := &adbTestRunner{sdk: "31"}
			provider := newTestADBProvider(t, runner)
			if err := provider.Scroll(context.Background(), test.delta, 0); err != nil {
				t.Fatalf("Scroll() error = %v", err)
			}
			if len(runner.commands) != 2 {
				t.Fatalf("commands = %#v, want wm size and swipe", runner.commands)
			}
			got := runner.commands[1]
			if len(got) < 10 || got[len(got)-6] != "swipe" {
				t.Fatalf("swipe command = %#v", got)
			}
			if got[len(got)-5] != test.wantStartX || got[len(got)-4] != "500" || got[len(got)-3] != test.wantEndX || got[len(got)-2] != "500" {
				t.Fatalf("swipe command = %#v, want horizontal direction", got)
			}
		})
	}
}

func TestADBProviderSwipeUsesDefaultDuration(t *testing.T) {
	runner := &adbTestRunner{sdk: "31"}
	provider := newTestADBProvider(t, runner)
	if err := provider.Swipe(context.Background(), [][2]float64{{700, 500}, {300, 500}}, ButtonLeft); err != nil {
		t.Fatalf("Swipe() error = %v", err)
	}
	if len(runner.commands) != 3 {
		t.Fatalf("commands = %#v, want wm size, discovery, and motion script", runner.commands)
	}
	script := runner.commands[2][3]
	if adbScriptSleepMillis(t, script) != 400 || !strings.Contains(script, "input touchscreen motionevent UP 300 500") {
		t.Fatalf("motion script = %q, want 300ms main motion plus 100ms moving release tail", script)
	}
}

func TestADBProviderDragStartAndReleaseUseSeparateRawPrograms(t *testing.T) {
	getevent := `add device 1: /dev/input/event3
  name:     "goodix_ts0"
  events:
    KEY (0001): BTN_TOOL_FINGER BTN_TOUCH
    ABS (0003): ABS_X                 : value 0, min 0, max 999, fuzz 0, flat 0, resolution 0
                ABS_Y                 : value 0, min 0, max 1999, fuzz 0, flat 0, resolution 0
                ABS_MT_SLOT           : value 0, min 0, max 9, fuzz 0, flat 0, resolution 0
                ABS_MT_POSITION_X     : value 0, min 0, max 999, fuzz 0, flat 0, resolution 0
                ABS_MT_POSITION_Y     : value 0, min 0, max 1999, fuzz 0, flat 0, resolution 0
                ABS_MT_TRACKING_ID    : value 0, min 0, max 65535, fuzz 0, flat 0, resolution 0
  input props:
    INPUT_PROP_DIRECT
`
	var commands [][]string
	provider := NewADBProvider(nil, nil, func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		commands = append(commands, append([]string(nil), args...))
		if strings.HasSuffix(strings.Join(args, " "), "shell getevent -lp") {
			return []byte(getevent), nil, nil
		}
		return nil, nil, nil
	})
	t.Setenv("AIDEN_ADB_PATH", "/fake/adb")
	t.Setenv("AIDEN_ADB_SERIAL", "serial-test")

	if err := provider.DragStart(context.Background(), 500, 500, ButtonLeft); err != nil {
		t.Fatalf("DragStart() error = %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("commands after start = %#v, want discovery and start program", commands)
	}
	startScript := commands[1][3]
	for _, want := range []string{"sleep 0.500", "sendevent /dev/input/event3 3 53 699", "sendevent /dev/input/event3 1 330 1"} {
		if !strings.Contains(startScript, want) {
			t.Errorf("drag_start script missing %q:\n%s", want, startScript)
		}
	}
	if got := strings.Count(startScript, "sendevent /dev/input/event3 3 53 "); got != 1+adbTouchMoveSteps {
		t.Errorf("drag_start X reports = %d, want touch-down plus %d interpolated moves", got, adbTouchMoveSteps)
	}
	if strings.Contains(startScript, "sendevent /dev/input/event3 3 57 -1") {
		t.Fatalf("drag_start released the contact:\n%s", startScript)
	}
	if err := provider.Click(context.Background(), 100, 100, ButtonLeft, 0); AsError(err) == nil {
		t.Fatalf("Click while dragging error = %v, want invalid arguments", err)
	}
	if err := provider.DragRelease(context.Background(), 800, 200); err != nil {
		t.Fatalf("DragRelease() error = %v", err)
	}
	if len(commands) != 3 {
		t.Fatalf("commands after release = %#v, want one release program", commands)
	}
	releaseScript := commands[2][3]
	for _, want := range []string{"sendevent /dev/input/event3 3 53 799", "sendevent /dev/input/event3 3 54 400", "sleep 0.200", "sendevent /dev/input/event3 3 57 -1"} {
		if !strings.Contains(releaseScript, want) {
			t.Errorf("drag_release script missing %q:\n%s", want, releaseScript)
		}
	}
}

func TestADBProviderDragStartFailureAttemptsRawTouchUp(t *testing.T) {
	getevent := `add device 1: /dev/input/event3
  name:     "goodix_ts0"
  events:
    KEY (0001): BTN_TOOL_FINGER BTN_TOUCH
    ABS (0003): ABS_X                 : value 0, min 0, max 999, fuzz 0, flat 0, resolution 0
                ABS_Y                 : value 0, min 0, max 1999, fuzz 0, flat 0, resolution 0
                ABS_MT_SLOT           : value 0, min 0, max 9, fuzz 0, flat 0, resolution 0
                ABS_MT_POSITION_X     : value 0, min 0, max 999, fuzz 0, flat 0, resolution 0
                ABS_MT_POSITION_Y     : value 0, min 0, max 1999, fuzz 0, flat 0, resolution 0
                ABS_MT_TRACKING_ID    : value 0, min 0, max 65535, fuzz 0, flat 0, resolution 0
  input props:
    INPUT_PROP_DIRECT
`
	var commands [][]string
	provider := NewADBProvider(nil, nil, func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		commands = append(commands, append([]string(nil), args...))
		joined := strings.Join(args, " ")
		if strings.HasSuffix(joined, "shell getevent -lp") {
			return []byte(getevent), nil, nil
		}
		if strings.Contains(joined, "sleep 0.500") {
			return nil, nil, errors.New("simulated raw start failure")
		}
		return nil, nil, nil
	})
	t.Setenv("AIDEN_ADB_PATH", "/fake/adb")
	t.Setenv("AIDEN_ADB_SERIAL", "serial-test")

	err := provider.DragStart(context.Background(), 500, 500, ButtonLeft)
	if err == nil || !strings.Contains(err.Error(), "simulated raw start failure") {
		t.Fatalf("DragStart() error = %v, want raw start failure", err)
	}
	if len(commands) != 3 {
		t.Fatalf("commands = %#v, want discovery, failed start, and cleanup", commands)
	}
	cleanupScript := commands[2][3]
	for _, want := range []string{
		"sendevent /dev/input/event3 3 57 -1",
		"sendevent /dev/input/event3 1 330 0",
		"sendevent /dev/input/event3 1 325 0",
	} {
		if !strings.Contains(cleanupScript, want) {
			t.Errorf("cleanup script missing %q:\n%s", want, cleanupScript)
		}
	}
	if provider.dragActive {
		t.Fatal("failed drag_start must not record an active drag after successful cleanup")
	}
}

func TestParseADBTouchDevicesPrefersPhysicalTouchscreen(t *testing.T) {
	output := `add device 1: /dev/input/event7
  name:     "Aiden Aiden HID+ECM"
  events:
    KEY (0001): BTN_TOUCH
    ABS (0003): ABS_X                 : value 0, min 0, max 32767, fuzz 0, flat 0, resolution 0
                ABS_Y                 : value 0, min 0, max 32767, fuzz 0, flat 0, resolution 0
                ABS_MT_SLOT           : value 0, min 0, max 9, fuzz 0, flat 0, resolution 0
                ABS_MT_POSITION_X     : value 0, min 0, max 32767, fuzz 0, flat 0, resolution 0
                ABS_MT_POSITION_Y     : value 0, min 0, max 32767, fuzz 0, flat 0, resolution 0
                ABS_MT_TRACKING_ID    : value 0, min 0, max 65535, fuzz 0, flat 0, resolution 0
  input props:
    INPUT_PROP_DIRECT
add device 2: /dev/input/event3
  name:     "goodix_ts0"
  events:
    KEY (0001): BTN_TOOL_FINGER BTN_TOUCH
    ABS (0003): ABS_X                 : value 0, min 0, max 1079, fuzz 0, flat 0, resolution 0
                ABS_Y                 : value 0, min 0, max 2399, fuzz 0, flat 0, resolution 0
                ABS_MT_SLOT           : value 0, min 0, max 9, fuzz 0, flat 0, resolution 0
                ABS_MT_POSITION_X     : value 0, min 0, max 1079, fuzz 0, flat 0, resolution 0
                ABS_MT_POSITION_Y     : value 0, min 0, max 2399, fuzz 0, flat 0, resolution 0
                ABS_MT_TRACKING_ID    : value 0, min 0, max 65535, fuzz 0, flat 0, resolution 0
  input props:
    INPUT_PROP_DIRECT
`
	devices := parseADBTouchDevices(output)
	if len(devices) != 2 {
		t.Fatalf("devices = %#v, want two touchscreen candidates", devices)
	}
	got := devices[0]
	if got.path != "/dev/input/event3" || got.name != "goodix_ts0" {
		t.Fatalf("selected device = %#v, want physical goodix touchscreen", got)
	}
	if got.xMin != 0 || got.xMax != 1079 || got.yMin != 0 || got.yMax != 2399 {
		t.Fatalf("selected ranges = %#v, want 1079x2399 maxima", got)
	}
	if !got.mt || !got.protocolB || !got.hasAbsXY || !got.hasTrackingID || !got.hasBtnTouch || !got.hasToolFinger {
		t.Fatalf("selected capabilities = %#v, want protocol-B direct touchscreen", got)
	}
}

func TestBuildADBTouchScriptKeepsContactAcrossWaitAndMove(t *testing.T) {
	device := adbTouchDevice{
		path:          "/dev/input/event3",
		xMin:          0,
		xMax:          1079,
		yMin:          0,
		yMax:          2399,
		mt:            true,
		protocolB:     true,
		hasAbsXY:      true,
		hasTrackingID: true,
		hasTouchMajor: true,
		hasPressure:   true,
		hasToolType:   true,
		hasBtnTouch:   true,
		hasToolFinger: true,
	}
	actions := []TouchAction{
		{Type: "touch_down", Point: &Point{X: 500, Y: 800}},
		{Type: "wait", DurationMs: 80},
		{Type: "move_to", Point: &Point{X: 500, Y: 200}},
		{Type: "touch_up"},
	}
	script, durationMs, err := buildADBTouchScript(device, actions, 42)
	if err != nil {
		t.Fatalf("buildADBTouchScript() error = %v", err)
	}
	if durationMs != 80 {
		t.Fatalf("duration = %d, want 80", durationMs)
	}
	for _, want := range []string{
		"sendevent /dev/input/event3 3 47 0",
		"sendevent /dev/input/event3 3 57 42",
		"sendevent /dev/input/event3 3 53 540",
		"sendevent /dev/input/event3 3 54 1919",
		"sendevent /dev/input/event3 1 330 1",
		"sleep 0.080",
		"sendevent /dev/input/event3 3 54 480",
		"sendevent /dev/input/event3 3 57 -1",
		"sendevent /dev/input/event3 1 330 0",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Index(script, "sleep 0.080") > strings.LastIndex(script, "sendevent /dev/input/event3 3 57 -1") {
		t.Fatalf("touch was released before wait completed:\n%s", script)
	}
}

func TestAppendADBTouchPositionProtocolAEmitsMTReportBeforeSynReport(t *testing.T) {
	device := adbTouchDevice{path: "/dev/input/event4", mt: true}
	var script strings.Builder

	appendADBTouchPosition(&script, device, 123, 456, true)

	want := "sendevent /dev/input/event4 3 53 123\n" +
		"sendevent /dev/input/event4 3 54 456\n" +
		"sendevent /dev/input/event4 0 2 0\n" +
		"sendevent /dev/input/event4 0 0 0\n"
	if got := script.String(); got != want {
		t.Fatalf("protocol-A move script =\n%s\nwant:\n%s", got, want)
	}
}

func TestADBProviderTouchActionsDiscoversDeviceAndRunsSingleScript(t *testing.T) {
	getevent := `add device 1: /dev/input/event3
  name:     "goodix_ts0"
  events:
    KEY (0001): BTN_TOOL_FINGER BTN_TOUCH
    ABS (0003): ABS_X                 : value 0, min 0, max 999, fuzz 0, flat 0, resolution 0
                ABS_Y                 : value 0, min 0, max 1999, fuzz 0, flat 0, resolution 0
                ABS_MT_SLOT           : value 0, min 0, max 9, fuzz 0, flat 0, resolution 0
                ABS_MT_POSITION_X     : value 0, min 0, max 999, fuzz 0, flat 0, resolution 0
                ABS_MT_POSITION_Y     : value 0, min 0, max 1999, fuzz 0, flat 0, resolution 0
                ABS_MT_TRACKING_ID    : value 0, min 0, max 65535, fuzz 0, flat 0, resolution 0
  input props:
    INPUT_PROP_DIRECT
`
	var commands [][]string
	provider := NewADBProvider(nil, nil, func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		commands = append(commands, append([]string(nil), args...))
		joined := strings.Join(args, " ")
		if strings.HasSuffix(joined, "shell getevent -lp") {
			return []byte(getevent), nil, nil
		}
		return nil, nil, nil
	})
	t.Setenv("AIDEN_ADB_PATH", "/fake/adb")
	t.Setenv("AIDEN_ADB_SERIAL", "serial-test")
	actions := []TouchAction{
		{Type: "touch_down", Point: &Point{X: 100, Y: 800}},
		{Type: "move_to", Point: &Point{X: 100, Y: 200}},
		{Type: "touch_up"},
	}
	if err := provider.TouchActions(context.Background(), actions); err != nil {
		t.Fatalf("TouchActions() error = %v", err)
	}
	if len(commands) != 2 {
		t.Fatalf("commands = %#v, want one getevent discovery and one event script", commands)
	}
	if got := commands[1]; len(got) != 4 || !strings.HasPrefix(got[3], "sh -c '") || !strings.Contains(got[3], "sendevent /dev/input/event3") {
		t.Fatalf("event script command = %#v", got)
	}
}

func TestShellSingleQuotePreservesScriptAsOneRemoteCommand(t *testing.T) {
	got := shellSingleQuote("printf '%s' hello world")
	want := `'printf '"'"'%s'"'"' hello world'`
	if got != want {
		t.Fatalf("shellSingleQuote() = %q, want %q", got, want)
	}
}

func TestBuildADBInputMotionScriptPreservesAtomicSequence(t *testing.T) {
	actions := []TouchAction{
		{Type: "touch_down", Point: &Point{X: 500, Y: 800}},
		{Type: "wait", DurationMs: 80},
		{Type: "move_to", Point: &Point{X: 500, Y: 200}},
		{Type: "touch_up"},
	}
	script, durationMs, err := buildADBInputMotionScript(actions, adbInputScreenSize{width: 1080, height: 2400})
	if err != nil {
		t.Fatalf("buildADBInputMotionScript() error = %v", err)
	}
	if durationMs != 80 {
		t.Fatalf("duration = %d, want 80", durationMs)
	}
	for _, want := range []string{
		"input touchscreen motionevent DOWN 540 1919",
		"sleep 0.080",
		"input touchscreen motionevent MOVE 540 480",
		"input touchscreen motionevent UP 540 480",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

func TestADBProviderTouchActionsFallsBackToInputMotionEvent(t *testing.T) {
	getevent := `add device 1: /dev/input/event3
  name:     "goodix_ts0"
  events:
    KEY (0001): BTN_TOUCH
    ABS (0003): ABS_X                 : value 0, min 0, max 1079, fuzz 0, flat 0, resolution 0
                ABS_Y                 : value 0, min 0, max 2399, fuzz 0, flat 0, resolution 0
                ABS_MT_SLOT           : value 0, min 0, max 9, fuzz 0, flat 0, resolution 0
                ABS_MT_POSITION_X     : value 0, min 0, max 1079, fuzz 0, flat 0, resolution 0
                ABS_MT_POSITION_Y     : value 0, min 0, max 2399, fuzz 0, flat 0, resolution 0
                ABS_MT_TRACKING_ID    : value 0, min 0, max 65535, fuzz 0, flat 0, resolution 0
  input props:
    INPUT_PROP_DIRECT
`
	var commands [][]string
	provider := NewADBProvider(nil, nil, func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		commands = append(commands, append([]string(nil), args...))
		joined := strings.Join(args, " ")
		switch {
		case strings.HasSuffix(joined, "shell getevent -lp"):
			return []byte(getevent), nil, nil
		case strings.Contains(joined, "sendevent"):
			return nil, []byte("sendevent: /dev/input/event3: Permission denied"), errors.New("exit status 1")
		case strings.HasSuffix(joined, "shell wm size"):
			return []byte("Physical size: 1080x2400"), nil, nil
		case strings.Contains(joined, "input touchscreen motionevent"):
			return nil, nil, nil
		default:
			return nil, nil, nil
		}
	})
	t.Setenv("AIDEN_ADB_PATH", "/fake/adb")
	t.Setenv("AIDEN_ADB_SERIAL", "serial-test")
	err := provider.TouchActions(context.Background(), []TouchAction{
		{Type: "touch_down", Point: &Point{X: 500, Y: 800}},
		{Type: "wait", DurationMs: 80},
		{Type: "move_to", Point: &Point{X: 500, Y: 200}},
		{Type: "touch_up"},
	})
	if err != nil {
		t.Fatalf("TouchActions() fallback error = %v", err)
	}
	if len(commands) != 4 {
		t.Fatalf("commands = %#v, want getevent, sendevent, wm size, and motionevent", commands)
	}
	if got := commands[3]; len(got) != 4 || !strings.Contains(got[3], "input touchscreen motionevent DOWN 540 1919") || !strings.Contains(got[3], "input touchscreen motionevent UP 540 480") {
		t.Fatalf("motionevent fallback command = %#v", got)
	}
	if err := provider.TouchActions(context.Background(), []TouchAction{
		{Type: "touch_down", Point: &Point{X: 500, Y: 500}},
		{Type: "touch_up"},
	}); err != nil {
		t.Fatalf("cached motionevent fallback error = %v", err)
	}
	if len(commands) != 5 || !strings.Contains(commands[4][3], "input touchscreen motionevent") {
		t.Fatalf("cached fallback retried raw discovery/injection: %#v", commands)
	}
}

func TestADBProviderTouchActionsReportsPermissionFailureAsUnavailable(t *testing.T) {
	provider := NewADBProvider(nil, nil, func(_ context.Context, _ string, args ...string) ([]byte, []byte, error) {
		joined := strings.Join(args, " ")
		if strings.HasSuffix(joined, "shell getevent -lp") {
			return []byte(`add device 1: /dev/input/event3
  name:     "touchscreen"
  events:
    KEY (0001): BTN_TOUCH
    ABS (0003): ABS_X                 : value 0, min 0, max 999, fuzz 0, flat 0, resolution 0
                ABS_Y                 : value 0, min 0, max 1999, fuzz 0, flat 0, resolution 0
  input props:
    INPUT_PROP_DIRECT
`), nil, nil
		}
		return nil, []byte("sendevent: /dev/input/event3: Permission denied"), errors.New("exit status 1")
	})
	t.Setenv("AIDEN_ADB_PATH", "/fake/adb")
	t.Setenv("AIDEN_ADB_SERIAL", "serial-test")
	err := provider.TouchActions(context.Background(), []TouchAction{
		{Type: "touch_down", Point: &Point{X: 500, Y: 500}},
		{Type: "touch_up"},
	})
	if got := AsError(err); got == nil || got.Kind != ErrModuleUnavailable {
		t.Fatalf("TouchActions() error = %v, want module unavailable", err)
	}
}

func stringSliceEqual(a, b []string) bool {
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

func TestADBProviderParseWMSizePrefersOverrideSize(t *testing.T) {
	size, ok := (&ADBProvider{}).parseWMSize("Physical size: 1080x2400\nOverride size: 720x1600\n")
	if !ok {
		t.Fatal("parseWMSize ok = false")
	}
	if size.width != 720 || size.height != 1600 {
		t.Fatalf("size = %+v, want 720x1600", size)
	}
}

func TestADBProviderNormalizedToPixelRejectsNonFiniteCoordinates(t *testing.T) {
	provider := &ADBProvider{}
	for _, value := range []float64{math.NaN(), math.Inf(1), math.Inf(-1)} {
		if _, _, err := provider.normalizedToPixel(context.Background(), value, 500); err == nil || !strings.Contains(err.Error(), "finite") {
			t.Fatalf("normalizedToPixel(%v, 500) error = %v, want finite-coordinate error", value, err)
		}
	}
}
