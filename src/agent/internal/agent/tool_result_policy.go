package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/model"
	"aico-agent/internal/agent/tokencounter"

	"github.com/tmc/langchaingo/llms"
)

const (
	ToolResultReasonInline         = "inline"
	ToolResultReasonIntrinsicLarge = "intrinsic_large"
	ToolResultReasonContextLarge   = "context_large"
	ToolResultReasonProcessingFail = "processing_failed"
	// ToolResultReasonCompleteTooLarge reports a result that must be seen in
	// full but cannot fit the model context even after compaction.
	ToolResultReasonCompleteTooLarge = "complete_result_too_large"

	toolResultInlineMaxBytes     = 8 * 1024
	toolResultInlineMaxTokens    = 2_000
	toolResultPreviewTargetToken = 1_200
	toolResultMinimumObservation = 96
	toolResultSoftLimitPercent   = 80
	// Deterministic pruning cleans to historicalPruneTargetNum /
	// historicalPruneTargetDen of its own trigger. The gap is hysteresis:
	// cleaning down to the trigger itself would re-trigger next turn. With the
	// default 0.5 trigger, a 10,000-token usable budget prunes at 5,000 and
	// cleans to 4,285. Kept as an integer ratio rather than a float so the
	// product is not truncated a token low.
	historicalPruneTargetNum   = 6
	historicalPruneTargetDen   = 7
	toolResultProjectionTopK   = 3
	toolResultProjectionFields = 24
	toolResultProjectionDepth  = 5
	toolResultProjectionRunes  = 256
)

var ErrToolResultRecoveryTextTooLarge = errors.New("tool result recovery text exceeds context budget")

type ToolResultPrepareInput struct {
	Call            ToolCall
	Result          ToolResult
	ActionCompleted bool
	ContextManager  *contextmanager.ContextManager
	ModelSpec       model.ModelSpec
	CallOptions     []llms.CallOption
}

type PreparedToolResult struct {
	Content              string
	ArtifactPath         string
	OriginalBytes        int64
	OriginalChars        int
	EstimatedTokens      int
	Complete             bool
	ArtifactComplete     bool
	Reason               string
	Summary              string
	ActionCompleted      bool
	ObservationComplete  bool
	ProcessingErrorCode  string
	ArtifactStoreError   string
	RequiresComplete     bool
	ContextBytes         int
	ContextTokens        int
	ProcessingDurationMs int64
}

type ToolResultPolicy interface {
	Prepare(context.Context, ToolResultPrepareInput) (PreparedToolResult, error)
}

type defaultToolResultPolicy struct{}

func NewToolResultPolicy() ToolResultPolicy {
	return defaultToolResultPolicy{}
}

func (defaultToolResultPolicy) Prepare(_ context.Context, input ToolResultPrepareInput) (prepared PreparedToolResult, err error) {
	startedAt := time.Now()
	defer func() {
		prepared.ContextBytes = len(prepared.Content)
		prepared.ContextTokens = tokencounter.EstimateTextTokens(prepared.Content)
		prepared.ProcessingDurationMs = time.Since(startedAt).Milliseconds()
	}()
	output := input.Result.Output
	prepared = PreparedToolResult{
		Content:             output,
		OriginalBytes:       int64(len(output)),
		OriginalChars:       utf8.RuneCountInString(output),
		EstimatedTokens:     tokencounter.EstimateTextTokens(output),
		Complete:            true,
		ActionCompleted:     input.ActionCompleted || input.Result.Error == nil,
		ObservationComplete: true,
		Reason:              ToolResultReasonInline,
	}
	if input.Call.Spec.RequiresCompleteResult {
		return prepareCompleteToolResult(input, prepared), nil
	}
	intrinsicLarge := len(output) > toolResultInlineMaxBytes || prepared.EstimatedTokens > toolResultInlineMaxTokens
	// The minimum observation budget must also apply to inline results.
	// Replacing a tiny result with a larger recovery notice hides the answer
	// and can create an endless chain of artifact recovery calls.
	availableTokens := availableToolResultTokens(input)
	contextLarge := availableTokens >= 0 && prepared.EstimatedTokens > max(toolResultMinimumObservation, availableTokens)
	if !intrinsicLarge && !contextLarge {
		prepared.Summary = projectToolResult(input.Call, output, 256)
		return prepared, nil
	}

	prepared.Complete = false
	prepared.ObservationComplete = false
	prepared.Reason = ToolResultReasonContextLarge
	contentBudget := toolResultInlineMaxTokens
	if availableTokens >= 0 {
		contentBudget = min(toolResultInlineMaxTokens, max(toolResultMinimumObservation, availableTokens))
	}
	if intrinsicLarge {
		prepared.Reason = ToolResultReasonIntrinsicLarge
	}
	previewBudget := min(toolResultPreviewTargetToken, contentBudget)
	preview, structured := projectToolResultWithKind(input.Call, output, previewBudget)
	prepared.Summary = boundTextToTokens(preview, 256)
	sensitive := toolResultIsSensitive(toolCallName(input.Call))
	if input.ContextManager != nil && !sensitive {
		mimeType := "text/plain"
		if structured {
			mimeType = "application/json"
		}
		stored, err := input.ContextManager.StoreArtifact(mimeType, []byte(output), contextmanager.ArtifactMetadata{
			ToolName:   input.Call.Spec.Name,
			ToolCallID: input.Call.Action.ToolID,
			Sensitive:  false,
		})
		if err == nil {
			prepared.ArtifactPath = stored.Path
			prepared.ArtifactComplete = stored.Complete
		} else {
			prepared.ProcessingErrorCode = "tool_result_persistence_failed"
			prepared.ArtifactStoreError = classifyArtifactStoreError(err)
		}
	}
	content := boundedToolResultObservation(input.Call, prepared, preview, contentBudget)
	if tokencounter.EstimateTextTokens(content) > contentBudget {
		prepared.Content = ""
		return prepared, fmt.Errorf("%w: budget=%d", ErrToolResultRecoveryTextTooLarge, contentBudget)
	}
	prepared.Content = content
	return prepared, nil
}

