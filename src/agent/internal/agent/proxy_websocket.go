package agent

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"

	"aiden-agent/internal/netproxy"
)

func newProxyWebSocketDialer(cfg ProxyConfig, handshakeTimeout time.Duration) (websocket.Dialer, error) {
	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		HandshakeTimeout: handshakeTimeout,
	}

	cfg = cfg.WithDefaults()
	proxyURL := strings.TrimSpace(cfg.AllProxy)
	if strings.TrimSpace(cfg.HTTPSProxy) != "" {
		proxyURL = cfg.HTTPSProxy
	} else if strings.TrimSpace(cfg.HTTPProxy) != "" {
		proxyURL = cfg.HTTPProxy
	}
	if proxyURL == "" {
		return dialer, nil
	}

	u, err := parseProxyURL(proxyURL)
	if err != nil {
		return dialer, err
	}

	switch strings.ToLower(u.Scheme) {
	case "socks5", "socks5h":
		d, err := proxy.FromURL(u, proxy.Direct)
		if err != nil {
			return dialer, fmt.Errorf("socks5 proxy: %w", err)
		}
		ctxDialer, ok := d.(proxy.ContextDialer)
		if !ok {
			return dialer, fmt.Errorf("socks5 proxy does not support context dialing")
		}
		direct := &net.Dialer{}
		dialer.NetDialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			if host, port, err := net.SplitHostPort(addr); err == nil && netproxy.Bypass(host, port, cfg.NoProxy) {
				return direct.DialContext(ctx, network, addr)
			}
			return ctxDialer.DialContext(ctx, network, addr)
		}
	case "http", "https":
		dialer.Proxy = func(req *http.Request) (*url.URL, error) {
			if req != nil && req.URL != nil && netproxy.Bypass(req.URL.Hostname(), req.URL.Port(), cfg.NoProxy) {
				return nil, nil
			}
			return u, nil
		}
	default:
		return dialer, fmt.Errorf("unsupported proxy URL scheme %q", u.Scheme)
	}

	return dialer, nil
}
