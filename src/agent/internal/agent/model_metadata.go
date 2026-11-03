package agent

import (
	"aico-agent/internal/agent/model"
	"aico-agent/internal/logging"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const providerModelMetadataTimeout = 5 * time.Second
const providerModelMetadataCacheVersion = 1
const maxModelsDevCatalogBytes = 8 * 1024 * 1024

type modelsDevCatalogCacheEntry struct {
	ready   chan struct{}
	catalog map[string]modelsDevProvider
	err     error
}

var modelsDevCatalogCache sync.Map

func providerSupportsModelMetadata(provider string) bool {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "openrouter", "ollama", "anthropic", "openai", "volcengine", "kimi", "kimi-cn", "deepseek":
		return true
	default:
		return false
	}
}

func (m *ModelManager) prefetchProviderModelSpecIfNeeded() {
	if !m.needsProviderModelMetadata() {
		return
	}
	m.cachedProviderModelSpec()
}

func (m *ModelManager) needsProviderModelMetadata() bool {
	spec, _ := LookupModelSpec(m.config.Provider, m.config.Model)
	return m.needsProviderModelMetadataForSpec(spec)
}

func (m *ModelManager) needsProviderModelMetadataForSpec(spec model.ModelSpec) bool {
	if strings.TrimSpace(m.config.Model) == "" {
		return false
	}
	if !providerSupportsModelMetadata(m.config.Provider) {
		return false
	}
	explicitContextWindow := m.config.ContextWindow > 0
	explicitMaxOutput := m.config.ModelMaxOutputTokens > 0
	needsReasoning := spec.Reasoning == nil && modelsDevProviderID(m.config.Provider) != "openrouter" &&
		modelsDevProviderID(m.config.Provider) != "ollama"
	return (!explicitContextWindow && spec.ContextWindow <= 0) ||
		(!explicitMaxOutput && spec.MaxOutput <= 0) || needsReasoning
}

func (m *ModelManager) cachedProviderModelSpec() model.ModelSpec {
	if strings.TrimSpace(m.config.Model) == "" {
		return model.ModelSpec{}
	}

	m.specMu.Lock()
	if m.providerSpecLoaded {
		spec := m.providerSpec
		m.specMu.Unlock()
		return spec
	}
	if m.providerSpecFetchStarted {
		m.specMu.Unlock()
		return model.ModelSpec{}
	}
	m.providerSpecFetchStarted = true
	m.specMu.Unlock()

	if spec, ok := m.readProviderModelSpecCache(); ok {
		m.storeProviderModelSpec(spec)
		return spec
	}

	go m.fetchProviderModelSpecInBackground()
	return model.ModelSpec{}
}

func (m *ModelManager) fetchProviderModelSpecInBackground() {
	ctx, cancel := context.WithTimeout(context.Background(), providerModelMetadataTimeout)
	defer cancel()

	spec, err := m.fetchProviderModelSpec(ctx)
	if err != nil {
		m.resetProviderModelSpecFetchStarted()
		logging.Warnf("agent", "model_spec", "fetch %s/%s metadata: %v", m.config.Provider, m.config.Model, err)
		return
	}
	if hasProviderModelSpecMetadata(spec) {
		if err := m.writeProviderModelSpecCache(spec); err != nil {
			logging.Warnf("agent", "model_spec", "write provider metadata cache %s: %v", m.providerMetadataCachePath, err)
		}
	}
	m.storeProviderModelSpec(spec)
}

func (m *ModelManager) resetProviderModelSpecFetchStarted() {
	m.specMu.Lock()
	defer m.specMu.Unlock()

	m.providerSpecFetchStarted = false
	m.providerSpecLoaded = false
}

func (m *ModelManager) storeProviderModelSpec(spec model.ModelSpec) {
	m.specMu.Lock()
	defer m.specMu.Unlock()

	m.providerSpec = spec
	m.providerSpecLoaded = true
}

type providerModelMetadataCacheFile struct {
	Version int                                        `json:"version"`
	Entries map[string]providerModelMetadataCacheEntry `json:"entries"`
}

type providerModelMetadataCacheEntry struct {
	Provider          string          `json:"provider"`
	Model             string          `json:"model"`
	Endpoint          string          `json:"endpoint"`
	Spec              model.ModelSpec `json:"spec"`
	PromptCachePolicy string          `json:"prompt_cache_policy,omitempty"`
	FetchedAt         time.Time       `json:"fetched_at"`
}

