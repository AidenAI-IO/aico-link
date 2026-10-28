package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/ble"
	"aico-agent/internal/logging"
	"github.com/tmc/langchaingo/llms"
)

func TestNotificationMemoryProcessorFiltersNoiseAndWritesTemporaryMemory(t *testing.T) {
	root := t.TempDir()
	reader := func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{
			{ID: "1", Source: "android", SourceID: "otp", NotificationUID: 1, SourceEventID: "otp", AppIdentifier: "com.auth", Title: "验证码", Message: "123456", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z"},
			{ID: "2", Source: "android", SourceID: "useful", NotificationUID: 2, SourceEventID: "useful", AppIdentifier: "com.delivery", Title: "包裹更新", Message: "明天送达", Event: "added", ReceivedAt: "2026-08-21T00:01:00Z"},
		}}, nil
	}
	ctxStore, err := NewNotificationContext(root, reader)
	if err != nil {
		t.Fatal(err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "results":[
    {"context_id":"1","proposal":{"actions":[{"action":"ignore"}]}},
    {"context_id":"2","proposal":{"actions":[{"action":"add","scope":"temporary","type":"fact","content":"包裹明天送达","confidence":0.9,"expires_at":"2099-01-01T00:00:00Z"}]}}
  ]
}`}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)
	_, err = processor.ProcessBatch(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	results, err := processor.temporary.Search(context.Background(), MemoryQuery{Tags: []string{"com.delivery"}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].ID != "tmp_notification_2" {
		t.Fatalf("temporary results=%#v", results)
	}
	if got := ctxStore.State().MemoryCursor; got != "2" {
		t.Fatalf("MemoryCursor=%q, want 2", got)
	}
	raw, err := os.ReadFile(filepath.Join(root, "notifications", "events", "2026-08-21.jsonl"))
	if err != nil {
		t.Fatalf("read raw notification log: %v", err)
	}
	if !strings.Contains(string(raw), `"message":"123456"`) || !strings.Contains(string(raw), `"message":"明天送达"`) {
		t.Fatalf("raw log did not retain both ignored and useful notifications: %s", raw)
	}
}

func TestNotificationMemoryPrefilterOnlyDropsStructuralNoise(t *testing.T) {
	tests := []struct {
		name  string
		event ble.NotificationEvent
		want  bool
	}{
		{name: "otp reaches semantic classifier", event: ble.NotificationEvent{Title: "验证码", Message: "123456"}, want: false},
		{name: "marketing reaches semantic classifier", event: ble.NotificationEvent{Title: "限时促销", Message: "点击领取优惠"}, want: false},
		{name: "bank repayment", event: ble.NotificationEvent{AppIdentifier: "com.bank", Title: "携程金融", Message: "应还 291.50 元，还款日 09月02日"}, want: false},
		{name: "health deadline", event: ble.NotificationEvent{Title: "健康提醒", Message: "复诊预约在 09月02日"}, want: false},
		{name: "private message", event: ble.NotificationEvent{Title: "私人聊天", Message: "明天十点开会"}, want: false},
		{name: "removed", event: ble.NotificationEvent{Event: "removed", Title: "账单", Message: "已撤回"}, want: true},
		{name: "empty", event: ble.NotificationEvent{}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := notificationMemoryIgnored(tt.event); got != tt.want {
				t.Fatalf("notificationMemoryIgnored() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestNotificationMemoryProcessorWithoutModelKeepsContextPending(t *testing.T) {
	root := t.TempDir()
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{
			ID: "1", Source: "ios_ancs", SourceID: "bill", NotificationUID: 1, SourceEventID: "bill",
			AppIdentifier: "com.ctrip.finance", Title: "携程金融", Message: "应还 291.50 元，还款日 09月02日", Event: "added",
			ReceivedAt: "2026-08-27T11:21:00Z",
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, nil)
	if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	memories, err := processor.temporary.Search(context.Background(), MemoryQuery{Limit: 10})
	if err != nil || len(memories) != 0 {
		t.Fatalf("memories=%#v err=%v, want no unclassified fallback memory", memories, err)
	}
	state := ctxStore.State()
	if state.StoredCursor != "1" || state.SourceCursor != "1" || state.MemoryCursor != "" {
		t.Fatalf("state=%#v, want raw context stored but pending semantic classification", state)
	}
}

func TestNotificationMemoryProcessorImplementsWorkerIsolationSeam(t *testing.T) {
	root := t.TempDir()
	ctxStore, err := NewNotificationContext(filepath.Join(root, "memory"), func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	processor := NewNotificationMemoryProcessor(ctxStore, filepath.Join(root, "memory"), nil, nil)
	var _ MemoryProcessor = processor
	worker := newMemoryWorker(processor, defaultMemoryWorkerIdleDelay)
	if worker == nil {
		t.Fatal("notification worker was not created")
	}
	worker.Stop()
}

func TestNotificationMemoryPromptDefinesTargetedActionContract(t *testing.T) {
	prompt := buildNotificationMemoryBatchPrompt(
		[]NotificationRecord{{ContextID: "1"}},
		map[string][]MemoryMergeReference{"1": {{
			Scope: "temporary", ID: "tmp-1", Revision: 2,
		}}},
	)
	for _, required := range []string{
		"add and update, content is required",
		"promote copies the exact temporary catalog memory_id and memory_revision into scope long_term",
		"Every targeted action must include the exact memory_id and memory_revision",
		"user_fact",
		"public_info",
		"general news, product facts, or public prices",
		"confidence",
	} {
		if !strings.Contains(prompt, required) {
			t.Fatalf("notification prompt missing %q: %s", required, prompt)
		}
	}
}

func TestNotificationMemoryConfidenceDistinguishesOmittedFromZero(t *testing.T) {
	var omitted notificationMemoryAction
	if err := json.Unmarshal([]byte(`{"action":"add"}`), &omitted); err != nil {
		t.Fatal(err)
	}
	if got, err := normalizeNotificationConfidence(omitted.Confidence, omitted.confidenceSet); err != nil || got != 0.7 {
		t.Fatalf("omitted confidence = %v, %v; want default 0.7", got, err)
	}
	var explicitZero notificationMemoryAction
	if err := json.Unmarshal([]byte(`{"action":"add","confidence":0}`), &explicitZero); err != nil {
		t.Fatal(err)
	}
	if _, err := normalizeNotificationConfidence(explicitZero.Confidence, explicitZero.confidenceSet); err == nil {
		t.Fatal("explicit zero confidence was accepted as omitted")
	}
}

func TestNotificationMemoryProcessorUsesMergeEngineForAdd(t *testing.T) {
	root := t.TempDir()
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1", AppIdentifier: "com.delivery", Title: "包裹更新", Message: "明天送达", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	llm := &episodeMemoryScriptedModel{responses: []string{`{"actions":[{"action":"add","scope":"temporary","type":"fact","title":"包裹更新","content":"包裹明天送达","confidence":0.86,"expires_at":"2099-01-01T00:00:00Z","tags":["notification","com.delivery"]}]}`}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, llm)
	if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	results, err := processor.temporary.Search(context.Background(), MemoryQuery{Tags: []string{"com.delivery"}, Limit: 5})
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].Content != "包裹明天送达" || results[0].Priority != 40 || results[0].Confidence != 0.86 {
		t.Fatalf("temporary results=%#v", results)
	}
	if got := ctxStore.State().MemoryCursor; got != "1" {
		t.Fatalf("MemoryCursor=%q, want 1", got)
	}
}

func TestNotificationMemoryProcessorRetainsUserBillAndIgnoresPublicProductNews(t *testing.T) {
	root := t.TempDir()
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{
			{ID: "1", Source: "ios_ancs", SourceID: "car-news", NotificationUID: 1, SourceEventID: "car-news", AppIdentifier: "com.dongchedi", Title: "懂车帝", Message: "宝马 iX3 售价 26.99 万", Event: "added", ReceivedAt: "2026-08-27T11:20:00Z"},
			{ID: "2", Source: "ios_ancs", SourceID: "bill", NotificationUID: 2, SourceEventID: "bill", AppIdentifier: "com.ctrip.finance", Title: "携程金融", Message: "应还 291.50 元，还款日 09月02日", Event: "added", ReceivedAt: "2026-08-27T11:21:00Z"},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "results":[
    {"context_id":"1","proposal":{"actions":[{"action":"ignore"}]}},
    {"context_id":"2","proposal":{"actions":[{"action":"add","scope":"temporary","type":"fact","title":"携程金融还款","content":"携程金融应还 291.50 元，还款日 09月02日","confidence":0.96}]}}
  ]
}`}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)
	if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	if prompt := model.firstCallText(); !strings.Contains(prompt, "宝马 iX3") || !strings.Contains(prompt, "应还 291.50 元") {
		t.Fatalf("model prompt did not contain both classifications: %s", prompt)
	}
	memories, err := processor.temporary.Search(context.Background(), MemoryQuery{Limit: 10})
	if err != nil || len(memories) != 1 || memories[0].Content != "携程金融应还 291.50 元，还款日 09月02日" {
		t.Fatalf("memories=%#v err=%v, want only the user-specific bill", memories, err)
	}
}

