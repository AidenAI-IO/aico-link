package agent

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"aico-agent/internal/logging"
	"github.com/tmc/langchaingo/llms"
)

type providerRetryContextKey struct{}

type providerRetryAttempt struct {
	statusCode int
	retryAfter time.Duration
}

func providerRetryManaged(ctx context.Context) bool {
	_, managed := ctx.Value(providerRetryContextKey{}).(*providerRetryAttempt)
	return managed
}

type providerRetryPolicy struct {
	maxRetries     int
	retryDelayBase time.Duration
	requestTimeout time.Duration
}

func defaultProviderRetryPolicy() providerRetryPolicy {
	return providerRetryPolicy{maxRetries: 5, retryDelayBase: 2 * time.Second, requestTimeout: defaultLLMHTTPTimeout}
}

// withProviderRetry gives each non-streaming attempt its own timeout, while the
// caller's cancellation/deadline bounds the entire operation. Streaming calls
// retain their caller-controlled lifetime and cannot replay delivered output.
func withProviderRetry[T any](ctx context.Context, policy providerRetryPolicy, options []llms.CallOption, call func(context.Context, []llms.CallOption) (T, error)) (T, error) {
	var zero T
	callOpts := llms.CallOptions{}
	for _, option := range options {
		option(&callOpts)
	}
	streaming := callOpts.StreamingFunc != nil || callOpts.StreamingReasoningFunc != nil
	var outputDelivered atomic.Bool
	guardedOptions := append([]llms.CallOption(nil), options...)
	if stream := callOpts.StreamingFunc; stream != nil {
		guardedOptions = append(guardedOptions, llms.WithStreamingFunc(func(ctx context.Context, chunk []byte) error {
			if len(chunk) > 0 {
				outputDelivered.Store(true)
			}
			err := stream(ctx, chunk)
			if err != nil {
				outputDelivered.Store(true)
			}
			return err
		}))
	}
	if stream := callOpts.StreamingReasoningFunc; stream != nil {
		guardedOptions = append(guardedOptions, llms.WithStreamingReasoningFunc(func(ctx context.Context, reasoning, chunk []byte) error {
			if len(reasoning) > 0 || len(chunk) > 0 {
				outputDelivered.Store(true)
			}
			err := stream(ctx, reasoning, chunk)
			if err != nil {
				outputDelivered.Store(true)
			}
			return err
		}))
	}
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		state := &providerRetryAttempt{}
		attemptCtx := context.WithValue(ctx, providerRetryContextKey{}, state)
		cancel := func() {}
		if !streaming && policy.requestTimeout > 0 {
			attemptCtx, cancel = context.WithTimeout(attemptCtx, policy.requestTimeout)
		}
		result, err := call(attemptCtx, guardedOptions)
		cancel()
		if err == nil {
			return result, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if attempt >= policy.maxRetries || outputDelivered.Load() || !isRetryableProviderError(err, state.statusCode) {
			return result, err
		}
		delay := time.Duration(attempt+1) * policy.retryDelayBase
		if state.retryAfter > delay {
			delay = state.retryAfter
		}
		logging.Warnf("agent", "provider_retry", "request failed on attempt %d/%d; retrying after %v: %v", attempt+1, policy.maxRetries+1, delay, err)
		if err := sleepWithContext(ctx, delay); err != nil {
			return zero, err
		}
	}
}

func isRetryableProviderError(err error, httpStatus int) bool {
	if err == nil || errors.Is(err, context.Canceled) || isProviderContextExceededError(err) {
		return false
	}
	// Anthropic retains its separate semantic-response repair budget. Its
	// transient stream failures use the shared provider budget instead.
	var protocolErr *anthropicResponseProtocolError
	if errors.As(err, &protocolErr) {
		return false
	}
	var streamErr *anthropicStreamError
	if errors.As(err, &streamErr) {
		if streamErr.outputDelivered {
			return false
		}
		if streamErr.retryable {
			return true
		}
	}
	var statusErr interface{ HTTPStatusCode() int }
	if errors.As(err, &statusErr) {
		return shouldRetryHTTPStatus(statusErr.HTTPStatusCode())
	}
	// Some SDKs (such as Ollama) do not expose their status in a public error
	// interface, so the shared HTTP transport also records the response status.
	if httpStatus >= 400 {
		return shouldRetryHTTPStatus(httpStatus)
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	for _, transient := range []error{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE, syscall.ENETUNREACH, syscall.EHOSTUNREACH} {
		if errors.Is(err, transient) {
			return true
		}
	}
	var netErr net.Error
	return errors.As(err, &netErr) && (netErr.Timeout() || netErr.Temporary())
}

func providerRetryAfter(header string, now time.Time) time.Duration {
	header = strings.TrimSpace(header)
	if seconds, err := strconv.ParseInt(header, 10, 64); err == nil {
		if seconds > 0 && seconds <= int64((1<<63-1)/time.Second) {
			return time.Duration(seconds) * time.Second
		}
		return 0
	}
	if date, err := http.ParseTime(header); err == nil && date.After(now) {
		return date.Sub(now)
	}
	return 0
}
