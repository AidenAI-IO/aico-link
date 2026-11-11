package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"aico-agent/internal/agent/tts"
	"nhooyr.io/websocket"
)

// Browser media protocol on /api/realtime/voice/ws.
//
// Binary frames carry signed 16-bit little-endian PCM: from the browser, its
// microphone at the capture format; to the browser, response speech at the
// playback format. Text frames carry JSON control messages from the agent:
//
//	{"type":"accepted"}                         the start is queued
//	{"type":"capture_start","format":{...}}     send microphone PCM in format
//	{"type":"capture_stop"}                     stop sending microphone PCM
//	{"type":"playback_start","format":{...}}    following PCM uses format
//	{"type":"playback_stop"}                    drop queued speech (barge-in)
//	{"type":"ended","reason":"..."}             the session is over
//
// Closing the socket ends the session.
const (
	webRealtimeMicQueue       = 32
	webRealtimeWriteTimeout   = 2 * time.Second
	webRealtimeDrainMargin    = 250 * time.Millisecond
	webRealtimeMaxMessageSize = 1 << 20
)

type webRealtimeAudio struct {
	conn    *websocket.Conn
	mic     chan []byte
	done    chan struct{}
	once    sync.Once
	writeMu sync.Mutex

	mu         sync.Mutex
	capturing  bool
	playbackID uint64
	bytesPerS  int64
	playEnd    time.Time
}

func newWebRealtimeAudio(conn *websocket.Conn) *webRealtimeAudio {
	return &webRealtimeAudio{conn: conn, mic: make(chan []byte, webRealtimeMicQueue), done: make(chan struct{})}
}

func (w *webRealtimeAudio) Route() RealtimeAudioRoute { return RealtimeAudioRouteWeb }

func (w *webRealtimeAudio) Recording() AudioRecordingBackend { return webRealtimeRecording{w} }

func (w *webRealtimeAudio) Playback() tts.AudioServiceBackend { return webRealtimePlayback{w} }

// Close tells the browser why the session ended and closes the socket.
func (w *webRealtimeAudio) Close(reason string) {
	w.send(map[string]string{"type": "ended", "reason": reason})
	w.shutdown()
	_ = w.conn.Close(websocket.StatusNormalClosure, "")
}

func (w *webRealtimeAudio) closed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

func (w *webRealtimeAudio) shutdown() {
	w.once.Do(func() { close(w.done) })
}

func (w *webRealtimeAudio) send(message any) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	w.write(websocket.MessageText, data)
}

// write drops output once the browser is gone: the session then ends through
// the end of its microphone stream instead of a playback error.
func (w *webRealtimeAudio) write(kind websocket.MessageType, data []byte) {
	if w.closed() {
		return
	}
	w.writeMu.Lock()
	defer w.writeMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), webRealtimeWriteTimeout)
	defer cancel()
	if err := w.conn.Write(ctx, kind, data); err != nil {
		w.shutdown()
	}
}

// serve reads microphone frames until the browser disconnects.
func (w *webRealtimeAudio) serve(ctx context.Context) {
	defer w.shutdown()
	for {
		kind, data, err := w.conn.Read(ctx)
		if err != nil {
			return
		}
		if kind != websocket.MessageBinary || len(data) < 2 {
			continue
		}
		w.mu.Lock()
		capturing := w.capturing
		w.mu.Unlock()
		if !capturing {
			continue
		}
		pcm := data[:len(data)&^1]
		select {
		case w.mic <- pcm:
		default:
			// The session fell behind; keep the newest audio.
			select {
			case <-w.mic:
			default:
			}
			select {
			case w.mic <- pcm:
			default:
			}
		}
	}
}

type webRealtimeFormat struct {
	SampleRate uint32 `json:"sample_rate"`
	Channels   uint32 `json:"channels"`
	BitWidth   uint32 `json:"bit_width"`
}

type webRealtimeRecording struct{ w *webRealtimeAudio }

