package stream

import (
	"testing"
	"time"
)

func replayEvent(epoch, sequence uint64) Event {
	return NewEvent(EventIdentity{
		EventID:        "event-" + string(rune('a'+sequence)),
		Sequence:       sequence,
		ConversationID: "conversation-1",
		SessionEpoch:   epoch,
	}, EventTextDelta, map[string]string{"text": "x"})
}

func TestReplayLogRetainsBoundedWindowAndReplaysAfterCursor(t *testing.T) {
	log := NewReplayLog(2)
	for i := uint64(1); i <= 3; i++ {
		if err := log.Append(replayEvent(1, i)); err != nil {
			t.Fatal(err)
		}
	}
	result := log.Snapshot(&ReplayCursor{SessionEpoch: 1, Sequence: 2})
	if result.ResyncRequired || len(result.Events) != 1 || result.Events[0].Sequence != 3 {
		t.Fatalf("replay result = %#v", result)
	}
	if got := log.Cursor(); got != (ReplayCursor{SessionEpoch: 1, Sequence: 3}) {
		t.Fatalf("cursor = %#v", got)
	}
}

func TestReplayLogRequiresResyncForExpiredOrStaleCursors(t *testing.T) {
	log := NewReplayLog(2)
	for i := uint64(1); i <= 3; i++ {
		if err := log.Append(replayEvent(1, i)); err != nil {
			t.Fatal(err)
		}
	}
	if result := log.Snapshot(&ReplayCursor{SessionEpoch: 1, Sequence: 0}); !result.ResyncRequired {
		t.Fatal("expired cursor did not require resync")
	}
	if result := log.Snapshot(&ReplayCursor{SessionEpoch: 0, Sequence: 3}); !result.ResyncRequired {
		t.Fatal("stale epoch did not require resync")
	}
}

func TestReplayLogStartsFreshWindowOnNewEpoch(t *testing.T) {
	log := NewReplayLog(4)
	if err := log.Append(replayEvent(1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(replayEvent(2, 1)); err != nil {
		t.Fatal(err)
	}
	result := log.Snapshot(nil)
	if result.ResyncRequired || len(result.Events) != 1 || result.Events[0].SessionEpoch != 2 {
		t.Fatalf("new epoch snapshot = %#v", result)
	}
}

func TestReplayLogKnownEpochAllowsEmptyCursorWindow(t *testing.T) {
	log := NewReplayLogWithEpoch(4, 7)
	if got := log.Cursor(); got != (ReplayCursor{SessionEpoch: 7}) {
		t.Fatalf("empty cursor = %#v", got)
	}
	result := log.Snapshot(&ReplayCursor{SessionEpoch: 7})
	if result.ResyncRequired || len(result.Events) != 0 || result.Current != (ReplayCursor{SessionEpoch: 7}) {
		t.Fatalf("empty replay result = %#v", result)
	}
	if result := log.Snapshot(&ReplayCursor{SessionEpoch: 6}); !result.ResyncRequired {
		t.Fatal("stale empty cursor did not require resync")
	}
}

func TestReplayLogRejectsConflictingOrdering(t *testing.T) {
	log := NewReplayLog(2)
	if err := log.Append(replayEvent(1, 2)); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(replayEvent(1, 2)); err != nil {
		t.Fatal(err)
	}
	conflict := replayEvent(1, 2)
	conflict.EventID = "different"
	if err := log.Append(conflict); err == nil {
		t.Fatal("conflicting duplicate was accepted")
	}
	if err := log.Append(replayEvent(1, 1)); err == nil {
		t.Fatal("out-of-order event was accepted")
	}
}

func TestReplayLogRejectsSequenceGaps(t *testing.T) {
	log := NewReplayLog(4)
	if err := log.Append(replayEvent(1, 1)); err != nil {
		t.Fatal(err)
	}
	if err := log.Append(replayEvent(1, 3)); err == nil {
		t.Fatal("sequence gap was accepted")
	}
}

func TestReplayLogRejectsEventIDReuseAndConflictingDuplicates(t *testing.T) {
	log := NewReplayLog(4)
	first := replayEvent(1, 1)
	if err := log.Append(first); err != nil {
		t.Fatal(err)
	}
	reused := replayEvent(1, 2)
	reused.EventID = first.EventID
	if err := log.Append(reused); err == nil {
		t.Fatal("event ID reuse was accepted")
	}
	conflict := replayEvent(1, 1)
	conflict.Payload = map[string]string{"text": "different"}
	if err := log.Append(conflict); err == nil {
		t.Fatal("conflicting duplicate payload was accepted")
	}
}

func TestReplayLogAcceptsSameEventWithReplayTimestamps(t *testing.T) {
	log := NewReplayLog(4)
	first := replayEvent(1, 1)
	if err := log.Append(first); err != nil {
		t.Fatal(err)
	}
	replayed := first
	replayed.ReceivedAt = first.ReceivedAt.Add(time.Second)
	replayed.OccurredAt = first.ReceivedAt.Add(2 * time.Second)
	if err := log.Append(replayed); err != nil {
		t.Fatalf("same event with replay timestamps was rejected: %v", err)
	}
}
