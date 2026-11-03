package agent

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"aico-agent/internal/agent/tts"
)

func TestStopPlaybackIgnoringEndedIgnoresMissingSession(t *testing.T) {
	audioServer := newTestAudioService(t)
	audioServer.stopStatus = "SESSION_NOT_FOUND"

	if err := stopPlaybackIgnoringEnded(NewAudioServiceClient(audioServer.socketPath), 77); err != nil {
		t.Fatalf("stopPlaybackIgnoringEnded() error = %v", err)
	}
}

func TestWaitForPlaybackDrainReturnsAfterPlaybackEnds(t *testing.T) {
	audioServer := newTestAudioService(t)
	audioServer.healthPlaybackSessions = []uint32{1, 1, 0}

	startedAt := time.Now()
	if err := waitForPlaybackDrain(context.Background(), NewAudioServiceClient(audioServer.socketPath), playbackDrainTimeout); err != nil {
		t.Fatalf("waitForPlaybackDrain() error = %v", err)
	}
	if audioServer.countOp("health") < 3 {
		t.Fatalf("health count = %d, want at least 3", audioServer.countOp("health"))
	}
	if elapsed := time.Since(startedAt); elapsed < 2*playbackDrainPollInterval {
		t.Fatalf("waitForPlaybackDrain returned before playback drain polling elapsed: %s", elapsed)
	}
}

func TestWaitForPlaybackDrainRetriesTransientHealthFailure(t *testing.T) {
	audioServer := newTestAudioService(t)
	audioServer.healthConnectionDrops = 1
	audioServer.healthPlaybackSessions = []uint32{1, 0}

	if err := waitForPlaybackDrain(context.Background(), NewAudioServiceClient(audioServer.socketPath), playbackDrainTimeout); err != nil {
		t.Fatalf("waitForPlaybackDrain() error = %v", err)
	}
	if audioServer.countOp("health") < 3 {
		t.Fatalf("health count = %d, want retry without consuming dropped health state", audioServer.countOp("health"))
	}
}

func TestPlayPromptSoundRetriesBusyStartPlayback(t *testing.T) {
	audioServer := newTestAudioService(t)
	audioServer.startPlaybackStatuses = []string{"SERVICE_RECOVERING", "OK"}
	audioServer.healthPlaybackSessions = []uint32{0}

	if err := playPromptSound(context.Background(), newAudioBackend(NewAudioServiceClient(audioServer.socketPath)), promptSoundRecordingStart, true); err != nil {
		t.Fatalf("playPromptSound() error = %v", err)
	}
	if got := audioServer.countOp("start_playback"); got != 2 {
		t.Fatalf("start_playback count = %d, want retry after busy failure", got)
	}
}

func TestAudioClientStartPlaybackRetriesServiceRecovering(t *testing.T) {
	audioServer := newTestAudioService(t)
	audioServer.startPlaybackStatuses = []string{"SERVICE_RECOVERING", "SERVICE_RECOVERING", "OK"}

	playback, err := NewAudioServiceClient(audioServer.socketPath).StartPlayback(AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatalf("StartPlayback() error = %v", err)
	}
	if playback.SessionID != 77 {
		t.Fatalf("sessionID = %d, want 77", playback.SessionID)
	}
	if got := audioServer.countOp("start_playback"); got != 3 {
		t.Fatalf("start_playback count = %d, want 3", got)
	}
}

func TestAudioClientStartPlaybackRetriesLongRecoveringWindow(t *testing.T) {
	audioServer := newTestAudioService(t)
	audioServer.startPlaybackStatuses = []string{
		"SERVICE_RECOVERING",
		"SERVICE_RECOVERING",
		"SERVICE_RECOVERING",
		"SERVICE_RECOVERING",
		"SERVICE_RECOVERING",
		"SERVICE_RECOVERING",
		"SERVICE_RECOVERING",
		"SERVICE_RECOVERING",
		"SERVICE_RECOVERING",
		"OK",
	}

	playback, err := NewAudioServiceClient(audioServer.socketPath).StartPlayback(AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatalf("StartPlayback() error = %v", err)
	}
	if playback.SessionID != 77 {
		t.Fatalf("sessionID = %d, want 77", playback.SessionID)
	}
	if got := audioServer.countOp("start_playback"); got != 10 {
		t.Fatalf("start_playback count = %d, want 10", got)
	}
}

func TestAudioClientStartPlaybackWaitsForPlaybackDrainOnServiceRecovering(t *testing.T) {
	audioServer := newTestAudioService(t)
	audioServer.startPlaybackStatuses = []string{"SERVICE_RECOVERING", "OK"}
	audioServer.healthPlaybackSessions = []uint32{1, 1, 0}

	startedAt := time.Now()
	playback, err := NewAudioServiceClient(audioServer.socketPath).StartPlayback(AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatalf("StartPlayback() error = %v", err)
	}
	if playback.SessionID != 77 {
		t.Fatalf("sessionID = %d, want 77", playback.SessionID)
	}
	if got := audioServer.countOp("health"); got < 3 {
		t.Fatalf("health count = %d, want at least 3", got)
	}
	if elapsed := time.Since(startedAt); elapsed < 2*playbackDrainPollInterval {
		t.Fatalf("StartPlayback() returned before waiting for playback drain: %s", elapsed)
	}
}

