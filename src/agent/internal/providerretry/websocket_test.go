package providerretry

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestDialCancellationInterruptsPendingHandshake(t *testing.T) {
	for _, mode := range []string{"direct", "proxy", "tls-dial"} {
		t.Run(mode, func(t *testing.T) {
			received := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				received <- struct{}{}
				<-r.Context().Done()
			}))
			defer server.Close()
			endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
			dialer := &websocket.Dialer{}
			switch mode {
			case "proxy":
				proxyURL, _ := url.Parse(server.URL)
				dialer.Proxy = http.ProxyURL(proxyURL)
				endpoint = "ws://provider.invalid/stream"
			case "tls-dial":
				endpoint = "wss" + strings.TrimPrefix(server.URL, "http")
				// The custom TLS hook owns transport establishment; a plain local socket
				// lets this test isolate cancellation of the subsequent HTTP upgrade.
				dialer.NetDialTLSContext = (&net.Dialer{}).DialContext
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			result := make(chan error, 1)
			go func() { _, _, err := Dial(ctx, dialer, endpoint, nil); result <- err }()
			select {
			case <-received:
			case <-time.After(time.Second):
				t.Fatal("handshake not received")
			}
			cancel()
			select {
			case err := <-result:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("error=%v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation did not interrupt handshake")
			}
		})
	}
}

func TestDialSuccessfulUpgradeSurvivesAttemptCancellation(t *testing.T) {
	for _, mode := range []string{"default", "net-dial", "context-dial", "tls"} {
		t.Run(mode, func(t *testing.T) {
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				kind, body, err := conn.ReadMessage()
				if err == nil {
					_ = conn.WriteMessage(kind, body)
				}
			}))
			dialer := &websocket.Dialer{HandshakeTimeout: time.Second}
			if mode == "tls" {
				server.StartTLS()
				dialer.TLSClientConfig = &tls.Config{InsecureSkipVerify: true}
			} else {
				server.Start()
			}
			defer server.Close()
			if mode == "net-dial" {
				dialer.NetDial = net.Dial
			}
			if mode == "context-dial" {
				dialer.NetDialContext = (&net.Dialer{}).DialContext
			}
			conn, _, err := Dial(context.Background(), dialer, "ws"+strings.TrimPrefix(server.URL, "http"), nil)
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			_ = conn.SetReadDeadline(time.Now().Add(time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, []byte("alive")); err != nil {
				t.Fatal(err)
			}
			_, body, err := conn.ReadMessage()
			if err != nil || string(body) != "alive" {
				t.Fatalf("body=%q error=%v", body, err)
			}
		})
	}
}