func TestNotificationMemoryProcessorCreateRetryWithRewordedProposalCommits(t *testing.T) {
	root := t.TempDir()
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1", AppIdentifier: "com.delivery", Title: "Delivery", Message: "Package arrives tomorrow", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{
		`{"actions":[{"action":"add","scope":"temporary","type":"fact","content":"The package arrives tomorrow"}]}`,
		`{"actions":[{"action":"add","scope":"temporary","type":"fact","content":"Delivery is expected tomorrow"}]}`,
	}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)
	records, err := ctxStore.Consume(context.Background(), 10)
	if err != nil || len(records) != 1 {
		t.Fatalf("consume records=%#v err=%v", records, err)
	}
	if err := processor.resolveRecords(context.Background(), records); err != nil {
		t.Fatalf("first resolve: %v", err)
	}
	if err := processor.resolveRecords(context.Background(), records); err != nil {
		t.Fatalf("retry resolve: %v", err)
	}
	if err := ctxStore.CommitProcessed(context.Background(), records); err != nil {
		t.Fatalf("commit retry: %v", err)
	}
	if got := ctxStore.State().MemoryCursor; got != "1" {
		t.Fatalf("memory cursor=%q, want 1", got)
	}
	memories, err := processor.temporary.Search(context.Background(), MemoryQuery{Limit: 10})
	if err != nil || len(memories) != 1 || memories[0].Content != "The package arrives tomorrow" || memories[0].Revision != 1 {
		t.Fatalf("memories=%#v err=%v, want first create unchanged", memories, err)
	}
}

func TestNotificationMemoryProcessorAddDoesNotSupersedeSimilarMemory(t *testing.T) {
	root := t.TempDir()
	temporary := NewLongTermMemoryStore(filepath.Join(root, "temporary"))
	if _, err := temporary.AddMemory(context.Background(), MemoryItem{
		ID: "existing-delivery", Type: "fact", TimeScope: "temporary", Title: "Delivery update", Content: "The package arrives tomorrow",
		Tags: []string{"notification", "com.delivery"}, Entities: []string{"com.delivery"}, EvidenceExcerpts: []string{"old notification"},
	}); err != nil {
		t.Fatal(err)
	}
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1", AppIdentifier: "com.delivery", Title: "Delivery window", Message: "The package arrives tomorrow at noon", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`{"actions":[{"action":"add","scope":"temporary","type":"fact","title":"Delivery window","content":"The package arrives tomorrow at noon","expires_at":"2099-01-01T00:00:00Z","tags":["notification","com.delivery"],"entities":["com.delivery"]}]}`}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)
	if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	memories, err := temporary.Search(context.Background(), MemoryQuery{Tags: []string{"com.delivery"}, Limit: 10})
	if err != nil || len(memories) != 2 {
		t.Fatalf("memories=%#v err=%v, want explicit add plus existing memory", memories, err)
	}
	active := map[string]bool{}
	for _, memory := range memories {
		active[memory.ID] = true
	}
	if !active["existing-delivery"] || !active["tmp_notification_1"] {
		t.Fatalf("active IDs=%#v, want both records", active)
	}
}

