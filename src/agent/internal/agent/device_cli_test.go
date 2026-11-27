package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"image"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"reflect"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/mnk"
	"aico-agent/internal/agent/screen"
	"aico-agent/internal/ble"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

func TestDeviceCLIShellCapturesRealRequestsNotText(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required")
	}
	var frame bytes.Buffer
	if err := jpeg.Encode(&frame, image.NewRGBA(image.Rect(0, 0, 8, 12)), nil); err != nil {
		t.Fatal(err)
	}
	output, _ := json.Marshal(screenshotResult{Width: 8, Height: 12, Data: base64.StdEncoding.EncodeToString(frame.Bytes())})
	shell := &ShellTool{}
	rt := &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{
		"screenshot": deviceCLITestTool{name: "screenshot", output: string(output)},
	}}}
	ctx := context.WithValue(context.Background(), deviceCLIRuntimeKey{}, rt)
	call := func(command string) ToolCallExecutionResult {
		raw, _ := json.Marshal(map[string]string{"command": command})
		return executeToolCall(ctx, ToolCallExecution{Specs: NewToolSpecs([]langtools.Tool{shell}), Action: schema.AgentAction{Tool: "shell", ToolInput: string(raw)}})
	}
	if got := call(`printf 'device-cli look'`); len(got.Result.deviceFrame.ImageBytes) != 0 {
		t.Fatal("stdout triggered screenshot")
	}
	look := call(`curl -fsS -H "Authorization: Bearer $AICO_DEVICE_CLI_TOKEN" -d '["look","--region","0","0","500","500"]' "$AICO_DEVICE_CLI_URL/command"`)
	if look.Result.IsError() || len(look.Result.deviceFrame.ImageBytes) != 0 || !strings.Contains(look.Result.Output, base64.StdEncoding.EncodeToString(frame.Bytes())) {
		t.Fatal("look must return the image to the CLI without adding a shell screenshot")
	}
	got := call(`curl -fsS -H "Authorization: Bearer $AICO_DEVICE_CLI_TOKEN" -d '["wait","0"]' "$AICO_DEVICE_CLI_URL/command" && exit 7`)
	if !got.Result.IsError() || len(got.Result.deviceFrame.ImageBytes) == 0 || len(deviceCLIStateMessages(got.Result.deviceFrame)) != 1 {
		t.Fatalf("failed shell discarded accepted observation: %+v", got.Result)
	}
	if got := call(`curl -fsS -d '["look"]' "$AICO_DEVICE_CLI_URL/command"`); !got.Result.IsError() || len(got.Result.deviceFrame.ImageBytes) != 0 {
		t.Fatal("unauthenticated request accepted")
	}
}

