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
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	agentmessages "aiden-agent/internal/agent/messages"
	"aiden-agent/internal/agent/model"
	"aiden-agent/internal/logging"
	"github.com/tmc/langchaingo/llms"
)

const (
	defaultAnthropicBaseURL          = "https://api.anthropic.com/v1"
	anthropicAPIVersion              = "2023-06-01"
	defaultAnthropicStreamMaxRetries = 5
	defaultAnthropicStreamRetryDelay = 2 * time.Second
	defaultAnthropicProtocolRetries  = 3
	anthropicDaemonRawSSELimit       = 128 * 1024
	anthropicRawHTTPRawSSELimit      = 4 * 1024 * 1024
	anthropicEmptyEndTurnMessage     = "end_turn response has no text or tool_use content"
)

type anthropicModel struct {
	baseURL          string
	model            string
	token            string
	useBearerAuth    bool
	httpClient       *http.Client
	rawLogger        RawHTTPLogger
	temperature      *float64
	reasoningEffort  string
	modelSpec        model.ModelSpec
	modelSpecFn      func() model.ModelSpec
	reasoningBudget  int
	streamMaxRetries int
	streamRetryDelay time.Duration
	protocolRetries  int
	protocolDelay    time.Duration
}

type anthropicModelOption func(*anthropicModel)

func withAnthropicRawHTTPLogger(logger RawHTTPLogger) anthropicModelOption {
	return func(m *anthropicModel) { m.rawLogger = logger }
}

func withAnthropicTemperature(temperature *float64) anthropicModelOption {
	return func(m *anthropicModel) { m.temperature = temperature }
}

func withAnthropicBearerAuth() anthropicModelOption {
	return func(m *anthropicModel) { m.useBearerAuth = true }
}

func withAnthropicReasoningEffort(effort string) anthropicModelOption {
	return func(m *anthropicModel) { m.reasoningEffort = strings.TrimSpace(effort) }
}

func withAnthropicReasoningBudget(tokens int) anthropicModelOption {
	return func(m *anthropicModel) {
		if tokens > 0 {
			m.reasoningBudget = tokens
		}
	}
}

// withAnthropicModelSpecFn supplies the live model spec. Reasoning capability
// and provider limits arrive asynchronously from metadata, so the request
// builder reads the latest capability declaration when selecting controls.
func withAnthropicModelSpecFn(fn func() model.ModelSpec) anthropicModelOption {
	return func(m *anthropicModel) { m.modelSpecFn = fn }
}

func withAnthropicStreamRetry(maxRetries int, delay time.Duration) anthropicModelOption {
	return func(m *anthropicModel) {
		if maxRetries >= 0 {
			m.streamMaxRetries = maxRetries
		}
		if delay >= 0 {
			m.streamRetryDelay = delay
		}
	}
}

func withAnthropicProtocolRetry(maxRetries int, delay time.Duration) anthropicModelOption {
	return func(m *anthropicModel) {
		if maxRetries >= 0 {
			m.protocolRetries = maxRetries
		}
		if delay >= 0 {
			m.protocolDelay = delay
		}
	}
}

type anthropicRequest struct {
	Model         string                    `json:"model"`
	System        []anthropicContentBlock   `json:"system,omitempty"`
	Messages      []anthropicRequestMessage `json:"messages"`
	MaxTokens     int                       `json:"max_tokens"`
	StopSequences []string                  `json:"stop_sequences,omitempty"`
	Temperature   *float64                  `json:"temperature,omitempty"`
	TopP          float64                   `json:"top_p,omitempty"`
	TopK          int                       `json:"top_k,omitempty"`
	Tools         []anthropicTool           `json:"tools,omitempty"`
	ToolChoice    any                       `json:"tool_choice,omitempty"`
	Thinking      *anthropicThinking        `json:"thinking,omitempty"`
	OutputConfig  *anthropicOutputConfig    `json:"output_config,omitempty"`
	Stream        bool                      `json:"stream,omitempty"`
}

type anthropicThinking struct {
	Type         string `json:"type"`
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

type anthropicOutputConfig struct {
	Effort string `json:"effort"`
}

type anthropicRequestMessage struct {
	Role    string                  `json:"role"`
	Content []anthropicContentBlock `json:"content"`
}

type anthropicContentBlock struct {
	Type         string                 `json:"type"`
	Text         string                 `json:"text,omitempty"`
	Thinking     string                 `json:"thinking,omitempty"`
	Signature    string                 `json:"signature,omitempty"`
	Source       *anthropicImageSource  `json:"source,omitempty"`
	ID           string                 `json:"id,omitempty"`
	Name         string                 `json:"name,omitempty"`
	Input        any                    `json:"input,omitempty"`
	ToolUseID    string                 `json:"tool_use_id,omitempty"`
	Content      any                    `json:"content,omitempty"`
	IsError      bool                   `json:"is_error,omitempty"`
	CacheControl *anthropicCacheControl `json:"cache_control,omitempty"`
}

type anthropicImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

type anthropicCacheControl struct {
	Type string `json:"type"`
}

type anthropicTool struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	InputSchema any    `json:"input_schema"`
}

type anthropicResponse struct {
	ID         string                  `json:"id"`
	Model      string                  `json:"model"`
	Role       string                  `json:"role"`
	StopReason string                  `json:"stop_reason"`
	Content    []anthropicContentBlock `json:"content"`
	Usage      anthropicUsage          `json:"usage"`
}

type anthropicUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

func (u anthropicUsage) generationInfo() map[string]any {
	promptTokens := u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens
	info := map[string]any{
		"prompt_tokens":     promptTokens,
		"completion_tokens": u.OutputTokens,
		"total_tokens":      promptTokens + u.OutputTokens,
	}
	if u.CacheReadInputTokens > 0 {
		info["cached_tokens"] = u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		info["cache_creation_input_tokens"] = u.CacheCreationInputTokens
	}
	return info
}

func IsAnthropicModel(provider, model string) bool {
	if strings.EqualFold(strings.TrimSpace(provider), "anthropic") {
		return true
	}
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "anthropic/")
}

func newAnthropicModel(baseURL, model, token string, httpClient *http.Client, opts ...anthropicModelOption) llms.Model {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	result := &anthropicModel{
		baseURL:          normalizeAnthropicBaseURL(baseURL),
		model:            strings.TrimSpace(model),
		token:            strings.TrimSpace(token),
		httpClient:       httpClient,
		streamMaxRetries: defaultAnthropicStreamMaxRetries,
		streamRetryDelay: defaultAnthropicStreamRetryDelay,
		protocolRetries:  defaultAnthropicProtocolRetries,
	}
	if spec, ok := LookupModelSpec("anthropic", model); ok {
		result.modelSpec = spec
	}
	for _, option := range opts {
		if option != nil {
			option(result)
		}
	}
	return result
}

func normalizeAnthropicBaseURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return defaultAnthropicBaseURL
	}
	parsed, err := url.Parse(baseURL)
	if err == nil && (parsed.Path == "" || parsed.Path == "/") {
		return baseURL + "/v1"
	}
	return baseURL
}

func (m *anthropicModel) Call(ctx context.Context, prompt string, options ...llms.CallOption) (string, error) {
	return llms.GenerateFromSinglePrompt(ctx, m, prompt, options...)
}

func (m *anthropicModel) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	return m.generateContent(ctx, messages, nil, options...)
}

// GenerateContentFromMessageList preserves signed Anthropic thinking blocks
// that are stored alongside the agent's provider-neutral message history.
// The common LangChain message shape has no field for signatures, so native
// Claude requests need this path to replay them after a tool result.
func (m *anthropicModel) GenerateContentFromMessageList(ctx context.Context, contextMessages []agentmessages.Message, options ...llms.CallOption) (*llms.ContentResponse, error) {
	thinkingBlocks, err := decodeAnthropicThinkingBlocks(contextMessages)
	if err != nil {
		return nil, err
	}
	return m.generateContent(ctx, agentmessages.ConvertMessageList(contextMessages), thinkingBlocks, options...)
}

