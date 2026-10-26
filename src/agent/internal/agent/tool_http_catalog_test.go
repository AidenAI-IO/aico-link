package agent

import (
	"aico-agent/internal/ble"
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	langtools "github.com/tmc/langchaingo/tools"
)

func toolAgentExposed(name string) bool {
	return NewToolSpec(&stubTool{name: name, description: name}).AgentExposed
}

func toolNameSet(tools []langtools.Tool) map[string]struct{} {
	names := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if tool != nil {
			names[tool.Name()] = struct{}{}
		}
	}
	return names
}

func toolSpecsForNames(names []string) *ToolSpecs {
	tools := make([]langtools.Tool, 0, len(names))
	for _, name := range names {
		tools = append(tools, &stubTool{name: name, description: name})
	}
	return NewToolSpecs(tools)
}

func newRuntimeWithTextEntryTools() *Runtime {
	return newRuntimeWithTextEntryToolsWithConfig(Config{})
}

func newRuntimeWithTextEntryToolsWithConfig(cfg Config) *Runtime {
	tools := NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{})
	tools.RegisterEnterTextTool(&testModelResolver{model: &scriptedModel{}}, nil)
	return NewRuntimeWithDeps(
		cfg,
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		tools,
		NewSkillIndex(),
	)
}

func TestQuickActionExposedToAgentAndToolLab(t *testing.T) {
	if !toolAgentExposed("quick_action") {
		t.Fatal("expected quick_action available to conversational agent")
	}
	if !toolAgentExposed("keyboard_tap") {
		t.Fatal("expected keyboard_tap available to agent")
	}
}

func TestWaitForWakeupExposedToAgentAndToolLab(t *testing.T) {
	runtime := NewRuntimeWithDeps(
		Config{},
		nil,
		NewMemoryManager(""),
		NewBuiltinToolSet(
			HIDConfig{},
			AudioConfig{},
			SearchConfig{},
			ProxyConfig{},
			WithWaitForWakeupController(NewWaitForWakeupController()),
		),
		NewSkillIndex(),
	)
	if _, ok := runtime.ToolDescriptorByName("wait_for_wakeup"); !ok {
		t.Fatal("expected wait_for_wakeup in Tool Lab HTTP catalog")
	}
	if !toolAgentExposed("wait_for_wakeup") {
		t.Fatal("expected wait_for_wakeup available to conversational agent")
	}
}

