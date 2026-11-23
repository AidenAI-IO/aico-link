package providerretry

import (
	"bytes"
	"context"
	"io"
	"net/http"
)

// HTTP retries a replayable buffered request, including body reads. Successful
// bodies are detached from the attempt context before it is canceled.
func HTTP(ctx context.Context, client *http.Client, req *http.Request) (*http.Response, error) {
	if req.GetBody != nil && req.Body != nil {
		defer req.Body.Close()
	}
	policy := VoicePolicy()
	if req.Body != nil && req.Body != http.NoBody && req.GetBody == nil {
		policy.MaxRetries = 0
	}
	return Do(ctx, policy, func(ctx context.Context) (*http.Response, error) {
		attempt := req.Clone(ctx)
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			attempt.Body = body
		}
		resp, err := client.Do(attempt)
		if err != nil {
			return nil, err
		}
		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			failure := ResponseError(resp, body).(*StatusError)
			failure.Cause = err
			return nil, failure
		}
		if err != nil {
			return nil, err
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return resp, nil
	})
}
