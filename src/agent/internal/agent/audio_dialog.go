package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	"aiden-agent/internal/agent/speech"
	"aiden-agent/internal/agent/tts"
	"aiden-agent/internal/logging"
)

const (
	toolContentSpeechTimeout   = 15 * time.Second
	outputInterruptWaitTimeout = 250 * time.Millisecond
)

var (
	recordingStartRetryTimeout             = 5 * time.Second
	recordingStartRetryInterval            = 250 * time.Millisecond
	audioDialogStreamingSTTFinalizeTimeout = 2 * time.Second
)

// AudioDialog manages the audio conversation loop
type AudioDialog struct {
	config              Config
	runtime             *Runtime
	audioClient         *AudioServiceClient
	recordBackend       audioRecordingBackend
	sttClient           STTClient
	ttsManager          *tts.ProviderManager
	ttsPlaybackBackend  tts.AudioServiceBackend
	vad                 *AudioVAD
	recordMu            sync.Mutex
	recordActive        bool
	sessionID           uint64
	recordReader        *AudioRecordChunkReader
	recordSTTMu         sync.Mutex
	recordSTT           *streamingSTTSession
	recordText          string
	recordSTTTelemetry  *sttTurnTelemetry
	pendingSTTTelemetry *sttTurnTelemetry
	speechMu            sync.Mutex
	outputMu            sync.Mutex
	activeOutputs       map[*activeTTSOutput]struct{}
	runControl          voiceRunControl
	messagePublish      func(Message)
	audioArchive        *AudioArchiveManager
	connWarmer          *ConnectionWarmer
}

type activeTTSOutput struct {
	cancel context.CancelFunc
	done   chan struct{}

	mu     sync.Mutex
	stream *streamSessionWriter
}

func newActiveTTSOutput(cancel context.CancelFunc) *activeTTSOutput {
	return &activeTTSOutput{
		cancel: cancel,
		done:   make(chan struct{}),
	}
}

func (o *activeTTSOutput) setStream(stream *streamSessionWriter) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.stream = stream
}

