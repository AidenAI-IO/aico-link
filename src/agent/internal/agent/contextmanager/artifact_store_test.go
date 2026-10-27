package contextmanager

import (
	"aico-agent/internal/agent/messages"
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
)

func TestNewContextManagerStoresArtifactsBySessionAndWritesLineageSidecar(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := NewContextManagerFromMessageList(sessionFolder, nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	if filepath.Base(filepath.Dir(manager.ArtifactStoreRoot())) != manager.GetSessionID() {
		t.Fatalf("artifact root = %q, want session %q", manager.ArtifactStoreRoot(), manager.GetSessionID())
	}
	metadata, found, err := loadSessionMetadata(sessionFolder, manager.GetSessionID())
	if err != nil {
		t.Fatalf("loadSessionMetadata() error = %v", err)
	}
	if !found {
		t.Fatal("new session did not write a metadata sidecar")
	}
	if metadata.ParentSessionID != "" {
		t.Fatalf("parent_session_id = %q, want empty for a root session", metadata.ParentSessionID)
	}
	if metadata.CreatedAt.IsZero() {
		t.Fatal("created_at is zero")
	}
}

func TestContextManagerReturnsReadableArtifactFilePath(t *testing.T) {
	manager, err := NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	stored, err := manager.StoreArtifact("text/plain", []byte("shell-readable-result"), ArtifactMetadata{
		ToolName:   "shell",
		ToolCallID: "call_path",
	})
	if err != nil {
		t.Fatalf("StoreArtifact() error = %v", err)
	}
	if stored.Path == "" || !filepath.IsAbs(stored.Path) {
		t.Fatalf("StoreArtifact() path = %q, want absolute path", stored.Path)
	}
	if filepath.Dir(stored.Path) != manager.ArtifactStoreRoot() {
		t.Fatalf("StoreArtifact() path = %q, want file in %q", stored.Path, manager.ArtifactStoreRoot())
	}
	data, err := os.ReadFile(stored.Path)
	if err != nil {
		t.Fatalf("ReadFile(%s) error = %v", stored.Path, err)
	}
	if string(data) != "shell-readable-result" {
		t.Fatalf("artifact file content = %q", data)
	}
}

func TestContextManagerReusesArtifactWithSameContent(t *testing.T) {
	manager, err := NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	content := []byte("same tool result")
	first, err := manager.StoreArtifact("text/plain", content, ArtifactMetadata{
		ToolName:   "web_search",
		ToolCallID: "call_search",
	})
	if err != nil {
		t.Fatalf("first StoreArtifact() error = %v", err)
	}
	second, err := manager.StoreArtifact("text/plain", content, ArtifactMetadata{
		ToolName:   "shell",
		ToolCallID: "call_cat",
	})
	if err != nil {
		t.Fatalf("second StoreArtifact() error = %v", err)
	}
	if second.Path != first.Path {
		t.Fatalf("duplicate artifact paths = %q and %q, want content-addressed reuse", first.Path, second.Path)
	}
	if second.SHA256 == "" || second.SHA256 != first.SHA256 {
		t.Fatalf("duplicate artifact hashes = %q and %q", first.SHA256, second.SHA256)
	}

	entries, err := os.ReadDir(manager.ArtifactStoreRoot())
	if err != nil {
		t.Fatalf("ReadDir(%s) error = %v", manager.ArtifactStoreRoot(), err)
	}
	if len(entries) != 2 {
		t.Fatalf("artifact store entries = %d, want one data/metadata pair", len(entries))
	}
}

func TestContextManagerDoesNotReuseArtifactAcrossStoragePolicies(t *testing.T) {
	manager, err := NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	content := []byte("same bytes with different storage policy")
	plain, err := manager.StoreArtifact("text/plain", content, ArtifactMetadata{})
	if err != nil {
		t.Fatalf("plain StoreArtifact() error = %v", err)
	}
	structured, err := manager.StoreArtifact("application/json", content, ArtifactMetadata{})
	if err != nil {
		t.Fatalf("structured StoreArtifact() error = %v", err)
	}
	sensitive, err := manager.StoreArtifact("text/plain", content, ArtifactMetadata{Sensitive: true})
	if err != nil {
		t.Fatalf("sensitive StoreArtifact() error = %v", err)
	}
	if plain.Path == structured.Path || plain.Path == sensitive.Path || structured.Path == sensitive.Path {
		t.Fatalf("artifacts with different storage policies were merged: plain=%q structured=%q sensitive=%q", plain.Path, structured.Path, sensitive.Path)
	}
}

func TestContextManagerReturnsAbsoluteArtifactPathForRelativeSessionFolder(t *testing.T) {
	t.Chdir(t.TempDir())
	if err := os.MkdirAll("sessions", 0o700); err != nil {
		t.Fatalf("MkdirAll(sessions) error = %v", err)
	}
	manager, err := NewContextManagerFromMessageList("sessions", nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	stored, err := manager.StoreArtifact("text/plain", []byte("relative-root"), ArtifactMetadata{})
	if err != nil {
		t.Fatalf("StoreArtifact() error = %v", err)
	}
	if !filepath.IsAbs(stored.Path) {
		t.Fatalf("StoreArtifact() path = %q, want absolute path", stored.Path)
	}
	if data, err := os.ReadFile(stored.Path); err != nil || string(data) != "relative-root" {
		t.Fatalf("ReadFile(%s) = %q, error = %v", stored.Path, data, err)
	}
}

func TestToolResultMetadataPersistsButIsNotSentToModel(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := NewContextManagerFromMessageList(sessionFolder, nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	message := messages.Message{
		Role: messages.MessageRoleToolResult,
		ToolResults: []messages.ToolResult{{
			ToolCallID: "call_1",
			Name:       "shell",
			Content:    "bounded observation",
			Meta: &messages.ToolResultMeta{
				ArtifactPath:     "/tmp/tool-results/tr_example.data",
				OriginalBytes:    12_345,
				OriginalChars:    12_345,
				Complete:         false,
				ArtifactComplete: true,
				Reason:           "intrinsic_large",
				Summary:          "exit_code=1",
			},
		}},
	}
	if err := manager.AppendMessage(message); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}
	reloaded, err := LoadContextManagerFromSessionID(sessionFolder, manager.GetSessionID())
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() error = %v", err)
	}
	got := reloaded.CloneMessageList()[0].ToolResults[0]
	if got.Meta == nil || got.Meta.ArtifactPath != "/tmp/tool-results/tr_example.data" || got.Meta.Summary != "exit_code=1" {
		t.Fatalf("reloaded ToolResult meta = %#v", got.Meta)
	}

	standard := messages.ConvertMessageList(reloaded.CloneMessageList())
	response, ok := standard[0].Parts[0].(llms.ToolCallResponse)
	if !ok {
		t.Fatalf("standard part = %T, want ToolCallResponse", standard[0].Parts[0])
	}
	if response.Content != "bounded observation" {
		t.Fatalf("model content = %q", response.Content)
	}
}

func TestContextManagerRevisionPreservesArtifactPathWithoutSharingStore(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := NewContextManagerFromMessageList(sessionFolder, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "system"},
	})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	stored, err := manager.StoreArtifact("text/plain", []byte("persisted-result"), ArtifactMetadata{
		ToolName:   "shell",
		ToolCallID: "call_1",
	})
	if err != nil {
		t.Fatalf("StoreArtifact() error = %v", err)
	}
	if err := manager.AppendMessage(messages.Message{
		Role: messages.MessageRoleToolResult,
		ToolResults: []messages.ToolResult{{
			ToolCallID: "call_1",
			Name:       "shell",
			Content:    "bounded result",
			Meta:       &messages.ToolResultMeta{ArtifactPath: stored.Path, ArtifactComplete: true},
		}},
	}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}

	revision, err := NewContextManagerRevisionFromMessageList(manager, manager.CloneMessageList())
	if err != nil {
		t.Fatalf("NewContextManagerRevisionFromMessageList() error = %v", err)
	}
	if revision.GetSessionID() == manager.GetSessionID() {
		t.Fatal("revision reused transcript session ID")
	}
	if revision.ArtifactStoreRoot() == manager.ArtifactStoreRoot() {
		t.Fatalf("revision artifact store = %q, want independent session store", revision.ArtifactStoreRoot())
	}
	revisionStored, err := revision.StoreArtifact("text/plain", []byte("revision-result"), ArtifactMetadata{ToolName: "shell"})
	if err != nil {
		t.Fatalf("revision StoreArtifact() error = %v", err)
	}
	if filepath.Dir(revisionStored.Path) != revision.ArtifactStoreRoot() || filepath.Base(filepath.Dir(revision.ArtifactStoreRoot())) != revision.GetSessionID() {
		t.Fatalf("revision artifact path = %q, want revision-owned directory %q", revisionStored.Path, revision.ArtifactStoreRoot())
	}
	if data, err := os.ReadFile(stored.Path); err != nil || string(data) != "persisted-result" {
		t.Fatalf("revision artifact file read = %q, error = %v", data, err)
	}

	reloaded, err := LoadContextManagerFromSessionID(sessionFolder, revision.GetSessionID())
	if err != nil {
		t.Fatalf("LoadContextManagerFromSessionID() error = %v", err)
	}
	result := reloaded.CloneMessageList()[1].ToolResults[0]
	if result.Meta == nil || result.Meta.ArtifactPath != stored.Path {
		t.Fatalf("reloaded artifact path = %#v, want %q", result.Meta, stored.Path)
	}
	if reloaded.ArtifactStoreRoot() == manager.ArtifactStoreRoot() {
		t.Fatalf("reloaded artifact store = %q, want revision-owned store", reloaded.ArtifactStoreRoot())
	}
	data, err := os.ReadFile(stored.Path)
	if err != nil || string(data) != "persisted-result" {
		t.Fatalf("reloaded artifact file read = %q, error = %v", data, err)
	}
}

