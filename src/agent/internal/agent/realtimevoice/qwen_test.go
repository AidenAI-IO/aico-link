package realtimevoice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestQwenProviderPreservesDashScopeProtocol(t *testing.T) {
	clientEvents := make(chan map[string]any, 16)
	updates := make(chan map[string]any, 1)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer qwen-key" {
			t.Errorf("authorization = %q", got)
		}
		if got := r.Header.Get("X-DashScope-WorkSpace"); got != "workspace-1" {
			t.Errorf("workspace header = %q", got)
		}
		if got := r.URL.Query().Get("model"); got != DefaultQwenRealtimeModel {
			t.Errorf("model query = %q", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if err := conn.WriteJSON(map[string]any{"type": "session.created", "session": map[string]any{"id": "qwen-session"}}); err != nil {
			t.Error(err)
			return
		}
		_, body, err := conn.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		var update map[string]any
		if err := json.Unmarshal(body, &update); err != nil {
			t.Error(err)
			return
		}
		updates <- update
		if err := conn.WriteJSON(map[string]any{"type": "session.updated", "session": map[string]any{"id": "qwen-session"}}); err != nil {
			t.Error(err)
			return
		}
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var event map[string]any
			if err := json.Unmarshal(body, &event); err != nil {
				t.Error(err)
				return
			}
			clientEvents <- event
		}
	}))
	defer server.Close()

	emotion := true
	threshold := 0.25
	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	session, err := (QwenProvider{Endpoint: endpoint, WorkspaceID: "workspace-1"}).Open(context.Background(), SessionConfig{
		APIKey:                 "qwen-key",
		Instructions:           "be concise",
		InputAudioFormat:       "pcm",
		OutputAudioFormat:      "pcm",
		MaxHistoryTurns:        12,
		EnableSpeechEmotion:    &emotion,
		TurnDetection:          "server_vad",
		TurnDetectionThresh:    &threshold,
		TurnDetectionSilenceMs: 650,
		Tools:                  []Tool{{Name: "clock", Description: "get time", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := session.Info(); got.ID != "qwen-session" || got.InputSampleRate != 16000 || got.OutputSampleRate != 24000 {
		t.Fatalf("session info = %+v", got)
	}

	select {
	case update := <-updates:
		if update["type"] != "session.update" {
			t.Fatalf("session update = %#v", update)
		}
		settings := update["session"].(map[string]any)
		if settings["voice"] != "longanqian" || settings["enable_speech_emotion"] != true || settings["max_history_turns"] != float64(12) {
			t.Fatalf("qwen settings = %#v", settings)
		}
		turnDetection := settings["turn_detection"].(map[string]any)
		if turnDetection["type"] != "server_vad" || turnDetection["threshold"] != 0.25 || turnDetection["silence_duration_ms"] != float64(650) {
			t.Fatalf("turn detection = %#v", turnDetection)
		}
		tools := settings["tools"].([]any)
		function := tools[0].(map[string]any)["function"].(map[string]any)
		if function["name"] != "clock" || function["description"] != "get time" {
			t.Fatalf("qwen tool = %#v", function)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for Qwen session.update")
	}

	assertClientEvent := func(call func() error, wantType string, check func(map[string]any)) {
		t.Helper()
		if err := call(); err != nil {
			t.Fatal(err)
		}
		select {
		case event := <-clientEvents:
			if event["type"] != wantType {
				t.Fatalf("client event = %#v, want type %q", event, wantType)
			}
			if check != nil {
				check(event)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", wantType)
		}
	}
	assertClientEvent(func() error { return session.SendAudio(context.Background(), []byte{1, 2, 3}) }, "input_audio_buffer.append", func(event map[string]any) {
		if event["audio"] != "AQID" {
			t.Fatalf("audio event = %#v", event)
		}
	})
	assertClientEvent(func() error { return session.(TurnCommitter).Commit(context.Background()) }, "input_audio_buffer.commit", nil)
	assertClientEvent(func() error {
		return session.(ResponseInterrupter).Interrupt(context.Background(), ResponseInterruption{})
	}, "response.cancel", nil)
	assertClientEvent(func() error {
		return session.(ToolResultSender).SendToolResult(context.Background(), "call-1", `{"ok":true}`)
	}, "conversation.item.create", func(event map[string]any) {
		item := event["item"].(map[string]any)
		if item["type"] != "function_call_output" || item["call_id"] != "call-1" || item["output"] != `{"ok":true}` {
			t.Fatalf("tool result = %#v", item)
		}
	})
	textSession := session.(TextSession)
	assertClientEvent(func() error { return textSession.SendText(context.Background(), "hello") }, "conversation.item.create", func(event map[string]any) {
		item := event["item"].(map[string]any)
		if item["type"] != "message" || item["role"] != "user" {
			t.Fatalf("text item = %#v", item)
		}
	})
	assertClientEvent(func() error { return textSession.CreateResponse(context.Background()) }, "response.create", nil)
	assertClientEvent(func() error {
		return session.(ContextReplayer).ReplayContext(context.Background(), []ContextItem{{Type: "function_call", CallID: "call-2", Name: "clock", Arguments: `{}`}})
	}, "conversation.item.create", func(event map[string]any) {
		item := event["item"].(map[string]any)
		if item["type"] != "function_call" || item["call_id"] != "call-2" || item["name"] != "clock" {
			t.Fatalf("replayed item = %#v", item)
		}
	})
}

func TestQwenServerDetectedBargeInDoesNotSendRedundantCancel(t *testing.T) {
	// Any attempted client write on this transport-free session would fail the
	// test; provider-managed barge-in must return without writing.
	session := &qwenSession{}
	if err := session.Interrupt(context.Background(), ResponseInterruption{ServerDetected: true}); err != nil {
		t.Fatal(err)
	}
}

func TestQwenProviderEndpoints(t *testing.T) {
	tests := []struct {
		name     string
		provider QwenProvider
		want     string
	}{
		{
			name:     "legacy Beijing endpoint",
			provider: QwenProvider{Region: "cn-beijing"},
			want:     "wss://dashscope.aliyuncs.com/api-ws/v1/realtime?model=qwen-model",
		},
		{
			name:     "Beijing workspace endpoint",
			provider: QwenProvider{Region: "cn-beijing", WorkspaceID: "workspace-1"},
			want:     "wss://workspace-1.cn-beijing.maas.aliyuncs.com/api-ws/v1/realtime?model=qwen-model",
		},
		{
			name:     "legacy Singapore endpoint",
			provider: QwenProvider{Region: "ap-southeast-1"},
			want:     "wss://dashscope-intl.aliyuncs.com/api-ws/v1/realtime?model=qwen-model",
		},
		{
			name:     "Singapore workspace endpoint",
			provider: QwenProvider{Region: "ap-southeast-1", WorkspaceID: "workspace-1"},
			want:     "wss://workspace-1.ap-southeast-1.maas.aliyuncs.com/api-ws/v1/realtime?model=qwen-model",
		},
		{
			name:     "endpoint override preserves other query parameters",
			provider: QwenProvider{Endpoint: "wss://example.com/realtime?version=1"},
			want:     "wss://example.com/realtime?model=qwen-model&version=1",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := tt.provider.endpoint("qwen-model")
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.want {
				t.Fatalf("endpoint = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestQwenProviderRejectsUnsupportedRegion(t *testing.T) {
	_, err := (QwenProvider{Region: "us-west-1"}).endpoint("qwen-model")
	if err == nil || !strings.Contains(err.Error(), "unsupported region") {
		t.Fatalf("endpoint error = %v, want unsupported region", err)
	}
}

func TestTranslateQwenEvents(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		kind  EventKind
		check func(*testing.T, Event)
	}{
		{name: "speech", raw: `{"type":"input_audio_buffer.speech_started","item_id":"item-1","audio_start_ms":120}`, kind: EventSpeechStarted, check: func(t *testing.T, e Event) {
			if e.ItemID != "item-1" || e.AudioStartMS != 120 {
				t.Fatalf("speech event = %+v", e)
			}
		}},
		{name: "invalid speech", raw: `{"type":"input_audio_buffer.speech_stopped","item_id":"item-1","audio_end_ms":840,"reason":"turn_invalid"}`, kind: EventSpeechStopped, check: func(t *testing.T, e Event) {
			if e.ItemID != "item-1" || e.AudioEndMS != 840 || e.Status != "turn_invalid" {
				t.Fatalf("speech status = %q, want turn_invalid", e.Status)
			}
		}},
		{name: "input committed", raw: `{"type":"input_audio_buffer.committed","item_id":"item-1","previous_item_id":"item-0"}`, kind: EventInputCommitted, check: func(t *testing.T, e Event) {
			if e.ItemID != "item-1" || e.PreviousItemID != "item-0" {
				t.Fatalf("committed event = %+v", e)
			}
		}},
		{name: "audio", raw: `{"type":"response.audio.delta","delta":"` + base64.StdEncoding.EncodeToString([]byte{1, 2}) + `"}`, kind: EventAudio, check: func(t *testing.T, e Event) {
			if string(e.PCM) != string([]byte{1, 2}) {
				t.Fatalf("pcm = %v", e.PCM)
			}
		}},
		{name: "transcript", raw: `{"type":"response.audio_transcript.delta","delta":"hi"}`, kind: EventTranscriptDelta, check: func(t *testing.T, e Event) {
			if e.Text != "hi" || e.TextSource != "audio" {
				t.Fatalf("event = %+v", e)
			}
		}},
		{name: "transcript done", raw: `{"type":"response.audio_transcript.done","response_id":"r1","item_id":"item-1","transcript":"hi"}`, kind: EventTranscriptFinal, check: func(t *testing.T, e Event) {
			if e.ResponseID != "r1" || e.ItemID != "item-1" || e.Text != "hi" {
				t.Fatalf("event = %+v", e)
			}
		}},
		{name: "tool", raw: `{"type":"response.function_call_arguments.done","response_id":"r1","call_id":"c1","name":"clock","arguments":"{}"}`, kind: EventToolCall, check: func(t *testing.T, e Event) {
			if e.ResponseID != "r1" || e.CallID != "c1" || e.Name != "clock" {
				t.Fatalf("event = %+v", e)
			}
		}},
		{name: "done", raw: `{"type":"response.done","response":{"id":"r1","status":"completed","usage":{"total_tokens":3,"input_tokens":1,"output_tokens":2}}}`, kind: EventResponseDone, check: func(t *testing.T, e Event) {
			if e.Usage.TotalTokens != 3 {
				t.Fatalf("usage = %+v", e.Usage)
			}
		}},
		{name: "error", raw: `{"type":"error","error":{"message":"boom"}}`, kind: EventError, check: func(t *testing.T, e Event) {
			if e.Error == nil || e.Error.Error() != "boom" {
				t.Fatalf("error = %v", e.Error)
			}
		}},
		{name: "transcription failed", raw: `{"type":"conversation.item.input_audio_transcription.failed","item_id":"item-1","error":{"code":"audio_unintelligible","message":"could not transcribe"}}`, kind: EventTranscriptFailed, check: func(t *testing.T, e Event) {
			if e.ItemID != "item-1" || e.Error == nil || !strings.Contains(e.Error.Error(), "audio_unintelligible") {
				t.Fatalf("transcription failure = %+v", e)
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			event, ok := translateQwenEvent([]byte(tt.raw))
			if !ok || event.Kind != tt.kind {
				t.Fatalf("event = %+v, ok=%t", event, ok)
			}
			if tt.check != nil {
				tt.check(t, event)
			}
		})
	}
}

func TestTranslateQwenTurnSequenceDoesNotDuplicateSpeechStopped(t *testing.T) {
	raw := []string{
		`{"type":"input_audio_buffer.speech_stopped"}`,
		`{"type":"input_audio_buffer.committed"}`,
		`{"type":"conversation.item.input_audio_transcription.completed","item_id":"item-1","transcript":"hello"}`,
		`{"type":"response.created","response":{"id":"response-1"}}`,
	}
	var kinds []EventKind
	for _, body := range raw {
		if event, ok := translateQwenEvent([]byte(body)); ok {
			kinds = append(kinds, event.Kind)
		}
	}
	want := []EventKind{EventSpeechStopped, EventInputCommitted, EventTranscriptFinal, EventResponseStarted}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("translated event kinds = %v, want %v", kinds, want)
	}
}

func TestTranslateQwenIgnoresIdempotentCancelError(t *testing.T) {
	event, ok := translateQwenEvent([]byte(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"Conversation has no active response.","param":"response.cancel"}}`))
	if ok {
		t.Fatalf("idempotent cancel error was surfaced: %+v", event)
	}

	// A refused response.create is not a session failure: the server's own
	// response is running and the request can be repeated after it.
	event, ok = translateQwenEvent([]byte(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"Cannot create response while another response is in progress.","param":"response.create"}}`))
	if !ok || event.Kind != EventResponseRejected {
		t.Fatalf("response-in-progress error = %+v, ok=%t; want a rejected response", event, ok)
	}

	// A user turn left unanswered behind a requested response is not a
	// session failure either: the turn can still be answered.
	event, ok = translateQwenEvent([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"Server VAD turn committed but no response was created because a manual response is already in progress."}}`))
	if !ok || event.Kind != EventTurnResponseMissed {
		t.Fatalf("unanswered-turn error = %+v, ok=%t; want a missed turn", event, ok)
	}

	event, ok = translateQwenEvent([]byte(`{"type":"error","error":{"type":"invalid_request_error","code":"invalid_value","message":"Invalid session config.","param":"session.update"}}`))
	if !ok || event.Kind != EventError || event.Error == nil {
		t.Fatalf("unrelated protocol error was suppressed: %+v, ok=%t", event, ok)
	}
}

func TestQwenResponseDoneFailureIsError(t *testing.T) {
	for _, status := range []string{"failed", "incomplete"} {
		t.Run(status, func(t *testing.T) {
			event, ok := translateQwenEvent([]byte(`{"type":"response.done","response":{"id":"resp_1","status":"` + status + `"}}`))
			if !ok || event.Kind != EventError || event.Error == nil {
				t.Fatalf("event = %+v, ok=%t; want provider failure", event, ok)
			}
			if !strings.Contains(event.Error.Error(), status) {
				t.Fatalf("error = %v, want status %q", event.Error, status)
			}
		})
	}
}
