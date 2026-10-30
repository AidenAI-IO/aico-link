package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/model"
	"aico-agent/internal/agent/tokencounter"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
)

func TestActivePromptTokensUsesUsageAndEstimatesOnlyNewContext(t *testing.T) {
	manager, err := contextmanager.NewContextManager(t.TempDir(), "system")
	if err != nil {
		t.Fatal(err)
	}
	options := []llms.CallOption{llms.WithTools([]llms.Tool{{
		Type: "function",
		Function: &llms.FunctionDefinition{
			Name:        "shell",
			Description: "Execute a shell command",
		},
	}})}
	var callOptions llms.CallOptions
	options[0](&callOptions)
	schemaTokens := tokencounter.EstimateToolSchemaTokens(callOptions)
	if schemaTokens <= 0 {
		t.Fatal("test requires a nonempty schema estimate")
	}
	want := tokencounter.EstimateMessagesTokens(manager.CloneMessageList()) + schemaTokens
	if got := estimateActivePromptTokens(manager, options); got != want {
		t.Fatalf("initial prompt tokens = %d, want %d", got, want)
	}
	if got := activeMessageTokens(manager, callOptions); got != want-schemaTokens {
		t.Fatalf("initial message tokens = %d, want %d", got, want-schemaTokens)
	}

	if err := manager.AppendMessage(messages.Message{
		Role: messages.MessageRoleAssistant, Content: "ok",
		Usage: &messages.Usage{InputTokens: 800, OutputTokens: 20, TotalTokens: 820},
	}); err != nil {
		t.Fatal(err)
	}
	if got := estimateActivePromptTokens(manager, options); got != 820 {
		t.Fatalf("measured prompt tokens = %d, want 820 without counting schema twice", got)
	}
	if got := activeMessageTokens(manager, callOptions); got != max(0, 820-schemaTokens) {
		t.Fatalf("measured message tokens = %d, want schema excluded", got)
	}
	delta := messages.Message{Role: messages.MessageRoleUser, Content: "next question"}
	if err := manager.AppendMessage(delta); err != nil {
		t.Fatal(err)
	}
	if got, want := estimateActivePromptTokens(manager, options), 820+tokencounter.EstimateMessageTokens(delta); got != want {
		t.Fatalf("incremental prompt tokens = %d, want %d", got, want)
	}

	revision, err := contextmanager.NewContextManagerRevisionFromMessageList(manager, manager.CloneMessageList())
	if err != nil {
		t.Fatal(err)
	}
	want = tokencounter.EstimateMessagesTokens(revision.CloneMessageList()) + schemaTokens
	if got := estimateActivePromptTokens(revision, options); got != want {
		t.Fatalf("revised prompt tokens = %d, want fresh estimate %d", got, want)
	}
	if got := activeMessageTokens(revision, callOptions); got != want-schemaTokens {
		t.Fatalf("revised message tokens = %d, want %d", got, want-schemaTokens)
	}
	largeSchema := llms.CallOptions{}
	llms.WithTools([]llms.Tool{{
		Type: "function",
		Function: &llms.FunctionDefinition{
			Name: "large", Description: strings.Repeat("schema ", 1_000),
		},
	}})(&largeSchema)
	if got := activeMessageTokens(manager, largeSchema); got != 0 {
		t.Fatalf("message tokens with schema larger than usage = %d, want zero", got)
	}
}

