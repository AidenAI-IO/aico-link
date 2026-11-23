package tts

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"aico-agent/internal/providerretry"
)

type retryAudioSink struct {
	AudioSink
	mu        sync.Mutex
	epoch     int
	delivered atomic.Bool
}

type attemptAudioSink struct {
	*retryAudioSink
	epoch int
}

func (s *attemptAudioSink) WritePCM(data []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.epoch != s.retryAudioSink.epoch {
		return context.Canceled
	}
	if len(data) > 0 {
		s.delivered.Store(true)
	}
	return s.AudioSink.WritePCM(data)
}

// A failed pre-audio attempt must not finalize the shared playback sink.
func (s *attemptAudioSink) Drain(ctx context.Context) error {
	if !s.activeOutput() {
		return nil
	}
	return s.AudioSink.Drain(ctx)
}

func (s *attemptAudioSink) Stop() error {
	if !s.activeOutput() {
		return nil
	}
	return s.AudioSink.Stop()
}

func (s *attemptAudioSink) activeOutput() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.epoch == s.retryAudioSink.epoch && s.delivered.Load()
}

func (s *retryAudioSink) invalidate() {
	s.mu.Lock()
	s.epoch++
	s.mu.Unlock()
}

type synthesisOp struct {
	kind string
	text string
	ctx  context.Context
}

// retrySession retains only pre-audio input. Every failed attempt is canceled
// and closed before its input is replayed to the same provider generation.
type retrySession struct {
	mu            sync.Mutex
	provider      TTSProvider
	raw           StreamSession
	sink          *retryAudioSink
	ctx           context.Context
	cancel        context.CancelFunc
	attemptCancel context.CancelFunc
	ops           []synthesisOp
	retained      int
	retries       int
	err           error
	closed        bool
	canceled      bool
	cancelErr     error
}

func beginRetryStream(ctx context.Context, p TTSProvider, sink AudioSink) (StreamSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)
	if err := ctx.Err(); err != nil {
		cancel()
		return nil, err
	}
	s := &retrySession{provider: p, sink: &retryAudioSink{AudioSink: sink}, ctx: providerretry.Managed(ctx), cancel: cancel}
	for {
		err := s.open()
		if err == nil {
			return s, nil
		}
		if err = s.prepareRetry(s.ctx, err); err != nil {
			cancel()
			return nil, err
		}
	}
}

func (s *retrySession) open() error {
	ctx, cancel := context.WithCancel(s.ctx)
	s.attemptCancel = cancel
	var err error
	err = s.bounded(func() error {
		var openErr error
		s.raw, openErr = s.provider.BeginStream(ctx, &attemptAudioSink{retryAudioSink: s.sink, epoch: s.sink.epoch})
		return openErr
	})
	if err != nil {
		cancel()
		if s.raw != nil {
			_ = CancelAndClose(s.raw)
			s.raw = nil
		}
	}
	return err
}

func (s *retrySession) prepareRetry(operationCtx context.Context, err error) error {
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	if operationCtx.Err() != nil {
		return operationCtx.Err()
	}
	if s.sink.delivered.Load() || s.retries >= 2 || s.retained > 256*1024 || !providerretry.Retryable(err) {
		return err
	}
	if s.attemptCancel != nil {
		s.attemptCancel()
	}
	s.sink.invalidate()
	if s.raw != nil {
		_ = CancelAndClose(s.raw)
		s.raw = nil
	}
	if s.sink.delivered.Load() {
		return err
	}
	s.retries++
	waitCtx, cancel := context.WithCancel(operationCtx)
	stop := context.AfterFunc(s.ctx, cancel)
	defer stop()
	defer cancel()
	waitErr := providerretry.Wait(waitCtx, providerretry.Delay(err, time.Duration(s.retries)*500*time.Millisecond))
	if s.ctx.Err() != nil {
		return s.ctx.Err()
	}
	return waitErr
}

func (s *retrySession) apply(op synthesisOp) error {
	return s.bounded(func() error { return s.applyRaw(op) })
}

