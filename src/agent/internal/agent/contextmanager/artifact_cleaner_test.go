package contextmanager

import (
	"aico-agent/internal/agent/messages"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestArtifactStoreCleanerRemovesExpiredAndOrphanedArtifacts(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := NewContextManagerFromMessageList(sessionFolder, nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	live, err := manager.StoreArtifact("text/plain", []byte("live-result"), ArtifactMetadata{})
	if err != nil {
		t.Fatalf("StoreArtifact(live) error = %v", err)
	}
	expired, err := manager.StoreArtifact("text/plain", []byte("expired-result"), ArtifactMetadata{
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("StoreArtifact(expired) error = %v", err)
	}

	orphanStore, err := newArtifactStore(sessionFolder, "s_orphan")
	if err != nil {
		t.Fatalf("newArtifactStore(orphan) error = %v", err)
	}
	orphan, err := orphanStore.store("text/plain", []byte("orphan-result"), ArtifactMetadata{})
	if err != nil {
		t.Fatalf("orphan store() error = %v", err)
	}
	old := time.Now().Add(-artifactOrphanGracePeriod - time.Minute)
	if err := os.Chtimes(orphanStore.root, old, old); err != nil {
		t.Fatalf("Chtimes(orphan root) error = %v", err)
	}

	cleaner := NewArtifactStoreCleaner(sessionFolder, 1)
	estimate, err := cleaner.EstimateReclaimable(context.Background())
	if err != nil {
		t.Fatalf("EstimateReclaimable() error = %v", err)
	}
	if estimate < uint64(len("expired-result")+len("orphan-result")) {
		t.Fatalf("EstimateReclaimable() = %d, want at least expired and orphan data bytes", estimate)
	}
	freed, err := cleaner.Clean(context.Background())
	if err != nil {
		t.Fatalf("Clean() error = %v", err)
	}
	if freed < uint64(len("expired-result")+len("orphan-result")) {
		t.Fatalf("Clean() freed = %d, want at least expired and orphan data bytes", freed)
	}
	if data, err := os.ReadFile(live.Path); err != nil || string(data) != "live-result" {
		t.Fatalf("live artifact read = %q, error = %v", data, err)
	}
	if _, err := os.Stat(expired.Path); !os.IsNotExist(err) {
		t.Fatalf("expired artifact remains after cleanup, stat error = %v", err)
	}
	if _, err := os.Stat(orphan.Path); !os.IsNotExist(err) {
		t.Fatalf("orphan artifact remains after cleanup, stat error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionFolder, "s_orphan", "tool-results")); !os.IsNotExist(err) {
		t.Fatalf("orphan tool-results directory still exists, stat error = %v", err)
	}
}

func TestArtifactStoreCleanerExpiresArtifactsButKeepsOrphansWhenTranscriptIsInvalid(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := NewContextManagerFromMessageList(sessionFolder, nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	expired, err := manager.StoreArtifact("text/plain", []byte("expired-result"), ArtifactMetadata{
		ExpiresAt: time.Now().Add(-time.Minute),
	})
	if err != nil {
		t.Fatalf("StoreArtifact(expired) error = %v", err)
	}
	orphanStore, err := newArtifactStore(sessionFolder, "s_orphan_invalid_metadata")
	if err != nil {
		t.Fatalf("newArtifactStore(orphan) error = %v", err)
	}
	orphan, err := orphanStore.store("text/plain", []byte("orphan-result"), ArtifactMetadata{})
	if err != nil {
		t.Fatalf("orphan store() error = %v", err)
	}
	old := time.Now().Add(-artifactOrphanGracePeriod - time.Minute)
	if err := os.Chtimes(orphanStore.root, old, old); err != nil {
		t.Fatalf("Chtimes(orphan root) error = %v", err)
	}
	// An unreadable transcript makes the reference set incomplete, so the cleaner
	// must still expire artifacts but must not delete directories it cannot prove
	// are unreferenced.
	if err := os.WriteFile(filepath.Join(sessionFolder, "s_broken.jsonl"), []byte("{not json\n"), 0o600); err != nil {
		t.Fatalf("WriteFile(invalid transcript) error = %v", err)
	}

	cleaner := NewArtifactStoreCleaner(sessionFolder, 1)
	freed, err := cleaner.Clean(context.Background())
	if err == nil {
		t.Fatal("Clean() error = nil, want invalid transcript reported")
	}
	if freed < uint64(len("expired-result")) {
		t.Fatalf("Clean() freed = %d, want expired artifact bytes", freed)
	}
	if _, err := os.Stat(expired.Path); !os.IsNotExist(err) {
		t.Fatalf("expired artifact remains after cleanup, stat error = %v", err)
	}
	if data, err := os.ReadFile(orphan.Path); err != nil || string(data) != "orphan-result" {
		t.Fatalf("orphan artifact was removed with incomplete references: data=%q error=%v", data, err)
	}
}

func TestArtifactStoreCleanerKeepsFreshOrphanScopeDuringGracePeriod(t *testing.T) {
	sessionFolder := t.TempDir()
	store, err := newArtifactStore(sessionFolder, "s_fresh_orphan")
	if err != nil {
		t.Fatalf("newArtifactStore() error = %v", err)
	}
	stored, err := store.store("text/plain", []byte("fresh"), ArtifactMetadata{})
	if err != nil {
		t.Fatalf("store() error = %v", err)
	}

	cleaner := NewArtifactStoreCleaner(sessionFolder, 1)
	cleaner.now = func() time.Time { return time.Now().Add(artifactOrphanGracePeriod / 2) }
	if freed, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatalf("Clean() error = %v", err)
	} else if freed != 0 {
		t.Fatalf("Clean() freed = %d, want 0 during grace period", freed)
	}
	if _, err := os.ReadFile(stored.Path); err != nil {
		t.Fatalf("fresh orphan artifact was removed during grace period: %v", err)
	}
}

func TestArtifactStoreCleanerTreatsLegacyTranscriptAsReferencedScope(t *testing.T) {
	sessionFolder := t.TempDir()
	sessionID := "s_legacy"
	if err := os.WriteFile(filepath.Join(sessionFolder, sessionID+".jsonl"), []byte(""), 0o644); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	store, err := newArtifactStore(sessionFolder, sessionID)
	if err != nil {
		t.Fatalf("newArtifactStore() error = %v", err)
	}
	stored, err := store.store("text/plain", []byte("legacy-live"), ArtifactMetadata{})
	if err != nil {
		t.Fatalf("store() error = %v", err)
	}

	cleaner := NewArtifactStoreCleaner(sessionFolder, 1)
	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatalf("Clean() error = %v", err)
	}
	if _, err := os.ReadFile(stored.Path); err != nil {
		t.Fatalf("legacy referenced artifact was removed: %v", err)
	}
}

func TestArtifactStoreCleanerRemovesStaleUncommittedFiles(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := NewContextManagerFromMessageList(sessionFolder, nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	orphanData := filepath.Join(manager.ArtifactStoreRoot(), "tr_uncommitted.data")
	tempFile := filepath.Join(manager.ArtifactStoreRoot(), ".artifact-interrupted")
	for _, path := range []string{orphanData, tempFile} {
		if err := os.WriteFile(path, []byte("stale"), 0o644); err != nil {
			t.Fatalf("WriteFile(%s) error = %v", filepath.Base(path), err)
		}
		old := time.Now().Add(-artifactOrphanGracePeriod - time.Minute)
		if err := os.Chtimes(path, old, old); err != nil {
			t.Fatalf("Chtimes(%s) error = %v", filepath.Base(path), err)
		}
	}

	cleaner := NewArtifactStoreCleaner(sessionFolder, 1)
	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatalf("Clean() error = %v", err)
	}
	for _, path := range []string{orphanData, tempFile} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("stale file %s still exists, stat error = %v", filepath.Base(path), err)
		}
	}
}

func TestArtifactStoreCleanerKeepsReferencedSessionArtifactsUntilLastRevisionIsRemoved(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := NewContextManagerFromMessageList(sessionFolder, []messages.Message{{Role: messages.MessageRoleSystem, Content: "system"}})
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	stored, err := manager.StoreArtifact("text/plain", []byte("lineage-result"), ArtifactMetadata{})
	if err != nil {
		t.Fatalf("StoreArtifact() error = %v", err)
	}
	if err := manager.AppendMessage(messages.Message{
		Role: messages.MessageRoleToolResult,
		ToolResults: []messages.ToolResult{{
			ToolCallID: "call_lineage",
			Name:       "shell",
			Content:    "bounded lineage result",
			Meta:       &messages.ToolResultMeta{ArtifactPath: stored.Path, ArtifactComplete: true},
		}},
	}); err != nil {
		t.Fatalf("AppendMessage() error = %v", err)
	}
	revision, err := NewContextManagerRevisionFromMessageList(manager, manager.CloneMessageList())
	if err != nil {
		t.Fatalf("NewContextManagerRevisionFromMessageList() error = %v", err)
	}
	removeTranscriptRevisionFiles := func(sessionID string) {
		t.Helper()
		for _, path := range []string{
			filepath.Join(sessionFolder, sessionID+".jsonl"),
			sessionMetadataPath(sessionFolder, sessionID),
		} {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				t.Fatalf("Remove(%s) error = %v", filepath.Base(path), err)
			}
		}
	}

	removeTranscriptRevisionFiles(manager.GetSessionID())
	cleaner := NewArtifactStoreCleaner(sessionFolder, 1)
	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatalf("Clean() with child revision error = %v", err)
	}
	if _, err := os.ReadFile(stored.Path); err != nil {
		t.Fatalf("artifact removed while child revision still referenced its path: %v", err)
	}

	removeTranscriptRevisionFiles(revision.GetSessionID())
	old := time.Now().Add(-artifactOrphanGracePeriod - time.Minute)
	if err := os.Chtimes(filepath.Join(sessionFolder, manager.GetSessionID(), "tool-results"), old, old); err != nil {
		t.Fatalf("Chtimes(artifact root) error = %v", err)
	}
	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatalf("Clean() after last revision removal error = %v", err)
	}
	if _, err := os.Stat(filepath.Join(sessionFolder, manager.GetSessionID(), "tool-results")); !os.IsNotExist(err) {
		t.Fatalf("artifact session directory remains after last revision reference removal, stat error = %v", err)
	}
}
