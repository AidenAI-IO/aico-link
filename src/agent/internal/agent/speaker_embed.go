package agent

import (
	"encoding/binary"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	speakerSampleRate      = 16000
	speakerWindowSamples   = 32240 // 2.015 s — native Fbank contract
	speakerEnrollSamples   = speakerSampleRate * 4
	speakerEmbeddingDim    = 512
	speakerEmbedTimeout    = 15 * time.Second
	defaultSpeakerToolsDir = "/usr/lib/aico"
	defaultSpeakerModelDir = "/usr/lib/aico/models"
	defaultSpeakerStoreDir = "/userdata/aico/speaker_profiles"
	// Classic is single-owner only: one enrolled profile, re-enrollment replaces it.
	speakerOwnerProfileID     = "speaker"
	defaultSpeakerMaxProfiles = 1
	defaultSpeakerThreshold   = 0.50
	speakerMinRMS             = 0.005
)

// SpeakerEmbedder turns PCM16 mono 16 kHz into an L2-normalized 512-d vector.
type SpeakerEmbedder interface {
	Embed(pcm []int16) ([]float32, error)
}

type nativeSpeakerEmbedder struct {
	toolsDir string
	modelDir string
	mu       sync.Mutex
}

func DefaultSpeakerToolsDir() string   { return defaultSpeakerToolsDir }
func DefaultSpeakerModelDir() string   { return defaultSpeakerModelDir }
func DefaultSpeakerStoreDir() string   { return defaultSpeakerStoreDir }
func DefaultSpeakerMaxProfiles() int   { return defaultSpeakerMaxProfiles }
func DefaultSpeakerThreshold() float64 { return defaultSpeakerThreshold }

func NewNativeSpeakerEmbedder(toolsDir, modelDir string) *nativeSpeakerEmbedder {
	toolsDir = strings.TrimSpace(toolsDir)
	if toolsDir == "" {
		toolsDir = defaultSpeakerToolsDir
	}
	modelDir = strings.TrimSpace(modelDir)
	if modelDir == "" {
		modelDir = defaultSpeakerModelDir
	}
	return &nativeSpeakerEmbedder{toolsDir: toolsDir, modelDir: modelDir}
}

