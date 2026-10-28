package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/executor"
	"aico-agent/internal/agent/messages"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
)

func TestInteractionsModelUsesNativeRequestShape(t *testing.T) {
	var raw map[string]any
	temperature := 0.7
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/interactions" {
			t.Errorf("path = %q, want /v1/interactions", r.URL.Path)
		}
		if got := r.Header.Get("x-goog-api-key"); got != "gemini-key" {
			t.Errorf("x-goog-api-key = %q", got)
		}
		if got := r.Header.Get("Authorization"); got != "" {
			t.Errorf("unexpected Authorization header %q", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":"ix_1","status":"requires_action","steps":[{"type":"thought","summary":[{"type":"text","text":"check"}]},{"type":"model_output","content":[{"type":"text","text":"I will tap."}]},{"type":"function_call","id":"call_1","name":"tap","arguments":{"x":10}}],"usage":{"total_input_tokens":12,"total_output_tokens":4,"total_thought_tokens":2,"total_tokens":18}}`))
	}))
	defer server.Close()

	model := newInteractionsModel(server.URL+"/v1", "gemini-test", "gemini-key", server.Client(), interactionsModelOptions{reasoningEffort: "low", temperature: &temperature})
	response, err := model.GenerateContent(context.Background(), []llms.MessageContent{
		{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextPart("system")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello"), llms.ImageURLContent{URL: "https://example.test/image.png"}}},
	}, llms.WithMaxTokens(128), llms.WithTools([]llms.Tool{{Type: "function", Function: &llms.FunctionDefinition{Name: "tap", Description: "Tap", Parameters: map[string]any{"type": "object"}}}}))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if raw["store"] != false {
		t.Fatalf("store = %#v, want false", raw["store"])
	}
	if raw["system_instruction"] != "system" {
		t.Fatalf("system_instruction = %#v", raw["system_instruction"])
	}
	input, ok := raw["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("input = %#v", raw["input"])
	}
	inputStep := input[0].(map[string]any)
	if inputStep["type"] != "user_input" {
		t.Fatalf("input step = %#v", inputStep)
	}
	config := raw["generation_config"].(map[string]any)
	// temperature is a documented generation_config field for Interactions, so
	// the configured sampling value stays on the wire. Gemini 3 models resolve to
	// Google's documented default of 1.0 via model_specs.go rather than the
	// global 0.2 fallback; an explicit config value (0.7 here) still wins.
	if config["max_output_tokens"] != float64(128) || config["thinking_level"] != "low" || config["temperature"] != 0.7 {
		t.Fatalf("generation_config = %#v", config)
	}
	choice := response.Choices[0]
	if choice.Content != "I will tap." || choice.ReasoningContent != "check" || len(choice.ToolCalls) != 1 {
		t.Fatalf("choice = %#v", choice)
	}
	if choice.ToolCalls[0].FunctionCall.Arguments != `{"x":10}` {
		t.Fatalf("arguments = %q", choice.ToolCalls[0].FunctionCall.Arguments)
	}
	if response.Choices[0].GenerationInfo["llm_response_id"] != "ix_1" {
		t.Fatalf("generation info = %#v", response.Choices[0].GenerationInfo)
	}
}

func TestInteractionsModelOmitsUnsetTemperature(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":"ix_default","status":"completed","steps":[]}`))
	}))
	defer server.Close()

	model := newInteractionsModel(server.URL+"/v1", "gemini-test", "gemini-key", server.Client(), interactionsModelOptions{})
	if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}},
	}, llms.WithMaxTokens(8)); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	config, ok := raw["generation_config"].(map[string]any)
	if !ok {
		t.Fatalf("generation_config = %#v", raw["generation_config"])
	}
	if _, exists := config["temperature"]; exists {
		t.Fatalf("generation_config unexpectedly includes temperature: %#v", config)
	}
}

