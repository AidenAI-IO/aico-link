package agent

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	ttsmodule "aiden-agent/internal/agent/tts"
)

func TestDenormalizeLocaleForFilename(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{"zh-cn", "zh-CN"},
		{"en-us", "en-US"},
		{"ja-jp", "ja-JP"},
		{"ko-kr", "ko-KR"},
		{"zh", "zh"},       // single part, returned as-is
		{"", ""},           // empty
		{"en-US", "en-US"}, // already uppercase in region part
	}

	for _, tt := range tests {
		got := denormalizeLocaleForFilename(tt.input)
		if got != tt.expected {
			t.Errorf("denormalizeLocaleForFilename(%q) = %q, want %q", tt.input, got, tt.expected)
		}
	}
}

func TestTTSUnavailableFallbackPathSelectsLocale(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ttsUnavailableFallbackDirEnv, dir)

	zhPath := ttsUnavailableFallbackPath(Config{
		Locale: "zh-CN",
	}, "")
	if want := filepath.Join(dir, ttsUnavailableFallbackChinese); zhPath != want {
		t.Fatalf("zh fallback path = %q, want %q", zhPath, want)
	}
	enPath := ttsUnavailableFallbackPath(Config{Locale: "en-US"}, "")
	if want := filepath.Join(dir, ttsUnavailableFallbackEnglish); enPath != want {
		t.Fatalf("en fallback path = %q, want %q", enPath, want)
	}

	disabled := false
	if got := ttsUnavailableFallbackPath(Config{VoiceNotifications: VoiceNotificationsConfig{Enabled: &disabled}}, ""); got != "" {
		t.Fatalf("disabled fallback path = %q, want empty", got)
	}
}

func TestTTSUnavailableFallbackPathPrefersFailureSpecificClip(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ttsUnavailableFallbackDirEnv, dir)

	// Write generic fallback
	writeTestTTSFallback(t, dir, ttsUnavailableFallbackChinese)

	// Write failure-specific clip
	writeTestTTSFallback(t, dir, "session_idle.zh-CN.wav")

	cfg := Config{Locale: "zh-CN"}

	// Without failure code, should return generic
	genericPath := ttsUnavailableFallbackPath(cfg, "")
	if want := filepath.Join(dir, ttsUnavailableFallbackChinese); genericPath != want {
		t.Fatalf("generic fallback path = %q, want %q", genericPath, want)
	}

	// With failure code, should return specific
	specificPath := ttsUnavailableFallbackPath(cfg, TurnFailureSessionIdle)
	if want := filepath.Join(dir, "session_idle.zh-CN.wav"); specificPath != want {
		t.Fatalf("session_idle fallback path = %q, want %q", specificPath, want)
	}

	// With unknown failure code, should fall back to generic
	unknownPath := ttsUnavailableFallbackPath(cfg, "unknown-code")
	if want := filepath.Join(dir, ttsUnavailableFallbackChinese); unknownPath != want {
		t.Fatalf("unknown code fallback path = %q, want %q", unknownPath, want)
	}
}

func TestPlayTTSUnavailableFallbackStreamsBundledWAV(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ttsUnavailableFallbackDirEnv, dir)
	writeTestTTSFallback(t, dir, ttsUnavailableFallbackChinese)

	ops := &recordedAudioOps{}
	audio := NewAudioServiceClient(startRecordedTTSPlaybackAudioSocket(t, ops))
	if err := playTTSUnavailableFallback(context.Background(), newAudioBackend(audio), Config{Locale: "zh-CN"}, ""); err != nil {
		t.Fatalf("playTTSUnavailableFallback() error = %v", err)
	}
	if got := ops.countOp("start_playback"); got != 1 {
		t.Fatalf("start_playback count = %d, want 1", got)
	}
	if got := ops.finalChunkCount(); got != 1 {
		t.Fatalf("final chunk count = %d, want 1", got)
	}
}