func TestDeviceCLIChainCapturesOnlyFinalState(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required")
	}
	shot := &stubTool{name: "screenshot"}
	gesture := &stubTool{name: "touch_gesture", output: "ok"}
	text := &stubTool{name: "enter_text", output: "ok"}
	open := &stubTool{name: toolOpenApp, output: "ok"}
	shot.callFn = func(context.Context, string) (string, error) {
		if len(gesture.inputs) != 2 || len(text.inputs) != 1 || len(open.inputs) != 1 {
			t.Error("screenshot taken before the chain ended")
		}
		return terminationPolicyScreenshotObservation(t, 400, 600, image.Rectangle{}), nil
	}
	rt := &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{
		"screenshot":    shot,
		"touch_gesture": newPostActionScreenshotTool(gesture, shot, 0),
		"enter_text":    newPostActionScreenshotTool(text, shot, 0),
		toolOpenApp:     newPostActionScreenshotTool(open, shot, 0),
	}}}
	ctx := context.WithValue(context.Background(), deviceCLIRuntimeKey{}, rt)
	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", nil)
	manager.StartTask("request", "Use phone")
	var nested []RunEvent
	ctx = context.WithValue(ctx, deviceCLIActivityKey{}, func(event RunEvent) {
		nested = append(nested, event)
		manager.UpdateFromRunEvent("request", event)
	})
	var outer []RunEvent
	callback := &runtimeCallbackHandler{eventHandler: func(event RunEvent) {
		outer = append(outer, event)
		manager.UpdateFromRunEvent("request", event)
	}}
	input, _ := json.Marshal(map[string]string{"command": `cli() { curl -fsS -H "Authorization: Bearer $AICO_DEVICE_CLI_TOKEN" -d "$1" "$AICO_DEVICE_CLI_URL/command"; }
 cli '["open","Settings"]' && cli '["tap","200","400"]' && cli '["swipe","500","800","500","300"]' && cli '["type","hello"]' && false && cli '["tap","0","0"]'`})
	got := executeToolCall(ctx, ToolCallExecution{Specs: NewToolSpecs([]langtools.Tool{&ShellTool{}}), Action: schema.AgentAction{Tool: "shell", ToolInput: string(input)}, Callback: callback}).Result
	if !got.IsError() || len(got.deviceCalls) != 4 || got.deviceCalls[3].Input["text"] != "hello" {
		t.Fatalf("expected only the operations before the error: %+v", got.deviceCalls)
	}
	var started []string
	for _, event := range nested {
		if event.Type == runEventToolCall {
			started = append(started, event.ToolName)
		}
		if strings.Contains(event.ToolInput+event.Content, "hello") {
			t.Fatal("typed text leaked into Live Activity")
		}
	}
	if !reflect.DeepEqual(started, []string{toolOpenApp, "touch_gesture", "touch_gesture", "enter_text"}) {
		t.Fatalf("unexpected Live Activity actions: %v", started)
	}
	if len(outer) != 2 || outer[0].ToolName != "shell" || outer[1].ToolName != "shell" {
		t.Fatalf("nested events reached the run stream: %+v", outer)
	}
	if state := manager.Snapshot("request"); state.ToolStatus != "failed" || state.LastError != "" {
		t.Fatalf("expected a display-safe shell failure after successful actions: %+v", state)
	}
	states := deviceCLIStateMessages(got.deviceFrame)
	if len(shot.inputs) != 1 || len(states) != 1 || len(states[0].Parts) != 2 {
		t.Fatalf("expected one final capture and attachment: captures=%d, states=%d", len(shot.inputs), len(states))
	}
	_, data, ok := telemetryDataURL(states[0].Parts[1].(llms.ImageURLContent).URL)
	if !ok || !bytes.Equal(data, got.deviceFrame.ImageBytes) || got.deviceFrame.Result.Width != 400 || got.deviceFrame.Result.Height != 600 {
		t.Fatal("expected one unmodified screen, not an annotated sheet")
	}
}

type deviceCLITestTool struct{ name, output string }

func (s deviceCLITestTool) Name() string                                 { return s.name }
func (s deviceCLITestTool) Description() string                          { return "test device tool" }
func (s deviceCLITestTool) Call(context.Context, string) (string, error) { return s.output, nil }

