package wifiproxy

import (
	"context"
	"crypto/tls"
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	xproxy "golang.org/x/net/proxy"
)

func startTestProxy(t *testing.T, config Config, fallback Upstreams) (*Server, *url.URL) {
	t.Helper()
	configPath := filepath.Join(t.TempDir(), "wifi-proxies.json")
	if err := Save(configPath, config); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(DefaultListenAddress, configPath, "missing-test-interface", fallback)
	if err != nil {
		t.Fatal(err)
	}
	server.SetEnvironmentPath("")
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- server.Serve(listener) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
		<-done
	})
	proxyURL, err := url.Parse("http://" + listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return server, proxyURL
}

func TestValidateListenAddressRejectsInvalidPorts(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "127.0.0.1:65536", "localhost:not-a-port"} {
		if err := validateListenAddress(address); err == nil {
			t.Fatalf("validateListenAddress(%q) succeeded", address)
		}
	}
	if err := validateListenAddress(DefaultListenAddress); err != nil {
		t.Fatalf("validateListenAddress(%q): %v", DefaultListenAddress, err)
	}
}

func TestLocalProxySupportsSOCKS5WithoutHTTPConversion(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "through socks5")
	}))
	defer origin.Close()
	_, localURL := startTestProxy(t, EmptyConfig(), Upstreams{})
	dialer, err := xproxy.FromURL(&url.URL{Scheme: "socks5", Host: localURL.Host}, xproxy.Direct)
	if err != nil {
		t.Fatal(err)
	}
	transport := &http.Transport{DialContext: contextDialer(dialer)}
	response, err := (&http.Client{Transport: transport}).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "through socks5" {
		t.Fatalf("body=%q", body)
	}
}

func TestLocalSOCKS5EntryUsesSOCKS5Upstream(t *testing.T) {
	upstreamAddress, _ := startSOCKS5TestServer(t)
	config := EmptyConfig()
	config.Networks["Office"] = Network{Mode: ModeProxy, ProxyURL: "socks5://" + upstreamAddress}
	local, _ := startTestProxy(t, config, Upstreams{})
	local.SetCurrentSSID("Office")
	upstream, err := local.upstreamFor(&url.URL{Scheme: "socks5", Host: "example.com:443"})
	if err != nil {
		t.Fatal(err)
	}
	if upstream == nil || upstream.Scheme != "socks5" || upstream.Host != upstreamAddress {
		t.Fatalf("upstream=%v, want SOCKS5 %s", upstream, upstreamAddress)
	}
}

func startSOCKS5TestServer(t *testing.T) (string, *atomic.Int32) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var handshakes atomic.Int32
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			client, err := listener.Accept()
			if err != nil {
				return
			}
			go func() {
				defer client.Close()
				var greeting [2]byte
				if _, err := io.ReadFull(client, greeting[:]); err != nil || greeting[0] != 0x05 {
					return
				}
				methods := make([]byte, int(greeting[1]))
				if _, err := io.ReadFull(client, methods); err != nil {
					return
				}
				handshakes.Add(1)
				if _, err := client.Write([]byte{0x05, 0x00}); err != nil {
					return
				}
				var header [4]byte
				if _, err := io.ReadFull(client, header[:]); err != nil || header[1] != 0x01 {
					return
				}
				host, err := readSOCKS5Host(client, header[3])
				if err != nil {
					return
				}
				var port [2]byte
				if _, err := io.ReadFull(client, port[:]); err != nil {
					return
				}
				remote, err := net.Dial("tcp", net.JoinHostPort(host, fmt.Sprintf("%d", binary.BigEndian.Uint16(port[:]))))
				if err != nil {
					_ = writeSOCKS5Reply(client, 0x05)
					return
				}
				if err := writeSOCKS5Reply(client, 0x00); err != nil {
					remote.Close()
					return
				}
				tunnel(client, remote)
			}()
		}
	}()
	t.Cleanup(func() {
		listener.Close()
		<-done
	})
	return listener.Addr().String(), &handshakes
}

