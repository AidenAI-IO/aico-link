package stream

import (
	"reflect"
	"testing"
)

func TestTranscriptAccumulatorReplacesHypothesesAndSupportsRetraction(t *testing.T) {
	a := NewTranscriptAccumulator()
	if err := a.Apply(NewTranscriptHypothesis("s1", 1, "hel")); err != nil {
		t.Fatal(err)
	}
	if got := a.Text(); got != "hel" {
		t.Fatalf("Text() = %q, want hel", got)
	}
	if err := a.Apply(NewTranscriptHypothesis("s1", 2, "hello")); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(NewTranscriptHypothesis("s1", 3, "")); err != nil {
		t.Fatal(err)
	}
	if got := a.Text(); got != "" {
		t.Fatalf("retracted Text() = %q, want empty", got)
	}
	if err := a.Apply(NewTranscriptHypothesis("s1", 2, "hello")); err == nil {
		t.Fatal("expected out-of-order hypothesis error")
	}
}

func TestTranscriptAccumulatorFinalSegmentsAreImmutable(t *testing.T) {
	a := NewTranscriptAccumulator()
	final := NewTranscriptSegmentFinal("s1", 2, "hello")
	if err := a.Apply(final); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(final); err != nil {
		t.Fatalf("identical duplicate final = %v", err)
	}
	if err := a.Apply(NewTranscriptSegmentFinal("s1", 3, "hello!")); err == nil {
		t.Fatal("expected revision of final segment to fail")
	}
	if err := a.Apply(NewTranscriptHypothesis("s1", 3, "hello!")); err == nil {
		t.Fatal("expected hypothesis after final to fail")
	}
}

func TestTranscriptAccumulatorCommitPreservesOrderAndReconcilesProvisionalState(t *testing.T) {
	a := NewTranscriptAccumulator()
	if err := a.Apply(NewTranscriptHypothesis("s2", 1, "world")); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(NewTranscriptHypothesis("s1", 1, "hello")); err != nil {
		t.Fatal(err)
	}
	segments := []TranscriptSegment{
		{SegmentID: "s1", Revision: 2, Text: "hello"},
		{SegmentID: "s2", Revision: 2, Text: "world"},
	}
	commit := NewTurnCommit(segments, "hello world")
	if err := a.Apply(commit); err != nil {
		t.Fatal(err)
	}
	segments[0].Text = "changed"
	if !a.Committed() || a.Text() != "hello world" {
		t.Fatalf("committed=%v text=%q", a.Committed(), a.Text())
	}
	if got := a.Segments(); !reflect.DeepEqual(got, commit.CommitSegments) {
		t.Fatalf("Segments() = %#v, want %#v", got, commit.CommitSegments)
	}
	if err := a.Apply(commit); err != nil {
		t.Fatalf("identical commit = %v", err)
	}
	if err := a.Apply(NewTranscriptHypothesis("s1", 3, "later")); err == nil {
		t.Fatal("expected event after commit to fail")
	}
}

func TestTranscriptAccumulatorRejectsConflictsWithoutMutatingState(t *testing.T) {
	a := NewTranscriptAccumulator()
	if err := a.Apply(NewTranscriptHypothesis("s1", 2, "hello")); err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(NewTranscriptHypothesis("s1", 2, "different")); err == nil {
		t.Fatal("expected conflicting duplicate revision to fail")
	}
	if got := a.Text(); got != "hello" {
		t.Fatalf("state changed after conflict: %q", got)
	}
	if err := a.Apply(NewTurnCommit([]TranscriptSegment{{SegmentID: "s1", Revision: 1, Text: "old"}}, "old")); err == nil {
		t.Fatal("expected stale commit to fail")
	}
	if a.Committed() {
		t.Fatal("stale commit marked accumulator committed")
	}
}

func TestTranscriptAccumulatorSortsProvisionalSegmentsDeterministically(t *testing.T) {
	a := NewTranscriptAccumulator()
	for _, segment := range []TranscriptSegment{
		{SegmentID: "unknown", Revision: 1, Text: "?"},
		{SegmentID: "late", Revision: 1, Text: "late", AudioStartSample: 20},
		{SegmentID: "early", Revision: 1, Text: "early", AudioStartSample: 10},
	} {
		if err := a.Apply(TranscriptEvent{Kind: TranscriptHypothesis, Segment: segment}); err != nil {
			t.Fatal(err)
		}
	}
	got := a.Segments()
	if len(got) != 3 || got[0].SegmentID != "early" || got[1].SegmentID != "late" || got[2].SegmentID != "unknown" {
		t.Fatalf("provisional order = %#v", got)
	}
}

func TestTranscriptAccumulatorReset(t *testing.T) {
	a := NewTranscriptAccumulator()
	if err := a.Apply(NewTurnCommit([]TranscriptSegment{{SegmentID: "s1", Revision: 1, Text: "hello"}}, "hello")); err != nil {
		t.Fatal(err)
	}
	a.Reset()
	if a.Committed() || a.Text() != "" || len(a.Segments()) != 0 {
		t.Fatalf("reset state: committed=%v text=%q segments=%#v", a.Committed(), a.Text(), a.Segments())
	}
}