func TestDeviceCLIDragReleasesOnCaptureFailure(t *testing.T) {
	for _, tc := range []struct {
		name       string
		cancel     bool
		useHTTP    bool
		standalone bool
		directTool bool
	}{
		{name: "composite shell"}, {name: "canceled composite shell", cancel: true},
		{name: "composite HTTP", useHTTP: true},
		{name: "split start shell", standalone: true},
		{name: "canceled split start shell", standalone: true, cancel: true},
		{name: "canceled split start HTTP", standalone: true, useHTTP: true, cancel: true},
		{name: "direct tool start", standalone: true, directTool: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			provider := &dragRecoveryProvider{}
			action := &TouchGestureTool{mnkProvider: provider}
			screen := &stubTool{name: "screenshot"}
			screen.callFn = func(context.Context, string) (string, error) {
				if (tc.useHTTP || tc.directTool) && len(screen.inputs) == 1 {
					return terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{}), nil
				}
				if len(screen.inputs) == 2 || (!tc.useHTTP && !tc.directTool) {
					if wantHeld := tc.useHTTP || tc.directTool || tc.standalone; provider.dragActive != wantHeld {
						t.Errorf("held contact at capture = %t, want %t", provider.dragActive, wantHeld)
					}
					if tc.cancel {
						cancel()
					}
				}
				return "", errors.New("capture unavailable")
			}
			tool := newPostActionStableScreenshotTool(action, &fakeInternalWaitTool{result: waitStableScreenResult{OK: true, Stable: true}}, screen, 0, ScreenStableDefaults{})
			rt := &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{"touch_gesture": tool, "screenshot": screen}}}
			server := &Server{runtime: rt}
			args := []string{"drag", "100", "250", "800", "900"}
			if tc.standalone {
				args = []string{"drag-start", "100", "250"}
			}
			if tc.directTool {
				res := executeToolCall(ctx, ToolCallExecution{
					Specs:  NewToolSpecs([]langtools.Tool{tool}),
					Action: schema.AgentAction{Tool: "touch_gesture", ToolInput: `{"type":"drag_start","point":{"x":100,"y":250}}`},
				})
				if !res.Result.IsError() {
					t.Fatal("drag unexpectedly succeeded")
				}
			} else if tc.useHTTP {
				w := httptest.NewRecorder()
				body, _ := json.Marshal(args)
				server.handleDeviceCLIDrag(w, httptest.NewRequest(http.MethodPost, "/api/device-cli/drag", bytes.NewReader(body)).WithContext(ctx))
				var result ToolInvokeResponse
				if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || !result.IsError {
					t.Fatalf("expected failed drag, got %s", w.Body.String())
				}
			} else {
				capture := &deviceCLICapture{deferObservation: true}
				result, err := executeDeviceCLI(ctx, server, args, capture)
				if err != nil || result.IsError {
					t.Fatalf("shell drag failed before finalizing: %+v, %v", result, err)
				}
				if err := capture.finishShell(ctx, rt, true); err == nil {
					t.Fatal("final capture unexpectedly succeeded")
				}
			}
			wantX, wantY := 100.0, 250.0
			if !tc.standalone && !tc.useHTTP {
				wantX, wantY = 800, 900
			}
			if provider.dragActive || provider.releaseX != wantX || provider.releaseY != wantY {
				t.Fatalf("contact was not released at the expected point: %+v", provider)
			}
		})
	}
}

func TestStandaloneDeviceCLIDragStopsOnUnstableStart(t *testing.T) {
	provider := &dragRecoveryProvider{}
	shot := &stubTool{name: "screenshot", output: terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{})}
	gesture := newPostActionStableScreenshotTool(&TouchGestureTool{mnkProvider: provider},
		&fakeInternalWaitTool{result: waitStableScreenResult{OK: true, Stable: false}}, shot, 0, ScreenStableDefaults{})
	s := &Server{runtime: &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{"touch_gesture": gesture}}}}
	capture := &deviceCLICapture{}
	result, err := executeDeviceCLI(context.Background(), s, []string{"drag", "100", "250", "800", "900"}, capture)
	if err != nil || !result.IsError || len(capture.calls) != 1 || provider.dragActive || provider.releaseX != 100 || provider.releaseY != 250 {
		t.Fatalf("unstable drag must release only at the origin: result=%+v err=%v calls=%+v provider=%+v", result.ToolError, err, capture.calls, provider)
	}
}

