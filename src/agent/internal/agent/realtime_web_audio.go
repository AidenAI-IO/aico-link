package agent

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
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
//	{"type":"session","resume_token":"..."}     how to rejoin (resumable only)
//
// Closing the socket ends the session. A client that connects with
// ?resumable=1 (the phone App) keeps its session through a dropped
// connection instead: the agent holds the session for webRealtimeResumeGrace,
// keeps the reply speech it produces meanwhile, and a socket opened with
// ?resume=<resume_token> takes the session over. The agent then repeats the
// current capture and playback formats and sends the held speech, so the
// conversation continues where it was. A clean close (1000 or 1001) from the
// client still ends the session at once.
//
// A new session asks for the microphone at once, before the provider is
// connected, and keeps what the client sends meanwhile (up to
// webRealtimePrerollLimit). The first capture of the session in the same
// format receives that audio first, so the user can start talking while the
// provider connects without losing the beginning.
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
	// webRealtimeResumeGrace is how long a resumable session waits for its
	// client to come back after the connection dropped. A USB re-enumeration
	// on the phone link takes a second or two, and a link iOS leaves down takes
	// the ECM watchdog about half a minute to repair; this covers both.
	webRealtimeResumeGrace = 60 * time.Second
	// webRealtimePrerollLimit bounds the microphone audio kept while the
	// provider connects: 10 s at 16 kHz mono 16-bit. Older audio is dropped.
	webRealtimePrerollLimit = 10 * 16000 * 2
)

// webRealtimePrerollFormat is the capture format asked for before the session
// knows its provider's; every realtime provider and Classic STT take it.
var webRealtimePrerollFormat = webRealtimeFormat{SampleRate: 16000, Channels: 1, BitWidth: 16}

// webRealtimeFrame is one outgoing message. A nil data closes the socket.
type webRealtimeFrame struct {
	kind websocket.MessageType
	data []byte
	// speech is how long a binary frame plays for.
	speech time.Duration
	// announces marks a capture_start, capture_stop or playback_start frame,
	// and format is the format it announces. The writer keeps what it last
	// sent, to tell a resumed client the formats of the frames that follow.
	announces webRealtimeAnnouncement
	format    webRealtimeFormat
}

type webRealtimeAnnouncement int

const (
	announcesNothing webRealtimeAnnouncement = iota
	announcesCaptureStart
	announcesCaptureStop
	announcesPlaybackStart
)

func controlFrame(message any) webRealtimeFrame {
	data, _ := json.Marshal(message)
	return webRealtimeFrame{kind: websocket.MessageText, data: data}
}

func captureStartFrame(format webRealtimeFormat) webRealtimeFrame {
	frame := controlFrame(map[string]any{"type": "capture_start", "format": format})
	frame.announces, frame.format = announcesCaptureStart, format
	return frame
}

func playbackStartFrame(format webRealtimeFormat) webRealtimeFrame {
	frame := controlFrame(map[string]any{"type": "playback_start", "format": format})
	frame.announces, frame.format = announcesPlaybackStart, format
	return frame
}

// webRealtimeAudio is the audio endpoint of a browser voice session. The
// session loop never waits on the network: outgoing frames are queued and
// written by run, and microphone frames are queued by run's reader.
type webRealtimeAudio struct {
	mic chan []byte
	// done is closed once the browser can no longer take part: it
	// disconnected, stopped keeping up, or the endpoint was closed.
	done     chan struct{}
	doneOnce sync.Once
	wake     chan struct{}

	mu sync.Mutex
	// conn is the client's current connection; connGen counts connections,
	// so a late error from a replaced one is ignored. conn is nil while a
	// resumable session waits for its client to return.
	conn    *websocket.Conn
	connGen uint64
	// resumeToken lets a new connection take over a resumable session; it is
	// empty when the client did not ask for resumption.
	resumeToken string
	// release runs once when the client is gone for good, to end the session
	// that uses this endpoint.
	release       func()
	writerStarted bool
	resumeTimer   *time.Timer
	// captureFormat is the format of the current capture (or preroll).
	captureFormat *webRealtimeFormat
	// sentCapture and sentPlayback are the formats in effect for the next
	// frame the writer sends: the last capture_start (nil after a
	// capture_stop) and playback_start it took from the queue. A resumed
	// client is told these first, so speech held across a format change
	// still plays in its own format.
	sentCapture  *webRealtimeFormat
	sentPlayback *webRealtimeFormat
	// prerolling is true from the early capture request until the session's
	// first capture; preroll holds the microphone audio received meanwhile,
	// and then that capture's audio not yet read.
	prerolling   bool
	preroll      [][]byte
	prerollBytes int
	closing      bool
	out          []webRealtimeFrame
	bytesPerS    int64
	// recordingID names the current capture and recordingStop ends its
	// reader, so a reader left over from an earlier capture cannot take
	// microphone frames meant for the next one. recordingStop is nil while
	// nothing is captured.
	recordingID   uint64
	recordingStop chan struct{}
	playbackID    uint64
	// unsent counts speech frames not yet written to the browser: queued,
	// being written, or dropped because the endpoint closed. playEnd is when
	// the browser finishes playing the speech written so far.
	unsent  int
	playEnd time.Time
}

