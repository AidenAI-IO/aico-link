package contextmanager

import (
	"aico-agent/internal/agent/messages"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

func newTestContextManager(t *testing.T) *ContextManager {
	t.Helper()

	sessionFolder := t.TempDir()
	sessionID := newSessionID()
	if err := saveCurrentSession(sessionFolder, sessionID); err != nil {
		t.Fatalf("saveCurrentSession() error = %v", err)
	}

	manager, err := LoadContextManagerFromSessionID(sessionFolder, sessionID)
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() error = %v", err)
	}
	return manager
}

func TestConvertStandardMessageToContextManagerMessage_Assistant(t *testing.T) {
	message := messages.ConvertChoiceToContextManagerMessage(llms.ContentChoice{
		Content: "hello",
	})
	if message.Role != messages.MessageRoleAssistant {
		t.Fatalf("role = %q, want %q", message.Role, messages.MessageRoleAssistant)
	}
	if message.Content != "hello" {
		t.Fatalf("content = %q, want %q", message.Content, "hello")
	}
}

func TestConvertStandardMessageToContextManagerMessage_NormalizesUsage(t *testing.T) {
	tests := []struct {
		name string
		info map[string]any
		want messages.Usage
	}{
		{
			name: "openai",
			info: map[string]any{"prompt_tokens": 19, "completion_tokens": 10, "total_tokens": 29},
			want: messages.Usage{InputTokens: 19, OutputTokens: 10, TotalTokens: 29},
		},
		{
			name: "realtime",
			info: map[string]any{"input_tokens": 336, "output_tokens": 41, "total_tokens": 377},
			want: messages.Usage{InputTokens: 336, OutputTokens: 41, TotalTokens: 377},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			message := messages.ConvertChoiceToContextManagerMessage(llms.ContentChoice{Content: "ok", GenerationInfo: tt.info})
			if message.Usage == nil || *message.Usage != tt.want {
				t.Fatalf("usage = %#v, want %#v", message.Usage, tt.want)
			}
		})
	}
}

func TestConvertStandardMessageToContextManagerMessage_ToolCall(t *testing.T) {
	message := messages.ConvertChoiceToContextManagerMessage(llms.ContentChoice{
		Content: "发送测试文本。",
		ToolCalls: []llms.ToolCall{{
			ID:   "call_1",
			Type: "function",
			FunctionCall: &llms.FunctionCall{
				Name:      "echo",
				Arguments: `{"input":"hello"}`,
			},
		}},
	})
	if message.Role != messages.MessageRoleToolCall {
		t.Fatalf("role = %q, want %q", message.Role, messages.MessageRoleToolCall)
	}
	if message.Content != "发送测试文本。" {
		t.Fatalf("content = %q, want assistant preamble only", message.Content)
	}
	if len(message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %#v, want one entry", message.ToolCalls)
	}
	if message.ToolCalls[0].ID != "call_1" || message.ToolCalls[0].Name != "echo" || message.ToolCalls[0].Arguments != `{"input":"hello"}` {
		t.Fatalf("tool_calls = %#v", message.ToolCalls)
	}
}

func TestConvertStandardMessageToContextManagerMessage_NormalizesInvalidToolCallArguments(t *testing.T) {
	rawArguments := `{"type": "tap", "point": {"x":}`
	message := messages.ConvertChoiceToContextManagerMessage(llms.ContentChoice{
		ToolCalls: []llms.ToolCall{{
			ID:   "call_1",
			Type: "function",
			FunctionCall: &llms.FunctionCall{
				Name:      "touch_gesture",
				Arguments: rawArguments,
			},
		}},
	})

	if len(message.ToolCalls) != 1 {
		t.Fatalf("tool_calls = %#v, want one entry", message.ToolCalls)
	}
	want := mustMarshalToolInput(t, rawArguments)
	if message.ToolCalls[0].Arguments != want {
		t.Fatalf("arguments = %q, want valid JSON wrapper", message.ToolCalls[0].Arguments)
	}
}

