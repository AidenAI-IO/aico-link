package agent

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func addTestEpisode(t *testing.T, store *TaskEpisodeStore, id, goal string, at time.Time) {
	t.Helper()
	if _, err := store.AddEpisode(context.Background(), TaskEpisode{
		ID: id, Status: "active", UserGoal: goal,
		StartedAt: at.Add(-time.Second).Format(time.RFC3339Nano),
		EndedAt:   at.Format(time.RFC3339Nano),
		Outcome:   TaskEpisodeOutcome{Success: true},
	}); err != nil {
		t.Fatalf("AddEpisode(%s) error = %v", id, err)
	}
}

func TestEpisodeIndexKeepsShortTextAndFullEpisode(t *testing.T) {
	store := NewTaskEpisodeStore(t.TempDir())
	goal := strings.Repeat("会议记录内容", 2000)
	addTestEpisode(t, store, "ep_long", goal, time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC))

	index, err := store.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	entry := index.Episodes[0]
	if n := len([]rune(entry.UserGoal)); n > episodeIndexTextLimit+3 {
		t.Fatalf("index user_goal kept %d runes", n)
	}
	if n := len([]rune(entry.Summary)); n > episodeIndexTextLimit+3 {
		t.Fatalf("index summary kept %d runes", n)
	}
	info, err := os.Stat(store.indexLogPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 4096 {
		t.Fatalf("index log is %d bytes for one episode", info.Size())
	}

	episode, err := store.Get(context.Background(), "ep_long")
	if err != nil || episode.UserGoal != goal {
		t.Fatalf("Get lost the full goal: len=%d err=%v", len(episode.UserGoal), err)
	}
}

func TestEpisodeIndexWriteShrinksExistingLongEntries(t *testing.T) {
	store := NewTaskEpisodeStore(t.TempDir())
	long := strings.Repeat("x", 50_000)
	// An index written before the limit existed.
	if err := writeYAMLAtomic(store.indexPath(), episodeIndex{Version: 1, Episodes: []episodeIndexEntry{
		{ID: "ep_old", File: "2026/ep_old/episode.yaml", Status: "active", UserGoal: long, Summary: long, StartedAt: "2026-10-01T00:00:00Z"},
	}}); err != nil {
		t.Fatal(err)
	}
	addTestEpisode(t, store, "ep_new", "open Settings", time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC))
	// Startup folds the update log into the snapshot.
	if _, err := store.MarkRunningEpisodesInterrupted(context.Background(), ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(store.indexPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 4096 {
		t.Fatalf("old long entries survived the next write: %d bytes", info.Size())
	}
}

func TestEpisodeIndexTaskAppendsInsteadOfRewriting(t *testing.T) {
	store := NewTaskEpisodeStore(t.TempDir())
	entries := make([]episodeIndexEntry, 0, 200)
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("ep_seed_%03d", i)
		entries = append(entries, episodeIndexEntry{ID: id, File: "2026/" + id + "/episode.yaml", Status: "active", StartedAt: fmt.Sprintf("2026-10-01T00:%02d:%02dZ", i/60, i%60)})
	}
	if err := store.writeIndex(episodeIndex{Version: 1, Episodes: entries}); err != nil {
		t.Fatal(err)
	}
	before, _, err := memoryFileSignatureForPath(store.indexPath())
	if err != nil {
		t.Fatal(err)
	}

	at := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	running := TaskEpisode{ID: "ep_task", UserGoal: "open Settings", StartedAt: at.Format(time.RFC3339Nano)}
	if _, err := store.StartEpisode(context.Background(), running); err != nil {
		t.Fatal(err)
	}
	addTestEpisode(t, store, "ep_task", "open Settings", at.Add(time.Second))

	after, _, err := memoryFileSignatureForPath(store.indexPath())
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Fatal("a task rewrote index.yaml; it should only append to the update log")
	}
	for _, reader := range []*TaskEpisodeStore{store, NewTaskEpisodeStore(store.rootDir)} {
		index, err := reader.loadIndex()
		if err != nil {
			t.Fatal(err)
		}
		if len(index.Episodes) != 201 || index.Episodes[0].ID != "ep_task" || index.Episodes[0].Status != "active" {
			t.Fatalf("index after start and finish: %d entries, first %+v", len(index.Episodes), index.Episodes[0])
		}
	}
}