func classifyArtifactStoreError(err error) string {
	switch {
	case errors.Is(err, contextmanager.ErrArtifactTooLarge):
		return "artifact_too_large"
	case errors.Is(err, contextmanager.ErrArtifactSessionFull):
		return "artifact_session_full"
	default:
		return "artifact_store_unavailable"
	}
}

func failedPreparedToolResult(result ToolResult, actionCompleted bool) PreparedToolResult {
	content := `{"status":"observation_incomplete","code":"tool_result_processing_failed","action_completed":true,"observation_complete":false,"message":"Tool action completed, but the result could not be prepared."}`
	completed := actionCompleted || result.Error == nil
	if !completed {
		content = `{"status":"observation_incomplete","code":"tool_result_processing_failed","action_completed":false,"observation_complete":false,"message":"The tool result could not be prepared."}`
	}
	content = boundTextToTokens(content, toolResultMinimumObservation)
	return PreparedToolResult{
		Content:             content,
		OriginalBytes:       int64(len(result.Output)),
		OriginalChars:       utf8.RuneCountInString(result.Output),
		EstimatedTokens:     tokencounter.EstimateTextTokens(result.Output),
		Complete:            false,
		Reason:              ToolResultReasonProcessingFail,
		Summary:             "tool result preparation failed",
		ActionCompleted:     completed,
		ObservationComplete: false,
		ProcessingErrorCode: "tool_result_processing_failed",
		ContextBytes:        len(content),
		ContextTokens:       tokencounter.EstimateTextTokens(content),
	}
}

func toolResultIsSensitive(toolName string) bool {
	switch strings.TrimSpace(toolName) {
	case toolBridgeClipboard, toolBridgeCalendar, toolBridgeContacts, toolBridgeNotification:
		return true
	default:
		return false
	}
}

func toolCallName(call ToolCall) string {
	if name := strings.TrimSpace(call.Spec.Name); name != "" {
		return name
	}
	return strings.TrimSpace(call.Action.Tool)
}

func availableToolResultTokens(input ToolResultPrepareInput) int {
	softLimit := toolResultSoftLimit(input)
	if softLimit < 0 {
		return -1
	}
	if softLimit == 0 {
		return 0
	}
	currentTokens := estimateActivePromptTokens(input.ContextManager, input.CallOptions)
	return max(0, softLimit-currentTokens)
}