func TestConvertStandardMessageToContextManagerMessage_ToolCallWithoutContent(t *testing.T) {
	message := messages.ConvertChoiceToContextManagerMessage(llms.ContentChoice{
		ToolCalls: []llms.ToolCall{{
			ID:   "call_1",
			Type: "function",
			FunctionCall: &llms.FunctionCall{
				Name:      "screenshot",
				Arguments: `{}`,
			},
		}},
	})
	if message.Role != messages.MessageRoleToolCall {
		t.Fatalf("role = %q, want %q", message.Role, messages.MessageRoleToolCall)
	}
	if message.Content != "" {
		t.Fatalf("content = %q, want empty", message.Content)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].ID != "call_1" || message.ToolCalls[0].Name != "screenshot" {
		t.Fatalf("tool_calls = %#v", message.ToolCalls)
	}
}

func TestConvertStandardMessageToContextManagerMessage_FuncCallFallback(t *testing.T) {
	message := messages.ConvertChoiceToContextManagerMessage(llms.ContentChoice{
		FuncCall: &llms.FunctionCall{
			Name:      "echo",
			Arguments: `{"value":"hello"}`,
		},
	})
	if message.Role != messages.MessageRoleToolCall {
		t.Fatalf("role = %q, want %q", message.Role, messages.MessageRoleToolCall)
	}
	if message.Content != "" {
		t.Fatalf("content = %q, want empty", message.Content)
	}
	if len(message.ToolCalls) != 1 || message.ToolCalls[0].Name != "echo" {
		t.Fatalf("tool_calls = %#v", message.ToolCalls)
	}
}

func TestConvertToStandardMessageList_ToolCalls(t *testing.T) {
	manager := newTestContextManager(t)
	manager.AppendMessage(messages.Message{
		Role:    messages.MessageRoleToolCall,
		Content: "发送测试文本。",
		ToolCalls: []messages.ToolCall{{
			ID:        "call_1",
			Name:      "echo",
			Arguments: `{"input":"hello"}`,
		}},
	})

	messages := messages.ConvertMessageList(manager.CloneMessageList())
	if len(messages) != 1 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0].Role != llms.ChatMessageTypeAI {
		t.Fatalf("role = %v, want AI", messages[0].Role)
	}
	if len(messages[0].Parts) != 2 {
		t.Fatalf("parts = %#v, want text + tool call", messages[0].Parts)
	}
	text, ok := messages[0].Parts[0].(llms.TextContent)
	if !ok || text.Text != "发送测试文本。" {
		t.Fatalf("text part = %#v", messages[0].Parts[0])
	}
	toolCall, ok := messages[0].Parts[1].(llms.ToolCall)
	if !ok || toolCall.ID != "call_1" || toolCall.FunctionCall == nil || toolCall.FunctionCall.Name != "echo" {
		t.Fatalf("tool call part = %#v", messages[0].Parts[1])
	}
}

func TestConvertToStandardMessageList_NormalizesInvalidToolCallArguments(t *testing.T) {
	rawArguments := `{"type": "tap", "point": {"x":}`
	manager := newTestContextManager(t)
	manager.AppendMessage(messages.Message{
		Role: messages.MessageRoleToolCall,
		ToolCalls: []messages.ToolCall{{
			ID:        "call_1",
			Name:      "touch_gesture",
			Arguments: rawArguments,
		}},
	})

	messages := messages.ConvertMessageList(manager.CloneMessageList())
	if len(messages) != 1 || len(messages[0].Parts) != 1 {
		t.Fatalf("messages = %#v, want one tool call message", messages)
	}
	toolCall, ok := messages[0].Parts[0].(llms.ToolCall)
	if !ok || toolCall.FunctionCall == nil {
		t.Fatalf("tool call part = %#v", messages[0].Parts[0])
	}
	want := mustMarshalToolInput(t, rawArguments)
	if toolCall.FunctionCall.Arguments != want {
		t.Fatalf("arguments = %q, want valid JSON wrapper", toolCall.FunctionCall.Arguments)
	}
}

