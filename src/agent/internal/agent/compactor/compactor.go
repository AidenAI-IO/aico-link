package compactor

import (
	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/executor"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/model"
	"aico-agent/internal/agent/tokencounter"
	"aico-agent/internal/logging"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/tmc/langchaingo/llms"
)

// ChunkWriter handles writing conversation chunks during compaction.
type ChunkWriter interface {
	WriteChunk(ctx context.Context, sessionID string, messages []messages.Message, summary string) error
}

type ProtectRule struct {
	HeadN int
	TailN int
}

var DefaultProtectRule = ProtectRule{
	HeadN: 2,
	TailN: 3,
}

const (
	historicalToolResultPruneReason    = "historical_prune"
	currentTurnToolExchangePruneReason = "current_turn_prune"
	currentTurnToolExchangeProtectN    = 3
)

type CompactionStats struct {
	TokensBefore                int
	TokensAfter                 int
	ConversationSummaryRequired bool
}

type HistoricalPruneStats struct {
	HistoricalStatesDropped        int
	HistoricalToolResultsPruned    int
	CurrentTurnStatesDropped       int
	CurrentTurnToolExchangesPruned int
	TokensBefore                   int
	TokensAfter                    int
}

type Compactor struct {
	ProtectRule ProtectRule
	Model       model.Model
	// Location is the user's configured timezone. Summaries resolve relative
	// times such as "tomorrow" against message timestamps shown in it; nil
	// uses the host's local time.
	Location            *time.Location
	lastCompactionStats CompactionStats
	lastPruneStats      HistoricalPruneStats
}

func NewCompactor(protectRule ProtectRule, model model.Model) *Compactor {
	validateProtectRule(protectRule)
	return &Compactor{
		ProtectRule: protectRule,
		Model:       model,
	}
}

func validateProtectRule(protectRule ProtectRule) {
	if protectRule.HeadN <= 0 || protectRule.TailN <= 0 {
		logging.Fatalf("agent", "compactor", "headN and tailN must be greater than 0")
	}
}

func (c *Compactor) LastCompactionStats() CompactionStats {
	if c == nil {
		return CompactionStats{}
	}
	return c.lastCompactionStats
}

func (c *Compactor) LastPruneStats() HistoricalPruneStats {
	if c == nil {
		return HistoricalPruneStats{}
	}
	return c.lastPruneStats
}

func estimateMessageListTokenUsage(messageList []messages.Message) int {
	return tokencounter.EstimateMessagesTokens(messageList)
}