func (e *nativeSpeakerEmbedder) Embed(pcm []int16) ([]float32, error) {
	e.mu.Lock()
	defer e.mu.Unlock()

	window := fitSpeakerWindow(pcm)
	if speakerRMS(window) < speakerMinRMS {
		return nil, fmt.Errorf("speaker embed: audio too quiet")
	}

	tmpDir, err := os.MkdirTemp(speakerTempRoot(), "aico-speaker-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmpDir)

	wavPath := filepath.Join(tmpDir, "audio.wav")
	featPath := filepath.Join(tmpDir, "features.f32")
	framesPath := filepath.Join(tmpDir, "frames.f32")
	embedPath := filepath.Join(tmpDir, "embedding.f32")
	if err := writePCM16WAV(wavPath, window, speakerSampleRate); err != nil {
		return nil, err
	}

	env := os.Environ()
	env = append(env, "LD_LIBRARY_PATH="+e.toolsDir+speakerLDPathSuffix())

	backbone := filepath.Join(e.modelDir, "eres2net_backbone_rv1106_mmse.rknn")
	tailWeights := filepath.Join(e.modelDir, "eres2net_tail.f32")
	commands := [][]string{
		{filepath.Join(e.toolsDir, "speaker_verify_fbank"), wavPath, featPath},
		{filepath.Join(e.toolsDir, "speaker_verify_rknn"), backbone, featPath, framesPath, "1"},
		{filepath.Join(e.toolsDir, "speaker_verify_tail"), tailWeights, framesPath, embedPath},
	}
	for _, argv := range commands {
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = env
		cmd.Stdout = nil
		cmd.Stderr = nil
		if err := runSpeakerCommand(cmd, speakerEmbedTimeout); err != nil {
			return nil, fmt.Errorf("%s: %w", filepath.Base(argv[0]), err)
		}
	}

	raw, err := os.ReadFile(embedPath)
	if err != nil {
		return nil, err
	}
	if len(raw) != speakerEmbeddingDim*4 {
		return nil, fmt.Errorf("speaker embed: unexpected embedding size %d", len(raw))
	}
	values := make([]float32, speakerEmbeddingDim)
	for i := range values {
		values[i] = math.Float32frombits(binary.LittleEndian.Uint32(raw[i*4:]))
	}
	return normalizeSpeakerEmbedding(values)
}

func runSpeakerCommand(cmd *exec.Cmd, timeout time.Duration) error {
	if err := cmd.Start(); err != nil {
		return err
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		return err
	case <-time.After(timeout):
		_ = cmd.Process.Kill()
		<-done
		return fmt.Errorf("timed out after %s", timeout)
	}
}

func speakerTempRoot() string {
	if info, err := os.Stat("/dev/shm"); err == nil && info.IsDir() {
		if f, err := os.CreateTemp("/dev/shm", "aico-speaker-probe-"); err == nil {
			name := f.Name()
			_ = f.Close()
			_ = os.Remove(name)
			return "/dev/shm"
		}
	}
	return os.TempDir()
}

func speakerLDPathSuffix() string {
	if cur := os.Getenv("LD_LIBRARY_PATH"); cur != "" {
		return string(os.PathListSeparator) + cur
	}
	return ""
}

func fitSpeakerWindow(pcm []int16) []int16 {
	if len(pcm) >= speakerWindowSamples {
		return append([]int16(nil), pcm[len(pcm)-speakerWindowSamples:]...)
	}
	out := make([]int16, speakerWindowSamples)
	copy(out[speakerWindowSamples-len(pcm):], pcm)
	return out
}

func speakerRMS(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, sample := range pcm {
		v := float64(sample) / 32768.0
		sum += v * v
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

func normalizeSpeakerEmbedding(values []float32) ([]float32, error) {
	if len(values) != speakerEmbeddingDim {
		return nil, fmt.Errorf("speaker embed: want %d dims, got %d", speakerEmbeddingDim, len(values))
	}
	var sum float64
	for _, v := range values {
		if math.IsNaN(float64(v)) || math.IsInf(float64(v), 0) {
			return nil, fmt.Errorf("speaker embed: non-finite value")
		}
		sum += float64(v) * float64(v)
	}
	norm := math.Sqrt(sum)
	if !(norm > 1e-12) || math.IsInf(norm, 0) {
		return nil, fmt.Errorf("speaker embed: invalid norm")
	}
	out := make([]float32, len(values))
	for i, v := range values {
		out[i] = float32(float64(v) / norm)
	}
	return out, nil
}

func writePCM16WAV(path string, pcm []int16, sampleRate int) error {
	dataBytes := len(pcm) * 2
	buf := make([]byte, 44+dataBytes)
	copy(buf[0:], []byte("RIFF"))
	binary.LittleEndian.PutUint32(buf[4:], uint32(36+dataBytes))
	copy(buf[8:], []byte("WAVE"))
	copy(buf[12:], []byte("fmt "))
	binary.LittleEndian.PutUint32(buf[16:], 16)
	binary.LittleEndian.PutUint16(buf[20:], 1) // PCM
	binary.LittleEndian.PutUint16(buf[22:], 1) // mono
	binary.LittleEndian.PutUint32(buf[24:], uint32(sampleRate))
	binary.LittleEndian.PutUint32(buf[28:], uint32(sampleRate*2))
	binary.LittleEndian.PutUint16(buf[32:], 2)
	binary.LittleEndian.PutUint16(buf[34:], 16)
	copy(buf[36:], []byte("data"))
	binary.LittleEndian.PutUint32(buf[40:], uint32(dataBytes))
	for i, sample := range pcm {
		binary.LittleEndian.PutUint16(buf[44+i*2:], uint16(sample))
	}
	return os.WriteFile(path, buf, 0o600)
}

// enrollmentWindows mirrors aico-speaker-verification web_demo: three windows
// across a 4-second enrollment clip, skipping quiet ones.
func enrollmentWindows(pcm []int16) [][]int16 {
	if len(pcm) < speakerEnrollSamples {
		padded := make([]int16, speakerEnrollSamples)
		copy(padded, pcm)
		pcm = padded
	} else if len(pcm) > speakerEnrollSamples {
		pcm = pcm[:speakerEnrollSamples]
	}
	starts := []int{0, (speakerEnrollSamples - speakerWindowSamples) / 2, speakerEnrollSamples - speakerWindowSamples}
	var windows [][]int16
	for _, start := range starts {
		window := append([]int16(nil), pcm[start:start+speakerWindowSamples]...)
		if speakerRMS(window) >= speakerMinRMS {
			windows = append(windows, window)
		}
	}
	return windows
}

func enrollSpeakerEmbedding(embedder SpeakerEmbedder, pcm []int16) ([]float32, error) {
	windows := enrollmentWindows(pcm)
	if len(windows) == 0 {
		return nil, fmt.Errorf("enrollment too quiet; record again")
	}
	acc := make([]float64, speakerEmbeddingDim)
	for _, window := range windows {
		vec, err := embedder.Embed(window)
		if err != nil {
			return nil, err
		}
		for i, v := range vec {
			acc[i] += float64(v)
		}
	}
	avg := make([]float32, speakerEmbeddingDim)
	n := float64(len(windows))
	for i := range avg {
		avg[i] = float32(acc[i] / n)
	}
	return normalizeSpeakerEmbedding(avg)
}

func cosineSpeakerSimilarity(a, b []float32) float64 {
	if len(a) != speakerEmbeddingDim || len(b) != speakerEmbeddingDim {
		return 0
	}
	var sum float64
	for i := range a {
		sum += float64(a[i]) * float64(b[i])
	}
	return sum
}
