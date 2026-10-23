package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"time"

	"aico-agent/internal/agent/tts"
)

var (
	localAudioLookPath       = exec.LookPath
	localAudioCommandContext = exec.CommandContext
)

const (
	wavMaxUint16    = uint64(65535)
	wavMaxUint32    = uint64(4294967295)
	wavMaxDataBytes = wavMaxUint32 - 36
)

type localAudioPlaybackBackend struct {
	mu                       sync.Mutex
	playerAdmission          sync.Mutex
	playerAdmissionBlockedFn func()
	nextID                   uint64
	sessions                 map[uint64]*localAudioPlaybackSession
	player                   *localAudioPlayer
	logger                   *Logger
	completedErrs            []error
}

type localAudioPlaybackSession struct {
	mu        sync.Mutex
	format    tts.AudioFormat
	file      *os.File
	path      string
	dataBytes uint64
	final     bool
	stopped   bool
	failedErr error
	cancel    context.CancelFunc
	done      chan error
}

type localAudioPlayer struct {
	name string
	args func(path string) []string
}

func newLocalAudioPlaybackBackend(logger *Logger) *localAudioPlaybackBackend {
	return &localAudioPlaybackBackend{
		sessions: make(map[uint64]*localAudioPlaybackSession),
		logger:   logger,
	}
}

func (b *localAudioPlaybackBackend) StartPlayback(format tts.AudioFormat) (uint64, error) {
	if err := validateLocalPlaybackFormat(format); err != nil {
		return 0, err
	}
	player, err := b.localPlayer()
	if err != nil {
		return 0, err
	}
	file, err := os.CreateTemp("", "aico-tts-*.wav")
	if err != nil {
		return 0, fmt.Errorf("create local playback wav: %w", err)
	}
	if err := writeWAVHeader(file, format, 0); err != nil {
		_ = file.Close()
		_ = os.Remove(file.Name())
		return 0, fmt.Errorf("write local playback wav header: %w", err)
	}

	b.mu.Lock()
	b.nextID++
	id := b.nextID
	b.sessions[id] = &localAudioPlaybackSession{
		format: format,
		file:   file,
		path:   file.Name(),
	}
	b.player = player
	b.mu.Unlock()
	return id, nil
}

func (b *localAudioPlaybackBackend) WritePlayChunk(sessionID uint64, data []byte, isFinal bool) error {
	b.mu.Lock()
	session := b.sessions[sessionID]
	player := b.player
	b.mu.Unlock()
	if session == nil {
		return fmt.Errorf("local playback session not found: %d", sessionID)
	}

	session.mu.Lock()
	defer session.mu.Unlock()
	if err := session.stateError(sessionID); err != nil {
		return err
	}
	if len(data) > 0 {
		if session.final {
			return fmt.Errorf("local playback session already finalized")
		}
		if session.file == nil {
			return fmt.Errorf("local playback session file is unavailable")
		}
		if err := validateLocalPlaybackAppendSize(session.dataBytes, uint64(len(data))); err != nil {
			return err
		}
		n, err := session.file.Write(data)
		if err != nil {
			session.dataBytes += uint64(n)
			return b.failLocalPlaybackSessionLocked(sessionID, session, fmt.Errorf("write local playback pcm: %w", err))
		}
		session.dataBytes += uint64(n)
		if n != len(data) {
			return b.failLocalPlaybackSessionLocked(sessionID, session, fmt.Errorf("write local playback pcm: wrote %d of %d bytes", n, len(data)))
		}
	}
	if !isFinal {
		return nil
	}
	if session.final {
		return nil
	}
	return b.finalizeLocalPlaybackSessionLocked(sessionID, session, player)
}

func (b *localAudioPlaybackBackend) StopPlayback(sessionID uint64) error {
	b.mu.Lock()
	session := b.sessions[sessionID]
	delete(b.sessions, sessionID)
	b.mu.Unlock()
	if session == nil {
		return nil
	}
	session.mu.Lock()
	session.stopped = true
	if session.cancel != nil {
		session.cancel()
	}
	if session.file != nil {
		_ = session.file.Close()
		session.file = nil
	}
	done := session.done
	path := session.path
	session.mu.Unlock()
	if done != nil {
		<-done
	}
	if path != "" {
		_ = os.Remove(path)
	}
	return nil
}