func TestInteractionsStatefulSendsOnlyIncrementalToolResult(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"id":"ix_2","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"done"}]}]}`))
	}))
	defer server.Close()

	model := newInteractionsModel(server.URL+"/v1", "gemini-test", "key", server.Client(), interactionsModelOptions{providerManagedContext: true}).(*interactionsModel)
	_, err := model.GenerateContentFromMessageList(context.Background(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleUser, Content: "hello"},
		{Role: messages.MessageRoleToolCall, Content: "", ToolCalls: []messages.ToolCall{{ID: "call_1", Name: "tap", Arguments: `{"x":10}`}}, ResponsesResponseID: "ix_1", InteractionsSteps: []json.RawMessage{json.RawMessage(`{"type":"function_call","id":"call_1","name":"tap","arguments":{"x":10}}`)}},
		{Role: messages.MessageRoleToolResult, ToolResults: []messages.ToolResult{{ToolCallID: "call_1", Name: "tap", Content: "ok"}}},
	})
	if err != nil {
		t.Fatalf("GenerateContentFromMessageList: %v", err)
	}
	if raw["store"] != true || raw["previous_interaction_id"] != "ix_1" {
		t.Fatalf("state fields = %#v", raw)
	}
	input, ok := raw["input"].([]any)
	if !ok || len(input) != 1 {
		t.Fatalf("incremental input = %#v", raw["input"])
	}
	step := input[0].(map[string]any)
	if step["type"] != "function_result" || step["call_id"] != "call_1" || step["result"] != "ok" {
		t.Fatalf("function result step = %#v", step)
	}
}

func TestInteractionsStatefulRetriesExpiredIDWithInstructions(t *testing.T) {
	type request struct {
		SystemInstruction     string           `json:"system_instruction"`
		PreviousInteractionID string           `json:"previous_interaction_id"`
		Input                 []map[string]any `json:"input"`
		Store                 bool             `json:"store"`
	}
	var requests []request
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var raw request
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests = append(requests, raw)
		if raw.PreviousInteractionID != "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"previous_interaction_not_found","message":"previous_interaction_id expired"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"ix_recovered","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"ok"}]}]}`))
	}))
	defer server.Close()

	model := newInteractionsModel(server.URL+"/v1", "gemini-test", "key", server.Client(), interactionsModelOptions{providerManagedContext: true}).(*interactionsModel)
	_, err := model.GenerateContentFromMessageList(context.Background(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system guidance"},
		{Role: messages.MessageRoleUser, Content: "first"},
		{Role: messages.MessageRoleAssistant, Content: "answer", ResponsesResponseID: "ix_expired", InteractionsSteps: []json.RawMessage{json.RawMessage(`{"type":"model_output","content":[{"type":"text","text":"answer"}]}`)}},
		{Role: messages.MessageRoleUser, Content: "second"},
	})
	if err != nil {
		t.Fatalf("GenerateContentFromMessageList: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if requests[0].PreviousInteractionID != "ix_expired" || len(requests[0].Input) != 1 {
		t.Fatalf("incremental request = %#v", requests[0])
	}
	if requests[1].PreviousInteractionID != "" || !requests[1].Store || requests[1].SystemInstruction != "system guidance" || len(requests[1].Input) != 3 {
		t.Fatalf("fallback request = %#v", requests[1])
	}
}

func TestInteractionsLocalReplayPreservesOpaqueStepFields(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"id":"ix_3","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"ok"}]}]}`))
	}))
	defer server.Close()
	model := newInteractionsModel(server.URL+"/v1", "gemini-test", "key", server.Client(), interactionsModelOptions{}).(*interactionsModel)
	_, err := model.GenerateContentFromMessageList(context.Background(), []messages.Message{
		{Role: messages.MessageRoleUser, Content: "hello"},
		{Role: messages.MessageRoleAssistant, Content: "thinking", InteractionsSteps: []json.RawMessage{json.RawMessage(`{"type":"thought","signature":"opaque_sig","summary":[{"type":"text","text":"thinking"}]}`), json.RawMessage(`{"type":"model_output","content":[{"type":"text","text":"answer"}],"vendor_field":"keep-me"}`)}},
	})
	if err != nil {
		t.Fatalf("GenerateContentFromMessageList: %v", err)
	}
	input := raw["input"].([]any)
	if len(input) != 3 {
		t.Fatalf("input = %#v", input)
	}
	thought := input[1].(map[string]any)
	if thought["signature"] != "opaque_sig" {
		t.Fatalf("thought signature was dropped: %#v", thought)
	}
	modelOutput := input[2].(map[string]any)
	if modelOutput["vendor_field"] != "keep-me" {
		t.Fatalf("opaque model output field was dropped: %#v", modelOutput)
	}
}