func (m *anthropicModel) generateContent(ctx context.Context, messages []llms.MessageContent, thinkingBlocks [][]anthropicContentBlock, options ...llms.CallOption) (*llms.ContentResponse, error) {
	callStarted := time.Now()
	callOpts := llms.CallOptions{}
	for _, option := range options {
		option(&callOpts)
	}

	system, converted, err := convertAnthropicMessages(messages, thinkingBlocks)
	if err != nil {
		return nil, err
	}
	request := anthropicRequest{
		Model:         firstNonEmpty(callOpts.Model, m.model),
		System:        system,
		Messages:      mergeConsecutiveAnthropicMessages(converted),
		MaxTokens:     callOpts.MaxTokens,
		StopSequences: callOpts.StopWords,
		TopP:          callOpts.TopP,
		TopK:          callOpts.TopK,
		Tools:         convertAnthropicTools(callOpts.Tools, callOpts.Functions),
		ToolChoice:    convertAnthropicToolChoice(callOpts.ToolChoice, callOpts.FunctionCallBehavior),
		Stream:        callOpts.StreamingFunc != nil || callOpts.StreamingReasoningFunc != nil,
	}
	if len(request.Tools) > 0 {
		request.ToolChoice = disableAnthropicParallelToolUse(request.ToolChoice)
	}
	if request.MaxTokens <= 0 {
		request.MaxTokens = 2048
	}
	if m.temperature != nil {
		request.Temperature = m.temperature
	} else if callOpts.Temperature != 0 {
		request.Temperature = &callOpts.Temperature
	}
	if thinking, outputConfig, enabled := m.anthropicThinkingRequest(request.MaxTokens); enabled && (len(request.Tools) == 0 || m.reasoningSupportsTools()) {
		if err := validateAnthropicThinkingLimit(request.MaxTokens, thinking); err != nil {
			return nil, err
		}
		request.Thinking = thinking
		request.OutputConfig = outputConfig
	}

	payload, err := json.Marshal(request)
	if err != nil {
		return nil, fmt.Errorf("marshal Anthropic request: %w", err)
	}
	ctx = m.withRawHTTPLogScope(ctx)
	streamRetries := 0
	protocolRetries := 0
	nonStreamingFallback := false
	var streamFallbackCause error
	fail := func(err error) error {
		if streamFallbackCause == nil || isAnthropicContextError(err) {
			return err
		}
		logging.Warnf("agent", "anthropic", "non-streaming fallback failed: %v", err)
		return streamFallbackCause
	}
	for {
		_ = m.logRawHTTP(ctx, request.Model, "request", 0, string(payload))

		httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, m.baseURL+"/messages", bytes.NewReader(payload))
		if err != nil {
			_ = m.logRawHTTP(ctx, request.Model, "response", 0, "create request error: "+err.Error())
			return nil, fail(fmt.Errorf("create Anthropic request: %w", err))
		}
		httpRequest.Header.Set("Content-Type", "application/json")
		httpRequest.Header.Set("anthropic-version", anthropicAPIVersion)
		if m.token != "" && m.useBearerAuth {
			httpRequest.Header.Set("Authorization", "Bearer "+m.token)
		} else if m.token != "" {
			httpRequest.Header.Set("x-api-key", m.token)
		}

		response, err := m.httpClient.Do(httpRequest)
		if err != nil {
			_ = m.logRawHTTP(ctx, request.Model, "response", 0, "transport error: "+err.Error())
			return nil, fail(fmt.Errorf("send Anthropic request: %w", err))
		}
		if response.StatusCode != http.StatusOK {
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			_ = m.logRawHTTP(ctx, request.Model, "response", response.StatusCode, string(body))
			return nil, fail(newProviderHTTPError(response.StatusCode, body))
		}

		if request.Stream {
			result, streamErr := m.decodeStreamingResponse(ctx, response.Body, response.Header, request.Model, response.StatusCode, callStarted, &callOpts, request.Thinking != nil)
			response.Body.Close()
			if streamErr == nil {
				return result, nil
			}
			retryLimit := m.streamMaxRetries
			retryDelay := m.streamRetryDelay
			var responseProtocolErr *anthropicResponseProtocolError
			if errors.As(streamErr, &responseProtocolErr) {
				retryLimit = m.protocolRetries
				retryDelay = m.protocolDelay
				if protocolRetries >= retryLimit && shouldRetryAnthropicStreamError(streamErr) &&
					isAnthropicEmptyEndTurnResponseError(streamErr) && !nonStreamingFallback {
					nonStreamingFallback = true
					streamFallbackCause = streamErr
					request.Stream = false
					payload, err = json.Marshal(request)
					if err != nil {
						return nil, fail(fmt.Errorf("marshal Anthropic non-streaming fallback request: %w", err))
					}
					logging.Warnf("agent", "anthropic", "semantic stream retries exhausted; falling back to non-streaming response")
					continue
				}
				if protocolRetries >= retryLimit || !shouldRetryAnthropicStreamError(streamErr) {
					return nil, streamErr
				}
				protocolRetries++
				logging.Warnf("agent", "anthropic", "retrying semantic protocol error (%d/%d): %v", protocolRetries, retryLimit, streamErr)
				if err := waitBeforeAnthropicRetry(ctx, protocolRetries, retryDelay); err != nil {
					return nil, err
				}
				continue
			}
			if streamRetries >= retryLimit || !shouldRetryAnthropicStreamError(streamErr) {
				return nil, streamErr
			}
			streamRetries++
			if err := waitBeforeAnthropicRetry(ctx, streamRetries, retryDelay); err != nil {
				return nil, err
			}
			continue
		}

		body, err := io.ReadAll(response.Body)
		response.Body.Close()
		if err != nil {
			_ = m.logRawHTTP(ctx, request.Model, "response", response.StatusCode, "read response error: "+err.Error())
			return nil, fail(fmt.Errorf("read Anthropic response: %w", err))
		}
		_ = m.logRawHTTP(ctx, request.Model, "response", response.StatusCode, string(body))
		var decoded anthropicResponse
		if err := json.Unmarshal(body, &decoded); err != nil {
			return nil, fail(fmt.Errorf("decode Anthropic response: %w", err))
		}
		normalized, recovery, protocolErr := normalizeAnthropicResponse(decoded, request.Thinking != nil)
		if protocolErr != nil {
			if isAnthropicOutputBudgetError(protocolErr) {
				return nil, fail(protocolErr)
			}
			if protocolRetries >= m.protocolRetries {
				return nil, fail(protocolErr)
			}
			protocolRetries++
			logging.Warnf("agent", "anthropic", "retrying semantic protocol error (%d/%d): %v", protocolRetries, m.protocolRetries, protocolErr)
			if err := waitBeforeAnthropicRetry(ctx, protocolRetries, m.protocolDelay); err != nil {
				return nil, err
			}
			continue
		}
		generationInfo := map[string]any{}
		if nonStreamingFallback {
			generationInfo["llm_anthropic_stream_fallback"] = "non_stream"
		}
		if recovery != "" {
			logging.Warnf("agent", "anthropic", "recovered response %s as user-visible text (response_id=%s)", recovery, decoded.ID)
			generationInfo["llm_anthropic_response_recovery"] = recovery
		}
		result := aggregateAnthropicResponseWithGenerationInfo(normalized, callStarted, generationInfo)
		if nonStreamingFallback {
			if err := emitAnthropicNonStreamingFallback(ctx, &callOpts, result, request.Thinking != nil); err != nil {
				return nil, err
			}
		}
		return result, nil
	}
}

func (m *anthropicModel) reasoningSupportsTools() bool {
	spec := m.reasoningSpecValue()
	return spec != nil && spec.Supported
}

