package providerretry

import (
	"context"
	"fmt"
	"io"
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
		conn, resp, err := dialer.DialContext(ctx, endpoint, headers)
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