func TestLocalProxyEnvironmentKeepsSOCKS5Scheme(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "wifi-proxies.json")
	config := EmptyConfig()
	config.Networks["Office"] = Network{Mode: ModeProxy, ProxyURL: "socks5://user:password@proxy.example:7897", NoProxy: "internal.example", NoProxySet: true}
	if err := Save(configPath, config); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer("127.0.0.1:18080", configPath, "missing", Upstreams{})
	if err != nil {
		t.Fatal(err)
	}
	environmentPath := filepath.Join(t.TempDir(), "proxy-env")
	server.SetEnvironmentPath(environmentPath)
	server.SetCurrentSSID("Office")
	if err := server.writeLocalProxyEnvironment(); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(environmentPath)
	if err != nil {
		t.Fatal(err)
	}
	want := "HTTP_PROXY=socks5h://127.0.0.1:18080\nHTTPS_PROXY=socks5h://127.0.0.1:18080\nALL_PROXY=socks5h://127.0.0.1:18080\nhttp_proxy=socks5h://127.0.0.1:18080\nhttps_proxy=socks5h://127.0.0.1:18080\nall_proxy=socks5h://127.0.0.1:18080\nNO_PROXY='internal.example'\nno_proxy='internal.example'\nAICO_WIFI_PROXY_NO_PROXY_SET=1\n"
	if string(data) != want {
		t.Fatalf("environment=%q, want %q", data, want)
	}
	assertMode := func(path string, want os.FileMode) {
		t.Helper()
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != want {
			t.Fatalf("%s mode=%o, want %o", path, info.Mode().Perm(), want)
		}
	}
	assertMode(environmentPath, 0o644)
	assertMode(configPath, 0o600)
	// An unchanged file must still have its old root-only permissions repaired.
	if err := os.Chmod(environmentPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := server.writeLocalProxyEnvironment(); err != nil {
		t.Fatal(err)
	}
	assertMode(environmentPath, 0o644)
	assertMode(configPath, 0o600)
}

func TestUpstreamsFromEnvironmentTracksNoProxyPresence(t *testing.T) {
	for _, name := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		value, existed := os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if existed {
				_ = os.Setenv(name, value)
			} else {
				_ = os.Unsetenv(name)
			}
		})
	}
	upstreams, err := UpstreamsFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if upstreams.NoProxy != DefaultNoProxy || upstreams.NoProxySet {
		t.Fatalf("absent NO_PROXY produced %#v", upstreams)
	}
	if err := os.Setenv("HTTP_PROXY", ""); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("http_proxy", "http://proxy.example:8080"); err != nil {
		t.Fatal(err)
	}
	upstreams, err = UpstreamsFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if upstreams.HTTPProxy != "http://proxy.example:8080" {
		t.Fatalf("lowercase HTTP proxy was masked by an empty uppercase value: %#v", upstreams)
	}
	if err := os.Setenv("NO_PROXY", ""); err != nil {
		t.Fatal(err)
	}
	upstreams, err = UpstreamsFromEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if upstreams.NoProxy != "" || !upstreams.NoProxySet {
		t.Fatalf("explicit empty NO_PROXY produced %#v", upstreams)
	}
	if err := os.Setenv("NO_PROXY", "bad\nvalue"); err != nil {
		t.Fatal(err)
	}
	if _, err := UpstreamsFromEnvironment(); err == nil {
		t.Fatal("invalid NO_PROXY was accepted")
	}
}

func TestLocalProxyEnvironmentMarksDefaultAndExplicitEmptyNoProxy(t *testing.T) {
	for _, test := range []struct {
		name     string
		fallback Upstreams
		noProxy  string
		flag     string
	}{
		{name: "default", fallback: Upstreams{NoProxy: DefaultNoProxy}, noProxy: DefaultNoProxy, flag: "0"},
		{name: "explicit empty", fallback: Upstreams{NoProxySet: true}, noProxy: "", flag: "1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := NewServer("127.0.0.1:18080", filepath.Join(t.TempDir(), "missing.json"), "missing", test.fallback)
			if err != nil {
				t.Fatal(err)
			}
			environmentPath := filepath.Join(t.TempDir(), "proxy-env")
			server.SetEnvironmentPath(environmentPath)
			if err := server.writeLocalProxyEnvironment(); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(environmentPath)
			if err != nil {
				t.Fatal(err)
			}
			contents := string(data)
			if !strings.Contains(contents, "NO_PROXY='"+test.noProxy+"'\n") ||
				!strings.Contains(contents, "AICO_WIFI_PROXY_NO_PROXY_SET="+test.flag+"\n") {
				t.Fatalf("environment=%q", contents)
			}
		})
	}
}

func TestSOCKS5FallbackUsesGenericProxyOrder(t *testing.T) {
	fallback := Upstreams{
		HTTPProxy:  "http://http-proxy.example:3128",
		HTTPSProxy: "http://https-proxy.example:3129",
	}
	if got := fallbackProxyForScheme(fallback, "socks5"); got != fallback.HTTPSProxy {
		t.Fatalf("SOCKS5 fallback=%q, want HTTPS_PROXY fallback %q", got, fallback.HTTPSProxy)
	}
	fallback.AllProxy = "socks5://all-proxy.example:1080"
	if got := fallbackProxyForScheme(fallback, "socks5"); got != fallback.AllProxy {
		t.Fatalf("SOCKS5 fallback=%q, want ALL_PROXY %q", got, fallback.AllProxy)
	}
}