func TestDeviceCLIRespectsHeadlessBenchmarkTools(t *testing.T) {
	gesture := &stubTool{name: "touch_gesture", output: "ok"}
	s := &Server{runtime: &Runtime{
		config: Config{Benchmark: BenchmarkConfig{Headless: true, Token: "fixture"}},
		tools:  &ToolSet{tools: map[string]langtools.Tool{"touch_gesture": gesture}},
	}}
	result, err := executeDeviceCLI(context.Background(), s, []string{"tap", "100", "250"}, &deviceCLICapture{deferObservation: true})
	if (err == nil && !result.IsError) || len(gesture.inputs) != 0 {
		t.Fatalf("headless benchmark must not dispatch gesture: error=%q err=%v calls=%v", result.Error, err, gesture.inputs)
	}
}

func TestDeviceCLIDragStartCanBeReleasedFromAnotherShell(t *testing.T) {
	provider := &dragRecoveryProvider{}
	shot := &stubTool{name: "screenshot", output: terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{})}
	tool := newPostActionStableScreenshotTool(&TouchGestureTool{mnkProvider: provider}, &fakeInternalWaitTool{result: waitStableScreenResult{OK: true, Stable: true}}, shot, 0, ScreenStableDefaults{})
	s := &Server{runtime: &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{"touch_gesture": tool, "screenshot": shot}}}}
	ctx := context.Background()
	start := deviceCLICapture{deferObservation: true}
	res, err := executeDeviceCLI(ctx, s, []string{"drag-start", "100", "250"}, &start)
	if err != nil || res.IsError || !provider.dragActive || len(shot.inputs) != 0 {
		t.Fatalf("start should leave contact held without a post-action capture: result error=%s err=%v", res.Error, err)
	}
	if err := start.finishShell(ctx, s.runtime, true); err != nil || !provider.dragActive || len(shot.inputs) != 1 {
		t.Fatalf("successful shell must leave its contact held: %v", err)
	}
	res, err = executeDeviceCLI(ctx, s, []string{"drag-release", "800", "900"}, &deviceCLICapture{deferObservation: true})
	if err != nil || res.IsError || provider.dragActive || provider.releaseX != 800 || provider.releaseY != 900 {
		t.Fatalf("release fail: result error=%s err=%v provider=%+v", res.Error, err, provider)
	}
	failed := deviceCLICapture{deferObservation: true}
	res, err = executeDeviceCLI(ctx, s, []string{"drag-start", "100", "250"}, &failed)
	if err != nil || res.IsError || !provider.dragActive {
		t.Fatalf("start failed: %s %v", res.Error, err)
	}
	shot.callFn = func(context.Context, string) (string, error) { return "", errors.New("capture unavailable") }
	if err := failed.finishShell(ctx, s.runtime, true); err == nil || provider.dragActive || provider.releaseX != 100 || provider.releaseY != 250 {
		t.Fatalf("failed final capture must release the contact at its origin: %v %+v", err, provider)
	}
}

func TestDeviceCLITapWaitsBeforeReturning(t *testing.T) {
	gesture := &stubTool{name: "touch_gesture", output: `{"ok":true}`}
	s := &Server{runtime: &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{"touch_gesture": gesture}}}}
	capture := &deviceCLICapture{deferObservation: true}
	var dispatched time.Time
	gesture.callFn = func(context.Context, string) (string, error) {
		dispatched = time.Now()
		return `{"ok":true}`, nil
	}
	result, err := executeDeviceCLI(context.Background(), s, []string{"tap", "350", "270"}, capture)
	if err != nil || result.IsError || dispatched.IsZero() || time.Since(dispatched) < 50*time.Millisecond {
		t.Fatalf("tap must settle after dispatch before the next command: %+v, %v", result, err)
	}
	if len(capture.calls) != 1 {
		t.Fatalf("tap delay must not add a device action: %+v", capture.calls)
	}
}

