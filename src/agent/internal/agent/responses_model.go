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
	modelAPIModeChatCompletions   = "chat_completions"
	modelAPIModeResponses         = "responses"
	modelAPIModeResponsesStateful = "responses_stateful"
)

const (
	responsesContextManagementCompaction = "compaction"
	responsesContextManagementArkEdits   = "ark_context_edit"
	responsesTruncationAuto              = "auto"
	responsesTruncationDisabled          = "disabled"
)

type responsesDialect string

const (
	responsesDialectOpenAI     responsesDialect = "openai"
	responsesDialectOpenRouter responsesDialect = "openrouter"
	responsesDialectVolcengine responsesDialect = "volcengine"
	responsesDialectDeepSeek   responsesDialect = "deepseek"
)

func normalizeResponsesContextManagement(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", "none", "disabled":
		return ""
	case responsesContextManagementCompaction:
		return responsesContextManagementCompaction
	case responsesContextManagementArkEdits, "ark", "volcengine", "ark-edits":
		return responsesContextManagementArkEdits
	default:
		return ""
	}
}

func normalizeResponsesTruncation(value string) string {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "", responsesTruncationDisabled:
		return ""
	case responsesTruncationAuto:
		return responsesTruncationAuto
	default:
		return ""
	}
}

func normalizeModelAPIMode(mode string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", "chat", "chat_completions", "chat-completions":
		return modelAPIModeChatCompletions
	case "responses":
		return modelAPIModeResponses
	case "responses_stateful", "responses-stateful", "responses_provider":
		return modelAPIModeResponsesStateful
	case "interactions":
		return modelAPIModeInteractions
	case "interactions_stateful", "interactions-stateful", "interactions_provider":
		return modelAPIModeInteractionsStateful
	default:
		return ""
	}
}

// responsesModel is deliberately separate from openAICompatibleModel. The two
// APIs have different item and streaming protocols even though their auth and
// base URL conventions are compatible.
type responsesModel struct {
	baseURL                  string
	model                    string
	token                    string
	httpClient               *http.Client
	rawLogger                RawHTTPLogger
	sessionIDProvider        func() string
	reasoningEffort          string
	temperature              *float64
	ignoreTemperature        bool
	routerMetadata           bool
	providerManagedContext   bool
	contextManagement        string
	compactThreshold         int
	contextEditTrigger       int
	contextEditKeep          int
	contextEditClearThinking bool
	truncation               string
	include                  []string
	dialect                  responsesDialect
}

type responsesModelOptions struct {
	rawLogger                RawHTTPLogger
	sessionIDProvider        func() string
	reasoningEffort          string
	temperature              *float64
	ignoreTemperature        bool
	routerMetadata           bool
	providerManagedContext   bool
	contextManagement        string
	compactThreshold         int
	contextEditTrigger       int
	contextEditKeep          int
	contextEditClearThinking bool
	truncation               string
	include                  []string
	dialect                  responsesDialect
}

func newResponsesModel(baseURL, model, token string, httpClient *http.Client, opts responsesModelOptions) llms.Model {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	dialect := opts.dialect
	if dialect == "" {
		dialect = responsesDialectOpenAI
	}
	return &responsesModel{
		baseURL:                  strings.TrimRight(baseURL, "/"),
		model:                    model,
		token:                    token,
		httpClient:               httpClient,
		rawLogger:                opts.rawLogger,
		sessionIDProvider:        opts.sessionIDProvider,
		reasoningEffort:          strings.TrimSpace(opts.reasoningEffort),
		temperature:              opts.temperature,
		ignoreTemperature:        opts.ignoreTemperature,
		routerMetadata:           opts.routerMetadata,
		providerManagedContext:   opts.providerManagedContext,
		contextManagement:        normalizeResponsesContextManagement(opts.contextManagement),
		compactThreshold:         opts.compactThreshold,
		contextEditTrigger:       opts.contextEditTrigger,
		contextEditKeep:          opts.contextEditKeep,
		contextEditClearThinking: opts.contextEditClearThinking,
		truncation:               normalizeResponsesTruncation(opts.truncation),
		include:                  append([]string(nil), opts.include...),
		dialect:                  dialect,
	}
}

func (m *responsesModel) Call(ctx context.Context, prompt string, options ...llms.CallOption) (string, error) {
	return llms.GenerateFromSinglePrompt(ctx, m, prompt, options...)
}

type responsesRequest struct {
	Model              string               `json:"model"`
	Instructions       string               `json:"instructions,omitempty"`
	PreviousResponseID string               `json:"previous_response_id,omitempty"`
	Input              []responsesInputItem `json:"input"`
	Tools              []responsesTool      `json:"tools,omitempty"`
	ToolChoice         any                  `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool                `json:"parallel_tool_calls,omitempty"`
	Store              *bool                `json:"store,omitempty"`
	Stream             bool                 `json:"stream,omitempty"`
	MaxOutputTokens    int                  `json:"max_output_tokens,omitempty"`
	Temperature        *float64             `json:"temperature,omitempty"`
	Reasoning          *responsesReasoning  `json:"reasoning,omitempty"`
	Text               *responsesTextConfig `json:"text,omitempty"`
	ContextManagement  any                  `json:"context_management,omitempty"`
	Include            []string             `json:"include,omitempty"`
	Truncation         string               `json:"truncation,omitempty"`
}

type responsesContextManagement struct {
	Type             string `json:"type"`
	CompactThreshold int    `json:"compact_threshold,omitempty"`
}

type responsesArkContextManagement struct {
	Edits []responsesArkContextEdit `json:"edits"`
}

type responsesArkContextEdit struct {
	Type    string                   `json:"type"`
	Keep    *responsesArkContextKeep `json:"keep,omitempty"`
	Trigger *responsesArkContextKeep `json:"trigger,omitempty"`
}

type responsesArkContextKeep struct {
	Type  string `json:"type,omitempty"`
	Value int    `json:"value,omitempty"`
}

type responsesInputItem struct {
	Type      string `json:"type,omitempty"`
	Role      string `json:"role,omitempty"`
	Phase     string `json:"phase,omitempty"`
	Content   any    `json:"content,omitempty"`
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
	Output    string `json:"output,omitempty"`
	raw       json.RawMessage
}

func (i responsesInputItem) MarshalJSON() ([]byte, error) {
	if len(i.raw) != 0 {
		return i.raw, nil
	}
	type wireItem responsesInputItem
	return json.Marshal(wireItem(i))
}

type responsesInputContent struct {
	Type       string               `json:"type"`
	Text       string               `json:"text,omitempty"`
	ImageURL   string               `json:"image_url,omitempty"`
	Detail     string               `json:"detail,omitempty"`
	FileData   string               `json:"file_data,omitempty"`
	Filename   string               `json:"filename,omitempty"`
	InputAudio *responsesInputAudio `json:"input_audio,omitempty"`
}

type responsesInputAudio struct {
	Data   string `json:"data"`
	Format string `json:"format"`
}

type responsesTool struct {
	Type        string `json:"type"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      bool   `json:"strict,omitempty"`
}