func TestNotificationMemoryProcessorPreservesReinforceAction(t *testing.T) {
	root := t.TempDir()
	temporary := NewLongTermMemoryStore(filepath.Join(root, "temporary"))
	if _, err := temporary.AddMemory(context.Background(), MemoryItem{
		ID: "tmp_delivery", Type: "fact", TimeScope: "temporary", Title: "Delivery", Content: "The package arrives tomorrow",
		Tags: []string{"notification", "com.delivery"}, Entities: []string{"com.delivery"}, EvidenceExcerpts: []string{"old notification"},
	}); err != nil {
		t.Fatal(err)
	}
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1", AppIdentifier: "com.delivery", Title: "Delivery", Message: "Another notification confirms the delivery", Event: "modified", ReceivedAt: "2026-08-21T00:00:00Z"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`{"actions":[{"action":"reinforce","scope":"temporary","memory_id":"tmp_delivery","memory_revision":1,"type":"fact","content":"Text that must not replace the conclusion"}]}`}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)
	if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	memories, err := temporary.Search(context.Background(), MemoryQuery{Tags: []string{"com.delivery"}, Limit: 10})
	if err != nil || len(memories) != 1 || memories[0].Revision != 2 || memories[0].Content != "The package arrives tomorrow" {
		t.Fatalf("memories=%#v err=%v, want reinforced evidence without conclusion update", memories, err)
	}
}

func TestNotificationMemoryProcessorBatchesUsefulNotificationsIntoOneModelCall(t *testing.T) {
	root := t.TempDir()
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{
			{ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1", AppIdentifier: "com.calendar", Title: "会议", Message: "明天十点开会", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z"},
			{ID: "2", Source: "android", SourceID: "n2", NotificationUID: 2, SourceEventID: "evt-2", AppIdentifier: "com.delivery", Title: "包裹", Message: "明天送达", Event: "added", ReceivedAt: "2026-08-21T00:01:00Z"},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "results":[
    {"context_id":"1","proposal":{"actions":[{"action":"ignore"}]}},
    {"context_id":"2","proposal":{"actions":[{"action":"ignore"}]}}
  ]
}`}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)
	if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatalf("ProcessBatch() error = %v", err)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want one call for the notification batch", got)
	}
	if got := ctxStore.State().MemoryCursor; got != "2" {
		t.Fatalf("MemoryCursor=%q, want 2", got)
	}
}

func TestNotificationMemoryBenchmarkPathSeedsProcessesAndExposesDerivedMemory(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "results":[
    {"context_id":"1","proposal":{"actions":[{"action":"add","scope":"temporary","type":"fact","title":"包裹更新","content":"包裹明天送达","tags":["delivery"]}]}}
  ]
}`}}
	plane := NewFilesystemMemoryPlane(root, DefaultMemoryExtractionConfig(), nil)
	if err := plane.StartMemoryWorker(model); err != nil {
		t.Fatalf("StartMemoryWorker() error = %v", err)
	}
	defer plane.StopEpisodeMemory()
	defer plane.StopNotificationMemory()

	records, err := plane.SeedNotificationMemoryForBenchmark(context.Background(), []ble.NotificationEvent{{
		ID: "fixture-1", Source: "android", SourceID: "notification-1",
		SourceEventID: "event-1", AppIdentifier: "com.delivery",
		Title: "包裹更新", Message: "明天送达", Event: "added",
		ReceivedAt: "2026-08-21T00:00:00Z",
	}})
	if err != nil {
		t.Fatalf("SeedNotificationMemoryForBenchmark() error = %v", err)
	}
	if len(records) != 1 || records[0].ContextID != "1" {
		t.Fatalf("seeded records = %#v, want one context_id=1 record", records)
	}
	cursor, memoryIDs, err := plane.ProcessNotificationMemoryNow(context.Background())
	if err != nil {
		t.Fatalf("ProcessNotificationMemoryNow() error = %v", err)
	}
	if cursor != "1" {
		t.Fatalf("memory cursor = %q, want 1", cursor)
	}
	if len(memoryIDs) != 1 || memoryIDs[0] != "tmp_notification_1" {
		t.Fatalf("derived memory IDs = %#v, want tmp_notification_1", memoryIDs)
	}
	if got := model.callCount(); got != 1 {
		t.Fatalf("model calls = %d, want one call", got)
	}
	plane.notificationProcessor.context.mu.Lock()
	consumeDisabled := plane.notificationProcessor.context.consumeDisabled
	plane.notificationProcessor.context.mu.Unlock()
	if consumeDisabled {
		t.Fatal("benchmark processing left live notification consumption disabled")
	}
	raw, err := os.ReadFile(filepath.Join(root, "notifications", "events", "2026-08-21.jsonl"))
	if err != nil {
		t.Fatalf("read raw notification fixture: %v", err)
	}
	if !strings.Contains(string(raw), `"message":"明天送达"`) {
		t.Fatalf("raw notification log did not preserve original message: %s", raw)
	}
}

func TestNotificationMemoryBenchmarkPathDrainsMultipleBatches(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	events := make([]ble.NotificationEvent, 0, 11)
	for index := 1; index <= 11; index++ {
		events = append(events, ble.NotificationEvent{
			ID: strconv.Itoa(index), Source: "android", SourceID: "calendar-batch-" + strconv.Itoa(index),
			SourceEventID: "calendar-batch-event-" + strconv.Itoa(index), DeviceID: "benchmark-device",
			NotificationUID: uint32(200 + index), AppIdentifier: "com.calendar",
			Title: "Calendar update", Message: "Meeting update " + strconv.Itoa(index), Event: "added",
			ReceivedAt: "2026-08-21T01:00:00Z",
		})
	}
	model := &episodeMemoryScriptedModel{responses: []string{
		notificationIgnoreBatchResponse(events[:10]),
		`{"results":[{"context_id":"11","proposal":{"actions":[{"action":"ignore"}]}}]}`,
	}}
	plane := NewFilesystemMemoryPlane(root, DefaultMemoryExtractionConfig(), nil)
	if err := plane.StartMemoryWorker(model); err != nil {
		t.Fatalf("StartMemoryWorker() error = %v", err)
	}
	defer plane.StopEpisodeMemory()
	defer plane.StopNotificationMemory()
	if records, err := plane.SeedNotificationMemoryForBenchmark(context.Background(), events); err != nil || len(records) != len(events) {
		t.Fatalf("seeded records=%d err=%v, want %d records", len(records), err, len(events))
	}
	cursor, memoryIDs, err := plane.ProcessNotificationMemoryNow(context.Background())
	if err != nil {
		t.Fatalf("ProcessNotificationMemoryNow() error = %v", err)
	}
	if cursor != "11" || len(memoryIDs) != 0 {
		t.Fatalf("cursor=%q memoryIDs=%#v, want cursor 11 and no memories", cursor, memoryIDs)
	}
	if got := model.callCount(); got != 2 {
		t.Fatalf("model calls=%d, want two model calls for two processor batches", got)
	}
}

