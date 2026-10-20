package openrouter

import (
	"context"
	"net/http"
	"net/url"
	"testing"

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