type responsesReasoning struct {
	Effort string `json:"effort,omitempty"`
}

type responsesTextConfig struct {
	Format map[string]string `json:"format,omitempty"`
}

type responsesResponse struct {
	ID                string                     `json:"id,omitempty"`
	Status            string                     `json:"status,omitempty"`
	Output            []responsesOutputItem      `json:"output,omitempty"`
	Usage             *responsesUsage            `json:"usage,omitempty"`
	Error             *responsesError            `json:"error,omitempty"`
	ErrorType         string                     `json:"error_type,omitempty"`
	IncompleteDetails *responsesIncompleteDetail `json:"incomplete_details,omitempty"`
}

type responsesError struct {
	Code         json.RawMessage `json:"code,omitempty"`
	Type         string          `json:"type,omitempty"`
	Message      string          `json:"message,omitempty"`
	Param        any             `json:"param,omitempty"`
	TopLevelType string          `json:"-"`
}

type responsesIncompleteDetail struct {
	Reason string `json:"reason,omitempty"`
}

type responsesOutputItem struct {
	ID        string                   `json:"id,omitempty"`
	Type      string                   `json:"type,omitempty"`
	Role      string                   `json:"role,omitempty"`
	Phase     string                   `json:"phase,omitempty"`
	Status    string                   `json:"status,omitempty"`
	Content   []responsesOutputContent `json:"content,omitempty"`
	CallID    string                   `json:"call_id,omitempty"`
	Name      string                   `json:"name,omitempty"`
	Arguments string                   `json:"arguments,omitempty"`
	raw       json.RawMessage
}

func (i *responsesOutputItem) UnmarshalJSON(data []byte) error {
	type wireItem responsesOutputItem
	var decoded wireItem
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*i = responsesOutputItem(decoded)
	i.raw = append(i.raw[:0], data...)
	return nil
}

type responsesOutputContent struct {
	Type string `json:"type,omitempty"`
	Text string `json:"text,omitempty"`
}

type responsesUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	TotalTokens        int `json:"total_tokens"`
	InputTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details,omitempty"`
}

func (u *responsesUsage) generationInfo() map[string]any {
	if u == nil {
		return nil
	}
	info := map[string]any{
		"prompt_tokens":     u.InputTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      u.TotalTokens,
	}
	if u.InputTokensDetails != nil {
		info["cached_tokens"] = u.InputTokensDetails.CachedTokens
	}
	if u.OutputTokensDetails != nil && u.OutputTokensDetails.ReasoningTokens > 0 {
		info["reasoning_tokens"] = u.OutputTokensDetails.ReasoningTokens
	}
	return info
}

func (m *responsesModel) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	return m.generateContent(ctx, messages, nil, "", "", options...)
}

// GenerateContentFromMessageList preserves opaque reasoning items that the
// Responses API requires callers to include again when they keep context
// locally with store=false. Other model implementations use GenerateContent.
func (m *responsesModel) GenerateContentFromMessageList(ctx context.Context, contextMessages []messages.Message, options ...llms.CallOption) (*llms.ContentResponse, error) {
	if m.providerManagedContext {
		instructions, previousResponseID, incrementalMessages := providerManagedContextInput(contextMessages)
		input, err := convertResponsesContextInput(incrementalMessages)
		if err != nil {
			return nil, err
		}
		response, err := m.generateContentWithInput(ctx, input, instructions, previousResponseID, options...)
		if err == nil || previousResponseID == "" || !shouldRetryResponsesWithoutPreviousID(err) {
			return response, err
		}
		fallbackMessages := make([]messages.Message, 0, len(contextMessages))
		for _, message := range contextMessages {
			if message.Role != messages.MessageRoleSystem {
				fallbackMessages = append(fallbackMessages, message)
			}
		}
		fallbackInput, fallbackErr := convertResponsesContextInput(fallbackMessages)
		if fallbackErr != nil {
			return nil, fallbackErr
		}
		return m.generateContentWithInput(ctx, fallbackInput, instructions, "", options...)
	}
	input, err := convertResponsesContextInput(contextMessages)
	if err != nil {
		return nil, err
	}
	return m.generateContentWithInput(ctx, input, "", "", options...)
}

func shouldRetryResponsesWithoutPreviousID(err error) bool {
	var providerErr *ProviderHTTPError
	if !errors.As(err, &providerErr) {
		return false
	}
	switch providerErr.StatusCode {
	case http.StatusBadRequest, http.StatusNotFound, http.StatusGone:
	default:
		return false
	}
	text := strings.ToLower(strings.TrimSpace(providerErr.ProviderCode + " " + providerErr.Message))
	if text == "" {
		text = strings.ToLower(providerErr.Body)
	}
	return strings.Contains(text, "previous_response_id") ||
		strings.Contains(text, "previous response") ||
		strings.Contains(text, "response not found") ||
		strings.Contains(text, "response expired")
}

