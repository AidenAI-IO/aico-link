package agent

import (
	"context"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestProxyFuncUsesConfiguredHTTPSProxy(t *testing.T) {
	fn := ProxyConfig{
		HTTPSProxy: "http://127.0.0.1:7890",
		NoProxy:    "localhost,127.0.0.1",
	}.ProxyFunc()

	req, err := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := fn(req)
	if err != nil {
		t.Fatalf("ProxyFunc() error = %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "http://127.0.0.1:7890" {
		t.Fatalf("proxyURL = %v, want configured proxy", proxyURL)
	}
}

func TestProxyFuncAcceptsSOCKS5HForRemoteDNS(t *testing.T) {
	fn := ProxyConfig{HTTPSProxy: "socks5h://127.0.0.1:18080"}.ProxyFunc()
	req, err := http.NewRequest(http.MethodGet, "https://api.openai.com/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := fn(req)
	if err != nil {
		t.Fatalf("ProxyFunc() error = %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "socks5h://127.0.0.1:18080" {
		t.Fatalf("proxyURL = %v, want SOCKS5H local proxy", proxyURL)
	}
}

func TestProxyFuncHonorsNoProxy(t *testing.T) {
	fn := ProxyConfig{
		AllProxy: "http://127.0.0.1:7890",
		NoProxy:  "localhost,*.invalid,api.openai.com,203.0.113.0/24",
	}.ProxyFunc()

	for _, target := range []string{
		"https://api.openai.com/v1/models",
		"https://api.invalid/v1/models",
		"http://203.0.113.10/status",
	} {
		req, err := http.NewRequest(http.MethodGet, target, nil)
		if err != nil {
			t.Fatal(err)
		}
		proxyURL, err := fn(req)
		if err != nil {
			t.Fatalf("ProxyFunc(%s) error = %v", target, err)
		}
		if proxyURL != nil {
			t.Fatalf("ProxyFunc(%s) = %v, want bypass", target, proxyURL)
		}
	}
}

func TestProxyFuncHonorsIPv6NoProxy(t *testing.T) {
	fn := ProxyConfig{
		AllProxy: "http://127.0.0.1:7890",
		NoProxy:  "::1",
	}.ProxyFunc()
	req, err := http.NewRequest(http.MethodGet, "http://[::1]:8080/status", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	proxyURL, err := fn(req)
	if err != nil {
		t.Fatalf("ProxyFunc() error = %v", err)
	}
	if proxyURL != nil {
		t.Fatalf("ProxyFunc() = %v, want IPv6 no_proxy bypass", proxyURL)
	}
}

func TestProxyFuncAppliesDefaultNoProxy(t *testing.T) {
	fn := ProxyConfig{
		AllProxy: "http://127.0.0.1:7890",
	}.ProxyFunc()
	req, err := http.NewRequest(http.MethodGet, "http://localhost/status", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	proxyURL, err := fn(req)
	if err != nil {
		t.Fatalf("ProxyFunc() error = %v", err)
	}
	if proxyURL != nil {
		t.Fatalf("ProxyFunc() = %v, want default no_proxy bypass", proxyURL)
	}
}

func TestProxyFuncUsesEnvironmentWhenNoProxyURLConfigured(t *testing.T) {
	clearAgentProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://proxy.example:7893")

	fn := ProxyConfig{}.ProxyFunc()
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/owner/repo/releases/latest", nil)
	if err != nil {
		t.Fatal(err)
	}
	proxyURL, err := fn(req)
	if err != nil {
		t.Fatalf("ProxyFunc() error = %v", err)
	}
	if proxyURL == nil || proxyURL.String() != "http://proxy.example:7893" {
		t.Fatalf("proxyURL = %v, want environment proxy", proxyURL)
	}
}

func TestProxyFuncRejectsInvalidEnvironmentProxy(t *testing.T) {
	clearAgentProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://http://proxy.example:7893")

	fn := ProxyConfig{}.ProxyFunc()
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/owner/repo/releases/latest", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fn(req)
	if err == nil {
		t.Fatal("ProxyFunc() error = nil, want duplicate scheme error")
	}
	if !strings.Contains(err.Error(), "HTTPS_PROXY") || !strings.Contains(err.Error(), "duplicate scheme") {
		t.Fatalf("ProxyFunc() error = %v, want env name and duplicate scheme", err)
	}
}

func TestProxyConfigValidateRejectsRelativeURL(t *testing.T) {
	if err := (ProxyConfig{HTTPProxy: "127.0.0.1:7890"}).Validate(); err == nil {
		t.Fatal("expected invalid relative proxy URL")
	}
}

func TestProxyConfigValidateRejectsWhitespace(t *testing.T) {
	err := (ProxyConfig{HTTPProxy: " http://proxy.example:7893"}).Validate()
	if err == nil {
		t.Fatal("expected invalid whitespace proxy URL")
	}
	if !strings.Contains(err.Error(), "whitespace") {
		t.Fatalf("Validate() error = %v, want whitespace", err)
	}
}

func TestProxyConfigValidateRejectsDuplicateScheme(t *testing.T) {
	err := (ProxyConfig{HTTPSProxy: "http://http://proxy.example:7893"}).Validate()
	if err == nil {
		t.Fatal("expected invalid duplicate scheme proxy URL")
	}
	if !strings.Contains(err.Error(), "duplicate scheme") {
		t.Fatalf("Validate() error = %v, want duplicate scheme", err)
	}
}

func TestProxyFuncRejectsDuplicateScheme(t *testing.T) {
	fn := ProxyConfig{HTTPSProxy: "http://http://proxy.example:7893"}.ProxyFunc()
	req, err := http.NewRequest(http.MethodGet, "https://api.github.com/repos/owner/repo/releases/latest", nil)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fn(req)
	if err == nil {
		t.Fatal("ProxyFunc() error = nil, want duplicate scheme error")
	}
	if !strings.Contains(err.Error(), "duplicate scheme") {
		t.Fatalf("ProxyFunc() error = %v, want duplicate scheme", err)
	}
}

func TestNewProxyWebSocketDialerSocks5HonorsNoProxy(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	defer listener.Close()

	accepted := make(chan struct{})
	go func() {
		conn, err := listener.Accept()
		if err == nil {
			_ = conn.Close()
			close(accepted)
		}
	}()

	dialer, err := newProxyWebSocketDialer(ProxyConfig{
		AllProxy: "socks5://127.0.0.1:1",
		NoProxy:  "127.0.0.1",
	}, time.Second)
	if err != nil {
		t.Fatalf("newProxyWebSocketDialer() error = %v", err)
	}
	if dialer.NetDialContext == nil {
		t.Fatal("expected websocket dialer to install NetDialContext")
	}

	conn, err := dialer.NetDialContext(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("NetDialContext() error = %v, want direct no_proxy dial", err)
	}
	_ = conn.Close()

	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for direct connection to bypass the proxy")
	}
}

func clearAgentProxyEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"http_proxy", "HTTP_PROXY", "https_proxy", "HTTPS_PROXY", "all_proxy", "ALL_PROXY", "no_proxy", "NO_PROXY"} {
		t.Setenv(key, "")
	}
}
