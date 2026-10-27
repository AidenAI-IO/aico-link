package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	agentmessages "aico-agent/internal/agent/messages"
	"github.com/tmc/langchaingo/llms"
)

func TestDeepSeekVisionToolContinuation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, effort := range []string{"", "none", "low", "high", "max"} {
			name := effort
			if stream {
				name += "/stream"
			}
			t.Run(name, func(t *testing.T) {
				thinking := effort != "" && effort != "none"
				const reasoning = "Inspect the screen before taking action."
				type capturedRequest struct {
					Model           string              `json:"model"`
					Temperature     *float64            `json:"temperature"`
					Thinking        compatibleThinking  `json:"thinking"`
					ReasoningEffort string              `json:"reasoning_effort"`
					Reasoning       json.RawMessage     `json:"reasoning"`
					Messages        []compatibleMessage `json:"messages"`
					Tools           []compatibleTool    `json:"tools"`
				}
				var requests []capturedRequest
				client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					if req.URL.String() != "https://api.deepseek.com/chat/completions" || req.Method != http.MethodPost {
						t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
					}
					if req.Header.Get("Authorization") != "Bearer deepseek-test-key" {
						t.Fatal("missing provider credential")
					}
					requests = append(requests, capturedRequest{})
					if err := json.NewDecoder(req.Body).Decode(&requests[len(requests)-1]); err != nil {
						t.Fatal(err)
					}
					message := map[string]any{"role": "assistant", "content": "done"}
					finish := "stop"
					if len(requests) == 1 {
						message["content"] = ""
						message["tool_calls"] = []map[string]any{{"index": 0, "id": "call_1", "type": "function", "function": map[string]any{"name": "inspect", "arguments": "{}"}}}
						finish = "tool_calls"
					}
					if thinking {
						message["reasoning_content"] = reasoning
					}
					field := "message"
					if stream {
						field = "delta"
					}
					body, err := json.Marshal(map[string]any{"choices": []any{map[string]any{field: message, "finish_reason": finish}}})
					if err != nil {
						t.Fatal(err)
					}
					if stream {
						body = []byte("data: " + string(body) + "\n\ndata: [DONE]\n\n")
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
				})}
				temperature := 0.4
				cfg := ModelConfig{Provider: "deepseek", Model: "deepseek-flash", APIKey: "deepseek-test-key", ReasoningEffort: effort, Temperature: &temperature}
				built, err := buildDeepSeekModel(ModelBuildContext{HTTPClient: client}, cfg)
				if err != nil {
					t.Fatal(err)
				}
				manager := NewModelManager(cfg, ProxyConfig{})
				manager.model = built
				imagePath := filepath.Join(t.TempDir(), "screen.png")
				if err := os.WriteFile(imagePath, []byte("test-image"), 0600); err != nil {
					t.Fatal(err)
				}
				history := []agentmessages.Message{{
					Role: agentmessages.MessageRoleUser, Content: "Inspect this image",
					Attachments: []agentmessages.Attachment{{MIMEType: "image/png", FilePath: imagePath}},
				}}
				opts := []llms.CallOption{
					llms.WithTools([]llms.Tool{{Type: "function", Function: &llms.FunctionDefinition{Name: "inspect", Parameters: map[string]any{"type": "object"}}}}),
					llms.WithTemperature(0.7),
				}
				if stream {
					opts = append(opts, llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }))
				}
				response, err := manager.GenerateContentFromMessageList(context.Background(), history, opts...)
				if err != nil {
					t.Fatal(err)
				}
				if len(response.Choices) != 1 || len(response.Choices[0].ToolCalls) != 1 {
					t.Fatalf("missing tool call: %+v", response)
				}
				history = append(history, agentmessages.ConvertChoiceToContextManagerMessage(*response.Choices[0]), agentmessages.Message{
					Role:        agentmessages.MessageRoleToolResult,
					ToolResults: []agentmessages.ToolResult{{ToolCallID: "call_1", Name: "inspect", Content: "ok"}},
				})
				response, err = manager.GenerateContentFromMessageList(context.Background(), history, opts...)
				if err != nil || len(response.Choices) != 1 || response.Choices[0].Content != "done" {
					t.Fatalf("continuation = %+v, %v", response, err)
				}
				// The next user turn must also replay reasoning from the final
				// assistant reply, even though that reply did not call a tool.
				history = append(history, agentmessages.ConvertChoiceToContextManagerMessage(*response.Choices[0]), agentmessages.Message{Role: agentmessages.MessageRoleUser, Content: "Again"})
				if _, err := manager.GenerateContentFromMessageList(context.Background(), history, opts...); err != nil {
					t.Fatal(err)
				}
				wantEffort, wantThinking := "none", "disabled"
				if thinking {
					wantEffort, wantThinking = effort, "enabled"
				}
				for _, request := range requests {
					if request.Model != cfg.Model || request.Thinking.Type != wantThinking || request.ReasoningEffort != wantEffort || request.Reasoning != nil || len(request.Tools) != 1 {
						t.Fatalf("unexpected request profile: %+v", request)
					}
					if thinking {
						if request.Temperature != nil {
							t.Fatalf("thinking request temperature = %v, want omitted", *request.Temperature)
						}
					} else if request.Temperature == nil || *request.Temperature != temperature {
						t.Fatalf("non-thinking request temperature = %v, want %v", request.Temperature, temperature)
					}
					for _, message := range request.Messages {
						if message.Role == "assistant" {
							// All OpenAI-compatible providers now replay reasoning_content.
							// DeepSeek requires it on every assistant message (even when empty)
							// to maintain context with tool calls. Other providers only set
							// it when non-empty, but the field presence is universal.
							if thinking {
								if message.ReasoningContent == nil || *message.ReasoningContent != reasoning {
									t.Fatalf("assistant reasoning was not replayed: %+v", message)
								}
							} else {
								// In non-thinking mode, DeepSeek still sets the field (to empty
								// string) because of tool calls, but the content should be empty.
								if message.ReasoningContent != nil && *message.ReasoningContent != "" {
									t.Fatalf("unexpected non-empty reasoning in non-thinking mode: %+v", message)
								}
							}
						}
					}
				}
				imageJSON, _ := json.Marshal(requests[0].Messages[0].Content)
				if !strings.Contains(string(imageJSON), "data:image/png;base64,dGVzdC1pbWFnZQ==") {
					t.Fatalf("image input missing: %s", imageJSON)
				}
				toolResult := requests[1].Messages[2]
				if toolResult.Role != "tool" || toolResult.ToolCallID != "call_1" || toolResult.Content != "ok" {
					t.Fatalf("tool result pairing lost: %+v", toolResult)
				}
			})
		}
	}
}

