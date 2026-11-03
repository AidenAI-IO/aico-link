package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"aico-agent/internal/ble"
)

func TestNotificationContextConsumesPersistsAndCommits(t *testing.T) {
	root := t.TempDir()
	page := ble.EventPage{
		Generation: "gen-a",
		Events: []ble.NotificationEvent{{
			ID: "1", Source: "ios_ancs", SourceEventID: "evt-1", DeviceID: "phone",
			Event: "added", AppIdentifier: "com.example", Title: "  Meeting  ", Message: "hello",
			ReceivedAt: "2026-08-21T01:02:03Z",
		}},
		OldestID: "1", LastID: "1",
	}
	reader := func(context.Context, string, string, int) (ble.EventPage, error) {
		return page, nil
	}
	c, err := NewNotificationContext(root, reader)
	if err != nil {
		t.Fatal(err)
	}
	events, err := c.Consume(context.Background(), 10)
	if err != nil || len(events) != 1 {
		t.Fatalf("Consume() events=%#v err=%v", events, err)
	}
	if events[0].Title != "  Meeting  " || c.State().SourceCursor != "1" {
		t.Fatalf("unexpected raw state: event=%#v state=%#v", events[0], c.State())
	}
	pending, err := c.ReadPending(context.Background(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("ReadPending() events=%#v err=%v", pending, err)
	}
	if err := c.CommitProcessed(context.Background(), pending); err != nil {
		t.Fatal(err)
	}
	if got := c.State().MemoryCursor; got != "1" {
		t.Fatalf("MemoryCursor=%q, want 1", got)
	}
	data, err := os.ReadFile(filepath.Join(root, "notifications", "events", "2026-08-21.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	var stored NotificationRecord
	if err := json.Unmarshal(data[:len(data)-1], &stored); err != nil {
		t.Fatal(err)
	}
	if stored.Title != "  Meeting  " {
		t.Fatalf("stored event was not preserved as raw data: %#v", stored)
	}
}

func TestNotificationContextCapsStoredNotificationText(t *testing.T) {
	root := t.TempDir()
	message := strings.Repeat("界", maxNotificationStoredText+1)
	c, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{
			ID: "1", Source: "android", SourceEventID: "evt-1", Message: message,
			ReceivedAt: "2026-08-21T01:02:03Z",
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	events, err := c.Consume(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || utf8.RuneCountInString(events[0].Message) != maxNotificationStoredText {
		t.Fatalf("stored message length=%d, want %d", utf8.RuneCountInString(events[0].Message), maxNotificationStoredText)
	}
}

func TestNotificationContextDeduplicatesAcrossRestart(t *testing.T) {
	root := t.TempDir()
	reader := func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{
			ID: "1", Source: "android", SourceEventID: "same", DeviceID: "phone", Event: "added",
			ReceivedAt: "2026-08-21T00:00:00Z",
		}}}, nil
	}
	first, err := NewNotificationContext(root, reader)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := first.Consume(context.Background(), 10); err != nil || len(got) != 1 {
		t.Fatalf("first Consume() got=%#v err=%v", got, err)
	}
	second, err := NewNotificationContext(root, reader)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := second.Consume(context.Background(), 10); err != nil || len(got) != 0 {
		t.Fatalf("duplicate Consume() got=%#v err=%v", got, err)
	}
	pending, err := second.ReadPending(context.Background(), 10)
	if err != nil || len(pending) != 1 {
		t.Fatalf("dedupe removed persisted event: pending=%#v err=%v", pending, err)
	}
}

func TestNotificationContextInstancesShareRootLock(t *testing.T) {
	root := t.TempDir()
	first, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	second, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.mu != second.mu {
		t.Fatal("NotificationContext instances for one root do not share the cleaner/write lock")
	}
}

func TestNotificationContextDoesNotConsumeWhenStorageDisablesWrites(t *testing.T) {
	called := false
	c, err := NewNotificationContext(t.TempDir(), func(context.Context, string, string, int) (ble.EventPage, error) {
		called = true
		return ble.EventPage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	c.SetStorageWriteGate(&recordingStorageWriteGate{allow: false})
	events, err := c.Consume(context.Background(), 10)
	if err != nil || len(events) != 0 {
		t.Fatalf("Consume() events=%#v err=%v", events, err)
	}
	if called {
		t.Fatal("Consume() contacted BLE while Notification Context writes were disabled")
	}
}

func TestNotificationContextUsesMonotonicContextCursorAcrossBLEGenerationReset(t *testing.T) {
	root := t.TempDir()
	phase := 0
	reader := func(_ context.Context, since, generation string, _ int) (ble.EventPage, error) {
		phase++
		switch phase {
		case 1:
			return ble.EventPage{Generation: "g1", Events: []ble.NotificationEvent{{
				ID: "1", Source: "android", SourceEventID: "old", Event: "added", ReceivedAt: "2026-08-20T00:00:00Z",
			}}}, nil
		case 2:
			if since != "1" || generation != "g1" {
				t.Fatalf("unexpected second request since=%q generation=%q", since, generation)
			}
			return ble.EventPage{Generation: "g2", ResetRequired: true, OldestID: "1"}, nil
		default:
			if since != "0" || generation != "g2" {
				t.Fatalf("unexpected reset request since=%q generation=%q", since, generation)
			}
			return ble.EventPage{Generation: "g2", Events: []ble.NotificationEvent{{
				ID: "1", Source: "android", SourceEventID: "new", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z",
			}}}, nil
		}
	}
	c, err := NewNotificationContext(root, reader)
	if err != nil {
		t.Fatal(err)
	}
	first, err := c.Consume(context.Background(), 10)
	if err != nil || len(first) != 1 || first[0].ContextID != "1" {
		t.Fatalf("first consume=%#v err=%v", first, err)
	}
	second, err := c.Consume(context.Background(), 10)
	if err != nil || len(second) != 1 || second[0].ID != "1" || second[0].ContextID != "2" {
		t.Fatalf("second consume=%#v err=%v", second, err)
	}
	if err := c.CommitProcessed(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	if err := c.CommitProcessed(context.Background(), second[:1]); err != nil {
		t.Fatal(err)
	}
	if got := c.State().MemoryCursor; got != "2" {
		t.Fatalf("MemoryCursor=%q, want 2", got)
	}
	state := c.State()
	if len(state.Gaps) != 1 || state.Gaps[0].Reason != "ble_generation_reset" || state.Gaps[0].Generation != "g2" || state.Gaps[0].PreviousGeneration != "g1" {
		t.Fatalf("generation reset gap=%#v, want old generation g1 and current generation g2", state.Gaps)
	}
}

func TestNotificationContextRecordsRingGapAndResetsGeneration(t *testing.T) {
	root := t.TempDir()
	calls := 0
	reader := func(_ context.Context, since, generation string, _ int) (ble.EventPage, error) {
		calls++
		if calls == 1 {
			return ble.EventPage{Generation: "new", ResetRequired: true, OldestID: "4"}, nil
		}
		if since != "0" || generation != "new" {
			t.Fatalf("reset request since=%q generation=%q", since, generation)
		}
		return ble.EventPage{Generation: "new", Truncated: true, OldestID: "4", LastID: "4", Events: []ble.NotificationEvent{{
			ID: "4", Source: "ios_ancs", SourceEventID: "evt-4", DeviceID: "phone", Event: "added",
			ReceivedAt: "2026-08-21T00:00:00Z",
		}}}, nil
	}
	c, err := NewNotificationContext(root, reader)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Consume(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	state := c.State()
	if state.Generation != "new" || state.SourceCursor != "4" || state.GapCount != 2 {
		t.Fatalf("unexpected gap state: %#v", state)
	}
}

func TestNotificationContextPersistsGenerationResetBeforeRetry(t *testing.T) {
	root := t.TempDir()
	calls := 0
	reader := func(_ context.Context, since, generation string, _ int) (ble.EventPage, error) {
		calls++
		if calls == 1 {
			if since != "295" || generation != "old" {
				t.Fatalf("initial request since=%q generation=%q", since, generation)
			}
			return ble.EventPage{Generation: "new", ResetRequired: true, OldestID: "9"}, nil
		}
		if since != "0" || generation != "new" {
			t.Fatalf("reset request since=%q generation=%q", since, generation)
		}
		return ble.EventPage{}, context.Canceled
	}
	c, err := NewNotificationContext(root, reader)
	if err != nil {
		t.Fatal(err)
	}
	c.mu.Lock()
	c.state.Generation = "old"
	c.state.SourceCursor = "295"
	c.state.StoredCursor = "295"
	if err := c.persistLocked(); err != nil {
		c.mu.Unlock()
		t.Fatal(err)
	}
	c.mu.Unlock()
	if _, err := c.Consume(context.Background(), 10); err != context.Canceled {
		t.Fatalf("Consume() err=%v, want context canceled", err)
	}
	state := c.State()
	if state.Generation != "new" || state.SourceCursor != "0" || state.StoredCursor != "295" {
		t.Fatalf("reset state=%#v, want generation new, source cursor 0, and stored cursor 295", state)
	}
	if len(state.Gaps) != 1 || state.Gaps[0].Generation != "new" || state.Gaps[0].PreviousGeneration != "old" || state.Gaps[0].FromID != "295" {
		t.Fatalf("reset gap=%#v, want explicit old-to-new generation diagnostics", state.Gaps)
	}
	reloaded, err := NewNotificationContext(root, reader)
	if err != nil {
		t.Fatal(err)
	}
	if state := reloaded.State(); state.Generation != "new" || state.SourceCursor != "0" || state.StoredCursor != "295" {
		t.Fatalf("persisted reset state=%#v, want generation new, source cursor 0, and stored cursor 295", state)
	}
}

func TestNotificationContextCommitRejectsNonContiguousBatch(t *testing.T) {
	c, err := NewNotificationContext(t.TempDir(), func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	err = c.CommitProcessed(context.Background(), []NotificationRecord{{ContextID: "2"}})
	if err == nil {
		t.Fatal("CommitProcessed accepted a non-contiguous cursor")
	}
}

func TestNotificationContextReadsAllCompleteRecordsWhenFinalLineHasNoNewline(t *testing.T) {
	root := t.TempDir()
	c, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "notifications", "events", "2026-08-21.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	first, _ := json.Marshal(NotificationRecord{ContextID: "1", NotificationEvent: ble.NotificationEvent{ReceivedAt: "2026-08-21T00:00:00Z"}})
	second, _ := json.Marshal(NotificationRecord{ContextID: "2", NotificationEvent: ble.NotificationEvent{ReceivedAt: "2026-08-21T00:01:00Z"}})
	if err := os.WriteFile(path, append(append(first, '\n'), second...), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := c.ReadPending(context.Background(), 10)
	if err != nil || len(got) != 2 || got[0].ContextID != "1" || got[1].ContextID != "2" {
		t.Fatalf("ReadPending() got=%#v err=%v", got, err)
	}
}

func TestNotificationContextReadPendingAppliesLimitAfterCursorSort(t *testing.T) {
	root := t.TempDir()
	c, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for date, record := range map[string]NotificationRecord{
		"2026-08-20": {ContextID: "2", NotificationEvent: ble.NotificationEvent{ReceivedAt: "2026-08-20T00:00:00Z"}},
		"2026-08-21": {ContextID: "1", NotificationEvent: ble.NotificationEvent{ReceivedAt: "2026-08-21T00:00:00Z"}},
	} {
		data, _ := json.Marshal(record)
		path := filepath.Join(root, "notifications", "events", date+".jsonl")
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := c.ReadPending(context.Background(), 1)
	if err != nil || len(got) != 1 || got[0].ContextID != "1" {
		t.Fatalf("ReadPending() got=%#v err=%v", got, err)
	}
}

func TestNotificationContextReadPendingAllDoesNotApplyDefaultLimit(t *testing.T) {
	root := t.TempDir()
	c, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	events := make([]ble.NotificationEvent, 0, 125)
	for index := 1; index <= 125; index++ {
		events = append(events, ble.NotificationEvent{
			Source:        "android",
			SourceEventID: "event-" + strconv.Itoa(index),
			Title:         "Notification " + strconv.Itoa(index),
			ReceivedAt:    "2026-08-21T00:00:00Z",
		})
	}
	if records, err := c.seedForBenchmark(context.Background(), events); err != nil || len(records) != len(events) {
		t.Fatalf("seedForBenchmark() records=%d err=%v", len(records), err)
	}
	got, err := c.ReadPendingAll(context.Background())
	if err != nil || len(got) != len(events) {
		t.Fatalf("ReadPendingAll() records=%d err=%v, want %d", len(got), err, len(events))
	}
	if got[0].ContextID != "1" || got[len(got)-1].ContextID != "125" {
		t.Fatalf("ReadPendingAll() cursors=%q..%q", got[0].ContextID, got[len(got)-1].ContextID)
	}
}

func TestNotificationEventFileNamePrefersPhoneDateOverReceivedAt(t *testing.T) {
	event := ble.NotificationEvent{
		Date:       "20260827T112215",
		ReceivedAt: "2021-01-01T00:00:00Z",
	}
	if got := notificationEventFileName(event); got != "2026-08-27.jsonl" {
		t.Fatalf("notificationEventFileName()=%q, want 2026-08-27.jsonl", got)
	}
}

func TestNotificationContextCleanerProtectsRecentRecordsInOldShard(t *testing.T) {
	root := t.TempDir()
	store, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "notifications", "events", "2021-01-01.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	record := NotificationRecord{
		ContextID: "1",
		NotificationEvent: ble.NotificationEvent{
			Date:       "20260827T112215",
			ReceivedAt: "2021-01-01T00:00:00Z",
		},
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := store.CommitProcessed(context.Background(), []NotificationRecord{record}); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 8, 27, 12, 0, 0, 0, time.UTC)
	freed, err := store.CleanupProcessedBefore(context.Background(), 14*24*time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if freed != 0 {
		t.Fatalf("CleanupProcessedBefore freed %d bytes from a recent record", freed)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("recent record in old shard was removed: %v", err)
	}
}

func TestNotificationContextSanitizeTruncatesUTF8ByRunes(t *testing.T) {
	value := truncateNotificationText("你好世界", 3)
	if value != "你好世" {
		t.Fatalf("truncateNotificationText()=%q, want 你好世", value)
	}
}

func TestNotificationFingerprintWindowIsBounded(t *testing.T) {
	fingerprints := make(map[string]string, maxNotificationFingerprints+1)
	for cursor := 1; cursor <= maxNotificationFingerprints+1; cursor++ {
		fingerprints["fingerprint-"+strconv.Itoa(cursor)] = strconv.Itoa(cursor)
	}
	pruneNotificationFingerprints(fingerprints)
	if len(fingerprints) != trimNotificationFingerprintsTo {
		t.Fatalf("fingerprints=%d, want %d", len(fingerprints), trimNotificationFingerprintsTo)
	}
	if _, exists := fingerprints["fingerprint-1"]; exists {
		t.Fatal("oldest fingerprint was not pruned")
	}
	if _, exists := fingerprints["fingerprint-"+strconv.Itoa(maxNotificationFingerprints+1)]; !exists {
		t.Fatal("newest fingerprint was pruned")
	}
}

func TestNotificationContextRecoversCursorAndDedupeFromEventLog(t *testing.T) {
	root := t.TempDir()
	seed, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	record := NotificationRecord{
		NotificationEvent: ble.NotificationEvent{ID: "7", Source: "android", SourceEventID: "evt-7", ReceivedAt: "2026-08-21T00:00:00Z"},
		ContextID:         "1",
		Generation:        "g",
	}
	seed.mu.Lock()
	err = seed.appendEventLocked(record)
	seed.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	recovered, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{record.NotificationEvent}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if state := recovered.State(); state.StoredCursor != "1" || state.SourceCursor != "7" || state.Generation != "g" {
		t.Fatalf("recovered state=%#v", state)
	}
	accepted, err := recovered.Consume(context.Background(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(accepted) != 0 {
		t.Fatalf("replayed event was appended again: %#v", accepted)
	}
	records, err := readNotificationRecordFile(filepath.Join(root, "notifications", "events", "2026-08-21.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 {
		t.Fatalf("event log contains %d records, want 1", len(records))
	}
}

func TestNotificationContextReadOnlyReadDoesNotRepairIncompleteTailRecord(t *testing.T) {
	root := t.TempDir()
	eventsDir := filepath.Join(root, "notifications", "events")
	if err := os.MkdirAll(eventsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(eventsDir, "2026-08-21.jsonl")
	complete := `{"id":"1","context_id":"1","received_at":"2026-08-21T00:00:00Z"}` + "\n"
	if err := os.WriteFile(path, append([]byte(complete), []byte(`{"id":"2","context_id":"2"`)...), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := readNotificationRecordFileReadOnly(path)
	if err == nil || len(results) != 0 {
		t.Fatalf("read-only query results=%#v err=%v, want incomplete-record error", results, err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	want := complete + `{"id":"2","context_id":"2"`
	if string(data) != want {
		t.Fatalf("query modified file=%q, want %q", string(data), want)
	}
}

func TestNotificationContextReadsRecordLargerThanScannerLimit(t *testing.T) {
	root := t.TempDir()
	eventsDir := filepath.Join(root, "notifications", "events")
	if err := os.MkdirAll(eventsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := NotificationRecord{
		NotificationEvent: ble.NotificationEvent{ID: "1", Source: "ios", SourceEventID: "large", Event: "added", Message: strings.Repeat("x", 256*1024), ReceivedAt: "2026-08-21T00:00:00Z"},
		ContextID:         "1",
		Generation:        "g",
	}
	line, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(eventsDir, "2026-08-21.jsonl")
	if err := os.WriteFile(path, append(line, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	results, err := readNotificationRecordFileReadOnly(path)
	if err != nil || len(results) != 1 || len(results[0].Message) != 256*1024 {
		messageLen := 0
		if len(results) > 0 {
			messageLen = len(results[0].Message)
		}
		t.Fatalf("Query() len=%d message=%d err=%v, want one 256 KiB record", len(results), messageLen, err)
	}
}

func TestNotificationContextRepairsIncompleteTailRecord(t *testing.T) {
	root := t.TempDir()
	eventsDir := filepath.Join(root, "notifications", "events")
	if err := os.MkdirAll(eventsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	record := NotificationRecord{
		NotificationEvent: ble.NotificationEvent{ID: "1", Source: "ios", SourceEventID: "evt-1", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z"},
		ContextID:         "1",
		Generation:        "g",
	}
	line, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(eventsDir, "2026-08-21.jsonl")
	partial := append(append(append([]byte{}, line...), '\n'), []byte(`{"context_id":"2"`)...)
	if err := os.WriteFile(path, partial, 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	pending, err := c.ReadPending(context.Background(), 10)
	if err != nil || len(pending) != 1 || pending[0].ContextID != "1" {
		t.Fatalf("ReadPending()=%#v err=%v, want the complete record only", pending, err)
	}
	repaired, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(repaired, append(line, '\n')) {
		t.Fatalf("repaired JSONL=%q, want complete first record with newline", repaired)
	}
}