func mustMarshalToolInput(t *testing.T, input string) string {
	t.Helper()
	encoded, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		t.Fatalf("marshal expected input wrapper: %v", err)
	}
	return string(encoded)
}

func TestConvertToStandardMessageList_ToolResults(t *testing.T) {
	manager := newTestContextManager(t)
	manager.AppendMessage(messages.Message{
		Role: messages.MessageRoleToolCall,
		ToolCalls: []messages.ToolCall{{
			ID:        "call_1",
			Name:      "echo",
			Arguments: `{}`,
		}},
	})
	manager.AppendMessage(messages.Message{
		Role: messages.MessageRoleToolResult,
		ToolResults: []messages.ToolResult{{
			ToolCallID: "call_1",
			Name:       "echo",
			Content:    `{"output":"hello"}`,
		}},
	})

	messages := messages.ConvertMessageList(manager.CloneMessageList())
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[1].Role != llms.ChatMessageTypeTool {
		t.Fatalf("role = %v, want Tool", messages[1].Role)
	}
	if len(messages[1].Parts) != 1 {
		t.Fatalf("parts = %#v, want one tool result", messages[1].Parts)
	}
	toolResponse, ok := messages[1].Parts[0].(llms.ToolCallResponse)
	if !ok || toolResponse.ToolCallID != "call_1" || toolResponse.Name != "echo" || toolResponse.Content != `{"output":"hello"}` {
		t.Fatalf("tool result part = %#v", messages[1].Parts[0])
	}
}

func TestNewContextManagerFromMessageListPersistsMessages(t *testing.T) {
	sessionFolder := t.TempDir()
	timestamp := time.Date(2026, time.August, 25, 12, 0, 0, 0, time.UTC)
	want := []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system prompt", Timestamp: timestamp},
		{
			Role:      messages.MessageRoleUser,
			Content:   "compacted summary",
			Timestamp: timestamp,
			RecoverableToolResults: []messages.RecoverableToolResult{{
				ToolName:         "shell",
				ArtifactPath:     "/tmp/tool-results/tr_persisted.data",
				ArtifactComplete: true,
				Summary:          "tests passed",
			}},
		},
		{
			Role:      messages.MessageRoleToolCall,
			Content:   "checking",
			Timestamp: timestamp,
			Usage:     &messages.Usage{InputTokens: 19, OutputTokens: 10, TotalTokens: 29},
			ToolCalls: []messages.ToolCall{{
				ID:        "call_1",
				Name:      "echo",
				Arguments: `{"input":"hello"}`,
			}},
		},
		{
			Role:      messages.MessageRoleToolResult,
			Timestamp: timestamp,
			ToolResults: []messages.ToolResult{{
				ToolCallID: "call_1",
				Name:       "echo",
				Content:    `{"output":"hello"}`,
			}},
		},
	}

	manager, err := NewContextManagerFromMessageList(sessionFolder, want)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}

	reloaded, err := LoadContextManagerFromSessionID(sessionFolder, manager.GetSessionID())
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() error = %v", err)
	}

	if got := reloaded.CloneMessageList(); !reflect.DeepEqual(got, want) {
		t.Fatalf("reloaded messages = %#v, want %#v", got, want)
	}
}

func TestAppendMessageAddsTimestampAndPersistsUsage(t *testing.T) {
	manager := newTestContextManager(t)
	usage := &messages.Usage{InputTokens: 336, OutputTokens: 41, TotalTokens: 377}
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleAssistant, Content: "ok", Usage: usage}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	reloaded, err := LoadContextManagerFromSessionID(manager.GetSessionFolder(), manager.GetSessionID())
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() error = %v", err)
	}
	got := reloaded.CloneMessageList()
	if len(got) != 1 || got[0].Timestamp.IsZero() {
		t.Fatalf("messages = %#v, want a timestamped message", got)
	}
	if got[0].Usage == nil || *got[0].Usage != *usage {
		t.Fatalf("usage = %#v, want %#v", got[0].Usage, usage)
	}
}

