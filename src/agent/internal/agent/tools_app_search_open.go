package agent

import (
	"aico-agent/internal/logging"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"
)

const appSearchOpenLaunchDelay = 1200 * time.Millisecond
const appSearchResultSettleDelay = 350 * time.Millisecond

// Restoring the pointer profile re-enumerates USB HID. iOS drops the hardware
// keyboard for a moment and can show the software keyboard, shifting results.
const appSearchPointerRestoreSettleDelay = 800 * time.Millisecond

// On iOS, Caps Lock switches between the active IME and English. Switching
// away commits pending pinyin as letters, so the query survives the later
// pointer-profile restore; the second tap switches back. With no pending text
// both taps only toggle the language and, unlike Return, never open the top
// result. iOS can ignore a second tap that follows too quickly.
const appSearchQuickCommitGap = 800 * time.Millisecond

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
	exactEntryOnly       bool
	checkSearchOpenFn    func(context.Context, screenshotResult) (androidSearchCheck, error)
	searchEntries        *androidSearchEntryStore
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
		platform = textInputPlatformFromDeviceType(t.deviceTypeFn())
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
		exactEntryOnly:   t.exactEntryOnly,
		// Android search entries are remembered per connected phone.
		phoneIDFn:         t.connectedPhoneID,
		checkSearchOpenFn: t.checkSearchOpenFn,
		searchEntries:     t.searchEntries,
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
	if result.Popup != nil {
		output["popup"] = map[string]any{
			"kind":    result.Popup.Kind,
			"summary": result.Popup.Summary,
			"note":    "A dialog is open over the app. Decide from the task whether to continue; do not accept permission, login, privacy or payment dialogs unless the user asked for it.",
		}
	}
	if !result.Opened {
		output["ok"] = false
	}
	return jsonString(output), nil
}

// connectedPhoneID identifies the phone on the board's USB link. It survives a
// Phone Bridge disconnect, which is when the search flow runs.
func (t *appSearchOpenTool) connectedPhoneID() string {
	if t == nil || t.entryTool == nil || t.entryTool.bridgeTool == nil {
		return ""
	}
	return t.entryTool.bridgeTool.currentBridge().HIDConnectionPhoneID()
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
	// exactEntryOnly skips the quick pinyin query (tests of exact entry).
	exactEntryOnly bool
	// phoneIDFn, checkSearchOpenFn and searchEntries drive the Android search
	// entry; see openAndroidSystemSearch.
	phoneIDFn         func() string
	checkSearchOpenFn func(context.Context, screenshotResult) (androidSearchCheck, error)
	searchEntries     *androidSearchEntryStore
	// checkSearchFieldFn replaces the empty-search-field check in tests.
	checkSearchFieldFn func(context.Context, screenshotResult, string) (searchFieldCheck, error)
	// checkPopupFn replaces the after-open dialog check in tests.
	checkPopupFn func(context.Context, screenshotResult, string) (popupCheck, error)
}

// resolvedPlatform is cfg.platform, or the entry tool's platform when the
// caller left it empty.
func (cfg appSearchOpenFlowConfig) resolvedPlatform() string {
	if cfg.platform != "" {
		return cfg.platform
	}
	return cfg.entryTool.platform()
}

