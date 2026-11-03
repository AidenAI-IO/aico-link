package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/executor"
	"aico-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/llms"
)

func TestResponsesModelUsesLocalContextAndStoreFalse(t *testing.T) {
	var captured struct {
		Model             string               `json:"model"`
		Input             []responsesInputItem `json:"input"`
		Tools             []responsesTool      `json:"tools"`
		ParallelToolCalls bool                 `json:"parallel_tool_calls"`
		Store             bool                 `json:"store"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q, want /v1/responses", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]},{"type":"function_call","call_id":"call_1","name":"tap","arguments":"{\"x\":10}"}],"usage":{"input_tokens":12,"output_tokens":4,"total_tokens":16,"input_tokens_details":{"cached_tokens":3}}}`))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL+"/v1", "test-model", "token", server.Client(), responsesModelOptions{})
	resp, err := model.GenerateContent(context.Background(), []llms.MessageContent{
		{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextPart("system")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}},
		{Role: llms.ChatMessageTypeAI, Parts: []llms.ContentPart{llms.TextPart("thinking"), llms.ToolCall{ID: "old_call", Type: "function", FunctionCall: &llms.FunctionCall{Name: "screenshot", Arguments: "{}"}}}},
		{Role: llms.ChatMessageTypeTool, Parts: []llms.ContentPart{llms.ToolCallResponse{ToolCallID: "old_call", Name: "screenshot", Content: "image ready"}}},
	}, llms.WithTools([]llms.Tool{{Type: "function", Function: &llms.FunctionDefinition{Name: "tap", Parameters: map[string]any{"type": "object"}}}}))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if captured.Model != "test-model" || captured.ParallelToolCalls {
		t.Fatalf("request metadata = %#v", captured)
	}
	if len(captured.Input) != 5 {
		t.Fatalf("input item count = %d, want 5: %#v", len(captured.Input), captured.Input)
	}
	if captured.Input[3].Type != "function_call" || captured.Input[4].Type != "function_call_output" {
		t.Fatalf("tool items = %#v", captured.Input[3:5])
	}
	if len(captured.Tools) != 1 || captured.Tools[0].Name != "tap" || captured.Tools[0].Type != "function" {
		t.Fatalf("tools = %#v", captured.Tools)
	}
	if len(resp.Choices) != 1 || resp.Choices[0].Content != "done" || len(resp.Choices[0].ToolCalls) != 1 {
		t.Fatalf("response = %#v", resp)
	}
	if got := resp.Choices[0].ToolCalls[0].FunctionCall.Arguments; got != `{"x":10}` {
		t.Fatalf("tool arguments = %q", got)
	}
	if got := resp.Choices[0].GenerationInfo["cached_tokens"]; got != 3 {
		t.Fatalf("cached tokens = %#v", got)
	}
}

func TestResponsesModelSendsStoreFalse(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[]}`))
	}))
	defer server.Close()
	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{}).(*responsesModel)
	if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}}); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	store, ok := raw["store"].(bool)
	if !ok || store {
		t.Fatalf("store = %#v, want explicit false", raw["store"])
	}
}

func TestResponsesModelSendsContextControls(t *testing.T) {
	var captured struct {
		ContextManagement []responsesContextManagement `json:"context_management"`
		Include           []string                     `json:"include"`
		Truncation        string                       `json:"truncation"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&captured); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[]}`))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{
		contextManagement: "compaction",
		compactThreshold:  32000,
		truncation:        "auto",
		include:           []string{"reasoning.encrypted_content"},
	})
	if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}}); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if len(captured.ContextManagement) != 1 || captured.ContextManagement[0].Type != "compaction" || captured.ContextManagement[0].CompactThreshold != 32000 {
		t.Fatalf("context_management = %#v", captured.ContextManagement)
	}
	if captured.Truncation != "auto" || len(captured.Include) != 1 || captured.Include[0] != "reasoning.encrypted_content" {
		t.Fatalf("truncation=%q include=%#v", captured.Truncation, captured.Include)
	}
}

func TestResponsesModelUsesVolcengineContextShape(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_ark","status":"completed","output":[]}`))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "doubao-test", "", server.Client(), responsesModelOptions{
		providerManagedContext: true,
		contextManagement:      "compaction",
		compactThreshold:       32000,
		truncation:             "auto",
		include:                []string{"reasoning.encrypted_content"},
		reasoningEffort:        "low",
		dialect:                responsesDialectVolcengine,
	})
	if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}}); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if store, ok := raw["store"].(bool); !ok || !store {
		t.Fatalf("store = %#v, want true", raw["store"])
	}
	if _, exists := raw["context_management"]; exists {
		t.Fatalf("Volcengine request unexpectedly contains context_management: %#v", raw)
	}
	if _, exists := raw["truncation"]; exists {
		t.Fatalf("Volcengine request unexpectedly contains truncation: %#v", raw)
	}
	if include, ok := raw["include"].([]any); !ok || len(include) != 1 || include[0] != "reasoning.encrypted_content" {
		t.Fatalf("include = %#v", raw["include"])
	}
	if reasoning, ok := raw["reasoning"].(map[string]any); !ok || reasoning["effort"] != "low" {
		t.Fatalf("reasoning = %#v", raw["reasoning"])
	}
}