func TestAppendMessageRepairsPersistedOrphanToolCallBeforeNextUser(t *testing.T) {
	sessionFolder := t.TempDir()
	sessionID := newSessionID()
	if err := saveCurrentSession(sessionFolder, sessionID); err != nil {
		t.Fatalf("saveCurrentSession() error = %v", err)
	}
	manager, err := LoadContextManagerFromSessionID(sessionFolder, sessionID)
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() error = %v", err)
	}
	if err := manager.AppendMessage(messages.Message{
		Role:    messages.MessageRoleToolCall,
		Content: "Checking the device.",
		ToolCalls: []messages.ToolCall{{
			ID:        "call_orphan",
			Name:      "slow",
			Arguments: `{}`,
		}},
	}); err != nil {
		t.Fatalf("AppendMessage(tool call) error = %v", err)
	}

	reloaded, err := LoadContextManagerFromSessionID(sessionFolder, sessionID)
	if err != nil {
		t.Fatalf("reload context manager error = %v", err)
	}
	if err := reloaded.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "continue"}); err != nil {
		t.Fatalf("AppendMessage(user) error = %v", err)
	}

	dump := reloaded.MessageListDump()
	if len(dump.Messages) != 3 {
		t.Fatalf("messages = %#v, want tool call, interrupted result, user", dump.Messages)
	}
	if dump.Messages[1].Role != messages.MessageRoleToolResult || len(dump.Messages[1].ToolResults) != 1 {
		t.Fatalf("repair message = %#v, want one tool result", dump.Messages[1])
	}
	if result := dump.Messages[1].ToolResults[0]; result.ToolCallID != "call_orphan" || result.Content != interruptedToolResultContent {
		t.Fatalf("repair result = %#v", result)
	}
	if dump.Messages[2].Role != messages.MessageRoleUser {
		t.Fatalf("last message = %#v, want user", dump.Messages[2])
	}

	messages := messages.ConvertMessageList(reloaded.CloneMessageList())
	if len(messages) != 3 || messages[0].Role != llms.ChatMessageTypeAI || messages[1].Role != llms.ChatMessageTypeTool || messages[2].Role != llms.ChatMessageTypeHuman {
		t.Fatalf("provider messages = %#v, want assistant/tool/user", messages)
	}
}

func TestAppendMessageRepairsToolCallBeforeHookExpandedUserBatch(t *testing.T) {
	manager := newTestContextManager(t)
	if err := manager.AppendMessage(messages.Message{
		Role: messages.MessageRoleToolCall,
		ToolCalls: []messages.ToolCall{{
			ID:        "call_1",
			Name:      "first",
			Arguments: `{}`,
		}},
	}); err != nil {
		t.Fatalf("AppendMessage(tool call) error = %v", err)
	}
	manager.AddAppendMessageHooks([]AppendMessageHook{func(message messages.Message) AppendMessageHookResult {
		if message.Role != messages.MessageRoleUser {
			return AppendMessageHookResult{Message: &message}
		}
		return AppendMessageHookResult{
			Before:  []messages.Message{{Role: messages.MessageRoleState, Content: "device state"}},
			Message: &message,
		}
	}})
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "continue"}); err != nil {
		t.Fatalf("AppendMessage(user) error = %v", err)
	}

	dump := manager.MessageListDump()
	if len(dump.Messages) != 4 {
		t.Fatalf("messages = %#v, want tool call, result, state, user", dump.Messages)
	}
	if got := dump.Messages[1].ToolResults[0]; got.ToolCallID != "call_1" || got.Content != interruptedToolResultContent {
		t.Fatalf("repair result = %#v", got)
	}
	if dump.Messages[2].Role != messages.MessageRoleState || dump.Messages[3].Role != messages.MessageRoleUser {
		t.Fatalf("tail messages = %#v, want state then user", dump.Messages[2:])
	}
}

