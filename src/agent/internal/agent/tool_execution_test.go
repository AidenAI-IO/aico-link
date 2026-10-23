package agent

import (
	"context"
	"encoding/base64"
	"errors"
	"strings"
	"testing"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

type toolExecutionCallbackRecorder struct {
	calls   []ToolCall
	results []ToolResult
	events  []string
}

func (r *toolExecutionCallbackRecorder) HandleText(ctx context.Context, text string) {}

func (r *toolExecutionCallbackRecorder) HandleLLMStart(ctx context.Context, prompts []string) {}

func (r *toolExecutionCallbackRecorder) HandleLLMGenerateContentStart(ctx context.Context, ms []llms.MessageContent) {
}

func (r *toolExecutionCallbackRecorder) HandleLLMGenerateContentEnd(ctx context.Context, res *llms.ContentResponse) {
}

func (r *toolExecutionCallbackRecorder) HandleLLMError(ctx context.Context, err error) {}

func (r *toolExecutionCallbackRecorder) HandleChainStart(ctx context.Context, inputs map[string]any) {
}

func (r *toolExecutionCallbackRecorder) HandleChainEnd(ctx context.Context, outputs map[string]any) {}

func (r *toolExecutionCallbackRecorder) HandleChainError(ctx context.Context, err error) {}

func (r *toolExecutionCallbackRecorder) HandleToolStart(ctx context.Context, input string) {
	r.events = append(r.events, "start")
}

func (r *toolExecutionCallbackRecorder) HandleToolEnd(ctx context.Context, output string) {}

func (r *toolExecutionCallbackRecorder) HandleToolError(ctx context.Context, err error) {}

func (r *toolExecutionCallbackRecorder) HandleAgentAction(ctx context.Context, action schema.AgentAction) {
	r.calls = append(r.calls, ToolCall{Action: action})
}

func (r *toolExecutionCallbackRecorder) HandleAgentFinish(ctx context.Context, finish schema.AgentFinish) {
}

func (r *toolExecutionCallbackRecorder) HandleRetrieverStart(ctx context.Context, query string) {}

func (r *toolExecutionCallbackRecorder) HandleRetrieverEnd(ctx context.Context, query string, documents []schema.Document) {
}

func (r *toolExecutionCallbackRecorder) HandleStreamingFunc(ctx context.Context, chunk []byte) {}

func (r *toolExecutionCallbackRecorder) BeforeToolCall(ctx context.Context, call ToolCall) (ToolResult, bool) {
	r.calls = append(r.calls, call)
	return ToolResult{}, true
}

func (r *toolExecutionCallbackRecorder) AfterToolCall(ctx context.Context, call ToolCall, result ToolResult) ToolResult {
	return DefaultAfterToolCall(ctx, call, result)
}

func (r *toolExecutionCallbackRecorder) HandleToolCallResult(ctx context.Context, call ToolCall, result ToolResult) {
	r.events = append(r.events, "result")
	r.results = append(r.results, result)
}

func TestExecuteToolCallNormalizesBeforeValidationAndHooks(t *testing.T) {
	tool := &stubTool{name: "echo", description: "Echo text.", output: "ok"}
	specs := NewToolSpecs([]langtools.Tool{tool})

	var beforeInput string
	var afterInput string
	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "echo",
			ToolInput: "hello\nObservation:",
		},
		Before: func(ctx context.Context, call ToolCall) (ToolResult, bool) {
			beforeInput = call.Input
			return ToolResult{}, true
		},
		After: func(ctx context.Context, call ToolCall, result ToolResult) ToolResult {
			afterInput = call.Input
			result.Output += "-after"
			return result
		},
	})

	if result.Error != nil {
		t.Fatalf("unexpected execution error: %v", result.Error)
	}
	if beforeInput != "hello" || afterInput != "hello" {
		t.Fatalf("hooks saw unnormalized input: before=%q after=%q", beforeInput, afterInput)
	}
	if len(tool.inputs) != 1 || tool.inputs[0] != "hello" {
		t.Fatalf("tool inputs = %#v, want normalized input", tool.inputs)
	}
	if result.Step.Action.ToolInput != "hello" {
		t.Fatalf("step input = %q, want normalized input", result.Step.Action.ToolInput)
	}
	if result.Step.Observation != "ok-after" {
		t.Fatalf("observation = %q, want after hook output", result.Step.Observation)
	}
}