func TestSOCKS5FallbackPrefersAllProxyOverSchemeSpecificProxy(t *testing.T) {
	server, _ := startTestProxy(t, EmptyConfig(), Upstreams{
		HTTPProxy:  "http://http-proxy.example:3128",
		HTTPSProxy: "http://https-proxy.example:3129",
		AllProxy:   "socks5://all-proxy.example:1080",
	})
	upstream, err := server.genericFallbackUpstream("example.com:8080")
	if err != nil {
		t.Fatal(err)
	}
	if upstream == nil || upstream.String() != "socks5://all-proxy.example:1080" {
		t.Fatalf("SOCKS5 upstream=%v", upstream)
	}
}

func TestCustomProxyBypassesLoopbackWhenNoProxyOmitted(t *testing.T) {
	config := EmptyConfig()
	config.Networks["Office"] = Network{Mode: ModeProxy, ProxyURL: "http://proxy.example:7890"}
	server, _ := startTestProxy(t, config, Upstreams{})
	server.SetCurrentSSID("Office")
	for _, host := range []string{
		"127.0.0.1:8080", "10.1.2.3:8080", "172.16.1.2:8080", "192.168.42.1:8080",
		"169.254.1.1:8080", "[fd00::1]:8080", "[fe80::1]:8080",
	} {
		upstream, err := server.upstreamFor(&url.URL{Scheme: "http", Host: host})
		if err != nil {
			t.Fatal(err)
		}
		if upstream != nil {
			t.Fatalf("bypassed target %s upstream=%v, want direct", host, upstream)
		}
	}
	upstream, err := server.upstreamFor(&url.URL{Scheme: "http", Host: "service.example:8080"})
	if err != nil {
		t.Fatal(err)
	}
	if upstream == nil || upstream.Host != "proxy.example:7890" {
		t.Fatalf("external upstream=%v, want custom proxy", upstream)
	}
}

func proxyClient(proxyURL *url.URL, tlsConfig *tls.Config) *http.Client {
	return &http.Client{Transport: &http.Transport{Proxy: http.ProxyURL(proxyURL), TLSClientConfig: tlsConfig}}
}

func TestLocalProxyForwardsHTTPDirect(t *testing.T) {
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		w.Header().Set("X-Origin", request.URL.Path)
		_, _ = io.WriteString(w, "direct")
	}))
	defer origin.Close()
	_, localURL := startTestProxy(t, EmptyConfig(), Upstreams{})
	response, err := proxyClient(localURL, nil).Get(origin.URL + "/through-proxy")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "direct" || response.Header.Get("X-Origin") != "/through-proxy" {
		t.Fatalf("status=%s body=%q headers=%v", response.Status, body, response.Header)
	}
}

func TestLocalProxySupportsHTTPSConnect(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "secure")
	}))
	defer origin.Close()
	_, localURL := startTestProxy(t, EmptyConfig(), Upstreams{})
	client := proxyClient(localURL, &tls.Config{InsecureSkipVerify: true}) // Test server certificate.
	response, err := client.Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "secure" {
		t.Fatalf("body=%q", body)
	}
}

func TestSSIDSelectsCustomUpstream(t *testing.T) {
	var upstreamRequests atomic.Int32
	upstreamHTTP := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests.Add(1)
		_, _ = io.WriteString(w, "custom upstream")
	}))
	defer upstreamHTTP.Close()

	config := EmptyConfig()
	config.Networks["Office"] = Network{Mode: ModeProxy, ProxyURL: upstreamHTTP.URL}
	local, localURL := startTestProxy(t, config, Upstreams{})
	local.SetCurrentSSID("Office")
	response, err := proxyClient(localURL, nil).Get("http://service.example/resource")
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if upstreamRequests.Load() != 1 {
		t.Fatalf("upstream requests=%d, want 1", upstreamRequests.Load())
	}
}

func TestSSIDDirectOverridesFallbackProxy(t *testing.T) {
	var fallbackRequests atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackRequests.Add(1)
		http.Error(w, "unexpected fallback", http.StatusBadGateway)
	}))
	defer fallback.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "direct")
	}))
	defer origin.Close()
	config := EmptyConfig()
	config.Networks["Home"] = Network{Mode: ModeDirect}
	local, localURL := startTestProxy(t, config, Upstreams{HTTPProxy: fallback.URL})
	local.SetCurrentSSID("Home")
	response, err := proxyClient(localURL, nil).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	if fallbackRequests.Load() != 0 {
		t.Fatalf("fallback requests=%d, want 0", fallbackRequests.Load())
	}
}

