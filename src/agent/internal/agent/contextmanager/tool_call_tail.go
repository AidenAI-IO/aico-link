package contextmanager

import (
	"aico-agent/internal/agent/messages"
	"strings"
)

const interruptedToolResultContent = "Tool execution was interrupted before a result was recorded."

// repairToolCallTailBeforeAppend enforces the tool-call protocol only at the
// append boundary. If the last persisted message is a tool call, the first
// incoming message must be its result. Otherwise an interrupted result is
// persisted before the incoming batch closes the tool call.
func repairToolCallTailBeforeAppend(existing, incoming []messages.Message) []messages.Message {
	if len(existing) == 0 || len(incoming) == 0 {
		return incoming
	}

	lastIndex := len(existing) - 1
	last := existing[lastIndex]
	if last.Role != messages.MessageRoleToolCall || len(last.ToolCalls) == 0 {
		return incoming
	}

	call, ok := lastPendingToolCall(last, lastIndex)
	if !ok {
		return incoming
	}
	if normalized, ok := normalizeMatchingToolResult(incoming[0], call); ok {
		result := cloneMessages(incoming)
		result[0] = normalized
		return result
	}

	result := make([]messages.Message, 0, len(incoming)+1)
	result = append(result, interruptedToolResult(call))
	result = append(result, incoming...)
	return result
}

type pendingToolCall struct {
	id                string
	name              string
	originalIDMissing bool
}

func lastPendingToolCall(message messages.Message, messageIndex int) (pendingToolCall, bool) {
	for toolIndex, call := range message.ToolCalls {
		name := strings.TrimSpace(call.Name)
		if name == "" {
			continue
		}
		originalID := strings.TrimSpace(call.ID)
		return pendingToolCall{
			id:                messages.ToolCallIDOrFallback(originalID, messageIndex, toolIndex),
			name:              name,
			originalIDMissing: originalID == "",
		}, true
	}
	return pendingToolCall{}, false
}

func normalizeMatchingToolResult(message messages.Message, call pendingToolCall) (messages.Message, bool) {
	if message.Role != messages.MessageRoleToolResult || len(message.ToolResults) != 1 {
		return messages.Message{}, false
	}

	toolResult := message.ToolResults[0]
	resultID := strings.TrimSpace(toolResult.ToolCallID)
	resultName := strings.TrimSpace(toolResult.Name)
	idMatches := resultID == call.id
	legacyMatches := call.originalIDMissing && (resultName == "" || resultName == call.name)
	if !idMatches && !legacyMatches {
		return messages.Message{}, false
	}

	toolResult.ToolCallID = call.id
	if resultName == "" {
		toolResult.Name = call.name
	}
	message.ToolResults = []messages.ToolResult{toolResult}
	return message, true
}

func interruptedToolResult(call pendingToolCall) messages.Message {
	return messages.Message{
		Role: messages.MessageRoleToolResult,
		ToolResults: []messages.ToolResult{{
			ToolCallID: call.id,
			Name:       call.name,
			Content:    interruptedToolResultContent,
		}},
	}
}
