package tokencounter

import (
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/logging"
	"encoding/json"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/tmc/langchaingo/llms"
)

// EstimateImageTokens is the number of tokens estimated for a single image, because in our agent, we usally use fixed size image, so we can use a constant value to estimate the tokens.
const EstimateImageTokens = 1000

func EstimateTextTokens(content string) int {
	if len(content) == 0 {
		return 0
	}
	cjkTokens := 0
	nonCJKBytes := 0
	for _, r := range content {
		if isCJK(r) {
			cjkTokens++
		} else {
			nonCJKBytes += utf8.RuneLen(r)
		}
	}
	return cjkTokens + (nonCJKBytes+3)/4
}

// isCJK reports whether r belongs to a CJK script (Han ideographs plus the
// Japanese and Korean syllabaries), which tokenizers split at roughly one token
// per character rather than the chars/4 ratio that holds for Latin text.
func isCJK(r rune) bool {
	return unicode.In(r, unicode.Han, unicode.Hiragana, unicode.Katakana, unicode.Hangul)
}

func EstimateAttachmentTokens(attachment messages.Attachment) int {
	// if attachment source is screenshot, then use fixed value to estimate the tokens
	if attachment.Source == messages.AttachmentSourceScreenshotObservation {
		return EstimateImageTokens
	}

	// TODO: file token consumption cannot be estimated, currently only take file as pure text, so we use file size to estimate the tokens
	return int(attachment.FileSize / 4)
}

func EstimateMessageTokens(message messages.Message) int {
	total := EstimateTextTokens(message.Content)
	for _, toolCall := range message.ToolCalls {
		total += EstimateTextTokens(toolCall.Arguments)
	}
	for _, toolResult := range message.ToolResults {
		total += EstimateTextTokens(toolResult.Content)
	}
	for _, attachment := range message.Attachments {
		total += EstimateAttachmentTokens(attachment)
	}
	return total
}

func EstimateMessagesTokens(messages []messages.Message) int {
	total := 0
	for _, message := range messages {
		total += EstimateMessageTokens(message)
	}
	return total
}

func EstimateToolSchemaTokens(opts llms.CallOptions) int {
	total := 0
	for _, tool := range opts.Tools {
		if tool.Function == nil {
			continue
		}
		// marshal the function to json and estimate the tokens
		json, err := json.Marshal(tool.Function)
		if err != nil {
			continue
		}
		total += EstimateTextTokens(string(json))
	}
	return total
}

func EstimateLLMMessageListTokens(messages []llms.MessageContent) int {
	total := 0
	for _, message := range messages {
		total += EstimateLLMMessageTokens(message)
	}
	return total
}

func EstimateLLMMessageTokens(message llms.MessageContent) int {
	total := 0
	for _, part := range message.Parts {
		total += estimaleLLMPartTokens(part)
	}
	return total
}

func estimaleLLMPartTokens(part llms.ContentPart) int {
	if text, ok := part.(llms.TextContent); ok {
		return EstimateTextTokens(text.Text)
	}
	if binary, ok := part.(llms.BinaryContent); ok {
		return EstimateAttachmentTokens(messages.Attachment{
			Source:   "",
			MIMEType: binary.MIMEType,
			FileSize: int64(len(binary.Data)),
		})
	}
	if imageURL, ok := part.(llms.ImageURLContent); ok {
		// if url is a data url, then extract the base64 encoded data and estimate the tokens
		if strings.HasPrefix(imageURL.URL, "data:") {
			return EstimateImageTokens
		}
		return EstimateTextTokens(imageURL.URL)
	}
	if toolCall, ok := part.(llms.ToolCall); ok {
		return EstimateTextTokens(toolCall.FunctionCall.Name + toolCall.FunctionCall.Arguments)
	}
	if toolCallResponse, ok := part.(llms.ToolCallResponse); ok {
		return EstimateTextTokens(toolCallResponse.Content)
	}

	logging.Warnf("agent", "tokencounter", "unknown part type: %T, %+v", part, part)
	return 0
}
