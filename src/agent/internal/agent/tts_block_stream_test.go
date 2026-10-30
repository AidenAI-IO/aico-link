package agent

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"aico-agent/internal/agent/speech"
	"aico-agent/internal/agent/tts"
)

func TestTTSBlockStreamReplacesToolSpeechBeforeNextResponse(t *testing.T) {
	provider := &blockRecordingProvider{}
	backend := &blockRecordingPlayback{}
	var latest latestTTSOutput
	stream, err := beginManagedTTSBlockStream(context.Background(), tts.NewProviderManager(provider, nil), backend, Config{}, &latest)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.interrupt()
	writer := speech.NewStreamWriter(stream)
	if _, err := writer.Write([]byte("<tts>Checking the weather.</tts>Visible tool progress.")); err != nil {
		t.Fatal(err)
	}
	if !writer.FinishResponse() {
		t.Fatal("tool speech was not streamed")
	}
	first := provider.session(0)
	if first.ctx.Err() != nil {
		t.Fatal("tool response completion cut off its speech")
	}
	if _, err := writer.Write([]byte("<tts>It is sunny.</tts>Visible final reply.")); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(first.ctx.Err(), context.Canceled) {
		t.Fatal("new TTS block did not cancel previous synthesis")
	}
	if got := backend.operations(); !reflect.DeepEqual(got, []string{"start", "stop", "start"}) {
		t.Fatalf("playback operations = %v, want old playback stopped before new playback", got)
	}
	if err := first.sink.WritePCM(make([]byte, testTTSPlaybackStartPCMBytes)); !errors.Is(err, tts.ErrSessionClosed) {
		t.Fatalf("late old PCM error = %v, want closed sink", err)
	}
	if got := provider.texts(); !reflect.DeepEqual(got, []string{"Checking the weather.", "It is sunny."}) {
		t.Fatalf("spoken text = %v", got)
	}
	if !writer.FinalizeResponse() {
		t.Fatal("final speech was not streamed")
	}
	if err := stream.closeAndWait(); err != nil {
		t.Fatal(err)
	}
}

func TestTTSBlockStreamReplacesConsecutiveTags(t *testing.T) {
	provider := &blockRecordingProvider{}
	backend := &blockRecordingPlayback{}
	var latest latestTTSOutput
	stream, err := beginManagedTTSBlockStream(context.Background(), tts.NewProviderManager(provider, nil), backend, Config{}, &latest)
	if err != nil {
		t.Fatal(err)
	}
	defer stream.interrupt()
	writer := speech.NewStreamWriter(stream)
	_, err = writer.Write([]byte("<tts>First.</tts><tt"))
	if err != nil {
		t.Fatal(err)
	}
	// The parser must recognize an opening tag split across transport chunks.
	if len(provider.texts()) != 1 {
		t.Fatal("partial tag started another provider session")
	}
	_, _ = writer.Write([]byte("s>Second.</tts><tts>Third.</tts>"))
	if got := provider.texts(); !reflect.DeepEqual(got, []string{"First.", "Second.", "Third."}) {
		t.Fatalf("spoken text = %v", got)
	}
	if got := backend.operations(); !reflect.DeepEqual(got, []string{"start", "stop", "start", "stop", "start"}) {
		t.Fatalf("playback operations = %v", got)
	}
}

