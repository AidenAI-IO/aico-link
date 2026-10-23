package agent

import (
	"context"
	"encoding/binary"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"aico-agent/internal/agent/tts"
)

func TestLocalAudioBackendWritesWAVAndCleansUp(t *testing.T) {
	oldLookPath := localAudioLookPath
	oldCommandContext := localAudioCommandContext
	defer func() {
		localAudioLookPath = oldLookPath
		localAudioCommandContext = oldCommandContext
	}()

	localAudioLookPath = func(name string) (string, error) {
		return name, nil
	}
	var playedPath string
	localAudioCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		if len(args) == 0 {
			t.Fatalf("local player %q received no wav path", name)
		}
		playedPath = args[len(args)-1]
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestLocalAudioPlaybackHelper")
		cmd.Env = append(os.Environ(), "AICO_LOCAL_AUDIO_HELPER=1")
		return cmd
	}

	backend := newLocalAudioPlaybackBackend(nil)
	format := tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}
	sessionID, err := backend.StartPlayback(format)
	if err != nil {
		t.Fatalf("StartPlayback() error = %v", err)
	}
	pcm := []byte{1, 2, 3, 4}
	if err := backend.WritePlayChunk(sessionID, pcm, false); err != nil {
		t.Fatalf("WritePlayChunk(pcm) error = %v", err)
	}
	if err := backend.WritePlayChunk(sessionID, nil, true); err != nil {
		t.Fatalf("WritePlayChunk(final) error = %v", err)
	}
	if playedPath == "" {
		t.Fatal("local player was not invoked")
	}
	wav, err := os.ReadFile(playedPath)
	if err != nil {
		t.Fatalf("read generated wav: %v", err)
	}
	if string(wav[0:4]) != "RIFF" || string(wav[8:12]) != "WAVE" || string(wav[36:40]) != "data" {
		t.Fatalf("invalid wav header: %q %q %q", wav[0:4], wav[8:12], wav[36:40])
	}
	if got := binary.LittleEndian.Uint32(wav[40:44]); got != uint32(len(pcm)) {
		t.Fatalf("data size = %d, want %d", got, len(pcm))
	}
	if got := wav[44:]; string(got) != string(pcm) {
		t.Fatalf("pcm payload = %#v, want %#v", got, pcm)
	}

	if err := backend.StopPlayback(sessionID); err != nil {
		t.Fatalf("StopPlayback() error = %v", err)
	}
	if _, err := os.Stat(playedPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("generated wav still exists or stat failed differently: %v", err)
	}
}

func TestLocalAudioBackendReportsMissingPlayer(t *testing.T) {
	oldLookPath := localAudioLookPath
	defer func() { localAudioLookPath = oldLookPath }()
	localAudioLookPath = func(name string) (string, error) {
		return "", exec.ErrNotFound
	}

	backend := newLocalAudioPlaybackBackend(nil)
	_, err := backend.StartPlayback(tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err == nil {
		t.Fatal("StartPlayback() error = nil, want missing local player")
	}
}

func TestLocalAudioRecordingBackendStreamsPCMAndStops(t *testing.T) {
	oldLookPath := localAudioRecorderLookPath
	oldCommandContext := localAudioRecorderCommandContext
	defer func() {
		localAudioRecorderLookPath = oldLookPath
		localAudioRecorderCommandContext = oldCommandContext
	}()
	localAudioRecorderLookPath = func(string) (string, error) { return "helper", nil }
	localAudioRecorderCommandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestLocalAudioRecordingHelper")
		cmd.Env = append(os.Environ(), "AICO_LOCAL_AUDIO_RECORD_HELPER=1")
		return cmd
	}

	backend := newLocalAudioRecordingBackend(nil)
	result, err := backend.StartRecording(AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatalf("StartRecording() error = %v", err)
	}
	chunk, err := backend.ReadRecordChunk(result.SessionID, 1000)
	if err != nil {
		t.Fatalf("ReadRecordChunk() error = %v", err)
	}
	if string(chunk.PCM) != "local-pcm" {
		t.Fatalf("PCM = %q, want local-pcm", chunk.PCM)
	}
	if err := backend.StopRecording(result.SessionID); err != nil {
		t.Fatalf("StopRecording() error = %v", err)
	}
}