func TestNotificationMemoryBenchmarkPathTracksProvenanceBeyondFirstHundredPendingRecords(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	events := make([]ble.NotificationEvent, 0, 101)
	responses := make([]string, 0, 11)
	for index := 1; index <= 101; index++ {
		events = append(events, ble.NotificationEvent{
			ID: strconv.Itoa(index), Source: "android", SourceID: "backlog-" + strconv.Itoa(index),
			SourceEventID: "backlog-event-" + strconv.Itoa(index), AppIdentifier: "com.backlog",
			Title: "Backlog", Message: "Backlog item " + strconv.Itoa(index), Event: "added",
			ReceivedAt: "2026-08-21T01:00:00Z",
		})
	}
	for start := 1; start <= 100; start += 10 {
		results := make([]notificationMemoryBatchResult, 0, 10)
		for contextID := start; contextID < start+10; contextID++ {
			results = append(results, notificationMemoryBatchResult{
				ContextID: strconv.Itoa(contextID),
				Proposal:  notificationMemoryProposal{Actions: []notificationMemoryAction{{Action: "ignore"}}},
			})
		}
		data, _ := json.Marshal(notificationMemoryBatchResponse{Results: results})
		responses = append(responses, string(data))
	}
	responses = append(responses, `{"results":[{"context_id":"101","proposal":{"actions":[{"action":"add","scope":"temporary","type":"fact","content":"Backlog item 101","tags":["backlog"]}]}}]}`)
	model := &episodeMemoryScriptedModel{responses: responses}
	plane := NewFilesystemMemoryPlane(root, DefaultMemoryExtractionConfig(), nil)
	if err := plane.StartMemoryWorker(model); err != nil {
		t.Fatalf("StartMemoryWorker() error = %v", err)
	}
	defer plane.StopEpisodeMemory()
	defer plane.StopNotificationMemory()
	if records, err := plane.notificationProcessor.context.seedForBenchmark(context.Background(), events); err != nil || len(records) != len(events) {
		t.Fatalf("seeded records=%d err=%v, want %d", len(records), err, len(events))
	}
	cursor, memoryIDs, err := plane.ProcessNotificationMemoryNow(context.Background())
	if err != nil {
		t.Fatalf("ProcessNotificationMemoryNow() error = %v", err)
	}
	if cursor != "101" || len(memoryIDs) != 1 || memoryIDs[0] != "tmp_notification_101" {
		t.Fatalf("cursor=%q memoryIDs=%#v", cursor, memoryIDs)
	}
}

