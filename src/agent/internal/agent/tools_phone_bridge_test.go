package agent

import (
	"aiden-agent/internal/agent/screen"
	"aiden-agent/internal/ble"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	langtools "github.com/tmc/langchaingo/tools"
	"nhooyr.io/websocket"
)

func TestPhoneBridgeScreenCacheFollowsPhoneIDAcrossHTTPFallback(t *testing.T) {
	bridge := newTestPhoneBridge(t)
	bridge.hidConnectionState = func() (bool, bool) { return false, false }
	bridge.hidMonitorEnabled = false
	screenState := &screen.ScreenState{}
	tools := &ToolSet{
		screen: screenState,
		tools:  make(map[string]langtools.Tool),
	}
	tools.RegisterPhoneBridge(bridge)

	if err := bridge.ApplyBenchmarkStatus(PhoneBridgeStatus{
		Connected: true,
		Platform:  "android",
		PhoneID:   "android-phone-a",
		Environment: &PhoneEnvironment{
			Screen: screen.PhoneScreenInfo{
				WidthPixels:  intPtr(1200),
				HeightPixels: intPtr(2608),
			},
		},
	}); err != nil {
		t.Fatalf("ApplyBenchmarkStatus() error = %v", err)
	}
	if got := screenshotMinimalWidth(screenState); got != 497 {
		t.Fatalf("live screenshot minimal width = %d, want 497", got)
	}

	if err := bridge.ApplyBenchmarkStatus(PhoneBridgeStatus{Platform: "android"}); err != nil {
		t.Fatalf("clear ApplyBenchmarkStatus() error = %v", err)
	}
	if got := screenshotMinimalWidth(screenState); got != 0 {
		t.Fatalf("disconnected screenshot minimal width = %d, want 0", got)
	}

	bridge.noteHTTPPollState("android", "android-phone-a", "background", "", "true")
	status := bridge.getStatus()
	if status.Environment == nil {
		t.Fatal("same-phone HTTP poll did not restore cached screen environment")
	}
	if status.Environment.Source != "phone-bridge-screen-cache" {
		t.Fatalf("environment source = %q, want phone-bridge-screen-cache", status.Environment.Source)
	}
	if got := screenshotMinimalWidth(screenState); got != 497 {
		t.Fatalf("same-phone cached screenshot minimal width = %d, want 497", got)
	}

	bridge.noteHTTPPollState("android", "android-phone-b", "background", "", "true")
	if status := bridge.getStatus(); status.Environment != nil {
		t.Fatalf("different-phone environment = %+v, want nil", status.Environment)
	}
	if got := screenshotMinimalWidth(screenState); got != 0 {
		t.Fatalf("different-phone screenshot minimal width = %d, want 0", got)
	}

	bridge.noteHTTPPollState("android", "", "background", "", "true")
	if status := bridge.getStatus(); status.Environment != nil {
		t.Fatalf("missing-phone-id environment = %+v, want nil", status.Environment)
	}
}

func TestRegisterPhoneBridgeWrapsOpenURLWithPostActionScreenshot(t *testing.T) {
	bridge := newTestPhoneBridge(t)
	tools := &ToolSet{tools: map[string]langtools.Tool{
		"screenshot": &stubTool{name: "screenshot", output: `{"width":1,"height":1,"format":"jpeg","size":1,"data":"YQ=="}`},
	}}

	tools.RegisterPhoneBridge(bridge)
	openURL, ok := tools.tools[toolOpenURL]
	if !ok || openURL == nil {
		t.Fatal("open_url was not registered")
	}
	visual, ok := openURL.(visualObservationTool)
	if !ok || !visual.ReturnsVisualObservation() {
		t.Fatalf("open_url = %T, want post-action visual observation wrapper", openURL)
	}
}

func TestPhoneBridgeScreenCacheFollowsPhysicalHIDConnection(t *testing.T) {
	bridge := newTestPhoneBridge(t)
	hidConnected := true
	bridge.hidConnectionState = func() (bool, bool) { return hidConnected, true }
	bridge.hidMonitorEnabled = false
	screenState := &screen.ScreenState{}
	tools := &ToolSet{
		screen: screenState,
		tools:  make(map[string]langtools.Tool),
	}
	tools.RegisterPhoneBridge(bridge)

	if err := bridge.ApplyBenchmarkStatus(PhoneBridgeStatus{
		Connected: true,
		Platform:  "android",
		PhoneID:   "android-phone-a",
		Environment: &PhoneEnvironment{Screen: screen.PhoneScreenInfo{
			WidthPixels: intPtr(1200), HeightPixels: intPtr(2608),
		}},
	}); err != nil {
		t.Fatalf("ApplyBenchmarkStatus() error = %v", err)
	}
	firstConnectionID := bridge.getStatus().HIDConnectionID
	if firstConnectionID == "" {
		t.Fatal("physical HID connection did not receive an ID")
	}
	if got := screenshotMinimalWidth(screenState); got != 497 {
		t.Fatalf("live screenshot minimal width = %d, want 497", got)
	}

	bridge.mu.Lock()
	bridge.connected = false
	bridge.phoneID = ""
	bridge.environment = nil
	bridge.environmentAt = time.Time{}
	bridge.mu.Unlock()
	bridge.notifyEnvironmentObserver()
	if got := bridge.getStatus().HIDConnectionID; got != firstConnectionID {
		t.Fatalf("WebSocket disconnect changed HID connection ID: got %q want %q", got, firstConnectionID)
	}
	if got := screenshotMinimalWidth(screenState); got != 497 {
		t.Fatalf("same-HID cached screenshot minimal width = %d, want 497", got)
	}

	hidConnected = false
	bridge.notifyEnvironmentObserver()
	if got := bridge.getStatus().HIDConnectionID; got != "" {
		t.Fatalf("disconnected HID connection ID = %q, want empty", got)
	}
	if got := screenshotMinimalWidth(screenState); got != 0 {
		t.Fatalf("disconnected HID screenshot minimal width = %d, want 0", got)
	}

	hidConnected = true
	bridge.refreshHIDConnectionNow()
	if got := bridge.getStatus().HIDConnectionID; got == "" || got == firstConnectionID {
		t.Fatalf("new HID connection ID = %q, want a new non-empty ID", got)
	}
	if got := screenshotMinimalWidth(screenState); got != 0 {
		t.Fatalf("new HID session reused old screen width = %d, want 0", got)
	}
}

