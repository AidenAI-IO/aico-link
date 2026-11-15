package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type fakeSearchBackend struct {
	query string
	out   string
	err   error
}

func TestWebSearchPropagatesContextErrors(t *testing.T) {
	for _, wantErr := range []error{context.Canceled, context.DeadlineExceeded} {
		t.Run(wantErr.Error(), func(t *testing.T) {
			tool := &WebSearchTool{backend: &fakeSearchBackend{err: wantErr}, provider: "fake"}
			_, err := tool.Call(context.Background(), `{"query":"golang"}`)
			if !errors.Is(err, wantErr) {
				t.Fatalf("Call error = %v, want %v", err, wantErr)
			}
		})
	}
}

func (b *fakeSearchBackend) Search(ctx context.Context, query string) (string, error) {
	if _, ok := ctx.Deadline(); !ok {
		return "", errors.New("missing deadline")
	}
	b.query = query
	return b.out, b.err
}

func TestWebSearchAcceptsJSONAndTruncatesOutput(t *testing.T) {
	backend := &fakeSearchBackend{out: strings.Repeat("x", maxWebToolOutputBytes+100)}
	tool := &WebSearchTool{backend: backend, provider: "duckduckgo"}

	out, err := tool.Call(context.Background(), `{"query":"golang testing"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if backend.query != "golang testing" {
		t.Fatalf("query = %q, want golang testing", backend.query)
	}
	if !strings.Contains(out, "output truncated") {
		t.Fatalf("expected truncated output marker, got %q", out[len(out)-80:])
	}
}

func TestWebSearchRejectsUnconfiguredProvider(t *testing.T) {
	tool := NewWebSearchTool(SearchConfig{Provider: "tavily"}, ProxyConfig{})

	out, err := tool.Call(context.Background(), "test")
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if !strings.Contains(out, "provider=tavily") {
		t.Fatalf("output = %q, want provider=tavily", out)
	}
}

func TestWebSearchConfiguresBraveProvider(t *testing.T) {
	t.Setenv(braveSearchAPIKeyEnv, "")

	tool := NewWebSearchTool(SearchConfig{Provider: "brave", APIKey: "BSA-token"}, ProxyConfig{})
	if tool.provider != searchProviderBrave {
		t.Fatalf("provider = %q, want %q", tool.provider, searchProviderBrave)
	}
	if _, ok := tool.backend.(*braveBackend); !ok {
		t.Fatalf("backend = %T, want *braveBackend", tool.backend)
	}
}

func TestWebSearchConfiguresBraveProviderFromEnv(t *testing.T) {
	t.Setenv(braveSearchAPIKeyEnv, "BSA-env-token")

	tool := NewWebSearchTool(SearchConfig{Provider: "brave-search"}, ProxyConfig{})
	if tool.provider != searchProviderBrave {
		t.Fatalf("provider = %q, want %q", tool.provider, searchProviderBrave)
	}
	backend, ok := tool.backend.(*braveBackend)
	if !ok {
		t.Fatalf("backend = %T, want *braveBackend", tool.backend)
	}
	if backend.apiKey != "BSA-env-token" {
		t.Fatalf("apiKey = %q, want env token", backend.apiKey)
	}
}

func TestBraveSearchBackendSendsTokenAndFormatsResults(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("method = %s, want GET", r.Method)
		}
		if got := r.Header.Get("X-Subscription-Token"); got != "BSA-token" {
			t.Errorf("X-Subscription-Token = %q, want BSA-token", got)
		}
		if got := r.URL.Query().Get("q"); got != "golang testing" {
			t.Errorf("q = %q, want golang testing", got)
		}
		if got := r.URL.Query().Get("count"); got != "5" {
			t.Errorf("count = %q, want 5", got)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"web":{"results":[{"title":"A","url":"https://a.example.com","description":"desc A"},{"title":"B","url":"https://b.example.com","description":"desc B"}]}}`))
	}))
	defer server.Close()

	backend := &braveBackend{
		apiKey:   "BSA-token",
		client:   *server.Client(),
		endpoint: server.URL,
	}
	out, err := backend.Search(context.Background(), "golang testing")
	if err != nil {
		t.Fatalf("Search() error = %v", err)
	}
	for _, want := range []string{"[1] A", "https://a.example.com", "desc A", "[2] B"} {
		if !strings.Contains(out, want) {
			t.Fatalf("output = %q, want substring %q", out, want)
		}
	}
}

