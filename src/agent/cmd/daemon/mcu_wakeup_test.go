package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"syscall"
	"testing"
	"time"

	"aico-agent/internal/agent"
	"aico-agent/internal/mcu"
)

type mcuWakeupEventsFunc func(context.Context, string, string, int, int) (*mcu.EventPage, error)

func (f mcuWakeupEventsFunc) EventsSince(ctx context.Context, since, generation string, limit, wait int) (*mcu.EventPage, error) {
	return f(ctx, since, generation, limit, wait)
}

func mcuASREvent(id string, at time.Time) mcu.HardwareEvent {
	return mcu.HardwareEvent{ID: id, Type: "wakeup", Source: "asr", SourceCode: 6, ReceivedAt: at.Format(time.RFC3339Nano)}
}

func TestMCUWakeupFiltersSourcesAndRetainsPaginationCursor(t *testing.T) {
	now := time.Now()
	s := newMCUWakeupSource(nil, filepath.Join(t.TempDir(), "cursor.json"))
	s.now = func() time.Time { return now }
	calls := 0
	page := &mcu.EventPage{Generation: "g1", NextID: "8", LastID: "20", Events: []mcu.HardwareEvent{
		{ID: "1", Type: "wakeup", Source: "touch_key", SourceCode: 3},
		{ID: "2", Type: "wakeup", Source: "rtc_timer", SourceCode: 4},
		{ID: "3", Type: "rk628_plugin_det_changed"},
		{ID: "4", Type: "wakeup", Source: "unknown", SourceCode: 99},
		mcuASREvent("5", now.Add(-mcuVoiceWakeupMaxAge-time.Second)),
		{ID: "6", Type: "wakeup", Source: "asr", SourceCode: 6},
		{ID: "7", Type: "wakeup", Source: "asr", SourceCode: 99, ReceivedAt: now.Format(time.RFC3339Nano)},
		mcuASREvent("8", now.Add(-time.Second)), // Retained boot wake.
	}}
	consume := func() {
		t.Helper()
		if err := s.consume(context.Background(), page, func() { calls++ }); err != nil {
			t.Fatal(err)
		}
	}
	consume()
	consume() // A replay of the same IDs must not repeat an action.
	if calls != 1 || s.cursor.ID != "8" {
		t.Fatalf("calls=%d cursor=%+v; want one fresh ASR and next_id=8", calls, s.cursor)
	}
	page = &mcu.EventPage{Generation: "g1", NextID: "9", LastID: "20", Events: []mcu.HardwareEvent{mcuASREvent("9", now)}}
	consume()
	if calls != 2 || s.cursor.ID != "9" {
		t.Fatalf("later limited page was skipped: calls=%d cursor=%+v", calls, s.cursor)
	}
}

func TestMCUWakeupTapSettingOnlyGatesTouchKey(t *testing.T) {
	now := time.Now()
	for _, tc := range []struct {
		name       string
		tapEnabled bool
		wantCalls  int
	}{
		{name: "disabled", wantCalls: 1},
		{name: "enabled", tapEnabled: true, wantCalls: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := newMCUWakeupSource(nil, filepath.Join(t.TempDir(), "cursor.json"))
			s.now = func() time.Time { return now }
			page := &mcu.EventPage{Generation: "g1", NextID: "2", Events: []mcu.HardwareEvent{
				{ID: "1", Type: "wakeup", Source: "touch_key", SourceCode: 3, ReceivedAt: now.Format(time.RFC3339Nano)},
				mcuASREvent("2", now),
			}}
			calls := 0
			if err := s.consumeWithTap(context.Background(), page, func() { calls++ }, tc.tapEnabled); err != nil {
				t.Fatal(err)
			}
			if calls != tc.wantCalls {
				t.Fatalf("calls=%d, want %d", calls, tc.wantCalls)
			}
		})
	}
}

