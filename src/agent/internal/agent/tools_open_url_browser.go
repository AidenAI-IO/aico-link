package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"aico-agent/internal/logging"
)

const (
	openURLBrowserFocusDelay    = 600 * time.Millisecond
	openURLBrowserNavigateDelay = 800 * time.Millisecond
)

// openURLBrowserApps lists the browsers the visible-UI fallback opens, in
// order. Android has no single system browser, so Chrome is tried before the
// vendor browser.
var openURLBrowserApps = map[string][]string{
	"ios":     {"Safari"},
	"android": {"Chrome", "Browser"},
}

// addressBarCheck locates a browser address bar in a screenshot.
type addressBarCheck struct {
	Found    bool            `json:"found"`
	TapPoint *focusPointArgs `json:"tap_point"`
	Reason   string          `json:"reason,omitempty"`
}

// openURLBrowserFallback opens an HTTP(S) URL without Phone Bridge: it opens
// the browser through visible system search, then types the URL into the
// address bar. It reuses the search tool's hardware, vision and text entry.
type openURLBrowserFallback struct {
	search *appSearchOpenTool
	// findAddressBarFn replaces the address bar lookup in tests.
	findAddressBarFn func(context.Context, screenshotResult, string) (addressBarCheck, error)
}

func openURLBrowserFallbackSupports(url string) bool {
	lower := strings.ToLower(strings.TrimSpace(url))
	return strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://")
}

func (f *openURLBrowserFallback) available() bool {
	return f != nil && f.search != nil && f.search.hw != nil && f.search.vision != nil && f.search.entryTool != nil
}

func (f *openURLBrowserFallback) platform() string {
	if f == nil || f.search == nil {
		return ""
	}
	if f.search.deviceTypeFn != nil {
		if platform := textInputPlatformFromDeviceType(f.search.deviceTypeFn()); platform != "" {
			return platform
		}
	}
	return textInputHardwarePlatform(f.search.hw)
}

// open runs the fallback. reason says why Phone Bridge was not used and is
// returned to the Agent with the result.
func (f *openURLBrowserFallback) open(ctx context.Context, url, reason string) (string, error) {
	var controller *iosKeyboardIsolationController
	if f != nil && f.search != nil {
		controller = f.search.iosKeyboardIsolation
	}
	return withIOSKeyboardIsolationBatchCall(ctx, controller, func(batchCtx context.Context) (string, error) {
		return f.run(batchCtx, url, reason)
	})
}

func (f *openURLBrowserFallback) run(ctx context.Context, url, reason string) (string, error) {
	platform := f.platform()
	browsers := openURLBrowserApps[platform]
	if len(browsers) == 0 {
		return f.fail(ctx, url, fmt.Sprintf("no browser fallback for platform %q", platform), nil), nil
	}
	reportToolProgress(ctx, ToolProgress{Status: "running", Content: "Opening the browser through system search"})
	var steps []string
	browser := ""
	for _, name := range browsers {
		out, _, err := callNestedTool(ctx, f.search, jsonString(map[string]string{"app": name}))
		if err != nil {
			return "", err
		}
		var opened struct {
			OK    bool   `json:"ok"`
			Error string `json:"error"`
		}
		_ = json.Unmarshal([]byte(out), &opened)
		if opened.OK {
			browser = name
			steps = append(steps, fmt.Sprintf("opened %s through system search", name))
			break
		}
		steps = append(steps, fmt.Sprintf("could not open %s through system search", name))
		if err := ctx.Err(); err != nil {
			return "", err
		}
	}
	if browser == "" {
		return f.fail(ctx, url, "browser did not open through system search", steps), nil
	}

	reportToolProgress(ctx, ToolProgress{Status: "running", Content: "Entering the link in the address bar"})
	engine := newTextInputEngineWithSleep(*f.search.hw, f.search.vision, f.search.sleep)
	// Tapping the address bar can move it, for example above the keyboard,
	// so it is located again after it is focused.
	bar, err := f.findAddressBar(ctx, engine, browser)
	if err != nil {
		return "", err
	}
	if !bar.Found || bar.TapPoint == nil {
		return f.fail(ctx, url, "browser address bar not found", steps), nil
	}
	if err := tapSearchOpenResult(ctx, f.search.hw, *bar.TapPoint); err != nil {
		return "", err
	}
	steps = append(steps, "tapped address bar")
	if err := engine.sleepFor(ctx, openURLBrowserFocusDelay); err != nil {
		return "", err
	}
	focused, err := f.findAddressBar(ctx, engine, browser)
	if err != nil {
		return "", err
	}
	focus := *bar.TapPoint
	if focused.Found && focused.TapPoint != nil {
		focus = *focused.TapPoint
	}

	input := map[string]any{"text": url, "focus": map[string]any{"x": focus.X, "y": focus.Y}}
	// enter_text records its own ToolError; keep it out of open_url's result.
	// The Bridge clipboard route is disabled: restoring the companion app
	// through Dynamic Island would leave the browser.
	entryCtx, _ := WithToolError(ctx)
	out, err := f.search.entryTool.enterTextInner(entryCtx, jsonString(input), enterTextOptions{disableBridge: true, replaceField: true})
	if err != nil {
		return "", err
	}
	var entered enterTextToolResult
	if err := json.Unmarshal([]byte(out), &entered); err != nil || !entered.OK {
		return f.fail(ctx, url, "could not enter the URL into the address bar", append(steps, "URL entry failed")), nil
	}
	steps = append(steps, "entered URL")
	if err := engine.tapKeys(ctx, []string{"enter"}); err != nil {
		return "", err
	}
	steps = append(steps, "pressed Enter")
	if err := engine.sleepFor(ctx, openURLBrowserNavigateDelay); err != nil {
		return "", err
	}
	logging.Infof("agent", "open_url", "browser fallback finished url=%q browser=%q reason=%q steps=%q", url, browser, reason, steps)
	return jsonString(map[string]any{
		"ok":        true,
		"method":    "open_url",
		"target":    url,
		"mechanism": "browser_address_bar",
		"browser":   browser,
		"reason":    reason,
		"steps":     steps,
	}), nil
}

