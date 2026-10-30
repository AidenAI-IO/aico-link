package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	"strings"
	"testing"

	"aico-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

func TestFunctionAgentParseOutputSkipsNilToolCalls(t *testing.T) {
	agent := &FunctionAgent{OutputKey: "output"}

	actions, finish, err := agent.ParseOutput(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			ToolCalls: []llms.ToolCall{
				{ID: "ignored", Type: "function"},
				{
					ID:   "call_1",
					Type: "function",
					FunctionCall: &llms.FunctionCall{
						Name:      "echo",
						Arguments: `{"__arg1":"hello"}`,
					},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("ParseOutput() error = %v", err)
	}
	if finish != nil {
		t.Fatalf("expected no finish, got %#v", finish)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %#v", actions)
	}
	if actions[0].Tool != "echo" || actions[0].ToolInput != "hello" || actions[0].ToolID != "call_1" {
		t.Fatalf("unexpected action: %#v", actions[0])
	}
}

func TestChoiceWithOnlyToolCallAssignsIDToLegacyFunctionCall(t *testing.T) {
	functionCall := &llms.FunctionCall{Name: "echo", Arguments: `{}`}
	choice := choiceWithOnlyToolCall(llms.ContentChoice{FuncCall: functionCall}, "call_legacy")

	if len(choice.ToolCalls) != 1 {
		t.Fatalf("tool calls = %#v, want one synthesized tool call", choice.ToolCalls)
	}
	if choice.ToolCalls[0].ID != "call_legacy" || choice.ToolCalls[0].FunctionCall != functionCall {
		t.Fatalf("tool call = %#v, want ID call_legacy and original function call", choice.ToolCalls[0])
	}
}

func TestChoiceWithOnlyToolCallDropsUnexecutedResponsesItems(t *testing.T) {
	originalItems := []json.RawMessage{
		json.RawMessage(`{"type":"reasoning","id":"rs_1","content":[{"type":"reasoning_text","text":"inspect"}]}`),
		json.RawMessage(`{"type":"message","id":"msg_1","role":"assistant","content":[]}`),
		json.RawMessage(`{"type":"function_call","id":"fc_1","call_id":"call_1","name":"first","arguments":"{}"}`),
		json.RawMessage(`{"type":"function_call","id":"fc_2","call_id":"call_2","name":"second","arguments":"{}"}`),
	}
	choice := llms.ContentChoice{
		ToolCalls: []llms.ToolCall{
			{ID: "call_1", Type: "function", FunctionCall: &llms.FunctionCall{Name: "first", Arguments: `{}`}},
			{ID: "call_2", Type: "function", FunctionCall: &llms.FunctionCall{Name: "second", Arguments: `{}`}},
		},
		GenerationInfo: map[string]any{"responses_output_items": originalItems},
	}

	selected := choiceWithOnlyToolCall(choice, "call_2")
	if len(selected.ToolCalls) != 1 || selected.ToolCalls[0].ID != "call_2" {
		t.Fatalf("selected tool calls = %#v", selected.ToolCalls)
	}
	items, ok := selected.GenerationInfo["responses_output_items"].([]json.RawMessage)
	if !ok || len(items) != 3 {
		t.Fatalf("selected Responses items = %#v, want reasoning, message, and selected call", selected.GenerationInfo["responses_output_items"])
	}
	parts := make([]string, len(items))
	for i := range items {
		parts[i] = string(items[i])
	}
	joined := strings.Join(parts, "\n")
	if strings.Contains(joined, `"call_id":"call_1"`) || !strings.Contains(joined, `"call_id":"call_2"`) {
		t.Fatalf("selected Responses items = %s", joined)
	}
	original, ok := choice.GenerationInfo["responses_output_items"].([]json.RawMessage)
	if !ok || len(original) != 4 {
		t.Fatal("original Responses output metadata was mutated")
	}
}

// Native Interactions steps are the provider's own record and every function_call
// in it needs a function_result, so the turn keeps all of its calls and the
// runtime answers the unexecuted ones instead of rewriting the step list.
func TestChoiceWithOnlyToolCallKeepsUnexecutedInteractionsSteps(t *testing.T) {
	originalSteps := []json.RawMessage{
		json.RawMessage(`{"type":"thought","signature":"sig_1","summary":[]}`),
		json.RawMessage(`{"type":"model_output","content":[{"type":"text","text":"working"}]}`),
		json.RawMessage(`{"type":"function_call","id":"call_1","name":"first","arguments":{}}`),
		json.RawMessage(`{"type":"function_call","id":"call_2","name":"second","arguments":{}}`),
	}
	choice := llms.ContentChoice{
		ToolCalls: []llms.ToolCall{
			{ID: "call_1", Type: "function", FunctionCall: &llms.FunctionCall{Name: "first", Arguments: `{}`}},
			{ID: "call_2", Type: "function", FunctionCall: &llms.FunctionCall{Name: "second", Arguments: `{}`}},
		},
		GenerationInfo: map[string]any{
			"interactions_steps": originalSteps,
			"marker":             "original",
		},
	}

	selected := choiceWithOnlyToolCall(choice, "call_2")
	if len(selected.ToolCalls) != 2 {
		t.Fatalf("selected tool calls = %#v, want both calls for Interactions replay", selected.ToolCalls)
	}
	if selected.FuncCall == nil || selected.FuncCall.Name != "second" {
		t.Fatalf("selected FuncCall = %#v, want second", selected.FuncCall)
	}
	steps, ok := selected.GenerationInfo["interactions_steps"].([]json.RawMessage)
	if !ok || len(steps) != len(originalSteps) {
		t.Fatalf("selected Interactions steps = %#v, want the provider step list unchanged", selected.GenerationInfo["interactions_steps"])
	}
	joined := strings.Join([]string{string(steps[0]), string(steps[1]), string(steps[2]), string(steps[3])}, "\n")
	for _, want := range []string{`"type":"thought"`, `"signature":"sig_1"`, `"type":"model_output"`, `"id":"call_1"`, `"id":"call_2"`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("selected Interactions steps lost %s: %s", want, joined)
		}
	}

	// The unexecuted calls are answered in the tool result instead.
	results := unexecutedInteractionToolResults(messages.ConvertChoiceToContextManagerMessage(selected), "call_2")
	if len(results) != 1 || results[0].ToolCallID != "call_1" || results[0].Name != "first" {
		t.Fatalf("unexecuted call results = %#v, want one result for call_1", results)
	}
	if results[0].Content != unexecutedInteractionToolResult {
		t.Fatalf("unexecuted call content = %q", results[0].Content)
	}
	executedOnly := messages.Message{ToolCalls: []messages.ToolCall{{ID: "call_2", Name: "second"}}}
	if extra := unexecutedInteractionToolResults(executedOnly, "call_2"); len(extra) != 0 {
		t.Fatalf("results for the executed call = %#v, want none", extra)
	}
}

func TestFunctionAgentParseOutputUsesChoiceContentAsToolContent(t *testing.T) {
	agent := &FunctionAgent{OutputKey: "output"}

	actions, finish, err := agent.ParseOutput(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			Content: "发送测试文本。",
			ToolCalls: []llms.ToolCall{{
				ID:   "call_1",
				Type: "function",
				FunctionCall: &llms.FunctionCall{
					Name:      "echo",
					Arguments: `{"input":"hello"}`,
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("ParseOutput() error = %v", err)
	}
	if finish != nil {
		t.Fatalf("expected no finish, got %#v", finish)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %#v", actions)
	}
	if actions[0].ToolInput != "hello" {
		t.Fatalf("ToolInput = %q, want stripped tool input", actions[0].ToolInput)
	}
	if got := toolContentFromAction(actions[0]); got != "发送测试文本。" {
		t.Fatalf("tool content = %q", got)
	}

	var log toolActionLog
	if err := json.Unmarshal([]byte(actions[0].Log), &log); err != nil {
		t.Fatalf("action log should be structured JSON: %v", err)
	}
	if log.Version != toolActionLogVersion || log.ToolContent != "发送测试文本。" {
		t.Fatalf("unexpected action log metadata: %#v", log)
	}
}

func TestFunctionAgentParseOutputBindsChoiceContentToFirstValidToolCall(t *testing.T) {
	agent := &FunctionAgent{OutputKey: "output"}

	actions, finish, err := agent.ParseOutput(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			Content: "先检查。",
			ToolCalls: []llms.ToolCall{
				{ID: "ignored", Type: "function"},
				{
					ID:   "call_1",
					Type: "function",
					FunctionCall: &llms.FunctionCall{
						Name:      "echo",
						Arguments: `{"input":"first"}`,
					},
				},
				{
					ID:   "call_2",
					Type: "function",
					FunctionCall: &llms.FunctionCall{
						Name:      "echo",
						Arguments: `{"input":"second"}`,
					},
				},
			},
		}},
	})
	if err != nil {
		t.Fatalf("ParseOutput() error = %v", err)
	}
	if finish != nil {
		t.Fatalf("expected no finish, got %#v", finish)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %#v", actions)
	}
	if actions[0].ToolInput != "first" || actions[0].ToolID != "call_1" {
		t.Fatalf("unexpected first action: %#v", actions[0])
	}
	if got := toolContentFromAction(actions[0]); got != "先检查。" {
		t.Fatalf("first tool content = %q", got)
	}
}

func TestFunctionAgentParseOutputKeepsTaggedFinalText(t *testing.T) {
	agent := &FunctionAgent{OutputKey: "output"}
	content := "我已经完成了音量检查。\n<tts>我已经完成了任务，请查收。</tts>"

	actions, finish, err := agent.ParseOutput(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			Content: content,
		}},
	})
	if err != nil {
		t.Fatalf("ParseOutput() error = %v", err)
	}
	if len(actions) != 0 {
		t.Fatalf("expected no actions, got %#v", actions)
	}
	if finish == nil {
		t.Fatal("finish = nil")
	}
	if got := finish.ReturnValues["output"]; got != content {
		t.Fatalf("finish output = %#v, want raw tagged content", got)
	}
}

