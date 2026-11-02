package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	langtools "github.com/tmc/langchaingo/tools"
)

var openAppCmdSeq atomic.Uint64

type routedOpenAppArgs struct {
	App  string `json:"app"`
	Name string `json:"name"`
}

// OpenAppTool is the public app-launch tool. It owns Phone Bridge routing so
// the model does not need to choose between the bridge and visible app search.
type OpenAppTool struct {
	bridge     *PhoneBridge
	bridgeOpen langtools.Tool
	searchOpen langtools.Tool
	logf       func(string, ...any)
}

func NewOpenAppTool(bridge *PhoneBridge, restorer *PhoneBridgeRestorer, searchOpen langtools.Tool) *OpenAppTool {
	tool := &OpenAppTool{
		bridge:     bridge,
		bridgeOpen: NewBridgeOpenAppTool(bridge, restorer),
		searchOpen: searchOpen,
	}
	if bridge != nil && bridge.logger != nil {
		tool.logf = bridge.logger.Info
	}
	return tool
}

func (t *OpenAppTool) Name() string { return toolOpenApp }

func (t *OpenAppTool) Description() string {
	return `Open an app by semantic app name when the target app is not clearly and uniquely visible in the latest screenshot, or when a direct visible-target tap has already failed. ` +
		`If the target app icon or card is clearly visible, use touch_gesture to tap its visible non-overlapping center instead of calling open_app. ` +
		`The tool automatically uses Phone Bridge when the companion app is ready, otherwise it searches for and opens the app through the visible system UI. ` +
		`If the Phone Bridge launch fails, the tool retries through visible system search. ` +
		`Returns ok:true when the selected launch path reports success. After launch, inspect the opened screen before performing in-app navigation or text entry.`
}

func (t *OpenAppTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"app":  stringArgSchema("App name or semantic alias to open, such as WeChat, 微信, browser, or Settings."),
		"name": stringArgSchema("Alias for app."),
	}, "app")
}

func parseRoutedOpenAppArgs(input string) (routedOpenAppArgs, *ToolError) {
	var args routedOpenAppArgs
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return args, NewToolErrorWithDetails(CodeInvalidArguments,
				fmt.Sprintf("invalid input: %v. Expected JSON like {\"app\":\"WeChat\"}", err),
				map[string]any{"raw_input": trimmed})
		}
	} else {
		args.App = trimmed
	}
	if strings.TrimSpace(args.App) == "" {
		args.App = strings.TrimSpace(args.Name)
	}
	args.App = strings.TrimSpace(args.App)
	args.Name = ""
	if args.App == "" {
		return args, NewToolError(CodeInvalidArguments, "app is required")
	}
	return args, nil
}

func callNestedTool(ctx context.Context, tool langtools.Tool, input string) (string, *ToolError, error) {
	if tool == nil {
		te := NewToolError(CodeToolExecutionFailed, "app launch route is not configured")
		return toolErrorString(te), te, nil
	}
	toolCtx, _ := WithToolError(ctx)
	out, err := tool.Call(toolCtx, input)
	return out, ToolErrorFromContext(toolCtx), err
}

func returnNestedToolResult(ctx context.Context, out string, te *ToolError, err error) (string, error) {
	if te != nil && (strings.TrimSpace(out) == "" || out == te.Message) {
		SetToolError(ctx, te)
	}
	return out, err
}

func (t *OpenAppTool) logRoute(format string, args ...any) {
	if t != nil && t.logf != nil {
		t.logf("open_app route: "+format, args...)
	}
}

func openAppSearchRouteReason(tool *OpenAppTool, status PhoneBridgeStatus) string {
	if tool == nil || tool.bridge == nil {
		return "phone_bridge_unavailable"
	}
	if phoneBridgePiPBackgroundEnabled(status) {
		return "ios_pip_background"
	}
	if phoneBridgeFGSBackgroundEnabled(status) {
		return "android_fgs_background"
	}
	if !status.Connected {
		return "phone_bridge_disconnected"
	}
	if phoneBridgeAppNeedsForeground(status) {
		return "companion_app_background"
	}
	return "phone_bridge_not_ready"
}

