package compactor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/executor"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/model"
	"aico-agent/internal/agent/tokencounter"

	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
)

type testModel struct {
	llms.Model
	model.ModelSpec
}

func (m *testModel) CallOptions() []chains.ChainCallOption { return nil }

func (m *testModel) Spec() model.ModelSpec { return m.ModelSpec }

type promptCapturingModel struct {
	prompts []string
	reply   string
}

type failingSummaryModel struct {
	err error
}

func (m failingSummaryModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	return "", m.err
}

func (m failingSummaryModel) GenerateContent(context.Context, []llms.MessageContent, ...llms.CallOption) (*llms.ContentResponse, error) {
	return nil, m.err
}

func (m *promptCapturingModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	panic("unexpected Call invocation")
}

func (m *promptCapturingModel) GenerateContent(_ context.Context, messages []llms.MessageContent, _ ...llms.CallOption) (*llms.ContentResponse, error) {
	var prompt strings.Builder
	for _, message := range messages {
		for _, part := range message.Parts {
			text, ok := part.(llms.TextContent)
			if !ok {
				continue
			}
			prompt.WriteString(text.Text)
		}
	}
	m.prompts = append(m.prompts, prompt.String())
	return &llms.ContentResponse{
		Choices: []*llms.ContentChoice{{
			Content: m.reply,
		}},
	}, nil
}

func TestGenerateSummaryIncludesToolPayloads(t *testing.T) {
	model := &promptCapturingModel{reply: "summary"}
	compactor := NewCompactor(DefaultProtectRule, &testModel{Model: model})

	got, err := compactor.generateSummary(context.Background(), []messages.Message{
		{
			Role: messages.MessageRoleToolCall,
			ToolCalls: []messages.ToolCall{{
				ID:        "call_1",
				Name:      "echo",
				Arguments: `{"input":"hello"}`,
			}},
		},
		{
			Role: messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{{
				ToolCallID: "call_1",
				Name:       "echo",
				Content:    `{"output":"world"}`,
			}},
		},
	})
	if err != nil {
		t.Fatalf("generateSummary() error = %v", err)
	}
	if got != "summary" {
		t.Fatalf("generateSummary() = %q, want summary", got)
	}
	if len(model.prompts) != 1 {
		t.Fatalf("summary prompts = %d, want 1", len(model.prompts))
	}
	prompt := model.prompts[0]
	for _, want := range []string{
		"tool_call_name: echo",
		`tool_call_arguments: {"input":"hello"}`,
		`tool_call_result: {"output":"world"}`,
	} {
		if !strings.Contains(prompt, want) {
			t.Fatalf("summary prompt missing %q:\n%s", want, prompt)
		}
	}
}

func TestCompactPreservesLLMFailureSource(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleUser, Content: "one"},
		{Role: messages.MessageRoleAssistant, Content: "two"},
		{Role: messages.MessageRoleUser, Content: "three"},
		{Role: messages.MessageRoleAssistant, Content: "four"},
		{Role: messages.MessageRoleUser, Content: "five"},
		{Role: messages.MessageRoleAssistant, Content: "six"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	compactor := NewCompactor(DefaultProtectRule, &testModel{
		Model: failingSummaryModel{err: errors.New("API error 429: insufficient_quota")},
	})

	_, _, err = compactor.Compact(context.Background(), manager, nil)
	if err == nil {
		t.Fatal("Compact() error = nil, want LLM failure")
	}
	var llmErr *executor.LLMCallError
	if !errors.As(err, &llmErr) {
		t.Fatalf("Compact() error = %T %v, want LLMCallError", err, err)
	}
}