func (c *Compactor) Compact(ctx context.Context, session *contextmanager.ContextManager, chunkWriter ChunkWriter) (*contextmanager.ContextManager, bool, error) {
	if session == nil {
		return nil, false, nil
	}
	messageList, sourceVersion := session.MessageSnapshot()
	tokensBefore := session.TokenCount()
	c.lastCompactionStats = CompactionStats{TokensBefore: tokensBefore, TokensAfter: tokensBefore}

	HeadN := c.ProtectRule.HeadN
	TailN := c.ProtectRule.TailN
	for i, m := range slices.Backward(messageList) {
		if m.Role == messages.MessageRoleUser {
			currentTurnLength := len(messageList) - i
			if currentTurnLength > TailN {
				TailN = currentTurnLength
			}
			break
		}
	}

	// adjust headN so that headN is assitant, tool_result or user_message, make sure headN not breaking tool_call pairs
	for i := HeadN - 1; i < len(messageList); i++ {
		if messageList[i].Role == messages.MessageRoleAssistant || messageList[i].Role == messages.MessageRoleToolResult || messageList[i].Role == messages.MessageRoleUser {
			break
		}
		HeadN++
	}

	// adjust tailN so that N - tailN - 1 is tool_result or user_message
	for i := len(messageList) - TailN - 1; i >= 0; i-- {
		if messageList[i].Role == messages.MessageRoleAssistant || messageList[i].Role == messages.MessageRoleToolResult || messageList[i].Role == messages.MessageRoleUser {
			break
		}
		TailN++
	}

	if HeadN+TailN >= len(messageList) {
		return nil, false, nil
	}

	heads := messageList[:HeadN]
	tails := messageList[len(messageList)-TailN:]
	mids := messageList[HeadN : len(messageList)-TailN]
	// compact mids into one single user message
	c.lastCompactionStats.ConversationSummaryRequired = true
	summary, err := c.generateSummary(ctx, mids)
	if err != nil {
		return nil, false, err
	}
	if summary == "" {
		return nil, false, fmt.Errorf("failed to generate summary")
	}

	// Write chunk for the compacted messages before creating revision
	if chunkWriter != nil {
		if err := chunkWriter.WriteChunk(ctx, session.GetSessionID(), mids, summary); err != nil {
			return nil, false, fmt.Errorf("write chunk: %w", err)
		}
	}

	// assemble
	formattedSummary := c.formatSummary(summary)
	newMessageList := append(append([]messages.Message(nil), heads...), messages.Message{
		Role:    messages.MessageRoleUser,
		Content: formattedSummary,
	})
	newMessageList = append(newMessageList, tails...)
	c.lastCompactionStats.TokensAfter = estimateMessageListTokenUsage(newMessageList)
	return newContextRevision(session, newMessageList, sourceVersion)
}

// PruneHistorical removes expired state snapshots and bounds old tool results
// without generating an LLM conversation summary. A changed context is written
// as a new revision so provider response anchors cannot refer to the old shape.
func (c *Compactor) PruneHistorical(session *contextmanager.ContextManager, targetTokens int) (*contextmanager.ContextManager, bool, error) {
	return c.pruneForBudget(session, targetTokens, false, currentTurnToolExchangeProtectN)
}

// PruneForBudget performs the historical cleanup and also bounds the active
// user turn. It is intended for checks made between tool iterations, where a
// single run may otherwise grow indefinitely without another user message.
func (c *Compactor) PruneForBudget(session *contextmanager.ContextManager, targetTokens int) (*contextmanager.ContextManager, bool, error) {
	return c.pruneForBudget(session, targetTokens, true, currentTurnToolExchangeProtectN)
}

// PruneForHardBudget is the emergency variant used when the next request would
// exceed the provider input budget. Unlike normal pruning, no completed tool
// exchange remains protected if removing it is required to fit.
func (c *Compactor) PruneForHardBudget(session *contextmanager.ContextManager, targetTokens int) (*contextmanager.ContextManager, bool, error) {
	return c.pruneForBudget(session, targetTokens, true, 0)
}

func (c *Compactor) pruneForBudget(session *contextmanager.ContextManager, targetTokens int, includeCurrentTurn bool, protectRecent int) (*contextmanager.ContextManager, bool, error) {
	if session == nil {
		return nil, false, nil
	}
	messageList, sourceVersion := session.MessageSnapshot()
	tokensBefore := session.TokenCount()
	c.lastPruneStats = HistoricalPruneStats{TokensBefore: tokensBefore, TokensAfter: tokensBefore}
	if targetTokens <= 0 {
		return nil, false, nil
	}

	var dropped int
	messageList, dropped = pruneHistoricalStates(messageList)
	c.lastPruneStats.HistoricalStatesDropped = dropped
	if includeCurrentTurn {
		messageList, dropped = pruneCurrentTurnStates(messageList)
		c.lastPruneStats.CurrentTurnStatesDropped = dropped
	}

	var pruned int
	messageList, pruned = compactHistoricalToolResults(messageList, targetTokens)
	c.lastPruneStats.HistoricalToolResultsPruned = pruned
	if includeCurrentTurn {
		messageList, pruned = compactCurrentTurnToolExchanges(messageList, targetTokens, protectRecent)
		c.lastPruneStats.CurrentTurnToolExchangesPruned = pruned
	}
	c.lastPruneStats.TokensAfter = estimateMessageListTokenUsage(messageList)
	if c.lastPruneStats.HistoricalStatesDropped == 0 &&
		c.lastPruneStats.HistoricalToolResultsPruned == 0 &&
		c.lastPruneStats.CurrentTurnStatesDropped == 0 &&
		c.lastPruneStats.CurrentTurnToolExchangesPruned == 0 {
		c.lastPruneStats.TokensAfter = tokensBefore
		return nil, false, nil
	}
	return newContextRevision(session, messageList, sourceVersion)
}

