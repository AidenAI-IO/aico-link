package agent

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"

	speechtext "aico-agent/internal/agent/speech"
	ttsmodule "aico-agent/internal/agent/tts"
)

func TestTTSPlaybackFormatUsesProviderOnlySampleRate(t *testing.T) {
	format := ttsPlaybackFormat(Config{Audio: AudioConfig{SampleRate: 16000}}, ttsmodule.Capabilities{
		SupportedSampleRates: []int{24000},
	})
	if format.SampleRate != 24000 {
		t.Fatalf("SampleRate = %d, want provider-only 24000", format.SampleRate)
	}
	if format.Channels != 1 || format.BitWidth != 16 {
		t.Fatalf("unexpected PCM format: %#v", format)
	}
}

func TestManagedTTSResamplesProviderOnly24kToConfiguredPlaybackRate(t *testing.T) {
	provider := &formatCheckingTTSProvider{
		name:       "provider-only-24k",
		caps:       ttsmodule.Capabilities{SupportedSampleRates: []int{24000}},
		wantFormat: ttsmodule.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16},
	}

	var startRate atomic.Uint32
	socketPath := startFakeAudioServiceSocket(t, func(req audioRequest) (audioResponse, []byte) {
		switch req.Op {
		case "start_playback":
			startRate.Store(req.SampleRate)
			return audioResponse{Status: "OK", SessionID: stringUint64(7)}, nil
		case "write_play_chunk":
			return audioResponse{Status: "OK"}, nil
		case "health":
			return audioResponse{Status: "OK", PlaybackSessions: 0}, nil
		case "stop_playback":
			return audioResponse{Status: "OK"}, nil
		default:
			return audioResponse{Status: "INTERNAL_ERROR"}, nil
		}
	})

	server := &Server{
		runtime: NewRuntimeWithDeps(
			Config{Model: ModelConfig{Provider: "fake"}, Audio: AudioConfig{SampleRate: 16000}},
			&testModelResolver{model: &scriptedModel{}},
			NewMemoryManager(""),
			NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
			NewSkillIndex(),
		),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
		audioClient: NewAudioServiceClient(socketPath),
	}

	if err := server.speakText(context.Background(), "hello", 0); err != nil {
		t.Fatalf("speakText() error = %v", err)
	}
	if got := startRate.Load(); got != 16000 {
		t.Fatalf("start_playback sample_rate = %d, want configured playback rate 16000", got)
	}
}

func TestStreamSessionWriterDoesNotReportSpokenWhenCloseFails(t *testing.T) {
	w := &streamSessionWriter{session: &failingStreamSession{closeErr: context.Canceled}}
	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := w.closeAndWait(); err == nil {
		t.Fatal("closeAndWait() error = nil, want failure")
	}
	if w.spoke {
		t.Fatal("spoke = true, want false when stream close failed")
	}
}

func TestStreamSessionWriterAbortsPreopenedSessionWhenNoTextWritten(t *testing.T) {
	session := &abortableStreamSession{}
	w := &streamSessionWriter{session: session}

	if err := w.closeAndWait(); err != nil {
		t.Fatalf("closeAndWait() error = %v", err)
	}
	if session.abortCalls != 1 {
		t.Fatalf("Abort() calls = %d, want 1", session.abortCalls)
	}
	if session.closeCalls != 0 {
		t.Fatalf("Close() calls = %d, want 0", session.closeCalls)
	}
}

func TestStreamSessionWriterInterruptAbortsSession(t *testing.T) {
	session := &abortableStreamSession{}
	w := &streamSessionWriter{session: session}

	w.interrupt()

	if session.abortCalls != 1 {
		t.Fatalf("Abort() calls = %d, want 1", session.abortCalls)
	}
	if session.closeCalls != 0 {
		t.Fatalf("Close() calls = %d, want 0", session.closeCalls)
	}
	if err := w.closeAndWait(); err != nil {
		t.Fatalf("closeAndWait() error = %v", err)
	}
	if session.abortCalls != 1 {
		t.Fatalf("Abort() calls after closeAndWait = %d, want 1", session.abortCalls)
	}
}

