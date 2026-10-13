package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"aiden-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/llms"
)

// Use the real HTTP adapter behind the runtime's accounting wrapper.
type benchmarkCostTestModel struct {
	*scriptedModel
	provider *openAICompatibleModel
}

func (m *benchmarkCostTestModel) GenerateContent(ctx context.Context, input []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	return m.provider.GenerateContent(ctx, input, options...)
}

func (m *benchmarkCostTestModel) GenerateContentFromMessageList(ctx context.Context, input []messages.Message, options ...llms.CallOption) (*llms.ContentResponse, error) {
	return m.provider.GenerateContentFromMessageList(ctx, input, options...)
}

func TestBenchmarkModelCostReachesEpisode(t *testing.T) {
	for _, streaming := range []bool{false, true} {
		for _, tc := range []struct {
			name  string
			field string
			known bool
			want  float64
		}{
			{"priced", `,"cost":0.0012`, true, 0.0024},
			{"free", `,"cost":0`, true, 0},
			{"missing", "", false, 0},
			{"null", `,"cost":null`, false, 0},
			{"negative", `,"cost":-1`, false, 0},
			{"error", "", false, 0},
		} {
			t.Run(fmt.Sprintf("%s/stream=%t", tc.name, streaming), func(t *testing.T) {
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					if tc.name == "error" {
						w.WriteHeader(http.StatusBadGateway)
						fmt.Fprint(w, `{"error":{"message":"unavailable"}}`)
						return
					}
					usage := `{"prompt_tokens":10,"completion_tokens":2,"total_tokens":12` + tc.field + `}`
					if streaming {
						w.Header().Set("Content-Type", "text/event-stream")
						fmt.Fprint(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"},\"finish_reason\":\"stop\"}]}\n\n")
						fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":%s}\n\ndata: [DONE]\n\n", usage)
					} else {
						fmt.Fprintf(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":%s}`, usage)
					}
				}))
				defer server.Close()
				tracked := &usageTrackingModel{
					inner: &benchmarkCostTestModel{scriptedModel: &scriptedModel{},
						provider: newOpenAICompatibleModel(server.URL, "model", "", server.Client()).(*openAICompatibleModel)},
					metrics: &RunMetrics{},
				}
				var options []llms.CallOption
				if streaming {
					options = append(options, llms.WithStreamingFunc(func(context.Context, []byte) error { return nil }))
				}
				if _, err := tracked.GenerateContent(context.Background(), nil, options...); (err != nil) != (tc.name == "error") {
					t.Fatalf("unexpected GenerateContent error: %v", err)
				}
				if _, err := tracked.GenerateContentFromMessageList(context.Background(), nil, options...); (err != nil) != (tc.name == "error") {
					t.Fatalf("unexpected context-list error: %v", err)
				}
				episode := NewEpisodeRecorder(MemoryRetrieveRequest{Input: "task"}, MemoryContext{}).Finish("ok", tracked.metrics, nil, nil, nil)
				// Assert the persisted wire contract consumed by the Python runner.
				raw, err := json.Marshal(episode)
				if err != nil {
					t.Fatal(err)
				}
				var persisted TaskEpisode
				if err := json.Unmarshal(raw, &persisted); err != nil {
					t.Fatal(err)
				}
				if got := persisted.Extra["llm_calls"]; got != float64(2) {
					t.Fatalf("llm_calls = %v, want 2", got)
				}
				if got := persisted.Extra["llm_time_ms"].(float64); got <= 0 {
					t.Fatalf("llm_time_ms = %v, want measured elapsed time", got)
				}
				wantTokens := float64(24)
				if tc.name == "error" {
					wantTokens = 0
				}
				if got := persisted.Extra["total_tokens"]; got != wantTokens {
					t.Fatalf("total_tokens = %v, want %v", got, wantTokens)
				}
				cost, exists := persisted.Extra["cost_usd"]
				if exists != tc.known || (exists && math.Abs(cost.(float64)-tc.want) > 1e-12) {
					t.Fatalf("cost_usd = %v (exists %t), want %v (exists %t)", cost, exists, tc.want, tc.known)
				}
				if tc.known && persisted.Extra["cost_source"] != "provider_reported" {
					t.Fatalf("cost_source = %v", persisted.Extra["cost_source"])
				}
				if tc.known && episodeCostDetails(persisted)["total"] != tc.want {
					t.Fatalf("exported cost = %v", episodeCostDetails(persisted))
				}
			})
		}
	}
}

func TestBenchmarkCostDoesNotBecomeCompleteAfterUnpricedCall(t *testing.T) {
	priced := usageResponse(10, 2, 12)
	priced.Choices[0].GenerationInfo["cost_usd"] = 0.001
	metrics := &RunMetrics{}
	for _, response := range []*llms.ContentResponse{priced, nil, priced} {
		recordModelCallMetrics(metrics, response, time.Now())
	}
	if metrics.LLMCalls != 3 || metrics.costReportedCalls != 2 || metrics.CostUSD != nil || metrics.CostSource != "" {
		t.Fatalf("incomplete cost must remain unknown: %#v", metrics)
	}
	episode := NewEpisodeRecorder(MemoryRetrieveRequest{}, MemoryContext{}).Finish("ok", metrics, nil, nil, nil)
	if _, exists := episode.Extra["cost_usd"]; exists {
		t.Fatalf("partial cost was exported: %#v", episode.Extra)
	}
	if episode.Extra["cost_reported_calls"] != 2 {
		t.Fatalf("billing coverage missing: %#v", episode.Extra)
	}
}

func TestBenchmarkProviderCostRequiresValidCompleteBilling(t *testing.T) {
	for _, tc := range []struct {
		info map[string]any
		ok   bool
		want float64
	}{
		{map[string]any{"total_cost": 0.002}, true, 0.002},
		{map[string]any{"input_cost": 0.001, "output_cost": 0.002}, true, 0.003},
		{map[string]any{"cost_usd": 0.0, "input_cost": 0.001}, true, 0},
		{map[string]any{"input_cost": 0.001}, false, 0},
		{map[string]any{"estimated_cost": 0.001}, false, 0},
		{map[string]any{"cost_usd": math.NaN()}, false, 0},
		{map[string]any{"cost_usd": math.Inf(1)}, false, 0},
		{map[string]any{"cost_usd": -0.1}, false, 0},
		{map[string]any{"cost_usd": "0.01"}, false, 0},
	} {
		response := &llms.ContentResponse{Choices: []*llms.ContentChoice{{GenerationInfo: tc.info}}}
		got, ok := providerResponseCostUSD(response)
		if ok != tc.ok || (ok && math.Abs(got-tc.want) > 1e-12) {
			t.Fatalf("billing %v = %v, %t; want %v, %t", tc.info, got, ok, tc.want, tc.ok)
		}
	}
	if got := telemetryCostDetailsFromMap(map[string]any{"cost_usd": 0.0, "input_cost": 0.001})["total"]; got != 0 {
		t.Fatalf("explicit zero cost was replaced: %v", got)
	}
}