func TestResponsesModelSendsVolcengineContextEdits(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_ark","status":"completed","output":[]}`))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "doubao-test", "", server.Client(), responsesModelOptions{
		dialect:                  responsesDialectVolcengine,
		contextManagement:        responsesContextManagementArkEdits,
		contextEditTrigger:       12,
		contextEditKeep:          4,
		contextEditClearThinking: true,
	})
	if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}}); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	management, ok := raw["context_management"].(map[string]any)
	if !ok {
		t.Fatalf("context_management = %#v, want object", raw["context_management"])
	}
	edits, ok := management["edits"].([]any)
	if !ok || len(edits) != 2 {
		t.Fatalf("context_management.edits = %#v, want two edits", management["edits"])
	}
	first := edits[0].(map[string]any)
	if first["type"] != "clear_tool_uses" ||
		first["keep"].(map[string]any)["value"] != float64(4) || first["trigger"].(map[string]any)["value"] != float64(12) {
		t.Fatalf("clear_tool_uses edit = %#v", first)
	}
	second := edits[1].(map[string]any)
	if second["type"] != "clear_thinking" || second["keep"].(map[string]any)["type"] != "thinking_turns" || second["keep"].(map[string]any)["value"] != float64(2) {
		t.Fatalf("clear_thinking edit = %#v", second)
	}
}

func TestResponsesModelUsesOpenRouterContextShape(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_router","status":"completed","output":[]}`))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "openai/test", "", server.Client(), responsesModelOptions{
		contextManagement: "compaction",
		truncation:        "auto",
		dialect:           responsesDialectOpenRouter,
	})
	if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}}); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if _, exists := raw["store"]; exists {
		t.Fatalf("OpenRouter request unexpectedly contains store: %#v", raw)
	}
	if _, exists := raw["previous_response_id"]; exists {
		t.Fatalf("OpenRouter request unexpectedly contains previous_response_id: %#v", raw)
	}
	if _, exists := raw["context_management"]; exists {
		t.Fatalf("OpenRouter request unexpectedly contains context_management: %#v", raw)
	}
	if raw["truncation"] != "auto" {
		t.Fatalf("OpenRouter truncation = %#v, want auto", raw["truncation"])
	}
}

func TestResponsesModelUsesDeepSeekStatelessShape(t *testing.T) {
	var raw map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_deepseek","status":"completed","output":[]}`))
	}))
	defer server.Close()

	temperature := 0.4
	model := newResponsesModel(server.URL, "deepseek-flash", "", server.Client(), responsesModelOptions{
		providerManagedContext: true,
		contextManagement:      "compaction",
		truncation:             "auto",
		include:                []string{"reasoning.encrypted_content"},
		reasoningEffort:        "high",
		temperature:            &temperature,
		ignoreTemperature:      true,
		dialect:                responsesDialectDeepSeek,
	}).(*responsesModel)
	if _, err := model.generateContentWithInput(context.Background(), []responsesInputItem{{Role: "user", Content: "hello"}}, "", "resp_previous", llms.WithTemperature(0.7)); err != nil {
		t.Fatalf("generateContentWithInput: %v", err)
	}
	for _, unsupported := range []string{"store", "previous_response_id", "parallel_tool_calls", "context_management", "truncation", "include", "temperature"} {
		if _, exists := raw[unsupported]; exists {
			t.Fatalf("DeepSeek request unexpectedly contains %s: %#v", unsupported, raw)
		}
	}
	if reasoning, ok := raw["reasoning"].(map[string]any); !ok || reasoning["effort"] != "high" {
		t.Fatalf("reasoning = %#v", raw["reasoning"])
	}
}

func TestAddResponsesOutputItemsDeduplicatesByItemID(t *testing.T) {
	info := make(map[string]any)
	var first responsesOutputItem
	if err := json.Unmarshal([]byte(`{"id":"msg_1","type":"message","role":"assistant","status":"in_progress"}`), &first); err != nil {
		t.Fatalf("unmarshal first item: %v", err)
	}
	var completed responsesOutputItem
	if err := json.Unmarshal([]byte(`{"id":"msg_1","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done"}]}`), &completed); err != nil {
		t.Fatalf("unmarshal completed item: %v", err)
	}
	addResponsesOutputItems(info, []responsesOutputItem{first, completed})
	items, ok := info["responses_output_items"].([]json.RawMessage)
	if !ok || len(items) != 1 {
		t.Fatalf("output items = %#v, want one item", info["responses_output_items"])
	}
	if !strings.Contains(string(items[0]), `"status":"completed"`) || !strings.Contains(string(items[0]), `"text":"done"`) {
		t.Fatalf("output item = %s, want latest completed representation", items[0])
	}
}

