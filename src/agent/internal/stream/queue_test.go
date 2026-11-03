package stream

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestQueueBoundsAndCloseDrainAcceptedItems(t *testing.T) {
	q, err := NewQueue[string](1)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.TryPush("first"); err != nil {
		t.Fatal(err)
	}
	if err := q.TryPush("second"); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("full error=%v", err)
	}
	q.Close()
	if err := q.TryPush("late"); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("closed push error=%v", err)
	}
	item, err := q.Pop(context.Background())
	if err != nil || item != "first" {
		t.Fatalf("drain item=%q err=%v", item, err)
	}
	if _, err := q.Pop(context.Background()); !errors.Is(err, ErrQueueClosed) {
		t.Fatalf("empty closed pop error=%v", err)
	}
}

func TestQueuePushAndPopRespectContext(t *testing.T) {
	q, err := NewQueue[int](1)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.Push(context.Background(), 1); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := q.Push(ctx, 2); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked push error=%v", err)
	}

	q2, err := NewQueue[int](1)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := q2.Pop(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("blocked pop error=%v", err)
	}
}

func TestQueueBlockedPushWakesOnClose(t *testing.T) {
	q, err := NewQueue[int](1)
	if err != nil {
		t.Fatal(err)
	}
	if err := q.TryPush(1); err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() { result <- q.Push(context.Background(), 2) }()
	q.Close()
	select {
	case err := <-result:
		if !errors.Is(err, ErrQueueClosed) {
			t.Fatalf("blocked push error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("blocked push did not wake on close")
	}
}