func validateAnthropicThinkingLimit(maxTokens int, thinking *anthropicThinking) error {
	if thinking == nil || thinking.BudgetTokens <= 0 {
		return nil
	}
	if thinking.BudgetTokens >= maxTokens {
		return fmt.Errorf("Anthropic thinking budget (%d) must be less than max_response_tokens (%d); thinking tokens are included in the response limit",
			thinking.BudgetTokens, maxTokens)
	}
	return nil
}

func (m *anthropicModel) anthropicThinkingRequest(maxTokens int) (*anthropicThinking, *anthropicOutputConfig, bool) {
	effort := normalizeAnthropicReasoningEffort(m.reasoningEffort)
	if effort == "none" {
		return nil, nil, false
	}
	spec := m.reasoningSpecValue()
	// An explicit unsupported declaration is authoritative: never send thinking
	// to a model the catalog says has no reasoning controls. A nil spec only
	// means unknown, which still falls through to the adaptive fallback below.
	if spec != nil && !spec.Supported {
		return nil, nil, false
	}
	// An explicit token budget is an advanced override. Models.dev may publish
	// both effort and budget_tokens for newer Claude models; the effort selector
	// remains the normal UI control, but an entered budget must still be honored.
	if m.reasoningBudget > 0 && (spec == nil || spec.BudgetTokensMin > 0 || spec.Mode == "budget_tokens") {
		budget := m.reasoningBudget
		if spec != nil {
			if spec.BudgetTokensMin > 0 && budget < spec.BudgetTokensMin {
				budget = spec.BudgetTokensMin
			}
			if spec.BudgetTokensMax > 0 && budget > spec.BudgetTokensMax {
				budget = spec.BudgetTokensMax
			}
		}
		return &anthropicThinking{Type: "enabled", BudgetTokens: budget}, nil, true
	}
	if effort == "" {
		return nil, nil, false
	}
	if spec != nil && len(spec.Efforts) == 0 && spec.BudgetTokensMin > 0 {
		if spec.BudgetTokensMin >= maxTokens {
			return nil, nil, false
		}
		budget := anthropicReasoningBudget(effort, spec.BudgetTokensMin, spec.BudgetTokensMax)
		if budget <= 0 {
			return nil, nil, false
		}
		if budget >= maxTokens {
			budget = maxTokens - 1
		}
		return &anthropicThinking{Type: "enabled", BudgetTokens: budget}, nil, true
	}
	if spec != nil && len(spec.Efforts) > 0 && !anthropicContainsStringFold(spec.Efforts, effort) {
		return nil, nil, false
	}
	// Adaptive thinking is the current Claude 4.6+ request surface. For an
	// unknown model keep the historical low/medium/high behavior as a
	// best-effort fallback; known budget-only models take the branch above.
	return &anthropicThinking{Type: "adaptive"}, &anthropicOutputConfig{Effort: effort}, true
}

func (m *anthropicModel) reasoningModelSpec() model.ModelSpec {
	if m == nil {
		return model.ModelSpec{}
	}
	if m.modelSpecFn != nil {
		if spec := m.modelSpecFn(); spec.Reasoning != nil || spec.MaxOutput > 0 {
			return spec
		}
	}
	return m.modelSpec
}

func (m *anthropicModel) reasoningSpecValue() *model.ReasoningSpec {
	return m.reasoningModelSpec().Reasoning
}

func anthropicReasoningBudget(effort string, min, max int) int {
	if min <= 0 {
		return 0
	}
	multipliers := map[string]int{"low": 1, "medium": 4, "high": 8, "xhigh": 16, "max": 32}
	budget := min * multipliers[effort]
	if budget == 0 {
		budget = min
	}
	if max > 0 && budget > max {
		budget = max
	}
	return budget
}

func anthropicContainsStringFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}

// decodeAnthropicThinkingBlocks decodes the stored thinking blocks into a slice
// parallel to contextMessages. Position, not assistant ordinal, is the join key:
// convertAnthropicMessages drops turns whose parts are empty, so counting
// assistant messages in the converted output would silently shift every
// subsequent signature onto the wrong turn.
func decodeAnthropicThinkingBlocks(contextMessages []agentmessages.Message) ([][]anthropicContentBlock, error) {
	decoded := make([][]anthropicContentBlock, len(contextMessages))
	found := false
	for index, message := range contextMessages {
		if len(message.AnthropicThinkingBlocks) == 0 {
			continue
		}
		if message.Role != agentmessages.MessageRoleAssistant && message.Role != agentmessages.MessageRoleToolCall {
			continue
		}
		for _, raw := range message.AnthropicThinkingBlocks {
			var block anthropicContentBlock
			if err := json.Unmarshal(raw, &block); err != nil {
				return nil, fmt.Errorf("decode stored Anthropic thinking block: %w", err)
			}
			// An unsigned block cannot be replayed; Anthropic rejects thinking
			// content whose signature is missing.
			if block.Type != "thinking" || strings.TrimSpace(block.Signature) == "" {
				continue
			}
			decoded[index] = append(decoded[index], block)
			found = true
		}
	}
	if !found {
		return nil, nil
	}
	return decoded, nil
}

