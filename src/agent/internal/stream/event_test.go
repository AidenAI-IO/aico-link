package stream

import (
	"errors"
	"testing"
	"time"
)

func testIdentity() EventIdentity {
	return EventIdentity{
		EventID: "evt-1", Sequence: 1, ConversationID: "conversation-1", SessionEpoch: 3,
		TurnID: "turn-1", RunID: "run-1", ResponseID: "response-1", AttemptID: "attempt-1",
		OutputEpoch: 4,
	}
}

func TestEventIdentityValidatesAndFiltersStaleEpochs(t *testing.T) {
	identity := testIdentity()
	if err := identity.Validate(); err != nil {
		t.Fatal(err)
	}
	if identity.StaleFor(4) {
		t.Fatal("current output epoch was treated as stale")
	}
	if !identity.StaleFor(5) {
		t.Fatal("invalidated output epoch was accepted")
	}
	identity.OutputEpoch = 0
	if identity.StaleFor(99) {
		t.Fatal("epoch-less event should not be stale")
	}

	for name, mutate := range map[string]func(*EventIdentity){
		"event id":        func(i *EventIdentity) { i.EventID = "" },
		"sequence":        func(i *EventIdentity) { i.Sequence = 0 },
		"conversation id": func(i *EventIdentity) { i.ConversationID = "" },
		"session epoch":   func(i *EventIdentity) { i.SessionEpoch = 0 },
	} {
		t.Run(name, func(t *testing.T) {
			invalid := testIdentity()
			mutate(&invalid)
			if err := invalid.Validate(); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

func TestEventValidationAndNewEvent(t *testing.T) {
	event := NewEvent(testIdentity(), EventTextDelta, map[string]string{"text": "hi"})
	if event.SchemaVersion != CurrentSchemaVersion || event.ReceivedAt.IsZero() {
		t.Fatalf("unexpected event defaults: %+v", event)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	event.OccurredAt = time.Now()
	event.ReceivedAt = event.OccurredAt.Add(-time.Second)
	if err := event.Validate(); err != nil {
		t.Fatalf("provider/local clock skew should be accepted: %v", err)
	}
	event = NewEvent(testIdentity(), "", nil)
	if err := event.Validate(); err == nil {
		t.Fatal("expected missing kind error")
	}
	event = NewEvent(testIdentity(), EventTextDelta, nil)
	event.ReceivedAt = time.Time{}
	if err := event.Validate(); err == nil {
		t.Fatal("expected missing received_at error")
	}
}

func TestTerminalTrackerAllowsOneTerminalOutcome(t *testing.T) {
	var tracker TerminalTracker
	if got := tracker.State(); got != TerminalPending {
		t.Fatalf("initial state=%q", got)
	}
	if err := tracker.Complete(); err != nil {
		t.Fatal(err)
	}
	if got := tracker.State(); got != TerminalCompleted {
		t.Fatalf("state=%q", got)
	}
	if err := tracker.Fail(errors.New("late failure")); !errors.Is(err, ErrTerminalAlreadySet) {
		t.Fatalf("late transition error=%v", err)
	}
	if err := tracker.Finish(TerminalPending, nil); !errors.Is(err, ErrInvalidTerminal) {
		t.Fatalf("pending transition error=%v", err)
	}
}

func TestTerminalTrackerRecordsFailure(t *testing.T) {
	tracker := &TerminalTracker{}
	want := errors.New("provider eof")
	if err := tracker.Fail(want); err != nil {
		t.Fatal(err)
	}
	outcome := tracker.Outcome()
	if outcome.State != TerminalFailed || !errors.Is(outcome.Err, want) {
		t.Fatalf("outcome=%+v", outcome)
	}
}

func TestTerminalTrackerTreatsEOFWithoutTerminalAsFailure(t *testing.T) {
	tracker := &TerminalTracker{}
	if err := tracker.FailEOF(); err != nil {
		t.Fatal(err)
	}
	outcome := tracker.Outcome()
	if outcome.State != TerminalFailed || !errors.Is(outcome.Err, ErrUnexpectedEOF) {
		t.Fatalf("outcome=%+v", outcome)
	}
}
