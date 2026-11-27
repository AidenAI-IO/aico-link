package agent

import (
	"context"
	"strings"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/util"

	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

// Reasons an image of a visual tool result is replaced by a placeholder.
const (
	// ImageOmittedModelUnsupported is used for a model that cannot receive
	// images in the conversation.
	ImageOmittedModelUnsupported = "the current model cannot receive images"
	// ImageOmittedNotReplayed is used for an earlier image that is not sent
	// again when a conversation is restored.
	ImageOmittedNotReplayed = "only the latest image is restored with the conversation"
)

// VisualToolObservation is the output of a tool that returns an image, split
// the way the backend loop splits it: a tool result without the image, and a
// state message with the image and its caption. The image never enters a
// model context as base64 text.
type VisualToolObservation struct {
	toolContent string
	followups   []llms.MessageContent
}

// VisualImage is one image of a visual tool result with the caption the
// backend loop sends with it.
type VisualImage struct {
	Caption  string
	MIMEType string
	Data     []byte
}

// RunRealtimeShell uses the same scoped Device CLI execution as the backend loop.
func (r *Runtime) RunRealtimeShell(ctx context.Context, tool langtools.Tool, input string) VisualToolObservation {
	ctx = context.WithValue(ctx, deviceCLIRuntimeKey{}, r)
	execution := executeToolCall(ctx, ToolCallExecution{
		Specs:  NewToolSpecs([]langtools.Tool{tool}),
		Action: schema.AgentAction{Tool: "shell", ToolInput: input},
	})
	content := execution.Result.Output
	if execution.Result.IsError() {
		content = jsonString(map[string]string{"error": content})
	}
	return VisualToolObservation{
		toolContent: content,
		followups:   deviceCLIStateMessages(execution.Result.deviceFrame),
	}
}

// SplitVisualToolOutput splits the output of a tool that returns a visual
// observation, such as screenshot or a post-action screenshot. It reports
// false when the tool is not visual or the output carries no image, in which
// case the output should be used unchanged.
func SplitVisualToolOutput(tool langtools.Tool, output string) (VisualToolObservation, bool) {
	if tool == nil || !returnsVisualObservation(tool) {
		return VisualToolObservation{}, false
	}
	parser := &FunctionAgent{Tools: []langtools.Tool{tool}}
	content, followups := parser.observationMessagesForStep(schema.AgentStep{
		Action:      schema.AgentAction{Tool: tool.Name()},
		Observation: output,
	})
	if len(followups) == 0 {
		return VisualToolObservation{}, false
	}
	// Without action_output the tool content is the whole observation; its
	// image travels in the state message, so drop the data here.
	return VisualToolObservation{toolContent: stripScreenshotData(content), followups: followups}, true
}

// ToolContent is the tool result the backend loop stores: the tool's text
// output, with the image moved to the state message.
func (o VisualToolObservation) ToolContent() string {
	return o.toolContent
}

// Images returns the images with their captions, as the state message after
// the tool result carries them.
func (o VisualToolObservation) Images() []VisualImage {
	var images []VisualImage
	for _, followup := range o.followups {
		var caption []string
		var data []VisualImage
		for _, part := range followup.Parts {
			switch typed := part.(type) {
			case llms.TextContent:
				caption = append(caption, typed.Text)
			case llms.ImageURLContent:
				if mimeType, bytes, ok := telemetryDataURL(typed.URL); ok && len(bytes) > 0 {
					data = append(data, VisualImage{MIMEType: mimeType, Data: bytes})
				}
			}
		}
		for _, image := range data {
			image.Caption = strings.TrimSpace(strings.Join(caption, "\n"))
			images = append(images, image)
		}
	}
	return images
}

// ToolContentWithoutImages is the tool result for a model that does not get
// the images: the state message is wrapped into the result with each image
// replaced by a placeholder that gives reason.
func (o VisualToolObservation) ToolContentWithoutImages(reason string) string {
	images := o.Images()
	captions := make([]string, 0, len(images))
	for _, image := range images {
		captions = append(captions, image.Caption)
	}
	return ToolResultWithOmittedImages(o.toolContent, captions, reason)
}

// ToolResultWithOmittedImages wraps the captions of a tool result's state
// messages into the result, each with a placeholder for its image, so a model
// that does not get the image still reads what the state message says.
func ToolResultWithOmittedImages(toolContent string, captions []string, reason string) string {
	if len(captions) == 0 {
		return toolContent
	}
	placeholder := "[Image omitted: " + strings.TrimSpace(reason) + ". Do not describe or guess what it shows.]"
	parts := make([]string, 0, len(captions))
	for _, caption := range captions {
		parts = append(parts, mergePromptText(caption, placeholder))
	}
	return mergePromptText(toolContent, util.STag("state", strings.Join(parts, "\n")))
}

// StateMessages stores the images in manager and returns the state messages
// that follow the tool result, built as the backend loop builds them.
func (o VisualToolObservation) StateMessages(manager *contextmanager.ContextManager) []messages.Message {
	if len(o.followups) == 0 {
		return nil
	}
	result := make([]messages.Message, 0, len(o.followups))
	for _, followup := range o.followups {
		result = append(result, visualFollowupMessageFromLLMContent(manager, followup))
	}
	return result
}