func TestProcessNotificationMemoryNowReturnsBusyAfterRetryExhaustion(t *testing.T) {
	root := filepath.Join(t.TempDir(), "memory")
	model := &episodeMemoryBlockingModel{
		inner:   &episodeMemoryScriptedModel{responses: []string{`{"results":[{"context_id":"1","proposal":{"actions":[{"action":"ignore"}]}}]}`}},
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	plane := NewFilesystemMemoryPlane(root, DefaultMemoryExtractionConfig(), nil)
	if err := plane.StartNotificationMemory(model); err != nil {
		t.Fatalf("StartNotificationMemory() error = %v", err)
	}
	defer plane.StopNotificationMemory()
	if _, err := plane.SeedNotificationMemoryForBenchmark(context.Background(), []ble.NotificationEvent{{
		Source: "android", SourceEventID: "busy-event", Title: "Busy", Event: "added",
		ReceivedAt: "2026-08-21T00:00:00Z",
	}}); err != nil {
		t.Fatal(err)
	}
	backgroundDone := make(chan error, 1)
	plane.notificationProcessor.context.setConsumeDisabledForBenchmark(true)
	go func() {
		_, err := plane.memoryWorker.ProcessProcessorNow(context.Background(), plane.notificationProcessor)
		backgroundDone <- err
	}()
	select {
	case <-model.started:
	case <-time.After(time.Second):
		t.Fatal("background notification batch did not start")
	}
	if _, _, err := plane.processNotificationMemoryNow(context.Background(), 2, 0); !errors.Is(err, errMemoryWorkerBusy) {
		t.Fatalf("processNotificationMemoryNow() error = %v, want %v", err, errMemoryWorkerBusy)
	}
	close(model.release)
	_ = <-backgroundDone
	plane.notificationProcessor.context.setConsumeDisabledForBenchmark(false)
}

func TestNotificationMemoryProcessorUsesTenRecordBatches(t *testing.T) {
	root := t.TempDir()
	events := make([]ble.NotificationEvent, 0, 11)
	for index := 1; index <= 11; index++ {
		id := strconv.Itoa(index)
		events = append(events, ble.NotificationEvent{
			ID: id, Source: "android", SourceID: "n" + id, NotificationUID: uint32(index), SourceEventID: "evt-" + id,
			AppIdentifier: "com.calendar", Title: "会议", Message: "待处理日程", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z",
		})
	}
	ctxStore, err := NewNotificationContext(root, func(_ context.Context, since, _ string, limit int) (ble.EventPage, error) {
		start, _ := strconv.Atoi(since)
		if start >= len(events) {
			return ble.EventPage{Generation: "g", LastID: strconv.Itoa(len(events))}, nil
		}
		end := min(start+limit, len(events))
		return ble.EventPage{Generation: "g", Events: events[start:end], LastID: strconv.Itoa(end)}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	response := func(first, last int) string {
		results := make([]notificationMemoryBatchResult, 0, last-first+1)
		for index := first; index <= last; index++ {
			results = append(results, notificationMemoryBatchResult{
				ContextID: strconv.Itoa(index),
				Proposal:  notificationMemoryProposal{Actions: []notificationMemoryAction{{Action: "ignore"}}},
			})
		}
		data, _ := json.Marshal(notificationMemoryBatchResponse{Results: results})
		return string(data)
	}
	model := &episodeMemoryScriptedModel{responses: []string{response(1, 10), response(11, 11)}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)

	result, err := processor.ProcessBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("ProcessBatch(first) error = %v", err)
	}
	if !result.HasPending || ctxStore.State().MemoryCursor != "10" || model.callCount() != 1 {
		t.Fatalf("first batch result=%#v cursor=%q calls=%d, want pending cursor 10 and one call", result, ctxStore.State().MemoryCursor, model.callCount())
	}
	result, err = processor.ProcessBatch(context.Background(), nil)
	if err != nil {
		t.Fatalf("ProcessBatch(second) error = %v", err)
	}
	if result.HasPending || ctxStore.State().MemoryCursor != "11" || model.callCount() != 2 {
		t.Fatalf("second batch result=%#v cursor=%q calls=%d, want drained cursor 11 and two calls", result, ctxStore.State().MemoryCursor, model.callCount())
	}
}

func TestNotificationMemoryProcessorSkipsInvalidSingleProposalAndAdvancesCursor(t *testing.T) {
	root := t.TempDir()
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1", AppIdentifier: "com.mail", Title: "会议", Message: "明天开会", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, &episodeMemoryScriptedModel{responses: []string{`{"actions":[{"action":"not-a-real-action","scope":"temporary"}]}`}})
	if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatalf("ProcessBatch() error=%v, want invalid record isolation", err)
	}
	if got := ctxStore.State().MemoryCursor; got != "1" {
		t.Fatalf("MemoryCursor=%q, want invalid record consumed", got)
	}
}

func TestNotificationMemoryProcessorRejectsBatchProtocolErrorWithoutAdvancingCursor(t *testing.T) {
	root := t.TempDir()
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{
			ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1",
			AppIdentifier: "com.mail", Title: "会议", Message: "明天开会", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z",
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, &episodeMemoryScriptedModel{responses: []string{
		`{"results":[{"context_id":"unknown","proposal":{"actions":[{"action":"ignore"}]}}]}`,
	}})
	_, err = processor.ProcessBatch(context.Background(), nil)
	var proposalErr *notificationProposalError
	if !errors.As(err, &proposalErr) || !strings.Contains(err.Error(), "unknown context_id") {
		t.Fatalf("ProcessBatch() error=%v, want batch protocol proposal error", err)
	}
	if got := ctxStore.State().MemoryCursor; got != "" {
		t.Fatalf("MemoryCursor=%q advanced after batch protocol error", got)
	}
}

func TestNotificationMemoryProcessorRetriesMalformedJSONAndAdvancesCursor(t *testing.T) {
	tests := []struct {
		name      string
		malformed string
	}{
		{name: "prose response", malformed: `I cannot produce that response.`},
		{name: "invalid object suffix", malformed: `{"results":[]A}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			root := t.TempDir()
			ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
				return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{
					ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1",
					AppIdentifier: "com.mail", Title: "会议", Message: "明天开会", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z",
				}}}, nil
			})
			if err != nil {
				t.Fatal(err)
			}
			model := &episodeMemoryScriptedModel{responses: []string{
				tt.malformed,
				`{"results":[{"context_id":"1","proposal":{"actions":[{"action":"ignore"}]}}]}`,
			}}
			processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)

			if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
				t.Fatalf("ProcessBatch() error=%v, want malformed JSON recovery", err)
			}
			if got := model.callCount(); got != 2 {
				t.Fatalf("model calls=%d, want one correction retry", got)
			}
			if got := ctxStore.State().MemoryCursor; got != "1" {
				t.Fatalf("MemoryCursor=%q, want corrected batch committed", got)
			}
			model.mu.Lock()
			retryMessages := append([]llms.MessageContent(nil), model.calls[1]...)
			model.mu.Unlock()
			if !episodeMemoryMessagesContain(retryMessages, "previous response was invalid JSON") {
				t.Fatal("correction retry did not explain the JSON failure")
			}
		})
	}
}

func TestNotificationMemoryProcessorMalformedJSONRetryExhaustionKeepsCursor(t *testing.T) {
	root := t.TempDir()
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{
			ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1",
			AppIdentifier: "com.mail", Title: "会议", Message: "明天开会", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z",
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`I cannot produce JSON.`, `{"results":[]A}`}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)

	_, err = processor.ProcessBatch(context.Background(), nil)
	var proposalErr *notificationProposalError
	if !errors.As(err, &proposalErr) || !strings.Contains(err.Error(), "parse notification memory batch proposal") {
		t.Fatalf("ProcessBatch() error=%v, want malformed batch proposal error", err)
	}
	if got := model.callCount(); got != 2 {
		t.Fatalf("model calls=%d, want exactly one correction retry", got)
	}
	if got := ctxStore.State().MemoryCursor; got != "" {
		t.Fatalf("MemoryCursor=%q advanced after retry exhaustion", got)
	}
}

func TestNotificationMemoryResponseFailureUsesFallbackLogger(t *testing.T) {
	var output bytes.Buffer
	restoreOutput := logging.SetOutput(&output)
	defer restoreOutput()
	restoreLevel := logging.SetMinimumLevel(logging.Debug)
	defer restoreLevel()

	raw := `{"sensitive":"otp-123456",A}`
	var decoded any
	parseErr := json.Unmarshal([]byte(raw), &decoded)
	if parseErr == nil {
		t.Fatal("malformed response unexpectedly parsed")
	}
	var syntaxErr *json.SyntaxError
	if !errors.As(parseErr, &syntaxErr) {
		t.Fatalf("parse error=%v, want json.SyntaxError", parseErr)
	}

	var processor *NotificationMemoryProcessor
	processor.logNotificationMemoryResponseFailure("batch proposal parse failed", raw, parseErr, 1)

	logged := output.String()
	if strings.Contains(logged, "otp-123456") || strings.Contains(logged, raw) {
		t.Fatalf("failure log persisted model response body: %q", logged)
	}
	for _, want := range []string{
		"batch proposal parse failed",
		"attempt=1/2",
		"response_bytes=" + strconv.Itoa(len(raw)),
		"response_sha256=",
		"json_offset=" + strconv.FormatInt(syntaxErr.Offset, 10),
	} {
		if !strings.Contains(logged, want) {
			t.Fatalf("failure log=%q, want %q", logged, want)
		}
	}
}

func TestNotificationMemoryProcessorRetainsDurablePendingAfterConsumeFailureWithoutModel(t *testing.T) {
	root := t.TempDir()
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{
			ID: "1", Source: "android", SourceEventID: "evt-1", AppIdentifier: "com.calendar",
			Title: "会议", Message: "明天十点", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z",
		}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ctxStore.Consume(context.Background(), 10); err != nil {
		t.Fatal(err)
	}
	consumeErr := errors.New("ble unavailable")
	ctxStore.reader = func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{}, consumeErr
	}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, nil)
	result, err := processor.ProcessBatch(context.Background(), nil)
	if !errors.Is(err, consumeErr) || !result.HasPending {
		t.Fatalf("ProcessBatch() result=%#v err=%v, want pending consume error", result, err)
	}
	if got := ctxStore.State().MemoryCursor; got != "" {
		t.Fatalf("MemoryCursor=%q, want durable backlog retained for a configured model", got)
	}
}

func TestNotificationMemoryProcessorAppliesValidRecordsAroundInvalidSibling(t *testing.T) {
	root := t.TempDir()
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{
			{ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1", AppIdentifier: "com.calendar", Title: "会议", Message: "明天十点开会", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z"},
			{ID: "2", Source: "android", SourceID: "n2", NotificationUID: 2, SourceEventID: "evt-2", AppIdentifier: "com.delivery", Title: "包裹", Message: "明天送达", Event: "added", ReceivedAt: "2026-08-21T00:01:00Z"},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "results":[
    {"context_id":"1","proposal":{"actions":[{"action":"add","scope":"temporary","type":"fact","content":"明天十点开会"}]}},
    {"context_id":"2","proposal":{"actions":[{"action":"invalid","scope":"temporary"}]}}
  ]
}`}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)
	if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatalf("ProcessBatch() error=%v, want invalid record isolation", err)
	}
	if got := ctxStore.State().MemoryCursor; got != "2" {
		t.Fatalf("MemoryCursor=%q, want batch committed", got)
	}
	memories, err := processor.temporary.Search(context.Background(), MemoryQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].Content != "明天十点开会" {
		t.Fatalf("valid sibling was not applied: %#v", memories)
	}
}