// newWebRealtimeAudio creates the endpoint of one browser or App voice
// session. A resumable endpoint survives a dropped connection for
// webRealtimeResumeGrace.
func newWebRealtimeAudio(resumable bool) *webRealtimeAudio {
	w := &webRealtimeAudio{
		mic:  make(chan []byte, webRealtimeMicQueue),
		done: make(chan struct{}),
		wake: make(chan struct{}, 1),
	}
	if resumable {
		token := make([]byte, 16)
		if _, err := rand.Read(token); err == nil {
			w.resumeToken = hex.EncodeToString(token)
		}
	}
	return w
}

// matchesResumeToken reports whether token rejoins this session, which must
// still be running.
func (w *webRealtimeAudio) matchesResumeToken(token string) bool {
	if w == nil || token == "" || w.closed() {
		return false
	}
	w.mu.Lock()
	own := w.resumeToken
	w.mu.Unlock()
	return own != "" && subtle.ConstantTimeCompare([]byte(own), []byte(token)) == 1
}

func (w *webRealtimeAudio) Route() RealtimeAudioRoute { return RealtimeAudioRouteWeb }

func (w *webRealtimeAudio) Recording() AudioRecordingBackend { return webRealtimeRecording{w} }

func (w *webRealtimeAudio) Playback() tts.AudioServiceBackend { return webRealtimePlayback{w} }

// startPreroll asks the client for its microphone before the session has a
// provider, so the user can start speaking while it connects.
func (w *webRealtimeAudio) startPreroll() {
	w.mu.Lock()
	if w.closing || w.recordingStop != nil || w.prerolling {
		w.mu.Unlock()
		return
	}
	format := webRealtimePrerollFormat
	w.captureFormat = &format
	w.prerolling = true
	// Queued under the lock that sets the format, so a capture the session
	// starts meanwhile cannot be announced ahead of this one.
	overflow := w.enqueueLocked(captureStartFrame(format))
	w.mu.Unlock()
	w.finishEnqueue(overflow)
}

