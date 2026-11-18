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
	if !got.Result.IsError() || len(got.Result.deviceFrames) != 1 || len(deviceCLIStateMessages(ctx, got.Result.deviceFrames, got.Result.deviceActions)) != 1 {
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
	changed := true
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
	for i, want := range []string{"01 tap 200,400", "02 swipe 500,800 500,300", "03 type", "04 back [failed]"} {
		if label := got.deviceActions[i].label(i + 1); label != want {
			t.Errorf("operation %d: %s", i, label)
		}
	}
	states := deviceCLIStateMessages(ctx, got.deviceFrames, got.deviceActions)
	if len(states) < 2 {
		t.Fatalf("missing intermediate/final states: %d", len(states))
	}
	for i, state := range states {
		caption := state.Parts[0].(llms.TextContent).Text
		_, data, ok := telemetryDataURL(state.Parts[1].(llms.ImageURLContent).URL)
		if !ok {
			t.Fatal("invalid screenshot attachment")
		}
		if i < len(states)-1 {
			if !bytes.Equal(raw, data) {
				t.Fatal("intermediate frame was changed")
			}
			continue
		}
		decoded, err := jpeg.Decode(bytes.NewReader(data))
		if err != nil {
			t.Fatal(err)
		}
		if decoded.Bounds().Dx() != 400 || decoded.Bounds().Dy() <= 600 || !strings.Contains(caption, "400×600") {
			t.Fatalf("wrong annotation geometry/caption: %v %s", decoded.Bounds(), caption)
		}
		for _, check := range []image.Point{{X: 84, Y: 239}, {X: 199, Y: 340}} {
			// Inspect a small neighborhood: the nominal points lie on the tap ring and swipe shaft.
			visible := false
			for y := check.Y - 3; y <= check.Y+3; y++ {
				for x := check.X - 3; x <= check.X+3; x++ {
					r, _, _, _ := decoded.At(x, y).RGBA()
					visible = visible || r > 0x9000
				}
			}
			if !visible {
				t.Errorf("missing gesture mark near %v", check)
			}
		}
	}
	last := turn.steps[len(turn.steps)-1]
	if !bytes.Equal(last.frame.ImageBytes, raw) || last.shown {
		t.Fatal("annotated frame would replace the raw UTG evidence")
	}
}

type deviceCLITestScreenshot struct{ output string }

func (deviceCLITestScreenshot) Name() string                                   { return "screenshot" }
func (deviceCLITestScreenshot) Description() string                            { return "test screenshot" }
func (s deviceCLITestScreenshot) Call(context.Context, string) (string, error) { return s.output, nil }