func isAnthropicContextError(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

func emitAnthropicNonStreamingFallback(ctx context.Context, opts *llms.CallOptions, response *llms.ContentResponse, thinkingEnabled bool) error {
	if opts == nil || response == nil || len(response.Choices) == 0 || response.Choices[0] == nil {
		return nil
	}
	choice := response.Choices[0]
	if thinkingEnabled && opts.StreamingReasoningFunc != nil && choice.ReasoningContent != "" {
		if err := opts.StreamingReasoningFunc(ctx, []byte(choice.ReasoningContent), nil); err != nil {
			return err
		}
	}
	if opts.StreamingFunc != nil && choice.Content != "" {
		return opts.StreamingFunc(ctx, []byte(choice.Content))
	}
	return nil
}

// convertAnthropicMessages converts the provider-neutral message shape to
// Anthropic request messages. thinkingBlocks is indexed by position in
// messages, so a signed thinking block stays bound to the exact assistant turn
// it was produced on even when other turns are dropped for having no content.
func convertAnthropicMessages(messages []llms.MessageContent, thinkingBlocks [][]anthropicContentBlock) ([]anthropicContentBlock, []anthropicRequestMessage, error) {
	var system []anthropicContentBlock
	converted := make([]anthropicRequestMessage, 0, len(messages))
	for messageIndex, message := range messages {
		switch message.Role {
		case llms.ChatMessageTypeSystem:
			blocks, err := convertAnthropicSystemParts(message.Parts)
			if err != nil {
				return nil, nil, err
			}
			system = append(system, blocks...)
		case llms.ChatMessageTypeHuman, llms.ChatMessageTypeGeneric:
			blocks, err := convertAnthropicUserParts(message.Parts)
			if err != nil {
				return nil, nil, err
			}
			if len(blocks) > 0 {
				converted = append(converted, anthropicRequestMessage{Role: "user", Content: blocks})
			}
		case llms.ChatMessageTypeAI:
			blocks, err := convertAnthropicAssistantParts(message.Parts)
			if err != nil {
				return nil, nil, err
			}
			// Thinking must precede the rest of the assistant content. Prepend
			// here so the pairing is decided by original index, before any
			// message is dropped or merged. A turn with no content of its own
			// is dropped along with its thinking: a thinking-only assistant
			// message carries no answer and no tool call, so replaying it would
			// inject an empty turn between two user messages.
			if len(blocks) > 0 {
				if messageIndex < len(thinkingBlocks) && len(thinkingBlocks[messageIndex]) > 0 {
					blocks = append(append([]anthropicContentBlock(nil), thinkingBlocks[messageIndex]...), blocks...)
				}
				converted = append(converted, anthropicRequestMessage{Role: "assistant", Content: blocks})
			}
		case llms.ChatMessageTypeTool, llms.ChatMessageTypeFunction:
			blocks, err := convertAnthropicToolResultParts(message.Parts)
			if err != nil {
				return nil, nil, err
			}
			if len(blocks) > 0 {
				converted = append(converted, anthropicRequestMessage{Role: "user", Content: blocks})
			}
		default:
			return nil, nil, fmt.Errorf("unsupported Anthropic message role: %s", message.Role)
		}
	}
	return system, converted, nil
}

func convertAnthropicSystemParts(parts []llms.ContentPart) ([]anthropicContentBlock, error) {
	blocks := make([]anthropicContentBlock, 0, len(parts))
	for _, part := range parts {
		block, err := anthropicTextBlock(part)
		if err != nil {
			return nil, fmt.Errorf("convert Anthropic system message: %w", err)
		}
		blocks = append(blocks, block)
	}
	return blocks, nil
}

func anthropicTextBlock(part llms.ContentPart) (anthropicContentBlock, error) {
	cacheControl := (*anthropicCacheControl)(nil)
	if cached, ok := part.(llms.CachedContent); ok {
		part = cached.ContentPart
		if cached.CacheControl != nil && strings.TrimSpace(cached.CacheControl.Type) != "" {
			cacheControl = &anthropicCacheControl{Type: cached.CacheControl.Type}
		}
	}
	text, ok := part.(llms.TextContent)
	if !ok {
		return anthropicContentBlock{}, fmt.Errorf("expected text content, got %T", part)
	}
	return anthropicContentBlock{Type: "text", Text: text.Text, CacheControl: cacheControl}, nil
}

func convertAnthropicUserParts(parts []llms.ContentPart) ([]anthropicContentBlock, error) {
	blocks := make([]anthropicContentBlock, 0, len(parts))
	for _, original := range parts {
		part := original
		cacheControl := (*anthropicCacheControl)(nil)
		if cached, ok := part.(llms.CachedContent); ok {
			part = cached.ContentPart
			if cached.CacheControl != nil && strings.TrimSpace(cached.CacheControl.Type) != "" {
				cacheControl = &anthropicCacheControl{Type: cached.CacheControl.Type}
			}
		}
		switch typed := part.(type) {
		case llms.TextContent:
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: typed.Text, CacheControl: cacheControl})
		case llms.BinaryContent:
			if !strings.HasPrefix(typed.MIMEType, "image/") {
				return nil, fmt.Errorf("unsupported Anthropic binary MIME type: %s", typed.MIMEType)
			}
			blocks = append(blocks, anthropicContentBlock{Type: "image", Source: &anthropicImageSource{
				Type: "base64", MediaType: typed.MIMEType, Data: base64.StdEncoding.EncodeToString(typed.Data),
			}, CacheControl: cacheControl})
		case llms.ImageURLContent:
			source, err := anthropicImageSourceFromURL(typed.URL)
			if err != nil {
				return nil, err
			}
			blocks = append(blocks, anthropicContentBlock{Type: "image", Source: source, CacheControl: cacheControl})
		case llms.ToolCallResponse:
			blocks = append(blocks, anthropicContentBlock{Type: "tool_result", ToolUseID: typed.ToolCallID, Content: typed.Content})
		default:
			return nil, fmt.Errorf("unsupported Anthropic user content part: %T", part)
		}
	}
	return blocks, nil
}

func anthropicImageSourceFromURL(value string) (*anthropicImageSource, error) {
	value = strings.TrimSpace(value)
	if strings.HasPrefix(value, "data:") {
		header, data, ok := strings.Cut(value, ",")
		if !ok || !strings.HasSuffix(header, ";base64") {
			return nil, fmt.Errorf("unsupported Anthropic image data URL")
		}
		mediaType := strings.TrimSuffix(strings.TrimPrefix(header, "data:"), ";base64")
		return &anthropicImageSource{Type: "base64", MediaType: mediaType, Data: data}, nil
	}
	if value == "" {
		return nil, fmt.Errorf("Anthropic image URL is empty")
	}
	return &anthropicImageSource{Type: "url", URL: value}, nil
}

func convertAnthropicAssistantParts(parts []llms.ContentPart) ([]anthropicContentBlock, error) {
	blocks := make([]anthropicContentBlock, 0, len(parts))
	for _, part := range parts {
		switch typed := part.(type) {
		case llms.TextContent:
			blocks = append(blocks, anthropicContentBlock{Type: "text", Text: typed.Text})
		case llms.ToolCall:
			if typed.FunctionCall == nil {
				return nil, fmt.Errorf("Anthropic tool call %q has no function", typed.ID)
			}
			var input any = map[string]any{}
			arguments := strings.TrimSpace(typed.FunctionCall.Arguments)
			if arguments != "" {
				if err := json.Unmarshal([]byte(arguments), &input); err != nil {
					return nil, fmt.Errorf("decode Anthropic tool arguments for %q: %w", typed.FunctionCall.Name, err)
				}
			}
			blocks = append(blocks, anthropicContentBlock{
				Type: "tool_use", ID: typed.ID, Name: typed.FunctionCall.Name, Input: input,
			})
		default:
			return nil, fmt.Errorf("unsupported Anthropic assistant content part: %T", part)
		}
	}
	return blocks, nil
}

func convertAnthropicToolResultParts(parts []llms.ContentPart) ([]anthropicContentBlock, error) {
	blocks := make([]anthropicContentBlock, 0, len(parts))
	for _, part := range parts {
		result, ok := part.(llms.ToolCallResponse)
		if !ok {
			return nil, fmt.Errorf("unsupported Anthropic tool result content part: %T", part)
		}
		blocks = append(blocks, anthropicContentBlock{
			Type: "tool_result", ToolUseID: result.ToolCallID, Content: result.Content,
		})
	}
	return blocks, nil
}

func mergeConsecutiveAnthropicMessages(messages []anthropicRequestMessage) []anthropicRequestMessage {
	if len(messages) < 2 {
		return messages
	}
	merged := make([]anthropicRequestMessage, 0, len(messages))
	for _, message := range messages {
		if len(merged) > 0 && merged[len(merged)-1].Role == message.Role {
			combined := append(merged[len(merged)-1].Content, message.Content...)
			merged[len(merged)-1].Content = hoistAnthropicThinkingBlocks(combined)
			continue
		}
		merged = append(merged, message)
	}
	return merged
}

// hoistAnthropicThinkingBlocks moves thinking blocks to the front of an
// assistant content list, preserving their relative order. Merging consecutive
// assistant turns concatenates their content, which would otherwise leave a
// second turn's thinking block sitting after the first turn's text; Anthropic
// requires all thinking to come first.
func hoistAnthropicThinkingBlocks(blocks []anthropicContentBlock) []anthropicContentBlock {
	firstNonThinking := -1
	needsHoist := false
	for index, block := range blocks {
		if block.Type == "thinking" {
			if firstNonThinking >= 0 {
				needsHoist = true
				break
			}
			continue
		}
		if firstNonThinking < 0 {
			firstNonThinking = index
		}
	}
	if !needsHoist {
		return blocks
	}
	hoisted := make([]anthropicContentBlock, 0, len(blocks))
	for _, block := range blocks {
		if block.Type == "thinking" {
			hoisted = append(hoisted, block)
		}
	}
	for _, block := range blocks {
		if block.Type != "thinking" {
			hoisted = append(hoisted, block)
		}
	}
	return hoisted
}

