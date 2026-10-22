package fishaudio

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/gorilla/websocket"

	"aiden-agent/internal/agent/tts"
)

func TestWebSocketDialerHonorsNoProxy(t *testing.T) {
	provider, err := New(tts.ProviderConfig{
		APIKey: "test-key",
		Proxy: tts.ProxyConfig{
			HTTPSProxy: "http://127.0.0.1:7890",
			AllProxy:   "http://127.0.0.1:7891",
			NoProxy:    "api.fish.audio",
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	dialer, err := provider.(*Adapter).websocketDialer()
	if err != nil {
		t.Fatalf("websocketDialer() error = %v", err)
	}

	assertWebSocketProxy(t, dialer, defaultEndpoint, "")
	assertWebSocketProxy(t, dialer, "wss://tts.example/v1/tts/live", "http://127.0.0.1:7890")
}

func assertWebSocketProxy(t *testing.T, dialer websocket.Dialer, endpoint, want string) {
	t.Helper()
	if dialer.Proxy == nil {
		t.Fatal("dialer.Proxy = nil, want configured HTTP proxy")
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	// gorilla/websocket passes the handshake URL with an http or https scheme.
	u.Scheme = strings.Replace(u.Scheme, "ws", "http", 1)
	got, err := dialer.Proxy(&http.Request{URL: u})
	if err != nil {
		t.Fatalf("Proxy(%s) error = %v", endpoint, err)
	}
	if want == "" && got != nil {
		t.Fatalf("Proxy(%s) = %v, want direct connection", endpoint, got)
	}
	if want != "" && (got == nil || got.String() != want) {
		t.Fatalf("Proxy(%s) = %v, want %s", endpoint, got, want)
	}
}