// prepareCompleteToolResult handles tools whose output is only useful in full.
// Such a result is never summarized to fit the space currently left: it is
// appended whole, and ContextBudgetGuard prunes or compacts older context
// before the next model call. Only a result that could not fit even after
// that reduction is refused, with an explicit observation instead of a
// misleading partial preview.
func prepareCompleteToolResult(input ToolResultPrepareInput, prepared PreparedToolResult) PreparedToolResult {
	limit := completeToolResultTokenLimit(input)
	if limit < 0 || prepared.EstimatedTokens <= limit {
		prepared.RequiresComplete = true
		prepared.Summary = projectToolResult(input.Call, input.Result.Output, 256)
		return prepared
	}

	content, _ := json.Marshal(map[string]any{
		"status":               "observation_incomplete",
		"code":                 ToolResultReasonCompleteTooLarge,
		"action_completed":     prepared.ActionCompleted,
		"observation_complete": false,
		"result_tokens":        prepared.EstimatedTokens,
		"context_limit_tokens": limit,
		"message":              "This result must be read in full but does not fit the model context even after compaction, together with complete results already loaded this turn, so it was not loaded. Do not retry the same call; continue with what is already loaded, request a smaller unit (for a skill, one of its linked files), or ask the user to split it.",
	})
	prepared.Content = string(content)
	prepared.Complete = false
	prepared.ObservationComplete = false
	prepared.Reason = ToolResultReasonCompleteTooLarge
	prepared.Summary = fmt.Sprintf("result not loaded: %d tokens exceed the %d-token context limit", prepared.EstimatedTokens, limit)
	return prepared
}

// completeToolResultTokenLimit is the largest complete result that could fit
// after pruning and compaction: the soft input limit minus tool schemas and
// the complete results already loaded in the current turn. Those results
// cannot be pruned, and compaction retains the whole current turn, so they
// stay in the next request. Everything else is reducible and is deliberately
// not subtracted; the soft limit's headroom below the usable budget covers the
// system prompt, the compaction summary, and the current user message. It
// returns -1 when the context window is unknown, in which case the tool's own
// output bound is the only limit.
func completeToolResultTokenLimit(input ToolResultPrepareInput) int {
	softLimit := toolResultSoftLimit(input)
	if softLimit < 0 {
		return -1
	}
	var options llms.CallOptions
	for _, option := range input.CallOptions {
		if option != nil {
			option(&options)
		}
	}
	pinned := currentTurnCompleteResultTokens(input.ContextManager)
	return max(0, softLimit-tokencounter.EstimateToolSchemaTokens(options)-pinned)
}

// currentTurnCompleteResultTokens estimates the complete-required results
// already loaded since the latest user message, matching the current turn that
// pruning and compaction use.
func currentTurnCompleteResultTokens(manager *contextmanager.ContextManager) int {
	if manager == nil {
		return 0
	}
	messageList, _ := manager.MessageSnapshot()
	total := 0
	for i := len(messageList) - 1; i >= 0; i-- {
		message := messageList[i]
		if message.Role == messages.MessageRoleUser {
			break
		}
		for _, result := range message.ToolResults {
			if result.Meta != nil && result.Meta.RequiresComplete {
				total += tokencounter.EstimateTextTokens(result.Content)
			}
		}
	}
	return total
}

// toolResultSoftLimit returns the soft input limit for the next model request,
// or -1 when the model's context window is unknown.
func toolResultSoftLimit(input ToolResultPrepareInput) int {
	contextWindow := input.ModelSpec.ContextWindow
	if contextWindow <= 0 {
		return -1
	}
	maxResponseTokens := input.ModelSpec.MaxOutput
	var options llms.CallOptions
	for _, option := range input.CallOptions {
		if option != nil {
			option(&options)
		}
	}
	if options.MaxTokens > 0 {
		maxResponseTokens = options.MaxTokens
	}
	return max(0, toolResultSoftInputLimit(contextWindow, maxResponseTokens))
}

func estimateActivePromptTokens(manager *contextmanager.ContextManager, options []llms.CallOption) int {
	var callOptions llms.CallOptions
	for _, option := range options {
		if option != nil {
			option(&callOptions)
		}
	}
	return activePromptTokens(manager, callOptions)
}

// activeMessageTokens excludes schema overhead from message-only prune budgets.
// Usage includes schemas, so remove their estimate without re-estimating messages.
func activeMessageTokens(manager *contextmanager.ContextManager, options llms.CallOptions) int {
	return max(0, activePromptTokens(manager, options)-tokencounter.EstimateToolSchemaTokens(options))
}

func activePromptTokens(manager *contextmanager.ContextManager, options llms.CallOptions) int {
	if manager == nil {
		return tokencounter.EstimateToolSchemaTokens(options)
	}
	// Provider input usage already includes tool schemas and prompt overhead.
	// Only add the schema estimate before the first measured response (including
	// after a context rewrite, which invalidates the old usage baseline).
	state := manager.TokenSnapshot()
	tokens := state.TokenCount
	if !state.HasTokenUsage {
		tokens += tokencounter.EstimateToolSchemaTokens(options)
	}
	return tokens
}

