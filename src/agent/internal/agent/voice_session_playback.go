package agent

import (
	"context"

	"aico-agent/internal/agent/tts"
)

// voiceSessionPlayback plays Classic replies for a voice conversation. A
// muted speaker plays silence in place of the reply, so playback timing,
// barge-in and drain behave as if it were audible.
type voiceSessionPlayback struct {
	tts.AudioServiceBackend
	voice *RealtimeVoiceSession
}

func (p voiceSessionPlayback) WritePlayChunk(sessionID uint64, data []byte, isFinal bool) error {
	if len(data) > 0 && p.voice.SpeakerMuted() {
		data = make([]byte, len(data))
	}
	return p.AudioServiceBackend.WritePlayChunk(sessionID, data, isFinal)
}

// WaitForPlaybackDrain waits for the wrapped backend when it can report
// drain.
func (p voiceSessionPlayback) WaitForPlaybackDrain(ctx context.Context) error {
	if drainer, ok := p.AudioServiceBackend.(interface {
		WaitForPlaybackDrain(context.Context) error
	}); ok {
		return drainer.WaitForPlaybackDrain(ctx)
	}
	return nil
}

// unwrapVoiceSessionPlayback returns the backend under a voice conversation
// wrapper, for callers that use audio_service directly.
func unwrapVoiceSessionPlayback(audio tts.AudioServiceBackend) tts.AudioServiceBackend {
	if wrapped, ok := audio.(voiceSessionPlayback); ok {
		return wrapped.AudioServiceBackend
	}
	return audio
}
