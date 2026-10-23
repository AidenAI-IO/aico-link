package agent

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/tmc/langchaingo/agents"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

type FunctionAgent struct {
	Tools     []langtools.Tool
	OutputKey string
}

type visualObservationTool interface {
	ReturnsVisualObservation() bool
}

type visualScreenshotObservation struct {
	Result     postActionScreenshotResult
	ImageBytes []byte
	MIMEType   string
}

type structuredInputTool interface {
	ArgsSchema() map[string]any
}

const toolActionLogVersion = 1
const maxToolObservationRunes = 4000
const maxSkillReadObservationRunes = 20000

type toolActionLog struct {
	Version     int    `json:"aico_action_log_version"`
	Message     string `json:"message"`
	ToolContent string `json:"tool_content,omitempty"`
}

type toolInvocation struct {
	Input string
}

func (a *FunctionAgent) ParseOutput(contentResp *llms.ContentResponse) ([]schema.AgentAction, *schema.AgentFinish, error) {
	if contentResp == nil || len(contentResp.Choices) == 0 {
		return nil, nil, fmt.Errorf("no choices in response")
	}
	choice := contentResp.Choices[0]
	if choice == nil {
		return nil, nil, fmt.Errorf("nil choice in response")
	}
	if len(choice.ToolCalls) == 0 && choice.FuncCall == nil {
		if visible, reasoning, found := normalizeTaggedThinkingText(choice.Content); found {
			choice.Content = visible
			if reasoning != "" {
				if choice.ReasoningContent != "" {
					choice.ReasoningContent += "\n"
				}
				choice.ReasoningContent += reasoning
			}
		}
	}

	if len(choice.ToolCalls) > 0 {
		for _, toolCall := range choice.ToolCalls {
			if toolCall.FunctionCall == nil {
				continue
			}
			functionName := toolCall.FunctionCall.Name
			toolInputStr := toolCall.FunctionCall.Arguments
			invocation := extractToolInvocation(toolInputStr)
			toolCallContent := strings.TrimSpace(choice.Content)

			contentMsg := "\n"
			if toolCallContent != "" {
				contentMsg = fmt.Sprintf("responded: %s\n", toolCallContent)
			}

			return []schema.AgentAction{{
				Tool:      functionName,
				ToolInput: invocation.Input,
				Log:       formatToolActionLog(functionName, toolInputStr, toolCallContent, contentMsg),
				ToolID:    ensureToolCallID(toolCall.ID, 0),
			}}, nil, nil
		}
		// All tool calls had nil FunctionCall - treat as parse failure
		return nil, nil, agents.ErrUnableToParseOutput
	}

	if choice.FuncCall != nil {
		functionName := choice.FuncCall.Name
		toolInputStr := choice.FuncCall.Arguments
		invocation := extractToolInvocation(toolInputStr)
		toolCallContent := strings.TrimSpace(choice.Content)

		contentMsg := "\n"
		if toolCallContent != "" {
			contentMsg = fmt.Sprintf("responded: %s\n", toolCallContent)
		}

		return []schema.AgentAction{{
			Tool:      functionName,
			ToolInput: invocation.Input,
			Log:       formatToolActionLog(functionName, toolInputStr, toolCallContent, contentMsg),
			ToolID:    ensureToolCallID("", 0),
		}}, nil, nil
	}

	return nil, &schema.AgentFinish{
		ReturnValues: map[string]any{
			a.OutputKey: strings.TrimSpace(choice.Content),
		},
		Log: choice.Content,
	}, nil
}

func (a *FunctionAgent) toolsAsLLM() []llms.Tool {
	result := make([]llms.Tool, 0, len(a.Tools))
	for _, tool := range a.Tools {
		if tool == nil {
			continue
		}
		result = append(result, NewToolSpec(tool).LLMTool())
	}
	return result
}

func genericToolParameters() map[string]any {
	return map[string]any{
		"type": "object",
		"properties": map[string]any{
			"input": map[string]string{
				"title":       "input",
				"type":        "string",
				"description": `The plain string input for the selected tool. Prefer tools with typed parameters when available.`,
			},
		},
		"required": []string{"input"},
	}
}

func toolParametersSchema(schema map[string]any) map[string]any {
	parameters := make(map[string]any, len(schema))
	for key, value := range schema {
		parameters[key] = value
	}
	parameters["type"] = "object"
	return parameters
}

func ensureToolCallID(id string, index int) string {
	if id = strings.TrimSpace(id); id != "" {
		return id
	}
	return fmt.Sprintf("call_%d", index+1)
}