type iosIsolationPointerTestTool struct {
	controller *iosKeyboardIsolationController
	events     *[]string
}

func (t iosIsolationPointerTestTool) Name() string        { return "touch_gesture" }
func (t iosIsolationPointerTestTool) Description() string { return "touch_gesture" }
func (t iosIsolationPointerTestTool) Call(ctx context.Context, _ string) (string, error) {
	return t.controller.withPointerCall(ctx, func(context.Context) (string, error) {
		*t.events = append(*t.events, "pointer")
		return "ok", nil
	})
}

func TestSearchLaunchAppSchemaInfersPlatformFromDeviceState(t *testing.T) {
	schema := (&appSearchOpenTool{}).ArgsSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("missing properties: %#v", schema)
	}
	if props["app"] == nil || props["name"] == nil {
		t.Fatalf("search_launch_app schema missing expected fields: %#v", props)
	}
	if _, found := props["platform"]; found {
		t.Fatalf("search_launch_app schema must infer platform from runtime device state: %#v", props)
	}
}

func TestOpenAppSchemaInfersPlatformFromDeviceState(t *testing.T) {
	schema := NewOpenAppTool(nil, nil, nil).ArgsSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("missing properties: %#v", schema)
	}
	if props["app"] == nil || props["name"] == nil {
		t.Fatalf("open_app schema missing expected fields: %#v", props)
	}
	if _, found := props["platform"]; found {
		t.Fatalf("open_app schema must infer fallback platform from runtime device state: %#v", props)
	}
}

func TestOpenAppDescriptionDefersToClearlyVisibleTarget(t *testing.T) {
	description := NewOpenAppTool(nil, nil, nil).Description()
	for _, expected := range []string{
		"not clearly and uniquely visible in the latest screenshot",
		"use touch_gesture",
		"instead of calling open_app",
	} {
		if !strings.Contains(description, expected) {
			t.Fatalf("open_app description missing %q: %s", expected, description)
		}
	}
}

func TestParseRoutedOpenAppArgsKeepsSemanticAlias(t *testing.T) {
	args, te := parseRoutedOpenAppArgs(`{"app":" weixin "}`)
	if te != nil {
		t.Fatalf("parseRoutedOpenAppArgs returned error: %v", te)
	}
	if args.App != "weixin" {
		t.Fatalf("app = %q, want semantic alias", args.App)
	}
}

func TestParseRoutedOpenAppArgsAcceptsNameAliasAndIgnoresLegacyPlatform(t *testing.T) {
	args, te := parseRoutedOpenAppArgs(`{"name":"微信","platform":"IOS"}`)
	if te != nil {
		t.Fatalf("parseRoutedOpenAppArgs returned error: %v", te)
	}
	if args.App != "微信" {
		t.Fatalf("args = %#v, want normalized alias with legacy platform ignored", args)
	}
}

func TestParseOpenURLArgsAcceptsSupportedSchemes(t *testing.T) {
	for _, value := range []string{
		"http://example.com/path",
		"https://example.com/path?q=1",
		"sms:+15551234567?body=example",
		"mailto:user@example.com?subject=example",
		"tel:+15551234567",
	} {
		args, te := parseOpenURLArgs(jsonString(map[string]string{"url": " " + value + " "}))
		if te != nil {
			t.Fatalf("parseOpenURLArgs(%q) returned error: %v", value, te)
		}
		if args.URL != value {
			t.Fatalf("url = %q, want %q", args.URL, value)
		}
	}
}

func TestParseOpenURLArgsRejectsUnsupportedScheme(t *testing.T) {
	if _, te := parseOpenURLArgs(`{"url":"weixin://scan"}`); te == nil {
		t.Fatal("parseOpenURLArgs returned nil error, want unsupported URL rejected")
	}
}

func TestBridgeOpenResultMechanismNormalizesAndroidPackage(t *testing.T) {
	if got := bridgeOpenResultMechanism("launch_package"); got != "android_package" {
		t.Fatalf("mechanism = %q, want android_package", got)
	}
	if got := bridgeOpenResultMechanism("open_url"); got != "open_url" {
		t.Fatalf("mechanism = %q, want open_url", got)
	}
}

func TestSearchOpenAppToolIsInternalToOpenApp(t *testing.T) {
	runtime := newRuntimeWithTextEntryTools()
	if _, ok := runtime.tools.Get(toolSearchLaunchApp); ok {
		t.Fatal("search_launch_app must not be exposed as a standalone tool")
	}
	if runtime.tools.searchOpenTool == nil {
		t.Fatal("expected internal search_launch_app implementation")
	}
	openApp, ok := runtime.tools.Get(toolOpenApp)
	if !ok {
		t.Fatal("expected public open_app router without Phone Bridge registration")
	}
	visual, ok := openApp.(visualObservationTool)
	if !ok || !visual.ReturnsVisualObservation() {
		t.Fatal("open_app must return a post-action screenshot observation")
	}
}