func inputContextBudget(contextWindow, maxResponseTokens int) int {
	if contextWindow <= 0 {
		return 0
	}
	if maxResponseTokens <= 0 {
		return contextWindow
	}
	if maxResponseTokens >= contextWindow {
		return 1
	}
	return contextWindow - maxResponseTokens
}

func toolResultUsableInputBudget(contextWindow, maxResponseTokens int) int {
	inputBudget := inputContextBudget(contextWindow, maxResponseTokens)
	if inputBudget <= 0 {
		return 0
	}
	safetyMargin := max(1_024, contextWindow*2/100)
	return max(0, inputBudget-safetyMargin)
}

func toolResultSoftInputLimit(contextWindow, maxResponseTokens int) int {
	return toolResultUsableInputBudget(contextWindow, maxResponseTokens) * toolResultSoftLimitPercent / 100
}

// conversationCompactionTrigger returns the token usage at which conversation
// compaction runs, as the given fraction of the usable input budget. The
// fraction comes from agent.context_compaction_threshold; callers should pass
// Config.ContextCompactionThresholdOrDefault so an unset value still gets the
// default.
//
// Compaction has no token target: Compactor.Compact reduces the transcript
// structurally via ProtectRule (head/tail retention plus an LLM summary of the
// middle), so the resulting size follows from the summary rather than a budget.
func conversationCompactionTrigger(usableInputBudget int, threshold float64) (trigger int, enabled bool) {
	if usableInputBudget <= 0 {
		return 0, false
	}
	if threshold <= 0 {
		threshold = defaultContextCompactionThreshold
	}
	trigger = int(float64(usableInputBudget) * threshold)
	if trigger <= 0 {
		return 0, false
	}
	return trigger, true
}

// historicalPruneBudgets returns the trigger and target for deterministic
// cleanup of stale state and older tool exchanges, both as token counts
// derived from the given fraction of the usable input budget. The fraction comes
// from agent.context_prune_threshold; callers should pass
// Config.ContextPruneThresholdOrDefault, which also caps it at the compaction
// threshold so this cheap pass runs first.
//
// Unlike compaction, pruning does have a real target: PruneHistorical bounds
// tool results until the transcript fits it.
func historicalPruneBudgets(usableInputBudget int, threshold float64) (trigger int, target int, enabled bool) {
	if usableInputBudget <= 0 {
		return 0, 0, false
	}
	if threshold <= 0 {
		threshold = defaultContextPruneThreshold
	}
	trigger = int(float64(usableInputBudget) * threshold)
	if trigger <= 0 {
		return 0, 0, false
	}
	target = trigger * historicalPruneTargetNum / historicalPruneTargetDen
	if target <= 0 {
		// A trigger of 1 token leaves no room for hysteresis below it; pruning
		// cannot make progress, so report disabled rather than thrash.
		return 0, 0, false
	}
	return trigger, target, true
}

func boundedToolResultObservation(call ToolCall, prepared PreparedToolResult, preview string, contentTokens int) string {
	if contentTokens <= 0 {
		contentTokens = toolResultMinimumObservation
	}
	toolName := toolCallName(call)
	if toolName == "" {
		toolName = "tool"
	}
	action := summarizeToolAction(call)
	if action == "" {
		action = "completed"
	}

	var optional strings.Builder
	if prepared.ProcessingErrorCode != "" {
		failureState, _ := json.Marshal(map[string]any{
			"status":               "observation_incomplete",
			"code":                 prepared.ProcessingErrorCode,
			"action_completed":     prepared.ActionCompleted,
			"observation_complete": prepared.ObservationComplete,
			"artifact_store_error": prepared.ArtifactStoreError,
			"message":              "Tool action completed, but the full result could not be persisted.",
		})
		optional.Write(failureState)
		optional.WriteByte('\n')
	}
	// Preserve useful output before descriptive metadata when space is tight.
	if strings.TrimSpace(preview) != "" {
		optional.WriteString(preview)
		if !strings.HasSuffix(preview, "\n") {
			optional.WriteByte('\n')
		}
	}

	fmt.Fprintf(&optional, "[%s] %s\n", toolName, action)
	fmt.Fprintf(
		&optional,
		"Tool action completed; model-visible result is partial (%d chars, %d bytes).\n",
		prepared.OriginalChars,
		prepared.OriginalBytes,
	)

	var recovery strings.Builder
	if prepared.ArtifactPath != "" {
		if prepared.ArtifactComplete {
			fmt.Fprintf(&recovery, "Full result file: %s\n", prepared.ArtifactPath)
		} else {
			fmt.Fprintf(&recovery, "Saved partial result file: %s\n", prepared.ArtifactPath)
		}
		recovery.WriteString("Read only if needed. Use bounded fq queries or byte ranges; never use cat or print the whole artifact file.\n")
	} else {
		optional.WriteString("Full result is unavailable; output was bounded before entering active context.\n")
	}

	mandatory := strings.TrimSpace(recovery.String())
	if mandatory == "" {
		return strings.TrimSpace(boundTextToTokens(optional.String(), contentTokens))
	}
	return combineMandatoryToolResultText(mandatory, optional.String(), contentTokens)
}

