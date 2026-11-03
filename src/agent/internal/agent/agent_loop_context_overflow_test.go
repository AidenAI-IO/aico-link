package agent

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/model"

	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
)

type contextOverflowThenSuccessModel struct {
	calls int
}

func (m *contextOverflowThenSuccessModel) GenerateContent(context.Context, []llms.MessageContent, ...llms.CallOption) (*llms.ContentResponse, error) {
	m.calls++
	if m.calls == 1 {
		return nil, newProviderHTTPError(http.StatusBadRequest, []byte(
			`{"error":{"code":"context_length_exceeded","message":"maximum context length exceeded"}}`,
		))
	}
	return contentResponse("Recovered"), nil
}

func (m *contextOverflowThenSuccessModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	return "", errors.New("unexpected Call invocation")
}

func (m *contextOverflowThenSuccessModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "fake", Name: "context-overflow-test", ContextWindow: 8_192}
}

func (m *contextOverflowThenSuccessModel) CallOptions() []chains.ChainCallOption { return nil }

func TestAgentLoopCompactsAndRetriesProviderContextExceededError(t *testing.T) {
	model := &contextOverflowThenSuccessModel{}
	manager, err := freshNewContextManager("system", "continue", nil, t.TempDir())
	if err != nil {
		t.Fatalf("freshNewContextManager() error = %v", err)
	}
	replacement, err := contextmanager.NewContextManagerRevisionFromMessageList(manager, manager.CloneMessageList())
	if err != nil {
		t.Fatalf("NewContextManagerRevisionFromMessageList() error = %v", err)
	}

	loop := NewAgentLoop(
		model,
		RoleProfile{},
		1,
		nil,
		nil,
		manager,
	)
	recoveryCalls := 0
	loop.ContextOverflowRecovery = func(_ context.Context, current *contextmanager.ContextManager) (*contextmanager.ContextManager, bool, error) {
		recoveryCalls++
		if current != manager {
			t.Fatalf("recovery current manager = %p, want %p", current, manager)
		}
		return replacement, true, nil
	}

	output, err := loop.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output != "Recovered" {
		t.Fatalf("output = %q, want Recovered", output)
	}
	if model.calls != 2 || recoveryCalls != 1 {
		t.Fatalf("calls = model:%d recovery:%d, want 2/1", model.calls, recoveryCalls)
	}
	if loop.contextManager != replacement {
		t.Fatal("agent loop did not switch to the compacted context manager")
	}
}

type contextBudgetGuardModel struct {
	calls [][]llms.MessageContent
}

func (m *contextBudgetGuardModel) GenerateContent(_ context.Context, messageList []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	m.calls = append(m.calls, messageList)
	if len(m.calls) == 1 {
		return &llms.ContentResponse{Choices: []*llms.ContentChoice{{
			ToolCalls: []llms.ToolCall{{
				ID:   "call_1",
				Type: "function",
				FunctionCall: &llms.FunctionCall{
					Name:      "echo",
					Arguments: `{"input":"hello"}`,
				},
			}},
		}}}, nil
	}
	return contentResponse("done"), nil
}

func (m *contextBudgetGuardModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	return "", errors.New("unexpected Call invocation")
}

func (m *contextBudgetGuardModel) Spec() model.ModelSpec {
	return model.ModelSpec{Provider: "fake", Name: "context-budget-guard", ContextWindow: 8_192}
}

func (m *contextBudgetGuardModel) CallOptions() []chains.ChainCallOption { return nil }

type contextBudgetGuardTool struct{}

func (contextBudgetGuardTool) Name() string { return "echo" }

func (contextBudgetGuardTool) Description() string { return "Echo the input." }

func (contextBudgetGuardTool) Call(_ context.Context, input string) (string, error) {
	return input, nil
}

