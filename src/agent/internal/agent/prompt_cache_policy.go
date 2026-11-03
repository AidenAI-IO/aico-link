package agent

import (
	"aico-agent/internal/logging"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"
)

type PromptCachePolicy string

const (
	PromptCachePolicyNone     PromptCachePolicy = "none"
	PromptCachePolicyImplicit PromptCachePolicy = "implicit"
	PromptCachePolicyExplicit PromptCachePolicy = "explicit"
)

func (p PromptCachePolicy) UsesExplicitCacheControl() bool {
	return p == PromptCachePolicyExplicit
}

func openRouterPromptCachePolicyFallback(model string) PromptCachePolicy {
	if openRouterExplicitPromptCacheSupported(model) {
		return PromptCachePolicyExplicit
	}
	return PromptCachePolicyNone
}

func resolveOpenRouterPromptCachePolicy(endpoints []openRouterEndpointMetadata) PromptCachePolicy {
	if len(endpoints) == 0 {
		return PromptCachePolicyNone
	}

	hasExplicit := false
	hasImplicit := false
	hasCachePricing := false

	for _, endpoint := range endpoints {
		if endpointSupportsExplicitPromptCache(endpoint) {
			hasExplicit = true
		}
		if endpoint.SupportsImplicitCaching != nil && *endpoint.SupportsImplicitCaching {
			hasImplicit = true
		}
		if endpointHasCacheReadPricing(endpoint.Pricing) {
			hasCachePricing = true
		}
	}

	switch {
	case hasExplicit:
		return PromptCachePolicyExplicit
	case hasImplicit || hasCachePricing:
		return PromptCachePolicyImplicit
	default:
		return PromptCachePolicyNone
	}
}

func endpointSupportsExplicitPromptCache(endpoint openRouterEndpointMetadata) bool {
	for _, param := range endpoint.SupportedParameters {
		switch strings.ToLower(strings.TrimSpace(param)) {
		case "cache_control", "prompt_cache":
			return true
		}
	}
	return false
}

func endpointHasCacheReadPricing(pricing map[string]any) bool {
	if len(pricing) == 0 {
		return false
	}
	for _, key := range []string{"input_cache_read", "cache_read", "cache_read_input"} {
		if value, ok := pricing[key]; ok && pricingValuePositive(value) {
			return true
		}
	}
	return false
}

func pricingValuePositive(value any) bool {
	switch typed := value.(type) {
	case string:
		typed = strings.TrimSpace(typed)
		return typed != "" && typed != "0"
	case float64:
		return typed > 0
	case int:
		return typed > 0
	default:
		return value != nil
	}
}

func openRouterModelEndpointsURL(baseURL, model string) (string, bool) {
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, "~")
	if idx := strings.Index(model, ":"); idx >= 0 {
		model = model[:idx]
	}
	parts := strings.SplitN(model, "/", 2)
	if len(parts) != 2 || strings.TrimSpace(parts[0]) == "" || strings.TrimSpace(parts[1]) == "" {
		return "", false
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1"
	}
	return fmt.Sprintf("%s/models/%s/%s/endpoints", baseURL, parts[0], parts[1]), true
}

type openRouterEndpointsResponse struct {
	Data openRouterEndpointsData `json:"data"`
}

type openRouterEndpointsData struct {
	Endpoints []openRouterEndpointMetadata `json:"endpoints"`
}

type openRouterEndpointMetadata struct {
	SupportedParameters     []string       `json:"supported_parameters"`
	SupportsImplicitCaching *bool          `json:"supports_implicit_caching"`
	Pricing                 map[string]any `json:"pricing"`
}

func (m *ModelManager) cachedOpenRouterPromptCachePolicy() PromptCachePolicy {
	if strings.ToLower(strings.TrimSpace(m.config.Provider)) != "openrouter" {
		return PromptCachePolicyNone
	}
	if policy, ok := m.readProviderPromptCachePolicyCache(); ok {
		return policy
	}
	ctx, cancel := context.WithTimeout(context.Background(), providerModelMetadataTimeout)
	defer cancel()
	policy, err := m.fetchOpenRouterPromptCachePolicy(ctx)
	if err != nil {
		return openRouterPromptCachePolicyFallback(m.config.Model)
	}
	if err := m.writeProviderPromptCachePolicyCache(policy); err != nil {
		logging.Warnf("agent", "model_spec", "write prompt cache policy cache %s: %v", m.providerMetadataCachePath, err)
	}
	return policy
}

func (m *ModelManager) fetchOpenRouterPromptCachePolicy(ctx context.Context) (PromptCachePolicy, error) {
	endpointURL, ok := openRouterModelEndpointsURL(m.config.BaseURL, m.config.Model)
	if !ok {
		return openRouterPromptCachePolicyFallback(m.config.Model), nil
	}
	token := resolveToken(m.config)
	if token == "" {
		return PromptCachePolicyNone, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpointURL, nil)
	if err != nil {
		return PromptCachePolicyNone, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := m.modelMetadataHTTPClient().Do(req)
	if err != nil {
		return PromptCachePolicyNone, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return PromptCachePolicyNone, fmt.Errorf("openrouter endpoints returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload openRouterEndpointsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return PromptCachePolicyNone, err
	}
	return resolveOpenRouterPromptCachePolicy(payload.Data.Endpoints), nil
}

func (m *ModelManager) readProviderPromptCachePolicyCache() (PromptCachePolicy, bool) {
	if strings.TrimSpace(m.providerMetadataCachePath) == "" {
		return PromptCachePolicyNone, false
	}
	data, err := os.ReadFile(m.providerMetadataCachePath)
	if err != nil {
		return PromptCachePolicyNone, false
	}
	var cache providerModelMetadataCacheFile
	if err := json.Unmarshal(data, &cache); err != nil {
		return PromptCachePolicyNone, false
	}
	if cache.Version != providerModelMetadataCacheVersion {
		return PromptCachePolicyNone, false
	}
	entry, ok := cache.Entries[m.providerModelSpecCacheKey()]
	if !ok || !m.providerModelSpecCacheEntryMatches(entry) {
		return PromptCachePolicyNone, false
	}
	if entry.PromptCachePolicy == "" {
		return PromptCachePolicyNone, false
	}
	return PromptCachePolicy(entry.PromptCachePolicy), true
}

func (m *ModelManager) writeProviderPromptCachePolicyCache(policy PromptCachePolicy) error {
	if strings.TrimSpace(m.providerMetadataCachePath) == "" || policy == "" {
		return nil
	}
	m.specMu.Lock()
	defer m.specMu.Unlock()

	cache := providerModelMetadataCacheFile{
		Version: providerModelMetadataCacheVersion,
		Entries: map[string]providerModelMetadataCacheEntry{},
	}
	data, err := os.ReadFile(m.providerMetadataCachePath)
	if err == nil && len(data) > 0 {
		var existing providerModelMetadataCacheFile
		if json.Unmarshal(data, &existing) == nil && existing.Version == providerModelMetadataCacheVersion {
			cache = existing
			if cache.Entries == nil {
				cache.Entries = map[string]providerModelMetadataCacheEntry{}
			}
		}
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}

	key := m.providerModelSpecCacheKey()
	entry := cache.Entries[key]
	entry.Provider = normalizedProviderName(m.config.Provider)
	entry.Model = normalizedModelName(m.config.Model)
	entry.Endpoint = m.providerMetadataEndpoint()
	entry.PromptCachePolicy = string(policy)
	entry.FetchedAt = time.Now().UTC()
	cache.Entries[key] = entry

	encoded, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeFileAtomic(m.providerMetadataCachePath, encoded, 0o644)
}