func TestLatestTTSOutputCancelsReservedToolSpeechBeforeItStarts(t *testing.T) {
	var latest latestTTSOutput
	oldCtx, oldOutput, releaseOld := latest.begin(context.Background())
	defer releaseOld()
	newCtx, _, releaseNew := latest.begin(context.Background())
	defer releaseNew()
	if !errors.Is(oldCtx.Err(), context.Canceled) || newCtx.Err() != nil {
		t.Fatal("latest speech did not supersede the pending older task")
	}
	oldSession := &abortableStreamSession{}
	oldOutput.setStream(&streamSessionWriter{session: oldSession})
	if oldSession.abortCalls != 1 {
		t.Fatal("late stream registration was not interrupted")
	}
	_, reused, cleanup := latest.acquire(oldCtx)
	defer cleanup()
	if reused != oldOutput || newCtx.Err() != nil {
		t.Fatal("stale goroutine claimed latest output")
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, _, cleanupCanceled := latest.begin(canceled)
	defer cleanupCanceled()
	if newCtx.Err() != nil {
		t.Fatal("canceled speech interrupted the current output")
	}
}

func TestTTSBlockStreamMarksSupersededSpeechWithoutAudio(t *testing.T) {
	provider := &blockRecordingProvider{noPCM: true}
	backend := &blockRecordingPlayback{}
	var latest latestTTSOutput
	manager := tts.NewProviderManager(provider, nil)
	old, err := beginManagedTTSBlockStream(context.Background(), manager, backend, Config{}, &latest)
	if err != nil {
		t.Fatal(err)
	}
	defer old.interrupt()
	writer := speech.NewStreamWriter(old)
	if _, err := writer.Write([]byte("<tts>Earlier progress.</tts>")); err != nil {
		t.Fatal(err)
	}
	if !writer.FinishResponse() {
		t.Fatal("old speech block was not accepted")
	}
	newer, err := beginManagedTTSBlockStream(context.Background(), manager, backend, Config{}, &latest)
	if err != nil {
		t.Fatal(err)
	}
	defer newer.interrupt()
	newer.BeginSpeechBlock()
	if !old.wasSuperseded() {
		t.Fatal("old response was not marked as superseded before fallback selection")
	}
	if old.emittedSpeech(nil) {
		t.Fatal("test requires no old PCM so fallback would otherwise be selected")
	}
}

func TestServerFinalSpeechInterruptsPendingToolSpeech(t *testing.T) {
	testFinalSpeechInterruptsPendingToolSpeech(t, false)
}

func TestAudioDialogFinalSpeechInterruptsPendingToolSpeech(t *testing.T) {
	testFinalSpeechInterruptsPendingToolSpeech(t, true)
}

func testFinalSpeechInterruptsPendingToolSpeech(t *testing.T, useDialog bool) {
	t.Helper()
	provider := &blockRecordingProvider{blockClose: true, started: make(chan struct{}, 2)}
	server := &Server{ttsManager: tts.NewProviderManager(provider, nil), ttsPlaybackBackend: &blockRecordingPlayback{}}
	speak := func(ctx context.Context, text string) error { return server.speakText(ctx, text, 0) }
	if useDialog {
		dialog := &AudioDialog{ttsManager: server.ttsManager, ttsPlaybackBackend: server.ttsPlaybackBackend}
		speak = func(ctx context.Context, text string) error { return dialog.Speak(ctx, text, nil) }
	}
	firstDone := make(chan error, 1)
	go func() { firstDone <- speak(context.Background(), "Tool progress.") }()
	waitForTestSignal(t, provider.started, "first tool playback")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	secondDone := make(chan error, 1)
	go func() { secondDone <- speak(ctx, "Final answer.") }()
	waitForTestSignal(t, provider.started, "replacement playback without waiting for old drain")
	select {
	case <-firstDone:
	case <-time.After(time.Second):
		t.Fatal("old playback did not stop")
	}
	if !errors.Is(provider.session(0).ctx.Err(), context.Canceled) {
		t.Fatal("tool synthesis was not canceled")
	}
	cancel()
	select {
	case <-secondDone:
	case <-time.After(time.Second):
		t.Fatal("replacement playback did not honor request cancellation")
	}
}

type blockRecordingProvider struct {
	mu         sync.Mutex
	sessions   []*blockRecordingSession
	blockClose bool
	noPCM      bool
	started    chan struct{}
}

func (p *blockRecordingProvider) Name() string                   { return "block-test" }
func (p *blockRecordingProvider) Capabilities() tts.Capabilities { return tts.Capabilities{} }
func (p *blockRecordingProvider) Close() error                   { return nil }
func (p *blockRecordingProvider) BeginStream(ctx context.Context, sink tts.AudioSink) (tts.StreamSession, error) {
	session := &blockRecordingSession{ctx: ctx, sink: sink, provider: p}
	p.mu.Lock()
	p.sessions = append(p.sessions, session)
	p.mu.Unlock()
	return session, nil
}
func (p *blockRecordingProvider) session(i int) *blockRecordingSession {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.sessions[i]
}
func (p *blockRecordingProvider) texts() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var result []string
	for _, session := range p.sessions {
		result = append(result, session.text)
	}
	return result
}

type blockRecordingSession struct {
	ctx      context.Context
	sink     tts.AudioSink
	provider *blockRecordingProvider
	text     string
}

func (s *blockRecordingSession) WriteText(text string) error {
	s.provider.mu.Lock()
	s.text += text
	noPCM := s.provider.noPCM
	s.provider.mu.Unlock()
	if noPCM {
		return nil
	}
	if err := s.sink.WritePCM(make([]byte, testTTSPlaybackStartPCMBytes)); err != nil {
		return err
	}
	if s.provider.started != nil {
		s.provider.started <- struct{}{}
	}
	return nil
}
func (s *blockRecordingSession) Flush() error { return nil }
func (s *blockRecordingSession) Close() error {
	if s.provider.blockClose {
		<-s.ctx.Done()
		return s.ctx.Err()
	}
	return nil
}
func (s *blockRecordingSession) Abort() error { return nil }
func (s *blockRecordingSession) Err() error   { return nil }

type blockRecordingPlayback struct {
	mu  sync.Mutex
	ops []string
}

func (b *blockRecordingPlayback) StartPlayback(tts.AudioFormat) (uint64, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ops = append(b.ops, "start")
	return uint64(len(b.ops)), nil
}
func (b *blockRecordingPlayback) WritePlayChunk(uint64, []byte, bool) error { return nil }
func (b *blockRecordingPlayback) StopPlayback(uint64) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.ops = append(b.ops, "stop")
	return nil
}
func (b *blockRecordingPlayback) operations() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.ops...)
}
