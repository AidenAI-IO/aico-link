package agent

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/logging"
)

const (
	sileroVADSampleRate       = 16000
	sileroVADFrameSamples     = 512
	defaultVADBackend         = "rknn"
	defaultVADSpeechThreshold = 0.5
	defaultVADModelPath       = "/usr/lib/aico/models/silero_vad_6_2_encoder_rv1106_w8a8_v1.rknn"
	defaultVADWeightsPath     = "/usr/lib/aico/models/silero_vad_6_2_lstm_decoder_weights.bin"
	defaultVADHelperPath      = "/usr/lib/aico/rknn_vad"
	defaultCPUVADHelperPath   = "/usr/lib/aico/cpu_vad"
	vadHelperReadinessTimeout = 30 * time.Second
	vadHelperProtocolTimeout  = 5 * time.Second
	vadHelperShutdownTimeout  = 2 * time.Second
)

type AudioVADConfig struct {
	SampleRate      int
	SilenceMs       int
	MinSpeechMs     int
	Backend         string
	ModelPath       string
	HelperPath      string
	SpeechThreshold float64
}

type VADScorer interface {
	Score(samples []int16) (float64, error)
	Reset() error
	Close() error
}

// AudioVAD segments utterances using Silero VAD probabilities produced by the helper.
type AudioVAD struct {
	scorer          VADScorer
	silenceCount    int
	speechFrames    int
	speaking        bool
	frameSamples    int
	silenceLimit    int
	minSpeechFrames int
	speechBuf       []int16
	utterance       []int16
	threshold       float64
	lastProbability float64
	lastErr         error
}

type VADDebugState struct {
	Probability   float64
	Threshold     float64
	Speaking      bool
	SpeechFrames  int
	SilenceFrames int
	LastError     string
}

func DefaultVADModelPath() string {
	return defaultVADModelPath
}

func DefaultVADBackend() string {
	return defaultVADBackend
}

func DefaultVADHelperPath() string {
	return defaultVADHelperPath
}

func DefaultVADHelperPathForBackend(backend string) string {
	switch strings.ToLower(strings.TrimSpace(backend)) {
	case "cpu":
		return defaultCPUVADHelperPath
	default:
		return defaultVADHelperPath
	}
}

func ResolveVADHelperPath(backend, helperPath string) string {
	backend = strings.ToLower(strings.TrimSpace(backend))
	helperPath = strings.TrimSpace(helperPath)
	if helperPath == "" {
		return DefaultVADHelperPathForBackend(backend)
	}
	if helperPath == defaultVADHelperPath || helperPath == defaultCPUVADHelperPath {
		return DefaultVADHelperPathForBackend(backend)
	}
	return helperPath
}

func DefaultVADSpeechThreshold() float64 {
	return defaultVADSpeechThreshold
}

func NewAudioVAD(cfg AudioVADConfig) (*AudioVAD, error) {
	backend, err := normalizeVADBackend(cfg.Backend)
	if err != nil {
		return nil, err
	}
	modelPath := strings.TrimSpace(cfg.ModelPath)
	if modelPath == "" {
		modelPath = defaultVADModelPath
	}
	helperPath := ResolveVADHelperPath(backend, cfg.HelperPath)
	return newAudioVAD(cfg, newHelperVADScorer(backend, modelPath, helperPath))
}

func NewAudioVADWithScorer(cfg AudioVADConfig, scorer VADScorer) (*AudioVAD, error) {
	if scorer == nil {
		return nil, errors.New("nil VAD scorer")
	}
	return newAudioVAD(cfg, scorer)
}