func newContextRevision(session *contextmanager.ContextManager, messageList []messages.Message, sourceVersion uint64) (*contextmanager.ContextManager, bool, error) {
	// A revision starts a fresh provider conversation. Retaining a Responses
	// response ID would chain the rewritten local transcript onto stale provider
	// state. The original session remains on disk for audit and recovery.
	//
	// InteractionsSteps are deliberately kept. They are not server-side state
	// pointers like a Responses ID: they are the Gemini thought and function-call
	// steps replayed inside the next request, and the provider validates every
	// replayed function call against the thought signature it was returned with.
	// Dropping them here would leave the retained tool exchanges in exactly the
	// shape Gemini rejects.
	for i := range messageList {
		messageList[i].ResponsesResponseID = ""
	}
	newManager, err := contextmanager.NewContextManagerRevisionFromMessageListAtVersion(session, messageList, sourceVersion)
	if err != nil {
		return nil, false, err
	}
	return newManager, true, nil
}

func pruneHistoricalStates(messageList []messages.Message) ([]messages.Message, int) {
	latestUserIndex := lastMessageIndexWithRole(messageList, messages.MessageRoleUser)
	if latestUserIndex < 0 {
		return messageList, 0
	}

	// State messages injected immediately before the current user input belong
	// to the active turn. State observations after that input do too. Every
	// earlier state is an expired snapshot and must not enter a later summary.
	currentTurnStart := latestUserIndex
	for currentTurnStart > 0 && messageList[currentTurnStart-1].Role == messages.MessageRoleState {
		currentTurnStart--
	}

	dropped := 0
	pruned := make([]messages.Message, 0, len(messageList))
	for i, message := range messageList {
		if i < currentTurnStart && message.Role == messages.MessageRoleState {
			dropped++
			continue
		}
		pruned = append(pruned, message)
	}
	if dropped == 0 {
		return messageList, 0
	}
	return pruned, dropped
}

func pruneCurrentTurnStates(messageList []messages.Message) ([]messages.Message, int) {
	latestUserIndex := lastMessageIndexWithRole(messageList, messages.MessageRoleUser)
	if latestUserIndex < 0 {
		return messageList, 0
	}

	latestStateIndex := -1
	for i := len(messageList) - 1; i > latestUserIndex; i-- {
		if messageList[i].Role == messages.MessageRoleState {
			latestStateIndex = i
			break
		}
	}
	if latestStateIndex < 0 {
		return messageList, 0
	}

	dropped := 0
	pruned := make([]messages.Message, 0, len(messageList))
	for i, message := range messageList {
		if i > latestUserIndex && i != latestStateIndex && message.Role == messages.MessageRoleState {
			dropped++
			continue
		}
		pruned = append(pruned, message)
	}
	if dropped == 0 {
		return messageList, 0
	}
	return pruned, dropped
}

