package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"aico-agent/internal/ble"
)

func TestNotificationContextCleanerRemovesOnlyFullyProcessedOldShards(t *testing.T) {
	root := t.TempDir()
	reader := func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{
			{ID: "1", Source: "android", SourceID: "old", NotificationUID: 1, SourceEventID: "old-1", AppIdentifier: "com.mail", Message: "old", Event: "added", ReceivedAt: "2026-08-19T00:00:00Z"},
			{ID: "2", Source: "android", SourceID: "new", NotificationUID: 2, SourceEventID: "new-1", AppIdentifier: "com.mail", Message: "new", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z"},
		}}, nil
	}
	ctxStore, err := NewNotificationContext(root, reader)
	if err != nil {
		t.Fatal(err)
	}
	accepted, err := ctxStore.Consume(context.Background(), 10)
	if err != nil || len(accepted) != 2 {
		t.Fatalf("Consume() accepted=%#v err=%v", accepted, err)
	}
	if err := ctxStore.CommitProcessed(context.Background(), accepted[:1]); err != nil {
		t.Fatal(err)
	}
	cleaner := NewNotificationContextCleaner(root, 1, 1)
	cleaner.now = func() time.Time { return time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC) }
	if freed, err := cleaner.Clean(context.Background()); err != nil {
		t.Fatal(err)
	} else if freed == 0 {
		t.Fatal("Clean() freed no old processed shard")
	}
	if _, err := os.Stat(filepath.Join(root, "notifications", "events", "2026-08-19.jsonl")); !os.IsNotExist(err) {
		t.Fatalf("old processed shard still exists, stat err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "notifications", "events", "2026-08-21.jsonl")); err != nil {
		t.Fatalf("new unprocessed shard was removed: %v", err)
	}
}

func TestNotificationContextCleanerForceDoesNotDeleteUnprocessedRecords(t *testing.T) {
	root := t.TempDir()
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{ID: "1", Source: "android", SourceID: "only", NotificationUID: 1, SourceEventID: "only-1", AppIdentifier: "com.mail", Message: "keep", Event: "added", ReceivedAt: "2026-08-01T00:00:00Z"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctxStore.Consume(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	cleaner := NewNotificationContextCleaner(root, 0, 1)
	if _, err := cleaner.ForceClean(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "notifications", "events", "2026-08-01.jsonl")); err != nil {
		t.Fatalf("unprocessed shard was deleted by ForceClean: %v", err)
	}
}
