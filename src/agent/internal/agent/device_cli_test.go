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
	"strings"
	"testing"

	"aico-agent/internal/agent/screen"

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
	if got := call(`printf 'device-cli look'`); len(got.Result.deviceFrames) != 0 {
		t.Fatal("stdout triggered screenshot")
	}
	look := call(`curl -fsS -H "Authorization: Bearer $AICO_DEVICE_CLI_TOKEN" -d '["look","--region","0","0","500","500"]' "$AICO_DEVICE_CLI_URL/command"`)
	if look.Result.IsError() || len(look.Result.deviceFrames) != 0 || len(look.Result.deviceActions) != 0 || !strings.Contains(look.Result.Output, base64.StdEncoding.EncodeToString(frame.Bytes())) {
		t.Fatal("look must return the image to the CLI without adding a shell screenshot")
	}
	got := call(`curl -fsS -H "Authorization: Bearer $AICO_DEVICE_CLI_TOKEN" -d '["wait","0"]' "$AICO_DEVICE_CLI_URL/command" && exit 7`)
	if !got.Result.IsError() || len(got.Result.deviceFrames) != 1 || len(deviceCLIStateMessages(got.Result.deviceFrames, got.Result.deviceActions)) != 1 {
		t.Fatalf("failed shell discarded accepted observation: %+v", got.Result)
	}
	if got := call(`curl -fsS -d '["look"]' "$AICO_DEVICE_CLI_URL/command"`); !got.Result.IsError() || len(got.Result.deviceFrames) != 0 {
		t.Fatal("unauthenticated request accepted")
	}
}

func TestDeviceCLIChainAnnotatesOnlyDispatchedOperations(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required")
	}
	var rawFrame bytes.Buffer
	if err := jpeg.Encode(&rawFrame, image.NewRGBA(image.Rect(0, 0, 400, 600)), nil); err != nil {
		t.Fatal(err)
	}
	raw := rawFrame.Bytes()
	changed := false
	output, _ := json.Marshal(postActionScreenshotResult{
		screenshotResult: screenshotResult{Width: 400, Height: 600, Data: base64.StdEncoding.EncodeToString(raw)}, ScreenChanged: &changed,
	})
	shell := &ShellTool{}
	rt := &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{
		"screenshot":    deviceCLITestTool{name: "screenshot", output: string(output)},
		"touch_gesture": deviceCLITestTool{name: "touch_gesture", output: string(output)},
		"enter_text":    deviceCLITestTool{name: "enter_text", output: string(output)},
	}}}
	ctx := context.WithValue(context.Background(), deviceCLIRuntimeKey{}, rt)
	input, _ := json.Marshal(map[string]string{"command": `cli() { curl -fsS -H "Authorization: Bearer $AICO_DEVICE_CLI_TOKEN" -d "$1" "$AICO_DEVICE_CLI_URL/command"; }
 cli '["tap","200","400"]' && cli '["swipe","500","800","500","300"]' && cli '["look"]' >/dev/null && cli '["type","private-text"]' && cli '["back"]' && false && cli '["look"]'`})
	got := executeToolCall(ctx, ToolCallExecution{Specs: NewToolSpecs([]langtools.Tool{shell}), Action: schema.AgentAction{Tool: "shell", ToolInput: string(input)}}).Result
	if !got.IsError() {
		t.Fatal("failed shell reported success")
	}
	if len(got.deviceActions) != 4 {
		t.Fatalf("recorded actions: %+v", got.deviceActions)
	}
	for i, want := range []string{"tap", "swipe", "type", "back"} {
		if action := got.deviceActions[i]; action.name != want || action.failed != (i == 3) {
			t.Errorf("unexpected operation %d: %+v", i, action)
		}
	}
	states := deviceCLIStateMessages(got.deviceFrames, got.deviceActions)
	if len(states) != 1 || len(states[0].Parts) != 2 {
		t.Fatalf("expected one screenshot attachment, got %d states", len(states))
	}
	caption := states[0].Parts[0].(llms.TextContent).Text
	_, data, ok := telemetryDataURL(states[0].Parts[1].(llms.ImageURLContent).URL)
	if !ok {
		t.Fatal("invalid screenshot attachment")
	}
	decoded, err := jpeg.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if decoded.Bounds().Size() != image.Pt(1612, 600) || !strings.Contains(caption, "1212,0; size 400×600") {
		t.Fatalf("wrong sheet geometry or last panel caption: %v %s", decoded.Bounds(), caption)
	}
	for _, check := range []struct {
		point  image.Point
		marked bool
	}{
		{image.Pt(84, 239), true},    // tap ring in panel 1
		{image.Pt(488, 239), false},  // tap not repeated in panel 2
		{image.Pt(604, 340), true},   // swipe shaft in panel 2
		{image.Pt(200, 340), false},  // swipe not drawn in panel 1
		{image.Pt(1008, 340), false}, // swipe not drawn on the text-entry panel
		{image.Pt(411, 4), false},    // swipe only has an endpoint label, not a corner badge
		{image.Pt(814, 4), true},     // text entry has a number badge inside its own panel
	} {
		r, _, _, _ := decoded.At(check.point.X, check.point.Y).RGBA()
		if (r > 0x9000) != check.marked {
			t.Errorf("incorrect marker at %v: bright=%v", check.point, r > 0x9000)
		}
	}

	for _, frame := range got.deviceFrames {
		if !bytes.Equal(frame.visual.ImageBytes, raw) {
			t.Fatal("rendering changed a captured frame")
		}
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
				if len(screen.inputs) == 1 {
					return terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{}), nil
				}
				if len(screen.inputs) == 2 {
					if !provider.dragActive {
						t.Error("expected a held contact when final capture failed")
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
				result, err := executeDeviceCLI(ctx, server, args, &deviceCLICapture{})
				if err == nil && !result.IsError {
					t.Fatal("drag unexpectedly succeeded")
				}
			}
			if provider.dragActive || provider.releaseX != 100 || provider.releaseY != 250 {
				t.Fatalf("contact cleanup did not release at origin: %+v", provider)
			}
		})
	}
}

