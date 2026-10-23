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
	// AddEpisode stamps an empty EndedAt with the current time, which would
	// make an old running episode look recent; StartEpisode keeps it old.
	if status == "running" {
		if _, err := store.StartEpisode(context.Background(), episode); err != nil {
			t.Fatalf("StartEpisode(%s): %v", id, err)
		}
		return
	}
	episode.EndedAt = ended.Format(time.RFC3339Nano)
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

func TestEpisodeRetentionNeverDeletesAYearDirectory(t *testing.T) {
	memoryDir, store, now := episodeCleanerFixture(t)
	addCleanerEpisode(t, store, "ep_recent", "active", now.Add(-time.Hour))
	index, err := store.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	year := filepath.Dir(filepath.Dir(filepath.Join(store.rootDir, index.Episodes[0].File)))
	// An old entry whose file sits directly in the year directory, as
	// RebuildIndex can produce, resolves to the year directory itself.
	index.Episodes = append(index.Episodes, episodeIndexEntry{
		ID: "ep_stray", File: filepath.Base(year) + "/episode.yaml", Status: "active",
		EndedAt: now.Add(-60 * 24 * time.Hour).Format(time.RFC3339Nano),
	})
	if err := writeYAMLAtomic(store.indexPath(), index); err != nil {
		t.Fatal(err)
	}
	cleaner := NewEpisodeRetentionCleaner(memoryDir, 1)
	cleaner.now = func() time.Time { return now }
	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatal(err)
	}
	if matches, _ := filepath.Glob(filepath.Join(year, "ep_recent")); len(matches) != 1 {
		t.Fatal("cleaner deleted the year directory with a recent episode in it")
	}
}

func TestEpisodeRetentionDropsIndexEntriesBeforeDeleting(t *testing.T) {
	memoryDir, store, now := episodeCleanerFixture(t)
	addCleanerEpisode(t, store, "ep_old", "active", now.Add(-40*24*time.Hour))
	cleaner := NewEpisodeRetentionCleaner(memoryDir, 1)
	cleaner.now = func() time.Time { return now }
	cleaner.removeAll = func(string) error { return os.ErrPermission }
	// The deletion fails; the entry must already be gone from the index, so
	// the directory is left as an unindexed orphan for a later run.
	if _, err := cleaner.Clean(context.Background()); err == nil {
		t.Fatal("expected the failed removal to be reported")
	}
	if indexedIDs(t, store)["ep_old"] {
		t.Fatal("index still lists an episode the cleaner was deleting")
	}
}

func TestEpisodeRetentionFoldsLargeIndexLog(t *testing.T) {
	memoryDir, store, now := episodeCleanerFixture(t)
	cleaner := NewEpisodeRetentionCleaner(memoryDir, 1)
	cleaner.now = func() time.Time { return now }

	addCleanerEpisode(t, store, "ep_small", "active", now.Add(-time.Hour))
	if estimate, err := cleaner.EstimateReclaimable(context.Background()); err != nil || estimate != 0 {
		t.Fatalf("estimate for a small update log = %d, %v; want 0", estimate, err)
	}
	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !store.indexLogExists() {
		t.Fatal("a small update log was folded; that rewrites index.yaml on every maintenance check")
	}

	for i := 0; store.indexLogExists(); i++ {
		if info, _ := os.Stat(store.indexLogPath()); info.Size() >= episodeIndexLogCompactBytes {
			break
		}
		addCleanerEpisode(t, store, fmt.Sprintf("ep_fill_%04d", i), "active", now.Add(-time.Duration(i)*time.Second))
	}
	want := len(indexedIDs(t, store))
	// The storage monitor only calls Clean when the estimate is non-zero.
	if estimate, err := cleaner.EstimateReclaimable(context.Background()); err != nil || estimate == 0 {
		t.Fatalf("estimate for a large update log = %d, %v; want > 0", estimate, err)
	}
	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatal(err)
	}
	if store.indexLogExists() {
		t.Fatal("a large update log was not folded into index.yaml")
	}
	if got := len(indexedIDs(t, NewTaskEpisodeStore(store.rootDir))); got != want {
		t.Fatalf("index after folding has %d entries, want %d", got, want)
	}
}

func TestEpisodeRetentionDropsOldEntriesWithoutDirectories(t *testing.T) {
	memoryDir, store, now := episodeCleanerFixture(t)
	addCleanerEpisode(t, store, "ep_recent", "active", now.Add(-time.Hour))
	addCleanerEpisode(t, store, "ep_gone", "active", now.Add(-40*24*time.Hour))
	gone, _ := store.entryDir(episodeIndexEntry{File: "2026/ep_gone/episode.yaml"})
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	cleaner := NewEpisodeRetentionCleaner(memoryDir, 1)
	cleaner.now = func() time.Time { return now }
	// Nothing is left on disk to free, but the storage monitor skips a
	// cleaner that estimates 0, so the stale entry would never be dropped.
	if estimate, err := cleaner.EstimateReclaimable(context.Background()); err != nil || estimate == 0 {
		t.Fatalf("estimate = %d, %v; want > 0", estimate, err)
	}
	if _, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ids := indexedIDs(t, NewTaskEpisodeStore(store.rootDir)); ids["ep_gone"] || !ids["ep_recent"] {
		t.Fatalf("index after cleaning: %v", ids)
	}
}
