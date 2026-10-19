package agent

import (
	"aiden-agent/internal/agent/screen"
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
)

func TestBundledQuickActionsPathUsesBusinessPackage(t *testing.T) {
	if BundledQuickActionsPath != "/usr/share/aiden/quick_actions.json" {
		t.Fatalf("BundledQuickActionsPath = %q, want business package path", BundledQuickActionsPath)
	}
}

func TestQuickActionsResolveAliasAndPlatform(t *testing.T) {
	table := newQuickActionsTable()
	if _, ok := table.resolveActionID("返回"); !ok {
		t.Fatal("expected Chinese alias to resolve")
	}
	if _, ok := table.resolveActionID("copy"); !ok {
		t.Fatal("expected action id to resolve")
	}
	if id, ok := table.resolveActionID("go back"); !ok || id != "back" {
		t.Fatalf("expected natural phrase to resolve to back, got %q ok=%v", id, ok)
	}
	if id, ok := table.resolveActionID("browser back"); !ok || id != "browser_back" {
		t.Fatalf("expected spaced action to resolve to browser_back, got %q ok=%v", id, ok)
	}
	if id, ok := table.resolveActionID("quick-switch-left"); !ok || id != "quick_app_switch_left" {
		t.Fatalf("expected hyphenated alias to resolve to quick_app_switch_left, got %q ok=%v", id, ok)
	}
	if id, ok := table.resolveActionID("switch_previous_app"); !ok || id != "app_switch_back" {
		t.Fatalf("expected previous-app alias to resolve to app_switch_back, got %q ok=%v", id, ok)
	}
	_, binding, ok := table.lookup("app_switch_back", "android")
	if !ok || binding.Status != quickActionStatusReserved || binding.Tool != "" || len(binding.Steps) != 0 || len(binding.Input) != 0 {
		t.Fatalf("Android app_switch_back binding = %#v, found=%v; want reserved binding without executable input", binding, ok)
	}
	if id, ok := table.resolveActionID("退格"); !ok || id != "delete_backward" {
		t.Fatalf("expected delete-backward alias to resolve to delete_backward, got %q ok=%v", id, ok)
	}

	platform, err := normalizeQuickActionPlatform("iPadOS")
	if err != nil || platform != "ios" {
		t.Fatalf("expected ios platform, got %q err=%v", platform, err)
	}
	for _, want := range []string{"windows", "linux"} {
		platform, err := normalizeQuickActionPlatform(want)
		if err != nil || platform != want {
			t.Fatalf("expected %s platform, got %q err=%v", want, platform, err)
		}
	}
}

func TestQuickActionListSupportsPlatformWithoutBindings(t *testing.T) {
	tool := &QuickActionTool{deviceTypeFn: func() string { return "windows" }}
	out, err := tool.Call(context.Background(), `{"action":"list"}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	var payload struct {
		OK       bool             `json:"ok"`
		Platform string           `json:"platform"`
		Actions  []map[string]any `json:"actions"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if !payload.OK || payload.Platform != "windows" || len(payload.Actions) != 0 {
		t.Fatalf("unexpected Windows list response: %s", out)
	}
}

func TestQuickActionExposesStructuredSchema(t *testing.T) {
	schema := (&QuickActionTool{}).ArgsSchema()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("missing properties: %#v", schema)
	}
	if props["action"] == nil || props["list"] == nil {
		t.Fatalf("quick_action schema missing expected fields: %#v", props)
	}
	if _, found := props["platform"]; found {
		t.Fatalf("quick_action schema must infer platform from runtime device state: %#v", props)
	}
	// quick_action exposes only the actions defined in quick_actions.json; it
	// carries no transport for bridge-invented capabilities such as open_url.
	if props["url"] != nil {
		t.Fatalf("quick_action schema must not carry a url field: %#v", props)
	}
	required, ok := schema["required"].([]string)
	if !ok || len(required) != 1 || required[0] != "action" {
		t.Fatalf("required = %#v, want action only", schema["required"])
	}
}

