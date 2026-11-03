package agent

import (
	"aico-agent/internal/agent/model"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func waitForModelSpec(t *testing.T, mgr *ModelManager, want model.ModelSpec) model.ModelSpec {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for {
		got := mgr.Spec()
		if got.ContextWindow == want.ContextWindow && got.MaxOutput == want.MaxOutput {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("Spec() = %+v, want %+v", got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestLookupModelSpecKnownModels(t *testing.T) {
	tests := []struct {
		name          string
		provider      string
		model         string
		wantContext   int
		wantMaxOutput int
	}{
		{"openrouter gemini flash", "openrouter", "google/gemini-3.5-flash", 1_048_576, 65_536},
		{"claude sonnet 3.5", "openrouter", "anthropic/claude-3.5-sonnet", 200_000, 8_192},
		{"bytedance seed lite", "openrouter", "bytedance-seed/seed-2.0-lite", 128_000, 8_192},
		{"gpt-4o", "openai", "openai/gpt-4o", 128_000, 16_384},
		{"gpt-5.5 prefixed", "openai", "openai/gpt-5.5", 1_050_000, 128_000},
		{"gpt-5.5 bare", "openai", "gpt-5.5", 1_050_000, 128_000},
		{"gpt-5.5 pro bare", "openai", "gpt-5.5-pro", 1_050_000, 128_000},
		{"gpt-5.4 prefixed", "openai", "openai/gpt-5.4", 1_050_000, 128_000},
		{"gpt-5.4 bare", "openai", "gpt-5.4", 1_050_000, 128_000},
		{"gpt-5.4 mini bare", "openai", "gpt-5.4-mini", 400_000, 128_000},
		{"gpt-5.4 nano bare", "openai", "gpt-5.4-nano", 400_000, 128_000},
		{"claude fable 5 bare", "anthropic", "claude-fable-5", 1_000_000, 128_000},
		{"claude opus 5 prefixed", "openrouter", "anthropic/claude-opus-5", 1_000_000, 128_000},
		{"claude opus 4.8 bare", "anthropic", "claude-opus-4-8", 1_000_000, 128_000},
		{"claude sonnet 4.6 prefixed", "openrouter", "anthropic/claude-sonnet-4-6", 1_000_000, 128_000},
		{"claude haiku 4.5 bare", "anthropic", "claude-haiku-4-5", 200_000, 64_000},
		{"gemini 3.5 pro bare", "google", "gemini-3.5-pro", 1_048_576, 65_536},
		{"kimi k3 bare", "openai", "kimi-k3", 1_048_576, 131_072},
		{"kimi k3 prefixed", "openrouter", "moonshotai/kimi-k3", 1_048_576, 131_072},
		{"doubao seed 2.1 pro", "volcengine", "doubao-seed-2-1-pro-260628", 262_144, 131_072},
		{"deepseek flash", "deepseek", "deepseek-flash", 1_000_000, 393_216},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok := LookupModelSpec(tt.provider, tt.model)
			if !ok {
				t.Fatalf("LookupModelSpec(%q, %q): expected ok, got !ok", tt.provider, tt.model)
			}
			if spec.ContextWindow != tt.wantContext {
				t.Errorf("ContextWindow = %d, want %d", spec.ContextWindow, tt.wantContext)
			}
			if spec.MaxOutput != tt.wantMaxOutput {
				t.Errorf("MaxOutput = %d, want %d", spec.MaxOutput, tt.wantMaxOutput)
			}
		})
	}
}

func TestDeepSeekModelsDefaultToNonThinking(t *testing.T) {
	for _, modelName := range []string{"deepseek-flash"} {
		spec, ok := LookupModelSpec("deepseek", modelName)
		if !ok || spec.DefaultReasoningEffort == nil || *spec.DefaultReasoningEffort != "none" || spec.Reasoning == nil || !spec.Reasoning.Supported {
			t.Fatalf("%s spec = %+v, want default reasoning_effort none", modelName, spec)
		}
	}
}

func TestDeepSeekTextOnlyModelsAndExperimentalAliasesAreNotPredefined(t *testing.T) {
	for _, modelName := range []string{"deepseek-v4-pro", "deepseek-v4-flash", "deepseek-v4-flash-vision-exp"} {
		if _, ok := LookupModelSpec("deepseek", modelName); ok {
			t.Errorf("LookupModelSpec(%q) unexpectedly returned a predefined spec", modelName)
		}
	}
}

func TestLookupModelSpecScopesNativeBudgetToAnthropic(t *testing.T) {
	native, ok := LookupModelSpec("anthropic", "claude-sonnet-4-6")
	if !ok || native.Reasoning == nil || native.Reasoning.BudgetTokensMin != 1024 {
		t.Fatalf("native spec = %+v, want Anthropic budget capability", native)
	}

	proxied, ok := LookupModelSpec("openrouter", "claude-sonnet-4-6")
	if !ok || proxied.Reasoning == nil {
		t.Fatalf("proxied spec = %+v, want effort capability", proxied)
	}
	if proxied.Reasoning.BudgetTokensMin != 0 || proxied.Reasoning.BudgetTokensMax != 0 || proxied.Reasoning.Mode != "effort" {
		t.Fatalf("proxied reasoning = %+v, want effort without native Anthropic budget", proxied.Reasoning)
	}
}

func TestLookupModelSpecClaudeEffortAndBudgetControls(t *testing.T) {
	spec, ok := LookupModelSpec("anthropic", "claude-sonnet-4-6")
	if !ok || spec.Reasoning == nil {
		t.Fatalf("expected Sonnet 4.6 reasoning metadata, got %+v", spec)
	}
	if spec.Reasoning.Mode != "effort" || spec.Reasoning.BudgetTokensMin != 1024 || !spec.Reasoning.CanDisable {
		t.Fatalf("Sonnet 4.6 reasoning = %+v, want effort + budget override", spec.Reasoning)
	}
}

func TestLookupModelSpecMarksNonReasoningModelsExplicitly(t *testing.T) {
	for _, test := range []struct{ provider, model string }{
		{"openai", "openai/gpt-4o"},
		{"openrouter", "anthropic/claude-3.5-sonnet"},
	} {
		spec, ok := LookupModelSpec(test.provider, test.model)
		if !ok || spec.Reasoning == nil || spec.Reasoning.Supported {
			t.Fatalf("%s reasoning = %+v, want explicit unsupported capability", test.model, spec.Reasoning)
		}
	}
}

func TestLookupModelSpecClaude4VersionSeparators(t *testing.T) {
	tests := []struct {
		name        string
		provider    string
		model       string
		wantContext int
		wantOutput  int
	}{
		{"opus dotted bare", "anthropic", "claude-opus-4.8", 1_000_000, 128_000},
		{"opus dotted provider-prefixed", "openrouter", "anthropic/claude-opus-4.8", 1_000_000, 128_000},
		{"sonnet dotted bare", "anthropic", "claude-sonnet-4.6", 1_000_000, 128_000},
		{"sonnet dotted provider-prefixed", "openrouter", "anthropic/claude-sonnet-4.6", 1_000_000, 128_000},
		{"haiku dotted bare", "anthropic", "claude-haiku-4.5", 200_000, 64_000},
		{"haiku dotted provider-prefixed", "openrouter", "anthropic/claude-haiku-4.5", 200_000, 64_000},
		{"opus hyphenated bare", "anthropic", "claude-opus-4-8", 1_000_000, 128_000},
		{"sonnet hyphenated provider-prefixed", "openrouter", "anthropic/claude-sonnet-4-6", 1_000_000, 128_000},
		{"haiku hyphenated bare", "anthropic", "claude-haiku-4-5", 200_000, 64_000},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spec, ok := LookupModelSpec(tt.provider, tt.model)
			if !ok {
				t.Fatalf("LookupModelSpec(%q, %q): expected ok, got !ok", tt.provider, tt.model)
			}
			if spec.ContextWindow != tt.wantContext {
				t.Errorf("ContextWindow = %d, want %d", spec.ContextWindow, tt.wantContext)
			}
			if spec.MaxOutput != tt.wantOutput {
				t.Errorf("MaxOutput = %d, want %d", spec.MaxOutput, tt.wantOutput)
			}
		})
	}
}

func TestLookupModelSpecDoubaoSeedDefaults(t *testing.T) {
	spec, ok := LookupModelSpec("volcengine", "doubao-seed-2-1-pro-260628")
	if !ok {
		t.Fatalf("LookupModelSpec: expected ok for doubao-seed-2-1-pro-260628")
	}
	// Ark defaults to "high" effort when reasoning_effort is omitted, which
	// stalls streaming before the first content token. Pin a lighter default so
	// voice stays responsive; an explicit model.reasoning_effort still wins.
	if spec.DefaultReasoningEffort == nil {
		t.Fatal("DefaultReasoningEffort = nil, want \"low\"")
	}
	if *spec.DefaultReasoningEffort != "low" {
		t.Errorf("DefaultReasoningEffort = %q, want \"low\"", *spec.DefaultReasoningEffort)
	}
	// Unlike Kimi K3, Ark accepts any temperature, so the global default applies.
	if spec.DefaultTemperature != nil {
		t.Errorf("DefaultTemperature = %v, want nil (no model-imposed requirement)", *spec.DefaultTemperature)
	}
}

// TestLookupModelSpecGeminiTemperatureDefaults guards the Gemini 3 temperature
// pin. Google documents 1.0 as the default for these models and warns that a
// lower value may cause looping or degraded reasoning, so the spec must not fall
// through to the global 0.2 fallback. Gemini 2.5 has no documented constant and
// stays unpinned.
func TestLookupModelSpecGeminiTemperatureDefaults(t *testing.T) {
	tests := []struct {
		provider string
		model    string
		want     *float64
	}{
		{provider: "gemini", model: "gemini-3.8-flash", want: floatPtr(1)},
		{provider: "gemini", model: "gemini-3.7-flash", want: floatPtr(1)},
		{provider: "gemini", model: "gemini-3.6-flash", want: floatPtr(1)},
		{provider: "gemini", model: "gemini-3.5-flash", want: floatPtr(1)},
		{provider: "gemini", model: "gemini-3.5-pro", want: floatPtr(1)},
		// Routed through OpenRouter the pin must survive the provider-prefixed id.
		{provider: "openrouter", model: "google/gemini-3.8-flash", want: floatPtr(1)},
		{provider: "gemini", model: "gemini-2.5-flash", want: nil},
		{provider: "gemini", model: "gemini-2.5-pro", want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.provider+"/"+tt.model, func(t *testing.T) {
			spec, ok := LookupModelSpec(tt.provider, tt.model)
			if !ok {
				t.Fatalf("LookupModelSpec(%q, %q): expected ok", tt.provider, tt.model)
			}
			if !floatPtrEqual(spec.DefaultTemperature, tt.want) {
				t.Errorf("DefaultTemperature = %v, want %v", formatFloatPtr(spec.DefaultTemperature), formatFloatPtr(tt.want))
			}
		})
	}
}

func TestLookupModelSpecCaseInsensitive(t *testing.T) {
	spec, ok := LookupModelSpec("OpenRouter", "GOOGLE/Gemini-3.5-Flash")
	if !ok {
		t.Fatalf("expected case-insensitive lookup to succeed")
	}
	if spec.ContextWindow != 1_048_576 {
		t.Errorf("ContextWindow = %d, want 1_048_576", spec.ContextWindow)
	}
}

func TestLookupModelSpecUnknownModelReturnsNotOK(t *testing.T) {
	if spec, ok := LookupModelSpec("openrouter", "vendor/unknown-model-9001"); ok {
		t.Fatalf("expected !ok for unknown model, got spec=%+v", spec)
	}
	if spec, ok := LookupModelSpec("openai", ""); ok {
		t.Fatalf("expected !ok for empty model, got spec=%+v", spec)
	}
}

func TestModelManagerSpecFetchesModelsDevReasoningMetadata(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"anthropic":{"api":null,"models":{"claude-custom":{"id":"claude-custom","reasoning":true,"reasoning_options":[{"type":"effort","values":["low","medium","high","max"]},{"type":"budget_tokens","min":1024}],"limit":{"context":200000,"output":64000}}}}}`)
	}))
	defer server.Close()

	mgr := NewModelManager(ModelConfig{Provider: "anthropic", Model: "claude-custom"}, ProxyConfig{},
		WithModelsDevURL(server.URL), WithProviderMetadataHTTPClient(server.Client()))
	deadline := time.Now().Add(2 * time.Second)
	for {
		spec := mgr.Spec()
		if spec.ContextWindow == 200_000 && spec.MaxOutput == 64_000 && spec.Reasoning != nil {
			if !spec.Reasoning.Supported || spec.Reasoning.Mode != "effort" || !spec.Reasoning.CanDisable || len(spec.Reasoning.Efforts) != 4 || spec.Reasoning.BudgetTokensMin != 1024 {
				t.Fatalf("reasoning spec = %+v", spec.Reasoning)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Spec() = %+v, want models.dev metadata", spec)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("models.dev requests = %d, want 1", got)
	}
}

func TestDeepSeekSpecFetchesModelsDevMetadata(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"deepseek":{"api":"https://api.deepseek.com","models":{"deepseek-custom":{"reasoning":true,"reasoning_options":[{"type":"effort","values":["none","low","high","max"]}],"limit":{"context":1000000,"output":393216}}}}}`)
	}))
	defer server.Close()

	// Use an unregistered model so the normal Spec path must pass the provider
	// discovery gate rather than returning complete built-in Flash metadata.
	mgr := NewModelManager(ModelConfig{Provider: "deepseek", Model: "deepseek-custom"}, ProxyConfig{},
		WithModelsDevURL(server.URL), WithProviderMetadataHTTPClient(server.Client()))
	spec := waitForModelSpec(t, mgr, model.ModelSpec{ContextWindow: 1_000_000, MaxOutput: 393_216})
	if spec.Reasoning == nil || !spec.Reasoning.Supported || spec.Reasoning.Mode != "effort" || !spec.Reasoning.CanDisable ||
		strings.Join(spec.Reasoning.Efforts, ",") != "none,low,high,max" {
		t.Fatalf("reasoning = %+v, want models.dev effort metadata", spec.Reasoning)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("models.dev requests = %d, want 1", got)
	}
}