func (o *activeTTSOutput) clearStream(stream *streamSessionWriter) {
	if o == nil {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.stream == stream {
		o.stream = nil
	}
}

func (o *activeTTSOutput) interrupt() {
	if o == nil {
		return
	}
	o.mu.Lock()
	stream := o.stream
	o.mu.Unlock()
	if stream != nil {
		stream.interrupt()
	}
	if o.cancel != nil {
		o.cancel()
	}
}

func (o *activeTTSOutput) finish() {
	if o == nil {
		return
	}
	select {
	case <-o.done:
	default:
		close(o.done)
	}
}

// collectWarmupEndpoints extracts base URLs from LLM, STT, and TTS configurations
func collectWarmupEndpoints(cfg Config) []string {
	seen := make(map[string]bool)
	var endpoints []string

	// LLM endpoint
	if cfg.Model.BaseURL != "" {
		base := extractBaseURL(cfg.Model.BaseURL)
		if !seen[base] {
			seen[base] = true
			endpoints = append(endpoints, base)
		}
	}

	// STT endpoint
	if cfg.STT.BaseURL != "" {
		base := extractBaseURL(cfg.STT.BaseURL)
		if !seen[base] {
			seen[base] = true
			endpoints = append(endpoints, base)
		}
	}

	// TTS endpoints - providers typically use different base URLs
	// Most TTS providers have built-in endpoints, we skip warmup for those

	return endpoints
}

// NewAudioDialog creates a new audio dialog manager backed by Runtime's shared
// TTS provider manager.
func NewAudioDialog(runtime *Runtime) (*AudioDialog, error) {
	if runtime == nil {
		return nil, fmt.Errorf("runtime is required")
	}
	return NewAudioDialogWithConfig(runtime, runtime.ConfigSnapshot())
}

func NewAudioDialogWithConfig(runtime *Runtime, cfg Config) (*AudioDialog, error) {
	// Create audio client
	audioClient := NewAudioServiceClient(cfg.Audio.SocketOrDefault())
	recordBackend := newAudioRecordingBackendFromConfig(cfg, audioClient, nil)

	// Create STT client if needed
	var sttClient STTClient
	if cfg.InputModeOrDefault() == "stt" {
		var err error
		sttClient, err = NewSTTClientFromConfig(cfg)
		if err != nil {
			return nil, err
		}
	}

	// Borrow the process-wide provider manager. Playback and interruption remain
	// dialog-owned, while provider selection has exactly one owner.
	ttsManager := runtime.ttsProviderManager()

	silenceMs := cfg.SilenceMs
	if silenceMs == 0 {
		silenceMs = defaultSilenceMs
	}
	minSpeechMs := cfg.MinSpeechMs
	if minSpeechMs == 0 {
		minSpeechMs = defaultMinSpeechMs
	}

	vad, err := NewAudioVAD(AudioVADConfig{
		SampleRate:      cfg.Audio.SampleRateOrDefault(),
		SilenceMs:       silenceMs,
		MinSpeechMs:     minSpeechMs,
		Backend:         cfg.VADBackend,
		ModelPath:       cfg.VADModelPath,
		HelperPath:      cfg.VADHelperPath,
		SpeechThreshold: cfg.VADSpeechThreshold,
	})
	if err != nil {
		return nil, err
	}

	// Keep a warmer even when endpoints are initially empty: a model reload
	// can enable an endpoint without rebuilding the dialog.
	client := newProxyHTTPClient(ProxyConfigFromEnvironment())
	connWarmer := NewConnectionWarmer(client, collectWarmupEndpoints(cfg))

	return &AudioDialog{
		config:             cfg,
		runtime:            runtime,
		audioClient:        audioClient,
		recordBackend:      recordBackend,
		sttClient:          sttClient,
		ttsManager:         ttsManager,
		ttsPlaybackBackend: newTTSPlaybackBackendFromConfig(cfg, audioClient, nil),
		vad:                vad,
		audioArchive:       NewAudioArchiveManager(cfg.AudioArchive),
		connWarmer:         connWarmer,
	}, nil
}

// SetStorageManager keeps new archived recordings on eMMC and makes older
// recordings eligible for watermark-driven migration to SD.
func (d *AudioDialog) SetStorageManager(sm *StorageManager) {
	if d.audioArchive != nil {
		d.audioArchive.SetStorageManager(sm)
	}
}

func (d *AudioDialog) SetStorageMonitor(monitor *StorageMonitor) {
	if d.audioArchive != nil {
		d.audioArchive.SetStorageMonitor(monitor)
	}
}

// PrepareInput initializes the staged VAD helper with silence without opening
// the microphone. A bad backend/model path must fail reload before publication.
func (d *AudioDialog) PrepareInput() error {
	if d == nil || d.vad == nil {
		return nil
	}
	if _, err := d.vad.scorer.Score(make([]int16, d.vad.FrameSamples())); err != nil {
		return fmt.Errorf("prepare VAD: %w", err)
	}
	return d.vad.Reset()
}

// PrepareInputReplacement stages input after the previous voice loop has
// drained. Unchanged VAD belongs to the previous dialog until commit, so a
// provider switch neither opens the NPU again nor destroys rollback state.
// The caller must serialize commit with preemption and close the old dialog
// only after calling the returned function. On rollback, close only d.
func (d *AudioDialog) PrepareInputReplacement(previous *AudioDialog) (func(), error) {
	if d != nil && previous != nil && d != previous && d.vad != nil && previous.vad != nil &&
		d.vadConfig() == previous.vadConfig() {
		return func() {
			_ = d.vad.Close() // The staged helper has not been started.
			d.vad, previous.vad = previous.vad, nil
		}, nil
	}
	return nil, d.PrepareInput()
}

func (d *AudioDialog) vadConfig() AudioVADConfig {
	backend := d.config.VADBackendOrDefault()
	modelPath := strings.TrimSpace(d.config.VADModelPath)
	if modelPath == "" {
		modelPath = defaultVADModelPath
	}
	helperPath := ResolveVADHelperPath(backend, d.config.VADHelperPath)
	silenceMs := d.config.SilenceMs
	if silenceMs <= 0 {
		silenceMs = defaultSilenceMs
	}
	minSpeechMs := d.config.MinSpeechMs
	if minSpeechMs <= 0 {
		minSpeechMs = defaultMinSpeechMs
	}
	threshold := d.config.VADSpeechThreshold
	if threshold <= 0 {
		threshold = defaultVADSpeechThreshold
	}
	return AudioVADConfig{
		SampleRate:      d.config.Audio.SampleRateOrDefault(),
		SilenceMs:       silenceMs,
		MinSpeechMs:     minSpeechMs,
		Backend:         backend,
		ModelPath:       modelPath,
		HelperPath:      helperPath,
		SpeechThreshold: threshold,
	}
}

// Close releases resources owned by the dialog. The TTS provider manager is
// borrowed from Runtime and must remain available until Runtime.Close.
func (d *AudioDialog) Close() error {
	if d == nil {
		return nil
	}

	d.InterruptOutput()
	stopErr := d.StopRecording()
	var vadErr error
	if d.vad != nil {
		vadErr = d.vad.Close()
	}
	return errors.Join(stopErr, vadErr)
}

// StartRecording starts an audio recording session
func (d *AudioDialog) StartRecording() error {
	d.recordMu.Lock()
	defer d.recordMu.Unlock()

	if d.recordActive {
		return nil
	}

	// Reset VAD up front so a failed helper aborts before any audio/STT
	// resources are allocated. Otherwise a successful start_recording plus
	// streaming STT session would leak when the VAD helper has crashed.
	if d.vad != nil {
		if err := d.vad.Reset(); err != nil {
			return fmt.Errorf("reset vad: %w", err)
		}
	}

	recordStartedAt := time.Now().UTC()
	logging.Infof("agent", "audio", "Opening record session...")
	format := AudioFormat{
		SampleRate: uint32(d.config.Audio.SampleRateOrDefault()),
		Channels:   uint32(d.config.Audio.ChannelsOrDefault()),
		BitWidth:   uint32(d.config.Audio.BitWidthOrDefault()),
	}

	result, err := startRecordingWithRetry(recordingBackendOrService(d.recordBackend, d.audioClient), format, recordingStartRetryTimeout, recordingStartRetryInterval)
	if err != nil {
		return fmt.Errorf("start recording: %w", err)
	}
	d.sessionID = result.SessionID
	if d.config.AudioBackendOrDefault() != AudioBackendLocal {
		if reader, err := d.audioClient.OpenRecordChunkReader(result.SessionID); err == nil {
			d.recordReader = reader
		} else {
			logging.Warnf("agent", "audio", "Persistent record reader unavailable, using per-request reads: %v", err)
		}
	}
	d.recordText = ""
	d.recordSTTTelemetry = newSTTTurnTelemetry(d.config, d.sttClient, recordStartedAt)
	d.recordActive = true

	// Play the cue tone immediately so the user gets fast feedback.
	d.playPromptSoundAsync(promptSoundRecordingStart, "recording")

	// Warmup connections in the background during the recording gap
	// This saves TLS handshake time for the upcoming LLM/STT/TTS requests
	if d.connWarmer != nil {
		cfg := d.config
		if d.runtime != nil {
			// Model settings reload independently; STT still belongs to this dialog.
			cfg.Model = d.runtime.ConfigSnapshot().Model
		}
		d.connWarmer.SetEndpoints(collectWarmupEndpoints(cfg))
		d.connWarmer.WarmupAsync(context.Background())
	}

	// Start STT streaming session asynchronously — the network handshake
	// (WebSocket dial + session setup) can take 1-3s and should not delay
	// the audible cue. Chunks are buffered by uploadRecordChunkToStreamingSTT
	// which checks currentRecordSTT() and silently drops if not yet ready.
	sttSessionID := result.SessionID
	go func() {
		recordSTT, err := beginStreamingSTTSession(context.Background(), d.sttClient, STTStreamConfig{
			SampleRate: d.config.Audio.SampleRateOrDefault(),
			Channels:   d.config.Audio.ChannelsOrDefault(),
			BitWidth:   d.config.Audio.BitWidthOrDefault(),
		})
		if err != nil {
			d.updateRecordSTTTelemetry(sttSessionID, func(meta *sttTurnTelemetry) {
				meta.streamingUnavailableError = err.Error()
			})
			logging.Warnf("agent", "stt", "streaming upload unavailable, falling back to one-shot STT: %v", err)
			return
		}
		if recordSTT == nil {
			return
		}
		// Guard: if recording was stopped before STT connected, discard.
		d.recordMu.Lock()
		stillActive := d.recordActive && d.sessionID == sttSessionID
		d.recordMu.Unlock()
		if !stillActive {
			_ = recordSTT.Close()
			return
		}
		logging.Infof("agent", "stt", "streaming upload enabled for realtime transcription")
		streamReadyAt := time.Now().UTC()
		d.updateRecordSTTTelemetry(sttSessionID, func(meta *sttTurnTelemetry) {
			meta.streamingReady = true
			meta.streamingReadyMS = streamReadyAt.Sub(meta.startedAt).Milliseconds()
		})
		d.setRecordSTT(recordSTT)
	}()

	return nil
}

// StopRecording stops the current recording session
func (d *AudioDialog) StopRecording() error {
	d.recordMu.Lock()
	if !d.recordActive {
		d.recordMu.Unlock()
		return nil
	}

	sessionID := d.sessionID
	reader := d.recordReader
	recordSTT := d.takeRecordSTT()
	sttTelemetry := d.recordSTTTelemetry
	d.recordSTTTelemetry = nil
	d.recordActive = false
	d.sessionID = 0
	d.recordReader = nil
	d.recordMu.Unlock()

	if recordSTT != nil {
		defer func() {
			_ = recordSTT.Close()
		}()
	}

	if reader != nil {
		_ = reader.Close()
	}

	if err := recordingBackendOrService(d.recordBackend, d.audioClient).StopRecording(sessionID); err != nil {
		return fmt.Errorf("stop recording: %w", err)
	}
	if recordSTT != nil {
		finalizeStart := time.Now()
		transcript, err := recordSTT.FinalizeWithTimeout(audioDialogStreamingSTTFinalizeTimeout)
		finalizeDurationMs := time.Since(finalizeStart).Milliseconds()
		if sttTelemetry == nil {
			sttTelemetry = newSTTTurnTelemetry(d.config, d.sttClient, finalizeStart)
		}
		sttTelemetry.streamingFinalizeMS = finalizeDurationMs
		if err != nil {
			sttTelemetry.streamingFinalizeError = err.Error()
			logging.Warnf("agent", "stt", "finalize streaming transcript failed, falling back to one-shot STT: %v", err)
		} else {
			sttTelemetry.transcript = transcript
			sttTelemetry.usedStreamingTranscript = strings.TrimSpace(transcript) != ""
			d.recordMu.Lock()
			d.recordText = transcript
			d.recordMu.Unlock()
		}
	}
	d.stashPendingSTTTelemetry(sttTelemetry)

	logging.Infof("agent", "audio", "Record session closed")
	return nil
}

func (d *AudioDialog) RecordingActive() bool {
	d.recordMu.Lock()
	defer d.recordMu.Unlock()
	return d.recordActive
}

// ReadRecordChunk reads a PCM chunk from the recording session
func (d *AudioDialog) ReadRecordChunk(timeoutMs uint32) (*AudioChunkResult, error) {
	d.recordMu.Lock()
	if !d.recordActive || d.sessionID == 0 {
		d.recordMu.Unlock()
		return nil, fmt.Errorf("recording is not active")
	}
	reader := d.recordReader
	sessionID := d.sessionID
	d.recordMu.Unlock()

	if reader != nil {
		chunk, err := reader.Read(timeoutMs)
		if err == nil {
			d.uploadRecordChunkToStreamingSTT(sessionID, chunk)
			return chunk, nil
		}
		logging.Warnf("agent", "audio", "Persistent record reader failed, falling back to per-request reads: %v", err)
		d.recordMu.Lock()
		if d.recordReader == reader {
			_ = d.recordReader.Close()
			d.recordReader = nil
		}
		d.recordMu.Unlock()
	}
	chunk, err := recordingBackendOrService(d.recordBackend, d.audioClient).ReadRecordChunk(sessionID, timeoutMs)
	if err == nil {
		d.uploadRecordChunkToStreamingSTT(sessionID, chunk)
	}
	return chunk, err
}

func (d *AudioDialog) uploadRecordChunkToStreamingSTT(sessionID uint64, chunk *AudioChunkResult) {
	if d == nil || chunk == nil || len(chunk.PCM) == 0 {
		return
	}
	recordSTT := d.currentRecordSTT()
	if recordSTT == nil {
		return
	}
	if err := recordSTT.UploadPCM(chunk.PCM); err != nil {
		d.markRecordSTTUploadError(sessionID, err)
		logging.Warnf("agent", "stt", "streaming upload failed, falling back to one-shot STT: %v", err)
		// Only clear if this is still the active session; StopRecording may have
		// already swapped it out from another goroutine.
		if d.clearRecordSTT(recordSTT) {
			_ = recordSTT.Close()
		}
	}
}

// setRecordSTT installs the active streaming STT session.
func (d *AudioDialog) setRecordSTT(session *streamingSTTSession) {
	d.recordSTTMu.Lock()
	d.recordSTT = session
	d.recordSTTMu.Unlock()
}

// currentRecordSTT returns the active streaming STT session, if any.
func (d *AudioDialog) currentRecordSTT() *streamingSTTSession {
	d.recordSTTMu.Lock()
	defer d.recordSTTMu.Unlock()
	return d.recordSTT
}

// takeRecordSTT clears and returns the active streaming STT session, giving the
// caller sole ownership so it can be closed without racing other goroutines.
func (d *AudioDialog) takeRecordSTT() *streamingSTTSession {
	d.recordSTTMu.Lock()
	defer d.recordSTTMu.Unlock()
	session := d.recordSTT
	d.recordSTT = nil
	return session
}

// clearRecordSTT clears the active session only if it still matches the given
// one, returning whether the caller now owns it. This prevents clobbering a
// session started concurrently by a new recording.
func (d *AudioDialog) clearRecordSTT(session *streamingSTTSession) bool {
	d.recordSTTMu.Lock()
	defer d.recordSTTMu.Unlock()
	if d.recordSTT != session {
		return false
	}
	d.recordSTT = nil
	return true
}

// ProcessVADFrame processes an audio frame through VAD.
func (d *AudioDialog) ProcessVADFrame(samples []int16) ([]int16, error) {
	return d.vad.Process(samples)
}

// FlushVAD flushes any buffered audio from VAD
func (d *AudioDialog) FlushVAD() []int16 {
	return d.vad.Flush()
}

// FinishPendingUtterance flushes VAD state and appends samples that were not
// yet aligned to a full VAD frame when a wakeup press stops recording.
func (d *AudioDialog) FinishPendingUtterance(pending []int16) []int16 {
	frameSamples := d.VADFrameSamples()
	consumed := 0
	for consumed+frameSamples <= len(pending) {
		if _, err := d.ProcessVADFrame(pending[consumed : consumed+frameSamples]); err != nil {
			logging.Warnf("agent", "vad", "finish pending utterance failed: %v", err)
			break
		}
		consumed += frameSamples
	}
	tail := pending[consumed:]
	utterance := d.FlushVAD()
	if len(tail) == 0 {
		return utterance
	}
	if len(utterance) == 0 {
		return append([]int16(nil), tail...)
	}
	return append(utterance, tail...)
}

// ResetVAD resets the VAD state
func (d *AudioDialog) ResetVAD() {
	if err := d.vad.Reset(); err != nil {
		logging.Warnf("agent", "vad", "reset failed: %v", err)
	}
}

// VADFrameSamples returns the number of samples to feed into each VAD frame.
func (d *AudioDialog) VADFrameSamples() int {
	return d.vad.FrameSamples()
}

func (d *AudioDialog) VADDebugState() VADDebugState {
	return d.vad.DebugState()
}

func (d *AudioDialog) registerActiveOutput(output *activeTTSOutput) func() {
	if output == nil {
		return func() {}
	}
	d.outputMu.Lock()
	if d.activeOutputs == nil {
		d.activeOutputs = make(map[*activeTTSOutput]struct{})
	}
	d.activeOutputs[output] = struct{}{}
	d.outputMu.Unlock()

	var once sync.Once
	return func() {
		once.Do(func() {
			d.outputMu.Lock()
			delete(d.activeOutputs, output)
			d.outputMu.Unlock()
			output.finish()
		})
	}
}

func (d *AudioDialog) snapshotActiveOutputs() []*activeTTSOutput {
	d.outputMu.Lock()
	defer d.outputMu.Unlock()
	if len(d.activeOutputs) == 0 {
		return nil
	}
	outputs := make([]*activeTTSOutput, 0, len(d.activeOutputs))
	for output := range d.activeOutputs {
		outputs = append(outputs, output)
	}
	return outputs
}

func (d *AudioDialog) beginManagedTTSStreamForRun(ctx context.Context) (*streamSessionWriter, func(), func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	streamCtx, streamCancel := context.WithCancel(ctx)
	manager := d.currentTTSManager()
	if manager == nil {
		streamCancel()
		return nil, nil, nil, errTTSNotConfigured
	}
	stream, err := beginManagedTTSStream(streamCtx, manager, d.currentTTSPlaybackBackend(), d.config)
	if err != nil {
		streamCancel()
		return nil, nil, nil, err
	}
	stream.setCancel(streamCancel)
	output := newActiveTTSOutput(streamCancel)
	output.setStream(stream)

	var lifecycleMu sync.Mutex
	var unregister func()
	activated := false
	cleaned := false
	activate := func() {
		lifecycleMu.Lock()
		defer lifecycleMu.Unlock()
		if activated || cleaned {
			return
		}
		activated = true
		unregister = d.registerActiveOutput(output)
	}
	cleanup := func() {
		lifecycleMu.Lock()
		if cleaned {
			lifecycleMu.Unlock()
			return
		}
		cleaned = true
		unregisterOutput := unregister
		lifecycleMu.Unlock()
		if unregisterOutput != nil {
			unregisterOutput()
		}
		streamCancel()
	}
	return stream, activate, cleanup, nil
}

// InterruptOutput immediately stops any TTS output owned by this dialog.
func (d *AudioDialog) InterruptOutput() {
	if d == nil {
		return
	}

	outputs := d.snapshotActiveOutputs()
	for _, output := range outputs {
		output.interrupt()
	}
	waitForActiveOutputs(outputs, outputInterruptWaitTimeout)
}

func waitForActiveOutputs(outputs []*activeTTSOutput, timeout time.Duration) {
	if len(outputs) == 0 || timeout <= 0 {
		return
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	for _, output := range outputs {
		select {
		case <-output.done:
		case <-timer.C:
			return
		}
	}
}

// ProcessUtterance processes a detected utterance
func (d *AudioDialog) ProcessUtterance(ctx context.Context, utterance []int16, runtime *Runtime) error {
	input, err := d.PrepareTurnInput(utterance)
	if err != nil {
		return err
	}
	result, err := d.RunVoiceTurn(ctx, input, utterance, runtime)
	if err != nil {
		if !result.SpeechStreamed {
			prepared := runtime.PrepareSpokenText(ctx, SpokenTextInput{TurnFailure: result.TurnFailure})
			if prepared.Text != "" {
				if speakErr := d.SpeakFinal(ctx, prepared.Text, nil); speakErr != nil {
					logging.Errorf("agent", "audio_dialog", "failure replacement TTS failed: %v", speakErr)
				}
			}
		}
		return err
	}
	if result.SpeechStreamed {
		return nil
	}
	prepared := runtime.PrepareSpokenText(ctx, SpokenTextInput{
		ResponseText:   result.SpokenTextForConfig(d.config),
		TailAppendable: d.CanSpeakFinalText(),
	})
	err = d.SpeakFinal(ctx, prepared.Text, nil)
	runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, err)
	return err
}

// SetMessagePublisher sends voice conversation updates to transient consumers
// such as the Web UI without persisting a separate conversation log.
func (d *AudioDialog) SetMessagePublisher(publish func(Message)) {
	d.messagePublish = publish
}

func (d *AudioDialog) publishVoiceUserInput(input TurnInput, utterance []int16, episodeID, requestID string) {
	if d.messagePublish == nil {
		return
	}
	input = d.ensureVoiceInputAudioArtifact(input, utterance, nil)
	content := strings.TrimSpace(input.InputText)
	if content == "" {
		content = strings.TrimSpace(input.Transcript)
	}
	if content == "" {
		return
	}
	message := messageFromTurnInput(input, episodeID, requestID, nil, time.Now())
	message.Content = content
	message.Source = "voice"
	d.publishVoiceMessage(message, requestID)
}

func (d *AudioDialog) publishVoiceRunEvent(event RunEvent, requestID string) {
	if d.messagePublish == nil || !d.runControl.isActiveRequest(requestID) {
		return
	}
	var message Message
	if event.Type == runEventReasoningDelta || event.Type == runEventReasoningReset {
		message = messageFromReasoningEvent(event, event.EpisodeID, requestID)
	} else {
		message = messageFromRunEvent(event, event.EpisodeID, requestID)
	}
	if message.Type == "" {
		return
	}
	message.Source = "voice"
	d.publishVoiceMessage(message, requestID)
}

func (d *AudioDialog) publishVoiceMessage(message Message, requestID string) {
	if requestID != "" && !d.runControl.isActiveRequest(requestID) {
		return
	}
	message, ok := normalizePublicMessage(message)
	if !ok {
		return
	}
	d.messagePublish(message)
}

func (d *AudioDialog) PrepareTurnInput(utterance []int16) (TurnInput, error) {
	duration := float64(len(utterance)) / float64(d.config.Audio.SampleRateOrDefault())
	logging.Infof("agent", "utterance", "%.1fs of speech", duration)

	// Convert to WAV
	wavData := pcm16MonoToWAV(utterance, d.config.Audio.SampleRateOrDefault())
	logging.Debugf("agent", "audio_dialog", "WAV size: %d bytes", len(wavData))

	transcriptHint, sttTelemetry := d.consumeRecordingTranscriptAndTelemetry()
	inputMode := d.config.InputModeOrDefault()
	if inputMode == TurnModalitySTT && sttTelemetry == nil {
		sttTelemetry = newSTTTurnTelemetry(d.config, d.sttClient, time.Now().UTC())
	}
	if sttTelemetry != nil {
		sttTelemetry.audioDurationMS = int64(duration * 1000)
	}
	transcribeStart := time.Now()
	audioInput, err := PrepareAudioInput(inputMode, d.sttClient, wavData, transcriptHint, "", nil)
	transcribeDurationMs := time.Since(transcribeStart).Milliseconds()
	if err != nil {
		return TurnInput{}, err
	}
	if sttTelemetry != nil && inputMode == TurnModalitySTT {
		if strings.TrimSpace(transcriptHint) == "" {
			sttTelemetry.fallbackOneShot = true
			sttTelemetry.oneShotMS = transcribeDurationMs
		} else {
			sttTelemetry.usedStreamingTranscript = true
		}
		sttTelemetry.transcript = audioInput.Transcript
		audioInput.TelemetryEvents = append(audioInput.TelemetryEvents, sttTelemetry.event(time.Now().UTC()))
	}
	audioInput = d.ensureVoiceInputAudioArtifact(audioInput, utterance, wavData)
	if audioInput.Transcript != "" {
		logging.Infof("agent", "stt", "Transcript: %s", audioInput.Transcript)
	}
	return audioInput, nil
}

func (d *AudioDialog) ensureVoiceInputAudioArtifact(input TurnInput, utterance []int16, wavData []byte) TurnInput {
	input = normalizeTurnInput(input)
	if artifact, ok := firstAudioArtifact(input); ok {
		if artifact.Path != "" {
			return input
		}
		if d.audioArchive == nil || !d.audioArchive.config.Enabled {
			if artifact.DurationMS > 0 || artifact.Size > 0 {
				return input
			}
		}
	}
	if d.audioArchive == nil {
		return input
	}

	audioPath, audioDuration, err := d.audioArchive.SaveAudio(utterance, d.config.Audio.SampleRateOrDefault())
	if err != nil {
		logging.Warnf("agent", "audio_archive", "save failed: %v", err)
		return input
	}
	size := int64(0)
	if audioPath != "" {
		if info, statErr := os.Stat(audioPath); statErr == nil {
			size = info.Size()
		}
	}
	if size == 0 && len(wavData) > 0 {
		size = int64(len(wavData))
	}
	if audioDuration == 0 && len(wavData) > 0 {
		audioDuration = int(wavDurationMS(wavData))
	}
	if audioPath == "" {
		return input
	}
	return withAudioArtifactPath(input, audioPath, int64(audioDuration), size)
}

func (d *AudioDialog) RunAgentTurn(ctx context.Context, input TurnInput, runtime *Runtime) (RunResult, error) {
	episodeID := ""
	if runtime != nil {
		episodeID = runtime.NewEpisodeID()
	}
	requestID := createVoiceRequestID()
	if !d.beginVoiceRunControl(requestID) {
		return RunResult{}, fmt.Errorf("voice run already active")
	}
	defer d.endVoiceRunControl(requestID)
	return d.runAgentTurnWithActiveRequest(ctx, input, runtime, episodeID, requestID, VoiceTurnContext{})
}

func (d *AudioDialog) RunVoiceTurn(ctx context.Context, input TurnInput, utterance []int16, runtime *Runtime) (RunResult, error) {
	return d.RunVoiceTurnWithContext(ctx, input, utterance, runtime, VoiceTurnContext{})
}

func (d *AudioDialog) RunVoiceTurnWithContext(ctx context.Context, input TurnInput, utterance []int16, runtime *Runtime, turnContext VoiceTurnContext) (RunResult, error) {
	episodeID := ""
	if runtime != nil {
		episodeID = runtime.NewEpisodeID()
	}
	requestID := createVoiceRequestID()
	if !d.beginVoiceRunControl(requestID) {
		return RunResult{}, fmt.Errorf("voice run already active")
	}
	defer d.endVoiceRunControl(requestID)
	d.publishVoiceUserInput(input, utterance, episodeID, requestID)
	result, err := d.runAgentTurnWithActiveRequest(ctx, input, runtime, episodeID, requestID, turnContext)
	if err != nil {
		return RunResult{}, err
	}
	return result, nil
}

func (d *AudioDialog) runAgentTurnWithActiveRequest(ctx context.Context, input TurnInput, runtime *Runtime, episodeID, requestID string, turnContext VoiceTurnContext) (RunResult, error) {
	turnContext = normalizeVoiceTurnContext(turnContext)

	promptStartedAt := time.Now().UTC()
	promptDispatchStartedAt := time.Now()
	d.playPromptSoundAsyncWithWait(promptSoundAgentSend, "agent send", false)
	promptDispatchDuration := time.Since(promptDispatchStartedAt)
	promptCueDuration := time.Duration(promptSoundDurationMS(promptSoundAgentSend)) * time.Millisecond
	input.TelemetryEvents = append(input.TelemetryEvents, voicePreRunTelemetryEvent(
		runEventVoicePromptSound,
		"agent send",
		promptStartedAt,
		promptCueDuration,
		map[string]interface{}{
			"prompt":                "agent_send",
			"wait":                  false,
			"async":                 true,
			"cue_audio_duration_ms": int64(promptCueDuration / time.Millisecond),
			"dispatch_duration_ms":  promptDispatchDuration.Milliseconds(),
		},
		nil,
	))
	var finalAssistantEvent *RunEvent

	var speechWriter *speech.StreamWriter
	req := RunRequest{
		Input:          input.InputText,
		Attachments:    input.Attachments,
		Turn:           input,
		RequestID:      requestID,
		RuntimeContext: turnContext.RuntimeContext,
		MaxTokens:      d.config.VoiceMaxResponseTokensOrDefault(),
		EventHandler: func(event RunEvent) {
			if event.Type == "assistant_output" {
				captured := event
				finalAssistantEvent = &captured
				return
			}
			toolSpeechStreamed := event.Type == runEventToolCall && speechWriter.FinalizeResponse()
			d.publishVoiceRunEvent(event, requestID)
			if toolSpeechStreamed {
				logging.Infof("agent", "tts", "Tool content already streamed: tool=%s", event.ToolName)
			} else {
				d.HandleRunEvent(ctx, event)
			}
		},
		SteerProvider: func(ctx context.Context) (RunSteerMessage, bool) {
			return d.consumePendingSteer(requestID)
		},
		FinalSteerProvider: func(ctx context.Context) (RunSteerMessage, bool) {
			return d.consumeFinalPendingSteer(requestID)
		},
		SteerInterrupt: func() <-chan struct{} {
			return d.steerInterruptChannel(requestID)
		},
		SteerWaiter: func(ctx context.Context) (RunSteerMessage, bool, error) {
			return d.waitForSteerInterrupt(ctx, requestID)
		},
	}
	req.EpisodeID = strings.TrimSpace(episodeID)

	var newStream *streamSessionWriter
	if manager := d.currentTTSManager(); d.config.VoiceStreamingTTSEnabledOrDefault() && manager != nil {
		preopenStartedAt := time.Now().UTC()
		stream, activate, cleanup, err := d.beginManagedTTSStreamForRun(ctx)
		metadata := map[string]interface{}{
			"provider": manager.Current(),
		}
		input.TelemetryEvents = append(input.TelemetryEvents, voicePreRunTelemetryEvent(
			runEventTTSStreamPreopen,
			"preopen TTS stream",
			preopenStartedAt,
			time.Since(preopenStartedAt),
			metadata,
			err,
		))
		if err != nil {
			logging.Errorf("agent", "audio_dialog", "TTS BeginStream failed: %v", err)
		} else {
			newStream = stream
			defer cleanup()
			req.OnRunActive = func(context.Context) {
				activate()
			}
			speechWriter = speech.NewStreamWriter(newStream)
			req.StreamWriter = speechWriter
		}
	}
	req.Turn = input

	result, err := runtime.Run(ctx, req)
	d.stopAcceptingSteer(requestID)
	if newStream != nil {
		finalSpeechStreamed := speechWriter.FinalizeResponse()
		closeErr := newStream.closeAndWait()
		if closeErr != nil {
			logging.Errorf("agent", "audio_dialog", "new TTS stream failed: %v", closeErr)
		}
		result.SpeechStreamed = finalSpeechStreamed && newStream.emittedSpeech(closeErr)
	}
	if err != nil {
		return result, fmt.Errorf("LLM request failed: %w", err)
	}
	if finalAssistantEvent != nil {
		d.publishVoiceRunEvent(*finalAssistantEvent, requestID)
	}
	logging.Infof("agent", "llm", "Response received")
	return result, nil
}

func (d *AudioDialog) beginVoiceRunControl(requestID string) bool {
	if d == nil {
		return false
	}
	return d.runControl.begin(requestID)
}

func (d *AudioDialog) endVoiceRunControl(requestID string) {
	if d == nil {
		return
	}
	d.runControl.end(requestID)
}

func (d *AudioDialog) WaitForVoiceRunIdle(ctx context.Context) bool {
	if d == nil {
		return true
	}
	return d.runControl.waitUntilInactive(ctx)
}

func (d *AudioDialog) ForceResetVoiceRun() {
	if d == nil {
		return
	}
	d.runControl.forceReset()
}

func (d *AudioDialog) QueueSteer(input TurnInput) bool {
	content := steerContentFromTurnInput(input)
	if content == "" {
		return false
	}
	queued, ok := d.runControl.queueSteer(content)
	if !ok {
		return false
	}
	if queued.interrupted {
		logging.Infof("agent", "steer", "Queued interrupted voice steer: request_id=%s len=%d", queued.requestID, queued.contentLength)
		return true
	}
	logging.Infof("agent", "steer", "Queued voice steer: request_id=%s len=%d", queued.requestID, queued.contentLength)
	return true
}

func (d *AudioDialog) BeginSteerInterrupt() bool {
	if d == nil {
		return false
	}
	requestID, started, ok := d.runControl.beginInterrupt()
	if !ok {
		return false
	}
	if started {
		logging.Infof("agent", "steer", "Voice run interrupted, waiting for steering input: request_id=%s", requestID)
	}
	return true
}

func (d *AudioDialog) ResumeSteerInterrupt() bool {
	if d == nil {
		return false
	}
	requestID, ok := d.runControl.resumeInterrupt()
	if !ok {
		return false
	}
	logging.Infof("agent", "steer", "Voice steer interruption resumed without input: request_id=%s", requestID)
	return true
}

func (d *AudioDialog) consumePendingSteer(requestID string) (RunSteerMessage, bool) {
	if d == nil {
		return RunSteerMessage{}, false
	}
	return d.runControl.consumePending(requestID)
}

func (d *AudioDialog) consumeFinalPendingSteer(requestID string) (RunSteerMessage, bool) {
	if d == nil {
		return RunSteerMessage{}, false
	}
	return d.runControl.consumeFinalPending(requestID)
}

func (d *AudioDialog) stopAcceptingSteer(requestID string) {
	if d == nil {
		return
	}
	d.runControl.stopAccepting(requestID)
}

func (d *AudioDialog) steerInterruptChannel(requestID string) <-chan struct{} {
	if d == nil {
		return nil
	}
	return d.runControl.interruptChannel(requestID)
}

func (d *AudioDialog) waitForSteerInterrupt(ctx context.Context, requestID string) (RunSteerMessage, bool, error) {
	if d == nil {
		return RunSteerMessage{}, false, nil
	}
	return d.runControl.waitForInterrupt(ctx, requestID)
}

func (d *AudioDialog) HandleRunEvent(ctx context.Context, event RunEvent) {
	if event.Type != runEventToolCall || !d.config.VoiceToolCallSpeechOrDefault() || event.ToolName == toolWaitForWakeup {
		return
	}
	if text := speech.BuildText(event.Content); text != "" {
		go d.SpeakToolContent(text)
	}
}

func (d *AudioDialog) SpeakToolContent(content string) {
	content = strings.TrimSpace(content)
	if content == "" {
		return
	}
	// Detached from the agent turn context so tool TTS is not cut off when
	// runtime.Run returns or the parent context is cancelled.
	if err := d.speak(context.Background(), content, nil, toolContentSpeechTimeout, false); err != nil {
		logging.Errorf("agent", "audio_dialog", "Tool content TTS failed: %v", err)
	}
}

func (d *AudioDialog) Speak(ctx context.Context, text string, interrupt <-chan struct{}) error {
	return d.speak(ctx, text, interrupt, 0, false)
}

func (d *AudioDialog) SpeakFinal(ctx context.Context, text string, interrupt <-chan struct{}) error {
	return d.speak(ctx, text, interrupt, 0, true)
}

func (d *AudioDialog) CanSpeakFinalText() bool {
	return d != nil && d.currentTTSPlaybackBackend() != nil && (d.currentTTSManager() != nil || canPlayTTSUnavailableFallback(d.config, ""))
}

func (d *AudioDialog) currentTTSManager() *tts.ProviderManager {
	if d == nil || d.ttsManager == nil || d.ttsManager.Current() == "" {
		return nil
	}
	return d.ttsManager
}

func (d *AudioDialog) currentTTSPlaybackBackend() tts.AudioServiceBackend {
	if d == nil {
		return nil
	}
	if d.ttsPlaybackBackend != nil {
		if backend, ok := d.ttsPlaybackBackend.(*audioBackend); ok && d.audioClient != nil && backend.c != d.audioClient {
			return newAudioBackend(d.audioClient)
		}
		return d.ttsPlaybackBackend
	}
	if d.audioClient == nil {
		return nil
	}
	return newAudioBackend(d.audioClient)
}

func (d *AudioDialog) speak(ctx context.Context, text string, interrupt <-chan struct{}, timeoutAfterLock time.Duration, allowFallback bool) error {
	if ctx == nil {
		ctx = context.Background()
	}
	baseCtx := ctx
	cancelInterrupt := func() {}
	if interrupt != nil {
		interruptCtx, cancel := context.WithCancel(ctx)
		baseCtx = interruptCtx
		cancelInterrupt = cancel
		defer cancelInterrupt()
		go func() {
			select {
			case <-interrupt:
				cancelInterrupt()
			case <-interruptCtx.Done():
			}
		}()
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	_ = logging.LogEvent(logging.Info, "agent", "reply", "assistant_output",
		logging.Field{Key: "message", Value: text})
	if d.currentTTSManager() == nil && !allowFallback {
		return nil
	}
	if d.currentTTSPlaybackBackend() == nil {
		if allowFallback {
			return fmt.Errorf("tts playback backend is not configured")
		}
		return nil
	}
	if err := baseCtx.Err(); err != nil {
		return err
	}
	outputCtx, cancelOutput := context.WithCancel(baseCtx)
	output := newActiveTTSOutput(cancelOutput)
	unregisterOutput := d.registerActiveOutput(output)
	defer func() {
		unregisterOutput()
		cancelOutput()
	}()
	d.speechMu.Lock()
	defer d.speechMu.Unlock()
	if err := outputCtx.Err(); err != nil {
		return err
	}
	speakCtx := outputCtx
	cancelTimeout := func() {}
	if timeoutAfterLock > 0 {
		speakCtx, cancelTimeout = context.WithTimeout(outputCtx, timeoutAfterLock)
		defer cancelTimeout()
	}
	if err := speakCtx.Err(); err != nil {
		return err
	}

	speechStarted := false
	ttsErr := errTTSNotConfigured
	if manager := d.currentTTSManager(); manager != nil {
		logging.Infof("agent", "tts", "Starting streaming playback...")
		speechStarted, ttsErr = speakWithTTSManagerObserved(speakCtx, manager, d.currentTTSPlaybackBackend(), d.config, text, func(stream *streamSessionWriter) func() {
			stream.setCancel(cancelOutput)
			output.setStream(stream)
			return func() {
				output.clearStream(stream)
			}
		})
		if ttsErr == nil && !speechStarted && allowFallback {
			ttsErr = errTTSNoAudio
		}
		if ttsErr == nil {
			logging.Infof("agent", "tts", "Streaming playback complete")
			return nil
		}
		logging.Errorf("agent", "audio_dialog", "TTS streaming failed: %v", ttsErr)
	}
	if !allowFallback {
		return ttsErr
	}
	fallbackPlayed, resultErr := attemptTTSUnavailableFallback(speakCtx, d.currentTTSPlaybackBackend(), d.config, "", speechStarted, ttsErr)
	if fallbackPlayed {
		logging.Warnf("agent", "tts", "Local unavailable fallback played: %s", ttsUnavailableFallbackPath(d.config, ""))
	}
	return resultErr
}

func (d *AudioDialog) playPromptSoundAsync(kind promptSoundKind, label string) {
	d.playPromptSoundAsyncWithWait(kind, label, true)
}

func (d *AudioDialog) playPromptSoundAsyncWithWait(kind promptSoundKind, label string, wait bool) {
	go func() {
		_ = d.playPromptSound(kind, label, wait)
	}()
}

func (d *AudioDialog) playPromptSound(kind promptSoundKind, label string, wait bool) error {
	if kind == promptSoundRecordingStart {
		d.playPromptSoundUninterruptible(kind, label, wait)
		return nil
	}

	outputCtx, cancelOutput := context.WithCancel(context.Background())
	output := newActiveTTSOutput(cancelOutput)
	unregisterOutput := d.registerActiveOutput(output)
	defer func() {
		unregisterOutput()
		cancelOutput()
	}()

	d.speechMu.Lock()
	defer d.speechMu.Unlock()
	if err := playPromptSound(outputCtx, d.currentTTSPlaybackBackend(), kind, wait); err != nil {
		logging.Warnf("agent", "audio", "%s prompt sound failed: %v", label, err)
		return err
	}
	return nil
}

func (d *AudioDialog) playPromptSoundUninterruptible(kind promptSoundKind, label string, wait bool) {
	startedAt := time.Now()
	logging.Infof("agent", "audio", "%s prompt sound requested (uninterruptible)", label)
	d.speechMu.Lock()
	defer d.speechMu.Unlock()
	if err := playPromptSound(context.Background(), d.currentTTSPlaybackBackend(), kind, wait); err != nil {
		logging.Warnf("agent", "audio", "%s prompt sound failed after %s: %v", label, time.Since(startedAt).Round(time.Millisecond), err)
		return
	}
	logging.Infof("agent", "audio", "%s prompt sound completed in %s", label, time.Since(startedAt).Round(time.Millisecond))
}

// ProcessTextInput processes text input and speaks the response
func (d *AudioDialog) ProcessTextInput(ctx context.Context, text string, runtime *Runtime) error {
	_ = logging.LogEvent(logging.Info, "agent", "text", "user_input",
		logging.Field{Key: "message", Value: text})
	d.playPromptSoundAsyncWithWait(promptSoundAgentSend, "agent send", false)

	var speechWriter *speech.StreamWriter

	req := RunRequest{
		Input: text,
		Turn:  NewTextTurnInput(text, nil),
		EventHandler: func(event RunEvent) {
			if event.Type == "assistant_output" {
				return
			}
			toolSpeechStreamed := event.Type == runEventToolCall && speechWriter.FinalizeResponse()
			if toolSpeechStreamed {
				logging.Infof("agent", "tts", "Tool content already streamed: tool=%s", event.ToolName)
			} else {
				d.HandleRunEvent(ctx, event)
			}
		},
	}
	var newStream *streamSessionWriter
	if d.config.VoiceStreamingTTSEnabledOrDefault() && d.currentTTSManager() != nil {
		stream, activate, cleanup, err := d.beginManagedTTSStreamForRun(ctx)
		if err != nil {
			logging.Errorf("agent", "audio_dialog", "TTS BeginStream failed: %v", err)
		} else {
			newStream = stream
			defer cleanup()
			req.OnRunActive = func(context.Context) {
				activate()
			}
			speechWriter = speech.NewStreamWriter(newStream)
			req.StreamWriter = speechWriter
		}
	}

	result, err := runtime.Run(ctx, req)
	if newStream != nil {
		finalSpeechStreamed := speechWriter.FinalizeResponse()
		closeErr := newStream.closeAndWait()
		if closeErr != nil {
			logging.Errorf("agent", "audio_dialog", "new TTS stream failed: %v", closeErr)
		}
		result.SpeechStreamed = finalSpeechStreamed && newStream.emittedSpeech(closeErr)
	}
	if err != nil {
		if d.CanSpeakFinalText() && !result.SpeechStreamed {
			prepared := runtime.PrepareSpokenText(ctx, SpokenTextInput{TurnFailure: result.TurnFailure})
			if prepared.Text != "" {
				if speakErr := d.SpeakFinal(ctx, prepared.Text, nil); speakErr != nil {
					logging.Errorf("agent", "audio_dialog", "failure replacement TTS failed: %v", speakErr)
				}
			}
		}
		return fmt.Errorf("LLM request failed: %w", err)
	}
	logging.Infof("agent", "llm", "Response received")

	// Speak response if TTS is available
	speechText := result.SpokenTextForConfig(d.config)
	if d.CanSpeakFinalText() && speechText != "" && !result.SpeechStreamed {
		prepared := runtime.PrepareSpokenText(ctx, SpokenTextInput{ResponseText: speechText, TailAppendable: true})
		if err := d.SpeakFinal(ctx, prepared.Text, nil); err != nil {
			runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, err)
			logging.Errorf("agent", "audio_dialog", "TTS streaming failed: %v", err)
		} else {
			runtime.ReportSpokenTextDelivery(prepared.DeliveryToken, nil)
		}
	} else if result.Output != "" {
		_ = logging.LogEvent(logging.Info, "agent", "reply", "assistant_output",
			logging.Field{Key: "message", Value: result.Output})
	}

	return nil
}

// pcm16MonoToWAV converts PCM16 mono samples to WAV format
func pcm16MonoToWAV(samples []int16, sampleRate int) []byte {
	dataSize := len(samples) * 2
	fileSize := 36 + dataSize

	wav := make([]byte, 44+dataSize)

	// RIFF header
	copy(wav[0:4], "RIFF")
	binary.LittleEndian.PutUint32(wav[4:8], uint32(fileSize))
	copy(wav[8:12], "WAVE")

	// fmt chunk
	copy(wav[12:16], "fmt ")
	binary.LittleEndian.PutUint32(wav[16:20], 16)                   // chunk size
	binary.LittleEndian.PutUint16(wav[20:22], 1)                    // PCM
	binary.LittleEndian.PutUint16(wav[22:24], 1)                    // mono
	binary.LittleEndian.PutUint32(wav[24:28], uint32(sampleRate))   // sample rate
	binary.LittleEndian.PutUint32(wav[28:32], uint32(sampleRate*2)) // byte rate
	binary.LittleEndian.PutUint16(wav[32:34], 2)                    // block align
	binary.LittleEndian.PutUint16(wav[34:36], 16)                   // bits per sample

	// data chunk
	copy(wav[36:40], "data")
	binary.LittleEndian.PutUint32(wav[40:44], uint32(dataSize))

	// PCM data
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(wav[44+i*2:], uint16(sample))
	}

	return wav
}

