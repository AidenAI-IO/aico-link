package agent

import (
	"context"
	"sync"

	"aico-agent/internal/stream"
)

const semanticEventSubscriberBufferSize = 64

// semanticEventBroadcaster owns the v2 observer path. It retains a bounded
// replay window and disconnects a subscriber whose queue is saturated so the
// producer never silently loses an event for that subscriber.
type semanticEventBroadcaster struct {
	mu          sync.Mutex
	replay      *stream.ReplayLog
	subscribers map[chan stream.Event]struct{}
}

func newSemanticEventBroadcaster(replayCapacity int) *semanticEventBroadcaster {
	return &semanticEventBroadcaster{
		replay:      stream.NewReplayLog(replayCapacity),
		subscribers: make(map[chan stream.Event]struct{}),
	}
}

// Emit implements stream.EventSink. The context bounds only the append
// admission check; subscriber delivery is nonblocking and bounded.
func (b *semanticEventBroadcaster) Emit(ctx context.Context, event stream.Event) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return b.Publish(event)
}

func (b *semanticEventBroadcaster) Publish(event stream.Event) error {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := b.replay.Append(event); err != nil {
		return err
	}
	for ch := range b.subscribers {
		select {
		case ch <- event:
		default:
			delete(b.subscribers, ch)
			close(ch)
		}
	}
	return nil
}

// Subscribe registers a subscriber before taking its replay snapshot. The
// broadcaster lock makes the snapshot watermark and live delivery atomic from
// the subscriber's perspective, so no event can fall between them.
func (b *semanticEventBroadcaster) Subscribe(cursor *stream.ReplayCursor) (stream.ReplayResult, <-chan stream.Event, func()) {
	if b == nil {
		return stream.ReplayResult{ResyncRequired: true}, nil, func() {}
	}
	b.mu.Lock()
	ch := make(chan stream.Event, semanticEventSubscriberBufferSize)
	b.subscribers[ch] = struct{}{}
	replay := b.replay.Snapshot(cursor)
	b.mu.Unlock()
	var once sync.Once
	unsubscribe := func() {
		once.Do(func() {
			b.mu.Lock()
			if _, ok := b.subscribers[ch]; ok {
				delete(b.subscribers, ch)
				close(ch)
			}
			b.mu.Unlock()
		})
	}
	return replay, ch, unsubscribe
}