func TestContextManagerRejectsArtifactAboveSingleLimit(t *testing.T) {
	manager, err := NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	_, err = manager.StoreArtifact("application/octet-stream", make([]byte, ArtifactSingleMaxBytes+1), ArtifactMetadata{})
	if !errors.Is(err, ErrArtifactTooLarge) {
		t.Fatalf("StoreArtifact() error = %v, want %v", err, ErrArtifactTooLarge)
	}
}

func TestContextManagerRejectsArtifactWhenSessionStoreIsFull(t *testing.T) {
	manager, err := NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	existingPath := filepath.Join(manager.ArtifactStoreRoot(), "existing.data")
	file, err := os.Create(existingPath)
	if err != nil {
		t.Fatalf("os.Create() error = %v", err)
	}
	if err := file.Truncate(ArtifactSessionMaxBytes); err != nil {
		_ = file.Close()
		t.Fatalf("Truncate() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	_, err = manager.StoreArtifact("text/plain", []byte("x"), ArtifactMetadata{})
	if !errors.Is(err, ErrArtifactSessionFull) {
		t.Fatalf("StoreArtifact() error = %v, want %v", err, ErrArtifactSessionFull)
	}
}

func TestContextManagerCountsMetadataTowardArtifactSessionLimit(t *testing.T) {
	manager, err := NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	existingPath := filepath.Join(manager.ArtifactStoreRoot(), "existing.json")
	file, err := os.Create(existingPath)
	if err != nil {
		t.Fatalf("os.Create() error = %v", err)
	}
	if err := file.Truncate(ArtifactSessionMaxBytes); err != nil {
		_ = file.Close()
		t.Fatalf("Truncate() error = %v", err)
	}
	if err := file.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	_, err = manager.StoreArtifact("text/plain", []byte("x"), ArtifactMetadata{})
	if !errors.Is(err, ErrArtifactSessionFull) {
		t.Fatalf("StoreArtifact() error = %v, want %v", err, ErrArtifactSessionFull)
	}
}

func TestContextManagerStoresArtifactsWithOwnerOnlyPermissions(t *testing.T) {
	manager, err := NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	stored, err := manager.StoreArtifact("text/plain", []byte("sensitive"), ArtifactMetadata{Sensitive: true})
	if err != nil {
		t.Fatalf("StoreArtifact() error = %v", err)
	}
	id := strings.TrimSuffix(filepath.Base(stored.Path), filepath.Ext(stored.Path))
	for path, want := range map[string]os.FileMode{
		manager.ArtifactStoreRoot():                            0o700,
		filepath.Join(manager.ArtifactStoreRoot(), id+".data"): 0o600,
		filepath.Join(manager.ArtifactStoreRoot(), id+".json"): 0o600,
	} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat(%s) error = %v", path, err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Fatalf("permissions for %s = %o, want %o", path, got, want)
		}
	}
}

