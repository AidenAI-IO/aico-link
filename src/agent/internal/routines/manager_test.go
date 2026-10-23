package routines

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aiden-agent/internal/mcu"
)

type testClock struct {
	mu sync.Mutex
	at time.Time
}

func (c *testClock) now() time.Time      { c.mu.Lock(); defer c.mu.Unlock(); return c.at }
func (c *testClock) add(d time.Duration) { c.mu.Lock(); c.at = c.at.Add(d); c.mu.Unlock() }

type fakeMCU struct {
	mu            sync.Mutex
	clock         *testClock
	until         time.Time
	sets          []int64
	cancels       int
	getErr        error
	setErr        error
	acceptLostSet bool
	pages         chan *mcu.EventPage
}

func (f *fakeMCU) GetState(context.Context) (*mcu.State, error) {
	return &mcu.State{Generation: "generation-1"}, nil
}
func (f *fakeMCU) GetWakeupTimer(context.Context) (*mcu.Timer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.getErr != nil {
		return nil, f.getErr
	}
	remaining := f.until.Sub(f.clock.now())
	return &mcu.Timer{Active: remaining > 0, RemainingS: uint32(max(0, (remaining+time.Second-1)/time.Second))}, nil
}
func (f *fakeMCU) SetWakeupTimer(_ context.Context, seconds int64, generation, id string) (*mcu.Timer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if generation == "" || id == "" {
		return nil, errors.New("missing MCU mutation identity")
	}
	f.sets = append(f.sets, seconds)
	if f.setErr == nil || f.acceptLostSet {
		f.until = f.clock.now().Add(time.Duration(seconds) * time.Second)
	}
	if f.setErr != nil {
		return nil, f.setErr
	}
	return &mcu.Timer{Active: true, RemainingS: uint32(seconds)}, nil
}
func (f *fakeMCU) CancelWakeupTimer(_ context.Context, generation, id string) (*mcu.Timer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if generation == "" || id == "" {
		return nil, errors.New("missing MCU mutation identity")
	}
	f.until = time.Time{}
	f.cancels++
	return &mcu.Timer{}, nil
}
func (f *fakeMCU) EventsSince(ctx context.Context, _, _ string, _, _ int) (*mcu.EventPage, error) {
	select {
	case page := <-f.pages:
		return page, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func newTestManager(t *testing.T, run Runner) (*Manager, *fakeMCU, *testClock) {
	t.Helper()
	clock := &testClock{at: time.Date(2026, 10, 22, 12, 0, 0, 0, time.UTC)}
	fake := &fakeMCU{clock: clock, pages: make(chan *mcu.EventPage, 8)}
	if run == nil {
		run = func(context.Context, Routine) (string, error) { return "done", nil }
	}
	m, err := Open(filepath.Join(t.TempDir(), "routines.json"), fake, run)
	if err != nil {
		t.Fatal(err)
	}
	m.now = clock.now
	t.Cleanup(m.Close)
	return m, fake, clock
}

func saveRoutine(t *testing.T, m *Manager, after time.Duration, interval int64) Routine {
	t.Helper()
	r, err := m.Save(context.Background(), Routine{Task: "Continue the original task", SessionID: "s_original", RunAt: m.now().Add(after), IntervalS: interval})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func wakePage(id string, at time.Time, source byte) *mcu.EventPage {
	name := "asr"
	if source == 4 {
		name = "rtc_timer"
	}
	return &mcu.EventPage{Generation: "generation-1", NextID: id, Events: []mcu.HardwareEvent{{ID: id, Type: "wakeup", Source: name, SourceCode: source, ReceivedAt: at.Format(time.RFC3339Nano)}}}
}

func waitRoutine(t *testing.T, m *Manager, id, status string) Routine {
	t.Helper()
	deadline := time.After(3 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		items, _ := m.List()
		for _, r := range items {
			if r.ID == id && r.Status == status {
				return r
			}
		}
		select {
		case <-deadline:
			t.Fatalf("routine %s did not become %s: %+v", id, status, items)
		case <-tick.C:
		}
	}
}

func TestRoutineMCUTimerMultiplexingAndRestart(t *testing.T) {
	m, fake, clock := newTestManager(t, nil)
	first := saveRoutine(t, m, 60*time.Second, 0)
	saveRoutine(t, m, 120*time.Second, 0)
	if len(fake.sets) != 1 || fake.sets[0] != 60 {
		t.Fatalf("sets = %v", fake.sets)
	}
	clock.add(10 * time.Second)
	resumed, err := Open(m.path, fake, m.run)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumed.now = clock.now
	if err := resumed.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.sets) != 1 {
		t.Fatalf("restart reset timer: %v", fake.sets)
	}
	if _, err := resumed.Cancel(context.Background(), first.ID); err != nil {
		t.Fatal(err)
	}
	// An earlier harmless wake may be retained, but cannot run the later task.
	clock.add(50 * time.Second)
	if err := resumed.consume(wakePage("1", clock.now(), 4)); err != nil {
		t.Fatal(err)
	}
	if err := resumed.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, _ := resumed.List()
	if items[1].Status != "scheduled" || fake.sets[len(fake.sets)-1] != 60 {
		t.Fatalf("items=%+v sets=%v", items, fake.sets)
	}
}

func TestRoutineWakeFiltersAndDurableDispatch(t *testing.T) {
	called := make(chan Routine, 4)
	m, _, clock := newTestManager(t, func(_ context.Context, r Routine) (string, error) { called <- r; return "continued", nil })
	r := saveRoutine(t, m, 10*time.Second, 0)
	clock.add(10 * time.Second)
	for _, page := range []*mcu.EventPage{wakePage("1", clock.now(), 6), wakePage("2", clock.now().Add(-time.Minute), 4)} {
		if err := m.consume(page); err != nil {
			t.Fatal(err)
		}
		items, _ := m.List()
		if items[0].Status != "scheduled" {
			t.Fatalf("unrelated/stale event queued work: %+v", items)
		}
	}
	page := wakePage("3", clock.now(), 4)
	if err := m.consume(page); err != nil {
		t.Fatal(err)
	}
	// Restart after enqueue, before execution: the same session and cursor survive.
	resumed, err := Open(m.path, m.mcu, m.run)
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumed.now = clock.now
	resumed.Start()
	got := waitRoutine(t, resumed, r.ID, "completed")
	if got.Result != "continued" || got.SessionID != "s_original" {
		t.Fatalf("result=%+v", got)
	}
	resumed.mu.Lock()
	err = resumed.consume(page)
	resumed.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	select {
	case call := <-called:
		if call.SessionID != r.SessionID {
			t.Fatal("wrong session")
		}
	default:
		t.Fatal("runner not called")
	}
	select {
	case <-called:
		t.Fatal("duplicate wake replayed completed run")
	default:
	}
}

func TestRoutineConfirmedExpiryRecoversLostWake(t *testing.T) {
	m, fake, clock := newTestManager(t, nil)
	saveRoutine(t, m, 10*time.Second, 0)
	clock.add(time.Minute)
	fake.getErr = errors.New("offline")
	if err := m.reconcile(context.Background()); err == nil {
		t.Fatal("expected MCU error")
	}
	items, _ := m.List()
	if items[0].Status != "scheduled" {
		t.Fatal("host clock alone dispatched work")
	}
	fake.getErr = nil
	if err := m.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	items, _ = m.List()
	if items[0].Status != "queued" {
		t.Fatalf("lost event not recovered: %+v", items)
	}
}

func TestRoutineLongDeadlineUsesMCUHops(t *testing.T) {
	m, fake, clock := newTestManager(t, nil)
	saveRoutine(t, m, 10*24*time.Hour, 0)
	if fake.sets[0] != maxDelay {
		t.Fatalf("sets=%v", fake.sets)
	}
	clock.add(24 * time.Hour)
	if err := m.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(fake.sets) != 1 {
		t.Fatal("reconciliation extended seven-day hop")
	}
	clock.add(6 * 24 * time.Hour)
	if err := m.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if fake.sets[1] != 3*24*3600 {
		t.Fatalf("sets=%v", fake.sets)
	}
	items, _ := m.List()
	if items[0].Status != "scheduled" {
		t.Fatal("hop ran task early")
	}
}

func TestRoutineLostSetResponseDoesNotResetCountdown(t *testing.T) {
	m, fake, clock := newTestManager(t, nil)
	fake.setErr, fake.acceptLostSet = errors.New("response lost"), true
	saveRoutine(t, m, time.Minute, 0)
	_, timerErr := m.List()
	if timerErr == "" {
		t.Fatal("uncertain arming must be visible")
	}
	fake.setErr = nil
	clock.add(10 * time.Second)
	m.reconcileLocked(context.Background())
	if len(fake.sets) != 1 {
		t.Fatalf("lost response reset countdown: %v", fake.sets)
	}
	_, timerErr = m.List()
	if timerErr != "" {
		t.Fatal(timerErr)
	}
}

func TestRoutineRepeatsWithoutCatchupBurstAndCanCancel(t *testing.T) {
	m, fake, clock := newTestManager(t, nil)
	r := saveRoutine(t, m, 10*time.Second, 30)
	clock.add(110 * time.Second)
	m.Start()
	// Expiry reconciliation should execute once, then retain the original cadence.
	deadline := time.After(3 * time.Second)
	for {
		items, _ := m.List()
		if items[0].Status == "scheduled" && !items[0].LastRunAt.IsZero() {
			if want := r.RunAt.Add(120 * time.Second); !items[0].RunAt.Equal(want) {
				t.Fatalf("next=%v want=%v", items[0].RunAt, want)
			}
			break
		}
		select {
		case <-deadline:
			t.Fatal("routine did not repeat")
		case <-time.After(time.Millisecond):
		}
	}
	if _, err := m.Cancel(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	fake.mu.Lock()
	active := fake.until.After(clock.now())
	fake.mu.Unlock()
	if active || m.OwnsTimer() {
		t.Fatal("last cancellation did not release MCU timer")
	}
}

func TestRoutineCancellationStopsRunAndRejectsUpdateUntilReturn(t *testing.T) {
	started, stopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	m, _, clock := newTestManager(t, func(ctx context.Context, _ Routine) (string, error) {
		close(started)
		<-ctx.Done()
		close(stopped)
		<-release
		return "late result", nil
	})
	t.Cleanup(func() { close(release) })
	r := saveRoutine(t, m, time.Second, 10)
	clock.add(time.Second)
	m.Start()
	select {
	case <-started:
	case <-time.After(3 * time.Second):
		t.Fatal("run did not start")
	}
	if _, err := m.Cancel(context.Background(), r.ID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-stopped:
	case <-time.After(3 * time.Second):
		t.Fatal("run not cancelled")
	}
	r.RunAt = clock.now().Add(time.Minute)
	if _, err := m.Save(context.Background(), r); err == nil {
		t.Fatal("updated a runner that has not returned")
	}
	got := waitRoutine(t, m, r.ID, "cancelled")
	if got.Result != "" {
		t.Fatal("late result overwrote cancellation")
	}
}

func TestRoutineRestartDoesNotReplayInterruptedWork(t *testing.T) {
	m, fake, clock := newTestManager(t, nil)
	r := saveRoutine(t, m, time.Second, 30)
	next := m.snapshot()
	next.Routines[0].Status = "running"
	if err := m.commit(next); err != nil {
		t.Fatal(err)
	}
	resumed, err := Open(m.path, fake, func(context.Context, Routine) (string, error) { t.Error("interrupted task replayed"); return "", nil })
	if err != nil {
		t.Fatal(err)
	}
	defer resumed.Close()
	resumed.now = clock.now
	resumed.Start()
	got := waitRoutine(t, resumed, r.ID, "interrupted")
	if got.Error == "" {
		t.Fatal("missing restart explanation")
	}
}

func TestRoutineOpenUnchangedStateNeedsNoWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	state := State{Version: 1, Generation: "generation-1", Cursor: "42"}
	for _, status := range []string{"scheduled", "queued", "completed", "cancelled", "failed", "interrupted"} {
		state.Routines = append(state.Routines, Routine{ID: status, Status: status})
	}
	data, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0400); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	m, err := Open(path, &fakeMCU{}, func(context.Context, Routine) (string, error) { return "", nil })
	if err != nil {
		t.Fatalf("read-only startup failed: %v", err)
	}
	defer m.Close()
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Also catch rewrites when the test is run with permission to bypass chmod.
	if !os.SameFile(before, after) || string(got) != string(data) {
		t.Fatal("opening unchanged state rewrote the file")
	}
	items, _ := m.List()
	if len(items) != len(state.Routines) || m.state.Cursor != state.Cursor {
		t.Fatal("read-only startup lost persisted work or event cursor")
	}
}

func TestRoutineShutdownPersistsInterruptedAndPreservesQueuedWork(t *testing.T) {
	for _, tc := range []struct {
		name     string
		interval int64
		runErr   error
	}{
		{"one-off", 0, context.Canceled},
		{"repeating", 30, context.Canceled},
		{"runner-returns-nil", 30, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			started := make(chan struct{})
			m, fake, clock := newTestManager(t, func(ctx context.Context, _ Routine) (string, error) {
				close(started)
				<-ctx.Done()
				return "partial result", tc.runErr
			})
			active := saveRoutine(t, m, time.Second, tc.interval)
			queued := saveRoutine(t, m, time.Second, 0)
			clock.add(time.Second)
			m.Start()
			select {
			case <-started:
			case <-time.After(3 * time.Second):
				t.Fatal("run did not start")
			}
			m.Close()
			data, err := os.ReadFile(m.path)
			if err != nil {
				t.Fatal(err)
			}
			var saved State
			if err := json.Unmarshal(data, &saved); err != nil {
				t.Fatal(err)
			}
			got := saved.Routines[0]
			if got.Status != "interrupted" || !strings.Contains(got.Error, "stopped during execution") || got.Result != "partial result" {
				t.Fatalf("shutdown result was not persisted: %+v", got)
			}
			if saved.Routines[1].Status != "queued" {
				t.Fatalf("shutdown changed queued work: %+v", saved.Routines[1])
			}
			resumed, err := Open(m.path, fake, func(_ context.Context, r Routine) (string, error) {
				if r.ID == active.ID {
					t.Error("interrupted task replayed")
				}
				return "done", nil
			})
			if err != nil {
				t.Fatal(err)
			}
			defer resumed.Close()
			resumed.now = clock.now
			resumed.Start()
			waitRoutine(t, resumed, queued.ID, "completed")
			waitRoutine(t, resumed, active.ID, "interrupted")
		})
	}
}

