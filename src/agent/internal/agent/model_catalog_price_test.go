package agent

import (
	"context"
	"math"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

func TestCatalogPricingRequiresEndpointAndExactModel(t *testing.T) {
	catalog, err := decodeModelsDevCatalog([]byte(`{
		"openai":{"name":"OpenAI","models":{"gpt-6-luna":{"cost":{
			"input":0.1,"cache_read":0.01,"output":0.5,
			"tiers":[{"input":0.2,"cache_read":0.02,"output":0.75,"tier":{"type":"context","size":272000}}],
			"context_over_200k":{"input":99,"output":99}
		}}}},
		"openrouter":{"models":{"openai/gpt-6-luna":{"cost":{"input":1,"cache_read":0.1,"output":5}}}}
	}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name, provider, host, model string
		input, read, output         int
		aggregated                  bool
		want                        float64
		ok                          bool
	}{
		{"reference", "openai", "apibest.ai", "gpt-6-luna", 74156, 71552, 71, true, .00101142, true},
		{"publisher", "openai", "api.openai.com", "gpt-6-luna", 272000, 0, 1000, false, .0277, true},
		{"long tier", "openai", "api.openai.com", "gpt-6-luna", 300000, 0, 1000, false, .06075, true},
		{"ambiguous tier", "openai", "api.openai.com", "gpt-6-luna", 300000, 0, 1000, true, 0, false},
		{"gateway not known", "openai", "other.example", "gpt-6-luna", 1000, 0, 1000, false, 0, false},
		{"wrong namespace", "openai", "api.openai.com", "other/gpt-6-luna", 1000, 0, 1000, false, 0, false},
		{"aggregator", "openrouter", "openrouter.ai", "openai/gpt-6-luna", 1000, 0, 1000, false, .006, true},
		{"unqualified aggregator", "openrouter", "openrouter.ai", "gpt-6-luna", 1000, 0, 1000, false, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			key := modelsDevPriceKeyForEndpoint(test.provider, test.model, test.host)
			got, ok := key.estimate(catalog, test.input, test.read, 0, test.output, test.aggregated)
			if ok != test.ok || math.Abs(got.usd-test.want) > 1e-12 {
				t.Fatalf("estimate = %+v (%t), want %v (%t)", got, ok, test.want, test.ok)
			}
		})
	}
}

func TestTaskHistoryPersistsCatalogPriceAfterProviderAndTelemetry(t *testing.T) {
	metrics := &RunMetrics{}
	var estimates int
	tracked := &usageTrackingModel{
		metrics: metrics,
		catalogEstimate: func(map[string]any) (catalogPriceEstimate, bool) {
			estimates++
			return catalogPriceEstimate{usd: .25, rate: "Catalog rate"}, true
		},
	}
	for _, info := range []map[string]any{{"cost_usd": 0}, {"estimated_cost": .125}, {"total_tokens": 1000}} {
		tracked.recordCallMetrics(&llms.ContentResponse{Choices: []*llms.ContentChoice{{GenerationInfo: info}}}, time.Now())
	}
	episode := NewEpisodeRecorder(MemoryRetrieveRequest{Input: "task"}, MemoryContext{}).
		Finish("", metrics, nil, nil, nil)
	cost, ok := taskHistoryEpisodeCost(episode)
	if !ok || cost.USD != .375 || cost.Partial || !cost.Estimated || cost.Source != "mixed" ||
		estimates != 1 || extraString(episode.Extra, "history_catalog_rates") != "Catalog rate" {
		t.Fatalf("cost = %+v (%t), estimates = %d, metadata = %#v", cost, ok, estimates, episode.Extra)
	}
	if _, ok := episode.Extra["cost_usd"]; ok {
		t.Fatal("estimated cost leaked into provider billing")
	}
}

func TestTaskHistoryBackfillPreservesUsageCoverage(t *testing.T) {
	catalog, err := decodeModelsDevCatalog([]byte(`{"openai":{"models":{"test-model":{"cost":{"input":1,"output":2}}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		second  *llms.ContentResponse
		legacy  bool
		partial bool
		usd     float64
	}{
		{name: "complete", second: usageResponse(100, 10, 110), usd: .00024},
		{name: "missing response", partial: true, usd: .00012},
		{name: "legacy aggregate", second: usageResponse(100, 10, 110), legacy: true, partial: true, usd: .00024},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			tracked := &usageTrackingModel{
				inner:   &scriptedModel{responses: []*llms.ContentResponse{usageResponse(80, 20, 100), test.second}},
				metrics: &RunMetrics{},
			}
			for range 2 {
				if _, err := tracked.GenerateContent(ctx, nil); err != nil {
					t.Fatal(err)
				}
			}
			episode := NewEpisodeRecorder(MemoryRetrieveRequest{Input: "task"}, MemoryContext{}).
				Finish("", tracked.metrics, nil, nil, nil)
			episode.Extra["model_provider"] = "openai"
			episode.Extra["model_name"] = "test-model"
			if test.legacy {
				delete(episode.Extra, "usage_reported_calls")
			}
			store := NewTaskEpisodeStore(t.TempDir())
			if _, err := store.AddEpisode(ctx, episode); err != nil {
				t.Fatal(err)
			}
			index, err := store.loadIndex()
			if err != nil {
				t.Fatal(err)
			}
			groups := groupTaskHistory(index, nil)
			if len(groups) != 1 {
				t.Fatalf("got %d history groups, want one", len(groups))
			}
			detail, err := store.readTaskHistory(groups[0], true, catalog)
			if err != nil {
				t.Fatal(err)
			}
			if detail.Cost == nil || math.Abs(detail.Cost.USD-test.usd) > 1e-12 || detail.Cost.Partial != test.partial {
				t.Fatalf("backfilled cost = %+v, want %v with partial=%t", detail.Cost, test.usd, test.partial)
			}
		})
	}
}