func TestEpisodeIndexStartupFoldsLogAndMarksRunning(t *testing.T) {
	root := t.TempDir()
	at := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	// Only the update log exists: the agent stopped before any compaction.
	if _, err := NewTaskEpisodeStore(root).StartEpisode(context.Background(), TaskEpisode{ID: "ep_run", UserGoal: "open Settings", StartedAt: at.Format(time.RFC3339Nano)}); err != nil {
		t.Fatal(err)
	}
	store := NewTaskEpisodeStore(root)
	interrupted, err := store.MarkRunningEpisodesInterrupted(context.Background(), "")
	if err != nil {
		t.Fatal(err)
	}
	if interrupted != 1 {
		t.Fatalf("interrupted = %d, want 1", interrupted)
	}
	if store.indexLogExists() {
		t.Fatal("startup left the update log in place")
	}
	index, err := NewTaskEpisodeStore(root).loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Episodes) != 1 || index.Episodes[0].Status == "running" {
		t.Fatalf("snapshot after startup: %+v", index.Episodes)
	}
}

func TestEpisodeIndexSkipsLogAlreadyInSnapshot(t *testing.T) {
	store := NewTaskEpisodeStore(t.TempDir())
	at := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	addTestEpisode(t, store, "ep_keep", "open Settings", at)
	addTestEpisode(t, store, "ep_drop", "open Clock", at.Add(time.Minute))
	logData, err := os.ReadFile(store.indexLogPath())
	if err != nil {
		t.Fatal(err)
	}
	index, err := store.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	index.Episodes = []episodeIndexEntry{index.Episodes[1]}
	if err := store.writeIndex(index); err != nil {
		t.Fatal(err)
	}
	// The agent stopped after the snapshot was written but before the log was
	// removed: the old log must not bring the dropped entry back.
	if err := os.WriteFile(store.indexLogPath(), logData, 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := NewTaskEpisodeStore(store.rootDir).loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Episodes) != 1 || got.Episodes[0].ID != "ep_keep" {
		t.Fatalf("absorbed log replayed: %+v", got.Episodes)
	}
}

func TestEpisodeIndexLogSurvivesPartialLine(t *testing.T) {
	store := NewTaskEpisodeStore(t.TempDir())
	at := time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC)
	addTestEpisode(t, store, "ep_1", "open Settings", at)
	file, err := os.OpenFile(store.indexLogPath(), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`{"id":"ep_cut","file":"2026/ep_`); err != nil {
		t.Fatal(err)
	}
	file.Close()
	addTestEpisode(t, store, "ep_2", "open Clock", at.Add(time.Minute))

	index, err := NewTaskEpisodeStore(store.rootDir).loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Episodes) != 2 || index.Episodes[0].ID != "ep_2" || index.Episodes[1].ID != "ep_1" {
		t.Fatalf("entries around a partial line: %+v", index.Episodes)
	}
}

func TestEpisodeIndexCacheSurvivesOwnWritesAndSeesOthers(t *testing.T) {
	store := NewTaskEpisodeStore(t.TempDir())
	addTestEpisode(t, store, "ep_1", "open Settings", time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC))
	if _, err := store.loadIndex(); err != nil {
		t.Fatal(err)
	}
	addTestEpisode(t, store, "ep_2", "open Clock", time.Date(2026, 10, 22, 8, 1, 0, 0, time.UTC))

	signature, logSignature, err := store.indexSignatures()
	if err != nil {
		t.Fatal(err)
	}
	if index, hit := store.cachedIndex(signature, logSignature); !hit || len(index.Episodes) != 2 {
		t.Fatal("the appended entry is not in the cache; the next read parses the index again")
	}

	// A snapshot written elsewhere changes the signature, so the next read
	// must see it.
	time.Sleep(10 * time.Millisecond)
	if err := writeYAMLAtomic(store.indexPath(), episodeIndex{Version: 1, Episodes: []episodeIndexEntry{
		{ID: "ep_external", Status: "active", StartedAt: "2026-10-22T09:00:00Z"},
	}}); err != nil {
		t.Fatal(err)
	}
	index, err := store.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	if len(index.Episodes) != 3 || index.Episodes[0].ID != "ep_external" {
		t.Fatalf("stale cached index returned: %+v", index.Episodes)
	}
}