func TestExecuteToolCallUnwrapsCompatibleInputBeforeValidation(t *testing.T) {
	tool := &stubTool{name: "shell", description: "Run shell commands.", output: "ok"}
	specs := NewToolSpecs([]langtools.Tool{tool})

	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "shell",
			ToolInput: `{"__arg1":"{\"command\":\"pwd\"}","description":"run pwd"}`,
		},
	})

	if result.Error != nil {
		t.Fatalf("unexpected execution error: %v", result.Error)
	}
	if len(tool.inputs) != 1 || tool.inputs[0] != `{"command":"pwd"}` {
		t.Fatalf("tool inputs = %#v, want unwrapped JSON before validation", tool.inputs)
	}
}

func TestExecuteToolCallSkillManageEditAcceptsLiteralNewlinesInContent(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, "alpha", testSkillA)
	tool := NewSkillManageTool(dir, "")
	specs := NewToolSpecs([]langtools.Tool{tool})
	updated := "---\nname: alpha\ndescription: Alpha edited\n---\n\nDo alpha things better.\n"
	input := `{"action":"edit","name":"alpha","content":"` + updated + `","reason":"test"}`

	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "skill_manage",
			ToolInput: input,
		},
	})

	if result.Result.IsError() {
		t.Fatalf("skill_manage edit should accept full SKILL.md content with literal newlines, got %s", result.Result.Output)
	}
	if got := readSKILL(t, dir, "alpha"); got != updated {
		t.Fatalf("updated skill content = %q, want %q", got, updated)
	}
}

func TestExecuteToolCallSkillManageEditAcceptsMarkdownQuotesInContent(t *testing.T) {
	dir := t.TempDir()
	writeSKILL(t, dir, "alpha", testSkillA)
	tool := NewSkillManageTool(dir, "")
	specs := NewToolSpecs([]langtools.Tool{tool})
	updated := "---\nname: alpha\ndescription: Alpha edited\n---\n\n1. **点击\"+\"号**：选择\"从相册选择\"。\n"
	input := `{"action":"edit","name":"alpha","content":"` + updated + `","reason":"test"}`
	input = strings.ReplaceAll(input, `\"`, `"`)

	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "skill_manage",
			ToolInput: input,
		},
	})

	if result.Result.IsError() {
		t.Fatalf("skill_manage edit should accept full SKILL.md content with markdown quotes, got %s", result.Result.Output)
	}
	if got := readSKILL(t, dir, "alpha"); got != updated {
		t.Fatalf("updated skill content = %q, want %q", got, updated)
	}
}

func TestExecuteToolCallRepairsQuotesInStringArrayElements(t *testing.T) {
	tool := &stubTool{name: "shell", description: "Run shell commands.", output: "ok"}
	specs := NewToolSpecs([]langtools.Tool{tool})
	input := `{"segments":["tap", "type "done"", "select "first" item"]}`

	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "shell",
			ToolInput: input,
		},
	})

	if result.Result.IsError() {
		t.Fatalf("string-array payload should be repaired, got %s", result.Result.Output)
	}
	if len(tool.inputs) != 1 || tool.inputs[0] != `{"segments":["tap", "type \"done\"", "select \"first\" item"]}` {
		t.Fatalf("tool inputs = %#v, want repaired string-array payload", tool.inputs)
	}
}

func TestExecuteToolCallInvalidToolEmitsToolCallAndResult(t *testing.T) {
	recorder := &toolExecutionCallbackRecorder{}
	specs := NewToolSpecs([]langtools.Tool{&stubTool{name: "echo", description: "Echo.", output: "ok"}})

	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "missing",
			ToolInput: "hello",
		},
		Callback: recorder,
	})

	if !result.Result.IsError() {
		t.Fatalf("invalid tool should be an error result: %#v", result.Result)
	}
	if result.Call.Spec.Name != "missing" || result.Call.Input != "hello" {
		t.Fatalf("invalid tool call = %#v", result.Call)
	}
	if len(recorder.events) != 2 || recorder.events[0] != "start" || recorder.events[1] != "result" {
		t.Fatalf("callback lifecycle events = %#v, want start then result", recorder.events)
	}
	if len(recorder.results) != 1 || !recorder.results[0].IsError() {
		t.Fatalf("expected one error result callback, got %#v", recorder.results)
	}
}