func TestSendLocalRecordingChunkPrefersBufferedDeliveryAfterStop(t *testing.T) {
	session := &localAudioRecordingSession{
		chunks: make(chan []byte, 1),
		stopCh: make(chan struct{}),
	}
	close(session.stopCh)

	want := []byte("recording-tail")
	if !sendLocalRecordingChunk(session, want) {
		t.Fatal("sendLocalRecordingChunk() = false, want already-read PCM delivered")
	}
	if got := <-session.chunks; string(got) != string(want) {
		t.Fatalf("delivered PCM = %q, want %q", got, want)
	}
}

func TestLocalAudioRecordingHelper(t *testing.T) {
	if os.Getenv("AICO_LOCAL_AUDIO_RECORD_HELPER") == "1" {
		_, _ = os.Stdout.Write([]byte("local-pcm"))
		os.Exit(0)
	}
	t.Skip()
}

func TestValidateLocalPlaybackFormatRejectsWAVHeaderOverflow(t *testing.T) {
	cases := []struct {
		name   string
		format tts.AudioFormat
		want   string
	}{
		{
			name:   "channels",
			format: tts.AudioFormat{SampleRate: 16000, Channels: 65536, BitWidth: 16},
			want:   "channels",
		},
		{
			name:   "bit width",
			format: tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 65536},
			want:   "bit_width",
		},
		{
			name:   "block align",
			format: tts.AudioFormat{SampleRate: 16000, Channels: 65535, BitWidth: 16},
			want:   "block_align",
		},
		{
			name:   "byte rate",
			format: tts.AudioFormat{SampleRate: 1000000, Channels: 1, BitWidth: 65528},
			want:   "byte_rate",
		},
	}
	if strconv.IntSize > 32 {
		tooLargeSampleRate := wavMaxUint32 + 1
		cases = append(cases, struct {
			name   string
			format tts.AudioFormat
			want   string
		}{
			name:   "sample rate",
			format: tts.AudioFormat{SampleRate: int(tooLargeSampleRate), Channels: 1, BitWidth: 16},
			want:   "sample_rate",
		})
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateLocalPlaybackFormat(tc.format)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("validateLocalPlaybackFormat() error = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestLocalAudioBackendRejectsRIFFDataOverflow(t *testing.T) {
	oldLookPath := localAudioLookPath
	defer func() { localAudioLookPath = oldLookPath }()
	localAudioLookPath = func(name string) (string, error) {
		return name, nil
	}

	backend := newLocalAudioPlaybackBackend(nil)
	sessionID, err := backend.StartPlayback(tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatalf("StartPlayback() error = %v", err)
	}
	defer func() {
		if err := backend.StopPlayback(sessionID); err != nil {
			t.Fatalf("StopPlayback() error = %v", err)
		}
	}()

	session := localAudioPlaybackTestSession(t, backend, sessionID)
	session.mu.Lock()
	session.dataBytes = wavMaxDataBytes
	session.mu.Unlock()
	err = backend.WritePlayChunk(sessionID, []byte{1}, false)
	if err == nil || !strings.Contains(err.Error(), "RIFF limit") {
		t.Fatalf("WritePlayChunk(overflow) error = %v, want RIFF limit", err)
	}

	session.mu.Lock()
	session.dataBytes = wavMaxDataBytes + 1
	session.mu.Unlock()
	err = backend.WritePlayChunk(sessionID, nil, true)
	if err == nil || !strings.Contains(err.Error(), "RIFF limit") {
		t.Fatalf("WritePlayChunk(final overflow) error = %v, want RIFF limit", err)
	}
}

func TestLocalAudioBackendFinalizationFailureCleansUp(t *testing.T) {
	oldLookPath := localAudioLookPath
	oldCommandContext := localAudioCommandContext
	defer func() {
		localAudioLookPath = oldLookPath
		localAudioCommandContext = oldCommandContext
	}()
	localAudioLookPath = func(name string) (string, error) {
		return name, nil
	}
	missingPlayer := filepath.Join(t.TempDir(), "missing-player")
	localAudioCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		return exec.CommandContext(ctx, missingPlayer)
	}

	backend := newLocalAudioPlaybackBackend(nil)
	sessionID, err := backend.StartPlayback(tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatalf("StartPlayback() error = %v", err)
	}
	session := localAudioPlaybackTestSession(t, backend, sessionID)
	path := session.path

	err = backend.WritePlayChunk(sessionID, []byte{1, 2}, true)
	if err == nil || !strings.Contains(err.Error(), "start local audio player") {
		t.Fatalf("WritePlayChunk(final start failure) error = %v, want start local audio player", err)
	}
	session.mu.Lock()
	final := session.final
	failedErr := session.failedErr
	session.mu.Unlock()
	if final {
		t.Fatal("session.final = true after failed finalization, want false")
	}
	if failedErr == nil {
		t.Fatal("session.failedErr = nil after failed finalization")
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary wav still exists or stat failed differently: %v", err)
	}
	if localAudioPlaybackSessionExists(backend, sessionID) {
		t.Fatal("failed session still present in backend session map")
	}
	err = backend.WritePlayChunk(sessionID, nil, true)
	if err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("WritePlayChunk(second final) error = %v, want session not found", err)
	}
}

