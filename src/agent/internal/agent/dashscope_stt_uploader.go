package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aico-agent/internal/stream"
	"github.com/gorilla/websocket"
)

// dashScopeUploader implements STTStreamUploader for DashScope real-time ASR.
type dashScopeUploader struct {
	conn        *websocket.Conn
	packetBytes int
	ctx         context.Context

	mu         sync.Mutex
	writeMu    sync.Mutex // Protects WriteMessage calls
	pending    []byte
	done       chan struct{}
	ready      chan struct{}
	ended      bool
	finished   bool
	transcript string
	err        error
	closeOnce  sync.Once
	eventSeq   atomic.Int64

	eventsMu     sync.Mutex
	events       chan stream.TranscriptEvent
	eventsClosed bool
	segmentID    string
	revision     uint64
	segmentFinal bool
	latestText   string
}

// TranscriptEvents exposes bounded, revisioned transcript updates from the
// realtime provider. The channel is closed when the session finishes.
func (u *dashScopeUploader) TranscriptEvents() <-chan stream.TranscriptEvent {
	if u == nil {
		return nil
	}
	return u.events
}

func (u *dashScopeUploader) emitTranscript(event stream.TranscriptEvent) {
	if u == nil || u.events == nil {
		return
	}
	if err := event.Validate(); err != nil {
		return
	}
	u.eventsMu.Lock()
	defer u.eventsMu.Unlock()
	if u.eventsClosed {
		return
	}
	select {
	case u.events <- event:
	default:
		// Hypotheses are replaceable snapshots. Keep the channel bounded and
		// prefer a recent update over blocking the websocket reader.
		select {
		case <-u.events:
		default:
		}
		select {
		case u.events <- event:
		default:
		}
	}
}

func (u *dashScopeUploader) closeTranscriptEvents() {
	if u == nil || u.events == nil {
		return
	}
	u.eventsMu.Lock()
	defer u.eventsMu.Unlock()
	if !u.eventsClosed {
		u.eventsClosed = true
		close(u.events)
	}
}

func (u *dashScopeUploader) transcriptUpdate(text, itemID string, final bool) {
	if u == nil {
		return
	}
	u.mu.Lock()
	if u.segmentFinal {
		u.mu.Unlock()
		return
	}
	if itemID == "" {
		itemID = u.segmentID
	}
	if itemID == "" {
		itemID = "dashscope-input-1"
	}
	if u.segmentID != itemID {
		u.segmentID = itemID
		u.revision = 0
		u.latestText = ""
	}
	u.revision++
	revision := u.revision
	u.latestText = text
	if final {
		u.segmentFinal = true
	}
	u.mu.Unlock()

	segment := stream.TranscriptSegment{SegmentID: itemID, Revision: revision, Text: text}
	if final {
		u.emitTranscript(stream.NewTranscriptSegmentFinal(itemID, revision, text))
		if strings.TrimSpace(text) != "" {
			u.emitTranscript(stream.NewTurnCommit([]stream.TranscriptSegment{segment}, text))
		}
		return
	}
	u.emitTranscript(stream.NewTranscriptHypothesis(itemID, revision, text))
}

func (u *dashScopeUploader) nextEventID() string {
	seq := u.eventSeq.Add(1)
	return "evt_" + strconv.FormatInt(seq, 36) + "_" + strconv.FormatInt(time.Now().UnixNano()%1e6, 36)
}

func (u *dashScopeUploader) UploadPCM(pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}

	u.mu.Lock()
	if u.ended {
		u.mu.Unlock()
		return fmt.Errorf("dashscope stream already finalized")
	}
	u.pending = append(u.pending, pcm...)
	chunks := make([][]byte, 0)
	for len(u.pending) >= u.packetBytes {
		chunk := append([]byte(nil), u.pending[:u.packetBytes]...)
		u.pending = u.pending[u.packetBytes:]
		chunks = append(chunks, chunk)
	}
	u.mu.Unlock()

	for _, chunk := range chunks {
		msg := dashScopeMessage{
			EventID: u.nextEventID(),
			Type:    "input_audio_buffer.append",
			Audio:   base64.StdEncoding.EncodeToString(chunk),
		}
		data, err := json.Marshal(msg)
		if err != nil {
			u.finish("", fmt.Errorf("marshal audio append: %w", err))
			return u.readErr()
		}
		u.writeMu.Lock()
		err = u.conn.WriteMessage(websocket.TextMessage, data)
		u.writeMu.Unlock()
		if err != nil {
			u.finish("", fmt.Errorf("write audio chunk: %w", err))
			return u.readErr()
		}
	}
	return nil
}