func TestStreamSessionWriterClosesSessionWhenTextWritten(t *testing.T) {
	session := &abortableStreamSession{}
	w := &streamSessionWriter{session: session}

	if _, err := w.Write([]byte("hello")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := w.closeAndWait(); err != nil {
		t.Fatalf("closeAndWait() error = %v", err)
	}
	if session.closeCalls != 1 {
		t.Fatalf("Close() calls = %d, want 1", session.closeCalls)
	}
	if session.abortCalls != 0 {
		t.Fatalf("Abort() calls = %d, want 0", session.abortCalls)
	}
}

func TestStreamSessionWriterFlushesProviderWithoutInterruptingLLMStream(t *testing.T) {
	flushErr := errors.New("flush failed")
	session := &flushTrackingStreamSession{flushErr: flushErr}
	w := &streamSessionWriter{session: session}

	if _, err := w.Write([]byte("short speech")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	if err := w.Flush(); err != nil {
		t.Fatalf("Flush() error = %v, want provider error isolated from LLM stream", err)
	}
	if session.flushCalls != 1 {
		t.Fatalf("provider Flush() calls = %d, want 1", session.flushCalls)
	}
	if err := w.closeAndWait(); !errors.Is(err, flushErr) {
		t.Fatalf("closeAndWait() error = %v, want recorded flush error", err)
	}
}

func TestHandleTTSSettingsPostEnablesAudioDialogWhenInitiallyUnconfigured(t *testing.T) {
	provider := &recordingTTSProvider{name: "minimax-cn"}
	manager := ttsmodule.NewProviderManagerWithFactory(nil, nil, func(cfg ttsmodule.ProviderConfig) (ttsmodule.TTSProvider, error) {
		if cfg.Provider != "minimax-cn" || cfg.APIKey != "test-key" {
			t.Fatalf("factory config = %#v, want minimax-cn with request API key", cfg)
		}
		return provider, nil
	})
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{
			Model: ModelConfig{Provider: "fake"},
			Audio: AudioConfig{
				Socket:     startTTSPlaybackAudioSocket(t),
				SampleRate: 16000,
			},
		}),
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	runtime.ttsManager = manager
	server := &Server{runtime: runtime}
	dialog, err := NewAudioDialog(runtime)
	if err != nil {
		t.Fatalf("NewAudioDialog() error = %v", err)
	}
	stableManager := runtime.ttsProviderManager()
	if dialog.ttsManager != stableManager {
		t.Fatal("audio dialog does not reference Runtime's stable TTS manager")
	}
	if dialog.currentTTSManager() != nil {
		t.Fatal("audio dialog TTS manager is configured before POST")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/settings/tts", bytes.NewBufferString(`{"provider":"minimax-cn","api_key":"test-key"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	server.handleTTSSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if manager := server.currentTTSManager(); manager != stableManager || manager.Current() != "minimax-cn" {
		t.Fatalf("manager = %#v, want shared initialized minimax-cn manager", manager)
	}
	if manager := dialog.currentTTSManager(); manager != stableManager || manager.Current() != "minimax-cn" {
		t.Fatalf("audio dialog manager = %#v, want shared initialized minimax-cn manager", manager)
	}

	if err := dialog.Speak(context.Background(), "enabled after POST", nil); err != nil {
		t.Fatalf("AudioDialog.Speak() error = %v", err)
	}
	if got := provider.beginCalls(); got != 1 {
		t.Fatalf("new provider BeginStream calls = %d, want 1", got)
	}
	if got := provider.texts(); len(got) != 1 || got[0] != "enabled after POST" {
		t.Fatalf("new provider texts = %#v, want AudioDialog speech", got)
	}
}

func TestTTSSettingsSwitchRoutesAudioDialogSpeechToNewProvider(t *testing.T) {
	oldProvider := &recordingTTSProvider{name: "alicloud"}
	newProvider := &recordingTTSProvider{name: "minimax-cn"}
	manager := ttsmodule.NewProviderManagerWithFactory(oldProvider, nil, func(cfg ttsmodule.ProviderConfig) (ttsmodule.TTSProvider, error) {
		if cfg.Provider != "minimax-cn" || cfg.APIKey != "new-key" {
			t.Fatalf("factory config = %#v, want minimax-cn with request API key", cfg)
		}
		return newProvider, nil
	})
	cfg := withTestConfigDir(t, Config{
		Model: ModelConfig{Provider: "fake"},
		TTS: TTSConfig{
			Provider: "alicloud",
			APIKey:   "old-key",
		},
		Audio: AudioConfig{Socket: startTTSPlaybackAudioSocket(t), SampleRate: 16000},
	})
	runtime := NewRuntimeWithDeps(
		cfg,
		&testModelResolver{model: &scriptedModel{}},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	runtime.ttsManager = manager
	server := &Server{runtime: runtime}
	dialog, err := NewAudioDialog(runtime)
	if err != nil {
		t.Fatalf("NewAudioDialog() error = %v", err)
	}
	if server.ttsProviderManager() != runtime.ttsProviderManager() || dialog.ttsManager != runtime.ttsProviderManager() {
		t.Fatal("Server and AudioDialog do not share Runtime's TTS manager")
	}

	req := httptest.NewRequest(http.MethodPost, "/api/settings/tts", bytes.NewBufferString(`{"provider":"minimax-cn","api_key":"new-key"}`))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleTTSSettings(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("unexpected status: %d body=%s", rec.Code, rec.Body.String())
	}
	if got := server.currentTTSManager().Current(); got != "minimax-cn" {
		t.Fatalf("server provider = %q, want minimax-cn", got)
	}
	if got := dialog.ttsManager.Current(); got != "minimax-cn" {
		t.Fatalf("audio dialog provider = %q, want minimax-cn", got)
	}

	if err := dialog.Speak(context.Background(), "spoken by switched provider", nil); err != nil {
		t.Fatalf("AudioDialog.Speak() error = %v", err)
	}
	if got := newProvider.beginCalls(); got != 1 {
		t.Fatalf("new provider BeginStream calls = %d, want 1", got)
	}
	if got := newProvider.texts(); len(got) != 1 || got[0] != "spoken by switched provider" {
		t.Fatalf("new provider texts = %#v, want AudioDialog speech", got)
	}
	if got := oldProvider.beginCalls(); got != 0 {
		t.Fatalf("old provider BeginStream calls = %d, want 0 after POST", got)
	}
}

func TestRuntimeCloseClosesSharedTTSManagerOnce(t *testing.T) {
	provider := &recordingTTSProvider{name: "shared-provider"}
	manager := ttsmodule.NewProviderManager(provider, nil)
	runtime := &Runtime{
		config:     Config{Model: ModelConfig{Provider: "fake"}, Audio: AudioConfig{Socket: "/tmp/audio.sock", SampleRate: 16000}},
		ttsManager: manager,
	}
	dialog, err := NewAudioDialog(runtime)
	if err != nil {
		t.Fatalf("NewAudioDialog() error = %v", err)
	}
	if dialog.ttsManager != manager {
		t.Fatal("AudioDialog does not reference Runtime's TTS manager")
	}

	if err := dialog.Close(); err != nil {
		t.Fatalf("AudioDialog.Close() error = %v", err)
	}
	if got := provider.providerCloseCalls(); got != 0 {
		t.Fatalf("provider Close() calls after AudioDialog.Close() = %d, want 0", got)
	}

	if err := runtime.Close(); err != nil {
		t.Fatalf("Runtime.Close() error = %v", err)
	}
	if got := provider.providerCloseCalls(); got != 1 {
		t.Fatalf("provider Close() calls = %d, want 1", got)
	}
}

func TestServerSpeakTextUsesProviderManager(t *testing.T) {
	provider := &recordingTTSProvider{name: "test-current-provider"}
	server := &Server{
		runtime: NewRuntimeWithDeps(
			Config{Model: ModelConfig{Provider: "fake"}, Audio: AudioConfig{SampleRate: 16000}},
			&testModelResolver{model: &scriptedModel{}},
			NewMemoryManager(""),
			NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
			NewSkillIndex(),
		),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
		audioClient: NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
	}

	if err := server.speakText(context.Background(), "hello", 0); err != nil {
		t.Fatalf("speakText() error = %v", err)
	}
	if got := provider.texts(); len(got) != 1 || got[0] != "hello" {
		t.Fatalf("provider texts = %#v, want hello", got)
	}
}

func TestSpeakTextDoesNotRetryAfterPlaybackStarts(t *testing.T) {
	provider := &playbackStartedTransientErrorProvider{name: "transient-after-playback"}
	audioOps := &recordedAudioOps{}
	server := &Server{
		runtime: NewRuntimeWithDeps(
			Config{Model: ModelConfig{Provider: "fake"}, Audio: AudioConfig{SampleRate: 16000}},
			&testModelResolver{model: &scriptedModel{}},
			NewMemoryManager(""),
			NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
			NewSkillIndex(),
		),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
		audioClient: NewAudioServiceClient(startRecordedTTSPlaybackAudioSocket(t, audioOps)),
	}

	err := server.speakText(context.Background(), "hello", 0)
	if err == nil || !isTransientTTSError(err) {
		t.Fatalf("speakText() error = %v, want transient TTS error after playback started", err)
	}
	if got := audioOps.countOp("start_playback"); got != 1 {
		t.Fatalf("start_playback count = %d, want no retry after playback started", got)
	}
	if got := provider.beginCalls(); got != 1 {
		t.Fatalf("BeginStream calls = %d, want 1", got)
	}
}

func TestNewAudioDialogUsesRuntimeProviderManager(t *testing.T) {
	dialog, err := NewAudioDialog(&Runtime{config: Config{
		Model: ModelConfig{Provider: "fake"},
		TTS: TTSConfig{
			Provider: "alicloud",
			APIKey:   "test-key",
		},
		Audio: AudioConfig{Socket: "/tmp/audio.sock", SampleRate: 16000},
	}})
	if err != nil {
		t.Fatalf("NewAudioDialog() error = %v", err)
	}
	if dialog.ttsManager == nil {
		t.Fatal("ttsManager = nil, want alicloud manager")
	}
	if dialog.ttsManager.Current() != "alicloud" {
		t.Fatalf("ttsManager current = %q, want alicloud", dialog.ttsManager.Current())
	}
}

func TestTTSProviderAliasesAreNotRegistered(t *testing.T) {
	for _, provider := range []string{"fish", "qwen", "qwen-tts"} {
		_, err := ttsmodule.New(ttsmodule.ProviderConfig{Provider: provider, APIKey: "test-key"})
		if err == nil {
			t.Fatalf("tts.New(%q) succeeded, want unsupported provider", provider)
		}
	}
}

func TestMinimaxTTSProvidersAreRegistered(t *testing.T) {
	for _, name := range []string{"minimax", "minimax-cn"} {
		t.Run(name, func(t *testing.T) {
			provider, err := ttsmodule.New(ttsmodule.ProviderConfig{Provider: name, APIKey: "test-key"})
			if err != nil {
				t.Fatalf("tts.New(%s) error = %v", name, err)
			}
			defer provider.Close()
			if provider.Name() != name {
				t.Fatalf("provider.Name() = %q, want %s", provider.Name(), name)
			}
			if !containsProviderName(ttsmodule.AvailableProviders(), name) {
				t.Fatalf("AvailableProviders() missing %s: %#v", name, ttsmodule.AvailableProviders())
			}
		})
	}
}

func TestMinimaxWebSocketLegacyProviderIsNotRegistered(t *testing.T) {
	_, err := ttsmodule.New(ttsmodule.ProviderConfig{Provider: "minimax-ws", APIKey: "test-key"})
	if err == nil {
		t.Fatal("tts.New(minimax-ws) succeeded, want unsupported legacy provider")
	}
	if containsProviderName(ttsmodule.AvailableProviders(), "minimax-ws") {
		t.Fatalf("AvailableProviders() includes minimax-ws: %#v", ttsmodule.AvailableProviders())
	}
}

func TestVolcengineTTSProviderIsRegistered(t *testing.T) {
	provider, err := ttsmodule.New(ttsmodule.ProviderConfig{
		Provider: "volcengine",
		APIKey:   "test-key",
		Voice:    "test-speaker",
	})
	if err != nil {
		t.Fatalf("tts.New(volcengine) error = %v", err)
	}
	defer provider.Close()
	if provider.Name() != "volcengine" {
		t.Fatalf("provider.Name() = %q, want volcengine", provider.Name())
	}
	if !containsProviderName(ttsmodule.AvailableProviders(), "volcengine") {
		t.Fatalf("AvailableProviders() missing volcengine: %#v", ttsmodule.AvailableProviders())
	}
}

func containsProviderName(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func TestAudioDialogRunAgentTurnStreamsTTSTagThroughProviderManager(t *testing.T) {
	model := &rawStreamingModel{
		content: "<tts>streamed answer</tts>\nstreamed answer",
		chunks:  []string{"<t", "ts>streamed ", "answer</tts>\nstreamed answer"},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	streamingEnabled := true
	provider := &recordingTTSProvider{name: "dialog-provider"}
	dialog := &AudioDialog{
		config: Config{
			Model:                    ModelConfig{Provider: "fake"},
			Audio:                    AudioConfig{SampleRate: 16000},
			VoiceStreamingTTSEnabled: &streamingEnabled,
		},
		audioClient: NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
	}
	runtime.RegisterPreemptHook(dialog.InterruptOutput)

	result, err := dialog.RunAgentTurn(context.Background(), TurnInput{InputText: "hello"}, runtime)
	if err != nil {
		t.Fatalf("RunAgentTurn() error = %v", err)
	}
	if !result.SpeechStreamed {
		t.Fatal("SpeechStreamed = false, want true with streaming callback")
	}
	if got := provider.texts(); len(got) != 1 || got[0] != "streamed answer" {
		t.Fatalf("provider texts = %#v, want TTS tag content", got)
	}
}

func TestAudioDialogRunAgentTurnReturnsFullAnswerWithStreamedSpeech(t *testing.T) {
	model := &rawStreamingModel{
		content: "<tts>已完成设置，当前音量是 42。</tts>\n已完成设置，当前音量是 42。\n\n完整回答保留给屏幕。",
		chunks: []string{
			"<tts>已完成设置，当前音量是 42。</tt",
			"s>\n已完成设置，当前音量是 42。\n\n完整回答保留给屏幕。",
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		NewBuiltinToolSet(HIDConfig{}, AudioConfig{}, SearchConfig{}, ProxyConfig{}),
		NewSkillIndex(),
	)
	streamingEnabled := true
	provider := &recordingTTSProvider{name: "dialog-provider"}
	dialog := &AudioDialog{
		config: Config{
			Model:                    ModelConfig{Provider: "fake"},
			Audio:                    AudioConfig{SampleRate: 16000},
			VoiceStreamingTTSEnabled: &streamingEnabled,
		},
		audioClient: NewAudioServiceClient(startTTSPlaybackAudioSocket(t)),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
	}

	result, err := dialog.RunAgentTurn(context.Background(), TurnInput{InputText: "hello"}, runtime)
	if err != nil {
		t.Fatalf("RunAgentTurn() error = %v", err)
	}
	if !result.SpeechStreamed {
		t.Fatal("SpeechStreamed = false, want true with streaming callback")
	}
	wantOutput := "<tts>已完成设置，当前音量是 42。</tts>\n已完成设置，当前音量是 42。\n\n完整回答保留给屏幕。"
	if result.Output != wantOutput {
		t.Fatalf("Output = %q", result.Output)
	}
	if got := provider.texts(); len(got) != 1 || got[0] != "已完成设置，当前音量是 42。" {
		t.Fatalf("provider texts = %#v, want TTS tag speech", got)
	}
}
func TestAudioDialogInterruptOutputStopsBackgroundToolSpeech(t *testing.T) {
	provider := newInterruptibleAudioTTSProvider("dialog-provider", 48000, true)
	audioOps := &recordedAudioOps{}
	dialog := &AudioDialog{
		config: Config{
			Model: ModelConfig{Provider: "fake"},
			Audio: AudioConfig{SampleRate: 48000},
		},
		audioClient: NewAudioServiceClient(startRecordedTTSPlaybackAudioSocket(t, audioOps)),
		ttsManager:  ttsmodule.NewProviderManager(provider, nil),
	}

	done := make(chan struct{})
	go func() {
		dialog.SpeakToolContent("tool is running")
		close(done)
	}()

	waitForTestSignal(t, provider.firstWriteDone(), "tool TTS playback to start")
	dialog.InterruptOutput()
	waitForTestSignal(t, done, "tool TTS to stop")

	if got := audioOps.countOp("stop_playback"); got != 1 {
		t.Fatalf("stop_playback count = %d, want 1", got)
	}
	if got := audioOps.finalChunkCount(); got != 0 {
		t.Fatalf("final write_play_chunk count = %d, want 0 after tool speech interrupt", got)
	}
}

func TestRuntimeRunStreamsTTSTaggedChunksToWriter(t *testing.T) {
	model := &rawStreamingModel{
		content: "<tts>播报摘要。</tts>\n完整回答保留给屏幕。",
		chunks: []string{
			"<t",
			"ts>播报摘要。</tts>\n完整回答保留给屏幕。",
		},
	}
	runtime := NewRuntimeWithDeps(
		withTestConfigDir(t, Config{Model: ModelConfig{Provider: "fake"}}),
		&testModelResolver{model: model},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	var stream strings.Builder

	result, err := runtime.Run(context.Background(), RunRequest{
		Input:        "hello",
		StreamWriter: speechtext.NewStreamWriter(&stream),
	})
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if stream.String() != "播报摘要。" {
		t.Fatalf("stream = %q, want TTS tag content", stream.String())
	}
	if result.Output != "<tts>播报摘要。</tts>\n完整回答保留给屏幕。" {
		t.Fatalf("Output = %q", result.Output)
	}
}

type rawStreamingModel struct {
	content string
	chunks  []string
}

func (m *rawStreamingModel) Call(ctx context.Context, prompt string, options ...llms.CallOption) (string, error) {
	return m.content, nil
}

func (m *rawStreamingModel) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	var callOptions llms.CallOptions
	for _, option := range options {
		option(&callOptions)
	}
	if callOptions.StreamingFunc != nil {
		for _, chunk := range m.chunks {
			if err := callOptions.StreamingFunc(ctx, []byte(chunk)); err != nil {
				return nil, err
			}
		}
	}
	return contentResponse(m.content), nil
}

type interruptibleAudioTTSProvider struct {
	name        string
	pcmBytes    int
	blockWrites bool
	firstWrite  chan struct{}
	writeOnce   sync.Once
}

func newInterruptibleAudioTTSProvider(name string, pcmBytes int, blockWrites bool) *interruptibleAudioTTSProvider {
	return &interruptibleAudioTTSProvider{
		name:        name,
		pcmBytes:    pcmBytes,
		blockWrites: blockWrites,
		firstWrite:  make(chan struct{}),
	}
}

func (p *interruptibleAudioTTSProvider) Name() string { return p.name }

func (p *interruptibleAudioTTSProvider) Capabilities() ttsmodule.Capabilities {
	return ttsmodule.Capabilities{}
}

func (p *interruptibleAudioTTSProvider) BeginStream(ctx context.Context, sink ttsmodule.AudioSink) (ttsmodule.StreamSession, error) {
	return &interruptibleAudioTTSSession{provider: p, ctx: ctx, sink: sink}, nil
}

func (p *interruptibleAudioTTSProvider) Close() error { return nil }

func (p *interruptibleAudioTTSProvider) firstWriteDone() <-chan struct{} {
	return p.firstWrite
}

func (p *interruptibleAudioTTSProvider) signalFirstWrite() {
	p.writeOnce.Do(func() {
		close(p.firstWrite)
	})
}

type interruptibleAudioTTSSession struct {
	provider *interruptibleAudioTTSProvider
	ctx      context.Context
	sink     ttsmodule.AudioSink
	err      error
}

func (s *interruptibleAudioTTSSession) WriteText(text string) error {
	if s.provider.pcmBytes > 0 {
		if err := s.sink.WritePCM(make([]byte, s.provider.pcmBytes)); err != nil {
			s.err = err
			s.provider.signalFirstWrite()
			return err
		}
	}
	s.provider.signalFirstWrite()

	if s.provider.blockWrites {
		<-s.ctx.Done()
		s.err = s.ctx.Err()
		return s.err
	}
	return nil
}

func (s *interruptibleAudioTTSSession) Flush() error { return nil }

func (s *interruptibleAudioTTSSession) Close() error {
	return nil
}

func (s *interruptibleAudioTTSSession) Err() error { return s.err }

type recordedAudioOps struct {
	mu  sync.Mutex
	ops []audioRequest
}

func startRecordedTTSPlaybackAudioSocket(t *testing.T, ops *recordedAudioOps) string {
	t.Helper()
	if ops == nil {
		ops = &recordedAudioOps{}
	}
	return startFakeAudioServiceSocket(t, func(req audioRequest) (audioResponse, []byte) {
		ops.append(req)
		switch req.Op {
		case "start_playback":
			return audioResponse{Status: "OK", SessionID: stringUint64(7)}, nil
		case "write_play_chunk":
			return audioResponse{Status: "OK"}, nil
		case "health":
			return audioResponse{Status: "OK", PlaybackSessions: 0}, nil
		case "stop_playback":
			return audioResponse{Status: "OK"}, nil
		default:
			return audioResponse{Status: "INTERNAL_ERROR"}, nil
		}
	})
}

func (r *recordedAudioOps) append(req audioRequest) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ops = append(r.ops, req)
}

func (r *recordedAudioOps) countOp(op string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, req := range r.ops {
		if req.Op == op {
			count++
		}
	}
	return count
}

func (r *recordedAudioOps) finalChunkCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	for _, req := range r.ops {
		if req.Op == "write_play_chunk" && req.IsFinal {
			count++
		}
	}
	return count
}

func (r *recordedAudioOps) finalChunkCountAfterFirstStop() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	count := 0
	seenStop := false
	for _, req := range r.ops {
		if req.Op == "stop_playback" {
			seenStop = true
			continue
		}
		if seenStop && req.Op == "write_play_chunk" && req.IsFinal {
			count++
		}
	}
	return count
}

func waitForTestSignal(t *testing.T, ch <-chan struct{}, name string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", name)
	}
}

type failingStreamSession struct {
	closeErr error
}

func (s *failingStreamSession) WriteText(string) error { return nil }
func (s *failingStreamSession) Flush() error           { return nil }
func (s *failingStreamSession) Close() error           { return s.closeErr }
func (s *failingStreamSession) Err() error             { return s.closeErr }

type abortableStreamSession struct {
	closeCalls int
	abortCalls int
}

func (s *abortableStreamSession) WriteText(string) error { return nil }
func (s *abortableStreamSession) Flush() error           { return nil }
func (s *abortableStreamSession) Close() error {
	s.closeCalls++
	return nil
}
func (s *abortableStreamSession) Abort() error {
	s.abortCalls++
	return nil
}
func (s *abortableStreamSession) Err() error { return nil }

type flushTrackingStreamSession struct {
	flushCalls int
	flushErr   error
}

func (s *flushTrackingStreamSession) WriteText(string) error { return nil }
func (s *flushTrackingStreamSession) Flush() error {
	s.flushCalls++
	return s.flushErr
}
func (s *flushTrackingStreamSession) Close() error { return nil }
func (s *flushTrackingStreamSession) Err() error   { return s.flushErr }

type formatCheckingTTSProvider struct {
	name       string
	caps       ttsmodule.Capabilities
	wantFormat ttsmodule.AudioFormat
}

func (p *formatCheckingTTSProvider) Name() string { return p.name }

func (p *formatCheckingTTSProvider) Capabilities() ttsmodule.Capabilities { return p.caps }

func (p *formatCheckingTTSProvider) BeginStream(ctx context.Context, sink ttsmodule.AudioSink) (ttsmodule.StreamSession, error) {
	format := sink.Format()
	if format != p.wantFormat {
		return nil, fmt.Errorf("sink format = %#v, want %#v", format, p.wantFormat)
	}
	return &formatCheckingTTSSession{sink: sink}, nil
}

func (p *formatCheckingTTSProvider) Close() error { return nil }

type formatCheckingTTSSession struct {
	sink ttsmodule.AudioSink
	err  error
}

func (s *formatCheckingTTSSession) WriteText(string) error { return nil }

func (s *formatCheckingTTSSession) Flush() error { return nil }

func (s *formatCheckingTTSSession) Close() error {
	if err := s.sink.WritePCM(make([]byte, testTTSPlaybackStartPCMBytes)); err != nil {
		s.err = err
		return err
	}
	return nil
}

func (s *formatCheckingTTSSession) Err() error { return s.err }

type recordingTTSProvider struct {
	name       string
	beginCount atomic.Int32
	closeCalls atomic.Int32
	mu         sync.Mutex
	seen       []string
}

type flushRecordingTTSProvider struct {
	name       string
	mu         sync.Mutex
	seen       []string
	writes     []string
	flushCalls int
}

const testTTSPlaybackStartPCMBytes = 48000

type playbackStartedTransientErrorProvider struct {
	name  string
	calls atomic.Int32
}

func (p *recordingTTSProvider) Name() string { return p.name }

func (p *playbackStartedTransientErrorProvider) Name() string { return p.name }

func (p *recordingTTSProvider) Capabilities() ttsmodule.Capabilities {
	return ttsmodule.Capabilities{SupportedSampleRates: []int{16000}}
}

func (p *playbackStartedTransientErrorProvider) Capabilities() ttsmodule.Capabilities {
	return ttsmodule.Capabilities{SupportedSampleRates: []int{16000}}
}

func (p *recordingTTSProvider) BeginStream(ctx context.Context, sink ttsmodule.AudioSink) (ttsmodule.StreamSession, error) {
	p.beginCount.Add(1)
	return &recordingTTSSession{provider: p, sink: sink}, nil
}

func (p *playbackStartedTransientErrorProvider) BeginStream(ctx context.Context, sink ttsmodule.AudioSink) (ttsmodule.StreamSession, error) {
	p.calls.Add(1)
	return &playbackStartedTransientErrorSession{sink: sink}, nil
}

func (p *recordingTTSProvider) Close() error {
	p.closeCalls.Add(1)
	return nil
}

func (p *recordingTTSProvider) providerCloseCalls() int {
	return int(p.closeCalls.Load())
}

func (p *recordingTTSProvider) beginCalls() int {
	return int(p.beginCount.Load())
}

func (p *flushRecordingTTSProvider) Name() string { return p.name }

func (p *flushRecordingTTSProvider) Capabilities() ttsmodule.Capabilities {
	return ttsmodule.Capabilities{}
}

func (p *flushRecordingTTSProvider) BeginStream(ctx context.Context, sink ttsmodule.AudioSink) (ttsmodule.StreamSession, error) {
	return &flushRecordingTTSSession{provider: p, sink: sink}, nil
}

func (p *flushRecordingTTSProvider) Close() error { return nil }

func (p *playbackStartedTransientErrorProvider) Close() error { return nil }

func (p *recordingTTSProvider) texts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func (p *flushRecordingTTSProvider) texts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.seen...)
}