func (r webRealtimeRecording) StartRecording(format AudioFormat) (*RecordStartResult, error) {
	r.w.mu.Lock()
	r.w.capturing = true
	r.w.mu.Unlock()
	r.w.send(map[string]any{"type": "capture_start", "format": webRealtimeFormat(format)})
	return &RecordStartResult{SessionID: 1}, nil
}

// ReadRecordChunk returns an empty chunk when the browser sent nothing within
// the timeout, and the end of the stream once it disconnected.
func (r webRealtimeRecording) ReadRecordChunk(_ uint64, timeoutMs uint32) (*AudioChunkResult, error) {
	timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case pcm := <-r.w.mic:
		return &AudioChunkResult{PCM: pcm}, nil
	case <-r.w.done:
		return &AudioChunkResult{EndOfStream: true}, nil
	case <-timer.C:
		return &AudioChunkResult{}, nil
	}
}

func (r webRealtimeRecording) StopRecording(uint64) error {
	r.w.mu.Lock()
	r.w.capturing = false
	r.w.mu.Unlock()
	r.w.send(map[string]string{"type": "capture_stop"})
	return nil
}

type webRealtimePlayback struct{ w *webRealtimeAudio }

func (p webRealtimePlayback) StartPlayback(format tts.AudioFormat) (uint64, error) {
	w := p.w
	w.mu.Lock()
	w.playbackID++
	id := w.playbackID
	w.bytesPerS = int64(format.SampleRate) * int64(format.Channels) * int64(format.BitWidth) / 8
	w.playEnd = time.Time{}
	w.mu.Unlock()
	w.send(map[string]any{"type": "playback_start", "format": webRealtimeFormat{
		SampleRate: uint32(format.SampleRate), Channels: uint32(format.Channels), BitWidth: uint32(format.BitWidth),
	}})
	return id, nil
}

// WritePlayChunk forwards speech to the browser. Empty writes are the
// session's keep-alive and finalize markers, which the browser does not need.
func (p webRealtimePlayback) WritePlayChunk(_ uint64, data []byte, _ bool) error {
	if len(data) == 0 {
		return nil
	}
	w := p.w
	w.mu.Lock()
	if w.bytesPerS > 0 {
		now := time.Now()
		if w.playEnd.Before(now) {
			w.playEnd = now
		}
		w.playEnd = w.playEnd.Add(time.Duration(int64(len(data)) * int64(time.Second) / w.bytesPerS))
	}
	w.mu.Unlock()
	w.write(websocket.MessageBinary, data)
	return nil
}

func (p webRealtimePlayback) StopPlayback(uint64) error {
	w := p.w
	w.mu.Lock()
	w.playEnd = time.Time{}
	w.mu.Unlock()
	w.send(map[string]string{"type": "playback_stop"})
	return nil
}

// WaitForPlaybackDrain waits until the browser has had time to play what was
// sent. The browser schedules PCM back to back as it arrives, so the
// real-time length of the written audio bounds when it finishes.
func (p webRealtimePlayback) WaitForPlaybackDrain(ctx context.Context) error {
	w := p.w
	for {
		w.mu.Lock()
		wait := time.Until(w.playEnd.Add(webRealtimeDrainMargin))
		w.mu.Unlock()
		if wait <= 0 {
			return nil
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-w.done:
			timer.Stop()
			return nil
		case <-timer.C:
		}
	}
}

// handleRealtimeVoiceSocket serves GET /api/realtime/voice/ws: a browser
// starts a realtime voice session that uses its own microphone and speaker.
func (s *Server) handleRealtimeVoiceSocket(w http.ResponseWriter, r *http.Request) {
	// The socket outlives the server's request timeouts.
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Time{})
	_ = controller.SetWriteDeadline(time.Time{})
	// Origin verification stays on: only the Web UI's own origin may connect.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(webRealtimeMaxMessageSize)
	web := newWebRealtimeAudio(conn)
	if err := s.startWebRealtimeVoice(web); err != nil {
		web.Close(err.Error())
		return
	}
	web.send(map[string]string{"type": "accepted"})
	web.serve(context.Background())
	s.releaseWebRealtimeVoice(web)
	web.Close("")
}