func convertAnthropicTools(tools []llms.Tool, functions []llms.FunctionDefinition) []anthropicTool {
	if len(tools) == 0 && len(functions) > 0 {
		tools = make([]llms.Tool, 0, len(functions))
		for i := range functions {
			function := functions[i]
			tools = append(tools, llms.Tool{Type: "function", Function: &function})
		}
	}
	converted := make([]anthropicTool, 0, len(tools))
	for _, tool := range tools {
		if tool.Function == nil || strings.TrimSpace(tool.Function.Name) == "" {
			continue
		}
		inputSchema := tool.Function.Parameters
		if inputSchema == nil {
			inputSchema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		converted = append(converted, anthropicTool{
			Name: tool.Function.Name, Description: tool.Function.Description, InputSchema: inputSchema,
		})
	}
	return converted
}

func convertAnthropicToolChoice(choice any, behavior llms.FunctionCallBehavior) any {
	if choice == nil && behavior != "" {
		choice = string(behavior)
	}
	switch typed := choice.(type) {
	case nil:
		return nil
	case string:
		switch strings.ToLower(strings.TrimSpace(typed)) {
		case "", "auto":
			return map[string]any{"type": "auto"}
		case "none":
			return map[string]any{"type": "none"}
		case "required", "any":
			return map[string]any{"type": "any"}
		default:
			return nil
		}
	case llms.ToolChoice:
		if typed.Function != nil && strings.TrimSpace(typed.Function.Name) != "" {
			return map[string]any{"type": "tool", "name": typed.Function.Name}
		}
		return convertAnthropicToolChoice(typed.Type, "")
	case map[string]any:
		if function, ok := typed["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok && strings.TrimSpace(name) != "" {
				return map[string]any{"type": "tool", "name": name}
			}
		}
		if name, ok := typed["name"].(string); ok && strings.TrimSpace(name) != "" {
			return map[string]any{"type": "tool", "name": name}
		}
		if kind, ok := typed["type"].(string); ok {
			return convertAnthropicToolChoice(kind, "")
		}
	case map[string]string:
		if name := strings.TrimSpace(typed["name"]); name != "" {
			return map[string]any{"type": "tool", "name": name}
		}
		if kind := strings.TrimSpace(typed["type"]); kind != "" {
			return convertAnthropicToolChoice(kind, "")
		}
	}
	return nil
}

func disableAnthropicParallelToolUse(choice any) map[string]any {
	converted, _ := choice.(map[string]any)
	if converted == nil {
		converted = map[string]any{"type": "auto"}
	} else {
		cloned := make(map[string]any, len(converted)+1)
		for key, value := range converted {
			cloned[key] = value
		}
		converted = cloned
	}
	converted["disable_parallel_tool_use"] = true
	return converted
}

type anthropicResponseProtocolError struct {
	message string
}

func (e *anthropicResponseProtocolError) Error() string {
	return "Anthropic response protocol error: " + e.message
}

func newAnthropicResponseProtocolError(message string) error {
	return &anthropicResponseProtocolError{message: message}
}

func isAnthropicEmptyEndTurnResponseError(err error) bool {
	var protocolErr *anthropicResponseProtocolError
	return errors.As(err, &protocolErr) && protocolErr.message == anthropicEmptyEndTurnMessage
}

// anthropicOutputBudgetError reports that max_tokens was exhausted before any
// visible content. It is deliberately not an anthropicResponseProtocolError:
// the response is well-formed, the configured limit is simply too small, so
// retrying the identical request cannot succeed.
type anthropicOutputBudgetError struct {
	thinkingEnabled bool
}

func (e *anthropicOutputBudgetError) Error() string {
	if e.thinkingEnabled {
		return "Anthropic response hit max_tokens before producing visible output; " +
			"thinking tokens are drawn from the same limit, so raise model.max_response_tokens or lower model.reasoning_effort"
	}
	return "Anthropic response hit max_tokens before producing visible output; raise model.max_response_tokens"
}

func newAnthropicOutputBudgetError(thinkingEnabled bool) error {
	return &anthropicOutputBudgetError{thinkingEnabled: thinkingEnabled}
}

func isAnthropicOutputBudgetError(err error) bool {
	var budgetErr *anthropicOutputBudgetError
	return errors.As(err, &budgetErr)
}

func normalizeAnthropicResponse(response anthropicResponse, thinkingEnabled bool) (anthropicResponse, string, error) {
	hasText := false
	hasThinking := false
	hasToolUse := false
	hasSignedThinking := false
	invalidToolUse := false
	for _, block := range response.Content {
		switch block.Type {
		case "text":
			hasText = hasText || strings.TrimSpace(block.Text) != ""
		case "thinking":
			hasThinking = hasThinking || strings.TrimSpace(block.Thinking) != ""
			hasSignedThinking = hasSignedThinking || strings.TrimSpace(block.Signature) != ""
		case "tool_use":
			valid := strings.TrimSpace(block.ID) != "" && strings.TrimSpace(block.Name) != ""
			hasToolUse = hasToolUse || valid
			invalidToolUse = invalidToolUse || !valid
		}
	}
	if invalidToolUse {
		return response, "", newAnthropicResponseProtocolError("tool_use content is missing an id or name")
	}

	switch response.StopReason {
	case "tool_use":
		if !hasToolUse {
			return response, "", newAnthropicResponseProtocolError("tool_use stop_reason has no valid tool_use content")
		}
	case "max_tokens":
		if hasText || hasToolUse {
			return response, "", nil
		}
		// The response limit was consumed before any visible output. With
		// thinking enabled that is nearly always thinking spending the whole
		// allowance. Retrying is pointless: the same limit produces the same
		// truncation, so surface it as a terminal, actionable error.
		return response, "", newAnthropicOutputBudgetError(thinkingEnabled)
	case "end_turn":
		if hasText || hasToolUse {
			return response, "", nil
		}
		if hasThinking && !hasSignedThinking && !thinkingEnabled {
			for index := range response.Content {
				block := &response.Content[index]
				if block.Type != "thinking" {
					continue
				}
				block.Type = "text"
				block.Text = block.Thinking
				block.Thinking = ""
				block.Signature = ""
			}
			return response, "thinking_as_text", nil
		}
		return response, "", newAnthropicResponseProtocolError(anthropicEmptyEndTurnMessage)
	}

	return response, "", nil
}

func aggregateAnthropicResponseWithGenerationInfo(response anthropicResponse, callStarted time.Time, generationInfo map[string]any) *llms.ContentResponse {
	var textParts []string
	var thinkingParts []string
	toolCalls := make([]llms.ToolCall, 0)
	for _, block := range response.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "thinking":
			thinkingParts = append(thinkingParts, block.Thinking)
		case "tool_use":
			arguments, err := json.Marshal(block.Input)
			if err != nil {
				arguments = []byte("{}")
			}
			toolCalls = append(toolCalls, llms.ToolCall{
				ID: block.ID, Type: "function", FunctionCall: &llms.FunctionCall{Name: block.Name, Arguments: string(arguments)},
			})
		}
	}
	choice := &llms.ContentChoice{
		Content: strings.Join(textParts, ""), ReasoningContent: strings.Join(thinkingParts, ""),
		StopReason: response.StopReason, ToolCalls: toolCalls,
	}
	if len(toolCalls) > 0 {
		choice.FuncCall = toolCalls[0].FunctionCall
	}
	info := response.Usage.generationInfo()
	info["llm_output_chars"] = len(choice.Content)
	info["llm_tool_call_count"] = len(choice.ToolCalls)
	info["llm_finish_reason"] = response.StopReason
	info["llm_response_id"] = response.ID
	if thinkingBlocks := signedAnthropicThinkingBlocks(response.Content); len(thinkingBlocks) > 0 {
		info["anthropic_thinking_blocks"] = thinkingBlocks
	}
	for key, value := range generationInfo {
		info[key] = value
	}
	choice.GenerationInfo = finalizeLLMGenerationInfo(info, callStarted)
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{choice}}
}

func signedAnthropicThinkingBlocks(content []anthropicContentBlock) []json.RawMessage {
	var blocks []json.RawMessage
	for _, block := range content {
		if block.Type != "thinking" || strings.TrimSpace(block.Thinking) == "" || strings.TrimSpace(block.Signature) == "" {
			continue
		}
		encoded, err := json.Marshal(block)
		if err == nil {
			blocks = append(blocks, encoded)
		}
	}
	return blocks
}