func TestInteractionsModelStreamsTextAndFunctionArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"event_type":"interaction.created","interaction":{"id":"ix_stream","status":"in_progress"}}`,
			`data: {"event_type":"step.start","index":0,"step":{"type":"model_output"}}`,
			`data: {"event_type":"step.delta","index":0,"delta":{"type":"text","text":"hello"}}`,
			`data: {"event_type":"step.start","index":1,"step":{"type":"function_call","id":"call_1","name":"tap"}}`,
			`data: {"event_type":"step.delta","index":1,"delta":{"type":"arguments_delta","arguments":"{\"x\":10}"}}`,
			`data: {"event_type":"interaction.status_update","interaction_id":"ix_stream","status":"requires_action"}`,
			`data: [DONE]`, ""}, "\n")))
	}))
	defer server.Close()
	var streamed string
	model := newInteractionsModel(server.URL+"/v1", "gemini-test", "key", server.Client(), interactionsModelOptions{})
	response, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("go")}}}, llms.WithStreamingFunc(func(_ context.Context, chunk []byte) error { streamed += string(chunk); return nil }))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if streamed != "hello" || response.Choices[0].Content != "hello" {
		t.Fatalf("streamed=%q response=%q", streamed, response.Choices[0].Content)
	}
	want := []llms.ToolCall{{ID: "call_1", Type: "function", FunctionCall: &llms.FunctionCall{Name: "tap", Arguments: `{"x":10}`}}}
	if !reflect.DeepEqual(response.Choices[0].ToolCalls, want) {
		t.Fatalf("tool calls = %#v, want %#v", response.Choices[0].ToolCalls, want)
	}
}

// A stream that stops before the provider's terminal event is only a prefix of
// the interaction. Reporting it as a finished response would let a function_call
// cut mid-arguments reach the tools with placeholder `{}` arguments.
func TestInteractionsModelRejectsStreamWithoutTerminalEvent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"event_type":"interaction.created","interaction":{"id":"ix_cut","status":"in_progress"}}`,
			`data: {"event_type":"step.start","index":0,"step":{"type":"function_call","id":"call_1","name":"tap"}}`,
			`data: {"event_type":"step.delta","index":0,"delta":{"type":"arguments_delta","arguments":"{\"x\":"}}`,
			""}, "\n")))
	}))
	defer server.Close()

	model := newInteractionsModel(server.URL, "gemini-test", "key", server.Client(), interactionsModelOptions{})
	response, err := model.GenerateContent(context.Background(),
		[]llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("go")}}},
		llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }))
	if !errors.Is(err, errInteractionsStreamIncomplete) {
		t.Fatalf("GenerateContent error = %v, want %v", err, errInteractionsStreamIncomplete)
	}
	if response != nil {
		t.Fatalf("GenerateContent returned a response for an incomplete stream: %#v", response.Choices[0])
	}
}

