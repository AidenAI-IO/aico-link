package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tmc/langchaingo/chains"
	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"

	"aico-agent/internal/agent/model"
	ttsmodule "aico-agent/internal/agent/tts"
)

func waitForAudioDialogRecordSTT(t *testing.T, dialog *AudioDialog) {
	t.Helper()
	deadline := time.After(time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		if dialog.currentRecordSTT() != nil {
			return
		}
		select {
		case <-deadline:
			t.Fatal("timed out waiting for streaming STT session")
		case <-ticker.C:
		}
	}
}

func TestAudioDialogFinishPendingUtterancePreservesTail(t *testing.T) {
	vad, err := NewAudioVADWithScorer(AudioVADConfig{
		SampleRate:      16000,
		SilenceMs:       650,
		MinSpeechMs:     300,
		SpeechThreshold: 0.5,
	}, &sequenceScorer{probabilities: []float64{0.9}})
	if err != nil {
		t.Fatalf("NewAudioVADWithScorer() error = %v", err)
	}

	dialog := &AudioDialog{vad: vad}
	frame := make([]int16, vad.FrameSamples())
	for i := range frame {
		frame[i] = 1000
	}
	if _, err := dialog.ProcessVADFrame(frame); err != nil {
		t.Fatalf("ProcessVADFrame() error = %v", err)
	}

	got := dialog.FinishPendingUtterance([]int16{7, 8, 9})
	if len(got) != len(frame)+3 {
		t.Fatalf("FinishPendingUtterance() len = %d, want %d", len(got), len(frame)+3)
	}
	if got[len(got)-3] != 7 || got[len(got)-2] != 8 || got[len(got)-1] != 9 {
		t.Fatalf("FinishPendingUtterance() tail = %#v, want [7 8 9]", got[len(got)-3:])
	}
}

func TestAudioDialogReadRecordChunkRequiresActiveRecording(t *testing.T) {
	dialog := &AudioDialog{}

	chunk, err := dialog.ReadRecordChunk(200)
	if err == nil {
		t.Fatal("expected error when recording is inactive")
	}
	if chunk != nil {
		t.Fatalf("expected nil chunk, got %#v", chunk)
	}
}

func TestAudioDialogStopRecordingClearsLocalStateOnStopError(t *testing.T) {
	missingSocket := filepath.Join(t.TempDir(), "missing-audio.sock")
	uploader := newBlockingFinalizeUploader("")
	dialog := &AudioDialog{
		audioClient:  NewAudioServiceClient(missingSocket),
		recordActive: true,
		sessionID:    123,
		recordSTT:    &streamingSTTSession{uploader: uploader},
	}

	if err := dialog.StopRecording(); err == nil {
		t.Fatal("expected stop recording error")
	}
	if dialog.recordActive {
		t.Fatal("recordActive should be cleared after stop attempt")
	}
	if dialog.sessionID != 0 {
		t.Fatalf("sessionID = %d, want 0", dialog.sessionID)
	}
	select {
	case <-uploader.closed:
	case <-time.After(time.Second):
		t.Fatal("expected streaming session to be closed on stop error")
	}
}

func TestAudioDialogStartRecordingRetriesUntilAudioServiceAvailable(t *testing.T) {
	oldTimeout := recordingStartRetryTimeout
	oldInterval := recordingStartRetryInterval
	recordingStartRetryTimeout = 300 * time.Millisecond
	recordingStartRetryInterval = 10 * time.Millisecond
	t.Cleanup(func() {
		recordingStartRetryTimeout = oldTimeout
		recordingStartRetryInterval = oldInterval
	})

	socketDir, err := os.MkdirTemp("/tmp", "aico-audio-dialog-*")
	if err != nil {
		t.Fatalf("create temp socket dir: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(socketDir)
	})
	socketPath := filepath.Join(socketDir, "audio.sock")
	ready := make(chan struct{})
	go serveDelayedStartRecording(t, socketPath, ready)

	dialog := &AudioDialog{
		config:      Config{Audio: AudioConfig{SampleRate: 16000, Channels: 1, BitWidth: 16}},
		audioClient: NewAudioServiceClient(socketPath),
	}
	if err := dialog.StartRecording(); err != nil {
		t.Fatalf("StartRecording() error = %v", err)
	}
	if !dialog.recordActive {
		t.Fatal("recordActive = false, want true")
	}
	if dialog.sessionID != 42 {
		t.Fatalf("sessionID = %d, want 42", dialog.sessionID)
	}
	select {
	case <-ready:
	case <-time.After(time.Second):
		t.Fatal("delayed audio service did not handle start_recording")
	}
}

func TestAudioDialogStartRecordingRollsBackOnVADResetFailure(t *testing.T) {
	var (
		opMu sync.Mutex
		ops  []string
	)
	socketPath := startFakeAudioServiceSocket(t, func(req audioRequest) (audioResponse, []byte) {
		opMu.Lock()
		ops = append(ops, req.Op)
		opMu.Unlock()
		switch req.Op {
		case "start_recording":
			return audioResponse{Status: "OK", SessionID: stringUint64(42)}, nil
		default:
			return audioResponse{Status: "OK"}, nil
		}
	})

	scorer := &sequenceScorer{resetErr: errors.New("vad helper crashed")}
	vad, err := NewAudioVADWithScorer(AudioVADConfig{
		SampleRate:      16000,
		SilenceMs:       650,
		MinSpeechMs:     300,
		SpeechThreshold: 0.5,
	}, scorer)
	if err != nil {
		t.Fatalf("NewAudioVADWithScorer() error = %v", err)
	}

	sttClient := &stubSTTClient{
		supportsStreaming: true,
		streamUploader:    &stubSTTStreamUploader{},
	}
	dialog := &AudioDialog{
		config: Config{
			InputMode: "stt",
			Audio:     AudioConfig{Socket: socketPath, SampleRate: 16000, Channels: 1, BitWidth: 16},
		},
		audioClient: NewAudioServiceClient(socketPath),
		sttClient:   sttClient,
		vad:         vad,
	}

	if err := dialog.StartRecording(); err == nil {
		t.Fatal("expected StartRecording error when VAD reset fails")
	}

	if dialog.recordActive {
		t.Fatal("recordActive should be false after VAD failure")
	}
	if dialog.sessionID != 0 {
		t.Fatalf("sessionID = %d, want 0 after VAD failure", dialog.sessionID)
	}
	if dialog.recordReader != nil {
		t.Fatal("recordReader should be nil after VAD failure")
	}
	if got := dialog.currentRecordSTT(); got != nil {
		t.Fatal("recordSTT should be nil after VAD failure")
	}
	if sttClient.streamUploaderUsed != 0 {
		t.Fatalf("streamUploaderUsed = %d, want 0; streaming session should not be opened when VAD reset fails", sttClient.streamUploaderUsed)
	}
	opMu.Lock()
	defer opMu.Unlock()
	for _, op := range ops {
		if op == "start_recording" {
			t.Fatalf("audio service received %q before VAD validated; ops = %v", op, ops)
		}
	}
}