func TestLocalAudioBackendWriteFailureCleansUp(t *testing.T) {
	oldLookPath := localAudioLookPath
	defer func() { localAudioLookPath = oldLookPath }()
	localAudioLookPath = func(name string) (string, error) {
		return name, nil
	}

	backend := newLocalAudioPlaybackBackend(nil)
	sessionID, err := backend.StartPlayback(tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatalf("StartPlayback() error = %v", err)
	}
	session := localAudioPlaybackTestSession(t, backend, sessionID)
	path := session.path
	session.mu.Lock()
	if err := session.file.Close(); err != nil {
		t.Fatalf("close test wav: %v", err)
	}
	session.mu.Unlock()

	err = backend.WritePlayChunk(sessionID, []byte{1, 2}, false)
	if err == nil || !strings.Contains(err.Error(), "write local playback pcm") {
		t.Fatalf("WritePlayChunk(write failure) error = %v, want write local playback pcm", err)
	}
	session.mu.Lock()
	failedErr := session.failedErr
	dataBytes := session.dataBytes
	session.mu.Unlock()
	if failedErr == nil {
		t.Fatal("session.failedErr = nil after write failure")
	}
	if dataBytes != 0 {
		t.Fatalf("session.dataBytes = %d, want 0 for failed zero-byte write", dataBytes)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary wav still exists or stat failed differently: %v", err)
	}
	if localAudioPlaybackSessionExists(backend, sessionID) {
		t.Fatal("failed session still present in backend session map")
	}
}

func TestLocalAudioBackendSerializesPlayerAdmission(t *testing.T) {
	oldLookPath := localAudioLookPath
	oldCommandContext := localAudioCommandContext
	defer func() {
		localAudioLookPath = oldLookPath
		localAudioCommandContext = oldCommandContext
	}()
	localAudioLookPath = func(name string) (string, error) {
		return name, nil
	}

	releaseFirst := filepath.Join(t.TempDir(), "release-first")
	starts := make(chan int, 2)
	admissionBlocked := make(chan struct{}, 1)
	var startCount int32
	localAudioCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		idx := int(atomic.AddInt32(&startCount, 1))
		starts <- idx
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestLocalAudioPlaybackHelper")
		cmd.Env = append(os.Environ(), "AICO_LOCAL_AUDIO_HELPER=1")
		if idx == 1 {
			cmd.Env = append(cmd.Env, "AICO_LOCAL_AUDIO_WAIT_FILE="+releaseFirst)
		} else {
			cmd.Env = append(cmd.Env, "AICO_LOCAL_AUDIO_EXIT=1")
		}
		return cmd
	}

	backend := newLocalAudioPlaybackBackend(nil)
	backend.playerAdmissionBlockedFn = func() {
		admissionBlocked <- struct{}{}
	}
	firstID, err := backend.StartPlayback(tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatalf("first StartPlayback() error = %v", err)
	}
	defer func() { _ = backend.StopPlayback(firstID) }()
	secondID, err := backend.StartPlayback(tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatalf("second StartPlayback() error = %v", err)
	}
	defer func() { _ = backend.StopPlayback(secondID) }()

	firstDone := make(chan error, 1)
	go func() {
		firstDone <- backend.WritePlayChunk(firstID, []byte{1}, true)
	}()
	if got := waitLocalAudioStart(t, starts); got != 1 {
		t.Fatalf("first player start index = %d, want 1", got)
	}
	if err := waitLocalAudioDone(t, firstDone); err != nil {
		t.Fatalf("first WritePlayChunk(final) error = %v", err)
	}

	secondDone := make(chan error, 1)
	go func() {
		secondDone <- backend.WritePlayChunk(secondID, []byte{2}, true)
	}()
	waitLocalAudioAdmissionBlocked(t, admissionBlocked)
	select {
	case got := <-starts:
		t.Fatalf("second player started before first admission released: start index %d", got)
	default:
	}

	if err := os.WriteFile(releaseFirst, []byte("done"), 0600); err != nil {
		t.Fatalf("release first player: %v", err)
	}
	if got := waitLocalAudioStart(t, starts); got != 2 {
		t.Fatalf("second player start index = %d, want 2", got)
	}
	if err := waitLocalAudioDone(t, secondDone); err != nil {
		t.Fatalf("second WritePlayChunk(final) error = %v", err)
	}
}

