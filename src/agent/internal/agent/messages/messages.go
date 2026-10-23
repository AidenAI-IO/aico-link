package messages

import (
	"encoding/json"
	"time"

	"github.com/tmc/langchaingo/llms"
)

const AttachmentSourceScreenshotObservation = "screenshot_observation"

type MessageRole string

const (
	MessageRoleUser       MessageRole = "user"
	MessageRoleAssistant  MessageRole = "assistant"
	MessageRoleToolCall   MessageRole = "tool_call"
	MessageRoleToolResult MessageRole = "tool_result"
	MessageRoleState      MessageRole = "state"
	MessageRoleSystem     MessageRole = "system"
	MessageRoleNotice     MessageRole = "notice"
)

func (r MessageRole) ToStandardRole() llms.ChatMessageType {
	switch r {
	case MessageRoleUser:
		return llms.ChatMessageTypeHuman
	case MessageRoleAssistant:
		return llms.ChatMessageTypeAI
	case MessageRoleToolCall:
		return llms.ChatMessageTypeAI
	case MessageRoleToolResult:
		return llms.ChatMessageTypeTool
	case MessageRoleState:
		return llms.ChatMessageTypeHuman
	case MessageRoleSystem:
		return llms.ChatMessageTypeSystem
	case MessageRoleNotice:
		return llms.ChatMessageTypeHuman
	default:
		return llms.ChatMessageTypeHuman
	}
}

type Message struct {
	Role                   MessageRole             `json:"role"`
	Content                string                  `json:"content"`
	ReasoningContent       string                  `json:"reasoning_content,omitempty"`
	Timestamp              time.Time               `json:"timestamp"`
	Usage                  *Usage                  `json:"usage,omitempty"`
	ToolCalls              []ToolCall              `json:"tool_calls,omitempty"`
	ToolResults            []ToolResult            `json:"tool_results,omitempty"`
	RecoverableToolResults []RecoverableToolResult `json:"recoverable_tool_results,omitempty"`
	Attachments            []Attachment            `json:"attachments,omitempty"`
	// ResponsesReasoningItems carries opaque Responses API reasoning output so a
	// later stateless request can submit it verbatim. It is deliberately not
	// converted into ordinary chat content.
	ResponsesReasoningItems []json.RawMessage `json:"responses_reasoning_items,omitempty"`
	// ResponsesResponseID identifies the provider-owned response that produced
	// this assistant/tool-call message. Stateful Responses mode uses it as the
	// anchor for the next incremental request.
	ResponsesResponseID string `json:"responses_response_id,omitempty"`
	// ResponsesOutputItems preserves the complete raw Responses output for
	// stateless replay, including item fields that the common message model does
	// not represent (for example phase and encrypted reasoning payloads).
	ResponsesOutputItems []json.RawMessage `json:"responses_output_items,omitempty"`
	// ResponsesAssistantPhase preserves the Responses assistant phase when a
	// raw output item is unavailable or a gateway omits it from persisted data.
	ResponsesAssistantPhase string `json:"responses_assistant_phase,omitempty"`
	// InteractionsSteps preserves native Gemini Interactions output steps,
	// including thought signatures and typed function-call arguments, for local
	// StepList replay when api_mode=interactions. Gemini requires every thought
	// step to be replayed exactly as received, so compaction and session revisions
	// keep these steps alongside the function calls they belong to.
	InteractionsSteps []json.RawMessage `json:"interactions_steps,omitempty"`
	// AnthropicThinkingBlocks preserves signed thinking blocks returned by the
	// native Messages API. Claude requires the signature to be replayed on the
	// next assistant turn when a tool result follows, so storing only the
	// human-readable reasoning text is insufficient.
	AnthropicThinkingBlocks []json.RawMessage `json:"anthropic_thinking_blocks,omitempty"`
}

