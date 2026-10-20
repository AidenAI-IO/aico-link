package agent

import (
	"net/http"
	"net/url"
	"strings"

	"aiden-agent/internal/netproxy"
)

func newProxyHTTPClient(proxy ProxyConfig) *http.Client {
	return &http.Client{Transport: newProxyTransport(proxy)}
}

func newProxyTransport(proxy ProxyConfig) http.RoundTripper {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.Proxy = proxyFunc(proxy)

	// Optimize for LLM/STT/TTS endpoints: increase per-host connection pool.
	// Other settings (MaxIdleConns=100, IdleConnTimeout=90s, TLSHandshakeTimeout=10s,
	// ExpectContinueTimeout=1s) already match http.DefaultTransport via Clone().
	transport.MaxIdleConnsPerHost = 8

	return transport
}

func proxyFunc(proxy ProxyConfig) func(*http.Request) (*url.URL, error) {
	proxy = proxy.WithDefaults()
	if !proxy.HasProxyURL() {
		return func(req *http.Request) (*url.URL, error) {
			return netproxy.ProxyFromEnvironment(req, "http", "https", "socks5", "socks5h")
		}
	}

	return func(req *http.Request) (*url.URL, error) {
		if req == nil || req.URL == nil {
			return nil, nil
		}
		if netproxy.Bypass(req.URL.Hostname(), req.URL.Port(), proxy.NoProxy) {
			return nil, nil
		}

		proxyURL := strings.TrimSpace(proxy.AllProxy)
		switch strings.ToLower(req.URL.Scheme) {
		case "http":
			if strings.TrimSpace(proxy.HTTPProxy) != "" {
				proxyURL = proxy.HTTPProxy
			}
		case "https":
			if strings.TrimSpace(proxy.HTTPSProxy) != "" {
				proxyURL = proxy.HTTPSProxy
			}
		}
		if strings.TrimSpace(proxyURL) == "" {
			return nil, nil
		}
		return parseProxyURL(proxyURL)
	}
}

func validateProxyURL(raw string) error {
	if raw == "" {
		return nil
	}
	_, err := parseProxyURL(raw)
	return err
}

func parseProxyURL(raw string) (*url.URL, error) {
	return netproxy.Parse(raw, "http", "https", "socks5", "socks5h")
}