func openAppBridgeFailureDetails(te *ToolError, err error) (string, string) {
	if te != nil {
		return te.Code, te.Message
	}
	if err != nil {
		return "", err.Error()
	}
	return "", "unknown bridge failure"
}

func (t *OpenAppTool) Call(ctx context.Context, input string) (string, error) {
	args, te := parseRoutedOpenAppArgs(input)
	if te != nil {
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	searchInput := map[string]string{"app": args.App}
	rawSearchInput, _ := json.Marshal(searchInput)

	status := PhoneBridgeStatus{}
	if t != nil && t.bridge != nil {
		status = t.bridge.getStatus()
	}
	if t != nil && t.bridge != nil && phoneBridgeCanUseDirectOpenApp(status) {
		t.logRoute("selected=bridge_open_app app=%q connected=%t bridge_platform=%q app_state=%q pip=%t fgs=%t",
			args.App, status.Connected, status.Platform, status.AppState,
			status.PipBridgeEnabled != nil && *status.PipBridgeEnabled,
			status.FgsBridgeEnabled != nil && *status.FgsBridgeEnabled)
		bridgeInput, _ := json.Marshal(map[string]string{"app": args.App})
		bridgeOut, bridgeTE, bridgeErr := callNestedTool(ctx, t.bridgeOpen, string(bridgeInput))
		if bridgeErr == nil && bridgeTE == nil {
			return bridgeOut, nil
		}
		if ctx.Err() != nil {
			return returnNestedToolResult(ctx, bridgeOut, bridgeTE, bridgeErr)
		}
		bridgeErrorCode, bridgeError := openAppBridgeFailureDetails(bridgeTE, bridgeErr)
		t.logRoute("selected=search_launch_app reason=bridge_failed previous=bridge_open_app app=%q bridge_error_code=%q bridge_error=%q",
			args.App, bridgeErrorCode, bridgeError)
	} else {
		t.logRoute("selected=search_launch_app reason=%s app=%q connected=%t bridge_platform=%q app_state=%q pip=%t fgs=%t",
			openAppSearchRouteReason(t, status), args.App, status.Connected, status.Platform, status.AppState,
			status.PipBridgeEnabled != nil && *status.PipBridgeEnabled,
			status.FgsBridgeEnabled != nil && *status.FgsBridgeEnabled)
	}

	searchOut, searchTE, searchErr := callNestedTool(ctx, t.searchOpen, string(rawSearchInput))
	return returnNestedToolResult(ctx, searchOut, searchTE, searchErr)
}

func phoneBridgeCanUseDirectOpenApp(status PhoneBridgeStatus) bool {
	return !phoneBridgeAppNeedsForeground(status) && phoneBridgeReadyForCommand(status, "open_app")
}

// BridgeOpenAppTool is the internal Phone Bridge implementation used by
// OpenAppTool when the companion app is ready for a foreground command.
type BridgeOpenAppTool struct {
	bridge   *PhoneBridge
	restorer *PhoneBridgeRestorer
}

func NewBridgeOpenAppTool(bridge *PhoneBridge, restorer *PhoneBridgeRestorer) *BridgeOpenAppTool {
	return &BridgeOpenAppTool{bridge: bridge, restorer: restorer}
}

func (t *BridgeOpenAppTool) Name() string { return toolBridgeOpenApp }

func (t *BridgeOpenAppTool) Description() string {
	return `Open an app by semantic app name through the connected phone companion app. This is an internal route used by open_app.`
}

func (t *BridgeOpenAppTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"app": stringArgSchema("App name or semantic alias, such as WeChat, 微信, browser, or Settings."),
	}, "app")
}

type bridgeOpenAppArgs struct {
	App string `json:"app"`
}

