package agent

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestLogVADHelperStderrPreservesStructuredLines(t *testing.T) {
	structured := "2026-08-05T06:22:03Z [ERROR][rknn_vad][inference] run_failed error=timeout"
	var output bytes.Buffer
	logVADHelperStderrTo("rknn", strings.NewReader(structured+"\nraw diagnostic\n"), &output)

	lines := strings.Split(strings.TrimSpace(output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("output lines = %d, want 2: %q", len(lines), output.String())
	}
	if lines[0] != structured {
		t.Fatalf("structured line = %q, want unchanged %q", lines[0], structured)
	}
	wrappedPattern := regexp.MustCompile(
		`^[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2}Z \[INFO\]\[agent\]\[vad_rknn\] helper_stderr message="raw diagnostic"$`,
	)
	if !wrappedPattern.MatchString(lines[1]) {
		t.Fatalf("wrapped line = %q, want complete structured helper_stderr record", lines[1])
	}
}

func TestAudioVADEndsUtteranceFromRKNNProbabilities(t *testing.T) {
	vad := newTestAudioVAD(t, []float64{0.9, 0.8, 0.1, 0.1, 0.1})

	speech := constantFrame(vad.FrameSamples(), 1000)
	silence := constantFrame(vad.FrameSamples(), 0)

	if utterance, err := vad.Process(speech); err != nil || utterance != nil {
		t.Fatalf("first speech Process() = len %d, err %v; want no utterance", len(utterance), err)
	}
	if utterance, err := vad.Process(speech); err != nil || utterance != nil {
		t.Fatalf("second speech Process() = len %d, err %v; want no utterance", len(utterance), err)
	}
	for i := 0; i < 2; i++ {
		if utterance, err := vad.Process(silence); err != nil || utterance != nil {
			t.Fatalf("silence Process(%d) = len %d, err %v; want no utterance", i, len(utterance), err)
		}
	}
	utterance, err := vad.Process(silence)
	if err != nil {
		t.Fatalf("Process() error = %v", err)
	}
	if len(utterance) != vad.FrameSamples()*5 {
		t.Fatalf("utterance samples = %d, want %d", len(utterance), vad.FrameSamples()*5)
	}
}

func TestAudioVADDoesNotUsePCMEnergyAsSpeech(t *testing.T) {
	vad := newTestAudioVAD(t, []float64{0.1, 0.1, 0.1, 0.1})
	loudFrame := alternatingFrame(vad.FrameSamples(), 0, 30000)

	for i := 0; i < 4; i++ {
		if utterance, err := vad.Process(loudFrame); err != nil || utterance != nil {
			t.Fatalf("Process(%d) = len %d, err %v; want no utterance from PCM energy", i, len(utterance), err)
		}
	}
	if state := vad.DebugState(); state.Speaking || state.SpeechFrames != 0 {
		t.Fatalf("VAD treated loud PCM as speech without RKNN probability: %#v", state)
	}
}

func TestAudioVADRequiresSileroFrameShape(t *testing.T) {
	_, err := NewAudioVADWithScorer(AudioVADConfig{
		SampleRate:  32000,
		SilenceMs:   90,
		MinSpeechMs: 60,
	}, &sequenceScorer{})
	if err == nil {
		t.Fatal("NewAudioVADWithScorer() error = nil, want sample-rate validation error")
	}

	vad := newTestAudioVAD(t, []float64{0.9})
	if _, err := vad.Process(make([]int16, vad.FrameSamples()-1)); err == nil {
		t.Fatal("Process() error = nil, want frame-size validation error")
	}
}

func TestNewAudioVADSelectsHelperFromBackend(t *testing.T) {
	vad, err := NewAudioVAD(AudioVADConfig{Backend: "cpu", HelperPath: defaultVADHelperPath})
	if err != nil {
		t.Fatalf("NewAudioVAD(cpu) error = %v", err)
	}
	scorer, ok := vad.scorer.(*helperVADScorer)
	if !ok {
		t.Fatalf("scorer type = %T, want *helperVADScorer", vad.scorer)
	}
	if scorer.backend != "cpu" {
		t.Fatalf("backend = %q, want cpu", scorer.backend)
	}
	if scorer.helperPath != defaultCPUVADHelperPath {
		t.Fatalf("helperPath = %q, want %q", scorer.helperPath, defaultCPUVADHelperPath)
	}

	defaultVAD, err := NewAudioVAD(AudioVADConfig{})
	if err != nil {
		t.Fatalf("NewAudioVAD(default) error = %v", err)
	}
	defaultScorer, ok := defaultVAD.scorer.(*helperVADScorer)
	if !ok {
		t.Fatalf("default scorer type = %T, want *helperVADScorer", defaultVAD.scorer)
	}
	if defaultScorer.backend != "rknn" {
		t.Fatalf("default backend = %q, want rknn", defaultScorer.backend)
	}
	if defaultScorer.helperPath != defaultVADHelperPath {
		t.Fatalf("default helperPath = %q, want %q", defaultScorer.helperPath, defaultVADHelperPath)
	}
}

func TestAudioVADPropagatesRKNNScorerErrors(t *testing.T) {
	wantErr := errors.New("rknn failed")
	vad, err := NewAudioVADWithScorer(AudioVADConfig{
		SampleRate:  16000,
		SilenceMs:   90,
		MinSpeechMs: 60,
	}, &sequenceScorer{err: wantErr})
	if err != nil {
		t.Fatalf("NewAudioVADWithScorer() error = %v", err)
	}

	if _, err := vad.Process(constantFrame(vad.FrameSamples(), 0)); !errors.Is(err, wantErr) {
		t.Fatalf("Process() error = %v, want %v", err, wantErr)
	}
	if state := vad.DebugState(); state.LastError != wantErr.Error() {
		t.Fatalf("LastError = %q, want %q", state.LastError, wantErr.Error())
	}
}

func TestHelperVADScorerStopsAfterMalformedScoreResponse(t *testing.T) {
	helperPath := filepath.Join(t.TempDir(), "vad-helper")
	script := "#!/bin/sh\n" +
		"printf 'READY\\n'\n" +
		"dd bs=1 count=1 >/dev/null 2>/dev/null\n" +
		"printf 'BOGUS\\n'\n" +
		"sleep 30\n"
	if err := os.WriteFile(helperPath, []byte(script), 0755); err != nil {
		t.Fatalf("write helper script: %v", err)
	}

	scorer := newHelperVADScorer("cpu", "", helperPath)
	if _, err := scorer.Score(make([]int16, sileroVADFrameSamples)); err == nil || !strings.Contains(err.Error(), "unexpected VAD helper response") {
		t.Fatalf("Score() error = %v, want unexpected response", err)
	}

	scorer.mu.Lock()
	helperStillRunning := scorer.cmd != nil
	scorer.mu.Unlock()
	if helperStillRunning {
		t.Fatal("helper process was kept after malformed response")
	}
}

func TestAudioVADSmartTurnCommitsOnCompleteProbability(t *testing.T) {
	detector := &stubTurnDetector{prob: 0.9}
	vad, err := NewAudioVADWithScorer(AudioVADConfig{
		SampleRate:        16000,
		MinSpeechMs:       60,
		SpeechThreshold:   0.5,
		TurnDetector:      detector,
		TurnThreshold:     0.5,
		AnalyzeSilenceMs:  90,  // 3 frames
		FallbackSilenceMs: 300, // 10 frames
	}, &sequenceScorer{probabilities: []float64{0.9, 0.9, 0.1, 0.1, 0.1}})
	if err != nil {
		t.Fatalf("NewAudioVADWithScorer() error = %v", err)
	}

	speech := constantFrame(vad.FrameSamples(), 1000)
	silence := constantFrame(vad.FrameSamples(), 0)
	if utterance, err := vad.Process(speech); err != nil || utterance != nil {
		t.Fatalf("speech1 = len %d err %v", len(utterance), err)
	}
	if utterance, err := vad.Process(speech); err != nil || utterance != nil {
		t.Fatalf("speech2 = len %d err %v", len(utterance), err)
	}
	for i := 0; i < 2; i++ {
		if utterance, err := vad.Process(silence); err != nil || utterance != nil {
			t.Fatalf("silence %d = len %d err %v", i, len(utterance), err)
		}
	}
	utterance, err := vad.Process(silence)
	if err != nil {
		t.Fatalf("analyze silence error = %v", err)
	}
	if utterance == nil {
		t.Fatal("expected Smart Turn to commit utterance")
	}
	if detector.calls != 1 {
		t.Fatalf("Predict calls = %d, want 1", detector.calls)
	}
}

func TestAudioVADSmartTurnPredictErrorFailsOpenUntilFallback(t *testing.T) {
	detector := &stubTurnDetector{err: errors.New("npu submit failed")}
	vad, err := NewAudioVADWithScorer(AudioVADConfig{
		SampleRate:        16000,
		MinSpeechMs:       60,
		SpeechThreshold:   0.5,
		TurnDetector:      detector,
		TurnThreshold:     0.5,
		AnalyzeSilenceMs:  90,
		FallbackSilenceMs: 2000, // long enough that repeated Predict failures commit first
	}, &sequenceScorer{probabilities: []float64{0.9, 0.9, 0.1}})
	if err != nil {
		t.Fatalf("NewAudioVADWithScorer() error = %v", err)
	}

	speech := constantFrame(vad.FrameSamples(), 1000)
	silence := constantFrame(vad.FrameSamples(), 0)
	for i := 0; i < 2; i++ {
		if _, err := vad.Process(speech); err != nil {
			t.Fatalf("speech Process error = %v", err)
		}
	}
	var utterance []int16
	for i := 0; i < 20 && utterance == nil; i++ {
		var processErr error
		utterance, processErr = vad.Process(silence)
		if processErr != nil {
			t.Fatalf("Process() error = %v, want fail-open commit without killing Process", processErr)
		}
	}
	if utterance == nil {
		t.Fatal("expected repeated Smart Turn errors to commit on silence")
	}
	if detector.calls < smartTurnFailCommitAfter {
		t.Fatalf("Predict calls = %d, want >= %d", detector.calls, smartTurnFailCommitAfter)
	}
}

func TestAudioVADSmartTurnWaitsWhenIncomplete(t *testing.T) {
	detector := &stubTurnDetector{prob: 0.1}
	// Two speech frames, then many silence frames (scorer repeats last value).
	probs := []float64{0.9, 0.9, 0.1}
	vad, err := NewAudioVADWithScorer(AudioVADConfig{
		SampleRate:        16000,
		MinSpeechMs:       60,
		SpeechThreshold:   0.5,
		TurnDetector:      detector,
		TurnThreshold:     0.5,
		AnalyzeSilenceMs:  90,
		FallbackSilenceMs: 200, // ~7 frames; force commit after longer silence
	}, &sequenceScorer{probabilities: probs})
	if err != nil {
		t.Fatalf("NewAudioVADWithScorer() error = %v", err)
	}

	speech := constantFrame(vad.FrameSamples(), 1000)
	silence := constantFrame(vad.FrameSamples(), 0)
	for i := 0; i < 2; i++ {
		if _, err := vad.Process(speech); err != nil {
			t.Fatalf("speech Process error = %v", err)
		}
	}
	// First analyze window: incomplete, so no commit.
	for i := 0; i < 3; i++ {
		if utterance, err := vad.Process(silence); err != nil || utterance != nil {
			t.Fatalf("analyze window silence %d = len %d err %v", i, len(utterance), err)
		}
	}
	if detector.calls != 1 {
		t.Fatalf("Predict calls after analyze = %d, want 1", detector.calls)
	}
	// Continue silence until fallback forces commit.
	var utterance []int16
	for i := 0; i < 20 && utterance == nil; i++ {
		var err error
		utterance, err = vad.Process(silence)
		if err != nil {
			t.Fatalf("fallback silence %d error = %v", i, err)
		}
	}
	if utterance == nil {
		t.Fatal("expected fallback silence to commit utterance")
	}
}

func newTestAudioVAD(t *testing.T, probabilities []float64) *AudioVAD {
	t.Helper()
	vad, err := NewAudioVADWithScorer(AudioVADConfig{
		SampleRate:      16000,
		SilenceMs:       90,
		MinSpeechMs:     60,
		SpeechThreshold: 0.5,
	}, &sequenceScorer{probabilities: probabilities})
	if err != nil {
		t.Fatalf("NewAudioVADWithScorer() error = %v", err)
	}
	return vad
}

type stubTurnDetector struct {
	prob  float64
	calls int
	err   error
}

func (s *stubTurnDetector) Predict(pcm []int16) (float64, error) {
	s.calls++
	if s.err != nil {
		return 0, s.err
	}
	return s.prob, nil
}

func (s *stubTurnDetector) Close() error { return nil }

type sequenceScorer struct {
	probabilities []float64
	err           error
	resetErr      error
	resetCalls    int
	index         int
}

func (s *sequenceScorer) Score(samples []int16) (float64, error) {
	if s.err != nil {
		return 0, s.err
	}
	if len(s.probabilities) == 0 {
		return 0, nil
	}
	if s.index >= len(s.probabilities) {
		return s.probabilities[len(s.probabilities)-1], nil
	}
	probability := s.probabilities[s.index]
	s.index++
	return probability, nil
}

func (s *sequenceScorer) Reset() error {
	s.resetCalls++
	if s.resetErr != nil {
		return s.resetErr
	}
	s.index = 0
	return nil
}

func (s *sequenceScorer) Close() error {
	return nil
}

func constantFrame(samples int, value int16) []int16 {
	frame := make([]int16, samples)
	for i := range frame {
		frame[i] = value
	}
	return frame
}

func alternatingFrame(samples int, center, amplitude int16) []int16 {
	frame := make([]int16, samples)
	for i := range frame {
		if i%2 == 0 {
			frame[i] = center + amplitude
		} else {
			frame[i] = center - amplitude
		}
	}
	return frame
}