func TestPruneHistoricalRemovesStateAndBoundsToolResultsWithoutSummary(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{
			Role:    messages.MessageRoleState,
			Content: "app: stale-old-app",
			Attachments: []messages.Attachment{{
				Source: messages.AttachmentSourceScreenshotObservation,
			}},
		},
		{Role: messages.MessageRoleUser, Content: "old request"},
		{
			Role:                messages.MessageRoleToolCall,
			ResponsesResponseID: "resp_old_tool",
			ToolCalls: []messages.ToolCall{{
				ID:        "old_call",
				Name:      "shell",
				Arguments: `{"command":"go test ./..."}`,
			}},
		},
		{
			Role: messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{{
				ToolCallID: "old_call",
				Name:       "shell",
				Content:    strings.Repeat("large historical output ", 1_000),
				Meta: &messages.ToolResultMeta{
					Complete:            true,
					ObservationComplete: true,
					Summary:             "128 passed, 2 failed",
				},
			}},
		},
		{
			Role:    messages.MessageRoleState,
			Content: "historical visual observation",
			Attachments: []messages.Attachment{{
				Source: messages.AttachmentSourceScreenshotObservation,
			}},
		},
		{Role: messages.MessageRoleAssistant, Content: "old work completed", ResponsesResponseID: "resp_old_answer"},
		{
			Role:    messages.MessageRoleState,
			Content: "app: current-app",
			Attachments: []messages.Attachment{{
				Source:   messages.AttachmentSourceScreenshotObservation,
				FilePath: "/tmp/current-state.jpg",
			}},
		},
		{Role: messages.MessageRoleUser, Content: "current request"},
		{
			Role:                messages.MessageRoleToolCall,
			ResponsesResponseID: "resp_current_tool",
			ToolCalls: []messages.ToolCall{{
				ID:        "current_call",
				Name:      "shell",
				Arguments: `{"command":"pwd"}`,
			}},
		},
		{
			Role: messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{{
				ToolCallID: "current_call",
				Name:       "shell",
				Content:    "current result",
			}},
		},
		{
			Role:    messages.MessageRoleState,
			Content: "current visual observation",
			Attachments: []messages.Attachment{{
				Source:   messages.AttachmentSourceScreenshotObservation,
				FilePath: "/tmp/current-visual.jpg",
			}},
		},
		{Role: messages.MessageRoleAssistant, Content: "current response", ResponsesResponseID: "resp_current_answer"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	model := &promptCapturingModel{reply: "summary should not be called"}
	compactor := NewCompactor(DefaultProtectRule, &testModel{Model: model})

	newManager, compacted, err := compactor.PruneHistorical(manager, 3_000)
	if err != nil {
		t.Fatalf("PruneHistorical() error = %v", err)
	}
	if !compacted || newManager == nil {
		t.Fatal("PruneHistorical() did not create a pruned context revision")
	}
	if len(model.prompts) != 0 {
		t.Fatalf("summary model calls = %d, want 0 after deterministic prune reached target", len(model.prompts))
	}

	got := newManager.CloneMessageList()
	stateContents := make([]string, 0, 2)
	results := make(map[string]messages.ToolResult)
	for _, message := range got {
		if message.Role == messages.MessageRoleState {
			stateContents = append(stateContents, message.Content)
		}
		if message.ResponsesResponseID != "" {
			t.Fatalf("pruned revision retained provider response ID: %#v", message)
		}
		for _, result := range message.ToolResults {
			results[result.ToolCallID] = result
		}
	}
	if len(stateContents) != 2 || stateContents[0] != "app: current-app" || stateContents[1] != "current visual observation" {
		t.Fatalf("retained state messages = %#v, want only current turn state", stateContents)
	}
	oldResult := results["old_call"]
	if !strings.Contains(oldResult.Content, `"status":"historical_prune"`) || !strings.Contains(oldResult.Content, "128 passed, 2 failed") || strings.Contains(oldResult.Content, "large historical output") {
		t.Fatalf("historical result placeholder = %q", oldResult.Content)
	}
	if oldResult.Meta == nil || oldResult.Meta.Complete || oldResult.Meta.Reason != historicalToolResultPruneReason {
		t.Fatalf("historical result metadata = %#v", oldResult.Meta)
	}
	if current := results["current_call"]; current.Content != "current result" || current.Meta != nil {
		t.Fatalf("current turn tool result changed: %#v", current)
	}
	stats := compactor.LastPruneStats()
	if stats.HistoricalStatesDropped != 2 || stats.HistoricalToolResultsPruned != 1 || stats.TokensAfter > 3_000 || stats.TokensBefore <= stats.TokensAfter {
		t.Fatalf("prune stats = %#v", stats)
	}
}

// The runtime's screenshot-count prune passes an unreachable target, so it
// drops expired states without compacting historical tool results.
func TestPruneHistoricalWithUnreachableTargetOnlyDropsStates(t *testing.T) {
	oldOutput := strings.Repeat("large historical output ", 1_000)
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleState, Content: "app: stale-old-app", Attachments: []messages.Attachment{{Source: messages.AttachmentSourceScreenshotObservation}}},
		{Role: messages.MessageRoleUser, Content: "old request"},
		{Role: messages.MessageRoleToolCall, ToolCalls: []messages.ToolCall{{ID: "old_call", Name: "shell", Arguments: `{"command":"go test ./..."}`}}},
		{Role: messages.MessageRoleToolResult, ToolResults: []messages.ToolResult{{ToolCallID: "old_call", Name: "shell", Content: oldOutput}}},
		{Role: messages.MessageRoleAssistant, Content: "old work completed"},
		{Role: messages.MessageRoleState, Content: "app: current-app", Attachments: []messages.Attachment{{Source: messages.AttachmentSourceScreenshotObservation}}},
		{Role: messages.MessageRoleUser, Content: "current request"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	compactor := NewCompactor(DefaultProtectRule, &testModel{Model: &promptCapturingModel{reply: "unused"}})

	newManager, pruned, err := compactor.PruneHistorical(manager, math.MaxInt)
	if err != nil {
		t.Fatalf("PruneHistorical() error = %v", err)
	}
	if !pruned || newManager == nil {
		t.Fatal("PruneHistorical() did not drop the expired state")
	}
	states := 0
	for _, message := range newManager.CloneMessageList() {
		if message.Role == messages.MessageRoleState {
			states++
		}
		for _, result := range message.ToolResults {
			if result.Content != oldOutput || result.Meta != nil {
				t.Fatalf("historical tool result was compacted: %#v", result)
			}
		}
	}
	if stats := compactor.LastPruneStats(); states != 1 || stats.HistoricalStatesDropped != 1 || stats.HistoricalToolResultsPruned != 0 {
		t.Fatalf("states=%d stats=%#v, want one current state and no tool result compaction", states, stats)
	}
}

func TestPruneHistoricalAndCompactRunAsSeparateStages(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleState, Content: "app: stale-old-app"},
		{Role: messages.MessageRoleUser, Content: "old request"},
		{Role: messages.MessageRoleAssistant, Content: strings.Repeat("old progress ", 500)},
		{Role: messages.MessageRoleState, Content: "app: current-app"},
		{Role: messages.MessageRoleUser, Content: "current request"},
		{Role: messages.MessageRoleAssistant, Content: "current response"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	model := &promptCapturingModel{reply: "historical work summary"}
	compactor := NewCompactor(DefaultProtectRule, &testModel{Model: model})

	prunedManager, pruned, err := compactor.PruneHistorical(manager, 1)
	if err != nil {
		t.Fatalf("PruneHistorical() error = %v", err)
	}
	if !pruned || prunedManager == nil {
		t.Fatal("PruneHistorical() did not create a pruned context revision")
	}
	if len(model.prompts) != 0 {
		t.Fatalf("prune summary model calls = %d, want 0", len(model.prompts))
	}

	newManager, compacted, err := compactor.Compact(context.Background(), prunedManager, nil)
	if err != nil {
		t.Fatalf("Compact() error = %v", err)
	}
	if !compacted || newManager == nil {
		t.Fatal("Compact() did not create a summarized context revision")
	}
	if len(model.prompts) != 1 {
		t.Fatalf("summary model calls = %d, want 1", len(model.prompts))
	}
	if strings.Contains(model.prompts[0], "stale-old-app") {
		t.Fatalf("summary prompt retained expired state:\n%s", model.prompts[0])
	}
	if !strings.Contains(model.prompts[0], "old progress") {
		t.Fatalf("summary prompt omitted historical conversation:\n%s", model.prompts[0])
	}

	stateContents := make([]string, 0, 1)
	for _, message := range newManager.CloneMessageList() {
		if message.Role == messages.MessageRoleState {
			stateContents = append(stateContents, message.Content)
		}
	}
	if len(stateContents) != 1 || stateContents[0] != "app: current-app" {
		t.Fatalf("retained state messages = %#v, want current state", stateContents)
	}
	stats := compactor.LastCompactionStats()
	if !stats.ConversationSummaryRequired {
		t.Fatalf("compaction stats = %#v", stats)
	}
}

func TestPruneHistoricalNeverCallsSummaryModel(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleState, Content: "expired state"},
		{Role: messages.MessageRoleUser, Content: "old request"},
		{Role: messages.MessageRoleAssistant, Content: strings.Repeat("large historical answer ", 500)},
		{Role: messages.MessageRoleState, Content: "current state"},
		{Role: messages.MessageRoleUser, Content: strings.Repeat("large current request ", 500)},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	model := &promptCapturingModel{reply: "summary should not be called"}
	compactor := NewCompactor(DefaultProtectRule, &testModel{Model: model})

	newManager, compacted, err := compactor.PruneHistorical(manager, 1)
	if err != nil {
		t.Fatalf("PruneHistorical() error = %v", err)
	}
	if !compacted || newManager == nil {
		t.Fatal("provider mode discarded deterministic state prune")
	}
	if len(model.prompts) != 0 {
		t.Fatalf("summary model calls = %d, want 0 in provider mode", len(model.prompts))
	}
	if messageListContains(newManager.CloneMessageList(), "expired state") {
		t.Fatal("provider-mode context retained expired state")
	}
	stats := compactor.LastPruneStats()
	if stats.HistoricalStatesDropped != 1 || stats.TokensAfter <= 1 {
		t.Fatalf("prune stats = %#v", stats)
	}
}

