package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestOpenAppSearchPastesBackgroundClipboardBeforeTyping(t *testing.T) {
	const platform = "ios"
	for _, query := range []string{"微信", "WeChat"} {
		t.Run(platform+"/"+query, func(t *testing.T) {
			pb := newTestPhoneBridge(t)
			pb.platform = platform
			pb.appState = "background"
			pb.appStateAt = time.Now().Add(-phoneBridgeBackgroundStateMaxAge - time.Second)
			pb.pipBridgeSeen, pb.pipBridgeEnabled = true, true
			// Search must refresh the clipboard even if the board remembers
			// the same text: another app may have changed the pasteboard.
			pb.NoteClipboardWrite(query)
			quick := &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`}
			touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
			keyboard := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
			hw := &textInputHardwareDeps{
				deviceTypeFn: func() string { return platform },
				quickAction:  quick, touchGesture: touch, keyboardText: keyboard,
				keyboardTap: &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
				screenshot:  textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
			}
			vision := unexpectedSearchFieldVision{t: t}
			entry := &EnterTextTool{
				engine:     newFastTextInputEngine(*hw, vision),
				bridgeTool: &textInputBridge{hw: hw, vision: vision, bridgeFn: func() *PhoneBridge { return pb }, sleep: testNoWaitSleep},
			}
			search := &appSearchOpenTool{
				hw: hw, vision: vision, entryTool: entry, deviceTypeFn: hw.deviceTypeFn, sleep: testNoWaitSleep,
				findAppTapFn: func(context.Context, screenshotResult, string) (bridgeSearchResult, error) {
					return bridgeSearchResult{Found: true, TapPoint: &focusPointArgs{X: 200, Y: 300}}, nil
				},
				confirmAppOpenFn: func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error) {
					return bridgeAppOpenResult{Opened: true}, nil
				},
			}
			queueResult := make(chan error, 1)
			go func() {
				cmd, err := waitForQueuedBridgeCommand(pb.queue, platform, time.Second)
				if err != nil {
					queueResult <- err
					return
				}
				var payload struct {
					Text string `json:"text"`
				}
				if err := json.Unmarshal(cmd.Payload, &payload); err != nil || cmd.Type != "clipboard_write" || payload.Text != query {
					queueResult <- fmt.Errorf("unexpected clipboard command: %+v", cmd)
					return
				}
				queueResult <- pb.queue.SubmitResult(BridgeCommandResponse{ID: cmd.ID, Method: "clipboard_write"})
			}()
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			out, err := NewOpenAppTool(pb, nil, search).Call(ctx, jsonString(map[string]string{"app": query}))
			if queueErr := <-queueResult; queueErr != nil {
				t.Fatal(queueErr)
			}
			if err != nil || !enterTextOutputOK(out, err) {
				t.Fatalf("open_app = %s, %v", out, err)
			}
			if len(keyboard.calls) != 0 {
				t.Fatalf("keyboard_text = %v, want no typing after verified paste", keyboard.calls)
			}
			if len(quick.calls) != 2 || !strings.Contains(quick.calls[0], "spotlight_search") || !strings.Contains(quick.calls[1], "paste") {
				t.Fatalf("quick actions = %v, want search then paste without app switching", quick.calls)
			}
			if len(touch.calls) != 1 || !strings.Contains(touch.calls[0], "200") {
				t.Fatalf("touch calls = %v, want only app result tap", touch.calls)
			}
		})
	}
}

// A successful paste must proceed to app lookup without field OCR or an
// input-mode probe, even if an OCR model would disagree about the field text.
type unexpectedSearchFieldVision struct{ t *testing.T }

func (v unexpectedSearchFieldVision) AnalyzeScreen(context.Context, screenshotResult, textInputScreenAnalysisRequest) (textInputScreenAnalysis, error) {
	v.t.Fatal("search paste must be verified by app results, not field OCR")
	return textInputScreenAnalysis{}, nil
}

func TestSearchPiPClipboardFailureFallsBackToLocal(t *testing.T) {
	pb := newTestPhoneBridge(t)
	pb.platform, pb.appState = "ios", "background"
	pb.pipBridgeSeen, pb.pipBridgeEnabled = true, true
	keyboard := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	keys := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	quick := &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`}
	touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
	hw := &textInputHardwareDeps{
		deviceTypeFn: func() string { return "ios" },
		keyboardText: keyboard, keyboardTap: keys, quickAction: quick, touchGesture: touch,
		screenshot: textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
	}
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}
	writes := 0
	entry := &EnterTextTool{
		engine: newFastTextInputEngine(*hw, vision), allowIOSKeyboardIsolationBypass: true,
		bridgeTool: &textInputBridge{
			hw: hw, bridgeFn: func() *PhoneBridge { return pb }, sleep: testNoWaitSleep,
			clipboardWriteFn: func(context.Context, *PhoneBridge, string) error {
				writes++
				return fmt.Errorf("clipboard unavailable")
			},
		},
	}
	result, err := runAppSearchOpenFlow(context.Background(), appSearchOpenFlowConfig{
		hw: hw, vision: vision, platform: "ios", searchTerm: "Demo", entryTool: entry, sleep: testNoWaitSleep,
		findAppTapFn: func(context.Context, screenshotResult, string) (bridgeSearchResult, error) {
			return bridgeSearchResult{Found: true, TapPoint: &focusPointArgs{X: 200, Y: 300}}, nil
		},
		confirmAppOpenFn: func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error) {
			return bridgeAppOpenResult{Opened: true}, nil
		},
	})
	if err != nil || !result.Opened || writes != 1 || len(keyboard.calls) != 2 || len(touch.calls) != 1 {
		t.Fatalf("result=%+v err=%v writes=%d keyboard=%v taps=%v", result, err, writes, keyboard.calls, touch.calls)
	}
	if len(keys.calls) < 2 || !strings.Contains(keys.calls[0], `"meta"`) || !strings.Contains(keys.calls[0], `"a"`) || !strings.Contains(keys.calls[1], "backspace") {
		t.Fatalf("keys=%v, want clear before local fallback", keys.calls)
	}
	for _, call := range keys.calls {
		if strings.Contains(call, "escape") {
			t.Fatal("Escape can dismiss system search during recovery")
		}
	}
}