func parseBridgeOpenAppArgs(input string) (bridgeOpenAppArgs, *ToolError) {
	var args bridgeOpenAppArgs
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return args, NewToolErrorWithDetails(CodeInvalidArguments,
				fmt.Sprintf("invalid input: %v. Expected JSON like {\"app\":\"WeChat\"}", err),
				map[string]any{"raw_input": trimmed})
		}
	} else {
		args.App = trimmed
	}
	args.App = strings.TrimSpace(args.App)
	if args.App == "" {
		return args, NewToolError(CodeInvalidArguments, "app is required")
	}
	return args, nil
}

func isSupportedOpenURL(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	for _, prefix := range []string{"http://", "https://", "sms:", "mailto:", "tel:"} {
		if strings.HasPrefix(value, prefix) {
			return true
		}
	}
	return false
}

func normalizeOpenURL(value string) (string, *ToolError) {
	value = strings.TrimSpace(value)
	if !isSupportedOpenURL(value) {
		return "", NewToolError(CodeInvalidArguments, "url must use http, https, sms, mailto, or tel")
	}
	return value, nil
}

type OpenURLTool struct {
	bridge   *PhoneBridge
	restorer *PhoneBridgeRestorer
}

func NewOpenURLTool(bridge *PhoneBridge, restorer *PhoneBridgeRestorer) *OpenURLTool {
	return &OpenURLTool{bridge: bridge, restorer: restorer}
}

func (t *OpenURLTool) Name() string { return toolOpenURL }

func (t *OpenURLTool) Description() string {
	return `Open an HTTP/HTTPS webpage, SMS composer, email composer, or telephone link on the connected phone through Phone Bridge. ` +
		`Supported formats: https://example.com, http://example.com, sms:<phone_number>?body=<message>, mailto:<email_address>?subject=<subject>, and tel:<phone_number>. ` +
		`Percent-encode spaces and reserved characters in SMS body and email subject query values. ` +
		`Use open_app to launch a browser without navigating to a fixed URL.`
}

func (t *OpenURLTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"url": stringArgSchema("URL to open using http, https, sms, mailto, or tel."),
	}, "url")
}

type openURLArgs struct {
	URL string `json:"url"`
}

func parseOpenURLArgs(input string) (openURLArgs, *ToolError) {
	var args openURLArgs
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(trimmed, "{") {
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return args, NewToolErrorWithDetails(CodeInvalidArguments,
				fmt.Sprintf("invalid input: %v. Expected JSON like {\"url\":\"https://example.com\"}", err),
				map[string]any{"raw_input": trimmed})
		}
	} else {
		args.URL = trimmed
	}
	url, te := normalizeOpenURL(args.URL)
	args.URL = url
	return args, te
}

func bridgeOpenResultMechanism(responseMethod string) string {
	method := strings.TrimSpace(responseMethod)
	switch method {
	case "launch_package", "package_name":
		return "android_package"
	default:
		return method
	}
}

