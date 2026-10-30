package tts

import (
	"context"
	"testing"
	"time"
)

type contextLifecycleSession struct {
	finishCtx context.Context
	cancelCtx context.Context
	closes    int
}

func (s *contextLifecycleSession) WriteText(string) error { return nil }
func (s *contextLifecycleSession) Flush() error           { return nil }
func (s *contextLifecycleSession) Close() error {
	s.closes++
	return nil
}
func (s *contextLifecycleSession) Err() error { return nil }
func (s *contextLifecycleSession) FinishInput(ctx context.Context) error {
	s.finishCtx = ctx
	return nil
}
func (s *contextLifecycleSession) Cancel(ctx context.Context) error {
	s.cancelCtx = ctx
	return nil
}

type lifecyclePlayback struct{ ctx context.Context }

func (p *lifecyclePlayback) Drain(ctx context.Context) error { p.ctx = ctx; return nil }

func TestContextLifecycleHelpersPreferContextCapabilities(t *testing.T) {
	s := &contextLifecycleSession{}
	ctx := context.WithValue(context.Background(), "k", "v")
	if supported, err := FinishInputContext(ctx, s); !supported || err != nil || s.finishCtx == nil {
		t.Fatalf("FinishInputContext() = supported=%v err=%v", supported, err)
	}
	if err := CancelContext(ctx, s); err != nil || s.cancelCtx != ctx {
		t.Fatalf("CancelContext() = err=%v ctx=%v", err, s.cancelCtx)
	}
}

type contextLifecycleProvider struct{ session StreamSession }

func (p *contextLifecycleProvider) Name() string               { return "context-lifecycle" }
func (p *contextLifecycleProvider) Capabilities() Capabilities { return Capabilities{} }
func (p *contextLifecycleProvider) BeginStream(context.Context, AudioSink) (StreamSession, error) {
	return p.session, nil
}
func (p *contextLifecycleProvider) Close() error { return nil }

func TestProviderHolderPreservesContextLifecycle(t *testing.T) {
	underlying := &contextLifecycleSession{}
	provider := &contextLifecycleProvider{session: underlying}
	holder := NewProviderHolder(provider)
	session, err := holder.BeginStream(context.Background(), noopSink{})
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	ctx := context.WithValue(context.Background(), "k", "v")
	if supported, err := FinishInputContext(ctx, session); !supported || err != nil || underlying.finishCtx != ctx {
		t.Fatalf("FinishInputContext() = supported=%v err=%v ctx=%v", supported, err, underlying.finishCtx)
	}
	if err := CancelContext(ctx, session); err != nil || underlying.cancelCtx != ctx {
		t.Fatalf("CancelContext() = err=%v ctx=%v", err, underlying.cancelCtx)
	}

	swapped := make(chan TTSProvider, 1)
	go func() { swapped <- holder.Swap(nil) }()
	select {
	case <-swapped:
		t.Fatal("Swap returned before the canceled session closed")
	case <-time.After(50 * time.Millisecond):
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	select {
	case old := <-swapped:
		if old != provider {
			t.Fatalf("Swap returned %v, want provider", old)
		}
	case <-time.After(time.Second):
		t.Fatal("Swap did not return after the canceled session closed")
	}
	if underlying.closes != 1 {
		t.Fatalf("underlying Close() calls = %d, want 1", underlying.closes)
	}
}

func TestProviderHolderCancelAndCloseForwardsContextCancellation(t *testing.T) {
	underlying := &contextLifecycleSession{}
	holder := NewProviderHolder(&contextLifecycleProvider{session: underlying})
	session, err := holder.BeginStream(context.Background(), noopSink{})
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	if err := CancelAndClose(session); err != nil {
		t.Fatalf("CancelAndClose() error = %v", err)
	}
	if underlying.cancelCtx == nil || underlying.closes != 1 {
		t.Fatalf("underlying cancellation context = %v, closes = %d; want cancellation and one close", underlying.cancelCtx, underlying.closes)
	}
}

type cancelLifecycleSession struct {
	legacyLifecycleSession
	cancels int
}

func (s *cancelLifecycleSession) Cancel() error {
	s.cancels++
	return nil
}

func TestProviderHolderCancelContextPreservesLegacyCancelLease(t *testing.T) {
	underlying := &cancelLifecycleSession{}
	holder := NewProviderHolder(&contextLifecycleProvider{session: underlying})
	session, err := holder.BeginStream(context.Background(), noopSink{})
	if err != nil {
		t.Fatalf("BeginStream() error = %v", err)
	}
	if err := CancelContext(context.Background(), session); err != nil {
		t.Fatalf("CancelContext() error = %v", err)
	}
	if underlying.cancels != 1 || underlying.closes != 0 {
		t.Fatalf("underlying cancels = %d, closes = %d; want cancellation without close", underlying.cancels, underlying.closes)
	}
	if err := session.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
}

func TestDrainPlaybackPassesContext(t *testing.T) {
	p := &lifecyclePlayback{}
	ctx := context.WithValue(context.Background(), "k", "v")
	if err := DrainPlayback(ctx, p); err != nil {
		t.Fatalf("DrainPlayback() error = %v", err)
	}
	if p.ctx != ctx {
		t.Fatal("DrainPlayback() did not pass caller context")
	}
}

type legacyLifecycleSession struct {
	flushes int
	closes  int
}

func (s *legacyLifecycleSession) WriteText(string) error { return nil }
func (s *legacyLifecycleSession) Flush() error {
	s.flushes++
	return nil
}
func (s *legacyLifecycleSession) Close() error {
	s.closes++
	return nil
}
func (s *legacyLifecycleSession) Err() error { return nil }

func TestLifecycleHelpersPreserveLegacyProviderSemantics(t *testing.T) {
	s := &legacyLifecycleSession{}
	if err := FinishInput(s); err != nil || s.flushes != 1 {
		t.Fatalf("FinishInput() err=%v flushes=%d", err, s.flushes)
	}
	if err := CancelAndClose(s); err != nil || s.closes != 1 {
		t.Fatalf("CancelAndClose() err=%v closes=%d", err, s.closes)
	}
}
