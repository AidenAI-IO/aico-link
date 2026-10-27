package agent

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

func TestApplyMemoryCandidateUpdatesAndReinforcesByID(t *testing.T) {
	store := NewLongTermMemoryStore(t.TempDir())
	ctx := context.Background()
	first := MemoryItem{
		ID: "tmp_notification_42", Type: "fact", TimeScope: "temporary",
		Title: "包裹更新", Content: "包裹明天送达", Tags: []string{"notification"},
		SourceRefs:       []MemorySourceRef{{Type: "notification", ID: "42", EventIDs: []string{"e1"}}},
		EvidenceExcerpts: []string{"包裹明天送达"},
	}
	got, err := store.ApplyMemoryCandidate(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != MemoryOperationAdd || got.ID != first.ID {
		t.Fatalf("first apply=%#v", got)
	}

	same := first
	same.SourceRefs = []MemorySourceRef{{Type: "notification", ID: "42", EventIDs: []string{"e2"}}}
	got, err = store.ApplyMemoryCandidate(ctx, same)
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != MemoryOperationReinforce {
		t.Fatalf("same apply=%#v, want reinforce", got)
	}

	updated := same
	updated.Content = "包裹今天送达"
	got, err = store.ApplyMemoryCandidate(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != MemoryOperationUpdate {
		t.Fatalf("updated apply=%#v, want update", got)
	}

	results, err := store.Search(ctx, MemoryQuery{Tags: []string{"notification"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Content != "包裹今天送达" {
		t.Fatalf("results=%#v", results)
	}
	parsed, err := readMemoryMarkdown(results[0].FilePath)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Item.Revision != 3 || len(parsed.Item.SourceRefs[0].EventIDs) != 2 {
		t.Fatalf("merged item=%#v", parsed.Item)
	}
}

func TestApplyMemoryIntentRequiresExplicitAction(t *testing.T) {
	store := NewLongTermMemoryStore(t.TempDir())
	if _, err := store.ApplyMemoryIntent(context.Background(), MemoryIntent{Item: MemoryItem{ID: "missing-action"}}); err == nil {
		t.Fatal("missing action unexpectedly accepted")
	}
	device := NewDeviceMemoryStore(t.TempDir())
	item := DeviceMemoryItem{ID: "missing-action"}
	if _, err := device.ApplyMemoryIntent(context.Background(), MemoryIntent{DeviceItem: &item}); err == nil {
		t.Fatal("device missing action unexpectedly accepted")
	}
}

func TestApplyMemoryIntentReportsNilStoreBeforeIntentValidation(t *testing.T) {
	var longTerm *LongTermMemoryStore
	if _, err := longTerm.ApplyMemoryIntent(context.Background(), MemoryIntent{}); err == nil || err.Error() != "memory store is not configured" {
		t.Fatalf("long-term nil store error = %v", err)
	}
	var device *DeviceMemoryStore
	if _, err := device.ApplyMemoryIntent(context.Background(), MemoryIntent{}); err == nil || err.Error() != "memory store is not configured" {
		t.Fatalf("device nil store error = %v", err)
	}
}

func TestApplyMemoryIntentCreateRestoresMatchingDeletedMemory(t *testing.T) {
	store := NewLongTermMemoryStore(t.TempDir())
	ctx := context.Background()
	item := MemoryItem{ID: "deleted-memory", Type: "fact", TimeScope: "temporary", Content: "same", SourceRefs: []MemorySourceRef{{Type: "notification", ID: "deleted", EventIDs: []string{"event-deleted"}}}, EvidenceExcerpts: []string{"same"}}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: item, Action: MemoryIntentActionCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: MemoryItem{ID: item.ID}, Action: MemoryIntentActionRemove, ExpectedRevision: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: item, Action: MemoryIntentActionCreate}); err != nil {
		t.Fatal(err)
	}
	memories, err := store.Search(ctx, MemoryQuery{Limit: 10})
	if err != nil || len(memories) != 1 || memories[0].Status != "active" {
		t.Fatalf("restored memories=%#v err=%v", memories, err)
	}
}

func TestLongTermMemoryStoresShareWriteLockByRoot(t *testing.T) {
	root := t.TempDir()
	first := NewLongTermMemoryStore(root)
	second := NewLongTermMemoryStore(root)
	other := NewLongTermMemoryStore(filepath.Join(root, "other"))
	if first.writeMu != second.writeMu {
		t.Fatal("stores for the same root do not share a write lock")
	}
	if first.writeMu == other.writeMu {
		t.Fatal("stores for different roots unexpectedly share a write lock")
	}
}

func TestApplyMemoryIntentRemovesByID(t *testing.T) {
	store := NewLongTermMemoryStore(t.TempDir())
	ctx := context.Background()
	item := MemoryItem{
		ID: "tmp_notification_7", Type: "fact", TimeScope: "temporary",
		Title: "提醒", Content: "会议即将开始", EvidenceExcerpts: []string{"会议即将开始"},
		SourceRefs: []MemorySourceRef{{Type: "notification", ID: "7", EventIDs: []string{"event-7"}}},
	}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: item, Action: MemoryIntentActionCreate}); err != nil {
		t.Fatal(err)
	}
	got, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: MemoryItem{ID: item.ID}, Action: MemoryIntentActionRemove, ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != MemoryOperationRemove {
		t.Fatalf("remove apply=%#v", got)
	}
	results, err := store.Search(ctx, MemoryQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Fatalf("removed memory still recalled: %#v", results)
	}
}

func TestApplyMemoryIntentRemoveChecksExpectedRevision(t *testing.T) {
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	ctx := context.Background()
	item := MemoryItem{ID: "mem_revision_remove", Type: "fact", TimeScope: "long_term", Content: "old", SourceRefs: []MemorySourceRef{{Type: "notification", ID: "remove", EventIDs: []string{"event-remove"}}}, EvidenceExcerpts: []string{"evidence"}}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: item, Action: MemoryIntentActionCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: MemoryItem{ID: item.ID}, Action: MemoryIntentActionRemove, ExpectedRevision: 2}); err != errMemoryRevisionChanged {
		t.Fatalf("stale remove error=%v, want %v", err, errMemoryRevisionChanged)
	}
	if _, err := store.Search(ctx, MemoryQuery{Tags: nil, Limit: 10}); err != nil {
		t.Fatal(err)
	}
}

