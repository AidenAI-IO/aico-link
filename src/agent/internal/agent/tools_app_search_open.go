package agent

import (
	"aiden-agent/internal/logging"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

const appSearchOpenLaunchDelay = 1200 * time.Millisecond
const appSearchResultSettleDelay = 350 * time.Millisecond

// Restoring the pointer profile re-enumerates USB HID. iOS drops the hardware
// keyboard for a moment and can show the software keyboard, shifting results.
const appSearchPointerRestoreSettleDelay = 800 * time.Millisecond

func sleepWithContext(ctx context.Context, delay time.Duration) error {
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

type appSearchOpenTool struct {
	hw                   *textInputHardwareDeps
	vision               textInputVision
	deviceTypeFn         func() string
	findAppTapFn         func(context.Context, screenshotResult, string) (bridgeSearchResult, error)
	confirmAppOpenFn     func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error)
	afterOpenFn          func() error
	searchTermFn         func(string) string
	entryTool            *EnterTextTool
	launchDelay          time.Duration
	sleep                func(context.Context, time.Duration) error
	iosKeyboardIsolation *iosKeyboardIsolationController
}

type appSearchOpenArgs struct {
	App  string `json:"app"`
	Name string `json:"name"`
}

func (t *appSearchOpenTool) SetDeviceTypeFunc(fn func() string) {
	if t != nil {
		t.deviceTypeFn = fn
	}
}

func (t *appSearchOpenTool) Name() string { return toolSearchLaunchApp }

func (t *appSearchOpenTool) Description() string {
	return strings.TrimSpace(`Search for an app from the system search UI, tap the result, and confirm it opened. ` +
		`This is the visible-UI fallback used internally by open_app when Phone Bridge is unavailable. ` +
		`Input JSON: {"app":"WeChat"}. Returns ok:true when the target app is visibly opened. Observe the opened screen and complete any create/open/navigation step before calling a text-entry tool.`)
}

func (t *appSearchOpenTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"app":  stringArgSchema("App name to search for and open."),
		"name": stringArgSchema("Alias for app."),
	}, "app")
}

func (t *appSearchOpenTool) Call(ctx context.Context, input string) (string, error) {
	var controller *iosKeyboardIsolationController
	if t != nil {
		controller = t.iosKeyboardIsolation
	}
	return withIOSKeyboardIsolationBatchCall(ctx, controller, func(batchCtx context.Context) (string, error) {
		return t.call(batchCtx, input)
	})
}

func (t *appSearchOpenTool) call(ctx context.Context, input string) (string, error) {
	if t == nil || t.hw == nil || t.vision == nil {
		return "error: search_launch_app is not fully configured", nil
	}
	var args appSearchOpenArgs
	if err := json.Unmarshal([]byte(strings.TrimSpace(input)), &args); err != nil {
		return fmt.Sprintf("error: invalid input: %v", err), nil
	}
	if strings.TrimSpace(args.App) == "" && strings.TrimSpace(args.Name) != "" {
		args.App = args.Name
	}
	args.App = strings.TrimSpace(args.App)
	if args.App == "" {
		return jsonString(map[string]any{"ok": false, "error": "app is required"}), nil
	}
	platform := ""
	if t.deviceTypeFn != nil {
		if derived := textInputPlatformFromDeviceType(t.deviceTypeFn()); derived != "" {
			platform = derived
		}
	}
	if platform == "" && t.iosKeyboardIsolation != nil {
		platform = "ios"
	}
	result, err := runAppSearchOpenFlow(ctx, appSearchOpenFlowConfig{
		hw:               t.hw,
		vision:           t.vision,
		platform:         platform,
		searchTerm:       t.searchTerm(args.App),
		findAppTapFn:     t.findAppTapFn,
		confirmAppOpenFn: t.confirmAppOpenFn,
		afterOpenFn:      t.afterOpenFn,
		entryTool:        t.entryTool,
		launchDelay:      t.launchDelay,
		sleep:            t.sleep,
	})
	if err != nil {
		logging.Infof("agent", "app_search", "open finished target=%q ok=false error=%q steps=%q", args.App, err.Error(), result.Steps)
		return jsonString(map[string]any{"ok": false, "error": err.Error(), "target": args.App, "steps": result.Steps, "vlm_calls": result.VLMCalls}), nil
	}
	logging.Infof("agent", "app_search", "open finished target=%q ok=%t reason=%q steps=%q", args.App, result.Opened, result.Reason, result.Steps)
	output := map[string]any{
		"ok":        result.Opened,
		"target":    args.App,
		"steps":     result.Steps,
		"vlm_calls": result.VLMCalls,
	}
	if result.Reason != "" {
		output["reason"] = result.Reason
	}
	if !result.Opened {
		output["ok"] = false
	}
	return jsonString(output), nil
}