func sendBridgeOpenCommand(ctx context.Context, bridge *PhoneBridge, restorer *PhoneBridgeRestorer, cmd BridgeCommand, method, target string) (string, error) {
	if bridge != nil && !phoneBridgeReadyForCommand(bridge.getStatus(), cmd.Type) {
		reportToolProgress(ctx, ToolProgress{
			Status:  "preparing",
			Content: "Preparing to switch apps; restoring the companion app",
		})
	}
	restored, err := ensurePhoneBridgeReadyForCommand(ctx, bridge, restorer, "open_app")
	if err != nil {
		status := PhoneBridgeStatus{}
		if bridge != nil {
			status = bridge.getStatus()
		}
		guidance := phoneBridgeOpenAppRecoveryGuidance(status)
		te := NewToolErrorWithDetails(CodeBridgeNotConnected,
			fmt.Sprintf("%v. %s", err, guidance),
			map[string]any{"fallback": guidance})
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	reportToolProgress(ctx, ToolProgress{
		Status:  "running",
		Content: "Preparing to switch apps; opening the target app",
	})
	resp, err := bridge.SendCommand(ctx, cmd)
	if err != nil {
		status := bridge.getStatus()
		te := NewToolErrorWithDetails(CodeToolExecutionFailed,
			fmt.Sprintf("send command: %v", err),
			map[string]any{"fallback": phoneBridgeRecoveryGuidance(status)})
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	if resp.Error != nil {
		status := bridge.getStatus()
		te := resp.Error
		if te.Details == nil {
			te.Details = map[string]any{}
		}
		te.Details["fallback"] = phoneBridgeRecoveryGuidance(status)
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}

	result := map[string]any{
		"ok":     true,
		"method": method,
		"target": target,
	}
	if restored {
		result["restored_from_return_entry"] = true
	}
	if mechanism := bridgeOpenResultMechanism(resp.Method); mechanism != "" {
		result["mechanism"] = mechanism
	}
	return jsonString(result), nil
}

func (t *BridgeOpenAppTool) Call(ctx context.Context, input string) (string, error) {
	args, te := parseBridgeOpenAppArgs(input)
	if te != nil {
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	cmd := BridgeCommand{
		ID:        fmt.Sprintf("open_%d_%d", time.Now().UnixMilli(), openAppCmdSeq.Add(1)),
		Type:      "open_app",
		App:       args.App,
		TimeoutMs: 10000,
	}
	return sendBridgeOpenCommand(ctx, t.bridge, t.restorer, cmd, "open_app", args.App)
}

func (t *OpenURLTool) Call(ctx context.Context, input string) (string, error) {
	args, te := parseOpenURLArgs(input)
	if te != nil {
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	cmd := BridgeCommand{
		ID:        fmt.Sprintf("url_%d_%d", time.Now().UnixMilli(), openAppCmdSeq.Add(1)),
		Type:      "open_app",
		URL:       args.URL,
		TimeoutMs: 10000,
	}
	return sendBridgeOpenCommand(ctx, t.bridge, t.restorer, cmd, "open_url", args.URL)
}

func (s *ToolSet) refreshOpenAppTool() {
	if s == nil || s.searchOpenTool == nil {
		return
	}
	openApp := NewOpenAppTool(s.phoneBridge, s.phoneBridgeRestorer, s.searchOpenTool)
	if screenshot, ok := s.tools["screenshot"]; ok && screenshot != nil {
		s.tools[toolOpenApp] = newPostActionScreenshotTool(openApp, screenshot, postActionScreenshotDelay)
		return
	}
	s.tools[toolOpenApp] = openApp
}

func (s *ToolSet) RegisterPhoneBridge(bridge *PhoneBridge) {
	if bridge == nil {
		return
	}
	s.phoneBridge = bridge
	if s.screen != nil {
		s.screen.SetPhoneScreenScopeFunc(bridge.ScreenCacheScopeID)
	}
	bridge.SetEnvironmentObserver(s.UpdateDeviceEnvironment)
	if s.phoneBridgeRestorer != nil {
		s.phoneBridgeRestorer.SetBridge(bridge)
	}
	s.refreshOpenAppTool()
	openURL := NewOpenURLTool(bridge, s.phoneBridgeRestorer)
	if screenshot, ok := s.tools["screenshot"]; ok && screenshot != nil {
		s.tools[toolOpenURL] = newPostActionScreenshotTool(openURL, screenshot, postActionScreenshotDelay)
	} else {
		s.tools[toolOpenURL] = openURL
	}
	s.tools[toolBridgeClipboard] = NewClipboardTool(bridge, s.phoneBridgeRestorer)
	s.tools[toolBridgeCalendar] = NewCalendarTool(bridge, s.phoneBridgeRestorer)
	s.tools[toolBridgeContacts] = NewContactsTool(bridge, s.phoneBridgeRestorer)
	s.tools[toolBridgeNotification] = NewNotificationTool(bridge, s.phoneBridgeRestorer)
}
