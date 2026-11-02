package agent

import (
	"context"
	"fmt"
	"time"

	"aico-agent/internal/logging"
)

const (
	speakerCaptureChunkTimeoutMs = 1000
	speakerCaptureDrainTimeout   = 2 * time.Second
)

// captureSpeakerEnrollmentPCM records about 4 seconds of board mic PCM16 mono
// at 16 kHz for speaker enrollment. It refuses when another STT test or speaker
// capture already owns the mic.
func (s *Server) captureSpeakerEnrollmentPCM(ctx context.Context) ([]int16, error) {
	if s == nil || s.runtime == nil {
		return nil, fmt.Errorf("speaker capture unavailable")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	s.recordMu.Lock()
	if s.sttConfigTestSession != nil || s.speakerCaptureActive {
		s.recordMu.Unlock()
		return nil, fmt.Errorf("microphone is busy; finish the voice session or STT test first")
	}
	s.speakerCaptureActive = true
	s.recordMu.Unlock()
	defer func() {
		s.recordMu.Lock()
		s.speakerCaptureActive = false
		s.recordMu.Unlock()
	}()

	cfg := s.runtime.ConfigSnapshot()
	audioClient := s.audioClient
	if audioClient == nil {
		audioClient = NewAudioServiceClient(cfg.Audio.SocketOrDefault())
	}
	if health, err := audioClient.Health(); err == nil && health.RecordingActive {
		return nil, fmt.Errorf("microphone is busy; wait for the current recording to finish")
	}

	backend := NewConfiguredAudioRecordingBackend(cfg, audioClient, s.logger)
	format := AudioFormat{
		SampleRate: uint32(speakerSampleRate),
		Channels:   1,
		BitWidth:   16,
	}
	started, err := startRecordingWithRetry(backend, format, recordingStartRetryTimeout, recordingStartRetryInterval)
	if err != nil {
		return nil, fmt.Errorf("start board recording: %w", err)
	}
	sessionID := started.SessionID
	// audio_service keeps the session mapped until a post-stop read returns
	// end_of_stream; without that drain, RecordingActive stays true and the
	// next enroll hits "microphone is busy".
	defer stopSpeakerCaptureSession(backend, sessionID)

	need := speakerEnrollSamples
	samples := make([]int16, 0, need)
	var hasPending bool
	var pending byte
	deadline := time.Now().Add(speakerEmbedTimeout)
	for len(samples) < need {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if time.Now().After(deadline) {
			break
		}
		chunk, err := backend.ReadRecordChunk(sessionID, speakerCaptureChunkTimeoutMs)
		if err != nil {
			return nil, fmt.Errorf("read board recording: %w", err)
		}
		if len(chunk.PCM) > 0 {
			AppendPCM16Samples(chunk.PCM, &samples, &hasPending, &pending)
		}
		if chunk.EndOfStream {
			break
		}
	}
	if len(samples) < speakerWindowSamples {
		return nil, fmt.Errorf("enrollment capture too short; speak closer to the device for about 4 seconds")
	}
	if len(samples) > need {
		samples = samples[:need]
	}
	return samples, nil
}

func stopSpeakerCaptureSession(backend AudioRecordingBackend, sessionID uint64) {
	if backend == nil {
		return
	}
	if err := backend.StopRecording(sessionID); err != nil {
		logging.Warnf("agent", "speaker", "stop enrollment recording failed: %v", err)
	}
	deadline := time.Now().Add(speakerCaptureDrainTimeout)
	for time.Now().Before(deadline) {
		chunk, err := backend.ReadRecordChunk(sessionID, 200)
		if err != nil || chunk == nil || chunk.EndOfStream {
			return
		}
	}
	logging.Warnf("agent", "speaker", "enrollment recording session %d did not drain before timeout", sessionID)
}
