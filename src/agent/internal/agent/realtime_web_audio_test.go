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

// A browser-started session must carry its audio over the socket in both
// directions, and the browser leaving must end the session's microphone
// stream so the session returns to standby.
func TestWebRealtimeVoiceSessionCarriesBrowserAudio(t *testing.T) {
	s := &Server{}
	activated := make(chan struct{}, 1)
	s.SetRealtimeVoiceActivation(func() { activated <- struct{}{} })
	srv := httptest.NewServer(http.HandlerFunc(s.handleRealtimeVoiceSocket))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	if got := readWebVoiceControl(t, ctx, conn)["type"]; got != "accepted" {
		t.Fatalf("first message = %v, want accepted", got)
	}
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
	s.EndRealtimeVoiceSession(voice)
	audio.Close("")
	if status := s.RealtimeVoiceStatus(); status.State != "idle" {
		t.Fatalf("status = %+v, want idle", status)
	}
}

// A browser start released before the session loop reaches it must not make
// the loop open the board microphone for its leftover wakeup.
func TestReleasedWebRealtimeVoiceStartOpensNoSession(t *testing.T) {
	s := &Server{}
	s.SetRealtimeVoiceActivation(func() {})
	srv := httptest.NewServer(http.HandlerFunc(s.handleRealtimeVoiceSocket))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(srv.URL, "http"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := readWebVoiceControl(t, ctx, conn)["type"]; got != "accepted" {
		t.Fatalf("first message = %v, want accepted", got)
	}
	conn.Close(websocket.StatusNormalClosure, "")
	for s.RealtimeVoicePending() {
		if ctx.Err() != nil {
			t.Fatal("browser disconnect did not withdraw the queued start")
		}
		time.Sleep(time.Millisecond)
	}

	if voice := s.ClaimRealtimeVoiceSession(Config{}, false); voice != nil {
		t.Fatalf("claimed a %s session for a withdrawn browser start", voice.Route())
	}
	if status := s.RealtimeVoiceStatus(); status.State != "idle" {
		t.Fatalf("status = %+v, want idle", status)
	}
	// A withdrawn browser start leaves nothing behind that would swallow a
	// later device activation.
	if voice := s.ClaimRealtimeVoiceSession(Config{}, true); voice == nil || voice.Route() != RealtimeAudioRouteDevice {
		t.Fatalf("device activation after a withdrawn browser start claimed %+v, want a device session", voice)
	}
}

// A browser endpoint that has closed but is not yet released must not make a
// claim fall through to the device: the browser asked for its own audio.
func TestClosedWebRealtimeVoiceStartDoesNotFallBackToDevice(t *testing.T) {
	s := &Server{}
	s.SetRealtimeVoiceActivation(func() {})
	web := &webRealtimeAudio{mic: make(chan []byte, 1), done: make(chan struct{})}
	if err := s.startWebRealtimeVoice(web); err != nil {
		t.Fatal(err)
	}
	web.shutdown() // Disconnected; releaseWebRealtimeVoice has not run yet.
	if voice := s.ClaimRealtimeVoiceSession(Config{}, false); voice != nil {
		t.Fatalf("claimed a %s session for a closed browser endpoint", voice.Route())
	}
}

// A browser session the user stopped must not carry over a provider
// rotation into a new session that has no stop request.
func TestStoppedWebRealtimeVoiceIsNotRequeued(t *testing.T) {
	s := &Server{}
	s.SetRealtimeVoiceActivation(func() {})
	web := &webRealtimeAudio{mic: make(chan []byte, 1), done: make(chan struct{})}
	if err := s.startWebRealtimeVoice(web); err != nil {
		t.Fatal(err)
	}
	voice := s.ClaimRealtimeVoiceSession(Config{}, false)
	if voice == nil {
		t.Fatal("browser session was not claimed")
	}
	s.stopRealtimeVoice()
	s.EndRealtimeVoiceSession(voice)
	if s.RequeueRealtimeAudioIO(voice) {
		t.Fatal("a stopped browser session was requeued for the rotated session")
	}
	if s.RealtimeVoicePending() {
		t.Fatal("a stopped browser session is still waiting to start")
	}
}
