package agent

import (
	"fmt"
	"net/url"
	"strings"

	"aico-agent/internal/util"
)

type modelsDevTokenRates struct {
	Input      *float64 `json:"input"`
	Output     *float64 `json:"output"`
	CacheRead  *float64 `json:"cache_read"`
	CacheWrite *float64 `json:"cache_write"`
}

type modelsDevCost struct {
	modelsDevTokenRates
	Tiers []struct {
		modelsDevTokenRates
		Tier struct {
			Type string `json:"type"`
			Size int    `json:"size"`
		} `json:"tier"`
	} `json:"tiers"`
	ContextOver200K *modelsDevTokenRates `json:"context_over_200k"`
}

type modelsDevPriceKey struct {
	provider  string
	model     string
	reference bool
}

func modelsDevPriceKeyForEndpoint(provider, modelName, host string) modelsDevPriceKey {
	host = strings.ToLower(strings.TrimSuffix(host, "."))
	key := modelsDevPriceKey{model: strings.TrimSpace(modelName)}
	switch host {
	case "api.openai.com":
		key.provider = "openai"
	case "api.anthropic.com":
		key.provider = "anthropic"
	case "generativelanguage.googleapis.com":
		key.provider = "google"
		key.model = strings.TrimPrefix(key.model, "models/")
	case "openrouter.ai":
		key.provider = "openrouter"
	case "api.deepseek.com":
		key.provider = "deepseek"
	case "api.moonshot.ai":
		key.provider = "moonshotai"
	case "api.moonshot.cn":
		key.provider = "moonshotai-cn"
	case "ark.cn-beijing.volces.com":
		key.provider = "volcengine"
	case "apibest.ai", "api.apibest.ai":
		// The gateway exposes this exact OpenAI model ID. Its own rates
		// are unavailable, so use the publisher's rate as a reference.
		if provider == "openai" && key.model == "gpt-6-luna" {
			key.provider = "openai"
			key.reference = true
		}
	}
	return key
}

func modelsDevPriceKeyForConfig(cfg ModelConfig) modelsDevPriceKey {
	endpoint, err := url.Parse(modelAPIEndpoint(cfg.Provider, cfg.BaseURL))
	if err != nil {
		return modelsDevPriceKey{}
	}
	return modelsDevPriceKeyForEndpoint(cfg.Provider, cfg.Model, endpoint.Hostname())
}

func (m *ModelManager) prefetchModelsDevPricing() {
	key := modelsDevPriceKeyForConfig(m.config)
	if key.provider == "" || key.model == "" {
		return
	}
	m.startModelsDevCatalog(m.modelsDevEndpoint())
}

// cachedModelsDevCatalog starts an eligible load and returns without waiting.
func (m *ModelManager) cachedModelsDevCatalog() map[string]modelsDevProvider {
	pending := m.startModelsDevCatalog(m.modelsDevEndpoint())
	select {
	case <-pending.ready:
		return pending.catalog
	default:
		return nil
	}
}

type catalogPriceEstimate struct {
	usd     float64
	rate    string
	partial bool
}