func newAudioVAD(cfg AudioVADConfig, scorer VADScorer) (*AudioVAD, error) {
	sampleRate := cfg.SampleRate
	if sampleRate == 0 {
		sampleRate = sileroVADSampleRate
	}
	if sampleRate != sileroVADSampleRate {
		return nil, fmt.Errorf("silero VAD requires %d Hz audio, got %d Hz", sileroVADSampleRate, sampleRate)
	}

	threshold := cfg.SpeechThreshold
	if threshold <= 0 {
		threshold = defaultVADSpeechThreshold
	}
	if threshold >= 1 {
		return nil, fmt.Errorf("vad_speech_threshold must be between 0 and 1, got %.3f", threshold)
	}

	silenceMs := cfg.SilenceMs
	if silenceMs <= 0 {
		silenceMs = defaultSilenceMs
	}
	minSpeechMs := cfg.MinSpeechMs
	if minSpeechMs <= 0 {
		minSpeechMs = defaultMinSpeechMs
	}

	frameMs := sileroVADFrameSamples * 1000 / sileroVADSampleRate
	silenceLimit := framesForDuration(silenceMs, frameMs)
	minSpeechFrames := framesForDuration(minSpeechMs, frameMs)

	return &AudioVAD{
		scorer:          scorer,
		frameSamples:    sileroVADFrameSamples,
		silenceLimit:    silenceLimit,
		minSpeechFrames: minSpeechFrames,
		speechBuf:       make([]int16, 0, sileroVADFrameSamples*100),
		utterance:       make([]int16, 0, sileroVADFrameSamples*100),
		threshold:       threshold,
	}, nil
}

func framesForDuration(durationMs, frameMs int) int {
	frames := durationMs / frameMs
	if durationMs%frameMs != 0 {
		frames++
	}
	if frames < 1 {
		return 1
	}
	return frames
}

func normalizeVADBackend(backend string) (string, error) {
	backend = strings.ToLower(strings.TrimSpace(backend))
	if backend == "" {
		return defaultVADBackend, nil
	}
	switch backend {
	case "rknn", "cpu":
		return backend, nil
	default:
		return "", fmt.Errorf("invalid vad_backend: %s (expected rknn or cpu)", backend)
	}
}

// FrameSamples returns the number of samples expected in each Silero VAD frame.
func (v *AudioVAD) FrameSamples() int {
	return v.frameSamples
}

func (v *AudioVAD) DebugState() VADDebugState {
	state := VADDebugState{
		Probability:   v.lastProbability,
		Threshold:     v.threshold,
		Speaking:      v.speaking,
		SpeechFrames:  v.speechFrames,
		SilenceFrames: v.silenceCount,
	}
	if v.lastErr != nil {
		state.LastError = v.lastErr.Error()
	}
	return state
}

// Process feeds one 512-sample 16 kHz frame to the VAD helper and returns an utterance when speech ends.
func (v *AudioVAD) Process(samples []int16) ([]int16, error) {
	if len(samples) != v.frameSamples {
		return nil, fmt.Errorf("invalid Silero VAD frame: got %d samples, want %d", len(samples), v.frameSamples)
	}

	probability, err := v.scorer.Score(samples)
	if err != nil {
		v.lastErr = err
		return nil, err
	}
	v.lastErr = nil
	v.lastProbability = probability
	isSpeech := probability > v.threshold

	if isSpeech {
		v.speechBuf = append(v.speechBuf, samples...)
		v.silenceCount = 0
		v.speechFrames++
		v.speaking = true
	} else if v.speaking {
		v.speechBuf = append(v.speechBuf, samples...)
		v.silenceCount++
		if v.silenceCount >= v.silenceLimit {
			if v.speechFrames >= v.minSpeechFrames {
				return v.finishUtterance(), nil
			}
			v.resetBuffers()
		}
	}

	return nil, nil
}

func (v *AudioVAD) finishUtterance() []int16 {
	v.utterance = make([]int16, len(v.speechBuf))
	copy(v.utterance, v.speechBuf)
	v.resetBuffers()
	return v.utterance
}

// Flush returns any buffered audio as an utterance.
func (v *AudioVAD) Flush() []int16 {
	if len(v.speechBuf) == 0 {
		_ = v.Reset()
		return nil
	}

	v.utterance = make([]int16, len(v.speechBuf))
	copy(v.utterance, v.speechBuf)
	_ = v.Reset()
	return v.utterance
}

// Reset clears the VAD segmentation buffers and RKNN recurrent state.
func (v *AudioVAD) Reset() error {
	v.resetBuffers()
	v.lastProbability = 0
	v.lastErr = nil
	if err := v.scorer.Reset(); err != nil {
		v.lastErr = err
		return err
	}
	return nil
}

func (v *AudioVAD) Close() error {
	return v.scorer.Close()
}