func TestToolResultPolicyKeepsSmallResultInline(t *testing.T) {
	policy := NewToolResultPolicy()
	prepared, err := policy.Prepare(context.Background(), ToolResultPrepareInput{
		Call: ToolCall{
			Spec:  ToolSpec{Name: "shell"},
			Input: `{"command":"pwd"}`,
		},
		Result:      ToolResult{Output: "/workspace\n"},
		ModelSpec:   model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
		CallOptions: []llms.CallOption{llms.WithMaxTokens(1_000)},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Content != "/workspace\n" {
		t.Fatalf("Prepare() content = %q, want raw output", prepared.Content)
	}
	if !prepared.Complete {
		t.Fatal("Prepare() complete = false, want true")
	}
	if prepared.ArtifactPath != "" {
		t.Fatalf("Prepare() artifact ref = %q, want empty", prepared.ArtifactPath)
	}
	if prepared.Reason != ToolResultReasonInline {
		t.Fatalf("Prepare() reason = %q, want %q", prepared.Reason, ToolResultReasonInline)
	}
}

func TestConversationCompactionTriggerRequiresPositiveUsableInput(t *testing.T) {
	if trigger, ok := conversationCompactionTrigger(0, defaultContextCompactionThreshold); ok || trigger != 0 {
		t.Fatalf("conversationCompactionTrigger(0) = %d, %v; want disabled", trigger, ok)
	}
	trigger, ok := conversationCompactionTrigger(10_000, defaultContextCompactionThreshold)
	if !ok || trigger != 8_000 {
		t.Fatalf("conversationCompactionTrigger(10000) = %d, %v; want 8000, true", trigger, ok)
	}
}

func TestConversationCompactionTriggerHonorsConfiguredThreshold(t *testing.T) {
	trigger, ok := conversationCompactionTrigger(10_000, 0.5)
	if !ok || trigger != 5_000 {
		t.Fatalf("conversationCompactionTrigger(10000, 0.5) = %d, %v; want 5000, true", trigger, ok)
	}

	// An unset threshold falls back to the default so callers that bypass
	// Config.ContextCompactionThresholdOrDefault still behave sanely.
	trigger, ok = conversationCompactionTrigger(10_000, 0)
	if !ok || trigger != 8_000 {
		t.Fatalf("conversationCompactionTrigger(10000, 0) = %d, %v; want default 8000, true", trigger, ok)
	}

	// A tiny budget combined with a small fraction rounds to zero; compaction
	// must report disabled rather than trigger on every turn.
	if trigger, ok = conversationCompactionTrigger(1, 0.1); ok || trigger != 0 {
		t.Fatalf("conversationCompactionTrigger(1, 0.1) = %d, %v; want disabled", trigger, ok)
	}
}

func TestHistoricalPruneBudgetsDeriveTokensFromFraction(t *testing.T) {
	trigger, target, ok := historicalPruneBudgets(10_000, 0.5)
	if !ok || trigger != 5_000 || target != 4_285 {
		t.Fatalf("historicalPruneBudgets(10000, 0.5) = %d, %d, %v; want 5000, 4285, true", trigger, target, ok)
	}

	// The default fraction must still produce the long-standing budget fractions.
	trigger, target, ok = historicalPruneBudgets(10_000, defaultContextPruneThreshold)
	if !ok || trigger != 5_000 || target != 4_285 {
		t.Fatalf("historicalPruneBudgets(10000, 0.5) = %d, %d, %v; want 5000, 4285, true", trigger, target, ok)
	}

	// An unset fraction falls back to the default for callers that bypass
	// Config.ContextPruneThresholdOrDefault.
	trigger, target, ok = historicalPruneBudgets(10_000, 0)
	if !ok || trigger != 5_000 || target != 4_285 {
		t.Fatalf("historicalPruneBudgets(10000, 0) = %d, %d, %v; want default 5000, 4285, true", trigger, target, ok)
	}

	if trigger, target, ok = historicalPruneBudgets(0, 0.7); ok || trigger != 0 || target != 0 {
		t.Fatalf("historicalPruneBudgets(0, 0.7) = %d, %d, %v; want disabled", trigger, target, ok)
	}

	// Too small to leave hysteresis below the trigger: pruning cannot make
	// progress, so it must report disabled instead of thrashing every turn.
	if trigger, target, ok = historicalPruneBudgets(1, 0.7); ok || trigger != 0 || target != 0 {
		t.Fatalf("historicalPruneBudgets(1, 0.7) = %d, %d, %v; want disabled", trigger, target, ok)
	}
}

func TestHistoricalPruneRunsBeforeConversationCompaction(t *testing.T) {
	const usableInputBudget = 10_000
	pruneTrigger, _, pruneOK := historicalPruneBudgets(usableInputBudget, defaultContextPruneThreshold)
	compactionTrigger, compactionOK := conversationCompactionTrigger(usableInputBudget, defaultContextCompactionThreshold)
	if !pruneOK || !compactionOK || pruneTrigger >= compactionTrigger {
		t.Fatalf("default prune trigger = %d, compaction trigger = %d; prune must run first", pruneTrigger, compactionTrigger)
	}

	// The clamp in ContextPruneThresholdOrDefault is what preserves this
	// ordering when a config sets a prune fraction above the compaction one.
	cfg := Config{ContextPruneThreshold: 0.95, ContextCompactionThreshold: 0.6}
	pruneTrigger, _, pruneOK = historicalPruneBudgets(usableInputBudget, cfg.ContextPruneThresholdOrDefault())
	compactionTrigger, compactionOK = conversationCompactionTrigger(usableInputBudget, cfg.ContextCompactionThresholdOrDefault())
	if !pruneOK || !compactionOK || pruneTrigger > compactionTrigger {
		t.Fatalf("clamped prune trigger = %d, compaction trigger = %d; prune must not exceed compaction", pruneTrigger, compactionTrigger)
	}
}

func TestToolResultPolicyBoundsResultWhenCurrentContextIsFull(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: strings.Repeat("context ", 3_650)},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	output := strings.Repeat("result ", 400)
	if len(output) >= toolResultInlineMaxBytes || tokencounter.EstimateTextTokens(output) >= toolResultInlineMaxTokens {
		t.Fatal("test setup output must be intrinsically small")
	}

	policy := NewToolResultPolicy()
	prepared, err := policy.Prepare(context.Background(), ToolResultPrepareInput{
		Call: ToolCall{
			Spec:  ToolSpec{Name: "recall_memory"},
			Input: `{"tags":["context"]}`,
		},
		Result:         ToolResult{Output: output},
		ContextManager: manager,
		ModelSpec:      model.ModelSpec{ContextWindow: 10_000, MaxOutput: 1_000},
		CallOptions:    []llms.CallOption{llms.WithMaxTokens(1_000)},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Reason != ToolResultReasonContextLarge {
		t.Fatalf("Prepare() reason = %q, want %q", prepared.Reason, ToolResultReasonContextLarge)
	}
	if prepared.Complete {
		t.Fatal("Prepare() complete = true, want false")
	}
	if got := tokencounter.EstimateTextTokens(prepared.Content); got >= tokencounter.EstimateTextTokens(output) {
		t.Fatalf("Prepare() content tokens = %d, want less than raw result", got)
	}
}

func TestToolResultPolicyDeliversCompleteRequiredResultInFull(t *testing.T) {
	skillOutput := "# Device Operator\nintro\n" + strings.Repeat("follow this step exactly\n", 1_200) + "## Final Rule\nTAIL_MARKER"
	if len(skillOutput) <= toolResultInlineMaxBytes {
		t.Fatal("test setup skill must exceed the generic inline limit")
	}
	// The context is already nearly full. A generic result would be bounded to
	// the space left; a complete-required result is appended whole and left to
	// ContextBudgetGuard to make room before the next model call.
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: strings.Repeat("context ", 40_000)},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}

	prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
		Call: ToolCall{
			Spec:  NewToolSpec(NewSkillReadTool(t.TempDir())),
			Input: `{"name":"device-operator"}`,
		},
		Result:         ToolResult{Output: skillOutput},
		ContextManager: manager,
		ModelSpec:      model.ModelSpec{ContextWindow: 64_000, MaxOutput: 4_000},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Content != skillOutput {
		t.Fatalf("Prepare() content was not delivered in full: got %d bytes, want %d", len(prepared.Content), len(skillOutput))
	}
	if !prepared.Complete || !prepared.ObservationComplete || !prepared.RequiresComplete || prepared.Reason != ToolResultReasonInline || prepared.ArtifactPath != "" {
		t.Fatalf("Prepare() = complete:%t observation:%t requires_complete:%t reason:%q artifact:%q",
			prepared.Complete, prepared.ObservationComplete, prepared.RequiresComplete, prepared.Reason, prepared.ArtifactPath)
	}
	if !strings.Contains(prepared.Summary, `"skill":"device-operator"`) {
		t.Fatalf("Prepare() summary = %q, want the skill projection for later placeholders", prepared.Summary)
	}
	meta := toolResultMessage("call_skill", "skill_read", prepared).ToolResults[0].Meta
	if meta == nil || !meta.RequiresComplete {
		t.Fatalf("tool result meta = %#v, want RequiresComplete for current-turn pruning", meta)
	}
}

func TestToolResultPolicyPreservesOversizedCompleteResultAsArtifact(t *testing.T) {
	skillOutput := "# Device Operator\n" + strings.Repeat("follow this step exactly\n", 1_200)
	manager, err := contextmanager.NewContextManager(t.TempDir(), "system")
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
		Call: ToolCall{
			Spec:   NewToolSpec(NewSkillReadTool(t.TempDir())),
			Input:  `{"name":"device-operator"}`,
			Action: schema.AgentAction{ToolID: "call_skill"},
		},
		Result:         ToolResult{Output: skillOutput},
		ContextManager: manager,
		ModelSpec:      model.ModelSpec{ContextWindow: 8_000, MaxOutput: 1_000},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Reason != ToolResultReasonCompleteTooLarge || prepared.Complete || prepared.ObservationComplete || prepared.RequiresComplete {
		t.Fatalf("Prepare() = reason:%q complete:%t observation:%t requires_complete:%t",
			prepared.Reason, prepared.Complete, prepared.ObservationComplete, prepared.RequiresComplete)
	}
	if prepared.ArtifactPath == "" || !prepared.ArtifactComplete {
		t.Fatalf("Prepare() artifact path=%q complete=%t", prepared.ArtifactPath, prepared.ArtifactComplete)
	}
	stored, err := os.ReadFile(prepared.ArtifactPath)
	if err != nil || string(stored) != skillOutput {
		t.Fatalf("artifact did not preserve complete skill: error=%v bytes=%d", err, len(stored))
	}
	for _, want := range []string{"observation_complete=false", "Skill instructions incomplete", "Full result file:", prepared.ArtifactPath, "dd bs=1", "# Device Operator"} {
		if !strings.Contains(prepared.Content, want) {
			t.Fatalf("Prepare() content missing %q:\n%s", want, prepared.Content)
		}
	}
	if tokencounter.EstimateTextTokens(prepared.Content) > toolResultInlineMaxTokens {
		t.Fatalf("Prepare() content exceeded inline budget: %d tokens", prepared.ContextTokens)
	}
	meta := toolResultMessage("call_skill", "skill_read", prepared).ToolResults[0].Meta
	if meta == nil || meta.RequiresComplete || meta.ObservationComplete || meta.ArtifactPath != prepared.ArtifactPath {
		t.Fatalf("tool result meta = %#v", meta)
	}
}

func TestToolResultPolicyCountsCompleteResultsAlreadyLoadedThisTurn(t *testing.T) {
	skillOutput := "# Device Operator\n" + strings.Repeat("follow this step exactly\n", 1_200)
	skillTokens := tokencounter.EstimateTextTokens(skillOutput)
	spec := model.ModelSpec{ContextWindow: 16_000, MaxOutput: 2_000}
	softLimit := toolResultSoftInputLimit(spec.ContextWindow, spec.MaxOutput)
	if skillTokens > softLimit || 2*skillTokens <= softLimit {
		t.Fatalf("test setup: skill=%d tokens must fit the %d-token limit once but not twice", skillTokens, softLimit)
	}
	prepare := func(manager *contextmanager.ContextManager) PreparedToolResult {
		t.Helper()
		prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
			Call:           ToolCall{Spec: NewToolSpec(NewSkillReadTool(t.TempDir())), Input: `{"name":"aico"}`},
			Result:         ToolResult{Output: skillOutput},
			ContextManager: manager,
			ModelSpec:      spec,
		})
		if err != nil {
			t.Fatalf("Prepare() error = %v", err)
		}
		return prepared
	}
	loadedSkill := messages.Message{
		Role: messages.MessageRoleToolResult,
		ToolResults: []messages.ToolResult{{
			ToolCallID: "call_device_operator",
			Name:       "skill_read",
			Content:    skillOutput,
			Meta:       &messages.ToolResultMeta{Complete: true, RequiresComplete: true},
		}},
	}
	newManager := func(t *testing.T, messageList []messages.Message) *contextmanager.ContextManager {
		t.Helper()
		manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), messageList)
		if err != nil {
			t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
		}
		return manager
	}

	// A skill loaded in an earlier turn is reducible history, so it does not
	// count against a new skill.
	previousTurn := newManager(t, []messages.Message{
		{Role: messages.MessageRoleUser, Content: "earlier task"},
		loadedSkill,
		{Role: messages.MessageRoleUser, Content: "operate the device"},
	})
	if prepared := prepare(previousTurn); prepared.Content != skillOutput || !prepared.RequiresComplete {
		t.Fatalf("skill after a previous-turn skill = reason:%q requires_complete:%t, want delivered in full", prepared.Reason, prepared.RequiresComplete)
	}

	// A skill loaded in this turn cannot be pruned or compacted away, so a
	// second one that would not fit beside it is saved for bounded recovery.
	currentTurn := newManager(t, []messages.Message{
		{Role: messages.MessageRoleUser, Content: "operate the device"},
		loadedSkill,
	})
	prepared := prepare(currentTurn)
	if prepared.Reason != ToolResultReasonCompleteTooLarge || prepared.RequiresComplete || !prepared.ArtifactComplete {
		t.Fatalf("second skill in one turn = reason:%q requires_complete:%t artifact:%q", prepared.Reason, prepared.RequiresComplete, prepared.ArtifactPath)
	}
}

