package agent

import (
	"context"
	"fmt"

	"aiden-agent/internal/decision"
)

// Questions the decision model answers for each text-input and app-open
// decision. Each mirrors the main model's prompt for the same decision, but as
// typed questions: Jev answers choices and probabilities, never free text.

func focusHint(focus focusPointArgs) string {
	return fmt.Sprintf("the focused text field near (%.0f, %.0f) on a 0-1000 grid over `screen`", focus.X, focus.Y)
}

// noulAnswer reads a noul answer and whether it is decisive at minConfidence.
func noulAnswer(resp decision.Response, id string, minConfidence float64) (yes bool, decisive bool, p float64, err error) {
	answer, err := resp.Answer(id, decision.TypeNoul)
	if err != nil {
		return false, false, 0, err
	}
	yes, decisive = decision.NoulConfident(*answer.Noul, minConfidence)
	return yes, decisive, *answer.Noul, nil
}

func (v *llmTextInputVision) probeDecision(screenshot screenshotResult, platform string, focus focusPointArgs) decisionPoint[textInputProbeAnalysis] {
	return decisionPoint[textInputProbeAnalysis]{
		op: DecisionOpIMEProbe,
		llm: func(ctx context.Context) (textInputProbeAnalysis, error) {
			return v.probeInputModeLLM(ctx, screenshot, platform, focus)
		},
		ask: func(ctx context.Context, client *decision.Client, minConfidence float64) (decisionOutcome[textInputProbeAnalysis], error) {
			resp, err := askOne(ctx, client, screenshot, map[string]any{"platform": platform}, map[string]decision.Question{
				"mode": {
					Type: decision.TypeChoice,
					Instructions: "Automation just typed the single lowercase letter \"a\" into " + focusHint(focus) +
						". Which keyboard input state does the field show? Judge the field and any candidate row or popup next to it; an on-screen keyboard may be absent.",
					Criteria: map[string]any{
						"composition": "the typed a is uncommitted IME preedit text, or a candidate bar or popup near the field shows Chinese (Han) candidates such as 啊 阿 爱",
						"ascii":       "the a was committed directly as ordinary text and no IME candidate bar or popup is visible near the field",
						"unknown":     "the focused field and the area around it are hidden or too unclear to inspect",
					},
				},
			})
			if err != nil {
				return decisionOutcome[textInputProbeAnalysis]{}, err
			}
			answer, err := resp.Answer("mode", decision.TypeChoice)
			if err != nil {
				return decisionOutcome[textInputProbeAnalysis]{}, err
			}
			mode, parseErr := parseObservedTextInputMode(answer.Choice)
			if parseErr != nil {
				mode = textInputModeUnknown
			}
			return decisionOutcome[textInputProbeAnalysis]{
				Value:      textInputProbeAnalysis{Mode: mode, Evidence: "decision model"},
				Confident:  answer.Confident(minConfidence) && mode != textInputModeUnknown,
				Confidence: answer.ConfidenceValue(),
			}, nil
		},
		summary: func(a textInputProbeAnalysis) string { return "mode=" + string(a.Mode) },
	}
}

