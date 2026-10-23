package agent

import (
	"fmt"
	"regexp"
	"strings"

	"aiden-agent/internal/agent/contextmanager"
	"aiden-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/llms"
)

func messageRoleFromLLM(role llms.ChatMessageType) messages.MessageRole {
	switch role {
	case llms.ChatMessageTypeSystem:
		return messages.MessageRoleSystem
	case llms.ChatMessageTypeHuman, llms.ChatMessageTypeGeneric:
		return messages.MessageRoleUser
	case llms.ChatMessageTypeAI:
		return messages.MessageRoleAssistant
	case llms.ChatMessageTypeTool, llms.ChatMessageTypeFunction:
		return messages.MessageRoleToolResult
	default:
		return messages.MessageRoleUser
	}
}

func messageFromLLMContent(manager *contextmanager.ContextManager, content llms.MessageContent) messages.Message {
	message := messages.Message{Role: messageRoleFromLLM(content.Role)}
	for _, part := range content.Parts {
		switch typed := part.(type) {
		case llms.TextContent:
			message.Content = mergePromptText(message.Content, typed.Text)
		case llms.ToolCall:
			if typed.FunctionCall == nil {
				continue
			}
			message.Role = messages.MessageRoleToolCall
			message.ToolCalls = append(message.ToolCalls, messages.ToolCall{
				ID:        strings.TrimSpace(typed.ID),
				Name:      strings.TrimSpace(typed.FunctionCall.Name),
				Arguments: strings.TrimSpace(typed.FunctionCall.Arguments),
			})
		case llms.ToolCallResponse:
			message.Role = messages.MessageRoleToolResult
			message.ToolResults = append(message.ToolResults, messages.ToolResult{
				ToolCallID: strings.TrimSpace(typed.ToolCallID),
				Name:       strings.TrimSpace(typed.Name),
				Content:    typed.Content,
			})
		case llms.BinaryContent:
			if manager == nil {
				continue
			}
			stored, err := manager.StoreAttachment(typed.MIMEType, typed.Data)
			if err != nil {
				message.Content = mergePromptText(message.Content, attachmentStorageFailureText(typed.MIMEType, err))
				continue
			}
			message.Attachments = append(message.Attachments, stored)
		case llms.ImageURLContent:
			if text := strings.TrimSpace(typed.URL); text != "" {
				message.Content = mergePromptText(message.Content, text)
			}
		}
	}
	return message
}

func userMessageFromInput(manager *contextmanager.ContextManager, input string, attachments []InputAttachment) messages.Message {
	descriptions := make([]string, 0, len(attachments))
	message := messages.Message{
		Role:    messages.MessageRoleUser,
		Content: attachmentAwarePrompt(normalizeRunInput(input, attachments), attachments, descriptions),
	}
	if manager == nil {
		return message
	}
	for _, attachment := range attachments {
		if len(attachment.Data) == 0 {
			continue
		}
		mimeType := attachmentMIMETypeOrDefault(attachment)
		stored, err := manager.StoreAttachment(mimeType, attachment.Data)
		if err != nil {
			message.Content = mergePromptText(message.Content, attachmentStorageFailureText(mimeType, err))
			continue
		}
		message.Attachments = append(message.Attachments, stored)
	}
	return message
}

func toolResultMessage(toolCallID, toolName string, prepared PreparedToolResult) messages.Message {
	return messages.Message{
		Role: messages.MessageRoleToolResult,
		ToolResults: []messages.ToolResult{{
			ToolCallID: strings.TrimSpace(toolCallID),
			Name:       strings.TrimSpace(toolName),
			Content:    prepared.Content,
			Meta: &messages.ToolResultMeta{
				ArtifactPath:        prepared.ArtifactPath,
				OriginalBytes:       prepared.OriginalBytes,
				OriginalChars:       prepared.OriginalChars,
				EstimatedTokens:     prepared.EstimatedTokens,
				Complete:            prepared.Complete,
				ArtifactComplete:    prepared.ArtifactComplete,
				Reason:              prepared.Reason,
				Summary:             prepared.Summary,
				ActionCompleted:     prepared.ActionCompleted,
				ObservationComplete: prepared.ObservationComplete,
				ProcessingErrorCode: prepared.ProcessingErrorCode,
				ArtifactStoreError:  prepared.ArtifactStoreError,
			},
		}},
	}
}

