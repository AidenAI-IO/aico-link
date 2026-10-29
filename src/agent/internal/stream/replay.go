package stream

import (
	"errors"
	"fmt"
	"sync"
)

// ReplayCursor identifies the last event observed by one projection.
// SessionEpoch changes when a live session is replaced or the process restarts.
type ReplayCursor struct {
	SessionEpoch uint64 `json:"session_epoch"`
	Sequence     uint64 `json:"sequence"`
}

// ReplayResult is a bounded replay attempt. ResyncRequired means the caller's
// cursor is outside the retained window or belongs to another session epoch.
type ReplayResult struct {
	Events         []Event
	Current        ReplayCursor
	ResyncRequired bool
}

// ReplayLog retains a bounded ordered event window for reconnecting observers.
// It is safe for concurrent append and snapshot operations.
type ReplayLog struct {
	mu             sync.RWMutex
	capacity       int
	events         []Event
	conversationID string
	sessionEpoch   uint64
}

func NewReplayLog(capacity int) *ReplayLog {
	if capacity < 1 {
		capacity = 1
	}
	return &ReplayLog{capacity: capacity}
}

// Append records one event. A new session epoch starts a fresh replay window;
// older epochs are rejected so stale producers cannot repopulate the window.
func (l *ReplayLog) Append(event Event) error {
	if l == nil {
		return errors.New("stream replay log is nil")
	}
	if err := event.Validate(); err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.conversationID == "" {
		l.conversationID = event.ConversationID
		l.sessionEpoch = event.SessionEpoch
	} else if event.ConversationID != l.conversationID {
		return fmt.Errorf("stream replay log: conversation_id changed from %q to %q", l.conversationID, event.ConversationID)
	} else if event.SessionEpoch < l.sessionEpoch {
		return fmt.Errorf("stream replay log: stale session_epoch %d", event.SessionEpoch)
	} else if event.SessionEpoch > l.sessionEpoch {
		l.sessionEpoch = event.SessionEpoch
		l.events = nil
	}
	if n := len(l.events); n > 0 {
		last := l.events[n-1]
		if event.Sequence < last.Sequence {
			return fmt.Errorf("stream replay log: sequence %d is older than %d", event.Sequence, last.Sequence)
		}
		if event.Sequence == last.Sequence {
			if event.EventID == last.EventID {
				return nil
			}
			return fmt.Errorf("stream replay log: conflicting duplicate sequence %d", event.Sequence)
		}
	}
	l.events = append(l.events, event)
	if len(l.events) > l.capacity {
		l.events = append([]Event(nil), l.events[len(l.events)-l.capacity:]...)
	}
	return nil
}

// Snapshot returns events after cursor. A nil cursor asks for the retained
// window. The returned event slice is defensive and safe for the caller to
// mutate.
func (l *ReplayLog) Snapshot(cursor *ReplayCursor) ReplayResult {
	if l == nil {
		return ReplayResult{ResyncRequired: true}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	result := ReplayResult{Current: l.currentLocked()}
	if cursor == nil {
		result.Events = append([]Event(nil), l.events...)
		return result
	}
	if cursor.SessionEpoch != l.sessionEpoch {
		result.ResyncRequired = true
		return result
	}
	if len(l.events) == 0 {
		if cursor.Sequence > 0 && cursor.Sequence > result.Current.Sequence {
			result.ResyncRequired = true
		}
		return result
	}
	first := l.events[0].Sequence
	if cursor.Sequence+1 < first || cursor.Sequence > result.Current.Sequence {
		result.ResyncRequired = true
		return result
	}
	for _, event := range l.events {
		if event.Sequence > cursor.Sequence {
			result.Events = append(result.Events, event)
		}
	}
	return result
}

func (l *ReplayLog) currentLocked() ReplayCursor {
	current := ReplayCursor{SessionEpoch: l.sessionEpoch}
	if len(l.events) > 0 {
		current.Sequence = l.events[len(l.events)-1].Sequence
	}
	return current
}

// Cursor returns the newest retained cursor.
func (l *ReplayLog) Cursor() ReplayCursor {
	if l == nil {
		return ReplayCursor{}
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.currentLocked()
}