func (p *flushRecordingTTSProvider) activity() ([]string, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.writes...), p.flushCalls
}

func (p *playbackStartedTransientErrorProvider) beginCalls() int {
	return int(p.calls.Load())
}

type recordingTTSSession struct {
	provider *recordingTTSProvider
	sink     ttsmodule.AudioSink
	buf      bytes.Buffer
	err      error
}

type flushRecordingTTSSession struct {
	provider *flushRecordingTTSProvider
	sink     ttsmodule.AudioSink
	buf      bytes.Buffer
	err      error
}

type playbackStartedTransientErrorSession struct {
	sink ttsmodule.AudioSink
	err  error
}

func (s *recordingTTSSession) WriteText(text string) error {
	_, _ = s.buf.WriteString(text)
	return nil
}

func (s *flushRecordingTTSSession) WriteText(text string) error {
	_, _ = s.buf.WriteString(text)
	s.provider.mu.Lock()
	s.provider.writes = append(s.provider.writes, text)
	s.provider.mu.Unlock()
	return nil
}

func (s *playbackStartedTransientErrorSession) WriteText(text string) error { return nil }

func (s *recordingTTSSession) Flush() error { return nil }

func (s *flushRecordingTTSSession) Flush() error {
	s.provider.mu.Lock()
	s.provider.flushCalls++
	s.provider.mu.Unlock()
	text := s.buf.String()
	s.buf.Reset()
	if text == "" {
		return nil
	}
	s.provider.mu.Lock()
	s.provider.seen = append(s.provider.seen, text)
	s.provider.mu.Unlock()
	if err := s.sink.WritePCM(make([]byte, testTTSPlaybackStartPCMBytes)); err != nil {
		s.err = err
		return err
	}
	return nil
}