func TestWriteWAVHeaderRejectsOversizeData(t *testing.T) {
	file, err := os.CreateTemp("", "aico-tts-header-*.wav")
	if err != nil {
		t.Fatalf("CreateTemp() error = %v", err)
	}
	defer func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}()

	err = writeWAVHeader(file, tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}, wavMaxDataBytes+1)
	if err == nil || !strings.Contains(err.Error(), "RIFF limit") {
		t.Fatalf("writeWAVHeader() error = %v, want RIFF limit", err)
	}
}

func localAudioPlaybackTestSession(t *testing.T, backend *localAudioPlaybackBackend, sessionID uint64) *localAudioPlaybackSession {
	t.Helper()
	backend.mu.Lock()
	defer backend.mu.Unlock()
	session := backend.sessions[sessionID]
	if session == nil {
		t.Fatalf("local playback session %d not found", sessionID)
	}
	return session
}

func localAudioPlaybackSessionExists(backend *localAudioPlaybackBackend, sessionID uint64) bool {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return backend.sessions[sessionID] != nil
}

func waitLocalAudioAdmissionBlocked(t *testing.T, blocked <-chan struct{}) {
	t.Helper()
	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for local audio admission block")
	}
}

func waitLocalAudioStart(t *testing.T, starts <-chan int) int {
	t.Helper()
	select {
	case got := <-starts:
		return got
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for local audio player start")
		return 0
	}
}

func waitLocalAudioDone(t *testing.T, done <-chan error) error {
	t.Helper()
	select {
	case err := <-done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for local playback finalization")
		return nil
	}
}

func TestLocalAudioPlaybackHelper(t *testing.T) {
	if os.Getenv("AICO_LOCAL_AUDIO_HELPER") != "1" {
		return
	}
	if os.Getenv("AICO_LOCAL_AUDIO_EXIT") == "1" {
		return
	}
	if os.Getenv("AICO_LOCAL_AUDIO_FAIL") == "1" {
		os.Exit(3)
	}
	if waitFile := os.Getenv("AICO_LOCAL_AUDIO_WAIT_FILE"); waitFile != "" {
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(waitFile); err == nil {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		os.Exit(2)
	}
	time.Sleep(5 * time.Second)
	os.Exit(0)
}

func TestLocalAudioBackendDrainReportsPlayerFailure(t *testing.T) {
	oldLookPath := localAudioLookPath
	oldCommandContext := localAudioCommandContext
	defer func() {
		localAudioLookPath = oldLookPath
		localAudioCommandContext = oldCommandContext
	}()
	localAudioLookPath = func(name string) (string, error) { return name, nil }
	localAudioCommandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=TestLocalAudioPlaybackHelper")
		cmd.Env = append(os.Environ(), "AICO_LOCAL_AUDIO_HELPER=1", "AICO_LOCAL_AUDIO_FAIL=1")
		return cmd
	}

	backend := newLocalAudioPlaybackBackend(nil)
	sessionID, err := backend.StartPlayback(tts.AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.WritePlayChunk(sessionID, []byte{1, 2}, true); err != nil {
		t.Fatal(err)
	}
	if err := backend.WaitForPlaybackDrain(context.Background()); err == nil {
		t.Fatal("WaitForPlaybackDrain() returned nil after player failure")
	}
}
