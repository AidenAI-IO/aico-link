package stream

import (
	"errors"
	"fmt"
	"strings"
)

// TranscriptUpdateKind identifies the lifecycle point represented by a
// transcript update. Hypotheses may be replaced by a later revision; a final
// segment is immutable; a turn commit makes the ordered final transcript
// available to downstream consumers.
type TranscriptUpdateKind string

const (
	TranscriptHypothesis   TranscriptUpdateKind = "hypothesis"
	TranscriptSegmentFinal TranscriptUpdateKind = "segment_final"
	TranscriptTurnCommit   TranscriptUpdateKind = "turn_commit"
)

// TranscriptSegment is one provider-independent piece of recognized speech.
// Revision starts at one for each SegmentID and increases monotonically while
// the segment is a hypothesis. Final segments must not be revised.
type TranscriptSegment struct {
	SegmentID        string  `json:"segment_id"`
	Revision         uint64  `json:"revision"`
	Text             string  `json:"text"`
	AudioStartSample uint64  `json:"audio_start_sample,omitempty"`
	AudioEndSample   uint64  `json:"audio_end_sample,omitempty"`
	StablePrefix     string  `json:"stable_prefix,omitempty"`
	Language         string  `json:"language,omitempty"`
	Confidence       float64 `json:"confidence,omitempty"`
}

func (s TranscriptSegment) Validate() error {
	if strings.TrimSpace(s.SegmentID) == "" {
		return errors.New("transcript segment: segment_id is required")
	}
	if s.Revision == 0 {
		return errors.New("transcript segment: revision must be positive")
	}
	return nil
}

// TranscriptEvent is the typed payload for transcript.updated,
// transcript.segment_final, and turn.committed events. Segment is populated
// for hypotheses and segment finals. CommitSegments is populated only for a
// turn commit and is ordered by speech position.
type TranscriptEvent struct {
	Kind           TranscriptUpdateKind `json:"kind"`
	Segment        TranscriptSegment    `json:"segment,omitempty"`
	CommitSegments []TranscriptSegment  `json:"segments,omitempty"`
	Text           string               `json:"text,omitempty"`
}

func (e TranscriptEvent) Validate() error {
	switch e.Kind {
	case TranscriptHypothesis:
		if len(e.CommitSegments) != 0 || e.Text != "" {
			return errors.New("transcript event: hypothesis cannot contain commit fields")
		}
		return e.Segment.Validate()
	case TranscriptSegmentFinal:
		if len(e.CommitSegments) != 0 || e.Text != "" {
			return errors.New("transcript event: segment final cannot contain commit fields")
		}
		if err := e.Segment.Validate(); err != nil {
			return err
		}
		if strings.TrimSpace(e.Segment.Text) == "" {
			return errors.New("transcript event: segment final requires text")
		}
		return nil
	case TranscriptTurnCommit:
		if e.Segment.SegmentID != "" || e.Segment.Revision != 0 {
			return errors.New("transcript event: turn commit cannot contain a segment")
		}
		if len(e.CommitSegments) == 0 {
			return errors.New("transcript event: turn commit requires segments")
		}
		seen := make(map[string]struct{}, len(e.CommitSegments))
		for i, segment := range e.CommitSegments {
			if err := segment.Validate(); err != nil {
				return fmt.Errorf("transcript event: commit segment %d: %w", i, err)
			}
			if _, ok := seen[segment.SegmentID]; ok {
				return errors.New("transcript event: turn commit contains duplicate segment_id")
			}
			seen[segment.SegmentID] = struct{}{}
		}
		if strings.TrimSpace(e.Text) == "" {
			return errors.New("transcript event: turn commit requires text")
		}
		return nil
	default:
		return errors.New("transcript event: kind is required or unknown")
	}
}

// NewTranscriptHypothesis constructs a revisioned hypothesis payload.
func NewTranscriptHypothesis(segmentID string, revision uint64, text string) TranscriptEvent {
	return TranscriptEvent{Kind: TranscriptHypothesis, Segment: TranscriptSegment{SegmentID: segmentID, Revision: revision, Text: text}}
}

// NewTranscriptSegmentFinal constructs an immutable segment final payload.
func NewTranscriptSegmentFinal(segmentID string, revision uint64, text string) TranscriptEvent {
	return TranscriptEvent{Kind: TranscriptSegmentFinal, Segment: TranscriptSegment{SegmentID: segmentID, Revision: revision, Text: text}}
}

// NewTurnCommit constructs a commit payload from ordered final segments.
func NewTurnCommit(segments []TranscriptSegment, text string) TranscriptEvent {
	copied := append([]TranscriptSegment(nil), segments...)
	return TranscriptEvent{Kind: TranscriptTurnCommit, CommitSegments: copied, Text: text}
}