func combineMandatoryToolResultText(mandatory, optional string, maxTokens int) string {
	mandatory = strings.TrimSpace(mandatory)
	optional = strings.TrimSpace(optional)
	if mandatory == "" || optional == "" {
		if mandatory != "" {
			return mandatory
		}
		return boundTextToTokens(optional, maxTokens)
	}
	if maxTokens <= 0 || tokencounter.EstimateTextTokens(mandatory) >= maxTokens {
		return mandatory
	}

	runes := []rune(optional)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		candidate := mandatory + "\n" + string(runes[:mid])
		if tokencounter.EstimateTextTokens(candidate) <= maxTokens {
			low = mid
		} else {
			high = mid - 1
		}
	}
	if low == 0 {
		return mandatory
	}
	return strings.TrimSpace(mandatory + "\n" + string(runes[:low]))
}

func summarizeToolAction(call ToolCall) string {
	toolName := toolCallName(call)
	var input map[string]any
	_ = json.Unmarshal([]byte(strings.TrimSpace(call.Input)), &input)
	switch toolName {
	case "shell":
		if command, _ := input["command"].(string); strings.TrimSpace(command) != "" {
			return truncateToolResultRunes(strings.TrimSpace(command), 160)
		}
		if action, _ := input["action"].(string); strings.TrimSpace(action) != "" {
			if sessionID, _ := input["session_id"].(string); strings.TrimSpace(sessionID) != "" {
				return truncateToolResultRunes(strings.TrimSpace(action)+" "+strings.TrimSpace(sessionID), 160)
			}
			return truncateToolResultRunes(strings.TrimSpace(action), 160)
		}
	case "skill_read":
		if name, _ := input["name"].(string); strings.TrimSpace(name) != "" {
			return "read " + truncateToolResultRunes(strings.TrimSpace(name), 150)
		}
	}
	if action, _ := input["action"].(string); strings.TrimSpace(action) != "" {
		return truncateToolResultRunes(strings.TrimSpace(action), 160)
	}
	return "completed"
}

func projectToolResult(call ToolCall, output string, maxTokens int) string {
	projected, _ := projectToolResultWithKind(call, output, maxTokens)
	return projected
}

func projectToolResultWithKind(call ToolCall, output string, maxTokens int) (string, bool) {
	if maxTokens <= 0 || output == "" {
		return "", false
	}
	toolName := toolCallName(call)
	if toolName == toolBridgeClipboard {
		if projected, ok := projectClipboardToolResult(output, maxTokens); ok {
			return projected, true
		}
	}
	if toolName == "skill_read" {
		if projected, ok := projectSkillReadToolResult(call, output, maxTokens); ok {
			return projected, true
		}
	}
	if toolName == "shell" {
		if projected, ok := projectShellTextToolResult(output, maxTokens); ok {
			return projected, false
		}
	}
	if toolName == "web_search" || toolName == "wikipedia" || toolName == "web_scraper" {
		if projected, ok := projectWebToolResult(call, output, maxTokens); ok {
			return projected, true
		}
	}
	if projected, ok := projectToolSpecificJSONResult(call, output, maxTokens); ok {
		return projected, true
	}
	if projected, ok := projectJSONToolResult(output, maxTokens); ok {
		return projected, true
	}
	maxRunes := maxTokens * 3
	if maxRunes < 96 {
		maxRunes = 96
	}
	preview := truncateHeadTail(output, maxRunes)
	return boundTextToTokens(preview, maxTokens), false
}