func TestSearchQueryDoesNotRestoreCompanionOrUseUnsupportedClipboardRoute(t *testing.T) {
	for _, state := range []string{"no_pip", "foreground", "android_fgs"} {
		t.Run(state, func(t *testing.T) {
			pb := newTestPhoneBridge(t)
			pb.platform, pb.appState = "ios", "background"
			pb.appStateAt = time.Now()
			pb.returnEntry, pb.returnEntryOK, pb.returnEntrySeen = "dynamic_island", true, true
			if state == "foreground" {
				pb.appState, pb.connected = "active", true
			}
			if state == "android_fgs" {
				pb.platform = "android"
				pb.fgsBridgeSeen, pb.fgsBridgeEnabled = true, true
				pb.fgsBridgeAt = time.Now()
			}
			pb.NoteClipboardWrite("Aiden")
			keyboard := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
			hw := &textInputHardwareDeps{
				deviceTypeFn: func() string { return pb.platform },
				keyboardText: keyboard,
				keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
				screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
			}
			vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}
			entry := &EnterTextTool{
				engine:                          newFastTextInputEngine(*hw, vision),
				allowIOSKeyboardIsolationBypass: true,
				bridgeTool: &textInputBridge{
					bridgeFn: func() *PhoneBridge { return pb },
					clipboardWriteFn: func(context.Context, *PhoneBridge, string) error {
						t.Fatal("clipboard write attempted without a target-preserving route")
						return nil
					},
				},
			}
			if err := enterSearchQuery(context.Background(), appSearchOpenFlowConfig{hw: hw, vision: vision, entryTool: entry, sleep: testNoWaitSleep}, "Aiden", false); err != nil {
				t.Fatal(err)
			}
			if len(keyboard.calls) != 2 {
				t.Fatalf("keyboard calls = %v, want local probe and query", keyboard.calls)
			}
		})
	}
}

func newSearchIsolationTestTool(t *testing.T, events *[]string, keys *recordingTextInputTool, entryVision textInputVision, lookup func(context.Context) bridgeSearchResult) (*appSearchOpenTool, *bool) {
	t.Helper()
	controller := newTestIOSKeyboardIsolationController(events)
	hw := &textInputHardwareDeps{
		deviceTypeFn: func() string { return "ios" },
		quickAction:  textInputStubTool{name: "quick_action", out: `{"ok":true}`},
		keyboardTap:  keys, keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
		touchGesture: iosIsolationPointerTestTool{controller: controller, events: events},
		screenshot:   textInputStubTool{name: "screenshot", out: `{"data":"abc"}`},
	}
	confirmed := false
	tool := &appSearchOpenTool{
		hw: hw, vision: entryVision,
		entryTool:            &EnterTextTool{engine: newFastTextInputEngine(*hw, entryVision), iosKeyboardIsolation: controller},
		iosKeyboardIsolation: controller, sleep: testNoWaitSleep,
		findAppTapFn: func(ctx context.Context, _ screenshotResult, _ string) (bridgeSearchResult, error) {
			if batch := controller.batchFromContext(ctx); batch != nil && batch.isolated {
				t.Fatal("result lookup must observe the screen after the pointer profile is restored")
			}
			return lookup(ctx), nil
		},
		confirmAppOpenFn: func(context.Context, screenshotResult, string) (bridgeAppOpenResult, error) {
			confirmed = true
			return bridgeAppOpenResult{Opened: true}, nil
		},
	}
	return tool, &confirmed
}