func TestResponsesStatefulModeChainsOnlyNewItems(t *testing.T) {
	var requests []struct {
		Instructions       string               `json:"instructions"`
		PreviousResponseID string               `json:"previous_response_id"`
		Input              []responsesInputItem `json:"input"`
		Store              bool                 `json:"store"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Instructions       string               `json:"instructions"`
			PreviousResponseID string               `json:"previous_response_id"`
			Input              []responsesInputItem `json:"input"`
			Store              bool                 `json:"store"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests = append(requests, request)
		id := fmt.Sprintf("resp_%d", len(requests))
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"id":%q,"status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`, id)
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{providerManagedContext: true}).(*responsesModel)
	first := []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system guidance"},
		{Role: messages.MessageRoleUser, Content: "first"},
	}
	response, err := model.GenerateContentFromMessageList(context.Background(), first)
	if err != nil {
		t.Fatalf("first GenerateContent: %v", err)
	}
	assistant := messages.ConvertChoiceToContextManagerMessage(*response.Choices[0])
	if assistant.ResponsesResponseID != "resp_1" {
		t.Fatalf("response id = %q, want resp_1", assistant.ResponsesResponseID)
	}
	second := append(first, assistant, messages.Message{Role: messages.MessageRoleUser, Content: "second"})
	if _, err := model.GenerateContentFromMessageList(context.Background(), second); err != nil {
		t.Fatalf("second GenerateContent: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if !requests[0].Store || requests[0].PreviousResponseID != "" || requests[0].Instructions != "system guidance" {
		t.Fatalf("first request = %#v", requests[0])
	}
	if !requests[1].Store || requests[1].PreviousResponseID != "resp_1" || requests[1].Instructions != "system guidance" {
		t.Fatalf("second request metadata = %#v", requests[1])
	}
	if len(requests[1].Input) != 1 || requests[1].Input[0].Role != "user" || requests[1].Input[0].Content != "second" {
		t.Fatalf("second incremental input = %#v", requests[1].Input)
	}
}

func TestResponsesStatefulModeRetriesExpiredAnchorWithFullLocalContext(t *testing.T) {
	var requests []struct {
		Instructions       string            `json:"instructions"`
		PreviousResponseID string            `json:"previous_response_id"`
		Input              []json.RawMessage `json:"input"`
		Store              bool              `json:"store"`
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Instructions       string            `json:"instructions"`
			PreviousResponseID string            `json:"previous_response_id"`
			Input              []json.RawMessage `json:"input"`
			Store              bool              `json:"store"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		requests = append(requests, request)
		if request.PreviousResponseID != "" {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"error":{"code":"previous_response_not_found","message":"previous_response_id expired"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"resp_recovered","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"ok"}]}]}`))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{providerManagedContext: true}).(*responsesModel)
	contextMessages := []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system guidance"},
		{Role: messages.MessageRoleUser, Content: "first"},
		{
			Role:                messages.MessageRoleAssistant,
			Content:             "answer",
			ResponsesResponseID: "resp_expired",
			ResponsesOutputItems: []json.RawMessage{
				json.RawMessage(`{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"answer"}]}`),
			},
		},
		{Role: messages.MessageRoleUser, Content: "second"},
	}
	if _, err := model.GenerateContentFromMessageList(context.Background(), contextMessages); err != nil {
		t.Fatalf("GenerateContentFromMessageList: %v", err)
	}
	if len(requests) != 2 {
		t.Fatalf("request count = %d, want 2", len(requests))
	}
	if requests[0].PreviousResponseID != "resp_expired" || len(requests[0].Input) != 1 {
		t.Fatalf("incremental request = %#v", requests[0])
	}
	if requests[1].PreviousResponseID != "" || !requests[1].Store || requests[1].Instructions != "system guidance" || len(requests[1].Input) != 3 {
		t.Fatalf("fallback request = %#v", requests[1])
	}
	if !strings.Contains(string(requests[1].Input[1]), `"phase":"final_answer"`) {
		t.Fatalf("fallback assistant item = %s", requests[1].Input[1])
	}
}

func TestResponsesStatefulModeDoesNotRetryUnrelatedError(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_request_error","message":"invalid tool schema"}}`))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{providerManagedContext: true}).(*responsesModel)
	_, err := model.GenerateContentFromMessageList(context.Background(), []messages.Message{
		{Role: messages.MessageRoleAssistant, Content: "answer", ResponsesResponseID: "resp_1"},
		{Role: messages.MessageRoleUser, Content: "second"},
	})
	if err == nil || requestCount != 1 {
		t.Fatalf("error=%v requestCount=%d, want one failed request", err, requestCount)
	}
}

func TestResponsesStatefulModeDoesNotRetryRequestEcho(t *testing.T) {
	requestCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requestCount++
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":{"code":"invalid_tool_schema","message":"invalid tool schema"},"request":{"previous_response_id":"resp_1"}}`))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{providerManagedContext: true}).(*responsesModel)
	_, err := model.GenerateContentFromMessageList(context.Background(), []messages.Message{
		{Role: messages.MessageRoleAssistant, Content: "answer", ResponsesResponseID: "resp_1"},
		{Role: messages.MessageRoleUser, Content: "second"},
	})
	if err == nil || requestCount != 1 {
		t.Fatalf("error=%v requestCount=%d, want one failed request", err, requestCount)
	}
}

