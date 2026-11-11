package realtimevoice

import (
	"fmt"
	"strings"
	"sync"
)

// ProviderConfig contains provider construction settings that are not part of
// a realtime session's semantic request. Keeping these values on the factory
// side prevents the session interface from growing whenever a provider adds a
// different endpoint or routing hint.
type ProviderConfig struct {
	Endpoint         string
	BaseURL          string
	AgentID          string
	UpstreamProvider string
	WorkspaceID      string
	Region           string
	AuthMode         string
	ProjectID        string
	Location         string
	RealtimeProtocol string
}

// ProviderFactory constructs one provider implementation from its static
// configuration. Session credentials and model options are supplied to Open.
type ProviderFactory func(ProviderConfig) Provider

// ProviderRegistry maps stable provider names to factories. It is deliberately
// small so tests and downstream builds can register an experimental provider
// without changing daemon dispatch code.
type ProviderRegistry struct {
	mu        sync.RWMutex
	factories map[string]ProviderFactory
}

func NewProviderRegistry() *ProviderRegistry {
	return &ProviderRegistry{factories: make(map[string]ProviderFactory)}
}

func (r *ProviderRegistry) Register(name string, factory ProviderFactory) {
	if r == nil || factory == nil {
		return
	}
	name = normalizeProviderName(name)
	if name == "" {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.factories == nil {
		r.factories = make(map[string]ProviderFactory)
	}
	r.factories[name] = factory
}

func (r *ProviderRegistry) New(name string, config ProviderConfig) (Provider, error) {
	if r == nil {
		return nil, fmt.Errorf("realtime provider registry is nil")
	}
	name = normalizeProviderName(name)
	r.mu.RLock()
	factory, ok := r.factories[name]
	r.mu.RUnlock()
	if !ok {
		return nil, fmt.Errorf("unsupported realtime provider %q", name)
	}
	provider := factory(config)
	if provider == nil {
		return nil, fmt.Errorf("realtime provider factory %q returned nil", name)
	}
	return provider, nil
}

// DefaultProviderRegistry returns the adapters shipped with the daemon. The
// factories only capture provider-specific construction settings; API keys and
// per-session options remain in SessionConfig.
func DefaultProviderRegistry() *ProviderRegistry {
	r := NewProviderRegistry()
	r.Register(ProviderQwen, func(c ProviderConfig) Provider {
		return QwenProvider{WorkspaceID: c.WorkspaceID, Region: c.Region, Endpoint: c.Endpoint}
	})
	r.Register(ProviderSpeko, func(c ProviderConfig) Provider {
		return SpekoProvider{BaseURL: c.BaseURL, AgentID: c.AgentID, UpstreamProvider: c.UpstreamProvider}
	})
	r.Register(ProviderOpenAI, func(c ProviderConfig) Provider {
		return OpenAIProvider{Endpoint: c.Endpoint, RealtimeProtocol: c.RealtimeProtocol}
	})
	r.Register(ProviderGemini, func(c ProviderConfig) Provider {
		return GeminiProvider{Endpoint: c.Endpoint, AuthMode: c.AuthMode, ProjectID: c.ProjectID, Location: c.Location}
	})
	r.Register(ProviderXAI, func(c ProviderConfig) Provider {
		return XAIProvider{Endpoint: c.Endpoint}
	})
	r.Register(ProviderMoss, func(c ProviderConfig) Provider {
		return MossProvider{Endpoint: c.Endpoint}
	})
	return r
}

func normalizeProviderName(name string) string {
	return strings.ToLower(strings.TrimSpace(name))
}