func compactHistoricalToolResults(messageList []messages.Message, targetTokens int) ([]messages.Message, int) {
	if targetTokens <= 0 || estimateMessageListTokenUsage(messageList) <= targetTokens {
		return messageList, 0
	}
	latestUserIndex := lastMessageIndexWithRole(messageList, messages.MessageRoleUser)
	if latestUserIndex <= 0 {
		return messageList, 0
	}

	currentTokens := estimateMessageListTokenUsage(messageList)
	pruned := 0
	for messageIndex := 0; messageIndex < latestUserIndex; messageIndex++ {
		message := &messageList[messageIndex]
		if message.Role != messages.MessageRoleToolResult {
			continue
		}
		for resultIndex := range message.ToolResults {
			result := &message.ToolResults[resultIndex]
			if result.Meta != nil && result.Meta.Reason == historicalToolResultPruneReason {
				continue
			}
			call, _ := findHistoricalToolCall(messageList, result.ToolCallID, messageIndex)
			content, summary := historicalToolResultPlaceholder(*result, call)
			beforeTokens := tokencounter.EstimateTextTokens(result.Content)
			afterTokens := tokencounter.EstimateTextTokens(content)
			if afterTokens >= beforeTokens {
				continue
			}

			meta := result.Meta
			if meta == nil {
				meta = &messages.ToolResultMeta{Complete: true, ObservationComplete: true}
				result.Meta = meta
			}
			if meta.OriginalBytes == 0 {
				meta.OriginalBytes = int64(len(result.Content))
			}
			if meta.OriginalChars == 0 {
				meta.OriginalChars = utf8.RuneCountInString(result.Content)
			}
			if meta.EstimatedTokens == 0 {
				meta.EstimatedTokens = beforeTokens
			}
			if strings.TrimSpace(meta.Summary) == "" {
				meta.Summary = summary
			}
			meta.Complete = false
			meta.Reason = historicalToolResultPruneReason
			result.Content = content
			currentTokens += afterTokens - beforeTokens
			pruned++
			if currentTokens <= targetTokens {
				return messageList, pruned
			}
		}
	}
	return messageList, pruned
}

type completedToolExchange struct {
	callIndex   int
	resultIndex int
}

func compactCurrentTurnToolExchanges(messageList []messages.Message, targetTokens, protectRecent int) ([]messages.Message, int) {
	if targetTokens <= 0 || estimateMessageListTokenUsage(messageList) <= targetTokens {
		return messageList, 0
	}
	latestUserIndex := lastMessageIndexWithRole(messageList, messages.MessageRoleUser)
	if latestUserIndex < 0 {
		return messageList, 0
	}

	exchanges := make([]completedToolExchange, 0)
	for i := latestUserIndex + 1; i+1 < len(messageList); i++ {
		if !isCompletedToolExchange(messageList[i], messageList[i+1]) {
			continue
		}
		exchanges = append(exchanges, completedToolExchange{callIndex: i, resultIndex: i + 1})
		i++
	}
	prunableCount := len(exchanges) - max(0, protectRecent)
	if prunableCount <= 0 {
		return messageList, 0
	}

	currentTokens := estimateMessageListTokenUsage(messageList)
	changedExchanges := make(map[int]struct{}, prunableCount)
	for _, exchange := range exchanges[:prunableCount] {
		if currentTokens <= targetTokens {
			break
		}
		callMessage := messageList[exchange.callIndex]
		resultMessage := messageList[exchange.resultIndex]
		compactedCall, compactedResult, changed := compactCurrentTurnToolExchange(callMessage, resultMessage)
		if !changed {
			continue
		}
		beforeTokens := tokencounter.EstimateMessageTokens(callMessage) + tokencounter.EstimateMessageTokens(resultMessage)
		afterTokens := tokencounter.EstimateMessageTokens(compactedCall) + tokencounter.EstimateMessageTokens(compactedResult)
		if afterTokens >= beforeTokens {
			continue
		}
		messageList[exchange.callIndex] = compactedCall
		messageList[exchange.resultIndex] = compactedResult
		currentTokens += afterTokens - beforeTokens
		changedExchanges[exchange.callIndex] = struct{}{}
	}

	// A tiny placeholder per exchange would still grow without bound in an
	// unlimited run. If compacting every eligible pair cannot reach the target,
	// remove the oldest completed pairs entirely. The parent revision keeps the
	// full transcript for audit, while the active revision remains bounded.
	dropIndexes := make(map[int]struct{})
	for _, exchange := range exchanges[:prunableCount] {
		if currentTokens <= targetTokens {
			break
		}
		callTokens := tokencounter.EstimateMessageTokens(messageList[exchange.callIndex])
		resultTokens := tokencounter.EstimateMessageTokens(messageList[exchange.resultIndex])
		dropIndexes[exchange.callIndex] = struct{}{}
		dropIndexes[exchange.resultIndex] = struct{}{}
		currentTokens -= callTokens + resultTokens
		changedExchanges[exchange.callIndex] = struct{}{}
	}
	if len(dropIndexes) == 0 {
		return messageList, len(changedExchanges)
	}
	prunedMessages := make([]messages.Message, 0, len(messageList)-len(dropIndexes))
	for i, message := range messageList {
		if _, drop := dropIndexes[i]; drop {
			continue
		}
		prunedMessages = append(prunedMessages, message)
	}
	return prunedMessages, len(changedExchanges)
}