func TestProviderManagedContextInputUsesLatestResponseAnchor(t *testing.T) {
	instructions, previousID, input := providerManagedContextInput([]messages.Message{
		{Role: messages.MessageRoleSystem, Content: "one"},
		{Role: messages.MessageRoleSystem, Content: "two"},
		{Role: messages.MessageRoleUser, Content: "old"},
		{Role: messages.MessageRoleAssistant, Content: "answer", ResponsesResponseID: "resp_old"},
		{Role: messages.MessageRoleToolResult, ToolResults: []messages.ToolResult{{ToolCallID: "call", Content: "result"}}},
	})
	if instructions != "one\n\ntwo" || previousID != "resp_old" {
		t.Fatalf("instructions=%q previousID=%q", instructions, previousID)
	}
	if len(input) != 1 || input[0].Role != messages.MessageRoleToolResult {
		t.Fatalf("incremental input = %#v", input)
	}
}

func TestResponsesModelStreamsTextAndFunctionArguments(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !request.Stream {
			t.Errorf("stream = false, want true")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\n",
			"event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n",
			"event: response.output_item.added\ndata: {\"type\":\"response.output_item.added\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"tap\"}}\n",
			"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"{\\\"x\\\":\"}\n",
			"event: response.function_call_arguments.delta\ndata: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"fc_1\",\"delta\":\"10}\"}\n",
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"rs_1\",\"type\":\"reasoning\",\"encrypted_content\":\"opaque\"}}\n",
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"phase\":\"final_answer\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n",
			"event: response.output_item.done\ndata: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"fc_1\",\"type\":\"function_call\",\"call_id\":\"call_1\",\"name\":\"tap\",\"arguments\":\"{\\\"x\\\":10}\"}}\n",
			"event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":3,\"total_tokens\":5}}}\n",
			"\n",
		}, "")))
	}))
	defer server.Close()
	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{})
	var streamed string
	resp, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}}, llms.WithStreamingFunc(func(_ context.Context, chunk []byte) error {
		streamed += string(chunk)
		return nil
	}))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if streamed != "hello" || resp.Choices[0].Content != "hello" {
		t.Fatalf("streamed=%q response=%q", streamed, resp.Choices[0].Content)
	}
	if len(resp.Choices[0].ToolCalls) != 1 || resp.Choices[0].ToolCalls[0].FunctionCall.Name != "tap" {
		t.Fatalf("tool calls = %#v", resp.Choices[0].ToolCalls)
	}
	if got := resp.Choices[0].ToolCalls[0].FunctionCall.Arguments; got != `{"x":10}` {
		t.Fatalf("arguments = %q", got)
	}
	reasoningItems, ok := resp.Choices[0].GenerationInfo["responses_reasoning_items"].([]json.RawMessage)
	if !ok || len(reasoningItems) != 1 || !strings.Contains(string(reasoningItems[0]), `"encrypted_content":"opaque"`) {
		t.Fatalf("streaming reasoning items = %#v", resp.Choices[0].GenerationInfo["responses_reasoning_items"])
	}
	outputItems, ok := resp.Choices[0].GenerationInfo["responses_output_items"].([]json.RawMessage)
	if !ok || len(outputItems) != 3 {
		t.Fatalf("streaming output items = %#v", resp.Choices[0].GenerationInfo["responses_output_items"])
	}
	if got := resp.Choices[0].GenerationInfo["responses_assistant_phase"]; got != "final_answer" {
		t.Fatalf("assistant phase = %#v", got)
	}
}

func TestResponsesModelStreamsOpenRouterContentPartEvents(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(strings.Join([]string{
			"data: {\"type\":\"response.content_part.delta\",\"delta\":\"hel\"}\n",
			"data: {\"type\":\"response.content_part.delta\",\"delta\":\"lo\"}\n",
			"data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n",
			"data: {\"type\":\"response.done\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n",
			"data: [DONE]\n",
			"\n",
		}, "")))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{})
	var streamed string
	resp, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}}, llms.WithStreamingFunc(func(_ context.Context, chunk []byte) error {
		streamed += string(chunk)
		return nil
	}))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if streamed != "hello" || resp.Choices[0].Content != "hello" {
		t.Fatalf("streamed=%q response=%q", streamed, resp.Choices[0].Content)
	}
	if resp.Choices[0].GenerationInfo["llm_response_id"] != "resp_1" {
		t.Fatalf("response id = %#v", resp.Choices[0].GenerationInfo["llm_response_id"])
	}
}

func TestResponsesModelUsesCompletedMessageItemWhenNoTextDeltaArrives(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.output_item.done\",\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"status\":\"completed\",\"content\":[{\"type\":\"output_text\",\"text\":\"fallback\"}]}}\n\ndata: {\"type\":\"response.done\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n"))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{})
	var streamed string
	resp, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}}, llms.WithStreamingFunc(func(_ context.Context, chunk []byte) error {
		streamed += string(chunk)
		return nil
	}))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if streamed != "fallback" || resp.Choices[0].Content != "fallback" {
		t.Fatalf("streamed=%q response=%q", streamed, resp.Choices[0].Content)
	}
}

