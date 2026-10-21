package openrouter

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"aiden-agent/internal/agent/tts"
)

func TestSessionHTTPClientHonorsNoProxy(t *testing.T) {
	provider, err := New(tts.ProviderConfig{
		APIKey: "test-key",
		Proxy: tts.ProxyConfig{
			HTTPSProxy: "http://127.0.0.1:7890",
			AllProxy:   "http://127.0.0.1:7891",
			NoProxy:    "openrouter.ai",
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	stream, err := provider.BeginStream(context.Background(), &openRouterTestSink{})
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	httpClient := stream.(*session).httpClient
	transport, ok := httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httpClient.Transport = %T, want *http.Transport", httpClient.Transport)
	}

	assertProxy(t, transport.Proxy, defaultEndpoint+"/audio/speech", "")
	assertProxy(t, transport.Proxy, "https://tts.example/api/v1/audio/speech", "http://127.0.0.1:7890")
}

func TestHTTPClientTimeoutAppliesOnlyWithProxyURL(t *testing.T) {
	for _, tc := range []struct {
		name  string
		proxy tts.ProxyConfig
		want  time.Duration
	}{
		{name: "direct", proxy: tts.ProxyConfig{NoProxy: "openrouter.ai"}, want: 0},
		{name: "proxied", proxy: tts.ProxyConfig{HTTPSProxy: "http://127.0.0.1:7890"}, want: requestTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			provider, err := New(tts.ProviderConfig{APIKey: "test-key", Proxy: tc.proxy})
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}
			if got := provider.(*Adapter).httpClient.Timeout; got != tc.want {
				t.Fatalf("httpClient.Timeout = %v, want %v", got, tc.want)
			}
		})
	}
}

func assertProxy(t *testing.T, proxy func(*http.Request) (*url.URL, error), target, want string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := proxy(req)
	if err != nil {
		t.Fatalf("Proxy(%s) error = %v", target, err)
	}
	if want == "" && got != nil {
		t.Fatalf("Proxy(%s) = %v, want direct connection", target, got)
	}
	if want != "" && (got == nil || got.String() != want) {
		t.Fatalf("Proxy(%s) = %v, want %s", target, got, want)
	}
}
