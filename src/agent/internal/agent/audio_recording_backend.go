package agent

import (
	"aico-agent/internal/logging"
	"fmt"
	"time"
)

// AudioRecordingBackend is the common recording interface used by device and
// host-local audio implementations.
type AudioRecordingBackend interface {
	StartRecording(format AudioFormat) (*RecordStartResult, error)
	ReadRecordChunk(sessionID uint64, timeoutMs uint32) (*AudioChunkResult, error)
	StopRecording(sessionID uint64) error
}

type audioRecordingBackend = AudioRecordingBackend

func recordingBackendOrService(backend audioRecordingBackend, service *AudioServiceClient) audioRecordingBackend {
	if backend != nil {
		return backend
	}
	return service
}

func newAudioRecordingBackendFromConfig(cfg Config, service *AudioServiceClient, logger *Logger) audioRecordingBackend {
	switch cfg.AudioBackendOrDefault() {
	case AudioBackendLocal:
		return newLocalAudioRecordingBackend(logger)
	default:
		return service
	}
}

// NewConfiguredAudioRecordingBackend selects the recording backend resolved
// from cfg. Callers should pass the shared audio_service client so board mode
// continues to reuse the configured Unix socket.
func NewConfiguredAudioRecordingBackend(cfg Config, service *AudioServiceClient, logger *Logger) AudioRecordingBackend {
	return newAudioRecordingBackendFromConfig(cfg, service, logger)
}

func startRecordingWithRetry(audio audioRecordingBackend, format AudioFormat, retryTimeout, retryInterval time.Duration) (*RecordStartResult, error) {
	if retryTimeout <= 0 {
		return audio.StartRecording(format)
	}
	if retryInterval <= 0 {
		retryInterval = 100 * time.Millisecond
	}

	deadline := time.Now().Add(retryTimeout)
	attempts := 0
	var lastErr error
	for {
		result, err := audio.StartRecording(format)
		if err == nil {
			if attempts > 0 {
				logging.Infof("agent", "audio", "Record session opened after %d retries", attempts)
			}
			return result, nil
		}
		lastErr = err
		attempts++
		if attempts == 1 {
			logging.Warnf("agent", "audio", "Record session unavailable, retrying for up to %s: %v", retryTimeout, err)
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			break
		}
		sleep := retryInterval
		if sleep > remaining {
			sleep = remaining
		}
		time.Sleep(sleep)
	}
	return nil, fmt.Errorf("after %s: %w", retryTimeout, lastErr)
}
