// Package providerretry supplies bounded retries for external inference calls.
package providerretry

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

type Policy struct {
	MaxRetries int
	BaseDelay  time.Duration
	Timeout    time.Duration
}

func VoicePolicy() Policy {
	return Policy{MaxRetries: 2, BaseDelay: 500 * time.Millisecond, Timeout: 30 * time.Second}
}

type managedKey struct{}

// Managed marks operations whose enclosing session owns the retry budget.
func Managed(ctx context.Context) context.Context { return context.WithValue(ctx, managedKey{}, true) }

// Do shares its budget with nested operations and respects the caller's lifetime.
func Do[T any](ctx context.Context, policy Policy, call func(context.Context) (T, error)) (T, error) {
	if ctx.Value(managedKey{}) != nil {
		// The owner supplies the retry count, but nested buffered requests and
		// WebSocket upgrades still need their own operation timeout.
		if policy.Timeout > 0 {
			attemptCtx, cancel := context.WithTimeout(ctx, policy.Timeout)
			defer cancel()
			return call(attemptCtx)
		}
		return call(ctx)
	}
	var zero T
	for attempt := 0; ; attempt++ {
		if err := ctx.Err(); err != nil {
			return zero, err
		}
		attemptCtx := context.WithValue(ctx, managedKey{}, true)
		cancel := func() {}
		if policy.Timeout > 0 {
			attemptCtx, cancel = context.WithTimeout(attemptCtx, policy.Timeout)
		}
		value, err := call(attemptCtx)
		cancel()
		if err == nil {
			return value, nil
		}
		if ctx.Err() != nil {
			return zero, ctx.Err()
		}
		if attempt >= policy.MaxRetries || !Retryable(err) {
			return value, err
		}
		if err := Wait(ctx, Delay(err, time.Duration(attempt+1)*policy.BaseDelay)); err != nil {
			return zero, err
		}
	}
}

func HTTPStatus(code int) bool {
	return code == 408 || code == 409 || code == 429 || code >= 500 && code < 600
}

func Retryable(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) {
		return false
	}
	var status interface{ HTTPStatusCode() int }
	if errors.As(err, &status) {
		return HTTPStatus(status.HTTPStatusCode())
	}
	var closed *websocket.CloseError
	if errors.As(err, &closed) {
		return closed.Code == websocket.CloseAbnormalClosure || closed.Code == websocket.CloseInternalServerErr || closed.Code == websocket.CloseServiceRestart || closed.Code == websocket.CloseTryAgainLater
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	for _, transient := range []error{syscall.ECONNRESET, syscall.ECONNREFUSED, syscall.EPIPE, syscall.ENETUNREACH, syscall.EHOSTUNREACH} {
		if errors.Is(err, transient) {
			return true
		}
	}
	var network net.Error
	return errors.As(err, &network) && (network.Timeout() || network.Temporary())
}

func Wait(ctx context.Context, delay time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if delay <= 0 {
		return nil
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func Delay(err error, minimum time.Duration) time.Duration {
	var after interface{ RetryDelay() time.Duration }
	if errors.As(err, &after) && after.RetryDelay() > minimum {
		return after.RetryDelay()
	}
	return minimum
}

func RetryAfter(header string, now time.Time) time.Duration {
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

// StatusError preserves structured HTTP failure details through adapter wrappers.
type StatusError struct {
	Status int
	Body   string
	After  time.Duration
	Cause  error
}

func (e *StatusError) Error() string             { return fmt.Sprintf("API error %d: %s", e.Status, e.Body) }
func (e *StatusError) HTTPStatusCode() int       { return e.Status }
func (e *StatusError) RetryDelay() time.Duration { return e.After }
func (e *StatusError) Unwrap() error             { return e.Cause }
func ResponseError(resp *http.Response, body []byte) error {
	return &StatusError{Status: resp.StatusCode, Body: string(body), After: RetryAfter(resp.Header.Get("Retry-After"), time.Now())}
}