func TestAppendMessagePairsLegacyMissingToolCallIDByName(t *testing.T) {
	manager := newTestContextManager(t)
	if err := manager.AppendMessage(messages.Message{
		Role: messages.MessageRoleToolCall,
		ToolCalls: []messages.ToolCall{{
			Name:      "echo",
			Arguments: `{}`,
		}},
	}); err != nil {
		t.Fatalf("AppendMessage(tool call) error = %v", err)
	}
	if err := manager.AppendMessage(messages.Message{
		Role: messages.MessageRoleToolResult,
		ToolResults: []messages.ToolResult{{
			ToolCallID: "call_legacy",
			Name:       "echo",
			Content:    "ok",
		}},
	}); err != nil {
		t.Fatalf("AppendMessage(tool result) error = %v", err)
	}

	messages := messages.ConvertMessageList(manager.CloneMessageList())
	if len(messages) != 2 {
		t.Fatalf("messages = %#v, want paired call and result", messages)
	}
	call, ok := messages[0].Parts[0].(llms.ToolCall)
	if !ok || call.ID != "ctx_tool_call_0_0" {
		t.Fatalf("tool call = %#v, want stable fallback ID", messages[0])
	}
	response, ok := messages[1].Parts[0].(llms.ToolCallResponse)
	if !ok || response.ToolCallID != call.ID {
		t.Fatalf("tool response = %#v, want ID %q", messages[1], call.ID)
	}
}

func TestConvertStandardMessageToContextManagerMessage_ReasoningContent(t *testing.T) {
	message := messages.ConvertChoiceToContextManagerMessage(llms.ContentChoice{
		ReasoningContent: "thinking",
		Content:          "answer",
	})
	want := "answer"
	if message.Content != want {
		t.Fatalf("content = %q, want %q", message.Content, want)
	}
	if message.ReasoningContent != "thinking" {
		t.Fatalf("reasoning_content = %q, want thinking", message.ReasoningContent)
	}
}

func TestContextManagerAppendMessageHookModifiesMessage(t *testing.T) {
	manager := newTestContextManager(t)
	manager.AddAppendMessageHooks([]AppendMessageHook{func(message messages.Message) AppendMessageHookResult {
		message.Content = strings.ToUpper(message.Content)
		return AppendMessageHookResult{Message: &message}
	}})

	manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"})

	dump := manager.MessageListDump()
	if len(dump.Messages) != 1 {
		t.Fatalf("messages = %#v, want one entry", dump.Messages)
	}
	if dump.Messages[0].Content != "HELLO" {
		t.Fatalf("content = %q, want %q", dump.Messages[0].Content, "HELLO")
	}
}

func TestContextManagerAppendMessageHookInjectsBeforeAndAfter(t *testing.T) {
	manager := newTestContextManager(t)
	manager.AddAppendMessageHooks([]AppendMessageHook{func(message messages.Message) AppendMessageHookResult {
		modified := message
		modified.Content = "core:" + message.Content
		return AppendMessageHookResult{
			Before:  []messages.Message{{Role: messages.MessageRoleNotice, Content: "before"}},
			Message: &modified,
			After:   []messages.Message{{Role: messages.MessageRoleNotice, Content: "after"}},
		}
	}})

	manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"})

	dump := manager.MessageListDump()
	if got, want := len(dump.Messages), 3; got != want {
		t.Fatalf("message count = %d, want %d", got, want)
	}
	if dump.Messages[0].Content != "before" || dump.Messages[1].Content != "core:hello" || dump.Messages[2].Content != "after" {
		t.Fatalf("messages = %#v", dump.Messages)
	}
}

func TestContextManagerAppendMessageHookCanDropOriginalMessage(t *testing.T) {
	manager := newTestContextManager(t)
	manager.AddAppendMessageHooks([]AppendMessageHook{func(message messages.Message) AppendMessageHookResult {
		return AppendMessageHookResult{
			Before: []messages.Message{{Role: messages.MessageRoleSystem, Content: "replacement"}},
		}
	}})

	manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"})

	dump := manager.MessageListDump()
	if len(dump.Messages) != 1 {
		t.Fatalf("messages = %#v, want one replacement entry", dump.Messages)
	}
	if dump.Messages[0].Role != messages.MessageRoleSystem || dump.Messages[0].Content != "replacement" {
		t.Fatalf("message = %#v", dump.Messages[0])
	}
}