func TestTimeAndCalculatorAreNotRegistered(t *testing.T) {
	runtime := NewRuntimeWithDeps(
		Config{},
		nil,
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	for _, name := range []string{"calculator", "current_time"} {
		if _, ok := runtime.tools.Get(name); ok {
			t.Fatalf("removed utility tool %s is still registered", name)
		}
		if _, ok := runtime.ToolDescriptorByName(name); ok {
			t.Fatalf("removed utility tool %s is still in the Tool Lab HTTP catalog", name)
		}
	}
}

func TestPhoneBridgeToolsExposedToAgent(t *testing.T) {
	for _, name := range []string{"open_app", "open_url", "bridge_clipboard", "bridge_calendar", "bridge_contacts", "bridge_notification"} {
		if !toolAgentExposed(name) {
			t.Fatalf("expected %s available to conversational agent", name)
		}
	}
}

func TestUnknownToolsDefaultToHTTPVisible(t *testing.T) {
	// Injected tools without built-in metadata stay visible by default so tests,
	// extensions, and environment bridges do not need extra catalog plumbing.
	spec := NewToolSpec(&stubTool{name: "unregistered_tool", description: "Unregistered."})
	if spec.Name != "unregistered_tool" {
		t.Fatalf("unexpected tool spec name: %q", spec.Name)
	}
	if !spec.AgentExposed {
		t.Fatal("expected unregistered tool to remain Agent-visible by default")
	}
	if !spec.HTTPExposed {
		t.Fatal("expected unregistered tool to remain HTTP-visible by default")
	}
}

func TestHTTPToolSkillDocumentsOptionalPostActionScreenChanged(t *testing.T) {
	markdown := buildHTTPToolSkillMarkdown(
		"test-http-tools",
		"Test HTTP tools.",
		defaultHTTPToolSkillBaseURL,
		[]ToolDescriptor{{Name: "touch_gesture", Category: "input"}},
	)
	for _, expected := range []string{
		"optional `screen_changed`",
		"When present, `screen_changed` reports meaningful structural change",
		"an omitted field means comparison was unavailable, not that no change occurred",
	} {
		if !strings.Contains(markdown, expected) {
			t.Fatalf("generated HTTP tool skill missing optional screen_changed guidance %q:\n%s", expected, markdown)
		}
	}
}

func TestHTTPDescriptorIncludesStructuredArgsSchema(t *testing.T) {
	runtime := NewRuntimeWithDeps(
		Config{},
		nil,
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	descriptor, ok := runtime.ToolDescriptorByName("shell")
	if !ok {
		t.Fatal("expected shell in HTTP catalog")
	}
	properties, ok := descriptor.ArgsSchema["properties"].(map[string]any)
	if !ok {
		t.Fatalf("shell args_schema missing properties: %#v", descriptor.ArgsSchema)
	}
	action, ok := properties["action"].(map[string]any)
	if !ok {
		t.Fatalf("shell args_schema missing action: %#v", properties)
	}
	if _, ok := action["enum"]; !ok {
		t.Fatalf("shell action schema missing enum: %#v", action)
	}
}

func TestAvailableToolsIncludesQuickAction(t *testing.T) {
	runtime := NewRuntimeWithDeps(
		Config{},
		nil,
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)

	tools := runtime.availableTools()
	names := toolNamesFromTools(tools)
	found := false
	for _, name := range names {
		if name == "quick_action" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("availableTools missing quick_action: %v", names)
	}
}

func TestAvailableToolsHidesUnavailablePhoneBridgeToolsWhenDisconnected(t *testing.T) {
	runtime := newRuntimeWithTextEntryTools()
	bridge := newPhoneBridgeForTest()
	t.Cleanup(func() { bridge.queue.Stop() })
	runtime.tools.RegisterPhoneBridge(bridge)

	names := toolNameSet(runtime.availableTools())
	for _, notWant := range []string{
		"open_url",
		"bridge_clipboard",
		"bridge_calendar",
		"bridge_contacts",
		"bridge_notification",
	} {
		if _, ok := names[notWant]; ok {
			t.Fatalf("agent catalog exposed unavailable %s while Phone Bridge and BLE are disconnected: %v", notWant, names)
		}
	}
	if _, ok := names["open_app"]; !ok {
		t.Fatalf("agent catalog should keep open_app because it can fall back to visible system search: %v", names)
	}
	if _, ok := names["enter_text"]; !ok {
		t.Fatalf("agent catalog should keep non-bridge input fallback enter_text: %v", names)
	}
	for _, internal := range []string{"bridge_open_app", "search_launch_app"} {
		if _, ok := names[internal]; ok {
			t.Fatalf("internal launch route %s leaked into agent catalog: %v", internal, names)
		}
		if _, ok := runtime.ToolDescriptorByName(internal); ok {
			t.Fatalf("internal launch route %s leaked into HTTP catalog", internal)
		}
	}
}

func TestAvailableToolsIncludesPhoneBridgeToolsWhenConnected(t *testing.T) {
	runtime := newRuntimeWithTextEntryTools()
	bridge := newPhoneBridgeForTest()
	t.Cleanup(func() { bridge.queue.Stop() })
	bridge.connected = true
	runtime.tools.RegisterPhoneBridge(bridge)

	defaultNames := toolNameSet(runtime.availableTools())
	for _, want := range []string{"open_app", "open_url", "bridge_clipboard", "bridge_calendar", "bridge_contacts", "bridge_notification"} {
		if _, ok := defaultNames[want]; !ok {
			t.Fatalf("default catalog missing connected phone bridge tool %s: %v", want, defaultNames)
		}
	}
}

func TestToolSpecsAgentCatalogPolicy(t *testing.T) {
	coreTools := []string{
		"audio_volume",
		"inspect_episode",
		"keyboard_tap",
		"keyboard_text",
		"enter_text",
		"mouse_move",
		"mouse_scroll",
		"quick_action",
		"recall_device_memory",
		"recall_memory",
		"save_memory",
		"shell",
		"screenshot",
		"skill_list",
		"skill_manage",
		"skill_mark_used",
		"skill_read",
		"touch_gesture",
		"wait_for_stable_screen",
		"weather",
		"web_search",
		"web_scraper",
		"wikipedia",
		"request_user_action",
		"open_app",
		"open_url",
		"bridge_clipboard",
		"bridge_calendar",
		"bridge_contacts",
		"bridge_notification",
	}
	specs := toolSpecsForNames(coreTools)

	defaultNames := toolNameSet(specs.AgentTools())
	for _, want := range coreTools {
		if _, ok := defaultNames[want]; !ok {
			t.Errorf("default catalog missing core tool %s", want)
		}
	}
}

func TestAgentToolsForPlatformFiltersPlatformSpecificTools(t *testing.T) {
	specs := toolSpecsForNames([]string{
		"screenshot",
		"quick_action",
		"enter_text",
		"open_app",
		"open_url",
		toolSearchLaunchApp,
		toolBridgeOpenApp,
		"bridge_clipboard",
	})

	tests := []struct {
		name     string
		platform string
		want     []string
		notWant  []string
	}{
		{
			name:     "ios",
			platform: "ios",
			want:     []string{"screenshot", "quick_action", "enter_text", "open_app", "open_url", "bridge_clipboard"},
			notWant:  []string{"search_launch_app", "bridge_open_app"},
		},
		{
			name:     "android",
			platform: "android",
			want:     []string{"screenshot", "quick_action", "enter_text", "open_app", "open_url", "bridge_clipboard"},
			notWant:  []string{"search_launch_app", "bridge_open_app"},
		},
		{
			name:     "macos",
			platform: "macOS",
			want:     []string{"screenshot", "quick_action", "enter_text", "open_app"},
			notWant:  []string{"open_url", "search_launch_app", "bridge_open_app", "bridge_clipboard"},
		},
		{
			name:     "windows",
			platform: "windows",
			want:     []string{"screenshot", "enter_text"},
			notWant:  []string{"quick_action", "open_app", "open_url", "search_launch_app", "bridge_open_app", "bridge_clipboard"},
		},
		{
			name:     "linux",
			platform: "linux",
			want:     []string{"screenshot", "enter_text"},
			notWant:  []string{"quick_action", "open_app", "open_url", "search_launch_app", "bridge_open_app", "bridge_clipboard"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			names := toolNameSet(specs.AgentToolsForPlatform(tc.platform))
			for _, want := range tc.want {
				if _, ok := names[want]; !ok {
					t.Errorf("catalog for %s missing %s: %v", tc.platform, want, names)
				}
			}
			for _, notWant := range tc.notWant {
				if _, ok := names[notWant]; ok {
					t.Errorf("catalog for %s exposed %s: %v", tc.platform, notWant, names)
				}
			}
		})
	}
}

func TestRuntimeAvailableToolsUsesDeviceTypePlatformFilter(t *testing.T) {
	runtime := newRuntimeWithTextEntryToolsWithConfig(Config{
		Device: DeviceConfig{DeviceType: "windows"},
	})
	bridge := newPhoneBridgeForTest()
	t.Cleanup(func() { bridge.queue.Stop() })
	bridge.connected = true
	runtime.tools.RegisterPhoneBridge(bridge)

	names := toolNameSet(runtime.availableTools())
	if _, ok := names["screenshot"]; !ok {
		t.Fatalf("availableTools missing portable tool screenshot: %v", names)
	}
	if _, ok := names["enter_text"]; !ok {
		t.Fatalf("availableTools missing windows-supported enter_text: %v", names)
	}
	for _, notWant := range []string{"quick_action", "open_app", "open_url", "search_launch_app", "bridge_open_app", "bridge_clipboard"} {
		if _, ok := names[notWant]; ok {
			t.Fatalf("availableTools exposed %s for windows device_type: %v", notWant, names)
		}
	}
	for _, httpWant := range []string{"quick_action", "enter_text", "open_app", "open_url", "bridge_clipboard"} {
		if _, ok := runtime.ToolDescriptorByName(httpWant); !ok {
			t.Fatalf("HTTP catalog hid %s while only model catalog should be platform-filtered", httpWant)
		}
	}
}

func TestRuntimeToolDescriptorsUseDeviceTypeSpecificSchemas(t *testing.T) {
	androidRuntime := NewRuntimeWithDeps(
		Config{Device: DeviceConfig{DeviceType: "Android"}},
		nil,
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	quickAction, ok := androidRuntime.ToolDescriptorByName("quick_action")
	if !ok {
		t.Fatal("Android runtime missing quick_action descriptor")
	}
	androidActions := stringEnumPropertyValues(t, quickAction.ArgsSchema, "action")
	if _, ok := androidActions["home"]; !ok {
		t.Fatalf("Android quick_action schema missing home: %v", androidActions)
	}
	if _, ok := androidActions["control_center"]; ok {
		t.Fatalf("Android quick_action schema exposed reserved control_center: %v", androidActions)
	}

	windowsRuntime := NewRuntimeWithDeps(
		Config{Device: DeviceConfig{DeviceType: "windows"}},
		nil,
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	touchGesture, ok := windowsRuntime.ToolDescriptorByName("touch_gesture")
	if !ok {
		t.Fatal("Windows runtime missing touch_gesture descriptor")
	}
	windowsGestureTypes := stringEnumPropertyValues(t, touchGesture.ArgsSchema, "type")
	for _, notWant := range []string{"back", "home"} {
		if _, ok := windowsGestureTypes[notWant]; ok {
			t.Fatalf("Windows touch_gesture schema exposed %q: %v", notWant, windowsGestureTypes)
		}
	}
}

func TestPhoneBridgeToolDescriptorsHaveUsefulExamples(t *testing.T) {
	runtime := newRuntimeWithTextEntryTools()
	bridge := newPhoneBridgeForTest()
	t.Cleanup(func() { bridge.queue.Stop() })
	bridge.connected = true
	runtime.tools.RegisterPhoneBridge(bridge)

	expected := map[string]string{
		"open_app":            `{"app":"微信"}`,
		"open_url":            `{"url":"https://example.com"}`,
		"bridge_clipboard":    `{"action":"read"}`,
		"bridge_calendar":     `{"action":"query","from":"2026-07-10T00:00:00+08:00","to":"2026-07-11T00:00:00+08:00"}`,
		"bridge_contacts":     `{"action":"query","query":"Alice","limit":20}`,
		"bridge_notification": `{"action":"query","limit":20}`,
	}
	for name, want := range expected {
		desc, ok := runtime.ToolDescriptorByName(name)
		if !ok {
			t.Fatalf("ToolDescriptorByName missing %s", name)
		}
		if desc.ExampleInput != want {
			t.Fatalf("%s example_input = %q, want %q", name, desc.ExampleInput, want)
		}
	}
}

func TestAvailableToolsExposesOnlyBackgroundSafeBridgeToolsDuringPiP(t *testing.T) {
	runtime := newRuntimeWithTextEntryTools()
	bridge := newIOSPiPBackgroundBridge(t)
	runtime.tools.RegisterPhoneBridge(bridge)

	names := toolNameSet(runtime.availableTools())
	for _, want := range []string{"bridge_clipboard", "bridge_calendar", "bridge_contacts", "bridge_notification"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("agent catalog missing PiP-safe %s: %v", want, names)
		}
	}
	for _, notWant := range []string{"open_url"} {
		if _, ok := names[notWant]; ok {
			t.Fatalf("agent catalog exposed foreground-only %s during PiP background state: %v", notWant, names)
		}
	}
	if _, ok := names["open_app"]; !ok {
		t.Fatalf("agent catalog should keep fallback-capable open_app during PiP: %v", names)
	}
	if _, ok := runtime.ToolDescriptorByName("open_app"); !ok {
		t.Fatalf("HTTP catalog should keep open_app available for direct diagnostics: %v", names)
	}
	if _, ok := runtime.ToolDescriptorByName("bridge_clipboard"); !ok {
		t.Fatalf("HTTP catalog missing PiP background bridge_clipboard: %v", names)
	}
}

func TestAvailableToolsExposesBackgroundSafeBridgeToolsThroughBLEWake(t *testing.T) {
	runtime := newRuntimeWithTextEntryTools()
	bridge := newPhoneBridgeForTest()
	t.Cleanup(func() { bridge.queue.Stop() })
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "background"
	bridge.appStateAt = time.Now()
	bridge.mu.Unlock()
	bridge.bleStatus = func(context.Context) (ble.RuntimeStatus, error) {
		return ble.RuntimeStatus{
			BackendAvailable: true,
			Connected:        true,
			WakeSubscriber:   true,
			ANCSSubscribed:   true,
		}, nil
	}
	runtime.tools.RegisterPhoneBridge(bridge)

	names := toolNameSet(runtime.availableTools())
	for _, want := range []string{"bridge_calendar", "bridge_contacts", "bridge_notification"} {
		if _, ok := names[want]; !ok {
			t.Fatalf("agent catalog missing BLE-Wake-safe %s: %v", want, names)
		}
	}
	for _, notWant := range []string{"open_url", "bridge_clipboard"} {
		if _, ok := names[notWant]; ok {
			t.Fatalf("agent catalog exposed foreground-only %s through BLE Wake: %v", notWant, names)
		}
	}
	if _, ok := names["open_app"]; !ok {
		t.Fatalf("agent catalog should keep fallback-capable open_app through BLE Wake: %v", names)
	}
	var contacts langtools.Tool
	for _, tool := range runtime.availableTools() {
		if tool != nil && tool.Name() == toolBridgeContacts {
			contacts = tool
			break
		}
	}
	if contacts == nil {
		t.Fatal("missing contacts capability tool")
	}
	properties := contacts.(structuredInputTool).ArgsSchema()["properties"].(map[string]any)
	actions := properties["action"].(map[string]any)["enum"].([]string)
	if !slices.Equal(actions, []string{"query", "create"}) {
		t.Fatalf("BLE contacts actions = %#v, want query/create", actions)
	}
	if _, ok := properties["contact_id"]; ok {
		t.Fatalf("BLE contacts schema exposed update-only contact_id: %#v", properties)
	}
	ctx, _ := WithToolError(context.Background())
	out, err := contacts.Call(ctx, `{"action":"update","contact_id":"abc","name":"Alice"}`)
	if err != nil {
		t.Fatalf("contacts update Call() error = %v", err)
	}
	if te := ToolErrorFromContext(ctx); te == nil || te.Code != CodeModuleUnavailable || out != te.Message {
		t.Fatalf("contacts update result = %q error=%#v, want module_unavailable", out, te)
	}
	for _, input := range []string{`{}`, `{"action":"delete"}`} {
		ctx, _ := WithToolError(context.Background())
		out, err := contacts.Call(ctx, input)
		if err != nil {
			t.Fatalf("contacts Call(%s) error = %v", input, err)
		}
		if te := ToolErrorFromContext(ctx); te == nil || te.Code != CodeInvalidArguments || out != te.Message {
			t.Fatalf("contacts Call(%s) result = %q error=%#v, want invalid_arguments", input, out, te)
		}
	}
}

func TestAvailableToolsExposesOnlyNotificationWhenBLECanOnlyQuery(t *testing.T) {
	runtime := newRuntimeWithTextEntryTools()
	bridge := newPhoneBridgeForTest()
	t.Cleanup(func() { bridge.queue.Stop() })
	bridge.bleStatus = func(context.Context) (ble.RuntimeStatus, error) {
		return ble.RuntimeStatus{
			BackendAvailable: true,
			Connected:        true,
			ANCSSubscribed:   true,
		}, nil
	}
	runtime.tools.RegisterPhoneBridge(bridge)

	tools := runtime.availableTools()
	names := toolNameSet(tools)
	if _, ok := names["bridge_notification"]; !ok {
		t.Fatalf("agent catalog missing BLE notification query tool: %v", names)
	}
	for _, notWant := range []string{"open_url", "bridge_clipboard", "bridge_calendar", "bridge_contacts"} {
		if _, ok := names[notWant]; ok {
			t.Fatalf("agent catalog exposed unavailable %s with query-only BLE: %v", notWant, names)
		}
	}
	if _, ok := names["open_app"]; !ok {
		t.Fatalf("agent catalog should keep fallback-capable open_app with query-only BLE: %v", names)
	}
	var notification langtools.Tool
	for _, tool := range tools {
		if tool != nil && tool.Name() == "bridge_notification" {
			notification = tool
			break
		}
	}
	if notification == nil {
		t.Fatal("missing query-only notification tool")
	}
	schema := notification.(structuredInputTool).ArgsSchema()
	properties := schema["properties"].(map[string]any)
	action := properties["action"].(map[string]any)
	enum := action["enum"].([]string)
	if len(enum) != 1 || enum[0] != "query" {
		t.Fatalf("query-only notification action enum = %#v", enum)
	}
	if _, ok := properties["title"]; ok {
		t.Fatalf("query-only notification schema leaked send fields: %#v", properties)
	}

	capability, ok := notification.(*notificationCapabilityTool)
	if !ok {
		t.Fatalf("notification tool type = %T, want *notificationCapabilityTool", notification)
	}
	inner, ok := capability.inner.(*NotificationTool)
	if !ok {
		t.Fatalf("notification inner tool type = %T, want *NotificationTool", capability.inner)
	}
	inner.socketPath = func() string { return "/tmp/test-ble.sock" }
	inner.statusReader = func(_ context.Context, socketPath string) (ble.RuntimeStatus, error) {
		if socketPath != "/tmp/test-ble.sock" {
			t.Fatalf("notification status socket = %q", socketPath)
		}
		return ble.RuntimeStatus{LastEventID: "1", EventGeneration: "generation-1"}, nil
	}
	inner.eventsReader = func(
		_ context.Context,
		socketPath string,
		since string,
		generation string,
		limit int,
	) (ble.EventPage, error) {
		if socketPath != "/tmp/test-ble.sock" || since != "0" || generation != "generation-1" || limit != 1 {
			t.Fatalf("unexpected notification query socket=%q since=%q generation=%q limit=%d", socketPath, since, generation, limit)
		}
		return ble.EventPage{Generation: generation, LastID: "1"}, nil
	}
	ctx, _ := WithToolError(context.Background())
	out, err := notification.Call(ctx, `{"limit":1}`)
	if err != nil {
		t.Fatalf("query-only notification Call() error = %v", err)
	}
	if te := ToolErrorFromContext(ctx); te != nil {
		t.Fatalf("query-only notification Call() tool error = %#v, output=%q", te, out)
	}
	if !strings.Contains(out, `"action": "query"`) {
		t.Fatalf("query-only notification Call() output = %q, want query action", out)
	}
}

func newIOSPiPBackgroundBridge(t *testing.T) *PhoneBridge {
	t.Helper()
	bridge := newPhoneBridgeForTest()
	t.Cleanup(func() { bridge.queue.Stop() })
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "background"
	bridge.appStateAt = time.Now()
	bridge.pipBridgeEnabled = true
	bridge.pipBridgeSeen = true
	bridge.mu.Unlock()
	return bridge
}