// AppendPCM16Samples converts PCM bytes to int16 samples
func AppendPCM16Samples(pcmBytes []byte, samples *[]int16, hasPending *bool, pending *byte) {
	if len(pcmBytes) == 0 {
		return
	}

	i := 0
	dst := *samples

	// Handle pending byte from previous chunk
	if *hasPending {
		word := uint16(*pending) | (uint16(pcmBytes[0]) << 8)
		dst = append(dst, int16(word))
		*hasPending = false
		i = 1
	}

	pairCount := (len(pcmBytes) - i) / 2
	if pairCount > 0 {
		oldLen := len(dst)
		needed := oldLen + pairCount
		if needed > cap(dst) {
			newCap := cap(dst) * 2
			if newCap < needed {
				newCap = needed
			}
			if newCap == 0 {
				newCap = pairCount
			}
			grown := make([]int16, oldLen, newCap)
			copy(grown, dst)
			dst = grown
		}
		dst = dst[:needed]
		for j := 0; j < pairCount; j++ {
			offset := i + j*2
			dst[oldLen+j] = int16(binary.LittleEndian.Uint16(pcmBytes[offset : offset+2]))
		}
		i += pairCount * 2
	}

	// Save odd byte for next chunk
	if i < len(pcmBytes) {
		*pending = pcmBytes[i]
		*hasPending = true
	}
	*samples = dst
}
