package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"
	"time"

	"aico-agent/internal/agent/tts"
	"aico-agent/internal/logging"
	"nhooyr.io/websocket"
)

// Browser media protocol on /api/realtime/voice/ws.
//
// Binary frames carry signed 16-bit little-endian PCM: from the browser, its
// microphone at the capture format; to the browser, response speech at the
// playback format. Text frames carry JSON control messages from the agent:
//
//	{"type":"capture_start","format":{...}}     send microphone PCM in format
//	{"type":"capture_stop"}                     stop sending microphone PCM
//	{"type":"playback_start","format":{...}}    following PCM uses format
//	{"type":"playback_stop"}                    drop queued speech (barge-in)
//	{"type":"ended","reason":"..."}             the session is over
//
// Closing the socket ends the session.
const (
	webRealtimeMicQueue = 32
	// webRealtimeSendQueue bounds the frames waiting for a slow browser. A
	// reply arrives from the provider faster than real time, so a brief stall
	// on Wi-Fi must fit: at 4 KiB of 24 kHz speech per frame this is about
	// 40 seconds of audio.
	webRealtimeSendQueue = 512
	// webRealtimeWriteTimeout is how long one frame may take to reach the
	// browser before the connection counts as lost.
	webRealtimeWriteTimeout   = 10 * time.Second
	webRealtimeDrainMargin    = 250 * time.Millisecond
	webRealtimeMaxMessageSize = 1 << 20
)

// webRealtimeFrame is one outgoing message. A nil data closes the socket.
type webRealtimeFrame struct {
	kind websocket.MessageType
	data []byte
}

// webRealtimeAudio is the audio endpoint of a browser voice session. The
// session loop never waits on the network: outgoing frames are queued and
// written by run, and microphone frames are queued by run's reader.
type webRealtimeAudio struct {
	conn *websocket.Conn
	mic  chan []byte
	// done is closed once the browser can no longer take part: it
	// disconnected, stopped keeping up, or the endpoint was closed.
	done     chan struct{}
	doneOnce sync.Once
	wake     chan struct{}

	mu         sync.Mutex
	capturing  bool
	closing    bool
	out        []webRealtimeFrame
	playbackID uint64
	bytesPerS  int64
	playEnd    time.Time
}

func newWebRealtimeAudio(conn *websocket.Conn) *webRealtimeAudio {
	return &webRealtimeAudio{
		conn: conn,
		mic:  make(chan []byte, webRealtimeMicQueue),
		done: make(chan struct{}),
		wake: make(chan struct{}, 1),
	}
}

func (w *webRealtimeAudio) Route() RealtimeAudioRoute { return RealtimeAudioRouteWeb }

func (w *webRealtimeAudio) Recording() AudioRecordingBackend { return webRealtimeRecording{w} }

func (w *webRealtimeAudio) Playback() tts.AudioServiceBackend { return webRealtimePlayback{w} }

// Close tells the browser why the session ended and closes the socket once
// that message is written. It does not wait for the network.
func (w *webRealtimeAudio) Close(reason string) {
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		return
	}
	w.closing = true
	ended, _ := json.Marshal(map[string]string{"type": "ended", "reason": reason})
	// Speech still queued is not worth sending to a session that is over.
	w.out = []webRealtimeFrame{{kind: websocket.MessageText, data: ended}, {}}
	w.mu.Unlock()
	w.shutdown("")
	w.signal()
}

func (w *webRealtimeAudio) closed() bool {
	select {
	case <-w.done:
		return true
	default:
		return false
	}
}

// shutdown marks the browser as gone. cause is logged when the browser or the
// network ended the connection, and is empty when the agent did.
func (w *webRealtimeAudio) shutdown(cause string) {
	w.doneOnce.Do(func() {
		if cause != "" {
			logging.Infof("agent", "realtime", "Browser voice connection closed: %s", cause)
		}
		close(w.done)
	})
}

func (w *webRealtimeAudio) signal() {
	select {
	case w.wake <- struct{}{}:
	default:
	}
}

func (w *webRealtimeAudio) send(message any) {
	data, err := json.Marshal(message)
	if err != nil {
		return
	}
	w.enqueue(webRealtimeFrame{kind: websocket.MessageText, data: data})
}

