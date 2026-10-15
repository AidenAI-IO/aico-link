package agent

import (
	"context"
	"fmt"
	"strings"
	"time"
)

const (
	playbackDrainPollInterval = 50 * time.Millisecond
	playbackDrainTimeout      = 30 * time.Second
	playbackChunkBytes        = 4096 // 2048 samples = 128ms @16kHz s16le mono
)

func waitForPlaybackDrain(ctx context.Context, audio *AudioServiceClient, timeout time.Duration) error {
	if timeout <= 0 {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(playbackDrainPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		health, err := audio.Health()
		if err != nil {
			if !isTransientAudioServiceError(err) {
				return fmt.Errorf("wait playback drain: %w", err)
			}
			lastErr = err
		} else {
			lastErr = nil
			if health.PlaybackSessions == 0 {
				return nil
			}
		}

		select {
		case <-waitCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("wait playback drain: %w", lastErr)
			}
			return waitCtx.Err()
		case <-ticker.C:
		}
	}
}

func stopPlaybackIgnoringEnded(audio *AudioServiceClient, sessionID uint64) error {
	err := audio.StopPlayback(sessionID)
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "session_not_found") ||
		strings.Contains(msg, "not_found") ||
		strings.Contains(msg, "not found") {
		return nil
	}
	return err
}