func TestPruneHistoricalSkipsContextWithoutHistoricalCandidates(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleUser, Content: "current request"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	model := &promptCapturingModel{reply: "summary should not be called"}
	compactor := NewCompactor(DefaultProtectRule, &testModel{Model: model})

	newManager, compacted, err := compactor.PruneHistorical(manager, 1_000)
	if err != nil {
		t.Fatalf("PruneHistorical() error = %v", err)
	}
	if compacted || newManager != nil {
		t.Fatal("context already within target created an unnecessary revision")
	}
	if len(model.prompts) != 0 {
		t.Fatalf("summary model calls = %d, want 0", len(model.prompts))
	}
}

func TestPruneForBudgetBoundsCurrentTurnToolExchangesAndStates(t *testing.T) {
	messageList := []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleState, Content: "state before current user"},
		{Role: messages.MessageRoleUser, Content: "finish the long task"},
	}
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("call_%d", i)
		messageList = append(messageList,
			messages.Message{
				Role:                    messages.MessageRoleToolCall,
				Content:                 fmt.Sprintf("working on step %d", i),
				ResponsesResponseID:     fmt.Sprintf("resp_%d", i),
				ResponsesReasoningItems: []json.RawMessage{json.RawMessage(`{"type":"reasoning"}`)},
				ResponsesOutputItems:    []json.RawMessage{json.RawMessage(`{"type":"function_call"}`)},
				ResponsesAssistantPhase: "commentary",
				InteractionsSteps:       []json.RawMessage{json.RawMessage(fmt.Sprintf(`{"type":"thought","signature":"sig_%d"}`, i))},
				ToolCalls: []messages.ToolCall{{
					ID:        id,
					Name:      "shell",
					Arguments: fmt.Sprintf(`{"command":"step-%d %s"}`, i, strings.Repeat("x", 1_000)),
				}},
			},
			messages.Message{
				Role: messages.MessageRoleToolResult,
				ToolResults: []messages.ToolResult{{
					ToolCallID: id,
					Name:       "shell",
					Content:    fmt.Sprintf("step %d result %s", i, strings.Repeat("y", 1_000)),
				}},
			},
			messages.Message{Role: messages.MessageRoleState, Content: fmt.Sprintf("state after step %d", i)},
		)
	}
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), messageList)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	model := &promptCapturingModel{reply: "summary should not be called"}
	compactor := NewCompactor(DefaultProtectRule, &testModel{Model: model})
	targetMessages, _ := pruneCurrentTurnStates(manager.CloneMessageList())
	compactedExchanges := 0
	for i := lastMessageIndexWithRole(targetMessages, messages.MessageRoleUser) + 1; i+1 < len(targetMessages) && compactedExchanges < 3; i++ {
		if !isCompletedToolExchange(targetMessages[i], targetMessages[i+1]) {
			continue
		}
		targetMessages[i], targetMessages[i+1], _ = compactCurrentTurnToolExchange(targetMessages[i], targetMessages[i+1])
		compactedExchanges++
		i++
	}
	targetTokens := estimateMessageListTokenUsage(targetMessages)

	newManager, pruned, err := compactor.PruneForBudget(manager, targetTokens)
	if err != nil {
		t.Fatalf("PruneForBudget() error = %v", err)
	}
	if !pruned || newManager == nil {
		t.Fatal("PruneForBudget() did not create a context revision")
	}
	if len(model.prompts) != 0 {
		t.Fatalf("summary model calls = %d, want 0", len(model.prompts))
	}

	got := newManager.CloneMessageList()
	states := make([]string, 0, 2)
	calls := make(map[string]messages.Message)
	results := make(map[string]messages.ToolResult)
	for _, message := range got {
		if message.Role == messages.MessageRoleState {
			states = append(states, message.Content)
		}
		if message.ResponsesResponseID != "" {
			t.Fatalf("pruned revision retained provider response ID: %#v", message)
		}
		for _, call := range message.ToolCalls {
			calls[call.ID] = message
		}
		for _, result := range message.ToolResults {
			results[result.ToolCallID] = result
		}
	}
	if len(states) != 2 || states[0] != "state before current user" || states[1] != "state after step 6" {
		t.Fatalf("retained current-turn states = %#v", states)
	}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("call_%d", i)
		call := calls[id]
		if call.ToolCalls[0].Arguments != `{}` || call.Content != "" || len(call.ResponsesReasoningItems) != 0 || len(call.ResponsesOutputItems) != 0 || call.ResponsesAssistantPhase != "" {
			t.Fatalf("old current-turn call %s was not compacted safely: %#v", id, call)
		}
		if len(call.InteractionsSteps) != 1 || !strings.Contains(string(call.InteractionsSteps[0]), fmt.Sprintf(`"sig_%d"`, i)) {
			t.Fatalf("old current-turn call %s lost its Gemini thought steps: %#v", id, call)
		}
		result := results[id]
		if result.Meta == nil || result.Meta.Reason != currentTurnToolExchangePruneReason || result.Meta.Complete ||
			!strings.Contains(result.Content, `"status":"current_turn_prune"`) || strings.Contains(result.Content, strings.Repeat("y", 200)) {
			t.Fatalf("old current-turn result %s was not compacted: %#v", id, result)
		}
	}
	for i := 4; i <= 6; i++ {
		id := fmt.Sprintf("call_%d", i)
		call := calls[id]
		if !strings.Contains(call.ToolCalls[0].Arguments, strings.Repeat("x", 100)) || len(call.ResponsesOutputItems) != 1 ||
			len(call.InteractionsSteps) != 1 {
			t.Fatalf("recent protected call %s changed: %#v", id, call)
		}
		if result := results[id]; result.Meta != nil || !strings.Contains(result.Content, strings.Repeat("y", 100)) {
			t.Fatalf("recent protected result %s changed: %#v", id, result)
		}
	}
	for id := range calls {
		if _, ok := results[id]; !ok {
			t.Fatalf("tool call %s has no matching result after pruning", id)
		}
	}
	if standard := messages.ConvertMessageList(got); len(standard) != len(got) {
		t.Fatalf("standard message count = %d, want %d", len(standard), len(got))
	}
	stats := compactor.LastPruneStats()
	if stats.HistoricalStatesDropped != 0 || stats.HistoricalToolResultsPruned != 0 ||
		stats.CurrentTurnStatesDropped != 5 || stats.CurrentTurnToolExchangesPruned != 3 ||
		stats.TokensAfter >= stats.TokensBefore {
		t.Fatalf("prune stats = %#v", stats)
	}

	boundedManager, pruned, err := compactor.PruneForBudget(newManager, 1)
	if err != nil {
		t.Fatalf("second PruneForBudget() error = %v", err)
	}
	if !pruned || boundedManager == nil {
		t.Fatal("second PruneForBudget() did not drop old compacted exchanges")
	}
	boundedCalls := make(map[string]bool)
	boundedResults := make(map[string]bool)
	for _, message := range boundedManager.CloneMessageList() {
		for _, call := range message.ToolCalls {
			boundedCalls[call.ID] = true
		}
		for _, result := range message.ToolResults {
			boundedResults[result.ToolCallID] = true
		}
	}
	for i := 1; i <= 3; i++ {
		id := fmt.Sprintf("call_%d", i)
		if boundedCalls[id] || boundedResults[id] {
			t.Fatalf("old compacted exchange %s was retained past the hard target", id)
		}
	}
	for i := 4; i <= 6; i++ {
		id := fmt.Sprintf("call_%d", i)
		if !boundedCalls[id] || !boundedResults[id] {
			t.Fatalf("recent protected exchange %s was dropped", id)
		}
	}
	if boundedManager.GetParentSessionID() != newManager.GetSessionID() {
		t.Fatalf("bounded revision parent = %q, want %q", boundedManager.GetParentSessionID(), newManager.GetSessionID())
	}

	hardBudgetManager, pruned, err := compactor.PruneForHardBudget(boundedManager, 1)
	if err != nil {
		t.Fatalf("PruneForHardBudget() error = %v", err)
	}
	if !pruned || hardBudgetManager == nil {
		t.Fatal("PruneForHardBudget() did not prune protected exchanges")
	}
	for _, message := range hardBudgetManager.CloneMessageList() {
		if len(message.ToolCalls) != 0 || len(message.ToolResults) != 0 {
			t.Fatalf("hard-budget revision retained a protected tool exchange: %#v", message)
		}
	}
}

