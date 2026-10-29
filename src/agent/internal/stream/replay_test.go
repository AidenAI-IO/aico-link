package stream

import "testing"

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
