package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aico-agent/internal/agent/model"
	"github.com/tmc/langchaingo/llms"
)

type capturedAnthropicRequest struct {
	Thinking     *anthropicThinking     `json:"thinking"`
	OutputConfig *anthropicOutputConfig `json:"output_config"`
	MaxTokens    int                    `json:"max_tokens"`
}

// newAnthropicThinkingProbe serves a fixed reply and records every request so
// tests can assert on the thinking/max_tokens combination that was sent.
func newAnthropicThinkingProbe(t *testing.T, body string) (*httptest.Server, *[]capturedAnthropicRequest) {
	t.Helper()
	captured := &[]capturedAnthropicRequest{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request capturedAnthropicRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		*captured = append(*captured, request)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(server.Close)
	return server, captured
}

const anthropicThinkingProbeOK = `{"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`

func TestAnthropicModelKeepsConfiguredMaxTokensWithEffort(t *testing.T) {
	server, captured := newAnthropicThinkingProbe(t, anthropicThinkingProbeOK)

	// claude-opus-5 is an effort model: adaptive thinking carries no explicit
	// budget, and the configured response limit remains authoritative.
	m := newAnthropicModel(server.URL, "claude-opus-5", "tok", server.Client(),
		withAnthropicReasoningEffort("max"))
	if _, err := m.GenerateContent(context.Background(),
		[]llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "hi")},
		llms.WithMaxTokens(1000)); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	got := (*captured)[0]
	if got.Thinking == nil || got.Thinking.Type != "adaptive" {
		t.Fatalf("thinking = %#v, want adaptive", got.Thinking)
	}
	if got.MaxTokens != 1000 {
		t.Fatalf("max_tokens = %d, want the configured 1000", got.MaxTokens)
	}
}

func TestAnthropicModelKeepsMaxTokensAboveEffortFloor(t *testing.T) {
	server, captured := newAnthropicThinkingProbe(t, anthropicThinkingProbeOK)

	m := newAnthropicModel(server.URL, "claude-opus-5", "tok", server.Client(),
		withAnthropicReasoningEffort("low"))
	if _, err := m.GenerateContent(context.Background(),
		[]llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "hi")},
		llms.WithMaxTokens(64_000)); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if got := (*captured)[0].MaxTokens; got != 64_000 {
		t.Fatalf("max_tokens = %d, want the configured 64000 left untouched", got)
	}
}

func TestAnthropicModelOmitsDerivedBudgetWhenMinimumDoesNotFit(t *testing.T) {
	server, captured := newAnthropicThinkingProbe(t, anthropicThinkingProbeOK)

	// claude-3.7-sonnet is budget-only, but its 1024-token floor cannot fit below
	// this response limit. The turn should continue without thinking.
	m := newAnthropicModel(server.URL, "anthropic/claude-3.7-sonnet", "tok", server.Client(),
		withAnthropicReasoningEffort("high"))
	if _, err := m.GenerateContent(context.Background(),
		[]llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "hi")},
		llms.WithMaxTokens(1000)); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if got := (*captured)[0].Thinking; got != nil {
		t.Fatalf("thinking = %#v, want omitted when the minimum budget cannot fit", got)
	}
}

func TestAnthropicModelClampsDerivedBudgetBelowMaxTokens(t *testing.T) {
	server, captured := newAnthropicThinkingProbe(t, anthropicThinkingProbeOK)
	m := newAnthropicModel(server.URL, "anthropic/claude-3.7-sonnet", "tok", server.Client(),
		withAnthropicReasoningEffort("high"))
	if _, err := m.GenerateContent(context.Background(),
		[]llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "hi")},
		llms.WithMaxTokens(2000)); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if got := (*captured)[0].Thinking; got == nil || got.BudgetTokens != 1999 {
		t.Fatalf("thinking = %#v, want derived budget clamped to 1999", got)
	}
}

func TestAnthropicModelRejectsExplicitBudgetAboveMaxTokens(t *testing.T) {
	server, _ := newAnthropicThinkingProbe(t, anthropicThinkingProbeOK)
	m := newAnthropicModel(server.URL, "anthropic/claude-3.7-sonnet", "tok", server.Client(),
		withAnthropicReasoningBudget(4096))
	_, err := m.GenerateContent(context.Background(),
		[]llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "hi")},
		llms.WithMaxTokens(2000))
	if err == nil || !strings.Contains(err.Error(), "less than max_response_tokens") {
		t.Fatalf("GenerateContent() error = %v, want response-limit validation", err)
	}
}