// Bound blocking setup/synthesis before audio, without imposing a total limit
// on a healthy session that incrementally accepts text and produces PCM.
func (s *retrySession) bounded(call func() error) error {
	if s.sink.delivered.Load() {
		return call()
	}
	var expired atomic.Bool
	done := make(chan struct{})
	timer := time.AfterFunc(30*time.Second, func() {
		if !s.sink.delivered.Load() {
			expired.Store(true)
			s.attemptCancel()
		}
		close(done)
	})
	err := call()
	if !timer.Stop() {
		<-done
	}
	if expired.Load() && s.ctx.Err() == nil {
		return context.DeadlineExceeded
	}
	return err
}

func (s *retrySession) applyRaw(op synthesisOp) error {
	switch op.kind {
	case "write":
		return s.raw.WriteText(op.text)
	case "flush":
		return s.raw.Flush()
	case "finish":
		if finisher, ok := s.raw.(ContextFinishInputter); ok && op.ctx != nil {
			return finisher.FinishInput(op.ctx)
		}
		return FinishInput(s.raw)
	case "close":
		return s.raw.Close()
	}
	return nil
}

func (s *retrySession) run(op synthesisOp) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.err
	}
	if s.err != nil {
		if op.kind == "close" {
			if s.raw != nil {
				_ = CancelAndClose(s.raw)
			}
			s.closed = true
			s.cancel()
		}
		return s.err
	}
	if s.ctx.Err() != nil {
		if op.kind == "finish" {
			return s.applyRaw(op)
		}
		if op.kind == "close" {
			var err error
			if s.raw != nil {
				err = s.raw.Close()
			}
			s.closed = true
			s.err = err
			return err
		}
		s.err = s.ctx.Err()
		return s.err
	}
	if s.sink.delivered.Load() {
		s.ops = nil
		s.retained = 0
	} else if s.retained <= 256*1024 {
		s.retained += len(op.text) + 16
		if s.retained > 256*1024 {
			s.ops = nil
		} else {
			s.ops = append(s.ops, op)
		}
	}
	err := s.apply(op)
	operationCtx := s.ctx
	if op.ctx != nil {
		operationCtx = op.ctx
	}
	for err != nil {
		if next := s.prepareRetry(operationCtx, err); next != nil {
			err = next
			break
		}
		err = s.open()
		if err != nil {
			continue
		}
		for _, previous := range s.ops {
			if err = s.apply(previous); err != nil {
				break
			}
		}
	}
	if op.kind == "close" {
		if err == nil && !s.sink.delivered.Load() {
			err = s.sink.AudioSink.Drain(s.ctx)
		}
		s.closed = true
		s.cancel()
	}
	if err != nil {
		s.err = err
	}
	return err
}

func (s *retrySession) WriteText(text string) error {
	return s.run(synthesisOp{kind: "write", text: text})
}
func (s *retrySession) Flush() error       { return s.run(synthesisOp{kind: "flush"}) }
func (s *retrySession) FinishInput() error { return s.run(synthesisOp{kind: "finish"}) }
func (s *retrySession) Close() error       { return s.run(synthesisOp{kind: "close"}) }
func (s *retrySession) Cancel() error      { return s.cancelContext(context.Background()) }
func (s *retrySession) cancelContext(ctx context.Context) error {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.canceled || s.closed {
		return s.cancelErr
	}
	s.canceled = true
	if canceller, ok := s.raw.(ContextCanceller); ok {
		s.cancelErr = canceller.Cancel(ctx)
	} else if canceller, ok := s.raw.(SessionCanceller); ok {
		s.cancelErr = canceller.Cancel()
	}
	return s.cancelErr
}
func (s *retrySession) Abort() error {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.err
	}
	s.closed = true
	if s.raw != nil {
		var err error
		if s.canceled {
			err = s.raw.Close()
		} else {
			err = CancelAndClose(s.raw)
		}
		if s.err == nil && !errors.Is(err, context.Canceled) {
			s.err = err
		}
	}
	return s.err
}
func (s *retrySession) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.raw != nil {
		return s.raw.Err()
	}
	return nil
}
func (s *retrySession) ResetBuffer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if resetter, ok := s.raw.(interface{ ResetBuffer() }); ok {
		resetter.ResetBuffer()
		s.ops = nil
		s.retained = 0
	}
}
