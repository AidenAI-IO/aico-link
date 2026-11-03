package contextmanager

import (
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/tokencounter"
	"os"
	"testing"
)

func TestTokenMetadataPersistsAndRebuildsUsageWithDeltas(t *testing.T) {
	folder := t.TempDir()
	manager, err := NewContextManager(folder, "system")
	if err != nil {
		t.Fatal(err)
	}
	check := func(want int, measured bool) {
		t.Helper()
		meta, found, err := loadSessionMetadata(folder, manager.GetSessionID())
		if err != nil || !found || meta.Tokens == nil {
			t.Fatalf("metadata = %+v, found=%v err=%v", meta, found, err)
		}
		if meta.Tokens.TokenCount != want || meta.Tokens.HasTokenUsage != measured ||
			meta.Tokens.MessageCount != len(manager.CloneMessageList()) {
			t.Fatalf("token metadata = %+v, want count=%d measured=%v", meta.Tokens, want, measured)
		}
		loaded, err := LoadContextManagerFromSessionID(folder, manager.GetSessionID())
		if err != nil {
			t.Fatal(err)
		}
		if loaded.TokenCount() != want || loaded.HasTokenUsage() != measured {
			t.Fatalf("reloaded occupancy = %+v", loaded.TokenSnapshot())
		}
	}
	check(tokencounter.EstimateTextTokens("system"), false)
	reply := messages.Message{
		Role: messages.MessageRoleAssistant, Content: "ok",
		Usage: &messages.Usage{InputTokens: 800, OutputTokens: 20, TotalTokens: 820},
	}
	// Direct session appends must update the sidecar too.
	if err := manager.currentSession.AppendMessages([]messages.Message{reply, {
		Role: messages.MessageRoleUser, Content: "1234",
	}}); err != nil {
		t.Fatal(err)
	}
	check(821, true)

	// Simulate a crash after JSONL commit but before metadata refresh.
	stale, _, err := loadSessionMetadata(folder, manager.GetSessionID())
	if err != nil {
		t.Fatal(err)
	}
	reply.Usage = &messages.Usage{InputTokens: 900, OutputTokens: 30}
	if err := manager.AppendMessage(reply); err != nil {
		t.Fatal(err)
	}
	check(930, true)
	if err := saveSessionMetadata(folder, manager.GetSessionID(), stale); err != nil {
		t.Fatal(err)
	}
	manager, err = LoadContextManagerFromSessionID(folder, manager.GetSessionID())
	if err != nil {
		t.Fatal(err)
	}
	check(930, true)

	revision, err := NewContextManagerRevisionFromMessageList(manager, manager.CloneMessageList())
	if err != nil {
		t.Fatal(err)
	}
	manager = revision
	check(tokencounter.EstimateMessagesTokens(manager.CloneMessageList()), false)
	if err := manager.AppendMessage(reply); err != nil {
		t.Fatal(err)
	}
	check(930, true)
}

func TestToolCallUsageEstablishesBaselineAndSurvivesReload(t *testing.T) {
	folder := t.TempDir()
	manager, err := NewContextManager(folder, "system")
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.currentSession.AppendMessages([]messages.Message{
		{Role: messages.MessageRoleAssistant, Usage: &messages.Usage{TotalTokens: 100}},
		{
			Role: messages.MessageRoleToolCall, Usage: &messages.Usage{TotalTokens: 250},
			ToolCalls: []messages.ToolCall{{ID: "call", Name: "shell", Arguments: "{}"}},
		},
		{Role: messages.MessageRoleToolResult, ToolResults: []messages.ToolResult{{ToolCallID: "call", Content: "1234"}}},
	}); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadContextManagerFromSessionID(folder, manager.GetSessionID())
	if err != nil {
		t.Fatal(err)
	}
	for _, current := range []*ContextManager{manager, loaded} {
		if current.TokenCount() != 251 || !current.HasTokenUsage() {
			t.Fatalf("tool call occupancy = %+v, want 251 measured", current.TokenSnapshot())
		}
	}
}

func TestCorruptRevisionMetadataDoesNotReviveInheritedUsage(t *testing.T) {
	folder := t.TempDir()
	manager, err := NewContextManagerFromMessageList(folder, []messages.Message{
		{Role: messages.MessageRoleAssistant, Content: "ok", Usage: &messages.Usage{TotalTokens: 900}},
	})
	if err != nil {
		t.Fatal(err)
	}
	revision, err := NewContextManagerRevisionFromMessageList(manager, manager.CloneMessageList())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sessionMetadataPath(folder, revision.GetSessionID()), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadContextManagerFromSessionID(folder, revision.GetSessionID())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.HasTokenUsage() || loaded.TokenCount() != tokencounter.EstimateTextTokens("ok") {
		t.Fatalf("corrupt revision revived inherited usage: %+v", loaded.TokenSnapshot())
	}
}

func TestTokenMetadataFailureDoesNotRetryCommittedAppend(t *testing.T) {
	folder := t.TempDir()
	manager, err := NewContextManager(folder, "system")
	if err != nil {
		t.Fatal(err)
	}
	path := sessionMetadataPath(folder, manager.GetSessionID())
	initial, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := manager.AppendMessage(messages.Message{
		Role: messages.MessageRoleAssistant, Usage: &messages.Usage{TotalTokens: 42},
	}); err != nil {
		t.Fatalf("committed append returned retryable error: %v", err)
	}
	if manager.TokenCount() != 42 {
		t.Fatalf("in-memory count = %d, want 42", manager.TokenCount())
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, initial, 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadContextManagerFromSessionID(folder, manager.GetSessionID())
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TokenCount() != 42 || len(loaded.CloneMessageList()) != 2 {
		t.Fatalf("recovered snapshot = %+v", loaded.TokenSnapshot())
	}
}

func TestLegacySessionWithoutMetadataReconstructsUsage(t *testing.T) {
	folder := t.TempDir()
	if err := appendSession(folder, "legacy", []messages.Message{
		{Role: messages.MessageRoleAssistant, Usage: &messages.Usage{TotalTokens: 100}},
		{Role: messages.MessageRoleUser, Content: "1234"},
	}); err != nil {
		t.Fatal(err)
	}
	manager, err := LoadContextManagerFromSessionID(folder, "legacy")
	if err != nil {
		t.Fatal(err)
	}
	if manager.TokenCount() != 101 || !manager.HasTokenUsage() {
		t.Fatalf("legacy occupancy = %+v", manager.TokenSnapshot())
	}
}