func (t *appSearchOpenTool) searchTerm(app string) string {
	if t != nil && t.searchTermFn != nil {
		if term := strings.TrimSpace(t.searchTermFn(app)); term != "" {
			return term
		}
	}
	return app
}

type appSearchOpenFlowConfig struct {
	hw               *textInputHardwareDeps
	vision           textInputVision
	platform         string
	searchTerm       string
	findAppTapFn     func(context.Context, screenshotResult, string) (bridgeSearchResult, error)
	confirmAppOpenFn func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error)
	afterOpenFn      func() error
	entryTool        *EnterTextTool
	launchDelay      time.Duration
	sleep            func(context.Context, time.Duration) error
}

type appSearchOpenFlowResult struct {
	Opened   bool
	Reason   string
	VLMCalls int
	Steps    []string
}

func runAppSearchOpenFlow(ctx context.Context, cfg appSearchOpenFlowConfig) (appSearchOpenFlowResult, error) {
	result := appSearchOpenFlowResult{}
	if cfg.hw == nil || cfg.vision == nil {
		return result, fmt.Errorf("app search open flow is not fully configured")
	}
	if cfg.hw.quickAction == nil || cfg.hw.touchGesture == nil {
		return result, fmt.Errorf("app search open tools are not fully configured")
	}
	if cfg.entryTool == nil {
		return result, fmt.Errorf("app search entry tool is not configured")
	}
	searchTerm := strings.TrimSpace(cfg.searchTerm)
	if searchTerm == "" {
		return result, fmt.Errorf("search term is required")
	}
	launchDelay := cfg.launchDelay
	if launchDelay <= 0 {
		launchDelay = appSearchOpenLaunchDelay
	}
	sleep := cfg.sleep
	if sleep == nil {
		sleep = sleepWithContext
	}
	if cfg.entryTool.engine != nil && cfg.sleep != nil && cfg.entryTool.engine.sleep == nil {
		cfg.entryTool.engine.sleep = cfg.sleep
	}
	steps := make([]string, 0, 12)
	callQuickAction := func(action string) error {
		out, err := callTextInputTool(ctx, cfg.hw.quickAction, jsonString(map[string]any{"action": action}))
		if err != nil {
			return err
		}
		return interpretTextInputToolOutput(out)
	}
	if err := callQuickAction("spotlight_search"); err != nil {
		return result, err
	}
	steps = append(steps, "opened system search")
	engine := newTextInputEngineWithSleep(*cfg.hw, cfg.vision, cfg.sleep)
	isolation := iosKeyboardIsolationControllerFromContext(ctx)
	searchTerms := appSearchFallbackTerms(searchTerm)
	for index, term := range searchTerms {
		if err := enterSearchQuery(ctx, cfg, term, index > 0); err != nil {
			var unverified *searchQueryUnverifiedError
			if !errors.As(err, &unverified) {
				result.Steps = append(steps, "search query entry failed")
				return result, err
			}
			// System search lists the app while the query is still uncommitted
			// IME text, so the result lookup and open confirmation decide.
			logging.Infof("agent", "app_search", "search query unverified term=%q; checking results", term)
			steps = append(steps, "search input unverified; checking results")
		}
		steps = append(steps, fmt.Sprintf("searched %q", term))
		// Switch profiles before the result screenshot: a tap point captured
		// under the keyboard-only profile can be stale after re-enumeration.
		restored, err := isolation.restoreForPointer(ctx)
		if err != nil {
			result.Steps = append(steps, "restore pointer profile failed")
			return result, err
		}
		settleDelay := appSearchResultSettleDelay
		if restored {
			logging.Infof("agent", "app_search", "pointer profile restored before result lookup term=%q", term)
			settleDelay = appSearchPointerRestoreSettleDelay
		}
		if err := sleep(ctx, settleDelay); err != nil {
			result.Steps = append(steps, "wait for search results canceled")
			return result, err
		}
		steps = append(steps, "waited for search results to settle")
		foundForTerm := false
		for attempt := 1; attempt <= 2; attempt++ {
			findResult, calls, err := findSearchOpenAppResult(ctx, cfg, engine, term)
			result.VLMCalls += calls
			if err != nil {
				result.Steps = append(steps, "locate app failed")
				return result, err
			}
			if !findResult.Found {
				if attempt < 2 {
					steps = append(steps, fmt.Sprintf("app result not found for %q; rechecking", term))
					if err := sleep(ctx, 350*time.Millisecond); err != nil {
						result.Steps = append(steps, "app result recheck wait canceled")
						return result, err
					}
					continue
				}
				steps = append(steps, fmt.Sprintf("app result not found for %q", term))
				break
			}
			foundForTerm = true
			if findResult.TapPoint == nil {
				result.Steps = append(steps, "app result is missing tap point")
				return result, fmt.Errorf("app search result is missing tap_point")
			}
			if err := tapSearchOpenResult(ctx, cfg.hw, *findResult.TapPoint); err != nil {
				result.Steps = append(steps, "tap app result failed")
				return result, err
			}
			if label := strings.TrimSpace(findResult.Label); label != "" {
				steps = append(steps, fmt.Sprintf("tapped app result %q", label))
			} else {
				steps = append(steps, "tapped app result")
			}
			if err := sleep(ctx, launchDelay); err != nil {
				result.Steps = append(steps, "app launch wait canceled")
				return result, err
			}
			opened, calls, err := confirmSearchOpenApp(ctx, cfg, engine, term)
			result.VLMCalls += calls
			if err != nil {
				result.Steps = append(steps, "confirm app open failed")
				return result, err
			}
			if opened.Opened {
				steps = append(steps, "app open confirmed")
				if cfg.afterOpenFn != nil {
					if err := cfg.afterOpenFn(); err != nil {
						result.Steps = append(steps, "after-open hook failed")
						return result, err
					}
				}
				result.Opened = true
				result.Reason = strings.TrimSpace(opened.Reason)
				result.Steps = steps
				return result, nil
			}
			result.Reason = strings.TrimSpace(opened.Reason)
			steps = append(steps, "app did not open; retrying")
		}
		if foundForTerm {
			break
		}
	}
	result.Steps = steps
	if result.Reason == "" {
		result.Reason = "app did not open"
	}
	return result, nil
}

