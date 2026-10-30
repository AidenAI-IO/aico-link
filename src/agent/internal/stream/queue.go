package stream

import (
	"context"
	"errors"
	"sync"
)

var (
	ErrQueueClosed = errors.New("stream queue closed")
	ErrQueueFull   = errors.New("stream queue full")
)

// Queue is a bounded FIFO with context-aware blocking operations.  Close
// rejects new pushes, wakes blocked callers, and leaves already accepted items
// available to Pop so an owner can drain them before releasing resources.
type Queue[T any] struct {
	mu       sync.Mutex
	notEmpty *sync.Cond
	notFull  *sync.Cond
	items    []T
	capacity int
	closed   bool
}

func NewQueue[T any](capacity int) (*Queue[T], error) {
	if capacity <= 0 {
		return nil, errors.New("stream queue capacity must be positive")
	}
	q := &Queue[T]{capacity: capacity, items: make([]T, 0, capacity)}
	q.notEmpty = sync.NewCond(&q.mu)
	q.notFull = sync.NewCond(&q.mu)
	return q, nil
}

func (q *Queue[T]) Capacity() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.capacity
}

func (q *Queue[T]) Len() int {
	if q == nil {
		return 0
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.items)
}

func (q *Queue[T]) IsClosed() bool {
	if q == nil {
		return true
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.closed
}

func (q *Queue[T]) TryPush(item T) error {
	if q == nil {
		return ErrQueueClosed
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.closed {
		return ErrQueueClosed
	}
	if len(q.items) >= q.capacity {
		return ErrQueueFull
	}
	q.items = append(q.items, item)
	q.notEmpty.Signal()
	return nil
}

func (q *Queue[T]) Push(ctx context.Context, item T) error {
	if q == nil {
		return ErrQueueClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stopWake := q.wakeOnCancel(ctx, q.notFull, q.notEmpty)
	defer stopWake()

	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) >= q.capacity && !q.closed && ctx.Err() == nil {
		q.notFull.Wait()
	}
	if q.closed {
		return ErrQueueClosed
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	q.items = append(q.items, item)
	q.notEmpty.Signal()
	return nil
}

func (q *Queue[T]) Pop(ctx context.Context) (T, error) {
	var zero T
	if q == nil {
		return zero, ErrQueueClosed
	}
	if ctx == nil {
		ctx = context.Background()
	}
	stopWake := q.wakeOnCancel(ctx, q.notFull, q.notEmpty)
	defer stopWake()

	q.mu.Lock()
	defer q.mu.Unlock()
	for len(q.items) == 0 && !q.closed && ctx.Err() == nil {
		q.notEmpty.Wait()
	}
	if len(q.items) == 0 {
		if q.closed {
			return zero, ErrQueueClosed
		}
		if err := ctx.Err(); err != nil {
			return zero, err
		}
	}
	item := q.items[0]
	copy(q.items, q.items[1:])
	q.items[len(q.items)-1] = zero
	q.items = q.items[:len(q.items)-1]
	q.notFull.Signal()
	return item, nil
}

// Close is idempotent.  It does not discard items already accepted by the
// queue; Pop drains them and then returns ErrQueueClosed.
func (q *Queue[T]) Close() {
	if q == nil {
		return
	}
	q.mu.Lock()
	if !q.closed {
		q.closed = true
		q.notEmpty.Broadcast()
		q.notFull.Broadcast()
	}
	q.mu.Unlock()
}

func (q *Queue[T]) wakeOnCancel(ctx context.Context, conds ...*sync.Cond) func() {
	if ctx.Done() == nil {
		return func() {}
	}
	stop := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			q.mu.Lock()
			for _, cond := range conds {
				cond.Broadcast()
			}
			q.mu.Unlock()
		case <-stop:
		}
	}()
	return func() { close(stop) }
}