func (m *ModelManager) readProviderModelSpecCache() (model.ModelSpec, bool) {
	if strings.TrimSpace(m.providerMetadataCachePath) == "" {
		return model.ModelSpec{}, false
	}

	data, err := os.ReadFile(m.providerMetadataCachePath)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			logging.Warnf("agent", "model_spec", "read provider metadata cache %s: %v", m.providerMetadataCachePath, err)
		}
		return model.ModelSpec{}, false
	}

	var cache providerModelMetadataCacheFile
	if err := json.Unmarshal(data, &cache); err != nil {
		logging.Warnf("agent", "model_spec", "parse provider metadata cache %s: %v", m.providerMetadataCachePath, err)
		return model.ModelSpec{}, false
	}
	if cache.Version != providerModelMetadataCacheVersion {
		return model.ModelSpec{}, false
	}

	entry, ok := cache.Entries[m.providerModelSpecCacheKey()]
	if !ok || !m.providerModelSpecCacheEntryMatches(entry) || !hasProviderModelSpecMetadata(entry.Spec) {
		return model.ModelSpec{}, false
	}
	return entry.Spec, true
}

func (m *ModelManager) writeProviderModelSpecCache(spec model.ModelSpec) error {
	if strings.TrimSpace(m.providerMetadataCachePath) == "" || !hasProviderModelSpecMetadata(spec) {
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
	if entry.Provider == "" {
		entry.Provider = normalizedProviderName(m.config.Provider)
		entry.Model = normalizedModelName(m.config.Model)
		entry.Endpoint = m.providerMetadataEndpoint()
	}
	entry.Spec = spec
	entry.FetchedAt = time.Now().UTC()
	cache.Entries[key] = entry

	encoded, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		return err
	}
	encoded = append(encoded, '\n')
	return writeFileAtomic(m.providerMetadataCachePath, encoded, 0o644)
}

func hasProviderModelSpecMetadata(spec model.ModelSpec) bool {
	return spec.ContextWindow > 0 || spec.MaxOutput > 0 || spec.Reasoning != nil
}

func (m *ModelManager) providerModelSpecCacheKey() string {
	parts := []string{
		normalizedProviderName(m.config.Provider),
		m.providerMetadataEndpoint(),
		normalizedModelName(m.config.Model),
	}
	sum := sha256.Sum256([]byte(strings.Join(parts, "\x00")))
	return hex.EncodeToString(sum[:])
}

func (m *ModelManager) providerModelSpecCacheEntryMatches(entry providerModelMetadataCacheEntry) bool {
	return entry.Provider == normalizedProviderName(m.config.Provider) &&
		entry.Model == normalizedModelName(m.config.Model) &&
		entry.Endpoint == m.providerMetadataEndpoint()
}

func (m *ModelManager) providerMetadataEndpoint() string {
	switch normalizedProviderName(m.config.Provider) {
	case "openrouter":
		return openRouterModelsURL(m.config.BaseURL)
	case "ollama":
		return ollamaShowURL(m.config.BaseURL)
	case "anthropic", "openai", "volcengine", "kimi", "kimi-cn", "deepseek":
		if strings.TrimSpace(m.modelsDevURL) != "" {
			return strings.TrimSpace(m.modelsDevURL)
		}
		return defaultModelsDevURL
	default:
		return ""
	}
}

func normalizedProviderName(provider string) string {
	return strings.ToLower(strings.TrimSpace(provider))
}

func normalizedModelName(model string) string {
	return strings.ToLower(strings.TrimSpace(model))
}

func (m *ModelManager) fetchProviderModelSpec(ctx context.Context) (model.ModelSpec, error) {
	switch strings.ToLower(strings.TrimSpace(m.config.Provider)) {
	case "openrouter":
		return m.fetchOpenRouterModelSpec(ctx)
	case "ollama":
		return m.fetchOllamaModelSpec(ctx)
	case "anthropic", "openai", "volcengine", "kimi", "kimi-cn", "deepseek":
		return m.fetchModelsDevModelSpec(ctx)
	default:
		return model.ModelSpec{}, nil
	}
}

const defaultModelsDevURL = "https://models.dev/api.json"

type modelsDevProvider struct {
	API    string                    `json:"api"`
	Models map[string]modelsDevModel `json:"models"`
}

type modelsDevModel struct {
	ID               string                     `json:"id"`
	Reasoning        bool                       `json:"reasoning"`
	ReasoningOptions []modelsDevReasoningOption `json:"reasoning_options"`
	Temperature      *bool                      `json:"temperature"`
	Limit            struct {
		Context int `json:"context"`
		Output  int `json:"output"`
	} `json:"limit"`
	Provider struct {
		API   string `json:"api"`
		Shape string `json:"shape"`
	} `json:"provider"`
}

type modelsDevReasoningOption struct {
	Type   string   `json:"type"`
	Values []string `json:"values"`
	Min    int      `json:"min"`
	Max    int      `json:"max"`
}

