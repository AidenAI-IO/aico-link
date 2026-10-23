// Package routines persists Agent schedules and multiplexes the MCU's single
// countdown timer. Host timers only retry/reconcile; execution is admitted by
// an MCU wake event or a confirmed expired MCU timer.
package routines

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"aiden-agent/internal/mcu"
	"github.com/google/uuid"
)

const maxDelay = 604800

type MCU interface {
	GetState(context.Context) (*mcu.State, error)
	GetWakeupTimer(context.Context) (*mcu.Timer, error)
	SetWakeupTimer(context.Context, int64, string, string) (*mcu.Timer, error)
	CancelWakeupTimer(context.Context, string, string) (*mcu.Timer, error)
	EventsSince(context.Context, string, string, int, int) (*mcu.EventPage, error)
}

type Routine struct {
	ID        string    `json:"id"`
	Task      string    `json:"task"`
	SessionID string    `json:"session_id"`
	RunAt     time.Time `json:"run_at"`
	IntervalS int64     `json:"interval_s"`
	Status    string    `json:"status"`
	LastRunAt time.Time `json:"last_run_at,omitempty"`
	Result    string    `json:"result,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type State struct {
	Version    int       `json:"version"`
	Routines   []Routine `json:"routines"`
	Generation string    `json:"generation,omitempty"`
	Cursor     string    `json:"cursor,omitempty"`
	OwnsTimer  bool      `json:"owns_timer"`
	TimerDue   time.Time `json:"timer_due,omitempty"`
}

type Runner func(context.Context, Routine) (string, error)

type Manager struct {
	mu         sync.Mutex
	state      State
	path       string
	mcu        MCU
	run        Runner
	now        func() time.Time
	timerErr   string
	changed    chan struct{}
	work       chan struct{}
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
	activeID   string
	activeStop context.CancelFunc
}

func Open(path string, client MCU, run Runner) (*Manager, error) {
	if path == "" || client == nil || run == nil {
		return nil, errors.New("routines require persistent storage, MCU and runner")
	}
	m := &Manager{path: path, mcu: client, run: run, now: time.Now, changed: make(chan struct{}, 1), work: make(chan struct{}, 1)}
	m.state = State{Version: 1, Routines: []Routine{}}
	data, err := os.ReadFile(path)
	if err == nil {
		if err = json.Unmarshal(data, &m.state); err != nil {
			return nil, fmt.Errorf("load routines: %w", err)
		}
		if m.state.Version != 1 {
			return nil, errors.New("unsupported routines version")
		}
		// Never replay a run whose side effects may have happened before a crash.
		for i := range m.state.Routines {
			r := &m.state.Routines[i]
			if r.Status == "running" {
				r.Status, r.Error = "interrupted", "Agent restarted during execution; inspect the session before rescheduling"
			}
		}
		if err = m.commit(m.state); err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	m.ctx, m.cancel = context.WithCancel(context.Background())
	return m, nil
}

// Start is called once after tools and runtime dependencies have been installed.
func (m *Manager) Start() {
	m.wg.Add(2)
	go m.watch()
	go m.worker()
	m.signal(m.work)
}

func (m *Manager) Close() {
	m.cancel()
	m.wg.Wait()
}

func (m *Manager) List() ([]Routine, string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return slices.Clone(m.state.Routines), m.timerErr
}

func (m *Manager) OwnsTimer() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state.OwnsTimer
}

// WithExternalTimer serializes the HTTP timer API with scheduler ownership.
// Direct UDS clients must also respect the shared timer's single-owner contract.
func (m *Manager) WithExternalTimer(action func() error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state.OwnsTimer {
		return &mcu.APIError{Status: "CONFLICT", Message: "scheduled routines own the MCU timer; manage them with the schedule tool"}
	}
	return action()
}

// Save creates a routine, or replaces the complete schedule of an existing
// non-running routine. Updates preserve the originating session.
func (m *Manager) Save(ctx context.Context, r Routine) (Routine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ctx.Err(); err != nil {
		return Routine{}, err
	}
	r.Task, r.SessionID = strings.TrimSpace(r.Task), strings.TrimSpace(r.SessionID)
	if r.Task == "" || len(r.Task) > 16000 || r.SessionID == "" || !r.RunAt.After(m.now()) || r.IntervalS < 0 || r.IntervalS > maxDelay {
		return Routine{}, errors.New("task, originating session and future run_at are required; interval_s must be 0..604800")
	}
	next := m.snapshot()
	index := -1
	if r.ID != "" {
		index = slices.IndexFunc(next.Routines, func(old Routine) bool { return old.ID == r.ID })
		if index < 0 {
			return Routine{}, errors.New("routine not found")
		}
		if next.Routines[index].Status == "running" || m.activeID == r.ID {
			return Routine{}, errors.New("wait for the running routine to stop before updating it")
		}
		r.SessionID = next.Routines[index].SessionID
	} else {
		if len(next.Routines) >= 1000 {
			return Routine{}, errors.New("routine storage is full")
		}
		r.ID = "routine_" + uuid.NewString()
	}
	// Refuse to steal an independently programmed MCU timer on first use.
	if !next.OwnsTimer {
		timer, err := m.mcu.GetWakeupTimer(ctx)
		if err != nil {
			return Routine{}, err
		}
		if timer == nil || timer.Active {
			return Routine{}, errors.New("MCU timer is unavailable or already in use")
		}
	}
	r.RunAt, r.Status = r.RunAt.UTC(), "scheduled"
	next.OwnsTimer = true
	if index < 0 {
		next.Routines = append(next.Routines, r)
	} else {
		next.Routines[index] = r
	}
	if err := m.commit(next); err != nil {
		return Routine{}, err
	}
	// Persistence precedes arming. A failed/uncertain MCU response leaves the
	// routine visible for reconciliation, with a separate timer_error in List.
	m.reconcileLocked(ctx)
	m.signal(m.changed)
	return r, nil
}

func (m *Manager) Cancel(ctx context.Context, id string) (Routine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.ctx.Err(); err != nil {
		return Routine{}, err
	}
	next := m.snapshot()
	i := slices.IndexFunc(next.Routines, func(r Routine) bool { return r.ID == id })
	if i < 0 {
		return Routine{}, errors.New("routine not found")
	}
	next.Routines[i].Status = "cancelled"
	if err := m.commit(next); err != nil {
		return Routine{}, err
	}
	if m.activeID == id && m.activeStop != nil {
		m.activeStop()
	}
	m.reconcileLocked(ctx)
	m.signal(m.changed)
	return m.state.Routines[i], nil
}

func (m *Manager) snapshot() State {
	s := m.state
	s.Routines = slices.Clone(s.Routines)
	return s
}

// commit replaces memory only after the complete state is on disk.
func (m *Manager) commit(s State) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err = os.MkdirAll(filepath.Dir(m.path), 0700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(m.path), ".routines-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	err = errors.Join(err, f.Close())
	if err != nil {
		return err
	}
	if err = os.Rename(f.Name(), m.path); err != nil {
		return err
	}
	m.state = s
	return nil
}

func (m *Manager) signal(ch chan struct{}) {
	select {
	case ch <- struct{}{}:
	default:
	}
}

func (m *Manager) reconcileLocked(ctx context.Context) {
	err := m.reconcile(ctx)
	m.timerErr = ""
	if err != nil {
		m.timerErr = err.Error()
	}
}

func (m *Manager) reconcile(ctx context.Context) error {
	if !m.state.OwnsTimer {
		return nil
	}
	state, err := m.mcu.GetState(ctx)
	if err != nil {
		return err
	}
	if state == nil || state.Generation == "" {
		return errors.New("MCU state unavailable")
	}
	timer, err := m.mcu.GetWakeupTimer(ctx)
	if err != nil {
		return err
	}
	if timer == nil {
		return errors.New("MCU timer unavailable")
	}
	// A lost UART event or daemon restart can erase the wake event. Confirmed
	// inactivity permits one catch-up run, never a burst of missed repetitions.
	if !timer.Active && !m.state.TimerDue.IsZero() && !m.state.TimerDue.After(m.now()) {
		if err := m.enqueueDue(m.snapshot()); err != nil {
			return err
		}
	}
	var earliest time.Time
	for _, r := range m.state.Routines {
		if r.Status == "scheduled" && (earliest.IsZero() || r.RunAt.Before(earliest)) {
			earliest = r.RunAt
		}
	}
	if earliest.IsZero() {
		if timer.Active {
			if _, err := m.mcu.CancelWakeupTimer(ctx, state.Generation, uuid.NewString()); err != nil {
				return err
			}
		}
		next := m.snapshot()
		next.OwnsTimer = slices.ContainsFunc(next.Routines, func(r Routine) bool { return r.Status == "queued" || r.Status == "running" })
		next.TimerDue = time.Time{}
		if next.OwnsTimer == m.state.OwnsTimer && m.state.TimerDue.IsZero() {
			return nil
		}
		return m.commit(next)
	}
	now := m.now()
	distance := min(earliest.Sub(now), time.Duration(maxDelay)*time.Second)
	seconds := int64(1)
	if distance > 0 {
		seconds = int64((distance + time.Second - 1) / time.Second)
	}
	delay := max(int64(1), min(seconds, int64(maxDelay)))
	target := now.Add(time.Duration(delay) * time.Second)
	// Confirm the actual timer every pass. Do not extend an already-correct
	// countdown, including when Agent restarted or a SET response was lost.
	remaining := time.Duration(timer.RemainingS) * time.Second
	if timer.Active && !m.state.TimerDue.IsZero() && !m.state.TimerDue.After(earliest.Add(time.Second)) && absDuration(remaining-m.state.TimerDue.Sub(now)) <= 2*time.Second {
		return nil
	}
	if timer.Active && absDuration(remaining-time.Duration(delay)*time.Second) <= 2*time.Second {
		next := m.snapshot()
		next.TimerDue = now.Add(remaining)
		return m.commit(next)
	}
	confirmed, err := m.mcu.SetWakeupTimer(ctx, delay, state.Generation, uuid.NewString())
	if err != nil {
		return err
	}
	if confirmed == nil || !confirmed.Active {
		return errors.New("MCU did not confirm an active timer")
	}
	next := m.snapshot()
	next.TimerDue = target
	return m.commit(next)
}

func absDuration(d time.Duration) time.Duration {
	if d < 0 {
		return -d
	}
	return d
}

func (m *Manager) enqueueDue(next State) error {
	changed := false
	for i := range next.Routines {
		r := &next.Routines[i]
		if r.Status == "scheduled" && !r.RunAt.After(m.now()) {
			r.Status = "queued"
			changed = true
		}
	}
	// The event cursor and due work are committed together before dispatch.
	if changed || next.Cursor != m.state.Cursor || next.Generation != m.state.Generation {
		if err := m.commit(next); err != nil {
			return err
		}
	}
	if changed {
		m.signal(m.work)
	}
	return nil
}

func (m *Manager) consume(page *mcu.EventPage) error {
	if page == nil || page.Generation == "" {
		return errors.New("invalid MCU event page")
	}
	next := m.snapshot()
	if page.ResetRequired {
		next.Generation, next.Cursor = page.Generation, "0"
		return m.commit(next)
	}
	next.Generation, next.Cursor = page.Generation, page.NextID
	for _, event := range page.Events {
		if event.Type == "wakeup" && event.SourceCode == 4 && event.Source == "rtc_timer" {
			at, err := time.Parse(time.RFC3339Nano, event.ReceivedAt)
			if err != nil || at.After(m.now().Add(time.Second)) {
				continue
			}
			for i := range next.Routines {
				r := &next.Routines[i]
				if r.Status == "scheduled" && !r.RunAt.After(at) && !r.RunAt.After(m.now()) {
					r.Status = "queued"
				}
			}
		}
	}
	if next.Cursor != m.state.Cursor || next.Generation != m.state.Generation {
		if err := m.commit(next); err != nil {
			return err
		}
		m.signal(m.work)
	}
	return nil
}

func (m *Manager) watch() {
	defer m.wg.Done()
	for m.ctx.Err() == nil {
		m.mu.Lock()
		m.reconcileLocked(m.ctx)
		// Also retry durable queued work after a transient state-write failure.
		m.signal(m.work)
		cursor, generation := m.state.Cursor, m.state.Generation
		m.mu.Unlock()
		// Changes wake a pending long poll so a newly created earlier deadline
		// does not wait for its timeout. Each consumer owns its independent cursor.
		pollCtx, stop := context.WithCancel(m.ctx)
		done := make(chan struct{})
		go func() {
			select {
			case <-m.changed:
				stop()
			case <-done:
			case <-pollCtx.Done():
			}
		}()
		page, err := m.mcu.EventsSince(pollCtx, cursor, generation, 100, 25000)
		close(done)
		stop()
		if err == nil {
			m.mu.Lock()
			err = m.consume(page)
			if err != nil {
				m.timerErr = err.Error()
			}
			m.mu.Unlock()
		}
		if err != nil && !errors.Is(err, context.Canceled) {
			timer := time.NewTimer(time.Second)
			select {
			case <-timer.C:
			case <-m.changed:
			case <-m.ctx.Done():
			}
			timer.Stop()
		}
	}
}

func (m *Manager) worker() {
	defer m.wg.Done()
	for {
		select {
		case <-m.ctx.Done():
			return
		case <-m.work:
		}
		for m.ctx.Err() == nil {
			m.mu.Lock()
			next := m.snapshot()
			i := -1
			for index, candidate := range next.Routines {
				if candidate.Status == "queued" && (i < 0 || candidate.RunAt.Before(next.Routines[i].RunAt)) {
					i = index
				}
			}
			if i < 0 {
				m.mu.Unlock()
				break
			}
			next.Routines[i].Status = "running"
			next.Routines[i].LastRunAt = m.now().UTC()
			if err := m.commit(next); err != nil {
				m.timerErr = err.Error()
				m.mu.Unlock()
				break
			}
			r := next.Routines[i]
			ctx, stop := context.WithCancel(m.ctx)
			m.activeID, m.activeStop = r.ID, stop
			m.mu.Unlock()
			result, err := m.run(ctx, r)
			if ctx.Err() != nil && err == nil {
				err = ctx.Err()
			}
			stop()
			for {
				if saveErr := m.finish(i, result, err); saveErr == nil {
					break
				}
				// Keep the result in memory while retrying a transient disk error.
				// Never invoke the runner again just because its result was not saved.
				retry := time.NewTimer(time.Second)
				select {
				case <-m.ctx.Done():
					retry.Stop()
					return
				case <-retry.C:
				}
			}
			m.signal(m.changed)
		}
	}
}

func (m *Manager) finish(i int, result string, runErr error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.activeID, m.activeStop = "", nil
	next := m.snapshot()
	current := &next.Routines[i]
	if current.Status != "running" {
		return nil
	}
	current.Result, current.Error, current.Status = limitText(result), "", "completed"
	if runErr != nil {
		current.Error, current.Status = runErr.Error(), "failed"
	} else if current.IntervalS > 0 {
		interval := time.Duration(current.IntervalS) * time.Second
		steps := m.now().Sub(current.RunAt)/interval + 1
		current.RunAt = current.RunAt.Add(max(steps, 1) * interval)
		current.Status = "scheduled"
		next.OwnsTimer = true
	}
	if err := m.commit(next); err != nil {
		m.timerErr = err.Error()
		return err
	}
	return nil
}

func limitText(s string) string {
	runes := []rune(s)
	if len(runes) > 8000 {
		return string(runes[:8000]) + "…"
	}
	return s
}