func TestApplyMemoryIntentCreateDoesNotRunSimilarityMerge(t *testing.T) {
	store := NewLongTermMemoryStore(t.TempDir())
	ctx := context.Background()
	if _, err := store.AddMemory(ctx, MemoryItem{
		ID: "existing", Type: "fact", Content: "The delivery arrives tomorrow", Tags: []string{"notification", "delivery"},
		EvidenceExcerpts: []string{"existing"},
	}); err != nil {
		t.Fatal(err)
	}
	item := MemoryItem{
		ID: "created", Type: "fact", Content: "The delivery arrives tomorrow at noon", Tags: []string{"notification", "delivery"},
		SourceRefs: []MemorySourceRef{{Type: "notification", ID: "ctx-1", EventIDs: []string{"evt-1"}}}, EvidenceExcerpts: []string{"new"},
	}
	result, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: item, Action: MemoryIntentActionCreate})
	if err != nil {
		t.Fatal(err)
	}
	if result.Operation != MemoryOperationAdd || result.ID != item.ID {
		t.Fatalf("result=%#v, want explicit create", result)
	}
	if retry, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: item, Action: MemoryIntentActionCreate}); err != nil || retry.Operation != MemoryOperationAdd {
		t.Fatalf("create retry=%#v err=%v", retry, err)
	}
	memories, err := store.Search(ctx, MemoryQuery{Tags: []string{"delivery"}, Limit: 10})
	if err != nil || len(memories) != 2 {
		t.Fatalf("memories=%#v err=%v, want both records", memories, err)
	}
	for _, memory := range memories {
		if memory.ID == item.ID && memory.Revision != 1 {
			t.Fatalf("create retry advanced revision: %#v", memory)
		}
	}
	missing := item
	missing.ID = "missing"
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: missing, Action: MemoryIntentActionUpdate, ExpectedRevision: 1}); !errors.Is(err, errMemoryTargetMissing) {
		t.Fatalf("missing update error=%v, want target missing", err)
	}
}