func visualFollowupMessageFromLLMContent(manager *contextmanager.ContextManager, content llms.MessageContent) messages.Message {
	message := messages.Message{Role: messages.MessageRoleState}
	for _, part := range content.Parts {
		switch typed := part.(type) {
		case llms.TextContent:
			message.Content = mergePromptText(message.Content, typed.Text)
		case llms.ImageURLContent:
			mimeType, data, ok := telemetryDataURL(typed.URL)
			if ok && strings.HasPrefix(strings.ToLower(mimeType), "image/") && len(data) > 0 && manager != nil {
				stored, err := manager.StoreAttachment(mimeType, data)
				if err == nil {
					stored.Source = messages.AttachmentSourceScreenshotObservation
					message.Attachments = append(message.Attachments, stored)
					continue
				}
				message.Content = mergePromptText(message.Content, attachmentStorageFailureText(mimeType, err))
				continue
			}
			if text := strings.TrimSpace(typed.URL); text != "" {
				message.Content = mergePromptText(message.Content, text)
			}
		case llms.BinaryContent:
			if manager == nil {
				continue
			}
			stored, err := manager.StoreAttachment(typed.MIMEType, typed.Data)
			if err != nil {
				message.Content = mergePromptText(message.Content, attachmentStorageFailureText(typed.MIMEType, err))
				continue
			}
			stored.Source = messages.AttachmentSourceScreenshotObservation
			message.Attachments = append(message.Attachments, stored)
		default:
			fallback := messageFromLLMContent(manager, llms.MessageContent{
				Role:  content.Role,
				Parts: []llms.ContentPart{part},
			})
			message.Content = mergePromptText(message.Content, fallback.Content)
			message.ToolCalls = append(message.ToolCalls, fallback.ToolCalls...)
			message.ToolResults = append(message.ToolResults, fallback.ToolResults...)
			message.Attachments = append(message.Attachments, fallback.Attachments...)
		}
	}
	return message
}

func attachmentMIMETypeOrDefault(attachment InputAttachment) string {
	mimeType := strings.TrimSpace(attachment.MIMEType)
	if mimeType != "" {
		return mimeType
	}
	switch attachment.Kind {
	case AttachmentKindImage:
		return "image/png"
	case AttachmentKindAudio:
		return "audio/wav"
	default:
		return "application/octet-stream"
	}
}

func attachmentStorageFailureText(mimeType string, err error) string {
	label := strings.TrimSpace(mimeType)
	if label == "" {
		label = "attachment"
	}
	if err == nil {
		return fmt.Sprintf("[Attachment omitted: %s could not be stored.]", label)
	}
	return fmt.Sprintf("[Attachment omitted: %s could not be stored: %v]", label, err)
}

func mergePromptText(existing, addition string) string {
	existing = strings.TrimSpace(existing)
	addition = strings.TrimSpace(addition)
	switch {
	case existing == "":
		return addition
	case addition == "":
		return existing
	default:
		return existing + "\n" + addition
	}
}

// InitializeContextManager initializes a context manager with a system prompt and a session folder if session is new.
func InitializeContextManager(
	systemPrompt string,
	sessionFolder string,
	hooks []contextmanager.AppendMessageHook,
) (*contextmanager.ContextManager, error) {
	manager, err := contextmanager.LoadContextManagerFromCurrentSession(sessionFolder)
	if err != nil || !contextManagerHasSystemPrompt(manager, systemPrompt) {
		// Create a new append-only session when there is no current session or
		// when configuration changes require a different system prompt.
		manager, err = contextmanager.NewContextManager(sessionFolder, systemPrompt)
		if err != nil {
			return nil, fmt.Errorf("failed to create context manager: %w", err)
		}
	} else if strings.TrimSpace(manager.MessageListDump().Messages[0].Content) != strings.TrimSpace(systemPrompt) {
		// The generated date is runtime context, not a configuration change.
		// Keep the append-only transcript and provider continuation intact while
		// supplying the new date before the next task resumes this conversation.
		dateContext := strings.SplitN(strings.TrimSpace(systemPrompt), "\n", 3)[1]
		if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleState, Content: dateContext}); err != nil {
			return nil, fmt.Errorf("refresh session date: %w", err)
		}
	}

	if manager == nil {
		return nil, fmt.Errorf("failed to create context manager")
	}

	// set hooks
	manager.AddAppendMessageHooks(hooks)

	return manager, nil
}

func contextManagerHasSystemPrompt(manager *contextmanager.ContextManager, systemPrompt string) bool {
	if manager == nil {
		return false
	}
	messageList := manager.MessageListDump().Messages
	return len(messageList) > 0 &&
		messageList[0].Role == messages.MessageRoleSystem &&
		systemPromptIdentity(messageList[0].Content) == systemPromptIdentity(systemPrompt)
}

// Only the date line emitted at the beginning of buildProfile is volatile.
// Preserve the timezone and every instruction, including date-like user text,
// so actual configuration changes still start a new conversation.
var generatedPromptDate = regexp.MustCompile(`^(You are the Aiden agent\.\nCurrent date: )[0-9]{4}-[0-9]{2}-[0-9]{2} \((?:Sunday|Monday|Tuesday|Wednesday|Thursday|Friday|Saturday|星期[一二三四五六日]), timezone: ([^)\n]+)\)\n`)

func systemPromptIdentity(prompt string) string {
	return generatedPromptDate.ReplaceAllString(strings.TrimSpace(prompt), "${1}(timezone: ${2})\n")
}
