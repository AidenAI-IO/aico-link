package agent

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"
	"time"
)

type openURLBrowserHarness struct {
	tool         *OpenURLTool
	quick        *recordingTextInputTool
	touch        *recordingTextInputTool
	keyboardText *recordingTextInputTool
	keyboardTap  *recordingTextInputTool
	searched     []string
}

// newOpenURLBrowserHarness builds open_url with a browser fallback whose app
// search opens apps in opens and whose address bar is always found.
func newOpenURLBrowserHarness(t *testing.T, bridge *PhoneBridge, platform string, opens func(app string) bool) *openURLBrowserHarness {
	t.Helper()
	h := &openURLBrowserHarness{
		quick:        &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`},
		touch:        &recordingTextInputTool{name: "touch_gesture", out: "ok"},
		keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
		keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
	}
	hw := &textInputHardwareDeps{
		deviceTypeFn: func() string { return platform },
		quickAction:  h.quick, touchGesture: h.touch,
		keyboardText: h.keyboardText, keyboardTap: h.keyboardTap,
		screenshot: textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}
	// One probe observation in ASCII mode; the URL is ASCII-only, so
	// nothing else is analyzed.
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}
	search := &appSearchOpenTool{
		hw: hw, vision: vision, deviceTypeFn: hw.deviceTypeFn, sleep: testNoWaitSleep,
		entryTool:      &EnterTextTool{engine: newFastTextInputEngine(*hw, vision), allowIOSKeyboardIsolationBypass: true},
		exactEntryOnly: true,
		findAppTapFn: func(context.Context, screenshotResult, string) (bridgeSearchResult, error) {
			return bridgeSearchResult{Found: true, TapPoint: &focusPointArgs{X: 200, Y: 300}}, nil
		},
		confirmAppOpenFn: func(_ context.Context, _ screenshotResult, app string) (bridgeAppOpenResult, error) {
			h.searched = append(h.searched, app)
			return bridgeAppOpenResult{Opened: opens(app)}, nil
		},
	}
	h.tool = newOpenURLToolWithBrowser(bridge, nil, search)
	h.tool.browser.findAddressBarFn = func(context.Context, screenshotResult, string) (addressBarCheck, error) {
		return addressBarCheck{Found: true, TapPoint: &focusPointArgs{X: 500, Y: 930}}, nil
	}
	return h
}

func pipBackgroundBridge(t *testing.T) *PhoneBridge {
	t.Helper()
	bridge := newTestPhoneBridge(t)
	bridge.platform = "ios"
	bridge.appState = "background"
	bridge.appStateAt = time.Now()
	bridge.pipBridgeSeen, bridge.pipBridgeEnabled = true, true
	return bridge
}

func TestOpenURLUsesBrowserWhenBridgeUnavailable(t *testing.T) {
	const target = "https://example.com/a?b=c"
	h := newOpenURLBrowserHarness(t, pipBackgroundBridge(t), "ios", func(string) bool { return true })

	ctx, _ := WithToolError(context.Background())
	out, err := h.tool.Call(ctx, jsonString(map[string]string{"url": target}))
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if te := ToolErrorFromContext(ctx); te != nil {
		t.Fatalf("browser fallback set ToolError: %+v", te)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		t.Fatalf("output = %s: %v", out, err)
	}
	if result["ok"] != true || result["mechanism"] != "browser_address_bar" || result["browser"] != "Safari" ||
		result["target"] != target || result["reason"] != "phone_bridge_unavailable" {
		t.Fatalf("output = %s, want Safari address-bar result", out)
	}
	if !slices.Equal(h.searched, []string{"Safari"}) {
		t.Fatalf("searched apps = %v, want Safari", h.searched)
	}
	if typed := strings.Join(typedTexts(t, h.keyboardText), ""); !strings.Contains(typed, target) {
		t.Fatalf("typed text = %q, want URL", typed)
	}
	keys := tappedKeys(t, h.keyboardTap)
	if len(keys) == 0 || !slices.Equal(keys[len(keys)-1], []string{"enter"}) {
		t.Fatalf("tapped keys = %v, want Enter last", keys)
	}
	if !slices.ContainsFunc(h.touch.calls, func(call string) bool { return strings.Contains(call, "930") }) {
		t.Fatalf("touch calls = %v, want address bar tap", h.touch.calls)
	}
}

func TestOpenURLBrowserFallbackTriesNextAndroidBrowser(t *testing.T) {
	bridge := newTestPhoneBridge(t)
	bridge.platform = "android"
	h := newOpenURLBrowserHarness(t, bridge, "android", func(app string) bool { return app == "Browser" })
	// Android search opens through the launcher; stub its entry so the flow
	// reaches the query step.
	h.tool.browser.search.checkSearchOpenFn = func(context.Context, screenshotResult) (androidSearchCheck, error) {
		return androidSearchCheck{SearchOpen: true}, nil
	}

	out, err := h.tool.Call(context.Background(), `{"url":"https://example.com"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["browser"] != "Browser" {
		t.Fatalf("output = %s, want fallback to the vendor browser", out)
	}
	// App search retries an app that did not open, so Chrome can be
	// confirmed more than once before the vendor browser.
	if !slices.Equal(slices.Compact(slices.Clone(h.searched)), []string{"Chrome", "Browser"}) {
		t.Fatalf("searched apps = %v, want Chrome then Browser", h.searched)
	}
}

func TestOpenURLBrowserFallbackReportsMissingBrowser(t *testing.T) {
	h := newOpenURLBrowserHarness(t, pipBackgroundBridge(t), "ios", func(string) bool { return false })

	ctx, _ := WithToolError(context.Background())
	out, err := h.tool.Call(ctx, `{"url":"https://example.com"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	te := ToolErrorFromContext(ctx)
	if te == nil || te.Code != CodeToolExecutionFailed || !strings.Contains(out, "browser did not open") {
		t.Fatalf("output = %s, ToolError = %+v; want browser-not-open failure", out, te)
	}
	if typed := strings.Join(typedTexts(t, h.keyboardText), ""); strings.Contains(typed, "example.com") {
		t.Fatalf("typed URL %q without an open browser", typed)
	}
}

func TestOpenURLNonWebSchemesStillRequireBridge(t *testing.T) {
	h := newOpenURLBrowserHarness(t, pipBackgroundBridge(t), "ios", func(string) bool { return true })

	ctx, _ := WithToolError(context.Background())
	out, err := h.tool.Call(ctx, `{"url":"tel:+15551234567"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if te := ToolErrorFromContext(ctx); te == nil || te.Code != CodeBridgeNotConnected {
		t.Fatalf("output = %s, ToolError = %+v; want bridge_not_connected", out, te)
	}
	if len(h.searched) != 0 || len(h.quick.calls) != 0 {
		t.Fatalf("tel: link used the browser fallback: searched=%v quick=%v", h.searched, h.quick.calls)
	}
}

func TestOpenURLPrefersBridgeWhenReady(t *testing.T) {
	var sent BridgeCommand
	bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
		sent = cmd
		return BridgeCommandResponse{ID: cmd.ID, Method: "open_url"}
	})
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "active"
	bridge.mu.Unlock()
	h := newOpenURLBrowserHarness(t, bridge, "ios", func(string) bool { return true })

	out, err := h.tool.Call(context.Background(), `{"url":"https://example.com"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if sent.URL != "https://example.com" || strings.Contains(out, "browser_address_bar") {
		t.Fatalf("output = %s, sent = %+v; want Phone Bridge route", out, sent)
	}
	if len(h.searched) != 0 {
		t.Fatalf("searched apps = %v, want no browser fallback", h.searched)
	}
}

func TestOpenURLFallsBackToBrowserWhenBridgeCommandFails(t *testing.T) {
	bridge := newTestPhoneBridgeWithApp(t, func(cmd BridgeCommand) BridgeCommandResponse {
		return BridgeCommandResponse{ID: cmd.ID, Error: NewToolError(CodeToolExecutionFailed, "open failed")}
	})
	bridge.mu.Lock()
	bridge.platform = "ios"
	bridge.appState = "active"
	bridge.mu.Unlock()
	h := newOpenURLBrowserHarness(t, bridge, "ios", func(string) bool { return true })

	ctx, _ := WithToolError(context.Background())
	out, err := h.tool.Call(ctx, `{"url":"https://example.com"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if te := ToolErrorFromContext(ctx); te != nil {
		t.Fatalf("browser fallback kept bridge ToolError: %+v", te)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["reason"] != "phone_bridge_failed" {
		t.Fatalf("output = %s, want browser fallback after bridge failure", out)
	}
}