func (m *responsesModel) generateContent(ctx context.Context, messages []llms.MessageContent, reasoningItems [][]json.RawMessage, instructions, previousResponseID string, options ...llms.CallOption) (*llms.ContentResponse, error) {
	input, err := convertResponsesInput(messages, reasoningItems)
	if err != nil {
		return nil, err
	}
	return m.generateContentWithInput(ctx, input, instructions, previousResponseID, options...)
}

func (m *responsesModel) generateContentWithInput(ctx context.Context, input []responsesInputItem, instructions, previousResponseID string, options ...llms.CallOption) (*llms.ContentResponse, error) {
	callStarted := time.Now()
	callOpts := llms.CallOptions{}
	for _, option := range options {
		option(&callOpts)
	}

	requestModel := firstNonEmpty(callOpts.Model, m.model)
	payload := responsesRequest{
		Model:              requestModel,
		Instructions:       instructions,
		PreviousResponseID: previousResponseID,
		Input:              input,
		Tools:              convertResponsesTools(callOpts.Tools, callOpts.Functions),
		ToolChoice:         normalizeResponsesToolChoice(callOpts.ToolChoice, callOpts.FunctionCallBehavior),
		// The agent loop executes at most one tool call per iteration
		// (choiceWithOnlyToolCall keeps the first valid call and drops the rest).
		// Parallel calls would leave the dropped ones without a matching
		// function_call_output item, which the Responses API rejects on the next
		// turn, so disabling them is required rather than merely conservative.
		ParallelToolCalls: boolPtr(false),
		Stream:            callOpts.StreamingFunc != nil || callOpts.StreamingReasoningFunc != nil,
		MaxOutputTokens:   callOpts.MaxTokens,
	}
	// OpenRouter and DeepSeek expose stateless Responses endpoints. Omit both
	// state fields instead of relying on their compatibility layers to ignore
	// them. OpenAI and Ark use store consistently for local versus
	// provider-managed context.
	if m.dialect != responsesDialectOpenRouter && m.dialect != responsesDialectDeepSeek {
		store := m.providerManagedContext
		payload.Store = &store
	} else {
		payload.PreviousResponseID = ""
	}
	// DeepSeek always enables parallel tool calling and ignores this field.
	// Omitting it makes the wire request accurately describe the provider's
	// behavior; the agent loop separately retains only the executed call when it
	// persists stateless Responses output for the next turn.
	if m.dialect == responsesDialectDeepSeek {
		payload.ParallelToolCalls = nil
	}
	// Ark supports its own object-shaped context_management edits, while
	// OpenRouter is stateless. Neither uses the OpenAI compaction array
	// represented by this configuration. OpenRouter does support the standard
	// truncation field; Ark does not use this OpenAI request shape.
	if m.dialect == responsesDialectOpenAI || m.dialect == responsesDialectOpenRouter {
		payload.Truncation = m.truncation
	}
	if m.dialect == responsesDialectOpenAI && m.contextManagement == responsesContextManagementCompaction {
		payload.ContextManagement = []responsesContextManagement{{
			Type:             responsesContextManagementCompaction,
			CompactThreshold: m.compactThreshold,
		}}
	}
	if m.dialect == responsesDialectVolcengine && m.contextManagement == responsesContextManagementArkEdits {
		trigger := m.contextEditTrigger
		if trigger <= 0 {
			trigger = 10
		}
		keep := m.contextEditKeep
		if keep <= 0 {
			keep = 3
		}
		edits := []responsesArkContextEdit{{
			Type:    "clear_tool_uses",
			Keep:    &responsesArkContextKeep{Type: "tool_uses", Value: keep},
			Trigger: &responsesArkContextKeep{Type: "tool_uses", Value: trigger},
		}}
		if m.contextEditClearThinking {
			edits = append(edits, responsesArkContextEdit{
				Type: "clear_thinking",
				Keep: &responsesArkContextKeep{Type: "thinking_turns", Value: 2},
			})
		}
		payload.ContextManagement = responsesArkContextManagement{Edits: edits}
	}
	if len(m.include) > 0 && m.dialect != responsesDialectDeepSeek {
		payload.Include = append([]string(nil), m.include...)
	}
	if !m.ignoreTemperature {
		payload.Temperature = m.temperature
		if payload.Temperature == nil && callOpts.Temperature != 0 {
			payload.Temperature = &callOpts.Temperature
		}
	}
	if m.reasoningEffort != "" {
		payload.Reasoning = &responsesReasoning{Effort: m.reasoningEffort}
	}
	if callOpts.JSONMode {
		payload.Text = &responsesTextConfig{Format: map[string]string{"type": "json_object"}}
	}

	generationInfo := map[string]any{
		"llm_stream":        payload.Stream,
		"llm_request_bytes": 0,
	}
	payloadBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal responses request: %w", err)
	}
	generationInfo["llm_request_bytes"] = len(payloadBytes)
	ctx = m.withRawHTTPLogFileTime(ctx)
	_ = m.logRawHTTP(ctx, requestModel, "request", 0, string(payloadBytes))

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/responses", bytes.NewReader(payloadBytes))
	if err != nil {
		_ = m.logRawHTTP(ctx, requestModel, "response", 0, "create request error: "+err.Error())
		return nil, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if payload.Stream {
		req.Header.Set("Accept", "text/event-stream")
	}
	if m.token != "" {
		req.Header.Set("Authorization", "Bearer "+m.token)
	}
	if m.sessionIDProvider != nil {
		if sid := strings.TrimSpace(m.sessionIDProvider()); sid != "" {
			if len(sid) > openRouterSessionIDMaxLen {
				sid = sid[:openRouterSessionIDMaxLen]
			}
			req.Header.Set("x-session-id", sid)
		}
	}
	if m.routerMetadata {
		req.Header.Set("X-OpenRouter-Metadata", "enabled")
	}

	doStart := time.Now()
	resp, err := m.httpClient.Do(req)
	generationInfo["llm_http_to_headers_ms"] = time.Since(doStart).Milliseconds()
	if err != nil {
		_ = m.logRawHTTP(ctx, requestModel, "response", 0, "transport error: "+err.Error())
		return nil, fmt.Errorf("send request: %w", err)
	}
	defer resp.Body.Close()
	generationInfo["llm_http_status"] = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		_ = m.logRawHTTP(ctx, requestModel, "response", resp.StatusCode, string(body))
		return nil, newProviderHTTPError(resp.StatusCode, body)
	}
	if payload.Stream {
		return m.decodeResponsesStream(ctx, resp.Body, callOpts.StreamingFunc, callOpts.StreamingReasoningFunc, requestModel, resp.StatusCode, callStarted, generationInfo)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		_ = m.logRawHTTP(ctx, requestModel, "response", resp.StatusCode, "read response error: "+err.Error())
		return nil, fmt.Errorf("read response: %w", err)
	}
	_ = m.logRawHTTP(ctx, requestModel, "response", resp.StatusCode, string(body))
	var decoded responsesResponse
	if err := json.Unmarshal(body, &decoded); err != nil {
		return nil, fmt.Errorf("decode responses response: %w", err)
	}
	return responsesContentResponse(decoded, callStarted, generationInfo)
}