func TestFunctionAgentParseOutputExtractsGenericInputWrapper(t *testing.T) {
	agent := &FunctionAgent{OutputKey: "output"}

	actions, finish, err := agent.ParseOutput(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			Content: "Echoing text.",
			ToolCalls: []llms.ToolCall{{
				ID:   "call_1",
				Type: "function",
				FunctionCall: &llms.FunctionCall{
					Name:      "echo",
					Arguments: `{"input":"hello"}`,
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("ParseOutput() error = %v", err)
	}
	if finish != nil {
		t.Fatalf("expected no finish, got %#v", finish)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %#v", actions)
	}
	if actions[0].ToolInput != "hello" {
		t.Fatalf("ToolInput = %q, want generic input wrapper", actions[0].ToolInput)
	}
	if got := toolContentFromAction(actions[0]); got != "Echoing text." {
		t.Fatalf("tool content = %q", got)
	}
}

func TestFunctionAgentParseOutputPassesStructuredToolArguments(t *testing.T) {
	agent := &FunctionAgent{OutputKey: "output"}

	actions, finish, err := agent.ParseOutput(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			Content: "按回车。",
			ToolCalls: []llms.ToolCall{{
				ID:   "call_1",
				Type: "function",
				FunctionCall: &llms.FunctionCall{
					Name:      "keyboard_tap",
					Arguments: `{"keys":["enter"]}`,
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("ParseOutput() error = %v", err)
	}
	if finish != nil {
		t.Fatalf("expected no finish, got %#v", finish)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %#v", actions)
	}
	if actions[0].ToolInput != `{"keys":["enter"]}` {
		t.Fatalf("ToolInput = %q, want structured args without description", actions[0].ToolInput)
	}
	if got := toolContentFromAction(actions[0]); got != "按回车。" {
		t.Fatalf("tool content = %q", got)
	}
}

func TestFunctionAgentParseOutputKeepsLegacyArg1CompatibilityWithoutContentFallback(t *testing.T) {
	agent := &FunctionAgent{OutputKey: "output"}

	actions, finish, err := agent.ParseOutput(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			ToolCalls: []llms.ToolCall{{
				ID:   "call_1",
				Type: "function",
				FunctionCall: &llms.FunctionCall{
					Name:      "keyboard_tap",
					Arguments: `{"__arg1":"{\"keys\":[\"enter\"]}","description":"我会按下回车键。","speech":"按回车。"}`,
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("ParseOutput() error = %v", err)
	}
	if finish != nil {
		t.Fatalf("expected no finish, got %#v", finish)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %#v", actions)
	}
	if actions[0].ToolInput != `{"keys":["enter"]}` {
		t.Fatalf("ToolInput = %q, want legacy __arg1", actions[0].ToolInput)
	}
	if got := toolContentFromAction(actions[0]); got != "" {
		t.Fatalf("tool content = %q, want empty without assistant content", got)
	}
}

func TestFunctionAgentParseOutputDoesNotConsumeSpeechArgumentAsMetadata(t *testing.T) {
	agent := &FunctionAgent{OutputKey: "output"}

	actions, finish, err := agent.ParseOutput(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			ToolCalls: []llms.ToolCall{{
				ID:   "call_1",
				Type: "function",
				FunctionCall: &llms.FunctionCall{
					Name:      "keyboard_tap",
					Arguments: `{"keys":["enter"],"speech":"按回车。"}`,
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("ParseOutput() error = %v", err)
	}
	if finish != nil {
		t.Fatalf("expected no finish, got %#v", finish)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %#v", actions)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(actions[0].ToolInput), &input); err != nil {
		t.Fatalf("ToolInput is not JSON: %q err=%v", actions[0].ToolInput, err)
	}
	if _, ok := input["keys"]; !ok || input["speech"] != "按回车。" {
		t.Fatalf("ToolInput should preserve speech as an ordinary tool argument: %#v", input)
	}
	if got := toolContentFromAction(actions[0]); got != "" {
		t.Fatalf("tool content = %q, want empty without assistant content", got)
	}
}

func TestFunctionAgentParseOutputDoesNotSynthesizeMissingToolContent(t *testing.T) {
	agent := &FunctionAgent{OutputKey: "output"}

	actions, finish, err := agent.ParseOutput(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			ToolCalls: []llms.ToolCall{{
				ID:   "call_1",
				Type: "function",
				FunctionCall: &llms.FunctionCall{
					Name:      "echo",
					Arguments: `{"__arg1":"hello","description":"  "}`,
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("ParseOutput() error = %v", err)
	}
	if finish != nil {
		t.Fatalf("expected no finish, got %#v", finish)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %#v", actions)
	}
	if got := toolContentFromAction(actions[0]); got != "" {
		t.Fatalf("tool content = %q, want empty", got)
	}
}

func TestFunctionAgentParseOutputPreservesSpeechArgumentWhenDisabled(t *testing.T) {
	agent := &FunctionAgent{OutputKey: "output"}

	actions, finish, err := agent.ParseOutput(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			ToolCalls: []llms.ToolCall{{
				ID:   "call_1",
				Type: "function",
				FunctionCall: &llms.FunctionCall{
					Name:      "say",
					Arguments: `{"speech":"hello","volume":1}`,
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("ParseOutput() error = %v", err)
	}
	if finish != nil {
		t.Fatalf("expected no finish, got %#v", finish)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %#v", actions)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(actions[0].ToolInput), &input); err != nil {
		t.Fatalf("ToolInput is not JSON: %q err=%v", actions[0].ToolInput, err)
	}
	if input["speech"] != "hello" || input["volume"] != float64(1) {
		t.Fatalf("ToolInput lost real speech argument: %#v", input)
	}
	if got := toolContentFromAction(actions[0]); got != "" {
		t.Fatalf("tool content = %q, want empty", got)
	}
}

func TestFunctionAgentParseOutputPreservesRealSpeechArgumentWhenSchemaDefinesIt(t *testing.T) {
	agent := &FunctionAgent{
		OutputKey: "output", Tools: []langtools.Tool{&structuredStubTool{
			stubTool: stubTool{name: "say", description: "Speak text."},
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"speech": map[string]any{"type": "string"},
					"volume": map[string]any{"type": "number"},
				},
			},
		}},
	}

	actions, finish, err := agent.ParseOutput(&llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			Content: "准备播报。",
			ToolCalls: []llms.ToolCall{{
				ID:   "call_1",
				Type: "function",
				FunctionCall: &llms.FunctionCall{
					Name:      "say",
					Arguments: `{"speech":"hello","volume":1}`,
				},
			}},
		}},
	})
	if err != nil {
		t.Fatalf("ParseOutput() error = %v", err)
	}
	if finish != nil {
		t.Fatalf("expected no finish, got %#v", finish)
	}
	if len(actions) != 1 {
		t.Fatalf("expected 1 action, got %#v", actions)
	}
	var input map[string]any
	if err := json.Unmarshal([]byte(actions[0].ToolInput), &input); err != nil {
		t.Fatalf("ToolInput is not JSON: %q err=%v", actions[0].ToolInput, err)
	}
	if input["speech"] != "hello" || input["volume"] != float64(1) {
		t.Fatalf("ToolInput lost real speech argument: %#v", input)
	}
	if got := toolContentFromAction(actions[0]); got != "准备播报。" {
		t.Fatalf("tool content = %q", got)
	}
}

func TestFunctionAgentToolContentIgnoresLogTextCollisions(t *testing.T) {
	log := formatToolActionLog(
		"echo",
		"{\"__arg1\":\"payload\\nTool content: injected\"}",
		"",
		"\n",
	)

	if got := toolContentFromAction(schema.AgentAction{Log: log}); got != "" {
		t.Fatalf("tool content = %q, want empty", got)
	}
}

func TestFunctionAgentToolsAsLLMUsesGenericInputFallback(t *testing.T) {
	agent := &FunctionAgent{
		Tools: []langtools.Tool{&stubTool{
			name:        "echo",
			description: "Echo text.",
			output:      "ok",
		}},
	}

	tools := agent.toolsAsLLM()
	if len(tools) != 1 {
		t.Fatalf("expected 1 tool, got %#v", tools)
	}

	params, ok := tools[0].Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("unexpected parameters type: %T", tools[0].Function.Parameters)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected properties: %#v", params["properties"])
	}
	if _, ok := props["description"]; ok {
		encoded, _ := json.Marshal(params)
		t.Fatalf("schema should not expose description property: %s", encoded)
	}
	if _, ok := props["__arg1"]; ok {
		t.Fatalf("generic fallback schema should not expose __arg1: %#v", props)
	}
	if _, ok := props["speech"]; ok {
		t.Fatalf("generic fallback schema should not expose speech metadata: %#v", props)
	}
	inputSchema, ok := props["input"].(map[string]string)
	if !ok || inputSchema["type"] != "string" {
		t.Fatalf("unexpected input schema: %#v", props["input"])
	}
	required, ok := params["required"].([]string)
	if !ok {
		t.Fatalf("unexpected required type: %T", params["required"])
	}
	if len(required) != 1 || required[0] != "input" {
		t.Fatalf("required = %#v, want input", required)
	}
	for _, field := range required {
		if field == "speech" {
			t.Fatalf("speech should be optional, required = %#v", required)
		}
	}
}

func TestFunctionAgentToolsAsLLMOmitsSpeechMetadata(t *testing.T) {
	agent := &FunctionAgent{
		Tools: []langtools.Tool{&stubTool{
			name:        "echo",
			description: "Echo text.",
			output:      "ok",
		}},
	}

	tools := agent.toolsAsLLM()
	params, ok := tools[0].Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("unexpected parameters type: %T", tools[0].Function.Parameters)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected properties: %#v", params["properties"])
	}
	if _, ok := props["speech"]; ok {
		t.Fatalf("schema should not expose speech metadata: %#v", props)
	}
}

type structuredStubTool struct {
	stubTool
	schema map[string]any
}

func (t *structuredStubTool) ArgsSchema() map[string]any { return t.schema }

func TestFunctionAgentToolsAsLLMUsesStructuredSchema(t *testing.T) {
	agent := &FunctionAgent{
		Tools: []langtools.Tool{&structuredStubTool{
			stubTool: stubTool{name: "structured", description: "Structured input."},
			schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"keys": map[string]any{"type": "array"},
				},
				"required": []string{"keys"},
			},
		}},
	}

	tools := agent.toolsAsLLM()
	params, ok := tools[0].Function.Parameters.(map[string]any)
	if !ok {
		t.Fatalf("unexpected parameters type: %T", tools[0].Function.Parameters)
	}
	props, ok := params["properties"].(map[string]any)
	if !ok {
		t.Fatalf("unexpected properties: %#v", params["properties"])
	}
	if _, ok := props["keys"]; !ok {
		t.Fatalf("missing structured keys property: %#v", props)
	}
	if _, ok := props["__arg1"]; ok {
		t.Fatalf("structured schema should not expose __arg1: %#v", props)
	}
	if _, ok := props["description"]; ok {
		t.Fatalf("structured schema should not expose description property: %#v", props)
	}
	if _, ok := props["speech"]; ok {
		t.Fatalf("structured schema should not expose speech metadata: %#v", props)
	}
}

func TestPostActionScreenshotToolForwardsStructuredSchema(t *testing.T) {
	inner := &structuredStubTool{
		stubTool: stubTool{name: "touch_gesture", description: "Touch."},
		schema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"type": map[string]any{"type": "string"},
			},
		},
	}
	wrapped := newPostActionScreenshotTool(inner, &stubTool{name: "screenshot", output: `{"width":1,"height":1,"format":"jpeg","size":1,"data":"YQ=="}`}, 0)
	structured, ok := wrapped.(structuredInputTool)
	if !ok {
		t.Fatalf("wrapped tool does not implement structuredInputTool")
	}
	props := structured.ArgsSchema()["properties"].(map[string]any)
	if _, ok := props["type"]; !ok {
		t.Fatalf("forwarded schema missing inner properties: %#v", props)
	}
}

func TestBuildImagePartDefaultsMimeType(t *testing.T) {
	part := buildImagePart("", []byte("image-bytes"))
	imagePart, ok := part.(llms.ImageURLContent)
	if !ok {
		t.Fatalf("expected image url content, got %T", part)
	}
	if imagePart.URL != "data:image/png;base64,aW1hZ2UtYnl0ZXM=" {
		t.Fatalf("unexpected image url: %q", imagePart.URL)
	}
}

func TestFunctionAgentCompactsInvalidVisualObservation(t *testing.T) {
	agent := &FunctionAgent{
		Tools: []langtools.Tool{&stubTool{name: "screenshot", visual: true}},
	}
	observation := strings.Repeat("x", maxToolObservationRunes+10)

	toolContent, followups := agent.observationMessagesForStep(schema.AgentStep{
		Action:      schema.AgentAction{Tool: "screenshot"},
		Observation: observation,
	})

	if followups != nil {
		t.Fatalf("expected no followup messages for invalid visual observation, got %#v", followups)
	}
	if toolContent == observation {
		t.Fatal("invalid visual observation was not compacted")
	}
	if !strings.Contains(toolContent, "[truncated 10 chars]") {
		t.Fatalf("unexpected compacted observation suffix: %q", toolContent[len(toolContent)-40:])
	}
}

func TestFunctionAgentRejectsEmptyVisualObservationData(t *testing.T) {
	agent := &FunctionAgent{
		Tools: []langtools.Tool{&stubTool{name: "screenshot", visual: true}},
	}
	observation := `{"width":800,"height":600,"format":"jpeg","size":0,"data":""}`
	step := schema.AgentStep{
		Action:      schema.AgentAction{Tool: "screenshot"},
		Observation: observation,
	}

	if agent.countVisualObservations([]schema.AgentStep{step}) != 0 {
		t.Fatal("empty screenshot data counted as a visual observation")
	}
	toolContent, followups := agent.observationMessagesForStep(step)
	if followups != nil {
		t.Fatalf("expected no followup messages for empty screenshot data, got %#v", followups)
	}
	if toolContent != observation {
		t.Fatalf("unexpected compacted observation: %q", toolContent)
	}
}

func TestFunctionAgentRejectsVisualObservationWithoutDimensions(t *testing.T) {
	agent := &FunctionAgent{
		Tools: []langtools.Tool{&stubTool{name: "screenshot", visual: true}},
	}
	observation := `{"width":0,"height":240,"format":"jpeg","size":4,"data":"` +
		base64.StdEncoding.EncodeToString([]byte("image-bytes")) + `"}`
	step := schema.AgentStep{
		Action:      schema.AgentAction{Tool: "screenshot"},
		Observation: observation,
	}

	if agent.countVisualObservations([]schema.AgentStep{step}) != 0 {
		t.Fatal("screenshot with invalid dimensions counted as a visual observation")
	}
	toolContent, followups := agent.observationMessagesForStep(step)
	if followups != nil {
		t.Fatalf("expected no followup messages for invalid screenshot dimensions, got %#v", followups)
	}
	if toolContent != observation {
		t.Fatalf("unexpected compacted observation: %q", toolContent)
	}
}

func TestFunctionAgentUsesSharedActionOutputFlowForScreenshot(t *testing.T) {
	agent := &FunctionAgent{
		Tools: []langtools.Tool{&stubTool{name: "screenshot", visual: true}},
	}
	encoded := base64.StdEncoding.EncodeToString([]byte("img1"))
	observation := `{"action_output":"ok","width":800,"height":600,"format":"jpeg","size":4,"capture_backend":"frame_service","data":"` + encoded + `"}`

	toolContent, followups := agent.observationMessagesForStep(schema.AgentStep{
		Action:      schema.AgentAction{Tool: "screenshot"},
		Observation: observation,
	})

	if toolContent != "ok" {
		t.Fatalf("tool result = %q, want shared action_output", toolContent)
	}
	if len(followups) != 1 || len(followups[0].Parts) != 2 {
		t.Fatalf("visual followups = %#v, want one captioned image", followups)
	}
	imagePart, ok := followups[0].Parts[1].(llms.ImageURLContent)
	if !ok || !strings.Contains(imagePart.URL, encoded) {
		t.Fatalf("follow-up image = %#v, want screenshot data", followups[0].Parts[1])
	}
}

func TestFunctionAgentPostActionScreenshotPreservesActionOutput(t *testing.T) {
	agent := &FunctionAgent{
		Tools: []langtools.Tool{&stubTool{name: "keyboard_tap", visual: true}},
	}
	observation := `{"action_output":"  ok\nwith details  ","screen_changed":false,"screen_stable":true,"stable_wait_ms":250,"width":800,"height":600,"format":"jpeg","size":4,"data":"` +
		base64.StdEncoding.EncodeToString([]byte("img1")) + `"}`
	step := schema.AgentStep{
		Action:      schema.AgentAction{Tool: "keyboard_tap"},
		Observation: observation,
	}

	toolContent, followups := agent.observationMessagesForStep(step)

	if toolContent != "  ok\nwith details  " {
		t.Fatalf("toolContent = %q, want original action output", toolContent)
	}
	if len(followups) != 1 {
		t.Fatalf("expected one followup screenshot message, got %#v", followups)
	}
}

func TestFunctionAgentPostActionScreenshotPreservesEmptyActionOutput(t *testing.T) {
	agent := &FunctionAgent{
		Tools: []langtools.Tool{&stubTool{name: "keyboard_tap", visual: true}},
	}
	observation := `{"action_output":"","width":800,"height":600,"format":"jpeg","size":4,"data":"` +
		base64.StdEncoding.EncodeToString([]byte("img1")) + `"}`

	toolContent, followups := agent.observationMessagesForStep(schema.AgentStep{
		Action:      schema.AgentAction{Tool: "keyboard_tap"},
		Observation: observation,
	})

	if toolContent != "" {
		t.Fatalf("toolContent = %q, want empty original action output", toolContent)
	}
	if len(followups) != 1 {
		t.Fatalf("expected one followup screenshot message, got %#v", followups)
	}
}

func TestFunctionAgentWaitStableScreenDoesNotTreatMotionAsActionResult(t *testing.T) {
	agent := &FunctionAgent{
		Tools: []langtools.Tool{&stubTool{name: "wait_for_stable_screen", visual: true}},
	}
	actionOutput := `{"ok":true,"stable":true,"elapsed_ms":250,"frames_differed":false}`
	observation := fmt.Sprintf(`{"action_output":%q,"ok":true,"stable":true,"elapsed_ms":250,"frames_differed":false,"width":800,"height":600,"format":"jpeg","size":4,"data":"`, actionOutput) +
		base64.StdEncoding.EncodeToString([]byte("img1")) + `"}`
	step := schema.AgentStep{
		Action:      schema.AgentAction{Tool: "wait_for_stable_screen"},
		Observation: observation,
	}

	toolContent, followups := agent.observationMessagesForStep(step)
	if toolContent != actionOutput {
		t.Fatalf("toolContent = %q, want stable-screen action_output %q", toolContent, actionOutput)
	}
	if strings.Contains(toolContent, base64.StdEncoding.EncodeToString([]byte("img1"))) {
		t.Fatalf("toolContent contains screenshot base64: %q", toolContent)
	}
	if len(followups) != 1 {
		t.Fatalf("expected one followup screenshot message, got %#v", followups)
	}
	caption, ok := followups[0].Parts[0].(llms.TextContent)
	if !ok {
		t.Fatalf("state message caption = %T, want text", followups[0].Parts[0])
	}
	if !strings.Contains(caption.Text, "No frame-to-frame screen motion was observed during this wait window") {
		t.Fatalf("state message missing wait-window motion note: %q", caption.Text)
	}
	if strings.Contains(caption.Text, "Do not assume the action succeeded") {
		t.Fatalf("standalone wait state message incorrectly warned about action success: %q", caption.Text)
	}
}

func TestFunctionAgentUsesMarkedTouchGestureScreenshotForModel(t *testing.T) {
	raw := solidJPEG(t, 120, 80, color.RGBA{R: 24, G: 48, B: 72, A: 255})
	marked, err := drawTouchGesturePostMarkers(raw, []touchGesturePostMarkerInfo{{Type: "tap", X: 500, Y: 500}})
	if err != nil {
		t.Fatalf("drawTouchGesturePostMarkers() error = %v", err)
	}
	agent := &FunctionAgent{Tools: []langtools.Tool{&stubTool{name: "touch_gesture", visual: true}}}
	observation := fmt.Sprintf(`{"action_output":"ok","width":120,"height":80,"format":"jpeg","size":%d,"data":"%s"}`, len(marked), base64.StdEncoding.EncodeToString(marked))
	step := schema.AgentStep{Action: schema.AgentAction{Tool: "touch_gesture"}, Observation: observation}

	visual, ok := agent.visualScreenshotObservation(step)
	if !ok {
		t.Fatal("touch gesture screenshot was not recognized as visual")
	}
	if !bytes.Equal(visual.ImageBytes, marked) {
		t.Fatal("model screenshot did not preserve the marked image")
	}

	toolContent, followups := agent.observationMessagesForStep(step)
	if strings.Contains(toolContent, "requested tap coordinate") {
		t.Fatalf("tool content should not carry marker metadata: %s", toolContent)
	}
	if len(followups) != 1 || len(followups[0].Parts) != 2 {
		t.Fatalf("visual followups = %#v", followups)
	}
	imagePart, ok := followups[0].Parts[1].(llms.ImageURLContent)
	if !ok {
		t.Fatalf("image part = %T", followups[0].Parts[1])
	}
	_, display, ok := telemetryDataURL(imagePart.URL)
	if !ok || !bytes.Equal(display, marked) {
		t.Fatal("model followup did not contain the marked screenshot")
	}
}

func solidJPEG(t *testing.T, width, height int, fill color.Color) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, fill)
		}
	}
	var output bytes.Buffer
	if err := jpeg.Encode(&output, img, &jpeg.Options{Quality: 95}); err != nil {
		t.Fatalf("encode jpeg: %v", err)
	}
	return output.Bytes()
}
