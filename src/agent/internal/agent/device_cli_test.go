package agent

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"image"
	"image/color"
	"image/jpeg"
	"os/exec"
	"strings"
	"testing"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

func TestDeviceCLIShellCapturesRealRequestsNotText(t *testing.T) {
	if _, err := exec.LookPath("curl"); err != nil {
		t.Skip("curl required")
	}
	frame := testUTGFrame(t, color.White)
	result := frame.Result
	result.Data = base64.StdEncoding.EncodeToString(frame.ImageBytes)
	result.ActionOutput = "observed"
	output, _ := json.Marshal(result)
	shell := &ShellTool{}
	rt := &Runtime{tools: &ToolSet{tools: map[string]langtools.Tool{
		"screenshot": deviceCLITestScreenshot{output: string(output)},
	}}}
	ctx := context.WithValue(context.Background(), deviceCLIRuntimeKey{}, rt)
	call := func(command string) ToolCallExecutionResult {
		raw, _ := json.Marshal(map[string]string{"command": command})
		return executeToolCall(ctx, ToolCallExecution{Specs: NewToolSpecs([]langtools.Tool{shell}), Action: schema.AgentAction{Tool: "shell", ToolInput: string(raw)}})
	}
	if got := call(`printf 'device-cli look'`); len(got.Result.deviceFrames) != 0 {
		t.Fatal("stdout triggered screenshot")
	}
	got := call(`curl -fsS -H "Authorization: Bearer $AICO_DEVICE_CLI_TOKEN" -d '["look"]' "$AICO_DEVICE_CLI_URL/command" && exit 7`)
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
		"screenshot":    deviceCLITestScreenshot{output: string(output)},
		"touch_gesture": utgReplayTestTool{"touch_gesture", func(string) string { return string(output) }},
		"enter_text":    utgReplayTestTool{"enter_text", func(string) string { return string(output) }},
	}}}
	ctx := context.WithValue(context.Background(), deviceCLIRuntimeKey{}, rt)
	turn := &utgTurn{}
	ctx = context.WithValue(ctx, utgTurnKey{}, turn)
	input, _ := json.Marshal(map[string]string{"command": `cli() { curl -fsS -H "Authorization: Bearer $AICO_DEVICE_CLI_TOKEN" -d "$1" "$AICO_DEVICE_CLI_URL/command"; }
 cli '["tap","200","400"]' && cli '["swipe","500","800","500","300"]' && cli '["type","private-text"]' && cli '["back"]' && false && cli '["look"]'`})
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

	last := turn.steps[len(turn.steps)-1]
	if !bytes.Equal(last.frame.ImageBytes, raw) {
		t.Fatal("annotated frame would replace the raw UTG evidence")
	}
}

type deviceCLITestScreenshot struct{ output string }

func (deviceCLITestScreenshot) Name() string                                   { return "screenshot" }
func (deviceCLITestScreenshot) Description() string                            { return "test screenshot" }
func (s deviceCLITestScreenshot) Call(context.Context, string) (string, error) { return s.output, nil }