func TestSSIDCustomNoProxyBypassesCustomProxy(t *testing.T) {
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests.Add(1)
		http.Error(w, "unexpected upstream", http.StatusBadGateway)
	}))
	defer upstream.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "direct")
	}))
	defer origin.Close()
	config := EmptyConfig()
	config.Networks["Office"] = Network{
		Mode: ModeProxy, ProxyURL: upstream.URL, NoProxy: "127.0.0.1",
	}
	local, localURL := startTestProxy(t, config, Upstreams{})
	local.SetCurrentSSID("Office")
	response, err := proxyClient(localURL, nil).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "direct" || upstreamRequests.Load() != 0 {
		t.Fatalf("body=%q upstream requests=%d", body, upstreamRequests.Load())
	}
}

func TestSSIDCustomProxyDoesNotInheritSystemNoProxy(t *testing.T) {
	var upstreamRequests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests.Add(1)
		_, _ = io.WriteString(w, "custom proxy")
	}))
	defer upstream.Close()
	config := EmptyConfig()
	config.Networks["Office"] = Network{
		Mode: ModeProxy, ProxyURL: upstream.URL, NoProxy: ".custom.example",
	}
	local, localURL := startTestProxy(t, config, Upstreams{NoProxy: ".system.example"})
	local.SetCurrentSSID("Office")
	response, err := proxyClient(localURL, nil).Get("http://service.system.example/resource")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "custom proxy" || upstreamRequests.Load() != 1 {
		t.Fatalf("body=%q upstream requests=%d", body, upstreamRequests.Load())
	}
}

func TestSSIDSystemUsesSystemNoProxy(t *testing.T) {
	var fallbackRequests atomic.Int32
	fallback := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackRequests.Add(1)
		http.Error(w, "unexpected fallback", http.StatusBadGateway)
	}))
	defer fallback.Close()
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "direct")
	}))
	defer origin.Close()
	local, localURL := startTestProxy(t, EmptyConfig(), Upstreams{
		HTTPProxy: fallback.URL, NoProxy: "127.0.0.1",
	})
	local.SetCurrentSSID("Office")
	response, err := proxyClient(localURL, nil).Get(origin.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "direct" || fallbackRequests.Load() != 0 {
		t.Fatalf("body=%q fallback requests=%d", body, fallbackRequests.Load())
	}
}

func TestSSIDCustomHTTPProxySupportsAuthenticatedHTTPSConnect(t *testing.T) {
	origin := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "secure through upstream")
	}))
	defer origin.Close()

	var connects atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Method != http.MethodConnect {
			http.Error(w, "CONNECT required", http.StatusMethodNotAllowed)
			return
		}
		if request.Header.Get("Proxy-Authorization") != "Basic YWxpY2U6c2VjcmV0" {
			http.Error(w, "proxy authentication required", http.StatusProxyAuthRequired)
			return
		}
		remote, err := net.Dial("tcp", origin.Listener.Addr().String())
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadGateway)
			return
		}
		hijacker, ok := w.(http.Hijacker)
		if !ok {
			remote.Close()
			http.Error(w, "hijacking unavailable", http.StatusInternalServerError)
			return
		}
		client, buffered, err := hijacker.Hijack()
		if err != nil {
			remote.Close()
			return
		}
		connects.Add(1)
		_, _ = buffered.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		if err := buffered.Flush(); err != nil {
			client.Close()
			remote.Close()
			return
		}
		tunnel(&bufferedConn{Conn: client, reader: buffered.Reader}, remote)
	}))
	defer upstream.Close()

	upstreamURL := strings.Replace(upstream.URL, "http://", "http://alice:secret@", 1)
	config := EmptyConfig()
	config.Networks["Office"] = Network{Mode: ModeProxy, ProxyURL: upstreamURL}
	local, localURL := startTestProxy(t, config, Upstreams{})
	local.SetCurrentSSID("Office")
	client := proxyClient(localURL, &tls.Config{InsecureSkipVerify: true}) // Test server certificate.
	response, err := client.Get("https://service.example/")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	body, _ := io.ReadAll(response.Body)
	if string(body) != "secure through upstream" || connects.Load() != 1 {
		t.Fatalf("body=%q upstream CONNECTs=%d", body, connects.Load())
	}
}