func TestApplyMemoryIntentCreateRetryKeepsFirstContentForSameSourceIdentity(t *testing.T) {
	store := NewLongTermMemoryStore(t.TempDir())
	ctx := context.Background()
	first := MemoryItem{
		ID: "tmp_notification_42", Type: "fact", TimeScope: "temporary",
		Content:          "The package arrives tomorrow.",
		SourceRefs:       []MemorySourceRef{{Type: "notification", ID: "42", EventIDs: []string{"event-42"}}},
		EvidenceExcerpts: []string{"The package arrives tomorrow."},
	}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: first, Action: MemoryIntentActionCreate}); err != nil {
		t.Fatal(err)
	}
	reworded := first
	reworded.Content = "Delivery is expected tomorrow."
	reworded.EvidenceExcerpts = []string{"Delivery is expected tomorrow."}
	if result, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: reworded, Action: MemoryIntentActionCreate}); err != nil || result.Operation != MemoryOperationAdd {
		t.Fatalf("reworded create retry=%#v err=%v", result, err)
	}
	memories, err := store.Search(ctx, MemoryQuery{Limit: 10})
	if err != nil || len(memories) != 1 || memories[0].Content != first.Content || memories[0].Revision != 1 {
		t.Fatalf("memories=%#v err=%v, want first create unchanged", memories, err)
	}

	unrelated := reworded
	unrelated.SourceRefs = []MemorySourceRef{{Type: "notification", ID: "99", EventIDs: []string{"event-99"}}}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: unrelated, Action: MemoryIntentActionCreate}); !errors.Is(err, errMemoryIDConflict) {
		t.Fatalf("unrelated same-ID create error=%v, want ID conflict", err)
	}
}

func TestApplyMemoryIntentPreservesActionAndIsRetrySafe(t *testing.T) {
	store := NewLongTermMemoryStore(t.TempDir())
	ctx := context.Background()
	item := MemoryItem{ID: "memory-1", Type: "fact", Title: "Delivery", Content: "Arrives tomorrow", SourceRefs: []MemorySourceRef{{Type: "notification", ID: "ctx-1", EventIDs: []string{"evt-1"}}}, EvidenceExcerpts: []string{"Arrives tomorrow"}}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: item, Action: MemoryIntentActionCreate}); err != nil {
		t.Fatal(err)
	}
	update := item
	update.Content = "Arrives today"
	update.SourceRefs = []MemorySourceRef{{Type: "notification", ID: "ctx-2", EventIDs: []string{"evt-2"}}}
	result, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: update, Action: MemoryIntentActionUpdate, ExpectedRevision: 1})
	if err != nil || result.Operation != MemoryOperationUpdate {
		t.Fatalf("update result=%#v err=%v", result, err)
	}
	result, err = store.ApplyMemoryIntent(ctx, MemoryIntent{Item: update, Action: MemoryIntentActionUpdate, ExpectedRevision: 1})
	if err != nil || result.Operation != MemoryOperationUpdate {
		t.Fatalf("retry result=%#v err=%v", result, err)
	}
	partial := update
	partial.Tags = []string{"not-applied"}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: partial, Action: MemoryIntentActionUpdate, ExpectedRevision: 1}); !errors.Is(err, errMemoryRevisionChanged) {
		t.Fatalf("partial retry error=%v, want revision conflict", err)
	}
	memories, err := store.Search(ctx, MemoryQuery{Limit: 10})
	if err != nil || len(memories) != 1 || memories[0].Revision != 2 || memories[0].Content != "Arrives today" {
		t.Fatalf("memories=%#v err=%v, want one revision-2 update", memories, err)
	}

	reinforce := MemoryItem{ID: item.ID, Type: item.Type, Title: item.Title, Content: "Different text must not replace the conclusion", SourceRefs: []MemorySourceRef{{Type: "notification", ID: "ctx-3", EventIDs: []string{"evt-3"}}}, EvidenceExcerpts: []string{"evt-3"}}
	result, err = store.ApplyMemoryIntent(ctx, MemoryIntent{Item: reinforce, Action: MemoryIntentActionReinforce, ExpectedRevision: 2})
	if err != nil || result.Operation != MemoryOperationReinforce {
		t.Fatalf("reinforce result=%#v err=%v", result, err)
	}
	memories, err = store.Search(ctx, MemoryQuery{Limit: 10})
	if err != nil || len(memories) != 1 || memories[0].Content != "Arrives today" || memories[0].Revision != 3 {
		t.Fatalf("reinforced memories=%#v err=%v, want unchanged content at revision 3", memories, err)
	}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{Item: MemoryItem{ID: item.ID}, Action: MemoryIntentActionUpdate, ExpectedRevision: 1}); !errors.Is(err, errMemoryRevisionChanged) {
		t.Fatalf("stale update error=%v, want revision conflict", err)
	}
	if strings.Contains(memories[0].Content, "Different text") {
		t.Fatal("reinforce changed the memory conclusion")
	}
}