func TestDeepSeekResponsesModeValidation(t *testing.T) {
	cfg := Config{ModelProviders: map[string]ModelProvider{"account": {Type: "deepseek"}}, Model: ModelConfig{Provider: "account", Model: "deepseek-flash", APIMode: "responses"}}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("validation rejected DeepSeek Responses mode: %v", err)
	}
	built, err := buildDeepSeekModel(ModelBuildContext{}, cfg.Model)
	if err != nil {
		t.Fatalf("builder rejected DeepSeek Responses mode: %v", err)
	}
	responses, ok := built.(*responsesModel)
	if !ok || responses.baseURL != deepseekBaseURL || responses.dialect != responsesDialectDeepSeek || responses.providerManagedContext || responses.reasoningEffort != "none" {
		t.Fatalf("DeepSeek Responses model = %#v", built)
	}

	cfg.Model.APIMode = "responses_stateful"
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "supports stored Responses") {
		t.Fatalf("stateful validation error = %v", err)
	}
	if _, err := buildDeepSeekModel(ModelBuildContext{}, cfg.Model); err == nil || !strings.Contains(err.Error(), "stateless") {
		t.Fatalf("stateful builder error = %v", err)
	}
}

func TestDeepSeekResponsesTemperature(t *testing.T) {
	configuredTemperature := 0.4
	for _, tt := range []struct {
		name        string
		effort      string
		temperature *float64
		options     []llms.CallOption
		want        *float64
	}{
		{name: "configured/non-thinking", effort: "none", temperature: &configuredTemperature, want: &configuredTemperature},
		{name: "per-call/non-thinking", effort: "none", options: []llms.CallOption{llms.WithTemperature(0.7)}, want: floatPtr(0.7)},
		{name: "configured/thinking", effort: "high", temperature: &configuredTemperature},
		{name: "per-call/thinking", effort: "high", options: []llms.CallOption{llms.WithTemperature(0.7)}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var request map[string]any
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				body := `{"id":"resp_1","status":"completed","output":[]}`
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			model, err := buildDeepSeekModel(ModelBuildContext{HTTPClient: client}, ModelConfig{
				Provider:        "deepseek",
				Model:           "deepseek-flash",
				APIMode:         "responses",
				ReasoningEffort: tt.effort,
				Temperature:     tt.temperature,
			})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := model.GenerateContent(context.Background(), []llms.MessageContent{{
				Role:  llms.ChatMessageTypeHuman,
				Parts: []llms.ContentPart{llms.TextPart("hello")},
			}}, tt.options...); err != nil {
				t.Fatal(err)
			}
			got, exists := request["temperature"]
			if tt.want == nil {
				if exists {
					t.Fatalf("temperature = %v, want omitted", got)
				}
				return
			}
			if !exists || got != *tt.want {
				t.Fatalf("temperature = %v, want %v", got, *tt.want)
			}
		})
	}
}

