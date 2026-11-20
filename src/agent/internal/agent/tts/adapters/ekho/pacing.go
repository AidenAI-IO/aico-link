package ekho

import (
	"context"
	"time"

	"aico-agent/internal/agent/tts"
)

// pacedSink keeps local synthesis from flooding audio_service's bounded queue.
// Track all sentences together so sentence transitions cannot add another
// burst. Leave a small lead; the playback sink supplies its own prebuffer.
// WritePCM is serialized by the session; Stop may run concurrently.
type pacedSink struct {
	tts.AudioSink
	ctx  context.Context
	next time.Time
}

func (s *pacedSink) WritePCM(p []byte) error {
	if err := s.ctx.Err(); err != nil {
		return err
	}
	// Do not accumulate a catch-up budget while waiting for LLM text or the
	// next synthesis: after an idle gap the queue must still receive paced PCM.
	if earliest := time.Now().Add(-100 * time.Millisecond); s.next.Before(earliest) {
		s.next = earliest
	}
	if wait := time.Until(s.next); wait > 0 {
		timer := time.NewTimer(wait)
		defer timer.Stop()
		select {
		case <-s.ctx.Done():
			return s.ctx.Err()
		case <-timer.C:
		}
	}
	if err := s.AudioSink.WritePCM(p); err != nil {
		return err
	}
	s.next = s.next.Add(time.Duration(len(p)) * time.Second / (sampleRate * 2))
	return nil
}