func TestRoutineRunErrorWithoutShutdownRemainsFailed(t *testing.T) {
	for _, runErr := range []error{errors.New("model unavailable"), context.Canceled} {
		t.Run(runErr.Error(), func(t *testing.T) {
			m, _, clock := newTestManager(t, func(context.Context, Routine) (string, error) { return "partial result", runErr })
			r := saveRoutine(t, m, time.Second, 30)
			clock.add(time.Second)
			m.Start()
			got := waitRoutine(t, m, r.ID, "failed")
			if got.Error != runErr.Error() || got.Result != "partial result" {
				t.Fatalf("run error changed: %+v", got)
			}
		})
	}
}

func TestRoutinePersistenceFailureDoesNotDispatchOrAdvanceCursor(t *testing.T) {
	m, _, clock := newTestManager(t, nil)
	saveRoutine(t, m, time.Second, 0)
	clock.add(time.Second)
	oldPath := m.path
	m.path = filepath.Join(oldPath, "cannot-write.json")
	if err := m.consume(wakePage("1", clock.now(), 4)); err == nil {
		t.Fatal("expected write error")
	}
	if m.state.Cursor != "" || m.state.Routines[0].Status != "scheduled" {
		t.Fatal("uncommitted work admitted")
	}
	m.path = oldPath
	if err := m.consume(wakePage("1", clock.now(), 4)); err != nil {
		t.Fatal(err)
	}
	if m.state.Cursor != "1" || m.state.Routines[0].Status != "queued" {
		t.Fatal("retry lost wake")
	}
}