func (u *dashScopeUploader) Finalize() (string, error) {
	u.mu.Lock()
	if u.finished {
		transcript, err := u.transcript, u.err
		u.mu.Unlock()
		return transcript, err
	}
	if !u.ended {
		u.ended = true
	}
	pending := append([]byte(nil), u.pending...)
	u.pending = nil
	u.mu.Unlock()

	// Send remaining audio.
	if len(pending) > 0 {
		msg := dashScopeMessage{
			EventID: u.nextEventID(),
			Type:    "input_audio_buffer.append",
			Audio:   base64.StdEncoding.EncodeToString(pending),
		}
		data, _ := json.Marshal(msg)
		u.writeMu.Lock()
		err := u.conn.WriteMessage(websocket.TextMessage, data)
		u.writeMu.Unlock()
		if err != nil {
			u.finish("", fmt.Errorf("write final audio chunk: %w", err))
			return u.readResult()
		}
	}

	// Send input_audio_buffer.commit to signal end of audio input.
	commitMsg := dashScopeMessage{
		EventID: u.nextEventID(),
		Type:    "input_audio_buffer.commit",
	}
	u.writeMu.Lock()
	err := u.conn.WriteJSON(commitMsg)
	u.writeMu.Unlock()
	if err != nil {
		u.finish("", fmt.Errorf("write commit: %w", err))
		return u.readResult()
	}

	// Wait for done with context timeout
	if u.ctx != nil {
		select {
		case <-u.done:
		case <-u.ctx.Done():
			u.finish("", fmt.Errorf("dashscope STT: context timeout waiting for response"))
		}
	} else {
		<-u.done
	}
	return u.readResult()
}

func (u *dashScopeUploader) Close() error {
	u.closeOnce.Do(func() {
		if u.conn != nil {
			_ = u.conn.Close()
		}
	})
	return nil
}

func (u *dashScopeUploader) readResult() (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.transcript, u.err
}

func (u *dashScopeUploader) readErr() error {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.err
}

func (u *dashScopeUploader) finish(transcript string, err error) {
	u.mu.Lock()
	if u.finished {
		u.mu.Unlock()
		return
	}
	u.finished = true
	u.transcript = transcript
	u.err = err
	close(u.done)
	u.mu.Unlock()
	u.closeTranscriptEvents()
}

func (u *dashScopeUploader) readLoop() {
	defer func() {
		u.mu.Lock()
		if !u.finished {
			u.finished = true
			if u.err == nil {
				u.err = fmt.Errorf("dashscope STT: readLoop exited unexpectedly")
			}
			close(u.done)
		}
		u.mu.Unlock()
		u.closeTranscriptEvents()
		_ = u.conn.Close()
	}()

	sessionReady := false
	for {
		_, payload, err := u.conn.ReadMessage()
		if err != nil {
			u.mu.Lock()
			if u.finished {
				u.mu.Unlock()
				return
			}
			u.mu.Unlock()
			u.finish("", fmt.Errorf("read websocket: %w", err))
			return
		}

		var event dashScopeServerEvent
		if err := json.Unmarshal(payload, &event); err != nil {
			u.finish("", fmt.Errorf("unmarshal server event: %w", err))
			return
		}

		switch event.Type {
		case "session.created", "session.updated":
			if !sessionReady {
				sessionReady = true
				close(u.ready)
			}
		case "conversation.item.input_audio_transcription.completed":
			text := event.Transcript
			if text == "" {
				text = event.Text
			}
			u.transcriptUpdate(text, event.ItemID, true)
			u.finish(text, nil)
			return
		case "error":
			msg := "unknown error"
			if event.Error != nil {
				msg = event.Error.Code + ": " + event.Error.Message
			}
			u.finish("", fmt.Errorf("dashscope STT server error: %s", msg))
			return
		case "input_audio_buffer.speech_started",
			"input_audio_buffer.speech_stopped",
			"input_audio_buffer.committed",
			"conversation.item.input_audio_transcription.text":
			if event.Type == "conversation.item.input_audio_transcription.text" {
				text := event.Transcript
				if text == "" {
					text = event.Text
				}
				u.transcriptUpdate(text, event.ItemID, false)
			}
			// Other input lifecycle events are informational.
		}
	}
}