func TestAudioDialogPrepareTurnInputUsesStreamingTranscriptFromRecording(t *testing.T) {
	stopCh := make(chan struct{})
	var stopOnce sync.Once
	socketPath := startFakeAudioServiceSocket(t, func(req audioRequest) (audioResponse, []byte) {
		switch req.Op {
		case "start_recording":
			return audioResponse{Status: "OK", SessionID: stringUint64(42)}, nil
		case "read_record_chunk":
			select {
			case <-stopCh:
				return audioResponse{Status: "OK", EndOfStream: true}, nil
			default:
				return audioResponse{Status: "OK"}, []byte{1, 0, 2, 0}
			}
		case "stop_recording":
			stopOnce.Do(func() { close(stopCh) })
			return audioResponse{Status: "OK"}, nil
		default:
			return audioResponse{Status: "OK"}, nil
		}
	})

	sttClient := &stubSTTClient{
		supportsStreaming: true,
		streamUploader:    &stubSTTStreamUploader{transcript: "streaming result"},
	}
	dialog := &AudioDialog{
		config: Config{
			InputMode: "stt",
			Audio: AudioConfig{
				Socket:     socketPath,
				SampleRate: 16000,
				Channels:   1,
				BitWidth:   16,
			},
		},
		audioClient:  NewAudioServiceClient(socketPath),
		sttClient:    sttClient,
		audioArchive: NewAudioArchiveManager(AudioArchiveConfig{Enabled: false}),
	}

	if err := dialog.StartRecording(); err != nil {
		t.Fatalf("StartRecording() error = %v", err)
	}
	waitForAudioDialogRecordSTT(t, dialog)
	chunk, err := dialog.ReadRecordChunk(200)
	if err != nil {
		t.Fatalf("ReadRecordChunk() error = %v", err)
	}
	if chunk == nil || len(chunk.PCM) == 0 {
		t.Fatalf("expected PCM chunk, got %#v", chunk)
	}
	if err := dialog.StopRecording(); err != nil {
		t.Fatalf("StopRecording() error = %v", err)
	}

	input, err := dialog.PrepareTurnInput([]int16{1, 2})
	if err != nil {
		t.Fatalf("PrepareTurnInput() error = %v", err)
	}
	if input.Transcript != "streaming result" {
		t.Fatalf("Transcript = %q, want streaming result", input.Transcript)
	}
	if len(input.TelemetryEvents) != 1 {
		t.Fatalf("TelemetryEvents len = %d, want 1", len(input.TelemetryEvents))
	}
	telemetry := input.TelemetryEvents[0]
	if telemetry.Type != runEventSTTTranscription || telemetry.Content != "streaming result" {
		t.Fatalf("STT telemetry event = %#v", telemetry)
	}
	if telemetry.Metadata["streaming_ready"] != true || telemetry.Metadata["used_streaming_transcript"] != true || telemetry.Metadata["fallback_one_shot"] != false {
		t.Fatalf("STT telemetry metadata = %#v", telemetry.Metadata)
	}
	if len(sttClient.inputs) != 0 {
		t.Fatalf("expected streaming transcript to skip TranscribeWAV, got %d calls", len(sttClient.inputs))
	}
	if sttClient.streamUploaderUsed != 1 {
		t.Fatalf("stream uploader begin count = %d, want 1", sttClient.streamUploaderUsed)
	}
	if sttClient.streamUploader == nil || len(sttClient.streamUploader.writes) != 1 {
		t.Fatalf("stream uploader writes = %#v, want one PCM write", sttClient.streamUploader)
	}
}

func TestAudioDialogStopRecordingFallsBackToOneShotWhenStreamingFinalizeTimesOut(t *testing.T) {
	oldTimeout := audioDialogStreamingSTTFinalizeTimeout
	audioDialogStreamingSTTFinalizeTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		audioDialogStreamingSTTFinalizeTimeout = oldTimeout
	})

	socketPath := startFakeAudioServiceSocket(t, func(req audioRequest) (audioResponse, []byte) {
		switch req.Op {
		case "stop_recording":
			return audioResponse{Status: "OK"}, nil
		default:
			return audioResponse{Status: "OK"}, nil
		}
	})

	uploader := newBlockingFinalizeUploader("")
	sttClient := &stubSTTClient{transcript: "one-shot result"}
	dialog := &AudioDialog{
		config: Config{
			InputMode: "stt",
			Audio: AudioConfig{
				Socket:     socketPath,
				SampleRate: 16000,
			},
		},
		audioClient:  NewAudioServiceClient(socketPath),
		sttClient:    sttClient,
		recordActive: true,
		sessionID:    42,
		recordSTT:    &streamingSTTSession{uploader: uploader},
	}

	if err := dialog.StopRecording(); err != nil {
		t.Fatalf("StopRecording() error = %v", err)
	}

	input, err := dialog.PrepareTurnInput([]int16{1, 2, 3, 4})
	if err != nil {
		t.Fatalf("PrepareTurnInput() error = %v", err)
	}
	if input.Transcript != "one-shot result" {
		t.Fatalf("Transcript = %q, want one-shot result", input.Transcript)
	}
	if len(input.TelemetryEvents) != 1 {
		t.Fatalf("TelemetryEvents len = %d, want 1", len(input.TelemetryEvents))
	}
	telemetry := input.TelemetryEvents[0]
	if telemetry.Type != runEventSTTTranscription || telemetry.Content != "one-shot result" {
		t.Fatalf("STT telemetry event = %#v", telemetry)
	}
	if telemetry.Metadata["fallback_one_shot"] != true || telemetry.Metadata["used_streaming_transcript"] != false {
		t.Fatalf("STT telemetry metadata = %#v", telemetry.Metadata)
	}
	if telemetry.Metadata["streaming_finalize_error"] == nil {
		t.Fatalf("STT telemetry missing streaming_finalize_error: %#v", telemetry.Metadata)
	}
	if len(sttClient.inputs) != 1 {
		t.Fatalf("expected fallback one-shot STT call, got %d", len(sttClient.inputs))
	}
	select {
	case <-uploader.closed:
	case <-time.After(time.Second):
		t.Fatal("expected uploader to be closed after finalize timeout")
	}
}

