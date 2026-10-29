package tts

import (
	"context"
	"sync"
)

// ProviderHolder holds a swappable TTSProvider behind a stable interface.
// Business code holds *ProviderHolder and calls BeginStream as usual.
//
// Safety guarantee: Swap waits for all in-flight sessions (created from the
// old provider) to finish before returning the old provider. This ensures
// the caller can safely Close the old provider without interrupting playback.
type ProviderHolder struct {
	mu      sync.RWMutex
	current TTSProvider

	// activeWG tracks in-flight sessions. Each BeginStream increments it;
	// the wrapped session's Close decrements it.
	activeWG *sync.WaitGroup
}

// NewProviderHolder wraps an initial provider.
func NewProviderHolder(initial TTSProvider) *ProviderHolder {
	return &ProviderHolder{current: initial, activeWG: &sync.WaitGroup{}}
}

// Swap replaces the current provider with next. It blocks until all sessions
// created from the OLD provider have been closed, then returns the old
// provider for the caller to Close.
func (h *ProviderHolder) Swap(next TTSProvider) TTSProvider {
	old, wait := h.replace(next)
	wait()
	return old
}

// replace installs next immediately and returns the previous provider together
// with a function that waits for that provider's sessions to drain.
func (h *ProviderHolder) replace(next TTSProvider) (TTSProvider, func()) {
	h.mu.Lock()
	old := h.current
	oldWG := h.activeWG
	h.current = next
	// Replace the WaitGroup for new sessions going forward.
	// We keep a reference to the old one to wait on.
	h.activeWG = &sync.WaitGroup{}
	h.mu.Unlock()
	return old, oldWG.Wait
}

func (h *ProviderHolder) Name() string {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.current == nil {
		return ""
	}
	return h.current.Name()
}

func (h *ProviderHolder) Capabilities() Capabilities {
	h.mu.RLock()
	defer h.mu.RUnlock()
	if h.current == nil {
		return Capabilities{}
	}
	return h.current.Capabilities()
}

func (h *ProviderHolder) BeginStream(ctx context.Context, sink AudioSink) (StreamSession, error) {
	p, wg, err := h.acquireProvider()
	if err != nil {
		return nil, err
	}
	return beginTrackedStream(ctx, p, wg, sink)
}

// BeginStreamWithCapabilities creates the sink from the same provider
// generation that opens the stream. The generation is pinned before reading
// capabilities, so a concurrent Swap cannot pair old capabilities with a new
// provider session.
func (h *ProviderHolder) BeginStreamWithCapabilities(ctx context.Context, makeSink func(Capabilities) AudioSink) (StreamSession, error) {
	p, wg, err := h.acquireProvider()
	if err != nil {
		return nil, err
	}
	sink := makeSink(p.Capabilities())
	return beginTrackedStream(ctx, p, wg, sink)
}

func (h *ProviderHolder) acquireProvider() (TTSProvider, *sync.WaitGroup, error) {
	h.mu.RLock()
	p := h.current
	if p == nil {
		h.mu.RUnlock()
		return nil, nil, ErrProviderNotFound
	}
	// Track this session in the current generation's WaitGroup.
	wg := h.activeWG
	wg.Add(1)
	h.mu.RUnlock()
	return p, wg, nil
}

func beginTrackedStream(ctx context.Context, p TTSProvider, wg *sync.WaitGroup, sink AudioSink) (StreamSession, error) {
	session, err := p.BeginStream(ctx, sink)
	if err != nil {
		wg.Done()
		return nil, err
	}

	return &trackedSession{
		StreamSession: session,
		done:          wg,
	}, nil
}

func (h *ProviderHolder) Close() error {
	h.mu.Lock()
	old := h.current
	oldWG := h.activeWG
	if old == nil {
		h.mu.Unlock()
		return nil
	}
	h.current = nil
	h.activeWG = &sync.WaitGroup{}
	h.mu.Unlock()

	oldWG.Wait()
	return old.Close()
}

// Compile-time check.
var _ TTSProvider = (*ProviderHolder)(nil)

// trackedSession wraps a StreamSession and decrements the WaitGroup on Close.
type trackedSession struct {
	StreamSession
	done      *sync.WaitGroup
	closeOnce sync.Once
	err       error
}

func (t *trackedSession) Close() error {
	t.closeOnce.Do(func() {
		defer t.done.Done()
		t.err = t.StreamSession.Close()
	})
	return t.err
}

func (t *trackedSession) Abort() error {
	t.closeOnce.Do(func() {
		defer t.done.Done()
		if aborter, ok := t.StreamSession.(interface{ Abort() error }); ok {
			t.err = aborter.Abort()
		} else {
			t.err = t.StreamSession.Close()
		}
	})
	return t.err
}

// FinishInput forwards the optional half-close operation while preserving the
// provider-generation accounting owned by trackedSession.Close.
func (t *trackedSession) FinishInput() error {
	if finisher, ok := t.StreamSession.(InputFinisher); ok {
		return finisher.FinishInput()
	}
	// A holder must remain transparent for legacy providers. Flush is the
	// closest safe approximation of a half-close for those sessions.
	return t.StreamSession.Flush()
}

// Cancel forwards optional upstream cancellation without releasing the holder
// lease; callers still call Close once resource release is complete.
func (t *trackedSession) Cancel() error {
	if canceller, ok := t.StreamSession.(SessionCanceller); ok {
		return canceller.Cancel()
	}
	return ErrLifecycleUnsupported
}

// ResetBuffer preserves optional buffering capabilities exposed by the
// underlying provider session. Without this forwarding method, callers only
// see trackedSession and cannot discard incomplete text between LLM turns.
func (t *trackedSession) ResetBuffer() {
	if resetter, ok := t.StreamSession.(interface{ ResetBuffer() }); ok {
		resetter.ResetBuffer()
	}
}