func projectToolSpecificJSONResult(call ToolCall, output string, maxTokens int) (string, bool) {
	toolName := toolCallName(call)
	var requestFields []string
	switch toolName {
	case "recall_memory", "recall_session_chunks", "recall_device_memory":
		requestFields = []string{"chunk_ids", "terms", "tags", "entities", "types", "device_id", "limit"}
	case "inspect_episode":
		requestFields = []string{"id"}
	case toolBridgeCalendar:
		requestFields = []string{"action", "event_id", "title", "start_at", "end_at", "from", "to", "all_day", "location"}
	case toolBridgeContacts:
		requestFields = []string{"action", "contact_id", "query", "limit", "name", "organization"}
	case toolBridgeNotification:
		requestFields = []string{"action", "title", "schedule_at", "sound", "badge", "since", "generation", "limit"}
	default:
		return "", false
	}

	payload, ok := decodeToolResultJSON(output)
	if !ok {
		return "", false
	}
	projected := projectJSONValueWithLimits(payload, 0, 8, 128)
	root, ok := projected.(map[string]any)
	if !ok {
		root = map[string]any{"result": projected}
	}
	request := selectedToolInput(call.Input, requestFields...)
	if toolName == "inspect_episode" {
		if episodeID, ok := request["id"]; ok {
			root["episode_id"] = episodeID
		}
	} else if strings.HasPrefix(toolName, "recall_") {
		if len(request) > 0 {
			root["query"] = request
		}
	} else {
		for key, value := range request {
			root[key] = value
		}
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		return "", false
	}
	return boundTextToTokens(string(encoded), maxTokens), true
}

func decodeToolResultJSON(output string) (any, bool) {
	data := []byte(strings.TrimSpace(output))
	if !json.Valid(data) {
		return nil, false
	}
	var payload any
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.UseNumber()
	if err := decoder.Decode(&payload); err != nil {
		return nil, false
	}
	return payload, true
}

func selectedToolInput(input string, fields ...string) map[string]any {
	var payload map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(input)), &payload); err != nil {
		return nil
	}
	selected := make(map[string]any)
	for _, field := range fields {
		value, exists := payload[field]
		if !exists || toolProjectionValueEmpty(value) {
			continue
		}
		selected[field] = projectJSONValue(value, 0)
	}
	return selected
}

func toolProjectionValueEmpty(value any) bool {
	switch value := value.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(value) == ""
	case []any:
		return len(value) == 0
	default:
		return false
	}
}

func projectSkillReadToolResult(call ToolCall, output string, maxTokens int) (string, bool) {
	request := selectedToolInput(call.Input, "name", "file_path")
	skillName, _ := request["name"].(string)
	if strings.TrimSpace(skillName) == "" {
		return "", false
	}
	fileName, _ := request["file_path"].(string)
	if strings.TrimSpace(fileName) == "" {
		fileName = "SKILL.md"
	}
	headings := make([]string, 0, 12)
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if !strings.HasPrefix(trimmed, "#") {
			continue
		}
		headings = append(headings, truncateToolResultRunes(trimmed, 160))
		if len(headings) >= 12 {
			break
		}
	}
	projection := map[string]any{
		"skill":    skillName,
		"file":     fileName,
		"bytes":    len(output),
		"chars":    utf8.RuneCountInString(output),
		"headings": headings,
		"preview":  truncateToolResultRunes(strings.TrimSpace(output), 768),
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return "", false
	}
	return boundTextToTokens(string(data), maxTokens), true
}

func projectWebToolResult(call ToolCall, output string, maxTokens int) (string, bool) {
	toolName := toolCallName(call)
	request := selectedToolInput(call.Input, "query", "url")
	if len(request) == 0 {
		trimmedInput := strings.TrimSpace(call.Input)
		if trimmedInput != "" && !strings.HasPrefix(trimmedInput, "{") {
			if toolName == "web_search" {
				request = map[string]any{"query": truncateToolResultRunes(trimmedInput, 256)}
			} else {
				request = map[string]any{"url": truncateToolResultRunes(trimmedInput, 512)}
			}
		}
	}
	if payload, ok := decodeToolResultJSON(output); ok {
		projected := projectJSONValue(payload, 0)
		root, isMap := projected.(map[string]any)
		if !isMap {
			root = map[string]any{"result": projected}
		}
		root["source"] = toolName
		for key, value := range request {
			root[key] = value
		}
		data, err := json.Marshal(root)
		if err == nil {
			return boundTextToTokens(string(data), maxTokens), true
		}
	}

	urls := make([]string, 0, toolResultProjectionTopK)
	headings := make([]string, 0, 12)
	answer := ""
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if answer == "" && strings.HasPrefix(trimmed, "Answer:") {
			answer = truncateToolResultRunes(strings.TrimSpace(strings.TrimPrefix(trimmed, "Answer:")), 512)
		}
		if len(urls) < toolResultProjectionTopK && (strings.HasPrefix(trimmed, "https://") || strings.HasPrefix(trimmed, "http://")) {
			urls = append(urls, truncateToolResultRunes(trimmed, 512))
		}
		if len(headings) < 12 && (strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, "Page Title:") || strings.HasPrefix(trimmed, "Page URL:")) {
			headings = append(headings, truncateToolResultRunes(trimmed, 256))
		}
	}
	projection := map[string]any{
		"source":  toolName,
		"bytes":   len(output),
		"urls":    urls,
		"results": headings,
		"preview": truncateHeadTail(strings.TrimSpace(output), 768),
	}
	if answer != "" {
		projection["answer"] = answer
	}
	for key, value := range request {
		projection[key] = value
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return "", false
	}
	return boundTextToTokens(string(data), maxTokens), true
}

