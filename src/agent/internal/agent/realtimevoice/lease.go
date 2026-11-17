package realtimevoice

import (
	"context"
	"fmt"
	"sync"
	"time"
)

type leasedSession struct {
	Session
	events    chan Event
	stop      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func newLeasedSession(session Session, expiresAt time.Time, refreshMargin time.Duration) *leasedSession {
	bufferSize := cap(session.Events())
	if bufferSize < 1 {
		bufferSize = 64
	}
	s := &leasedSession{
		Session: session,
		events:  make(chan Event, bufferSize),
		stop:    make(chan struct{}),
	}
	go s.forwardEvents(expiresAt, refreshMargin)
	return s
}

func (s *leasedSession) Events() <-chan Event { return s.events }

func (s *leasedSession) Close() error {
	s.closeOnce.Do(func() {
		close(s.stop)
		s.closeErr = s.Session.Close()
	})
	return s.closeErr
}

func (s *leasedSession) forwardEvents(expiresAt time.Time, refreshMargin time.Duration) {
	defer close(s.events)
	rotateAt := expiresAt.Add(-refreshMargin)
	now := time.Now()
	if !rotateAt.After(now) {
		rotateAt = now.Add(time.Until(expiresAt) / 2)
	}
	delay := time.Until(rotateAt)
	if delay < 0 {
		delay = 0
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	source := s.Session.Events()
	for {
		select {
		case event, ok := <-source:
			if !ok {
				return
			}
			select {
			case s.events <- event:
			case <-s.stop:
				return
			}
		case <-timer.C:
			event := Event{Kind: EventError, Error: fmt.Errorf("%w: Speko delegated session lease expires at %s", ErrSessionRotated, expiresAt.Format(time.RFC3339Nano))}
			select {
			case s.events <- event:
			case <-s.stop:
			}
			return
		case <-s.stop:
			return
		}
	}
}

type leasedGeminiSession struct {
	*leasedSession
	tool   ToolResultSender
	text   TextSession
	replay ContextReplayer
}

func newLeasedGeminiSession(session Session, expiresAt time.Time, refreshMargin time.Duration) Session {
	return &leasedGeminiSession{
		leasedSession: newLeasedSession(session, expiresAt, refreshMargin),
		tool:          session.(ToolResultSender),
		text:          session.(TextSession),
		replay:        session.(ContextReplayer),
	}
}

func (s *leasedGeminiSession) SendToolResult(ctx context.Context, id, output string) error {
	return s.tool.SendToolResult(ctx, id, output)
}
func (s *leasedGeminiSession) SendToolResultWithImages(ctx context.Context, id, output string, images []Image) error {
	if sender, ok := s.tool.(ImageToolResultSender); ok {
		return sender.SendToolResultWithImages(ctx, id, output, images)
	}
	return s.tool.SendToolResult(ctx, id, output)
}
func (s *leasedGeminiSession) SendText(ctx context.Context, text string) error {
	return s.text.SendText(ctx, text)
}
func (s *leasedGeminiSession) CreateResponse(ctx context.Context) error {
	return s.text.CreateResponse(ctx)
}
func (s *leasedGeminiSession) ReplayContext(ctx context.Context, items []ContextItem) error {
	return s.replay.ReplayContext(ctx, items)
}

type leasedXAISession struct {
	*leasedGeminiSession
	commit    TurnCommitter
	interrupt ResponseInterrupter
}

func newLeasedXAISession(session Session, expiresAt time.Time, refreshMargin time.Duration) Session {
	return &leasedXAISession{
		leasedGeminiSession: &leasedGeminiSession{
			leasedSession: newLeasedSession(session, expiresAt, refreshMargin),
			tool:          session.(ToolResultSender),
			text:          session.(TextSession),
			replay:        session.(ContextReplayer),
		},
		commit:    session.(TurnCommitter),
		interrupt: session.(ResponseInterrupter),
	}
}

func (s *leasedXAISession) Commit(ctx context.Context) error {
	return s.commit.Commit(ctx)
}
func (s *leasedXAISession) Interrupt(ctx context.Context, interruption ResponseInterruption) error {
	return s.interrupt.Interrupt(ctx, interruption)
}
