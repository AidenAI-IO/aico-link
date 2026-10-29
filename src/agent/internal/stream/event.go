// Package stream contains the small, provider-independent contracts shared by
// streaming conversation components.  It deliberately has no dependency on
// the Agent runtime, transports, or provider SDKs.
package stream

import (
	"context"
	"errors"
	"time"
)

// CurrentSchemaVersion is the first version of the shared semantic event
// envelope.  Consumers should still tolerate additive fields from newer
// versions when they can safely do so.
const CurrentSchemaVersion uint16 = 2

// EventKind identifies the semantic family of an event.  The list is kept
// intentionally small; provider-specific details belong in Payload or Origin.
type EventKind string

const (
	EventSessionReady        EventKind = "session.ready"
	EventSessionReconnecting EventKind = "session.reconnecting"
	EventSessionClosed       EventKind = "session.closed"
	EventSpeechStarted       EventKind = "input.speech_started"
	EventSpeechStopped       EventKind = "input.speech_stopped"
	EventTranscriptUpdated   EventKind = "transcript.updated"
	EventTranscriptFinal     EventKind = "transcript.segment_final"
	EventTurnCommitted       EventKind = "turn.committed"
	EventResponseStarted     EventKind = "response.started"
	EventTextDelta           EventKind = "text.delta"
	EventTextFinal           EventKind = "text.final"
	EventToolArgumentsDelta  EventKind = "tool.arguments_delta"
	EventToolCallReady       EventKind = "tool.call_ready"
	EventResponseTerminal    EventKind = "response.terminal"
	EventSpeechTextCommitted EventKind = "speech.text_committed"
	EventAudioStarted        EventKind = "audio.started"
	EventAudioEnded          EventKind = "audio.ended"
	EventPlaybackStarted     EventKind = "playback.started"
	EventPlaybackProgress    EventKind = "playback.progress"
	EventPlaybackDrained     EventKind = "playback.drained"
	EventPlaybackStopped     EventKind = "playback.stopped"
	EventToolStarted         EventKind = "tool.started"
	EventToolProgress        EventKind = "tool.progress"
	EventToolCompleted       EventKind = "tool.completed"
	EventToolFailed          EventKind = "tool.failed"
	EventTaskUpdated         EventKind = "task.updated"
	EventCancelRequested     EventKind = "cancel.requested"
	EventCancelAcknowledged  EventKind = "cancel.acknowledged"
	EventStreamDiscontinuity EventKind = "stream.discontinuity"
	EventStreamError         EventKind = "stream.error"
	EventUsageUpdated        EventKind = "usage.updated"
	EventRunTerminal         EventKind = "run.terminal"
	EventReasoningDelta      EventKind = "reasoning.delta"
	EventReasoningReset      EventKind = "reasoning.reset"
)

// EventSink accepts validated semantic events. Implementations may apply
// backpressure, but must honor the supplied context while doing so.
type EventSink interface {
	Emit(context.Context, Event) error
}

// Visibility describes which projections may consume an event.
type Visibility string

const (
	VisibilityUser       Visibility = "user"
	VisibilitySpeech     Visibility = "speech"
	VisibilityInternal   Visibility = "internal"
	VisibilityDiagnostic Visibility = "diagnostic"
)

// EventIdentity contains the correlation fields used for ordering, replay,
// cancellation, and stale-output filtering.  Empty optional IDs are valid for
// events that do not belong to that object (for example a session event).
type EventIdentity struct {
	EventID        string `json:"event_id"`
	Sequence       uint64 `json:"sequence"`
	ConversationID string `json:"conversation_id"`
	SessionEpoch   uint64 `json:"session_epoch"`
	TurnID         string `json:"turn_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
	ResponseID     string `json:"response_id,omitempty"`
	AttemptID      string `json:"attempt_id,omitempty"`
	ItemID         string `json:"item_id,omitempty"`
	StreamID       string `json:"stream_id,omitempty"`
	OutputEpoch    uint64 `json:"output_epoch,omitempty"`
	CallID         string `json:"call_id,omitempty"`
	TaskID         string `json:"task_id,omitempty"`
	ParentID       string `json:"parent_id,omitempty"`
}

// EventScope contains the correlation identifiers shared by one run. EventID
// and Sequence are assigned by the producer for each emitted event.
type EventScope struct {
	ConversationID string
	SessionEpoch   uint64
	TurnID         string
	RunID          string
	ResponseID     string
	AttemptID      string
	ItemID         string
	StreamID       string
	OutputEpoch    uint64
	CallID         string
	TaskID         string
	ParentID       string
}

// Identity is a short alias for callers that do not need the Event prefix.
type Identity = EventIdentity

// Validate checks fields that are required for every event.  Optional
// correlation IDs are validated by the component that owns their event kind.
func (i EventIdentity) Validate() error {
	switch {
	case i.EventID == "":
		return errors.New("stream event identity: event_id is required")
	case i.Sequence == 0:
		return errors.New("stream event identity: sequence must be positive")
	case i.ConversationID == "":
		return errors.New("stream event identity: conversation_id is required")
	case i.SessionEpoch == 0:
		return errors.New("stream event identity: session_epoch must be positive")
	default:
		return nil
	}
}

// StaleFor reports whether this identity belongs to an invalidated output
// epoch.  Events without an output epoch (for example input or accounting
// events) are never considered stale by this helper.
func (i EventIdentity) StaleFor(currentOutputEpoch uint64) bool {
	return i.OutputEpoch != 0 && i.OutputEpoch != currentOutputEpoch
}

// Event is the versioned semantic envelope used by streaming projections.
// Payload is one typed value selected by Kind; the envelope does not prescribe
// a serialization format for that value.
type Event struct {
	SchemaVersion uint16 `json:"schema_version"`
	EventIdentity
	Kind       EventKind  `json:"kind"`
	Origin     string     `json:"origin,omitempty"`
	Visibility Visibility `json:"visibility,omitempty"`
	OccurredAt time.Time  `json:"occurred_at,omitempty"`
	ReceivedAt time.Time  `json:"received_at,omitempty"`
	Payload    any        `json:"payload,omitempty"`
}

// NewEvent builds an event with the current schema version.  Callers may
// replace timestamps when replaying a recorded fixture.
func NewEvent(identity EventIdentity, kind EventKind, payload any) Event {
	now := time.Now().UTC()
	return Event{
		SchemaVersion: CurrentSchemaVersion,
		EventIdentity: identity,
		Kind:          kind,
		ReceivedAt:    now,
		Payload:       payload,
	}
}

// Validate checks the envelope's required fields.  OccurredAt is a provider
// timestamp and ReceivedAt is local wall-clock time; their ordering is not
// validated because clocks on different machines are not a duration source.
func (e Event) Validate() error {
	if e.SchemaVersion == 0 {
		return errors.New("stream event: schema_version is required")
	}
	if err := e.EventIdentity.Validate(); err != nil {
		return err
	}
	if e.Kind == "" {
		return errors.New("stream event: kind is required")
	}
	if e.ReceivedAt.IsZero() {
		return errors.New("stream event: received_at is required")
	}
	return nil
}