func TestContextManagerConcurrentAppendWithStatefulHook(t *testing.T) {
	manager := newTestContextManager(t)
	var hookMu sync.Mutex
	var hookCalls int
	manager.AddAppendMessageHook(func(message messages.Message) AppendMessageHookResult {
		hookMu.Lock()
		hookCalls++
		hookMu.Unlock()
		return AppendMessageHookResult{Message: &message}
	})

	const appends = 32
	var wg sync.WaitGroup
	for range appends {
		wg.Go(func() {
			if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
				t.Errorf("AppendMessage() error = %v", err)
			}
		})
	}
	wg.Wait()

	hookMu.Lock()
	gotHookCalls := hookCalls
	hookMu.Unlock()
	if gotHookCalls != appends {
		t.Fatalf("hook calls = %d, want %d", gotHookCalls, appends)
	}
	if got := len(manager.MessageListDump().Messages); got != appends {
		t.Fatalf("message count = %d, want %d", got, appends)
	}
}

func TestStoreAttachmentPersistsMetadataOnly(t *testing.T) {
	manager := newTestContextManager(t)
	stored, err := manager.StoreAttachment("image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatalf("StoreAttachment() error = %v", err)
	}
	if stored.FilePath == "" || stored.FileSize != 9 || stored.MIMEType != "image/png" {
		t.Fatalf("stored attachment = %#v", stored)
	}

	manager.AppendMessage(messages.Message{
		Role:        messages.MessageRoleUser,
		Content:     "see image",
		Attachments: []messages.Attachment{stored},
	})

	messages := messages.ConvertMessageList(manager.CloneMessageList())
	if len(messages) != 1 || len(messages[0].Parts) != 2 {
		t.Fatalf("messages = %#v, want text + binary parts", messages)
	}
	binaryPart, ok := messages[0].Parts[1].(llms.BinaryContent)
	if !ok || string(binaryPart.Data) != "png-bytes" {
		t.Fatalf("binary part = %#v", messages[0].Parts[1])
	}
}

func TestScreenshotAttachmentIsExposed(t *testing.T) {
	manager := newTestContextManager(t)
	stored, err := manager.StoreAttachment("image/jpeg", []byte("jpeg-bytes"))
	if err != nil {
		t.Fatalf("StoreAttachment() error = %v", err)
	}
	stored.Source = messages.AttachmentSourceScreenshotObservation
	if err := manager.AppendMessage(messages.Message{
		Role:        messages.MessageRoleUser,
		Content:     "screenshot",
		Attachments: []messages.Attachment{stored},
	}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	messages := messages.ConvertMessageList(manager.CloneMessageList())
	if len(messages) != 1 || len(messages[0].Parts) != 2 {
		t.Fatalf("messages = %#v, want text + binary", messages)
	}
	data, ok := messages[0].Parts[1].(llms.BinaryContent)
	if !ok || string(data.Data) != "jpeg-bytes" {
		t.Fatalf("attachment = %#v, want jpeg binary", messages[0].Parts[1])
	}
}

func TestLoadContextManagerFromSessionIDReloadsMessages(t *testing.T) {
	sessionFolder := t.TempDir()
	sessionID := newSessionID()
	if err := saveCurrentSession(sessionFolder, sessionID); err != nil {
		t.Fatalf("saveCurrentSession() error = %v", err)
	}
	manager, err := LoadContextManagerFromSessionID(sessionFolder, sessionID)
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() error = %v", err)
	}

	manager.AppendMessage(messages.Message{Role: messages.MessageRoleSystem, Content: "system"})
	manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"})

	reloaded, err := LoadContextManagerFromSessionID(sessionFolder, sessionID)
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() error = %v", err)
	}

	dump := reloaded.MessageListDump()
	if got, want := dump.SessionID, sessionID; got != want {
		t.Fatalf("session id = %q, want %q", got, want)
	}
	if got, want := len(dump.Messages), 2; got != want {
		t.Fatalf("message count = %d, want %d", got, want)
	}
	if dump.Messages[0].Content != "system" || dump.Messages[1].Content != "hello" {
		t.Fatalf("unexpected reloaded messages = %#v", dump.Messages)
	}

	sessionFile := filepath.Join(sessionFolder, sessionID+".jsonl")
	if _, err := os.Stat(sessionFile); err != nil {
		t.Fatalf("session file missing: %v", err)
	}
}