func TestNotificationMemoryProcessorKeepsLatestActionForRepeatedBatchTarget(t *testing.T) {
	root := t.TempDir()
	temporary := NewLongTermMemoryStore(filepath.Join(root, "temporary"))
	if _, err := temporary.AddMemory(context.Background(), MemoryItem{
		ID:               "tmp_shared",
		Type:             "fact",
		TimeScope:        "temporary",
		Title:            "共享日程",
		Content:          "原始内容",
		Tags:             []string{"notification", "com.calendar"},
		EvidenceExcerpts: []string{"原始通知"},
	}); err != nil {
		t.Fatal(err)
	}
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{
			{ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1", AppIdentifier: "com.calendar", Title: "会议", Message: "改到十点", Event: "modified", ReceivedAt: "2026-08-21T00:00:00Z"},
			{ID: "2", Source: "android", SourceID: "n2", NotificationUID: 2, SourceEventID: "evt-2", AppIdentifier: "com.calendar", Title: "会议", Message: "改到十一点", Event: "modified", ReceivedAt: "2026-08-21T00:01:00Z"},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "results":[
    {"context_id":"1","proposal":{"actions":[{"action":"update","scope":"temporary","memory_id":"tmp_shared","memory_revision":1,"type":"fact","content":"改到十点"}]}},
    {"context_id":"2","proposal":{"actions":[{"action":"update","scope":"temporary","memory_id":"tmp_shared","memory_revision":1,"type":"fact","content":"改到十一点"}]}}
  ]
}`}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)

	if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatalf("ProcessBatch() error=%v", err)
	}
	if got := ctxStore.State().MemoryCursor; got != "2" {
		t.Fatalf("MemoryCursor=%q, want 2", got)
	}
	memories, err := temporary.Search(context.Background(), MemoryQuery{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(memories) != 1 || memories[0].Revision != 2 || memories[0].Content != "改到十一点" {
		t.Fatalf("memory did not keep the latest batch action: %#v", memories)
	}
}

func TestNotificationMemoryProcessorCoalescesBeforeValidatingStaleEarlierAction(t *testing.T) {
	root := t.TempDir()
	temporary := NewLongTermMemoryStore(filepath.Join(root, "temporary"))
	if _, err := temporary.AddMemory(context.Background(), MemoryItem{
		ID: "tmp_shared", Type: "fact", TimeScope: "temporary", Title: "共享日程", Content: "原始内容",
		Tags: []string{"notification", "com.calendar"}, EvidenceExcerpts: []string{"原始通知"},
	}); err != nil {
		t.Fatal(err)
	}
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{
			{ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1", AppIdentifier: "com.calendar", Title: "会议", Message: "旧修改", Event: "modified", ReceivedAt: "2026-08-21T00:00:00Z"},
			{ID: "2", Source: "android", SourceID: "n2", NotificationUID: 2, SourceEventID: "evt-2", AppIdentifier: "com.calendar", Title: "会议", Message: "新修改", Event: "modified", ReceivedAt: "2026-08-21T00:01:00Z"},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "results":[
    {"context_id":"1","proposal":{"actions":[{"action":"update","scope":"temporary","memory_id":"tmp_shared","memory_revision":99,"type":"fact","content":"旧修改"}]}},
    {"context_id":"2","proposal":{"actions":[{"action":"UPDATE","scope":" TEMPORARY ","memory_id":"tmp_shared","memory_revision":1,"type":"fact","content":"新修改"}]}}
  ]
}`}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)
	if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatalf("ProcessBatch() error=%v", err)
	}
	memories, err := temporary.Search(context.Background(), MemoryQuery{Limit: 10})
	if err != nil || len(memories) != 1 || memories[0].Content != "新修改" || memories[0].Revision != 2 {
		t.Fatalf("memories=%#v err=%v, want latest action applied", memories, err)
	}
}