func TestModelManagerSpecPreservesModelsDevReasoningFalse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"openai":{"models":{"custom-chat":{"reasoning":false,"limit":{"context":128000,"output":4096}}}}}`)
	}))
	defer server.Close()

	mgr := NewModelManager(ModelConfig{Provider: "openai", Model: "custom-chat"}, ProxyConfig{},
		WithModelsDevURL(server.URL), WithProviderMetadataHTTPClient(server.Client()))
	deadline := time.Now().Add(2 * time.Second)
	for {
		spec := mgr.Spec()
		if spec.Reasoning != nil {
			if spec.Reasoning.Supported {
				t.Fatalf("reasoning = %+v, want explicit unsupported", spec.Reasoning)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("models.dev reasoning=false metadata did not load")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestModelsDevCatalogCachedAcrossManagers(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_, _ = io.WriteString(w, `{"anthropic":{"models":{"claude-a":{"limit":{"context":1000,"output":100}},"claude-b":{"limit":{"context":2000,"output":200}}}}}`)
	}))
	defer server.Close()

	for _, modelName := range []string{"claude-a", "claude-b"} {
		mgr := NewModelManager(ModelConfig{Provider: "anthropic", Model: modelName}, ProxyConfig{},
			WithModelsDevURL(server.URL), WithProviderMetadataHTTPClient(server.Client()))
		if _, err := mgr.fetchModelsDevModelSpec(context.Background()); err != nil {
			t.Fatalf("fetchModelsDevModelSpec(%s): %v", modelName, err)
		}
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("models.dev requests = %d, want one catalog fetch shared across managers", got)
	}
}

func TestModelsDevCatalogReportsOversizedResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, strings.Repeat("x", maxModelsDevCatalogBytes+1))
	}))
	defer server.Close()

	mgr := NewModelManager(ModelConfig{Provider: "anthropic", Model: "claude-test"}, ProxyConfig{},
		WithModelsDevURL(server.URL), WithProviderMetadataHTTPClient(server.Client()))
	_, err := mgr.fetchModelsDevModelSpec(context.Background())
	if err == nil || !strings.Contains(err.Error(), "catalog exceeds") {
		t.Fatalf("fetchModelsDevModelSpec() error = %v, want explicit oversized catalog error", err)
	}
}

func TestModelManagerSpecUsesConfig(t *testing.T) {
	mgr := NewModelManager(ModelConfig{Provider: "openrouter", Model: "google/gemini-3.5-flash"}, ProxyConfig{})
	if got := mgr.Spec().ContextWindow; got != 1_048_576 {
		t.Errorf("ModelManager.Spec().ContextWindow = %d, want 1_048_576", got)
	}

	unknown := NewModelManager(ModelConfig{Provider: "openrouter", Model: "vendor/no-such-model"}, ProxyConfig{})
	if got := unknown.Spec().ContextWindow; got != 0 {
		t.Errorf("unknown model: ContextWindow = %d, want 0 (caller falls back to yaml default)", got)
	}
}

func TestGeminiReasoningCapabilitiesMatchInteractionsThinkingLevels(t *testing.T) {
	tests := []struct {
		model   string
		efforts string
	}{
		{model: "gemini-3.8-flash", efforts: "low,medium,high"},
		{model: "gemini-3.6-flash", efforts: "minimal,low,medium,high"},
		{model: "gemini-2.5-pro", efforts: "low,medium,high"},
	}
	for _, tt := range tests {
		spec, ok := LookupModelSpec("gemini", tt.model)
		if !ok || spec.Reasoning == nil {
			t.Fatalf("LookupModelSpec(%q) = %#v, %v", tt.model, spec, ok)
		}
		if got := strings.Join(spec.Reasoning.Efforts, ","); got != tt.efforts {
			t.Errorf("%s efforts = %q, want %q", tt.model, got, tt.efforts)
		}
		if spec.Reasoning.CanDisable {
			t.Errorf("%s exposes disabled thinking, which native Interactions does not support", tt.model)
		}
	}
}

func TestModelManagerSpecUsesExplicitConfigOverrides(t *testing.T) {
	mgr := NewModelManager(ModelConfig{
		Provider:             "openrouter",
		Model:                "google/gemini-3.5-flash",
		ContextWindow:        64_000,
		ModelMaxOutputTokens: 4_096,
	}, ProxyConfig{})

	spec := mgr.Spec()
	if spec.ContextWindow != 64_000 {
		t.Errorf("ContextWindow = %d, want 64_000", spec.ContextWindow)
	}
	if spec.MaxOutput != 4_096 {
		t.Errorf("MaxOutput = %d, want 4_096", spec.MaxOutput)
	}
}

func TestModelManagerSpecAllowsPartialExplicitConfigOverrides(t *testing.T) {
	mgr := NewModelManager(ModelConfig{
		Provider:      "openrouter",
		Model:         "google/gemini-3.5-flash",
		ContextWindow: 64_000,
	}, ProxyConfig{})

	spec := mgr.Spec()
	if spec.ContextWindow != 64_000 {
		t.Errorf("ContextWindow = %d, want 64_000", spec.ContextWindow)
	}
	if spec.MaxOutput != 65_536 {
		t.Errorf("MaxOutput = %d, want registry value 65_536", spec.MaxOutput)
	}
}

func TestModelManagerSpecUsesRegistryBeforeProviderMetadata(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "provider_model_metadata.json")
	cfg := ModelConfig{
		Provider: "openrouter",
		Model:    "google/gemini-3.5-flash",
		BaseURL:  server.URL + "/api/v1",
		APIKey:   "test-token",
	}
	cacheWriter := NewModelManager(cfg, ProxyConfig{}, WithProviderModelMetadataCachePath(cachePath))
	if err := cacheWriter.writeProviderModelSpecCache(model.ModelSpec{ContextWindow: 524_288, MaxOutput: 16_384}); err != nil {
		t.Fatalf("writeProviderModelSpecCache: %v", err)
	}

	mgr := NewModelManager(cfg, ProxyConfig{}, WithProviderModelMetadataCachePath(cachePath))

	spec := mgr.Spec()
	if spec.ContextWindow != 1_048_576 {
		t.Fatalf("Spec().ContextWindow = %d, want registry value 1_048_576", spec.ContextWindow)
	}
	if spec.MaxOutput != 65_536 {
		t.Fatalf("Spec().MaxOutput = %d, want registry value 65_536", spec.MaxOutput)
	}
	if got := requests.Load(); got != 0 {
		t.Fatalf("requests = %d, want 0", got)
	}
}

func TestModelManagerSpecFetchesOpenRouterContextWindowWhenRegistryUnknown(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/v1/models" {
			t.Errorf("path = %s, want /api/v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer test-token" {
			t.Errorf("Authorization = %q, want Bearer test-token", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"data": [
				{"id": "other/model", "context_length": 1024},
				{
					"id": "vendor/new-model",
					"context_length": 1000000,
					"top_provider": {
						"context_length": 524288,
						"max_completion_tokens": 16384
					}
				}
			]
		}`)
	}))
	defer server.Close()

	mgr := NewModelManager(ModelConfig{
		Provider: "openrouter",
		Model:    "vendor/new-model",
		BaseURL:  server.URL + "/api/v1",
		APIKey:   "test-token",
	}, ProxyConfig{})

	for i := 0; i < 2; i++ {
		waitForModelSpec(t, mgr, model.ModelSpec{ContextWindow: 524_288, MaxOutput: 16_384})
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 cached request", got)
	}
}