func TestConvertToStandardMessageListLoadsAttachmentFromFilePath(t *testing.T) {
	manager := newTestContextManager(t)
	filePath := filepath.Join(t.TempDir(), "manual.png")
	if err := os.WriteFile(filePath, []byte("manual-bytes"), 0o644); err != nil {
		t.Fatalf("write attachment file: %v", err)
	}
	manager.AppendMessage(messages.Message{
		Role:    messages.MessageRoleUser,
		Content: "see image",
		Attachments: []messages.Attachment{{
			MIMEType: "image/png",
			FileSize: 12,
			FilePath: filePath,
		}},
	})

	messages := messages.ConvertMessageList(manager.CloneMessageList())
	if len(messages) != 1 || len(messages[0].Parts) != 2 {
		t.Fatalf("messages = %#v, want text + binary parts", messages)
	}
	binaryPart, ok := messages[0].Parts[1].(llms.BinaryContent)
	if !ok || string(binaryPart.Data) != "manual-bytes" {
		t.Fatalf("binary part = %#v", messages[0].Parts[1])
	}
}

func TestConvertMessageListUsesProvidedSlice(t *testing.T) {
	manager := newTestContextManager(t)
	stored, err := manager.StoreAttachment("image/png", []byte("png-bytes"))
	if err != nil {
		t.Fatalf("StoreAttachment() error = %v", err)
	}
	stored.Source = messages.AttachmentSourceScreenshotObservation
	if err := manager.AppendMessage(messages.Message{
		Role:        messages.MessageRoleUser,
		Content:     "caption",
		Attachments: []messages.Attachment{stored},
	}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	cloned := manager.CloneMessageList()
	if len(cloned) != 1 || len(cloned[0].Attachments) != 1 {
		t.Fatalf("clone = %#v", cloned)
	}
	if cloned[0].Attachments[0].Source != messages.AttachmentSourceScreenshotObservation {
		t.Fatalf("Source = %q", cloned[0].Attachments[0].Source)
	}

	// Ephemeral prune: drop attachment, keep text — ConvertMessageList must not read the file.
	cloned[0].Attachments = nil
	cloned[0].Content = "caption\n[Image omitted]"
	converted := messages.ConvertMessageList(cloned)
	if len(converted) != 1 {
		t.Fatalf("converted len = %d", len(converted))
	}
	for _, part := range converted[0].Parts {
		if _, ok := part.(llms.BinaryContent); ok {
			t.Fatalf("expected no binary parts after ephemeral prune, got %#v", converted[0].Parts)
		}
	}
	dump := manager.MessageListDump()
	if len(dump.Messages) != 1 || len(dump.Messages[0].Attachments) != 1 {
		t.Fatalf("persisted dump mutated: %#v", dump)
	}
}

func TestConvertToStandardMessageListReportsMissingAttachment(t *testing.T) {
	manager := newTestContextManager(t)
	manager.AppendMessage(messages.Message{
		Role:    messages.MessageRoleUser,
		Content: "see image",
		Attachments: []messages.Attachment{{
			MIMEType: "image/png",
			FileSize: 12,
			FilePath: filepath.Join(t.TempDir(), "missing.png"),
		}},
	})

	messages := messages.ConvertMessageList(manager.CloneMessageList())
	if len(messages) != 1 || len(messages[0].Parts) != 2 {
		t.Fatalf("messages = %#v, want text + omitted attachment notice", messages)
	}
	textPart, ok := messages[0].Parts[1].(llms.TextContent)
	if !ok || !strings.Contains(textPart.Text, "Attachment omitted") {
		t.Fatalf("missing attachment part = %#v", messages[0].Parts[1])
	}
}

func TestContextManagerSwitchKeepsManagerAndHooks(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := NewContextManager(sessionFolder, "system")
	if err != nil {
		t.Fatalf("NewContextManager() error = %v", err)
	}
	managerPointer := manager
	var hookCalls int
	manager.AddAppendMessageHook(func(message messages.Message) AppendMessageHookResult {
		hookCalls++
		message.Content = "hooked:" + message.Content
		return AppendMessageHookResult{Message: &message}
	})

	candidate, err := NewContextManagerRevisionFromMessageList(manager, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
		{Role: messages.MessageRoleUser, Content: "revision"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerRevisionFromMessageList() error = %v", err)
	}
	parentID := manager.GetSessionID()
	if err := manager.Activate(candidate); err != nil {
		t.Fatalf("Activate() error = %v", err)
	}

	if manager != managerPointer {
		t.Fatal("manager pointer changed during session activation")
	}
	if manager.GetSessionID() == parentID {
		t.Fatal("session ID did not change during activation")
	}
	activeID := manager.GetSessionID()
	if got := CurrentSessionID(sessionFolder); got != manager.GetSessionID() {
		t.Fatalf("current session file = %q, manager session = %q", got, manager.GetSessionID())
	}
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "next"}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}
	if hookCalls != 1 {
		t.Fatalf("hook calls = %d, want 1 after switching sessions", hookCalls)
	}
	if got := manager.CloneMessageList()[2].Content; got != "hooked:next" {
		t.Fatalf("switched session message = %q, want hooked message", got)
	}
	reloaded, err := LoadContextManagerFromSessionID(sessionFolder, activeID)
	if err != nil {
		t.Fatalf("reload active session: %v", err)
	}
	if got := reloaded.CloneMessageList()[2].Content; got != "hooked:next" {
		t.Fatalf("reloaded active session message = %q, want hooked message", got)
	}
	parent, err := LoadContextManagerFromSessionID(sessionFolder, parentID)
	if err != nil {
		t.Fatalf("reload parent session: %v", err)
	}
	if len(parent.CloneMessageList()) != 1 {
		t.Fatalf("parent session was appended after activation: %#v", parent.CloneMessageList())
	}
}