func TestNotificationMemoryProcessorSkipsInvalidRecordAndProcessesValidBatchResults(t *testing.T) {
	root := t.TempDir()
	temporary := NewLongTermMemoryStore(filepath.Join(root, "temporary"))
	if _, err := temporary.AddMemory(context.Background(), MemoryItem{
		ID: "tmp_shared", Type: "fact", TimeScope: "temporary", Title: "共享日程", Content: "原始内容",
		Tags: []string{"notification", "com.calendar"}, EvidenceExcerpts: []string{"原始通知"},
	}); err != nil {
		t.Fatal(err)
	}
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{
			{ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1", AppIdentifier: "com.calendar", Title: "会议", Message: "旧修改", Event: "modified", ReceivedAt: "2026-08-21T00:00:00Z"},
			{ID: "2", Source: "android", SourceID: "n2", NotificationUID: 2, SourceEventID: "evt-2", AppIdentifier: "com.calendar", Title: "会议", Message: "新修改", Event: "modified", ReceivedAt: "2026-08-21T00:01:00Z"},
		}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &episodeMemoryScriptedModel{responses: []string{`{
  "results":[
    {"context_id":"1","proposal":{"actions":[
      {"action":"update","scope":"temporary","memory_id":"tmp_shared","memory_revision":99,"type":"fact","content":"旧修改"},
      {"action":"add","scope":"temporary","type":"fact","content":"不应写入"}
    ]}},
    {"context_id":"2","proposal":{"actions":[{"action":"update","scope":"temporary","memory_id":"tmp_shared","memory_revision":1,"type":"fact","content":"新修改"}]}}
  ]
}`}}
	processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)
	if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatalf("ProcessBatch() error=%v, want invalid record isolation", err)
	}
	if got := ctxStore.State().MemoryCursor; got != "2" {
		t.Fatalf("MemoryCursor=%q, want valid batch results committed", got)
	}
	memories, err := temporary.Search(context.Background(), MemoryQuery{Limit: 10})
	if err != nil || len(memories) != 1 || memories[0].Content != "新修改" || memories[0].Revision != 2 {
		t.Fatalf("memories=%#v err=%v, want valid record applied", memories, err)
	}
}