// Close tells the browser why the session ended and closes the socket once
// that message is written. It does not wait for the network.
func (w *webRealtimeAudio) Close(reason string) {
	w.mu.Lock()
	if w.closing {
		w.mu.Unlock()
		return
	}
	w.closing = true
	if w.resumeTimer != nil {
		w.resumeTimer.Stop()
	}
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
// instead of a playback error. Dropped speech stays counted as unsent, so a
// drain does not report it as heard.
func (w *webRealtimeAudio) enqueue(frame webRealtimeFrame) {
	w.mu.Lock()
	overflow := w.enqueueLocked(frame)
	w.mu.Unlock()
	w.finishEnqueue(overflow)
}

// enqueueLocked queues frame; the caller holds w.mu and passes the result to
// finishEnqueue once it has let go of the lock. It reports whether the queue
// was full.
func (w *webRealtimeAudio) enqueueLocked(frame webRealtimeFrame) (overflow bool) {
	if frame.kind == websocket.MessageBinary {
		w.unsent++
	}
	if w.closing || w.closed() {
		return false
	}
	if len(w.out) >= webRealtimeSendQueue {
		return true
	}
	w.out = append(w.out, frame)
	return false
}

func (w *webRealtimeAudio) finishEnqueue(overflow bool) {
	if overflow {
		logging.Warnf("agent", "realtime", "Browser voice connection is too slow: %d frames waiting; ending the session", webRealtimeSendQueue)
		w.Close("The connection to the board is too slow for voice")
		return
	}
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

// serve carries one client connection until it ends. The first connection
// starts the session; with a resume token a later one takes it over. It
// returns when this connection is over, while a resumable session may live
// on and wait for the next. A later connection is turned away (false) once
// the session is ending: its writer may be gone, so nothing would tell the
// client.
func (w *webRealtimeAudio) serve(conn *websocket.Conn) bool {
	w.mu.Lock()
	if w.connGen > 0 && (w.closing || w.closed()) {
		w.mu.Unlock()
		return false
	}
	old := w.conn
	w.conn = conn
	w.connGen++
	gen := w.connGen
	if w.resumeTimer != nil {
		w.resumeTimer.Stop()
		w.resumeTimer = nil
	}
	if gen == 1 && w.resumeToken != "" {
		w.out = append([]webRealtimeFrame{controlFrame(map[string]string{"type": "session", "resume_token": w.resumeToken})}, w.out...)
	}
	startWriter := !w.writerStarted
	w.writerStarted = true
	w.mu.Unlock()
	if old != nil {
		_ = old.Close(websocket.StatusGoingAway, "replaced by a resumed connection")
	}
	if startWriter {
		go w.write()
	}
	w.signal()
	w.read(conn, gen)
	return true
}

// connectionLost handles the end of connection gen. A resumable session whose
// client did not close cleanly waits for it to come back; otherwise, and when
// the wait runs out, the session ends.
func (w *webRealtimeAudio) connectionLost(gen uint64, cause string, clean bool) {
	w.mu.Lock()
	if gen != w.connGen {
		w.mu.Unlock()
		return
	}
	if w.closed() {
		// The agent ended the session (or gave up on a client too slow to
		// keep up); whatever used this endpoint still has to let go of it.
		release := w.release
		w.release = nil
		w.conn = nil
		w.mu.Unlock()
		if release != nil {
			release()
		}
		return
	}
	conn := w.conn
	if conn == nil {
		// The reader and the writer both report a lost connection; the
		// first one already handled it.
		w.mu.Unlock()
		return
	}
	w.conn = nil
	if w.resumeToken != "" && !clean {
		w.resumeTimer = time.AfterFunc(webRealtimeResumeGrace, func() {
			reason := "not resumed within " + webRealtimeResumeGrace.String() + " after: " + cause
			w.mu.Lock()
			expired := w.conn == nil && w.connGen == gen
			if expired {
				// Decided under the lock serve attaches under: a resume that
				// arrives now is turned away instead of joining a session
				// that is ending.
				w.shutdown(reason)
			}
			w.mu.Unlock()
			if expired {
				w.gone(reason)
			}
		})
		w.mu.Unlock()
		logging.Infof("agent", "realtime", "Voice client connection lost; holding the session for %s: %s", webRealtimeResumeGrace, cause)
		go func() { _ = conn.Close(websocket.StatusGoingAway, "") }()
		return
	}
	w.mu.Unlock()
	go func() { _ = conn.Close(websocket.StatusGoingAway, "") }()
	w.gone(cause)
}

// gone ends the session after its client left for good.
func (w *webRealtimeAudio) gone(cause string) {
	w.shutdown(cause)
	w.mu.Lock()
	release := w.release
	w.release = nil
	w.mu.Unlock()
	if release != nil {
		release()
	}
	w.Close("")
}

// write sends queued frames in order to the current connection and closes it
// after the last. While a resumable session waits for its client, frames are
// held; a frame whose write failed is kept for the next connection, also when
// that connection replaced this one while the write was in flight.
func (w *webRealtimeAudio) write() {
	var writingGen uint64
	for {
		w.mu.Lock()
		conn, gen := w.conn, w.connGen
		if conn != nil && gen != writingGen {
			if gen > 1 {
				// A resumed client starts from nothing: tell it the formats
				// of the frames that follow, then send what was held.
				var restate []webRealtimeFrame
				if w.sentCapture != nil {
					restate = append(restate, captureStartFrame(*w.sentCapture))
				}
				if w.sentPlayback != nil {
					restate = append(restate, playbackStartFrame(*w.sentPlayback))
				}
				w.out = append(restate, w.out...)
				logging.Infof("agent", "realtime", "Voice client resumed its session: held_frames=%d", len(w.out))
			}
			writingGen = gen
		}
		if conn == nil {
			if w.closed() {
				w.mu.Unlock()
				return
			}
			w.mu.Unlock()
			select {
			case <-w.wake:
			case <-w.done:
			}
			continue
		}
		if len(w.out) == 0 {
			gone := w.closed() && !w.closing
			w.mu.Unlock()
			if gone {
				// Lost without a goodbye to send; drop the socket.
				_ = conn.Close(websocket.StatusGoingAway, "")
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
		switch frame.announces {
		case announcesCaptureStart:
			format := frame.format
			w.sentCapture = &format
		case announcesCaptureStop:
			w.sentCapture = nil
		case announcesPlaybackStart:
			format := frame.format
			w.sentPlayback = &format
		}
		w.mu.Unlock()
		if frame.data == nil {
			_ = conn.Close(websocket.StatusNormalClosure, "")
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), webRealtimeWriteTimeout)
		err := conn.Write(ctx, frame.kind, frame.data)
		cancel()
		if err != nil {
			status := websocket.CloseStatus(err)
			clean := status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
			w.mu.Lock()
			if w.resumeToken != "" && !w.closing {
				// Still counted in unsent: it is sent on the next connection.
				w.out = append([]webRealtimeFrame{frame}, w.out...)
			}
			w.mu.Unlock()
			w.connectionLost(gen, fmt.Sprintf("write failed: %v", err), clean)
			continue
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

// read queues microphone frames until connection gen ends.
func (w *webRealtimeAudio) read(conn *websocket.Conn, gen uint64) {
	for {
		kind, data, err := conn.Read(context.Background())
		if err != nil {
			status := websocket.CloseStatus(err)
			clean := status == websocket.StatusNormalClosure || status == websocket.StatusGoingAway
			w.connectionLost(gen, fmt.Sprintf("read ended (close status %d): %v", status, err), clean)
			return
		}
		if kind != websocket.MessageBinary || len(data) < 2 {
			continue
		}
		pcm := data[:len(data)&^1]
		// Enqueue under the lock that StartRecording uses to clear the queue,
		// so a frame from a previous capture never lands after the reset.
		w.mu.Lock()
		if w.prerolling && w.recordingStop == nil && gen == w.connGen {
			if len(pcm) > webRealtimePrerollLimit {
				// Keep the newest audio; the limit is a whole number of samples.
				pcm = pcm[len(pcm)-webRealtimePrerollLimit:]
			}
			w.preroll = append(w.preroll, pcm)
			w.prerollBytes += len(pcm)
			for w.prerollBytes > webRealtimePrerollLimit && len(w.preroll) > 1 {
				w.prerollBytes -= len(w.preroll[0])
				w.preroll = w.preroll[1:]
			}
			w.mu.Unlock()
			continue
		}
		if w.recordingStop != nil && gen == w.connGen {
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
	captureFormat := webRealtimeFormat(format)
	w.mu.Lock()
	// The first capture takes over the audio the client sent while the
	// provider connected, if the client already captures in its format.
	continuesPreroll := w.prerolling && w.captureFormat != nil && *w.captureFormat == captureFormat
	held := w.preroll
	w.prerolling = false
	// A reconnected session reuses this endpoint; audio captured for the
	// previous one must not reach it.
	w.stopRecordingLocked()
	w.recordingID++
	id := w.recordingID
	w.recordingStop = make(chan struct{})
	w.captureFormat = &captureFormat
	overflow := false
	if continuesPreroll {
		w.preroll = held
	} else {
		overflow = w.enqueueLocked(captureStartFrame(captureFormat))
	}
	w.mu.Unlock()
	w.finishEnqueue(overflow)
	return &RecordStartResult{SessionID: id}, nil
}

// stopRecordingLocked ends the current capture and its reader, and drops
// microphone audio not yet read. The caller holds w.mu.
func (w *webRealtimeAudio) stopRecordingLocked() {
	if w.recordingStop != nil {
		close(w.recordingStop)
		w.recordingStop = nil
	}
	w.preroll = nil
	w.prerollBytes = 0
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
	if stopped != nil && len(w.preroll) > 0 {
		pcm := w.preroll[0]
		w.preroll = w.preroll[1:]
		w.prerollBytes -= len(pcm)
		w.mu.Unlock()
		return &AudioChunkResult{PCM: pcm}, nil
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
	stop := controlFrame(map[string]string{"type": "capture_stop"})
	stop.announces = announcesCaptureStop
	w.enqueue(stop)
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
	playbackFormat := webRealtimeFormat{
		SampleRate: uint32(format.SampleRate), Channels: uint32(format.Channels), BitWidth: uint32(format.BitWidth),
	}
	overflow := w.enqueueLocked(playbackStartFrame(playbackFormat))
	w.mu.Unlock()
	w.finishEnqueue(overflow)
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

// handleRealtimeVoiceSocket serves GET /api/realtime/voice/ws: a browser or
// the phone App starts a realtime voice session that uses its own microphone
// and speaker (?resumable=1 lets it survive a dropped connection), or takes
// over its running session again (?resume=<token>).
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
	query := r.URL.Query()
	if token := query.Get("resume"); token != "" {
		if web := s.resumableWebRealtimeVoice(token); web != nil && web.serve(conn) {
			return
		}
		// The session ended while the client was away; say so, so it stops
		// trying.
		ended := newWebRealtimeAudio(false)
		ended.Close("The voice session is over")
		ended.serve(conn)
		return
	}
	web := newWebRealtimeAudio(query.Get("resumable") == "1")
	web.release = func() { s.releaseWebRealtimeVoice(web) }
	if err := s.startWebRealtimeVoice(web); err != nil {
		web.release = nil
		web.Close(err.Error())
	} else {
		web.startPreroll()
	}
	web.serve(conn)
}