// The terminal event is what proves a stream finished, so a provider that closes
// the body without the trailing [DONE] marker is still a complete turn.
func TestInteractionsModelAcceptsTerminalEventWithoutDoneMarker(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"event_type":"interaction.created","interaction":{"id":"ix_ok","status":"in_progress"}}`,
			`data: {"event_type":"step.start","index":0,"step":{"type":"model_output"}}`,
			`data: {"event_type":"step.delta","index":0,"delta":{"type":"text","text":"done"}}`,
			`data: {"event_type":"interaction.completed","interaction":{"id":"ix_ok","status":"completed"}}`,
			""}, "\n")))
	}))
	defer server.Close()

	model := newInteractionsModel(server.URL, "gemini-test", "key", server.Client(), interactionsModelOptions{})
	response, err := model.GenerateContent(context.Background(),
		[]llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("go")}}},
		llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if response.Choices[0].Content != "done" || response.Choices[0].StopReason != "completed" {
		t.Fatalf("choice = %#v", response.Choices[0])
	}
}

func TestInteractionsModelStreamsAndPreservesThoughtSignature(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"event_type":"interaction.created","interaction":{"id":"ix_stream","status":"in_progress"}}`,
			`data: {"event_type":"step.start","index":0,"step":{"type":"thought"}}`,
			`data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","content":{"type":"text","text":"checking"}}}`,
			`data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"opaque_sig"}}`,
			`data: {"event_type":"interaction.completed","interaction":{"id":"ix_stream","status":"completed"}}`,
			`data: [DONE]`, ""}, "\n")))
	}))
	defer server.Close()

	model := newInteractionsModel(server.URL+"/v1", "gemini-test", "key", server.Client(), interactionsModelOptions{})
	response, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("go")}}}, llms.WithStreamingReasoningFunc(func(_ context.Context, _, _ []byte) error { return nil }))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if response.Choices[0].ReasoningContent != "checking" {
		t.Fatalf("reasoning = %q", response.Choices[0].ReasoningContent)
	}
	config := raw["generation_config"].(map[string]any)
	if config["thinking_summaries"] != "auto" {
		t.Fatalf("generation_config = %#v, want thinking_summaries=auto", config)
	}
	steps, ok := response.Choices[0].GenerationInfo["interactions_steps"].([]json.RawMessage)
	if !ok || len(steps) != 1 {
		t.Fatalf("interactions steps = %#v", response.Choices[0].GenerationInfo["interactions_steps"])
	}
	var thought map[string]any
	if err := json.Unmarshal(steps[0], &thought); err != nil {
		t.Fatalf("decode thought step: %v", err)
	}
	if thought["signature"] != "opaque_sig" {
		t.Fatalf("thought signature = %#v", thought)
	}
}

