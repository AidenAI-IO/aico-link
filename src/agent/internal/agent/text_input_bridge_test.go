package agent

import (
	"aico-agent/internal/ble"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	langtools "github.com/tmc/langchaingo/tools"
)

func callEnterTextBridgeForTest(ctx context.Context, tool *textInputBridge, input string) (string, error) {
	var args textInputArgs
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return "", err
	}
	result, _ := tool.runClipboardFirstResult(ctx, args, true)
	return jsonString(result), nil
}

func newPhoneBridgeForTest() *PhoneBridge {
	bridge := NewPhoneBridge(newTestLogger())
	bridge.bleStatus = func(context.Context) (ble.RuntimeStatus, error) {
		return ble.RuntimeStatus{}, nil
	}
	return bridge
}

func newTestPhoneBridge(t *testing.T) *PhoneBridge {
	t.Helper()
	pb := newPhoneBridgeForTest()
	t.Cleanup(func() {
		pb.queue.Stop()
	})
	return pb
}

func waitForQueuedBridgeCommand(queue *CommandQueue, platform string, timeout time.Duration) (BridgeCommand, error) {
	deadline := time.Now().Add(timeout)
	for {
		commands := queue.PollForPhone(platform, "", 10)
		if len(commands) > 0 {
			if len(commands) != 1 {
				return BridgeCommand{}, fmt.Errorf("expected one queued command, got %d", len(commands))
			}
			return commands[0], nil
		}
		if time.Now().After(deadline) {
			return BridgeCommand{}, fmt.Errorf("timed out waiting for queued %s command", platform)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestTextInputBridgeWriteClipboardIsolatesNestedToolError(t *testing.T) {
	pb := newTestPhoneBridge(t)
	tool := &textInputBridge{}
	ctx, _ := WithToolError(context.Background())

	err := tool.writeClipboard(ctx, pb, "小红书")
	if err == nil {
		t.Fatal("expected clipboard write to fail when phone bridge is disconnected")
	}
	if !strings.Contains(err.Error(), "phone bridge not connected") {
		t.Fatalf("error = %v, want phone bridge not connected", err)
	}
	if got := ToolErrorFromContext(ctx); got != nil {
		t.Fatalf("parent ToolError = %+v, want nil after nested clipboard isolation", got)
	}
}

func TestTextInputBridgePreparedClipboardRequiresExactText(t *testing.T) {
	pb := newTestPhoneBridge(t)
	pb.NoteClipboardWrite("hello")
	if !pb.ClipboardRecentlyContains(" hello ", preparedClipboardMaxAge) {
		t.Fatal("legacy contains check should continue ignoring surrounding whitespace")
	}
	if pb.ClipboardRecentlyEquals(" hello ", preparedClipboardMaxAge) {
		t.Fatal("text entry must not reuse clipboard content with different whitespace")
	}
	if !pb.ClipboardRecentlyEquals("hello", preparedClipboardMaxAge) {
		t.Fatal("exact clipboard content should be reusable")
	}
}

func TestTextInputBridgeUsesPiPBackgroundClipboardQueueWithStaleAppState(t *testing.T) {
	message := "桥接输入测试"
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{
		ObservedMode: textInputModeComposition,
		FieldText:    message,
	}}}
	pb := newTestPhoneBridge(t)
	pb.platform = "ios"
	pb.appState = "background"
	pb.appStateAt = time.Now().Add(-phoneBridgeBackgroundStateMaxAge - time.Second)
	pb.pipBridgeEnabled = true
	pb.pipBridgeSeen = true
	touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
	keyboardText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	quick := &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`}
	tool := &textInputBridge{
		hw: &textInputHardwareDeps{
			touchGesture: touch,
			keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
			keyboardText: keyboardText,
			quickAction:  quick,
			screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
		},
		vision:   vision,
		bridgeFn: func() *PhoneBridge { return pb },
		sleep:    testNoWaitSleep,
	}

	queueResult := make(chan string, 1)
	go func() {
		command, err := waitForQueuedBridgeCommand(pb.queue, "ios", 500*time.Millisecond)
		if err != nil {
			queueResult <- err.Error()
			return
		}
		if command.Type != "clipboard_write" {
			queueResult <- "unexpected queued command type: " + command.Type
			return
		}
		if !strings.Contains(string(command.Payload), message) {
			queueResult <- "queued clipboard payload missing target text: " + string(command.Payload)
			return
		}
		if err := pb.queue.SubmitResult(BridgeCommandResponse{ID: command.ID, Method: "queued"}); err != nil {
			queueResult <- "submit queued clipboard result failed: " + err.Error()
			return
		}
		queueResult <- ""
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	out, err := callEnterTextBridgeForTest(ctx, tool, `{"text":"`+message+`","focus":{"x":400,"y":950}}`)
	if err != nil {
		t.Fatal(err)
	}
	if queueErr := <-queueResult; queueErr != "" {
		t.Fatal(queueErr)
	}
	if !strings.Contains(out, `"committed": true`) {
		t.Fatalf("unexpected output: %s", out)
	}
	if len(quick.calls) != 1 || !strings.Contains(quick.calls[0], `"action": "paste"`) {
		t.Fatalf("quick_action calls=%v", quick.calls)
	}
	if len(touch.calls) != 1 || !strings.Contains(touch.calls[0], `"type": "tap"`) {
		t.Fatalf("touch_gesture calls=%v, want one focus tap and no app switching in PiP background path", touch.calls)
	}
	if len(keyboardText.calls) != 0 {
		t.Fatalf("keyboard_text calls=%v, want no bridge app search in PiP background path", keyboardText.calls)
	}
}

func TestTextInputBridgeUsesEnabledPiPDespiteStaleAppState(t *testing.T) {
	enabled := true
	status := PhoneBridgeStatus{
		Platform:          "ios",
		AppState:          "background",
		PipBridgeEnabled:  &enabled,
		AppStateUpdatedAt: ptrTime(time.Now().Add(-phoneBridgeBackgroundStateMaxAge - time.Second)),
	}
	if !phoneBridgeCanUsePiPBackground(status, "clipboard_write") {
		t.Fatal("enabled PiP bridge should allow clipboard queue despite stale app-state timestamp")
	}
}

func TestTextInputBridgeUsesClipboardPathAndVerifiesField(t *testing.T) {
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{
		ObservedMode: textInputModeASCII,
		FieldText:    "hello world",
	}}}
	touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
	quick := &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`}
	pb := newTestPhoneBridge(t)
	pb.connected = true
	tool := &textInputBridge{
		hw: &textInputHardwareDeps{
			pointerMode:  "absolute",
			deviceTypeFn: func() string { return "Android" },
			touchGesture: touch,
			keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
			keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
			quickAction:  quick,
			screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
		},
		vision:   vision,
		bridgeFn: func() *PhoneBridge { return pb },
		sleep:    testNoWaitSleep,
		clipboardWriteFn: func(_ context.Context, _ *PhoneBridge, text string) error {
			if text != "hello world" {
				t.Fatalf("clipboard text = %q", text)
			}
			return nil
		},
	}
	out, err := callEnterTextBridgeForTest(context.Background(), tool, `{"text":"hello world","focus":{"x":500,"y":100}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"committed": true`) {
		t.Fatalf("unexpected output: %s", out)
	}
	if len(touch.calls) != 1 || !strings.Contains(touch.calls[0], `"type": "tap"`) {
		t.Fatalf("touch_gesture calls=%v, want one focus tap and no app switching on Android target-preserving clipboard path", touch.calls)
	}
	if len(quick.calls) != 1 || !strings.Contains(quick.calls[0], `"action": "paste"`) {
		t.Fatalf("quick_action calls=%v", quick.calls)
	}
}