type anthropicStreamBlock struct {
	Type      string
	ID        string
	Name      string
	Text      strings.Builder
	Thinking  strings.Builder
	Signature strings.Builder
	InputJSON strings.Builder
	Stopped   bool
}

type anthropicStreamError struct {
	err             error
	outputDelivered bool
	retryable       bool
	protocol        bool
}

func (e *anthropicStreamError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *anthropicStreamError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func newAnthropicStreamProtocolError(outputDelivered bool, format string, args ...any) error {
	return &anthropicStreamError{
		err:             fmt.Errorf("Anthropic stream protocol error: "+format, args...),
		outputDelivered: outputDelivered,
		retryable:       true,
		protocol:        true,
	}
}

type anthropicStreamBlockDiagnostic struct {
	Index int    `json:"index"`
	Type  string `json:"type"`
}

type anthropicStreamFailureDiagnostic struct {
	StreamError       string                           `json:"stream_error"`
	RawSSE            string                           `json:"raw_sse"`
	RawSSETotalBytes  int                              `json:"raw_sse_total_bytes"`
	RawSSETruncated   bool                             `json:"raw_sse_truncated"`
	RawSSEBase64      string                           `json:"raw_sse_base64,omitempty"`
	ResponseID        string                           `json:"response_id"`
	UpstreamRequestID string                           `json:"upstream_request_id"`
	ResponseHeaders   map[string]string                `json:"response_headers"`
	EventTypeCounts   map[string]int                   `json:"event_type_counts"`
	ContentBlocks     []anthropicStreamBlockDiagnostic `json:"content_blocks"`
}

type anthropicSSECapture struct {
	daemon       bytes.Buffer
	totalBytes   int
	rawFile      *os.File
	rawBytes     int
	rawWriteErr  error
	rawFileClose bool
}

var anthropicDiagnosticResponseHeaders = map[string]struct{}{
	"anthropic-ratelimit-input-tokens-limit":      {},
	"anthropic-ratelimit-input-tokens-remaining":  {},
	"anthropic-ratelimit-input-tokens-reset":      {},
	"anthropic-ratelimit-output-tokens-limit":     {},
	"anthropic-ratelimit-output-tokens-remaining": {},
	"anthropic-ratelimit-output-tokens-reset":     {},
	"anthropic-ratelimit-requests-limit":          {},
	"anthropic-ratelimit-requests-remaining":      {},
	"anthropic-ratelimit-requests-reset":          {},
	"anthropic-ratelimit-tokens-limit":            {},
	"anthropic-ratelimit-tokens-remaining":        {},
	"anthropic-ratelimit-tokens-reset":            {},
	"anthropic-request-id":                        {},
	"request-id":                                  {},
	"retry-after":                                 {},
	"trace-id":                                    {},
	"x-amz-request-id":                            {},
	"x-amzn-requestid":                            {},
	"x-goog-request-id":                           {},
	"x-request-id":                                {},
	"x-trace-id":                                  {},
}

func newAnthropicSSECapture(rawLoggingEnabled bool) *anthropicSSECapture {
	capture := &anthropicSSECapture{}
	if !rawLoggingEnabled {
		return capture
	}
	rawFile, err := os.CreateTemp("", "aiden-anthropic-sse-*")
	if err != nil {
		capture.rawWriteErr = err
		return capture
	}
	capture.rawFile = rawFile
	return capture
}

func (c *anthropicSSECapture) Write(p []byte) (int, error) {
	if c == nil {
		return len(p), nil
	}
	c.totalBytes += len(p)
	if remaining := anthropicDaemonRawSSELimit - c.daemon.Len(); remaining > 0 {
		if remaining > len(p) {
			remaining = len(p)
		}
		_, _ = c.daemon.Write(p[:remaining])
	}
	if c.rawFile == nil || c.rawWriteErr != nil || c.rawBytes >= anthropicRawHTTPRawSSELimit {
		return len(p), nil
	}
	rawChunk := p
	if remaining := anthropicRawHTTPRawSSELimit - c.rawBytes; len(rawChunk) > remaining {
		rawChunk = rawChunk[:remaining]
	}
	written, err := c.rawFile.Write(rawChunk)
	c.rawBytes += written
	if err == nil && written != len(rawChunk) {
		err = io.ErrShortWrite
	}
	if err != nil {
		c.rawWriteErr = err
	}
	return len(p), nil
}

func (c *anthropicSSECapture) daemonBytes() []byte {
	if c == nil {
		return nil
	}
	return c.daemon.Bytes()
}

func (c *anthropicSSECapture) rawHTTPBytes() ([]byte, error) {
	if c == nil || c.rawFile == nil {
		if c != nil && c.rawWriteErr != nil {
			return nil, c.rawWriteErr
		}
		return nil, nil
	}
	if c.rawWriteErr != nil {
		return nil, c.rawWriteErr
	}
	raw := make([]byte, c.rawBytes)
	if _, err := c.rawFile.ReadAt(raw, 0); err != nil {
		return nil, err
	}
	return raw, nil
}

func (c *anthropicSSECapture) Close() {
	if c == nil || c.rawFile == nil || c.rawFileClose {
		return
	}
	c.rawFileClose = true
	rawPath := c.rawFile.Name()
	_ = c.rawFile.Close()
	_ = os.Remove(rawPath)
}

func (d *anthropicStreamFailureDiagnostic) setRawSSE(rawSSE []byte, rawSSELimit int) {
	rawSample := rawSSE
	truncated := false
	if rawSSELimit >= 0 && len(rawSample) > rawSSELimit {
		rawSample = rawSample[:rawSSELimit]
		truncated = true
	}
	rawSSEValidUTF8 := utf8.Valid(rawSSE)
	if rawSSEValidUTF8 {
		for len(rawSample) > 0 && !utf8.Valid(rawSample) {
			rawSample = rawSample[:len(rawSample)-1]
		}
	}

	d.RawSSE = string(rawSample)
	d.RawSSETotalBytes = len(rawSSE)
	d.RawSSETruncated = truncated
	d.RawSSEBase64 = ""
	if !rawSSEValidUTF8 {
		d.RawSSEBase64 = base64.StdEncoding.EncodeToString(rawSample)
	}
}

func (d *anthropicStreamFailureDiagnostic) setCapturedRawSSE(rawSSE []byte, totalBytes int) {
	d.setRawSSE(rawSSE, -1)
	d.RawSSETotalBytes = totalBytes
	d.RawSSETruncated = len(rawSSE) < totalBytes
}

func normalizeAnthropicResponseHeaders(headers http.Header) map[string]string {
	normalized := make(map[string]string)
	for key, values := range headers {
		normalizedKey := strings.ToLower(strings.TrimSpace(key))
		if _, allowed := anthropicDiagnosticResponseHeaders[normalizedKey]; !allowed {
			continue
		}
		normalized[normalizedKey] = strings.Join(values, ", ")
	}
	return normalized
}

func anthropicUpstreamRequestID(headers map[string]string) string {
	for _, key := range []string{
		"request-id",
		"x-request-id",
		"anthropic-request-id",
		"x-amzn-requestid",
		"x-amz-request-id",
		"x-goog-request-id",
		"x-trace-id",
		"trace-id",
	} {
		if value := strings.TrimSpace(headers[key]); value != "" {
			return value
		}
	}
	return ""
}

func anthropicStreamBlockDiagnostics(blocks map[int]*anthropicStreamBlock) []anthropicStreamBlockDiagnostic {
	indexes := make([]int, 0, len(blocks))
	for index := range blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)

	diagnostics := make([]anthropicStreamBlockDiagnostic, 0, len(indexes))
	for _, index := range indexes {
		diagnostics = append(diagnostics, anthropicStreamBlockDiagnostic{
			Index: index,
			Type:  blocks[index].Type,
		})
	}
	return diagnostics
}

