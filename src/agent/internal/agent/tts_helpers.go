package agent

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/agent/tts"
)

func newTTSProviderManagerFromConfig(cfg Config, logger *Logger) (*tts.ProviderManager, error) {
	if strings.TrimSpace(cfg.TTS.Provider) == "" {
		return nil, nil
	}
	provider, err := tts.New(buildTTSProviderConfig(cfg))
	if err != nil {
		return nil, err
	}
	return tts.NewProviderManager(provider, &ttsLoggerAdapter{logger: logger}), nil
}

// buildTTSProviderConfig converts the agent-level Config into a tts.ProviderConfig.
// If cfg.TTS.Credentials contains an entry for the resolved provider, those
// per-provider values override the top-level [tts] fallbacks.
func buildTTSProviderConfig(cfg Config) tts.ProviderConfig {
	provider := cfg.TTS.Provider
	return buildTTSProviderConfigFor(cfg, provider)
}

// buildTTSProviderConfigFor resolves the config for a specific provider, which
// may be a [tts_providers] record name or a bare provider type -- the config
// page switches by name, the phone switches by type. Per-provider values are
// merged over the top-level [tts] fallbacks.
func buildTTSProviderConfigFor(cfg Config, providerRef string) tts.ProviderConfig {
	// Resolve the record before normalizing: normalizing lowercases, and a
	// record name from the config page may carry capitals.
	record, hasRecord := lookupTTSProviderRecord(cfg, providerRef)

	provider := normalizeTTSProvider(providerRef)
	if hasRecord && strings.TrimSpace(record.Type) != "" {
		provider = normalizeTTSProvider(record.Type)
	}

	// Defaults from the top-level [tts] section. A blank per-provider field
	// falling back to these is the documented contract for [tts] api_key
	// ("used as fallback for any provider"), so it is preserved as-is.
	apiKey := cfg.TTS.APIKey
	voice := cfg.TTS.VoiceID
	emotion := cfg.TTS.Emotion
	model := cfg.TTS.Model
	speed := cfg.TTS.Speed
	referenceID := cfg.TTS.ReferenceID

	// A [tts_providers] record wins. Without this, migration -- which clears the
	// legacy Credentials map -- would make every switch fall back to the ACTIVE
	// provider's key and authenticate against the wrong service.
	//
	// speed is deliberately not per-provider: it stays global on [tts] so
	// changing voice never changes playback speed.
	if hasRecord {
		if strings.TrimSpace(record.APIKey) != "" {
			apiKey = resolveProviderAPIKey(record.APIKey)
		}
		if record.VoiceID != "" {
			voice = record.VoiceID
		}
		if record.Emotion != "" {
			emotion = record.Emotion
		}
		if record.Model != "" {
			model = record.Model
		}
		if record.ReferenceID != "" {
			referenceID = record.ReferenceID
		}
	}

	extra := map[string]any{}
	if emotion != "" {
		extra["emotion"] = emotion
	}
	if model != "" {
		extra["model"] = model
	}
	if referenceID != "" {
		extra["reference_id"] = referenceID
	}

	// Provider-specific parameter cleanup to avoid cross-provider pollution.
	// Each provider should only receive parameters it actually uses.
	switch provider {
	case "fish-audio":
		// Fish Audio uses reference_id exclusively; clear voice to prevent
		// inheriting voice_id from other providers (e.g., Minimax, OpenRouter).
		voice = ""
		// Fish Audio doesn't use emotion
		delete(extra, "emotion")

	case "minimax", "minimax-cn":
		// Minimax uses voice_id and emotion; clear Fish Audio's reference_id
		delete(extra, "reference_id")

	case "volcengine":
		// Volcengine uses voice_id (as speaker) and emotion; clear Fish Audio's reference_id
		delete(extra, "reference_id")

	case "openrouter":
		// OpenRouter uses voice_id; clear Fish Audio's reference_id and provider-specific emotion
		delete(extra, "reference_id")
		delete(extra, "emotion")

	case "alicloud":
		// Alicloud uses voice_id; clear Fish Audio's reference_id and Minimax's emotion
		delete(extra, "reference_id")
		delete(extra, "emotion")

	case "google-cloud":
		// Google Cloud uses voice_id; clear Fish Audio's reference_id and Minimax's emotion
		delete(extra, "reference_id")
		delete(extra, "emotion")
	}

	return tts.ProviderConfig{
		Provider:   provider,
		APIKey:     apiKey,
		Voice:      voice,
		SampleRate: cfg.Audio.SampleRateOrDefault(),
		SpeedRatio: speed,
		Proxy:      ProxyConfigFromEnvironment(),
		Extra:      extra,
	}
}