func TestMCUWakeupReconnectResetAndCancellation(t *testing.T) {
	queries := make(chan mcuVoiceCursor, 8)
	events := make(chan string, 4)
	waiting := make(chan struct{})
	call := 0
	client := mcuWakeupEventsFunc(func(ctx context.Context, since, gen string, limit, wait int) (*mcu.EventPage, error) {
		queries <- mcuVoiceCursor{Generation: gen, ID: since}
		if limit != 50 || wait != 25000 {
			t.Errorf("long-poll bounds limit=%d wait=%d", limit, wait)
		}
		call++
		switch call {
		case 1:
			return nil, errors.New("service offline")
		case 2:
			return &mcu.EventPage{Generation: "g1", NextID: "1", LastID: "50", Events: []mcu.HardwareEvent{{ID: "1", Type: "rk628_plugin_det_changed"}}}, nil
		case 3:
			return &mcu.EventPage{Generation: "g1", NextID: "2", LastID: "50", Events: []mcu.HardwareEvent{mcuASREvent("2", time.Now())}}, nil
		case 4:
			return &mcu.EventPage{Generation: "g2", NextID: since, LastID: "1", ResetRequired: true}, nil
		case 5:
			return &mcu.EventPage{Generation: "g2", NextID: "1", LastID: "1", Truncated: true, Events: []mcu.HardwareEvent{mcuASREvent("1", time.Now())}}, nil
		default:
			close(waiting)
			<-ctx.Done()
			return nil, ctx.Err()
		}
	})
	s := newMCUWakeupSource(client, filepath.Join(t.TempDir(), "cursor.json"))
	s.retry = time.Millisecond
	w, _ := s.newWatcher(func() { events <- s.cursor.Generation + ":" + s.cursor.ID })
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(w.Stop)
	select {
	case <-waiting:
	case <-time.After(2 * time.Second):
		t.Fatal("subscriber did not reconnect and reach a pending long poll")
	}
	done := make(chan struct{})
	go func() { w.Stop(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the pending long poll")
	}
	w.Stop()
	if len(events) != 2 || <-events != "g1:2" || <-events != "g2:1" {
		t.Fatal("ASR events were lost or repeated across generation reset")
	}
	want := []mcuVoiceCursor{{ID: "0"}, {ID: "0"}, {Generation: "g1", ID: "1"}, {Generation: "g1", ID: "2"}, {Generation: "g2", ID: "0"}, {Generation: "g2", ID: "1"}}
	var got []mcuVoiceCursor
	for len(queries) > 0 {
		got = append(got, <-queries)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("queries=%+v, want %+v", got, want)
	}
}

func TestMCUWakeupReloadAndAgentRestartDoNotReplay(t *testing.T) {
	store := mcu.NewEventStore(10)
	defer store.Close()
	polls := make(chan mcuVoiceCursor, 16)
	client := mcuWakeupEventsFunc(func(ctx context.Context, since, gen string, limit, wait int) (*mcu.EventPage, error) {
		polls <- mcuVoiceCursor{Generation: gen, ID: since}
		p, err := store.Page(ctx, since, gen, limit, wait)
		return &p, err
	})
	path := filepath.Join(t.TempDir(), "cursor.json")
	source := newMCUWakeupSource(client, path)
	calls := 0
	for i := 1; i <= 3; i++ {
		if i < 3 {
			store.Append(mcuASREvent("", time.Now()))
		}
		if i == 3 {
			// A new source represents a new Agent process in the same boot.
			source = newMCUWakeupSource(client, path)
		}
		watcher, _ := source.newWatcher(func() { calls++ })
		if err := watcher.Start(); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(watcher.Stop)
		wantID := strconv.Itoa(i)
		if i == 3 {
			wantID = "2"
		}
		for {
			select {
			case q := <-polls:
				if q.ID != wantID {
					continue
				}
			case <-time.After(time.Second):
				t.Fatal("watcher did not advance to expected cursor")
			}
			break
		}
		watcher.Stop()
		if calls != min(i, 2) {
			t.Fatalf("iteration=%d callbacks=%d; prior events were replayed", i, calls)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cursor mcuVoiceCursor
	if err := json.Unmarshal(data, &cursor); err != nil || cursor.ID != "2" || cursor.Generation != store.Generation() {
		t.Fatalf("saved cursor=%s err=%v", data, err)
	}
	// The Agent's private cursor must not consume an application's history.
	page, err := store.Page(context.Background(), "0", "", 50, 0)
	if err != nil || len(page.Events) != 2 {
		t.Fatalf("application history was consumed: %+v %v", page, err)
	}
}

func TestMCUWakeupCheckpointFailureDoesNotTriggerVoice(t *testing.T) {
	dir := t.TempDir()
	blocker := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(blocker, []byte("x"), 0600); err != nil {
		t.Fatal(err)
	}
	s := newMCUWakeupSource(nil, filepath.Join(blocker, "cursor"))
	page := &mcu.EventPage{Generation: "g1", NextID: "1", Events: []mcu.HardwareEvent{mcuASREvent("1", time.Now())}}
	if err := s.consume(context.Background(), page, func() { t.Error("uncheckpointed event triggered voice") }); err == nil {
		t.Fatal("checkpoint error was ignored")
	}
	if s.cursor.ID != "0" {
		t.Fatal("failed checkpoint advanced the cursor")
	}
}

func TestMCUWakeupStartsExistingVoiceRecording(t *testing.T) {
	store := mcu.NewEventStore(10)
	defer store.Close()
	store.Append(mcu.HardwareEvent{Type: "wakeup", Source: "touch_key", SourceCode: 3})
	store.Append(mcuASREvent("", time.Now()))
	client := mcuWakeupEventsFunc(func(ctx context.Context, since, gen string, limit, wait int) (*mcu.EventPage, error) {
		page, err := store.Page(ctx, since, gen, limit, wait)
		return &page, err
	})
	source := newMCUWakeupSource(client, filepath.Join(t.TempDir(), "cursor.json"))
	signals := make(chan os.Signal, 1)
	dialog := &fakeAudioDialog{
		frameSamples: 2, chunks: []*agent.AudioChunkResult{{PCM: pcm16BytesFromSamples(100, 200)}},
		utterance: []int16{100, 200}, onUtterance: func() { signals <- syscall.SIGTERM },
	}
	done := make(chan struct{})
	go func() {
		runWakeupMode(agent.Config{}, dialog, nil, signals, source.newWatcher)
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		signals <- syscall.SIGTERM
		<-done
		t.Fatal("MCU ASR did not reach the voice runner")
	}
	if dialog.starts != 1 || len(dialog.utterances) != 1 || !opsHasPrefix(dialog.ops, []string{"interrupt_output", "start", "reset"}) {
		t.Fatalf("voice behavior: starts=%d ops=%v utterances=%d", dialog.starts, dialog.ops, len(dialog.utterances))
	}
}

func TestMCUWakeupRetainsBootTouchThroughSlowAgentStartup(t *testing.T) {
	for _, tc := range []struct {
		name        string
		age         time.Duration
		retryDelay  time.Duration
		savedCursor bool
		wantCalls   int
	}{
		{name: "observed boot delay", age: 53 * time.Second, wantCalls: 1},
		{name: "startup limit", age: 2 * time.Minute, wantCalls: 1},
		{name: "old boot history", age: 2*time.Minute + time.Second},
		{name: "wake during initial retry", age: 44 * time.Second, retryDelay: 45 * time.Second, wantCalls: 1},
		{name: "restart retains normal limit", age: 53 * time.Second, savedCursor: true},
		{name: "restart retry retains normal limit", age: 44 * time.Second, retryDelay: 45 * time.Second, savedCursor: true},
		{name: "fresh after restart", age: time.Second, savedCursor: true, wantCalls: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			now := time.Now()
			path := filepath.Join(t.TempDir(), "cursor.json")
			if tc.savedCursor {
				if err := os.WriteFile(path, []byte(`{"generation":"g1","id":"1"}`), 0600); err != nil {
					t.Fatal(err)
				}
			}
			drained := make(chan struct{})
			retried := false
			client := mcuWakeupEventsFunc(func(ctx context.Context, since, _ string, _, _ int) (*mcu.EventPage, error) {
				if tc.retryDelay > 0 && !retried {
					retried = true
					// The wake arrives after Start, while the initial request fails.
					now = now.Add(tc.retryDelay)
					return nil, errors.New("service offline during startup")
				}
				switch since {
				case "0":
					// The touch can be on a later page of boot history.
					return &mcu.EventPage{Generation: "g1", NextID: "1", LastID: "2", Events: []mcu.HardwareEvent{{ID: "1", Type: "rk628_plugin_det_changed"}}}, nil
				case "1":
					return &mcu.EventPage{Generation: "g1", NextID: "2", LastID: "2", Events: []mcu.HardwareEvent{
						{ID: "2", Type: "wakeup", Source: "touch_key", SourceCode: 3, ReceivedAt: now.Add(-tc.age).Format(time.RFC3339Nano)},
					}}, nil
				default:
					close(drained)
					<-ctx.Done()
					return nil, ctx.Err()
				}
			})
			source := newMCUWakeupSource(client, path)
			source.now = func() time.Time { return now }
			source.retry = time.Millisecond
			calls := 0
			watcher, _ := source.newWatcherWithTap(true, func() { calls++ })
			if err := watcher.Start(); err != nil {
				t.Fatal(err)
			}
			defer watcher.Stop()
			select {
			case <-drained:
			case <-time.After(2 * time.Second):
				t.Fatal("initial history did not drain")
			}
			watcher.Stop()
			if calls != tc.wantCalls || source.cursor.ID != "2" {
				t.Fatalf("calls=%d cursor=%+v, want calls=%d and cursor=2", calls, source.cursor, tc.wantCalls)
			}
			// Once caught up, a delayed runtime event must use the 30s limit.
			page := &mcu.EventPage{Generation: "g1", NextID: "3", LastID: "3", Events: []mcu.HardwareEvent{
				{ID: "3", Type: "wakeup", Source: "touch_key", SourceCode: 3, ReceivedAt: now.Add(-53 * time.Second).Format(time.RFC3339Nano)},
			}}
			if err := source.consumeWithTap(context.Background(), page, func() { t.Error("startup allowance leaked into runtime") }, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}