// providerManagedContextInput translates the durable local transcript into the
// incremental shape required by previous_response_id. The response that owns
// the latest ResponsesResponseID is already retained by the provider, so it is
// excluded along with all earlier local items. System guidance is always
// lifted into top-level instructions because the Responses API does not carry
// instructions forward through previous_response_id.
func providerManagedContextInput(contextMessages []messages.Message) (instructions, previousResponseID string, incremental []messages.Message) {
	start := 0
	for i, message := range contextMessages {
		if id := strings.TrimSpace(message.ResponsesResponseID); id != "" {
			previousResponseID = id
			start = i + 1
		}
		if message.Role == messages.MessageRoleSystem && strings.TrimSpace(message.Content) != "" {
			if instructions != "" {
				instructions += "\n\n"
			}
			instructions += strings.TrimSpace(message.Content)
		}
	}
	for _, message := range contextMessages[start:] {
		if message.Role == messages.MessageRoleSystem {
			continue
		}
		incremental = append(incremental, message)
	}
	return instructions, previousResponseID, incremental
}

func (m *responsesModel) withRawHTTPLogFileTime(ctx context.Context) context.Context {
	if m == nil || m.rawLogger == nil || !rawHTTPLogEnabled(ctx) {
		return ctx
	}
	return m.rawLogger.BeginScope(ctx)
}

func (m *responsesModel) logRawHTTP(ctx context.Context, modelName, kind string, statusCode int, raw string) error {
	if m == nil || m.rawLogger == nil || !rawHTTPLogEnabled(ctx) {
		return nil
	}
	return m.rawLogger.Log(ctx, RawHTTPLogEntry{Model: modelName, Kind: kind, StatusCode: statusCode, Raw: raw})
}

func convertResponsesInput(messages []llms.MessageContent, reasoningItems ...[][]json.RawMessage) ([]responsesInputItem, error) {
	var perMessageReasoning [][]json.RawMessage
	if len(reasoningItems) > 0 {
		perMessageReasoning = reasoningItems[0]
	}
	return convertResponsesInputWithMetadata(messages, perMessageReasoning, nil, nil)
}

func convertResponsesContextInput(contextMessages []messages.Message) ([]responsesInputItem, error) {
	standardMessages := messages.ConvertMessageList(contextMessages)
	reasoningItems := make([][]json.RawMessage, len(contextMessages))
	outputItems := make([][]json.RawMessage, len(contextMessages))
	phases := make([]string, len(contextMessages))
	for i := range contextMessages {
		for _, item := range contextMessages[i].ResponsesReasoningItems {
			if len(item) != 0 {
				reasoningItems[i] = append(reasoningItems[i], append(json.RawMessage(nil), item...))
			}
		}
		for _, item := range contextMessages[i].ResponsesOutputItems {
			if len(item) != 0 {
				outputItems[i] = append(outputItems[i], append(json.RawMessage(nil), item...))
			}
		}
		phases[i] = contextMessages[i].ResponsesAssistantPhase
	}
	return convertResponsesInputWithMetadata(standardMessages, reasoningItems, outputItems, phases)
}

