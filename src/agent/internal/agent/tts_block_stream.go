package agent

import (
	"context"
	"sync"

	"aico-agent/internal/agent/tts"
)

// latestTTSOutput gives spoken blocks a latest-wins lifetime. Reserving output
// before launching asynchronous tool speech prevents an older goroutine from
// starting after a newer response has already taken over.
type latestTTSOutput struct {
	mu      sync.Mutex
	current *activeTTSOutput
}

type ttsOutputContextKey struct{}

func (s *latestTTSOutput) begin(ctx context.Context) (context.Context, *activeTTSOutput, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	output := newActiveTTSOutput(cancel)
	s.mu.Lock()
	var previous *activeTTSOutput
	if ctx.Err() == nil {
		previous = s.current
		s.current = output
	}
	s.mu.Unlock()
	if ctx.Err() != nil {
		output.interrupt()
	}
	if previous != nil {
		previous.supersede()
	}
	var once sync.Once
	cleanup := func() {
		once.Do(func() {
			s.mu.Lock()
			if s.current == output {
				s.current = nil
			}
			s.mu.Unlock()
			cancel()
			output.finish()
		})
	}
	return context.WithValue(ctx, ttsOutputContextKey{}, output), output, cleanup
}

func (s *latestTTSOutput) acquire(ctx context.Context) (context.Context, *activeTTSOutput, func()) {
	if ctx == nil {
		ctx = context.Background()
	}
	if output, ok := ctx.Value(ttsOutputContextKey{}).(*activeTTSOutput); ok {
		return ctx, output, func() {}
	}
	return s.begin(ctx)
}

// ttsBlockStream preopens the first provider connection, but gives each spoken
// block a separate synthesis/playback session. A new block stops the previous
// audio immediately instead of appending behind its unplayed PCM.
type ttsBlockStream struct {
	ctx     context.Context
	latest  *latestTTSOutput
	begin   func(context.Context) (*streamSessionWriter, error)
	mu      sync.Mutex
	current *streamSessionWriter
	output  *activeTTSOutput
	used    bool
	stopped bool
	err     error
	release func()
}

func beginManagedTTSBlockStream(ctx context.Context, manager *tts.ProviderManager, playback tts.AudioServiceBackend, cfg Config, latest *latestTTSOutput) (*ttsBlockStream, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	begin := func(parent context.Context) (*streamSessionWriter, error) {
		streamCtx, cancel := context.WithCancel(parent)
		stream, err := beginManagedTTSStream(streamCtx, manager, playback, cfg)
		if err != nil {
			cancel()
			return nil, err
		}
		stream.setCancel(cancel)
		return stream, nil
	}
	first, err := begin(ctx)
	if err != nil {
		return nil, err
	}
	return &ttsBlockStream{ctx: ctx, latest: latest, begin: begin, current: first}, nil
}

func (w *ttsBlockStream) BeginSpeechBlock() {
	w.mu.Lock()
	if w.stopped || w.ctx.Err() != nil {
		w.mu.Unlock()
		return
	}
	previous, used, release := w.current, w.used, w.release
	w.used = true
	w.current = nil
	w.release = nil
	w.mu.Unlock()
	if used && previous != nil {
		previous.interrupt()
	}
	if release != nil {
		release()
	}
	ctx, output, cleanup := w.latest.begin(w.ctx)
	current := previous
	var err error
	if used || current == nil {
		current, err = w.begin(ctx)
	}
	if current != nil {
		output.setStream(current)
	}
	w.mu.Lock()
	if w.stopped || w.ctx.Err() != nil {
		w.mu.Unlock()
		output.interrupt()
		cleanup()
		return
	}
	w.current, w.output, w.err, w.release = current, output, err, cleanup
	w.mu.Unlock()
}

func (w *ttsBlockStream) snapshot() *streamSessionWriter {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stopped {
		return nil
	}
	return w.current
}

func (w *ttsBlockStream) Write(p []byte) (int, error) {
	if current := w.snapshot(); current != nil {
		return current.Write(p)
	}
	return 0, nil
}

func (w *ttsBlockStream) Flush() error {
	if current := w.snapshot(); current != nil {
		return current.Flush()
	}
	return nil
}

func (w *ttsBlockStream) ResetBuffer() {
	w.mu.Lock()
	current, release := w.current, w.release
	w.current, w.release = nil, nil
	w.used = true
	w.mu.Unlock()
	if current != nil {
		current.interrupt()
	}
	if release != nil {
		release()
	}
}

func (w *ttsBlockStream) interrupt() {
	w.mu.Lock()
	w.stopped = true
	current := w.current
	w.mu.Unlock()
	if current != nil {
		current.interrupt()
	}
}

func (w *ttsBlockStream) closeAndWait() error {
	w.mu.Lock()
	current, err, release := w.current, w.err, w.release
	w.release = nil
	w.mu.Unlock()
	if release != nil {
		defer release()
	}
	if current != nil {
		return current.closeAndWait()
	}
	return err
}

func (w *ttsBlockStream) emittedSpeech(err error) bool {
	if current := w.snapshot(); current != nil {
		return current.emittedSpeech(err)
	}
	return false
}

func (w *ttsBlockStream) wasSuperseded() bool {
	w.mu.Lock()
	output := w.output
	w.mu.Unlock()
	return output.wasSuperseded()
}