func TestResponsesModelReportsFailedStreamEvents(t *testing.T) {
	tests := []struct {
		name     string
		payload  string
		wantCode string
		wantText string
	}{
		{
			name:     "response failed",
			payload:  `{"type":"response.failed","response":{"status":"failed","error":{"code":"invalid_request_error","message":"bad input"}}}`,
			wantCode: "invalid_request_error",
			wantText: "bad input",
		},
		{
			name:     "top-level error",
			payload:  `{"type":"error","code":"rate_limit_exceeded","message":"slow down"}`,
			wantCode: "rate_limit_exceeded",
			wantText: "slow down",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = fmt.Fprintf(w, "data: %s\n\n", tt.payload)
			}))
			defer server.Close()

			model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{})
			_, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}}, llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }))
			var providerErr *ProviderHTTPError
			if !errors.As(err, &providerErr) {
				t.Fatalf("GenerateContent error = %T %v, want ProviderHTTPError", err, err)
			}
			if providerErr.ProviderCode != tt.wantCode || providerErr.Message != tt.wantText {
				t.Fatalf("provider error = %#v", providerErr)
			}
		})
	}
}

func TestResponsesModelReportsFailedNonStreamingResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"id":"resp_failed","status":"failed","error":{"code":"server_error","message":"provider failed"},"error_type":"authentication"}`))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{})
	_, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}})
	var providerErr *ProviderHTTPError
	if !errors.As(err, &providerErr) || providerErr.ProviderCode != "authentication" || providerErr.Message != "provider failed" || providerErr.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GenerateContent error = %T %#v, want structured provider failure", err, err)
	}
}

func TestResponsesModelAcceptsIncompleteStreamResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.incomplete\",\"response\":{\"id\":\"resp_partial\",\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"output\":[{\"type\":\"message\",\"role\":\"assistant\",\"content\":[{\"type\":\"output_text\",\"text\":\"partial\"}]}]}}\n\n"))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{})
	resp, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}}, llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if resp.Choices[0].Content != "partial" || resp.Choices[0].StopReason != "incomplete" {
		t.Fatalf("response = %#v", resp.Choices[0])
	}
}

func TestModelAPIModeValidation(t *testing.T) {
	if got := normalizeModelAPIMode(""); got != modelAPIModeChatCompletions {
		t.Fatalf("empty mode = %q", got)
	}
	if got := normalizeModelAPIMode("responses"); got != modelAPIModeResponses {
		t.Fatalf("responses mode = %q", got)
	}
	if got := normalizeModelAPIMode("responses_stateful"); got != modelAPIModeResponsesStateful {
		t.Fatalf("responses_stateful mode = %q", got)
	}
	if got := normalizeModelAPIMode("interactions"); got != modelAPIModeInteractions {
		t.Fatalf("interactions mode = %q", got)
	}
	if got := normalizeModelAPIMode("interactions_stateful"); got != modelAPIModeInteractionsStateful {
		t.Fatalf("interactions_stateful mode = %q", got)
	}
	if err := (Config{Model: ModelConfig{Provider: "gemini", Model: "gemini-3.8-flash", APIMode: "interactions"}}).Validate(); err != nil {
		t.Fatalf("Gemini Interactions Validate() error = %v", err)
	}
	if err := (Config{Model: ModelConfig{Provider: "gemini", Model: "gemini-3.8-flash", APIMode: "interactions_stateful"}}).Validate(); err != nil {
		t.Fatalf("Gemini stateful Interactions Validate() error = %v", err)
	}
	if err := (Config{Model: ModelConfig{Provider: "openai", Model: "gpt-test", APIMode: "interactions"}}).Validate(); err == nil || !strings.Contains(err.Error(), "native Gemini Interactions") {
		t.Fatalf("OpenAI Interactions Validate() error = %v", err)
	}
	cfg := Config{
		ModelProviders: map[string]ModelProvider{"gateway": {Type: "openai", BaseURL: "https://gateway.example.test/v1"}},
		Model:          ModelConfig{Provider: "gateway", Model: "test-model", APIMode: "responses"},
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected a named OpenAI-compatible endpoint: %v", err)
	}
	cfg.Model.APIMode = "responses_stateful"
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected stateful named OpenAI-compatible endpoint: %v", err)
	}
	cfg.ModelProviders["gateway"] = ModelProvider{Type: "openai", BaseURL: "https://openrouter.ai/api/v1"}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "supports stored Responses") {
		t.Fatalf("OpenRouter URL saved as openai Validate() error = %v", err)
	}
	if err := (Config{Model: ModelConfig{Provider: "anthropic", Model: "claude-test", APIMode: "responses"}}).Validate(); err == nil || !strings.Contains(err.Error(), "OpenAI-compatible /responses endpoint") {
		t.Fatalf("Validate() error = %v, want native transport compatibility error", err)
	}
	if err := (Config{Model: ModelConfig{Provider: "openrouter", Model: "openai/gpt-test", APIMode: "responses_stateful"}}).Validate(); err == nil || !strings.Contains(err.Error(), "supports stored Responses") {
		t.Fatalf("OpenRouter stateful Validate() error = %v", err)
	}
	if err := (Config{Model: ModelConfig{Provider: "kimi", Model: "kimi-k3", APIMode: "responses"}}).Validate(); err == nil || !strings.Contains(err.Error(), "OpenAI-compatible /responses endpoint") {
		t.Fatalf("Kimi Responses Validate() error = %v", err)
	}
	cfg.ModelProviders["gateway"] = ModelProvider{Type: "openai", BaseURL: "https://ark.cn-beijing.volces.com/api/v3"}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate() rejected Ark URL saved as generic openai: %v", err)
	}
	for _, tt := range []struct {
		name  string
		model ModelConfig
		field string
	}{
		{name: "context management", model: ModelConfig{Provider: "openai", Model: "test", APIMode: "responses", ResponsesContextManagement: "invalid"}, field: "responses_context_management"},
		{name: "compact threshold negative", model: ModelConfig{Provider: "openai", Model: "test", APIMode: "responses", ResponsesCompactThreshold: -1}, field: "responses_compact_threshold"},
		{name: "compact threshold below provider minimum", model: ModelConfig{Provider: "openai", Model: "test", APIMode: "responses", ResponsesCompactThreshold: 999}, field: "responses_compact_threshold"},
		{name: "ark trigger", model: ModelConfig{Provider: "volcengine", Model: "test", APIMode: "responses", ResponsesContextEditTrigger: -1}, field: "responses_context_edit_trigger"},
		{name: "ark keep", model: ModelConfig{Provider: "volcengine", Model: "test", APIMode: "responses", ResponsesContextEditKeep: -1}, field: "responses_context_edit_keep"},
		{name: "blank include", model: ModelConfig{Provider: "openai", Model: "test", APIMode: "responses", ResponsesInclude: []string{"reasoning.encrypted_content", " "}}, field: "responses_include"},
		{name: "truncation", model: ModelConfig{Provider: "openai", Model: "test", APIMode: "responses", ResponsesTruncation: "invalid"}, field: "responses_truncation"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if err := (Config{Model: tt.model}).Validate(); err == nil || !strings.Contains(err.Error(), tt.field) {
				t.Fatalf("Validate() error = %v, want %s", err, tt.field)
			}
		})
	}
	for _, threshold := range []int{0, 1000, 4096} {
		cfg := Config{Model: ModelConfig{
			Provider:                  "openai",
			Model:                     "test",
			APIMode:                   "responses",
			ResponsesCompactThreshold: threshold,
		}}
		if err := cfg.Validate(); err != nil {
			t.Fatalf("ResponsesCompactThreshold=%d: Validate() error = %v", threshold, err)
		}
	}
	if !(ModelConfig{Provider: "openai", APIMode: "responses_stateful", ResponsesContextManagement: "compaction"}).ResponsesProviderCompactionEnabled() {
		t.Fatal("provider compaction should be enabled for stateful Responses mode")
	}
	if (ModelConfig{Provider: "openai", APIMode: "chat_completions", ResponsesContextManagement: "compaction"}).ResponsesProviderCompactionEnabled() {
		t.Fatal("provider compaction should not affect Chat Completions mode")
	}
	if !(ModelConfig{Provider: "openai", APIMode: "responses", ResponsesContextManagement: "compaction"}).ResponsesProviderCompactionEnabled() {
		t.Fatal("stateless Responses compaction must suppress duplicate local compaction")
	}
	if (ModelConfig{Provider: "openrouter", APIMode: "responses", ResponsesContextManagement: "compaction"}).ResponsesProviderCompactionEnabled() {
		t.Fatal("OpenRouter does not support OpenAI provider compaction")
	}
	if (ModelConfig{Provider: "volcengine", APIMode: "responses_stateful", ResponsesContextManagement: "compaction"}).ResponsesProviderCompactionEnabled() {
		t.Fatal("Volcengine does not support OpenAI provider compaction")
	}
	if (ModelConfig{Provider: "openai", BaseURL: "https://ark.cn-beijing.volces.com/api/v3", APIMode: "responses", ResponsesContextManagement: "compaction"}).ResponsesProviderCompactionEnabled() {
		t.Fatal("Ark URL saved as openai must not suppress local compaction")
	}
	if (ModelConfig{Provider: "volcengine", APIMode: "responses_stateful", ResponsesContextManagement: "ark_context_edit"}).ResponsesProviderCompactionEnabled() {
		t.Fatal("Ark context edits must not suppress local token compaction")
	}
}

func TestConvertResponsesInputPreservesMixedContentOrderAndAudioShape(t *testing.T) {
	input, err := convertResponsesInput([]llms.MessageContent{{
		Role: llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{
			llms.TextPart("before"),
			llms.ImageURLContent{URL: "https://example.test/image.png", Detail: "low"},
			llms.TextPart("between"),
			llms.BinaryPart("audio/wav", []byte("audio")),
			llms.TextPart("after"),
		},
	}})
	if err != nil {
		t.Fatalf("convertResponsesInput: %v", err)
	}
	if len(input) != 1 {
		t.Fatalf("input = %#v, want one message", input)
	}
	parts, ok := input[0].Content.([]responsesInputContent)
	if !ok {
		t.Fatalf("content = %#v, want rich content parts", input[0].Content)
	}
	if len(parts) != 5 {
		t.Fatalf("content parts = %#v, want five", parts)
	}
	if parts[0].Type != "input_text" || parts[0].Text != "before" ||
		parts[1].Type != "input_image" || parts[1].ImageURL != "https://example.test/image.png" || parts[1].Detail != "low" ||
		parts[2].Type != "input_text" || parts[2].Text != "between" ||
		parts[3].Type != "input_audio" || parts[3].InputAudio == nil || parts[3].InputAudio.Data == "" || parts[3].InputAudio.Format != "wav" ||
		parts[4].Type != "input_text" || parts[4].Text != "after" {
		t.Fatalf("content parts = %#v", parts)
	}
}

func TestConvertResponsesInputDoesNotAliasFlushedRichContent(t *testing.T) {
	input, err := convertResponsesInput([]llms.MessageContent{{
		Role: llms.ChatMessageTypeAI,
		Parts: []llms.ContentPart{
			llms.TextPart("before"),
			llms.ImageURLContent{URL: "https://example.test/first.png"},
			llms.ToolCall{ID: "call_1", Type: "function", FunctionCall: &llms.FunctionCall{Name: "tap", Arguments: "{}"}},
			llms.ImageURLContent{URL: "https://example.test/second.png"},
		},
	}})
	if err != nil {
		t.Fatalf("convertResponsesInput: %v", err)
	}
	if len(input) != 3 {
		t.Fatalf("input = %#v, want rich content, tool call, rich content", input)
	}
	first, ok := input[0].Content.([]responsesInputContent)
	if !ok || len(first) != 2 || first[0].Text != "before" || first[1].ImageURL != "https://example.test/first.png" {
		t.Fatalf("first rich item = %#v", input[0])
	}
	if input[1].Type != "function_call" || input[1].CallID != "call_1" {
		t.Fatalf("tool item = %#v", input[1])
	}
	last, ok := input[2].Content.([]responsesInputContent)
	if !ok || len(last) != 1 || last[0].ImageURL != "https://example.test/second.png" {
		t.Fatalf("second rich item = %#v", input[2])
	}
}

func TestResponsesModelStreamsWhenOnlyReasoningCallbackIsSet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Stream bool `json:"stream"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if !request.Stream {
			t.Errorf("stream = false, want true")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.reasoning_text.delta\",\"delta\":\"think\"}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	var reasoning string
	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{})
	if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}}, llms.WithStreamingReasoningFunc(func(_ context.Context, chunk []byte, _ []byte) error {
		reasoning += string(chunk)
		return nil
	})); err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	if reasoning != "think" {
		t.Fatalf("reasoning = %q, want think", reasoning)
	}
}