func TestIsTransientTTSErrorTreatsServiceRecoveringAsRetryable(t *testing.T) {
	err := &audioStatusError{op: "start_playback", status: "SERVICE_RECOVERING"}
	if !isTransientTTSError(err) {
		t.Fatal("isTransientTTSError() = false, want true for SERVICE_RECOVERING")
	}
}

func TestPlayPromptSoundDoesNotRetryNonRetryableStartPlaybackFailure(t *testing.T) {
	audioServer := newTestAudioService(t)
	audioServer.startPlaybackStatuses = []string{"INTERNAL_ERROR", "OK"}

	if err := playPromptSound(context.Background(), newAudioBackend(NewAudioServiceClient(audioServer.socketPath)), promptSoundRecordingStart, true); err == nil {
		t.Fatal("playPromptSound() error = nil, want start playback failure")
	}
	if got := audioServer.countOp("start_playback"); got != 1 {
		t.Fatalf("start_playback count = %d, want no retry for non-retryable failure", got)
	}
}

func TestPlayPromptSoundReturnsContextErrorWhenCanceledBeforeRetry(t *testing.T) {
	audioServer := newTestAudioService(t)
	audioServer.startPlaybackStatuses = []string{"SERVICE_RECOVERING", "OK"}
	firstStart := make(chan struct{})
	var once sync.Once
	audioServer.onStartPlayback = func() {
		once.Do(func() {
			close(firstStart)
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() {
		<-firstStart
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	err := playPromptSound(ctx, newAudioBackend(NewAudioServiceClient(audioServer.socketPath)), promptSoundRecordingStart, true)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("playPromptSound() error = %v, want context.Canceled", err)
	}
	if got := audioServer.countOp("start_playback"); got != 1 {
		t.Fatalf("start_playback count = %d, want no retry after cancellation", got)
	}
}

func TestPlayPromptSoundStopsPlaybackWhenCanceledDuringChunkWrite(t *testing.T) {
	backend := &blockingPromptAudioBackend{
		writeStarted: make(chan struct{}),
		releaseWrite: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- playPromptSound(ctx, backend, promptSoundAgentSend, false)
	}()

	waitForTestSignal(t, backend.writeStarted, "first prompt chunk write")
	cancel()
	close(backend.releaseWrite)

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("playPromptSound() error = %v, want context.Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("playPromptSound() did not return after cancellation")
	}

	backend.mu.Lock()
	writes := backend.writes
	finalWrites := backend.finalWrites
	stopCalls := backend.stopCalls
	backend.mu.Unlock()
	if writes != 1 {
		t.Fatalf("non-final writes = %d, want 1", writes)
	}
	if finalWrites != 0 {
		t.Fatalf("final writes = %d, want 0", finalWrites)
	}
	if stopCalls != 1 {
		t.Fatalf("StopPlayback calls = %d, want 1", stopCalls)
	}
}

type blockingPromptAudioBackend struct {
	mu           sync.Mutex
	writeOnce    sync.Once
	writeStarted chan struct{}
	releaseWrite chan struct{}
	writes       int
	finalWrites  int
	stopCalls    int
}

func (b *blockingPromptAudioBackend) StartPlayback(tts.AudioFormat) (uint64, error) {
	return 1, nil
}

func (b *blockingPromptAudioBackend) WritePlayChunk(_ uint64, _ []byte, isFinal bool) error {
	if isFinal {
		b.mu.Lock()
		b.finalWrites++
		b.mu.Unlock()
		return nil
	}
	b.mu.Lock()
	b.writes++
	b.mu.Unlock()
	b.writeOnce.Do(func() {
		close(b.writeStarted)
		<-b.releaseWrite
	})
	return nil
}

func (b *blockingPromptAudioBackend) StopPlayback(uint64) error {
	b.mu.Lock()
	b.stopCalls++
	b.mu.Unlock()
	return nil
}

func TestAudioDialogInterruptOutputStopsAgentSendPromptSound(t *testing.T) {
	audioServer := newTestAudioService(t)
	audioServer.healthPlaybackSessions = []uint32{1}
	firstStart := make(chan struct{})
	var once sync.Once
	audioServer.onStartPlayback = func() {
		once.Do(func() {
			close(firstStart)
		})
	}
	dialog := &AudioDialog{
		audioClient: NewAudioServiceClient(audioServer.socketPath),
	}

	done := make(chan struct{})
	go func() {
		dialog.playPromptSound(promptSoundAgentSend, "agent send", true)
		close(done)
	}()

	waitForTestSignal(t, firstStart, "prompt sound playback to start")
	dialog.InterruptOutput()
	waitForTestSignal(t, done, "prompt sound to stop")

	if got := audioServer.countOp("stop_playback"); got != 1 {
		t.Fatalf("stop_playback count = %d, want 1", got)
	}
}

func TestAudioDialogInterruptOutputDoesNotStopRecordingPromptSound(t *testing.T) {
	audioServer := newTestAudioService(t)
	audioServer.healthPlaybackSessions = []uint32{0}
	firstStart := make(chan struct{})
	var once sync.Once
	audioServer.onStartPlayback = func() {
		once.Do(func() {
			close(firstStart)
		})
	}
	dialog := &AudioDialog{
		audioClient: NewAudioServiceClient(audioServer.socketPath),
	}

	done := make(chan struct{})
	go func() {
		dialog.playPromptSound(promptSoundRecordingStart, "recording", true)
		close(done)
	}()

	waitForTestSignal(t, firstStart, "recording prompt sound playback to start")
	dialog.InterruptOutput()
	waitForTestSignal(t, done, "recording prompt sound to finish")

	if got := audioServer.countOp("stop_playback"); got != 0 {
		t.Fatalf("stop_playback count = %d, want recording prompt to ignore InterruptOutput", got)
	}
}

type testAudioService struct {
	socketPath               string
	listener                 net.Listener
	mu                       sync.Mutex
	ops                      []string
	stopStatus               string
	startPlaybackStatuses    []string
	onStartPlayback          func()
	healthConnectionDrops    int
	healthPlaybackSessions   []uint32
	lastHealthPlaybackStatus uint32
}

func newTestAudioService(t *testing.T) *testAudioService {
	t.Helper()
	tempDir, err := os.MkdirTemp("", "aico-tts-")
	if err != nil {
		t.Fatalf("create temp dir: %v", err)
	}
	t.Cleanup(func() {
		os.RemoveAll(tempDir)
	})
	socketPath := filepath.Join(tempDir, "audio.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen unix socket: %v", err)
	}
	s := &testAudioService{socketPath: socketPath, listener: listener, stopStatus: "OK"}
	t.Cleanup(func() {
		listener.Close()
	})
	go s.serve()
	return s
}

func (s *testAudioService) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}
		go s.handleConn(conn)
	}
}

