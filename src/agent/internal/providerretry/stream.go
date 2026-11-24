package providerretry

import (
	"context"
	"io"
	"net/http"
	"sync"
	"time"
)

// RoundTrip bounds time to headers and each response-body read without limiting
// the total stream lifetime. Heartbeats count as transport activity. Time spent
// in a consumer callback is outside the read timeout.
func RoundTrip(base http.RoundTripper, req *http.Request, idle time.Duration) (*http.Response, error) {
	if idle <= 0 {
		return base.RoundTrip(req)
	}
	ctx, cancel := context.WithCancel(req.Context())
	watch := &idleBody{cancel: cancel, idle: idle}
	watch.arm()
	resp, err := base.RoundTrip(req.Clone(ctx))
	expired := watch.disarm()
	if err != nil || expired {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		_ = watch.Close()
		if expired {
			return nil, context.DeadlineExceeded
		}
		return nil, err
	}
	watch.body = resp.Body
	resp.Body = watch
	return resp, nil
}

type idleBody struct {
	body       io.ReadCloser
	cancel     context.CancelFunc
	mu         sync.Mutex
	timer      *time.Timer
	idle       time.Duration
	generation uint64
	expired    bool
	closed     bool
}

func (b *idleBody) arm() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.expired {
		return
	}
	b.generation++
	generation := b.generation
	b.timer = time.AfterFunc(b.idle, func() {
		b.mu.Lock()
		defer b.mu.Unlock()
		if !b.closed && generation == b.generation {
			b.expired = true
			b.cancel()
		}
	})
}

func (b *idleBody) disarm() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.generation++
	if b.timer != nil {
		b.timer.Stop()
	}
	return b.expired
}

func (b *idleBody) Read(p []byte) (int, error) {
	b.arm()
	n, err := b.body.Read(p)
	if b.disarm() {
		return n, context.DeadlineExceeded
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.generation++
	if b.timer != nil {
		b.timer.Stop()
	}
	b.cancel()
	b.mu.Unlock()
	if b.body != nil {
		return b.body.Close()
	}
	return nil
}
