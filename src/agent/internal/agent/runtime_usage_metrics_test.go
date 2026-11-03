package agent

import (
	"context"
	"testing"

	"aico-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/llms"
)

func usageResponse(prompt, completion, total int) *llms.ContentResponse {
	return &llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			GenerationInfo: map[string]any{
				"prompt_tokens":     prompt,
				"completion_tokens": completion,
				"total_tokens":      total,
			},
		}},
	}
}

// TestRecordUsageMetricsAccumulatesAcrossCalls verifies that the role-collaborative
// loop's multiple LLM calls sum into the run metrics instead of overwriting, while
// LastPromptTokens tracks the largest single prompt in the run for the compression
// heuristic.
func TestRecordUsageMetricsAccumulatesAcrossCalls(t *testing.T) {
	metrics := &RunMetrics{}

	recordUsageMetrics(metrics, usageResponse(100, 20, 120)) // planner
	recordUsageMetrics(metrics, usageResponse(150, 30, 180)) // executor
	recordUsageMetrics(metrics, usageResponse(90, 10, 100))  // verifier

	if metrics.PromptTokens != 340 {
		t.Errorf("PromptTokens = %d, want 340 (100+150+90)", metrics.PromptTokens)
	}
	if metrics.CompletionTokens != 60 {
		t.Errorf("CompletionTokens = %d, want 60 (20+30+10)", metrics.CompletionTokens)
	}
	if metrics.TotalTokens != 400 {
		t.Errorf("TotalTokens = %d, want 400 (120+180+100)", metrics.TotalTokens)
	}
	if metrics.LastPromptTokens != 150 {
		t.Errorf("LastPromptTokens = %d, want 150 (max single prompt)", metrics.LastPromptTokens)
	}
}

func TestRunMetricsCacheHitRateClampsProviderReportedValues(t *testing.T) {
	if got := (*RunMetrics)(nil).CacheHitRate(); got != 0 {
		t.Fatalf("nil CacheHitRate() = %v, want 0", got)
	}
	if got := (&RunMetrics{}).CacheHitRate(); got != 0 {
		t.Fatalf("zero prompt CacheHitRate() = %v, want 0", got)
	}
	if got := (&RunMetrics{PromptTokens: 200, CachedPromptTokens: 50}).CacheHitRate(); got != 0.25 {
		t.Fatalf("normal CacheHitRate() = %v, want 0.25", got)
	}
	if got := (&RunMetrics{PromptTokens: 100, CachedPromptTokens: 150}).CacheHitRate(); got != 1 {
		t.Fatalf("oversized cached tokens CacheHitRate() = %v, want 1", got)
	}
}

func TestUsageTrackingModelCapturesFullPromptForTelemetry(t *testing.T) {
	capture := newTelemetryPromptCapture(true)
	model := &usageTrackingModel{
		inner:         &scriptedModel{responses: []*llms.ContentResponse{usageResponse(10, 5, 15)}},
		metrics:       &RunMetrics{},
		promptCapture: capture,
	}
	messages := []llms.MessageContent{
		{Role: llms.ChatMessageTypeSystem, Parts: []llms.ContentPart{llms.TextPart("full system prompt")}},
		{Role: llms.ChatMessageTypeHuman, Parts: []llms.ContentPart{llms.TextPart("full user prompt")}},
	}
	tools := []llms.Tool{{
		Type: "function",
		Function: &llms.FunctionDefinition{
			Name:        "echo",
			Description: "Echo text.",
			Parameters: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"value": map[string]any{"type": "string"},
				},
				"required": []string{"value"},
			},
		},
	}}

	ctx := context.WithValue(context.Background(), telemetryPromptContextKey{}, "agent")
	_, err := model.GenerateContent(ctx, messages,
		llms.WithTemperature(0.3), llms.WithMaxTokens(123), llms.WithTools(tools))
	if err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	snapshot := capture.Snapshot()
	if len(snapshot) != 1 {
		t.Fatalf("captured prompts = %d, want 1", len(snapshot))
	}
	if snapshot[0].Role != "agent" {
		t.Fatalf("captured role = %q, want agent", snapshot[0].Role)
	}
	parts, ok := snapshot[0].Input[0]["parts"].([]map[string]interface{})
	if !ok || len(parts) != 1 || parts[0]["text"] != "full system prompt" {
		t.Fatalf("captured system prompt = %#v", snapshot[0].Input[0]["parts"])
	}
	if snapshot[0].UsageDetails["input"] != 10 || snapshot[0].UsageDetails["output"] != 5 || snapshot[0].UsageDetails["total"] != 15 {
		t.Fatalf("captured usage = %#v, want input/output/total 10/5/15", snapshot[0].UsageDetails)
	}
	if snapshot[0].ModelParameters["temperature"] != 0.3 || snapshot[0].ModelParameters["max_response_tokens"] != 123 {
		t.Fatalf("captured model parameters = %#v, want temperature/max_response_tokens", snapshot[0].ModelParameters)
	}
	if _, ok := snapshot[0].ModelParameters["max_tokens"]; ok {
		t.Fatalf("captured model parameters = %#v, did not expect legacy max_tokens", snapshot[0].ModelParameters)
	}
	if _, ok := snapshot[0].ModelParameters["tools_count"]; ok {
		t.Fatalf("captured model parameters = %#v, did not expect tools_count", snapshot[0].ModelParameters)
	}
	if _, ok := snapshot[0].ModelParameters["tools"]; ok {
		t.Fatalf("captured model parameters = %#v, did not expect tools", snapshot[0].ModelParameters)
	}
	if snapshot[0].Metadata["tools_count"] != 1 {
		t.Fatalf("captured metadata = %#v, want tools_count=1", snapshot[0].Metadata)
	}
	capturedTools, ok := snapshot[0].Metadata["tool_schemas"].([]map[string]interface{})
	if !ok || len(capturedTools) != 1 {
		t.Fatalf("captured tool_schemas = %#v, want one tool definition", snapshot[0].Metadata["tool_schemas"])
	}
	function, ok := capturedTools[0]["function"].(map[string]interface{})
	if !ok || function["name"] != "echo" {
		t.Fatalf("captured tool function = %#v, want echo", capturedTools[0]["function"])
	}
	parameters, ok := function["parameters"].(map[string]interface{})
	if !ok || parameters["type"] != "object" {
		t.Fatalf("captured tool parameters = %#v, want object schema", function["parameters"])
	}
}

type contextListUsageTestModel struct {
	*scriptedModel
	calls int
}

func (m *contextListUsageTestModel) GenerateContentFromMessageList(_ context.Context, _ []messages.Message, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	m.calls++
	return usageResponse(7, 2, 9), nil
}

func TestUsageTrackingModelForwardsContextMessageList(t *testing.T) {
	inner := &contextListUsageTestModel{scriptedModel: &scriptedModel{}}
	tracked := &usageTrackingModel{inner: inner, metrics: &RunMetrics{}}
	if _, err := tracked.GenerateContentFromMessageList(context.Background(), []messages.Message{{}}, llms.WithMaxTokens(4)); err != nil {
		t.Fatalf("GenerateContentFromMessageList() error = %v", err)
	}
	if inner.calls != 1 {
		t.Fatalf("context-list calls = %d, want 1", inner.calls)
	}
	if tracked.metrics.TotalTokens != 9 {
		t.Fatalf("tracked total tokens = %d, want 9", tracked.metrics.TotalTokens)
	}
}