func (s *testAudioService) handleConn(conn net.Conn) {
	defer conn.Close()
	msg, err := readUdsMessage(conn)
	if err != nil {
		return
	}
	var req audioRequest
	if err := json.Unmarshal([]byte(msg.HeaderJSON), &req); err != nil {
		return
	}
	s.mu.Lock()
	s.ops = append(s.ops, req.Op)
	stopStatus := s.stopStatus
	dropConnection := false
	var playbackSessions uint32
	if req.Op == "health" {
		if s.healthConnectionDrops > 0 {
			s.healthConnectionDrops--
			dropConnection = true
		} else {
			playbackSessions = s.nextHealthPlaybackSessionsLocked()
		}
	}
	s.mu.Unlock()
	if dropConnection {
		return
	}

	status := "OK"
	extra := map[string]interface{}{}
	if req.Op == "start_playback" {
		status = s.nextStartPlaybackStatus()
		if s.onStartPlayback != nil {
			s.onStartPlayback()
		}
		extra["session_id"] = uint64(77)
	}
	if req.Op == "start_recording" {
		extra["session_id"] = uint64(77)
	}
	if req.Op == "stop_playback" {
		status = stopStatus
	}
	if req.Op == "health" {
		extra["playback_sessions"] = playbackSessions
		extra["playback_active"] = playbackSessions > 0
		extra["record_sessions"] = uint32(0)
		extra["recording_active"] = false
	}
	resp := map[string]interface{}{"status": status}
	for k, v := range extra {
		resp[k] = v
	}
	data, _ := json.Marshal(resp)
	_ = writeUdsMessage(conn, udsMessage{HeaderJSON: string(data)})
}

func (s *testAudioService) nextStartPlaybackStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.startPlaybackStatuses) == 0 {
		return "OK"
	}
	status := s.startPlaybackStatuses[0]
	s.startPlaybackStatuses = s.startPlaybackStatuses[1:]
	return status
}

func (s *testAudioService) nextHealthPlaybackSessionsLocked() uint32 {
	if len(s.healthPlaybackSessions) > 0 {
		value := s.healthPlaybackSessions[0]
		s.healthPlaybackSessions = s.healthPlaybackSessions[1:]
		s.lastHealthPlaybackStatus = value
		return value
	}
	return s.lastHealthPlaybackStatus
}

func (s *testAudioService) countOp(op string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	count := 0
	for _, got := range s.ops {
		if got == op {
			count++
		}
	}
	return count
}