func TestAttemptTTSUnavailableFallbackPreservesOriginalError(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ttsUnavailableFallbackDirEnv, dir)
	writeTestTTSFallback(t, dir, ttsUnavailableFallbackChinese)

	ops := &recordedAudioOps{}
	audio := NewAudioServiceClient(startRecordedTTSPlaybackAudioSocket(t, ops))
	original := errors.New("dial tcp: connection refused")
	played, err := attemptTTSUnavailableFallback(context.Background(), newAudioBackend(audio), Config{Locale: "zh-CN"}, "", false, original)
	if !played {
		t.Fatal("fallback played = false, want true")
	}
	if !errors.Is(err, original) {
		t.Fatalf("fallback error = %v, want original TTS error", err)
	}
	if got := ops.countOp("start_playback"); got != 1 {
		t.Fatalf("start_playback count = %d, want 1", got)
	}
}

func TestAttemptTTSUnavailableFallbackSkipsPartialSpeechAndCancellation(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ttsUnavailableFallbackDirEnv, dir)
	writeTestTTSFallback(t, dir, ttsUnavailableFallbackChinese)

	ops := &recordedAudioOps{}
	audio := NewAudioServiceClient(startRecordedTTSPlaybackAudioSocket(t, ops))
	original := errors.New("dial tcp: connection reset")
	if played, err := attemptTTSUnavailableFallback(context.Background(), newAudioBackend(audio), Config{}, "", true, original); played || !errors.Is(err, original) {
		t.Fatalf("partial speech fallback = (%v, %v), want (false, original error)", played, err)
	}
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if played, err := attemptTTSUnavailableFallback(canceledCtx, newAudioBackend(audio), Config{}, "", false, original); played || !errors.Is(err, original) {
		t.Fatalf("canceled fallback = (%v, %v), want (false, original error)", played, err)
	}
	if got := ops.countOp("start_playback"); got != 0 {
		t.Fatalf("start_playback count = %d, want 0", got)
	}
}

func TestServerFinalSpeechUsesFallbackWithoutTTSManager(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ttsUnavailableFallbackDirEnv, dir)
	writeTestTTSFallback(t, dir, ttsUnavailableFallbackEnglish)

	cfg := Config{
		Locale: "en-US",
	}
	ops := &recordedAudioOps{}
	server := &Server{
		runtime:     &Runtime{config: cfg},
		audioClient: NewAudioServiceClient(startRecordedTTSPlaybackAudioSocket(t, ops)),
	}
	if !server.canSpeakFinalText() {
		t.Fatal("canSpeakFinalText() = false with local fallback available")
	}
	played, err := server.speakFinalTextForRequest(context.Background(), "", "hello", 0)
	if !played {
		t.Fatal("fallback played = false, want true")
	}
	if !errors.Is(err, errTTSNotConfigured) {
		t.Fatalf("speakFinalTextForRequest() error = %v, want errTTSNotConfigured", err)
	}
	if got := ops.countOp("start_playback"); got != 1 {
		t.Fatalf("start_playback count = %d, want 1", got)
	}
}

func TestServerNonFinalSpeechWithoutTTSManagerRemainsNoop(t *testing.T) {
	server := &Server{}
	if err := server.speakText(context.Background(), "tool progress", 0); err != nil {
		t.Fatalf("speakText() error = %v, want nil without TTS manager", err)
	}
}

func TestServerFinalSpeechDoesNotFallbackAfterPlaybackStarts(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ttsUnavailableFallbackDirEnv, dir)
	writeTestTTSFallback(t, dir, ttsUnavailableFallbackChinese)

	ops := &recordedAudioOps{}
	server := &Server{
		runtime:     &Runtime{config: Config{Audio: AudioConfig{SampleRate: 16000}}},
		ttsManager:  ttsmodule.NewProviderManager(&playbackStartedTransientErrorProvider{name: "partial"}, nil),
		audioClient: NewAudioServiceClient(startRecordedTTSPlaybackAudioSocket(t, ops)),
	}
	played, err := server.speakFinalTextForRequest(context.Background(), "", "hello", 0)
	if played {
		t.Fatal("fallback played = true after TTS playback started")
	}
	if err == nil {
		t.Fatal("speakFinalTextForRequest() error = nil, want TTS error")
	}
	if got := ops.countOp("start_playback"); got != 1 {
		t.Fatalf("start_playback count = %d, want only partial TTS playback", got)
	}
}