// enqueue queues a frame for the browser. Output for a browser that is gone
// is dropped: the session then ends through the end of its microphone stream
// instead of a playback error.
func (w *webRealtimeAudio) enqueue(frame webRealtimeFrame) {
	w.mu.Lock()
	if w.closing || w.closed() {
		w.mu.Unlock()
		return
	}
	if len(w.out) >= webRealtimeSendQueue {
		w.mu.Unlock()
		logging.Warnf("agent", "realtime", "Browser voice connection is too slow: %d frames waiting; ending the session", webRealtimeSendQueue)
		w.Close("The connection to the board is too slow for voice")
		return
	}
	w.out = append(w.out, frame)
	w.mu.Unlock()
	w.signal()
}

// dropQueuedSpeech removes speech that has not been written yet, so a
// barge-in is not followed by the rest of the interrupted reply.
func (w *webRealtimeAudio) dropQueuedSpeech() {
	w.mu.Lock()
	kept := w.out[:0]
	for _, frame := range w.out {
		if frame.kind != websocket.MessageBinary {
			kept = append(kept, frame)
		}
	}
	w.out = kept
	w.mu.Unlock()
}

// run carries the socket until the browser leaves or the endpoint is closed.
// onGone runs as soon as the browser is known to be gone.
func (w *webRealtimeAudio) run(onGone func()) {
	writerDone := make(chan struct{})
	go func() {
		defer close(writerDone)
		w.write()
	}()
	w.read()
	onGone()
	w.Close("")
	<-writerDone
}

// write sends queued frames in order and closes the socket after the last.
func (w *webRealtimeAudio) write() {
	for {
		w.mu.Lock()
		if len(w.out) == 0 {
			gone := w.closed() && !w.closing
			w.mu.Unlock()
			if gone {
				// Lost without a goodbye to send; drop the socket.
				_ = w.conn.Close(websocket.StatusGoingAway, "")
				return
			}
			select {
			case <-w.wake:
			case <-w.done:
				// Re-check: Close queues its goodbye before marking done.
			}
			continue
		}
		frame := w.out[0]
		w.out = w.out[1:]
		w.mu.Unlock()
		if frame.data == nil {
			_ = w.conn.Close(websocket.StatusNormalClosure, "")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), webRealtimeWriteTimeout)
		err := w.conn.Write(ctx, frame.kind, frame.data)
		cancel()
		if err != nil {
			w.shutdown(fmt.Sprintf("write failed: %v", err))
			return
		}
	}
}

// read queues microphone frames until the browser disconnects.
func (w *webRealtimeAudio) read() {
	for {
		kind, data, err := w.conn.Read(context.Background())
		if err != nil {
			w.shutdown(fmt.Sprintf("read ended (close status %d): %v", websocket.CloseStatus(err), err))
			return
		}
		if kind != websocket.MessageBinary || len(data) < 2 {
			continue
		}
		pcm := data[:len(data)&^1]
		// Enqueue under the lock that StartRecording uses to clear the queue,
		// so a frame from a previous capture never lands after the reset.
		w.mu.Lock()
		if w.capturing {
			select {
			case w.mic <- pcm:
			default:
				// The session fell behind; keep the newest audio. This is the
				// only sender, so the slot freed here cannot be taken.
				select {
				case <-w.mic:
				default:
				}
				w.mic <- pcm
			}
		}
		w.mu.Unlock()
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
	// A reconnected session reuses this endpoint; audio captured for the
	// previous one must not reach it.
	drainWebRealtimeMic(r.w.mic)
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
	drainWebRealtimeMic(r.w.mic)
	r.w.mu.Unlock()
	r.w.send(map[string]string{"type": "capture_stop"})
	return nil
}

func drainWebRealtimeMic(mic chan []byte) {
	for {
		select {
		case <-mic:
		default:
			return
		}
	}
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
	if w.closed() {
		return nil
	}
	// The frame outlives this call, and the caller may reuse its buffer.
	w.enqueue(webRealtimeFrame{kind: websocket.MessageBinary, data: append([]byte(nil), data...)})
	return nil
}

func (p webRealtimePlayback) StopPlayback(uint64) error {
	w := p.w
	w.mu.Lock()
	w.playEnd = time.Time{}
	w.mu.Unlock()
	w.dropQueuedSpeech()
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
	// The socket outlives the server's read timeout, which net/http leaves
	// armed on a hijacked connection.
	_ = http.NewResponseController(w).SetReadDeadline(time.Time{})
	// Origin verification stays on: only the Web UI's own origin may connect.
	conn, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	conn.SetReadLimit(webRealtimeMaxMessageSize)
	web := newWebRealtimeAudio(conn)
	if err := s.startWebRealtimeVoice(web); err != nil {
		web.Close(err.Error())
		web.write()
		return
	}
	web.run(func() { s.releaseWebRealtimeVoice(web) })
}
