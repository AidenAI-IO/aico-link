package agent

import (
	"net/http"
)

func newProxyHTTPClient(proxy ProxyConfig) *http.Client {
	return &http.Client{Transport: newProxyTransport(proxy)}
}

func newProxyTransport(proxy ProxyConfig) http.RoundTripper {
	transport := proxy.Transport()

	// Optimize for LLM/STT/TTS endpoints: increase per-host connection pool.
	// Other settings (MaxIdleConns=100, IdleConnTimeout=90s, TLSHandshakeTimeout=10s,
	// ExpectContinueTimeout=1s) already match http.DefaultTransport via Clone().
	transport.MaxIdleConnsPerHost = 8

	return transport
}