func TestModelManagerPrefetchesProviderMetadataWithoutBlockingSpec(t *testing.T) {
	var requests atomic.Int64
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	var closeStarted atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if closeStarted.CompareAndSwap(false, true) {
			close(requestStarted)
		}
		<-releaseResponse
		http.Error(w, "provider unavailable", http.StatusInternalServerError)
	}))
	defer server.Close()

	mgr := NewModelManager(ModelConfig{
		Provider: "openrouter",
		Model:    "vendor/slow-model",
		BaseURL:  server.URL + "/api/v1",
		APIKey:   "test-token",
	}, ProxyConfig{})
	mgr.prefetchProviderModelSpecIfNeeded()

	select {
	case <-requestStarted:
	case <-time.After(500 * time.Millisecond):
		close(releaseResponse)
		t.Fatal("provider metadata request was not prefetched")
	}

	startedAt := time.Now()
	spec := mgr.Spec()
	if elapsed := time.Since(startedAt); elapsed > 100*time.Millisecond {
		close(releaseResponse)
		t.Fatalf("Spec() blocked for %v waiting on provider metadata", elapsed)
	}
	if spec.ContextWindow != 0 || spec.MaxOutput != 0 {
		close(releaseResponse)
		t.Fatalf("Spec() = %+v, want zero while provider metadata is pending", spec)
	}

	close(releaseResponse)
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestNewRuntimePrefetchesProviderMetadataBeforeFirstRun(t *testing.T) {
	var requests atomic.Int64
	requestStarted := make(chan struct{})
	releaseResponse := make(chan struct{})
	var closeStarted atomic.Bool
	var released atomic.Bool
	release := func() {
		if released.CompareAndSwap(false, true) {
			close(releaseResponse)
		}
	}
	defer release()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if r.URL.Path != "/api/v1/models" {
			t.Errorf("path = %s, want /api/v1/models", r.URL.Path)
		}
		if closeStarted.CompareAndSwap(false, true) {
			close(requestStarted)
		}
		<-releaseResponse
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()

	rt, err := NewRuntime(Config{
		Model: ModelConfig{
			Provider: "openrouter",
			Model:    "vendor/runtime-prefetch-model",
			BaseURL:  server.URL + "/api/v1",
			APIKey:   "test-token",
		},
	})
	if err != nil {
		t.Fatalf("NewRuntime() error = %v", err)
	}
	defer rt.Close()

	select {
	case <-requestStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("provider metadata request was not prefetched during runtime initialization")
	}

	release()
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestModelManagerSpecReadsProviderMetadataFromFileCache(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"data": [
				{
					"id": "vendor/cached-model",
					"context_length": 262144,
					"top_provider": {
						"context_length": 131072,
						"max_completion_tokens": 4096
					}
				}
			]
		}`)
	}))
	defer server.Close()

	cachePath := filepath.Join(t.TempDir(), "provider_model_metadata.json")
	cfg := ModelConfig{
		Provider: "openrouter",
		Model:    "vendor/cached-model",
		BaseURL:  server.URL + "/api/v1",
		APIKey:   "test-token",
	}

	first := NewModelManager(cfg, ProxyConfig{}, WithProviderModelMetadataCachePath(cachePath))
	waitForModelSpec(t, first, model.ModelSpec{ContextWindow: 131_072, MaxOutput: 4_096})

	second := NewModelManager(cfg, ProxyConfig{}, WithProviderModelMetadataCachePath(cachePath))
	if spec := second.Spec(); spec.ContextWindow != 131_072 || spec.MaxOutput != 4_096 {
		t.Fatalf("second Spec() = %+v, want cached provider metadata", spec)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("ReadFile(cache): %v", err)
	}
	if strings.Contains(string(data), "test-token") {
		t.Fatalf("cache file leaked API key: %s", string(data))
	}
}

func TestModelManagerSpecReadsProviderMaxOutputOnlyFromFileCache(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "provider_model_metadata.json")
	cfg := ModelConfig{
		Provider: "openrouter",
		Model:    "vendor/max-output-only-cached-model",
		BaseURL:  "http://provider.test/api/v1",
		APIKey:   "test-token",
	}

	cacheWriter := NewModelManager(cfg, ProxyConfig{}, WithProviderModelMetadataCachePath(cachePath))
	if err := cacheWriter.writeProviderModelSpecCache(model.ModelSpec{MaxOutput: 16_384}); err != nil {
		t.Fatalf("writeProviderModelSpecCache: %v", err)
	}

	mgr := NewModelManager(cfg, ProxyConfig{}, WithProviderModelMetadataCachePath(cachePath))
	if spec := mgr.Spec(); spec.ContextWindow != 0 || spec.MaxOutput != 16_384 {
		t.Fatalf("Spec() = %+v, want cached max-output-only provider metadata", spec)
	}
}

func TestModelManagerSpecFetchesProviderMaxOutputWhenContextWindowOverrideSet(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"data": [
				{
					"id": "vendor/context-override-model",
					"context_length": 262144,
					"top_provider": {
						"context_length": 131072,
						"max_completion_tokens": 16384
					}
				}
			]
		}`)
	}))
	defer server.Close()

	mgr := NewModelManager(ModelConfig{
		Provider:      "openrouter",
		Model:         "vendor/context-override-model",
		BaseURL:       server.URL + "/api/v1",
		APIKey:        "test-token",
		ContextWindow: 64_000,
	}, ProxyConfig{})

	spec := waitForModelSpec(t, mgr, model.ModelSpec{ContextWindow: 64_000, MaxOutput: 16_384})
	if spec.ContextWindow != 64_000 {
		t.Fatalf("Spec().ContextWindow = %d, want explicit override 64_000", spec.ContextWindow)
	}
	if spec.MaxOutput != 16_384 {
		t.Fatalf("Spec().MaxOutput = %d, want provider metadata 16_384", spec.MaxOutput)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1", got)
	}
}