func (s *playbackStartedTransientErrorSession) Flush() error { return nil }

func (s *recordingTTSSession) Close() error {
	text := s.buf.String()
	if text != "" {
		if err := s.sink.WritePCM(make([]byte, testTTSPlaybackStartPCMBytes)); err != nil {
			s.err = err
			return err
		}
		s.provider.mu.Lock()
		s.provider.seen = append(s.provider.seen, text)
		s.provider.mu.Unlock()
	}
	return nil
}

func (s *flushRecordingTTSSession) Close() error { return s.Flush() }

func (s *playbackStartedTransientErrorSession) Close() error {
	if err := s.sink.WritePCM(make([]byte, 16000)); err != nil {
		s.err = err
		return err
	}
	err := errors.New("dial tcp: i/o timeout")
	s.err = err
	return err
}

func (s *recordingTTSSession) Err() error { return s.err }

func (s *flushRecordingTTSSession) Err() error { return s.err }

func (s *playbackStartedTransientErrorSession) Err() error { return s.err }

func startTTSPlaybackAudioSocket(t *testing.T) string {
	t.Helper()
	return startFakeAudioServiceSocket(t, func(req audioRequest) (audioResponse, []byte) {
		switch req.Op {
		case "start_playback":
			return audioResponse{Status: "OK", SessionID: stringUint64(7)}, nil
		case "write_play_chunk":
			return audioResponse{Status: "OK"}, nil
		case "health":
			return audioResponse{Status: "OK", PlaybackSessions: 0}, nil
		case "stop_playback":
			return audioResponse{Status: "OK"}, nil
		default:
			return audioResponse{Status: "INTERNAL_ERROR"}, nil
		}
	})
}
