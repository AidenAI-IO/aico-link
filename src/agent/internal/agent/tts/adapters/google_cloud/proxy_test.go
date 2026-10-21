package google_cloud

import (
	"net/http"
	"net/url"
	"testing"

	"aiden-agent/internal/agent/tts"
)

func TestHTTPClientHonorsNoProxy(t *testing.T) {
	provider, err := New(tts.ProviderConfig{
		APIKey: "test-key",
		Proxy: tts.ProxyConfig{
			HTTPSProxy: "http://127.0.0.1:7890",
			AllProxy:   "http://127.0.0.1:7891",
			NoProxy:    "texttospeech.googleapis.com",
		},
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	transport, ok := provider.(*Adapter).httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("httpClient.Transport = %T, want *http.Transport", provider.(*Adapter).httpClient.Transport)
	}

	assertProxy(t, transport.Proxy, defaultEndpoint, "")
	assertProxy(t, transport.Proxy, "https://tts.example/v1/text:synthesize", "http://127.0.0.1:7890")
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
