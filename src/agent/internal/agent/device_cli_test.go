package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"image/color"
	"os/exec"
	"testing"

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
	if !got.Result.IsError() || len(got.Result.deviceFrames) != 1 || len(deviceCLIStateMessages(ctx, got.Result.deviceFrames)) != 1 {
		t.Fatalf("failed shell discarded accepted observation: %+v", got.Result)
	}
	if got := call(`curl -fsS -d '["look"]' "$AICO_DEVICE_CLI_URL/command"`); !got.Result.IsError() || len(got.Result.deviceFrames) != 0 {
		t.Fatal("unauthenticated request accepted")
	}
}

type deviceCLITestScreenshot struct{ output string }

func (deviceCLITestScreenshot) Name() string                                   { return "screenshot" }
func (deviceCLITestScreenshot) Description() string                            { return "test screenshot" }
func (s deviceCLITestScreenshot) Call(context.Context, string) (string, error) { return s.output, nil }
