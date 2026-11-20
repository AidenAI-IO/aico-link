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
		OutputChannels:           1,
		OutputBitWidth:           16,
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
	s := &session{
		ctx: ctx, cancel: cancel, sink: sink, binary: binary, speed: a.speed,
		pending: make(chan synthesisJob, 1), audio: make(chan playbackJob),
		done: make(chan struct{}), watcherDone: make(chan struct{}),
	}
	go s.synthesisLoop()
	go s.playbackLoop()
	go func() {
		defer close(s.watcherDone)
		select {
		case <-ctx.Done():
			s.fail(ctx.Err())
		case <-s.done:
		}
	}()
	return s, nil
}

type synthesisJob struct {
	text  string
	voice string
	ack   chan struct{}
}

type playbackJob struct {
	path string
	ack  chan struct{}
}

type session struct {
	ctx    context.Context
	cancel context.CancelFunc
	sink   tts.AudioSink
	binary string
	speed  int

	// Input calls serialize segmentation and queue ownership. Workers never
	// take this lock, so cancellation and errors can unblock a full queue.
	inputMu sync.Mutex
	buffer  string
	voice   string
	closed  bool
	pending chan synthesisJob

	// An unbuffered handoff permits exactly one next WAV to be synthesized
	// while the playback worker consumes the current one. There is only one
	// synthesis process, plus one bounded text job waiting for it.
	audio       chan playbackJob
	done        chan struct{}
	watcherDone chan struct{}
	closeOnce   sync.Once
	stopOnce    sync.Once
	stopErr     error
	errMu       sync.Mutex
	lastErr     error
}

func (s *session) WriteText(text string) error {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	s.buffer += text
	return s.emit(false)
}

// Flush emits an unfinished sentence and waits for all preceding PCM writes.
// It leaves input open and does not drain/end the playback session.
func (s *session) Flush() error {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	if err := s.ready(); err != nil {
		return err
	}
	if err := s.emit(true); err != nil {
		return err
	}
	ack := make(chan struct{})
	if err := s.enqueue(synthesisJob{ack: ack}); err != nil {
		return err
	}
	select {
	case <-ack:
		return s.Err()
	case <-s.ctx.Done():
		return s.record(s.ctx.Err())
	}
}

func (s *session) ResetBuffer() {
	s.inputMu.Lock()
	defer s.inputMu.Unlock()
	s.buffer = ""
	s.voice = ""
}

func (s *session) stop() error {
	s.stopOnce.Do(func() { s.stopErr = s.sink.Stop() })
	return s.stopErr
}

func (s *session) Cancel() error {
	s.record(context.Canceled)
	s.cancel()
	return s.stop()
}

func (s *session) Close() error {
	s.closeOnce.Do(func() {
		s.inputMu.Lock()
		if s.ready() == nil {
			s.emit(true)
		}
		s.closed = true
		s.buffer = ""
		close(s.pending)
		s.inputMu.Unlock()

		// Workers own their WAVs until playback/removal finishes, including on
		// cancellation. Join them before releasing the provider/session lease.
		<-s.done
		<-s.watcherDone
		s.record(s.ctx.Err())
		if s.Err() == nil {
			if err := s.sink.Drain(s.ctx); err != nil {
				s.fail(err)
			}
		}
		s.cancel()
	})
	return s.Err()
}

func (s *session) Err() error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	return s.lastErr
}

// ready is called with inputMu held; lastErr has its own worker-safe lock.
func (s *session) ready() error {
	if err := s.Err(); err != nil {
		return err
	}
	if s.closed {
		return tts.ErrSessionClosed
	}
	return s.record(s.ctx.Err())
}

func (s *session) record(err error) error {
	s.errMu.Lock()
	defer s.errMu.Unlock()
	if s.lastErr == nil {
		s.lastErr = err
	}
	return s.lastErr
}

func (s *session) fail(err error) {
	s.record(err)
	s.cancel()
	s.stop()
}

func (s *session) enqueue(job synthesisJob) error {
	if err := s.ctx.Err(); err != nil {
		return s.record(err)
	}
	select {
	case s.pending <- job:
		return s.Err()
	case <-s.ctx.Done():
		return s.record(s.ctx.Err())
	}
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
		// Mandarin normalizes digits to Chinese before its English fallback.
		// Route Latin-only text as a whole to English so Flite also receives
		// numbers, currency, dates and units. Digit-only continuations inherit
		// the preceding chunk's language, including across the size limit.
		s.voice = voiceForText(text, s.voice)
		if err := s.enqueue(synthesisJob{text: text, voice: s.voice}); err != nil {
			return err
		}
	}
}

func (s *session) synthesisLoop() {
	defer close(s.audio)
	for {
		var job synthesisJob
		select {
		case <-s.ctx.Done():
			return
		case next, ok := <-s.pending:
			if !ok {
				return
			}
			job = next
		}
		if s.ctx.Err() != nil {
			return
		}
		out := playbackJob{ack: job.ack}
		if job.ack == nil {
			path, err := s.synthesize(job.text, job.voice)
			if err != nil {
				s.fail(err)
				return
			}
			out.path = path
		}
		select {
		case s.audio <- out:
			// The playback worker now owns the file.
		case <-s.ctx.Done():
			if out.path != "" {
				os.Remove(out.path)
			}
			return
		}
	}
}

func (s *session) playbackLoop() {
	defer close(s.done)
	for job := range s.audio {
		if job.path != "" {
			if s.ctx.Err() == nil {
				if err := s.play(job.path); err != nil {
					s.fail(err)
				}
			}
			os.Remove(job.path)
		}
		if job.ack != nil {
			close(job.ack)
		}
	}
}

func (s *session) play(path string) error {
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

func (s *session) synthesize(text, voice string) (path string, err error) {
	// Ekho/libsndfile needs a seekable WAV, so stdout is not usable. Keep one
	// bounded sentence on disk; hand ownership to playback only on success.
	f, err := os.CreateTemp("", "aico-ekho-*.wav")
	if err != nil {
		return "", fmt.Errorf("ekho: create WAV: %w", err)
	}
	path = f.Name()
	defer func() {
		if err != nil {
			os.Remove(f.Name())
		}
	}()
	if err := f.Close(); err != nil {
		return "", err
	}
	ctx, cancel := context.WithTimeout(s.ctx, 30*time.Second)
	defer cancel()
	// -a MUST follow --english-speed: Ekho 9.0's option parser otherwise
	// falls through and applies the English speed value to volume as well.
	cmd := exec.CommandContext(ctx, s.binary, "-v", voice, "-s", strconv.Itoa(s.speed),
		"--english-speed", "-10", "-a", "0", "--samplerate", strconv.Itoa(sampleRate), "-f", "-", "-o", path)
	cmd.Stdin = strings.NewReader(text)
	var stderr boundedStderr
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return "", ctx.Err()
		}
		return "", fmt.Errorf("ekho: synthesis failed: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return path, nil
}

func voiceForText(text, previous string) string {
	latin := false
	for _, r := range text {
		if unicode.IsLetter(r) {
			if !unicode.Is(unicode.Latin, r) {
				return "Mandarin"
			}
			latin = true
		}
	}
	if latin || previous == "English" {
		return "English"
	}
	return "Mandarin"
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