func enterSearchQuery(ctx context.Context, cfg appSearchOpenFlowConfig, term string, clearFirst bool) error {
	if clearFirst {
		engine := newTextInputEngineWithSleep(*cfg.hw, cfg.vision, cfg.sleep)
		if err := engine.clearField(ctx, cfg.platform); err != nil {
			return err
		}
	}
	attempted, err := pasteSearchQuery(ctx, cfg, term)
	if err := ctx.Err(); err != nil {
		return err
	}
	if attempted && err == nil {
		logging.Infof("agent", "app_search", "query input route=pip_clipboard_paste")
		return nil
	}
	if attempted {
		logging.Infof("agent", "app_search", "query input route=local_hid clipboard_error=%v", err)
		// A failed paste may have inserted text. Clear it without Escape,
		// which could dismiss Spotlight, before the existing local input path.
		engine := newTextInputEngineWithSleep(*cfg.hw, cfg.vision, cfg.sleep)
		if err := engine.tapKeys(ctx, []string{"meta", "a"}); err != nil {
			return err
		}
		if err := engine.sleepFor(ctx, textInputKeystrokeGap); err != nil {
			return err
		}
		if err := engine.tapKeys(ctx, []string{"backspace"}); err != nil {
			return err
		}
	}

	input := map[string]any{
		"text":  term,
		"focus": map[string]any{"x": 500, "y": 120},
	}
	out, err := cfg.entryTool.enterTextInner(ctx, jsonString(input), enterTextOptions{disableBridge: true, replaceField: true})
	if err != nil {
		return err
	}
	var result enterTextToolResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return fmt.Errorf("parse search entry result: %w", err)
	}
	if !result.OK {
		return &searchQueryUnverifiedError{suggestion: strings.TrimSpace(result.Suggestion)}
	}
	return nil
}

