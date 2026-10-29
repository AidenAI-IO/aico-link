package messages

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"aico-agent/internal/util"

	"github.com/tmc/langchaingo/llms"
)

func ConvertMessageList(messageList []Message) []llms.MessageContent {
	standardMessageList := make([]llms.MessageContent, len(messageList))
	for i, message := range messageList {
		newMessage := llms.MessageContent{
			Role:  message.Role.ToStandardRole(),
			Parts: []llms.ContentPart{},
		}
		if message.Role == MessageRoleToolResult {
			for resultIndex, result := range message.ToolResults {
				toolCallID := strings.TrimSpace(result.ToolCallID)
				if toolCallID == "" {
					toolCallID = ToolCallIDOrFallback("", i, resultIndex)
				}
				newMessage.Parts = append(newMessage.Parts, llms.ToolCallResponse{
					ToolCallID: toolCallID,
					Name:       strings.TrimSpace(result.Name),
					Content:    result.Content,
				})
			}
			standardMessageList[i] = newMessage
			continue
		}
		if content := standardMessageContent(message); content != "" {
			newMessage.Parts = append(newMessage.Parts, llms.TextPart(content))
		}
		for toolIndex, call := range message.ToolCalls {
			name := strings.TrimSpace(call.Name)
			if name == "" {
				continue
			}
			newMessage.Parts = append(newMessage.Parts, llms.ToolCall{
				ID:   ToolCallIDOrFallback(call.ID, i, toolIndex),
				Type: "function",
				FunctionCall: &llms.FunctionCall{
					Name:      name,
					Arguments: normalizeToolCallArguments(call.Arguments),
				},
			})
		}
		for _, attachment := range message.Attachments {
			filePath := strings.TrimSpace(attachment.FilePath)
			if filePath == "" {
				continue
			}
			data, err := os.ReadFile(filePath)
			if err != nil {
				newMessage.Parts = append(newMessage.Parts, llms.TextPart(attachmentOmittedMessage(attachment.MIMEType, err)))
				continue
			}
			if len(data) == 0 {
				continue
			}
			newMessage.Parts = append(newMessage.Parts, llms.BinaryPart(attachment.MIMEType, data))
		}
		standardMessageList[i] = newMessage
	}
	return standardMessageList
}

func standardMessageContent(message Message) string {
	content := strings.TrimSpace(message.Content)
	if content == "" {
		return content
	}
	switch message.Role {
	case MessageRoleNotice:
		return util.STag("notice", content)
	case MessageRoleState:
		return util.STag("state", content)
	default:
		return content
	}
}

// ConvertChoiceToContextManagerMessage converts a content choice to a context manager message
func ConvertChoiceToContextManagerMessage(choice llms.ContentChoice) Message {
	role := MessageRoleAssistant
	if contentChoiceHasToolCalls(choice) {
		role = MessageRoleToolCall
	}
	return Message{
		Role:                    role,
		Content:                 contentChoiceText(choice),
		ReasoningContent:        strings.TrimSpace(choice.ReasoningContent),
		Usage:                   UsageFromGenerationInfo(choice.GenerationInfo),
		ToolCalls:               toolCallsFromContentChoice(choice),
		ResponsesReasoningItems: responsesReasoningItemsFromGenerationInfo(choice.GenerationInfo),
		ResponsesResponseID:     responsesResponseIDFromGenerationInfo(choice.GenerationInfo),
		ResponsesOutputItems:    responsesOutputItemsFromGenerationInfo(choice.GenerationInfo),
		ResponsesAssistantPhase: responsesAssistantPhaseFromGenerationInfo(choice.GenerationInfo),
		InteractionsSteps:       interactionsStepsFromGenerationInfo(choice.GenerationInfo),
		AnthropicThinkingBlocks: anthropicThinkingBlocksFromGenerationInfo(choice.GenerationInfo),
	}
}

func interactionsStepsFromGenerationInfo(info map[string]any) []json.RawMessage {
	if len(info) == 0 {
		return nil
	}
	items, ok := info["interactions_steps"].([]json.RawMessage)
	if !ok || len(items) == 0 {
		return nil
	}
	cloned := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if len(item) != 0 {
			cloned = append(cloned, append(json.RawMessage(nil), item...))
		}
	}
	return cloned
}

func anthropicThinkingBlocksFromGenerationInfo(info map[string]any) []json.RawMessage {
	if len(info) == 0 {
		return nil
	}
	items, ok := info["anthropic_thinking_blocks"].([]json.RawMessage)
	if !ok || len(items) == 0 {
		return nil
	}
	cloned := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if len(item) != 0 {
			cloned = append(cloned, append(json.RawMessage(nil), item...))
		}
	}
	return cloned
}