func TestAnthropicModelOmitsThinkingWhenSpecSaysUnsupported(t *testing.T) {
	server, captured := newAnthropicThinkingProbe(t, anthropicThinkingProbeOK)

	// claude-3.5-sonnet carries an explicit Supported=false declaration.
	m := newAnthropicModel(server.URL, "anthropic/claude-3.5-sonnet", "tok", server.Client(),
		withAnthropicReasoningEffort("high"))
	if _, err := m.GenerateContent(context.Background(),
		[]llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "hi")}); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	got := (*captured)[0]
	if got.Thinking != nil || got.OutputConfig != nil {
		t.Fatalf("thinking = %#v / output_config = %#v, want both omitted for a non-reasoning model",
			got.Thinking, got.OutputConfig)
	}
}

func TestAnthropicModelUnknownModelKeepsAdaptiveFallback(t *testing.T) {
	server, captured := newAnthropicThinkingProbe(t, anthropicThinkingProbeOK)

	// A nil spec means unknown, not unsupported: the historical adaptive
	// behavior must survive so new model IDs are not silently degraded.
	m := newAnthropicModel(server.URL, "claude-unreleased-9", "tok", server.Client(),
		withAnthropicReasoningEffort("high"))
	if _, err := m.GenerateContent(context.Background(),
		[]llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "hi")}); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if got := (*captured)[0]; got.Thinking == nil || got.Thinking.Type != "adaptive" {
		t.Fatalf("thinking = %#v, want the adaptive fallback for an unknown model", got.Thinking)
	}
}

func TestAnthropicModelUsesLiveSpecForBudgetMode(t *testing.T) {
	server, captured := newAnthropicThinkingProbe(t, anthropicThinkingProbeOK)

	// Provider metadata arrives asynchronously, so the request mode must read the
	// live spec rather than the value captured at construction time.
	live := model.ModelSpec{
		MaxOutput: 6_000,
		Reasoning: &model.ReasoningSpec{Supported: true, Mode: "budget_tokens", BudgetTokensMin: 1024},
	}
	m := newAnthropicModel(server.URL, "claude-late-metadata", "tok", server.Client(),
		withAnthropicReasoningBudget(60_000),
		withAnthropicModelSpecFn(func() model.ModelSpec { return live }))
	if _, err := m.GenerateContent(context.Background(),
		[]llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "hi")},
		llms.WithMaxTokens(64_000)); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}

	got := (*captured)[0]
	if got.Thinking == nil || got.Thinking.Type != "enabled" || got.Thinking.BudgetTokens != 60_000 {
		t.Fatalf("thinking = %#v, want the explicit budget preserved", got.Thinking)
	}
}

func TestAnthropicModelReportsMaxTokensTruncationWithoutRetrying(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		// Thinking consumed the whole allowance: signed thinking, no text.
		_, _ = w.Write([]byte(`{"id":"msg_trunc","content":[{"type":"thinking","thinking":"deep","signature":"sig"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":1000}}`))
	}))
	defer server.Close()

	m := newAnthropicModel(server.URL, "claude-opus-5", "tok", server.Client(),
		withAnthropicReasoningEffort("high"))
	_, err := m.GenerateContent(context.Background(),
		[]llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "hi")},
		llms.WithMaxTokens(64_000))
	if err == nil {
		t.Fatal("GenerateContent() error = nil, want a max_tokens truncation error instead of a silent empty turn")
	}
	if !isAnthropicOutputBudgetError(err) {
		t.Fatalf("error = %v, want an anthropicOutputBudgetError", err)
	}
	if !strings.Contains(err.Error(), "max_response_tokens") {
		t.Fatalf("error = %q, want actionable guidance naming the setting to change", err)
	}
	if requests != 1 {
		t.Fatalf("requests = %d, want 1: an identical retry cannot succeed", requests)
	}
}

func TestAnthropicModelKeepsPartialOutputOnMaxTokensStop(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"msg_partial","content":[{"type":"text","text":"partial answer"}],"stop_reason":"max_tokens","usage":{"input_tokens":1,"output_tokens":50}}`))
	}))
	defer server.Close()

	m := newAnthropicModel(server.URL, "claude-opus-5", "tok", server.Client())
	response, err := m.GenerateContent(context.Background(),
		[]llms.MessageContent{llms.TextParts(llms.ChatMessageTypeHuman, "hi")})
	if err != nil {
		t.Fatalf("GenerateContent() error = %v, want truncated-but-usable content preserved", err)
	}
	if got := response.Choices[0].Content; got != "partial answer" {
		t.Fatalf("content = %q, want the partial answer preserved", got)
	}
}
