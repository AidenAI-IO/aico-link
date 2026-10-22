package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func episodeCleanerFixture(t *testing.T) (string, *TaskEpisodeStore, time.Time) {
	t.Helper()
	memoryDir := t.TempDir()
	return memoryDir, NewTaskEpisodeStore(filepath.Join(memoryDir, "episodes")), time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)
}

func addCleanerEpisode(t *testing.T, store *TaskEpisodeStore, id, status string, ended time.Time) {
	t.Helper()
	episode := TaskEpisode{ID: id, Status: status, UserGoal: "open " + id, StartedAt: ended.Add(-time.Minute).Format(time.RFC3339Nano)}
	if status != "running" {
		episode.EndedAt = ended.Format(time.RFC3339Nano)
	}
	if _, err := store.AddEpisode(context.Background(), episode); err != nil {
		t.Fatalf("AddEpisode(%s): %v", id, err)
	}
}

func indexedIDs(t *testing.T, store *TaskEpisodeStore) map[string]bool {
	t.Helper()
	index, err := store.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, entry := range index.Episodes {
		ids[entry.ID] = true
	}
	return ids
}

func TestEpisodeRetentionDeletesOldUnreferencedEpisodes(t *testing.T) {
	memoryDir, store, now := episodeCleanerFixture(t)
	addCleanerEpisode(t, store, "ep_recent", "active", now.Add(-24*time.Hour))
	addCleanerEpisode(t, store, "ep_old", "active", now.Add(-40*24*time.Hour))
	addCleanerEpisode(t, store, "ep_old_cited", "active", now.Add(-40*24*time.Hour))
	addCleanerEpisode(t, store, "ep_old_running", "running", now.Add(-40*24*time.Hour))

	longTerm := NewLongTermMemoryStore(filepath.Join(memoryDir, "long_term"))
	if _, err := longTerm.AddMemory(context.Background(), MemoryItem{
		Type: "fact", Priority: 50, Title: "evidence", Content: "cites an old episode",
		EvidenceExcerpts: []string{"e"},
		EvidenceRefs:     []MemorySourceRef{{Type: "episode", ID: "ep_old_cited"}},
	}); err != nil {
		t.Fatal(err)
	}

	cleaner := NewEpisodeRetentionCleaner(memoryDir, 1)
	cleaner.now = func() time.Time { return now }
	estimate, err := cleaner.EstimateReclaimable(context.Background())
	if err != nil || estimate == 0 {
		t.Fatalf("estimate = %d, err = %v", estimate, err)
	}
	freed, err := cleaner.Clean(context.Background())
	if err != nil || freed == 0 {
		t.Fatalf("freed = %d, err = %v", freed, err)
	}

	ids := indexedIDs(t, store)
	for id, want := range map[string]bool{"ep_recent": true, "ep_old": false, "ep_old_cited": true, "ep_old_running": true} {
		if ids[id] != want {
			t.Errorf("%s indexed = %v, want %v", id, ids[id], want)
		}
		matches, _ := filepath.Glob(filepath.Join(store.rootDir, "*", id))
		if (len(matches) == 1) != want {
			t.Errorf("%s directory present = %v, want %v", id, len(matches) == 1, want)
		}
	}
	if again, err := cleaner.EstimateReclaimable(context.Background()); err != nil || again != 0 {
		t.Fatalf("second estimate = %d, err = %v", again, err)
	}
}

func TestEpisodeRetentionCapsEntryCount(t *testing.T) {
	memoryDir, store, now := episodeCleanerFixture(t)
	for i := 0; i < 5; i++ {
		addCleanerEpisode(t, store, fmt.Sprintf("ep_%d", i), "active", now.Add(-time.Duration(i)*time.Hour))
	}
	cleaner := NewEpisodeRetentionCleaner(memoryDir, 1)
	cleaner.now = func() time.Time { return now }
	cleaner.maxEntries = 3
	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatal(err)
	}
	ids := indexedIDs(t, store)
	if len(ids) != 3 || !ids["ep_0"] || !ids["ep_1"] || !ids["ep_2"] {
		t.Fatalf("kept %v, want the three newest", ids)
	}
}

func TestEpisodeRetentionRemovesOldOrphanDirectories(t *testing.T) {
	memoryDir, store, now := episodeCleanerFixture(t)
	addCleanerEpisode(t, store, "ep_kept", "active", now.Add(-time.Hour))
	oldOrphan := filepath.Join(store.rootDir, "2026", "ep_orphan_old")
	newOrphan := filepath.Join(store.rootDir, "2026", "ep_orphan_new")
	for _, dir := range []string{oldOrphan, newOrphan} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "episode.yaml"), []byte("id: x\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	old := now.Add(-40 * 24 * time.Hour)
	if err := os.Chtimes(oldOrphan, old, old); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(newOrphan, now, now); err != nil {
		t.Fatal(err)
	}
	cleaner := NewEpisodeRetentionCleaner(memoryDir, 1)
	cleaner.now = func() time.Time { return now }
	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(oldOrphan); !os.IsNotExist(err) {
		t.Fatal("old orphan directory survived")
	}
	if _, err := os.Stat(newOrphan); err != nil {
		t.Fatal("recent orphan directory was removed")
	}
	if !indexedIDs(t, store)["ep_kept"] {
		t.Fatal("indexed episode was dropped")
	}
}

func TestEpisodeRetentionIgnoresPathsOutsideTheEpisodesDirectory(t *testing.T) {
	memoryDir, store, now := episodeCleanerFixture(t)
	outside := filepath.Join(memoryDir, "long_term")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeYAMLAtomic(store.indexPath(), episodeIndex{Version: 1, Episodes: []episodeIndexEntry{
		{ID: "ep_escape", File: "../long_term/episode.yaml", Status: "active", EndedAt: now.Add(-60 * 24 * time.Hour).Format(time.RFC3339Nano)},
	}}); err != nil {
		t.Fatal(err)
	}
	cleaner := NewEpisodeRetentionCleaner(memoryDir, 1)
	cleaner.now = func() time.Time { return now }
	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("cleaner removed a directory outside memory/episodes")
	}
}