func (v *AudioVAD) resetBuffers() {
	v.speechBuf = v.speechBuf[:0]
	v.silenceCount = 0
	v.speechFrames = 0
	v.speaking = false
}

type helperVADScorer struct {
	backend     string
	modelPath   string
	weightsPath string
	helperPath  string

	mu      sync.Mutex
	cmd     *exec.Cmd
	stdin   io.WriteCloser
	stdout  *bufio.Reader
	waitErr chan error
	frame   []byte
}

type helperReadResult struct {
	line string
	err  error
}

func newHelperVADScorer(backend, modelPath, helperPath string) *helperVADScorer {
	return &helperVADScorer{
		backend:     backend,
		modelPath:   modelPath,
		weightsPath: defaultVADWeightsPath,
		helperPath:  helperPath,
	}
}

func (s *helperVADScorer) Score(samples []int16) (float64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if err := s.ensureStarted(); err != nil {
		return 0, err
	}

	frameLen := 1 + len(samples)*2
	if cap(s.frame) < frameLen {
		s.frame = make([]byte, frameLen)
	} else {
		s.frame = s.frame[:frameLen]
	}
	frame := s.frame
	frame[0] = 'F'
	for i, sample := range samples {
		binary.LittleEndian.PutUint16(frame[1+i*2:], uint16(sample))
	}
	if _, err := s.stdin.Write(frame); err != nil {
		s.stopAfterProtocolError()
		return 0, fmt.Errorf("write VAD helper frame: %w", err)
	}

	line, err := s.readLineWithTimeout("read VAD helper score")
	if err != nil {
		s.stopAfterProtocolError()
		return 0, fmt.Errorf("read VAD helper score: %w", err)
	}
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "ERR ") {
		return 0, errors.New(strings.TrimPrefix(line, "ERR "))
	}
	if !strings.HasPrefix(line, "P ") {
		s.stopAfterProtocolError()
		return 0, fmt.Errorf("unexpected VAD helper response %q", line)
	}
	probability, err := strconv.ParseFloat(strings.TrimSpace(strings.TrimPrefix(line, "P ")), 64)
	if err != nil {
		s.stopAfterProtocolError()
		return 0, fmt.Errorf("parse VAD helper probability %q: %w", line, err)
	}
	return probability, nil
}

func (s *helperVADScorer) Reset() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cmd == nil {
		return nil
	}
	if _, err := s.stdin.Write([]byte{'R'}); err != nil {
		s.stopAfterProtocolError()
		return fmt.Errorf("write VAD helper reset: %w", err)
	}
	line, err := s.readLineWithTimeout("read VAD helper reset response")
	if err != nil {
		s.stopAfterProtocolError()
		return fmt.Errorf("read VAD helper reset response: %w", err)
	}
	line = strings.TrimSpace(line)
	if line == "OK" {
		return nil
	}
	if strings.HasPrefix(line, "ERR ") {
		return errors.New(strings.TrimPrefix(line, "ERR "))
	}
	s.stopAfterProtocolError()
	return fmt.Errorf("unexpected VAD helper reset response %q", line)
}

func (s *helperVADScorer) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closeLocked()
}

func (s *helperVADScorer) ensureStarted() error {
	if s.cmd != nil {
		return nil
	}

	args := []string{}
	if s.backend != "cpu" {
		args = append(args, "--model", s.modelPath)
	}
	if s.weightsPath != "" {
		args = append(args, "--weights", s.weightsPath)
	}
	cmd := exec.Command(s.helperPath, args...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("open VAD helper stdin: %w", err)
	}
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open VAD helper stdout: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		return fmt.Errorf("open VAD helper stderr: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		return fmt.Errorf("start VAD helper %q: %w", s.helperPath, err)
	}

	go logVADHelperStderr(s.backend, stderrPipe)

	reader := bufio.NewReader(stdoutPipe)
	line, err := readHelperLineWithTimeout(cmd, reader, "wait for VAD helper readiness", vadHelperReadinessTimeout)
	if err != nil {
		_ = terminateCommand(cmd)
		return fmt.Errorf("wait for VAD helper readiness: %w", err)
	}
	line = strings.TrimSpace(line)
	if strings.HasPrefix(line, "ERR ") {
		_ = terminateCommand(cmd)
		return errors.New(strings.TrimPrefix(line, "ERR "))
	}
	if line != "READY" {
		_ = terminateCommand(cmd)
		return fmt.Errorf("unexpected VAD helper readiness response %q", line)
	}

	s.cmd = cmd
	s.stdin = stdin
	s.stdout = reader
	s.waitErr = make(chan error, 1)
	go func() {
		s.waitErr <- cmd.Wait()
	}()
	return nil
}

