package netproxy

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

// DefaultNoProxy keeps loopback, link-local, and private-network destinations
// off an explicit proxy when NO_PROXY is not set.
const DefaultNoProxy = "localhost,127.0.0.1,::1,10.0.0.0/8,172.16.0.0/12,192.168.0.0/16,169.254.0.0/16,fc00::/7,fe80::/10"

// Config holds explicit proxy settings, normally read from HTTP_PROXY,
// HTTPS_PROXY, ALL_PROXY, and NO_PROXY.
type Config struct {
	HTTPProxy  string
	HTTPSProxy string
	AllProxy   string
	NoProxy    string
}

func (c Config) HasProxyURL() bool {
	return strings.TrimSpace(c.HTTPProxy) != "" ||
		strings.TrimSpace(c.HTTPSProxy) != "" ||
		strings.TrimSpace(c.AllProxy) != ""
}

// WithDefaults applies DefaultNoProxy when a proxy URL is set without NO_PROXY.
func (c Config) WithDefaults() Config {
	if c.HasProxyURL() && strings.TrimSpace(c.NoProxy) == "" {
		c.NoProxy = DefaultNoProxy
	}
	return c
}

// ProxyFunc returns an http.Transport proxy function. Requests skip NO_PROXY
// destinations and otherwise use the proxy for their scheme, falling back to
// ALL_PROXY. Without any proxy URL it defers to ProxyFromEnvironment.
func (c Config) ProxyFunc() func(*http.Request) (*url.URL, error) {
	c = c.WithDefaults()
	if !c.HasProxyURL() {
		return func(req *http.Request) (*url.URL, error) {
			return ProxyFromEnvironment(req)
		}
	}

	return func(req *http.Request) (*url.URL, error) {
		if req == nil || req.URL == nil {
			return nil, nil
		}
		if Bypass(req.URL.Hostname(), req.URL.Port(), c.NoProxy) {
			return nil, nil
		}

		proxyURL := strings.TrimSpace(c.AllProxy)
		switch strings.ToLower(req.URL.Scheme) {
		case "http":
			if strings.TrimSpace(c.HTTPProxy) != "" {
				proxyURL = c.HTTPProxy
			}
		case "https":
			if strings.TrimSpace(c.HTTPSProxy) != "" {
				proxyURL = c.HTTPSProxy
			}
		}
		if strings.TrimSpace(proxyURL) == "" {
			return nil, nil
		}
		return Parse(proxyURL)
	}
}

// Transport returns a clone of http.DefaultTransport that selects proxies with
// ProxyFunc.
func (c Config) Transport() *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = c.ProxyFunc()
	return transport
}

// ConfigureWebSocketDialer routes dialer through HTTPS_PROXY, HTTP_PROXY, or
// ALL_PROXY, in that order, and dials NO_PROXY destinations directly for both
// HTTP and SOCKS5 proxies. Without any proxy URL the dialer is left unchanged.
func (c Config) ConfigureWebSocketDialer(dialer *websocket.Dialer) error {
	c = c.WithDefaults()
	proxyURL := strings.TrimSpace(c.AllProxy)
	if strings.TrimSpace(c.HTTPSProxy) != "" {
		proxyURL = c.HTTPSProxy
	} else if strings.TrimSpace(c.HTTPProxy) != "" {
		proxyURL = c.HTTPProxy
	}
	if proxyURL == "" {
		return nil
	}

	u, err := Parse(proxyURL)
	if err != nil {
		return err
	}

	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		d, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return fmt.Errorf("socks5 proxy: %w", err)
		}
		ctxDialer, ok := d.(proxy.ContextDialer)
		if !ok {
			return fmt.Errorf("socks5 proxy does not support context dialing")
		}
		direct := &net.Dialer{}
		dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, port, err := net.SplitHostPort(addr); err == nil && Bypass(host, port, c.NoProxy) {
				return direct.DialContext(ctx, network, addr)
			}
			return ctxDialer.DialContext(ctx, network, addr)
		}
	case "http", "https":
		dialer.Proxy = func(req *http.Request) (*url.URL, error) {
			if req != nil && req.URL != nil && Bypass(req.URL.Hostname(), req.URL.Port(), c.NoProxy) {
				return nil, nil
			}
			return u, nil
		}
	default:
		return fmt.Errorf("unsupported proxy URL scheme %q", u.Scheme)
	}
	return nil
}