// searchQueryUnverifiedError means local entry ran but its result was not
// confirmed; the query may still be on screen as pending IME composition.
type searchQueryUnverifiedError struct {
	suggestion string
}

func (e *searchQueryUnverifiedError) Error() string {
	return "enter search query: " + e.suggestion
}

// PiP can write the clipboard without leaving the focused system search UI.
// The existing result lookup, tap and app-open confirmation verify the outcome.
func pasteSearchQuery(ctx context.Context, cfg appSearchOpenFlowConfig, term string) (attempted bool, err error) {
	platform := cfg.platform
	if platform == "" {
		platform = cfg.entryTool.platform()
	}
	if platform != "ios" || cfg.entryTool.bridgeTool == nil {
		return false, nil
	}
	bridgeTool := cfg.entryTool.bridgeTool
	bridge := bridgeTool.currentBridge()
	if bridge == nil || !phoneBridgeCanUsePiPBackground(bridge.getStatus(), "clipboard_write") {
		return false, nil
	}
	if err := bridgeTool.writeClipboard(ctx, bridge, term); err != nil {
		return true, err
	}
	if err := bridgeTool.sleepAfterClipboardWrite(ctx); err != nil {
		return true, err
	}
	_, _, err = bridgeTool.pasteClipboard(ctx, platform)
	return true, err
}

func appSearchFallbackTerms(searchTerm string) []string {
	base := strings.TrimSpace(searchTerm)
	if base == "" {
		return nil
	}
	terms := []string{base}
	parts := strings.Fields(base)
	if len(parts) > 1 {
		terms = append(terms, parts[0])
	}
	runes := []rune(base)
	if len(runes) > 4 {
		terms = append(terms, string(runes[:4]))
	}
	seen := map[string]struct{}{}
	unique := make([]string, 0, len(terms))
	for _, term := range terms {
		term = strings.TrimSpace(term)
		if term == "" {
			continue
		}
		if _, ok := seen[term]; ok {
			continue
		}
		seen[term] = struct{}{}
		unique = append(unique, term)
	}
	sort.SliceStable(unique, func(i, j int) bool { return len([]rune(unique[i])) > len([]rune(unique[j])) })
	return unique
}