func TestTextInputBridgeReturnsLastObservedFieldTextAfterFailedPasteAttempts(t *testing.T) {
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{
		ObservedMode: textInputModeASCII,
		FieldText:    "hello",
	}, {
		ObservedMode: textInputModeASCII,
		FieldText:    "hello wor",
	}}}
	pb := newTestPhoneBridge(t)
	pb.connected = true
	tool := &textInputBridge{
		hw: &textInputHardwareDeps{
			pointerMode:  "absolute",
			deviceTypeFn: func() string { return "Android" },
			touchGesture: &recordingTextInputTool{name: "touch_gesture", out: "ok"},
			keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
			keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
			quickAction:  &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`},
			screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
		},
		vision:           vision,
		bridgeFn:         func() *PhoneBridge { return pb },
		sleep:            testNoWaitSleep,
		clipboardWriteFn: func(context.Context, *PhoneBridge, string) error { return nil },
	}
	out, err := callEnterTextBridgeForTest(context.Background(), tool, `{"text":"hello world","focus":{"x":500,"y":100}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"committed": false`) || !strings.Contains(out, `"field_text": "hello"`) {
		t.Fatalf("unexpected output: %s", out)
	}
}

func TestTextInputBridgeFallsBackToKeyboardPasteWhenQuickActionFails(t *testing.T) {
	message := "Example Contact number 555-0101 still active?"
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{
		ObservedMode: textInputModeASCII,
		FieldText:    message,
	}}}
	pb := newTestPhoneBridge(t)
	pb.NoteClipboardWrite(message)
	pb.platform = "ios"
	pb.appState = "background"
	pb.appStateAt = time.Now()
	keyboardTap := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	quick := &stubTool{name: "quick_action", callFn: func(ctx context.Context, _ string) (string, error) {
		return toolErrorResultString(ctx, CodeQuickActionReserved, "paste unavailable"), nil
	}}
	tool := &textInputBridge{
		hw: &textInputHardwareDeps{
			touchGesture: &recordingTextInputTool{name: "touch_gesture", out: "ok"},
			keyboardTap:  keyboardTap,
			keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
			quickAction:  quick,
			screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
		},
		vision:   vision,
		bridgeFn: func() *PhoneBridge { return pb },
		sleep:    testNoWaitSleep,
	}
	out, err := callEnterTextBridgeForTest(context.Background(), tool, `{"text":"`+message+`","focus":{"x":400,"y":950}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"committed": true`) {
		t.Fatalf("unexpected output: %s", out)
	}
	if len(quick.inputs) != 1 || !strings.Contains(quick.inputs[0], `"action": "paste"`) {
		t.Fatalf("quick_action calls=%v", quick.inputs)
	}
	if len(keyboardTap.calls) != 1 || !strings.Contains(keyboardTap.calls[0], `"meta"`) || !strings.Contains(keyboardTap.calls[0], `"v"`) {
		t.Fatalf("keyboard_tap calls=%v", keyboardTap.calls)
	}
}

func TestTextInputBridgeObservesFieldBeforeFallbackAfterShortcutError(t *testing.T) {
	message := "已由快捷键粘贴"
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{
		ObservedMode: textInputModeComposition,
		FieldText:    message,
	}}}
	pb := newTestPhoneBridge(t)
	pb.NoteClipboardWrite(message)
	pb.platform = "ios"
	pb.appState = "background"
	pb.appStateAt = time.Now()
	touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
	tool := &textInputBridge{
		hw: &textInputHardwareDeps{
			touchGesture: touch,
			keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: `{"ok":false,"message":"keyboard report unavailable"}`},
			keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
			quickAction:  &recordingTextInputTool{name: "quick_action", out: `{"ok":false,"message":"post-action capture failed"}`},
			screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
		},
		vision:   vision,
		bridgeFn: func() *PhoneBridge { return pb },
		sleep:    testNoWaitSleep,
		findPasteMenuFn: func(context.Context, screenshotResult, string) (pasteMenuResult, error) {
			t.Fatal("paste menu lookup must not run when fresh observation shows the target text")
			return pasteMenuResult{}, nil
		},
	}

	out, err := callEnterTextBridgeForTest(context.Background(), tool, `{"text":"`+message+`","focus":{"x":300,"y":940}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"committed": true`) {
		t.Fatalf("unexpected output: %s", out)
	}
	if len(touch.calls) != 1 || !strings.Contains(touch.calls[0], `"type": "tap"`) {
		t.Fatalf("touch_gesture calls=%v, want only the initial focus tap after observed success", touch.calls)
	}
}

func TestTextInputBridgeRestoresViaSharedPhoneBridgeRestorerAndSwitchesBack(t *testing.T) {
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{
		ObservedMode: textInputModeASCII, FieldText: "hello world", TargetMatched: true,
	}}}
	pb := newTestPhoneBridge(t)
	pb.platform = "ios"
	pb.appState = "background"
	pb.returnEntry = "dynamic_island"
	pb.returnEntrySeen = true
	pb.returnEntryOK = true
	restoreCalls := 0
	restorer := NewPhoneBridgeRestorer(pb, nil)
	restorer.waitTimeout = time.Second
	restorer.tapReturnEntry = func(context.Context, PhoneBridgeStatus) error {
		restoreCalls++
		pb.mu.Lock()
		pb.connected = true
		pb.appState = "active"
		pb.mu.Unlock()
		return nil
	}

	keyboardText := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
	quickAction := &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`}
	bridgePath := &textInputBridge{
		hw: &textInputHardwareDeps{
			pointerMode:  "absolute",
			touchGesture: &recordingTextInputTool{name: "touch_gesture", out: "ok"},
			keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
			keyboardText: keyboardText,
			quickAction:  quickAction,
			screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
		},
		vision:   vision,
		bridgeFn: func() *PhoneBridge { return pb },
		restorer: restorer,
		sleep:    testNoWaitSleep,
		clipboardWriteFn: func(_ context.Context, _ *PhoneBridge, text string) error {
			if text != "hello world" {
				t.Fatalf("clipboard text = %q", text)
			}
			return nil
		},
	}
	controller := &iosKeyboardIsolationController{
		controlPath: "test",
		run: func(context.Context, string, ...string) ([]byte, error) {
			return nil, nil
		},
	}
	entry := &EnterTextTool{
		engine:               newTextInputEngineWithSleep(*bridgePath.hw, vision, testNoWaitSleep),
		bridgeTool:           bridgePath,
		iosKeyboardIsolation: controller,
	}

	out, err := entry.Call(context.Background(), `{"text":"hello world","focus":{"x":500,"y":100}}`)
	if err != nil {
		t.Fatal(err)
	}
	var result enterTextToolResult
	if err := json.Unmarshal([]byte(out), &result); err != nil || !result.OK {
		t.Fatalf("unexpected output: %s (err=%v)", out, err)
	}
	if !pb.connected {
		t.Fatal("expected bridge to reconnect before clipboard write")
	}
	if restoreCalls != 1 {
		t.Fatalf("shared bridge restore calls=%d, want 1", restoreCalls)
	}
	if len(quickAction.calls) != 2 ||
		!strings.Contains(quickAction.calls[0], `"action": "app_switch_back"`) ||
		!strings.Contains(quickAction.calls[1], `"action": "paste"`) {
		t.Fatalf("quick_action calls=%v", quickAction.calls)
	}
	if len(keyboardText.calls) != 0 {
		t.Fatalf("keyboard_text calls=%v, want no Aico search input", keyboardText.calls)
	}
}

func TestTextInputBridgeAllowsPastePermissionDialogBeforeMenuFallback(t *testing.T) {
	message := "权限弹窗测试"
	vision := &stubTextInputVision{analyses: []textInputScreenAnalysis{{
		ObservedMode: textInputModeComposition,
		FieldText:    "",
	}, {
		ObservedMode: textInputModeComposition,
		FieldText:    message,
	}}}
	pb := newTestPhoneBridge(t)
	pb.NoteClipboardWrite(message)
	pb.platform = "ios"
	pb.appState = "background"
	pb.appStateAt = time.Now()
	touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
	checks := 0
	tool := &textInputBridge{
		hw: &textInputHardwareDeps{
			touchGesture: touch,
			keyboardTap:  &recordingTextInputTool{name: "keyboard_tap", out: "ok"},
			keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
			quickAction:  &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`},
			screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
		},
		vision:   vision,
		bridgeFn: func() *PhoneBridge { return pb },
		sleep:    testNoWaitSleep,
		findPopupFn: func(context.Context, screenshotResult, string) (popupCheck, error) {
			checks++
			return popupCheck{Kind: popupKindPastePermission, Summary: "允许粘贴", SafeTap: &focusPointArgs{X: 700, Y: 560}}, nil
		},
		findPasteMenuFn: func(context.Context, screenshotResult, string) (pasteMenuResult, error) {
			t.Fatal("long-press menu must not run once the allowed paste landed")
			return pasteMenuResult{}, nil
		},
	}

	out, err := callEnterTextBridgeForTest(context.Background(), tool, `{"text":"`+message+`","focus":{"x":300,"y":940}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"committed": true`) || checks != 1 {
		t.Fatalf("output=%s checks=%d", out, checks)
	}
	last := touch.calls[len(touch.calls)-1]
	if !strings.Contains(last, `"type": "tap"`) || !strings.Contains(last, `"x": 700`) {
		t.Fatalf("touch_gesture calls=%v, want the allow-paste tap last", touch.calls)
	}
}

// The legacy automatic route must not replay an uncertain paste into the focused field.
// A missing match is not evidence that a paste did not reach that field.
func TestEnterTextDoesNotReplayPossibleBridgePaste(t *testing.T) {
	const text = "上海今日天气 11月23日"
	for _, tc := range []struct {
		name      string
		analysis  textInputScreenAnalysis
		visionErr error
		pasteErr  error
	}{
		{name: "verified", analysis: textInputScreenAnalysis{TargetMatched: true}},
		{name: "empty observation"},
		{name: "unmatched field", analysis: textInputScreenAnalysis{FieldText: "上海今日天气"}},
		{name: "verification timed out", visionErr: context.DeadlineExceeded},
		{name: "ambiguous shortcut error", pasteErr: fmt.Errorf("could not verify shortcut")},
		{name: "observed commit despite shortcut error", pasteErr: fmt.Errorf("could not verify shortcut"), analysis: textInputScreenAnalysis{TargetMatched: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pb := newTestPhoneBridge(t)
			pb.NoteClipboardWrite(text)
			keyboard := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
			typing := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
			touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
			quick := &stubTool{name: "quick_action", callFn: func(context.Context, string) (string, error) { return `{"ok":true}`, tc.pasteErr }}
			vision := &nativeVerificationStub{analysis: tc.analysis, err: tc.visionErr}
			hw := &textInputHardwareDeps{
				deviceTypeFn: func() string { return "ios" }, keyboardTap: keyboard, keyboardText: typing,
				touchGesture: touch, quickAction: quick, screenshot: textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
			}
			tool := &EnterTextTool{engine: newFastTextInputEngine(*hw, &stubTextInputVision{}), allowIOSKeyboardIsolationBypass: true,
				bridgeTool: &textInputBridge{hw: hw, vision: vision, bridgeFn: func() *PhoneBridge { return pb }, sleep: testNoWaitSleep,
					findPopupFn: func(context.Context, screenshotResult, string) (popupCheck, error) { return popupCheck{}, nil },
				},
			}
			ctx, _ := WithToolError(context.Background())
			out, err := tool.Call(ctx, jsonString(map[string]any{"text": text}))
			wantOK := tc.analysis.TargetMatched
			if err != nil || enterTextOutputOK(out, nil) != wantOK || (ToolErrorFromContext(ctx) == nil) != wantOK {
				t.Fatalf("output=%s, error=%v, tool error=%v; want success=%t", out, err, ToolErrorFromContext(ctx), wantOK)
			}
			if !wantOK && !strings.Contains(out, "Text may already be present") {
				t.Fatalf("unverified paste should warn against replay: %s", out)
			}
			if len(quick.inputs) != 1 || len(keyboard.calls)+len(typing.calls)+len(touch.calls) != 0 {
				t.Fatalf("expected one paste and no replay, quick=%v keyboard=%v typing=%v touch=%v", quick.inputs, keyboard.calls, typing.calls, touch.calls)
			}
			if vision.request.InsertionBefore == nil || vision.request.Focus != nil {
				t.Fatal("verification must compare before and after at the existing focus")
			}
		})
	}
}

func TestDeviceCLITextRoutesDoNotSwitchBackends(t *testing.T) {
	for _, tc := range []struct {
		name, route, platform, code                 string
		connected, fgs, pip, writeFails, pasteFails bool
		wantWrites, wantPastes                      int
		wantDevice                                  bool
	}{
		{name: "type bypasses connected app", route: "type", platform: "android", connected: true, wantDevice: true},
		{name: "app requires transport even with cached clipboard", route: "app", platform: "android", code: CodeBridgeNotConnected},
		{name: "app pastes through FGS without WebSocket", route: "app", platform: "android", fgs: true, wantWrites: 1, wantPastes: 1},
		{name: "app pastes through PiP without WebSocket", route: "app", platform: "ios", pip: true, wantWrites: 1, wantPastes: 1},
		{name: "app only pastes", route: "app", platform: "android", connected: true, wantWrites: 1, wantPastes: 1},
		{name: "app write failure does not fall back", route: "app", platform: "android", connected: true, writeFails: true, wantWrites: 1, code: CodeToolExecutionFailed},
		{name: "app ambiguous paste failure does not replay", route: "app", platform: "ios", pip: true, pasteFails: true, wantWrites: 1, wantPastes: 1, code: CodeToolExecutionFailed},
		{name: "connected but no usable paste route", route: "app", platform: "ios", connected: true, code: CodeModuleUnavailable},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const text = "hello"
			pb := newTestPhoneBridge(t)
			pb.platform, pb.connected, pb.appState = tc.platform, tc.connected, "active"
			if tc.fgs || tc.pip {
				pb.appState = "background"
				pb.fgsBridgeSeen, pb.fgsBridgeEnabled, pb.fgsBridgeAt = tc.fgs, tc.fgs, time.Now()
				pb.pipBridgeSeen, pb.pipBridgeEnabled = tc.pip, tc.pip
			} else if !tc.connected {
				pb.NoteClipboardWrite(text)
			}
			typing := &recordingTextInputTool{name: "keyboard_text", out: "ok"}
			keyboard := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
			quick := &stubTool{name: "quick_action", callFn: func(context.Context, string) (string, error) {
				if tc.pasteFails {
					return "", fmt.Errorf("fixture paste acknowledgement lost")
				}
				return `{"ok":true}`, nil
			}}
			shot := &recordingTextInputTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`}
			vision := &nativeVerificationStub{err: fmt.Errorf("unexpected vision verification")}
			hw := &textInputHardwareDeps{deviceTypeFn: func() string { return tc.platform }, keyboardText: typing, keyboardTap: keyboard,
				quickAction: quick, screenshot: shot}
			writes := 0
			tool := &EnterTextTool{
				engine: newFastTextInputEngine(*hw, &stubTextInputVision{analyses: []textInputScreenAnalysis{{ObservedMode: textInputModeASCII}}}),
				bridgeTool: &textInputBridge{hw: hw, vision: vision,
					bridgeFn: func() *PhoneBridge { return pb }, sleep: testNoWaitSleep,
					clipboardWriteFn: func(context.Context, *PhoneBridge, string) error {
						writes++
						if tc.writeFails {
							return fmt.Errorf("fixture clipboard write failed")
						}
						return nil
					},
				},
			}
			args := []string{"type", text}
			if tc.route == "app" {
				args = append([]string{"app"}, args...)
			}
			s := &Server{runtime: &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{"enter_text": tool}}}}
			result, err := executeDeviceCLI(context.Background(), s, args, &deviceCLICapture{deferObservation: true})
			if err != nil || result.IsError != (tc.code != "") || (tc.code != "" && (result.ToolError == nil || result.ToolError.Code != tc.code)) {
				t.Fatalf("unexpected result %+v, err=%v", result, err)
			}
			if tc.route == "app" && (len(shot.calls) != 0 || vision.calls != 0) {
				t.Fatalf("explicit app typing must not capture or verify the field: screenshots=%d vision=%d", len(shot.calls), vision.calls)
			}
			if tc.pasteFails && !strings.Contains(result.Error, "Text may already be present") {
				t.Fatalf("ambiguous dispatch must warn against replay: %q", result.Error)
			}
			if writes != tc.wantWrites || len(quick.inputs) != tc.wantPastes || (len(typing.calls) > 0) != tc.wantDevice || (!tc.wantDevice && len(keyboard.calls) > 0) {
				t.Fatalf("writes=%d, pastes=%v, typing=%v, keypresses=%v", writes, quick.inputs, typing.calls, keyboard.calls)
			}
		})
	}
}
