package agent

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"aico-agent/internal/agent/messages"
	"github.com/tmc/langchaingo/llms"
)

const (
	modelAPIModeInteractions         = "interactions"
	modelAPIModeInteractionsStateful = "interactions_stateful"

	// geminiInteractionsBaseURL is the native Interactions API root. The
	// OpenAI-compatible ".../v1beta/openai" endpoint is deliberately unused: it
	// drops thought signatures and breaks multi-turn tool calls on Gemini 3.
	geminiInteractionsBaseURL = "https://generativelanguage.googleapis.com/v1beta"
)

// errInteractionsStreamIncomplete reports a stream that stopped before the
// provider's terminal event. The steps it produced are a prefix of the
// interaction, so the caller must not treat them as a finished response.
var errInteractionsStreamIncomplete = errors.New("interactions stream ended before interaction.completed")

// interactionsModel implements Gemini's native Interactions API. It is kept
// separate from responsesModel because Interactions uses typed steps rather
// than OpenAI response items, and function arguments are JSON objects.
type interactionsModel struct {
	baseURL                string
	model                  string
	token                  string
	httpClient             *http.Client
	rawLogger              RawHTTPLogger
	reasoningEffort        string
	temperature            *float64
	providerManagedContext bool
}

type interactionsModelOptions struct {
	rawLogger              RawHTTPLogger
	reasoningEffort        string
	temperature            *float64
	providerManagedContext bool
}

func newInteractionsModel(baseURL, model, token string, httpClient *http.Client, opts interactionsModelOptions) llms.Model {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &interactionsModel{
		baseURL:                strings.TrimRight(baseURL, "/"),
		model:                  strings.TrimSpace(model),
		token:                  strings.TrimSpace(token),
		httpClient:             httpClient,
		rawLogger:              opts.rawLogger,
		reasoningEffort:        normalizeInteractionsThinkingLevel(opts.reasoningEffort),
		temperature:            opts.temperature,
		providerManagedContext: opts.providerManagedContext,
	}
}

func (m *interactionsModel) Call(ctx context.Context, prompt string, options ...llms.CallOption) (string, error) {
	return llms.GenerateFromSinglePrompt(ctx, m, prompt, options...)
}

func (m *interactionsModel) GenerateContent(ctx context.Context, input []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	steps, systemInstruction, err := convertInteractionsMessages(input)
	if err != nil {
		return nil, err
	}
	return m.generateContent(ctx, steps, systemInstruction, "", options...)
}

// GenerateContentFromMessageList retains the durable transcript's tool calls,
// tool results, and thinking summaries. Stateful mode sends only messages after
// the latest interaction ID; local mode resubmits the complete StepList.
func (m *interactionsModel) GenerateContentFromMessageList(ctx context.Context, contextMessages []messages.Message, options ...llms.CallOption) (*llms.ContentResponse, error) {
	var inputMessages []messages.Message
	var previousID string
	if m.providerManagedContext {
		var instructions string
		instructions, previousID, inputMessages = providerManagedContextInput(contextMessages)
		steps, _, err := convertInteractionsContextMessagesStateful(contextMessages, inputMessages)
		if err != nil {
			return nil, err
		}
		response, err := m.generateContent(ctx, steps, instructions, previousID, options...)
		if err == nil || previousID == "" || !shouldRetryInteractionWithoutPreviousID(err) {
			return response, err
		}
		// Provider state can expire while the local transcript is still valid.
		// Retry once as a fresh stateful interaction with the complete transcript.
		inputMessages = nil
		for _, message := range contextMessages {
			if message.Role == messages.MessageRoleSystem {
				continue
			}
			inputMessages = append(inputMessages, message)
		}
		steps, _, err = convertInteractionsContextMessages(inputMessages)
		if err != nil {
			return nil, err
		}
		return m.generateContent(ctx, steps, instructions, "", options...)
	}
	steps, systemInstruction, err := convertInteractionsContextMessages(contextMessages)
	if err != nil {
		return nil, err
	}
	return m.generateContent(ctx, steps, systemInstruction, "", options...)
}

func shouldRetryInteractionWithoutPreviousID(err error) bool {
	var providerErr *ProviderHTTPError
	if !errors.As(err, &providerErr) {
		return false
	}
	if providerErr.StatusCode != http.StatusBadRequest && providerErr.StatusCode != http.StatusNotFound && providerErr.StatusCode != http.StatusGone {
		return false
	}
	text := strings.ToLower(strings.TrimSpace(providerErr.ProviderCode + " " + providerErr.Message + " " + providerErr.Body))
	return strings.Contains(text, "previous_interaction") || strings.Contains(text, "interaction not found") || strings.Contains(text, "interaction expired")
}

type interactionsRequest struct {
	Model                 string                        `json:"model"`
	Input                 []interactionStep             `json:"input"`
	SystemInstruction     string                        `json:"system_instruction,omitempty"`
	Tools                 []interactionTool             `json:"tools,omitempty"`
	PreviousInteractionID string                        `json:"previous_interaction_id,omitempty"`
	Store                 *bool                         `json:"store,omitempty"`
	Stream                bool                          `json:"stream,omitempty"`
	GenerationConfig      *interactionsGenerationConfig `json:"generation_config,omitempty"`
	ResponseFormat        any                           `json:"response_format,omitempty"`
}