func TestAgentLoopChecksContextBudgetBeforeEveryModelRequest(t *testing.T) {
	model := &contextBudgetGuardModel{}
	manager, err := freshNewContextManager("original system", "continue", nil, t.TempDir())
	if err != nil {
		t.Fatalf("freshNewContextManager() error = %v", err)
	}
	loop := NewAgentLoop(
		model,
		RoleProfile{Tools: []langtools.Tool{contextBudgetGuardTool{}}},
		2,
		nil,
		nil,
		manager,
	)
	guardCalls := 0
	var replacement *contextmanager.ContextManager
	loop.ContextBudgetGuard = func(_ context.Context, current *contextmanager.ContextManager, options llms.CallOptions) (*contextmanager.ContextManager, bool, error) {
		guardCalls++
		if len(options.Tools) != 1 {
			t.Fatalf("guard tool schemas = %d, want 1", len(options.Tools))
		}
		if guardCalls == 1 {
			return current, false, nil
		}
		messageList := current.CloneMessageList()
		messageList[0].Content = "replacement system"
		replacement, err = contextmanager.NewContextManagerRevisionFromMessageList(current, messageList)
		return replacement, true, err
	}

	output, err := loop.Run(context.Background(), "continue")
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if output != "done" || guardCalls != 2 || len(model.calls) != 2 {
		t.Fatalf("output/calls = %q/%d/%d, want done/2/2", output, guardCalls, len(model.calls))
	}
	if loop.contextManager != replacement {
		t.Fatal("agent loop did not install the budget guard replacement")
	}
	var secondPrompt strings.Builder
	toolResultSeen := false
	for _, message := range model.calls[1] {
		for _, part := range message.Parts {
			switch part := part.(type) {
			case llms.TextContent:
				secondPrompt.WriteString(part.Text)
			case llms.ToolCallResponse:
				toolResultSeen = true
			}
		}
	}
	if !strings.Contains(secondPrompt.String(), "replacement system") || !toolResultSeen {
		t.Fatalf("second model request did not use replacement context after tool result: prompt=%q toolResult=%v", secondPrompt.String(), toolResultSeen)
	}
	for _, message := range replacement.CloneMessageList() {
		if message.Role == messages.MessageRoleToolResult {
			return
		}
	}
	t.Fatal("replacement context omitted the completed tool result")
}

func TestAgentLoopRechecksBudgetAfterThresholdCompaction(t *testing.T) {
	manager, err := freshNewContextManager("system", "continue", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := contextmanager.NewContextManagerRevisionFromMessageList(manager, manager.CloneMessageList())
	if err != nil {
		t.Fatal(err)
	}
	model := &contextBudgetGuardModel{}
	loop := NewAgentLoop(model, RoleProfile{}, 1, nil, nil, manager)
	var sequence []string
	budgetErr := errors.New("summary exceeds budget")
	loop.ContextBudgetGuard = func(_ context.Context, current *contextmanager.ContextManager, _ llms.CallOptions) (*contextmanager.ContextManager, bool, error) {
		sequence = append(sequence, "guard")
		if current == replacement {
			return nil, false, budgetErr
		}
		return current, false, nil
	}
	loop.ContextCompactionTrigger = 1
	loop.ContextThresholdCompaction = func(_ context.Context, current *contextmanager.ContextManager) (*contextmanager.ContextManager, bool, error) {
		sequence = append(sequence, "summary")
		return replacement, true, nil
	}
	_, err = loop.Run(context.Background(), "continue")
	if !errors.Is(err, budgetErr) {
		t.Fatalf("Run() error = %v, want summary budget error", err)
	}
	if got := strings.Join(sequence, ","); got != "guard,summary,guard" {
		t.Fatalf("preparation order = %q", got)
	}
	if len(model.calls) != 0 {
		t.Fatal("sent an over-budget summary to the model")
	}
}

func TestAgentLoopRejectsFailedBudgetRevision(t *testing.T) {
	manager, err := freshNewContextManager("system", "continue", nil, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	replacement, err := contextmanager.NewContextManagerRevisionFromMessageList(manager, manager.CloneMessageList())
	if err != nil {
		t.Fatal(err)
	}
	model := &contextBudgetGuardModel{}
	loop := NewAgentLoop(model, RoleProfile{}, 1, nil, nil, manager)
	budgetErr := errors.New("cannot fit context")
	loop.ContextBudgetGuard = func(context.Context, *contextmanager.ContextManager, llms.CallOptions) (*contextmanager.ContextManager, bool, error) {
		return replacement, true, budgetErr
	}
	_, err = loop.Run(context.Background(), "continue")
	if !errors.Is(err, budgetErr) || loop.contextManager != manager || len(model.calls) != 0 {
		t.Fatalf("Run() error=%v manager=%p calls=%d, want error with original manager and no request", err, loop.contextManager, len(model.calls))
	}
}