func TestQuickActionSchemaListsOnlyActiveActionsForDeviceType(t *testing.T) {
	tool := &QuickActionTool{}
	tool.SetDeviceTypeFunc(func() string { return "Android" })
	schema := tool.ArgsSchema()
	actionEnum := stringEnumPropertyValues(t, schema, "action")

	for _, want := range []string{"list", "back", "home", "app_switch", "spotlight_search"} {
		if _, ok := actionEnum[want]; !ok {
			t.Fatalf("quick_action schema missing active Android action %q: %v", want, actionEnum)
		}
	}
	for _, notWant := range []string{"control_center", "quick_app_switch_left"} {
		if _, ok := actionEnum[notWant]; ok {
			t.Fatalf("quick_action schema exposed unavailable Android action %q: %v", notWant, actionEnum)
		}
	}
}

func TestQuickActionsSuggestUnknownAction(t *testing.T) {
	table := newQuickActionsTable()
	suggestions := table.suggestActionIDs("go browser backward", 3)
	if len(suggestions) == 0 {
		t.Fatal("expected suggestions for related unknown action")
	}
	joined := strings.Join(suggestions, ",")
	if !strings.Contains(joined, "browser_back") && !strings.Contains(joined, "back") {
		t.Fatalf("expected back-related suggestions, got %v", suggestions)
	}
}

