package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"aico-agent/internal/ble"
)

func BenchmarkNotificationContextReadPending(b *testing.B) {
	root := filepath.Join(b.TempDir(), "memory")
	ctx, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
		return ble.EventPage{}, nil
	})
	if err != nil {
		b.Fatal(err)
	}
	events := make([]ble.NotificationEvent, 0, 100)
	for index := 1; index <= 100; index++ {
		events = append(events, ble.NotificationEvent{
			ID: strconv.Itoa(index), Source: "android", SourceID: fmt.Sprintf("notification-%d", index),
			SourceEventID: fmt.Sprintf("event-%d", index), AppIdentifier: "com.calendar",
			Title: "会议", Message: "待处理日程", Event: "added",
			ReceivedAt: time.Date(2026, 8, 21, 0, 0, index, 0, time.UTC).Format(time.RFC3339Nano),
		})
	}
	if _, err := ctx.seedForBenchmark(context.Background(), events); err != nil {
		b.Fatal(err)
	}
	b.ReportMetric(float64(len(events)), "events")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := ctx.ReadPending(context.Background(), 10); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkNotificationMemoryBatchPrompt(b *testing.B) {
	records := make([]NotificationRecord, 0, notificationMemoryBatchLimit)
	for index := 1; index <= notificationMemoryBatchLimit; index++ {
		records = append(records, NotificationRecord{
			ContextID: strconv.Itoa(index),
			NotificationEvent: ble.NotificationEvent{
				ID: strconv.Itoa(index), Source: "android",
				SourceEventID: fmt.Sprintf("event-%d", index),
				AppIdentifier: "com.calendar", Title: "会议",
				Message: "明天十点开会", Event: "added",
			},
		})
	}
	b.ReportMetric(float64(len(records)), "events")
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = buildNotificationMemoryBatchPrompt(records, nil)
	}
}

func BenchmarkNotificationMemoryProcessBatch(b *testing.B) {
	b.ReportMetric(float64(notificationMemoryBatchLimit), "events")
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		root := filepath.Join(b.TempDir(), strconv.Itoa(i))
		ctxStore, err := NewNotificationContext(root, func(context.Context, string, string, int) (ble.EventPage, error) {
			return ble.EventPage{}, nil
		})
		if err != nil {
			b.Fatal(err)
		}
		events := make([]ble.NotificationEvent, 0, notificationMemoryBatchLimit)
		for index := 1; index <= notificationMemoryBatchLimit; index++ {
			id := strconv.Itoa(index)
			events = append(events, ble.NotificationEvent{
				ID: id, Source: "android", SourceID: "notification-" + id,
				SourceEventID: "event-" + id, AppIdentifier: "com.calendar",
				Title: "会议", Message: "明天十点开会", Event: "added",
				ReceivedAt: "2026-08-21T00:00:00Z",
			})
		}
		if _, err := ctxStore.seedForBenchmark(context.Background(), events); err != nil {
			b.Fatal(err)
		}
		model := &episodeMemoryScriptedModel{responses: []string{notificationIgnoreBatchResponse(events)}}
		processor := NewNotificationMemoryProcessor(ctxStore, root, nil, model)
		processor.context.setConsumeDisabledForBenchmark(true)
		b.StartTimer()
		if _, err := processor.ProcessBatch(context.Background(), nil); err != nil {
			b.Fatal(err)
		}
		b.StopTimer()
		if got := model.callCount(); got != 1 {
			b.Fatalf("model calls = %d, want one", got)
		}
	}
}

func notificationIgnoreBatchResponse(events []ble.NotificationEvent) string {
	results := make([]notificationMemoryBatchResult, 0, len(events))
	for index := range events {
		results = append(results, notificationMemoryBatchResult{
			ContextID: strconv.Itoa(index + 1),
			Proposal: notificationMemoryProposal{
				Actions: []notificationMemoryAction{{Action: "ignore"}},
			},
		})
	}
	data, _ := json.Marshal(notificationMemoryBatchResponse{Results: results})
	return string(data)
}