type shellTextSection struct {
	label   string
	start   int
	content string
}

func projectShellTextToolResult(output string, maxTokens int) (string, bool) {
	sections := splitShellTextSections(output)
	if len(sections) == 0 {
		return "", false
	}
	maxRunes := max(96, maxTokens*3)
	prefix := strings.TrimSpace(output[:sections[0].start])
	criticalLines := shellCriticalStatusLines(output)
	weight := 0
	if prefix != "" {
		weight++
	}
	if len(criticalLines) > 0 {
		weight += 3
	}
	for _, section := range sections {
		if section.label == "Stderr" {
			weight += 3
		} else {
			weight += 2
		}
	}
	if weight <= 0 {
		return "", false
	}

	var builder strings.Builder
	if prefix != "" {
		builder.WriteString(truncateHeadTail(prefix, max(64, maxRunes/weight)))
		builder.WriteByte('\n')
	}
	if len(criticalLines) > 0 {
		builder.WriteString("Key status:\n")
		builder.WriteString(truncateHeadTail(strings.Join(criticalLines, "\n"), max(192, maxRunes*3/weight)))
		builder.WriteByte('\n')
	}
	for _, section := range sections {
		sectionWeight := 2
		if section.label == "Stderr" {
			sectionWeight = 3
		}
		budget := max(96, maxRunes*sectionWeight/weight)
		fmt.Fprintf(&builder, "%s:\n", section.label)
		builder.WriteString(truncateHeadTail(strings.TrimSpace(section.content), budget))
		builder.WriteByte('\n')
	}
	return boundTextToTokens(strings.TrimSpace(builder.String()), maxTokens), true
}

func shellCriticalStatusLines(output string) []string {
	lines := make([]string, 0, 24)
	seen := make(map[string]struct{})
	for _, line := range strings.Split(output, "\n") {
		trimmed := strings.TrimSpace(line)
		lower := strings.ToLower(trimmed)
		critical := strings.HasPrefix(trimmed, "--- FAIL:") ||
			trimmed == "FAIL" || trimmed == "PASS" ||
			strings.HasPrefix(trimmed, "FAIL\t") ||
			strings.HasPrefix(lower, "exit status ") ||
			strings.HasPrefix(lower, "error:") ||
			(strings.Contains(lower, " passed") && strings.Contains(lower, " failed"))
		if !critical {
			continue
		}
		trimmed = truncateToolResultRunes(trimmed, 256)
		if _, exists := seen[trimmed]; exists {
			continue
		}
		seen[trimmed] = struct{}{}
		lines = append(lines, trimmed)
		if len(lines) >= 24 {
			break
		}
	}
	return lines
}

func splitShellTextSections(output string) []shellTextSection {
	sections := make([]shellTextSection, 0, 2)
	for _, label := range []string{"Stderr", "Stdout"} {
		marker := label + ":\n"
		for offset := 0; offset < len(output); {
			index := strings.Index(output[offset:], marker)
			if index < 0 {
				break
			}
			index += offset
			if index == 0 || output[index-1] == '\n' {
				sections = append(sections, shellTextSection{
					label: label,
					start: index,
				})
				break
			}
			offset = index + len(marker)
		}
	}
	if len(sections) == 0 {
		return nil
	}
	sort.Slice(sections, func(i, j int) bool { return sections[i].start < sections[j].start })
	for i := range sections {
		contentStart := sections[i].start + len(sections[i].label) + len(":\n")
		contentEnd := len(output)
		if i+1 < len(sections) {
			contentEnd = sections[i+1].start
		}
		sections[i].content = output[contentStart:contentEnd]
	}
	return sections
}

