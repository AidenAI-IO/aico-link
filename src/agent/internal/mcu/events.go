package mcu

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"strconv"
	"sync"
	"time"
)

type HardwareEvent struct {
	ID         string `json:"id"`
	Type       string `json:"type"`
	Source     string `json:"source,omitempty"`
	SourceCode byte   `json:"source_code"`
	UARTSeq    byte   `json:"uart_seq"`
	ReceivedAt string `json:"received_at"`
	Inserted   *bool  `json:"inserted,omitempty"`
	RawLevel   *byte  `json:"raw_level,omitempty"`
}
type EventPage struct {
	Events        []HardwareEvent `json:"events"`
	Generation    string          `json:"generation"`
	NextID        string          `json:"next_id"`
	OldestID      string          `json:"oldest_id"`
	LastID        string          `json:"last_id"`
	ResetRequired bool            `json:"reset_required"`
	Truncated     bool            `json:"truncated"`
}
type EventStore struct {
	mu         sync.Mutex
	events     []HardwareEvent
	capacity   int
	next       uint64
	generation string
	changed    chan struct{}
	closed     bool
	waiters    int
}

func NewEventStore(capacity int) *EventStore {
	if capacity < 1 {
		capacity = 512
	}
	var id [16]byte
	if _, err := rand.Read(id[:]); err != nil {
		panic(err)
	}
	return &EventStore{capacity: capacity, generation: hex.EncodeToString(id[:]), changed: make(chan struct{})}
}
func (s *EventStore) Generation() string { return s.generation }
func (s *EventStore) Append(e HardwareEvent) HardwareEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return HardwareEvent{}
	}
	s.next++
	e.ID = strconv.FormatUint(s.next, 10)
	e.ReceivedAt = time.Now().UTC().Format(time.RFC3339Nano)
	if len(s.events) == s.capacity {
		copy(s.events, s.events[1:])
		s.events = s.events[:len(s.events)-1]
	}
	s.events = append(s.events, e)
	close(s.changed)
	s.changed = make(chan struct{})
	return e
}
func (s *EventStore) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		close(s.changed)
	}
}
func (s *EventStore) Page(ctx context.Context, since, generation string, limit, waitMS int) (EventPage, error) {
	if since == "" {
		since = "0"
	}
	cursor, err := strconv.ParseUint(since, 10, 64)
	if err != nil || (cursor > 0 && generation == "") || limit < 0 || limit > 100 || waitMS < 0 || waitMS > 25000 {
		return EventPage{}, invalid("invalid event cursor, limit or wait_ms")
	}
	if limit == 0 {
		limit = 50
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(waitMS)*time.Millisecond)
	defer cancel()
	registered := false
	defer func() {
		if registered {
			s.mu.Lock()
			s.waiters--
			s.mu.Unlock()
		}
	}()
	for {
		s.mu.Lock()
		p := EventPage{Events: []HardwareEvent{}, Generation: s.generation, NextID: since, OldestID: "0", LastID: strconv.FormatUint(s.next, 10)}
		if len(s.events) > 0 {
			p.OldestID = s.events[0].ID
			oldest, _ := strconv.ParseUint(p.OldestID, 10, 64)
			p.Truncated = oldest > cursor && oldest-cursor > 1
		}
		p.ResetRequired = (generation != "" && generation != s.generation) || cursor > s.next
		if !p.ResetRequired {
			for _, e := range s.events {
				id, _ := strconv.ParseUint(e.ID, 10, 64)
				if id > cursor {
					p.Events = append(p.Events, e)
					p.NextID = e.ID
					if len(p.Events) == limit {
						break
					}
				}
			}
		}
		if s.closed {
			s.mu.Unlock()
			return EventPage{}, &APIError{Status: "NOT_READY", Message: "MCU service is stopping"}
		}
		if p.ResetRequired || len(p.Events) > 0 || waitMS == 0 || ctx.Err() != nil {
			s.mu.Unlock()
			return p, nil
		}
		if !registered {
			if s.waiters >= 32 {
				s.mu.Unlock()
				return EventPage{}, &APIError{Status: "BUSY", Message: "too many event waiters"}
			}
			s.waiters++
			registered = true
		}
		changed := s.changed
		s.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
		}
	}
}