func (v *llmTextInputVision) probeCleanupDecision(before, after screenshotResult, platform string, focus focusPointArgs) decisionPoint[bool] {
	return decisionPoint[bool]{
		op: DecisionOpProbeCleanup,
		llm: func(ctx context.Context) (bool, error) {
			return v.verifyProbeCleanupLLM(ctx, before, after, platform, focus)
		},
		ask: func(ctx context.Context, client *decision.Client, minConfidence float64) (decisionOutcome[bool], error) {
			beforeImg, err := screenshotAsset(before)
			if err != nil {
				return decisionOutcome[bool]{}, err
			}
			afterImg, err := screenshotAsset(after)
			if err != nil {
				return decisionOutcome[bool]{}, err
			}
			field := fmt.Sprintf("the focused text field near (%.0f, %.0f) on a 0-1000 grid", focus.X, focus.Y)
			resp, err := client.Ask(ctx, decision.Request{
				State: map[string]any{"before": beforeImg.Ref, "after": afterImg.Ref, "platform": platform},
				Assets: map[string]decision.Asset{
					beforeImg.Ref: beforeImg.Asset,
					afterImg.Ref:  afterImg.Asset,
				},
				Questions: map[string]decision.Question{
					"visible": {Type: decision.TypeNoul, Instructions: "`before` was captured before automation typed a probe letter \"a\" into " + field +
						"; `after` was captured after automation sent undo. In `after`, is a lowercase \"a\" that is not in `before` still visible at the insertion point, as committed text or uncommitted preedit?"},
					"safe": {Type: decision.TypeNoul, Instructions: "Comparing `before` and `after` in " + field +
						", would one Backspace in `after` remove only the probe letter \"a\" and none of the text already present in `before`?"},
				},
			})
			if err != nil {
				return decisionOutcome[bool]{}, err
			}
			visible, visibleSure, p, err := noulAnswer(resp, "visible", minConfidence)
			if err != nil {
				return decisionOutcome[bool]{}, err
			}
			safe, safeSure, _, err := noulAnswer(resp, "safe", minConfidence)
			if err != nil {
				return decisionOutcome[bool]{}, err
			}
			// A visible probe that is unsafe to delete is an error on the main
			// model's path; leave that case to it rather than guessing.
			confident := visibleSure && (!visible || (safeSure && safe))
			return decisionOutcome[bool]{Value: visible && safe, Confident: confident, Confidence: p}, nil
		},
		summary: func(remove bool) string { return fmt.Sprintf("remove_probe=%v", remove) },
	}
}

// screenAnalysisDecision lets the decision model correct only the match and
// pending flags: the main model also transcribes the field text, which the
// input engine reports and checks commits against. On-device runs showed the
// main model reading IME next-word predictions and search suggestions below a
// committed field as pending composition, where Jev was right.
func (v *llmTextInputVision) screenAnalysisDecision(screenshot screenshotResult, req textInputScreenAnalysisRequest) decisionPoint[textInputScreenAnalysis] {
	match := fmt.Sprintf("Does the text of %s read exactly %q, fully committed?", focusHint(req.Focus), req.TargetText)
	if req.MatchTextSuffix {
		match = fmt.Sprintf("Does the committed text of %s end with exactly %q?", focusHint(req.Focus), req.TargetText)
	}
	return decisionPoint[textInputScreenAnalysis]{
		op: DecisionOpScreenAnalysis,
		merge: func(jev, llm textInputScreenAnalysis) textInputScreenAnalysis {
			llm.TargetMatched = jev.TargetMatched
			llm.CompositionPending = jev.CompositionPending
			return llm
		},
		llm: func(ctx context.Context) (textInputScreenAnalysis, error) {
			return v.analyzeScreenLLM(ctx, screenshot, req)
		},
		ask: func(ctx context.Context, client *decision.Client, minConfidence float64) (decisionOutcome[textInputScreenAnalysis], error) {
			resp, err := askOne(ctx, client, screenshot, map[string]any{"platform": req.Platform}, map[string]decision.Question{
				"matched": {Type: decision.TypeNoul, Instructions: match + " Ignore candidate bars and uncommitted preedit text."},
				"pending": {Type: decision.TypeNoul, Instructions: "Is IME composition still pending in " + focusHint(req.Focus) +
					": uncommitted preedit text in the field, or an IME candidate bar or popup next to it?"},
			})
			if err != nil {
				return decisionOutcome[textInputScreenAnalysis]{}, err
			}
			matched, matchedSure, p, err := noulAnswer(resp, "matched", minConfidence)
			if err != nil {
				return decisionOutcome[textInputScreenAnalysis]{}, err
			}
			pending, pendingSure, _, err := noulAnswer(resp, "pending", minConfidence)
			if err != nil {
				return decisionOutcome[textInputScreenAnalysis]{}, err
			}
			return decisionOutcome[textInputScreenAnalysis]{
				Value:      textInputScreenAnalysis{TargetMatched: matched, CompositionPending: pending},
				Confident:  matchedSure && pendingSure,
				Confidence: p,
			}, nil
		},
		summary: func(a textInputScreenAnalysis) string {
			return fmt.Sprintf("matched=%v pending=%v", a.TargetMatched, a.CompositionPending)
		},
	}
}