// WaitForPlaybackDrain waits for finalized local player processes to exit.
// Realtime uses this to report notification delivery only after playback,
// rather than file submission, has completed.
func (b *localAudioPlaybackBackend) WaitForPlaybackDrain(ctx context.Context) error {
	if b == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for {
		b.mu.Lock()
		pending := len(b.sessions)
		var completedErr error
		if pending == 0 && len(b.completedErrs) > 0 {
			completedErr = errors.Join(b.completedErrs...)
			b.completedErrs = nil
		}
		b.mu.Unlock()
		if pending == 0 {
			return completedErr
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func (s *localAudioPlaybackSession) stateError(sessionID uint64) error {
	if s.failedErr != nil {
		return fmt.Errorf("local playback session failed: %w", s.failedErr)
	}
	if s.stopped {
		return fmt.Errorf("local playback session stopped: %d", sessionID)
	}
	return nil
}

func (b *localAudioPlaybackBackend) finalizeLocalPlaybackSessionLocked(sessionID uint64, session *localAudioPlaybackSession, player *localAudioPlayer) error {
	if err := validateLocalPlaybackDataSize(session.dataBytes); err != nil {
		return b.failLocalPlaybackSessionLocked(sessionID, session, err)
	}
	if session.file == nil {
		return b.failLocalPlaybackSessionLocked(sessionID, session, fmt.Errorf("local playback session file is unavailable"))
	}
	if err := patchWAVHeader(session.file, session.format, session.dataBytes); err != nil {
		return b.failLocalPlaybackSessionLocked(sessionID, session, fmt.Errorf("finalize local playback wav header: %w", err))
	}
	if err := session.file.Close(); err != nil {
		return b.failLocalPlaybackSessionLocked(sessionID, session, fmt.Errorf("close local playback wav: %w", err))
	}
	session.file = nil
	if player == nil {
		return b.failLocalPlaybackSessionLocked(sessionID, session, fmt.Errorf("local audio player is unavailable"))
	}
	b.acquirePlayerAdmission()
	ctx, cancel := context.WithCancel(context.Background())
	session.cancel = cancel
	cmd := localAudioCommandContext(ctx, player.name, player.args(session.path)...)
	if err := cmd.Start(); err != nil {
		cancel()
		b.playerAdmission.Unlock()
		return b.failLocalPlaybackSessionLocked(sessionID, session, fmt.Errorf("start local audio player %q: %w", player.name, err))
	}
	session.done = make(chan error, 1)
	session.final = true
	go func(path string, id uint64, done chan<- error) {
		defer b.playerAdmission.Unlock()
		err := cmd.Wait()
		cancel()
		_ = os.Remove(path)
		session.mu.Lock()
		stopped := session.stopped
		if err != nil && !stopped {
			session.failedErr = err
		}
		session.mu.Unlock()
		b.mu.Lock()
		if err != nil && !stopped {
			b.completedErrs = append(b.completedErrs, err)
		}
		delete(b.sessions, id)
		b.mu.Unlock()
		done <- err
	}(session.path, sessionID, session.done)
	return nil
}

func (b *localAudioPlaybackBackend) acquirePlayerAdmission() {
	if b.playerAdmission.TryLock() {
		return
	}
	if b.playerAdmissionBlockedFn != nil {
		b.playerAdmissionBlockedFn()
	}
	b.playerAdmission.Lock()
}

func (b *localAudioPlaybackBackend) failLocalPlaybackSessionLocked(sessionID uint64, session *localAudioPlaybackSession, err error) error {
	session.failedErr = err
	b.cleanupLocalPlaybackSessionLocked(sessionID, session)
	return err
}

func (b *localAudioPlaybackBackend) cleanupLocalPlaybackSessionLocked(sessionID uint64, session *localAudioPlaybackSession) {
	if session.cancel != nil {
		session.cancel()
		session.cancel = nil
	}
	if session.file != nil {
		_ = session.file.Close()
		session.file = nil
	}
	if session.path != "" {
		_ = os.Remove(session.path)
		session.path = ""
	}
	b.mu.Lock()
	delete(b.sessions, sessionID)
	b.mu.Unlock()
}

func (b *localAudioPlaybackBackend) localPlayer() (*localAudioPlayer, error) {
	b.mu.Lock()
	player := b.player
	b.mu.Unlock()
	if player != nil {
		return player, nil
	}
	player, err := detectLocalAudioPlayer()
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	if b.player == nil {
		b.player = player
	}
	player = b.player
	b.mu.Unlock()
	if b.logger != nil {
		b.logger.Info("Local TTS playback enabled: player=%s", player.name)
	}
	return player, nil
}

func detectLocalAudioPlayer() (*localAudioPlayer, error) {
	for _, candidate := range localAudioPlayerCandidates(runtime.GOOS) {
		if _, err := localAudioLookPath(candidate.name); err == nil {
			return &candidate, nil
		}
	}
	return nil, fmt.Errorf("no local audio player found (tried afplay, pw-play, paplay, aplay, ffplay, powershell)")
}

func localAudioPlayerCandidates(goos string) []localAudioPlayer {
	switch goos {
	case "darwin":
		return []localAudioPlayer{
			{name: "afplay", args: func(path string) []string { return []string{path} }},
			{name: "ffplay", args: func(path string) []string { return []string{"-nodisp", "-autoexit", "-loglevel", "error", path} }},
		}
	case "windows":
		return []localAudioPlayer{
			{name: "powershell", args: func(path string) []string {
				return []string{"-NoProfile", "-Command", "(New-Object Media.SoundPlayer $args[0]).PlaySync()", path}
			}},
			{name: "powershell.exe", args: func(path string) []string {
				return []string{"-NoProfile", "-Command", "(New-Object Media.SoundPlayer $args[0]).PlaySync()", path}
			}},
		}
	default:
		return []localAudioPlayer{
			{name: "pw-play", args: func(path string) []string { return []string{path} }},
			{name: "paplay", args: func(path string) []string { return []string{path} }},
			{name: "aplay", args: func(path string) []string { return []string{"-q", path} }},
			{name: "ffplay", args: func(path string) []string { return []string{"-nodisp", "-autoexit", "-loglevel", "error", path} }},
		}
	}
}

func validateLocalPlaybackFormat(format tts.AudioFormat) error {
	if format.SampleRate <= 0 {
		return fmt.Errorf("local playback requires sample_rate > 0")
	}
	if uint64(format.SampleRate) > wavMaxUint32 {
		return fmt.Errorf("local playback sample_rate %d exceeds WAV uint32 limit", format.SampleRate)
	}
	if format.Channels <= 0 {
		return fmt.Errorf("local playback requires channels > 0")
	}
	if uint64(format.Channels) > wavMaxUint16 {
		return fmt.Errorf("local playback channels %d exceeds WAV uint16 limit", format.Channels)
	}
	if format.BitWidth <= 0 || format.BitWidth%8 != 0 {
		return fmt.Errorf("local playback requires byte-aligned bit_width, got %d", format.BitWidth)
	}
	if uint64(format.BitWidth) > wavMaxUint16 {
		return fmt.Errorf("local playback bit_width %d exceeds WAV uint16 limit", format.BitWidth)
	}
	blockAlign := uint64(format.Channels) * uint64(format.BitWidth/8)
	if blockAlign > wavMaxUint16 {
		return fmt.Errorf("local playback block_align %d exceeds WAV uint16 limit", blockAlign)
	}
	byteRate := uint64(format.SampleRate) * blockAlign
	if byteRate > wavMaxUint32 {
		return fmt.Errorf("local playback byte_rate %d exceeds WAV uint32 limit", byteRate)
	}
	return nil
}

func validateLocalPlaybackDataSize(dataBytes uint64) error {
	if dataBytes > wavMaxDataBytes {
		return fmt.Errorf("local playback wav data size %d exceeds RIFF limit %d", dataBytes, wavMaxDataBytes)
	}
	return nil
}

func validateLocalPlaybackAppendSize(currentBytes, chunkBytes uint64) error {
	if err := validateLocalPlaybackDataSize(currentBytes); err != nil {
		return err
	}
	if chunkBytes > wavMaxDataBytes-currentBytes {
		return fmt.Errorf("local playback wav data size %d exceeds RIFF limit %d", currentBytes+chunkBytes, wavMaxDataBytes)
	}
	return nil
}

func writeWAVHeader(file *os.File, format tts.AudioFormat, dataBytes uint64) error {
	if err := validateLocalPlaybackFormat(format); err != nil {
		return err
	}
	if err := validateLocalPlaybackDataSize(dataBytes); err != nil {
		return err
	}
	header := make([]byte, 44)
	copy(header[0:4], "RIFF")
	binary.LittleEndian.PutUint32(header[4:8], uint32(36+dataBytes))
	copy(header[8:12], "WAVE")
	copy(header[12:16], "fmt ")
	binary.LittleEndian.PutUint32(header[16:20], 16)
	binary.LittleEndian.PutUint16(header[20:22], 1)
	binary.LittleEndian.PutUint16(header[22:24], uint16(format.Channels))
	binary.LittleEndian.PutUint32(header[24:28], uint32(format.SampleRate))
	blockAlign := uint16(format.Channels * (format.BitWidth / 8))
	binary.LittleEndian.PutUint32(header[28:32], uint32(format.SampleRate)*uint32(blockAlign))
	binary.LittleEndian.PutUint16(header[32:34], blockAlign)
	binary.LittleEndian.PutUint16(header[34:36], uint16(format.BitWidth))
	copy(header[36:40], "data")
	binary.LittleEndian.PutUint32(header[40:44], uint32(dataBytes))
	if _, err := file.Write(header); err != nil {
		return err
	}
	return nil
}

func patchWAVHeader(file *os.File, format tts.AudioFormat, dataBytes uint64) error {
	if _, err := file.Seek(0, 0); err != nil {
		return err
	}
	if err := writeWAVHeader(file, format, dataBytes); err != nil {
		return err
	}
	_, err := file.Seek(0, 2)
	return err
}
