package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"aico-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/agents"
	"github.com/tmc/langchaingo/callbacks"
	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
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

func taggedReasoningOnlyResponse(stopReason string) *llms.ContentResponse {
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{
		Content:    "<think>weighing every candidate move before acting</think>",
		StopReason: stopReason,
	}}}
}

func unclosedTaggedReasoningResponse(stopReason string) *llms.ContentResponse {
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{
		Content:    "<think>weighing every candidate move before",
		StopReason: stopReason,
	}}}
}

// An unclosed thinking block is kept as the answer when the model did not hit
// its output limit, matching normalizeTaggedThinkingText.
func TestAgentLoopUnclosedThinkingWithoutTruncationIsAnswer(t *testing.T) {
	model := &interruptTestModel{generate: func(context.Context, []llms.MessageContent) (*llms.ContentResponse, error) {
		return unclosedTaggedReasoningResponse("stop"), nil
	}}
	loop := newInterruptTestLoop(t, model)

	answer, err := loop.Run(context.Background(), "task")
	if err != nil {
		t.Fatalf("run failed: %v", err)
	}
	if answer != "<think>weighing every candidate move before" {
		t.Fatalf("answer = %q, want the unclosed block kept visible", answer)
	}
}

func TestAgentLoopTruncatedReasoningStopsWithNoticeAndKeepsHistoryReplayable(t *testing.T) {
	cases := []struct {
		name     string
		response func(string) *llms.ContentResponse
		reason   string
	}{
		{"length", reasoningOnlyResponse, "length"},
		{"max_tokens", reasoningOnlyResponse, "max_tokens"},
		{"incomplete", reasoningOnlyResponse, "incomplete"},
		{"tagged_reasoning_length", taggedReasoningOnlyResponse, "length"},
		{"unclosed_tagged_reasoning_length", unclosedTaggedReasoningResponse, "length"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			model := &interruptTestModel{generate: func(_ context.Context, input []llms.MessageContent) (*llms.ContentResponse, error) {
				calls++
				requireNoEmptyAssistantTurns(t, input)
				if calls == 1 {
					return tc.response(tc.reason), nil
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

// An empty stop turn is retried once with a notice; a second consecutive empty
// turn fails the run with an error that names the provider's report of it.
func TestAgentLoopRetriesEmptyTurnOnceAndNeverPersistsIt(t *testing.T) {
	cases := []struct {
		name       string
		emptyTurns int
		wantAnswer string
	}{
		{"recovers_after_one_empty_turn", 1, "done"},
		{"fails_after_two_empty_turns", 2, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			model := &interruptTestModel{generate: func(_ context.Context, input []llms.MessageContent) (*llms.ContentResponse, error) {
				calls++
				requireNoEmptyAssistantTurns(t, input)
				if calls > 1 && !runtimeModelCallContains(input, emptyModelTurnRetryNotice) {
					t.Fatal("retry did not receive the empty-turn notice")
				}
				if calls <= tc.emptyTurns {
					response := reasoningOnlyResponse("stop")
					response.Choices[0].GenerationInfo = map[string]any{"completion_tokens": 128}
					return response, nil
				}
				return contentResponse("done"), nil
			}}
			loop := newInterruptTestLoop(t, model)

			answer, err := loop.Run(context.Background(), "task")
			if tc.wantAnswer != "" {
				if err != nil || answer != tc.wantAnswer {
					t.Fatalf("Run() = %q, %v; want %q", answer, err, tc.wantAnswer)
				}
			} else {
				if !errors.Is(err, agents.ErrAgentNoReturn) {
					t.Fatalf("error = %v, want ErrAgentNoReturn", err)
				}
				for _, want := range []string{"empty turn", `finish_reason="stop"`, "completion_tokens=128"} {
					if !strings.Contains(err.Error(), want) {
						t.Fatalf("error = %q, want it to contain %q", err, want)
					}
				}
				requireInterruptNotices(t, loop.contextManager, "execution_error")
			}
			// Both cases end on the second call: a recovery or the failed retry.
			if calls != 2 {
				t.Fatalf("model calls = %d, want 2", calls)
			}
			requireNoEmptyAssistantTurns(t, messages.ConvertMessageList(loop.contextManager.CloneMessageList()))
		})
	}
}

func TestWritesToolCallAsText(t *testing.T) {
	offered := []langtools.Tool{&staticTool{name: "bridge_calendar"}, &staticTool{name: "shell"}, &staticTool{name: "request_user_action"}}
	cases := []struct {
		content string
		want    bool
	}{
		// Final answers that gemini-3.5-flash-lite produced in benchmark runs.
		{"<tts>Checking the calendar.</tts><call>default_api:bridge_calendar{action:query,from:2026-07-25T00:00:00+08:00}</call>", true},
		{"<tts>Writing text.</tts><router-call:default_api:bridge_clipboard{action:write,text:Cedar room 619}/>", true},
		{"<tts>Opening settings</tts><call:default_api:shell{command:device-cli tap 174 701}", true},
		{"Manual action is needed.\nrequest_user_action(reason='permission_confirmation', details='Calendar access is required.')", true},
		{"<memo>call:default_api.recall_memory{entities:[delivery]}</memo>", true},
		{"bridge_calendar {action: query}", true},
		{"Your calendar has two events: Standup at 09:30 and Product review at 14:00.", false},
		{"I used bridge_calendar to check June 2; there are no events.", false},
		{"Call `bridge_calendar(action=query)` to list events.", false},
		{"Example: `default_api:shell{command:pwd}`", false},
		{"Run `echo shell(x)` in a terminal.", false},
		{"Example:\n```\nshell(command=\"pwd\")\n```", false},
		{"See `shell` docs.\nshell(command=\"pwd\")", true},
		{"The subshell{} syntax groups commands.", false},
		{"<tts>Locating search.</tts><parcel_point>{\"x\": 934, \"y\": 77}</parcel_point>", false},
	}
	for _, tc := range cases {
		if got := writesToolCallAsText(tc.content, offered); got != tc.want {
			t.Errorf("writesToolCallAsText(%q) = %v, want %v", tc.content, got, tc.want)
		}
	}
}

// A final answer that writes a tool call as text is retried once with a notice
// and kept out of history; a second consecutive one is accepted as the answer.
func TestAgentLoopRetriesToolCallWrittenAsTextOnce(t *testing.T) {
	const textCall = "<tts>Checking.</tts><call>default_api:bridge_calendar{action:query}</call>"
	cases := []struct {
		name       string
		textTurns  int
		wantAnswer string
	}{
		{"recovers_after_one_text_call", 1, "done"},
		{"accepts_second_consecutive_text_call", 2, textCall},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			model := &interruptTestModel{generate: func(_ context.Context, input []llms.MessageContent) (*llms.ContentResponse, error) {
				calls++
				if calls > 1 {
					if !runtimeModelCallContains(input, textToolCallRetryNotice) {
						t.Fatal("retry did not receive the text tool call notice")
					}
					if runtimeModelCallContains(input, "default_api:") {
						t.Fatal("the text tool call was replayed to the model")
					}
				}
				if calls <= tc.textTurns {
					return contentResponse(textCall), nil
				}
				return contentResponse("done"), nil
			}}
			loop := newInterruptTestLoop(t, model, &staticTool{name: "bridge_calendar", output: "[]"})

			answer, err := loop.Run(context.Background(), "task")
			if err != nil || answer != tc.wantAnswer {
				t.Fatalf("Run() = %q, %v; want %q", answer, err, tc.wantAnswer)
			}
			if calls != 2 {
				t.Fatalf("model calls = %d, want 2", calls)
			}
		})
	}
}

type roleOutputRecorder struct {
	callbacks.SimpleHandler
	contents []string
}

func (r *roleOutputRecorder) HandleRoleOutput(_ context.Context, _ string, content, _ string) {
	r.contents = append(r.contents, content)
}

// Truncated thinking must not reach role output as visible text, and visible
// text before an unclosed block is kept as the answer.
func TestAgentLoopTruncatedThinkingIsNotPublishedAsOutput(t *testing.T) {
	cases := []struct {
		name       string
		content    string
		wantOutput []string
		wantAnswer string
		wantErr    error
	}{
		{"thinking_only", "<think>weighing every candidate move before", nil, "", errModelOutputTruncated},
		{"text_then_thinking", "Done. <think>checking once more before", []string{"Done."}, "Done.", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model := &interruptTestModel{generate: func(context.Context, []llms.MessageContent) (*llms.ContentResponse, error) {
				return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: tc.content, StopReason: "length"}}}, nil
			}}
			loop := newInterruptTestLoop(t, model)
			recorder := &roleOutputRecorder{}
			loop.CallbacksHandler = recorder

			answer, err := loop.Run(context.Background(), "task")
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
			} else if err != nil {
				t.Fatalf("run failed: %v", err)
			}
			if answer != tc.wantAnswer {
				t.Fatalf("answer = %q, want %q", answer, tc.wantAnswer)
			}
			for _, content := range recorder.contents {
				if content != "" && (len(tc.wantOutput) == 0 || content != tc.wantOutput[0]) {
					t.Fatalf("role output = %q, want %q", recorder.contents, tc.wantOutput)
				}
			}
		})
	}
}
