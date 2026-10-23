package minimax

import (
	"testing"

	"aico-agent/internal/agent/tts"
)

func TestWebSocketUsesProviderDefaultEndpoint(t *testing.T) {
	tests := []struct {
		provider string
		endpoint string
	}{
		{provider: "minimax", endpoint: "wss://api.minimax.io/ws/v1/t2a_v2"},
		{provider: "minimax-cn", endpoint: "wss://api.minimaxi.com/ws/v1/t2a_v2"},
	}

	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			provider, err := NewWebSocket(tts.ProviderConfig{Provider: tt.provider, APIKey: "test-key"})
			if err != nil {
				t.Fatalf("NewWebSocket() error = %v", err)
			}
			ws, ok := provider.(*WebSocketAdapter)
			if !ok {
				t.Fatalf("provider type = %T, want *WebSocketAdapter", provider)
			}
			if ws.Name() != tt.provider {
				t.Fatalf("Name() = %q, want %q", ws.Name(), tt.provider)
			}
			if ws.cfg.endpoint != tt.endpoint {
				t.Fatalf("endpoint = %q, want %q", ws.cfg.endpoint, tt.endpoint)
			}
		})
	}
}
