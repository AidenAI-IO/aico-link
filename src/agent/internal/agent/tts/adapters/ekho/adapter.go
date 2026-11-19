// Package ekho implements local, sentence-based synthesis using the Ekho CLI.
package ekho

import (
	"bytes"
	"context"
	"fmt"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"aico-agent/internal/agent/tts"
)

const sampleRate = 22050

func init() { tts.Register("ekho", New) }

type Adapter struct {
	speed    int
	pace     bool
	lookPath func(string) (string, error)
}

func New(cfg tts.ProviderConfig) (tts.TTSProvider, error) {
	ratio := cfg.SpeedRatio
	if ratio == 0 {
		ratio = 1
	}
	if math.IsNaN(ratio) || ratio < 0.5 || ratio > 2 {
		return nil, fmt.Errorf("ekho: speed ratio must be between 0.5 and 2")
	}
	// Global speed is relative to the tuned baseline, -s 20 (120%).
	pace := true
	if value, ok := cfg.Extra["pace_audio"].(bool); ok {
		pace = value
	}
	return &Adapter{speed: int(math.Round(120*ratio - 100)), pace: pace}, nil
}

func (*Adapter) Name() string { return "ekho" }
func (*Adapter) Close() error { return nil }
func (*Adapter) Capabilities() tts.Capabilities {
	return tts.Capabilities{
		SupportedSampleRates:     []int{sampleRate},
		TextGranularity:          tts.TextGranularitySentence,
		SupportsIncrementalAudio: true,
		SupportsFlush:            true,
		SupportsCancellation:     true,
	}
}

func (a *Adapter) BeginStream(ctx context.Context, sink tts.AudioSink) (tts.StreamSession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if sink == nil || sink.Format() != (tts.AudioFormat{SampleRate: sampleRate, Channels: 1, BitWidth: 16}) {
		return nil, fmt.Errorf("ekho: sink must accept 22050 Hz mono PCM16")
	}
	// Prefer the package-owned build over legacy manual installations. systemd's
	// PATH need not include /usr/local/bin or /userdata.
	var binary string
	var err error
	lookPath := a.lookPath
	if lookPath == nil {
		lookPath = exec.LookPath
	}
	for _, path := range []string{"/usr/lib/aico/ekho", "ekho", "/usr/local/bin/ekho", "/userdata/ekho/bin/ekho"} {
		if binary, err = lookPath(path); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("ekho: install the executable and Mandarin voice data: %w", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	if a.pace {
		sink = &pacedSink{AudioSink: sink, ctx: ctx}
	}
	return &session{ctx: ctx, cancel: cancel, sink: sink, binary: binary, speed: a.speed}, nil
}

type session struct {
	ctx    context.Context
	cancel context.CancelFunc
	sink   tts.AudioSink
	binary string
	speed  int

	// Serialize synthesis and PCM writes. Cancellation deliberately bypasses
	// this lock so it can kill a running child or unblock a playback write.
	mu      sync.Mutex
	buffer  string
	closed  bool
	lastErr error
}

func (s *session) WriteText(text string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	s.buffer += text
	return s.emit(false)
}

func (s *session) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	return s.emit(true)
}

func (s *session) ResetBuffer() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.buffer = ""
}

func (s *session) Cancel() error {
	s.cancel()
	return s.sink.Stop()
}

func (s *session) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.lastErr
	}
	defer s.cancel()
	if s.ready() == nil {
		s.emit(true)
	}
	s.closed = true
	s.buffer = ""
	if s.lastErr == nil {
		s.record(s.sink.Drain(s.ctx))
	}
	return s.lastErr
}

func (s *session) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr
}

func (s *session) ready() error {
	if s.lastErr != nil {
		return s.lastErr
	}
	if s.closed {
		return tts.ErrSessionClosed
	}
	return s.record(s.ctx.Err())
}

func (s *session) record(err error) error {
	if s.lastErr == nil {
		s.lastErr = err
	}
	return s.lastErr
}

func (s *session) emit(flush bool) error {
	for {
		n := nextBoundary(s.buffer, flush)
		if n == 0 {
			return nil
		}
		text := strings.TrimSpace(s.buffer[:n])
		s.buffer = s.buffer[n:]
		if strings.IndexFunc(text, func(r rune) bool { return unicode.IsLetter(r) || unicode.IsNumber(r) }) < 0 {
			continue
		}
		if err := s.synthesize(text); err != nil {
			return s.record(err)
		}
	}
}

func (s *session) synthesize(text string) error {
	// Ekho/libsndfile needs a seekable WAV, so stdout is not usable. Keep one
	// bounded sentence on disk and stream its PCM to the existing audio sink.
	f, err := os.CreateTemp("", "aico-ekho-*.wav")
	if err != nil {
		return fmt.Errorf("ekho: create WAV: %w", err)
	}
	path := f.Name()
	defer os.Remove(path)
	if err := f.Close(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	// -a MUST follow --english-speed: Ekho 9.0's option parser otherwise
	// falls through and applies the English speed value to volume as well.
	cmd := exec.CommandContext(ctx, s.binary, "-v", "Mandarin", "-s", strconv.Itoa(s.speed),
		"--english-speed", "-10", "-a", "0", "--samplerate", strconv.Itoa(sampleRate), "-f", "-", "-o", path)
	cmd.Stdin = strings.NewReader(text)
	var stderr boundedStderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("ekho: synthesis failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	wav, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("ekho: open WAV: %w", err)
	}
	defer wav.Close()
	if err := writeWAVPCM(s.ctx, wav, s.sink); err != nil {
		return fmt.Errorf("ekho: output: %w", err)
	}
	return nil
}

// Bound diagnostic output independently of the text and audio sizes.
type boundedStderr struct{ buffer bytes.Buffer }

func (b *boundedStderr) String() string { return b.buffer.String() }

func (b *boundedStderr) Write(p []byte) (int, error) {
	n := len(p)
	if remaining := 4096 - b.buffer.Len(); remaining > 0 {
		if len(p) > remaining {
			p = p[:remaining]
		}
		b.buffer.Write(p)
	}
	return n, nil
}