type appSearchOpenFlowResult struct {
	Opened   bool
	Reason   string
	VLMCalls int
	Steps    []string
	// Popup is a dialog left open over the app for the Agent to decide on.
	Popup *popupCheck
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
	engine := newTextInputEngineWithSleep(*cfg.hw, cfg.vision, cfg.sleep)
	// Resolve once so every step below works on the same platform.
	cfg.platform = cfg.resolvedPlatform()
	if cfg.platform == "android" {
		entry, err := openAndroidSystemSearch(ctx, cfg, engine)
		result.VLMCalls += entry.vlmCalls
		steps = append(steps, entry.steps...)
		if err != nil {
			result.Steps = steps
			return result, err
		}
		if entry.appIcon != nil {
			result.Steps = steps
			if err := tapSearchOpenResult(ctx, cfg.hw, *entry.appIcon); err != nil {
				return result, err
			}
			result.Steps = append(result.Steps, "tapped visible app icon")
			if err := sleep(ctx, launchDelay); err != nil {
				return result, err
			}
			// This is a launcher icon, not a search result. Keep target-app
			// confirmation instead of the search-page transition comparison.
			opened, calls, err := confirmSearchOpenApp(ctx, cfg, engine, searchTerm, screenshotResult{})
			result.VLMCalls += calls
			if err != nil {
				return result, err
			}
			result.Opened = opened.Opened
			result.Reason = strings.TrimSpace(opened.Reason)
			if opened.Opened {
				result.Steps = append(result.Steps, "app open confirmed")
				if cfg.afterOpenFn != nil {
					if err := cfg.afterOpenFn(); err != nil {
						return result, err
					}
				}
			}
			return result, nil
		}
	} else {
		if err := callQuickAction("spotlight_search"); err != nil {
			return result, err
		}
		steps = append(steps, "opened system search")
	}
	isolation := iosKeyboardIsolationControllerFromContext(ctx)
	type searchAttempt struct {
		term       string
		allowQuick bool
	}
	var attempts []searchAttempt
	for index, term := range appSearchFallbackTerms(searchTerm) {
		attempts = append(attempts, searchAttempt{term: term, allowQuick: index == 0})
	}
	previousQuick := false
	for index := 0; index < len(attempts); index++ {
		term := attempts[index].term
		if previousQuick {
			// The quick query is committed ASCII text, so no screen analysis
			// is needed to clear it.
			if err := clearSearchQueryText(ctx, engine); err != nil {
				result.Steps = append(steps, "clear quick query failed")
				return result, err
			}
		}
		entered, err := enterSearchQuery(ctx, cfg, engine, term, index > 0 && !previousQuick, attempts[index].allowQuick)
		quick := entered == searchQueryQuick
		previousQuick = quick
		if err != nil {
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
		if quick {
			steps = append(steps, fmt.Sprintf("searched %q with quick query", term))
		} else {
			steps = append(steps, fmt.Sprintf("searched %q", term))
		}
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
		recovered := false
	lookup:
		for attempt := 1; attempt <= 2; attempt++ {
			findResult, searchShot, calls, err := findSearchOpenAppResult(ctx, cfg, engine, term)
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
				// A pasted query that never reached the field leaves no
				// results; re-enter it once before trying the next term.
				if entered == searchQueryPasted && !recovered {
					recovered = true
					reentered, calls, err := recoverEmptySearchPaste(ctx, cfg, engine, term)
					result.VLMCalls += calls
					if err != nil {
						result.Steps = append(steps, "search paste recovery failed")
						return result, err
					}
					if reentered {
						steps = append(steps, fmt.Sprintf("search field was empty after paste; re-entered %q", term))
						if err := sleep(ctx, settleDelay); err != nil {
							result.Steps = append(steps, "wait for search results canceled")
							return result, err
						}
						attempt = 0
						continue lookup
					}
				}
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
			opened, calls, err := confirmSearchOpenApp(ctx, cfg, engine, term, searchShot)
			result.VLMCalls += calls
			if err != nil {
				result.Steps = append(steps, "confirm app open failed")
				return result, err
			}
			if opened.Opened {
				steps = append(steps, "app open confirmed")
				steps, result = checkPopupAfterOpen(ctx, cfg, engine, steps, result)
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
		if quick {
			// The quick query assumes standard pinyin input; retry with exact entry.
			attempts = slices.Insert(attempts, index+1, searchAttempt{term: term})
		}
	}
	result.Steps = steps
	if result.Reason == "" {
		result.Reason = "app did not open"
	}
	return result, nil
}

// searchQueryEntry is how enterSearchQuery entered the query.
type searchQueryEntry int

const (
	searchQueryTyped searchQueryEntry = iota
	// searchQueryQuick is the quick ASCII query, which the caller retries
	// with exact entry if no result is found.
	searchQueryQuick
	// searchQueryPasted went through a background clipboard route.
	searchQueryPasted
)

func enterSearchQuery(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine, term string, clearFirst, allowQuick bool) (searchQueryEntry, error) {
	if clearFirst {
		if err := engine.clearField(ctx, cfg.resolvedPlatform()); err != nil {
			return searchQueryTyped, err
		}
	}
	route, err := pasteSearchQuery(ctx, cfg, term)
	attempted := route != ""
	if err := ctx.Err(); err != nil {
		return searchQueryTyped, err
	}
	if attempted && err == nil {
		logging.Infof("agent", "app_search", "query input route=%s", route)
		return searchQueryPasted, nil
	}
	if attempted {
		logging.Infof("agent", "app_search", "query input route=local_hid clipboard_error=%v", err)
		// A failed paste may have inserted text. Clear it without Escape,
		// which could dismiss Spotlight, before the existing local input path.
		if err := clearSearchQueryText(ctx, engine); err != nil {
			return searchQueryTyped, err
		}
	}
	if allowQuick {
		typed, err := typeQuickSearchQuery(ctx, cfg, engine, term)
		if err != nil {
			return searchQueryTyped, err
		}
		if typed {
			return searchQueryQuick, nil
		}
	}

	input := map[string]any{
		"text":  term,
		"focus": map[string]any{"x": 500, "y": 120},
	}
	out, err := cfg.entryTool.enterTextInner(ctx, jsonString(input), enterTextOptions{disableBridge: true, replaceField: true})
	if err != nil {
		return searchQueryTyped, err
	}
	var result enterTextToolResult
	if err := json.Unmarshal([]byte(out), &result); err != nil {
		return searchQueryTyped, fmt.Errorf("parse search entry result: %w", err)
	}
	if !result.OK {
		return searchQueryTyped, &searchQueryUnverifiedError{suggestion: strings.TrimSpace(result.Suggestion)}
	}
	return searchQueryTyped, nil
}

// searchFieldCheck is the state of the system search field after a paste.
type searchFieldCheck struct {
	Empty       bool            `json:"empty"`
	SearchField *focusPointArgs `json:"search_field"`
	Reason      string          `json:"reason,omitempty"`
}

// recoverEmptySearchPaste handles a background clipboard paste that sent the
// paste keys but left the search field empty, which shows up only as missing
// results. It pastes through the long-press menu and, if the field is still
// empty, types the query locally. It reports whether the query was re-entered.
func recoverEmptySearchPaste(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine, term string) (bool, int, error) {
	field, calls, err := checkSearchField(ctx, cfg, engine)
	if err != nil || !field.Empty || field.SearchField == nil {
		return false, calls, err
	}
	focus := *field.SearchField
	logging.Infof("agent", "app_search", "search field empty after paste term=%q; trying long-press paste at %.0f,%.0f", term, focus.X, focus.Y)
	if bridgeTool := cfg.entryTool.bridgeTool; bridgeTool != nil {
		if err := bridgeTool.pasteViaContextMenu(ctx, engine, cfg.resolvedPlatform(), focus); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return false, calls, ctxErr
			}
			logging.Infof("agent", "app_search", "long-press paste failed term=%q: %v", term, err)
		} else {
			after, more, err := checkSearchField(ctx, cfg, engine)
			calls += more
			if err != nil {
				return false, calls, err
			}
			if !after.Empty {
				logging.Infof("agent", "app_search", "query input route=long_press_paste")
				return true, calls, nil
			}
		}
	}
	logging.Infof("agent", "app_search", "query input route=local_hid after empty paste term=%q", term)
	input := map[string]any{"text": term, "focus": map[string]any{"x": focus.X, "y": focus.Y}}
	// Results decide whether local entry worked, as for unverified entry.
	if _, err := cfg.entryTool.enterTextInner(ctx, jsonString(input), enterTextOptions{disableBridge: true, replaceField: true}); err != nil {
		return false, calls, err
	}
	return true, calls, nil
}

func checkSearchField(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine) (searchFieldCheck, int, error) {
	shot, err := engine.captureScreenshot(ctx)
	if err != nil {
		return searchFieldCheck{}, 0, err
	}
	if cfg.checkSearchFieldFn != nil {
		check, err := cfg.checkSearchFieldFn(ctx, shot, cfg.resolvedPlatform())
		return check, 0, err
	}
	modelVision, ok := cfg.vision.(*llmTextInputVision)
	if !ok || modelVision == nil {
		return searchFieldCheck{}, 0, nil
	}
	raw, err := modelVision.visionDecisionJSON(ctx, "search_field_empty", buildSearchFieldCheckPrompt(), []string{"empty"}, shot)
	if err != nil {
		return searchFieldCheck{}, 1, err
	}
	var check searchFieldCheck
	if err := json.Unmarshal([]byte(raw), &check); err != nil {
		return searchFieldCheck{}, 1, fmt.Errorf("parse search field check: %w", err)
	}
	return check, 1, nil
}

func buildSearchFieldCheckPrompt() string {
	return strings.TrimSpace(`Analyze this phone screenshot of a system app search page right after automation pasted a query into its search field.
Decide whether the search field is still empty, which means the paste had no effect.
Return JSON only:
{
  "empty": true,
  "search_field": {"x": 500, "y": 80},
  "reason": "search field shows only its placeholder"
}

Rules:
- empty=true when the search field shows no typed text: only placeholder text such as "搜索" or "Search", or nothing at all.
- empty=false when any query text, pinyin, or uncommitted IME text is in the field.
- search_field is the center of the search text field in normalized 0-1000 coordinates, or null when no search field is visible.
- Keep reason short and concrete.`)
}

// clearSearchQueryText removes committed query text with Select All and
// Backspace, avoiding Escape, which could dismiss Spotlight.
func clearSearchQueryText(ctx context.Context, engine *textInputEngine) error {
	selectAll, err := textInputKeyboardKeysForSelectAll(engine.hw.platform())
	if err != nil {
		return err
	}
	if err := engine.tapKeys(ctx, selectAll); err != nil {
		return err
	}
	if err := engine.sleepFor(ctx, textInputKeystrokeGap); err != nil {
		return err
	}
	return engine.tapKeys(ctx, []string{"backspace"})
}

// typeQuickSearchQuery types an ASCII form of term without probing the input
// mode. System search lists apps for pinyin in both English and pinyin input,
// so only the pending composition must be committed (see
// appSearchQuickCommitGap). It reports false when no quick query applies.
func typeQuickSearchQuery(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine, term string) (bool, error) {
	platform := cfg.resolvedPlatform()
	if platform != "ios" || cfg.exactEntryOnly {
		return false, nil
	}
	query, err := appSearchQuickQuery(ctx, engine, term)
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return false, ctxErr
		}
		logging.Infof("agent", "app_search", "quick query unavailable term=%q: %v; using exact entry", term, err)
		return false, nil
	}
	logging.Infof("agent", "app_search", "query input route=local_hid_quick query=%q", query)
	if err := engine.typeASCII(ctx, query); err != nil {
		return false, err
	}
	switchKeys, err := textInputKeyboardKeysForIMESwitch(platform)
	if err != nil {
		return false, err
	}
	for tap := 0; tap < 2; tap++ {
		if err := engine.sleepFor(ctx, appSearchQuickCommitGap); err != nil {
			return false, err
		}
		if err := engine.tapKeys(ctx, switchKeys); err != nil {
			return false, err
		}
	}
	return true, nil
}

