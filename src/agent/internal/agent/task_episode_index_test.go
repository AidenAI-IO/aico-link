package agent

import (
	"context"
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
	info, err := os.Stat(store.indexPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 4096 {
		t.Fatalf("index is %d bytes for one episode", info.Size())
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
	info, err := os.Stat(store.indexPath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() > 4096 {
		t.Fatalf("old long entries survived the next write: %d bytes", info.Size())
	}
}

func TestEpisodeIndexCacheSurvivesOwnWritesAndSeesOthers(t *testing.T) {
	store := NewTaskEpisodeStore(t.TempDir())
	addTestEpisode(t, store, "ep_1", "open Settings", time.Date(2026, 10, 22, 8, 0, 0, 0, time.UTC))

	signature, ok, err := memoryFileSignatureForPath(store.indexPath())
	if err != nil || !ok {
		t.Fatal(err)
	}
	if _, hit := store.cachedIndex(signature); !hit {
		t.Fatal("the index just written is not cached; the next read parses the file again")
	}

	// A write from elsewhere (lifecycle compaction writes the file directly)
	// changes the signature, so the next read must see it.
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
	if len(index.Episodes) != 1 || index.Episodes[0].ID != "ep_external" {
		t.Fatalf("stale cached index returned: %+v", index.Episodes)
	}
}