func TestCompactHistoricalToolResultsPrunesOldestUntilTarget(t *testing.T) {
	first := messages.ToolResult{ToolCallID: "first", Name: "shell", Content: strings.Repeat("first output ", 500)}
	second := messages.ToolResult{ToolCallID: "second", Name: "shell", Content: strings.Repeat("second output ", 500)}
	messageList := []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleUser, Content: "old request"},
		{Role: messages.MessageRoleToolCall, ToolCalls: []messages.ToolCall{{ID: "first", Name: "shell", Arguments: `{}`}}},
		{Role: messages.MessageRoleToolResult, ToolResults: []messages.ToolResult{first}},
		{Role: messages.MessageRoleAssistant, Content: "continue"},
		{Role: messages.MessageRoleToolCall, ToolCalls: []messages.ToolCall{{ID: "second", Name: "shell", Arguments: `{}`}}},
		{Role: messages.MessageRoleToolResult, ToolResults: []messages.ToolResult{second}},
		{Role: messages.MessageRoleAssistant, Content: "done"},
		{Role: messages.MessageRoleUser, Content: "current request"},
	}
	firstPlaceholder, _ := historicalToolResultPlaceholder(first, messageList[2].ToolCalls[0])
	target := estimateMessageListTokenUsage(messageList) - tokencounter.EstimateTextTokens(first.Content) + tokencounter.EstimateTextTokens(firstPlaceholder)

	got, pruned := compactHistoricalToolResults(messageList, target)
	if pruned != 1 {
		t.Fatalf("pruned tool results = %d, want 1", pruned)
	}
	if got[3].ToolResults[0].Meta == nil || got[3].ToolResults[0].Meta.Reason != historicalToolResultPruneReason {
		t.Fatalf("oldest result was not pruned: %#v", got[3].ToolResults[0])
	}
	if got[6].ToolResults[0].Content != second.Content || got[6].ToolResults[0].Meta != nil {
		t.Fatalf("newer historical result changed after reaching target: %#v", got[6].ToolResults[0])
	}
}