func TestAppSearchOpenFlowCanBeReused(t *testing.T) {
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}
	quick := &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`}
	touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
	keyboardText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	hw := &textInputHardwareDeps{quickAction: quick, touchGesture: touch, keyboardText: keyboardText, keyboardTap: textInputStubTool{name: "keyboard_tap", out: "ok"}}
	hw.pointerMode = "touchscreen"
	hw.deviceTypeFn = func() string { return "Android" }
	hw.screenshot = textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`}
	entryTool := &EnterTextTool{engine: newFastTextInputEngine(*hw, vision)}
	entryTool.SetDeviceTypeFunc(hw.deviceTypeFn)
	called := 0
	result, err := runAppSearchOpenFlow(context.Background(), appSearchOpenFlowConfig{
		hw:                hw,
		vision:            vision,
		platform:          "android",
		searchTerm:        "Aiden",
		entryTool:         entryTool,
		sleep:             testNoWaitSleep,
		checkSearchOpenFn: androidSearchAlwaysOpen,
		searchEntries:     &androidSearchEntryStore{},
		findAppTapFn: func(context.Context, screenshotResult, string) (bridgeSearchResult, error) {
			called++
			return bridgeSearchResult{Found: true, TapPoint: &focusPointArgs{X: 500, Y: 200}, Label: "Aiden"}, nil
		},
		confirmAppOpenFn: func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error) {
			return bridgeAppOpenResult{Opened: true, Reason: "Aiden app visible"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Opened {
		t.Fatalf("expected opened result, got %#v", result)
	}
	if called != 1 {
		t.Fatalf("findAppTapFn calls=%d", called)
	}
	// Android opens search with Home and a swipe instead of quick_action.
	if len(quick.calls) != 0 || len(keyboardText.calls) != 2 || len(touch.calls) != 2 {
		t.Fatalf("unexpected tool calls: quick=%v keyboard=%v touch=%v", quick.calls, keyboardText.calls, touch.calls)
	}
}

func TestSearchLaunchAppTextEntryDisablesBridgePath(t *testing.T) {
	pb := newTestPhoneBridge(t)
	pb.platform = "android"
	pb.connected = true
	pb.appState = "background"
	keyboardText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	hw := &textInputHardwareDeps{
		pointerMode:  "touchscreen",
		deviceTypeFn: func() string { return "Android" },
		keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
		keyboardText: keyboardText,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}
	bridgeWrites := 0
	entryTool := &EnterTextTool{
		engine: newFastTextInputEngine(*hw, vision),
		bridgeTool: &textInputBridge{
			hw:       hw,
			vision:   vision,
			bridgeFn: func() *PhoneBridge { return pb },
			clipboardWriteFn: func(context.Context, *PhoneBridge, string) error {
				bridgeWrites++
				return context.Canceled
			},
		},
	}
	entryTool.SetDeviceTypeFunc(hw.deviceTypeFn)

	_, err := enterSearchQuery(context.Background(), appSearchOpenFlowConfig{
		hw:        hw,
		vision:    vision,
		platform:  "android",
		entryTool: entryTool,
	}, newFastTextInputEngine(*hw, vision), "Aiden", false, false)
	if err != nil {
		t.Fatalf("enterSearchQuery() error = %v", err)
	}
	if bridgeWrites != 0 {
		t.Fatalf("bridge writes = %d, want search_launch_app text entry to disable Bridge", bridgeWrites)
	}
	if len(keyboardText.calls) == 0 {
		t.Fatal("keyboard_text was not called; want local text-entry path")
	}
}

func TestSearchLaunchAppUsesRuntimeAndroidPlatformBeforeIOSFallback(t *testing.T) {
	vision := &stubTextInputVision{}
	quick := &recordingTextInputTool{name: "quick_action", out: "ok"}
	touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
	keyboardText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	keys := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	hw := &textInputHardwareDeps{
		quickAction:  quick,
		touchGesture: touch,
		keyboardText: keyboardText,
		keyboardTap:  keys,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}
	tool := &appSearchOpenTool{
		hw:                   hw,
		vision:               vision,
		entryTool:            &EnterTextTool{engine: newFastTextInputEngine(*hw, vision)},
		sleep:                testNoWaitSleep,
		iosKeyboardIsolation: newTestIOSKeyboardIsolationController(&[]string{}),
		checkSearchOpenFn:    androidSearchAlwaysOpen,
		searchEntries:        &androidSearchEntryStore{},
		findAppTapFn: func(context.Context, screenshotResult, string) (bridgeSearchResult, error) {
			return bridgeSearchResult{Found: true, TapPoint: &focusPointArgs{X: 500, Y: 200}, Label: "WeChat"}, nil
		},
		confirmAppOpenFn: func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error) {
			return bridgeAppOpenResult{Opened: true, Reason: "WeChat visible"}, nil
		},
	}
	toolSet := &ToolSet{tools: map[string]langtools.Tool{}, searchOpenTool: tool}
	toolSet.SetRuntimeDeviceTypeFn(func() string { return "Android" })

	out, err := tool.Call(context.Background(), `{"app":"WeChat"}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("decode Call() output: %v: %s", err, out)
	}
	if !result.OK {
		t.Fatalf("Call() output = %s, want ok", out)
	}
	// The runtime Android device type wins over the iOS isolation fallback, so
	// search opens with the Android entry instead of iOS Spotlight.
	if len(quick.calls) != 0 {
		t.Fatalf("quick_action calls = %v, want none on Android", quick.calls)
	}
	if tapped := tappedKeys(t, keys); len(tapped) == 0 || !reflect.DeepEqual(tapped[0], []string{"KEYCODE_HOME"}) {
		t.Fatalf("keyboard_tap = %v, want the Android search entry to start with Home", tapped)
	}
}

func TestTypeQuickSearchQuerySkipsAndroid(t *testing.T) {
	// Android IMEs drop the pending pinyin on a language switch instead of
	// committing it, so Android search must use exact entry.
	vision := &plannedTextInputVision{
		stubTextInputVision: &stubTextInputVision{},
		plans:               map[string][]string{"小红书": {"xiao", "hong", "shu"}},
	}
	keys := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	keyboardText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	hw := &textInputHardwareDeps{keyboardText: keyboardText, keyboardTap: keys}
	cfg := appSearchOpenFlowConfig{hw: hw, vision: vision, platform: "android", sleep: testNoWaitSleep}

	quick, err := typeQuickSearchQuery(context.Background(), cfg, newFastTextInputEngine(*hw, vision), "小红书")
	if err != nil || quick {
		t.Fatalf("typeQuickSearchQuery() = %v, %v; want false, nil on Android", quick, err)
	}
	if len(keys.calls) != 0 || len(keyboardText.calls) != 0 {
		t.Fatalf("unexpected input on Android: keyboard_tap=%v keyboard_text=%v", keys.calls, keyboardText.calls)
	}
}

func TestSearchLaunchAppUsesRealQuickActionDeviceType(t *testing.T) {
	skipHIDSleeps(t)
	skipQuickActionDelays(t)

	t.Run("ios spotlight search binding", func(t *testing.T) {
		vision := &stubTextInputVision{}
		keyboardDev, keyboardPath := newTestHIDDevice(t)
		events := []string{}
		controller := newTestIOSKeyboardIsolationController(&events)
		controller.keyboardDev = keyboardDev
		keyboardTap := testKeyboardTapTool(t, testMNKOpts{keyboard: keyboardDev, gate: newIOSKeyboardIsolationProfileGate(controller)})
		touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
		keyboardText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
		quick := &QuickActionTool{keyboard: keyboardTap, iosKeyboardIsolation: controller}
		hw := &textInputHardwareDeps{
			quickAction:  quick,
			touchGesture: touch,
			keyboardText: keyboardText,
			keyboardTap:  keyboardTap,
			screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
		}
		entryTool := &EnterTextTool{
			engine:               newFastTextInputEngine(*hw, vision),
			iosKeyboardIsolation: controller,
		}
		tool := &appSearchOpenTool{
			hw:                   hw,
			vision:               vision,
			entryTool:            entryTool,
			sleep:                testNoWaitSleep,
			iosKeyboardIsolation: controller,
			findAppTapFn: func(context.Context, screenshotResult, string) (bridgeSearchResult, error) {
				return bridgeSearchResult{Found: true, TapPoint: &focusPointArgs{X: 500, Y: 200}, Label: "WeChat"}, nil
			},
			confirmAppOpenFn: func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error) {
				return bridgeAppOpenResult{Opened: true, Reason: "WeChat visible"}, nil
			},
		}
		toolSet := &ToolSet{tools: map[string]langtools.Tool{"enter_text": entryTool, "keyboard_tap": keyboardTap, "quick_action": quick}, searchOpenTool: tool}
		toolSet.SetRuntimeDeviceTypeFn(func() string { return "iOS" })

		out, err := tool.Call(context.Background(), `{"app":"WeChat"}`)
		if err != nil {
			t.Fatalf("Call() error = %v", err)
		}
		var result struct {
			OK bool `json:"ok"`
		}
		if err := json.Unmarshal([]byte(out), &result); err != nil {
			t.Fatalf("decode Call() output: %v: %s", err, out)
		}
		if !result.OK {
			t.Fatalf("Call() output = %s, want ok", out)
		}
		reports := readKeyboardReports(t, keyboardDev, keyboardPath)
		if len(reports) < 2 {
			t.Fatalf("keyboard reports = %d, want iOS spotlight binding", len(reports))
		}
		assertKeyboardReport(t, reports[0], hidModifierLeftMeta, hidUsageSpace, "Cmd+Space")
		assertReleaseReport(t, reports[1], "release after Cmd+Space")
	})
}

func TestSearchLaunchAppBatchesIOSModifierIsolationAcrossSubtools(t *testing.T) {
	skipHIDSleeps(t)
	skipQuickActionDelays(t)

	keyboardDev, _ := newTestHIDDevice(t)
	events := []string{}
	controller := newTestIOSKeyboardIsolationController(&events)
	controller.keyboardDev = keyboardDev
	keyboardTap := testKeyboardTapTool(t, testMNKOpts{keyboard: keyboardDev, gate: newIOSKeyboardIsolationProfileGate(controller)})
	keyboardText := &KeyboardTextTool{dev: keyboardDev, iosKeyboardIsolation: controller}
	quickAction := &QuickActionTool{
		keyboard:             keyboardTap,
		iosKeyboardIsolation: controller,
	}
	vision := &stubTextInputVision{}
	hw := &textInputHardwareDeps{
		touchGesture: iosIsolationPointerTestTool{controller: controller, events: &events},
		keyboardTap:  keyboardTap,
		keyboardText: keyboardText,
		quickAction:  quickAction,
		screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}
	entryTool := &EnterTextTool{
		engine:               newFastTextInputEngine(*hw, vision),
		iosKeyboardIsolation: controller,
	}
	tool := &appSearchOpenTool{
		hw:                   hw,
		vision:               vision,
		entryTool:            entryTool,
		sleep:                testNoWaitSleep,
		iosKeyboardIsolation: controller,
		findAppTapFn: func(context.Context, screenshotResult, string) (bridgeSearchResult, error) {
			return bridgeSearchResult{Found: true, TapPoint: &focusPointArgs{X: 500, Y: 200}, Label: "WeChat"}, nil
		},
		confirmAppOpenFn: func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error) {
			return bridgeAppOpenResult{Opened: true, Reason: "WeChat visible"}, nil
		},
	}
	toolSet := &ToolSet{tools: map[string]langtools.Tool{"quick_action": quickAction}, searchOpenTool: tool}
	toolSet.SetRuntimeDeviceTypeFn(func() string { return "iOS" })

	out, err := tool.Call(context.Background(), `{"app":"WeChat"}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("Call() output is not JSON: %v: %s", err, out)
	}
	if !result.OK {
		t.Fatalf("Call() output = %s, want ok", out)
	}
	if want := []string{"isolate", "restore", "pointer"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("profile events = %v, want %v", events, want)
	}
}

func TestAppSearchOpenFlowFallsBackToShorterTerm(t *testing.T) {
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}, {ObservedMode: textInputModeASCII}}}
	quick := &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`}
	touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
	keyboardText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	kbTap := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	hw := &textInputHardwareDeps{quickAction: quick, touchGesture: touch, keyboardText: keyboardText, keyboardTap: kbTap}
	hw.pointerMode = "touchscreen"
	hw.deviceTypeFn = func() string { return "Android" }
	hw.screenshot = textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`}
	entryTool := &EnterTextTool{engine: newFastTextInputEngine(*hw, vision)}
	entryTool.SetDeviceTypeFunc(hw.deviceTypeFn)
	terms := []string{}
	result, err := runAppSearchOpenFlow(context.Background(), appSearchOpenFlowConfig{
		hw:                hw,
		vision:            vision,
		platform:          "android",
		searchTerm:        "Aiden Bridge",
		entryTool:         entryTool,
		sleep:             testNoWaitSleep,
		checkSearchOpenFn: androidSearchAlwaysOpen,
		searchEntries:     &androidSearchEntryStore{},
		findAppTapFn: func(_ context.Context, _ screenshotResult, term string) (bridgeSearchResult, error) {
			terms = append(terms, term)
			if term == "Aiden" {
				return bridgeSearchResult{Found: true, TapPoint: &focusPointArgs{X: 500, Y: 220}, Label: "Aiden"}, nil
			}
			return bridgeSearchResult{Found: false}, nil
		},
		confirmAppOpenFn: func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error) {
			return bridgeAppOpenResult{Opened: true, Reason: "Aiden app visible"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Opened {
		t.Fatalf("expected opened result, got %#v", result)
	}
	if len(terms) < 3 || terms[0] != "Aiden Bridge" || terms[1] != "Aiden Bridge" || terms[2] != "Aiden" {
		t.Fatalf("unexpected search terms: %#v", terms)
	}
	if len(keyboardText.calls) < 2 {
		t.Fatalf("expected two search entry attempts, got keyboard_text calls=%v", keyboardText.calls)
	}
}

func TestAppSearchOpenFlowRechecksSameTermBeforeFallback(t *testing.T) {
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}
	quick := &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`}
	touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
	keyboardText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	kbTap := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	hw := &textInputHardwareDeps{quickAction: quick, touchGesture: touch, keyboardText: keyboardText, keyboardTap: kbTap}
	hw.pointerMode = "touchscreen"
	hw.deviceTypeFn = func() string { return "Android" }
	hw.screenshot = textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`}
	entryTool := &EnterTextTool{engine: newFastTextInputEngine(*hw, vision)}
	entryTool.SetDeviceTypeFunc(hw.deviceTypeFn)
	terms := []string{}
	findCalls := 0
	result, err := runAppSearchOpenFlow(context.Background(), appSearchOpenFlowConfig{
		hw:                hw,
		vision:            vision,
		platform:          "android",
		searchTerm:        "Aiden Bridge",
		entryTool:         entryTool,
		sleep:             testNoWaitSleep,
		checkSearchOpenFn: androidSearchAlwaysOpen,
		searchEntries:     &androidSearchEntryStore{},
		findAppTapFn: func(_ context.Context, _ screenshotResult, term string) (bridgeSearchResult, error) {
			terms = append(terms, term)
			if term == "Aiden Bridge" {
				findCalls++
				if findCalls == 1 {
					return bridgeSearchResult{Found: false}, nil
				}
				return bridgeSearchResult{Found: true, TapPoint: &focusPointArgs{X: 500, Y: 220}, Label: "Aiden Bridge"}, nil
			}
			return bridgeSearchResult{Found: false}, nil
		},
		confirmAppOpenFn: func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error) {
			return bridgeAppOpenResult{Opened: true, Reason: "Aiden app visible"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !result.Opened {
		t.Fatalf("expected opened result, got %#v", result)
	}
	if len(terms) != 2 || terms[0] != "Aiden Bridge" || terms[1] != "Aiden Bridge" {
		t.Fatalf("expected same-term recheck before fallback, got terms=%#v", terms)
	}
	if len(keyboardText.calls) != 3 {
		t.Fatalf("expected probe followed by the two ASCII query parts, got keyboard_text calls=%v", keyboardText.calls)
	}
	if len(touch.calls) != 2 {
		t.Fatalf("touch_gesture calls=%v, want the search swipe and one result tap", touch.calls)
	}
}

func TestOpenAppMissingArgsReturnsInvalidArguments(t *testing.T) {
	tool := &OpenAppTool{}
	ctx, _ := WithToolError(context.Background())
	out, err := tool.Call(ctx, `{}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	te := ToolErrorFromContext(ctx)
	if te == nil {
		t.Fatalf("expected structured ToolError on context; got nil")
	}
	if te.Code != CodeInvalidArguments {
		t.Errorf("Code = %q want %q", te.Code, CodeInvalidArguments)
	}
	if out != te.Message {
		t.Errorf("Call output (%q) must equal Error.Message (%q)", out, te.Message)
	}
}

func TestOpenAppDisconnectedRoutesToSearchLaunchApp(t *testing.T) {
	search := &recordingTextInputTool{name: toolSearchLaunchApp, out: `{"ok":true,"target":"小红书"}`}
	tool := NewOpenAppTool(nil, nil, search)
	var logs []string
	tool.logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	ctx, _ := WithToolError(context.Background())
	out, err := tool.Call(ctx, `{"app":"小红书"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if got := ToolErrorFromContext(ctx); got != nil {
		t.Fatalf("unexpected ToolError: %+v", got)
	}
	if out != search.out {
		t.Fatalf("output = %s, want search output %s", out, search.out)
	}
	if len(search.calls) != 1 || !strings.Contains(search.calls[0], `"app":"小红书"`) {
		t.Fatalf("search calls = %#v, want semantic app", search.calls)
	}
	if got := strings.Join(logs, "\n"); !strings.Contains(got, "selected=search_launch_app") || !strings.Contains(got, "reason=phone_bridge_unavailable") {
		t.Fatalf("route logs = %q, want direct search route and reason", got)
	}
}

func TestOpenAppConnectedForegroundRoutesToBridge(t *testing.T) {
	var sent BridgeCommand
	bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
		sent = cmd
		return BridgeCommandResponse{ID: cmd.ID, Method: "ios_url_scheme"}
	})
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "active"
	bridge.mu.Unlock()
	search := &recordingTextInputTool{name: toolSearchLaunchApp, out: `{"ok":true}`}
	tool := NewOpenAppTool(bridge, nil, search)

	out, err := tool.Call(context.Background(), `{"app":"微信"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if sent.Type != "open_app" || sent.App != "微信" || sent.URL != "" {
		t.Fatalf("sent command = %#v, want semantic app command", sent)
	}
	if len(search.calls) != 0 {
		t.Fatalf("search calls = %#v, want bridge route", search.calls)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["ok"] != true || result["method"] != "open_app" {
		t.Fatalf("output = %s, want bridge app result: %v", out, err)
	}
}

func TestOpenAppBridgeFailureFallsBackToSearchLaunchApp(t *testing.T) {
	var sent BridgeCommand
	bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
		sent = cmd
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodeToolExecutionFailed, "bridge launch failed"),
		}
	})
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "active"
	bridge.mu.Unlock()
	search := &recordingTextInputTool{name: toolSearchLaunchApp, out: `{"ok":true,"target":"微信"}`}
	tool := NewOpenAppTool(bridge, nil, search)
	var logs []string
	tool.logf = func(format string, args ...any) {
		logs = append(logs, fmt.Sprintf(format, args...))
	}
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{"app":"微信","platform":"ios"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if sent.Type != "open_app" || sent.App != "微信" {
		t.Fatalf("sent command = %#v, want failed semantic bridge launch", sent)
	}
	if len(search.calls) != 1 || !strings.Contains(search.calls[0], `"app":"微信"`) || strings.Contains(search.calls[0], `"platform"`) {
		t.Fatalf("search calls = %#v, want one semantic fallback call without platform override", search.calls)
	}
	if out != search.out {
		t.Fatalf("output = %s, want search fallback output %s", out, search.out)
	}
	if te := ToolErrorFromContext(ctx); te != nil {
		t.Fatalf("successful search fallback retained bridge ToolError: %+v", te)
	}
	if got := strings.Join(logs, "\n"); !strings.Contains(got, "selected=bridge_open_app") ||
		!strings.Contains(got, "selected=search_launch_app") ||
		!strings.Contains(got, "reason=bridge_failed") ||
		!strings.Contains(got, `bridge_error_code="tool_execution_failed"`) {
		t.Fatalf("route logs = %q, want bridge selection and search fallback details", got)
	}
}

func TestOpenURLSendsURLTarget(t *testing.T) {
	var sent BridgeCommand
	bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
		sent = cmd
		return BridgeCommandResponse{ID: cmd.ID, Method: "open_url"}
	})
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "active"
	bridge.mu.Unlock()
	tool := NewOpenURLTool(bridge, nil)

	const target = "sms:+15551234567?body=example"
	out, err := tool.Call(context.Background(), jsonString(map[string]string{"url": target}))
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if sent.Type != "open_app" || sent.URL != target || sent.App != "" {
		t.Fatalf("sent command = %#v, want URL-only bridge command", sent)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["method"] != "open_url" || result["target"] != target {
		t.Fatalf("output = %s, want open_url result: %v", out, err)
	}
	properties := NewOpenURLTool(nil, nil).ArgsSchema()["properties"].(map[string]any)
	if _, ok := properties["url"]; !ok || len(properties) != 1 {
		t.Fatalf("open_url schema = %#v, want URL-only input", properties)
	}
}

func TestOpenAppDisconnectedAndroidRoutesToSearch(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	t.Cleanup(func() { bridge.queue.Stop() })
	bridge.mu.Lock()
	bridge.platform = "android"
	bridge.mu.Unlock()
	search := &recordingTextInputTool{name: toolSearchLaunchApp, out: `{"ok":true}`}
	tool := NewOpenAppTool(bridge, nil, search)

	ctx, _ := WithToolError(context.Background())
	_, err := tool.Call(ctx, `{"app":"微信","platform":"android"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if len(search.calls) != 1 || !strings.Contains(search.calls[0], `"app":"微信"`) || strings.Contains(search.calls[0], `"platform"`) {
		t.Fatalf("search calls = %#v, want fallback without platform override", search.calls)
	}
}

func TestOpenAppConnectedAndroidBackgroundRoutesToSearch(t *testing.T) {
	sent := false
	bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
		sent = true
		return BridgeCommandResponse{ID: cmd.ID}
	})
	bridge.mu.Lock()
	bridge.platform = "android"
	bridge.appState = "background"
	bridge.appStateAt = time.Now()
	bridge.mu.Unlock()
	search := &recordingTextInputTool{name: toolSearchLaunchApp, out: `{"ok":true}`}
	tool := NewOpenAppTool(bridge, nil, search)

	ctx, _ := WithToolError(context.Background())
	out, err := tool.Call(ctx, `{"app":"微信"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if sent {
		t.Fatal("background Android open_app was sent over Phone Bridge")
	}
	if out != search.out || len(search.calls) != 1 {
		t.Fatalf("output=%s search calls=%#v, want search fallback", out, search.calls)
	}
}

func TestOpenAppConnectedUnknownPlatformBackgroundRoutesToSearch(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	t.Cleanup(func() { bridge.queue.Stop() })
	bridge.mu.Lock()
	bridge.connected = true
	bridge.appState = "background"
	bridge.mu.Unlock()
	search := &recordingTextInputTool{name: toolSearchLaunchApp, out: `{"ok":true}`}
	tool := NewOpenAppTool(bridge, nil, search)

	if _, err := tool.Call(context.Background(), `{"app":"Maps"}`); err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if len(search.calls) != 1 {
		t.Fatalf("search calls = %#v, want fallback for background state", search.calls)
	}
}

func TestParseRoutedOpenAppArgsUnknownAppStaysSemantic(t *testing.T) {
	args, te := parseRoutedOpenAppArgs(`{"app":"NoSuchApp12345"}`)
	if te != nil {
		t.Fatalf("parseRoutedOpenAppArgs returned error: %v", te)
	}
	if args.App != "NoSuchApp12345" {
		t.Fatalf("app = %q, want semantic alias", args.App)
	}
}

func TestContactsInvalidInputReturnsStructuredError(t *testing.T) {
	tool := NewContactsTool(nil, nil)
	ctx, _ := WithToolError(context.Background())
	out, err := tool.Call(ctx, `{bad json`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	te := ToolErrorFromContext(ctx)
	if te == nil || te.Code != CodeInvalidArguments {
		t.Fatalf("expected invalid_arguments; got %+v", te)
	}
	if out != te.Message {
		t.Fatalf("Call output (%q) must equal Error.Message (%q)", out, te.Message)
	}
}

func TestNotificationInvalidInputReturnsStructuredError(t *testing.T) {
	tool := NewNotificationTool(nil, nil)
	ctx, _ := WithToolError(context.Background())
	out, err := tool.Call(ctx, `{bad json`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	te := ToolErrorFromContext(ctx)
	if te == nil || te.Code != CodeInvalidArguments {
		t.Fatalf("expected invalid_arguments; got %+v", te)
	}
	if out != te.Message {
		t.Fatalf("Call output (%q) must equal Error.Message (%q)", out, te.Message)
	}
}

func TestNotificationQueryReadsBLEEventRingWithoutPhoneBridge(t *testing.T) {
	tool := NewNotificationTool(nil, nil)
	tool.socketPath = func() string { return "/tmp/test-ble.sock" }
	tool.statusReader = func(context.Context, string) (ble.RuntimeStatus, error) {
		t.Fatal("explicit cursor query must not read status")
		return ble.RuntimeStatus{}, nil
	}
	tool.eventsReader = func(
		_ context.Context,
		socketPath string,
		since string,
		generation string,
		limit int,
	) (ble.EventPage, error) {
		if socketPath != "/tmp/test-ble.sock" || since != "7" || generation != "generation-1" || limit != 5 {
			t.Fatalf("unexpected query socket=%q since=%q generation=%q limit=%d", socketPath, since, generation, limit)
		}
		return ble.EventPage{
			Events: []ble.NotificationEvent{{
				ID:               "8",
				NotificationUID:  42,
				Event:            "added",
				AppIdentifier:    "com.example.chat",
				Title:            "Alice",
				Message:          "hello",
				MetadataComplete: true,
			}},
			Generation: "generation-1",
			OldestID:   "1",
			LastID:     "8",
		}, nil
	}

	out, err := tool.Call(context.Background(), `{"action":"query","since":"7","generation":"generation-1","limit":5}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"action": "query"`, `"title": "Alice"`, `"message": "hello"`, `"last_id": "8"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("query output missing %q: %s", want, out)
		}
	}
}

func TestNotificationQueryDefaultsToLatestEvents(t *testing.T) {
	tool := NewNotificationTool(nil, nil)
	tool.socketPath = func() string { return "/tmp/test-ble.sock" }
	tool.statusReader = func(_ context.Context, socketPath string) (ble.RuntimeStatus, error) {
		if socketPath != "/tmp/test-ble.sock" {
			t.Fatalf("socket = %q", socketPath)
		}
		return ble.RuntimeStatus{LastEventID: "32", EventGeneration: "generation-2"}, nil
	}
	tool.eventsReader = func(
		_ context.Context,
		_ string,
		since string,
		generation string,
		limit int,
	) (ble.EventPage, error) {
		if since != "27" || generation != "generation-2" || limit != 5 {
			t.Fatalf("unexpected latest query since=%q generation=%q limit=%d", since, generation, limit)
		}
		return ble.EventPage{Events: []ble.NotificationEvent{}, Generation: generation, LastID: "32"}, nil
	}

	out, err := tool.Call(context.Background(), `{"action":"query","limit":5}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"query_mode": "latest"`, `"since": "27"`, `"last_id": "32"`} {
		if !strings.Contains(out, want) {
			t.Fatalf("latest output missing %q: %s", want, out)
		}
	}
}

func TestNotificationQueryRequiresGenerationForIncrementalCursor(t *testing.T) {
	tool := NewNotificationTool(nil, nil)
	ctx, _ := WithToolError(context.Background())
	out, err := tool.Call(ctx, `{"action":"query","since":"7"}`)
	if err != nil {
		t.Fatal(err)
	}
	te := ToolErrorFromContext(ctx)
	if te == nil || te.Code != CodeInvalidArguments || out != te.Message {
		t.Fatalf("expected invalid cursor error, got error=%#v output=%q", te, out)
	}
}

func TestNotificationLegacyPayloadStillSends(t *testing.T) {
	bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
		if cmd.Type != "notification_send" {
			t.Fatalf("command type = %q", cmd.Type)
		}
		return BridgeCommandResponse{ID: cmd.ID, Data: json.RawMessage(`{"notification_id":"legacy-1"}`)}
	})
	tool := NewNotificationTool(bridge, nil)
	out, err := tool.Call(context.Background(), `{"title":"Legacy","body":"still sends"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"notification_id": "legacy-1"`) {
		t.Fatalf("legacy send failed: %s", out)
	}
}

func TestContactsUpdatePropagatesAppToolError(t *testing.T) {
	bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
		return BridgeCommandResponse{
			ID:    cmd.ID,
			Error: NewToolError(CodePermissionDenied, "contacts permission denied"),
		}
	})
	tool := NewContactsTool(bridge, nil)
	ctx, _ := WithToolError(context.Background())

	out, err := tool.Call(ctx, `{"action":"update","contact_id":"abc","name":"Alice"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	te := ToolErrorFromContext(ctx)
	if te == nil || te.Code != CodePermissionDenied {
		t.Fatalf("expected permission_denied; got %+v output=%q", te, out)
	}
	if out != te.Message {
		t.Fatalf("Call output (%q) must equal Error.Message (%q)", out, te.Message)
	}
}

func TestClipboardReadSuccessPreservesOKField(t *testing.T) {
	bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
		return BridgeCommandResponse{
			ID:   cmd.ID,
			Data: json.RawMessage(`{"text":"hello"}`),
		}
	})
	tool := NewClipboardTool(bridge, nil)

	out, err := tool.Call(context.Background(), `{"action":"read"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	var payload struct {
		OK   bool   `json:"ok"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if !payload.OK || payload.Text != "hello" {
		t.Fatalf("clipboard success payload = %+v, raw=%s; want ok=true text=hello", payload, out)
	}
}

func TestClipboardWriteRequiresTextArgument(t *testing.T) {
	for _, input := range []string{`{"action":"write"}`, `{"action":"write","text":null}`, `{"action":"write","text":42}`} {
		t.Run(input, func(t *testing.T) {
			tool := NewClipboardTool(nil, nil)
			ctx, _ := WithToolError(context.Background())
			out, err := tool.Call(ctx, input)
			if err != nil {
				t.Fatalf("Call returned err: %v", err)
			}
			te := ToolErrorFromContext(ctx)
			if te == nil || te.Code != CodeInvalidArguments {
				t.Fatalf("expected invalid_arguments; got %+v output=%q", te, out)
			}
		})
	}
}

func TestClipboardWriteAllowsExplicitEmptyText(t *testing.T) {
	bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
		if string(cmd.Payload) != `{"text":""}` {
			t.Errorf("clipboard payload = %s, want explicit empty text", cmd.Payload)
		}
		return BridgeCommandResponse{ID: cmd.ID}
	})
	tool := NewClipboardTool(bridge, nil)

	out, err := tool.Call(context.Background(), `{"action":"write","text":""}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	var result struct {
		OK bool `json:"ok"`
	}
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatal(err)
	}
	if !result.OK {
		t.Fatalf("clipboard write output = %s, want ok=true", out)
	}
}

func TestClipboardReadUsesPiPBackgroundQueueWhenActive(t *testing.T) {
	bridge := newPhoneBridgeForTest()
	t.Cleanup(func() { bridge.queue.Stop() })
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "background"
	bridge.appStateAt = time.Now()
	bridge.pipBridgeEnabled = true
	bridge.pipBridgeSeen = true
	bridge.mu.Unlock()

	go func() {
		time.Sleep(10 * time.Millisecond)
		commands := bridge.queue.PollForPhone("ios", "", 10)
		if len(commands) != 1 {
			t.Errorf("expected one background command, got %d", len(commands))
			return
		}
		if commands[0].Type != "clipboard_read" {
			t.Errorf("background command type = %q, want clipboard_read", commands[0].Type)
			return
		}
		if err := bridge.queue.SubmitResult(BridgeCommandResponse{
			ID:   commands[0].ID,
			Data: json.RawMessage(`{"text":"queued"}`),
		}); err != nil {
			t.Errorf("SubmitResult() error = %v", err)
		}
	}()

	tool := NewClipboardTool(bridge, nil)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := tool.Call(ctx, `{"action":"read"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	var payload struct {
		OK   bool   `json:"ok"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("output is not JSON: %v", err)
	}
	if !payload.OK || payload.Text != "queued" {
		t.Fatalf("clipboard queue payload = %+v, raw=%s; want ok=true text=queued", payload, out)
	}
}

func TestPhoneBridgeToolsRestoreFromDynamicIslandBeforeCommand(t *testing.T) {
	tests := []struct {
		name        string
		commandType string
		input       string
		response    json.RawMessage
		newTool     func(*PhoneBridge, *PhoneBridgeRestorer) langtools.Tool
	}{
		{
			name:        "open URL",
			commandType: "open_app",
			input:       `{"url":"https://example.com"}`,
			newTool: func(bridge *PhoneBridge, restorer *PhoneBridgeRestorer) langtools.Tool {
				return NewOpenURLTool(bridge, restorer)
			},
		},
		{
			name:        "clipboard read",
			commandType: "clipboard_read",
			input:       `{"action":"read"}`,
			response:    json.RawMessage(`{"text":"hello"}`),
			newTool: func(bridge *PhoneBridge, restorer *PhoneBridgeRestorer) langtools.Tool {
				return NewClipboardTool(bridge, restorer)
			},
		},
		{
			name:        "calendar query",
			commandType: "calendar_query",
			input:       `{"action":"query","from":"2026-07-23T00:00:00+08:00","to":"2026-07-24T00:00:00+08:00"}`,
			response:    json.RawMessage(`{"events":[]}`),
			newTool: func(bridge *PhoneBridge, restorer *PhoneBridgeRestorer) langtools.Tool {
				return NewCalendarTool(bridge, restorer)
			},
		},
		{
			name:        "contacts query",
			commandType: "contacts_query",
			input:       `{"action":"query","query":"Biden"}`,
			response:    json.RawMessage(`{"contacts":[]}`),
			newTool: func(bridge *PhoneBridge, restorer *PhoneBridgeRestorer) langtools.Tool {
				return NewContactsTool(bridge, restorer)
			},
		},
		{
			name:        "notification send",
			commandType: "notification_send",
			input:       `{"title":"Bridge test","body":"restored"}`,
			response:    json.RawMessage(`{"notification_id":"notification-1"}`),
			newTool: func(bridge *PhoneBridge, restorer *PhoneBridgeRestorer) langtools.Tool {
				return NewNotificationTool(bridge, restorer)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
				if cmd.Type != tt.commandType {
					t.Errorf("command type = %q, want %q", cmd.Type, tt.commandType)
				}
				return BridgeCommandResponse{ID: cmd.ID, Data: tt.response}
			})
			bridge.mu.Lock()
			bridge.platform = "ios"
			bridge.appState = "background"
			bridge.appStateAt = time.Now()
			bridge.returnEntry = "dynamic_island"
			bridge.returnEntrySeen = true
			bridge.returnEntryOK = true
			bridge.mu.Unlock()

			restorer := NewPhoneBridgeRestorer(bridge, nil)
			restorer.waitTimeout = time.Second
			tapped := false
			restorer.tapReturnEntry = func(context.Context, PhoneBridgeStatus) error {
				tapped = true
				bridge.mu.Lock()
				bridge.appState = "active"
				bridge.returnEntry = "none"
				bridge.returnEntrySeen = true
				bridge.returnEntryOK = false
				bridge.lastHeartbeatAt = time.Now()
				bridge.mu.Unlock()
				return nil
			}

			out, err := tt.newTool(bridge, restorer).Call(context.Background(), tt.input)
			if err != nil {
				t.Fatalf("Call returned err: %v", err)
			}
			if !tapped {
				t.Fatal("Dynamic Island return entry was not tapped")
			}
			var payload map[string]any
			if err := json.Unmarshal([]byte(out), &payload); err != nil {
				t.Fatalf("output is not JSON: %v; raw=%s", err, out)
			}
			if restored, _ := payload["restored_from_return_entry"].(bool); !restored {
				t.Fatalf("output missing restored_from_return_entry=true: %s", out)
			}
		})
	}
}

func newTestPhoneBridgeWithApp(t *testing.T, handle func(BridgeCommand) BridgeCommandResponse) *PhoneBridge {
	t.Helper()
	bridge := newPhoneBridgeForTest()
	t.Cleanup(func() { bridge.queue.Stop() })

	server := httptest.NewServer(http.HandlerFunc(bridge.HandleWebSocket))
	t.Cleanup(server.Close)

	dialURL := "ws" + strings.TrimPrefix(server.URL, "http") + "?platform=android"
	dialCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(dialCtx, dialURL, nil)
	if err != nil {
		t.Fatalf("websocket dial failed: %v", err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	go func() {
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var cmd BridgeCommand
			if err := json.Unmarshal(data, &cmd); err != nil {
				return
			}
			resp := handle(cmd)
			if resp.ID == "" {
				resp.ID = cmd.ID
			}
			payload, err := json.Marshal(resp)
			if err != nil {
				return
			}
			if err := conn.Write(context.Background(), websocket.MessageText, payload); err != nil {
				return
			}
		}
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bridge.getStatus().Connected {
			return bridge
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("phone bridge did not connect")
	return nil
}