func ttsPlaybackFormat(cfg Config, caps tts.Capabilities) tts.AudioFormat {
	sampleRate := cfg.Audio.SampleRateOrDefault()
	if len(caps.SupportedSampleRates) == 1 {
		sampleRate = caps.SupportedSampleRates[0]
	} else if len(caps.SupportedSampleRates) > 1 && !containsInt(caps.SupportedSampleRates, sampleRate) {
		sampleRate = caps.SupportedSampleRates[0]
	}
	return tts.AudioFormat{
		SampleRate: sampleRate,
		Channels:   cfg.Audio.ChannelsOrDefault(),
		BitWidth:   cfg.Audio.BitWidthOrDefault(),
	}
}

func ttsPlaybackTargetFormat(cfg Config, source tts.AudioFormat) tts.AudioFormat {
	target := source
	if configuredRate := cfg.Audio.SampleRateOrDefault(); configuredRate > 0 {
		target.SampleRate = configuredRate
	}
	return target
}

func newTTSPlaybackBackendFromConfig(cfg Config, audio *AudioServiceClient, logger *Logger) tts.AudioServiceBackend {
	switch cfg.AudioBackendOrDefault() {
	case AudioBackendLocal:
		return newLocalAudioPlaybackBackend(logger)
	default:
		if audio == nil {
			return nil
		}
		return newAudioBackend(audio)
	}
}

// NewConfiguredAudioPlaybackBackend selects the playback backend resolved from
// cfg for callers that manage their own audio stream lifecycle.
func NewConfiguredAudioPlaybackBackend(cfg Config, audio *AudioServiceClient, logger *Logger) tts.AudioServiceBackend {
	return newTTSPlaybackBackendFromConfig(cfg, audio, logger)
}

func containsInt(values []int, target int) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func beginManagedTTSStream(ctx context.Context, manager *tts.ProviderManager, playback tts.AudioServiceBackend, cfg Config) (*streamSessionWriter, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if playback == nil {
		return nil, errors.New("tts playback backend is not configured")
	}
	holder := manager.Holder()
	var targetSink *tts.AudioServiceSink
	session, err := holder.BeginStreamWithCapabilities(ctx, func(caps tts.Capabilities) tts.AudioSink {
		sourceFormat := ttsPlaybackFormat(cfg, caps)
		targetSink = tts.NewAudioServiceSink(playback, ttsPlaybackTargetFormat(cfg, sourceFormat))
		return tts.NewResamplingSink(sourceFormat, targetSink)
	})
	if err != nil {
		return nil, err
	}
	return &streamSessionWriter{session: session, sink: targetSink}, nil
}

type ttsStreamObserver func(*streamSessionWriter) func()

func speakWithTTSManager(ctx context.Context, manager *tts.ProviderManager, playback tts.AudioServiceBackend, cfg Config, text string) (bool, error) {
	return speakWithTTSManagerObserved(ctx, manager, playback, cfg, text, nil)
}

