package main

import (
	"context"
	"testing"

	"aico-agent/internal/agent/realtimevoice"
	streamcontract "aico-agent/internal/stream"
)

type recordingRealtimeSemanticSink struct {
	events []streamcontract.Event
}

func (s *recordingRealtimeSemanticSink) Emit(_ context.Context, event streamcontract.Event) error {
	s.events = append(s.events, event)
	return nil
}

func TestRealtimeSemanticEmitterBridgesAcceptedEventsAndDropsStaleOutput(t *testing.T) {
	sink := &recordingRealtimeSemanticSink{}
	emitter := newRealtimeSemanticEmitter(sink)
	emitter.beginResponse("response-1")
	emitter.emit(realtimevoice.Event{
		Kind:       realtimevoice.EventTranscriptDelta,
		Role:       "assistant",
		ResponseID: "response-1",
		Text:       "hello",
	})
	emitter.invalidateOutput()
	emitter.emit(realtimevoice.Event{
		Kind:       realtimevoice.EventTranscriptDelta,
		Role:       "assistant",
		ResponseID: "response-1",
		Text:       " late",
	})

	if len(sink.events) != 1 {
		t.Fatalf("semantic events = %d, want one accepted event", len(sink.events))
	}
	event := sink.events[0]
	if event.Kind != streamcontract.EventTextDelta || event.ResponseID != "response-1" {
		t.Fatalf("unexpected bridged event: %#v", event)
	}
	if event.OutputEpoch != 1 {
		t.Fatalf("output epoch = %d, want 1", event.OutputEpoch)
	}
}

func TestRealtimeSemanticEmitterKeepsPCMOffSemanticPath(t *testing.T) {
	sink := &recordingRealtimeSemanticSink{}
	emitter := newRealtimeSemanticEmitter(sink)
	emitter.emit(realtimevoice.Event{Kind: realtimevoice.EventAudio, PCM: []byte{1, 2}})
	emitter.emit(realtimevoice.Event{Kind: realtimevoice.EventResponseCancelled, ResponseID: "response-1", Status: "cancelled"})

	if len(sink.events) != 1 || sink.events[0].Kind != streamcontract.EventResponseTerminal {
		t.Fatalf("semantic events = %#v, want only response terminal", sink.events)
	}
}

func TestRealtimeSemanticEmitterBridgesUserTranscriptUpdates(t *testing.T) {
	sink := &recordingRealtimeSemanticSink{}
	emitter := newRealtimeSemanticEmitter(sink)
	emitter.emit(realtimevoice.Event{
		Kind: realtimevoice.EventTranscriptDelta,
		Role: "user",
		Text: "hel",
	})

	if len(sink.events) != 1 {
		t.Fatalf("semantic events = %d, want one user transcript update", len(sink.events))
	}
	if sink.events[0].Kind != streamcontract.EventTranscriptUpdated || sink.events[0].OutputEpoch != 0 {
		t.Fatalf("unexpected user transcript event: %#v", sink.events[0])
	}
}
