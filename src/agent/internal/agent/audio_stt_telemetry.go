package agent

import (
	"strings"
	"time"
	"unicode/utf8"
)

type sttTurnTelemetry struct {
	startedAt                 time.Time
	provider                  string
	model                     string
	language                  string
	inputMode                 string
	audioDurationMS           int64
	streamingSupported        bool
	streamingReady            bool
	streamingReadyMS          int64
	streamingPreReadyOverflow bool
	streamingUnavailableError string
	streamingUploadError      string
	streamingFinalizeMS       int64
	streamingFinalizeError    string
	usedStreamingTranscript   bool
	fallbackOneShot           bool
	oneShotMS                 int64
	oneShotError              string
	transcript                string
	err                       string
}

func newSTTTurnTelemetry(cfg Config, client STTClient, startedAt time.Time) *sttTurnTelemetry {
	if startedAt.IsZero() {
		startedAt = time.Now().UTC()
	}
	provider := strings.TrimSpace(cfg.STT.Provider)
	if provider == "" {
		provider = defaultSTTProvider
	}
	model := strings.TrimSpace(cfg.STT.Model)
	if model == "" && (provider == "openai" || provider == "openai-whisper") {
		model = defaultSTTModel
	}
	meta := &sttTurnTelemetry{
		startedAt:  startedAt.UTC(),
		provider:   provider,
		model:      model,
		language:   strings.TrimSpace(cfg.STT.Language),
		inputMode:  cfg.InputModeOrDefault(),
		transcript: "",
	}
	if client != nil {
		meta.streamingSupported = client.Capabilities().SupportsStreamingUpload
	}
	return meta
}

func (m *sttTurnTelemetry) event(end time.Time) TaskEpisodeEvent {
	if m == nil {
		return TaskEpisodeEvent{}
	}
	if end.IsZero() {
		end = time.Now().UTC()
	}
	end = end.UTC()
	start := m.startedAt
	if start.IsZero() {
		start = end
	}
	durationMs := end.Sub(start).Milliseconds()
	if durationMs < 0 {
		durationMs = 0
	}
	metadata := map[string]interface{}{
		"provider":                  m.provider,
		"model":                     m.model,
		"language":                  m.language,
		"input_mode":                m.inputMode,
		"audio_duration_ms":         m.audioDurationMS,
		"streaming_supported":       m.streamingSupported,
		"streaming_ready":           m.streamingReady,
		"used_streaming_transcript": m.usedStreamingTranscript,
		"fallback_one_shot":         m.fallbackOneShot,
		"transcript_chars":          utf8.RuneCountInString(strings.TrimSpace(m.transcript)),
	}
	if m.streamingReadyMS > 0 {
		metadata["streaming_ready_ms"] = m.streamingReadyMS
	}
	if m.streamingPreReadyOverflow {
		metadata["streaming_pre_ready_overflow"] = true
	}
	if strings.TrimSpace(m.streamingUnavailableError) != "" {
		metadata["streaming_unavailable_error"] = strings.TrimSpace(m.streamingUnavailableError)
	}
	if strings.TrimSpace(m.streamingUploadError) != "" {
		metadata["streaming_upload_error"] = strings.TrimSpace(m.streamingUploadError)
	}
	if m.streamingFinalizeMS > 0 {
		metadata["streaming_finalize_ms"] = m.streamingFinalizeMS
	}
	if strings.TrimSpace(m.streamingFinalizeError) != "" {
		metadata["streaming_finalize_error"] = strings.TrimSpace(m.streamingFinalizeError)
	}
	if m.oneShotMS > 0 {
		metadata["one_shot_ms"] = m.oneShotMS
	}
	if strings.TrimSpace(m.oneShotError) != "" {
		metadata["one_shot_error"] = strings.TrimSpace(m.oneShotError)
	}

	event := TaskEpisodeEvent{
		Type:       runEventSTTTranscription,
		Role:       "system",
		Ts:         start.Format(time.RFC3339Nano),
		Content:    strings.TrimSpace(m.transcript),
		DurationMs: &durationMs,
		Metadata:   metadata,
	}
	if strings.TrimSpace(m.err) != "" {
		event.IsError = true
		event.Reason = strings.TrimSpace(m.err)
	}
	return event
}

func (d *AudioDialog) updateRecordSTTTelemetry(sessionID uint64, update func(*sttTurnTelemetry)) {
	if d == nil || update == nil {
		return
	}
	d.recordMu.Lock()
	defer d.recordMu.Unlock()
	if !d.recordActive || d.sessionID != sessionID || d.recordSTTTelemetry == nil {
		return
	}
	update(d.recordSTTTelemetry)
}

func (d *AudioDialog) markRecordSTTUploadError(sessionID uint64, err error) {
	if d == nil || err == nil {
		return
	}
	d.updateRecordSTTTelemetry(sessionID, func(meta *sttTurnTelemetry) {
		meta.streamingUploadError = err.Error()
	})
}

func (d *AudioDialog) stashPendingSTTTelemetry(meta *sttTurnTelemetry) {
	if d == nil || meta == nil {
		return
	}
	d.recordMu.Lock()
	d.pendingSTTTelemetry = meta
	d.recordMu.Unlock()
}

func (d *AudioDialog) consumeRecordingTranscriptAndTelemetry() (string, *sttTurnTelemetry) {
	if d == nil {
		return "", nil
	}
	d.recordMu.Lock()
	transcript := strings.TrimSpace(d.recordText)
	d.recordText = ""
	meta := d.pendingSTTTelemetry
	d.pendingSTTTelemetry = nil
	d.recordMu.Unlock()
	return transcript, meta
}