func TestContextManagerRejectsStaleRevisionActivation(t *testing.T) {
	manager, err := NewContextManager(t.TempDir(), "system")
	if err != nil {
		t.Fatalf("NewContextManager() error = %v", err)
	}
	candidate, err := NewContextManagerRevisionFromMessageList(manager, manager.CloneMessageList())
	if err != nil {
		t.Fatalf("NewContextManagerRevisionFromMessageList() error = %v", err)
	}
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "new parent message"}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}
	if err := manager.Activate(candidate); err == nil {
		t.Fatal("Activate() succeeded for a revision based on a stale parent")
	}
	if got := manager.GetSessionID(); got != candidate.GetParentSessionID() {
		t.Fatalf("session changed after stale activation: got %q, want parent %q", got, candidate.GetParentSessionID())
	}
}

func TestContextManagerSwitchRejectsMissingSession(t *testing.T) {
	manager, err := NewContextManager(t.TempDir(), "system")
	if err != nil {
		t.Fatalf("NewContextManager() error = %v", err)
	}
	originalID := manager.GetSessionID()
	if err := manager.SwitchSession("s_missing"); err == nil {
		t.Fatal("SwitchSession() succeeded for a missing session")
	}
	if got := manager.GetSessionID(); got != originalID {
		t.Fatalf("session changed after missing-session switch: got %q, want %q", got, originalID)
	}
}
