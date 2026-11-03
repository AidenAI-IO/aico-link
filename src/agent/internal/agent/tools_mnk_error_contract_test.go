package agent

import (
	"context"
	"testing"

	"aico-agent/internal/agent/mnk"

	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

func TestMNKAdapterToolCallsReturnStructuredToolErrors(t *testing.T) {
	tests := []struct {
		name     string
		tool     langtools.Tool
		input    string
		wantCode string
	}{
		{
			name:     "keyboard tap empty keys",
			tool:     &KeyboardTapTool{},
			input:    `{"keys":[]}`,
			wantCode: CodeInvalidArguments,
		},
		{
			name:     "touch gesture missing type",
			tool:     &TouchGestureTool{},
			input:    `{}`,
			wantCode: CodeInvalidArguments,
		},
		{
			name:     "mouse move unconfigured",
			tool:     &MouseMoveTool{},
			input:    `{"x":10,"y":20}`,
			wantCode: CodeModuleUnavailable,
		},
		{
			name:     "mouse scroll out of range",
			tool:     &MouseScrollTool{mnkProvider: mnk.NewMockProvider()},
			input:    `{"delta": 200}`,
			wantCode: CodeInvalidArguments,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			// Direct Call path: must return (message, nil) with SetToolError.
			ctx, _ := WithToolError(context.Background())
			out, err := tc.tool.Call(ctx, tc.input)
			if err != nil {
				t.Fatalf("Call() hard error = %v, want nil Go error", err)
			}
			te := ToolErrorFromContext(ctx)
			if te == nil {
				t.Fatalf("ToolErrorFromContext = nil, output=%q", out)
			}
			if te.Code != tc.wantCode {
				t.Fatalf("ToolError.Code = %q, want %q (msg=%q)", te.Code, tc.wantCode, te.Message)
			}
			if out != te.Message {
				t.Fatalf("output = %q, want Error.Message %q", out, te.Message)
			}

			// executeToolCall path: recoverable observation, not hard execution failure.
			result := executeToolCall(context.Background(), ToolCallExecution{
				Specs:  NewToolSpecs([]langtools.Tool{tc.tool}),
				Action: schema.AgentAction{Tool: tc.tool.Name(), ToolInput: tc.input},
			})
			if result.Error != nil {
				t.Fatalf("executeToolCall hard error = %v, want nil", result.Error)
			}
			if result.Result.Error == nil || result.Result.Error.Code != tc.wantCode {
				t.Fatalf("result.Error = %+v, want code %q", result.Result.Error, tc.wantCode)
			}
		})
	}
}