func (key modelsDevPriceKey) estimate(catalog map[string]modelsDevProvider, input, cached, cacheWrite, output int, aggregated bool) (catalogPriceEstimate, bool) {
	provider := catalog[key.provider]
	cost := provider.Models[key.model].Cost
	if cost == nil || input < 0 || output < 0 || cached < 0 || cacheWrite < 0 ||
		cached > input || cacheWrite > input-cached || (input == 0 && output == 0) {
		return catalogPriceEstimate{}, false
	}
	rates := cost.modelsDevTokenRates
	threshold := 0
	for _, tier := range cost.Tiers {
		if tier.Tier.Type != "context" || tier.Tier.Size <= 0 {
			return catalogPriceEstimate{}, false
		}
		if input > tier.Tier.Size && tier.Tier.Size > threshold {
			rates = tier.modelsDevTokenRates
			threshold = tier.Tier.Size
		}
	}
	if len(cost.Tiers) == 0 && cost.ContextOver200K != nil && input > 200_000 {
		rates = *cost.ContextOver200K
		threshold = 200_000
	}
	if aggregated && threshold > 0 {
		return catalogPriceEstimate{}, false
	}
	var usd float64
	for _, bucket := range []struct {
		tokens int
		rate   *float64
	}{
		{input - cached - cacheWrite, rates.Input},
		{cached, rates.CacheRead},
		{cacheWrite, rates.CacheWrite},
		{output, rates.Output},
	} {
		if bucket.tokens == 0 {
			continue
		}
		if bucket.rate == nil {
			return catalogPriceEstimate{}, false
		}
		rate, ok := costMetricFloat(*bucket.rate)
		if !ok {
			return catalogPriceEstimate{}, false
		}
		usd += float64(bucket.tokens) * (rate / 1_000_000)
	}
	if _, ok := costMetricFloat(usd); !ok {
		return catalogPriceEstimate{}, false
	}
	source := "models.dev"
	if key.reference {
		displayName := firstNonEmptyString([]string{provider.Name, key.provider})
		source = displayName + " reference price"
	}
	tier := "standard"
	if threshold > 0 {
		tier = fmt.Sprintf("input > %d", threshold)
	}
	return catalogPriceEstimate{usd: usd, rate: fmt.Sprintf("%s · %s/%s (%s)", source, key.provider, key.model, tier)}, true
}

func (key modelsDevPriceKey) estimateUsage(catalog map[string]modelsDevProvider, info map[string]any) (catalogPriceEstimate, bool) {
	input, hasInput := util.UsageMetricInt(info["prompt_tokens"])
	output, hasOutput := util.UsageMetricInt(info["completion_tokens"])
	if !hasInput || !hasOutput {
		return catalogPriceEstimate{}, false
	}
	cached, _ := util.UsageMetricInt(info["cached_tokens"])
	cacheWrite, _ := util.UsageMetricInt(info["cache_creation_input_tokens"])
	return key.estimate(catalog, input, cached, cacheWrite, output, false)
}

func estimateHistoricalCatalogCost(episode TaskEpisode, catalog map[string]modelsDevProvider) (catalogPriceEstimate, bool) {
	if len(catalog) == 0 {
		return catalogPriceEstimate{}, false
	}
	reported, _ := util.UsageMetricInt(episode.Extra["cost_reported_calls"])
	if reported > 0 {
		return catalogPriceEstimate{}, false
	}
	provider := extraString(episode.Extra, "model_provider")
	modelName := extraString(episode.Extra, "model_name")
	host := extraString(episode.Extra, "model_provider_host")
	key := modelsDevPriceKeyForEndpoint(provider, modelName, host)
	if host == "" {
		key = modelsDevPriceKeyForConfig(ModelConfig{Provider: provider, Model: modelName})
		// Legacy metadata may omit an override endpoint. Label any publisher
		// rate as a reference when the route cannot be identified.
		key.reference = provider == "openai" || provider == "anthropic" || provider == "gemini"
	}
	input, hasInput := util.UsageMetricInt(episode.Extra["prompt_tokens"])
	output, hasOutput := util.UsageMetricInt(episode.Extra["completion_tokens"])
	if !hasInput || !hasOutput {
		return catalogPriceEstimate{}, false
	}
	// Older Anthropic records only capture cache reads in the episode
	// summary. Cache writes cannot be separated from uncached input.
	if key.provider == "anthropic" {
		return catalogPriceEstimate{}, false
	}
	cached, _ := util.UsageMetricInt(episode.Extra["cached_prompt_tokens"])
	calls, _ := util.UsageMetricInt(episode.Extra["llm_calls"])
	estimate, ok := key.estimate(catalog, input, cached, 0, output, calls != 1)
	if ok {
		if usageCalls, tracked := util.UsageMetricInt(episode.Extra["usage_reported_calls"]); tracked {
			estimate.partial = usageCalls < calls
		} else {
			// One call with recorded input and output can be priced directly.
			// Legacy totals cannot establish coverage over multiple calls.
			estimate.partial = calls != 1
		}
	}
	return estimate, ok
}