func TestQuickActionListForRuntimePlatform(t *testing.T) {
	tool := &QuickActionTool{deviceTypeFn: func() string { return "iOS" }}
	out, err := tool.Call(context.Background(), `{"list":true}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	var payload struct {
		OK      bool `json:"ok"`
		Actions []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if !payload.OK {
		t.Fatalf("expected ok list response: %s", out)
	}
	foundBack := false
	for _, action := range payload.Actions {
		if action.ID == "back" && action.Status == quickActionStatusActive {
			foundBack = true
			break
		}
	}
	if !foundBack {
		t.Fatalf("expected active back action in list: %s", out)
	}
}

func TestQuickActionListOnlyReturnsActivePlatformActions(t *testing.T) {
	tool := &QuickActionTool{deviceTypeFn: func() string { return "Android" }}
	out, err := tool.Call(context.Background(), `{"action":"list"}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	var payload struct {
		OK      bool `json:"ok"`
		Actions []struct {
			ID string `json:"id"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if !payload.OK {
		t.Fatalf("expected list response, got %s", out)
	}
	for _, action := range payload.Actions {
		if action.ID == "control_center" || action.ID == "quick_app_switch_left" {
			t.Fatalf("list exposed unavailable Android action %q: %s", action.ID, out)
		}
	}
}

func TestQuickActionListActionAlias(t *testing.T) {
	tool := &QuickActionTool{deviceTypeFn: func() string { return "Android" }}
	out, err := tool.Call(context.Background(), `{"action":"list"}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	var payload struct {
		OK      bool `json:"ok"`
		Actions []struct {
			ID string `json:"id"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if !payload.OK || len(payload.Actions) == 0 {
		t.Fatalf("expected list response, got %s", out)
	}
}

func TestQuickActionUsesRuntimeDeviceTypeProvider(t *testing.T) {
	tool := &QuickActionTool{}
	tool.SetDeviceTypeFunc(func() string { return "Android" })
	out, err := tool.Call(context.Background(), `{"action":"list"}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	var payload struct {
		OK       bool   `json:"ok"`
		Platform string `json:"platform"`
		Actions  []struct {
			ID string `json:"id"`
		} `json:"actions"`
	}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if !payload.OK || payload.Platform != "android" || len(payload.Actions) == 0 {
		t.Fatalf("expected Android list response from runtime provider, got %s", out)
	}
}

func TestQuickActionRejectsLegacyPlatformArgument(t *testing.T) {
	tool := &QuickActionTool{}
	tool.SetDeviceTypeFunc(func() string { return "Android" })
	ctx, _ := WithToolError(context.Background())
	out, err := tool.Call(ctx, `{"action":"list","platform":"ios"}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	te := ToolErrorFromContext(ctx)
	if te == nil || te.Code != CodeInvalidArguments {
		t.Fatalf("expected invalid_arguments; got %+v (output %q)", te, out)
	}
	if !strings.Contains(te.Message, `unknown field "platform"`) {
		t.Fatalf("unexpected message: %s", te.Message)
	}
}

func TestQuickActionDoesNotExposeScreenshotFull(t *testing.T) {
	table := newQuickActionsTable()
	if id, ok := table.resolveActionID("screenshot_full"); ok {
		t.Fatalf("screenshot_full resolved to %q", id)
	}
}

func TestQuickActionReservedBinding(t *testing.T) {
	tool := &QuickActionTool{deviceTypeFn: func() string { return "iOS" }}
	ctx, _ := WithToolError(context.Background())
	out, err := tool.Call(ctx, `{"action":"app_drawer"}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	te := ToolErrorFromContext(ctx)
	if te == nil || te.Code != CodeQuickActionReserved {
		t.Fatalf("expected quick_action_reserved; got %+v", te)
	}
	if te.Category != CategoryUnsupported {
		t.Errorf("Category = %q, want %q", te.Category, CategoryUnsupported)
	}
	if out != te.Message {
		t.Errorf("Output (%q) must equal Error.Message (%q)", out, te.Message)
	}
}

func TestQuickActionExecutesDelegatedTouchGesture(t *testing.T) {
	skipHIDSleeps(t)
	skipQuickActionDelays(t)

	dev, path := newTestHIDDevice(t)
	tool := &QuickActionTool{
		deviceTypeFn: func() string { return "iOS" },
		touch:        testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev}),
	}
	out, err := tool.Call(context.Background(), `{"action":"back"}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if !quickActionResultOK(t, out) {
		t.Fatalf("unexpected output: %s", out)
	}
	if len(readMouseReports(t, dev, path)) == 0 {
		t.Fatal("expected delegated touch gesture writes")
	}
}

func TestQuickActionExecutesDelegatedKeyboardTap(t *testing.T) {
	skipHIDSleeps(t)
	skipQuickActionDelays(t)

	dev, path := newTestHIDDevice(t)
	tool := &QuickActionTool{
		keyboard:     testKeyboardTapTool(t, testMNKOpts{keyboard: dev}),
		deviceTypeFn: func() string { return "macOS" },
	}
	out, err := tool.Call(context.Background(), `{"action":"copy"}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if !quickActionResultOK(t, out) {
		t.Fatalf("unexpected output: %s", out)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat failed: %v", err)
	}
	if info.Size() < 16 {
		t.Fatalf("expected keyboard press/release writes, got %d bytes", info.Size())
	}
}

func TestQuickActionSpotlightSearchClearsSearchField(t *testing.T) {
	skipHIDSleeps(t)
	skipQuickActionDelays(t)

	dev, path := newTestHIDDevice(t)
	tool := &QuickActionTool{
		keyboard:     testKeyboardTapTool(t, testMNKOpts{keyboard: dev}),
		deviceTypeFn: func() string { return "iOS" },
	}
	out, err := tool.Call(context.Background(), `{"action":"spotlight_search"}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if !quickActionResultOK(t, out) {
		t.Fatalf("unexpected output: %s", out)
	}

	reports := readKeyboardReports(t, dev, path)
	if got, want := len(reports), 10; got != want {
		t.Fatalf("keyboard reports = %d, want %d (Cmd+Space, two Cmd+A/Backspace clear passes): %v", got, want, reports)
	}
	assertKeyboardReport(t, reports[0], hidModifierMap["meta"], hidKeyboardMap["space"], "Cmd+Space")
	assertReleaseReport(t, reports[1], "release after Cmd+Space")
	assertKeyboardReport(t, reports[2], hidModifierMap["meta"], hidKeyboardMap["a"], "first Cmd+A")
	assertReleaseReport(t, reports[3], "release after first Cmd+A")
	assertKeyboardReport(t, reports[4], 0, hidKeyboardMap["backspace"], "first Backspace")
	assertReleaseReport(t, reports[5], "release after first Backspace")
	assertKeyboardReport(t, reports[6], hidModifierMap["meta"], hidKeyboardMap["a"], "second Cmd+A")
	assertReleaseReport(t, reports[7], "release after second Cmd+A")
	assertKeyboardReport(t, reports[8], 0, hidKeyboardMap["backspace"], "second Backspace")
	assertReleaseReport(t, reports[9], "release after second Backspace")
}

func TestQuickActionAndroidSpotlightSearchOpensLauncherSearchFromHome(t *testing.T) {
	_, binding, ok := newQuickActionsTable().lookup("spotlight_search", "android")
	if !ok || binding.Status != quickActionStatusActive {
		t.Fatalf("Android spotlight_search binding = %#v, found=%v; want active", binding, ok)
	}
	if len(binding.Steps) != 2 {
		t.Fatalf("Android spotlight_search steps = %#v, want Home then swipe down", binding.Steps)
	}
	var homeInput struct {
		Keys []string `json:"keys"`
	}
	home := binding.Steps[0]
	if err := json.Unmarshal(home.Input, &homeInput); err != nil || home.Tool != "keyboard_tap" || !reflect.DeepEqual(homeInput.Keys, []string{"KEYCODE_HOME"}) {
		t.Fatalf("first step = %s %s (err=%v), want keyboard_tap KEYCODE_HOME", home.Tool, home.Input, err)
	}
	var swipeInput struct {
		Type  string             `json:"type"`
		Start map[string]float64 `json:"start"`
		End   map[string]float64 `json:"end"`
	}
	swipe := binding.Steps[1]
	if err := json.Unmarshal(swipe.Input, &swipeInput); err != nil || swipe.Tool != "touch_gesture" || swipeInput.Type != "swipe" {
		t.Fatalf("second step = %s %s (err=%v), want touch_gesture swipe", swipe.Tool, swipe.Input, err)
	}
	start, end := swipeInput.Start, swipeInput.End
	// Start below the status bar so the swipe opens launcher search, not the
	// notification shade, and move downward.
	if start["y"] < 200 || end["y"] <= start["y"] {
		t.Fatalf("swipe start=%v end=%v, want a mid-screen downward swipe", start, end)
	}
}

func TestQuickActionSpotlightSearchBatchesIOSModifierIsolation(t *testing.T) {
	skipHIDSleeps(t)
	skipQuickActionDelays(t)

	dev, _ := newTestHIDDevice(t)
	events := []string{}
	controller := newTestIOSKeyboardIsolationController(&events)
	controller.keyboardDev = dev
	tool := &QuickActionTool{
		keyboard:             testKeyboardTapTool(t, testMNKOpts{keyboard: dev, gate: newIOSKeyboardIsolationProfileGate(controller)}),
		deviceTypeFn:         func() string { return "iOS" },
		iosKeyboardIsolation: controller,
	}

	out, err := tool.Call(context.Background(), `{"action":"spotlight_search"}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if !quickActionResultOK(t, out) {
		t.Fatalf("unexpected output: %s", out)
	}
	if want := []string{"isolate", "restore"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("profile events = %v, want %v", events, want)
	}
}

func TestQuickActionRestoresIOSPointerWhenCanceledMidSequence(t *testing.T) {
	skipHIDSleeps(t)

	previousSleep := sleepQuickActionDelay
	sleepQuickActionDelay = func(context.Context, int) error { return context.Canceled }
	t.Cleanup(func() { sleepQuickActionDelay = previousSleep })

	dev, _ := newTestHIDDevice(t)
	events := []string{}
	controller := newTestIOSKeyboardIsolationController(&events)
	controller.keyboardDev = dev
	tool := &QuickActionTool{
		keyboard:             testKeyboardTapTool(t, testMNKOpts{keyboard: dev, gate: newIOSKeyboardIsolationProfileGate(controller)}),
		deviceTypeFn:         func() string { return "iOS" },
		iosKeyboardIsolation: controller,
	}

	_, err := tool.Call(context.Background(), `{"action":"spotlight_search"}`)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Call error = %v, want context canceled", err)
	}
	if want := []string{"isolate", "restore"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("profile events = %v, want %v", events, want)
	}
}

func readKeyboardReports(t *testing.T, dev *HIDDevice, path string) [][]byte {
	t.Helper()

	dev.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data)%8 != 0 {
		t.Fatalf("keyboard report data length = %d, want multiple of 8", len(data))
	}
	reports := make([][]byte, 0, len(data)/8)
	for i := 0; i < len(data); i += 8 {
		reports = append(reports, data[i:i+8])
	}
	return reports
}

func assertKeyboardReport(t *testing.T, report []byte, modifier, key uint8, label string) {
	t.Helper()
	if report[0] != modifier || report[2] != key {
		t.Fatalf("%s report = %v, want modifier 0x%02x key 0x%02x", label, report, modifier, key)
	}
}

func assertReleaseReport(t *testing.T, report []byte, label string) {
	t.Helper()
	for _, b := range report {
		if b != 0 {
			t.Fatalf("%s = %v, want all zeros", label, report)
		}
	}
}

func TestQuickActionDeleteBackwardUsesBackspace(t *testing.T) {
	skipHIDSleeps(t)
	skipQuickActionDelays(t)

	dev, path := newTestHIDDevice(t)
	tool := &QuickActionTool{
		keyboard:     testKeyboardTapTool(t, testMNKOpts{keyboard: dev}),
		deviceTypeFn: func() string { return "macOS" },
	}
	out, err := tool.Call(context.Background(), `{"action":"delete_backward"}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	if !quickActionResultOK(t, out) {
		t.Fatalf("unexpected output: %s", out)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	if len(data) != 16 {
		t.Fatalf("report bytes = %d, want 16 (backspace + release)", len(data))
	}
	if data[2] != hidKeyboardMap["backspace"] {
		t.Fatalf("delete_backward key = 0x%02x, want backspace 0x%02x", data[2], hidKeyboardMap["backspace"])
	}
}

func TestQuickActionAlternativeBinding(t *testing.T) {
	skipHIDSleeps(t)
	skipQuickActionDelays(t)

	dev, _ := newTestHIDDevice(t)
	tool := &QuickActionTool{
		keyboard:     testKeyboardTapTool(t, testMNKOpts{keyboard: dev}),
		deviceTypeFn: func() string { return "Android" },
		touch:        testTouchGestureTool(t, testMNKOpts{screenState: &screen.ScreenState{}, pointer: dev}),
	}
	out, err := tool.Call(context.Background(), `{"action":"back","alternative":true}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	if payload["binding"] != "alternative_1" {
		t.Fatalf("expected alternative binding, got %s", out)
	}
}

func TestQuickActionUnknownAction(t *testing.T) {
	tool := &QuickActionTool{deviceTypeFn: func() string { return "iOS" }}
	ctx, _ := WithToolError(context.Background())
	out, err := tool.Call(ctx, `{"action":"browser backward"}`)
	if err != nil {
		t.Fatalf("Call failed: %v", err)
	}
	te := ToolErrorFromContext(ctx)
	if te == nil || te.Code != CodeQuickActionUnknown {
		t.Fatalf("expected quick_action_unknown; got %+v", te)
	}
	if !strings.Contains(te.Message, "unknown action") {
		t.Fatalf("unexpected message: %s", te.Message)
	}
	if !strings.Contains(te.Message, "suggested actions") {
		t.Fatalf("expected suggested actions in message, got %s", te.Message)
	}
	if out != te.Message {
		t.Errorf("Output (%q) must equal Error.Message (%q)", out, te.Message)
	}
}

func stringEnumPropertyValues(t *testing.T, schema map[string]any, property string) map[string]struct{} {
	t.Helper()
	props, ok := schema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("schema missing properties: %#v", schema)
	}
	prop, ok := props[property].(map[string]any)
	if !ok {
		t.Fatalf("schema missing property %q: %#v", property, props)
	}
	values, ok := prop["enum"].([]string)
	if !ok {
		t.Fatalf("property %q missing string enum: %#v", property, prop)
	}
	result := make(map[string]struct{}, len(values))
	for _, value := range values {
		result[value] = struct{}{}
	}
	return result
}

func quickActionResultOK(t *testing.T, out string) bool {
	t.Helper()
	var payload map[string]interface{}
	if err := json.Unmarshal([]byte(out), &payload); err != nil {
		t.Fatalf("invalid json: %v", err)
	}
	return payload["ok"] == true
}
