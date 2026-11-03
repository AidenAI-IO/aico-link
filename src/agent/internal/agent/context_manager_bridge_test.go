package agent

import (
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/llms"
)

func TestInitializeContextManagerStartsNewSessionWhenSystemPromptChanges(t *testing.T) {
	sessionFolder := t.TempDir()

	manager, err := InitializeContextManager("system v1", sessionFolder, nil)
	if err != nil {
		t.Fatalf("freshNewContextManager() error = %v", err)
	}
	rawMessages := manager.CloneMessageList()
	messageList := messages.ConvertMessageList(rawMessages)
	if len(messageList) != 1 {
		t.Fatalf("messages = %d, want 1", len(messageList))
	}
	if text := messageText(messageList); text != "system v1\n" {
		t.Fatalf("system prompt = %q, want original system v1", text)
	}
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "first request"}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}
	originalSessionID := manager.GetSessionID()

	reloaded, err := InitializeContextManager("system v2", sessionFolder, nil)
	if err != nil {
		t.Fatalf("reload freshNewContextManager() error = %v", err)
	}
	reloadedMessages := messages.ConvertMessageList(reloaded.CloneMessageList())
	if reloaded.GetSessionID() == originalSessionID {
		t.Fatal("system prompt change reused the existing session")
	}
	if len(reloadedMessages) != 1 {
		t.Fatalf("messages = %d, want only the new system prompt", len(reloadedMessages))
	}
	if text := messageText(reloadedMessages); text != "system v2\n" {
		t.Fatalf("new session system prompt = %q, want system v2", text)
	}

	original, err := contextmanager.LoadContextManagerFromSessionID(sessionFolder, originalSessionID)
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() error = %v", err)
	}
	if text := messageText(messages.ConvertMessageList(original.CloneMessageList())); text != "system v1\nfirst request\n" {
		t.Fatalf("original session was modified: %q", text)
	}
}

func TestInitializeContextManagerReusesSessionWhenSystemPromptMatches(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := InitializeContextManager("system", sessionFolder, nil)
	if err != nil {
		t.Fatalf("InitializeContextManager() error = %v", err)
	}
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	reloaded, err := InitializeContextManager("system", sessionFolder, nil)
	if err != nil {
		t.Fatalf("InitializeContextManager() reload error = %v", err)
	}
	if reloaded.GetSessionID() != manager.GetSessionID() {
		t.Fatal("unchanged system prompt started a new session")
	}
	if text := messageText(messages.ConvertMessageList(reloaded.CloneMessageList())); text != "system\nhello\n" {
		t.Fatalf("reloaded session = %q", text)
	}
}

func TestInitializeContextManagerDateRolloverPreservesConfigurationChanges(t *testing.T) {
	originalNow := promptNow
	t.Cleanup(func() { promptNow = originalNow })
	for _, tc := range []struct {
		name   string
		config AgentConfig
	}{
		{"timezone", AgentConfig{Timezone: "Asia/Shanghai"}},
		{"locale", AgentConfig{Locale: "zh-CN"}},
		{"instruction", AgentConfig{Instruction: "Current date: 2030-01-01 (Tuesday, timezone: UTC)"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			promptNow = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }
			folder := t.TempDir()
			original, err := InitializeContextManager(testPromptProfile(AgentConfig{}).SystemPrompt, folder, nil)
			if err != nil {
				t.Fatal(err)
			}
			promptNow = func() time.Time { return time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC) }
			changed, err := InitializeContextManager(testPromptProfile(tc.config).SystemPrompt, folder, nil)
			if err != nil {
				t.Fatal(err)
			}
			if changed.GetSessionID() == original.GetSessionID() {
				t.Fatal("date rollover hid a configuration change")
			}
		})
	}
}