func TestModelManagerNeedsProviderMetadataWhenOnlyMaxOutputMissing(t *testing.T) {
	mgr := NewModelManager(ModelConfig{
		Provider:      "openrouter",
		Model:         "vendor/context-override-model",
		ContextWindow: 64_000,
	}, ProxyConfig{})

	if !mgr.needsProviderModelMetadata() {
		t.Fatal("needsProviderModelMetadata() = false, want true when max output is still auto")
	}
}

func TestModelManagerRetriesProviderMetadataAfterFailure(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if requests.Add(1) == 1 {
			http.Error(w, "temporary provider failure", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"data": [
				{
					"id": "vendor/retry-model",
					"context_length": 262144,
					"top_provider": {
						"context_length": 131072,
						"max_completion_tokens": 4096
					}
				}
			]
		}`)
	}))
	defer server.Close()

	mgr := NewModelManager(ModelConfig{
		Provider: "openrouter",
		Model:    "vendor/retry-model",
		BaseURL:  server.URL + "/api/v1",
		APIKey:   "test-token",
	}, ProxyConfig{})

	waitForModelSpec(t, mgr, model.ModelSpec{ContextWindow: 131_072, MaxOutput: 4_096})
	if got := requests.Load(); got != 2 {
		t.Fatalf("requests = %d, want 2 with retry after failure", got)
	}
}

func TestModelManagerSpecFetchesOllamaContextWindowWhenAuto(t *testing.T) {
	var requests atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Method != http.MethodPost {
			t.Errorf("method = %s, want POST", r.Method)
		}
		if r.URL.Path != "/api/show" {
			t.Errorf("path = %s, want /api/show", r.URL.Path)
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatalf("ReadAll request body: %v", err)
		}
		if !strings.Contains(string(body), `"model":"gemma4:latest"`) {
			t.Errorf("body = %s, want model name", string(body))
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{
			"model_info": {
				"gemma4.context_length": 131072
			},
			"parameters": "num_ctx 8192\nnum_predict -1"
		}`)
	}))
	defer server.Close()

	mgr := NewModelManager(ModelConfig{
		Provider: "ollama",
		Model:    "gemma4:latest",
		BaseURL:  server.URL,
	}, ProxyConfig{})

	for i := 0; i < 2; i++ {
		waitForModelSpec(t, mgr, model.ModelSpec{ContextWindow: 8_192})
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("requests = %d, want 1 cached request", got)
	}
}