// Gemini can answer one turn with several function calls and has no switch to
// turn that off. The agent loop runs one call per iteration, so the turn keeps
// the provider's whole step list and the tool result answers the calls that were
// not run: nothing is dropped from the record, and a stored interaction is never
// left with a function_call that has no function_result.
func TestInteractionsStatefulAnswersEveryCallOfTheTurn(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests = append(requests, payload)
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{"id":"ix_two","status":"requires_action","steps":[` +
				`{"type":"thought","signature":"sig_1","summary":[{"type":"text","text":"do both"}]},` +
				`{"type":"function_call","id":"call_1","name":"first","arguments":{"x":1}},` +
				`{"type":"function_call","id":"call_2","name":"second","arguments":{"y":2}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"ix_after","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"done"}]}]}`))
	}))
	defer server.Close()

	manager := NewModelManager(ModelConfig{
		Provider: "gemini", Model: "gemini-3.8-flash", APIKey: "test-key",
		BaseURL: server.URL, APIMode: "interactions_stateful",
	}, ProxyConfig{})
	contextManager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system guidance"},
		{Role: messages.MessageRoleUser, Content: "run the first check, then the second"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList: %v", err)
	}
	llmExecutor := executor.NewLLMExecutor(manager, contextManager)
	tools := []llms.Tool{
		{Type: "function", Function: &llms.FunctionDefinition{Name: "first"}},
		{Type: "function", Function: &llms.FunctionDefinition{Name: "second"}},
	}

	_, response, err := llmExecutor.Generate(context.Background(), llms.WithTools(tools))
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if len(response.Choices[0].ToolCalls) != 2 {
		t.Fatalf("first response tool calls = %#v", response.Choices[0].ToolCalls)
	}

	// What the agent loop does with a turn that carries several calls.
	executed := response.Choices[0].ToolCalls[0]
	selected := choiceWithOnlyToolCall(*response.Choices[0], executed.ID)
	toolCallMessage := messages.ConvertChoiceToContextManagerMessage(selected)
	if len(toolCallMessage.InteractionsSteps) != 3 || len(toolCallMessage.ToolCalls) != 2 {
		t.Fatalf("stored turn = %#v, want the provider's steps and both calls", toolCallMessage)
	}
	if err := appendToolExecutionMessages(llmExecutor, nil, toolCallMessage, schema.AgentStep{
		Action: schema.AgentAction{ToolID: executed.ID, Tool: "first"},
	}, PreparedToolResult{Content: "ok", Complete: true}); err != nil {
		t.Fatalf("appendToolExecutionMessages: %v", err)
	}
	stored := contextManager.CloneMessageList()
	resultMessage := stored[len(stored)-1]
	if resultMessage.Role != messages.MessageRoleToolResult || len(resultMessage.ToolResults) != 2 {
		t.Fatalf("stored tool result = %#v, want a result for both calls", resultMessage)
	}
	if resultMessage.ToolResults[1].ToolCallID != "call_2" || resultMessage.ToolResults[1].Content != unexecutedInteractionToolResult {
		t.Fatalf("unexecuted call result = %#v", resultMessage.ToolResults[1])
	}

	if _, _, err := llmExecutor.Generate(context.Background(), llms.WithTools(tools)); err != nil {
		t.Fatalf("second Generate: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	second := requests[1]
	if second["previous_interaction_id"] != "ix_two" {
		t.Fatalf("second request previous_interaction_id = %#v", second["previous_interaction_id"])
	}
	input, ok := second["input"].([]any)
	if !ok || len(input) != 2 {
		t.Fatalf("second request input = %#v, want one function_result per call", second["input"])
	}
	answered := map[string]string{}
	for _, rawStep := range input {
		step := rawStep.(map[string]any)
		if step["type"] != "function_result" {
			t.Fatalf("second request step = %#v, want function_result", step)
		}
		answered[step["call_id"].(string)] = step["name"].(string)
	}
	if answered["call_1"] != "first" || answered["call_2"] != "second" {
		t.Fatalf("second request answered calls = %#v", answered)
	}
}

// Gemini requires every thought block to be replayed exactly as received, so a
// stream carrying several thought steps must keep each step's own summary and
// its own signature rather than collapsing them into the first one.
func TestInteractionsModelKeepsPerStepThoughtSignatures(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"event_type":"step.start","index":0,"step":{"type":"thought"}}`,
			`data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_summary","content":{"type":"text","text":"first"}}}`,
			`data: {"event_type":"step.delta","index":0,"delta":{"type":"thought_signature","signature":"sig_one"}}`,
			`data: {"event_type":"step.start","index":1,"step":{"type":"model_output"}}`,
			`data: {"event_type":"step.delta","index":1,"delta":{"type":"text","text":"answer"}}`,
			`data: {"event_type":"step.start","index":2,"step":{"type":"thought"}}`,
			`data: {"event_type":"step.delta","index":2,"delta":{"type":"thought_summary","content":{"type":"text","text":"second"}}}`,
			`data: {"event_type":"step.delta","index":2,"delta":{"type":"thought_signature","signature":"sig_two"}}`,
			`data: {"event_type":"interaction.completed","interaction":{"id":"ix_multi","status":"completed"}}`,
			`data: [DONE]`, ""}, "\n")))
	}))
	defer server.Close()

	model := newInteractionsModel(server.URL, "gemini-test", "key", server.Client(), interactionsModelOptions{})
	response, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("go")}}},
		llms.WithStreamingReasoningFunc(func(_ context.Context, _, _ []byte) error { return nil }))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	steps, ok := response.Choices[0].GenerationInfo["interactions_steps"].([]json.RawMessage)
	if !ok || len(steps) != 3 {
		t.Fatalf("interactions steps = %#v", response.Choices[0].GenerationInfo["interactions_steps"])
	}
	for i, want := range []struct {
		stepType  string
		text      string
		signature string
	}{
		{stepType: "thought", text: "first", signature: "sig_one"},
		{stepType: "model_output", text: "answer"},
		{stepType: "thought", text: "second", signature: "sig_two"},
	} {
		var decoded struct {
			Type      string               `json:"type"`
			Content   []interactionContent `json:"content"`
			Summary   []interactionContent `json:"summary"`
			Signature string               `json:"signature"`
		}
		if err := json.Unmarshal(steps[i], &decoded); err != nil {
			t.Fatalf("decode step %d: %v", i, err)
		}
		if decoded.Type != want.stepType || decoded.Signature != want.signature {
			t.Fatalf("step %d = %s", i, steps[i])
		}
		parts := decoded.Summary
		if want.stepType == "model_output" {
			parts = decoded.Content
		}
		if len(parts) != 1 || parts[0].Text != want.text {
			t.Fatalf("step %d text = %s, want %q", i, steps[i], want.text)
		}
	}
}