func TestExecuteToolCallValidationFailureEmitsToolCallAndResult(t *testing.T) {
	recorder := &toolExecutionCallbackRecorder{}
	specs := NewToolSpecs([]langtools.Tool{&stubTool{name: "shell", description: "Run shell commands.", output: "ok"}})

	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "shell",
			ToolInput: `not-json`,
		},
		Callback: recorder,
	})

	if !result.Result.IsError() {
		t.Fatalf("invalid JSON should be an error result: %#v", result.Result)
	}
	if result.ActionCompleted {
		t.Fatal("validation failure marked action_completed=true")
	}
	if len(recorder.calls) != 1 || recorder.calls[0].Input != "not-json" {
		t.Fatalf("expected one tool_call before validation failure, got %#v", recorder.calls)
	}
	if len(recorder.results) != 1 || !recorder.results[0].IsError() {
		t.Fatalf("expected one error result callback, got %#v", recorder.results)
	}
	if len(recorder.events) != 2 || recorder.events[0] != "start" || recorder.events[1] != "result" {
		t.Fatalf("callback lifecycle events = %#v, want start then result", recorder.events)
	}
	if recorder.results[0].Duration <= 0 {
		t.Fatalf("expected validation failure duration to be recorded, got %s", recorder.results[0].Duration)
	}
	if got := recorder.results[0].Output; got != "shell input must be valid JSON after compatibility conversion" {
		t.Fatalf("generic validation output = %q", got)
	}
}

func TestExecuteToolCallTouchGestureValidationFailureRequestsStrictRetry(t *testing.T) {
	tool := &stubTool{name: "touch_gesture", description: "Perform a gesture.", output: "should-not-run"}
	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: NewToolSpecs([]langtools.Tool{tool}),
		Action: schema.AgentAction{
			Tool:      "touch_gesture",
			ToolInput: `{"type":"tap","point":{"x":500,500}}`,
		},
	})

	if !result.Result.IsError() || result.Result.Error.Code != CodeInvalidArguments {
		t.Fatalf("result = %#v, want invalid_arguments", result.Result)
	}
	if result.ActionCompleted {
		t.Fatal("invalid touch_gesture marked action_completed=true")
	}
	if len(tool.inputs) != 0 {
		t.Fatalf("touch_gesture executed with malformed input: %#v", tool.inputs)
	}
	for _, want := range []string{
		"no touch action was executed",
		"Retry touch_gesture now with strict JSON",
		`both named keys "x" and "y"`,
		`Invalid: {"type":"tap","point":{"x":500,500}}`,
		`Correct: {"type":"tap","point":{"x":500,"y":500}}`,
		"Do not continue with another action until the corrected call succeeds",
	} {
		if !strings.Contains(result.Result.Output, want) {
			t.Fatalf("touch_gesture validation output missing %q:\n%s", want, result.Result.Output)
		}
	}
}

func TestExecuteToolCallBeforeMayRejectWithToolResult(t *testing.T) {
	recorder := &toolExecutionCallbackRecorder{}
	tool := &stubTool{name: "echo", description: "Echo text.", output: "should-not-run"}
	specs := NewToolSpecs([]langtools.Tool{tool})

	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "echo",
			ToolInput: "hello",
		},
		Before: func(ctx context.Context, call ToolCall) (ToolResult, bool) {
			return ToolResult{
				Output: "blocked by policy",
				Error:  NewToolError(CodeToolExecutionFailed, "blocked by policy"),
			}, false
		},
		Callback: recorder,
	})

	if len(tool.inputs) != 0 {
		t.Fatalf("tool should not have been called, inputs=%#v", tool.inputs)
	}
	if !result.Result.IsError() || result.Result.Output != "blocked by policy" {
		t.Fatalf("unexpected rejection result: %#v", result.Result)
	}
	if result.ActionCompleted {
		t.Fatal("rejected tool call marked action_completed=true")
	}
	if result.Step.Observation != "blocked by policy" {
		t.Fatalf("step observation = %q", result.Step.Observation)
	}
	if len(recorder.events) != 2 || recorder.events[0] != "start" || recorder.events[1] != "result" {
		t.Fatalf("callback lifecycle events = %#v, want start then result", recorder.events)
	}
}