func TestAudioDialogSTTUploadErrorIgnoresStaleSession(t *testing.T) {
	dialog := &AudioDialog{
		recordActive:       true,
		sessionID:          2,
		recordSTTTelemetry: &sttTurnTelemetry{},
	}

	dialog.markRecordSTTUploadError(1, errors.New("old upload failed"))
	if dialog.recordSTTTelemetry.streamingUploadError != "" {
		t.Fatalf("stale upload error contaminated telemetry: %#v", dialog.recordSTTTelemetry)
	}

	dialog.markRecordSTTUploadError(2, errors.New("current upload failed"))
	if dialog.recordSTTTelemetry.streamingUploadError != "current upload failed" {
		t.Fatalf("current upload error = %q", dialog.recordSTTTelemetry.streamingUploadError)
	}
}

func TestNewAudioDialogSTTWakeupCreatesSTTClient(t *testing.T) {
	dialog, err := NewAudioDialog(&Runtime{config: Config{
		Model:     ModelConfig{Provider: "fake"},
		TTS:       TTSConfig{Provider: "minimax-cn", APIKey: "test-key"},
		STT:       STTConfig{Provider: "openai-whisper"},
		Audio:     AudioConfig{Socket: "/tmp/audio.sock", SampleRate: 16000},
		InputMode: "stt",
	}})
	if err != nil {
		t.Fatalf("NewAudioDialog() error = %v", err)
	}

	if dialog.sttClient == nil {
		t.Fatal("stt input mode should create an STT client")
	}
	if dialog.audioClient.socketPath != "/tmp/audio.sock" {
		t.Fatalf("audio socket = %q, want /tmp/audio.sock", dialog.audioClient.socketPath)
	}
	if got := dialog.VADFrameSamples(); got != 512 {
		t.Fatalf("VADFrameSamples() = %d, want 512 for Silero RKNN VAD", got)
	}
}

func TestNewAudioDialogIgnoresInvalidOptionalTTS(t *testing.T) {
	dialog, err := NewAudioDialog(&Runtime{config: Config{
		Model:     ModelConfig{Provider: "fake"},
		TTS:       TTSConfig{Provider: "missing-provider", APIKey: "test-key"},
		STT:       STTConfig{Provider: "openai-whisper"},
		Audio:     AudioConfig{Socket: "/tmp/audio.sock", SampleRate: 16000},
		InputMode: "stt",
	}})
	if err != nil {
		t.Fatalf("NewAudioDialog() error = %v", err)
	}
	if dialog.ttsManager == nil {
		t.Fatal("ttsManager = nil, want stable empty manager after optional TTS init failure")
	}
	if dialog.currentTTSManager() != nil {
		t.Fatalf("currentTTSManager() = %#v, want unavailable TTS after init failure", dialog.currentTTSManager())
	}
}

func TestProcessUtteranceSpeaksTaggedTTSOutput(t *testing.T) {
	output := "Setup completed, current volume is 42.\n\n- Read volume\n- Confirm status\n\nThis detailed description is reserved for the screen.\n<tts>Setup completed, current volume is 42.</tts>"
	expectedSpeech := "Setup completed, current volume is 42."
	model := &scriptedModel{
		responses: roleDirectResponses(output),
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Answer directly.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)

	provider := &recordingTTSProvider{name: "dialog-provider"}
	dialog := &AudioDialog{
		config: Config{
			Model:                    ModelConfig{Provider: "fake"},
			Audio:                    AudioConfig{SampleRate: 16000},
			InputMode:                "stt",
			VoiceStreamingTTSEnabled: boolPtr(false),
		},
		sttClient:    &stubSTTClient{transcript: "check volume"},
		audioClient:  NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
		ttsManager:   ttsmodule.NewProviderManager(provider, nil),
		audioArchive: NewAudioArchiveManager(AudioArchiveConfig{Enabled: false}),
	}
	if err := dialog.ProcessUtterance(context.Background(), []int16{100, -100, 200, -200}, runtime); err != nil {
		t.Fatalf("ProcessUtterance() error = %v", err)
	}

	texts := provider.texts()
	if len(texts) != 1 {
		t.Fatalf("unexpected TTS texts: %#v", texts)
	}
	if texts[0] != expectedSpeech {
		t.Fatalf("TTS should use result output directly, got %q want %q", texts[0], expectedSpeech)
	}
}

func TestProcessUtteranceAppendsVoiceNotificationOnlyToSpokenText(t *testing.T) {
	output := "Setup completed.\n<tts>Setup completed.</tts>"
	model := &scriptedModel{responses: roleDirectResponses(output)}
	cfg := DefaultConfig()
	cfg.Model = ModelConfig{Provider: "fake"}
	cfg.Instruction = "Answer directly."
	cfg.Locale = "zh-CN"
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, cfg),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	if err := runtime.VoiceNotificationSink().Publish(context.Background(), VoiceNotificationEvent{
		Code: "storage", Severity: SeverityWarning, State: VoiceNotificationActive, DedupeKey: "storage:device",
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}

	provider := &recordingTTSProvider{name: "dialog-provider"}
	dialog := &AudioDialog{
		config: Config{
			Model:                    ModelConfig{Provider: "fake"},
			Audio:                    AudioConfig{SampleRate: 16000},
			InputMode:                "stt",
			VoiceStreamingTTSEnabled: boolPtr(false),
		},
		sttClient:    &stubSTTClient{transcript: "check volume"},
		audioClient:  NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
		ttsManager:   ttsmodule.NewProviderManager(provider, nil),
		audioArchive: NewAudioArchiveManager(AudioArchiveConfig{Enabled: false}),
	}
	if err := dialog.ProcessUtterance(context.Background(), []int16{100, -100, 200, -200}, runtime); err != nil {
		t.Fatalf("ProcessUtterance() error = %v", err)
	}
	texts := provider.texts()
	if len(texts) != 1 || texts[0] != "Setup completed.另外提醒一下，设备存储空间不足。" {
		t.Fatalf("spoken texts = %#v", texts)
	}
}

