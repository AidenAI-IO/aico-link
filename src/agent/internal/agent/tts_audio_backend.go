package agent

import (
	"context"

	"aico-agent/internal/agent/tts"
)

// audioBackend bridges the internal *AudioServiceClient to the
// tts.AudioServiceBackend interface used by the new pluggable TTS module.
type audioBackend struct{ c *AudioServiceClient }

func newAudioBackend(c *AudioServiceClient) *audioBackend { return &audioBackend{c: c} }

var _ tts.AudioServiceBackend = (*audioBackend)(nil)

func (a *audioBackend) StartPlayback(format tts.AudioFormat) (uint64, error) {
	res, err := a.c.StartPlayback(AudioFormat{
		SampleRate: uint32(format.SampleRate),
		Channels:   uint32(format.Channels),
		BitWidth:   uint32(format.BitWidth),
	})
	if err != nil {
		return 0, err
	}
	return res.SessionID, nil
}

func (a *audioBackend) WritePlayChunk(sessionID uint64, data []byte, isFinal bool) error {
	return a.c.WritePlayChunk(sessionID, data, isFinal)
}

func (a *audioBackend) StopPlayback(sessionID uint64) error {
	return a.c.StopPlayback(sessionID)
}

// WaitForPlaybackDrain waits until audio_service has finished draining all
// finalized playback sessions. Realtime uses this to report notification
// delivery only after playback, rather than file submission, has completed.
func (a *audioBackend) WaitForPlaybackDrain(ctx context.Context) error {
	if a == nil || a.c == nil {
		return nil
	}
	return waitForPlaybackDrain(ctx, a.c, playbackDrainTimeout)
}
