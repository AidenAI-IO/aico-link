package agent

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/stream"
)

var errStreamingSTTFinalizeTimeout = errors.New("streaming STT finalize timed out")

type streamingSTTSession struct {
	uploader       STTStreamUploader
	events         <-chan stream.TranscriptEvent
	eventsDone     chan struct{}
	eventsDoneOnce sync.Once

	mu           sync.Mutex
	transcript   string
	finalizeErr  error
	finalized    bool
	finalizing   bool
	finalizeDone chan struct{}
}

func beginStreamingSTTSession(ctx context.Context, client STTClient, cfg STTStreamConfig) (*streamingSTTSession, error) {
	if client == nil || !client.Capabilities().SupportsStreamingUpload {
		return nil, nil
	}
	uploader, err := client.NewStreamingUploader(ctx, cfg)
	if err != nil {
		return nil, err
	}
	var events <-chan stream.TranscriptEvent
	if source, ok := uploader.(STTTranscriptEventSource); ok {
		events = source.TranscriptEvents()
	}
	return &streamingSTTSession{uploader: uploader, events: events, eventsDone: make(chan struct{})}, nil
}

// TranscriptEvents returns optional revisioned transcript updates. A nil
// channel means the provider only exposes the legacy final transcript.
func (s *streamingSTTSession) TranscriptEvents() <-chan stream.TranscriptEvent {
	if s == nil {
		return nil
	}
	return s.events
}

func (s *streamingSTTSession) UploadPCM(pcm []byte) error {
	if s == nil || s.uploader == nil || len(pcm) == 0 {
		return nil
	}
	if err := s.uploader.UploadPCM(pcm); err != nil {
		return fmt.Errorf("streaming STT upload: %w", err)
	}
	return nil
}

func (s *streamingSTTSession) Finalize() (string, error) {
	if s == nil || s.uploader == nil {
		return "", nil
	}

	s.mu.Lock()
	if s.finalized {
		transcript, err := s.transcript, s.finalizeErr
		s.mu.Unlock()
		return transcript, err
	}
	if s.finalizing {
		done := s.finalizeDone
		s.mu.Unlock()
		<-done
		s.mu.Lock()
		transcript, err := s.transcript, s.finalizeErr
		s.mu.Unlock()
		return transcript, err
	}
	done := make(chan struct{})
	s.finalizing = true
	s.finalizeDone = done
	uploader := s.uploader
	s.mu.Unlock()

	transcript, err := uploader.Finalize()
	if err != nil {
		err = fmt.Errorf("streaming STT finalize: %w", err)
	}

	transcript = strings.TrimSpace(transcript)
	s.mu.Lock()
	s.transcript = transcript
	s.finalizeErr = err
	s.finalized = true
	s.finalizing = false
	close(done)
	s.mu.Unlock()
	return transcript, err
}

func (s *streamingSTTSession) FinalizeWithTimeout(timeout time.Duration) (string, error) {
	if s == nil || s.uploader == nil {
		return "", nil
	}
	if timeout <= 0 {
		return s.Finalize()
	}

	type result struct {
		transcript string
		err        error
	}
	done := make(chan result, 1)
	go func() {
		transcript, err := s.Finalize()
		done <- result{transcript: transcript, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case result := <-done:
		return result.transcript, result.err
	case <-timer.C:
		_ = s.Close()
		return "", fmt.Errorf("%w after %s", errStreamingSTTFinalizeTimeout, timeout)
	}
}

func (s *streamingSTTSession) Close() error {
	if s == nil {
		return nil
	}
	s.eventsDoneOnce.Do(func() {
		if s.eventsDone != nil {
			close(s.eventsDone)
		}
	})
	if s.uploader == nil {
		return nil
	}
	if err := s.uploader.Close(); err != nil {
		return fmt.Errorf("streaming STT close: %w", err)
	}
	return nil
}