func TestNotificationMemoryProcessorUpdatesWithRevisionAndCanPromote(t *testing.T) {
	root := t.TempDir()
	temporary := NewLongTermMemoryStore(filepath.Join(root, "temporary"))
	longTerm := NewLongTermMemoryStore(filepath.Join(root, "long_term"))
	if _, err := temporary.AddMemory(context.Background(), MemoryItem{ID: "tmp_existing", Type: "fact", TimeScope: "temporary", Priority: 90, Confidence: 0.95, Title: "包裹", Content: "包裹今天送达", Tags: []string{"notification", "com.delivery"}, EvidenceExcerpts: []string{"旧通知"}}); err != nil {
		t.Fatal(err)
	}
	ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{ID: "1", Source: "android", SourceID: "n1", NotificationUID: 1, SourceEventID: "evt-1", AppIdentifier: "com.delivery", Title: "包裹更新", Message: "包裹改为明天送达", Event: "modified", ReceivedAt: "2026-08-21T00:00:00Z"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	responses := []string{`{"actions":[{"action":"update","scope":"temporary","memory_id":"tmp_existing","memory_revision":1,"type":"fact","title":"包裹","content":"包裹改为明天送达","confidence":0.82,"expires_at":"2099-01-01T00:00:00Z"}]}`}
	processor := NewNotificationMemoryProcessor(ctxStore, root, longTerm, &episodeMemoryScriptedModel{responses: responses})
	if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	updated, err := temporary.Search(context.Background(), MemoryQuery{Tags: []string{"com.delivery"}, Limit: 5})
	if err != nil || len(updated) != 1 || updated[0].Revision != 2 || updated[0].Content != "包裹改为明天送达" || updated[0].Priority != 40 || updated[0].Confidence != 0.82 {
		t.Fatalf("updated=%#v err=%v", updated, err)
	}

	// Exercise the same processor seam for a temporary -> long-term promotion.
	ctxStore2, err := NewNotificationContext(filepath.Join(root, "promote"), func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{Generation: "g", Events: []ble.NotificationEvent{{ID: "1", Source: "android", SourceID: "n2", NotificationUID: 2, SourceEventID: "evt-2", AppIdentifier: "com.delivery", Title: "稳定规则", Message: "以后统一寄到公司", Event: "added", ReceivedAt: "2026-08-21T00:00:00Z"}}}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.ApplyMemoryIntent(context.Background(), MemoryIntent{Item: MemoryItem{ID: "tmp_notification_1", Type: "rule", TimeScope: "temporary", Priority: 90, Confidence: 0.91, Title: "稳定规则", Content: "以后统一寄到公司", Tags: []string{"notification", "com.delivery"}, SourceRefs: []MemorySourceRef{{Type: "notification", ID: "1", EventIDs: []string{"event-1"}}}, EvidenceExcerpts: []string{"通知"}}, Action: MemoryIntentActionCreate}); err != nil {
		t.Fatal(err)
	}
	processor2 := NewNotificationMemoryProcessor(ctxStore2, root, longTerm, &episodeMemoryScriptedModel{responses: []string{`{"actions":[{"action":"promote","scope":"long_term","memory_id":"tmp_notification_1","memory_revision":1}]}`}})
	if _, err := processor2.ProcessBatch(context.Background(), nil); err != nil {
		t.Fatal(err)
	}
	if promoted, err := longTerm.Search(context.Background(), MemoryQuery{Tags: []string{"com.delivery"}, Limit: 5}); err != nil || len(promoted) == 0 || !strings.Contains(promoted[0].Content, "公司") || promoted[0].Priority != 90 || promoted[0].Confidence != 0.91 {
		t.Fatalf("promoted=%#v err=%v", promoted, err)
	}
	if active, err := temporary.Search(context.Background(), MemoryQuery{Tags: []string{"com.delivery"}, Limit: 10}); err != nil {
		t.Fatal(err)
	} else {
		for _, item := range active {
			if item.ID == "tmp_notification_1" {
				t.Fatalf("promoted temporary memory still active: %#v", item)
			}
		}
	}
}

func TestNotificationMemoryPromoteRetryAfterTemporaryRemovalConverges(t *testing.T) {
	root := t.TempDir()
	temporary := NewLongTermMemoryStore(filepath.Join(root, "temporary"))
	longTerm := NewLongTermMemoryStore(filepath.Join(root, "long_term"))
	record := NotificationRecord{ContextID: "retry", NotificationEvent: ble.NotificationEvent{ID: "event-retry", Source: "android", SourceEventID: "source-retry", AppIdentifier: "com.delivery", Title: "Delivery rule", Message: "Always deliver to the office"}}
	temporaryItem := MemoryItem{ID: "tmp_notification_retry", Type: "rule", TimeScope: "temporary", Title: "Delivery rule", Content: "Always deliver to the office", Tags: []string{"notification", "com.delivery"}, SourceRefs: []MemorySourceRef{{Type: "notification", ID: "original", EventIDs: []string{"original-event"}}}, EvidenceExcerpts: []string{"Always deliver to the office"}}
	if _, err := temporary.ApplyMemoryIntent(context.Background(), MemoryIntent{Item: temporaryItem, Action: MemoryIntentActionCreate}); err != nil {
		t.Fatal(err)
	}
	processor := NewNotificationMemoryProcessor(nil, root, longTerm, nil)
	proposal := notificationMemoryProposal{Actions: []notificationMemoryAction{{Action: "promote", Scope: "long_term", MemoryID: temporaryItem.ID, MemoryRevision: 1, SourceEventIDs: []string{"source-retry"}}}}
	activeRef := memoryResultMergeReference("temporary", MemoryResult{ID: temporaryItem.ID, Type: temporaryItem.Type, Status: "active", Revision: 1, Title: temporaryItem.Title, Summary: temporaryItem.Content, Content: temporaryItem.Content, Tags: temporaryItem.Tags, SourceRefs: temporaryItem.SourceRefs})
	validated, err := processor.validateNotificationProposal(context.Background(), record, proposal, []MemoryMergeReference{activeRef})
	if err != nil {
		t.Fatalf("validate initial promote: %v", err)
	}
	if err := processor.applyNotificationProposal(context.Background(), record, validated, []MemoryMergeReference{activeRef}); err != nil {
		t.Fatalf("apply initial promote: %v", err)
	}
	validated, err = processor.validateNotificationProposal(context.Background(), record, proposal, nil)
	if err != nil {
		t.Fatalf("validate retry promote: %v", err)
	}
	if err := processor.applyNotificationProposal(context.Background(), record, validated, nil); err != nil {
		t.Fatalf("apply retry promote: %v", err)
	}
	promoted, err := longTerm.Search(context.Background(), MemoryQuery{Tags: []string{"com.delivery"}, Limit: 10})
	if err != nil || len(promoted) != 1 {
		t.Fatalf("promoted=%#v err=%v", promoted, err)
	}
	if !memorySourceRefsContain(promoted[0].SourceRefs, []MemorySourceRef{{Type: "notification", ID: record.ContextID, EventIDs: []string{"source-retry"}}}) {
		t.Fatalf("promotion omitted retry evidence: %#v", promoted[0].SourceRefs)
	}
}

func TestNotificationMemoryPromoteRejectsUnrelatedDeletedTemporary(t *testing.T) {
	root := t.TempDir()
	temporary := NewLongTermMemoryStore(filepath.Join(root, "temporary"))
	processor := NewNotificationMemoryProcessor(nil, root, NewLongTermMemoryStore(filepath.Join(root, "long_term")), nil)
	item := MemoryItem{ID: "tmp_deleted", Type: "rule", TimeScope: "temporary", Content: "Do not restore me", SourceRefs: []MemorySourceRef{{Type: "notification", ID: "old", EventIDs: []string{"old-event"}}}, EvidenceExcerpts: []string{"Do not restore me"}}
	if _, err := temporary.ApplyMemoryIntent(context.Background(), MemoryIntent{Item: item, Action: MemoryIntentActionCreate}); err != nil {
		t.Fatal(err)
	}
	if _, err := temporary.ApplyMemoryIntent(context.Background(), MemoryIntent{Item: MemoryItem{ID: item.ID}, Action: MemoryIntentActionRemove, ExpectedRevision: 1}); err != nil {
		t.Fatal(err)
	}
	record := NotificationRecord{ContextID: "new", NotificationEvent: ble.NotificationEvent{ID: "new-event", SourceEventID: "new-event", Message: "Do not restore me"}}
	proposal := notificationMemoryProposal{Actions: []notificationMemoryAction{{Action: "promote", Scope: "long_term", MemoryID: item.ID, MemoryRevision: 1}}}
	if _, err := processor.validateNotificationProposal(context.Background(), record, proposal, nil); err == nil {
		t.Fatal("deleted temporary without a completed promotion was accepted")
	}
}

func TestNotificationMemoryRelatedSearchSkipsSensitiveMemories(t *testing.T) {
	root := t.TempDir()
	longTerm := NewLongTermMemoryStore(filepath.Join(root, "long_term"))
	if _, err := longTerm.AddMemory(context.Background(), MemoryItem{ID: "mem_bank_pin", Type: "fact", Priority: 80, Confidence: 0.9, Tags: []string{"com.bank"}, Title: "网银口令", Content: "用户的网银口令是 7316", Sensitive: true, EvidenceExcerpts: []string{"帮我记住"}}); err != nil {
		t.Fatal(err)
	}
	processor := NewNotificationMemoryProcessor(nil, root, longTerm, nil)
	record := NotificationRecord{ContextID: "bank", NotificationEvent: ble.NotificationEvent{ID: "bank-event", AppIdentifier: "com.bank", Title: "账单", Message: "本月账单 300 元"}}
	refs, err := processor.searchRelatedNotificationMemories(context.Background(), record)
	if err != nil {
		t.Fatal(err)
	}
	for _, ref := range refs {
		if ref.ID == "mem_bank_pin" {
			t.Fatalf("sensitive memory reached the notification prompt: %#v", ref)
		}
	}
}
