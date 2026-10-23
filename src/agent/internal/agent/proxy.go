package agent

import (
	"net"
	"net/http"
	"net/url"
	"strings"

	"aico-agent/internal/netproxy"
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
		if bypassProxy(req.URL.Hostname(), req.URL.Port(), proxy.NoProxy) {
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

func bypassProxy(host, port, noProxy string) bool {
	host = strings.Trim(strings.ToLower(strings.TrimSpace(host)), "[]")
	if host == "" {
		return false
	}
	hostPort := host
	if port != "" {
		hostPort = net.JoinHostPort(host, port)
	}

	for _, rawRule := range strings.FieldsFunc(noProxy, func(r rune) bool { return r == ',' || r == ' ' || r == '\t' || r == '\n' }) {
		rule := strings.TrimSpace(strings.ToLower(rawRule))
		if rule == "" {
			continue
		}
		if rule == "*" {
			return true
		}
		if strings.Contains(rule, "/") {
			if ip := net.ParseIP(host); ip != nil {
				if _, network, err := net.ParseCIDR(rule); err == nil && network.Contains(ip) {
					return true
				}
			}
			continue
		}
		if ruleIP := net.ParseIP(rule); ruleIP != nil {
			if hostIP := net.ParseIP(host); hostIP != nil && hostIP.Equal(ruleIP) {
				return true
			}
			continue
		}
		if strings.Contains(rule, ":") && !strings.HasPrefix(rule, ".") {
			if strings.EqualFold(hostPort, rule) {
				return true
			}
			continue
		}
		if strings.HasPrefix(rule, "*.") {
			base := strings.TrimPrefix(rule, "*.")
			if host == base || strings.HasSuffix(host, "."+base) {
				return true
			}
			continue
		}
		rule = strings.TrimPrefix(rule, ".")
		if host == rule || strings.HasSuffix(host, "."+rule) {
			return true
		}
	}
	return false
}
