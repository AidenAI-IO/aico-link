package tts

import (
	"context"
	"errors"
)

var ErrLifecycleUnsupported = errors.New("tts lifecycle operation unsupported")

// InputFinisher is an optional half-close capability.
type InputFinisher interface {
	FinishInput() error
}

// SessionCanceller is an optional cancellation capability.
type SessionCanceller interface {
	Cancel() error
}

// FinishInput ends synthesis input while preserving pending output. Legacy
// providers use Flush as the closest compatible half-close operation.
func FinishInput(session StreamSession) error {
	if session == nil {
		return nil
	}
	if finisher, ok := session.(InputFinisher); ok {
		return finisher.FinishInput()
	}
	return session.Flush()
}

// Cancel requests prompt termination and preserves legacy Abort behavior.
func Cancel(session StreamSession) error {
	if session == nil {
		return nil
	}
	if aborter, ok := session.(interface{ Abort() error }); ok {
		return aborter.Abort()
	}
	if canceller, ok := session.(SessionCanceller); ok {
		return canceller.Cancel()
	}
	return session.Close()
}

// ContextFinishInputter is an optional context-aware half-close capability.
type ContextFinishInputter interface {
	FinishInput(context.Context) error
}

// ContextCanceller is an optional context-aware cancellation capability.
type ContextCanceller interface {
	Cancel(context.Context) error
}

// PlaybackDrainer is the small lifecycle interface implemented by AudioSink.
type PlaybackDrainer interface {
	Drain(context.Context) error
}

// FinishInputContext invokes a context-aware half-close when available and
// falls back to the existing FinishInput compatibility helper.
func FinishInputContext(ctx context.Context, session StreamSession) (bool, error) {
	if session == nil {
		return false, ErrLifecycleUnsupported
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if finisher, ok := session.(ContextFinishInputter); ok {
		return true, finisher.FinishInput(ctx)
	}
	return true, FinishInput(session)
}

// CancelContext invokes a context-aware cancellation operation when
// available, then preserves the existing Cancel fallback chain.
func CancelContext(ctx context.Context, session StreamSession) error {
	if session == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if canceller, ok := session.(ContextCanceller); ok {
		return canceller.Cancel(ctx)
	}
	return Cancel(session)
}

// CancelAndClose cancels upstream work and releases the session. Existing
// Abort implementations own both operations; sessions exposing only Cancel
// are closed after cancellation so provider resources cannot leak.
func CancelAndClose(session StreamSession) error {
	if session == nil {
		return nil
	}
	if aborter, ok := session.(interface{ Abort() error }); ok {
		return aborter.Abort()
	}
	var cancelErr error
	if _, ok := session.(ContextCanceller); ok {
		cancelErr = CancelContext(context.Background(), session)
	} else if canceller, ok := session.(SessionCanceller); ok {
		cancelErr = canceller.Cancel()
	} else {
		return session.Close()
	}
	closeErr := session.Close()
	if cancelErr != nil {
		return cancelErr
	}
	return closeErr
}

// DrainPlayback waits for queued audio to finish.
func DrainPlayback(ctx context.Context, playback PlaybackDrainer) error {
	if playback == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	return playback.Drain(ctx)
}
