package netproxy

import (
	"context"
	"net"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestConfigProxyFuncPrefersSchemeProxyOverAllProxy(t *testing.T) {
	fn := Config{
		HTTPProxy:  "http://http-proxy.example:1",
		HTTPSProxy: "http://https-proxy.example:2",
		AllProxy:   "http://all-proxy.example:3",
		NoProxy:    "localhost",
	}.ProxyFunc()

	for target, want := range map[string]string{
		"http://api.example/v1":  "http://http-proxy.example:1",
		"https://api.example/v1": "http://https-proxy.example:2",
		"ftp://api.example/v1":   "http://all-proxy.example:3",
	} {
		assertProxyFuncResult(t, fn, target, want)
	}
}

func TestConfigProxyFuncFallsBackToAllProxy(t *testing.T) {
	fn := Config{AllProxy: "socks5h://127.0.0.1:18080", NoProxy: "localhost"}.ProxyFunc()

	assertProxyFuncResult(t, fn, "https://api.example/v1", "socks5h://127.0.0.1:18080")
	assertProxyFuncResult(t, fn, "http://api.example/v1", "socks5h://127.0.0.1:18080")
}

func TestConfigProxyFuncHonorsNoProxy(t *testing.T) {
	fn := Config{
		HTTPSProxy: "http://127.0.0.1:7890",
		AllProxy:   "http://127.0.0.1:7890",
		NoProxy:    "*.invalid,api.example,203.0.113.0/24,::1",
	}.ProxyFunc()

	for _, target := range []string{
		"https://api.example/v1",
		"https://tts.api.example/v1",
		"https://service.invalid/v1",
		"http://203.0.113.10/status",
		"http://[::1]:8080/status",
	} {
		assertProxyFuncResult(t, fn, target, "")
	}
	assertProxyFuncResult(t, fn, "https://other.example/v1", "http://127.0.0.1:7890")
}

func TestConfigProxyFuncAppliesDefaultNoProxy(t *testing.T) {
	fn := Config{AllProxy: "http://127.0.0.1:7890"}.ProxyFunc()

	for _, target := range []string{
		"http://localhost/status",
		"http://192.168.1.20/status",
		"http://[fe80::1]/status",
	} {
		assertProxyFuncResult(t, fn, target, "")
	}
	assertProxyFuncResult(t, fn, "https://api.example/v1", "http://127.0.0.1:7890")
}

func TestConfigProxyFuncUsesEnvironmentWhenNoProxyURLConfigured(t *testing.T) {
	clearProxyEnv(t)
	t.Setenv("HTTPS_PROXY", "http://proxy.example:7893")
	t.Setenv("NO_PROXY", "bypass.example")

	fn := Config{}.ProxyFunc()

	assertProxyFuncResult(t, fn, "https://api.example/v1", "http://proxy.example:7893")
	assertProxyFuncResult(t, fn, "https://bypass.example/v1", "")
}

func TestConfigProxyFuncRejectsInvalidProxyURL(t *testing.T) {
	fn := Config{HTTPSProxy: "http://http://proxy.example:7893"}.ProxyFunc()

	req, err := http.NewRequest(http.MethodGet, "https://api.example/v1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fn(req); err == nil || !strings.Contains(err.Error(), "duplicate scheme") {
		t.Fatalf("ProxyFunc() error = %v, want duplicate scheme error", err)
	}
}

func TestConfigTransportUsesProxyFunc(t *testing.T) {
	transport := Config{HTTPSProxy: "http://127.0.0.1:7890", NoProxy: "bypass.example"}.Transport()

	assertProxyFuncResult(t, transport.Proxy, "https://api.example/v1", "http://127.0.0.1:7890")
	assertProxyFuncResult(t, transport.Proxy, "https://bypass.example/v1", "")
}

func TestConfigureWebSocketDialerPrecedence(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "https proxy first",
			cfg:  Config{HTTPProxy: "http://http-proxy.example:1", HTTPSProxy: "http://https-proxy.example:2", AllProxy: "http://all-proxy.example:3"},
			want: "http://https-proxy.example:2",
		},
		{
			name: "http proxy before all proxy",
			cfg:  Config{HTTPProxy: "http://http-proxy.example:1", AllProxy: "http://all-proxy.example:3"},
			want: "http://http-proxy.example:1",
		},
		{
			name: "all proxy last",
			cfg:  Config{AllProxy: "http://all-proxy.example:3"},
			want: "http://all-proxy.example:3",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var dialer websocket.Dialer
			if err := tc.cfg.ConfigureWebSocketDialer(&dialer); err != nil {
				t.Fatalf("ConfigureWebSocketDialer() error = %v", err)
			}
			if dialer.Proxy == nil {
				t.Fatal("dialer.Proxy = nil, want configured proxy")
			}
			assertProxyFuncResult(t, dialer.Proxy, "https://api.example/ws", tc.want)
		})
	}
}