func isAnthropicStreamProtocolFailure(err error) bool {
	var streamErr *anthropicStreamError
	if errors.As(err, &streamErr) && streamErr.protocol {
		return true
	}
	var responseProtocolErr *anthropicResponseProtocolError
	return errors.As(err, &responseProtocolErr)
}

func firstOpenAnthropicStreamBlock(blocks map[int]*anthropicStreamBlock) (int, bool) {
	openIndex := 0
	found := false
	for index, block := range blocks {
		if block.Stopped || found && index >= openIndex {
			continue
		}
		openIndex = index
		found = true
	}
	return openIndex, found
}

func (m *anthropicModel) decodeStreamingResponse(ctx context.Context, body io.Reader, responseHeaders http.Header, model string, statusCode int, callStarted time.Time, opts *llms.CallOptions, thinkingEnabled bool) (result *llms.ContentResponse, resultErr error) {
	rawLoggingEnabled := m.rawLogger != nil && rawHTTPLogEnabled(ctx)
	rawCapture := newAnthropicSSECapture(rawLoggingEnabled)
	defer rawCapture.Close()
	scanner := bufio.NewScanner(io.TeeReader(body, rawCapture))
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	blocks := map[int]*anthropicStreamBlock{}
	eventTypeCounts := map[string]int{}
	normalizedResponseHeaders := normalizeAnthropicResponseHeaders(responseHeaders)
	usage := anthropicUsage{}
	responseID := ""
	stopReason := ""
	firstContent := false
	var firstContentAt int64
	outputDelivered := false
	messageStarted := false
	messageDeltaSeen := false
	messageStopped := false
	defer func() {
		if resultErr == nil {
			return
		}
		logStatusCode := statusCode
		var statusErr interface{ HTTPStatusCode() int }
		if errors.As(resultErr, &statusErr) && statusErr.HTTPStatusCode() != 0 {
			logStatusCode = statusErr.HTTPStatusCode()
		}
		failureDiagnostic := anthropicStreamFailureDiagnostic{
			StreamError:       resultErr.Error(),
			ResponseID:        responseID,
			UpstreamRequestID: anthropicUpstreamRequestID(normalizedResponseHeaders),
			ResponseHeaders:   normalizedResponseHeaders,
			EventTypeCounts:   eventTypeCounts,
			ContentBlocks:     anthropicStreamBlockDiagnostics(blocks),
		}
		failureRawSSE := rawCapture.daemonBytes()
		if rawLoggingEnabled {
			if captured, err := rawCapture.rawHTTPBytes(); err == nil && captured != nil {
				failureRawSSE = captured
			}
		}
		failureDiagnostic.setCapturedRawSSE(failureRawSSE, rawCapture.totalBytes)
		failureBody, _ := json.Marshal(failureDiagnostic)
		_ = m.logRawHTTP(ctx, model, "response", logStatusCode, string(failureBody))
		if isAnthropicStreamProtocolFailure(resultErr) {
			daemonDiagnostic := failureDiagnostic
			daemonDiagnostic.setCapturedRawSSE(rawCapture.daemonBytes(), rawCapture.totalBytes)
			daemonBody, _ := json.Marshal(daemonDiagnostic)
			logging.Errorf("agent", "anthropic", "stream protocol failure %s", daemonBody)
		}
	}()
	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(trimmed, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event struct {
			Type         string                `json:"type"`
			Index        int                   `json:"index"`
			Message      anthropicResponse     `json:"message"`
			ContentBlock anthropicContentBlock `json:"content_block"`
			Delta        map[string]any        `json:"delta"`
			Usage        anthropicUsage        `json:"usage"`
			Error        map[string]any        `json:"error"`
		}
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			return nil, newAnthropicStreamProtocolError(outputDelivered, "decode event: %v", err)
		}
		eventTypeCounts[event.Type]++
		if messageStopped {
			return nil, newAnthropicStreamProtocolError(outputDelivered, "event %q received after message_stop", event.Type)
		}
		switch event.Type {
		case "message_start":
			if messageStarted {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "duplicate message_start")
			}
			messageStarted = true
			responseID = event.Message.ID
			usage = event.Message.Usage
		case "content_block_start":
			if !messageStarted {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "content_block_start received before message_start")
			}
			if messageDeltaSeen {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "content_block_start received after message_delta")
			}
			if _, exists := blocks[event.Index]; exists {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "duplicate content_block_start for block %d", event.Index)
			}
			block := &anthropicStreamBlock{Type: event.ContentBlock.Type, ID: event.ContentBlock.ID, Name: event.ContentBlock.Name}
			if event.ContentBlock.Text != "" {
				block.Text.WriteString(event.ContentBlock.Text)
			}
			if event.ContentBlock.Thinking != "" {
				block.Thinking.WriteString(event.ContentBlock.Thinking)
			}
			if event.ContentBlock.Signature != "" {
				block.Signature.WriteString(event.ContentBlock.Signature)
			}
			blocks[event.Index] = block
		case "content_block_delta":
			if !messageStarted {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "content_block_delta received before message_start")
			}
			if messageDeltaSeen {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "content_block_delta received after message_delta")
			}
			block := blocks[event.Index]
			if block == nil {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "content_block_delta references missing block %d", event.Index)
			}
			if block.Stopped {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "content_block_delta received after content_block_stop for block %d", event.Index)
			}
			deltaType, _ := event.Delta["type"].(string)
			switch deltaType {
			case "text_delta":
				chunk, _ := event.Delta["text"].(string)
				block.Text.WriteString(chunk)
				if chunk != "" && !firstContent {
					firstContent = true
					firstContentAt = time.Since(callStarted).Milliseconds()
				}
				if chunk != "" && opts.StreamingFunc != nil {
					if err := opts.StreamingFunc(ctx, []byte(chunk)); err != nil {
						return nil, err
					}
					outputDelivered = true
				}
			case "thinking_delta":
				chunk, _ := event.Delta["thinking"].(string)
				block.Thinking.WriteString(chunk)
				if chunk != "" && thinkingEnabled && opts.StreamingReasoningFunc != nil {
					if err := opts.StreamingReasoningFunc(ctx, []byte(chunk), nil); err != nil {
						return nil, err
					}
					outputDelivered = true
				}
			case "input_json_delta":
				chunk, _ := event.Delta["partial_json"].(string)
				block.InputJSON.WriteString(chunk)
			case "signature_delta":
				chunk, _ := event.Delta["signature"].(string)
				block.Signature.WriteString(chunk)
			case "citations_delta":
				// Citations annotate an existing text block and do not change its text content.
			default:
				return nil, newAnthropicStreamProtocolError(outputDelivered, "unknown content_block_delta type %q for block %d", deltaType, event.Index)
			}
		case "content_block_stop":
			if !messageStarted {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "content_block_stop received before message_start")
			}
			if messageDeltaSeen {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "content_block_stop received after message_delta")
			}
			block := blocks[event.Index]
			if block == nil {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "content_block_stop references missing block %d", event.Index)
			}
			if block.Stopped {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "duplicate content_block_stop for block %d", event.Index)
			}
			block.Stopped = true
		case "message_delta":
			if !messageStarted {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "message_delta received before message_start")
			}
			if index, found := firstOpenAnthropicStreamBlock(blocks); found {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "message_delta received with open content block %d", index)
			}
			if value, ok := event.Delta["stop_reason"].(string); ok {
				stopReason = value
			}
			if event.Usage.OutputTokens > 0 {
				usage.OutputTokens = event.Usage.OutputTokens
			}
			messageDeltaSeen = true
		case "message_stop":
			if !messageStarted {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "message_stop received before message_start")
			}
			if index, found := firstOpenAnthropicStreamBlock(blocks); found {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "message_stop received with open content block %d", index)
			}
			if !messageDeltaSeen {
				return nil, newAnthropicStreamProtocolError(outputDelivered, "message_stop received before message_delta")
			}
			messageStopped = true
		case "ping":
		case "error":
			encoded, _ := json.Marshal(map[string]any{"error": event.Error})
			providerErr := newProviderHTTPError(anthropicErrorHTTPStatus(event.Error), encoded)
			return nil, &anthropicStreamError{err: providerErr, outputDelivered: outputDelivered}
		default:
			return nil, newAnthropicStreamProtocolError(outputDelivered, "unknown event type %q", event.Type)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, &anthropicStreamError{
			err:             fmt.Errorf("read Anthropic stream: %w", err),
			outputDelivered: outputDelivered,
			retryable:       true,
		}
	}
	if !messageStarted {
		return nil, newAnthropicStreamProtocolError(outputDelivered, "ended before message_start")
	}
	if !messageStopped {
		return nil, newAnthropicStreamProtocolError(outputDelivered, "ended before message_stop")
	}
	response := anthropicResponse{ID: responseID, Model: model, StopReason: stopReason, Usage: usage}
	indexes := make([]int, 0, len(blocks))
	for index := range blocks {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		block := blocks[index]
		converted := anthropicContentBlock{Type: block.Type, ID: block.ID, Name: block.Name}
		switch block.Type {
		case "text":
			converted.Text = block.Text.String()
		case "thinking":
			converted.Thinking = block.Thinking.String()
			converted.Signature = block.Signature.String()
		case "tool_use":
			input := any(map[string]any{})
			if rawInput := strings.TrimSpace(block.InputJSON.String()); rawInput != "" {
				if err := json.Unmarshal([]byte(rawInput), &input); err != nil {
					return nil, newAnthropicStreamProtocolError(outputDelivered, "decode tool input for block %d: %v", index, err)
				}
			}
			converted.Input = input
		}
		response.Content = append(response.Content, converted)
	}
	encodedResponse, encodeErr := json.Marshal(response)
	normalized, recovery, protocolErr := normalizeAnthropicResponse(response, thinkingEnabled)
	if protocolErr != nil {
		retryable := !isAnthropicOutputBudgetError(protocolErr)
		return nil, &anthropicStreamError{err: protocolErr, outputDelivered: outputDelivered, retryable: retryable, protocol: retryable}
	}
	response = normalized
	if recovery != "" {
		logging.Warnf("agent", "anthropic", "recovered streamed response %s as user-visible text (response_id=%s)", recovery, response.ID)
	}
	if recovery != "" && opts.StreamingFunc != nil {
		recoveredText := anthropicResponseText(response)
		if recoveredText != "" {
			if err := opts.StreamingFunc(ctx, []byte(recoveredText)); err != nil {
				return nil, err
			}
			outputDelivered = true
			if !firstContent {
				firstContent = true
				firstContentAt = time.Since(callStarted).Milliseconds()
			}
		}
	}
	if encodeErr == nil {
		_ = m.logRawHTTP(ctx, model, "response", statusCode, string(encodedResponse))
	} else {
		rawSSE := rawCapture.daemonBytes()
		if rawLoggingEnabled {
			if captured, err := rawCapture.rawHTTPBytes(); err == nil && captured != nil {
				rawSSE = captured
			}
		}
		_ = m.logRawHTTP(ctx, model, "response", statusCode, string(rawSSE))
	}
	generationInfo := map[string]any{}
	if firstContent {
		generationInfo["llm_stream"] = true
		generationInfo["llm_time_to_first_content_ms"] = firstContentAt
	}
	if recovery != "" {
		generationInfo["llm_anthropic_response_recovery"] = recovery
	}
	result = aggregateAnthropicResponseWithGenerationInfo(response, callStarted, generationInfo)
	return result, nil
}

