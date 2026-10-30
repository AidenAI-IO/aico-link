package agent

import (
	"context"
	"strings"
	"testing"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
)

func TestAgentLoopPreservesStreamedToolCallTextInContextAndHistory(t *testing.T) {
	const preamble = "<tts>我来检查一下。</tts>\nChecking the requested information."
	const arguments = `{"input":"hello"}`
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponseWithContent("call_1", "echo", arguments, preamble),
			contentResponse("Done"),
		},
		streamChunks: [][]string{
			{"<tt", "s>我来检查一下。</tts>\n", "Checking the requested information."},
			{"Done"},
		},
	}
	sessionFolder := t.TempDir()
	manager, err := freshNewContextManager("system", "run the tool", nil, sessionFolder)
	if err != nil {
		t.Fatal(err)
	}
	var events []RunEvent
	handler := &runtimeCallbackHandler{eventHandler: func(event RunEvent) {
		events = append(events, event)
	}}
	loop := NewAgentLoop(model, RoleProfile{Tools: []langtools.Tool{
		&stubTool{name: "echo", output: "hello"},
	}}, 2, handler, nil, manager)
	var streamed strings.Builder
	answer, err := loop.Run(context.Background(), "run the tool", chains.WithStreamingFunc(func(_ context.Context, chunk []byte) error {
		streamed.Write(chunk)
		return nil
	}))
	if err != nil || answer != "Done" {
		t.Fatalf("Run() = %q, %v", answer, err)
	}
	if streamed.String() != preamble+"Done" {
		t.Fatalf("streamed text = %q", streamed.String())
	}
	toolEvents := 0
	for _, event := range events {
		if event.Type == runEventToolCall {
			toolEvents++
			if event.Content != preamble || event.ToolCallID != "call_1" {
				t.Fatalf("completed tool event lost its text: %#v", event)
			}
		}
	}
	if toolEvents != 1 {
		t.Fatalf("tool events = %d, want 1", toolEvents)
	}

	// The next provider request must retain the preamble on the same assistant
	// message as its native tool call, including the speech tags.
	if len(model.messages) != 2 {
		t.Fatalf("model calls = %d, want 2", len(model.messages))
	}
	found := false
	for _, message := range model.messages[1] {
		wire, err := convertMessageContent(message, false)
		if err != nil {
			t.Fatal(err)
		}
		if len(wire.ToolCalls) == 0 {
			continue
		}
		found = true
		if wire.Role != "assistant" || wire.Content != preamble || len(wire.ToolCalls) != 1 ||
			wire.ToolCalls[0].ID != "call_1" || wire.ToolCalls[0].Function.Arguments != arguments {
			t.Fatalf("next provider request lost text or tool call: %#v", wire)
		}
	}
	if !found {
		t.Fatal("next provider request did not contain the tool-call message")
	}

	// Reload from disk instead of inspecting only the in-memory context.
	reloaded, err := contextmanager.LoadContextManagerFromSessionID(sessionFolder, manager.GetSessionID())
	if err != nil {
		t.Fatal(err)
	}
	found = false
	for _, message := range reloaded.CloneMessageList() {
		if message.Role != messages.MessageRoleToolCall {
			continue
		}
		found = true
		if message.Content != preamble || len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "call_1" {
			t.Fatalf("persisted tool-call message lost text: %#v", message)
		}
		history, ok := webMessageFromContextMessage(message, "backend")
		if !ok || history.Type != "tool_call" || history.Content != preamble || history.ToolInput != arguments {
			t.Fatalf("Web history lost tool-call text: %#v", history)
		}
	}
	if !found {
		t.Fatal("reloaded context did not contain the tool-call message")
	}
}