func convertResponsesInputWithMetadata(messages []llms.MessageContent, perMessageReasoning, perMessageOutputItems [][]json.RawMessage, phases []string) ([]responsesInputItem, error) {
	items := make([]responsesInputItem, 0, len(messages))
	for messageIndex, message := range messages {
		hasRawOutput := messageIndex < len(perMessageOutputItems) && len(perMessageOutputItems[messageIndex]) > 0
		rawHasAssistantMessage := false
		rawFunctionCallIDs := make(map[string]struct{})
		if hasRawOutput {
			for _, item := range perMessageOutputItems[messageIndex] {
				if len(item) != 0 {
					items = append(items, responsesInputItem{raw: append(json.RawMessage(nil), item...)})
					var metadata struct {
						Type   string `json:"type"`
						Role   string `json:"role"`
						CallID string `json:"call_id"`
					}
					if json.Unmarshal(item, &metadata) == nil {
						switch metadata.Type {
						case "message":
							rawHasAssistantMessage = metadata.Role == "" || metadata.Role == "assistant"
						case "function_call":
							if callID := strings.TrimSpace(metadata.CallID); callID != "" {
								rawFunctionCallIDs[callID] = struct{}{}
							}
						}
					}
				}
			}
		}
		if !hasRawOutput && messageIndex < len(perMessageReasoning) {
			for _, item := range perMessageReasoning[messageIndex] {
				if isResponsesReasoningItem(item) {
					items = append(items, responsesInputItem{raw: append(json.RawMessage(nil), item...)})
				}
			}
		}
		if message.Role == llms.ChatMessageTypeTool || message.Role == llms.ChatMessageTypeFunction {
			for partIndex, part := range message.Parts {
				response, ok := part.(llms.ToolCallResponse)
				if !ok {
					return nil, fmt.Errorf("responses tool message part %d must be llms.ToolCallResponse, got %T", partIndex, part)
				}
				callID := strings.TrimSpace(response.ToolCallID)
				if callID == "" {
					callID = fmt.Sprintf("ctx_tool_call_%d_%d", messageIndex, partIndex)
				}
				items = append(items, responsesInputItem{Type: "function_call_output", CallID: callID, Output: response.Content})
			}
			continue
		}

		role := responsesInputRole(message.Role)
		textParts := make([]string, 0, len(message.Parts))
		contentParts := make([]responsesInputContent, 0, len(message.Parts))
		hasRichContent := false
		flushContent := func() {
			if hasRichContent {
				if len(contentParts) > 0 {
					item := responsesInputItem{Role: role, Content: contentParts}
					if messageIndex < len(phases) && role == "assistant" {
						item.Phase = strings.TrimSpace(phases[messageIndex])
					}
					items = append(items, item)
				}
			} else if len(textParts) > 0 {
				item := responsesInputItem{Role: role, Content: strings.Join(textParts, "\n\n")}
				if messageIndex < len(phases) && role == "assistant" {
					item.Phase = strings.TrimSpace(phases[messageIndex])
				}
				items = append(items, item)
			}
			textParts = nil
			contentParts = nil
			hasRichContent = false
		}
		appendRichContent := func(part responsesInputContent) {
			if !hasRichContent {
				for _, text := range textParts {
					contentParts = append(contentParts, responsesInputContent{Type: responsesInputTextType(role), Text: text})
				}
				textParts = nil
				hasRichContent = true
			}
			contentParts = append(contentParts, part)
		}
		for partIndex, part := range message.Parts {
			switch typed := part.(type) {
			case llms.TextContent:
				if rawHasAssistantMessage && role == "assistant" {
					continue
				}
				if hasRichContent {
					contentParts = append(contentParts, responsesInputContent{Type: responsesInputTextType(role), Text: typed.Text})
				} else {
					textParts = append(textParts, typed.Text)
				}
			case llms.ImageURLContent:
				if rawHasAssistantMessage && role == "assistant" {
					continue
				}
				appendRichContent(responsesInputContent{Type: "input_image", ImageURL: typed.URL, Detail: typed.Detail})
			case llms.BinaryContent:
				if rawHasAssistantMessage && role == "assistant" {
					continue
				}
				switch {
				case strings.HasPrefix(strings.ToLower(typed.MIMEType), "image/"):
					appendRichContent(responsesInputContent{Type: "input_image", ImageURL: "data:" + typed.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(typed.Data)})
				case strings.HasPrefix(strings.ToLower(typed.MIMEType), "audio/"):
					appendRichContent(responsesInputContent{Type: "input_audio", InputAudio: &responsesInputAudio{Data: base64.StdEncoding.EncodeToString(typed.Data), Format: audioFormatFromMIME(typed.MIMEType)}})
				default:
					appendRichContent(responsesInputContent{Type: "input_file", FileData: "data:" + typed.MIMEType + ";base64," + base64.StdEncoding.EncodeToString(typed.Data)})
				}
			case llms.ToolCall:
				if typed.FunctionCall == nil {
					return nil, fmt.Errorf("responses tool call %d has no function call", partIndex)
				}
				callID := strings.TrimSpace(typed.ID)
				if callID == "" {
					callID = fmt.Sprintf("ctx_tool_call_%d_%d", messageIndex, partIndex)
				}
				if _, exists := rawFunctionCallIDs[callID]; exists {
					continue
				}
				flushContent()
				items = append(items, responsesInputItem{Type: "function_call", CallID: callID, Name: typed.FunctionCall.Name, Arguments: normalizeCompatibleToolArguments(typed.FunctionCall.Arguments)})
			default:
				return nil, fmt.Errorf("unsupported responses content part type: %T", part)
			}
		}
		flushContent()
	}
	return items, nil
}

func isResponsesReasoningItem(raw json.RawMessage) bool {
	var item struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &item) == nil && item.Type == "reasoning"
}

func responsesInputRole(role llms.ChatMessageType) string {
	switch role {
	case llms.ChatMessageTypeSystem:
		return "system"
	case llms.ChatMessageTypeAI:
		return "assistant"
	default:
		return "user"
	}
}

func responsesInputTextType(role string) string {
	if role == "assistant" {
		return "output_text"
	}
	return "input_text"
}

func convertResponsesTools(tools []llms.Tool, functions []llms.FunctionDefinition) []responsesTool {
	if len(tools) == 0 && len(functions) > 0 {
		for _, function := range functions {
			fn := function
			tools = append(tools, llms.Tool{Type: "function", Function: &fn})
		}
	}
	converted := make([]responsesTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Function == nil {
			continue
		}
		converted = append(converted, responsesTool{
			Type: "function", Name: tool.Function.Name, Description: tool.Function.Description,
			Parameters: tool.Function.Parameters, Strict: tool.Function.Strict,
		})
	}
	return converted
}

func normalizeResponsesToolChoice(choice any, behavior llms.FunctionCallBehavior) any {
	if choice == nil && behavior != "" {
		choice = behavior
	}
	switch typed := choice.(type) {
	case llms.ToolChoice:
		if typed.Function == nil {
			return typed.Type
		}
		return map[string]any{"type": "function", "name": typed.Function.Name}
	case map[string]any:
		if fn, ok := typed["function"].(map[string]any); ok {
			if name, ok := fn["name"].(string); ok {
				return map[string]any{"type": "function", "name": name}
			}
		}
	}
	return choice
}