func TestCompactSummarizesHistoricalToolResults(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := contextmanager.NewContextManagerFromMessageList(sessionFolder, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleUser, Content: "old request"},
		{
			Role:                messages.MessageRoleToolCall,
			ResponsesResponseID: "resp_old_tool",
			ResponsesReasoningItems: []json.RawMessage{
				json.RawMessage(`{"type":"reasoning","id":"rs_old"}`),
			},
			ResponsesOutputItems: []json.RawMessage{
				json.RawMessage(`{"type":"function_call","call_id":"old_call","name":"shell","arguments":"{}"}`),
			},
			ResponsesAssistantPhase: "commentary",
			ToolCalls: []messages.ToolCall{{
				ID:        "old_call",
				Name:      "shell",
				Arguments: `{"command":"go test ./..."}`,
			}},
		},
		{
			Role: messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{{
				ToolCallID: "old_call",
				Name:       "shell",
				Content:    strings.Repeat("large historical output ", 1_000),
				Meta: &messages.ToolResultMeta{
					ArtifactPath:     "/tmp/tool-results/tr_old",
					OriginalBytes:    24_000,
					OriginalChars:    24_000,
					Complete:         false,
					ArtifactComplete: true,
					Summary:          "128 passed, 2 failed",
				},
			}},
		},
		{Role: messages.MessageRoleAssistant, Content: "old answer", ResponsesResponseID: "resp_old_answer"},
		{Role: messages.MessageRoleUser, Content: "new request"},
		{
			Role:                messages.MessageRoleToolCall,
			ToolCalls:           []messages.ToolCall{{ID: "new_call", Name: "shell", Arguments: `{"command":"pwd"}`}},
			ResponsesResponseID: "resp_new_tool",
			ResponsesReasoningItems: []json.RawMessage{
				json.RawMessage(`{"type":"reasoning","id":"rs_new"}`),
			},
			ResponsesOutputItems: []json.RawMessage{
				json.RawMessage(`{"type":"function_call","call_id":"new_call","name":"shell","arguments":"{}"}`),
			},
			ResponsesAssistantPhase: "commentary",
		},
		{
			Role:        messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{{ToolCallID: "new_call", Name: "shell", Content: "current result"}},
		},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	model := &promptCapturingModel{reply: "historical work summary"}
	compactor := NewCompactor(DefaultProtectRule, &testModel{Model: model})

	newManager, compacted, err := compactor.Compact(context.Background(), manager, nil)
	if err != nil {
		t.Fatalf("Compact() error = %v", err)
	}
	if !compacted || newManager == nil {
		t.Fatal("Compact() did not create a compacted manager")
	}
	if len(model.prompts) != 1 {
		t.Fatalf("summary model calls = %d, want 1", len(model.prompts))
	}
	if newManager.GetSessionID() == manager.GetSessionID() {
		t.Fatalf("compacted manager reused session ID %q", manager.GetSessionID())
	}

	if !strings.Contains(model.prompts[0], "large historical output") {
		t.Fatalf("summary prompt omitted historical tool result:\n%s", model.prompts[0])
	}
	compactedMessages := newManager.CloneMessageList()
	if got := compactedMessages[2].Content; !strings.Contains(got, "historical work summary") {
		t.Fatalf("compacted summary = %q", got)
	}
	if got := compactedMessages[5].ToolResults[0].Content; got != "current result" {
		t.Fatalf("current tool result = %q, want unchanged", got)
	}
	for _, message := range compactedMessages {
		if message.ResponsesResponseID != "" {
			t.Fatalf("compacted message retained provider response ID: %#v", message)
		}
	}
	retainedToolCall := compactedMessages[4]
	if len(retainedToolCall.ResponsesReasoningItems) != 1 || len(retainedToolCall.ResponsesOutputItems) != 1 || retainedToolCall.ResponsesAssistantPhase != "commentary" {
		t.Fatalf("compacted retained message lost replay metadata: %#v", retainedToolCall)
	}
}