func speakWithTTSManagerObserved(ctx context.Context, manager *tts.ProviderManager, playback tts.AudioServiceBackend, cfg Config, text string, observe ttsStreamObserver) (bool, error) {
	text = strings.TrimSpace(text)
	if text == "" || manager == nil || playback == nil {
		return false, nil
	}

	// Retry transient network errors
	maxRetries := 2
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		if attempt > 0 {
			// Short backoff between retries
			select {
			case <-ctx.Done():
				return false, ctx.Err()
			case <-time.After(time.Duration(attempt) * 500 * time.Millisecond):
			}
		}

		stream, err := beginManagedTTSStream(ctx, manager, playback, cfg)
		if err != nil {
			lastErr = err
			if isTransientTTSError(err) && attempt < maxRetries {
				continue
			}
			return false, err
		}
		unobserve := func() {}
		if observe != nil {
			if cleanup := observe(stream); cleanup != nil {
				unobserve = cleanup
			}
		}

		if _, err := stream.Write([]byte(text)); err != nil {
			speechStarted := stream.startedPlayback()
			var stopErr error
			if ctx.Err() != nil {
				stream.interrupt()
			}
			_ = stream.closeAndWait()
			speechStarted = speechStarted || stream.startedPlayback()
			if ctx.Err() == nil {
				stopErr = stream.stopPlayback()
			}
			unobserve()
			lastErr = errors.Join(err, stopErr)
			if stopErr == nil && isTransientTTSError(err) && attempt < maxRetries && !speechStarted {
				continue
			}
			return speechStarted, lastErr
		}

		if err := stream.closeAndWait(); err != nil {
			speechStarted := stream.startedPlayback()
			var stopErr error
			if ctx.Err() != nil {
				stream.interrupt()
			} else {
				stopErr = stream.stopPlayback()
			}
			unobserve()
			lastErr = errors.Join(err, stopErr)
			if stopErr == nil && isTransientTTSError(err) && attempt < maxRetries && !speechStarted {
				continue
			}
			return speechStarted, lastErr
		}
		unobserve()

		return stream.spokeSuccessfully(), nil
	}
	return false, lastErr
}

// isTransientTTSError checks if an error is transient and should be retried.
func isTransientTTSError(err error) bool {
	if err == nil {
		return false
	}
	if isRetryableAudioStartPlaybackError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, fragment := range []string{
		"connection refused",
		"connection reset by peer",
		"broken pipe",
		"temporary failure",
		"temporarily unavailable",
		"i/o timeout",
		"eof",
		"dial tcp",
		"context deadline exceeded",
	} {
		if strings.Contains(msg, fragment) {
			return true
		}
	}
	return false
}

// ttsLoggerAdapter bridges the agent Logger to tts.Logger.
type ttsLoggerAdapter struct{ logger *Logger }

func (a *ttsLoggerAdapter) Info(format string, args ...any) {
	if a.logger != nil {
		a.logger.Info(format, args...)
	}
}

func (a *ttsLoggerAdapter) Warn(format string, args ...any) {
	if a.logger != nil {
		a.logger.Warn(format, args...)
	}
}

// streamSessionWriter wraps a tts.StreamSession to satisfy io.Writer.
// LLM streaming output is written byte-slice by byte-slice; we forward each
// fragment to the TTS session immediately.
type streamSessionWriter struct {
	mu           sync.Mutex
	session      tts.StreamSession
	sink         *tts.AudioServiceSink
	cancel       context.CancelFunc
	spoke        bool
	lastErr      error
	interrupted  bool
	textWritten  bool
	terminalOnce sync.Once
	terminalErr  error
}

func (w *streamSessionWriter) setCancel(cancel context.CancelFunc) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.cancel = cancel
}

// Write deliberately returns (0, nil) after interruption or a provider write
// failure so the tag parser can avoid marking speech as emitted without
// interrupting the LLM stream. Do not wrap streamSessionWriter with generic
// io.Writer helpers such as io.Copy or bufio.Writer, which assume short writes
// return a non-nil error.
func (w *streamSessionWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.mu.Lock()
	if w.interrupted {
		w.mu.Unlock()
		// Report zero accepted bytes to the tag parser while still suppressing an
		// error. The parser returns success to the LLM stream but will not mark
		// this speech as emitted, allowing the non-streaming fallback to run.
		return 0, nil
	}
	session := w.session
	w.textWritten = true
	w.mu.Unlock()

	if err := session.WriteText(string(p)); err != nil {
		w.mu.Lock()
		if w.lastErr == nil && !w.interrupted {
			w.lastErr = err
		}
		w.mu.Unlock()
		return 0, nil
	}
	// Always return success so the LLM stream is not interrupted by TTS errors.
	return len(p), nil
}

