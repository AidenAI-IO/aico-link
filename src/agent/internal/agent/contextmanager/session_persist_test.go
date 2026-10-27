package contextmanager

import (
	"aico-agent/internal/agent/messages"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSaveCurrentSessionAtomicallyReplacesFile(t *testing.T) {
	sessionFolder := t.TempDir()
	sessionPath := filepath.Join(sessionFolder, ".current_session")
	if err := os.WriteFile(sessionPath, []byte("old-session"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	before, err := os.Stat(sessionPath)
	if err != nil {
		t.Fatalf("Stat() before save error = %v", err)
	}

	const sessionID = "new-session"
	if err := saveCurrentSession(sessionFolder, sessionID); err != nil {
		t.Fatalf("saveCurrentSession() error = %v", err)
	}

	after, err := os.Stat(sessionPath)
	if err != nil {
		t.Fatalf("Stat() after save error = %v", err)
	}
	if os.SameFile(before, after) {
		t.Fatal("saveCurrentSession() updated the existing file instead of atomically replacing it")
	}
	if got := fetchCurrentSession(sessionFolder); got != sessionID {
		t.Fatalf("fetchCurrentSession() = %q, want %q", got, sessionID)
	}
	if got := after.Mode().Perm(); got != 0o644 {
		t.Fatalf(".current_session permissions = %o, want 644", got)
	}

	entries, err := os.ReadDir(sessionFolder)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".current_session-") {
			t.Errorf("temporary file was not removed: %s", entry.Name())
		}
	}
}

func TestContextManagerRevisionRecordsParentSessionID(t *testing.T) {
	sessionFolder := t.TempDir()
	parent, err := NewContextManagerFromMessageList(sessionFolder, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	if got := parent.GetParentSessionID(); got != "" {
		t.Fatalf("root GetParentSessionID() = %q, want empty", got)
	}

	revision, err := NewContextManagerRevisionFromMessageList(parent, parent.CloneMessageList())
	if err != nil {
		t.Fatalf("NewContextManagerRevisionFromMessageList() error = %v", err)
	}
	if revision.GetSessionID() == parent.GetSessionID() {
		t.Fatal("revision reused the parent session ID")
	}
	if got, want := revision.GetParentSessionID(), parent.GetSessionID(); got != want {
		t.Fatalf("revision GetParentSessionID() = %q, want %q", got, want)
	}

	// The lineage must survive a reload, since that is the only way a later
	// process can walk the revision chain.
	reloaded, err := LoadContextManagerFromSessionID(sessionFolder, revision.GetSessionID())
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() error = %v", err)
	}
	if got, want := reloaded.GetParentSessionID(), parent.GetSessionID(); got != want {
		t.Fatalf("reloaded GetParentSessionID() = %q, want %q", got, want)
	}
	if got, want := reloaded.MessageListDump().ParentSessionID, parent.GetSessionID(); got != want {
		t.Fatalf("dump parent_session_id = %q, want %q", got, want)
	}
}

func TestSessionMetadataSidecarIsAtomicAndJSONEncoded(t *testing.T) {
	sessionFolder := t.TempDir()
	const sessionID = "s_child"
	if err := saveSessionMetadata(sessionFolder, sessionID, sessionMetadata{ParentSessionID: "s_parent"}); err != nil {
		t.Fatalf("saveSessionMetadata() error = %v", err)
	}

	data, err := os.ReadFile(sessionMetadataPath(sessionFolder, sessionID))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(data, &decoded); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if decoded["parent_session_id"] != "s_parent" {
		t.Fatalf("parent_session_id = %v, want s_parent", decoded["parent_session_id"])
	}

	entries, err := os.ReadDir(sessionFolder)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		if strings.Contains(entry.Name(), ".meta.json-") {
			t.Errorf("temporary metadata file was not removed: %s", entry.Name())
		}
	}
}

func TestLoadSessionMetadataIgnoresLegacyArtifactScopeField(t *testing.T) {
	sessionFolder := t.TempDir()
	const sessionID = "s_legacy_scope"
	// Sidecars from builds that shared an artifact directory carried
	// artifact_scope_id. The field is gone, so it must decode as an unknown key
	// instead of failing the load.
	if err := os.WriteFile(
		sessionMetadataPath(sessionFolder, sessionID),
		[]byte(`{"artifact_scope_id":"s_old_scope","parent_session_id":"s_parent"}`),
		0o644,
	); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	metadata, found, err := loadSessionMetadata(sessionFolder, sessionID)
	if err != nil {
		t.Fatalf("loadSessionMetadata() error = %v", err)
	}
	if !found {
		t.Fatal("legacy sidecar was not found")
	}
	if metadata.ParentSessionID != "s_parent" {
		t.Fatalf("parent_session_id = %q, want s_parent", metadata.ParentSessionID)
	}
}

func TestSaveSessionMetadataRejectsSessionIDEscapingTheFolder(t *testing.T) {
	sessionFolder := t.TempDir()
	for _, sessionID := range []string{"", "..", "../escape", "nested/child"} {
		if err := saveSessionMetadata(sessionFolder, sessionID, sessionMetadata{}); err == nil {
			t.Fatalf("saveSessionMetadata(%q) error = nil, want rejection", sessionID)
		}
	}
}

func TestLoadContextManagerToleratesMissingAndCorruptMetadataSidecar(t *testing.T) {
	sessionFolder := t.TempDir()
	const sessionID = "s_legacy"
	if err := appendSession(sessionFolder, sessionID, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
	}); err != nil {
		t.Fatalf("appendSession() error = %v", err)
	}

	// A session written before the sidecar existed still loads, with unknown lineage.
	manager, err := LoadContextManagerFromSessionID(sessionFolder, sessionID)
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() without sidecar error = %v", err)
	}
	if got := manager.GetParentSessionID(); got != "" {
		t.Fatalf("GetParentSessionID() = %q, want empty for a legacy session", got)
	}

	if err := os.WriteFile(sessionMetadataPath(sessionFolder, sessionID), []byte("{not json"), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	manager, err = LoadContextManagerFromSessionID(sessionFolder, sessionID)
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() with corrupt sidecar error = %v", err)
	}
	if got := manager.GetParentSessionID(); got != "" {
		t.Fatalf("GetParentSessionID() = %q, want empty for a corrupt sidecar", got)
	}
	if len(manager.CloneMessageList()) != 1 {
		t.Fatalf("messages = %d, want the transcript to remain readable", len(manager.CloneMessageList()))
	}
}
