package agent

import (
	"aico-agent/internal/ble"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

func callEnterTextBridgeForTest(ctx context.Context, tool *textInputBridge, input string) (string, error) {
	var args textInputArgs
	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return "", err
	}
	result, _ := tool.runClipboardFirstResult(ctx, args)
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
	quick := &recordingTextInputTool{name: "quick_action", out: `{"ok":false,"status":"reserved","message":"paste unavailable"}`}
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
	if len(quick.calls) != 1 || !strings.Contains(quick.calls[0], `"action": "paste"`) {
		t.Fatalf("quick_action calls=%v", quick.calls)
	}
	if len(keyboardTap.calls) != 1 || !strings.Contains(keyboardTap.calls[0], `"meta"`) || !strings.Contains(keyboardTap.calls[0], `"v"`) {
		t.Fatalf("keyboard_tap calls=%v", keyboardTap.calls)
	}
}

func TestTextInputBridgeFallsBackToLongPressPasteMenuWhenShortcutHasNoEffect(t *testing.T) {
	message := "桥接粘贴测试"
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
	keyboardTap := &recordingTextInputTool{name: "keyboard_tap", out: "ok"}
	quick := &recordingTextInputTool{name: "quick_action", out: `{"ok":true}`}
	touch := &recordingTextInputTool{name: "touch_gesture", out: "ok"}
	tool := &textInputBridge{
		hw: &textInputHardwareDeps{
			touchGesture: touch,
			keyboardTap:  keyboardTap,
			keyboardText: &recordingTextInputTool{name: "keyboard_text", out: "ok"},
			quickAction:  quick,
			screenshot:   textInputStubTool{name: "screenshot", out: `{"format":"jpeg","width":100,"height":100,"data":"abc"}`},
		},
		vision:   vision,
		bridgeFn: func() *PhoneBridge { return pb },
		sleep:    testNoWaitSleep,
		findPasteMenuFn: func(context.Context, screenshotResult, string) (pasteMenuResult, error) {
			return pasteMenuResult{
				Found:    true,
				TapPoint: &focusPointArgs{X: 430, Y: 720},
				Label:    "粘贴",
			}, nil
		},
	}

	out, err := callEnterTextBridgeForTest(context.Background(), tool, `{"text":"`+message+`","focus":{"x":300,"y":940}}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, `"committed": true`) {
		t.Fatalf("unexpected output: %s", out)
	}
	if len(quick.calls) != 1 {
		t.Fatalf("quick_action calls=%v, want one shortcut attempt", quick.calls)
	}
	if len(keyboardTap.calls) != 0 {
		t.Fatalf("keyboard_tap calls=%v, want no keyboard fallback after quick_action returned ok", keyboardTap.calls)
	}
	if len(touch.calls) != 4 || !strings.Contains(touch.calls[0], `"type": "tap"`) || !strings.Contains(touch.calls[1], `"type": "tap"`) || !strings.Contains(touch.calls[2], `"type": "long_press"`) || !strings.Contains(touch.calls[3], `"type": "tap"`) {
		t.Fatalf("touch_gesture calls=%v, want two focus taps, long_press, then paste-menu tap", touch.calls)
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

type recordingBridgeQuickActionTool struct {
	*recordingTextInputTool
	onCall func(string)
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

func (s *recordingBridgeQuickActionTool) Call(ctx context.Context, input string) (string, error) {
	if s.onCall != nil {
		s.onCall(input)
	}
	return s.recordingTextInputTool.Call(ctx, input)
}