func TestToolResultPolicyCountsCurrentUserMessageBeforeLoadingSkill(t *testing.T) {
	skillOutput := "# Device Operator\n" + strings.Repeat("follow this step exactly\n", 800)
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleUser, Content: strings.Repeat("long user request ", 2_000)},
	})
	if err != nil {
		t.Fatal(err)
	}
	spec := model.ModelSpec{ContextWindow: 16_000, MaxOutput: 2_000}
	if tokencounter.EstimateTextTokens(skillOutput) > completeToolResultTokenLimit(ToolResultPrepareInput{ModelSpec: spec}) {
		t.Fatal("test setup: skill must fit without the current user message")
	}
	prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
		Call:           ToolCall{Spec: NewToolSpec(NewSkillReadTool(t.TempDir())), Input: `{"name":"aico"}`},
		Result:         ToolResult{Output: skillOutput},
		ContextManager: manager,
		ModelSpec:      spec,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Reason != ToolResultReasonCompleteTooLarge || !prepared.ArtifactComplete || prepared.RequiresComplete {
		t.Fatalf("long user message admitted complete skill: reason=%q artifact=%t requires_complete=%t", prepared.Reason, prepared.ArtifactComplete, prepared.RequiresComplete)
	}
	if err := manager.AppendMessage(messages.Message{
		Role:      messages.MessageRoleToolCall,
		ToolCalls: []messages.ToolCall{{ID: "skill", Name: "skill_read", Arguments: `{"name":"aico"}`}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := manager.AppendMessage(toolResultMessage("skill", "skill_read", prepared)); err != nil {
		t.Fatal(err)
	}
	if got, budget := estimateActivePromptTokens(manager, nil), toolResultUsableInputBudget(spec.ContextWindow, spec.MaxOutput); got > budget {
		t.Fatalf("recoverable skill result still exceeds the next model input budget: got=%d budget=%d", got, budget)
	}
}

func TestToolResultPolicyBoundsIntrinsicallyLargeResult(t *testing.T) {
	output := "HEAD_MARKER\n" + strings.Repeat("0123456789", 1_200) + "\nTAIL_MARKER"
	policy := NewToolResultPolicy()
	prepared, err := policy.Prepare(context.Background(), ToolResultPrepareInput{
		Call: ToolCall{
			Spec:  ToolSpec{Name: "shell"},
			Input: `{"command":"pwd"}`,
		},
		Result:      ToolResult{Output: output},
		ModelSpec:   model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
		CallOptions: []llms.CallOption{llms.WithMaxTokens(1_000)},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.Complete {
		t.Fatal("Prepare() complete = true, want false")
	}
	if prepared.Reason != ToolResultReasonIntrinsicLarge {
		t.Fatalf("Prepare() reason = %q, want %q", prepared.Reason, ToolResultReasonIntrinsicLarge)
	}
	for _, want := range []string{"[shell] pwd", "HEAD_MARKER", "TAIL_MARKER", "chars"} {
		if !strings.Contains(prepared.Content, want) {
			t.Fatalf("Prepare() content missing %q:\n%s", want, prepared.Content)
		}
	}
	if strings.Contains(prepared.Content, "tool result omitted") {
		t.Fatalf("Prepare() used opaque omission text: %s", prepared.Content)
	}
	if got := tokencounter.EstimateTextTokens(prepared.Content); got > toolResultInlineMaxTokens {
		t.Fatalf("Prepare() tokens = %d, want <= %d", got, toolResultInlineMaxTokens)
	}
}

func TestToolResultPolicyShrinksIntrinsicResultToCurrentBudget(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: strings.Repeat("context ", 3_000)},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	output := strings.Repeat("large-result ", 1_000)
	input := ToolResultPrepareInput{
		Call:           ToolCall{Spec: ToolSpec{Name: "shell"}, Input: `{"command":"go test ./..."}`},
		Result:         ToolResult{Output: output},
		ContextManager: manager,
		ModelSpec:      model.ModelSpec{ContextWindow: 10_000, MaxOutput: 1_000},
		CallOptions:    []llms.CallOption{llms.WithMaxTokens(1_000)},
	}
	available := availableToolResultTokens(input)
	if available <= toolResultMinimumObservation || available >= toolResultPreviewTargetToken {
		t.Fatalf("test setup available tokens = %d, want constrained positive budget", available)
	}
	prepared, err := NewToolResultPolicy().Prepare(context.Background(), input)
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if got := tokencounter.EstimateTextTokens(prepared.Content); got > available {
		t.Fatalf("Prepare() content tokens = %d, want <= available %d", got, available)
	}
	if prepared.ArtifactPath == "" || !strings.Contains(prepared.Content, prepared.ArtifactPath) {
		t.Fatalf("Prepare() content lost recovery ref %q:\n%s", prepared.ArtifactPath, prepared.Content)
	}
}

func TestToolResultPolicyRejectsRecoveryTextThatExceedsMinimumBudget(t *testing.T) {
	sessionFolder := t.TempDir()
	for index := 0; index < 6; index++ {
		sessionFolder = filepath.Join(sessionFolder, fmt.Sprintf("long-session-directory-%02d-%s", index, strings.Repeat("x", 32)))
	}
	if err := os.MkdirAll(sessionFolder, 0o700); err != nil {
		t.Fatalf("MkdirAll(sessionFolder) error = %v", err)
	}
	manager, err := contextmanager.NewContextManagerFromMessageList(sessionFolder, nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
		Call:           ToolCall{Spec: ToolSpec{Name: "shell"}, Input: `{"command":"produce-large-output"}`},
		Result:         ToolResult{Output: strings.Repeat("large-result ", 1_000)},
		ContextManager: manager,
		ModelSpec:      model.ModelSpec{ContextWindow: 128, MaxOutput: 16},
		CallOptions:    []llms.CallOption{llms.WithMaxTokens(16)},
	})
	if !errors.Is(err, ErrToolResultRecoveryTextTooLarge) {
		t.Fatalf("Prepare() error = %v, want %v", err, ErrToolResultRecoveryTextTooLarge)
	}
	if prepared.ArtifactPath == "" || len(prepared.ArtifactPath) < 300 {
		t.Fatalf("Prepare() artifact path = %q, want long path", prepared.ArtifactPath)
	}
	if prepared.Content != "" {
		t.Fatalf("Prepare() content = %q, want empty on oversized recovery text", prepared.Content)
	}
}

func TestToolResultPolicyUsesClipboardProjection(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	clipboardText := "VISIBLE_PREFIX_" + strings.Repeat("secret-value-", 800) + "TAIL_SECRET"
	output := `{"ok":true,"text":` + mustMarshalJSONString(t, clipboardText) + `}`
	policy := NewToolResultPolicy()
	prepared, err := policy.Prepare(context.Background(), ToolResultPrepareInput{
		Call: ToolCall{
			Spec:  ToolSpec{Name: toolBridgeClipboard},
			Input: `{"action":"read"}`,
		},
		Result:         ToolResult{Output: output},
		ContextManager: manager,
		ModelSpec:      model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
		CallOptions:    []llms.CallOption{llms.WithMaxTokens(1_000)},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	for _, want := range []string{"[bridge_clipboard] read", `"ok":true`, `"text_chars":`, "VISIBLE_PREFIX_"} {
		if !strings.Contains(prepared.Content, want) {
			t.Fatalf("Prepare() content missing %q:\n%s", want, prepared.Content)
		}
	}
	if strings.Contains(prepared.Content, "TAIL_SECRET") {
		t.Fatalf("Prepare() leaked clipboard tail into active context: %s", prepared.Content)
	}
	if prepared.ArtifactPath != "" || strings.Contains(prepared.Content, "Full result file:") {
		t.Fatalf("Prepare() exposed a sensitive artifact path: path=%q content=%s", prepared.ArtifactPath, prepared.Content)
	}
}

func TestToolResultPolicyPersistsLargeResultAsArtifact(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	output := "HEAD\n" + strings.Repeat("large-output-", 1_000) + "\nTAIL"
	policy := NewToolResultPolicy()
	prepared, err := policy.Prepare(context.Background(), ToolResultPrepareInput{
		Call: ToolCall{
			Spec:   ToolSpec{Name: "shell"},
			Action: schema.AgentAction{ToolID: "call_1", Tool: "shell"},
			Input:  `{"command":"go test ./..."}`,
		},
		Result:         ToolResult{Output: output},
		ContextManager: manager,
		ModelSpec:      model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
		CallOptions:    []llms.CallOption{llms.WithMaxTokens(1_000)},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.ArtifactPath == "" || !prepared.ArtifactComplete {
		t.Fatalf("Prepare() artifact path = %q complete=%v", prepared.ArtifactPath, prepared.ArtifactComplete)
	}
	if !strings.Contains(prepared.Content, "Full result file: "+prepared.ArtifactPath) {
		t.Fatalf("Prepare() content missing artifact path: %s", prepared.Content)
	}
	for _, want := range []string{
		"Use bounded",
		"never use cat",
		"whole artifact file",
	} {
		if !strings.Contains(prepared.Content, want) {
			t.Fatalf("Prepare() content missing shell recovery guidance %q: %s", want, prepared.Content)
		}
	}
	// The board rootfs has no jq, so naming it here sends the model to a
	// "not found" and then into a python3 growing-slice loop on this artifact.
	// fq is present and evaluates jq expressions.
	if forms := findJqCommandForms(prepared.Content); len(forms) > 0 {
		t.Fatalf("Prepare() recovery guidance invokes uninstalled jq binary %v: %s", forms, prepared.Content)
	}
	if !strings.Contains(prepared.Content, "fq") {
		t.Fatalf("Prepare() recovery guidance missing fq: %s", prepared.Content)
	}
	data, err := os.ReadFile(prepared.ArtifactPath)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(data) != output {
		t.Fatalf("artifact file bytes=%d, want full %d bytes", len(data), len(output))
	}
}

func TestToolResultPolicyReportsArtifactPersistenceFailureWithoutRepeatingAction(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	output := strings.Repeat("x", contextmanager.ArtifactSingleMaxBytes+1)
	prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
		Call: ToolCall{
			Spec:   ToolSpec{Name: "shell"},
			Action: schema.AgentAction{ToolID: "call_side_effect", Tool: "shell"},
			Input:  `{"command":"apply-side-effect"}`,
		},
		Result:         ToolResult{Output: output},
		ContextManager: manager,
		ModelSpec:      model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
		CallOptions:    []llms.CallOption{llms.WithMaxTokens(1_000)},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	if prepared.ArtifactPath != "" || prepared.ArtifactComplete {
		t.Fatalf("Prepare() artifact ref=%q complete=%v, want unavailable", prepared.ArtifactPath, prepared.ArtifactComplete)
	}
	if !prepared.ActionCompleted || prepared.ObservationComplete {
		t.Fatalf("Prepare() action_completed=%v observation_complete=%v", prepared.ActionCompleted, prepared.ObservationComplete)
	}
	if prepared.ProcessingErrorCode != "tool_result_persistence_failed" || prepared.ArtifactStoreError != "artifact_too_large" {
		t.Fatalf("Prepare() error code=%q artifact error=%q", prepared.ProcessingErrorCode, prepared.ArtifactStoreError)
	}
	for _, want := range []string{
		`"code":"tool_result_persistence_failed"`,
		`"action_completed":true`,
		`"observation_complete":false`,
		`"artifact_store_error":"artifact_too_large"`,
	} {
		if !strings.Contains(prepared.Content, want) {
			t.Fatalf("Prepare() content missing %q:\n%s", want, prepared.Content)
		}
	}
	if strings.Contains(prepared.Content, `"ok":false`) {
		t.Fatalf("persistence failure incorrectly reported the tool action as failed:\n%s", prepared.Content)
	}
	if got := tokencounter.EstimateTextTokens(prepared.Content); got > toolResultInlineMaxTokens {
		t.Fatalf("Prepare() tokens = %d, want <= %d", got, toolResultInlineMaxTokens)
	}

	message := toolResultMessage("call_side_effect", "shell", prepared)
	meta := message.ToolResults[0].Meta
	if meta == nil || !meta.ActionCompleted || meta.ObservationComplete || meta.ProcessingErrorCode != prepared.ProcessingErrorCode || meta.ArtifactStoreError != prepared.ArtifactStoreError {
		t.Fatalf("persisted metadata = %#v", meta)
	}
}

func TestToolResultPolicyPreservesStructuredStatusOutsideHeadTail(t *testing.T) {
	output := `{"output":"` + strings.Repeat("A", 6_000) + `","exit_code":7,"exit_error":"tests failed","padding":"` + strings.Repeat("Z", 6_000) + `"}`
	prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
		Call:        ToolCall{Spec: ToolSpec{Name: "shell"}, Input: `{"action":"poll","session_id":"sh_1"}`},
		Result:      ToolResult{Output: output},
		ModelSpec:   model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
		CallOptions: []llms.CallOption{llms.WithMaxTokens(1_000)},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	for _, want := range []string{`"exit_code":7`, `"exit_error":"tests failed"`} {
		if !strings.Contains(prepared.Content, want) {
			t.Fatalf("Prepare() content missing %q:\n%s", want, prepared.Content)
		}
	}
}

func TestToolResultPolicyPreservesShellStderrTail(t *testing.T) {
	output := "Error: exit status 1\nStderr:\nFAIL TestPhoneBridge\n" + strings.Repeat("stderr-detail ", 600) + "STDERR_TAIL\nStdout:\n" + strings.Repeat("stdout-detail ", 600)
	prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
		Call:        ToolCall{Spec: ToolSpec{Name: "shell"}, Input: `{"command":"go test ./..."}`},
		Result:      ToolResult{Output: output},
		ModelSpec:   model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
		CallOptions: []llms.CallOption{llms.WithMaxTokens(1_000)},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	for _, want := range []string{"Error: exit status 1", "FAIL TestPhoneBridge", "STDERR_TAIL", "Stderr:", "Stdout:"} {
		if !strings.Contains(prepared.Content, want) {
			t.Fatalf("Prepare() content missing %q:\n%s", want, prepared.Content)
		}
	}
}

func TestToolResultPolicyPreservesShellTestFailuresOutsideHeadTail(t *testing.T) {
	output := "Stdout:\nHEAD\n" + strings.Repeat("setup detail\n", 500) +
		"--- FAIL: TestCalendarLargePayload (0.20s)\n" +
		"--- FAIL: TestContactsLargePayload (0.10s)\n" +
		strings.Repeat("cleanup detail\n", 500) +
		"FAIL\nexit status 1\n"
	prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
		Call:        ToolCall{Spec: ToolSpec{Name: "shell"}, Input: `{"command":"go test ./..."}`},
		Result:      ToolResult{Output: output},
		ModelSpec:   model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
		CallOptions: []llms.CallOption{llms.WithMaxTokens(1_000)},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	for _, want := range []string{"Key status:", "--- FAIL: TestCalendarLargePayload", "--- FAIL: TestContactsLargePayload", "exit status 1"} {
		if !strings.Contains(prepared.Content, want) {
			t.Fatalf("Prepare() content missing %q:\n%s", want, prepared.Content)
		}
	}
}

func TestToolResultPolicyProjectsCollectionCountAndTopK(t *testing.T) {
	results := make([]map[string]any, 100)
	for i := range results {
		results[i] = map[string]any{
			"id":      fmt.Sprintf("memory_%03d", i+1),
			"title":   fmt.Sprintf("Memory %03d", i+1),
			"content": strings.Repeat("detail ", 20),
		}
	}
	data, err := json.Marshal(map[string]any{
		"results":  results,
		"has_more": true,
		"cursor":   "cursor_2",
	})
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
		Call:        ToolCall{Spec: ToolSpec{Name: "recall_memory"}, Input: `{"tags":["test"]}`},
		Result:      ToolResult{Output: string(data)},
		ModelSpec:   model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
		CallOptions: []llms.CallOption{llms.WithMaxTokens(1_000)},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	for _, want := range []string{`"results_total":100`, `"has_more":true`, `"cursor":"cursor_2"`, `"id":"memory_001"`} {
		if !strings.Contains(prepared.Content, want) {
			t.Fatalf("Prepare() content missing %q:\n%s", want, prepared.Content)
		}
	}
	if strings.Contains(prepared.Content, "memory_100") {
		t.Fatalf("Prepare() included collection tail instead of Top-K: %s", prepared.Content)
	}
}

func TestToolResultPolicyUsesMemoryAndEpisodeProjections(t *testing.T) {
	tests := []struct {
		name   string
		call   ToolCall
		output string
		wants  []string
	}{
		{
			name: "memory recall keeps query and ids",
			call: ToolCall{Spec: ToolSpec{Name: "recall_memory"}, Input: `{"tags":["verification"],"entities":["Aico"],"limit":100}`},
			output: mustMarshalToolResultJSON(t, map[string]any{
				"results": []map[string]any{
					{"id": "memory_1", "title": "First", "content": strings.Repeat("detail ", 800)},
					{"id": "memory_2", "title": "Second", "content": strings.Repeat("detail ", 800)},
					{"id": "memory_3", "title": "Third", "content": strings.Repeat("detail ", 800)},
					{"id": "memory_4", "title": "Fourth", "content": strings.Repeat("detail ", 800)},
				},
			}),
			wants: []string{`"query":{"entities":["Aico"],"limit":100,"tags":["verification"]}`, `"results_total":4`, `"id":"memory_1"`},
		},
		{
			name: "episode keeps identity outcome and event count",
			call: ToolCall{Spec: ToolSpec{Name: "inspect_episode"}, Input: `{"id":"ep_123"}`},
			output: mustMarshalToolResultJSON(t, map[string]any{
				"episode": map[string]any{
					"id":      "ep_123",
					"status":  "completed",
					"outcome": "success",
					"events": []map[string]any{
						{"type": "tool_call", "id": "call_1", "payload": strings.Repeat("detail ", 1_000)},
						{"type": "tool_result", "id": "call_1", "payload": strings.Repeat("detail ", 1_000)},
					},
				},
			}),
			wants: []string{`"episode_id":"ep_123"`, `"status":"completed"`, `"outcome":"success"`, `"events_total":2`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
				Call:        tt.call,
				Result:      ToolResult{Output: tt.output},
				ModelSpec:   model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
				CallOptions: []llms.CallOption{llms.WithMaxTokens(1_000)},
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			for _, want := range tt.wants {
				if !strings.Contains(prepared.Content, want) {
					t.Fatalf("Prepare() content missing %q:\n%s", want, prepared.Content)
				}
			}
		})
	}
}

func TestToolResultPolicyUsesPhoneBridgeDataProjections(t *testing.T) {
	tests := []struct {
		name   string
		call   ToolCall
		output string
		wants  []string
	}{
		{
			name: "calendar query",
			call: ToolCall{Spec: ToolSpec{Name: toolBridgeCalendar}, Input: `{"action":"query","from":"2026-07-01T00:00:00+08:00","to":"2026-08-01T00:00:00+08:00"}`},
			output: mustMarshalToolResultJSON(t, map[string]any{
				"ok": true,
				"events": []map[string]any{
					{"event_id": "event_1", "title": "First", "start_at": "2026-07-02T10:00:00+08:00", "notes": strings.Repeat("detail ", 600)},
					{"event_id": "event_2", "title": "Second", "start_at": "2026-07-03T10:00:00+08:00", "notes": strings.Repeat("detail ", 600)},
					{"event_id": "event_3", "title": "Third", "start_at": "2026-07-04T10:00:00+08:00", "notes": strings.Repeat("detail ", 600)},
					{"event_id": "event_4", "title": "Fourth", "start_at": "2026-07-05T10:00:00+08:00", "notes": strings.Repeat("detail ", 600)},
				},
			}),
			wants: []string{`"action":"query"`, `"from":"2026-07-01T00:00:00+08:00"`, `"to":"2026-08-01T00:00:00+08:00"`, `"events_total":4`, `"event_id":"event_1"`},
		},
		{
			name: "contacts query",
			call: ToolCall{Spec: ToolSpec{Name: toolBridgeContacts}, Input: `{"action":"query","query":"Alice","limit":50}`},
			output: mustMarshalToolResultJSON(t, map[string]any{
				"ok":                true,
				"permission_status": "authorized",
				"contacts": []map[string]any{
					{"contact_id": "contact_1", "name": "Alice", "notes": strings.Repeat("detail ", 1_000)},
					{"contact_id": "contact_2", "name": "Alice B", "notes": strings.Repeat("detail ", 1_000)},
				},
			}),
			wants: []string{`"action":"query"`, `"query":"Alice"`, `"contacts_total":2`, `"contact_id":"contact_1"`, `"permission_status":"authorized"`},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
				Call:        tt.call,
				Result:      ToolResult{Output: tt.output},
				ModelSpec:   model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
				CallOptions: []llms.CallOption{llms.WithMaxTokens(1_000)},
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			for _, want := range tt.wants {
				if !strings.Contains(prepared.Content, want) {
					t.Fatalf("Prepare() content missing %q:\n%s", want, prepared.Content)
				}
			}
		})
	}
}

func TestToolResultPolicyPrioritizesPhoneResourceIDsAndPermissionStatus(t *testing.T) {
	contact := map[string]any{
		"contact_id": "contact_critical",
		"name":       "Alice",
	}
	for i := 0; i < toolResultProjectionFields+10; i++ {
		contact[fmt.Sprintf("a_padding_%02d", i)] = strings.Repeat("detail", 100)
	}
	payload := map[string]any{
		"permission_status": "authorized",
		"contacts":          []map[string]any{contact},
	}
	for i := 0; i < toolResultProjectionFields+10; i++ {
		payload[fmt.Sprintf("a_root_padding_%02d", i)] = strings.Repeat("detail", 100)
	}
	prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
		Call:        ToolCall{Spec: ToolSpec{Name: toolBridgeContacts}, Input: `{"action":"query","query":"Alice"}`},
		Result:      ToolResult{Output: mustMarshalToolResultJSON(t, payload)},
		ModelSpec:   model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
		CallOptions: []llms.CallOption{llms.WithMaxTokens(1_000)},
	})
	if err != nil {
		t.Fatalf("Prepare() error = %v", err)
	}
	for _, want := range []string{`"permission_status":"authorized"`, `"contacts_total":1`, `"contact_id":"contact_critical"`} {
		if !strings.Contains(prepared.Content, want) {
			t.Fatalf("Prepare() content missing high-priority field %q:\n%s", want, prepared.Content)
		}
	}
}

func TestToolResultPolicyUsesWebProjections(t *testing.T) {
	webOutput := "Answer: current answer\n\n[1] First result\nhttps://one.example.com\n" + strings.Repeat("description ", 900) + "\n\n[2] Second result\nhttps://two.example.com\nsecond"
	tests := []struct {
		name   string
		call   ToolCall
		output string
		wants  []string
	}{
		{
			name:   "web search",
			call:   ToolCall{Spec: ToolSpec{Name: "web_search"}, Input: `{"query":"large tool results"}`},
			output: webOutput,
			wants:  []string{`"query":"large tool results"`, `"source":"web_search"`, "https://one.example.com", "https://two.example.com"},
		},
		{
			name:   "wikipedia",
			call:   ToolCall{Spec: ToolSpec{Name: "wikipedia"}, Input: `{"query":"Raspberry Pi"}`},
			output: webOutput,
			wants:  []string{`"query":"Raspberry Pi"`, `"source":"wikipedia"`, "https://one.example.com"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
				Call:        tt.call,
				Result:      ToolResult{Output: tt.output},
				ModelSpec:   model.ModelSpec{ContextWindow: 32_000, MaxOutput: 1_000},
				CallOptions: []llms.CallOption{llms.WithMaxTokens(1_000)},
			})
			if err != nil {
				t.Fatalf("Prepare() error = %v", err)
			}
			for _, want := range tt.wants {
				if !strings.Contains(prepared.Content, want) {
					t.Fatalf("Prepare() content missing %q:\n%s", want, prepared.Content)
				}
			}
		})
	}
}

func mustMarshalJSONString(t *testing.T, value string) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return string(data)
}