func projectJSONToolResult(output string, maxTokens int) (string, bool) {
	payload, ok := decodeToolResultJSON(output)
	if !ok {
		return "", false
	}
	projected := projectJSONValue(payload, 0)
	if values, ok := payload.([]any); ok {
		projected = map[string]any{
			"items":       projected,
			"items_total": len(values),
		}
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		return "", false
	}
	return boundTextToTokens(string(encoded), maxTokens), true
}

func projectJSONValue(value any, depth int) any {
	return projectJSONValueWithLimits(value, depth, toolResultProjectionFields, toolResultProjectionRunes)
}

func projectJSONValueWithLimits(value any, depth, maxFields, maxRunes int) any {
	if depth >= toolResultProjectionDepth {
		return "[nested value omitted]"
	}
	switch value := value.(type) {
	case map[string]any:
		keys := make([]string, 0, len(value))
		for key := range value {
			keys = append(keys, key)
		}
		sort.Slice(keys, func(i, j int) bool {
			left, right := jsonProjectionPriority(keys[i]), jsonProjectionPriority(keys[j])
			if left != right {
				return left < right
			}
			return keys[i] < keys[j]
		})
		if len(keys) > maxFields {
			keys = keys[:maxFields]
		}

		projected := make(map[string]any, len(keys)+1)
		for _, key := range keys {
			field := value[key]
			projected[key] = projectJSONValueWithLimits(field, depth+1, maxFields, maxRunes)
			if values, ok := field.([]any); ok {
				totalKey := key + "_total"
				if _, exists := value[totalKey]; !exists {
					projected[totalKey] = len(values)
				}
			}
		}
		if len(value) > len(keys) {
			projected["fields_total"] = len(value)
		}
		return projected
	case []any:
		limit := min(len(value), toolResultProjectionTopK)
		projected := make([]any, 0, limit)
		for _, item := range value[:limit] {
			projected = append(projected, projectJSONValueWithLimits(item, depth+1, maxFields, maxRunes))
		}
		return projected
	case string:
		return truncateToolResultRunes(value, maxRunes)
	default:
		return value
	}
}

func jsonProjectionPriority(key string) int {
	switch key {
	case "ok":
		return 0
	case "status":
		return 1
	case "error":
		return 2
	case "code":
		return 3
	case "exit_code":
		return 4
	case "exit_error":
		return 5
	case "id", "event_id", "contact_id", "notification_id", "episode_id":
		return 6
	case "permission_status":
		return 7
	case "count", "total", "items_total", "results_total", "events_total", "contacts_total", "notifications_total":
		return 8
	case "path":
		return 9
	case "url":
		return 10
	case "cursor":
		return 11
	case "has_more":
		return 12
	case "continuation":
		return 13
	case "results", "items", "entries", "data", "events", "contacts", "notifications":
		return 14
	default:
		return 100
	}
}

func projectClipboardToolResult(output string, maxTokens int) (string, bool) {
	var payload struct {
		OK     *bool  `json:"ok"`
		Status string `json:"status"`
		Error  any    `json:"error"`
		Text   string `json:"text"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		return "", false
	}
	projection := map[string]any{
		"text_chars": utf8.RuneCountInString(payload.Text),
	}
	if payload.OK != nil {
		projection["ok"] = *payload.OK
	}
	if strings.TrimSpace(payload.Status) != "" {
		projection["status"] = payload.Status
	}
	if payload.Error != nil {
		projection["error"] = payload.Error
	}
	if payload.Text != "" {
		projection["text_preview"] = truncateToolResultRunes(payload.Text, 256)
	}
	data, err := json.Marshal(projection)
	if err != nil {
		return "", false
	}
	return boundTextToTokens(string(data), maxTokens), true
}

func truncateHeadTail(text string, maxRunes int) string {
	runes := []rune(text)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return text
	}
	marker := fmt.Sprintf("\n... %d chars omitted ...\n", len(runes)-maxRunes)
	head := maxRunes / 2
	tail := maxRunes - head
	return string(runes[:head]) + marker + string(runes[len(runes)-tail:])
}

func truncateToolResultRunes(text string, maxRunes int) string {
	runes := []rune(text)
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return text
	}
	return string(runes[:maxRunes]) + "…"
}

func boundTextToTokens(text string, maxTokens int) string {
	if maxTokens <= 0 || tokencounter.EstimateTextTokens(text) <= maxTokens {
		return text
	}
	runes := []rune(text)
	low, high := 0, len(runes)
	for low < high {
		mid := (low + high + 1) / 2
		candidate := string(runes[:mid])
		if tokencounter.EstimateTextTokens(candidate) <= maxTokens {
			low = mid
		} else {
			high = mid - 1
		}
	}
	return string(runes[:low])
}