func findSearchOpenAppResult(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine, searchTerm string) (bridgeSearchResult, int, error) {
	shot, err := engine.captureScreenshot(ctx)
	if err != nil {
		return bridgeSearchResult{}, 0, err
	}
	if cfg.findAppTapFn != nil {
		result, err := cfg.findAppTapFn(ctx, shot, searchTerm)
		return result, 0, err
	}
	modelVision, ok := cfg.vision.(*llmTextInputVision)
	if !ok || modelVision == nil {
		return bridgeSearchResult{}, 0, fmt.Errorf("app search vision is not configured")
	}
	prompt := buildAppSearchResultPrompt(searchTerm)
	raw, err := modelVision.visionDecisionJSON(ctx, "app_search", prompt, []string{"found"}, shot)
	if err != nil {
		return bridgeSearchResult{}, 1, err
	}
	var result bridgeSearchResult
	if err := decodeStrictJSONObject(raw, &result); err != nil {
		return bridgeSearchResult{}, 1, fmt.Errorf("parse app search result: %w", err)
	}
	if result.Found && result.TapPoint == nil {
		return bridgeSearchResult{}, 1, fmt.Errorf("parse app search result: found result is missing tap_point")
	}
	return result, 1, nil
}

func buildAppSearchResultPrompt(searchTerm string) string {
	return strings.TrimSpace(fmt.Sprintf(`Analyze this device screenshot of a system search results page.
Find the visible direct app-launch result for query %q.
Return JSON only:
{
  "found": true,
  "tap_point": {"x": 180, "y": 180},
  "label": "App"
}

Rules:
- A valid result must directly launch the requested app itself. On iOS this is commonly the standalone app icon/tile under a localized "Best Search Result" heading or an Apps section.
- Do not select a result from a localized Settings section, a result with a Settings gear badge, an app settings page, a localized "Search in App" action, a web suggestion, or content from another app even when it contains the exact query text or app icon.
- First discard every result that is not a direct app launch. If multiple valid direct app-launch results remain, scan from top to bottom and choose the topmost one.
- tap_point must be centered inside the actual app icon or its directly associated app-launch tile, using normalized 0-1000 coordinates. Do not use the center of the screen or a large container's empty area.
- Return found=true only when the direct app-launch result is clearly identifiable and tappable. If it cannot be distinguished from Settings or content results, return found=false.
- If not visible, return {"found": false, "tap_point": {"x": 0, "y": 0}}.`, searchTerm))
}

func confirmSearchOpenApp(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine, searchTerm string) (bridgeAppOpenResult, int, error) {
	shot, err := engine.captureScreenshot(ctx)
	if err != nil {
		return bridgeAppOpenResult{}, 0, err
	}
	if cfg.confirmAppOpenFn != nil {
		result, err := cfg.confirmAppOpenFn(ctx, shot, searchTerm)
		return result, 0, err
	}
	modelVision, ok := cfg.vision.(*llmTextInputVision)
	if !ok || modelVision == nil {
		return bridgeAppOpenResult{}, 0, fmt.Errorf("app open confirmation vision is not configured")
	}
	prompt := strings.TrimSpace(fmt.Sprintf(`Analyze this device screenshot immediately after tapping the app search result for query %q.
Decide whether the screen has opened the requested app instead of remaining on the search results page.
Return JSON only:
{
  "opened": true,
  "reason": "app screen is visible"
}

Rules:
- opened=true only when the screenshot clearly shows the target app screen or a loading transition into that app.
- opened=false if the screenshot still looks like the system search page, launcher, keyboard search results, or any unrelated app.
- Keep reason short and concrete.`, searchTerm))
	raw, err := modelVision.visionDecisionJSON(ctx, "app_open_confirmation", prompt, []string{"opened"}, shot)
	if err != nil {
		return bridgeAppOpenResult{}, 1, err
	}
	var result bridgeAppOpenResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return bridgeAppOpenResult{}, 1, fmt.Errorf("parse app open confirmation: %w", err)
	}
	return result, 1, nil
}

func tapSearchOpenResult(ctx context.Context, hw *textInputHardwareDeps, point focusPointArgs) error {
	out, err := hw.touchGesture.Call(ctx, jsonString(map[string]any{
		"type":  "tap",
		"point": map[string]any{"x": point.X, "y": point.Y},
	}))
	if err != nil {
		return err
	}
	return interpretTextInputToolOutput(out)
}