func mustMarshalToolResultJSON(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("json.Marshal() error = %v", err)
	}
	return string(data)
}

func TestToolResultPolicyKeepsShortResultsWhenContextIsFull(t *testing.T) {
	for _, output := range []string{"3", "Error: exit status 1\nStderr:\nKeyError: 'content'"} {
		t.Run(output, func(t *testing.T) {
			manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), nil)
			if err != nil {
				t.Fatal(err)
			}
			prepared, err := NewToolResultPolicy().Prepare(context.Background(), ToolResultPrepareInput{
				Call:   ToolCall{Spec: ToolSpec{Name: "shell"}, Input: `{"command":"python3 -c query"}`},
				Result: ToolResult{Output: output}, ContextManager: manager,
				ModelSpec: model.ModelSpec{ContextWindow: 128, MaxOutput: 16},
			})
			if err != nil {
				t.Fatal(err)
			}
			if prepared.Content != output || !prepared.Complete || prepared.ArtifactPath != "" {
				t.Fatalf("short result lost at exhausted budget: %+v", prepared)
			}
		})
	}
}

func TestBoundedToolResultObservationPrioritizesPreview(t *testing.T) {
	path := "/userdata/agent/sessions/backend/s_b5d49cb5-a47e-438e-af91-666254403ae3/tool-results/tr_1ab053c3-8238-4302-a046-c3d5213d55c9.data"
	input, _ := json.Marshal(map[string]string{"command": "python3 -c " + strings.Repeat("long-command-", 20)})
	observation := boundedToolResultObservation(ToolCall{Spec: ToolSpec{Name: "shell"}, Input: string(input)},
		PreparedToolResult{ArtifactPath: path, ArtifactComplete: true}, "KeyError: 'content'", toolResultMinimumObservation)
	if !strings.Contains(observation, "KeyError: 'content'") || !strings.Contains(observation, path) {
		t.Fatalf("recovery metadata displaced useful preview: %s", observation)
	}
	if tokencounter.EstimateTextTokens(observation) > toolResultMinimumObservation {
		t.Fatal("observation exceeds budget")
	}
}
