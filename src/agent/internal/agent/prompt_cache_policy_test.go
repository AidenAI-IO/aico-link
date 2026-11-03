package agent

import (
	"aico-agent/internal/agent/model"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveOpenRouterPromptCachePolicy(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		endpoints []openRouterEndpointMetadata
		want      PromptCachePolicy
	}{
		{
			name: "explicit cache_control parameter",
			endpoints: []openRouterEndpointMetadata{{
				SupportedParameters: []string{"temperature", "cache_control"},
			}},
			want: PromptCachePolicyExplicit,
		},
		{
			name: "implicit caching only",
			endpoints: []openRouterEndpointMetadata{{
				SupportedParameters:     []string{"temperature"},
				SupportsImplicitCaching: boolPtr(true),
			}},
			want: PromptCachePolicyImplicit,
		},
		{
			name: "cache read pricing only",
			endpoints: []openRouterEndpointMetadata{{
				Pricing: map[string]any{"input_cache_read": "0.0000003"},
			}},
			want: PromptCachePolicyImplicit,
		},
		{
			name:      "none",
			endpoints: []openRouterEndpointMetadata{{SupportedParameters: []string{"temperature"}}},
			want:      PromptCachePolicyNone,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := resolveOpenRouterPromptCachePolicy(tc.endpoints); got != tc.want {
				t.Fatalf("resolveOpenRouterPromptCachePolicy() = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestOpenRouterModelEndpointsURL(t *testing.T) {
	t.Parallel()

	got, ok := openRouterModelEndpointsURL("https://openrouter.ai/api/v1", "anthropic/claude-sonnet-4")
	if !ok {
		t.Fatal("expected valid endpoints URL")
	}
	want := "https://openrouter.ai/api/v1/models/anthropic/claude-sonnet-4/endpoints"
	if got != want {
		t.Fatalf("endpoints URL = %q, want %q", got, want)
	}
}

func TestCachedOpenRouterPromptCachePolicyReadsDiskCache(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "provider-model-metadata.json")
	manager := NewModelManager(
		ModelConfig{Provider: "openrouter", Model: "vendor/model-x", APIKey: "k", BaseURL: "http://127.0.0.1:1"},
		ProxyConfig{},
		WithProviderModelMetadataCachePath(cachePath),
	)

	key := manager.providerModelSpecCacheKey()
	cache := providerModelMetadataCacheFile{
		Version: providerModelMetadataCacheVersion,
		Entries: map[string]providerModelMetadataCacheEntry{
			key: {
				Provider:          "openrouter",
				Model:             "vendor/model-x",
				Endpoint:          openRouterModelsURL("http://127.0.0.1:1"),
				PromptCachePolicy: string(PromptCachePolicyExplicit),
			},
		},
	}
	encoded, err := json.MarshalIndent(cache, "", "  ")
	if err != nil {
		t.Fatalf("marshal cache: %v", err)
	}
	if err := writeFileAtomic(cachePath, append(encoded, '\n'), 0o644); err != nil {
		t.Fatalf("write cache: %v", err)
	}

	if got := manager.cachedOpenRouterPromptCachePolicy(); got != PromptCachePolicyExplicit {
		t.Fatalf("cached policy = %q, want explicit", got)
	}
}

func TestProviderMetadataCacheWritesPreserveSpecAndPromptCachePolicy(t *testing.T) {
	cachePath := filepath.Join(t.TempDir(), "provider-model-metadata.json")
	manager := NewModelManager(
		ModelConfig{Provider: "openrouter", Model: "vendor/model-x", APIKey: "k", BaseURL: "http://127.0.0.1:1"},
		ProxyConfig{},
		WithProviderModelMetadataCachePath(cachePath),
	)

	if err := manager.writeProviderModelSpecCache(model.ModelSpec{ContextWindow: 1024}); err != nil {
		t.Fatalf("writeProviderModelSpecCache: %v", err)
	}
	if err := manager.writeProviderPromptCachePolicyCache(PromptCachePolicyExplicit); err != nil {
		t.Fatalf("writeProviderPromptCachePolicyCache: %v", err)
	}
	if err := manager.writeProviderModelSpecCache(model.ModelSpec{ContextWindow: 2048}); err != nil {
		t.Fatalf("writeProviderModelSpecCache second write: %v", err)
	}

	data, err := os.ReadFile(cachePath)
	if err != nil {
		t.Fatalf("ReadFile(cache): %v", err)
	}
	var cache providerModelMetadataCacheFile
	if err := json.Unmarshal(data, &cache); err != nil {
		t.Fatalf("unmarshal cache: %v", err)
	}
	entry := cache.Entries[manager.providerModelSpecCacheKey()]
	if entry.Spec.ContextWindow != 2048 {
		t.Fatalf("cached spec = %+v, want context window 2048", entry.Spec)
	}
	if entry.PromptCachePolicy != string(PromptCachePolicyExplicit) {
		t.Fatalf("cached prompt cache policy = %q, want explicit", entry.PromptCachePolicy)
	}
}
