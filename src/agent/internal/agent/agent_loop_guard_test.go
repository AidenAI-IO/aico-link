package agent

import (
	"context"
	"strings"
	"testing"

	"aiden-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
)

type loopGuardEchoTool struct {
	output string
}

func (t *loopGuardEchoTool) Name() string        { return "touch_gesture" }
func (t *loopGuardEchoTool) Description() string { return "echo gesture" }
func (t *loopGuardEchoTool) Call(context.Context, string) (string, error) {
	return t.output, nil
}

type loopToolExecutionHookRecorder struct {
	beforeCalls     int
	afterCalls      int
	afterSawSummary bool
}

func (r *loopToolExecutionHookRecorder) BeforeToolCall(context.Context, ToolCall) (ToolResult, bool) {
	r.beforeCalls++
	return ToolResult{}, true
}

func (r *loopToolExecutionHookRecorder) AfterToolCall(_ context.Context, _ ToolCall, result ToolResult) ToolResult {
	r.afterCalls++
	r.afterSawSummary = result.Summary != ""
	return result
}

func TestAgentLoopCombinesTerminationPolicyWithToolExecutionHooks(t *testing.T) {
	t.Parallel()

	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolCallResponse("call-1", "touch_gesture", `{"type":"tap"}`),
		contentResponse("Done"),
	}}
	manager, err := freshNewContextManager("system", "tap once", nil, t.TempDir())
	if err != nil {
		t.Fatalf("freshNewContextManager() error = %v", err)
	}
	recorder := &loopToolExecutionHookRecorder{}
	factoryCalls := 0
	loop := NewAgentLoop(
		model,
		RoleProfile{Tools: []langtools.Tool{&loopGuardEchoTool{output: "ok"}}},
		10,
		nil,
		nil,
		manager,
	)
	loop.toolExecutionHookFactory = func() toolExecutionHookHandler {
		factoryCalls++
		return recorder
	}
	loop.TerminationPolicy = NewTerminationPolicy(TerminationPolicyConfig{
		RepeatActionLimit:       100,
		SameResultLimit:         100,
		ScreenUnchangedLimit:    100,
		SoftNoticeStallScore:    100,
		RestrictToolsStallScore: 100,
		TerminateStallScore:     100,
	})

	output, err := loop.Run(context.Background(), "tap once")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output != "Done" {
		t.Fatalf("output = %q, want Done", output)
	}
	if factoryCalls != 1 || recorder.beforeCalls != 1 || recorder.afterCalls != 1 {
		t.Fatalf("hook calls = factory:%d before:%d after:%d, want 1/1/1", factoryCalls, recorder.beforeCalls, recorder.afterCalls)
	}
	if !recorder.afterSawSummary {
		t.Fatal("custom after hook should observe DefaultAfterToolCall normalization")
	}
}

func TestAgentLoopPersistsSoftNoticeInContext(t *testing.T) {
	t.Parallel()

	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolCallResponse("call-1", "touch_gesture", `{"type":"swipe","start":{"x":500,"y":800},"direction":"up"}`),
		toolCallResponse("call-2", "touch_gesture", `{"type":"swipe","start":{"x":500,"y":800},"direction":"up"}`),
		contentResponse("I will change strategy."),
	}}
	manager, err := freshNewContextManager("system", "swipe until done", nil, t.TempDir())
	if err != nil {
		t.Fatalf("freshNewContextManager() error = %v", err)
	}
	loop := NewAgentLoop(
		model,
		RoleProfile{Tools: []langtools.Tool{&loopGuardEchoTool{output: "same"}}},
		10,
		nil,
		nil,
		manager,
	)
	loop.TerminationPolicy = NewTerminationPolicy(TerminationPolicyConfig{
		RepeatActionLimit:       100,
		SameResultLimit:         100,
		ScreenUnchangedLimit:    100,
		SoftNoticeStallScore:    1,
		RestrictToolsStallScore: 100,
		TerminateStallScore:     100,
	})

	output, err := loop.Run(context.Background(), "swipe until done")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output != "I will change strategy." {
		t.Fatalf("output = %q", output)
	}
	if got := countLoopGuardNotices(model.messages[1]); got != 1 {
		t.Fatalf("soft notice count in deciding prompt = %d, want 1", got)
	}
	notices := collectMessagesByRole(manager.MessageListDump().Messages, messages.MessageRoleNotice)
	if len(notices) != 1 {
		t.Fatalf("persisted notice count = %d, want 1; messages=%#v", len(notices), manager.MessageListDump().Messages)
	}
	if strings.Contains(notices[0].Content, "<notice>") || !strings.Contains(notices[0].Content, "Loop guard: recent tool calls are repeating or not changing the screen.") {
		t.Fatalf("persisted notice = %#v", notices[0])
	}
}