func (f *openURLBrowserFallback) fail(ctx context.Context, url, message string, steps []string) string {
	logging.Infof("agent", "open_url", "browser fallback failed url=%q error=%q steps=%q", url, message, steps)
	te := NewToolErrorWithDetails(CodeToolExecutionFailed,
		fmt.Sprintf("open_url browser fallback failed: %s", message),
		map[string]any{
			"target":   url,
			"steps":    steps,
			"fallback": "Inspect the latest screenshot, then open the browser and enter the URL with touch_gesture and enter_text.",
		})
	SetToolError(ctx, te)
	return toolErrorString(te)
}

func (f *openURLBrowserFallback) findAddressBar(ctx context.Context, engine *textInputEngine, browser string) (addressBarCheck, error) {
	shot, err := engine.captureScreenshot(ctx)
	if err != nil {
		return addressBarCheck{}, err
	}
	if f.findAddressBarFn != nil {
		return f.findAddressBarFn(ctx, shot, browser)
	}
	modelVision, ok := f.search.vision.(*llmTextInputVision)
	if !ok || modelVision == nil {
		return addressBarCheck{}, fmt.Errorf("address bar vision is not configured")
	}
	raw, err := modelVision.visionDecisionJSON(ctx, "browser_address_bar", buildAddressBarPrompt(browser), []string{"found"}, shot)
	if err != nil {
		return addressBarCheck{}, err
	}
	var check addressBarCheck
	if err := json.Unmarshal([]byte(raw), &check); err != nil {
		return addressBarCheck{}, fmt.Errorf("parse address bar check: %w", err)
	}
	return check, nil
}

func buildAddressBarPrompt(browser string) string {
	return strings.TrimSpace(fmt.Sprintf(`Analyze this phone screenshot of the %s browser.
Locate the browser address bar, the field that shows or accepts the page URL or a search query.
Return JSON only:
{
  "found": true,
  "tap_point": {"x": 500, "y": 930},
  "reason": "address bar at the bottom of the page"
}

Rules:
- tap_point is the center of the address bar text area in normalized 0-1000 coordinates.
- The address bar may be at the top or bottom of the screen, collapsed to a small URL label, or focused with the keyboard open.
- found=false with tap_point null when no address bar is visible, for example when a dialog covers it or the browser is not open.
- Keep reason short and concrete.`, browser))
}

func (s *ToolSet) openURLBrowserFallbackAvailable() bool {
	if s == nil {
		return false
	}
	return (&openURLBrowserFallback{search: s.searchOpenTool}).available()
}