func TestCompactSkipsFullyProtectedConversation(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: strings.Repeat("system ", 200)},
		{
			Role: messages.MessageRoleToolCall,
			ToolCalls: []messages.ToolCall{{
				ID:        "old_call",
				Name:      "shell",
				Arguments: `{"command":"generate-report"}`,
			}},
		},
		{
			Role: messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{{
				ToolCallID: "old_call",
				Name:       "shell",
				Content:    strings.Repeat("large historical output ", 1_000),
				Meta:       &messages.ToolResultMeta{Summary: "report generated"},
			}},
		},
		{Role: messages.MessageRoleUser, Content: "current request"},
		{Role: messages.MessageRoleAssistant, Content: "current draft"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	model := &promptCapturingModel{reply: "summary should not be called"}
	compactor := NewCompactor(DefaultProtectRule, &testModel{Model: model})

	newManager, compacted, err := compactor.Compact(context.Background(), manager, nil)
	if err != nil {
		t.Fatalf("Compact() error = %v", err)
	}
	if compacted || newManager != nil {
		t.Fatal("Compact() compacted a fully protected conversation")
	}
	if len(model.prompts) != 0 {
		t.Fatalf("summary model calls = %d, want 0 for fully protected window", len(model.prompts))
	}
}

func TestCompactLeavesProtectedOrphanedToolResultUnchanged(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{
			Role: messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{{
				ToolCallID: "missing_call",
				Name:       "shell",
				Content:    strings.Repeat("orphaned historical output ", 1_000),
			}},
		},
		{Role: messages.MessageRoleUser, Content: "current request"},
		{Role: messages.MessageRoleAssistant, Content: "current response"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	compactor := NewCompactor(DefaultProtectRule, &testModel{})

	newManager, compacted, err := compactor.Compact(context.Background(), manager, nil)
	if err != nil {
		t.Fatalf("Compact() error = %v", err)
	}
	if compacted || newManager != nil {
		t.Fatal("Compact() compacted a fully protected orphaned tool result")
	}
}

func TestCompactDropsDeletedRecoverableToolResultMode(t *testing.T) {
	sessionFolder := t.TempDir()
	completePath := filepath.Join(sessionFolder, "tr_complete.data")
	partialPath := filepath.Join(sessionFolder, "tr_partial.data")
	for _, path := range []string{completePath, partialPath} {
		writeTestArtifact(t, path, time.Now().Add(time.Hour))
	}
	manager, err := contextmanager.NewContextManagerFromMessageList(sessionFolder, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleUser, Content: "old request"},
		{
			Role: messages.MessageRoleToolCall,
			ToolCalls: []messages.ToolCall{
				{ID: "old_complete", Name: "shell", Arguments: `{"command":"go test ./..."}`},
				{ID: "old_partial", Name: "inspect_episode", Arguments: `{"episode_id":"ep_1"}`},
			},
		},
		{
			Role: messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{
				{
					ToolCallID: "old_complete",
					Name:       "shell",
					Content:    strings.Repeat("complete historical output ", 1_000),
					Meta: &messages.ToolResultMeta{
						ArtifactPath:     completePath,
						ArtifactComplete: true,
						Summary:          "128 passed, 2 failed",
					},
				},
				{
					ToolCallID: "old_partial",
					Name:       "inspect_episode",
					Content:    strings.Repeat("partial historical output ", 1_000),
					Meta: &messages.ToolResultMeta{
						ArtifactPath:     partialPath,
						ArtifactComplete: false,
						Summary:          "episode ep_1 completed",
					},
				},
			},
		},
		{Role: messages.MessageRoleAssistant, Content: strings.Repeat("old answer ", 500)},
		{Role: messages.MessageRoleUser, Content: "current request"},
		{
			Role:      messages.MessageRoleToolCall,
			ToolCalls: []messages.ToolCall{{ID: "current_call", Name: "shell", Arguments: `{"command":"pwd"}`}},
		},
		{
			Role:        messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{{ToolCallID: "current_call", Name: "shell", Content: "current result"}},
		},
		{
			Role:      messages.MessageRoleToolCall,
			ToolCalls: []messages.ToolCall{{ID: "current_call_2", Name: "shell", Arguments: `{"command":"grep -F recovery /tmp/tool-results/tr_current.data"}`}},
		},
		{
			Role:        messages.MessageRoleToolResult,
			ToolResults: []messages.ToolResult{{ToolCallID: "current_call_2", Name: "shell", Content: "current recovery result"}},
		},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	model := &promptCapturingModel{reply: "summary deliberately omits every artifact reference"}
	compactor := NewCompactor(DefaultProtectRule, &testModel{Model: model})

	newManager, compacted, err := compactor.Compact(context.Background(), manager, nil)
	if err != nil {
		t.Fatalf("Compact() error = %v", err)
	}
	if !compacted || newManager == nil {
		t.Fatal("Compact() did not create a compacted manager")
	}
	if len(model.prompts) != 1 {
		t.Fatalf("summary model calls = %d, want 1", len(model.prompts))
	}

	messageList := newManager.CloneMessageList()
	combined := ""
	for _, message := range messageList {
		combined += message.Content
	}
	for _, unwanted := range []string{
		"## Recoverable Tool Results",
		completePath,
		partialPath,
	} {
		if strings.Contains(combined, unwanted) {
			t.Fatalf("compacted context retained deleted recovery data %q:\n%s", unwanted, combined)
		}
	}
	for _, message := range messageList {
		if len(message.RecoverableToolResults) != 0 {
			t.Fatalf("compacted context retained recovery metadata: %#v", message.RecoverableToolResults)
		}
	}
	currentContents := make([]string, 0, 2)
	currentUserPreserved := false
	for _, message := range messageList {
		if message.Role == messages.MessageRoleUser && message.Content == "current request" {
			currentUserPreserved = true
		}
		for _, result := range message.ToolResults {
			if result.ToolCallID == "current_call" || result.ToolCallID == "current_call_2" {
				currentContents = append(currentContents, result.Content)
			}
		}
	}
	if !currentUserPreserved || len(currentContents) != 2 || currentContents[0] != "current result" || currentContents[1] != "current recovery result" {
		t.Fatalf("current turn was not preserved: user=%v results=%#v messages=%#v", currentUserPreserved, currentContents, messageList)
	}
}

func messageListContains(messages []messages.Message, value string) bool {
	for _, message := range messages {
		if strings.Contains(message.Content, value) {
			return true
		}
		for _, result := range message.ToolResults {
			if strings.Contains(result.Content, value) {
				return true
			}
		}
	}
	return false
}

func writeTestArtifact(t *testing.T, dataPath string, expiresAt time.Time) {
	t.Helper()
	if err := os.WriteFile(dataPath, []byte("artifact"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", dataPath, err)
	}
	metadata, err := json.Marshal(contextmanager.ArtifactMetadata{
		MIMEType:  "text/plain",
		Size:      int64(len("artifact")),
		CreatedAt: time.Now().Add(-time.Hour),
		ExpiresAt: expiresAt,
		Complete:  true,
	})
	if err != nil {
		t.Fatalf("Marshal artifact metadata: %v", err)
	}
	metadataPath := strings.TrimSuffix(dataPath, ".data") + ".json"
	if err := os.WriteFile(metadataPath, metadata, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", metadataPath, err)
	}
}
