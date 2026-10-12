package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aiden-agent/internal/agent/tts"
)

const (
	defaultTTSUnavailableFallbackDir = "/usr/share/aiden/audio/voice-notifications"
	ttsUnavailableFallbackDirEnv     = "AIDEN_TTS_FALLBACK_DIR"
	ttsUnavailableFallbackEnglish    = "tts-unavailable.en-US.wav"
	ttsUnavailableFallbackChinese    = "tts-unavailable.zh-CN.wav"
)

var (
	errTTSNotConfigured = errors.New("tts is not configured")
	errTTSNoAudio       = errors.New("tts completed without writing audio")

	errStandaloneTTSUnavailable = errors.New("standalone TTS is unavailable")
)

// denormalizeLocaleForFilename converts a normalized locale (lowercase with
// hyphens, e.g. "zh-cn", "en-us") to standard BCP 47 format (e.g. "zh-CN",
// "en-US") for matching prerecorded audio filenames.
func denormalizeLocaleForFilename(locale string) string {
	parts := strings.Split(locale, "-")
	if len(parts) == 2 {
		return parts[0] + "-" + strings.ToUpper(parts[1])
	}
	return locale
}

// ttsUnavailableFallbackPath returns the path to the prerecorded fallback WAV
// clip for the given failure code and locale. If a failure-specific clip exists
// (e.g. "session-idle.zh-CN.wav"), it is preferred; otherwise the generic
// "tts-unavailable.<locale>.wav" is returned.
//
// failureCode is optional; pass empty string to skip failure-specific lookup.
func ttsUnavailableFallbackPath(cfg Config, failureCode TurnFailureCode) string {
	if !cfg.VoiceNotifications.EnabledOrDefault() {
		return ""
	}
	dir := strings.TrimSpace(os.Getenv(ttsUnavailableFallbackDirEnv))
	if dir == "" {
		dir = defaultTTSUnavailableFallbackDir
	}

	locale := resolvedVoiceNotificationLocale(cfg)

	// Try failure-specific clip first if a code is provided.
	// The locale is normalized (lowercase), but filenames use standard BCP 47
	// format (e.g. zh-CN, en-US), so we denormalize it for filesystem lookup.
	if failureCode != "" {
		standardLocale := denormalizeLocaleForFilename(locale)
		filename := fmt.Sprintf("%s.%s.wav", failureCode, standardLocale)
		path := filepath.Join(dir, filename)
		if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 44 {
			return path
		}
	}

	// Fall back to the generic "tts-unavailable.<locale>.wav".
	filename := ttsUnavailableFallbackChinese
	if strings.HasPrefix(locale, "en") {
		filename = ttsUnavailableFallbackEnglish
	}
	return filepath.Join(dir, filename)
}

func canPlayTTSUnavailableFallback(cfg Config, failureCode TurnFailureCode) bool {
	path := ttsUnavailableFallbackPath(cfg, failureCode)
	if path == "" {
		return false
	}
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Size() > 44
}

func playTTSUnavailableFallback(ctx context.Context, audio tts.AudioServiceBackend, cfg Config, failureCode TurnFailureCode) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if audio == nil {
		return errors.New("audio backend is not configured")
	}
	path := ttsUnavailableFallbackPath(cfg, failureCode)
	if path == "" {
		return errors.New("local TTS fallback is disabled")
	}
	wavData, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read local TTS fallback %s: %w", path, err)
	}
	pcm, sampleRate, err := extractPCMFromWAV(wavData)
	if err != nil {
		return fmt.Errorf("decode local TTS fallback %s: %w", path, err)
	}
	if len(pcm) == 0 {
		return fmt.Errorf("local TTS fallback %s contains no PCM audio", path)
	}

	sink := tts.NewAudioServiceSink(audio, tts.AudioFormat{
		SampleRate: sampleRate,
		Channels:   1,
		BitWidth:   16,
	})
	if err := sink.WritePCM(pcm); err != nil {
		_ = sink.Stop()
		return fmt.Errorf("start local TTS fallback playback: %w", err)
	}
	if err := sink.Drain(ctx); err != nil {
		_ = sink.Stop()
		return fmt.Errorf("finish local TTS fallback playback: %w", err)
	}
	return nil
}

// attemptTTSUnavailableFallback preserves the original TTS error even when
// the local recording plays successfully. Callers use the non-nil error to
// avoid acknowledging response-tail delivery for speech that was not spoken.
//
// failureCode is optional; pass empty string to use only the generic fallback clip.
func attemptTTSUnavailableFallback(ctx context.Context, audio tts.AudioServiceBackend, cfg Config, failureCode TurnFailureCode, speechStarted bool, ttsErr error) (bool, error) {
	if ttsErr == nil || speechStarted || !canPlayTTSUnavailableFallback(cfg, failureCode) {
		return false, ttsErr
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return false, ttsErr
	}
	if errors.Is(ttsErr, context.Canceled) {
		return false, ttsErr
	}
	if err := playTTSUnavailableFallback(ctx, audio, cfg, failureCode); err != nil {
		return false, errors.Join(ttsErr, err)
	}
	return true, ttsErr
}