func TestExecuteToolCallWrapsToolErrorsAndContinues(t *testing.T) {
	tool := &stubTool{name: "echo", description: "Echo text.", err: errors.New("boom")}
	specs := NewToolSpecs([]langtools.Tool{tool})

	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "echo",
			ToolInput: "hello",
		},
	})

	if result.Error != nil {
		t.Fatalf("tool failure should be returned as tool result, got execution error: %v", result.Error)
	}
	if !result.Result.IsError() {
		t.Fatalf("result should be marked as error: %#v", result.Result)
	}
	if result.Result.Error == nil || result.Result.Error.Code != CodeToolExecutionFailed || result.Result.Error.Message != "boom" {
		t.Fatalf("ToolError = %+v, want tool_execution_failed boom", result.Result.Error)
	}
	if result.Step.Observation != "boom" {
		t.Fatalf("observation = %q", result.Step.Observation)
	}
	if !result.ActionCompleted {
		t.Fatal("invoked tool failure marked action_completed=false")
	}
}

func TestExecuteToolCallBuiltInStringErrorToolsReturnStructuredErrors(t *testing.T) {
	tests := []struct {
		name     string
		tool     langtools.Tool
		input    string
		wantCode string
	}{
		{
			name:     "shell missing command",
			tool:     &ShellTool{},
			input:    `{}`,
			wantCode: CodeInvalidArguments,
		},
		{
			name:     "web search unconfigured",
			tool:     &WebSearchTool{provider: "brave"},
			input:    `{"query":"aico"}`,
			wantCode: CodeModuleUnavailable,
		},
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
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			result := executeToolCall(context.Background(), ToolCallExecution{
				Specs:  NewToolSpecs([]langtools.Tool{tc.tool}),
				Action: schema.AgentAction{Tool: tc.tool.Name(), ToolInput: tc.input},
			})
			if result.Result.Error == nil {
				t.Fatalf("expected structured ToolError, got result %#v", result.Result)
			}
			if result.Result.Error.Code != tc.wantCode {
				t.Fatalf("Error.Code = %q, want %q (error=%+v)", result.Result.Error.Code, tc.wantCode, result.Result.Error)
			}
			if result.Result.Output != result.Result.Error.Message {
				t.Fatalf("Output must equal Error.Message, got %q vs %q", result.Result.Output, result.Result.Error.Message)
			}
		})
	}
}

func TestExecuteToolCallWrapsToolLocalContextErrorsAsObservations(t *testing.T) {
	for _, err := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(err.Error(), func(t *testing.T) {
			recorder := &toolExecutionCallbackRecorder{}
			tool := &stubTool{name: "echo", description: "Echo text.", err: err}
			specs := NewToolSpecs([]langtools.Tool{tool})

			result := executeToolCall(context.Background(), ToolCallExecution{
				Specs: specs,
				Action: schema.AgentAction{
					Tool:      "echo",
					ToolInput: "hello",
				},
				Callback: recorder,
			})

			if result.Error != nil {
				t.Fatalf("tool-local context error should stay in the tool result, got execution error: %v", result.Error)
			}
			if result.Result.Error == nil {
				t.Fatalf("result.Error is nil, want structured error for %v", err)
			}
			wantCode := CodeCanceled
			if errors.Is(err, context.DeadlineExceeded) {
				wantCode = CodeDeadlineExceeded
			}
			if result.Result.Error.Code != wantCode {
				t.Fatalf("result error code = %q, want %q", result.Result.Error.Code, wantCode)
			}
			if result.Result.Output != result.Result.Error.Message {
				t.Fatalf("Output must equal Error.Message, got %q vs %q", result.Result.Output, result.Result.Error.Message)
			}
			if len(recorder.events) != 2 || recorder.events[0] != "start" || recorder.events[1] != "result" {
				t.Fatalf("callback lifecycle events = %#v, want start then result", recorder.events)
			}
			if len(recorder.results) != 1 || recorder.results[0].Error == nil || recorder.results[0].Error.Code != wantCode {
				t.Fatalf("terminal result callback = %#v, want error code %v", recorder.results, wantCode)
			}
			if recorder.results[0].Output != recorder.results[0].Error.Message {
				t.Fatalf("callback Output must equal Error.Message, got %q vs %q", recorder.results[0].Output, recorder.results[0].Error.Message)
			}
		})
	}
}

