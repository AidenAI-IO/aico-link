package stream

import (
	"fmt"
	"sort"
	"strings"
)

// TranscriptAccumulator folds revisioned transcript events into one current
// view. Hypotheses replace earlier revisions, final segments become
// immutable, and a turn commit replaces the provisional view with its ordered
// final segments.
//
// TranscriptAccumulator is intentionally not safe for concurrent use. Keep
// event folding on the provider/session reader goroutine, or protect one
// accumulator with the caller's own mutex.
type TranscriptAccumulator struct {
	segments       map[string]TranscriptSegment
	finalSegments  map[string]struct{}
	committed      bool
	commitText     string
	commitSegments []TranscriptSegment
}

// NewTranscriptAccumulator creates an empty transcript fold.
func NewTranscriptAccumulator() *TranscriptAccumulator {
	return &TranscriptAccumulator{
		segments:      make(map[string]TranscriptSegment),
		finalSegments: make(map[string]struct{}),
	}
}

// Apply folds one transcript event into the current state. Invalid,
// conflicting, or out-of-order events return an error without changing state.
// An identical duplicate of the current revision or commit is accepted as a
// no-op, which makes reconnect replay safe.
func (a *TranscriptAccumulator) Apply(event TranscriptEvent) error {
	if a == nil {
		return fmt.Errorf("transcript accumulator is nil")
	}
	if err := event.Validate(); err != nil {
		return err
	}
	a.ensureMaps()

	switch event.Kind {
	case TranscriptHypothesis:
		if a.committed {
			return fmt.Errorf("transcript accumulator: event after turn commit")
		}
		return a.applySegment(event.Segment, false)
	case TranscriptSegmentFinal:
		if a.committed {
			return fmt.Errorf("transcript accumulator: event after turn commit")
		}
		return a.applySegment(event.Segment, true)
	case TranscriptTurnCommit:
		return a.applyCommit(event)
	default:
		return fmt.Errorf("transcript accumulator: unsupported transcript kind %q", event.Kind)
	}
}

func (a *TranscriptAccumulator) ensureMaps() {
	if a.segments == nil {
		a.segments = make(map[string]TranscriptSegment)
	}
	if a.finalSegments == nil {
		a.finalSegments = make(map[string]struct{})
	}
}

func (a *TranscriptAccumulator) applySegment(segment TranscriptSegment, final bool) error {
	current, exists := a.segments[segment.SegmentID]
	_, alreadyFinal := a.finalSegments[segment.SegmentID]
	if alreadyFinal {
		if current == segment && (!final || strings.TrimSpace(segment.Text) != "") {
			return nil
		}
		return fmt.Errorf("transcript accumulator: final segment %q cannot be revised", segment.SegmentID)
	}
	if exists {
		switch {
		case segment.Revision < current.Revision:
			return fmt.Errorf("transcript accumulator: segment %q revision %d is older than %d", segment.SegmentID, segment.Revision, current.Revision)
		case segment.Revision == current.Revision && current != segment:
			return fmt.Errorf("transcript accumulator: conflicting duplicate revision for segment %q", segment.SegmentID)
		case segment.Revision == current.Revision && !final:
			return nil
		}
	}
	a.segments[segment.SegmentID] = segment
	if final {
		a.finalSegments[segment.SegmentID] = struct{}{}
	}
	return nil
}

func (a *TranscriptAccumulator) applyCommit(event TranscriptEvent) error {
	if a.committed {
		if event.Text == a.commitText && equalSegments(event.CommitSegments, a.commitSegments) {
			return nil
		}
		return fmt.Errorf("transcript accumulator: conflicting duplicate turn commit")
	}
	for _, segment := range event.CommitSegments {
		if strings.TrimSpace(segment.Text) == "" {
			return fmt.Errorf("transcript accumulator: turn commit segment %q is empty", segment.SegmentID)
		}
		if current, ok := a.segments[segment.SegmentID]; ok {
			if _, final := a.finalSegments[segment.SegmentID]; final && current != segment {
				return fmt.Errorf("transcript accumulator: commit changes final segment %q", segment.SegmentID)
			}
			if segment.Revision < current.Revision {
				return fmt.Errorf("transcript accumulator: commit segment %q revision %d is older than %d", segment.SegmentID, segment.Revision, current.Revision)
			}
		}
	}

	a.segments = make(map[string]TranscriptSegment, len(event.CommitSegments))
	a.finalSegments = make(map[string]struct{}, len(event.CommitSegments))
	a.commitSegments = append([]TranscriptSegment(nil), event.CommitSegments...)
	for _, segment := range event.CommitSegments {
		a.segments[segment.SegmentID] = segment
		a.finalSegments[segment.SegmentID] = struct{}{}
	}
	a.commitText = event.Text
	a.committed = true
	return nil
}

func equalSegments(left, right []TranscriptSegment) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

// Segments returns a defensive copy of the current segments. A committed
// turn preserves the provider's explicit order. A provisional view is sorted
// by audio position when available and then by stable identifiers.
func (a *TranscriptAccumulator) Segments() []TranscriptSegment {
	if a == nil {
		return nil
	}
	if a.committed {
		return append([]TranscriptSegment(nil), a.commitSegments...)
	}
	segments := make([]TranscriptSegment, 0, len(a.segments))
	for _, segment := range a.segments {
		segments = append(segments, segment)
	}
	sort.SliceStable(segments, func(i, j int) bool {
		left, right := segments[i], segments[j]
		if left.AudioStartSample != right.AudioStartSample {
			if left.AudioStartSample == 0 {
				return false
			}
			if right.AudioStartSample == 0 {
				return true
			}
			return left.AudioStartSample < right.AudioStartSample
		}
		return left.SegmentID < right.SegmentID
	})
	return segments
}

// Text returns the committed turn text, or a deterministic provisional
// presentation assembled from the current non-empty segment snapshots.
func (a *TranscriptAccumulator) Text() string {
	if a == nil {
		return ""
	}
	if a.committed {
		return a.commitText
	}
	segments := a.Segments()
	parts := make([]string, 0, len(segments))
	for _, segment := range segments {
		if text := strings.TrimSpace(segment.Text); text != "" {
			parts = append(parts, text)
		}
	}
	return strings.TrimSpace(strings.Join(parts, " "))
}

// Committed reports whether a turn commit has been applied.
func (a *TranscriptAccumulator) Committed() bool {
	return a != nil && a.committed
}

// Reset clears the current provisional or committed turn.
func (a *TranscriptAccumulator) Reset() {
	if a == nil {
		return
	}
	a.segments = make(map[string]TranscriptSegment)
	a.finalSegments = make(map[string]struct{})
	a.commitSegments = nil
	a.commitText = ""
	a.committed = false
}