// tapTargetOutcome answers "is the target visible, and where": a noul for
// presence plus a box, since a box answer has no "not found" result.
func tapTargetOutcome(resp decision.Response, minConfidence float64) (found bool, point *focusPointArgs, confident bool, p float64, err error) {
	found, foundSure, p, err := noulAnswer(resp, "found", minConfidence)
	if err != nil {
		return false, nil, false, 0, err
	}
	if !found {
		return false, nil, foundSure, p, nil
	}
	// A missing box (OpenAI has none) leaves the target found but untappable,
	// which is never confident.
	where, err := resp.Answer("where", decision.TypeBox)
	if err != nil {
		return true, nil, false, p, nil
	}
	point = boxPoint(where)
	return true, point, foundSure && point != nil && where.Confident(minConfidence), p, nil
}

func appSearchDecision(vision *llmTextInputVision, shot screenshotResult, searchTerm string) decisionPoint[bridgeSearchResult] {
	return decisionPoint[bridgeSearchResult]{
		op: DecisionOpAppSearch,
		llm: func(ctx context.Context) (bridgeSearchResult, error) {
			raw, err := vision.visionDecisionJSON(ctx, "app_search", buildAppSearchResultPrompt(searchTerm), []string{"found"}, shot)
			if err != nil {
				return bridgeSearchResult{}, err
			}
			return parseAppSearchResult(raw)
		},
		ask: func(ctx context.Context, client *decision.Client, minConfidence float64) (decisionOutcome[bridgeSearchResult], error) {
			resp, err := askOne(ctx, client, shot, map[string]any{"query": searchTerm}, map[string]decision.Question{
				"found": {Type: decision.TypeNoul, Instructions: "`screen` is a system search results page. Is a result visible that directly launches the app named by `query` itself? " +
					"Results in a Settings section (设置), app settings pages, \"Search in App\" actions, web suggestions, and content from other apps do not count."},
				"where": {Type: decision.TypeBox, Instructions: "the app icon of the topmost result in `screen` that directly launches the app named by `query`"},
			})
			if err != nil {
				return decisionOutcome[bridgeSearchResult]{}, err
			}
			found, point, confident, p, err := tapTargetOutcome(resp, minConfidence)
			if err != nil {
				return decisionOutcome[bridgeSearchResult]{}, err
			}
			// Only "found" is trusted. A screenshot taken before the results
			// settle, or after the app already opened, reads as "not found";
			// that answer goes to the main model, which then rechecks.
			return decisionOutcome[bridgeSearchResult]{
				Value:      bridgeSearchResult{Found: found, TapPoint: point, Label: "decision model"},
				Confident:  confident && found,
				Confidence: p,
			}, nil
		},
		summary: func(r bridgeSearchResult) string {
			return fmt.Sprintf("found=%v tap=%s", r.Found, decisionPointSummary(r.TapPoint))
		},
		agree: func(jev, llm bridgeSearchResult) bool {
			return jev.Found == llm.Found && (!jev.Found || pointsAgree(jev.TapPoint, llm.TapPoint))
		},
	}
}

func appOpenDecision(llm func(context.Context) (bridgeAppOpenResult, error), shot screenshotResult, searchTerm string) decisionPoint[bridgeAppOpenResult] {
	return decisionPoint[bridgeAppOpenResult]{
		op:  DecisionOpAppOpen,
		llm: llm,
		ask: func(ctx context.Context, client *decision.Client, minConfidence float64) (decisionOutcome[bridgeAppOpenResult], error) {
			resp, err := askOne(ctx, client, shot, map[string]any{"query": searchTerm}, map[string]decision.Question{
				"opened": {Type: decision.TypeNoul, Instructions: "`screen` was captured right after tapping the app search result for `query`. " +
					"Does it show the requested app's own screen, or a loading transition into it? A system search page, launcher, keyboard search results, or an unrelated app means no."},
			})
			if err != nil {
				return decisionOutcome[bridgeAppOpenResult]{}, err
			}
			opened, sure, p, err := noulAnswer(resp, "opened", minConfidence)
			if err != nil {
				return decisionOutcome[bridgeAppOpenResult]{}, err
			}
			// Only "opened" is trusted. Jev reads the dimmed app-switch
			// transition as "not opened", sometimes confidently, so a "not
			// opened" answer always goes to the main model.
			return decisionOutcome[bridgeAppOpenResult]{
				Value:      bridgeAppOpenResult{Opened: opened, Reason: fmt.Sprintf("decision model p=%.2f", p)},
				Confident:  sure && opened,
				Confidence: p,
			}, nil
		},
		summary: func(r bridgeAppOpenResult) string { return fmt.Sprintf("opened=%v", r.Opened) },
	}
}