func TestInitializeContextManagerReusesSessionWhenOnlyUserProfileChanges(t *testing.T) {
	originalNow := promptNow
	t.Cleanup(func() { promptNow = originalNow })
	promptNow = func() time.Time { return time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC) }
	base := testPromptProfile(AgentConfig{}).SystemPrompt
	folder := t.TempDir()
	original, err := InitializeContextManager(withUserProfileSection(base, "- Prefers metric units."), folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := original.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "hello"}); err != nil {
		t.Fatal(err)
	}

	for _, profile := range []string{"- Prefers imperial units.", ""} {
		reloaded, err := InitializeContextManager(withUserProfileSection(base, profile), folder, nil)
		if err != nil {
			t.Fatal(err)
		}
		if reloaded.GetSessionID() != original.GetSessionID() {
			t.Fatalf("profile %q started a new session", profile)
		}
		dump := reloaded.MessageListDump().Messages
		if last := dump[len(dump)-1]; last.Role != messages.MessageRoleUser || last.Content != "hello" {
			t.Fatalf("profile change appended %#v, want transcript untouched", last)
		}
	}

	changed, err := InitializeContextManager(withUserProfileSection(testPromptProfile(AgentConfig{Locale: "zh-CN"}).SystemPrompt, "- Prefers metric units."), folder, nil)
	if err != nil {
		t.Fatal(err)
	}
	if changed.GetSessionID() == original.GetSessionID() {
		t.Fatal("profile section hid a configuration change")
	}
}

func TestUserMessageFromInputPreservesAttachments(t *testing.T) {
	manager, err := InitializeContextManager("system", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("freshNewContextManager() error = %v", err)
	}
	if err := manager.AppendMessage(userMessageFromInput(manager, "hello", []InputAttachment{{
		Kind:     "image",
		Name:     "screen.png",
		MIMEType: "image/png",
		Data:     []byte("data"),
	}})); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	messages := messages.ConvertMessageList(manager.CloneMessageList())
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	userMessage := messages[1]
	if userMessage.Role != llms.ChatMessageTypeHuman {
		t.Fatalf("role = %q, want human", userMessage.Role)
	}
	if len(userMessage.Parts) != 2 {
		t.Fatalf("parts = %#v, want text + binary attachment", userMessage.Parts)
	}
}

func TestVisualFollowupMarksScreenshotObservationSource(t *testing.T) {
	manager, err := InitializeContextManager("system", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("InitializeContextManager() error = %v", err)
	}
	msg := visualFollowupMessageFromLLMContent(manager, llms.MessageContent{
		Role: llms.ChatMessageTypeHuman,
		Parts: []llms.ContentPart{
			llms.TextPart("This image is the screenshot observation returned by the screenshot tool."),
			llms.BinaryPart("image/jpeg", []byte("jpeg-bytes")),
		},
	})
	if msg.Role != messages.MessageRoleState {
		t.Fatalf("Role = %q, want state", msg.Role)
	}
	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %#v", msg.Attachments)
	}
	if msg.Attachments[0].Source != messages.AttachmentSourceScreenshotObservation {
		t.Fatalf("Source = %q", msg.Attachments[0].Source)
	}
}

func TestUserMessageAttachmentsRemainUnmarked(t *testing.T) {
	manager, err := InitializeContextManager("system", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("InitializeContextManager() error = %v", err)
	}
	msg := userMessageFromInput(manager, "hello", []InputAttachment{{
		Kind:     AttachmentKindImage,
		MIMEType: "image/png",
		Data:     []byte("png"),
	}})
	if len(msg.Attachments) != 1 {
		t.Fatalf("attachments = %#v", msg.Attachments)
	}
	if msg.Attachments[0].Source != "" {
		t.Fatalf("user upload Source = %q, want empty", msg.Attachments[0].Source)
	}
}

func TestUserMessageAttachmentPromptIncludesImageDimensions(t *testing.T) {
	manager, err := InitializeContextManager("system", t.TempDir(), nil)
	if err != nil {
		t.Fatalf("InitializeContextManager() error = %v", err)
	}
	msg := userMessageFromInput(manager, "inspect", []InputAttachment{{
		Kind:     AttachmentKindImage,
		MIMEType: "image/jpeg",
		Width:    447,
		Height:   972,
		Data:     []byte("jpeg"),
	}})
	if !strings.Contains(msg.Content, "width=447 height=972") {
		t.Fatalf("content missing image dimensions: %q", msg.Content)
	}
}