func isCompletedToolExchange(callMessage, resultMessage messages.Message) bool {
	if callMessage.Role != messages.MessageRoleToolCall || resultMessage.Role != messages.MessageRoleToolResult ||
		len(callMessage.ToolCalls) == 0 || len(callMessage.ToolCalls) != len(resultMessage.ToolResults) {
		return false
	}
	callIDs := make(map[string]struct{}, len(callMessage.ToolCalls))
	for _, call := range callMessage.ToolCalls {
		id := strings.TrimSpace(call.ID)
		if id == "" {
			return false
		}
		callIDs[id] = struct{}{}
	}
	for _, result := range resultMessage.ToolResults {
		id := strings.TrimSpace(result.ToolCallID)
		if _, ok := callIDs[id]; !ok {
			return false
		}
		delete(callIDs, id)
	}
	return len(callIDs) == 0
}

func compactCurrentTurnToolExchange(callMessage, resultMessage messages.Message) (messages.Message, messages.Message, bool) {
	for _, result := range resultMessage.ToolResults {
		if result.Meta != nil && result.Meta.Reason == currentTurnToolExchangePruneReason {
			return callMessage, resultMessage, false
		}
	}

	compactedCall := callMessage.Clone()
	compactedCall.Content = ""
	compactedCall.ResponsesReasoningItems = nil
	compactedCall.ResponsesResponseID = ""
	compactedCall.ResponsesOutputItems = nil
	compactedCall.ResponsesAssistantPhase = ""
	// InteractionsSteps stay because the provider still requires the thought
	// signatures that accompany the retained function calls; only the bulky tool
	// payloads below are replaced with placeholders.
	for i := range compactedCall.ToolCalls {
		compactedCall.ToolCalls[i].Arguments = `{}`
	}

	compactedResult := resultMessage.Clone()
	compactedResult.Content = ""
	compactedResult.ResponsesReasoningItems = nil
	compactedResult.ResponsesResponseID = ""
	compactedResult.ResponsesOutputItems = nil
	compactedResult.ResponsesAssistantPhase = ""
	for i := range compactedResult.ToolResults {
		result := &compactedResult.ToolResults[i]
		call, _ := toolCallByID(compactedCall.ToolCalls, result.ToolCallID)
		content, summary := currentTurnToolResultPlaceholder(*result, call)
		beforeTokens := tokencounter.EstimateTextTokens(result.Content)
		meta := result.Meta
		if meta == nil {
			meta = &messages.ToolResultMeta{ObservationComplete: true}
			result.Meta = meta
		}
		if meta.OriginalBytes == 0 {
			meta.OriginalBytes = int64(len(result.Content))
		}
		if meta.OriginalChars == 0 {
			meta.OriginalChars = utf8.RuneCountInString(result.Content)
		}
		if meta.EstimatedTokens == 0 {
			meta.EstimatedTokens = beforeTokens
		}
		if strings.TrimSpace(meta.Summary) == "" {
			meta.Summary = summary
		}
		meta.Complete = false
		meta.Reason = currentTurnToolExchangePruneReason
		result.Content = content
	}
	return compactedCall, compactedResult, true
}