// Usage is the provider-neutral token usage recorded for an LLM response.
// Providers may use different names for input/output tokens; they are
// normalized to these three fields before being persisted with the message.
type Usage struct {
	TotalTokens  int `json:"total_tokens"`
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

func (msg Message) Clone() Message {
	cloned := msg
	if msg.Usage != nil {
		usage := *msg.Usage
		cloned.Usage = &usage
	}
	if len(msg.ToolCalls) > 0 {
		cloned.ToolCalls = append([]ToolCall(nil), msg.ToolCalls...)
	}
	if len(msg.ToolResults) > 0 {
		cloned.ToolResults = append([]ToolResult(nil), msg.ToolResults...)
		for i := range cloned.ToolResults {
			if msg.ToolResults[i].Meta == nil {
				continue
			}
			meta := *msg.ToolResults[i].Meta
			cloned.ToolResults[i].Meta = &meta
		}
	}
	if len(msg.RecoverableToolResults) > 0 {
		cloned.RecoverableToolResults = append([]RecoverableToolResult(nil), msg.RecoverableToolResults...)
	}
	if len(msg.Attachments) > 0 {
		cloned.Attachments = append([]Attachment(nil), msg.Attachments...)
	}
	if len(msg.ResponsesReasoningItems) > 0 {
		cloned.ResponsesReasoningItems = make([]json.RawMessage, len(msg.ResponsesReasoningItems))
		for i := range msg.ResponsesReasoningItems {
			cloned.ResponsesReasoningItems[i] = append(json.RawMessage(nil), msg.ResponsesReasoningItems[i]...)
		}
	}
	if len(msg.ResponsesOutputItems) > 0 {
		cloned.ResponsesOutputItems = make([]json.RawMessage, len(msg.ResponsesOutputItems))
		for i := range msg.ResponsesOutputItems {
			cloned.ResponsesOutputItems[i] = append(json.RawMessage(nil), msg.ResponsesOutputItems[i]...)
		}
	}
	if len(msg.InteractionsSteps) > 0 {
		cloned.InteractionsSteps = make([]json.RawMessage, len(msg.InteractionsSteps))
		for i := range msg.InteractionsSteps {
			cloned.InteractionsSteps[i] = append(json.RawMessage(nil), msg.InteractionsSteps[i]...)
		}
	}
	if len(msg.AnthropicThinkingBlocks) > 0 {
		cloned.AnthropicThinkingBlocks = make([]json.RawMessage, len(msg.AnthropicThinkingBlocks))
		for i := range msg.AnthropicThinkingBlocks {
			cloned.AnthropicThinkingBlocks[i] = append(json.RawMessage(nil), msg.AnthropicThinkingBlocks[i]...)
		}
	}
	return cloned
}

// CloneList deep-copies every message in messageList. An empty list clones
// to nil.
func CloneList(messageList []Message) []Message {
	if len(messageList) == 0 {
		return nil
	}
	cloned := make([]Message, len(messageList))
	for i, message := range messageList {
		cloned[i] = message.Clone()
	}
	return cloned
}

// Attachment tracks file metadata for message attachments. Binary content is stored on disk
// and only loaded when ConvertToStandardMessageList is called.
type Attachment struct {
	MIMEType string `json:"mime_type"`
	FileSize int64  `json:"file_size"`
	FilePath string `json:"file_path"`
	Source   string `json:"source,omitempty"`
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type ToolResult struct {
	ToolCallID string          `json:"tool_call_id"`
	Name       string          `json:"name"`
	Content    string          `json:"content"`
	Meta       *ToolResultMeta `json:"meta,omitempty"`
}

type ToolResultMeta struct {
	ArtifactPath        string `json:"artifact_path,omitempty"`
	OriginalBytes       int64  `json:"original_bytes,omitempty"`
	OriginalChars       int    `json:"original_chars,omitempty"`
	EstimatedTokens     int    `json:"estimated_tokens,omitempty"`
	Complete            bool   `json:"complete"`
	ArtifactComplete    bool   `json:"artifact_complete"`
	Reason              string `json:"reason,omitempty"`
	Summary             string `json:"summary,omitempty"`
	ActionCompleted     bool   `json:"action_completed,omitempty"`
	ObservationComplete bool   `json:"observation_complete,omitempty"`
	ProcessingErrorCode string `json:"processing_error_code,omitempty"`
	ArtifactStoreError  string `json:"artifact_store_error,omitempty"`
}

// RecoverableToolResult carries trusted recovery metadata across compaction
// revisions. It is persisted with the context message but is not sent to the
// model separately from the bounded recovery block in Message.Content.
type RecoverableToolResult struct {
	ToolName         string `json:"tool_name"`
	ArtifactPath     string `json:"artifact_path"`
	ArtifactComplete bool   `json:"artifact_complete"`
	Summary          string `json:"summary,omitempty"`
}

func (m *ToolResultMeta) UnmarshalJSON(data []byte) error {
	type toolResultMeta ToolResultMeta
	var decoded toolResultMeta
	if err := json.Unmarshal(data, &decoded); err != nil {
		return err
	}
	*m = ToolResultMeta(decoded)
	return nil
}
