package realtimevoice

import (
	"fmt"

	"aico-agent/internal/stream"
)

// BridgeEvent converts one normalized realtime provider event into the shared
// semantic event envelope. PCM deltas are intentionally omitted: they belong
// to the binary AudioFrame path, whose negotiated format and frame sequence
// are owned by the session/media controller.
func BridgeEvent(identity stream.EventIdentity, currentOutputEpoch uint64, source Event) (stream.Event, bool) {
	if identity.OutputEpoch != 0 && identity.OutputEpoch != currentOutputEpoch {
		return stream.Event{}, false
	}
	kind, visibility, ok := bridgeEventKind(source.Kind, source.Role)
	if !ok {
		return stream.Event{}, false
	}
	if source.ResponseID != "" {
		identity.ResponseID = source.ResponseID
	}
	if source.ItemID != "" {
		identity.ItemID = source.ItemID
	}
	if source.CallID != "" {
		identity.CallID = source.CallID
	}
	payload := bridgePayload(source)
	event := stream.NewEvent(identity, kind, payload)
	event.Origin = "realtimevoice"
	event.Visibility = visibility
	if err := event.Validate(); err != nil {
		return stream.Event{}, false
	}
	return event, true
}

// bridgePayload keeps transport errors useful to JSON observers. The native
// Event type stores errors behind the error interface, which encoding/json
// otherwise serializes as an empty object and loses the message.
func bridgePayload(source Event) any {
	if source.Error == nil {
		payload := source
		payload.PCM = append([]byte(nil), source.PCM...)
		return payload
	}
	return map[string]any{
		"error":       source.Error.Error(),
		"status":      source.Status,
		"response_id": source.ResponseID,
		"item_id":     source.ItemID,
		"call_id":     source.CallID,
		"text":        source.Text,
	}
}

func bridgeEventKind(kind EventKind, role string) (stream.EventKind, stream.Visibility, bool) {
	switch kind {
	case EventReady:
		return stream.EventSessionReady, stream.VisibilityInternal, true
	case EventSpeechStarted:
		return stream.EventSpeechStarted, stream.VisibilityInternal, true
	case EventSpeechStopped:
		return stream.EventSpeechStopped, stream.VisibilityInternal, true
	case EventInputCommitted:
		return stream.EventTurnCommitted, stream.VisibilityInternal, true
	case EventTranscriptDelta:
		if role == "assistant" {
			return stream.EventTextDelta, stream.VisibilityUser, true
		}
		return stream.EventTranscriptUpdated, stream.VisibilityUser, true
	case EventTranscriptFinal:
		if role == "assistant" {
			return stream.EventTextFinal, stream.VisibilityUser, true
		}
		return stream.EventTranscriptFinal, stream.VisibilityUser, true
	case EventTranscriptFailed:
		return stream.EventStreamError, stream.VisibilityDiagnostic, true
	case EventResponseStarted:
		return stream.EventResponseStarted, stream.VisibilityInternal, true
	case EventResponseDone:
		return stream.EventResponseTerminal, stream.VisibilityInternal, true
	case EventResponseCancelled:
		return stream.EventResponseTerminal, stream.VisibilityInternal, true
	case EventInterruption:
		return stream.EventStreamDiscontinuity, stream.VisibilityInternal, true
	case EventToolCall:
		return stream.EventToolCallReady, stream.VisibilityInternal, true
	case EventToolCallCancelled:
		return stream.EventToolFailed, stream.VisibilityInternal, true
	case EventUsage:
		return stream.EventUsageUpdated, stream.VisibilityInternal, true
	case EventError:
		return stream.EventStreamError, stream.VisibilityDiagnostic, true
	case EventClosed:
		return stream.EventSessionClosed, stream.VisibilityInternal, true
	case EventAudio:
		// Audio bytes require negotiated format and frame sequencing; the
		// semantic event stream must not pretend to be the PCM transport.
		return "", "", false
	default:
		return "", "", false
	}
}

// BridgeErrorEvent creates a diagnostic stream event for a transport error
// when no normalized provider Event exists yet.
func BridgeErrorEvent(identity stream.EventIdentity, currentOutputEpoch uint64, err error) (stream.Event, bool) {
	if err == nil {
		return stream.Event{}, false
	}
	return BridgeEvent(identity, currentOutputEpoch, Event{Kind: EventError, Error: fmt.Errorf("realtimevoice: %w", err)})
}