func TestDeviceCLIShellFinalCaptureAfterWait(t *testing.T) {
	for _, tc := range []struct {
		name, ms string
		delay    time.Duration
	}{
		{"no delay", "0", 0},
		{"fixed delay", "20", 20 * time.Millisecond},
	} {
		t.Run(tc.name, func(t *testing.T) {
			shot := &stubTool{name: "screenshot", output: terminationPolicyScreenshotObservation(t, 400, 200, image.Rectangle{})}
			s := &Server{runtime: &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{"screenshot": shot}}}}
			capture := &deviceCLICapture{deferObservation: true}
			ctx := context.Background()
			started := time.Now()
			if got, err := executeDeviceCLI(ctx, s, []string{"wait", tc.ms}, capture); err != nil || got.IsError {
				t.Fatalf("wait failed: %+v, %v", got, err)
			}
			if elapsed := time.Since(started); elapsed < tc.delay {
				t.Fatalf("wait returned after %v, want at least %v", elapsed, tc.delay)
			}
			if len(shot.inputs) != 0 {
				t.Fatal("wait should not capture a frame or dispatch a device tool")
			}
			if len(capture.calls) != 1 || capture.calls[0].Tool != "device_cli_wait" || capture.calls[0].Input["milliseconds"] != tc.delay.Milliseconds() || capture.calls[0].IsError {
				t.Fatalf("expected completed wait evidence, got %+v", capture.calls)
			}
			if err := capture.finishShell(ctx, s.runtime, true); err != nil || capture.frame.Result.Width != 400 || len(shot.inputs) != 1 {
				t.Fatalf("expected one final screenshot, got width %d and %d captures: %v", capture.frame.Result.Width, len(shot.inputs), err)
			}
		})
	}
}

func TestDeviceCLIWaitHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	capture := &deviceCLICapture{deferObservation: true}
	if _, err := executeDeviceCLI(ctx, nil, []string{"wait", "30000"}, capture); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("wait error = %v, want context deadline exceeded", err)
	}
	if capture.needsScreenshot {
		t.Fatal("canceled wait should not request a screenshot on its own")
	}
}

func TestDeviceCLICanceledFinalCaptureDoesNotHandOffContact(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	shot := &stubTool{name: "screenshot", callFn: func(context.Context, string) (string, error) {
		cancel()
		return terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{}), nil
	}}
	rt := &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{"screenshot": shot}}}
	released := false
	capture := &deviceCLICapture{deferObservation: true, needsScreenshot: true, drag: compositeDrag{release: func(ctx context.Context) error {
		released = true
		return ctx.Err()
	}}}
	if err := capture.finishShell(ctx, rt, true); !errors.Is(err, context.Canceled) || !released {
		t.Fatalf("canceled final capture must clean up contact: err=%v released=%t", err, released)
	}
}

func TestDeviceCLIRotationRejectionReturnsErrorAndScreenshot(t *testing.T) {
	state := &screen.ScreenState{}
	state.UpdateScreenshot([]byte("seen"), 200, 400)
	shot := &stubTool{name: "screenshot", output: terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{})}
	inner := &stubTool{name: "touch_gesture", output: "ok"}
	tool := newPostActionScreenshotTool(inner, shot, 0).(*postActionScreenshotTool).withTurnedScreenGuard(state)
	s := &Server{runtime: &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{"touch_gesture": tool, "screenshot": shot}}}}
	capture := &deviceCLICapture{deferObservation: true}
	if got, err := executeDeviceCLI(context.Background(), s, []string{"tap", "100", "250"}, capture); err != nil || got.IsError {
		t.Fatalf("first tap should dispatch: error=%q err=%v", got.Error, err)
	}
	shot.output = terminationPolicyScreenshotObservation(t, 400, 200, image.Rectangle{})
	res, err := executeDeviceCLI(context.Background(), s, []string{"tap", "100", "250"}, capture)
	if err != nil || !res.IsError || !actionNotDispatched(res.ToolError) || !strings.Contains(res.Error, "not sent") || len(inner.inputs) != 1 {
		t.Fatalf("expected a skipped action error: error=%q tool error=%+v err=%v calls=%v", res.Error, res.ToolError, err, inner.inputs)
	}
	if len(capture.calls) != 2 || !capture.calls[1].NotDispatched || !capture.calls[1].IsError {
		t.Fatalf("skipped gesture was not recorded correctly: %+v", capture.calls)
	}
	if err := capture.finishShell(context.Background(), s.runtime, false); err != nil || len(capture.frame.ImageBytes) == 0 {
		t.Fatalf("expected final screenshot after rejection: %v", err)
	}

}

