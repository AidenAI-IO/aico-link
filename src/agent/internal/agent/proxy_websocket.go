package agent

import (
	"crypto/tls"
	"time"

	"github.com/gorilla/websocket"
)

func newProxyWebSocketDialer(cfg ProxyConfig, handshakeTimeout time.Duration) (websocket.Dialer, error) {
	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		HandshakeTimeout: handshakeTimeout,
	}
	err := cfg.ConfigureWebSocketDialer(&dialer)
	return dialer, err
}