func TestContextManagerUsesShortTTLForSensitiveArtifact(t *testing.T) {
	manager, err := NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	stored, err := manager.StoreArtifact("text/plain", []byte("clipboard"), ArtifactMetadata{Sensitive: true})
	if err != nil {
		t.Fatalf("StoreArtifact() error = %v", err)
	}
	id := strings.TrimSuffix(filepath.Base(stored.Path), filepath.Ext(stored.Path))
	metadataData, err := os.ReadFile(filepath.Join(manager.ArtifactStoreRoot(), id+".json"))
	if err != nil {
		t.Fatalf("ReadFile(metadata) error = %v", err)
	}
	var metadata ArtifactMetadata
	if err := json.Unmarshal(metadataData, &metadata); err != nil {
		t.Fatalf("json.Unmarshal(metadata) error = %v", err)
	}
	if got := metadata.ExpiresAt.Sub(metadata.CreatedAt); got != artifactSensitiveTTL {
		t.Fatalf("sensitive TTL = %v, want %v", got, artifactSensitiveTTL)
	}
}

func TestArtifactPathRecoverableRejectsUnsafeMetadataSidecars(t *testing.T) {
	root := t.TempDir()
	dataPath := filepath.Join(root, "tr_safe.data")
	metadataPath := filepath.Join(root, "tr_safe.json")
	if err := os.WriteFile(dataPath, []byte("artifact"), 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", dataPath, err)
	}
	validMetadata, err := json.Marshal(ArtifactMetadata{
		MIMEType:  "text/plain",
		Size:      int64(len("artifact")),
		CreatedAt: time.Now().Add(-time.Minute),
		ExpiresAt: time.Now().Add(time.Hour),
		Complete:  true,
	})
	if err != nil {
		t.Fatalf("Marshal metadata: %v", err)
	}
	if err := os.WriteFile(metadataPath, validMetadata, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", metadataPath, err)
	}
	if !ArtifactPathRecoverable(dataPath, time.Now()) {
		t.Fatal("valid artifact metadata was rejected")
	}

	if err := os.Remove(metadataPath); err != nil {
		t.Fatalf("Remove(%s): %v", metadataPath, err)
	}
	targetPath := filepath.Join(root, "target.json")
	if err := os.WriteFile(targetPath, validMetadata, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", targetPath, err)
	}
	if err := os.Symlink(targetPath, metadataPath); err == nil {
		if ArtifactPathRecoverable(dataPath, time.Now()) {
			t.Fatal("symlink metadata sidecar was accepted")
		}
		if err := os.Remove(metadataPath); err != nil {
			t.Fatalf("Remove symlink(%s): %v", metadataPath, err)
		}
	}

	if err := os.Mkdir(metadataPath, 0o700); err != nil {
		t.Fatalf("Mkdir(%s): %v", metadataPath, err)
	}
	if ArtifactPathRecoverable(dataPath, time.Now()) {
		t.Fatal("non-regular metadata sidecar was accepted")
	}
	if err := os.Remove(metadataPath); err != nil {
		t.Fatalf("Remove directory(%s): %v", metadataPath, err)
	}

	oversized := bytes.Repeat([]byte("x"), artifactMetadataMaxBytes+1)
	if err := os.WriteFile(metadataPath, oversized, 0o600); err != nil {
		t.Fatalf("WriteFile(%s): %v", metadataPath, err)
	}
	if ArtifactPathRecoverable(dataPath, time.Now()) {
		t.Fatal("oversized metadata sidecar was accepted")
	}
}

func TestClearAllSessionsRemovesSessionArtifactDirectory(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := NewContextManagerFromMessageList(sessionFolder, nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	if _, err := manager.StoreArtifact("text/plain", []byte("artifact"), ArtifactMetadata{}); err != nil {
		t.Fatalf("StoreArtifact() error = %v", err)
	}
	if err := ClearAllSessions(sessionFolder); err != nil {
		t.Fatalf("ClearAllSessions() error = %v", err)
	}
	if _, err := os.Stat(sessionDataDir(sessionFolder, manager.GetSessionID())); !os.IsNotExist(err) {
		t.Fatalf("session data directory still exists, stat error = %v", err)
	}
}