func (a *FunctionAgent) observationMessagesForStep(step schema.AgentStep, includeVisual bool) (string, []llms.MessageContent) {
	visual, ok := a.visualScreenshotObservation(step)
	if !ok {
		return compactToolObservationForTool(step.Action.Tool, step.Observation), nil
	}
	result := visual.Result

	toolContent := step.Observation
	if actionOutput, ok := actionOutputFromScreenshotObservation(step.Observation); ok {
		toolContent = actionOutput
	}
	caption := fmt.Sprintf("This image is the screenshot observation returned by the %s tool. Use it when answering the original request.", step.Action.Tool)
	if summary := screenshotObservationStatusSummary(step.Action.Tool, result); summary != "" {
		caption += " " + summary
	}
	if !includeVisual {
		return toolContent, []llms.MessageContent{{
			Role: llms.ChatMessageTypeHuman,
			Parts: []llms.ContentPart{
				llms.TextPart(caption),
				llms.TextPart("[Image omitted]"),
			},
		}}
	}

	return toolContent, []llms.MessageContent{{
		Role: llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{
			llms.TextPart(caption),
			buildImagePart(visual.MIMEType, visual.ImageBytes),
		},
	}}
}

func screenshotObservationStatusSummary(toolName string, result postActionScreenshotResult) string {
	var notes []string
	if result.ScreenChanged != nil {
		if strings.EqualFold(strings.TrimSpace(toolName), "wait_for_stable_screen") {
			if *result.ScreenChanged {
				notes = append(notes, "Screen motion was observed during this wait window; this does not by itself prove that the preceding action succeeded.")
			} else {
				notes = append(notes, "No frame-to-frame screen motion was observed during this wait window; this does not by itself prove that the preceding action had no effect.")
			}
		} else if *result.ScreenChanged {
			notes = append(notes, "A meaningful visible UI change was detected between the pre-action baseline and the final settled screenshot.")
		} else {
			notes = append(notes, "No meaningful visible UI change was detected between the pre-action baseline and the final settled screenshot.")
			notes = append(notes, "Do not assume the action succeeded from tool output alone; inspect the screenshot and verify whether the expected UI change happened before answering or retrying.")
		}
	}
	if result.ScreenStable != nil {
		if *result.ScreenStable {
			notes = append(notes, "The screen was stable when the screenshot was captured.")
		} else {
			notes = append(notes, "The wait timed out while the screen was still changing; treat the screenshot as a best-effort observation.")
		}
	}
	return strings.Join(notes, " ")
}

func (a *FunctionAgent) countVisualObservations(steps []schema.AgentStep) int {
	count := 0
	for _, step := range steps {
		if a.hasVisualObservation(step) {
			count++
		}
	}
	return count
}

func (a *FunctionAgent) hasVisualObservation(step schema.AgentStep) bool {
	_, ok := a.visualScreenshotObservation(step)
	return ok
}

func (a *FunctionAgent) visualScreenshotObservation(step schema.AgentStep) (visualScreenshotObservation, bool) {
	if !a.isVisualObservationTool(step.Action.Tool) {
		return visualScreenshotObservation{}, false
	}
	return parseScreenshotObservation(step.Observation)
}

func parseScreenshotObservation(observation string) (visualScreenshotObservation, bool) {
	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(observation), &result); err != nil {
		return visualScreenshotObservation{}, false
	}
	if result.Width <= 0 || result.Height <= 0 {
		return visualScreenshotObservation{}, false
	}
	if strings.TrimSpace(result.Data) == "" {
		return visualScreenshotObservation{}, false
	}
	imageBytes, err := base64.StdEncoding.DecodeString(result.Data)
	if err != nil || len(imageBytes) == 0 {
		return visualScreenshotObservation{}, false
	}
	format, ok := normalizeScreenshotFormat(result.Format)
	if !ok {
		return visualScreenshotObservation{}, false
	}
	result.Format = format
	if result.Size <= 0 {
		result.Size = len(imageBytes)
	}
	return visualScreenshotObservation{
		Result:     result,
		ImageBytes: imageBytes,
		MIMEType:   screenshotMIMEType(format),
	}, true
}

func normalizeScreenshotFormat(format string) (string, bool) {
	format = strings.ToLower(strings.TrimSpace(format))
	format = strings.TrimPrefix(format, "image/")
	switch format {
	case "":
		return "jpeg", true
	case "jpg", "jpeg":
		return "jpeg", true
	case "png", "webp", "gif":
		return format, true
	default:
		return "", false
	}
}

func screenshotMIMEType(format string) string {
	format, ok := normalizeScreenshotFormat(format)
	if !ok {
		format = "jpeg"
	}
	return "image/" + format
}

func compactToolObservation(observation string) string {
	compacted, _ := compactToolObservationWithStatus(observation)
	return compacted
}

