package agent

import (
	"context"
	"errors"
	"testing"

	"aico-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/agents"
	"github.com/tmc/langchaingo/llms"
)

func reasoningOnlyResponse(stopReason string) *llms.ContentResponse {
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{
		ReasoningContent: "weighing every candidate move before acting",
		StopReason:       stopReason,
	}}}
}

// requireNoEmptyAssistantTurns fails when history holds an assistant turn that
// a provider would receive with neither content nor tool calls.
func requireNoEmptyAssistantTurns(t *testing.T, input []llms.MessageContent) {
	t.Helper()
	for i, message := range input {
		if message.Role == llms.ChatMessageTypeAI && len(message.Parts) == 0 {
			t.Fatalf("message %d is an assistant turn without content or tool calls", i)
		}
	}
}

func TestAgentLoopTruncatedReasoningStopsWithNoticeAndKeepsHistoryReplayable(t *testing.T) {
	for _, stopReason := range []string{"length", "max_tokens", "incomplete"} {
		t.Run(stopReason, func(t *testing.T) {
			calls := 0
			model := &interruptTestModel{generate: func(_ context.Context, input []llms.MessageContent) (*llms.ContentResponse, error) {
				calls++
				requireNoEmptyAssistantTurns(t, input)
				if calls == 1 {
					return reasoningOnlyResponse(stopReason), nil
				}
				if !runtimeModelCallContains(input, "Interrupt [output_truncated]") {
					t.Fatal("follow-up model call did not receive the truncation notice")
				}
				return contentResponse("resumed"), nil
			}}
			loop := newInterruptTestLoop(t, model)

			_, err := loop.Run(context.Background(), "task")
			if !errors.Is(err, errModelOutputTruncated) {
				t.Fatalf("error = %v, want errModelOutputTruncated", err)
			}
			requireInterruptNotices(t, loop.contextManager, "output_truncated")
			for _, message := range loop.contextManager.CloneMessageList() {
				if message.Role == messages.MessageRoleAssistant {
					t.Fatalf("truncated turn was persisted: %+v", message)
				}
			}

			answer, err := loop.Run(context.Background(), "continue")
			if err != nil {
				t.Fatalf("follow-up run failed: %v", err)
			}
			if answer != "resumed" {
				t.Fatalf("answer = %q, want resumed", answer)
			}
		})
	}
}

func TestAgentLoopEmptyTurnIsNotPersisted(t *testing.T) {
	model := &interruptTestModel{generate: func(context.Context, []llms.MessageContent) (*llms.ContentResponse, error) {
		return reasoningOnlyResponse("stop"), nil
	}}
	loop := newInterruptTestLoop(t, model)

	if _, err := loop.Run(context.Background(), "task"); !errors.Is(err, agents.ErrAgentNoReturn) {
		t.Fatalf("error = %v, want ErrAgentNoReturn", err)
	}
	requireInterruptNotices(t, loop.contextManager, "execution_error")
	requireNoEmptyAssistantTurns(t, messages.ConvertMessageList(loop.contextManager.CloneMessageList()))
}