func (m *ModelManager) fetchModelsDevModelSpec(ctx context.Context) (model.ModelSpec, error) {
	endpoint := strings.TrimSpace(m.modelsDevURL)
	if endpoint == "" {
		endpoint = defaultModelsDevURL
	}
	catalog, err := m.loadModelsDevCatalog(ctx, endpoint)
	if err != nil {
		return model.ModelSpec{}, err
	}
	providerID := modelsDevProviderID(m.config.Provider)
	provider, ok := catalog[providerID]
	if !ok {
		return model.ModelSpec{}, nil
	}
	metadata, ok := lookupModelsDevModel(provider.Models, m.config.Model)
	if !ok {
		return model.ModelSpec{}, nil
	}
	spec := model.ModelSpec{
		API:           provider.API,
		APIShape:      metadata.Provider.Shape,
		ContextWindow: metadata.Limit.Context,
		MaxOutput:     metadata.Limit.Output,
	}
	if spec.API == "" {
		spec.API = metadata.Provider.API
	}
	reasoning := &model.ReasoningSpec{Supported: metadata.Reasoning}
	for _, option := range metadata.ReasoningOptions {
		switch strings.ToLower(strings.TrimSpace(option.Type)) {
		case "toggle":
			reasoning.CanDisable = true
			if reasoning.Mode == "" {
				reasoning.Mode = "toggle"
			}
		case "effort":
			reasoning.Mode = "effort"
			for _, value := range option.Values {
				value = strings.ToLower(strings.TrimSpace(value))
				if value == "" || value == "default" || containsStringFold(reasoning.Efforts, value) {
					continue
				}
				reasoning.Efforts = append(reasoning.Efforts, value)
				if value == "none" {
					reasoning.CanDisable = true
				}
			}
		case "budget_tokens":
			if reasoning.Mode == "" {
				reasoning.Mode = "budget_tokens"
			}
			reasoning.BudgetTokensMin = option.Min
			reasoning.BudgetTokensMax = option.Max
		}
	}
	// Native Anthropic reasoning is disabled by omitting the reasoning object,
	// even when models.dev lists only effort or budget controls and no explicit
	// toggle option.
	if providerID == "anthropic" && reasoning.Supported {
		reasoning.CanDisable = true
	}
	// A catalog hit is authoritative even when reasoning is false and no
	// options are listed. Preserve that explicit unsupported declaration so the
	// configuration UI does not fall back to generic effort choices.
	spec.Reasoning = reasoning
	return spec, nil
}

func (m *ModelManager) loadModelsDevCatalog(ctx context.Context, endpoint string) (map[string]modelsDevProvider, error) {
	pending := &modelsDevCatalogCacheEntry{ready: make(chan struct{})}
	actual, loaded := modelsDevCatalogCache.LoadOrStore(endpoint, pending)
	entry := actual.(*modelsDevCatalogCacheEntry)
	if loaded {
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-entry.ready:
			return entry.catalog, entry.err
		}
	}

	entry.catalog, entry.err = m.downloadModelsDevCatalog(ctx, endpoint)
	if entry.err != nil {
		modelsDevCatalogCache.Delete(endpoint)
	}
	close(entry.ready)
	return entry.catalog, entry.err
}

func (m *ModelManager) downloadModelsDevCatalog(ctx context.Context, endpoint string) (map[string]modelsDevProvider, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := m.modelMetadataHTTPClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("models.dev returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxModelsDevCatalogBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read models.dev catalog: %w", err)
	}
	if len(body) > maxModelsDevCatalogBytes {
		return nil, fmt.Errorf("models.dev catalog exceeds %d-byte limit", maxModelsDevCatalogBytes)
	}
	var catalog map[string]modelsDevProvider
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, fmt.Errorf("decode models.dev catalog: %w", err)
	}
	return catalog, nil
}

func modelsDevProviderID(provider string) string {
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "kimi":
		return "moonshotai"
	case "kimi-cn":
		return "moonshotai-cn"
	default:
		return strings.ToLower(strings.TrimSpace(provider))
	}
}

func lookupModelsDevModel(models map[string]modelsDevModel, name string) (modelsDevModel, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	candidates := []string{want, strings.TrimPrefix(want, "~")}
	if i := strings.LastIndex(want, "/"); i >= 0 {
		candidates = append(candidates, want[i+1:])
	}
	for _, candidate := range candidates {
		if metadata, ok := models[candidate]; ok {
			return metadata, true
		}
	}
	for key, metadata := range models {
		if strings.EqualFold(strings.TrimPrefix(key, "~"), strings.TrimPrefix(want, "~")) {
			return metadata, true
		}
	}
	return modelsDevModel{}, false
}

func mergeModelSpecs(primary, supplemental model.ModelSpec) model.ModelSpec {
	if primary.ContextWindow <= 0 {
		primary.ContextWindow = supplemental.ContextWindow
	}
	if primary.MaxOutput <= 0 {
		primary.MaxOutput = supplemental.MaxOutput
	}
	if primary.API == "" {
		primary.API = supplemental.API
	}
	if primary.APIShape == "" {
		primary.APIShape = supplemental.APIShape
	}
	if primary.Reasoning == nil {
		primary.Reasoning = supplemental.Reasoning
	}
	return primary
}