func TestResponsesModelStreamsLargeReasoningItem(t *testing.T) {
	const scannerLimit = 1024 * 1024
	largeEncryptedContent := strings.Repeat("x", scannerLimit+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"type":"response.output_item.done","item":{"id":"rs_large","type":"reasoning","encrypted_content":"` + largeEncryptedContent + `"}}` + "\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{})
	resp, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}}, llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }))
	if err != nil {
		t.Fatalf("GenerateContent: %v", err)
	}
	reasoningItems, ok := resp.Choices[0].GenerationInfo["responses_reasoning_items"].([]json.RawMessage)
	if !ok || len(reasoningItems) != 1 || !strings.Contains(string(reasoningItems[0]), largeEncryptedContent) {
		t.Fatalf("streaming reasoning items = %#v", resp.Choices[0].GenerationInfo["responses_reasoning_items"])
	}
}

func TestResponsesModelReplaysOpaqueReasoningFromLocalContext(t *testing.T) {
	requestCount := 0
	var secondInput []json.RawMessage
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request struct {
			Input []json.RawMessage `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		if requestCount == 1 {
			secondInput = request.Input
		}
		requestCount++
		_, _ = w.Write([]byte(`{"id":"resp_1","status":"completed","output":[{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"},{"type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"done"}]}]}`))
	}))
	defer server.Close()

	model := newResponsesModel(server.URL, "test-model", "", server.Client(), responsesModelOptions{}).(*responsesModel)
	response, err := model.GenerateContent(context.Background(), []llms.MessageContent{{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("hello")}}})
	if err != nil {
		t.Fatalf("first GenerateContent: %v", err)
	}
	contextMessage := messages.ConvertChoiceToContextManagerMessage(*response.Choices[0])
	if len(contextMessage.ResponsesReasoningItems) != 1 {
		t.Fatalf("stored reasoning items = %#v", contextMessage.ResponsesReasoningItems)
	}
	if len(contextMessage.ResponsesOutputItems) != 2 || contextMessage.ResponsesAssistantPhase != "final_answer" {
		t.Fatalf("stored output metadata = %#v phase=%q", contextMessage.ResponsesOutputItems, contextMessage.ResponsesAssistantPhase)
	}
	if _, err := model.GenerateContentFromMessageList(context.Background(), []messages.Message{contextMessage}); err != nil {
		t.Fatalf("second GenerateContent: %v", err)
	}
	if len(secondInput) != 2 || !strings.Contains(string(secondInput[0]), `"type":"reasoning"`) || !strings.Contains(string(secondInput[0]), `"encrypted_content":"opaque"`) || !strings.Contains(string(secondInput[1]), `"phase":"final_answer"`) {
		t.Fatalf("second input = %s, want complete raw output", secondInput)
	}
}

