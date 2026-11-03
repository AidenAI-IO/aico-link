package agent

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
)

func TestWithUserProfileSectionReplacesAndRemoves(t *testing.T) {
	base := "You are the Aiden agent."
	withProfile := withUserProfileSection(base, "- (preference) 用户希望回复尽量简短。")
	if !strings.HasPrefix(withProfile, base+userProfileSectionHeader) || !strings.HasSuffix(withProfile, "用户希望回复尽量简短。") {
		t.Fatalf("added section = %q", withProfile)
	}
	replaced := withUserProfileSection(withProfile, "- (rule) Always use metric units.")
	if strings.Contains(replaced, "简短") || strings.Count(replaced, "## User profile") != 1 {
		t.Fatalf("replaced section = %q", replaced)
	}
	if got := withUserProfileSection(replaced, ""); got != base {
		t.Fatalf("removed section = %q, want %q", got, base)
	}
}

// The system prompt is written once per session and a session can last for
// days, so a changed profile must replace it in place: a memory the user
// forgets has to leave the prompt before the next model request.
func TestRefreshUserProfileSectionRewritesSystemMessageOnChange(t *testing.T) {
	ctx := context.Background()
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	if _, err := store.AddMemory(ctx, MemoryItem{ID: "mem_brevity", Type: "preference", Priority: 80, Confidence: 0.9, Content: "用户希望回复尽量简短。", EvidenceExcerpts: []string{"说短一点"}}); err != nil {
		t.Fatal(err)
	}
	profile := func() string {
		value, err := store.ProfilePrompt(ctx, userProfilePromptMaxChars)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), []messages.Message{
		{Role: messages.MessageRoleSystem, Content: withUserProfileSection("You are the Aiden agent.", profile())},
		{Role: messages.MessageRoleUser, Content: "hello"},
		{Role: messages.MessageRoleAssistant, Content: "hi", ResponsesResponseID: "resp_1"},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID := manager.GetSessionID()

	// Unchanged profile: the session is left alone.
	if err := refreshUserProfileSection(manager, profile()); err != nil {
		t.Fatal(err)
	}
	if manager.GetSessionID() != sessionID {
		t.Fatal("unchanged profile created a new revision")
	}

	if err := store.Forget(ctx, "mem_brevity", "user asked"); err != nil {
		t.Fatal(err)
	}
	if err := refreshUserProfileSection(manager, profile()); err != nil {
		t.Fatal(err)
	}
	messageList := manager.MessageListDump().Messages
	if manager.GetSessionID() == sessionID {
		t.Fatal("changed profile did not create a new revision")
	}
	if messageList[0].Content != "You are the Aiden agent." {
		t.Fatalf("system message = %q, want the profile section removed", messageList[0].Content)
	}
	if len(messageList) != 3 || messageList[1].Content != "hello" || messageList[2].Content != "hi" {
		t.Fatalf("conversation not preserved: %#v", messageList)
	}
	if messageList[2].ResponsesResponseID != "" {
		t.Fatal("revision kept a stale Responses ID")
	}
	if ok, err := manager.HasSessionAncestor(sessionID); err != nil || !ok {
		t.Fatalf("revision lost its lineage to %s: ok=%v err=%v", sessionID, ok, err)
	}
}
