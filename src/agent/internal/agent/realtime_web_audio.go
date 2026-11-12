package agent

import (
	"context"
	"encoding/json"
	"errors"
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
	// speech is how long a binary frame plays for.
	speech time.Duration
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

	mu        sync.Mutex
	closing   bool
	out       []webRealtimeFrame
	bytesPerS int64
	// recordingID names the current capture and recordingStop ends its
	// reader, so a reader left over from an earlier capture cannot take
	// microphone frames meant for the next one. recordingStop is nil while
	// nothing is captured.
	recordingID   uint64
	recordingStop chan struct{}
	playbackID    uint64
	// unsent counts speech frames not yet written to the browser: queued,
	// being written, or discarded when the endpoint closed. playEnd is when
	// the browser finishes playing the speech written so far.
	unsent  int
	playEnd time.Time
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
	// Speech still queued is not worth sending to a session that is over. It
	// stays counted in unsent, so a later drain does not report it as heard.
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
	if frame.kind == websocket.MessageBinary {
		w.unsent++
	}
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
	w.unsent -= len(w.out) - len(kept)
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
		if frame.kind == websocket.MessageBinary {
			// The browser plays speech back to back as it arrives, and
			// starts again from now after a gap.
			w.mu.Lock()
			if now := time.Now(); w.playEnd.Before(now) {
				w.playEnd = now
			}
			w.playEnd = w.playEnd.Add(frame.speech)
			if w.unsent > 0 {
				w.unsent--
			}
			w.mu.Unlock()
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
		if w.recordingStop != nil {
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
	w := r.w
	w.mu.Lock()
	// A reconnected session reuses this endpoint; audio captured for the
	// previous one must not reach it.
	w.stopRecordingLocked()
	w.recordingID++
	id := w.recordingID
	w.recordingStop = make(chan struct{})
	w.mu.Unlock()
	w.send(map[string]any{"type": "capture_start", "format": webRealtimeFormat(format)})
	return &RecordStartResult{SessionID: id}, nil
}

// stopRecordingLocked ends the current capture and its reader, and drops
// microphone audio not yet read. The caller holds w.mu.
func (w *webRealtimeAudio) stopRecordingLocked() {
	if w.recordingStop != nil {
		close(w.recordingStop)
		w.recordingStop = nil
	}
	drainWebRealtimeMic(w.mic)
}

// ReadRecordChunk returns an empty chunk when the browser sent nothing within
// the timeout, and the end of the stream once the browser disconnected or the
// capture named by id was stopped.
func (r webRealtimeRecording) ReadRecordChunk(id uint64, timeoutMs uint32) (*AudioChunkResult, error) {
	w := r.w
	w.mu.Lock()
	stopped := w.recordingStop
	if id != w.recordingID {
		stopped = nil
	}
	w.mu.Unlock()
	if stopped == nil {
		return &AudioChunkResult{EndOfStream: true}, nil
	}
	timer := time.NewTimer(time.Duration(timeoutMs) * time.Millisecond)
	defer timer.Stop()
	select {
	case pcm := <-w.mic:
		return &AudioChunkResult{PCM: pcm}, nil
	case <-stopped:
		return &AudioChunkResult{EndOfStream: true}, nil
	case <-w.done:
		return &AudioChunkResult{EndOfStream: true}, nil
	case <-timer.C:
		return &AudioChunkResult{}, nil
	}
}

func (r webRealtimeRecording) StopRecording(id uint64) error {
	w := r.w
	w.mu.Lock()
	if id == w.recordingID {
		w.stopRecordingLocked()
	}
	w.mu.Unlock()
	w.send(map[string]string{"type": "capture_stop"})
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
	w := p.w
	if len(data) == 0 || w.closed() {
		return nil
	}
	w.mu.Lock()
	var speech time.Duration
	if w.bytesPerS > 0 {
		speech = time.Duration(int64(len(data)) * int64(time.Second) / w.bytesPerS)
	}
	w.mu.Unlock()
	// The frame outlives this call, and the caller may reuse its buffer.
	w.enqueue(webRealtimeFrame{kind: websocket.MessageBinary, data: append([]byte(nil), data...), speech: speech})
	return nil
}

func (p webRealtimePlayback) StopPlayback(uint64) error {
	w := p.w
	w.dropQueuedSpeech()
	w.mu.Lock()
	w.playEnd = time.Time{}
	w.mu.Unlock()
	w.send(map[string]string{"type": "playback_stop"})
	return nil
}

// webRealtimeDrainPoll is how often a drain re-checks speech that is still
// waiting to be written.
const webRealtimeDrainPoll = 20 * time.Millisecond

// WaitForPlaybackDrain waits until every speech frame has been written to
// the browser and the browser has had time to play it. It fails when the
// browser leaves first, so speech that never arrived is not reported as
// heard.
func (p webRealtimePlayback) WaitForPlaybackDrain(ctx context.Context) error {
	w := p.w
	for {
		w.mu.Lock()
		unsent := w.unsent
		wait := time.Until(w.playEnd.Add(webRealtimeDrainMargin))
		w.mu.Unlock()
		if unsent == 0 && wait <= 0 {
			return nil
		}
		if unsent > 0 && wait < webRealtimeDrainPoll {
			wait = webRealtimeDrainPoll
		}
		timer := time.NewTimer(wait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-w.done:
			timer.Stop()
			return errors.New("the browser disconnected before playback finished")
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