func TestDeviceCLIShellRecordsBenchmarkFixtureResultInEpisode(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required")
	}
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if r.URL.Path != "/api/tools/touch_gesture" || r.Header.Get("benchmark-task-id") != "fixture-task" ||
			body["input"] != `{"point":{"x":180,"y":310},"type":"tap"}` {
			t.Errorf("unexpected fixture request %s: %+v", r.URL, body)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"output": `{"ok":true}`})
	}))
	defer fixture.Close()
	shot := terminationPolicyScreenshotObservation(t, 100, 150, image.Rectangle{})
	rt := &Runtime{
		config: Config{Benchmark: BenchmarkConfig{MockTools: true}, EnvironmentBridge: EnvironmentBridgeConfig{
			Enabled: true, Endpoint: fixture.URL, BenchmarkTaskID: "fixture-task",
		}},
		tools: &ToolSet{tools: map[string]langtools.Tool{
			"touch_gesture": mockPolicyTestTool{"touch_gesture"}, // panics if real execution leaks through
			"screenshot":    deviceCLITestTool{name: "screenshot", output: shot},
		}},
	}
	ctx := context.WithValue(context.Background(), deviceCLIRuntimeKey{}, rt)
	input, _ := json.Marshal(map[string]string{"command": `curl -fsS -H "Authorization: Bearer $AICO_DEVICE_CLI_TOKEN" -d '["tap","180","310"]' "$AICO_DEVICE_CLI_URL/command"`})
	got := executeToolCall(ctx, ToolCallExecution{Specs: NewToolSpecs([]langtools.Tool{&ShellTool{}}), Action: schema.AgentAction{Tool: "shell", ToolInput: string(input)}})
	if got.Result.IsError() || len(got.Result.deviceCalls) != 1 || got.Result.deviceCalls[0].IsError || len(got.Result.deviceFrame.ImageBytes) == 0 {
		t.Fatalf("expected one successful fixture tap and a fallback screenshot: %+v", got.Result)
	}
	recorder := NewEpisodeRecorder(MemoryRetrieveRequest{Input: "tap"}, MemoryContext{})
	recorder.RecordExecution(got)
	for _, event := range recorder.Finish("", nil, nil, nil, nil).Events {
		if event.Type != "tool_result" || event.ToolName != "shell" {
			continue
		}
		if event.Metadata["has_screenshot"] != true {
			t.Fatalf("episode did not record screenshot evidence: %+v", event.Metadata)
		}
		raw, err := json.Marshal(event.Metadata["device_cli_calls"])
		if err != nil || !strings.Contains(string(raw), `"tool":"touch_gesture","input":{"point":{"x":180,"y":310},"type":"tap"},"is_error":false`) {
			t.Fatalf("unexpected exported metadata: %s, %v", raw, err)
		}
		return
	}
	t.Fatal("episode did not record shell result")
}

