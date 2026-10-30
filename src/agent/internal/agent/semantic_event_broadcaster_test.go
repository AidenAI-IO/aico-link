package agent

import (
	"context"
	"testing"

	"aico-agent/internal/stream"
)

func semanticTestEvent(sequence uint64) stream.Event {
	return stream.NewEvent(stream.EventIdentity{
		EventID:        "semantic-" + string(rune('a'+sequence)),
		Sequence:       sequence,
		ConversationID: "conversation-1",
		SessionEpoch:   1,
	}, stream.EventTextDelta, map[string]string{"text": "x"})
}

func TestSemanticEventBroadcasterReplaysBeforeLiveEvents(t *testing.T) {
	b := newSemanticEventBroadcaster(8)
	for i := uint64(1); i <= 2; i++ {
		if err := b.Publish(semanticTestEvent(i)); err != nil {
			t.Fatal(err)
		}
	}
	replay, ch, unsubscribe := b.Subscribe(&stream.ReplayCursor{SessionEpoch: 1, Sequence: 1})
	defer unsubscribe()
	if replay.ResyncRequired || len(replay.Events) != 1 || replay.Events[0].Sequence != 2 {
		t.Fatalf("replay = %#v", replay)
	}
	if err := b.Emit(context.Background(), semanticTestEvent(3)); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-ch:
		if event.Sequence != 3 {
			t.Fatalf("live event sequence = %d, want 3", event.Sequence)
		}
	default:
		t.Fatal("live event was not delivered")
	}
}

func TestSemanticEventBroadcasterSubscribeLatestSkipsRetainedEvents(t *testing.T) {
	b := newSemanticEventBroadcaster(8)
	if err := b.Publish(semanticTestEvent(1)); err != nil {
		t.Fatal(err)
	}
	replay, ch, unsubscribe := b.SubscribeLatest()
	defer unsubscribe()
	if replay.ResyncRequired || len(replay.Events) != 0 || replay.Current.Sequence != 1 {
		t.Fatalf("latest replay = %#v", replay)
	}
	if err := b.Publish(semanticTestEvent(2)); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-ch:
		if event.Sequence != 2 {
			t.Fatalf("live sequence = %d, want 2", event.Sequence)
		}
	default:
		t.Fatal("latest subscriber missed the next event")
	}
}

func TestSemanticEventBroadcasterDisconnectsSaturatedSubscriber(t *testing.T) {
	b := newSemanticEventBroadcaster(4)
	_, ch, _ := b.Subscribe(nil)
	for i := uint64(1); i <= semanticEventSubscriberBufferSize+1; i++ {
		if err := b.Publish(semanticTestEvent(i)); err != nil {
			t.Fatal(err)
		}
	}
	count := 0
	for range ch {
		count++
	}
	if count != semanticEventSubscriberBufferSize {
		t.Fatalf("events before disconnect = %d, want %d", count, semanticEventSubscriberBufferSize)
	}
}

func TestSemanticEventBroadcasterReportsReplayResync(t *testing.T) {
	b := newSemanticEventBroadcaster(2)
	for i := uint64(1); i <= 3; i++ {
		if err := b.Publish(semanticTestEvent(i)); err != nil {
			t.Fatal(err)
		}
	}
	replay, _, unsubscribe := b.Subscribe(&stream.ReplayCursor{SessionEpoch: 1, Sequence: 0})
	defer unsubscribe()
	if !replay.ResyncRequired {
		t.Fatal("expired cursor did not require resync")
	}
}