func TestProcessUtteranceKeepsVoiceNotificationPendingWithoutTTS(t *testing.T) {
	model := &scriptedModel{responses: roleDirectResponses("Setup completed.")}
	cfg := DefaultConfig()
	cfg.Model = ModelConfig{Provider: "fake"}
	cfg.Instruction = "Answer directly."
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, cfg),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	if err := runtime.VoiceNotificationSink().Publish(context.Background(), VoiceNotificationEvent{
		Code: "storage", Severity: SeverityWarning, State: VoiceNotificationActive, DedupeKey: "storage:device",
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	dialog := &AudioDialog{
		config: Config{
			Model:                    ModelConfig{Provider: "fake"},
			Audio:                    AudioConfig{SampleRate: 16000},
			InputMode:                "stt",
			VoiceStreamingTTSEnabled: boolPtr(false),
		},
		sttClient:    &stubSTTClient{transcript: "check volume"},
		audioArchive: NewAudioArchiveManager(AudioArchiveConfig{Enabled: false}),
	}

	if err := dialog.ProcessUtterance(context.Background(), []int16{100, -100, 200, -200}, runtime); err != nil {
		t.Fatalf("ProcessUtterance() error = %v", err)
	}
	prepared := runtime.PrepareSpokenText(context.Background(), SpokenTextInput{ResponseText: "next reply", TailAppendable: true})
	if prepared.Mode != SpokenTextModeTail {
		t.Fatalf("pending reminder mode = %q, want %q", prepared.Mode, SpokenTextModeTail)
	}
}

func TestProcessTextInputKeepsVoiceNotificationPendingWithoutAudioClient(t *testing.T) {
	model := &scriptedModel{responses: roleDirectResponses("Setup completed.\n<tts>Setup completed.</tts>")}
	cfg := DefaultConfig()
	cfg.Model = ModelConfig{Provider: "fake"}
	cfg.Instruction = "Answer directly."
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, cfg),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	if err := runtime.VoiceNotificationSink().Publish(context.Background(), VoiceNotificationEvent{
		Code: "storage", Severity: SeverityWarning, State: VoiceNotificationActive, DedupeKey: "storage:device",
	}); err != nil {
		t.Fatalf("Publish() error = %v", err)
	}
	dialog := &AudioDialog{
		config: Config{
			Model:                    ModelConfig{Provider: "fake"},
			VoiceStreamingTTSEnabled: boolPtr(false),
		},
		ttsManager: ttsmodule.NewProviderManager(&recordingTTSProvider{name: "no-audio-client"}, nil),
	}

	if err := dialog.ProcessTextInput(context.Background(), "check volume", runtime); err != nil {
		t.Fatalf("ProcessTextInput() error = %v", err)
	}
	prepared := runtime.PrepareSpokenText(context.Background(), SpokenTextInput{ResponseText: "next reply", TailAppendable: true})
	if prepared.Mode != SpokenTextModeTail {
		t.Fatalf("pending reminder mode = %q, want %q", prepared.Mode, SpokenTextModeTail)
	}
}

func TestAudioDialogSpeaksToolContentAsynchronously(t *testing.T) {
	toolSpeech := true
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponseWithContent("call_1", "audio_volume", `{"__arg1":"{}"}`, "Checking volume.\n<tts>Check volume.</tts>"),
			contentResponse("Current volume is 42.\n<tts>Current volume is 42.</tts>"),
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:               ModelConfig{Provider: "fake"},
			Instruction:         "Use tools when external state is requested.",
			VoiceToolCallSpeech: &toolSpeech,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": &stubTool{
				name:        "audio_volume",
				description: "Get the current audio playback volume.",
				output:      `{"volume":42}`,
			},
		}},
		NewSkillIndex(),
	)
	provider := &recordingTTSProvider{name: "dialog-provider"}
	dialog := &AudioDialog{
		config: Config{
			Model:                    ModelConfig{Provider: "fake"},
			Audio:                    AudioConfig{SampleRate: 16000},
			InputMode:                "stt",
			VoiceStreamingTTSEnabled: boolPtr(false),
			VoiceToolCallSpeech:      &toolSpeech,
		},
		sttClient:    &stubSTTClient{transcript: "check volume"},
		audioClient:  NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
		ttsManager:   ttsmodule.NewProviderManager(provider, nil),
		audioArchive: NewAudioArchiveManager(AudioArchiveConfig{Enabled: false}),
	}

	if err := dialog.ProcessUtterance(context.Background(), []int16{100, -100, 200, -200}, runtime); err != nil {
		t.Fatalf("ProcessUtterance() error = %v", err)
	}
	waitForProviderTextCount(t, provider, 2)
	texts := provider.texts()
	if len(texts) != 2 {
		t.Fatalf("expected tool content and final answer TTS, got %#v", texts)
	}
	if !containsString(texts, "Check volume.") || !containsString(texts, "Current volume is 42.") {
		t.Fatalf("unexpected TTS texts: %#v", texts)
	}
}

func TestAudioDialogStreamsLeadingToolSpeechWithoutDuplicatePlayback(t *testing.T) {
	toolSpeech := true
	toolContent := "<tts>Check volume.</tts>\nChecking volume."
	finalContent := "<tts>Current volume is 42.</tts>\nCurrent volume is 42."
	model := &scriptedModel{
		responses: []*llms.ContentResponse{
			toolCallResponseWithContent("call_1", "audio_volume", `{"__arg1":"{}"}`, toolContent),
			contentResponse(finalContent),
		},
		streamChunks: [][]string{
			{toolContent},
			{finalContent},
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:                    ModelConfig{Provider: "fake"},
			Instruction:              "Use tools when external state is requested.",
			VoiceStreamingTTSEnabled: boolPtr(true),
			VoiceToolCallSpeech:      &toolSpeech,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"audio_volume": &stubTool{
				name:        "audio_volume",
				description: "Get the current audio playback volume.",
				output:      `{"volume":42}`,
			},
		}},
		NewSkillIndex(),
	)
	provider := &flushRecordingTTSProvider{name: "dialog-provider"}
	dialog := &AudioDialog{
		config: Config{
			Model:                    ModelConfig{Provider: "fake"},
			Audio:                    AudioConfig{SampleRate: 16000},
			InputMode:                "stt",
			VoiceStreamingTTSEnabled: boolPtr(true),
			VoiceToolCallSpeech:      &toolSpeech,
		},
		sttClient:    &stubSTTClient{transcript: "check volume"},
		audioClient:  NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
		ttsManager:   ttsmodule.NewProviderManager(provider, nil),
		audioArchive: NewAudioArchiveManager(AudioArchiveConfig{Enabled: false}),
	}

	if err := dialog.ProcessUtterance(context.Background(), []int16{100, -100, 200, -200}, runtime); err != nil {
		t.Fatalf("ProcessUtterance() error = %v", err)
	}
	waitForFlushProviderTextCount(t, provider, 2)
	time.Sleep(100 * time.Millisecond)

	texts := provider.texts()
	if len(texts) != 2 {
		t.Fatalf("tool-event and final TTS should each play once, got %#v", texts)
	}
	if countString(texts, "Check volume.") != 1 {
		t.Fatalf("tool TTS count = %d, want 1: %#v", countString(texts, "Check volume."), texts)
	}
	if countString(texts, "Current volume is 42.") != 1 {
		t.Fatalf("final TTS count = %d, want 1: %#v", countString(texts, "Current volume is 42."), texts)
	}
	if texts[0] != "Check volume." || texts[1] != "Current volume is 42." {
		t.Fatalf("TTS playback order = %#v, want tool progress before final response", texts)
	}
}