func TestServerFinalSpeechStopsFailedPlaybackBeforeFallback(t *testing.T) {
	dir := t.TempDir()
	t.Setenv(ttsUnavailableFallbackDirEnv, dir)
	writeTestTTSFallback(t, dir, ttsUnavailableFallbackChinese)

	ops := &recordedAudioOps{}
	server := &Server{
		runtime:     &Runtime{config: Config{Locale: "zh-CN", Audio: AudioConfig{SampleRate: 16000}}},
		ttsManager:  ttsmodule.NewProviderManager(&recordingTTSProvider{name: "first-chunk-failure"}, nil),
		audioClient: NewAudioServiceClient(startFirstChunkFailureThenFallbackAudioSocket(t, ops)),
	}
	played, err := server.speakFinalTextForRequest(context.Background(), "", "hello", 0)
	if !played {
		t.Fatalf("fallback played = false, want true after failed TTS session is stopped; error = %v", err)
	}
	if err == nil {
		t.Fatal("speakFinalTextForRequest() error = nil, want original TTS error")
	}
	if got := ops.countOp("start_playback"); got != 2 {
		t.Fatalf("start_playback count = %d, want failed TTS plus fallback", got)
	}
	if got := ops.countOp("stop_playback"); got != 1 {
		t.Fatalf("stop_playback count = %d, want failed TTS cleanup", got)
	}
	if got := ops.finalChunkCount(); got != 1 {
		t.Fatalf("final chunk count = %d, want fallback completion", got)
	}
}

func TestBundledTTSUnavailableFallbackAssetsArePCM16Mono16k(t *testing.T) {
	assetDir := filepath.Join("..", "..", "..", "..", "assets", "business", "audio", "voice-notifications")
	for _, filename := range []string{ttsUnavailableFallbackChinese, ttsUnavailableFallbackEnglish} {
		wavData, err := os.ReadFile(filepath.Join(assetDir, filename))
		if err != nil {
			t.Fatalf("read bundled fallback %s: %v", filename, err)
		}
		pcm, sampleRate, err := extractPCMFromWAV(wavData)
		if err != nil {
			t.Fatalf("decode bundled fallback %s: %v", filename, err)
		}
		if sampleRate != 16000 {
			t.Fatalf("bundled fallback %s sample rate = %d, want 16000", filename, sampleRate)
		}
		if len(pcm) == 0 {
			t.Fatalf("bundled fallback %s contains no PCM audio", filename)
		}
	}
}

func writeTestTTSFallback(t *testing.T, dir, filename string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir fallback dir: %v", err)
	}
	wavData := pcm16MonoToWAV(make([]int16, 16000), 16000)
	if err := os.WriteFile(filepath.Join(dir, filename), wavData, 0o644); err != nil {
		t.Fatalf("write fallback WAV: %v", err)
	}
}

func startFirstChunkFailureThenFallbackAudioSocket(t *testing.T, ops *recordedAudioOps) string {
	t.Helper()
	var mu sync.Mutex
	active := false
	startCount := 0
	return startFakeAudioServiceSocket(t, func(req audioRequest) (audioResponse, []byte) {
		ops.append(req)
		mu.Lock()
		defer mu.Unlock()
		switch req.Op {
		case "start_playback":
			if active {
				return audioResponse{Status: "PLAYBACK_BUSY"}, nil
			}
			startCount++
			active = true
			return audioResponse{Status: "OK", SessionID: stringUint64(startCount)}, nil
		case "write_play_chunk":
			if startCount == 1 && !req.IsFinal {
				return audioResponse{Status: "INTERNAL_ERROR"}, nil
			}
			if req.IsFinal {
				active = false
			}
			return audioResponse{Status: "OK"}, nil
		case "stop_playback":
			active = false
			return audioResponse{Status: "OK"}, nil
		case "health":
			playbackSessions := uint32(0)
			if active {
				playbackSessions = 1
			}
			return audioResponse{Status: "OK", PlaybackSessions: playbackSessions}, nil
		default:
			return audioResponse{Status: "INTERNAL_ERROR"}, nil
		}
	})
}