func responsesContentResponse(decoded responsesResponse, callStarted time.Time, generationInfo map[string]any) (*llms.ContentResponse, error) {
	if strings.EqualFold(strings.TrimSpace(decoded.Status), "failed") {
		return nil, newResponsesProviderError(responsesResponseError(&decoded), "response failed")
	}
	content := ""
	reasoning := ""
	toolCalls := make([]llms.ToolCall, 0)
	for _, item := range decoded.Output {
		switch item.Type {
		case "message":
			content += responsesOutputText(item.Content)
		case "reasoning":
			reasoning += responsesReasoningText(item.Content)
		case "function_call":
			toolCalls = append(toolCalls, llms.ToolCall{ID: item.CallID, Type: "function", FunctionCall: &llms.FunctionCall{Name: item.Name, Arguments: normalizeCompatibleToolArguments(item.Arguments)}})
		}
	}
	addResponsesReasoningItems(generationInfo, decoded.Output)
	addResponsesOutputItems(generationInfo, decoded.Output)
	if phase := responsesAssistantPhase(decoded.Output); phase != "" {
		generationInfo["responses_assistant_phase"] = phase
	}
	if decoded.ID != "" {
		generationInfo["llm_response_id"] = decoded.ID
	}
	if decoded.Status != "" {
		generationInfo["llm_finish_reason"] = decoded.Status
	}
	generationInfo["llm_output_chars"] = len(content)
	generationInfo["llm_tool_call_count"] = len(toolCalls)
	choice := &llms.ContentChoice{Content: content, ReasoningContent: reasoning, StopReason: decoded.Status, ToolCalls: toolCalls}
	if len(toolCalls) > 0 {
		choice.FuncCall = toolCalls[0].FunctionCall
	}
	choice.GenerationInfo = finalizeLLMGenerationInfo(mergeGenerationInfo(decoded.Usage.generationInfo(), generationInfo), callStarted)
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{choice}}, nil
}

func addResponsesOutputItems(generationInfo map[string]any, output []responsesOutputItem) {
	if generationInfo == nil {
		return
	}
	items, _ := generationInfo["responses_output_items"].([]json.RawMessage)
	for _, item := range output {
		if len(item.raw) == 0 {
			continue
		}
		identity := responsesOutputItemIdentity(item)
		duplicate := false
		for index, existing := range items {
			if bytes.Equal(existing, item.raw) {
				duplicate = true
				break
			}
			if identity != "" && responsesRawOutputItemIdentity(existing) == identity {
				items[index] = append(json.RawMessage(nil), item.raw...)
				duplicate = true
				break
			}
		}
		if !duplicate {
			items = append(items, append(json.RawMessage(nil), item.raw...))
		}
	}
	if len(items) != 0 {
		generationInfo["responses_output_items"] = items
	}
}

func responsesOutputItemIdentity(item responsesOutputItem) string {
	if id := strings.TrimSpace(item.ID); id != "" {
		return item.Type + ":id:" + id
	}
	if callID := strings.TrimSpace(item.CallID); callID != "" {
		return item.Type + ":call_id:" + callID
	}
	return ""
}

func responsesRawOutputItemIdentity(raw json.RawMessage) string {
	var item responsesOutputItem
	if json.Unmarshal(raw, &item) != nil {
		return ""
	}
	return responsesOutputItemIdentity(item)
}

func responsesAssistantPhase(output []responsesOutputItem) string {
	for _, item := range output {
		if item.Type == "message" && item.Role == "assistant" && strings.TrimSpace(item.Phase) != "" {
			return strings.TrimSpace(item.Phase)
		}
	}
	return ""
}

func addResponsesReasoningItems(generationInfo map[string]any, output []responsesOutputItem) {
	if generationInfo == nil {
		return
	}
	items, _ := generationInfo["responses_reasoning_items"].([]json.RawMessage)
	for _, item := range output {
		if item.Type != "reasoning" || !isResponsesReasoningItem(item.raw) {
			continue
		}
		duplicate := false
		for _, existing := range items {
			if bytes.Equal(existing, item.raw) {
				duplicate = true
				break
			}
		}
		if !duplicate {
			items = append(items, append(json.RawMessage(nil), item.raw...))
		}
	}
	if len(items) != 0 {
		generationInfo["responses_reasoning_items"] = items
	}
}

