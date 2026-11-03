package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"aico-agent/internal/agent/model"
	"github.com/tmc/langchaingo/llms"
)

var (
	// errMemoryMergeTruncated marks a response that stopped because the token
	// budget ran out rather than because the model finished. The content is
	// returned alongside it so callers can log where it stopped, but it is not
	// parseable JSON and the call should be retried with a larger budget.
	errMemoryMergeTruncated = errors.New("memory merge model response was truncated")
	// errMemoryMergeEmpty marks a response with no usable content. Keep the
	// message stable: it is matched against historical device logs.
	errMemoryMergeEmpty = errors.New("memory merge model returned an empty response")
	// errMemoryMergeModelCall marks a failed request to the model, as opposed
	// to a response that arrived but could not be used.
	errMemoryMergeModelCall = errors.New("memory merge model call")
)

// isMemoryMergeTruncatedStopReason reports whether a finish reason means the
// model hit its output ceiling. OpenAI-compatible APIs report "length",
// Anthropic reports "max_tokens", and the Responses API reports "incomplete".
func isMemoryMergeTruncatedStopReason(reason string) bool {
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "length", "max_tokens", "max_output_tokens", "incomplete":
		return true
	default:
		return false
	}
}

// memoryMergeTokenBudget sizes the output budget for a batch of itemCount items.
//
// It stays linear in the batch size up to maxTokens, so a larger batch never
// leaves each item with less room than a smaller one -- a cap applied to a
// per-item product inverts that, which is how a full batch ended up with a
// smaller share than a single item.
//
// It also grows by half per retry attempt, so a batch that was truncated is
// retried with more headroom instead of replaying the same failing call.
func memoryMergeTokenBudget(perItem, itemCount, maxTokens, attempt int) int {
	if itemCount < 1 {
		itemCount = 1
	}
	if attempt < 0 {
		attempt = 0
	}
	budget := perItem * itemCount
	for range attempt {
		if budget >= maxTokens {
			break
		}
		budget += budget / 2
	}
	return min(budget, maxTokens)
}

// MemoryMergeReference is the normalized top-k context passed to the model.
// The scenario processor chooses how to serialize its raw source records, but
// the engine owns collecting related memories and making the model call.
type MemoryMergeReference struct {
	Scope        string            `json:"scope"`
	ID           string            `json:"id"`
	Type         string            `json:"type,omitempty"`
	Status       string            `json:"status,omitempty"`
	Title        string            `json:"title,omitempty"`
	Summary      string            `json:"summary,omitempty"`
	Content      string            `json:"content,omitempty"`
	Tags         []string          `json:"tags,omitempty"`
	Entities     []string          `json:"entities,omitempty"`
	Revision     int               `json:"revision,omitempty"`
	ExpiresAt    string            `json:"expires_at,omitempty"`
	Priority     int               `json:"priority,omitempty"`
	Confidence   float64           `json:"confidence,omitempty"`
	SourceRefs   []MemorySourceRef `json:"source_refs,omitempty"`
	EvidenceRefs []MemorySourceRef `json:"evidence_refs,omitempty"`
}

// MemoryMergeRequest is the scenario seam. Search and BuildMessages are
// supplied by a Processor; retrieval, LLM invocation, timeout and response
// extraction are owned by MemoryMergeEngine.
type MemoryMergeRequest struct {
	Search        func(context.Context) ([]MemoryMergeReference, error)
	BuildMessages func([]MemoryMergeReference) ([]llms.MessageContent, error)
	MaxTokens     int
	Timeout       time.Duration
}

type MemoryMergeEngine struct {
	model model.Model
}

func NewMemoryMergeEngine(m model.Model) *MemoryMergeEngine {
	return &MemoryMergeEngine{model: m}
}

func (e *MemoryMergeEngine) Extract(ctx context.Context, req MemoryMergeRequest) ([]MemoryMergeReference, string, error) {
	if e == nil || e.model == nil {
		return nil, "", fmt.Errorf("memory merge model is not configured")
	}
	if req.Search == nil || req.BuildMessages == nil {
		return nil, "", fmt.Errorf("memory merge request is incomplete")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	references, err := req.Search(ctx)
	if err != nil {
		return nil, "", err
	}
	messages, err := req.BuildMessages(references)
	if err != nil {
		return nil, "", err
	}
	if len(messages) == 0 {
		return references, "", fmt.Errorf("memory merge request has no messages")
	}
	timeout := req.Timeout
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	maxTokens := req.MaxTokens
	if maxTokens <= 0 {
		maxTokens = 2200
	}
	response, err := e.model.GenerateContent(callCtx, messages, llms.WithJSONMode(), llms.WithMaxTokens(maxTokens))
	if err != nil {
		return references, "", fmt.Errorf("%w: %w", errMemoryMergeModelCall, err)
	}
	content, err := memoryMergeResponseContent(response, maxTokens)
	return references, content, err
}

// memoryMergeResponseContent consistently classifies model responses used by
// memory extraction and its review gates. Keeping stop-reason handling here
// prevents one call site from parsing a known-truncated response as malformed
// JSON while another schedules the intended retry.
func memoryMergeResponseContent(response *llms.ContentResponse, maxTokens int) (string, error) {
	if response == nil || len(response.Choices) == 0 {
		return "", errMemoryMergeEmpty
	}
	choice := response.Choices[0]
	content := stripJSONFences(choice.Content)
	// A budget-exhausted response is syntactically incomplete, so parsing it
	// would only report a confusing "unexpected end of JSON input". Report the
	// real cause instead, and hand back the partial content for diagnostics.
	if isMemoryMergeTruncatedStopReason(choice.StopReason) {
		return content, fmt.Errorf("%w: stop_reason=%s max_tokens=%d output_bytes=%d",
			errMemoryMergeTruncated, strings.TrimSpace(choice.StopReason), maxTokens, len(choice.Content))
	}
	if strings.TrimSpace(content) == "" {
		return "", errMemoryMergeEmpty
	}
	return content, nil
}
