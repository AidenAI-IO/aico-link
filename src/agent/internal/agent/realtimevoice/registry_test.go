package realtimevoice

import (
	"context"
	"testing"
)

func TestProviderRegistrySupportsNamedFactories(t *testing.T) {
	registry := NewProviderRegistry()
	registry.Register("test", func(ProviderConfig) Provider { return testProvider{} })
	registry.Register("nil", func(ProviderConfig) Provider { return nil })

	provider, err := registry.New(" TEST ", ProviderConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := provider.(testProvider); !ok {
		t.Fatalf("provider = %T, want testProvider", provider)
	}
	if _, err := registry.New("missing", ProviderConfig{}); err == nil {
		t.Fatal("missing provider unexpectedly resolved")
	}
	if _, err := registry.New("nil", ProviderConfig{}); err == nil {
		t.Fatal("nil provider unexpectedly resolved")
	}
}

func TestDefaultProviderRegistryContainsRealtimeProviders(t *testing.T) {
	registry := DefaultProviderRegistry()
	for _, name := range []string{"qwen", "speko", "openai", "gemini", "xai", "moss"} {
		if _, err := registry.New(name, ProviderConfig{}); err != nil {
			t.Fatalf("default registry missing %q: %v", name, err)
		}
	}
}

func TestDefaultProviderRegistryPropagatesOpenAIRealtimeProtocol(t *testing.T) {
	provider, err := DefaultProviderRegistry().New("openai", ProviderConfig{RealtimeProtocol: "legacy"})
	if err != nil {
		t.Fatal(err)
	}
	openAI, ok := provider.(OpenAIProvider)
	if !ok {
		t.Fatalf("provider = %T, want OpenAIProvider", provider)
	}
	if openAI.RealtimeProtocol != "legacy" {
		t.Fatalf("RealtimeProtocol = %q, want legacy", openAI.RealtimeProtocol)
	}
}

type testProvider struct{}

func (testProvider) Open(context.Context, SessionConfig) (Session, error) { return nil, nil }