func TestExecuteToolCallPropagatesParentCancellation(t *testing.T) {
	tests := []struct {
		name string
		tool langtools.Tool
	}{
		{
			name: "tool returns parent context error",
			tool: &blockingTool{name: "echo", description: "Echo text.", release: make(chan struct{})},
		},
		{
			name: "tool ignores canceled context and succeeds",
			tool: &stubTool{name: "echo", description: "Echo text.", output: "ok"},
		},
		{
			name: "tool ignores canceled context and fails normally",
			tool: &stubTool{name: "echo", description: "Echo text.", err: errors.New("boom")},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			recorder := &toolExecutionCallbackRecorder{}
			specs := NewToolSpecs([]langtools.Tool{tc.tool})

			result := executeToolCall(ctx, ToolCallExecution{
				Specs: specs,
				Action: schema.AgentAction{
					Tool:      "echo",
					ToolInput: "hello",
				},
				Callback: recorder,
			})

			if !errors.Is(result.Error, context.Canceled) {
				t.Fatalf("execution error = %v, want parent cancellation", result.Error)
			}
			if result.Result.Error == nil || result.Result.Error.Code != CodeCanceled {
				t.Fatalf("result error = %+v, want canceled structured error", result.Result.Error)
			}
			if len(recorder.results) != 1 || recorder.results[0].Error == nil || recorder.results[0].Error.Code != CodeCanceled {
				t.Fatalf("terminal result callback = %#v, want canceled error", recorder.results)
			}
		})
	}
}

func TestDefaultAfterToolCallPreservesScreenshotActionOutputAndMarksTerminate(t *testing.T) {
	image := []byte("jpeg")
	output := `{"action_output":"ok","width":320,"height":240,"format":"jpeg","size":4,"data":"` +
		base64.StdEncoding.EncodeToString(image) + `"}`
	tool := &stubTool{name: "screenshot", description: "Take screenshot.", output: output, visual: true}
	specs := NewToolSpecs([]langtools.Tool{tool})

	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "screenshot",
			ToolInput: `{}`,
		},
		After: DefaultAfterToolCall,
	})

	if result.Error != nil {
		t.Fatalf("unexpected execution error: %v", result.Error)
	}
	if !result.Result.Terminate {
		t.Fatalf("screenshot result should be marked terminate: %#v", result.Result)
	}
	if result.Result.Output != output {
		t.Fatalf("raw screenshot output should remain available to the agent")
	}
	if result.Result.Summary != "ok" {
		t.Fatalf("screenshot summary = %q, want original action output", result.Result.Summary)
	}
	if result.Step.Observation != result.Result.Output {
		t.Fatalf("step observation should preserve raw output")
	}
}

func TestExecuteToolCallIgnoresNestedToolErrorPollution(t *testing.T) {
	// Regression from production logs: search_launch_app / enter_text
	// recover after a nested ClipboardTool SetToolError, but executeToolCall used
	// to overwrite the parent JSON observation with the leftover bridge error.
	const parentOutput = `{"ok":false,"error":"app did not open","target":"小红书","steps":["opened system search"]}`
	nestedErr := NewToolError(CodeBridgeNotConnected, "phone bridge not connected and no supported Dynamic Island return entry is available")
	tool := &stubTool{
		name:        "search_launch_app",
		description: "Search and launch an app.",
		callFn: func(ctx context.Context, input string) (string, error) {
			SetToolError(ctx, nestedErr)
			return parentOutput, nil
		},
	}
	specs := NewToolSpecs([]langtools.Tool{tool})

	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "search_launch_app",
			ToolInput: `{"app":"小红书"}`,
		},
	})

	if result.Error != nil {
		t.Fatalf("nested pollution should not become an execution error: %v", result.Error)
	}
	if result.Result.IsError() {
		t.Fatalf("parent recovery should not be marked as tool error: %#v", result.Result)
	}
	if result.Result.Output != parentOutput {
		t.Fatalf("Output = %q, want parent JSON preserved", result.Result.Output)
	}
	if result.Step.Observation != parentOutput {
		t.Fatalf("Observation = %q, want parent JSON preserved", result.Step.Observation)
	}
}