func androidSearchDecision(llm func(context.Context) (androidSearchCheck, error), shot screenshotResult) decisionPoint[androidSearchCheck] {
	return decisionPoint[androidSearchCheck]{
		op:  DecisionOpAndroidSearchEntry,
		llm: llm,
		ask: func(ctx context.Context, client *decision.Client, minConfidence float64) (decisionOutcome[androidSearchCheck], error) {
			resp, err := askOne(ctx, client, shot, nil, map[string]decision.Question{
				"open": {Type: decision.TypeNoul, Instructions: "Does this Android `screen` show a launcher or system app search page: a page built around a search field, usually with app suggestions, recent searches, or empty results? " +
					"Judge the page itself, not a cursor or keyboard. The notification shade, quick settings, the home screen (even with a search widget), an app drawer still showing its app grid, a browser, or another app mean no."},
				"found": {Type: decision.TypeNoul, Instructions: "Is an app search bar visible in `screen`, such as the search bar at the top of an app drawer?"},
				"where": {Type: decision.TypeBox, Instructions: "the app search bar in `screen`"},
			})
			if err != nil {
				return decisionOutcome[androidSearchCheck]{}, err
			}
			open, openSure, p, err := noulAnswer(resp, "open", minConfidence)
			if err != nil {
				return decisionOutcome[androidSearchCheck]{}, err
			}
			check := androidSearchCheck{SearchOpen: open, Reason: fmt.Sprintf("decision model p=%.2f", p)}
			if open {
				return decisionOutcome[androidSearchCheck]{Value: check, Confident: openSure, Confidence: p}, nil
			}
			barFound, bar, barSure, _, err := tapTargetOutcome(resp, minConfidence)
			if err != nil {
				return decisionOutcome[androidSearchCheck]{}, err
			}
			if barFound {
				check.SearchField = bar
			}
			return decisionOutcome[androidSearchCheck]{Value: check, Confident: openSure && barSure, Confidence: p}, nil
		},
		summary: func(c androidSearchCheck) string {
			return fmt.Sprintf("search_open=%v field=%s", c.SearchOpen, decisionPointSummary(c.SearchField))
		},
		agree: func(jev, llm androidSearchCheck) bool {
			return jev.SearchOpen == llm.SearchOpen && (jev.SearchOpen || pointsAgree(jev.SearchField, llm.SearchField))
		},
	}
}

func pasteMenuDecision(llm func(context.Context) (pasteMenuResult, error), shot screenshotResult, platform string) decisionPoint[pasteMenuResult] {
	return decisionPoint[pasteMenuResult]{
		op:  DecisionOpPasteMenu,
		llm: llm,
		ask: func(ctx context.Context, client *decision.Client, minConfidence float64) (decisionOutcome[pasteMenuResult], error) {
			resp, err := askOne(ctx, client, shot, map[string]any{"platform": platform}, map[string]decision.Question{
				"found": {Type: decision.TypeNoul, Instructions: "`screen` was captured after a long press inside a focused text field. Is a system context-menu action visible that pastes the clipboard, labelled Paste, 粘贴, or a localized equivalent?"},
				"where": {Type: decision.TypeBox, Instructions: "the Paste (粘贴) action in the context menu in `screen`, not Select, Look Up, Share, or Autofill"},
			})
			if err != nil {
				return decisionOutcome[pasteMenuResult]{}, err
			}
			found, point, confident, p, err := tapTargetOutcome(resp, minConfidence)
			if err != nil {
				return decisionOutcome[pasteMenuResult]{}, err
			}
			return decisionOutcome[pasteMenuResult]{
				Value:      pasteMenuResult{Found: found, TapPoint: point, Label: "decision model"},
				Confident:  confident,
				Confidence: p,
			}, nil
		},
		summary: func(r pasteMenuResult) string {
			return fmt.Sprintf("found=%v tap=%s", r.Found, decisionPointSummary(r.TapPoint))
		},
		agree: func(jev, llm pasteMenuResult) bool {
			return jev.Found == llm.Found && (!jev.Found || pointsAgree(jev.TapPoint, llm.TapPoint))
		},
	}
}