// UsageFromGenerationInfo normalizes provider-specific generation metadata to
// the compact usage shape persisted on messages. OpenAI-compatible providers
// report prompt/completion tokens; Responses, Anthropic, and realtime paths
// use input/output tokens. Both forms are accepted here.
func UsageFromGenerationInfo(info map[string]any) *Usage {
	if len(info) == 0 {
		return nil
	}
	input, inputOK := usageMetricInt(info["input_tokens"])
	if prompt, ok := usageMetricInt(info["prompt_tokens"]); ok {
		input = prompt
		inputOK = true
	}
	output, outputOK := usageMetricInt(info["output_tokens"])
	if completion, ok := usageMetricInt(info["completion_tokens"]); ok {
		output = completion
		outputOK = true
	}
	total, totalOK := usageMetricInt(info["total_tokens"])
	if !inputOK && !outputOK && !totalOK {
		return nil
	}
	if !totalOK && (inputOK || outputOK) {
		total = input + output
	}
	return &Usage{TotalTokens: total, InputTokens: input, OutputTokens: output}
}

func usageMetricInt(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case float64:
		return int(typed), true
	case float32:
		return int(typed), true
	default:
		return 0, false
	}
}

func responsesResponseIDFromGenerationInfo(info map[string]any) string {
	if len(info) == 0 {
		return ""
	}
	id, _ := info["llm_response_id"].(string)
	return strings.TrimSpace(id)
}

func responsesReasoningItemsFromGenerationInfo(info map[string]any) []json.RawMessage {
	if len(info) == 0 {
		return nil
	}
	items, ok := info["responses_reasoning_items"].([]json.RawMessage)
	if !ok || len(items) == 0 {
		return nil
	}
	cloned := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if len(item) != 0 {
			cloned = append(cloned, append(json.RawMessage(nil), item...))
		}
	}
	return cloned
}

func responsesOutputItemsFromGenerationInfo(info map[string]any) []json.RawMessage {
	if len(info) == 0 {
		return nil
	}
	items, ok := info["responses_output_items"].([]json.RawMessage)
	if !ok || len(items) == 0 {
		return nil
	}
	cloned := make([]json.RawMessage, 0, len(items))
	for _, item := range items {
		if len(item) != 0 {
			cloned = append(cloned, append(json.RawMessage(nil), item...))
		}
	}
	return cloned
}

func responsesAssistantPhaseFromGenerationInfo(info map[string]any) string {
	if len(info) == 0 {
		return ""
	}
	phase, _ := info["responses_assistant_phase"].(string)
	return strings.TrimSpace(phase)
}

func contentChoiceHasToolCalls(choice llms.ContentChoice) bool {
	if len(choice.ToolCalls) > 0 {
		return true
	}
	return choice.FuncCall != nil
}

func contentChoiceText(choice llms.ContentChoice) string {
	return strings.TrimSpace(choice.Content)
}

func toolCallsFromContentChoice(choice llms.ContentChoice) []ToolCall {
	toolCalls := choice.ToolCalls
	if len(toolCalls) == 0 && choice.FuncCall != nil {
		toolCalls = []llms.ToolCall{{
			Type:         "function",
			FunctionCall: choice.FuncCall,
		}}
	}
	result := make([]ToolCall, 0, len(toolCalls))
	for _, call := range toolCalls {
		if call.FunctionCall == nil {
			continue
		}
		name := strings.TrimSpace(call.FunctionCall.Name)
		if name == "" {
			continue
		}
		result = append(result, ToolCall{
			ID:        strings.TrimSpace(call.ID),
			Name:      name,
			Arguments: normalizeToolCallArguments(call.FunctionCall.Arguments),
		})
	}
	return result
}

func normalizeToolCallArguments(arguments string) string {
	trimmed := strings.TrimSpace(arguments)
	if trimmed == "" {
		return "{}"
	}
	if json.Valid([]byte(trimmed)) {
		return trimmed
	}
	encoded, err := json.Marshal(map[string]string{"input": arguments})
	if err != nil {
		return "{}"
	}
	return string(encoded)
}

func attachmentOmittedMessage(mimeType string, err error) string {
	label := strings.TrimSpace(mimeType)
	if label == "" {
		label = "attachment"
	}
	if err == nil {
		return fmt.Sprintf("[Attachment omitted: %s could not be loaded.]", label)
	}
	return fmt.Sprintf("[Attachment omitted: %s could not be loaded: %v]", label, err)
}

func ToolCallIDOrFallback(id string, messageIndex, toolIndex int) string {
	if id = strings.TrimSpace(id); id != "" {
		return id
	}
	return fmt.Sprintf("ctx_tool_call_%d_%d", messageIndex, toolIndex)
}