// interactionsGenerationConfig mirrors the Interactions API GenerationConfig.
// Google documents temperature here; Aico forwards a resolved or explicit value
// and leaves it absent when runtime configuration delegates to the model default.
type interactionsGenerationConfig struct {
	MaxOutputTokens   int      `json:"max_output_tokens,omitempty"`
	Seed              int      `json:"seed,omitempty"`
	StopSequences     []string `json:"stop_sequences,omitempty"`
	Temperature       *float64 `json:"temperature,omitempty"`
	ThinkingLevel     string   `json:"thinking_level,omitempty"`
	ThinkingSummaries string   `json:"thinking_summaries,omitempty"`
	ToolChoice        any      `json:"tool_choice,omitempty"`
}

type interactionTool struct {
	Type        string `json:"type"`
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
}

type interactionStep struct {
	Type      string               `json:"type"`
	Content   []interactionContent `json:"content,omitempty"`
	Summary   []interactionContent `json:"summary,omitempty"`
	Signature string               `json:"signature,omitempty"`
	ID        string               `json:"id,omitempty"`
	Name      string               `json:"name,omitempty"`
	CallID    string               `json:"call_id,omitempty"`
	Arguments json.RawMessage      `json:"arguments,omitempty"`
	Result    any                  `json:"result,omitempty"`
	IsError   bool                 `json:"is_error,omitempty"`
	raw       json.RawMessage
}

func (s interactionStep) MarshalJSON() ([]byte, error) {
	if len(s.raw) != 0 {
		return s.raw, nil
	}
	type wireStep interactionStep
	return json.Marshal(wireStep(s))
}

func (s *interactionStep) UnmarshalJSON(data []byte) error {
	type wireStep interactionStep
	var decoded wireStep
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*s = interactionStep(decoded)
	s.raw = append(s.raw[:0], data...)
	return nil
}

type interactionContent struct {
	Type     string `json:"type"`
	Text     string `json:"text,omitempty"`
	Data     string `json:"data,omitempty"`
	MIMEType string `json:"mime_type,omitempty"`
	URI      string `json:"uri,omitempty"`
}

type interactionsResponse struct {
	ID     string             `json:"id,omitempty"`
	Status string             `json:"status,omitempty"`
	Steps  []json.RawMessage  `json:"steps,omitempty"`
	Usage  *interactionsUsage `json:"usage,omitempty"`
	Errors []interactionError `json:"errors,omitempty"`
}

type interactionsUsage struct {
	TotalCachedTokens  int `json:"total_cached_tokens"`
	TotalInputTokens   int `json:"total_input_tokens"`
	TotalOutputTokens  int `json:"total_output_tokens"`
	TotalThoughtTokens int `json:"total_thought_tokens"`
	TotalTokens        int `json:"total_tokens"`
	TotalToolUseTokens int `json:"total_tool_use_tokens"`
}

func (u *interactionsUsage) generationInfo() map[string]any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"prompt_tokens": u.TotalInputTokens, "completion_tokens": u.TotalOutputTokens,
		"total_tokens": u.TotalTokens, "cached_tokens": u.TotalCachedTokens,
		"reasoning_tokens": u.TotalThoughtTokens, "tool_use_tokens": u.TotalToolUseTokens,
	}
}