func TestResponsesContextSurvivesProductionModelWrappers(t *testing.T) {
	tests := []struct {
		name             string
		apiMode          string
		wantStore        bool
		wantPreviousID   string
		wantSecondInputs int
		wantOpaqueReplay bool
	}{
		{
			name:             "stateless replays opaque output",
			apiMode:          "responses",
			wantStore:        false,
			wantSecondInputs: 5,
			wantOpaqueReplay: true,
		},
		{
			name:             "stateful sends only incremental input",
			apiMode:          "responses_stateful",
			wantStore:        true,
			wantPreviousID:   "resp_1",
			wantSecondInputs: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var requests []struct {
				PreviousResponseID string            `json:"previous_response_id"`
				Input              []json.RawMessage `json:"input"`
				Store              bool              `json:"store"`
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				var request struct {
					PreviousResponseID string            `json:"previous_response_id"`
					Input              []json.RawMessage `json:"input"`
					Store              bool              `json:"store"`
				}
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Errorf("decode request: %v", err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				requests = append(requests, request)
				responseID := fmt.Sprintf("resp_%d", len(requests))
				_, _ = fmt.Fprintf(w, `{"id":%q,"status":"completed","output":[{"id":"rs_1","type":"reasoning","encrypted_content":"opaque"},{"id":"msg_1","type":"message","role":"assistant","phase":"final_answer","content":[{"type":"output_text","text":"done"}]}]}`, responseID)
			}))
			defer server.Close()

			manager := NewModelManager(ModelConfig{
				Provider:         "openai",
				Model:            "test-model",
				BaseURL:          server.URL,
				APIMode:          tt.apiMode,
				ResponsesInclude: []string{"reasoning.encrypted_content"},
			}, ProxyConfig{})
			tracked := &usageTrackingModel{inner: manager, metrics: &RunMetrics{}}
			contextManager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
				{Role: messages.MessageRoleSystem, Content: "system guidance"},
				{Role: messages.MessageRoleUser, Content: "first"},
			})
			if err != nil {
				t.Fatalf("NewContextManagerFromMessageList: %v", err)
			}
			exec := executor.NewLLMExecutor(tracked, contextManager)
			if _, _, err := exec.Generate(context.Background()); err != nil {
				t.Fatalf("first Generate: %v", err)
			}
			if err := exec.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "second"}); err != nil {
				t.Fatalf("append second user message: %v", err)
			}
			if _, _, err := exec.Generate(context.Background()); err != nil {
				t.Fatalf("second Generate: %v", err)
			}

			if len(requests) != 2 {
				t.Fatalf("request count = %d, want 2", len(requests))
			}
			second := requests[1]
			if second.Store != tt.wantStore || second.PreviousResponseID != tt.wantPreviousID || len(second.Input) != tt.wantSecondInputs {
				t.Fatalf("second request = %#v", second)
			}
			parts := make([]string, len(second.Input))
			for i := range second.Input {
				parts[i] = string(second.Input[i])
			}
			joined := strings.Join(parts, "\n")
			if tt.wantOpaqueReplay && (!strings.Contains(joined, `"encrypted_content":"opaque"`) || !strings.Contains(joined, `"phase":"final_answer"`)) {
				t.Fatalf("second input lost opaque Responses output: %s", joined)
			}
			if !tt.wantOpaqueReplay && strings.Contains(joined, `"encrypted_content":"opaque"`) {
				t.Fatalf("stateful input replayed provider-owned output: %s", joined)
			}
		})
	}
}

func TestConvertResponsesContextInputReconstructsMissingRawOutputItems(t *testing.T) {
	input, err := convertResponsesContextInput([]messages.Message{{
		Role:    messages.MessageRoleToolCall,
		Content: "I will tap.",
		ToolCalls: []messages.ToolCall{{
			ID:        "call_1",
			Name:      "tap",
			Arguments: `{"x":10}`,
		}},
		ResponsesOutputItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning","id":"rs_1","encrypted_content":"opaque"}`)},
	}})
	if err != nil {
		t.Fatalf("convertResponsesContextInput: %v", err)
	}
	if len(input) != 3 || len(input[0].raw) == 0 || input[1].Role != "assistant" || input[2].Type != "function_call" || input[2].CallID != "call_1" {
		t.Fatalf("input = %#v, want raw reasoning plus reconstructed message and call", input)
	}
}
