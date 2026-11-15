package agent

import (
	"context"
	"testing"
	"time"

	"aico-agent/internal/stream"
)

func TestBeginChatRunOwnsRequestAndSteerLifecycle(t *testing.T) {
	server := &Server{}
	req := ChatRequest{RequestID: "request-1"}
	input := TurnInput{InputText: "hello"}
	user := Message{Type: "user", RequestID: req.RequestID, Content: input.InputText}

	run, ok := server.beginChatRun(req, input, user)
	if !ok || run == nil {
		t.Fatal("beginChatRun rejected the first request")
	}
	if _, ok := server.beginChatRun(req, input, user); ok {
		t.Fatal("beginChatRun accepted a duplicate active request")
	}
	if _, ok := server.setPendingSteer(req.RequestID, "change direction"); !ok {
		t.Fatal("steer was not accepted immediately after registration")
	}
	select {
	case <-run.steerInterrupt():
	default:
		t.Fatal("pending steer did not signal the run")
	}
	if steer, ok := run.steerProvider(run.ctx); !ok || steer.Content != "change direction" {
		t.Fatalf("steer callback returned %+v, %t", steer, ok)
	}
	select {
	case <-run.steerInterrupt():
		t.Fatal("steer interrupt remained signaled after consumption")
	default:
	}

	if _, ok := server.setPendingSteer(req.RequestID, "last word"); !ok {
		t.Fatal("steer was refused before the final check")
	}
	if steer, ok := run.finalSteerProvider(run.ctx); !ok || steer.Content != "last word" {
		t.Fatalf("final steer callback returned %+v, %t", steer, ok)
	}
	if _, ok := run.finalSteerProvider(run.ctx); ok {
		t.Fatal("final steer callback returned a steer twice")
	}
	if _, ok := server.setPendingSteer(req.RequestID, "too late"); ok {
		t.Fatal("steer was accepted after the run's final check")
	}

	run.cleanup()
	run.cleanup()
	if run.ctx.Err() != context.Canceled {
		t.Fatalf("run context error = %v, want canceled", run.ctx.Err())
	}
	if _, ok := server.steerSignals[req.RequestID]; ok {
		t.Fatal("cleanup retained the steer signal")
	}
	reused, ok := server.beginChatRun(req, input, user)
	if !ok {
		t.Fatal("request ID could not be reused after cleanup")
	}
	if _, ok := server.setPendingSteer(req.RequestID, "fresh run"); !ok {
		t.Fatal("a reused request ID kept the previous run's closed steer window")
	}
	reused.cleanup()
}

func TestNewChatRunRequestKeepsSharedEventIdentity(t *testing.T) {
	server := &Server{semanticEvents: newSemanticEventBroadcaster(8)}
	run, ok := server.beginChatRun(ChatRequest{RequestID: "request-2"}, TurnInput{InputText: "hello"}, Message{Type: "user"})
	if !ok {
		t.Fatal("beginChatRun failed")
	}
	defer run.cleanup()
	handler := func(RunEvent) {}
	req := server.newChatRunRequest(run, TurnInput{InputText: "hello"}, "episode-2", "request-2", handler)
	if req.Input != "hello" || req.EpisodeID != "episode-2" || req.RequestID != "request-2" {
		t.Fatalf("run request = %+v", req)
	}
	if req.StreamSink != server.semanticEvents || req.StreamScope != (stream.EventScope{ConversationID: "agent", SessionEpoch: 1, TurnID: "request-2", RunID: "request-2"}) {
		t.Fatalf("stream setup = sink %T, scope %+v", req.StreamSink, req.StreamScope)
	}
	if req.EventHandler == nil || req.SteerProvider == nil || req.FinalSteerProvider == nil || req.SteerInterrupt == nil || !req.AsyncEpisodeMaintenance {
		t.Fatal("run callbacks or maintenance setting missing")
	}
}

func TestHandleChatRunEventProjectsReasoningAndAssistant(t *testing.T) {
	server := &Server{}
	var reasoning []RunEvent
	var messages []Message
	deliverReasoning := func(event RunEvent) { reasoning = append(reasoning, event) }
	deliverMessage := func(message Message) { messages = append(messages, message) }
	server.handleChatRunEvent(context.Background(), RunEvent{
		Type: runEventReasoningDelta, ReasoningContent: "draft",
	}, "episode-3", "request-3", nil, deliverReasoning, deliverMessage)
	server.handleChatRunEvent(context.Background(), RunEvent{
		Type: "assistant_output", Content: "answer", Timestamp: time.Now(),
	}, "episode-3", "request-3", nil, deliverReasoning, deliverMessage)
	if len(reasoning) != 1 || len(messages) != 1 {
		t.Fatalf("reasoning=%d messages=%d, want 1 each", len(reasoning), len(messages))
	}
	if messages[0].Type != "assistant" || messages[0].Content != "answer" || messages[0].EpisodeID != "episode-3" || messages[0].RequestID != "request-3" {
		t.Fatalf("assistant projection = %+v", messages[0])
	}
}
