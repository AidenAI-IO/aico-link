package tts

import (
	"context"
	"testing"
)

type contextLifecycleSession struct {
	finishCtx context.Context
	cancelCtx context.Context
}

func (s *contextLifecycleSession) WriteText(string) error { return nil }
func (s *contextLifecycleSession) Flush() error           { return nil }
func (s *contextLifecycleSession) Close() error           { return nil }
func (s *contextLifecycleSession) Err() error             { return nil }
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