// appSearchQuickQuery converts term to lowercase ASCII, romanizing non-ASCII
// runs with the IME planner and dropping punctuation that only an IME can
// produce. Lowercase avoids Shift, which some IMEs treat as a mode switch.
// Spaces are dropped: typeASCII would tap Space between words, and Space
// selects the first candidate while pinyin is still composing. System search
// still lists "App Store" for "appstore".
func appSearchQuickQuery(ctx context.Context, engine *textInputEngine, term string) (string, error) {
	chunks, err := splitTextInputChunks(term)
	if err != nil {
		return "", err
	}
	var query strings.Builder
	for _, chunk := range chunks {
		switch {
		case chunk.ascii && chunk.text == chunk.input:
			query.WriteString(strings.ToLower(chunk.input))
		case textInputChunkIsPunctuation(chunk):
			continue
		default:
			segments, err := engine.compositionSegmentsForText(ctx, chunk.text, nil)
			if err != nil {
				return "", err
			}
			query.WriteString(strings.Join(segments, ""))
		}
	}
	result := strings.Join(strings.Fields(query.String()), "")
	if result == "" {
		return "", fmt.Errorf("no ASCII query for %q", term)
	}
	return result, nil
}

// searchQueryUnverifiedError means local entry ran but its result was not
// confirmed; the query may still be on screen as pending IME composition.
type searchQueryUnverifiedError struct {
	suggestion string
}