func TestDeepSeekResponsesVisionToolContinuation(t *testing.T) {
	for _, stream := range []bool{false, true} {
		name := "no_stream"
		if stream {
			name = "stream"
		}
		t.Run(name, func(t *testing.T) {
			var requests []map[string]any
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				if req.URL.String() != deepseekBaseURL+"/responses" || req.Method != http.MethodPost {
					t.Fatalf("unexpected request: %s %s", req.Method, req.URL)
				}
				if req.Header.Get("Authorization") != "Bearer deepseek-test-key" {
					t.Fatal("missing provider credential")
				}
				var request map[string]any
				if err := json.NewDecoder(req.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				requests = append(requests, request)
				requestStream, _ := request["stream"].(bool)
				if requestStream != stream {
					t.Fatalf("stream = %v, want %v", requestStream, stream)
				}
				if len(requests) == 1 {
					if stream {
						body := strings.Join([]string{
							`event: response.output_item.done`,
							`data: {"type":"response.output_item.done","item":{"id":"rs_1","type":"reasoning","status":"completed","content":[{"type":"reasoning_text","text":"Inspect the screenshot."}]}}`,
							``,
							`event: response.output_item.done`,
							`data: {"type":"response.output_item.done","item":{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"inspect","arguments":"{}"}}`,
							``,
							`event: response.completed`,
							`data: {"type":"response.completed","response":{"id":"resp_1","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":15}}}`,
							``,
						}, "\n")
						return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{"text/event-stream"}}}, nil
					}
					body := `{"id":"resp_1","status":"completed","output":[{"id":"rs_1","type":"reasoning","status":"completed","content":[{"type":"reasoning_text","text":"Inspect the screenshot."}]},{"id":"fc_1","type":"function_call","status":"completed","call_id":"call_1","name":"inspect","arguments":"{}"}],"usage":{"input_tokens":10,"output_tokens":5,"output_tokens_details":{"reasoning_tokens":3},"total_tokens":15}}`
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
				}
				if stream {
					body := strings.Join([]string{
						`event: response.output_text.delta`,
						`data: {"type":"response.output_text.delta","delta":"done"}`,
						``,
						`event: response.output_item.done`,
						`data: {"type":"response.output_item.done","item":{"id":"msg_2","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done"}]}}`,
						``,
						`event: response.completed`,
						`data: {"type":"response.completed","response":{"id":"resp_2","status":"completed"}}`,
						``,
					}, "\n")
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{"Content-Type": []string{"text/event-stream"}}}, nil
				}
				body := `{"id":"resp_2","status":"completed","output":[{"id":"msg_2","type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"done"}]}]}`
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
			})}
			cfg := ModelConfig{
				Provider:                   "deepseek",
				Model:                      "deepseek-flash",
				APIKey:                     "deepseek-test-key",
				APIMode:                    "responses",
				ReasoningEffort:            "high",
				ResponsesContextManagement: "compaction",
				ResponsesTruncation:        "auto",
				ResponsesInclude:           []string{"reasoning.encrypted_content"},
			}
			temperature := 0.4
			cfg.Temperature = &temperature
			built, err := buildDeepSeekModel(ModelBuildContext{HTTPClient: client}, cfg)
			if err != nil {
				t.Fatal(err)
			}
			manager := NewModelManager(cfg, ProxyConfig{})
			manager.model = built
			imagePath := filepath.Join(t.TempDir(), "screen.png")
			if err := os.WriteFile(imagePath, []byte("test-image"), 0600); err != nil {
				t.Fatal(err)
			}
			history := []agentmessages.Message{{
				Role: agentmessages.MessageRoleUser, Content: "Inspect this image",
				Attachments: []agentmessages.Attachment{{MIMEType: "image/png", FilePath: imagePath}},
			}}
			opts := []llms.CallOption{
				llms.WithTools([]llms.Tool{{Type: "function", Function: &llms.FunctionDefinition{Name: "inspect", Parameters: map[string]any{"type": "object"}}}}),
				llms.WithTemperature(0.7),
			}
			if stream {
				opts = append(opts, llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }))
			}
			response, err := manager.GenerateContentFromMessageList(context.Background(), history, opts...)
			if err != nil {
				t.Fatal(err)
			}
			if len(response.Choices) != 1 || response.Choices[0].ReasoningContent != "Inspect the screenshot." || len(response.Choices[0].ToolCalls) != 1 || response.Choices[0].ToolCalls[0].ID != "call_1" {
				t.Fatalf("missing tool call: %+v", response)
			}
			history = append(history, agentmessages.ConvertChoiceToContextManagerMessage(*response.Choices[0]), agentmessages.Message{
				Role:        agentmessages.MessageRoleToolResult,
				ToolResults: []agentmessages.ToolResult{{ToolCallID: "call_1", Name: "inspect", Content: "ok"}},
			})
			response, err = manager.GenerateContentFromMessageList(context.Background(), history, opts...)
			if err != nil || len(response.Choices) != 1 || response.Choices[0].Content != "done" {
				t.Fatalf("continuation = %+v, %v", response, err)
			}
			if len(requests) != 2 {
				t.Fatalf("request count = %d, want 2", len(requests))
			}
			for _, request := range requests {
				for _, unsupported := range []string{"store", "previous_response_id", "parallel_tool_calls", "context_management", "truncation", "include", "temperature"} {
					if _, exists := request[unsupported]; exists {
						t.Fatalf("DeepSeek request unexpectedly contains %s: %#v", unsupported, request)
					}
				}
				reasoning, ok := request["reasoning"].(map[string]any)
				if !ok || reasoning["effort"] != "high" {
					t.Fatalf("reasoning = %#v", request["reasoning"])
				}
			}
			firstInput, _ := json.Marshal(requests[0]["input"])
			if !strings.Contains(string(firstInput), `"type":"input_image"`) || !strings.Contains(string(firstInput), "data:image/png;base64,dGVzdC1pbWFnZQ==") {
				t.Fatalf("image input missing: %s", firstInput)
			}
			secondInput, _ := json.Marshal(requests[1]["input"])
			for _, want := range []string{`"type":"reasoning"`, `"type":"function_call"`, `"type":"function_call_output"`, `"call_id":"call_1"`} {
				if !strings.Contains(string(secondInput), want) {
					t.Fatalf("continuation input missing %s: %s", want, secondInput)
				}
			}
		})
	}
}