// When the completed event carries the provider's own steps they are already
// final, so they must be replayed byte-for-byte including fields this client
// does not model.
func TestInteractionsModelReplaysCompletedStepsVerbatim(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			`data: {"event_type":"step.start","index":0,"step":{"type":"model_output"}}`,
			`data: {"event_type":"step.delta","index":0,"delta":{"type":"text","text":"partial"}}`,
			`data: {"event_type":"interaction.completed","interaction":{"id":"ix_done","status":"completed","steps":[{"type":"thought","signature":"final_sig","summary":[{"type":"text","text":"thinking"}],"vendor_field":"keep-me"},{"type":"model_output","content":[{"type":"text","text":"partial"}]}]}}`,
			`data: [DONE]`, ""}, "\n")))
	}))
	defer server.Close()

	model := newInteractionsModel(server.URL, "gemini-test", "key", server.Client(), interactionsModelOptions{})
	response, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("go")}}},
		llms.WithStreamingFunc(func(_ context.Context, _ []byte) error { return nil }))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	steps, ok := response.Choices[0].GenerationInfo["interactions_steps"].([]json.RawMessage)
	if !ok || len(steps) != 2 {
		t.Fatalf("interactions steps = %#v", response.Choices[0].GenerationInfo["interactions_steps"])
	}
	var thought map[string]any
	if err := json.Unmarshal(steps[0], &thought); err != nil {
		t.Fatalf("decode thought step: %v", err)
	}
	if thought["signature"] != "final_sig" || thought["vendor_field"] != "keep-me" {
		t.Fatalf("completed thought step was rewritten: %s", steps[0])
	}
}

func TestNormalizeInteractionsThinkingLevelDoesNotAliasDisabledThinking(t *testing.T) {
	if got := normalizeInteractionsThinkingLevel("none"); got != "" {
		t.Fatalf("normalizeInteractionsThinkingLevel(none) = %q, want empty", got)
	}
}