func (m *ModelManager) modelMetadataHTTPClient() *http.Client {
	if m.metadataHTTPClient != nil {
		return m.metadataHTTPClient
	}
	return newProxyHTTPClient(m.proxy)
}

func (m *ModelManager) fetchOpenRouterModelSpec(ctx context.Context) (model.ModelSpec, error) {
	token := resolveToken(m.config)
	if token == "" {
		return model.ModelSpec{}, nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, openRouterModelsURL(m.config.BaseURL), nil)
	if err != nil {
		return model.ModelSpec{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := m.modelMetadataHTTPClient().Do(req)
	if err != nil {
		return model.ModelSpec{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return model.ModelSpec{}, fmt.Errorf("openrouter models returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload openRouterModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return model.ModelSpec{}, err
	}

	modelID := strings.ToLower(strings.TrimSpace(m.config.Model))
	for _, m := range payload.Data {
		if strings.ToLower(strings.TrimSpace(m.ID)) != modelID {
			continue
		}
		spec := model.ModelSpec{
			ContextWindow: firstPositiveInt(m.TopProvider.ContextLength, m.ContextLength),
			MaxOutput:     m.TopProvider.MaxCompletionTokens,
		}
		return spec, nil
	}
	return model.ModelSpec{}, nil
}

func openRouterModelsURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "https://openrouter.ai/api/v1"
	}
	return baseURL + "/models"
}

type openRouterModelsResponse struct {
	Data []openRouterModelMetadata `json:"data"`
}

type openRouterModelMetadata struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
	TopProvider   struct {
		ContextLength       int `json:"context_length"`
		MaxCompletionTokens int `json:"max_completion_tokens"`
	} `json:"top_provider"`
}

func (m *ModelManager) fetchOllamaModelSpec(ctx context.Context) (model.ModelSpec, error) {
	body, err := json.Marshal(map[string]string{"model": m.config.Model})
	if err != nil {
		return model.ModelSpec{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, ollamaShowURL(m.config.BaseURL), bytes.NewReader(body))
	if err != nil {
		return model.ModelSpec{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := m.modelMetadataHTTPClient().Do(req)
	if err != nil {
		return model.ModelSpec{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return model.ModelSpec{}, fmt.Errorf("ollama show returned %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload ollamaShowResponse
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return model.ModelSpec{}, err
	}

	return model.ModelSpec{ContextWindow: firstPositiveInt(
		ollamaParametersNumCtx(payload.Parameters),
		ollamaModelInfoContextLength(payload.ModelInfo),
	)}, nil
}

func ollamaShowURL(baseURL string) string {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://localhost:11434"
	}
	return baseURL + "/api/show"
}

type ollamaShowResponse struct {
	ModelInfo  map[string]json.RawMessage `json:"model_info"`
	Parameters json.RawMessage            `json:"parameters"`
}

func ollamaModelInfoContextLength(modelInfo map[string]json.RawMessage) int {
	best := 0
	for key, raw := range modelInfo {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if normalized != "context_length" && !strings.HasSuffix(normalized, ".context_length") {
			continue
		}
		if value := positiveJSONInt(raw); value > best {
			best = value
		}
	}
	return best
}

func ollamaParametersNumCtx(raw json.RawMessage) int {
	if len(raw) == 0 || string(raw) == "null" {
		return 0
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err == nil {
		for key, value := range fields {
			if strings.EqualFold(strings.TrimSpace(key), "num_ctx") {
				return positiveJSONInt(value)
			}
		}
	}

	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0
	}
	return parseOllamaNumCtx(text)
}

func parseOllamaNumCtx(text string) int {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.ReplaceAll(line, "=", " ")
		parts := strings.Fields(line)
		for i, part := range parts {
			if !strings.EqualFold(part, "num_ctx") || i+1 >= len(parts) {
				continue
			}
			if value, err := strconv.Atoi(strings.Trim(parts[i+1], `"'`)); err == nil && value > 0 {
				return value
			}
		}
	}
	return 0
}

func positiveJSONInt(raw json.RawMessage) int {
	var value int
	if err := json.Unmarshal(raw, &value); err == nil && value > 0 {
		return value
	}
	var floatValue float64
	if err := json.Unmarshal(raw, &floatValue); err == nil && floatValue > 0 {
		return int(floatValue)
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		parsed, err := strconv.Atoi(strings.TrimSpace(text))
		if err == nil && parsed > 0 {
			return parsed
		}
	}
	return 0
}

func firstPositiveInt(values ...int) int {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}