func TestOpenAppSearchChecksResultsAfterUnverifiedEntry(t *testing.T) {
	events := []string{}
	keys := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	vision := &plannedTextInputVision{
		stubTextInputVision: &stubTextInputVision{analyses: []textInputScreenAnalysis{
			{ObservedMode: textInputModeComposition}, // Probe.
			{ObservedMode: textInputModeUnknown},     // Entry verification fails.
		}},
		plans: map[string][]string{"微信": {"wei", "xin"}},
	}
	tool, confirmed := newSearchIsolationTestTool(t, &events, keys, vision, func(context.Context) bridgeSearchResult {
		// The app is listed while "wei xin" is still uncommitted IME text.
		return bridgeSearchResult{Found: true, Label: "微信", TapPoint: &focusPointArgs{X: 170, Y: 170}}
	})
	out, err := tool.Call(context.Background(), `{"app":"微信"}`)
	if err != nil || !enterTextOutputOK(out, err) || !*confirmed {
		t.Fatalf("open_app=%s, %v; confirmed=%t", out, err, *confirmed)
	}
	if !reflect.DeepEqual(events, []string{"isolate", "restore", "pointer"}) {
		t.Fatalf("HID events=%v", events)
	}
	if !strings.Contains(out, "search input unverified; checking results") {
		t.Fatalf("steps do not record the unverified entry: %s", out)
	}
}

func TestOpenAppSearchDoesNotTapWhenUnverifiedEntryHasNoResult(t *testing.T) {
	events := []string{}
	keys := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	vision := &plannedTextInputVision{
		stubTextInputVision: &stubTextInputVision{analyses: []textInputScreenAnalysis{
			{ObservedMode: textInputModeComposition},
			{ObservedMode: textInputModeUnknown},
		}},
		plans: map[string][]string{"微信": {"wei", "xin"}},
	}
	tool, confirmed := newSearchIsolationTestTool(t, &events, keys, vision, func(context.Context) bridgeSearchResult {
		return bridgeSearchResult{Found: false}
	})
	tool.searchTermFn = func(string) string { return "微信" }
	out, err := tool.Call(context.Background(), `{"app":"微信"}`)
	if err != nil || enterTextOutputOK(out, err) || *confirmed {
		t.Fatalf("open_app=%s, %v; confirmed=%t", out, err, *confirmed)
	}
	for _, event := range events {
		if event == "pointer" {
			t.Fatalf("tapped without a located result: events=%v", events)
		}
	}
}

func TestOpenAppSearchRestoresHIDBeforeLookupAfterVerifiedEntry(t *testing.T) {
	events := []string{}
	keys := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	vision := &plannedTextInputVision{
		stubTextInputVision: &stubTextInputVision{analyses: []textInputScreenAnalysis{
			{ObservedMode: textInputModeComposition},
			{ObservedMode: textInputModeComposition, FieldText: "豆包"},
		}},
		plans: map[string][]string{"豆包": {"dou", "bao"}},
	}
	var delays []time.Duration
	tool, confirmed := newSearchIsolationTestTool(t, &events, keys, vision, func(context.Context) bridgeSearchResult {
		if len(vision.analyses) != 0 {
			t.Fatal("verified entry must not need another composition check")
		}
		return bridgeSearchResult{Found: true, Label: "豆包", TapPoint: &focusPointArgs{X: 150, Y: 160}}
	})
	tool.sleep = func(_ context.Context, delay time.Duration) error {
		delays = append(delays, delay)
		return nil
	}
	out, err := tool.Call(context.Background(), `{"app":"豆包"}`)
	if err != nil || !enterTextOutputOK(out, err) || !*confirmed {
		t.Fatalf("open_app=%s, %v; confirmed=%t", out, err, *confirmed)
	}
	if !reflect.DeepEqual(events, []string{"isolate", "restore", "pointer"}) {
		t.Fatalf("HID events=%v", events)
	}
	found := false
	for _, delay := range delays {
		found = found || delay == appSearchPointerRestoreSettleDelay
	}
	if !found {
		t.Fatalf("delays=%v, want pointer restore settle %s", delays, appSearchPointerRestoreSettleDelay)
	}
}