func TestAudioDialogDoesNotSpeakWaitForWakeupToolContent(t *testing.T) {
	toolSpeech := true
	provider := &recordingTTSProvider{name: "dialog-provider"}
	dialog := &AudioDialog{
		config: Config{
			VoiceToolCallSpeech: &toolSpeech,
		},
		audioClient: NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
	}

	dialog.HandleRunEvent(context.Background(), RunEvent{
		Type:     runEventToolCall,
		ToolName: toolWaitForWakeup,
		Content:  "Preparing to return to waiting for wakeup state.",
	})

	assertNoProviderTextWithin(t, provider, 200*time.Millisecond)
}

func TestAudioDialogSpeaksToolCallContent(t *testing.T) {
	toolSpeech := true
	provider := &recordingTTSProvider{name: "dialog-provider"}
	dialog := &AudioDialog{
		config: Config{
			VoiceToolCallSpeech: &toolSpeech,
		},
		audioClient: NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
	}

	dialog.HandleRunEvent(context.Background(), RunEvent{
		Type:     runEventToolCall,
		ToolName: "audio_volume",
		Content:  "Reading volume.\n<tts>Read current volume.</tts>",
	})

	waitForProviderTextCount(t, provider, 1)
	if got := provider.texts(); len(got) != 1 || got[0] != "Read current volume." {
		t.Fatalf("unexpected TTS texts: %#v", got)
	}
}

func TestAudioDialogDoesNotSpeakToolCallWithoutTTSTag(t *testing.T) {
	toolSpeech := true
	provider := &recordingTTSProvider{name: "dialog-provider"}
	dialog := &AudioDialog{
		config: Config{
			VoiceToolCallSpeech: &toolSpeech,
		},
		audioClient: NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
	}

	dialog.HandleRunEvent(context.Background(), RunEvent{
		Type:     runEventToolCall,
		ToolName: "recall_memory",
		Content:  "I will check your preferences first.",
	})

	assertNoProviderTextWithin(t, provider, 200*time.Millisecond)
}

func TestAudioDialogDoesNotSpeakToolCallWithoutContent(t *testing.T) {
	toolSpeech := true
	provider := &recordingTTSProvider{name: "dialog-provider"}
	dialog := &AudioDialog{
		config: Config{
			VoiceToolCallSpeech: &toolSpeech,
		},
		audioClient: NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
	}

	dialog.HandleRunEvent(context.Background(), RunEvent{
		Type:     runEventToolCall,
		ToolName: "audio_volume",
	})

	assertNoProviderTextWithin(t, provider, 200*time.Millisecond)
}

func TestAudioDialogStreamingSpeechErrorDoesNotHideWaitForWakeupRequest(t *testing.T) {
	model := &scriptedModel{
		responses: roleToolResponses("wait_for_wakeup", `{"__arg1":"{\"reason\":\"user asked\"}"}`, "I will wait for the next wakeup."),
	}
	controller := NewWaitForWakeupController()
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:       ModelConfig{Provider: "fake"},
			Instruction: "Use tools when external state is requested.",
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{
			"wait_for_wakeup": NewWaitForWakeupTool(controller),
		}},
		NewSkillIndex(),
	)
	dialog := &AudioDialog{
		config: Config{
			Model:                    ModelConfig{Provider: "fake"},
			VoiceStreamingTTSEnabled: boolPtr(true),
		},
		audioClient: NewAudioServiceClient(filepath.Join(t.TempDir(), "missing-audio.sock")),
		ttsManager:  ttsmodule.NewProviderManager(&recordingTTSProvider{name: "dialog-provider"}, nil),
	}

	result, err := dialog.RunAgentTurn(context.Background(), TurnInput{InputText: "go to sleep"}, runtime)
	if err != nil {
		t.Fatalf("RunAgentTurn() error = %v", err)
	}
	if !result.WaitForWakeupRequested {
		t.Fatal("WaitForWakeupRequested = false, want true even when streaming speech fails")
	}
	if result.SpeechStreamed {
		t.Fatal("SpeechStreamed = true, want false when TTS playback failed before successful speech")
	}
}

func TestAudioDialogCountsPartialStreamingPlaybackAsStreamedWhenCloseFails(t *testing.T) {
	output := "<tts>Partial spoken reply.</tts>\nPartial spoken reply."
	model := &scriptedModel{
		responses:    roleDirectResponses(output),
		streamChunks: [][]string{{output}},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}, Instruction: "Answer directly."}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	dialog := &AudioDialog{
		config: Config{
			Model:                    ModelConfig{Provider: "fake"},
			Audio:                    AudioConfig{SampleRate: 16000},
			VoiceStreamingTTSEnabled: boolPtr(true),
		},
		audioClient: NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
		ttsManager:  ttsmodule.NewProviderManager(&playbackStartedTransientErrorProvider{name: "partial-stream"}, nil),
	}

	result, err := dialog.RunAgentTurn(context.Background(), TurnInput{InputText: "say something"}, runtime)
	if err != nil {
		t.Fatalf("RunAgentTurn() error = %v", err)
	}
	if !result.SpeechStreamed {
		t.Fatal("SpeechStreamed = false after PCM playback began; caller could replay the full response")
	}
}

