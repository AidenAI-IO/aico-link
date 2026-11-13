package agent

import (
	"aico-agent/internal/agent/tts"
)

// voiceSessionPlayback plays replies for a voice conversation. A muted
// speaker plays silence in place of the reply, so playback timing and
// barge-in behave as if it were audible.
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

// voicePlayback returns where a reply plays: device outside a conversation,
// otherwise the conversation's route (the browser that started it, or
// device) with its speaker mute applied.
func voicePlayback(voice *RealtimeVoiceSession, device tts.AudioServiceBackend) tts.AudioServiceBackend {
	if voice == nil {
		return device
	}
	if voice.Route() == RealtimeAudioRouteWeb {
		device = voice.Audio().Playback()
	}
	if device == nil {
		return nil
	}
	return voiceSessionPlayback{AudioServiceBackend: device, voice: voice}
}

// unwrapVoiceSessionPlayback returns the backend under a voice conversation
// wrapper, for callers that use audio_service directly.
func unwrapVoiceSessionPlayback(audio tts.AudioServiceBackend) tts.AudioServiceBackend {
	if wrapped, ok := audio.(voiceSessionPlayback); ok {
		return wrapped.AudioServiceBackend
	}
	return audio
}
