package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/tts"
	"nhooyr.io/websocket"
)

// dialWebVoice opens the voice socket against a server that serves only it.
func dialWebVoice(t *testing.T, s *Server) (context.Context, *websocket.Conn) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(s.handleRealtimeVoiceSocket))
	t.Cleanup(srv.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })
	return ctx, conn
}

func readWebVoiceControl(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	kind, data, err := conn.Read(ctx)
	if err != nil {
		t.Fatalf("read control message: %v", err)
	}
	if kind != websocket.MessageText {
		t.Fatalf("message kind = %v, want text", kind)
	}
	var message map[string]any
	if err := json.Unmarshal(data, &message); err != nil {
		t.Fatalf("decode control message %q: %v", data, err)
	}
	return message
}

func waitWebVoice(t *testing.T, ctx context.Context, what string, ok func() bool) {
	t.Helper()
	for !ok() {
		if ctx.Err() != nil {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

// A browser-started session must carry its audio over the socket in both
// directions, and the browser leaving must end the session's microphone
// stream so the session returns to standby.
func TestWebRealtimeVoiceSessionCarriesBrowserAudio(t *testing.T) {
	s := &Server{}
	activated := make(chan struct{}, 1)
	s.SetRealtimeVoiceActivation(func() { activated <- struct{}{} })
	ctx, conn := dialWebVoice(t, s)
	select {
	case <-activated:
	case <-ctx.Done():
		t.Fatal("browser start did not wake the session loop")
	}
	if status := s.RealtimeVoiceStatus(); status.State != "connecting" || status.Route != RealtimeAudioRouteWeb {
		t.Fatalf("status = %+v, want a connecting web session", status)
	}

	voice := s.ClaimRealtimeVoiceSession(Config{}, false)
	if voice == nil || voice.Route() != RealtimeAudioRouteWeb {
		t.Fatalf("claimed %+v, want the browser session", voice)
	}
	audio := voice.Audio()
	voice.MarkReady()
	if status := s.RealtimeVoiceStatus(); status.State != "active" || status.Route != RealtimeAudioRouteWeb {
		t.Fatalf("status = %+v, want an active web session", status)
	}

	// Muting one side must leave the other unchanged.
	if !s.setRealtimeVoiceMuted(nil, boolPtr(true)) || !voice.SpeakerMuted() || voice.Muted() {
		t.Fatalf("speaker mute: speaker=%t microphone=%t, want only the speaker muted", voice.SpeakerMuted(), voice.Muted())
	}
	if status := s.RealtimeVoiceStatus(); !status.SpeakerMuted || status.Muted {
		t.Fatalf("status = %+v, want only the speaker muted", status)
	}

	recording := audio.Recording()
	if _, err := recording.StartRecording(AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}); err != nil {
		t.Fatal(err)
	}
	start := readWebVoiceControl(t, ctx, conn)
	if format, _ := start["format"].(map[string]any); start["type"] != "capture_start" || format["sample_rate"] != float64(16000) {
		t.Fatalf("capture start = %v", start)
	}
	if err := conn.Write(ctx, websocket.MessageBinary, []byte{1, 2, 3, 4}); err != nil {
		t.Fatal(err)
	}
	chunk, err := recording.ReadRecordChunk(1, 2000)
	if err != nil || string(chunk.PCM) != string([]byte{1, 2, 3, 4}) {
		t.Fatalf("microphone chunk = %+v, %v", chunk, err)
	}

	playback := audio.Playback()
	id, err := playback.StartPlayback(tts.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatal(err)
	}
	if got := readWebVoiceControl(t, ctx, conn)["type"]; got != "playback_start" {
		t.Fatalf("message = %v, want playback_start", got)
	}
	if err := playback.WritePlayChunk(id, []byte{5, 6}, false); err != nil {
		t.Fatal(err)
	}
	kind, data, err := conn.Read(ctx)
	if err != nil || kind != websocket.MessageBinary || string(data) != string([]byte{5, 6}) {
		t.Fatalf("speech frame = %v %v, %v", kind, data, err)
	}
	// Speech that reached the browser drains once it has had time to play.
	if err := playback.(interface {
		WaitForPlaybackDrain(context.Context) error
	}).WaitForPlaybackDrain(ctx); err != nil {
		t.Fatalf("drain after delivered speech: %v", err)
	}

	conn.Close(websocket.StatusNormalClosure, "")
	select {
	case <-voice.StopRequested():
	case <-ctx.Done():
		t.Fatal("browser disconnect did not stop the session")
	}
	for {
		chunk, err := recording.ReadRecordChunk(1, 200)
		if err != nil {
			t.Fatal(err)
		}
		if chunk.EndOfStream {
			break
		}
		if ctx.Err() != nil {
			t.Fatal("microphone stream did not end after the browser left")
		}
	}
	s.EndRealtimeVoiceSession(voice, "")
	if status := s.RealtimeVoiceStatus(); status.State != "idle" {
		t.Fatalf("status = %+v, want idle", status)
	}
}

// A browser start withdrawn before the session loop reaches it must not make
// the loop open the board microphone for its leftover wakeup, and must leave
// nothing behind that would swallow a later device activation.
func TestWithdrawnWebRealtimeVoiceStartOpensNoSession(t *testing.T) {
	s := &Server{}
	s.SetRealtimeVoiceActivation(func() {})
	ctx, conn := dialWebVoice(t, s)
	waitWebVoice(t, ctx, "the queued start", s.RealtimeVoicePending)
	conn.Close(websocket.StatusNormalClosure, "")
	waitWebVoice(t, ctx, "the start to be withdrawn", func() bool { return !s.RealtimeVoicePending() })

	if voice := s.ClaimRealtimeVoiceSession(Config{}, false); voice != nil {
		t.Fatalf("claimed a %s session for a withdrawn browser start", voice.Route())
	}
	if status := s.RealtimeVoiceStatus(); status.State != "idle" {
		t.Fatalf("status = %+v, want idle", status)
	}
	if voice := s.ClaimRealtimeVoiceSession(Config{}, true); voice == nil || voice.Route() != RealtimeAudioRouteDevice {
		t.Fatalf("device activation after a withdrawn browser start claimed %+v, want a device session", voice)
	}
}

// A browser endpoint that has closed but is not yet released must not make a
// claim fall through to the device: the browser asked for its own audio.
func TestClosedWebRealtimeVoiceStartDoesNotFallBackToDevice(t *testing.T) {
	s := &Server{}
	s.SetRealtimeVoiceActivation(func() {})
	web := newWebRealtimeAudio(nil)
	if err := s.startWebRealtimeVoice(web); err != nil {
		t.Fatal(err)
	}
	web.shutdown("") // Disconnected; releaseWebRealtimeVoice has not run yet.
	if voice := s.ClaimRealtimeVoiceSession(Config{}, false); voice != nil {
		t.Fatalf("claimed a %s session for a closed browser endpoint", voice.Route())
	}
}

// A session that was asked to stop is over for the Web UI at once, even
// though the loop is still tearing it down: the status is idle and a new
// browser start queues behind it instead of being refused as busy.
func TestStoppedRealtimeVoiceSessionYieldsToNewBrowserStart(t *testing.T) {
	s := &Server{}
	s.SetRealtimeVoiceActivation(func() {})
	voice := s.ClaimRealtimeVoiceSession(Config{}, true)
	if status := s.RealtimeVoiceStatus(); status.State != "connecting" || status.Route != RealtimeAudioRouteDevice {
		t.Fatalf("status = %+v, want a connecting device session", status)
	}
	s.stopRealtimeVoice()
	if !voice.Stopped() {
		t.Fatal("stop did not reach the running session")
	}
	if status := s.RealtimeVoiceStatus(); status.State != "idle" {
		t.Fatalf("status after stop = %+v, want idle", status)
	}
	if s.setRealtimeVoiceMuted(boolPtr(true), nil) {
		t.Fatal("a stopped session still accepted a mute")
	}

	ctx, _ := dialWebVoice(t, s)
	waitWebVoice(t, ctx, "the new browser start to queue", s.RealtimeVoicePending)
	if status := s.RealtimeVoiceStatus(); status.State != "connecting" || status.Route != RealtimeAudioRouteWeb {
		t.Fatalf("status = %+v, want the new browser start connecting", status)
	}
	// The old session's teardown must not take the new start with it.
	s.EndRealtimeVoiceSession(voice, "")
	if next := s.ClaimRealtimeVoiceSession(Config{}, false); next == nil || next.Route() != RealtimeAudioRouteWeb {
		t.Fatalf("claimed %+v, want the new browser session", next)
	}
}

// A browser that stops reading must not stall the session loop: playback
// writes return at once, and the session's microphone stream ends once the
// backlog is full.
func TestStalledBrowserDoesNotBlockRealtimePlayback(t *testing.T) {
	s := &Server{}
	s.SetRealtimeVoiceActivation(func() {})
	ctx, _ := dialWebVoice(t, s) // Never read from: the browser has stalled.
	waitWebVoice(t, ctx, "the queued start", s.RealtimeVoicePending)
	voice := s.ClaimRealtimeVoiceSession(Config{}, false)
	if voice == nil {
		t.Fatal("browser session was not claimed")
	}
	playback := voice.Audio().Playback()
	id, err := playback.StartPlayback(tts.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatal(err)
	}
	// More speech than the socket buffers and the backlog can hold.
	chunk := make([]byte, 64<<10)
	started := time.Now()
	for i := 0; i < 2*webRealtimeSendQueue; i++ {
		if err := playback.WritePlayChunk(id, chunk, false); err != nil {
			t.Fatal(err)
		}
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("playback writes to a stalled browser took %s; the session loop would stall", elapsed)
	}
	end, err := voice.Audio().Recording().ReadRecordChunk(1, 2000)
	if err != nil || !end.EndOfStream {
		t.Fatalf("microphone read = %+v, %v; want the stream ended for a browser that cannot keep up", end, err)
	}
}

// A reconnected provider session captures again on the same endpoint. The
// reader of the earlier capture must end instead of taking microphone audio
// meant for the new one.
func TestWebRealtimeRecordingEndsReaderOfEarlierCapture(t *testing.T) {
	web := newWebRealtimeAudio(nil)
	recording := web.Recording()
	format := AudioFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}
	first, err := recording.StartRecording(format)
	if err != nil {
		t.Fatal(err)
	}
	if err := recording.StopRecording(first.SessionID); err != nil {
		t.Fatal(err)
	}
	second, err := recording.StartRecording(format)
	if err != nil {
		t.Fatal(err)
	}
	web.mic <- []byte{1, 2}
	if chunk, err := recording.ReadRecordChunk(first.SessionID, 1000); err != nil || !chunk.EndOfStream {
		t.Fatalf("read on the earlier capture = %+v, %v; want the end of its stream", chunk, err)
	}
	if chunk, err := recording.ReadRecordChunk(second.SessionID, 1000); err != nil || string(chunk.PCM) != string([]byte{1, 2}) {
		t.Fatalf("read on the new capture = %+v, %v; want the microphone frame", chunk, err)
	}
}

// A playback drain confirms that speech reached the listener. It must wait
// for speech still queued for a slow browser, and fail when the browser
// leaves before that speech was written.
func TestWebRealtimePlaybackDrainWaitsForUnsentSpeech(t *testing.T) {
	// No writer runs, as when the browser has stopped taking data.
	web := newWebRealtimeAudio(nil)
	playback := web.Playback()
	id, err := playback.StartPlayback(tts.AudioFormat{SampleRate: 24000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatal(err)
	}
	if err := playback.WritePlayChunk(id, make([]byte, 480), false); err != nil {
		t.Fatal(err)
	}
	waiter := playback.(interface {
		WaitForPlaybackDrain(context.Context) error
	})
	// The speech is 10 ms long, so its play time has long passed.
	short, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	if err := waiter.WaitForPlaybackDrain(short); err == nil {
		t.Fatal("the drain completed while speech was still waiting to be written")
	}
	drained := make(chan error, 1)
	go func() { drained <- waiter.WaitForPlaybackDrain(context.Background()) }()
	web.shutdown("") // The browser leaves with the speech unsent.
	select {
	case err := <-drained:
		if err == nil {
			t.Fatal("the drain succeeded although the browser left before the speech was written")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the drain did not end after the browser left")
	}
	// Closing the endpoint discards the queued speech. A drain that starts
	// only afterwards must still fail.
	web.Close("")
	if err := waiter.WaitForPlaybackDrain(context.Background()); err == nil {
		t.Fatal("a drain after the endpoint closed reported discarded speech as played")
	}

	// Speech submitted only after the endpoint closed is dropped, and a drain
	// must not report it as played either.
	late := newWebRealtimeAudio(nil)
	late.Close("")
	latePlayback := late.Playback()
	if err := latePlayback.WritePlayChunk(1, make([]byte, 480), false); err != nil {
		t.Fatal(err)
	}
	if err := latePlayback.(interface {
		WaitForPlaybackDrain(context.Context) error
	}).WaitForPlaybackDrain(context.Background()); err == nil {
		t.Fatal("a drain reported speech submitted after the endpoint closed as played")
	}
}