func TestAgentLoopEscalatesFromPersistedNoticeToRestrictionAndTermination(t *testing.T) {
	t.Parallel()

	screen := `{"width":100,"height":100,"format":"jpeg","data":"same-screen"}`
	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolCallResponse("call-1", "touch_gesture", `{"type":"swipe","start":{"x":500,"y":800},"direction":"up"}`),
		toolCallResponse("call-2", "touch_gesture", `{"type":"swipe","start":{"x":500,"y":800},"direction":"up"}`),
		toolCallResponse("call-3", "touch_gesture", `{"type":"swipe","start":{"x":500,"y":800},"direction":"up"}`),
		toolCallResponse("call-4", "touch_gesture", `{"type":"swipe","start":{"x":500,"y":800},"direction":"up"}`),
	}}
	manager, err := freshNewContextManager("system", "swipe until done", nil, t.TempDir())
	if err != nil {
		t.Fatalf("freshNewContextManager() error = %v", err)
	}
	loop := NewAgentLoop(
		model,
		RoleProfile{Tools: []langtools.Tool{&loopGuardEchoTool{output: screen}}},
		10,
		nil,
		nil,
		manager,
	)
	loop.TerminationPolicy = NewTerminationPolicy(DefaultTerminationPolicyConfig())

	output, err := loop.Run(context.Background(), "swipe until done")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(output, "not making measurable progress") {
		t.Fatalf("output = %q, want loop-guard stop message", output)
	}
	if model.callCount != 3 {
		t.Fatalf("model call count = %d, want termination before fourth response", model.callCount)
	}
	if got := countLoopGuardNotices(model.messages[2]); got != 1 {
		t.Fatalf("restriction notice count in deciding prompt = %d, want 1", got)
	}
	notices := collectMessagesByRole(manager.MessageListDump().Messages, messages.MessageRoleNotice)
	if len(notices) != 2 {
		t.Fatalf("persisted notices = %d, want restriction and termination notices", len(notices))
	}
	if !strings.HasPrefix(notices[1].Content, "Interrupt [loop_detected]:") {
		t.Fatalf("termination notice = %q", notices[1].Content)
	}
	if strings.Contains(notices[0].Content, "<notice>") || !strings.Contains(notices[0].Content, "Loop guard: UI action tools are temporarily restricted because repeated actions produced no progress.") {
		t.Fatalf("persisted notice = %#v", notices[0])
	}
}

func TestAgentLoopBudgetExhaustionReturnsGracefulStop(t *testing.T) {
	t.Parallel()

	model := &scriptedModel{responses: []*llms.ContentResponse{
		toolCallResponse("call-1", "touch_gesture", `{"type":"swipe","start":{"x":500,"y":800},"direction":"up"}`),
		toolCallResponse("call-2", "touch_gesture", `{"type":"swipe","start":{"x":500,"y":800},"direction":"up"}`),
	}}
	screen := `{"width":100,"height":100,"format":"jpeg","data":"screen-1"}`
	manager, err := freshNewContextManager("system", "keep going", nil, t.TempDir())
	if err != nil {
		t.Fatalf("freshNewContextManager() error = %v", err)
	}

	loop := NewAgentLoop(
		model,
		RoleProfile{Tools: []langtools.Tool{&loopGuardEchoTool{output: screen}}},
		1,
		nil,
		nil,
		manager,
	)
	loop.TerminationPolicy = NewTerminationPolicy(TerminationPolicyConfig{
		RepeatActionLimit: 99,
		SameResultLimit:   99,
	})

	output, err := loop.Run(context.Background(), "keep going")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if !strings.Contains(output, "budget_exceeded") {
		t.Fatalf("output = %q, want budget exhaustion message", output)
	}
}

func TestAgentLoopReturnsContextErrorBeforeGracefulStop(t *testing.T) {
	t.Parallel()

	model := &scriptedModel{responses: roleDirectResponses("should not be called")}
	manager, err := freshNewContextManager("system", "stop", nil, t.TempDir())
	if err != nil {
		t.Fatalf("freshNewContextManager() error = %v", err)
	}
	loop := NewAgentLoop(
		model,
		RoleProfile{},
		10,
		nil,
		nil,
		manager,
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	output, err := loop.Run(ctx, "stop")
	if err != context.Canceled {
		t.Fatalf("Run() error = %v, want context.Canceled", err)
	}
	if output != "" {
		t.Fatalf("output = %q, want empty output", output)
	}
	if model.callCount != 0 {
		t.Fatalf("model call count = %d, want 0", model.callCount)
	}
}

func countLoopGuardNotices(messages []llms.MessageContent) int {
	var notices int
	for _, message := range messages {
		for _, part := range message.Parts {
			text, ok := part.(llms.TextContent)
			if ok && strings.Contains(text.Text, "Loop guard:") {
				notices++
			}
		}
	}
	return notices
}

func collectMessagesByRole(messageList []messages.Message, role messages.MessageRole) []messages.Message {
	var result []messages.Message
	for _, message := range messageList {
		if message.Role == role {
			result = append(result, message)
		}
	}
	return result
}
