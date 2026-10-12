package langfuse

import (
	"net/http"
	"testing"
)

func TestDedicatedTelemetryProxyRespectsRoutingAndNoProxy(t *testing.T) {
	t.Setenv("AIDEN_LANGFUSE_HTTPS_PROXY", "http://telemetry-proxy.example:8080")
	t.Setenv("HTTP_PROXY", "")
	t.Setenv("http_proxy", "")
	t.Setenv("NO_PROXY", "internal.example")
	client := NewClient(Config{})
	transport, ok := client.httpClient.Transport.(*http.Transport)
	if !ok {
		t.Fatal("dedicated telemetry transport missing")
	}
	if transport == http.DefaultTransport {
		t.Fatal("telemetry must not mutate the shared model/device transport")
	}
	for _, tc := range []struct{ target, want string }{
		{"https://langfuse.example/api/public/otel/v1/traces", "http://telemetry-proxy.example:8080"},
		{"https://internal.example/api/public/scores", ""},
		{"http://langfuse.example/api/public/scores", ""},
	} {
		req, _ := http.NewRequest(http.MethodPost, tc.target, nil)
		proxy, err := transport.Proxy(req)
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		if proxy != nil {
			got = proxy.String()
		}
		if got != tc.want {
			t.Errorf("proxy for %s = %q, want %q", tc.target, got, tc.want)
		}
	}
}