func TestConfigureWebSocketDialerHTTPProxyHonorsNoProxy(t *testing.T) {
	var dialer websocket.Dialer
	err := Config{AllProxy: "http://127.0.0.1:7890", NoProxy: "api.example,::1"}.ConfigureWebSocketDialer(&dialer)
	if err != nil {
		t.Fatalf("ConfigureWebSocketDialer() error = %v", err)
	}

	assertProxyFuncResult(t, dialer.Proxy, "https://api.example/ws", "")
	assertProxyFuncResult(t, dialer.Proxy, "http://[::1]:8080/ws", "")
	assertProxyFuncResult(t, dialer.Proxy, "https://other.example/ws", "http://127.0.0.1:7890")
}

func TestConfigureWebSocketDialerAppliesDefaultNoProxy(t *testing.T) {
	var dialer websocket.Dialer
	if err := (Config{HTTPSProxy: "http://127.0.0.1:7890"}).ConfigureWebSocketDialer(&dialer); err != nil {
		t.Fatalf("ConfigureWebSocketDialer() error = %v", err)
	}

	assertProxyFuncResult(t, dialer.Proxy, "http://10.1.2.3:8080/ws", "")
	assertProxyFuncResult(t, dialer.Proxy, "https://api.example/ws", "http://127.0.0.1:7890")
}

func TestConfigureWebSocketDialerSOCKS5HonorsNoProxy(t *testing.T) {
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

	// Port 1 is not listening, so only a direct dial can reach the listener.
	var dialer websocket.Dialer
	err = Config{AllProxy: "socks5://127.0.0.1:1", NoProxy: "127.0.0.1"}.ConfigureWebSocketDialer(&dialer)
	if err != nil {
		t.Fatalf("ConfigureWebSocketDialer() error = %v", err)
	}
	if dialer.NetDialContext == nil {
		t.Fatal("dialer.NetDialContext = nil, want SOCKS5 dialer")
	}

	conn, err := dialer.NetDialContext(context.Background(), "tcp", listener.Addr().String())
	if err != nil {
		t.Fatalf("NetDialContext() error = %v, want direct NO_PROXY dial", err)
	}
	_ = conn.Close()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for the direct NO_PROXY connection")
	}

	if conn, err := dialer.NetDialContext(context.Background(), "tcp", "api.example:443"); err == nil {
		_ = conn.Close()
		t.Fatal("NetDialContext() error = nil, want SOCKS5 proxy dial failure")
	}
}

func TestConfigureWebSocketDialerLeavesDialerWithoutProxyURL(t *testing.T) {
	var dialer websocket.Dialer
	if err := (Config{NoProxy: "api.example"}).ConfigureWebSocketDialer(&dialer); err != nil {
		t.Fatalf("ConfigureWebSocketDialer() error = %v", err)
	}
	if dialer.Proxy != nil || dialer.NetDialContext != nil {
		t.Fatal("ConfigureWebSocketDialer() changed the dialer without a proxy URL")
	}
}

func TestConfigureWebSocketDialerRejectsInvalidProxyURL(t *testing.T) {
	var dialer websocket.Dialer
	err := Config{AllProxy: "ftp://127.0.0.1:21"}.ConfigureWebSocketDialer(&dialer)
	if err == nil || !strings.Contains(err.Error(), "unsupported proxy URL scheme") {
		t.Fatalf("ConfigureWebSocketDialer() error = %v, want unsupported scheme error", err)
	}
}

func assertProxyFuncResult(t *testing.T, fn func(*http.Request) (*url.URL, error), target, want string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, target, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := fn(req)
	if err != nil {
		t.Fatalf("proxy(%s) error = %v", target, err)
	}
	if want == "" {
		if got != nil {
			t.Fatalf("proxy(%s) = %v, want direct connection", target, got)
		}
		return
	}
	if got == nil || got.String() != want {
		t.Fatalf("proxy(%s) = %v, want %s", target, got, want)
	}
}
