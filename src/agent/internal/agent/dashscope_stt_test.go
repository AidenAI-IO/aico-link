package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"aico-agent/internal/stream"
	"github.com/gorilla/websocket"
)

func TestDashScopeRealtimeSTTEmitsRevisionedTranscriptEvents(t *testing.T) {
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Errorf("Upgrade() error = %v", err)
			return
		}
		defer conn.Close()

		if err := conn.WriteJSON(map[string]any{"type": "session.created", "event_id": "evt-ready"}); err != nil {
			t.Errorf("WriteJSON(session.created) error = %v", err)
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Errorf("ReadMessage(session.update) error = %v", err)
			return
		}
		for {
			_, payload, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var message dashScopeMessage
			if err := json.Unmarshal(payload, &message); err != nil {
				t.Errorf("decode client message: %v", err)
				return
			}
			if message.Type == "input_audio_buffer.commit" {
				break
			}
		}
		for _, event := range []map[string]any{
			{"type": "conversation.item.input_audio_transcription.text", "item_id": "item-1", "transcript": "hel"},
			{"type": "conversation.item.input_audio_transcription.text", "item_id": "item-1", "transcript": "hello"},
			{"type": "conversation.item.input_audio_transcription.completed", "item_id": "item-1", "transcript": "hello"},
		} {
			if err := conn.WriteJSON(event); err != nil {
				t.Errorf("WriteJSON(%v) error = %v", event["type"], err)
				return
			}
		}
	}))
	defer server.Close()

	client := NewDashScopeRealtimeSTT("test-key", "qwen3-asr-flash-realtime", "ws"+strings.TrimPrefix(server.URL, "http"), "en")
	uploader, err := client.NewStreamingUploader(context.Background(), STTStreamConfig{SampleRate: 16000, Channels: 1, BitWidth: 16})
	if err != nil {
		t.Fatalf("NewStreamingUploader() error = %v", err)
	}
	if err := uploader.UploadPCM(make([]byte, 3200)); err != nil {
		t.Fatalf("UploadPCM() error = %v", err)
	}
	transcript, err := uploader.Finalize()
	if err != nil {
		t.Fatalf("Finalize() error = %v", err)
	}
	if transcript != "hello" {
		t.Fatalf("transcript = %q, want hello", transcript)
	}

	source, ok := uploader.(STTTranscriptEventSource)
	if !ok {
		t.Fatal("uploader does not expose transcript events")
	}
	var events []stream.TranscriptEvent
	for event := range source.TranscriptEvents() {
		events = append(events, event)
	}
	if len(events) != 4 {
		t.Fatalf("events = %#v, want two hypotheses, final, and commit", events)
	}
	if events[0].Kind != stream.TranscriptHypothesis || events[0].Segment.Revision != 1 || events[0].Segment.Text != "hel" {
		t.Fatalf("first event = %#v", events[0])
	}
	if events[1].Kind != stream.TranscriptHypothesis || events[1].Segment.Revision != 2 || events[1].Segment.Text != "hello" {
		t.Fatalf("second event = %#v", events[1])
	}
	if events[2].Kind != stream.TranscriptSegmentFinal || events[2].Segment.Revision != 3 || events[2].Segment.Text != "hello" {
		t.Fatalf("final event = %#v", events[2])
	}
	if events[3].Kind != stream.TranscriptTurnCommit || events[3].Text != "hello" || len(events[3].CommitSegments) != 1 {
		t.Fatalf("commit event = %#v", events[3])
	}
}