func responsesOutputText(content []responsesOutputContent) string {
	var text strings.Builder
	for _, part := range content {
		if part.Type == "output_text" || part.Type == "text" || part.Type == "" {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

func responsesReasoningText(content []responsesOutputContent) string {
	var text strings.Builder
	for _, part := range content {
		if part.Type == "reasoning_text" || part.Type == "summary_text" {
			text.WriteString(part.Text)
		}
	}
	return text.String()
}

type responsesStreamEvent struct {
	Type        string               `json:"type,omitempty"`
	Delta       string               `json:"delta,omitempty"`
	ItemID      string               `json:"item_id,omitempty"`
	OutputIndex int                  `json:"output_index,omitempty"`
	CallID      string               `json:"call_id,omitempty"`
	Name        string               `json:"name,omitempty"`
	Arguments   string               `json:"arguments,omitempty"`
	Item        *responsesOutputItem `json:"item,omitempty"`
	Response    *responsesResponse   `json:"response,omitempty"`
	Error       *responsesError      `json:"error,omitempty"`
	ErrorType   string               `json:"error_type,omitempty"`
	Code        json.RawMessage      `json:"code,omitempty"`
	Message     string               `json:"message,omitempty"`
	Param       any                  `json:"param,omitempty"`
}

type responsesStreamToolCall struct {
	ID        string
	Name      string
	Arguments strings.Builder
}

func (m *responsesModel) decodeResponsesStream(ctx context.Context, body io.Reader, stream func(context.Context, []byte) error, reasoningStream func(context.Context, []byte, []byte) error, requestModel string, statusCode int, callStarted time.Time, generationInfo map[string]any) (*llms.ContentResponse, error) {
	reader := bufio.NewReader(body)
	var content strings.Builder
	var reasoning strings.Builder
	tools := map[string]*responsesStreamToolCall{}
	itemCallIDs := map[string]string{}
	var completed *responsesResponse
	var eventName string
	hadTextDelta := false
	hadReasoningDelta := false
	var rawStream strings.Builder
	defer func() {
		if rawStream.Len() > 0 {
			_ = m.logRawHTTP(ctx, requestModel, "response", statusCode, rawStream.String())
		}
	}()
	for {
		rawLine, readErr := reader.ReadString('\n')
		if rawLine != "" {
			rawLine = strings.TrimSuffix(rawLine, "\n")
			rawLine = strings.TrimSuffix(rawLine, "\r")
		}
		if rawLine != "" {
			rawStream.WriteString(rawLine)
			rawStream.WriteByte('\n')
			line := strings.TrimSpace(rawLine)
			if line == "" {
				eventName = ""
				continue
			}
			if strings.HasPrefix(line, ":") {
				continue
			}
			if strings.HasPrefix(line, "event:") {
				eventName = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
				continue
			}
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			if data == "[DONE]" {
				break
			}
			var event responsesStreamEvent
			if err := json.Unmarshal([]byte(data), &event); err != nil {
				return nil, fmt.Errorf("decode responses stream event: %w", err)
			}
			if event.Type == "" {
				event.Type = eventName
			}
			switch event.Type {
			case "response.output_text.delta", "response.content_part.delta":
				hadTextDelta = true
				content.WriteString(event.Delta)
				if stream != nil && event.Delta != "" {
					if err := stream(ctx, []byte(event.Delta)); err != nil {
						return nil, err
					}
				}
			case "response.reasoning_summary_text.delta", "response.reasoning_text.delta", "response.reasoning.delta":
				hadReasoningDelta = true
				reasoning.WriteString(event.Delta)
				if reasoningStream != nil && event.Delta != "" {
					if err := reasoningStream(ctx, []byte(event.Delta), nil); err != nil {
						return nil, err
					}
				}
			case "response.function_call_arguments.delta":
				call := responsesStreamToolCallForEvent(tools, itemCallIDs, event)
				call.Arguments.WriteString(event.Delta)
			case "response.function_call_arguments.done":
				call := responsesStreamToolCallForEvent(tools, itemCallIDs, event)
				if event.Arguments != "" {
					call.Arguments.Reset()
					call.Arguments.WriteString(event.Arguments)
				}
			case "response.output_item.added", "response.output_item.done":
				if event.Item != nil && event.Type == "response.output_item.done" {
					addResponsesOutputItems(generationInfo, []responsesOutputItem{*event.Item})
					if event.Item.Type == "reasoning" {
						addResponsesReasoningItems(generationInfo, []responsesOutputItem{*event.Item})
						if !hadReasoningDelta {
							fallbackReasoning := responsesReasoningText(event.Item.Content)
							reasoning.WriteString(fallbackReasoning)
							if reasoningStream != nil && fallbackReasoning != "" {
								if err := reasoningStream(ctx, []byte(fallbackReasoning), nil); err != nil {
									return nil, err
								}
							}
						}
					}
					if phase := responsesAssistantPhase([]responsesOutputItem{*event.Item}); phase != "" {
						generationInfo["responses_assistant_phase"] = phase
					}
					if event.Item.Type == "message" && !hadTextDelta {
						fallbackText := responsesOutputText(event.Item.Content)
						content.WriteString(fallbackText)
						if stream != nil && fallbackText != "" {
							if err := stream(ctx, []byte(fallbackText)); err != nil {
								return nil, err
							}
						}
					}
				}
				if event.Item != nil && event.Item.Type == "function_call" {
					callID := firstNonEmpty(event.Item.CallID, event.Item.ID, event.ItemID, fmt.Sprintf("responses_call_%d", event.OutputIndex))
					call := tools[callID]
					if call == nil {
						call = &responsesStreamToolCall{ID: callID}
						tools[callID] = call
					}
					if event.Item.ID != "" {
						itemCallIDs[event.Item.ID] = callID
					}
					call.Name = firstNonEmpty(event.Item.Name, call.Name)
					if event.Item.Arguments != "" {
						call.Arguments.Reset()
						call.Arguments.WriteString(event.Item.Arguments)
					}
				}
			case "response.completed", "response.done", "response.incomplete":
				completed = event.Response
			case "response.failed":
				if event.Response != nil {
					return nil, newResponsesProviderError(responsesResponseError(event.Response), "response failed")
				}
				return nil, newResponsesProviderError(responsesEventError(event), "response failed")
			case "error":
				apiErr := responsesEventError(event)
				if apiErr == nil {
					apiErr = &responsesError{Code: event.Code, Message: event.Message, Param: event.Param}
				}
				return nil, newResponsesProviderError(apiErr, "responses stream error")
			}
		}
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return nil, fmt.Errorf("read responses stream: %w", readErr)
		}
	}
	if completed != nil {
		if completed.ID != "" {
			generationInfo["llm_response_id"] = completed.ID
		}
		if completed.Status != "" {
			generationInfo["llm_finish_reason"] = completed.Status
		}
		// Some compatible gateways send only response.completed. Fill in output
		// text/tool calls from it when no deltas/items were observed.
		if content.Len() == 0 {
			for _, item := range completed.Output {
				if item.Type == "message" {
					fallbackText := responsesOutputText(item.Content)
					content.WriteString(fallbackText)
					if !hadTextDelta && stream != nil && fallbackText != "" {
						if err := stream(ctx, []byte(fallbackText)); err != nil {
							return nil, err
						}
					}
				}
			}
		}
		if reasoning.Len() == 0 {
			for _, item := range completed.Output {
				if item.Type == "reasoning" {
					fallbackReasoning := responsesReasoningText(item.Content)
					reasoning.WriteString(fallbackReasoning)
					if !hadReasoningDelta && reasoningStream != nil && fallbackReasoning != "" {
						if err := reasoningStream(ctx, []byte(fallbackReasoning), nil); err != nil {
							return nil, err
						}
					}
				}
			}
		}
		for _, item := range completed.Output {
			if item.Type != "function_call" {
				continue
			}
			callID := firstNonEmpty(item.CallID, item.ID, fmt.Sprintf("responses_call_%d", len(tools)))
			call := tools[callID]
			if call == nil {
				call = &responsesStreamToolCall{ID: callID}
				tools[callID] = call
			}
			call.Name = firstNonEmpty(item.Name, call.Name)
			if item.Arguments != "" {
				call.Arguments.Reset()
				call.Arguments.WriteString(item.Arguments)
			}
		}
		if completed.Usage != nil {
			generationInfo = mergeGenerationInfo(completed.Usage.generationInfo(), generationInfo)
		}
		addResponsesReasoningItems(generationInfo, completed.Output)
		addResponsesOutputItems(generationInfo, completed.Output)
		if phase := responsesAssistantPhase(completed.Output); phase != "" {
			generationInfo["responses_assistant_phase"] = phase
		}
	}
	toolKeys := make([]string, 0, len(tools))
	for key := range tools {
		toolKeys = append(toolKeys, key)
	}
	sort.Strings(toolKeys)
	toolCalls := make([]llms.ToolCall, 0, len(toolKeys))
	for _, key := range toolKeys {
		call := tools[key]
		toolCalls = append(toolCalls, llms.ToolCall{ID: call.ID, Type: "function", FunctionCall: &llms.FunctionCall{Name: call.Name, Arguments: normalizeCompatibleToolArguments(call.Arguments.String())}})
	}
	choice := &llms.ContentChoice{Content: content.String(), ReasoningContent: reasoning.String(), ToolCalls: toolCalls}
	if completed != nil {
		choice.StopReason = completed.Status
	}
	if len(toolCalls) > 0 {
		choice.FuncCall = toolCalls[0].FunctionCall
	}
	generationInfo["llm_output_chars"] = content.Len()
	generationInfo["llm_tool_call_count"] = len(toolCalls)
	choice.GenerationInfo = finalizeLLMGenerationInfo(generationInfo, callStarted)
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{choice}}, nil
}

func responsesResponseError(response *responsesResponse) *responsesError {
	if response == nil {
		return nil
	}
	apiErr := response.Error
	if strings.TrimSpace(response.ErrorType) == "" {
		return apiErr
	}
	if apiErr == nil {
		return &responsesError{TopLevelType: response.ErrorType}
	}
	cloned := *apiErr
	cloned.TopLevelType = response.ErrorType
	return &cloned
}

func responsesEventError(event responsesStreamEvent) *responsesError {
	apiErr := event.Error
	if apiErr == nil || strings.TrimSpace(event.ErrorType) == "" {
		return apiErr
	}
	cloned := *apiErr
	cloned.TopLevelType = event.ErrorType
	return &cloned
}

func newResponsesProviderError(apiErr *responsesError, fallbackMessage string) *ProviderHTTPError {
	if apiErr == nil {
		apiErr = &responsesError{Message: fallbackMessage}
	} else if strings.TrimSpace(apiErr.Message) == "" {
		cloned := *apiErr
		cloned.Message = fallbackMessage
		apiErr = &cloned
	}
	body, err := json.Marshal(struct {
		Error     *responsesError `json:"error"`
		ErrorType string          `json:"error_type,omitempty"`
	}{Error: apiErr, ErrorType: apiErr.TopLevelType})
	if err != nil {
		body = []byte(`{"error":{"message":"responses request failed"}}`)
	}
	return newProviderHTTPError(responsesErrorHTTPStatus(apiErr), body)
}

func responsesErrorHTTPStatus(apiErr *responsesError) int {
	if apiErr == nil {
		return http.StatusInternalServerError
	}
	code := strings.ToLower(strings.TrimSpace(normalizeProviderErrorCode(apiErr.Code)))
	errorType := strings.ToLower(strings.TrimSpace(firstNonEmpty(apiErr.TopLevelType, apiErr.Type)))
	markers := code + " " + errorType
	switch {
	case strings.Contains(markers, "rate_limit"):
		return http.StatusTooManyRequests
	case strings.Contains(markers, "authentication"), strings.Contains(markers, "invalid_api_key"):
		return http.StatusUnauthorized
	case strings.Contains(markers, "permission"):
		return http.StatusForbidden
	case strings.Contains(markers, "not_found"):
		return http.StatusNotFound
	case strings.Contains(markers, "invalid_request"), strings.Contains(markers, "invalid_prompt"), strings.Contains(markers, "context_length"), strings.Contains(markers, "content_policy"):
		return http.StatusBadRequest
	default:
		return http.StatusInternalServerError
	}
}

func responsesStreamToolCallForEvent(tools map[string]*responsesStreamToolCall, itemCallIDs map[string]string, event responsesStreamEvent) *responsesStreamToolCall {
	key := firstNonEmpty(event.CallID, itemCallIDs[event.ItemID], event.ItemID, fmt.Sprintf("responses_call_%d", event.OutputIndex))
	call := tools[key]
	if call == nil {
		call = &responsesStreamToolCall{ID: key, Name: event.Name}
		tools[key] = call
	}
	if event.Name != "" {
		call.Name = event.Name
	}
	return call
}
