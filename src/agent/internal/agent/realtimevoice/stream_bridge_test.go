package realtimevoice

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"aico-agent/internal/stream"
)

func bridgeIdentity() stream.EventIdentity {
	return stream.EventIdentity{
		EventID:        "evt-1",
		Sequence:       1,
		ConversationID: "conversation-1",
		SessionEpoch:   2,
		OutputEpoch:    4,
	}
}

func TestBridgeEventMapsRealtimeTranscriptAndIdentity(t *testing.T) {
	event, ok := BridgeEvent(bridgeIdentity(), 4, Event{
		Kind:       EventTranscriptDelta,
		ResponseID: "response-1",
		ItemID:     "item-1",
		Role:       "user",
		Text:       "hello",
	})
	if !ok {
		t.Fatal("BridgeEvent() rejected transcript delta")
	}
	if event.Kind != stream.EventTranscriptUpdated || event.Visibility != stream.VisibilityUser {
		t.Fatalf("event kind/visibility = %q/%q", event.Kind, event.Visibility)
	}
	if event.ResponseID != "response-1" || event.ItemID != "item-1" {
		t.Fatalf("event identity = %+v", event.EventIdentity)
	}
	if err := event.Validate(); err != nil {
		t.Fatalf("event validation failed: %v", err)
	}
}

func TestBridgeEventMapsTerminalAndErrors(t *testing.T) {
	terminal, ok := BridgeEvent(bridgeIdentity(), 4, Event{Kind: EventResponseCancelled, ResponseID: "response-1"})
	if !ok || terminal.Kind != stream.EventResponseTerminal {
		t.Fatalf("terminal = %#v ok=%v", terminal, ok)
	}
	diagnostic, ok := BridgeErrorEvent(bridgeIdentity(), 4, errors.New("socket closed"))
	if !ok || diagnostic.Kind != stream.EventStreamError || diagnostic.Visibility != stream.VisibilityDiagnostic {
		t.Fatalf("diagnostic = %#v ok=%v", diagnostic, ok)
	}
	encoded, err := json.Marshal(diagnostic.Payload)
	if err != nil || !strings.Contains(string(encoded), "realtimevoice: socket closed") {
		t.Fatalf("diagnostic payload = %s, marshal error = %v", encoded, err)
	}
}

func TestBridgeEventDropsStaleOutputAndPCMTransport(t *testing.T) {
	if _, ok := BridgeEvent(bridgeIdentity(), 5, Event{Kind: EventResponseStarted}); ok {
		t.Fatal("stale output epoch was bridged")
	}
	if _, ok := BridgeEvent(bridgeIdentity(), 4, Event{Kind: EventAudio, PCM: []byte{1, 2}}); ok {
		t.Fatal("PCM delta entered semantic event stream")
	}
}
