package minimax

import (
	"crypto/tls"

	"github.com/gorilla/websocket"
)

func websocketDialerForConfig(cfg commonConfig) (websocket.Dialer, error) {
	dialer := websocket.Dialer{
		TLSClientConfig: &tls.Config{
			MinVersion: tls.VersionTLS12,
		},
		HandshakeTimeout: wsConnTimeout,
	}
	if err := cfg.proxy.ConfigureWebSocketDialer(&dialer); err != nil {
		return dialer, err
	}
	return dialer, nil
}