func TestDeviceCLIKeyChordAndMaskedFailure(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required")
	}
	keyboard := &cliTestKeyboard{}
	keys := &KeyboardTapTool{mnkProvider: mnk.NewHIDProvider(nil, keyboard, nil, nil, false, "qwerty", nil)}
	shot := &stubTool{name: "screenshot", output: terminationPolicyScreenshotObservation(t, 60, 120, image.Rectangle{})}
	rt := &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{"keyboard_tap": keys, "screenshot": shot}}}
	ctx := context.WithValue(context.Background(), deviceCLIRuntimeKey{}, rt)
	call := func(command string) ToolResult {
		t.Helper()
		command = `cli() { curl -fsS -H "Authorization: Bearer $AICO_DEVICE_CLI_TOKEN" -d "$1" "$AICO_DEVICE_CLI_URL/command"; }; ` + command
		input, _ := json.Marshal(map[string]string{"command": command})
		return executeToolCall(ctx, ToolCallExecution{Specs: NewToolSpecs([]langtools.Tool{&ShellTool{}}), Action: schema.AgentAction{Tool: "shell", ToolInput: string(input)}}).Result
	}
	if got := call(`cli '["key","meta+a"]' && cli '["key","ctrl+a"]'`); got.IsError() {
		t.Fatalf("valid key chords failed: %s", got.Output)
	}
	want := [][]byte{{8, 0, 4, 0, 0, 0, 0, 0}, make([]byte, 8), {1, 0, 4, 0, 0, 0, 0, 0}, make([]byte, 8)}
	if !reflect.DeepEqual(keyboard.reports, want) {
		t.Fatalf("HID reports=%v; want two simultaneous press-and-release chords", keyboard.reports)
	}
	got := call(`cli '["key","ctrl+not-a-key"]' 2>&1 | cat >/dev/null; printf done`)
	if !got.IsError() || got.Error.Code != CodeInvalidArguments || !strings.Contains(got.Output, `unknown key: "not-a-key"`) || !strings.Contains(got.Output, "done") {
		t.Fatalf("masked keyboard error was lost: %+v", got)
	}
	if len(keyboard.reports) != len(want) || len(got.deviceCalls) != 1 || !got.deviceCalls[0].IsError {
		t.Fatalf("invalid chord was dispatched or not recorded: reports=%v calls=%+v", keyboard.reports, got.deviceCalls)
	}
}

type cliTestKeyboard struct{ reports [][]byte }

func (d *cliTestKeyboard) Write(data []byte) error {
	d.reports = append(d.reports, bytes.Clone(data))
	return nil
}
func (*cliTestKeyboard) Close() {}

func TestDeviceCLILiveActivityPreservesBridgeRecovery(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required")
	}
	rt := &Runtime{tools: &ToolSet{}}
	manager := NewLiveActivityManager(LiveActivityConfig{}, "en-US", nil)
	manager.StartTask("request", "Check contacts")
	ctx := context.WithValue(context.Background(), deviceCLIRuntimeKey{}, rt)
	ctx = context.WithValue(ctx, deviceCLIActivityKey{}, func(event RunEvent) {
		state := manager.UpdateFromRunEvent("request", event)
		if state.CurrentTarget != "" || strings.Contains(state.CurrentStep, "Private Name") {
			t.Errorf("private contact query reached Live Activity: %+v", state)
		}
	})
	callback := &runtimeCallbackHandler{eventHandler: func(event RunEvent) {
		manager.UpdateFromRunEvent("request", event)
	}}
	input := jsonString(map[string]any{"command": `curl -fsS -H "Authorization: Bearer $AICO_DEVICE_CLI_TOKEN" -d '["app","contacts","query","--query","Private Name"]' "$AICO_DEVICE_CLI_URL/command" && exit 1`})
	got := executeToolCall(ctx, ToolCallExecution{
		Specs: NewToolSpecs([]langtools.Tool{&ShellTool{}}), Callback: callback,
		Action: schema.AgentAction{Tool: "shell", ToolInput: input},
	}).Result
	if !got.IsError() || len(got.deviceCalls) != 1 || !got.deviceCalls[0].NotDispatched {
		t.Fatalf("expected shell failure with evidence of a rejected Bridge operation: %+v", got)
	}
	state := manager.Snapshot("request")
	if state.Status != LiveActivityStatusNeedsApp || state.ToolStatus != "waiting_app" || !state.RequiresApp || state.LastError != "" {
		t.Fatalf("shell completion must preserve the Bridge recovery prompt: %+v", state)
	}
	if state := manager.CompleteTask("request", "Finished"); state.Status != LiveActivityStatusCompleted {
		t.Fatalf("expected terminal task status: %+v", state)
	}
}