type interactionError struct {
	Code    string `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

func (m *interactionsModel) generateContent(ctx context.Context, steps []interactionStep, systemInstruction, previousID string, options ...llms.CallOption) (*llms.ContentResponse, error) {
	callStarted := time.Now()
	callOpts := llms.CallOptions{}
	for _, option := range options {
		option(&callOpts)
	}
	requestModel := firstNonEmpty(callOpts.Model, m.model)
	store := m.providerManagedContext
	payload := interactionsRequest{
		Model: requestModel, Input: steps, SystemInstruction: systemInstruction,
		PreviousInteractionID: previousID, Store: &store,
		Stream: callOpts.StreamingFunc != nil || callOpts.StreamingReasoningFunc != nil,
		Tools:  convertInteractionsTools(callOpts.Tools, callOpts.Functions),
	}
	config := &interactionsGenerationConfig{
		MaxOutputTokens: callOpts.MaxTokens,
		Seed:            callOpts.Seed,
		StopSequences:   callOpts.StopWords,
		Temperature:     m.temperature,
		ThinkingLevel:   m.reasoningEffort,
		ToolChoice:      normalizeInteractionsToolChoice(callOpts.ToolChoice, callOpts.FunctionCallBehavior),
	}
	if config.Temperature == nil && callOpts.Temperature != 0 {
		config.Temperature = &callOpts.Temperature
	}
	if callOpts.StreamingReasoningFunc != nil {
		config.ThinkingSummaries = "auto"
	}
	if config.MaxOutputTokens > 0 || config.Seed != 0 || len(config.StopSequences) > 0 || config.Temperature != nil || config.ThinkingLevel != "" || config.ThinkingSummaries != "" || config.ToolChoice != nil {
		payload.GenerationConfig = config
	}
	if callOpts.JSONMode {
		payload.ResponseFormat = map[string]any{"type": "text", "mime_type": "application/json"}
	}
	generationInfo := map[string]any{"llm_stream": payload.Stream, "llm_request_bytes": 0}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal interactions request: %w", err)
	}
	generationInfo["llm_request_bytes"] = len(payloadBytes)
	ctx = m.withRawHTTPLogFileTime(ctx)
	_ = m.logRawHTTP(ctx, requestModel, "request", 0, string(payloadBytes))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/interactions", bytes.NewReader(payloadBytes))
	if err != nil {
		return nil, fmt.Errorf("create interactions request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if payload.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if m.token != "" {
		req.Header.Set("x-goog-api-key", m.token)
	}
	doStart := time.Now()
	resp, err := m.httpClient.Do(req)
	generationInfo["llm_http_to_headers_ms"] = time.Since(doStart).Milliseconds()
	if err != nil {
		_ = m.logRawHTTP(ctx, requestModel, "response", 0, "transport error: "+err.Error())
		return nil, fmt.Errorf("send interactions request: %w", err)
	}
	defer resp.Body.Close()
	generationInfo["llm_http_status"] = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = m.logRawHTTP(ctx, requestModel, "response", resp.StatusCode, string(body))
		return nil, newProviderHTTPError(resp.StatusCode, body)
	}
	if payload.Stream {
		return m.decodeInteractionsStream(ctx, resp.Body, callOpts.StreamingFunc, callOpts.StreamingReasoningFunc, requestModel, resp.StatusCode, callStarted, generationInfo)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read interactions response: %w", err)
	}
	_ = m.logRawHTTP(ctx, requestModel, "response", resp.StatusCode, string(body))
	var decoded interactionsResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode interactions response: %w", err)
	}
	if interactionTerminalFailure(decoded.Status) {
		return nil, newProviderHTTPError(http.StatusBadRequest, body)
	}
	return interactionsContentResponse(decoded, callStarted, generationInfo)
}

func (m *interactionsModel) withRawHTTPLogFileTime(ctx context.Context) context.Context {
	if m == nil || m.rawLogger == nil || !rawHTTPLogEnabled(ctx) {
		return ctx
	}
	return m.rawLogger.BeginScope(ctx)
}

func (m *interactionsModel) logRawHTTP(ctx context.Context, modelName, kind string, statusCode int, raw string) error {
	if m == nil || m.rawLogger == nil || !rawHTTPLogEnabled(ctx) {
		return nil
	}
	return m.rawLogger.Log(ctx, RawHTTPLogEntry{Model: modelName, Kind: kind, StatusCode: statusCode, Raw: raw})
}

func interactionsContentResponse(decoded interactionsResponse, callStarted time.Time, generationInfo map[string]any) (*llms.ContentResponse, error) {
	content, reasoning, calls, err := parseInteractionSteps(decoded.Steps)
	if err != nil {
		return nil, err
	}
	if decoded.ID != "" {
		generationInfo["llm_response_id"] = decoded.ID
	}
	generationInfo["llm_interaction_status"] = decoded.Status
	generationInfo["interactions_steps"] = cloneRawJSONItems(decoded.Steps)
	generationInfo["llm_output_chars"] = len(content)
	generationInfo["llm_tool_call_count"] = len(calls)
	choice := &llms.ContentChoice{Content: content, ReasoningContent: reasoning, ToolCalls: calls, StopReason: decoded.Status}
	if len(calls) > 0 {
		choice.FuncCall = calls[0].FunctionCall
	}
	choice.GenerationInfo = finalizeLLMGenerationInfo(mergeGenerationInfo(decoded.Usage.generationInfo(), generationInfo), callStarted)
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{choice}}, nil
}

func parseInteractionSteps(rawSteps []json.RawMessage) (string, string, []llms.ToolCall, error) {
	var content, reasoning strings.Builder
	calls := make([]llms.ToolCall, 0)
	for _, raw := range rawSteps {
		var step interactionStep
		if err := json.Unmarshal(raw, &step); err != nil {
			return "", "", nil, fmt.Errorf("decode interaction step: %w", err)
		}
		switch step.Type {
		case "model_output":
			appendInteractionContent(&content, step.Content)
		case "thought":
			appendInteractionContent(&reasoning, step.Summary)
		case "function_call":
			args := string(step.Arguments)
			if strings.TrimSpace(args) == "" {
				args = "{}"
			}
			calls = append(calls, llms.ToolCall{ID: step.ID, Type: "function", FunctionCall: &llms.FunctionCall{Name: step.Name, Arguments: args}})
		}
	}
	return content.String(), strings.TrimSpace(reasoning.String()), calls, nil
}

func appendInteractionContent(out *strings.Builder, content []interactionContent) {
	for _, part := range content {
		if part.Type == "text" && part.Text != "" {
			if out.Len() > 0 {
				out.WriteString("\n")
			}
			out.WriteString(part.Text)
		}
	}
}

type interactionsStreamEvent struct {
	EventType     string             `json:"event_type"`
	EventID       string             `json:"event_id,omitempty"`
	Interaction   *interactionSSE    `json:"interaction,omitempty"`
	InteractionID string             `json:"interaction_id,omitempty"`
	Status        string             `json:"status,omitempty"`
	Index         int                `json:"index,omitempty"`
	Step          json.RawMessage    `json:"step,omitempty"`
	Delta         json.RawMessage    `json:"delta,omitempty"`
	StepUsage     *interactionsUsage `json:"step_usage,omitempty"`
	Usage         *interactionsUsage `json:"usage,omitempty"`
	Error         *interactionError  `json:"error,omitempty"`
}

type interactionSSE struct {
	ID     string             `json:"id,omitempty"`
	Status string             `json:"status,omitempty"`
	Steps  []json.RawMessage  `json:"steps,omitempty"`
	Usage  *interactionsUsage `json:"usage,omitempty"`
	Errors []interactionError `json:"errors,omitempty"`
}

// interactionStreamCall accumulates one streamed function call. The API sends
// the arguments twice in different shapes: step.start carries an `arguments`
// JSON object that is usually the empty placeholder `{}`, and the following
// arguments_delta events carry the real payload as JSON *text* chunks.
// Concatenating the two produces `{}{"name":"aico"}`, so the seed and the
// deltas are kept apart and the deltas win whenever any arrived.
type interactionStreamCall struct {
	ID       string
	Name     string
	seed     string
	deltas   strings.Builder
	hasDelta bool
}

func (c *interactionStreamCall) writeSeed(arguments json.RawMessage) {
	c.seed = strings.TrimSpace(string(arguments))
}

func (c *interactionStreamCall) writeDelta(chunk string) {
	c.hasDelta = true
	c.deltas.WriteString(chunk)
}

// argumentsJSON returns the arguments the provider actually described. Deltas
// are authoritative once present; otherwise the step.start object stands on its
// own. An empty or unparsable result falls back to `{}` so a tool still receives
// a well-formed object.
func (c *interactionStreamCall) argumentsJSON() string {
	arguments := strings.TrimSpace(c.seed)
	if c.hasDelta {
		arguments = strings.TrimSpace(c.deltas.String())
	}
	if arguments == "" || !json.Valid([]byte(arguments)) {
		return "{}"
	}
	return arguments
}

func (m *interactionsModel) decodeInteractionsStream(ctx context.Context, body io.Reader, stream func(context.Context, []byte) error, reasoningStream func(context.Context, []byte, []byte) error, requestModel string, statusCode int, callStarted time.Time, generationInfo map[string]any) (*llms.ContentResponse, error) {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var content, reasoning strings.Builder
	calls := map[int]*interactionStreamCall{}
	status := ""
	streamDone := false
	terminalEventSeen := false
	var usageInfo map[string]any
	var rawStream strings.Builder
	var interactionSteps []json.RawMessage
	// Text and thought summaries are accumulated per step index as well as
	// globally. A single interaction can contain several model_output or thought
	// steps, and each replayed step must carry only its own text.
	thoughtSignatures := map[int]string{}
	stepTexts := map[int]*strings.Builder{}
	stepThoughts := map[int]*strings.Builder{}
	authoritativeSteps := false
	appendStepChunk := func(store map[int]*strings.Builder, index int, chunk string) {
		if chunk == "" {
			return
		}
		builder, ok := store[index]
		if !ok {
			builder = &strings.Builder{}
			store[index] = builder
		}
		builder.WriteString(chunk)
	}
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if rawHTTPLogEnabled(ctx) {
			if rawStream.Len() > 0 {
				rawStream.WriteByte('\n')
			}
			rawStream.WriteString(line)
		}
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			streamDone = true
			break
		}
		var event interactionsStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil, fmt.Errorf("decode interactions stream event: %w", err)
		}
		if event.Interaction != nil {
			if event.Interaction.ID != "" {
				generationInfo["llm_response_id"] = event.Interaction.ID
			}
			if event.Interaction.Status != "" {
				status = event.Interaction.Status
			}
			if event.Interaction.Usage != nil {
				usageInfo = event.Interaction.Usage.generationInfo()
			}
		}
		if event.InteractionID != "" {
			generationInfo["llm_response_id"] = event.InteractionID
		}
		if event.Status != "" {
			status = event.Status
		}
		if event.Usage != nil {
			usageInfo = event.Usage.generationInfo()
		}
		if event.StepUsage != nil && usageInfo == nil {
			usageInfo = event.StepUsage.generationInfo()
		}
		if event.Error != nil {
			return nil, newProviderHTTPError(statusCode, []byte(event.Error.Message))
		}
		switch event.EventType {
		case "step.start":
			for len(interactionSteps) <= event.Index {
				interactionSteps = append(interactionSteps, nil)
			}
			interactionSteps[event.Index] = append(json.RawMessage(nil), event.Step...)
			var step interactionStep
			if err := json.Unmarshal(event.Step, &step); err != nil {
				return nil, fmt.Errorf("decode interaction step start: %w", err)
			}
			switch step.Type {
			case "model_output":
				chunk := interactionContentText(step.Content)
				if chunk != "" {
					content.WriteString(chunk)
					appendStepChunk(stepTexts, event.Index, chunk)
					if stream != nil {
						if err := stream(ctx, []byte(chunk)); err != nil {
							return nil, err
						}
					}
				}
			case "thought":
				if step.Signature != "" {
					thoughtSignatures[event.Index] = step.Signature
				}
				chunk := interactionContentText(step.Summary)
				if chunk != "" {
					reasoning.WriteString(chunk)
					appendStepChunk(stepThoughts, event.Index, chunk)
					if reasoningStream != nil {
						if err := reasoningStream(ctx, []byte(chunk), nil); err != nil {
							return nil, err
						}
					}
				}
			case "function_call":
				call := &interactionStreamCall{ID: step.ID, Name: step.Name}
				call.writeSeed(step.Arguments)
				calls[event.Index] = call
			}
		case "step.delta":
			var delta struct {
				Type      string             `json:"type"`
				Text      string             `json:"text,omitempty"`
				Arguments string             `json:"arguments,omitempty"`
				Signature string             `json:"signature,omitempty"`
				Content   interactionContent `json:"content,omitempty"`
			}
			if err := json.Unmarshal(event.Delta, &delta); err != nil {
				return nil, fmt.Errorf("decode interaction step delta: %w", err)
			}
			switch delta.Type {
			case "text":
				chunk := delta.Text
				content.WriteString(chunk)
				appendStepChunk(stepTexts, event.Index, chunk)
				if stream != nil && chunk != "" {
					if err := stream(ctx, []byte(chunk)); err != nil {
						return nil, err
					}
				}
			case "thought_summary":
				chunk := delta.Content.Text
				reasoning.WriteString(chunk)
				appendStepChunk(stepThoughts, event.Index, chunk)
				if reasoningStream != nil && chunk != "" {
					if err := reasoningStream(ctx, []byte(chunk), nil); err != nil {
						return nil, err
					}
				}
			case "thought_signature":
				if delta.Signature != "" {
					thoughtSignatures[event.Index] = delta.Signature
				}
			case "arguments_delta":
				if call := calls[event.Index]; call != nil {
					call.writeDelta(delta.Arguments)
				}
			}
		case "interaction.completed":
			terminalEventSeen = true
			if event.Interaction != nil && interactionTerminalFailure(event.Interaction.Status) {
				failure, _ := json.Marshal(event.Interaction)
				return nil, newProviderHTTPError(http.StatusBadGateway, failure)
			}
			if event.Interaction != nil && len(event.Interaction.Steps) > 0 {
				// The completed event carries the provider's own final steps,
				// including thought signatures. Replay them verbatim.
				interactionSteps = cloneRawJSONItems(event.Interaction.Steps)
				authoritativeSteps = true
				parsedContent, parsedReasoning, parsedCalls, err := parseInteractionSteps(event.Interaction.Steps)
				if err != nil {
					return nil, err
				}
				if content.Len() == 0 {
					content.WriteString(parsedContent)
				}
				if reasoning.Len() == 0 {
					reasoning.WriteString(parsedReasoning)
				}
				if len(calls) == 0 {
					for i, call := range parsedCalls {
						calls[i] = &interactionStreamCall{ID: call.ID, Name: call.FunctionCall.Name}
						calls[i].writeDelta(call.FunctionCall.Arguments)
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read interactions stream: %w", err)
	}
	if !streamDone && !terminalEventSeen {
		// A dropped connection ends the body the same way a completed stream does.
		// Without the terminal event what was accumulated is only a prefix, and a
		// function_call cut mid-arguments would reach the tools as `{}`.
		return nil, errInteractionsStreamIncomplete
	}
	if status == "" {
		status = "completed"
	}
	generationInfo["llm_stream_read_ms"] = time.Since(callStarted).Milliseconds()
	generationInfo["llm_interaction_status"] = status
	// The completed event may omit steps; retain step.start/delta-derived output
	// in the common choice fields and preserve full raw steps when provided.
	toolIndexes := make([]int, 0, len(calls))
	for index := range calls {
		toolIndexes = append(toolIndexes, index)
	}
	sort.Ints(toolIndexes)
	toolCalls := make([]llms.ToolCall, 0, len(toolIndexes))
	for _, index := range toolIndexes {
		call := calls[index]
		toolCalls = append(toolCalls, llms.ToolCall{ID: call.ID, Type: "function", FunctionCall: &llms.FunctionCall{Name: call.Name, Arguments: call.argumentsJSON()}})
	}
	if !authoritativeSteps {
		interactionSteps = completeInteractionStreamSteps(interactionSteps, stepTexts, stepThoughts, calls, thoughtSignatures)
	}
	generationInfo["interactions_steps"] = cloneRawJSONItems(interactionSteps)
	choice := &llms.ContentChoice{Content: content.String(), ReasoningContent: strings.TrimSpace(reasoning.String()), StopReason: status, ToolCalls: toolCalls}
	if len(toolCalls) > 0 {
		choice.FuncCall = toolCalls[0].FunctionCall
	}
	generationInfo["llm_output_chars"] = content.Len()
	generationInfo["llm_tool_call_count"] = len(toolCalls)
	choice.GenerationInfo = finalizeLLMGenerationInfo(mergeGenerationInfo(usageInfo, generationInfo), callStarted)
	if m.rawLogger != nil && rawHTTPLogEnabled(ctx) {
		_ = m.logRawHTTP(ctx, requestModel, "response", statusCode, rawStream.String())
	}
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{choice}}, nil
}

func interactionTerminalFailure(status string) bool {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "failed", "cancelled", "budget_exceeded":
		return true
	default:
		return false
	}
}

// completeInteractionStreamSteps fills streamed step envelopes with the text,
// thought summaries, signatures, and function arguments accumulated for each
// step index. Fields the provider sent that this client does not model are kept
// byte-for-byte, because Gemini requires thought steps to be replayed exactly as
// received when the local transcript is resubmitted.
func completeInteractionStreamSteps(rawSteps []json.RawMessage, texts, thoughts map[int]*strings.Builder, calls map[int]*interactionStreamCall, thoughtSignatures map[int]string) []json.RawMessage {
	if len(rawSteps) == 0 {
		return synthesizeInteractionStreamSteps(texts, thoughts, calls, thoughtSignatures)
	}
	completed := make([]json.RawMessage, 0, len(rawSteps))
	for i, raw := range rawSteps {
		if len(raw) == 0 {
			continue
		}
		var step interactionStep
		if json.Unmarshal(raw, &step) != nil {
			completed = append(completed, append(json.RawMessage(nil), raw...))
			continue
		}
		patch := map[string]json.RawMessage{}
		switch step.Type {
		case "model_output":
			if text := interactionStepText(texts, i); text != "" {
				patch["content"] = marshalRawJSON([]interactionContent{{Type: "text", Text: text}})
			}
		case "thought":
			if thought := interactionStepText(thoughts, i); thought != "" {
				patch["summary"] = marshalRawJSON([]interactionContent{{Type: "text", Text: thought}})
			}
			if signature := thoughtSignatures[i]; signature != "" {
				patch["signature"] = marshalRawJSON(signature)
			}
		case "function_call":
			if call := calls[i]; call != nil {
				patch["id"] = marshalRawJSON(call.ID)
				patch["name"] = marshalRawJSON(call.Name)
				patch["arguments"] = json.RawMessage(call.argumentsJSON())
			}
		}
		completed = append(completed, patchRawJSONObject(raw, patch))
	}
	return completed
}

// synthesizeInteractionStreamSteps builds steps when the provider streamed
// deltas without step envelopes. Steps are emitted in stream index order so
// several thought or model_output steps keep their own text and signature.
func synthesizeInteractionStreamSteps(texts, thoughts map[int]*strings.Builder, calls map[int]*interactionStreamCall, thoughtSignatures map[int]string) []json.RawMessage {
	indexes := make([]int, 0, len(texts)+len(thoughts)+len(calls))
	seen := map[int]bool{}
	for _, store := range []map[int]*strings.Builder{texts, thoughts} {
		for index := range store {
			if !seen[index] {
				seen[index] = true
				indexes = append(indexes, index)
			}
		}
	}
	for index := range calls {
		if !seen[index] {
			seen[index] = true
			indexes = append(indexes, index)
		}
	}
	for index := range thoughtSignatures {
		if !seen[index] {
			seen[index] = true
			indexes = append(indexes, index)
		}
	}
	sort.Ints(indexes)
	result := make([]json.RawMessage, 0, len(indexes))
	for _, index := range indexes {
		var step interactionStep
		switch {
		case calls[index] != nil:
			call := calls[index]
			step = interactionStep{Type: "function_call", ID: call.ID, Name: call.Name, Arguments: json.RawMessage(call.argumentsJSON())}
		case interactionStepText(thoughts, index) != "" || thoughtSignatures[index] != "":
			step = interactionStep{Type: "thought", Signature: thoughtSignatures[index]}
			if thought := interactionStepText(thoughts, index); thought != "" {
				step.Summary = []interactionContent{{Type: "text", Text: thought}}
			}
		case interactionStepText(texts, index) != "":
			step = interactionStep{Type: "model_output", Content: []interactionContent{{Type: "text", Text: interactionStepText(texts, index)}}}
		default:
			continue
		}
		if encoded := mustMarshalInteractionStep(step); len(encoded) != 0 {
			result = append(result, encoded)
		}
	}
	return result
}

func interactionStepText(store map[int]*strings.Builder, index int) string {
	if builder, ok := store[index]; ok && builder != nil {
		return builder.String()
	}
	return ""
}

func marshalRawJSON(value any) json.RawMessage {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil
	}
	return encoded
}

// patchRawJSONObject overwrites named fields in a JSON object while preserving
// every other field exactly as received, including ones this client does not
// model. The original bytes are returned unchanged when nothing can be patched.
func patchRawJSONObject(raw json.RawMessage, patch map[string]json.RawMessage) json.RawMessage {
	original := append(json.RawMessage(nil), raw...)
	if len(patch) == 0 {
		return original
	}
	fields := map[string]json.RawMessage{}
	if json.Unmarshal(raw, &fields) != nil {
		return original
	}
	for key, value := range patch {
		if len(value) == 0 {
			continue
		}
		fields[key] = value
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return original
	}
	return encoded
}

func mustMarshalInteractionStep(step interactionStep) json.RawMessage {
	encoded, err := json.Marshal(step)
	if err != nil {
		return nil
	}
	return encoded
}

func cloneRawJSONItems(items []json.RawMessage) []json.RawMessage {
	if len(items) == 0 {
		return nil
	}
	cloned := make([]json.RawMessage, len(items))
	for i, item := range items {
		cloned[i] = append(json.RawMessage(nil), item...)
	}
	return cloned
}

func interactionContentText(content []interactionContent) string {
	var out strings.Builder
	appendInteractionContent(&out, content)
	return out.String()
}

func normalizeInteractionsThinkingLevel(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "minimal", "low", "medium", "high":
		return strings.ToLower(strings.TrimSpace(value))
	default:
		return ""
	}
}

func normalizeInteractionsToolChoice(choice any, behavior llms.FunctionCallBehavior) any {
	if choice != nil {
		if typed, ok := choice.(llms.ToolChoice); ok && typed.Function != nil {
			return map[string]any{"allowed_tools": map[string]any{"mode": "any", "tools": []string{typed.Function.Name}}}
		}
		return choice
	}
	if behavior != "" {
		return string(behavior)
	}
	return nil
}

func convertInteractionsTools(tools []llms.Tool, functions []llms.FunctionDefinition) []interactionTool {
	if len(tools) == 0 && len(functions) > 0 {
		for _, function := range functions {
			fn := function
			tools = append(tools, llms.Tool{Type: "function", Function: &fn})
		}
	}
	converted := make([]interactionTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Function == nil {
			continue
		}
		converted = append(converted, interactionTool{Type: "function", Name: tool.Function.Name, Description: tool.Function.Description, Parameters: tool.Function.Parameters})
	}
	return converted
}

// convertInteractionsContextMessagesStateful converts only the messages that
// follow the latest stored interaction, which is what previous_interaction_id
// requires. The signed calls it must recognize live in the earlier messages the
// provider already holds, so the whole transcript is scanned for them first: a
// function_result whose call is missing from that set would otherwise be demoted
// to text even though the provider has the matching signed call on its side.
func convertInteractionsContextMessagesStateful(allMessages, incrementalMessages []messages.Message) ([]interactionStep, string, error) {
	nativeCallIDs := map[string]bool{}
	for _, message := range allMessages {
		for _, raw := range message.InteractionsSteps {
			var step interactionStep
			if err := json.Unmarshal(raw, &step); err != nil {
				// A stored step that no longer decodes is corrupt history. Its
				// call stays unknown, so the result is carried as text rather
				// than failing a request the provider would have accepted.
				continue
			}
			if step.Type == "function_call" {
				nativeCallIDs[strings.TrimSpace(step.ID)] = true
			}
		}
	}
	return convertInteractionsContextMessagesWithCallIDs(incrementalMessages, nativeCallIDs)
}

// convertInteractionsContextMessages converts a complete transcript, for local
// context mode where every turn is resubmitted.
func convertInteractionsContextMessages(contextMessages []messages.Message) ([]interactionStep, string, error) {
	return convertInteractionsContextMessagesWithCallIDs(contextMessages, nil)
}

// convertInteractionsContextMessagesWithCallIDs translates the durable
// transcript into the native StepList the Interactions API accepts. nativeCallIDs
// seeds the set of function calls already known to be signed; it may be nil when
// the transcript being converted is self-contained.
//
// Stored steps are authoritative: they are what the provider returned, including
// the thought signatures it validates on replay. Transcript turns without stored
// steps came from another provider or from a build that predates this transport,
// and two parts of them cannot be sent as native steps:
//
//   - function calls are unsigned, and Gemini rejects a replayed call whose
//     thought signature is missing;
//   - thought steps are synthesized from reasoning text only, and the API never
//     emits an unsigned thought step.
//
// Both are carried as text instead, so the exchange stays visible to the model
// without the request being rejected.
func convertInteractionsContextMessagesWithCallIDs(contextMessages []messages.Message, nativeCallIDs map[string]bool) ([]interactionStep, string, error) {
	standard := messages.ConvertMessageList(contextMessages)
	steps := make([]interactionStep, 0, len(standard))
	var systemInstruction string
	if nativeCallIDs == nil {
		nativeCallIDs = map[string]bool{}
	}
	for i, message := range standard {
		if message.Role == llms.ChatMessageTypeSystem {
			text := interactionTextFromParts(message.Parts)
			if text != "" {
				if systemInstruction != "" {
					systemInstruction += "\n\n"
				}
				systemInstruction += text
			}
			continue
		}
		if i < len(contextMessages) && len(contextMessages[i].InteractionsSteps) > 0 {
			for _, raw := range contextMessages[i].InteractionsSteps {
				var step interactionStep
				if err := json.Unmarshal(raw, &step); err != nil {
					return nil, "", fmt.Errorf("decode stored interaction step: %w", err)
				}
				if step.Type == "function_call" {
					nativeCallIDs[strings.TrimSpace(step.ID)] = true
				}
				steps = append(steps, step)
			}
			continue
		}
		converted, instruction, err := convertInteractionsMessages([]llms.MessageContent{message})
		if err != nil {
			return nil, "", err
		}
		steps = append(steps, carryUnsignedToolStepsAsText(converted, nativeCallIDs)...)
		if instruction != "" {
			if systemInstruction != "" {
				systemInstruction += "\n\n"
			}
			systemInstruction += instruction
		}
	}
	return steps, systemInstruction, nil
}

// carryUnsignedToolStepsAsText rewrites the function_call steps of a transcript
// turn that has no stored native steps, along with any function_result whose call
// was not replayed from stored steps. Gemini validates every replayed call against
// the thought signature it was returned with, and a result without its call is
// invalid too, so both become text: the model still sees which tools ran and what
// they returned.
func carryUnsignedToolStepsAsText(steps []interactionStep, nativeCallIDs map[string]bool) []interactionStep {
	unsignedCall, unsignedResult := false, false
	for _, step := range steps {
		switch step.Type {
		case "function_call":
			unsignedCall = true
		case "function_result":
			unsignedResult = unsignedResult || !nativeCallIDs[strings.TrimSpace(step.CallID)]
		}
	}
	if !unsignedCall && !unsignedResult {
		return steps
	}
	kept := make([]interactionStep, 0, len(steps))
	calls := make([]string, 0, len(steps))
	results := make([]string, 0, len(steps))
	for _, step := range steps {
		switch step.Type {
		case "function_call":
			calls = append(calls, fmt.Sprintf("%s(%s)", step.Name, interactionStepArgumentsText(step)))
		case "function_result":
			if nativeCallIDs[strings.TrimSpace(step.CallID)] {
				kept = append(kept, step)
				continue
			}
			results = append(results, fmt.Sprintf("%s -> %s", step.Name, interactionStepResultText(step)))
		default:
			kept = append(kept, step)
		}
	}
	if len(calls) > 0 {
		kept = append(kept, interactionStep{Type: "model_output", Content: []interactionContent{{Type: "text", Text: "Called " + strings.Join(calls, "; ")}}})
	}
	if len(results) > 0 {
		kept = append(kept, interactionStep{Type: "user_input", Content: []interactionContent{{Type: "text", Text: "Tool results: " + strings.Join(results, "; ")}}})
	}
	return kept
}

func interactionStepArgumentsText(step interactionStep) string {
	arguments := strings.TrimSpace(string(step.Arguments))
	if arguments == "" {
		return "{}"
	}
	return arguments
}

func interactionStepResultText(step interactionStep) string {
	if step.Result == nil {
		return ""
	}
	if text, ok := step.Result.(string); ok {
		return text
	}
	return fmt.Sprintf("%v", step.Result)
}

func convertInteractionsMessages(input []llms.MessageContent) ([]interactionStep, string, error) {
	steps := make([]interactionStep, 0, len(input))
	var systemInstruction string
	for _, message := range input {
		if message.Role == llms.ChatMessageTypeSystem {
			text := interactionTextFromParts(message.Parts)
			if text != "" {
				if systemInstruction != "" {
					systemInstruction += "\n\n"
				}
				systemInstruction += text
			}
			continue
		}
		if message.Role == llms.ChatMessageTypeTool || message.Role == llms.ChatMessageTypeFunction {
			for _, part := range message.Parts {
				response, ok := part.(llms.ToolCallResponse)
				if !ok {
					return nil, "", fmt.Errorf("interactions tool message part must be llms.ToolCallResponse, got %T", part)
				}
				steps = append(steps, interactionStep{Type: "function_result", CallID: response.ToolCallID, Name: response.Name, Result: response.Content})
			}
			continue
		}
		role := "user"
		if message.Role == llms.ChatMessageTypeAI {
			role = "assistant"
		}
		content, calls, err := interactionParts(message.Parts)
		if err != nil {
			return nil, "", err
		}
		if role == "assistant" {
			if len(content) > 0 {
				steps = append(steps, interactionStep{Type: "model_output", Content: content})
			}
			for _, call := range calls {
				steps = append(steps, call)
			}
		} else if len(content) > 0 || len(calls) == 0 {
			steps = append(steps, interactionStep{Type: "user_input", Content: content})
			for _, call := range calls {
				steps = append(steps, call)
			}
		}
	}
	return steps, systemInstruction, nil
}

func interactionParts(parts []llms.ContentPart) ([]interactionContent, []interactionStep, error) {
	content := make([]interactionContent, 0)
	calls := make([]interactionStep, 0)
	for _, part := range parts {
		switch typed := part.(type) {
		case llms.TextContent:
			content = append(content, interactionContent{Type: "text", Text: typed.Text})
		case llms.ImageURLContent:
			if strings.HasPrefix(strings.ToLower(typed.URL), "data:") {
				mime, data, ok := decodeInteractionDataURI(typed.URL)
				if ok {
					content = append(content, interactionContent{Type: "image", Data: data, MIMEType: mime})
					continue
				}
			}
			content = append(content, interactionContent{Type: "image", URI: typed.URL})
		case llms.BinaryContent:
			mime := strings.TrimSpace(typed.MIMEType)
			kind, err := interactionContentType(mime)
			if err != nil {
				return nil, nil, err
			}
			content = append(content, interactionContent{Type: kind, Data: base64.StdEncoding.EncodeToString(typed.Data), MIMEType: mime})
		case llms.ToolCall:
			if typed.FunctionCall == nil {
				continue
			}
			callID := strings.TrimSpace(typed.ID)
			if callID == "" {
				callID = fmt.Sprintf("interactions_call_%d", len(calls))
			}
			args := normalizeCompatibleToolArguments(typed.FunctionCall.Arguments)
			if !json.Valid([]byte(args)) {
				args = "{}"
			}
			calls = append(calls, interactionStep{Type: "function_call", ID: callID, Name: typed.FunctionCall.Name, Arguments: json.RawMessage(args)})
		default:
			return nil, nil, fmt.Errorf("unsupported interactions content part type: %T", part)
		}
	}
	return content, calls, nil
}

// interactionContentType maps an inline binary part onto one of the native
// Interactions content types: the API models image, audio, video, and document
// blocks separately, so the MIME type decides the step type. Anything outside
// that set is reported instead of being sent as a wrongly typed block.
func interactionContentType(mimeType string) (string, error) {
	mime := strings.ToLower(strings.TrimSpace(mimeType))
	switch {
	case strings.HasPrefix(mime, "image/"):
		return "image", nil
	case strings.HasPrefix(mime, "audio/"):
		return "audio", nil
	case strings.HasPrefix(mime, "video/"):
		return "video", nil
	case mime == "application/pdf", mime == "text/csv":
		return "document", nil
	default:
		return "", fmt.Errorf("unsupported Gemini Interactions binary MIME type: %s", strings.TrimSpace(mimeType))
	}
}

func interactionTextFromParts(parts []llms.ContentPart) string {
	var out strings.Builder
	for _, part := range parts {
		if typed, ok := part.(llms.TextContent); ok {
			if out.Len() > 0 {
				out.WriteString("\n\n")
			}
			out.WriteString(typed.Text)
		}
	}
	return out.String()
}

func decodeInteractionDataURI(value string) (mime, data string, ok bool) {
	parts := strings.SplitN(value, ",", 2)
	if len(parts) != 2 || !strings.HasPrefix(parts[0], "data:") {
		return "", "", false
	}
	header := strings.TrimPrefix(parts[0], "data:")
	if !strings.HasSuffix(header, ";base64") {
		return "", "", false
	}
	return strings.TrimSuffix(header, ";base64"), parts[1], true
}