func TestBraveSearchBackendReportsHTTPError(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()

	backend := &braveBackend{
		apiKey:   "BSA-token",
		client:   *server.Client(),
		endpoint: server.URL,
	}
	_, err := backend.Search(context.Background(), "golang testing")
	if err == nil {
		t.Fatal("expected HTTP error")
	}
	if !strings.Contains(err.Error(), "429") || !strings.Contains(err.Error(), "rate limited") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestWebScraperRejectsInvalidURL(t *testing.T) {
	ctx, _ := WithToolError(context.Background())
	out, err := NewWebScraperTool(ProxyConfig{}).Call(ctx, `{"url":"not a url"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "invalid url scheme: only http/https allowed" {
		t.Fatalf("output = %q, want invalid URL error message", out)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeInvalidArguments || got.Message != out {
		t.Fatalf("ToolError = %+v, want invalid_arguments with output message", got)
	}
}

func TestWebScraperRejectsHostnamesResolvingToPrivateIPs(t *testing.T) {
	original := lookupScrapeHostIPs
	lookupScrapeHostIPs = func(host string) ([]net.IP, error) {
		if host != "public.example" {
			t.Fatalf("lookup host = %q, want public.example", host)
		}
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}
	defer func() { lookupScrapeHostIPs = original }()

	ctx, _ := WithToolError(context.Background())
	out, err := NewWebScraperTool(ProxyConfig{}).Call(ctx, `{"url":"https://public.example/page"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if out != "url host resolves to a private or link-local address" {
		t.Fatalf("output = %q", out)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeInvalidArguments || got.Message != out {
		t.Fatalf("ToolError = %+v, want invalid_arguments with output message", got)
	}
}

func TestWebScraperRejectsHostnamesRebindingToPrivateIPsAtDialTime(t *testing.T) {
	for _, env := range []string{"HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "all_proxy", "no_proxy"} {
		t.Setenv(env, "")
	}

	originalDelay := webScraperRequestDelay
	webScraperRequestDelay = 0
	defer func() { webScraperRequestDelay = originalDelay }()

	original := lookupScrapeHostIPs
	lookupCalls := 0
	lookupScrapeHostIPs = func(host string) ([]net.IP, error) {
		if host != "rebind.invalid" {
			t.Fatalf("lookup host = %q, want rebind.invalid", host)
		}
		lookupCalls++
		if lookupCalls < 3 {
			return []net.IP{net.ParseIP("93.184.216.34")}, nil
		}
		return []net.IP{net.ParseIP("169.254.169.254")}, nil
	}
	defer func() { lookupScrapeHostIPs = original }()

	ctx, _ := WithToolError(context.Background())
	out, err := NewWebScraperTool(ProxyConfig{}).Call(ctx, `{"url":"http://rebind.invalid/"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if lookupCalls < 3 {
		t.Fatalf("lookup calls = %d, want dial-time validation", lookupCalls)
	}
	if !strings.Contains(out, "url host resolves to a private or link-local address") {
		t.Fatalf("output = %q, want private IP rejection", out)
	}
	if got := ToolErrorFromContext(ctx); got == nil || got.Code != CodeToolExecutionFailed || got.Message != out {
		t.Fatalf("ToolError = %+v, want tool_execution_failed with output message", got)
	}
}

func TestWebScraperDialsLoopbackProxyForPublicTarget(t *testing.T) {
	originalDelay := webScraperRequestDelay
	webScraperRequestDelay = 0
	defer func() { webScraperRequestDelay = originalDelay }()

	original := lookupScrapeHostIPs
	lookupScrapeHostIPs = func(host string) ([]net.IP, error) {
		return []net.IP{net.ParseIP("93.184.216.34")}, nil
	}
	defer func() { lookupScrapeHostIPs = original }()

	var proxiedURL string
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxiedURL = r.URL.String()
		w.Header().Set("Content-Type", "text/html")
		_, _ = io.WriteString(w, "<html><head><title>Proxied</title></head><body><p>hello</p></body></html>")
	}))
	defer proxy.Close()

	ctx, _ := WithToolError(context.Background())
	out, err := NewWebScraperTool(ProxyConfig{HTTPProxy: proxy.URL}).Call(ctx, `{"url":"http://public.example/"}`)
	if err != nil {
		t.Fatalf("Call returned error: %v", err)
	}
	if got := ToolErrorFromContext(ctx); got != nil {
		t.Fatalf("ToolError = %+v, output = %q", got, out)
	}
	if proxiedURL != "http://public.example/" || !strings.Contains(out, "Page Title: Proxied") {
		t.Fatalf("proxied URL = %q, output = %q", proxiedURL, out)
	}
}