func TestDeviceCLIAppDataRequiresTransportWithoutCapturingScreen(t *testing.T) {
	for _, tc := range []struct {
		name, platform                                       string
		connected, fgs, pip, staleFGS, bleWake, wantDispatch bool
	}{
		{name: "no route"},
		{name: "connected", connected: true, wantDispatch: true},
		{name: "FGS without WebSocket", platform: "android", fgs: true, wantDispatch: true},
		{name: "PiP without WebSocket", platform: "ios", pip: true, wantDispatch: true},
		{name: "BLE Wake without WebSocket", platform: "ios", bleWake: true, wantDispatch: true},
		{name: "expired FGS", platform: "android", fgs: true, staleFGS: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bridge := newTestPhoneBridge(t)
			bridge.platform, bridge.appState, bridge.connected = tc.platform, "background", tc.connected
			bridge.fgsBridgeSeen, bridge.fgsBridgeEnabled, bridge.fgsBridgeAt = tc.fgs, tc.fgs, time.Now()
			bridge.pipBridgeSeen, bridge.pipBridgeEnabled = tc.pip, tc.pip
			if tc.bleWake {
				bridge.bleStatus = func(context.Context) (ble.RuntimeStatus, error) {
					return ble.RuntimeStatus{BackendAvailable: true, Connected: true, WakeSubscriber: true}, nil
				}
			}
			if tc.staleFGS {
				bridge.fgsBridgeAt = time.Now().Add(-time.Minute)
			}
			contacts := &stubTool{name: toolBridgeContacts, output: `{"ok":true,"contacts":[{"name":"Alice"}]}`}
			shot := &stubTool{name: "screenshot"}
			s := &Server{runtime: &Runtime{tools: &ToolSet{phoneBridge: bridge, tools: map[string]langtools.Tool{
				toolBridgeContacts: contacts, "screenshot": shot,
			}}}}
			args := []string{"app", "contacts", "query", "--query", "Alice", "--limit", "5"}
			assertResult := func(result ToolInvokeResponse) {
				t.Helper()
				if tc.wantDispatch {
					if result.IsError || result.Output != contacts.output {
						t.Fatalf("unexpected result: %+v", result)
					}
				} else if !result.IsError || !actionNotDispatched(result.ToolError) || result.ToolError.Code != CodeBridgeNotConnected {
					t.Fatalf("expected bridge connection error before dispatch: %+v", result)
				}
			}
			capture := &deviceCLICapture{deferObservation: true}
			result, err := executeDeviceCLI(context.Background(), s, args, capture)
			if err != nil {
				t.Fatal(err)
			}
			assertResult(result)
			if len(capture.calls) != 1 || capture.calls[0].IsError == tc.wantDispatch || capture.calls[0].NotDispatched == tc.wantDispatch || capture.needsScreenshot {
				t.Fatalf("unexpected shell evidence or capture request: %+v", capture)
			}
			body, _ := json.Marshal(args)
			w := httptest.NewRecorder()
			s.handleDeviceCLIApp(w, httptest.NewRequest(http.MethodPost, "/api/device-cli/app", bytes.NewReader(body)))
			if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &result) != nil {
				t.Fatalf("HTTP response: %d %s", w.Code, w.Body)
			}
			assertResult(result)
			if len(shot.inputs) != 0 || (!tc.wantDispatch && len(contacts.inputs) != 0) || (tc.wantDispatch && len(contacts.inputs) != 2) {
				t.Fatalf("unexpected dispatch: captures=%d, contacts=%v", len(shot.inputs), contacts.inputs)
			}
		})
	}
}