func TestInteractionsModelReturnsTerminalFailureWithoutDiagnostics(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"ix_failed","status":"budget_exceeded"}`))
	}))
	defer server.Close()

	model := newInteractionsModel(server.URL, "gemini-test", "key", server.Client(), interactionsModelOptions{})
	_, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("go")}}})
	if err == nil || !strings.Contains(err.Error(), "budget_exceeded") {
		t.Fatalf("GenerateContent error = %v, want budget_exceeded", err)
	}
}

// The Interactions API models image, audio, video, and document blocks as
// separate step types, so an inline attachment must be labeled by its MIME type
// instead of being forced into the image or audio shape.
func TestInteractionsBinaryContentMapsMIMETypeToContentType(t *testing.T) {
	content, calls, err := interactionParts([]llms.ContentPart{
		llms.BinaryContent{MIMEType: "image/png", Data: []byte("png")},
		llms.BinaryContent{MIMEType: "audio/wav", Data: []byte("wav")},
		llms.BinaryContent{MIMEType: "video/mp4", Data: []byte("mp4")},
		llms.BinaryContent{MIMEType: "application/pdf", Data: []byte("%PDF")},
		llms.BinaryContent{MIMEType: "text/csv", Data: []byte("a,b")},
	})
	if err != nil {
		t.Fatalf("interactionParts: %v", err)
	}
	if len(calls) != 0 {
		t.Fatalf("binary parts produced tool calls: %#v", calls)
	}
	want := []string{"image", "audio", "video", "document", "document"}
	if len(content) != len(want) {
		t.Fatalf("content = %#v, want %d blocks", content, len(want))
	}
	for i, kind := range want {
		if content[i].Type != kind {
			t.Fatalf("content[%d].Type = %q, want %q", i, content[i].Type, kind)
		}
		if content[i].MIMEType == "" || content[i].Data == "" {
			t.Fatalf("content[%d] lost its inline payload: %#v", i, content[i])
		}
	}
	if _, _, err := interactionParts([]llms.ContentPart{llms.BinaryContent{MIMEType: "text/plain", Data: []byte("hi")}}); err == nil {
		t.Fatal("unsupported binary MIME type was accepted instead of reported")
	}
}

// The agent loop persists a native Gemini tool call as a transcript message, so
// the next local request has to replay the provider's own thought and
// function-call steps. Gemini rejects a replayed function call whose thought
// signature was dropped, so this covers the whole executor path rather than the
// transport alone.
func TestInteractionsExecutorReplaysStoredThoughtSteps(t *testing.T) {
	var requests []map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests = append(requests, payload)
		if len(requests) == 1 {
			_, _ = w.Write([]byte(`{"id":"ix_tool","status":"requires_action","steps":[` +
				`{"type":"thought","signature":"sig_1","summary":[{"type":"text","text":"tap the button"}]},` +
				`{"type":"function_call","id":"call_1","name":"tap","arguments":{"x":10}}]}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"ix_done","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"done"}]}]}`))
	}))
	defer server.Close()

	manager := NewModelManager(ModelConfig{
		Provider: "gemini", Model: "gemini-3.8-flash", APIKey: "test-key",
		BaseURL: server.URL, APIMode: "interactions",
	}, ProxyConfig{})
	contextManager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system guidance"},
		{Role: messages.MessageRoleUser, Content: "tap the button"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList: %v", err)
	}
	llmExecutor := executor.NewLLMExecutor(manager, contextManager)
	tools := []llms.Tool{{Type: "function", Function: &llms.FunctionDefinition{Name: "tap"}}}

	stored, response, err := llmExecutor.Generate(context.Background(), llms.WithTools(tools))
	if err != nil {
		t.Fatalf("first Generate: %v", err)
	}
	if len(response.Choices[0].ToolCalls) != 1 || response.Choices[0].ToolCalls[0].ID != "call_1" {
		t.Fatalf("first response = %#v", response.Choices[0])
	}
	if len(stored.InteractionsSteps) != 2 || !strings.Contains(string(stored.InteractionsSteps[0]), "sig_1") {
		t.Fatalf("stored transcript steps = %#v", stored.InteractionsSteps)
	}
	if err := llmExecutor.AppendMessage(messages.Message{
		Role:        messages.MessageRoleToolResult,
		ToolResults: []messages.ToolResult{{ToolCallID: "call_1", Name: "tap", Content: "ok"}},
	}); err != nil {
		t.Fatalf("append tool result: %v", err)
	}
	if _, _, err := llmExecutor.Generate(context.Background(), llms.WithTools(tools)); err != nil {
		t.Fatalf("second Generate: %v", err)
	}

	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	second := requests[1]
	if second["store"] != false {
		t.Fatalf("second request store = %#v, want false", second["store"])
	}
	if _, ok := second["previous_interaction_id"]; ok {
		t.Fatalf("local mode chained provider state: %#v", second["previous_interaction_id"])
	}
	if _, ok := second["tools"]; !ok {
		t.Fatal("second request dropped the tool declarations")
	}
	replayed, err := json.Marshal(second["input"])
	if err != nil {
		t.Fatalf("marshal replayed input: %v", err)
	}
	for _, want := range []string{`"type":"thought"`, `"signature":"sig_1"`, `"type":"function_call"`, `"arguments":{"x":10}`, `"type":"function_result"`, `"call_id":"call_1"`} {
		if !strings.Contains(string(replayed), want) {
			t.Fatalf("second request lost %s: %s", want, replayed)
		}
	}
}

