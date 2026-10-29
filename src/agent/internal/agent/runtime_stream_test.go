package agent

import (
	"context"
	"errors"
	"testing"
	"time"

	"aico-agent/internal/stream"
)

type recordingStreamSink struct {
	events []stream.Event
	err    error
}

func (s *recordingStreamSink) Emit(_ context.Context, event stream.Event) error {
	if s.err != nil {
		return s.err
	}
	s.events = append(s.events, event)
	return nil
}

func TestRuntimeStreamEmitterAssignsIdentityAndOrder(t *testing.T) {
	sink := &recordingStreamSink{}
	emitter := newStreamEventEmitter(sink, stream.EventScope{
		ConversationID: "conversation-1",
		SessionEpoch:   4,
		TurnID:         "turn-1",
		OutputEpoch:    2,
	}, "request-1", "run-1", "episode-1")
	emitter.emitTextDelta(context.Background(), "hello")
	emitter.emitRunEvent(context.Background(), RunEvent{
		Type:       runEventToolProgress,
		ToolCallID: "call-1",
		ToolName:   "shell",
		ToolStatus: "running",
		Content:    "started",
	})

	if len(sink.events) != 2 {
		t.Fatalf("event count=%d, want 2", len(sink.events))
	}
	first, second := sink.events[0], sink.events[1]
	if first.Kind != stream.EventTextDelta || second.Kind != stream.EventToolProgress {
		t.Fatalf("kinds=%q,%q", first.Kind, second.Kind)
	}
	if first.Sequence != 1 || second.Sequence != 2 {
		t.Fatalf("sequences=%d,%d", first.Sequence, second.Sequence)
	}
	if first.ConversationID != "conversation-1" || first.SessionEpoch != 4 || first.OutputEpoch != 2 {
		t.Fatalf("identity=%+v", first.EventIdentity)
	}
	if second.CallID != "call-1" || second.ItemID != "shell" {
		t.Fatalf("tool identity=%+v", second.EventIdentity)
	}
	for _, event := range sink.events {
		if err := event.Validate(); err != nil {
			t.Fatalf("invalid emitted event: %v", err)
		}
	}
}

func TestRuntimeCallbackEmitsTypedTextAlongsideLegacyWriter(t *testing.T) {
	sink := &recordingStreamSink{}
	legacy := &recordingWriter{}
	h := &runtimeCallbackHandler{
		writer:        legacy,
		streamEmitter: newStreamEventEmitter(sink, stream.EventScope{}, "request-1", "run-1", "episode-1"),
	}
	h.HandleStreamingFunc(context.Background(), []byte("delta"))
	h.HandleAssistantOutput(context.Background(), "final")

	if got := string(legacy.buf); got != "delta" {
		t.Fatalf("legacy writer=%q", got)
	}
	if len(sink.events) != 2 || sink.events[0].Kind != stream.EventTextDelta || sink.events[1].Kind != stream.EventTextFinal {
		t.Fatalf("events=%+v", sink.events)
	}
}

func TestRuntimeStreamEmitterBoundsFailedSink(t *testing.T) {
	sink := &blockingStreamSink{}
	emitter := newStreamEventEmitter(sink, stream.EventScope{}, "request-1", "run-1", "episode-1")
	started := time.Now()
	emitter.emitTextDelta(context.Background(), "delta")
	if elapsed := time.Since(started); elapsed > streamEventEmitTimeout+100*time.Millisecond {
		t.Fatalf("sink blocked for %s", elapsed)
	}
	if sink.lastErr == nil || !errors.Is(sink.lastErr, context.DeadlineExceeded) {
		t.Fatalf("sink error=%v", sink.lastErr)
	}
}

type recordingWriter struct{ buf []byte }

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.buf = append(w.buf, p...)
	return len(p), nil
}

type blockingStreamSink struct{ lastErr error }

func (s *blockingStreamSink) Emit(ctx context.Context, _ stream.Event) error {
	<-ctx.Done()
	s.lastErr = ctx.Err()
	return s.lastErr
}
