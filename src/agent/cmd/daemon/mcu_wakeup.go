package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"aico-agent/internal/logging"
	"aico-agent/internal/mcu"
)

const (
	defaultMCUVoiceCursorPath = "/run/aico/agent-mcu-wakeup.json"
	mcuVoiceWakeupMaxAge      = 30 * time.Second
	mcuVoiceStartupMaxAge     = 2 * time.Minute
)

type mcuWakeupEvents interface {
	EventsSince(context.Context, string, string, int, int) (*mcu.EventPage, error)
}

type mcuVoiceCursor struct {
	Generation string `json:"generation"`
	ID         string `json:"id"`
}

// One source owns the cursor across voice configuration reloads. The checkpoint
// in /run also prevents replay when only the Agent process is restarted.
type mcuWakeupSource struct {
	client    mcuWakeupEvents
	statePath string
	retry     time.Duration
	now       func() time.Time
	mu        sync.Mutex
	active    bool
	loaded    bool
	cursor    mcuVoiceCursor
	startupAt time.Time
}

func newMCUWakeupSource(client mcuWakeupEvents, statePath string) *mcuWakeupSource {
	return &mcuWakeupSource{client: client, statePath: statePath, retry: time.Second, now: time.Now, cursor: mcuVoiceCursor{ID: "0"}}
}

func (s *mcuWakeupSource) newWatcher(callback func()) (wakeupWatcher, error) {
	return s.newWatcherWithTap(false, callback)
}

func (s *mcuWakeupSource) newWatcherWithTap(tapEnabled bool, callback func()) (wakeupWatcher, error) {
	return &mcuWakeupWatcher{source: s, callback: callback, tapEnabled: tapEnabled}, nil
}

type mcuWakeupWatcher struct {
	source     *mcuWakeupSource
	callback   func()
	tapEnabled bool
	mu         sync.Mutex
	cancel     context.CancelFunc
	done       chan struct{}
}

func (w *mcuWakeupWatcher) Start() error {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.done != nil {
		return errors.New("MCU wakeup watcher already started")
	}
	s := w.source
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active {
		return errors.New("MCU wakeup source already has a watcher")
	}
	if !s.loaded {
		data, err := os.ReadFile(s.statePath)
		if err == nil {
			var cursor mcuVoiceCursor
			if err := json.Unmarshal(data, &cursor); err != nil {
				return fmt.Errorf("read MCU wakeup cursor: %w", err)
			}
			if _, err := strconv.ParseUint(cursor.ID, 10, 64); err != nil || cursor.Generation == "" {
				return errors.New("invalid saved MCU wakeup cursor")
			}
			s.cursor = cursor
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("read MCU wakeup cursor: %w", err)
		} else {
			// A boot wake can precede slow Agent initialization. Only an
			// uncheckpointed first subscription gets the startup allowance.
			s.startupAt = s.now()
		}
		s.loaded = true
	}
	ctx, cancel := context.WithCancel(context.Background())
	w.cancel, w.done = cancel, make(chan struct{})
	s.active = true
	go func() {
		defer close(w.done)
		defer func() { s.mu.Lock(); s.active = false; s.mu.Unlock() }()
		s.run(ctx, w.callback, w.tapEnabled)
	}()
	return nil
}

func (w *mcuWakeupWatcher) Stop() {
	w.mu.Lock()
	cancel, done := w.cancel, w.done
	w.mu.Unlock()
	if cancel != nil {
		cancel()
		<-done
	}
}

func (s *mcuWakeupSource) checkpoint(cursor mcuVoiceCursor) error {
	if cursor == s.cursor {
		return nil
	}
	data, err := json.Marshal(cursor)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.statePath), 0755); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(s.statePath), ".agent-mcu-wakeup-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	_, err = f.Write(data)
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err := os.Rename(f.Name(), s.statePath); err != nil {
		return err
	}
	s.cursor = cursor
	return nil
}