// A transcript can hold tool exchanges Gemini never produced: turns that ran on
// another provider, or turns stored before this transport existed. Their function
// calls carry no thought signature, and the API rejects a replayed call without
// one, so local replay has to carry the exchange as text while still replaying the
// native steps of turns that do have them.
func TestInteractionsLocalReplayCarriesUnsignedToolExchangesAsText(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_, _ = w.Write([]byte(`{"id":"ix_4","status":"completed","steps":[{"type":"model_output","content":[{"type":"text","text":"done"}]}]}`))
	}))
	defer server.Close()

	model := newInteractionsModel(server.URL, "gemini-test", "key", server.Client(), interactionsModelOptions{}).(*interactionsModel)
	_, err := model.GenerateContentFromMessageList(context.Background(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleUser, Content: "first"},
		{
			Role:      messages.MessageRoleToolCall,
			ToolCalls: []messages.ToolCall{{ID: "legacy_call", Name: "shell", Arguments: `{"command":"date"}`}},
		},
		{Role: messages.MessageRoleToolResult, ToolResults: []messages.ToolResult{{ToolCallID: "legacy_call", Name: "shell", Content: "Thu Sep 17 06:00 UTC 2026"}}},
		{
			Role:      messages.MessageRoleToolCall,
			ToolCalls: []messages.ToolCall{{ID: "native_call", Name: "tap", Arguments: `{"x":1}`}},
			InteractionsSteps: []json.RawMessage{
				json.RawMessage(`{"type":"thought","signature":"sig_native","summary":[{"type":"text","text":"tap it"}]}`),
				json.RawMessage(`{"type":"function_call","id":"native_call","name":"tap","arguments":{"x":1}}`),
			},
		},
		{Role: messages.MessageRoleToolResult, ToolResults: []messages.ToolResult{{ToolCallID: "native_call", Name: "tap", Content: "ok"}}},
		{Role: messages.MessageRoleUser, Content: "second"},
	})
	if err != nil {
		t.Fatalf("GenerateContentFromMessageList: %v", err)
	}
	input := raw["input"].([]any)
	kinds := make([]string, 0, len(input))
	for _, step := range input {
		kinds = append(kinds, step.(map[string]any)["type"].(string))
	}
	want := []string{"user_input", "model_output", "user_input", "thought", "function_call", "function_result", "user_input"}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("input steps = %#v, want %#v", kinds, want)
	}
	legacyCall := input[1].(map[string]any)
	if text := legacyCall["content"].([]any)[0].(map[string]any)["text"]; text != `Called shell({"command":"date"})` {
		t.Fatalf("legacy call step = %#v", legacyCall)
	}
	legacyResult := input[2].(map[string]any)
	if text := legacyResult["content"].([]any)[0].(map[string]any)["text"]; text != "Tool results: shell -> Thu Sep 17 06:00 UTC 2026" {
		t.Fatalf("legacy result step = %#v", legacyResult)
	}
	nativeCall := input[4].(map[string]any)
	if nativeCall["id"] != "native_call" {
		t.Fatalf("stored native call was rewritten: %#v", nativeCall)
	}
}

// The API describes streamed function-call arguments twice in different shapes:
// step.start carries an `arguments` JSON object, in practice the placeholder
// `{}`, and the arguments_delta events that follow carry the real payload as
// JSON text. Appending both yields `{}{"command":"date"}`, which no tool can
// parse, so the deltas have to replace the placeholder rather than extend it.
// The event bytes below are taken from a real device stream.
func TestInteractionsStreamedArgumentsDoNotAppendToPlaceholder(t *testing.T) {
	const stream = `event: step.start
data: {"index":0,"step":{"id":"call_373117","type":"function_call","name":"shell","arguments":{}},"event_type":"step.start"}

event: step.delta
data: {"index":0,"delta":{"arguments":"{\"command\":\"date\"}","type":"arguments_delta"},"event_type":"step.delta"}

event: step.stop
data: {"index":0,"event_type":"step.stop"}

event: interaction.completed
data: {"interaction":{"id":"ix_args","status":"requires_action"},"event_type":"interaction.completed"}

data: [DONE]

`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(stream))
	}))
	defer server.Close()

	model := newInteractionsModel(server.URL, "gemini-test", "key", server.Client(), interactionsModelOptions{})
	response, err := model.GenerateContent(context.Background(),
		[]llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("go")}}},
		llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	calls := response.Choices[0].ToolCalls
	if len(calls) != 1 {
		t.Fatalf("tool call count = %d, want 1", len(calls))
	}
	if got := calls[0].FunctionCall.Arguments; got != `{"command":"date"}` {
		t.Fatalf("arguments = %q, want %q", got, `{"command":"date"}`)
	}
	// The replayed step must carry the same arguments: a malformed value here
	// would be sent back to the provider on the next turn.
	steps, ok := response.Choices[0].GenerationInfo["interactions_steps"].([]json.RawMessage)
	if !ok || len(steps) != 1 {
		t.Fatalf("interactions steps = %#v", response.Choices[0].GenerationInfo["interactions_steps"])
	}
	var replayed map[string]any
	if err := json.Unmarshal(steps[0], &replayed); err != nil {
		t.Fatalf("decode replayed step: %v", err)
	}
	arguments, err := json.Marshal(replayed["arguments"])
	if err != nil {
		t.Fatalf("marshal replayed arguments: %v", err)
	}
	if string(arguments) != `{"command":"date"}` {
		t.Fatalf("replayed arguments = %s", arguments)
	}
}