func TestAudioDialogRunAgentTurnQueuesButDoesNotConsumeSteer(t *testing.T) {
	firstCallStarted := make(chan struct{})
	releaseFirstCall := make(chan struct{})
	model := &blockingFirstCallModel{
		firstCallStarted: firstCallStarted,
		releaseFirstCall: releaseFirstCall,
		responses: []*llms.ContentResponse{
			contentResponse("first answer"),
			contentResponse("steered answer"),
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:           ModelConfig{Provider: "fake"},
			Instruction:     "Answer directly.",
			ForceSimpleLoop: true,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	dialog := &AudioDialog{
		config: Config{
			Model: ModelConfig{Provider: "fake"},
		},
	}
	resultCh := make(chan struct {
		result RunResult
		err    error
	}, 1)
	go func() {
		result, err := dialog.RunAgentTurn(context.Background(), TurnInput{InputText: "original"}, runtime)
		resultCh <- struct {
			result RunResult
			err    error
		}{result: result, err: err}
	}()

	select {
	case <-firstCallStarted:
	case <-time.After(time.Second):
		t.Fatal("first model call did not start")
	}
	if !dialog.QueueSteer(TurnInput{InputText: "change direction", Transcript: "change direction"}) {
		t.Fatal("QueueSteer returned false while voice run was active")
	}
	close(releaseFirstCall)

	var runResult struct {
		result RunResult
		err    error
	}
	select {
	case runResult = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("RunAgentTurn did not finish")
	}
	if runResult.err != nil {
		t.Fatalf("RunAgentTurn() error = %v", runResult.err)
	}
	if runResult.result.Output != "first answer" {
		t.Fatalf("Output = %q, want first answer", runResult.result.Output)
	}
	if dialog.QueueSteer(TurnInput{InputText: "too late"}) {
		t.Fatal("QueueSteer returned true after voice run completed")
	}
}

func TestAudioDialogBeginSteerInterruptAppliesQueuedCorrection(t *testing.T) {
	firstCallStarted := make(chan struct{})
	releaseFirstCall := make(chan struct{})
	model := &blockingFirstCallModel{
		firstCallStarted: firstCallStarted,
		releaseFirstCall: releaseFirstCall,
		responses: []*llms.ContentResponse{
			contentResponse("stale answer"),
			contentResponse("corrected answer"),
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model:           ModelConfig{Provider: "fake"},
			Instruction:     "Answer directly.",
			ForceSimpleLoop: true,
		}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	dialog := &AudioDialog{
		config: Config{
			Model: ModelConfig{Provider: "fake"},
		},
	}

	resultCh := make(chan struct {
		result RunResult
		err    error
	}, 1)
	go func() {
		result, err := dialog.RunAgentTurn(context.Background(), TurnInput{InputText: "original"}, runtime)
		resultCh <- struct {
			result RunResult
			err    error
		}{result: result, err: err}
	}()

	select {
	case <-firstCallStarted:
	case <-time.After(time.Second):
		t.Fatal("first model call did not start")
	}
	if !dialog.BeginSteerInterrupt() {
		t.Fatal("BeginSteerInterrupt returned false while voice run was active")
	}
	if !dialog.QueueSteer(TurnInput{InputText: "change direction", Transcript: "change direction"}) {
		t.Fatal("QueueSteer returned false for interrupted voice run")
	}

	var runResult struct {
		result RunResult
		err    error
	}
	select {
	case runResult = <-resultCh:
	case <-time.After(time.Second):
		t.Fatal("RunAgentTurn did not finish")
	}
	if runResult.err != nil {
		t.Fatalf("RunAgentTurn() error = %v", runResult.err)
	}
	if runResult.result.Output != "corrected answer" {
		t.Fatalf("Output = %q, want corrected answer", runResult.result.Output)
	}
	if dialog.ResumeSteerInterrupt() {
		t.Fatal("ResumeSteerInterrupt returned true after voice run completed")
	}
}

func TestAudioDialogRunVoiceTurnRejectsConcurrentRun(t *testing.T) {
	dialog := &AudioDialog{
		config: Config{
			Model:        ModelConfig{Provider: "fake"},
			Audio:        AudioConfig{SampleRate: 16000},
			AudioArchive: AudioArchiveConfig{Enabled: false},
		},
		audioArchive: NewAudioArchiveManager(AudioArchiveConfig{Enabled: false}),
	}
	if !dialog.beginVoiceRunControl("existing-request") {
		t.Fatal("beginVoiceRunControl returned false for setup")
	}
	defer dialog.endVoiceRunControl("existing-request")

	_, err := dialog.RunVoiceTurnWithContext(
		context.Background(),
		TurnInput{InputText: "late correction", Transcript: "late correction"},
		[]int16{100, -100},
		nil,
		VoiceTurnContext{RuntimeContext: "voice interruption"},
	)
	if err == nil || !strings.Contains(err.Error(), "voice run already active") {
		t.Fatalf("RunVoiceTurnWithContext() error = %v, want voice run already active", err)
	}
}

func TestAudioDialogPublishesVoiceMessages(t *testing.T) {
	dialog := &AudioDialog{config: Config{Audio: AudioConfig{SampleRate: 16000}}}
	var messages []Message
	dialog.SetMessagePublisher(func(message Message) {
		messages = append(messages, message)
	})
	if !dialog.beginVoiceRunControl("voice-request") {
		t.Fatal("beginVoiceRunControl returned false")
	}
	defer dialog.endVoiceRunControl("voice-request")

	dialog.publishVoiceUserInput(
		TurnInput{InputText: "hello", Modality: TurnModalitySTT},
		nil,
		"episode-1",
		"voice-request",
	)
	dialog.publishVoiceRunEvent(RunEvent{
		Type:      "assistant_output",
		EpisodeID: "episode-1",
		Content:   "hi",
	}, "voice-request")

	if len(messages) != 2 {
		t.Fatalf("published messages = %#v, want user and assistant", messages)
	}
	if messages[0].Type != "user" || messages[0].Source != "voice" {
		t.Fatalf("user message = %#v", messages[0])
	}
	if messages[1].Type != "assistant" || messages[1].Source != "voice" {
		t.Fatalf("assistant message = %#v", messages[1])
	}
}

func TestAudioDialogPublishesStreamingReasoningForVoiceRun(t *testing.T) {
	dialog := &AudioDialog{}
	var published []Message
	dialog.SetMessagePublisher(func(message Message) {
		published = append(published, message)
	})
	if !dialog.beginVoiceRunControl("voice-request") {
		t.Fatal("beginVoiceRunControl returned false")
	}
	defer dialog.endVoiceRunControl("voice-request")

	dialog.publishVoiceRunEvent(RunEvent{
		Type:             runEventReasoningDelta,
		EpisodeID:        "episode-1",
		ReasoningContent: "first thought",
	}, "voice-request")

	if len(published) != 1 {
		t.Fatalf("published messages = %#v, want one streaming reasoning message", published)
	}
	message := published[0]
	if message.Type != "assistant" || message.Status != "streaming" || message.Source != "voice" {
		t.Fatalf("streaming reasoning message = %#v", message)
	}
	if message.RequestID != "voice-request" || message.EpisodeID != "episode-1" || message.ReasoningContent != "first thought" {
		t.Fatalf("streaming reasoning payload = %#v", message)
	}
}

func TestAudioDialogEnsureVoiceInputAudioArtifactSavesAudioFile(t *testing.T) {
	tmpDir := t.TempDir()

	cfg := Config{
		ConfigDir: tmpDir,
		InputMode: "stt",
		STT:       STTConfig{Provider: "openai-whisper"},
		Audio:     AudioConfig{SampleRate: 16000},
		AudioArchive: AudioArchiveConfig{
			Enabled:     true,
			StoragePath: filepath.Join(tmpDir, "audio"),
		},
		Model: ModelConfig{Provider: "fake"},
	}

	dialog, err := NewAudioDialog(&Runtime{config: cfg})
	if err != nil {
		t.Fatal(err)
	}

	utterance := make([]int16, 16000) // 1 second
	input := dialog.ensureVoiceInputAudioArtifact(TurnInput{Transcript: "hello"}, utterance, nil)

	// Verify audio file saved
	files, err := os.ReadDir(filepath.Join(tmpDir, "audio"))
	if err != nil {
		t.Fatal(err)
	}

	if len(files) != 1 {
		t.Fatalf("Expected 1 audio file, got %d", len(files))
	}

	if !strings.HasSuffix(files[0].Name(), ".wav") {
		t.Errorf("Audio file should be WAV: %s", files[0].Name())
	}

	artifact, ok := firstAudioArtifact(input)
	if !ok {
		t.Fatal("audio artifact missing")
	}
	if artifact.Path == "" {
		t.Error("audio artifact path is empty")
	}
	if artifact.DurationMS == 0 {
		t.Error("audio artifact duration is zero")
	}
	if _, err := os.Stat(artifact.Path); err != nil {
		t.Errorf("audio file should exist at %s: %v", artifact.Path, err)
	}
}

type blockingFirstCallModel struct {
	firstCallStarted chan struct{}
	releaseFirstCall chan struct{}
	responses        []*llms.ContentResponse
	callCount        int
}

func (m *blockingFirstCallModel) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	call := m.callCount
	m.callCount++
	if call == 0 {
		close(m.firstCallStarted)
		select {
		case <-m.releaseFirstCall:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if call >= len(m.responses) {
		return contentResponse(""), nil
	}
	return m.responses[call], nil
}

func (m *blockingFirstCallModel) Call(ctx context.Context, prompt string, options ...llms.CallOption) (string, error) {
	resp, err := m.GenerateContent(ctx, nil, options...)
	if err != nil {
		return "", err
	}
	if resp == nil || len(resp.Choices) == 0 || resp.Choices[0] == nil {
		return "", nil
	}
	return resp.Choices[0].Content, nil
}

func (m *blockingFirstCallModel) Spec() model.ModelSpec {
	return model.ModelSpec{
		Provider:      "fake",
		Name:          "scripted",
		ContextWindow: 1_000_000,
	}
}

func (m *blockingFirstCallModel) CallOptions() []chains.ChainCallOption { return nil }

func TestAudioDialogRunAgentTurnUsesModelMaxResponseTokens(t *testing.T) {
	for _, tt := range []struct {
		name      string
		maxTokens int
	}{
		{name: "default", maxTokens: DefaultConfig().Model.MaxResponseTokens},
		{name: "custom", maxTokens: 4096},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := withTestConfigDir(t, Config{
				Model: ModelConfig{Provider: "fake", MaxResponseTokens: tt.maxTokens},
			})
			model := &scriptedModel{responses: roleDirectResponses("ok")}
			models := NewModelManager(cfg.Model, ProxyConfig{})
			models.model = model
			runtime := NewRuntimeWithDeps(
				cfg, models, NewMemoryManager(""),
				&ToolSet{tools: map[string]langtools.Tool{}}, NewSkillIndex(),
			)
			// The runtime owns the current model settings; a live voice dialog
			// may still hold the configuration from before a model reload.
			dialogConfig := cfg
			dialogConfig.Model.MaxResponseTokens = 128
			dialog := &AudioDialog{config: dialogConfig}
			if _, err := dialog.RunAgentTurn(context.Background(), TurnInput{InputText: "hello"}, runtime); err != nil {
				t.Fatalf("RunAgentTurn() error = %v", err)
			}
			if len(model.maxTokens) == 0 {
				t.Fatal("voice turn did not call the model")
			}
			for i, got := range model.maxTokens {
				if got != tt.maxTokens {
					t.Fatalf("model call %d max tokens = %d, want %d", i, got, tt.maxTokens)
				}
			}
		})
	}
}

func TestAudioDialogRunAgentTurnCommitsVoicePreRunTelemetry(t *testing.T) {
	plane := &capturingEpisodePlane{}
	model := &scriptedModel{responses: roleDirectResponses("ok")}
	cfg := withTestConfigDir(t, Config{
		Model:                    ModelConfig{Provider: "fake"},
		Audio:                    AudioConfig{SampleRate: 16000},
		VoiceStreamingTTSEnabled: boolPtr(true),
	})
	runtime := NewRuntimeWithDeps(
		cfg,
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.memoryPlane = plane
	provider := &recordingTTSProvider{name: "dialog-provider"}
	dialog := &AudioDialog{
		config:      cfg,
		audioClient: NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
	}

	if _, err := dialog.RunAgentTurn(context.Background(), TurnInput{InputText: "hello"}, runtime); err != nil {
		t.Fatalf("RunAgentTurn() error = %v", err)
	}

	var promptIndex, preopenIndex, sessionIndex = -1, -1, -1
	for index, event := range plane.episode.Events {
		switch event.Type {
		case runEventVoicePromptSound:
			promptIndex = index
			if event.Metadata["prompt"] != "agent_send" || event.Metadata["success"] != true || event.Metadata["wait"] != false || event.Metadata["async"] != true {
				t.Fatalf("prompt telemetry metadata = %#v", event.Metadata)
			}
			if event.DurationMs == nil || *event.DurationMs <= 0 || event.Metadata["cue_audio_duration_ms"] == nil {
				t.Fatalf("prompt telemetry duration = %v metadata = %#v", event.DurationMs, event.Metadata)
			}
		case runEventTTSStreamPreopen:
			preopenIndex = index
			if event.Metadata["provider"] != "dialog-provider" || event.Metadata["success"] != true {
				t.Fatalf("TTS preopen telemetry metadata = %#v", event.Metadata)
			}
		case runEventSessionBegin:
			sessionIndex = index
		}
	}
	if promptIndex < 0 || preopenIndex < 0 || sessionIndex < 0 {
		t.Fatalf("missing pre-run telemetry/session events: %#v", plane.episode.Events)
	}
	if !(promptIndex < preopenIndex && preopenIndex < sessionIndex) {
		t.Fatalf("event order prompt=%d preopen=%d session=%d events=%#v", promptIndex, preopenIndex, sessionIndex, plane.episode.Events)
	}
}

func TestAudioDialogRunAgentTurnDoesNotWaitForAgentSendPromptDrain(t *testing.T) {
	audioServer := newTestAudioService(t)
	audioServer.healthPlaybackSessions = []uint32{1}
	model := &scriptedModel{responses: roleDirectResponses("ok")}
	cfg := withTestConfigDir(t, Config{
		Model:                    ModelConfig{Provider: "fake"},
		VoiceStreamingTTSEnabled: boolPtr(false),
	})
	runtime := NewRuntimeWithDeps(
		cfg,
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	dialog := &AudioDialog{
		config:      cfg,
		audioClient: NewAudioServiceClient(audioServer.socketPath),
	}

	startedAt := time.Now()
	if _, err := dialog.RunAgentTurn(context.Background(), TurnInput{InputText: "hello"}, runtime); err != nil {
		t.Fatalf("RunAgentTurn() error = %v", err)
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("RunAgentTurn waited for prompt drain: elapsed=%s", elapsed)
	}
	if got := audioServer.countOp("health"); got != 0 {
		t.Fatalf("agent send prompt health polls = %d, want 0 for wait=false", got)
	}
}

func TestAudioDialogRunAgentTurnDoesNotWaitForAgentSendPromptStart(t *testing.T) {
	audioServer := newTestAudioService(t)
	promptStarted := make(chan struct{})
	releasePrompt := make(chan struct{})
	var promptOnce sync.Once
	var releaseOnce sync.Once
	release := func() {
		releaseOnce.Do(func() { close(releasePrompt) })
	}
	t.Cleanup(release)
	audioServer.onStartPlayback = func() {
		promptOnce.Do(func() { close(promptStarted) })
		<-releasePrompt
	}
	model := &scriptedModel{responses: roleDirectResponses("ok")}
	cfg := withTestConfigDir(t, Config{
		Model:                    ModelConfig{Provider: "fake"},
		VoiceStreamingTTSEnabled: boolPtr(false),
	})
	runtime := NewRuntimeWithDeps(
		cfg,
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	dialog := &AudioDialog{
		config:      cfg,
		audioClient: NewAudioServiceClient(audioServer.socketPath),
	}

	startedAt := time.Now()
	done := make(chan error, 1)
	go func() {
		_, err := dialog.RunAgentTurn(context.Background(), TurnInput{InputText: "hello"}, runtime)
		done <- err
	}()
	waitForTestSignal(t, promptStarted, "prompt sound playback to start")

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("RunAgentTurn() error = %v", err)
		}
	case <-time.After(500 * time.Millisecond):
		t.Fatal("RunAgentTurn waited for agent send prompt start to complete")
	}
	if elapsed := time.Since(startedAt); elapsed > 500*time.Millisecond {
		t.Fatalf("RunAgentTurn waited for prompt start: elapsed=%s", elapsed)
	}
}

func waitForProviderTextCount(t *testing.T, provider *recordingTTSProvider, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got := len(provider.texts())
		if got >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func waitForFlushProviderTextCount(t *testing.T, provider *flushRecordingTTSProvider, count int) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		got := len(provider.texts())
		if got >= count {
			return
		}
		time.Sleep(time.Millisecond)
	}
}

func assertNoProviderTextWithin(t *testing.T, provider *recordingTTSProvider, duration time.Duration) {
	t.Helper()
	deadline := time.Now().Add(duration)
	for time.Now().Before(deadline) {
		got := provider.texts()
		if len(got) != 0 {
			t.Fatalf("unexpected TTS calls: %#v", got)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func countString(values []string, want string) int {
	count := 0
	for _, value := range values {
		if value == want {
			count++
		}
	}
	return count
}

func serveDelayedStartRecording(t *testing.T, socketPath string, ready chan<- struct{}) {
	t.Helper()
	time.Sleep(30 * time.Millisecond)
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Errorf("listen unix socket: %v", err)
		close(ready)
		return
	}
	defer os.Remove(socketPath)
	defer listener.Close()

	if unixListener, ok := listener.(*net.UnixListener); ok {
		_ = unixListener.SetDeadline(time.Now().Add(2 * time.Second))
	}

	for {
		conn, err := listener.Accept()
		if err != nil {
			t.Errorf("accept unix socket: %v", err)
			close(ready)
			return
		}

		msg, err := readUdsMessage(conn)
		if err != nil {
			conn.Close()
			t.Errorf("read request: %v", err)
			close(ready)
			return
		}
		var req audioRequest
		if err := json.Unmarshal([]byte(msg.HeaderJSON), &req); err != nil {
			conn.Close()
			t.Errorf("unmarshal request: %v", err)
			close(ready)
			return
		}

		resp := `{"status":"OK"}`
		if req.Op == "start_playback" {
			resp = `{"status":"OK","session_id":"7"}`
		}
		if req.Op == "health" {
			resp = `{"status":"OK","recording_active":false,"playback_active":false,"record_sessions":0,"playback_sessions":0}`
		}
		if req.Op == "start_recording" {
			resp = `{"status":"OK","session_id":"42"}`
		}
		if err := writeUdsMessage(conn, udsMessage{HeaderJSON: resp}); err != nil {
			conn.Close()
			t.Errorf("write response: %v", err)
			close(ready)
			return
		}
		conn.Close()

		if req.Op == "start_recording" {
			close(ready)
			return
		}
	}
}
