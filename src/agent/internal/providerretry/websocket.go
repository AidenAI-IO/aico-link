package providerretry

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
)

// Dial retries only connection establishment; it never replays session media.
func Dial(ctx context.Context, dialer *websocket.Dialer, endpoint string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	var response *http.Response
	policy := VoicePolicy()
	policy.Timeout = 10 * time.Second
	conn, err := Do(ctx, policy, func(ctx context.Context) (*websocket.Conn, error) {
		conn, resp, err := dialContext(ctx, dialer, endpoint, headers)
		response = resp
		if err != nil && resp != nil {
			var body []byte
			if resp.Body != nil {
				body, _ = io.ReadAll(io.LimitReader(resp.Body, 4096))
				resp.Body.Close()
				resp.Body = http.NoBody
			}
			failure := ResponseError(resp, body).(*StatusError)
			failure.Cause = err
			return nil, failure
		}
		if err != nil {
			return nil, fmt.Errorf("websocket connect: %w", err)
		}
		return conn, nil
	})
	return conn, response, err
}

// Gorilla applies context deadlines to the socket, but cancellation alone
// cannot interrupt an HTTP upgrade waiting for headers (or a proxy CONNECT).
func dialContext(ctx context.Context, dialer *websocket.Dialer, endpoint string, headers http.Header) (*websocket.Conn, *http.Response, error) {
	if dialer == nil {
		dialer = websocket.DefaultDialer
	}
	attempt := *dialer
	type cancellation struct {
		stop func() bool
		done chan struct{}
	}
	var cancellations []cancellation
	watch := func(conn net.Conn, err error) (net.Conn, error) {
		if err == nil {
			done := make(chan struct{})
			stop := context.AfterFunc(ctx, func() {
				_ = conn.Close()
				close(done)
			})
			cancellations = append(cancellations, cancellation{stop, done})
		}
		return conn, err
	}
	attempt.NetDialContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
		if dialer.NetDialContext != nil {
			return watch(dialer.NetDialContext(dialCtx, network, address))
		}
		if dialer.NetDial != nil {
			return watch(dialer.NetDial(network, address))
		}
		return watch((&net.Dialer{}).DialContext(dialCtx, network, address))
	}
	if dialer.NetDialTLSContext != nil {
		attempt.NetDialTLSContext = func(dialCtx context.Context, network, address string) (net.Conn, error) {
			return watch(dialer.NetDialTLSContext(dialCtx, network, address))
		}
	}
	conn, resp, err := attempt.DialContext(ctx, endpoint, headers)
	// Detach before Do cancels its attempt context so a successful session
	// keeps its connection. Join callbacks already running before returning.
	for _, cancellation := range cancellations {
		if !cancellation.stop() {
			<-cancellation.done
		}
	}
	if ctx.Err() != nil {
		if conn != nil {
			_ = conn.Close()
		}
		return nil, resp, ctx.Err()
	}
	return conn, resp, err
}