func TestRoutineValidationAndTimerOwnership(t *testing.T) {
	m, fake, clock := newTestManager(t, nil)
	fake.until = clock.now().Add(time.Minute)
	if _, err := m.Save(context.Background(), Routine{Task: "test", SessionID: "s_original", RunAt: clock.now().Add(time.Hour)}); err == nil {
		t.Fatal("stole external timer")
	}
	fake.until = time.Time{}
	r := saveRoutine(t, m, time.Minute, 0)
	called := false
	if err := m.WithExternalTimer(func() error { called = true; return nil }); err == nil || called {
		t.Fatal("manual timer bypassed ownership")
	}
	r.SessionID, r.Task, r.RunAt = "s_other", "updated", clock.now().Add(time.Hour)
	updated, err := m.Save(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if updated.SessionID != "s_original" {
		t.Fatal("update changed originating session")
	}
	for _, bad := range []Routine{
		{Task: "", SessionID: "s", RunAt: clock.now().Add(time.Hour)},
		{Task: "test", RunAt: clock.now().Add(time.Hour)},
		{Task: "test", SessionID: "s", RunAt: clock.now()},
		{Task: "test", SessionID: "s", RunAt: clock.now().Add(time.Hour), IntervalS: -1},
		{Task: "test", SessionID: "s", RunAt: clock.now().Add(time.Hour), IntervalS: maxDelay + 1},
	} {
		if _, err := m.Save(context.Background(), bad); err == nil {
			t.Fatalf("accepted %+v", bad)
		}
	}
}

func TestRoutineGenerationResetAndCorruptState(t *testing.T) {
	m, fake, _ := newTestManager(t, nil)
	saveRoutine(t, m, time.Minute, 0)
	if err := m.consume(&mcu.EventPage{Generation: "new-generation", ResetRequired: true, NextID: "999"}); err != nil {
		t.Fatal(err)
	}
	if m.state.Cursor != "0" || m.state.Generation != "new-generation" {
		t.Fatal("did not reset cursor")
	}
	data, _ := json.Marshal(map[string]any{"version": 99})
	if err := os.WriteFile(m.path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(m.path, fake, m.run); err == nil {
		t.Fatal("silently accepted incompatible state")
	}
}