func TestExecuteToolCallAdoptsIntentionalTopLevelToolError(t *testing.T) {
	toolErr := NewToolError(CodeBridgeNotConnected, "phone bridge not connected and no supported Dynamic Island return entry is available")
	tool := &stubTool{
		name:        "open_app",
		description: "Open an app.",
		callFn: func(ctx context.Context, input string) (string, error) {
			SetToolError(ctx, toolErr)
			return toolErrorString(toolErr), nil
		},
	}
	specs := NewToolSpecs([]langtools.Tool{tool})

	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "open_app",
			ToolInput: `{"app":"小红书"}`,
		},
	})

	if !result.Result.IsError() {
		t.Fatalf("intentional top-level SetToolError should remain an error: %#v", result.Result)
	}
	if result.Result.Error == nil || result.Result.Error.Code != CodeBridgeNotConnected {
		t.Fatalf("ToolError = %+v, want bridge_not_connected", result.Result.Error)
	}
	if result.Result.Output != toolErr.Message {
		t.Fatalf("Output = %q, want %q", result.Result.Output, toolErr.Message)
	}
}

func TestExecuteToolCallBeforeHookRejectWithEmptyOutputSatisfiesInvariant(t *testing.T) {
	// Regression: normalizeRejectedToolResult called normalizeToolResult which back-filled
	// Output with "error: " + Error.Message, breaking Output == Error.Message invariant.
	tool := &stubTool{name: "echo", description: "Echo text.", output: "should-not-run"}
	specs := NewToolSpecs([]langtools.Tool{tool})

	result := executeToolCall(context.Background(), ToolCallExecution{
		Specs: specs,
		Action: schema.AgentAction{
			Tool:      "echo",
			ToolInput: "hello",
		},
		Before: func(ctx context.Context, call ToolCall) (ToolResult, bool) {
			// Hook returns Error but leaves Output empty — the bug case.
			return ToolResult{
				Error: NewToolError(CodeToolExecutionFailed, "denied"),
			}, false
		},
	})

	if len(tool.inputs) != 0 {
		t.Fatalf("tool should not have been called, inputs=%#v", tool.inputs)
	}
	if strings.TrimSpace(result.Result.Output) == "" {
		t.Fatalf("Output must be non-empty so LLM sees a message; got empty string")
	}
	if result.Result.Error == nil {
		t.Fatalf("Error must be set on a rejected result")
	}
	if result.Result.Output != result.Result.Error.Message {
		t.Fatalf("Output == Error.Message invariant violated: Output=%q Error.Message=%q",
			result.Result.Output, result.Result.Error.Message)
	}
}

func TestNormalizeToolResultAlignsNonEmptyErrorMessageToOutput(t *testing.T) {
	result := normalizeToolResult(ToolResult{
		Output: "policy override message",
		Error:  NewToolError(CodeToolExecutionFailed, "original message"),
	})
	if result.Error == nil {
		t.Fatal("expected structured error")
	}
	if result.Output != result.Error.Message {
		t.Fatalf("Output == Error.Message invariant violated: Output=%q Error.Message=%q", result.Output, result.Error.Message)
	}
}

func TestToolResultErrorIsStructured(t *testing.T) {
	ok := ToolResult{Output: "done", Summary: "ok"}
	if ok.IsError() {
		t.Errorf("success result reported IsError")
	}
	if ok.Error != nil {
		t.Errorf("success result has non-nil Error: %+v", ok.Error)
	}

	err := ToolResult{
		Output: "WeChat is not installed",
		Error:  NewToolError(CodeAppNotInstalled, "WeChat is not installed"),
	}
	if !err.IsError() {
		t.Errorf("failure result did not report IsError")
	}
	if err.Error.Code != CodeAppNotInstalled {
		t.Errorf("Error.Code = %q", err.Error.Code)
	}
	if err.Output != err.Error.Message {
		t.Errorf("Output must equal Error.Message on failure; got %q vs %q", err.Output, err.Error.Message)
	}
}
