package stream

import "testing"

func TestTranscriptLifecycleValidation(t *testing.T) {
	cases := []struct {
		name    string
		event   TranscriptEvent
		wantErr bool
	}{
		{"hypothesis", NewTranscriptHypothesis("s1", 1, "hel"), false},
		{"segment final", NewTranscriptSegmentFinal("s1", 2, "hello"), false},
		{"turn commit", NewTurnCommit([]TranscriptSegment{{SegmentID: "s1", Revision: 2, Text: "hello"}}, "hello"), false},
		{"unknown kind", TranscriptEvent{Kind: "delta"}, true},
		{"empty segment id", NewTranscriptHypothesis("", 1, "hello"), true},
		{"zero revision", NewTranscriptHypothesis("s1", 0, "hello"), true},
		{"hypothesis retraction", NewTranscriptHypothesis("s1", 2, ""), false},
		{"empty segment final", NewTranscriptSegmentFinal("s1", 2, ""), true},
		{"empty commit", NewTurnCommit(nil, ""), true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.event.Validate(); (got != nil) != tc.wantErr {
				t.Fatalf("Validate()=%v, wantErr=%v", got, tc.wantErr)
			}
		})
	}
}

func TestTranscriptCommitRejectsNonAdjacentDuplicateSegments(t *testing.T) {
	event := NewTurnCommit([]TranscriptSegment{
		{SegmentID: "s1", Revision: 1, Text: "one"},
		{SegmentID: "s2", Revision: 1, Text: "two"},
		{SegmentID: "s1", Revision: 2, Text: "one again"},
	}, "one two one again")
	if err := event.Validate(); err == nil {
		t.Fatal("expected duplicate segment validation error")
	}
}

func TestTranscriptCommitPreservesSegmentOrderAndCopiesInput(t *testing.T) {
	segments := []TranscriptSegment{{SegmentID: "s1", Revision: 2, Text: "one"}, {SegmentID: "s2", Revision: 1, Text: "two"}}
	event := NewTurnCommit(segments, "one two")
	segments[0].Text = "changed"
	if event.CommitSegments[0].Text != "one" || event.CommitSegments[1].SegmentID != "s2" {
		t.Fatalf("commit segments changed or reordered: %+v", event.CommitSegments)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
}