func TestDeviceCLIVerifiedDragStartCanBeReleasedFromAnotherShell(t *testing.T) {
	provider := &dragRecoveryProvider{}
	shot := &stubTool{name: "screenshot", output: terminationPolicyScreenshotObservation(t, 200, 400, image.Rectangle{})}
	tool := newPostActionStableScreenshotTool(&TouchGestureTool{mnkProvider: provider}, &fakeInternalWaitTool{result: waitStableScreenResult{OK: true, Stable: true}}, shot, 0, ScreenStableDefaults{})
	s := &Server{runtime: &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{"touch_gesture": tool, "screenshot": shot}}}}
	res, err := executeDeviceCLI(context.Background(), s, []string{"drag-start", "100", "250"}, &deviceCLICapture{})
	if err != nil || res.IsError || !provider.dragActive {
		t.Fatalf("verified start must leave contact held: result error=%s err=%v", res.Error, err)
	}
	res, err = executeDeviceCLI(context.Background(), s, []string{"drag-release", "800", "900"}, &deviceCLICapture{})
	if err != nil || res.IsError || provider.dragActive || provider.releaseX != 800 || provider.releaseY != 900 {
		t.Fatalf("release failed: result error=%s err=%v provider=%+v", res.Error, err, provider)
	}
}

func TestDeviceCLIRotationRejectionReturnsErrorAndScreenshot(t *testing.T) {
	state := &screen.ScreenState{}
	state.UpdateScreenshot([]byte("seen"), 200, 400)
	shot := &stubTool{name: "screenshot", output: terminationPolicyScreenshotObservation(t, 400, 200, image.Rectangle{})}
	inner := &stubTool{name: "touch_gesture", output: "ok"}
	tool := newPostActionScreenshotTool(inner, shot, 0).(*postActionScreenshotTool).withTurnedScreenGuard(state)
	s := &Server{runtime: &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{"touch_gesture": tool, "screenshot": shot}}}}
	capture := &deviceCLICapture{}
	res, err := executeDeviceCLI(context.Background(), s, []string{"tap", "100", "250"}, capture)
	if err != nil || !res.IsError || !actionNotDispatched(res.ToolError) || !strings.Contains(res.Error, "not sent") || len(inner.inputs) != 0 {
		t.Fatalf("expected a skipped action error: error=%q tool error=%+v err=%v calls=%v", res.Error, res.ToolError, err, inner.inputs)
	}
	if len(capture.frames) != 1 || len(capture.actions) != 1 || !capture.actions[0].failed || capture.actions[0].pointCount != 0 {
		t.Fatalf("expected fresh screenshot with a failed step but no touch marker: frames=%d actions=%+v", len(capture.frames), capture.actions)
	}
}