func toolCallByID(calls []messages.ToolCall, id string) (messages.ToolCall, bool) {
	id = strings.TrimSpace(id)
	for _, call := range calls {
		if strings.TrimSpace(call.ID) == id {
			return call, true
		}
	}
	return messages.ToolCall{}, false
}

func currentTurnToolResultPlaceholder(result messages.ToolResult, call messages.ToolCall) (string, string) {
	toolName := strings.TrimSpace(result.Name)
	if toolName == "" {
		toolName = strings.TrimSpace(call.Name)
	}
	if toolName == "" {
		toolName = "tool"
	}
	summary := ""
	if result.Meta != nil {
		summary = truncateRunes(result.Meta.Summary, 160)
	}
	if summary == "" {
		summary = truncateRunes(result.Content, 160)
	}
	reference := historicalToolResultReference{
		Status:  currentTurnToolExchangePruneReason,
		Tool:    toolName,
		Summary: summary,
	}
	if result.Meta != nil && contextmanager.ArtifactPathRecoverable(result.Meta.ArtifactPath, time.Now()) {
		reference.ArtifactPath = strings.TrimSpace(result.Meta.ArtifactPath)
		reference.ArtifactComplete = result.Meta.ArtifactComplete
	}
	data, _ := json.Marshal(reference)
	return string(data), summary
}

func lastMessageIndexWithRole(messageList []messages.Message, role messages.MessageRole) int {
	for i := len(messageList) - 1; i >= 0; i-- {
		if messageList[i].Role == role {
			return i
		}
	}
	return -1
}

func findHistoricalToolCall(messageList []messages.Message, toolCallID string, before int) (messages.ToolCall, bool) {
	toolCallID = strings.TrimSpace(toolCallID)
	if toolCallID == "" {
		return messages.ToolCall{}, false
	}
	for i := before - 1; i >= 0; i-- {
		for _, call := range messageList[i].ToolCalls {
			if strings.TrimSpace(call.ID) == toolCallID {
				return call, true
			}
		}
	}
	return messages.ToolCall{}, false
}

type historicalToolResultReference struct {
	Status           string `json:"status"`
	Tool             string `json:"tool"`
	Summary          string `json:"summary,omitempty"`
	ArtifactPath     string `json:"artifact_path,omitempty"`
	ArtifactComplete bool   `json:"artifact_complete,omitempty"`
}

func historicalToolResultPlaceholder(result messages.ToolResult, call messages.ToolCall) (string, string) {
	toolName := strings.TrimSpace(result.Name)
	if toolName == "" {
		toolName = strings.TrimSpace(call.Name)
	}
	if toolName == "" {
		toolName = "tool"
	}
	summary := ""
	if result.Meta != nil {
		summary = strings.TrimSpace(result.Meta.Summary)
	}
	if summary == "" {
		summary = compactHistoricalSummary(result.Content)
	} else {
		summary = truncateRunes(summary, 512)
	}

	reference := historicalToolResultReference{
		Status:  historicalToolResultPruneReason,
		Tool:    toolName,
		Summary: summary,
	}
	if result.Meta != nil && contextmanager.ArtifactPathRecoverable(result.Meta.ArtifactPath, time.Now()) {
		reference.ArtifactPath = strings.TrimSpace(result.Meta.ArtifactPath)
		reference.ArtifactComplete = result.Meta.ArtifactComplete
	}
	data, _ := json.Marshal(reference)
	return string(data), summary
}