// Flush asks the provider to synthesize any text it has buffered so a complete
// <tts> block can start playing before the rest of the assistant response has
// finished streaming. As with Write, provider errors are recorded without
// interrupting the LLM response stream.
func (w *streamSessionWriter) Flush() error {
	w.mu.Lock()
	if w.interrupted {
		w.mu.Unlock()
		return nil
	}
	session := w.session
	w.mu.Unlock()

	if err := session.Flush(); err != nil {
		w.mu.Lock()
		if w.lastErr == nil && !w.interrupted {
			w.lastErr = err
		}
		w.mu.Unlock()
	}
	// Keep TTS failures isolated from the LLM stream; closeAndWait reports the
	// recorded error after the model response has completed.
	return nil
}

// ttsBufferResetter is implemented by TTS sessions that buffer text internally
// (e.g. sentence-boundary buffering in non-incremental providers) and can drop
// that buffer on demand.
type ttsBufferResetter interface {
	ResetBuffer()
}

// ResetBuffer discards any text buffered by the underlying TTS session but not
// yet synthesized. Used to prevent residual content from a tool-call turn from
// leaking into a later streaming turn.
func (w *streamSessionWriter) ResetBuffer() {
	w.mu.Lock()
	session := w.session
	w.mu.Unlock()
	if resetter, ok := session.(ttsBufferResetter); ok {
		resetter.ResetBuffer()
	}
}

func (w *streamSessionWriter) interrupt() {
	w.mu.Lock()
	if w.interrupted {
		w.mu.Unlock()
		return
	}
	w.interrupted = true
	w.spoke = false
	sink := w.sink
	cancel := w.cancel
	w.mu.Unlock()

	if sink != nil {
		_ = sink.Stop()
	}
	if cancel != nil {
		cancel()
	}
	_ = w.terminateSession(true)
}

func (w *streamSessionWriter) spokeSuccessfully() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.spoke && !w.interrupted
}

func (w *streamSessionWriter) startedPlayback() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.sink != nil && w.sink.PCMBytes() > 0
}

func (w *streamSessionWriter) stopPlayback() error {
	w.mu.Lock()
	sink := w.sink
	w.mu.Unlock()
	if sink == nil {
		return nil
	}
	return sink.Stop()
}

// emittedSpeech reports whether callers must treat the response as already
// spoken. Once PCM reaches the playback sink, a later stream-close error must
// not cause the full response or a failure replacement to be played again.
func (w *streamSessionWriter) emittedSpeech(closeErr error) bool {
	return w.startedPlayback() || (closeErr == nil && w.spokeSuccessfully())
}

// closeAndWait flushes the session and waits for playback to drain.
func (w *streamSessionWriter) closeAndWait() error {
	w.mu.Lock()
	if w.interrupted {
		w.spoke = false
		w.mu.Unlock()
		_ = w.terminateSession(true)
		return nil
	}
	textWritten := w.textWritten
	w.mu.Unlock()

	var finishErr error
	if textWritten {
		finishErr = tts.FinishInput(w.session)
	}
	closeErr := w.terminateSession(!textWritten)

	w.mu.Lock()
	defer w.mu.Unlock()
	if w.interrupted {
		w.spoke = false
		return nil
	}
	if w.lastErr != nil {
		return w.lastErr
	}
	if finishErr != nil {
		return finishErr
	}
	if closeErr != nil {
		return closeErr
	}
	if w.sink == nil || w.sink.PCMBytes() > 0 {
		w.spoke = true
	}
	return nil
}

func (w *streamSessionWriter) terminateSession(abort bool) error {
	w.terminalOnce.Do(func() {
		if abort {
			w.terminalErr = tts.CancelAndClose(w.session)
			return
		}
		w.terminalErr = w.session.Close()
	})
	return w.terminalErr
}