func (s *mcuWakeupSource) run(ctx context.Context, callback func(), tapEnabled bool) {
	failed := false
	for ctx.Err() == nil {
		page, err := s.client.EventsSince(ctx, s.cursor.ID, s.cursor.Generation, 50, 25000)
		if err == nil && ctx.Err() == nil {
			err = s.consumeWithTap(ctx, page, callback, tapEnabled)
		}
		if ctx.Err() != nil {
			return
		}
		if err == nil {
			if failed {
				logging.Infof("agent", "wakeup", "MCU wakeup event subscription recovered")
			}
			failed = false
			continue
		}
		if !failed {
			logging.Warnf("agent", "wakeup", "MCU wakeup event subscription unavailable; retrying: %v", err)
		}
		failed = true
		timer := time.NewTimer(s.retry)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (s *mcuWakeupSource) consume(ctx context.Context, page *mcu.EventPage, callback func()) error {
	return s.consumeWithTap(ctx, page, callback, false)
}

func (s *mcuWakeupSource) consumeWithTap(ctx context.Context, page *mcu.EventPage, callback func(), tapEnabled bool) error {
	if page == nil || page.Generation == "" {
		return errors.New("missing MCU event page or generation")
	}
	if page.ResetRequired {
		logging.Warnf("agent", "wakeup", "MCU event generation/cursor reset; reading retained history")
		s.startupAt = time.Time{}
		return s.checkpoint(mcuVoiceCursor{Generation: page.Generation, ID: "0"})
	}
	if s.cursor.Generation != "" && s.cursor.Generation != page.Generation {
		return errors.New("MCU event generation changed without reset")
	}
	if page.Truncated {
		logging.Warnf("agent", "wakeup", "MCU event history was truncated; some wake events may be lost")
	}
	cursorID, _ := strconv.ParseUint(s.cursor.ID, 10, 64)
	for _, event := range page.Events {
		if err := ctx.Err(); err != nil {
			return err
		}
		id, err := strconv.ParseUint(event.ID, 10, 64)
		if err != nil {
			return fmt.Errorf("invalid MCU event ID: %w", err)
		}
		if id <= cursorID {
			continue
		}
		// Commit before invoking the voice callback. Delivery is best effort:
		// a process crash here may lose this action, but cannot replay it.
		if err := s.checkpoint(mcuVoiceCursor{Generation: page.Generation, ID: event.ID}); err != nil {
			return fmt.Errorf("save MCU wakeup cursor: %w", err)
		}
		cursorID = id
		asr := event.Source == "asr" && event.SourceCode == 6
		tap := tapEnabled && event.Source == "touch_key" && event.SourceCode == 3
		if event.Type != "wakeup" || (!asr && !tap) {
			continue
		}
		received, err := time.Parse(time.RFC3339Nano, event.ReceivedAt)
		maxAge := mcuVoiceWakeupMaxAge
		if !s.startupAt.IsZero() && !received.After(s.startupAt) {
			maxAge = mcuVoiceStartupMaxAge
		}
		if err != nil || s.now().Sub(received) > maxAge {
			logging.Warnf("agent", "wakeup", "Skipping stale or undated MCU wakeup event generation=%s id=%s", page.Generation, event.ID)
			continue
		}
		logging.Infof("agent", "wakeup", "MCU %s wakeup event generation=%s id=%s", event.Source, page.Generation, event.ID)
		callback()
	}
	// Advance over every event type, using next_id rather than the ring's
	// last_id so a limited page never skips later wakeup events.
	next, err := strconv.ParseUint(page.NextID, 10, 64)
	if err != nil || next < cursorID {
		return errors.New("invalid MCU next_id")
	}
	if err := s.checkpoint(mcuVoiceCursor{Generation: page.Generation, ID: page.NextID}); err != nil {
		return err
	}
	if page.NextID == page.LastID {
		// Keep the allowance across limited pages, then restore normal
		// freshness checks as soon as retained startup history is drained.
		s.startupAt = time.Time{}
	}
	return nil
}