func compactHistoricalSummary(content string) string {
	const maxRunes = 512
	runes := []rune(strings.TrimSpace(content))
	if len(runes) <= maxRunes {
		return string(runes)
	}
	head := maxRunes / 2
	tail := maxRunes - head
	return fmt.Sprintf("%s\n... %d chars omitted ...\n%s", string(runes[:head]), len(runes)-maxRunes, string(runes[len(runes)-tail:]))
}

func truncateRunes(value string, maxRunes int) string {
	runes := []rune(strings.TrimSpace(value))
	if maxRunes <= 0 || len(runes) <= maxRunes {
		return string(runes)
	}
	return string(runes[:maxRunes])
}

func (c *Compactor) generateSummary(ctx context.Context, messageList []messages.Message) (string, error) {
	var transcript strings.Builder
	for _, msg := range messageList {
		label := c.transcriptLabel(msg)
		switch msg.Role {
		case messages.MessageRoleToolCall:
			for _, call := range msg.ToolCalls {
				fmt.Fprintf(&transcript, "[%s] tool_call_id: %s\n tool_call_name: %s\n tool_call_arguments: %s\n", label, call.ID, call.Name, call.Arguments)
			}
		case messages.MessageRoleToolResult:
			for _, result := range msg.ToolResults {
				fmt.Fprintf(&transcript, "[%s] tool_call_id: %s\n tool_call_name: %s\n tool_call_result: %s\n", label, result.ToolCallID, result.Name, result.Content)
			}
		default:
			fmt.Fprintf(&transcript, "[%s] %s\n", label, msg.Content)
		}
	}
	if transcript.Len() == 0 {
		return "", nil
	}

	prompt := `
Please summarize the conversation in a concised summary. Template:
## Goal
[What the user is trying to accomplish]
 
## Progress
### Done
[Completed work — specific file paths, commands run, results]
### In Progress
[Work currently underway]
### Blocked
[Any blockers or issues encountered]
 
## Key Decisions
[Important technical decisions and why]
 
## Next Steps
[What needs to happen next]
 
## Critical Context
[Specific values, error messages, configuration details]

## User Plans and Time-Bound Information
[Appointments, plans, deadlines, and other near-term information the user mentioned, even when unrelated to the goal. Resolve relative dates to absolute ones using the message timestamps, for example "2026-10-28 morning" for "tomorrow morning". Give a clock time only when the user stated one. Keep the user's own wording of what the plan is.]

The summary is written at ` + c.now().Format(time.RFC3339) + `. Message labels show when each message was sent.

And here are the conversation details:

` + transcript.String()
	// TODO: dynamic token budget for summary
	result, err := llms.GenerateFromSinglePrompt(ctx, c.Model, prompt, llms.WithMaxTokens(800))
	if err != nil {
		return "", executor.MarkLLMCallError(err)
	}
	result = strings.TrimSpace(result)
	if result == "" {
		return "", executor.MarkLLMCallError(fmt.Errorf("LLM returned an empty summary"))
	}
	return result, nil
}

// transcriptLabel names a message's role and, when known, the time it was
// sent, so the summary can turn relative times into absolute ones.
func (c *Compactor) transcriptLabel(msg messages.Message) string {
	if msg.Timestamp.IsZero() {
		return string(msg.Role)
	}
	return string(msg.Role) + " " + c.inLocation(msg.Timestamp).Format(time.RFC3339)
}

func (c *Compactor) now() time.Time {
	return c.inLocation(time.Now())
}

func (c *Compactor) inLocation(t time.Time) time.Time {
	if c.Location == nil {
		return t.Local()
	}
	return t.In(c.Location)
}

func (c *Compactor) formatSummary(summary string) string {
	// wrap summary into <summary>...</summary>, make sure llm understand the summary is a summary of the conversation
	formatted := fmt.Sprintf("<summary>\n%s\n</summary>\n", summary)
	return formatted
}