func TestDeviceMemoryApplyUsesCommonUpdateAndRevision(t *testing.T) {
	store := NewDeviceMemoryStore(t.TempDir())
	ctx := context.Background()
	first := DeviceMemoryItem{
		ID: "devmem_delivery", Type: "fact", Status: deviceMemoryStatusActive,
		Title: "包裹", Summary: "明天送达", Content: "包裹明天送达",
		EvidenceRefs: []MemorySourceRef{{Type: "episode", ID: "episode-1", EventIDs: []string{"event-1"}}},
	}
	got, err := store.ApplyMemoryIntent(ctx, MemoryIntent{DeviceItem: &first, Action: MemoryIntentActionCreate})
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != MemoryOperationAdd || got.ID != first.ID {
		t.Fatalf("first apply=%#v", got)
	}

	updated := first
	updated.Revision = 1
	updated.Summary = "今天送达"
	updated.Content = "包裹今天送达"
	updated.EvidenceRefs = []MemorySourceRef{{Type: "episode", ID: "episode-2", EventIDs: []string{"event-2"}}}
	got, err = store.ApplyMemoryIntent(ctx, MemoryIntent{DeviceItem: &updated, Action: MemoryIntentActionUpdate, ExpectedRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if got.Operation != MemoryOperationUpdate {
		t.Fatalf("updated apply=%#v", got)
	}

	current, found, err := store.Get(ctx, first.ID)
	if err != nil || !found {
		t.Fatalf("get current found=%v err=%v", found, err)
	}
	if current.Revision != 2 || len(current.RevisionHistory) != 1 || len(current.EvidenceRefs) != 2 {
		t.Fatalf("merged device memory=%#v", current)
	}
}

func TestDeviceMemoryProcedureUpdateReplacesOnlyWhenRequested(t *testing.T) {
	store := NewDeviceMemoryStore(t.TempDir())
	ctx := context.Background()
	oldStep := ProcedureStep{Tool: "touch_gesture", Description: "Use the old route"}
	newStep := ProcedureStep{Tool: "touch_gesture", Description: "Use the new route"}
	first := DeviceMemoryItem{
		ID: "devmem_route", Type: "procedure", Status: deviceMemoryStatusActive,
		Title: "Route", Summary: "Use the route", Content: "Current route",
		Steps:        []ProcedureStep{oldStep},
		EvidenceRefs: []MemorySourceRef{{Type: "episode", ID: "episode-1", EventIDs: []string{"result-1"}}},
	}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{DeviceItem: &first, Action: MemoryIntentActionCreate}); err != nil {
		t.Fatal(err)
	}
	update := first
	update.Revision = 1
	update.Steps = []ProcedureStep{newStep}
	update.EvidenceRefs = []MemorySourceRef{{Type: "episode", ID: "episode-2", EventIDs: []string{"result-2"}}}
	intent := MemoryIntent{DeviceItem: &update, Action: MemoryIntentActionUpdate, ExpectedRevision: 1}
	if _, err := store.ApplyMemoryIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	merged, found, err := store.Get(ctx, first.ID)
	if err != nil || !found || !equalEpisodeMemorySteps(merged.Steps, []ProcedureStep{oldStep, newStep}) {
		t.Fatalf("ordinary update should merge steps: found=%v err=%v item=%#v", found, err, merged)
	}
	intent.ReplaceSteps = true
	if _, err := store.ApplyMemoryIntent(ctx, intent); !errors.Is(err, errEpisodeMemoryRevisionChanged) {
		t.Fatalf("stale replacement accepted as already applied: %v", err)
	}
	intent.ExpectedRevision = 2
	if _, err := store.ApplyMemoryIntent(ctx, intent); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyMemoryIntent(ctx, intent); err != nil {
		t.Fatalf("identical replacement retry failed: %v", err)
	}
	replaced, found, err := store.Get(ctx, first.ID)
	if err != nil || !found || replaced.Revision != 3 || !equalEpisodeMemorySteps(replaced.Steps, []ProcedureStep{newStep}) ||
		len(replaced.RevisionHistory) != 2 || !equalEpisodeMemorySteps(replaced.RevisionHistory[1].Steps, []ProcedureStep{oldStep, newStep}) {
		t.Fatalf("replacement lost order or history: found=%v err=%v item=%#v", found, err, replaced)
	}
}

func TestDeviceMemoryCreateRetryKeepsFirstContentForSameEpisode(t *testing.T) {
	store := NewDeviceMemoryStore(t.TempDir())
	ctx := context.Background()
	first := DeviceMemoryItem{
		ID: "devmem_retry", Type: "fact", Status: deviceMemoryStatusActive,
		Title: "Delivery", Summary: "Tomorrow", Content: "The package arrives tomorrow.",
		EvidenceRefs: []MemorySourceRef{{Type: "episode", ID: "episode-retry", EventIDs: []string{"event-1"}}},
	}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{DeviceItem: &first, Action: MemoryIntentActionCreate}); err != nil {
		t.Fatal(err)
	}
	reworded := first
	reworded.Summary = "Expected tomorrow"
	reworded.Content = "Delivery is expected tomorrow."
	if result, err := store.ApplyMemoryIntent(ctx, MemoryIntent{DeviceItem: &reworded, Action: MemoryIntentActionCreate}); err != nil || result.Operation != MemoryOperationAdd {
		t.Fatalf("device create retry=%#v err=%v", result, err)
	}
	current, found, err := store.Get(ctx, first.ID)
	if err != nil || !found || current.Content != first.Content || effectiveDeviceMemoryRevision(current) != 1 {
		t.Fatalf("current=%#v found=%v err=%v, want first create unchanged", current, found, err)
	}
}