func compactToolObservationWithStatus(observation string) (string, bool) {
	return compactToolObservationLimitWithStatus(observation, maxToolObservationRunes)
}

func compactToolObservationForTool(toolName, observation string) string {
	if strings.TrimSpace(toolName) == "skill_read" {
		return compactToolObservationLimit(observation, maxSkillReadObservationRunes)
	}
	return compactToolObservation(observation)
}

func compactToolObservationLimit(observation string, maxRunes int) string {
	compacted, _ := compactToolObservationLimitWithStatus(observation, maxRunes)
	return compacted
}

func compactToolObservationLimitWithStatus(observation string, maxRunes int) (string, bool) {
	originalRuneCount := len([]rune(observation))
	observation = strings.TrimSpace(observation)
	runes := []rune(observation)
	if observation == "" || len(runes) <= maxRunes {
		return observation, originalRuneCount > maxRunes
	}
	return string(runes[:maxRunes]) + fmt.Sprintf("\n...[truncated %d chars]", len(runes)-maxRunes), true
}

func (a *FunctionAgent) isVisualObservationTool(name string) bool {
	for _, tool := range a.Tools {
		if tool.Name() != name {
			continue
		}
		visualTool, ok := tool.(visualObservationTool)
		return ok && visualTool.ReturnsVisualObservation()
	}
	return false
}

func buildImagePart(mimeType string, data []byte) llms.ContentPart {
	if strings.TrimSpace(mimeType) == "" {
		mimeType = "image/png"
	}
	return llms.ImageURLPart("data:" + mimeType + ";base64," + base64.StdEncoding.EncodeToString(data))
}

func attachmentAwarePrompt(text string, attachments []InputAttachment, descriptions []string) string {
	if descriptions == nil {
		descriptions = make([]string, 0, len(attachments))
	}
	for _, attachment := range attachments {
		label := attachment.Kind
		if attachment.Name != "" {
			label += ": " + attachment.Name
		}
		if attachment.MIMEType != "" {
			label += " (" + attachment.MIMEType + ")"
		}
		if attachment.Width > 0 && attachment.Height > 0 {
			label += fmt.Sprintf(" width=%d height=%d", attachment.Width, attachment.Height)
		}
		descriptions = append(descriptions, label)
	}
	if len(descriptions) == 0 {
		return text
	}
	return text + "\n\nAttached content:\n- " + strings.Join(descriptions, "\n- ")
}

func extractToolInvocation(raw string) toolInvocation {
	var toolInputMap map[string]any
	if err := json.Unmarshal([]byte(raw), &toolInputMap); err != nil {
		return toolInvocation{Input: raw}
	}
	invocation := toolInvocation{Input: raw}
	_, hasLegacyArg := toolInputMap["__arg1"]
	hasGenericInput := false
	if arg1, ok := toolInputMap["__arg1"].(string); ok {
		invocation.Input = arg1
	} else if input, ok := toolInputMap["input"].(string); ok && isGenericStringInputWrapper(toolInputMap) {
		invocation.Input = input
		hasGenericInput = true
	}
	if !hasLegacyArg && !hasGenericInput {
		if encoded, err := json.Marshal(toolInputMap); err == nil {
			invocation.Input = string(encoded)
		}
	}
	return invocation
}

func isGenericStringInputWrapper(fields map[string]any) bool {
	if _, ok := fields["input"]; !ok {
		return false
	}
	for key := range fields {
		if key != "input" {
			return false
		}
	}
	return true
}

func encodeToolArguments(input string) string {
	args := map[string]any{}
	trimmed := strings.TrimSpace(input)
	if strings.HasPrefix(trimmed, "{") {
		var fields map[string]any
		if err := json.Unmarshal([]byte(trimmed), &fields); err == nil {
			for key, value := range fields {
				args[key] = value
			}
		}
	}
	if len(args) == 0 && trimmed != "" {
		args["input"] = input
	}
	encoded, err := json.Marshal(args)
	if err != nil {
		return input
	}
	return string(encoded)
}

func formatToolActionLog(name, arguments, content, contentMsg string) string {
	message := fmt.Sprintf("Invoking: %s with %s %s", name, arguments, contentMsg)
	metadata := toolActionLog{
		Version:     toolActionLogVersion,
		Message:     message,
		ToolContent: strings.TrimSpace(content),
	}
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return message
	}
	return string(encoded)
}

func toolContentFromAction(action schema.AgentAction) string {
	var metadata toolActionLog
	if err := json.Unmarshal([]byte(action.Log), &metadata); err != nil {
		return ""
	}
	if metadata.Version != toolActionLogVersion {
		return ""
	}
	return strings.TrimSpace(metadata.ToolContent)
}