func TestReasoningContentReplayPreservesAssistantBoundaries(t *testing.T) {
	for _, tt := range []struct {
		name      string
		options   []openAICompatibleModelOption
		alwaysSet bool
	}{
		{name: "compatible", options: nil},
		{name: "deepseek", options: []openAICompatibleModelOption{withOpenAICompatibleDialect(compatibleDialectDeepSeek)}, alwaysSet: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			client := &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				var payload compatibleChatRequest
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil {
					t.Fatal(err)
				}
				if len(payload.Messages) != 4 || payload.Messages[1].ReasoningContent == nil || *payload.Messages[1].ReasoningContent != "first thought" {
					t.Fatalf("assistant reasoning/boundaries lost: %+v", payload.Messages)
				}
				if tt.alwaysSet {
					if payload.Messages[2].ReasoningContent == nil || *payload.Messages[2].ReasoningContent != "" {
						t.Fatalf("empty assistant reasoning field missing: %+v", payload.Messages)
					}
				} else if payload.Messages[2].ReasoningContent != nil {
					t.Fatalf("empty assistant reasoning field should be omitted: %+v", payload.Messages)
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`))}, nil
			})}
			options := append([]openAICompatibleModelOption{withOpenAICompatibleReasoningEffort("high")}, tt.options...)
			m := newOpenAICompatibleModel("https://example.test", "test", "", client, options...).(*openAICompatibleModel)
			_, err := m.GenerateContentFromMessageList(context.Background(), []agentmessages.Message{
				{Role: agentmessages.MessageRoleUser, Content: "hello"},
				{Role: agentmessages.MessageRoleAssistant, Content: "first", ReasoningContent: "first thought"},
				{Role: agentmessages.MessageRoleAssistant, Content: "second"},
				{Role: agentmessages.MessageRoleUser, Content: "continue"},
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestDeepSeekNamedProviderConfig(t *testing.T) {
	t.Setenv("AICO_TEST_DEEPSEEK_KEY", "test-key")
	path := filepath.Join(t.TempDir(), "agent.toml")
	if err := os.WriteFile(path, []byte(`[model_settings.providers.account]
type = "DeepSeek"
api_key = "$AICO_TEST_DEEPSEEK_KEY"

[model_settings.model]
provider = "account"
model = "deepseek-flash"
`), 0600); err != nil {
		t.Fatal(err)
	}
	runtimeConfig, err := LoadRuntimeConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if runtimeConfig.Model.Provider != "deepseek" || runtimeConfig.Model.ReasoningEffort != "none" {
		t.Fatalf("unexpected runtime config: %+v", runtimeConfig.Model)
	}
	m, err := NewModelManager(runtimeConfig.Model, ProxyConfig{}).build()
	if err != nil {
		t.Fatal(err)
	}
	if got := m.(*openAICompatibleModel); got.token != "test-key" || got.baseURL != deepseekBaseURL || got.dialect != compatibleDialectDeepSeek || got.reasoningEffort != "none" {
		t.Fatal("named provider did not resolve to DeepSeek request profile")
	}
	editorConfig, err := LoadResolvedConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if editorConfig.Model.Provider != "account" || editorConfig.Model.ReasoningEffort != "" {
		t.Fatalf("runtime defaults leaked into editor: %+v", editorConfig.Model)
	}
}
