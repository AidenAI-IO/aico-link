package stream

import (
	"errors"
	"sync"
)

var (
	ErrTerminalAlreadySet = errors.New("stream terminal outcome already set")
	ErrInvalidTerminal    = errors.New("stream terminal state is not terminal")
	ErrUnexpectedEOF      = errors.New("stream ended without a terminal outcome")
)

// TerminalState is the local outcome of one response attempt, synthesis, tool
// call, or run.  Pending is the only non-terminal state.
type TerminalState string

const (
	TerminalPending   TerminalState = "pending"
	TerminalCompleted TerminalState = "completed"
	TerminalCancelled TerminalState = "cancelled"
	TerminalFailed    TerminalState = "failed"
)

func (s TerminalState) IsTerminal() bool {
	return s == TerminalCompleted || s == TerminalCancelled || s == TerminalFailed
}

// TerminalOutcome is an immutable snapshot returned by Tracker.  Err is
// informational and should be sanitized before crossing a public boundary.
type TerminalOutcome struct {
	State TerminalState
	Err   error
}

// TerminalTracker enforces the exactly-once local terminal transition rule.
// Its zero value is ready for use.
type TerminalTracker struct {
	mu    sync.Mutex
	state TerminalState
	err   error
	set   bool
}

func (t *TerminalTracker) State() TerminalState {
	if t == nil {
		return TerminalPending
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.set {
		return TerminalPending
	}
	return t.state
}

func (t *TerminalTracker) Outcome() TerminalOutcome {
	if t == nil {
		return TerminalOutcome{State: TerminalPending}
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.set {
		return TerminalOutcome{State: TerminalPending}
	}
	return TerminalOutcome{State: t.state, Err: t.err}
}

// Finish records exactly one terminal outcome.  Calls after the first outcome
// return ErrTerminalAlreadySet, even if they repeat the same state.
func (t *TerminalTracker) Finish(state TerminalState, err error) error {
	if t == nil {
		return errors.New("stream terminal tracker is nil")
	}
	if !state.IsTerminal() {
		return ErrInvalidTerminal
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.set {
		return ErrTerminalAlreadySet
	}
	t.state = state
	t.err = err
	t.set = true
	return nil
}

func (t *TerminalTracker) Complete() error {
	return t.Finish(TerminalCompleted, nil)
}

func (t *TerminalTracker) Cancel(err error) error {
	return t.Finish(TerminalCancelled, err)
}

func (t *TerminalTracker) Fail(err error) error {
	return t.Finish(TerminalFailed, err)
}

// FailEOF records the required failure outcome for a producer that closes or
// reaches EOF before it emitted a terminal event.
func (t *TerminalTracker) FailEOF() error {
	return t.Fail(ErrUnexpectedEOF)
}