func (s *helperVADScorer) readLineWithTimeout(operation string) (string, error) {
	return readHelperLineWithTimeout(s.cmd, s.stdout, operation, vadHelperProtocolTimeout)
}

func readHelperLineWithTimeout(cmd *exec.Cmd, reader *bufio.Reader, operation string, timeout time.Duration) (string, error) {
	if reader == nil {
		return "", fmt.Errorf("%s: VAD helper stdout is closed", operation)
	}
	result := make(chan helperReadResult, 1)
	go func() {
		line, err := reader.ReadString('\n')
		result <- helperReadResult{line: line, err: err}
	}()

	timer := time.NewTimer(timeout)
	defer timer.Stop()

	select {
	case res := <-result:
		return res.line, res.err
	case <-timer.C:
		if cmd != nil && cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		return "", fmt.Errorf("%s timed out after %s", operation, timeout)
	}
}

func logVADHelperStderr(backend string, stderr io.Reader) {
	logVADHelperStderrTo(backend, stderr, os.Stderr)
}

func logVADHelperStderrTo(backend string, stderr io.Reader, output io.Writer) {
	scanner := bufio.NewScanner(stderr)
	for scanner.Scan() {
		line := scanner.Text()
		if logging.IsStructuredLine(line) {
			_, _ = io.WriteString(output, line+"\n")
			continue
		}
		record := logging.FormatEventAt(time.Now(), logging.Info, "agent", "vad_"+backend,
			"helper_stderr", logging.Field{Key: "message", Value: line})
		_, _ = io.WriteString(output, record+"\n")
	}
	if err := scanner.Err(); err != nil {
		record := logging.FormatEventAt(time.Now(), logging.Error, "agent", "vad_"+backend,
			"stderr_read_failed", logging.Field{Key: "error", Value: err})
		_, _ = io.WriteString(output, record+"\n")
	}
}

func (s *helperVADScorer) stopAfterProtocolError() {
	if s.cmd != nil && s.cmd.Process != nil {
		_ = s.cmd.Process.Kill()
	}
	_ = s.closeLocked()
}

func (s *helperVADScorer) closeLocked() error {
	if s.cmd == nil {
		return nil
	}
	_, _ = s.stdin.Write([]byte{'Q'})
	_ = s.stdin.Close()
	err := s.waitForExitLocked()
	s.cmd = nil
	s.stdin = nil
	s.stdout = nil
	s.waitErr = nil
	if err != nil && !strings.Contains(err.Error(), "signal: killed") {
		return err
	}
	return nil
}

func (s *helperVADScorer) waitForExitLocked() error {
	if s.waitErr == nil {
		return nil
	}
	timer := time.NewTimer(vadHelperShutdownTimeout)
	defer timer.Stop()

	select {
	case err := <-s.waitErr:
		return err
	case <-timer.C:
		if s.cmd != nil && s.cmd.Process != nil {
			_ = s.cmd.Process.Kill()
		}
	}

	timer.Reset(vadHelperShutdownTimeout)
	select {
	case err := <-s.waitErr:
		return err
	case <-timer.C:
		return fmt.Errorf("wait for VAD helper exit timed out after %s", vadHelperShutdownTimeout)
	}
}

func terminateCommand(cmd *exec.Cmd) error {
	if cmd == nil {
		return nil
	}
	if cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
	done := make(chan error, 1)
	go func() {
		done <- cmd.Wait()
	}()
	timer := time.NewTimer(vadHelperShutdownTimeout)
	defer timer.Stop()
	select {
	case err := <-done:
		return err
	case <-timer.C:
		return fmt.Errorf("wait for VAD helper exit timed out after %s", vadHelperShutdownTimeout)
	}
}