func anthropicResponseText(response anthropicResponse) string {
	var result strings.Builder
	for _, block := range response.Content {
		if block.Type == "text" {
			result.WriteString(block.Text)
		}
	}
	return result.String()
}

func anthropicErrorHTTPStatus(eventError map[string]any) int {
	errorType, _ := eventError["type"].(string)
	switch strings.ToLower(strings.TrimSpace(errorType)) {
	case "invalid_request_error":
		return http.StatusBadRequest
	case "authentication_error":
		return http.StatusUnauthorized
	case "permission_error":
		return http.StatusForbidden
	case "not_found_error":
		return http.StatusNotFound
	case "request_too_large":
		return http.StatusRequestEntityTooLarge
	case "rate_limit_error":
		return http.StatusTooManyRequests
	case "overloaded_error":
		return 529
	case "api_error":
		return http.StatusInternalServerError
	default:
		return http.StatusInternalServerError
	}
}

func shouldRetryAnthropicStreamError(err error) bool {
	var streamErr *anthropicStreamError
	if !errors.As(err, &streamErr) || streamErr.outputDelivered {
		return false
	}
	if streamErr.retryable {
		return true
	}
	var statusErr interface{ HTTPStatusCode() int }
	return errors.As(err, &statusErr) && shouldRetryHTTPStatus(statusErr.HTTPStatusCode())
}

func waitBeforeAnthropicRetry(ctx context.Context, retryNumber int, retryDelay time.Duration) error {
	return sleepWithContext(ctx, time.Duration(retryNumber)*retryDelay)
}

func (m *anthropicModel) withRawHTTPLogScope(ctx context.Context) context.Context {
	if m == nil || m.rawLogger == nil || !rawHTTPLogEnabled(ctx) {
		return ctx
	}
	return m.rawLogger.BeginScope(ctx)
}

func (m *anthropicModel) logRawHTTP(ctx context.Context, model, kind string, statusCode int, raw string) error {
	if m == nil || m.rawLogger == nil || !rawHTTPLogEnabled(ctx) {
		return nil
	}
	return m.rawLogger.Log(ctx, RawHTTPLogEntry{
		Model:      model,
		Kind:       kind,
		StatusCode: statusCode,
		Raw:        raw,
	})
}

func buildAnthropicModelOptions(ctx ModelBuildContext, cfg ModelConfig) []anthropicModelOption {
	var options []anthropicModelOption
	if ctx.RawHTTPLogger != nil {
		options = append(options, withAnthropicRawHTTPLogger(ctx.RawHTTPLogger))
	}
	if cfg.Temperature != nil {
		options = append(options, withAnthropicTemperature(cfg.Temperature))
	}
	if cfg.ReasoningEffort != "" {
		options = append(options, withAnthropicReasoningEffort(cfg.ReasoningEffort))
	}
	if cfg.ReasoningBudgetTokens > 0 {
		options = append(options, withAnthropicReasoningBudget(cfg.ReasoningBudgetTokens))
	}
	return options
}

func normalizeAnthropicReasoningEffort(effort string) string {
	switch strings.ToLower(strings.TrimSpace(effort)) {
	case "none", "low", "medium", "high", "xhigh", "max":
		return strings.ToLower(strings.TrimSpace(effort))
	default:
		return ""
	}
}

func resolveAnthropicBaseURL(configured string) string {
	if configured = strings.TrimSpace(configured); configured != "" {
		return configured
	}
	return defaultAnthropicBaseURL
}

func resolveAnthropicToken(configured string) (string, bool) {
	if token := strings.TrimSpace(resolveProviderAPIKey(configured)); token != "" {
		environment, isReference := providerAPIKeyEnv(configured)
		return token, isReference && strings.EqualFold(environment, "ANTHROPIC_AUTH_TOKEN")
	}
	if token := strings.TrimSpace(os.Getenv("ANTHROPIC_AUTH_TOKEN")); token != "" {
		return token, true
	}
	if token := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); token != "" {
		return token, false
	}
	return "", false
}