func TestDeviceMemoryRemoveIsIdempotent(t *testing.T) {
	store := NewDeviceMemoryStore(t.TempDir())
	ctx := context.Background()
	item := DeviceMemoryItem{ID: "devmem_remove", Type: "fact", Status: deviceMemoryStatusActive, Content: "remove me", EvidenceRefs: []MemorySourceRef{{Type: "episode", ID: "remove", EventIDs: []string{"event-remove"}}}}
	if _, err := store.ApplyMemoryIntent(ctx, MemoryIntent{DeviceItem: &item, Action: MemoryIntentActionCreate}); err != nil {
		t.Fatal(err)
	}
	removed, err := store.ApplyMemoryIntent(ctx, MemoryIntent{DeviceItem: &DeviceMemoryItem{ID: item.ID}, Action: MemoryIntentActionRemove, ExpectedRevision: 1})
	if err != nil || removed.Operation != MemoryOperationRemove {
		t.Fatalf("remove result=%#v err=%v", removed, err)
	}
	retried, err := store.ApplyMemoryIntent(ctx, MemoryIntent{DeviceItem: &DeviceMemoryItem{ID: item.ID}, Action: MemoryIntentActionRemove, ExpectedRevision: 1})
	if err != nil || retried.Operation != MemoryOperationIgnore {
		t.Fatalf("remove retry result=%#v err=%v", retried, err)
	}
	missing, err := store.ApplyMemoryIntent(ctx, MemoryIntent{DeviceItem: &DeviceMemoryItem{ID: "missing"}, Action: MemoryIntentActionRemove, ExpectedRevision: 1})
	if err != nil || missing.Operation != MemoryOperationIgnore {
		t.Fatalf("missing remove result=%#v err=%v", missing, err)
	}
}