func (e *searchQueryUnverifiedError) Error() string {
	return "enter search query: " + e.suggestion
}

// The iOS PiP and Android foreground-service companions can write the
// clipboard without leaving the focused system search UI. Pasting the exact
// query also avoids local IME composition, which some Android vendors do not
// support for external keyboards. The existing result lookup, tap and app-open
// confirmation verify the outcome. route is empty when no background
// clipboard route applies.
func pasteSearchQuery(ctx context.Context, cfg appSearchOpenFlowConfig, term string) (route string, err error) {
	platform := cfg.resolvedPlatform()
	if cfg.entryTool.bridgeTool == nil {
		return "", nil
	}
	bridgeTool := cfg.entryTool.bridgeTool
	bridge := bridgeTool.currentBridge()
	if bridge == nil {
		return "", nil
	}
	status := bridge.getStatus()
	switch {
	case platform == "ios" && phoneBridgeCanUsePiPBackground(status, "clipboard_write"):
		route = "pip_clipboard_paste"
	case platform == "android" && phoneBridgeCanUseFGSBackground(status, "clipboard_write"):
		route = "fgs_clipboard_paste"
	default:
		return "", nil
	}
	if err := bridgeTool.writeClipboard(ctx, bridge, term); err != nil {
		return route, err
	}
	if err := bridgeTool.sleepAfterClipboardWrite(ctx); err != nil {
		return route, err
	}
	_, _, err = bridgeTool.pasteClipboard(ctx, platform)
	return route, err
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

// findSearchOpenAppResult also returns the search page screenshot the result
// was located on.
func findSearchOpenAppResult(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine, searchTerm string) (bridgeSearchResult, screenshotResult, int, error) {
	shot, err := engine.captureScreenshot(ctx)
	if err != nil {
		return bridgeSearchResult{}, screenshotResult{}, 0, err
	}
	if cfg.findAppTapFn != nil {
		result, err := cfg.findAppTapFn(ctx, shot, searchTerm)
		return result, shot, 0, err
	}
	modelVision, ok := cfg.vision.(*llmTextInputVision)
	if !ok || modelVision == nil {
		return bridgeSearchResult{}, shot, 0, fmt.Errorf("app search vision is not configured")
	}
	result, usedLLM, err := decide(ctx, modelVision.decider, appSearchDecision(modelVision, shot, searchTerm))
	return result, shot, vlmCallCount(usedLLM), err
}

func vlmCallCount(usedLLM bool) int {
	if usedLLM {
		return 1
	}
	return 0
}

// parseAppSearchResult ignores unknown fields: some models echo the response
// format next to the decision (deepseek-flash added "type":"json_object").
// visionDecisionJSON has already required "found".
func parseAppSearchResult(raw string) (bridgeSearchResult, error) {
	var result bridgeSearchResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return bridgeSearchResult{}, fmt.Errorf("parse app search result: %w", err)
	}
	if result.Found && result.TapPoint == nil {
		return bridgeSearchResult{}, fmt.Errorf("parse app search result: found result is missing tap_point")
	}
	return result, nil
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

func confirmSearchOpenApp(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine, searchTerm string, searchShot screenshotResult) (bridgeAppOpenResult, int, error) {
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
	llm := func(ctx context.Context) (bridgeAppOpenResult, error) {
		if searchShot.Data != "" {
			return confirmSearchLeftLLM(ctx, modelVision, searchShot, shot, searchTerm)
		}
		return confirmSearchOpenAppLLM(ctx, modelVision, shot, searchTerm)
	}
	result, usedLLM, err := decide(ctx, modelVision.decider, appOpenDecision(llm, shot, searchTerm))
	return result, vlmCallCount(usedLLM), err
}

// confirmSearchLeftLLM asks whether the screen left the system search page,
// comparing the search page the result was tapped on with the screen after the
// tap, instead of asking the model to recognize the target app. An app's first
// screen varies too much for that: Settings reopens on its own search page and
// Safari on a start page that looks like the home screen, which both read as
// "not opened" from one screenshot. In on-device iOS runs the comparison was
// right 16 of 16 times, where the single-screenshot question was right 12.
func confirmSearchLeftLLM(ctx context.Context, modelVision *llmTextInputVision, before, after screenshotResult, searchTerm string) (bridgeAppOpenResult, error) {
	raw, err := modelVision.visionDecisionJSON(ctx, "app_open_confirmation", buildSearchLeftPrompt(searchTerm), []string{"left_search"}, before, after)
	if err != nil {
		return bridgeAppOpenResult{}, err
	}
	var parsed struct {
		LeftSearch bool   `json:"left_search"`
		Reason     string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		return bridgeAppOpenResult{}, fmt.Errorf("parse app open confirmation: %w", err)
	}
	return bridgeAppOpenResult{Opened: parsed.LeftSearch, Reason: parsed.Reason}, nil
}

func buildSearchLeftPrompt(searchTerm string) string {
	return strings.TrimSpace(fmt.Sprintf(`You get two device screenshots. The first is a system search results page for query %q, captured just before tapping an app result. The second was captured shortly after the tap.
Decide whether the second screenshot has left that system search page for another screen.
Return JSON only:
{
  "left_search": true,
  "reason": "an app screen replaced the search results"
}

Rules:
- left_search=true when the second screenshot shows any screen other than the system search page and the launcher: an app screen, an app's own search or start page, a blank or dark app page, or a loading transition. You do not need to recognize the app.
- left_search=false when the second screenshot still shows the same system search page, with or without the keyboard, or the home screen.
- Keep reason short and concrete.`, searchTerm))
}

func confirmSearchOpenAppLLM(ctx context.Context, modelVision *llmTextInputVision, shot screenshotResult, searchTerm string) (bridgeAppOpenResult, error) {
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
		return bridgeAppOpenResult{}, err
	}
	var result bridgeAppOpenResult
	if err := json.Unmarshal([]byte(raw), &result); err != nil {
		return bridgeAppOpenResult{}, fmt.Errorf("parse app open confirmation: %w", err)
	}
	return result, nil
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

// checkPopupAfterOpen looks for a dialog over the freshly opened app. Ads are
// closed; anything else is returned to the Agent in the result.
func checkPopupAfterOpen(ctx context.Context, cfg appSearchOpenFlowConfig, engine *textInputEngine, steps []string, result appSearchOpenFlowResult) ([]string, appSearchOpenFlowResult) {
	shot, err := engine.captureScreenshot(ctx)
	if err != nil {
		logging.Warnf("agent", "popup", "after-open screenshot failed: %v", err)
		return steps, result
	}
	popup, calls, err := handlePopup(ctx, cfg.hw, cfg.vision, cfg.checkPopupFn, shot, cfg.platform, popupMomentAppOpen)
	result.VLMCalls += calls
	switch {
	case err != nil:
		logging.Warnf("agent", "popup", "after-open dialog handling failed: %v", err)
	case popup.Handled:
		steps = append(steps, fmt.Sprintf("closed %s dialog", popup.Kind))
	case popup.blocking():
		steps = append(steps, fmt.Sprintf("%s dialog is open", popup.Kind))
		result.Popup = &popup
	}
	return steps, result
}
