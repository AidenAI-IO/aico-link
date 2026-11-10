package realtimevoice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestOpenAIProviderNormalizesRealtimeSession(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	interruptEvents := make(chan map[string]any, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer openai-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if got := r.URL.Query().Get("model"); got != "gpt-realtime" {
			t.Errorf("model query = %q", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		write := func(v any) {
			if err := conn.WriteJSON(v); err != nil {
				t.Error(err)
			}
		}
		write(map[string]any{"type": "session.created", "session": map[string]any{"id": "sess_1", "model": "gpt-realtime"}})
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
		if update["type"] != "session.update" {
			t.Errorf("first client event = %#v", update)
		}
		sessionSettings, _ := update["session"].(map[string]any)
		audioSettings, _ := sessionSettings["audio"].(map[string]any)
		inputSettings, _ := audioSettings["input"].(map[string]any)
		transcription, _ := inputSettings["transcription"].(map[string]any)
		if transcription["model"] != DefaultOpenAIInputTranscriptionModel {
			t.Errorf("input transcription = %#v", inputSettings["transcription"])
		}
		write(map[string]any{"type": "session.updated", "session": map[string]any{"id": "sess_1"}})

		for {
			typ, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if typ != websocket.TextMessage {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal(body, &event); err != nil {
				t.Error(err)
				return
			}
			switch event["type"] {
			case "input_audio_buffer.append":
				if _, ok := event["audio"].(string); !ok {
					t.Errorf("audio event = %#v", event)
				}
			case "response.cancel":
				interruptEvents <- event
				write(map[string]any{"type": "response.done", "response": map[string]any{"id": "resp_1", "status": "cancelled"}})
			case "conversation.item.truncate":
				interruptEvents <- event
			case "conversation.item.create":
				write(map[string]any{"type": "response.function_call_arguments.done", "response_id": "resp_1", "call_id": "call_1", "name": "clock", "arguments": `{"timezone":"UTC"}`})
				write(map[string]any{"type": "response.audio.delta", "delta": base64.StdEncoding.EncodeToString([]byte{1, 2, 3})})
			case "response.create":
				write(map[string]any{"type": "response.audio_transcript.done", "transcript": "done"})
			}
		}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"
	session, err := (OpenAIProvider{Endpoint: endpoint}).Open(context.Background(), SessionConfig{
		APIKey: "openai-key", Model: "gpt-realtime", Voice: "alloy", Instructions: "be concise",
		TurnDetection: "server_vad", Tools: []Tool{{Name: "clock", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := session.Info(); got.ID != "sess_1" || got.InputSampleRate != 24000 || got.OutputSampleRate != 24000 || got.InputAudioFormat != (AudioFormat{Encoding: "pcm_s16le", SampleRate: 24000, Channels: 1, BitDepth: 16}) || got.OutputAudioFormat != (AudioFormat{Encoding: "pcm_s16le", SampleRate: 24000, Channels: 1, BitDepth: 16}) {
		t.Fatalf("session info = %+v", got)
	}
	if err := session.SendAudio(context.Background(), []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := session.(ResponseInterrupter).Interrupt(context.Background(), ResponseInterruption{ItemID: "item_1", AudioEndMS: 125}); err != nil {
		t.Fatal(err)
	}
	for _, wantType := range []string{"response.cancel", "conversation.item.truncate"} {
		select {
		case event := <-interruptEvents:
			if event["type"] != wantType {
				t.Fatalf("interrupt event type = %q, want %q", event["type"], wantType)
			}
			if wantType == "conversation.item.truncate" {
				if event["item_id"] != "item_1" || event["audio_end_ms"] != float64(125) || event["content_index"] != float64(0) {
					t.Fatalf("truncate event = %#v", event)
				}
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", wantType)
		}
	}
	if err := session.(ToolResultSender).SendToolResult(context.Background(), "call_1", `{"ok":true}`); err != nil {
		t.Fatal(err)
	}
	textSession, ok := session.(TextSession)
	if !ok {
		t.Fatal("OpenAI session does not support text input")
	}
	if err := textSession.CreateResponse(context.Background()); err != nil {
		t.Fatal(err)
	}

	want := []EventKind{EventResponseCancelled, EventToolCall, EventAudio, EventTranscriptFinal}
	for _, kind := range want {
		select {
		case event := <-session.Events():
			if event.Kind != kind {
				t.Fatalf("event kind = %s, want %s (%+v)", event.Kind, kind, event)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for %s", kind)
		}
	}
}

func TestOpenAILegacyProviderUses24kHzNativeInput(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if err := conn.WriteJSON(map[string]any{"type": "session.created", "session": map[string]any{"id": "sess_legacy"}}); err != nil {
			t.Error(err)
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "session.updated", "session": map[string]any{"id": "sess_legacy"}}); err != nil {
			t.Error(err)
			return
		}
		_, _, _ = conn.ReadMessage()
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	session, err := (OpenAIProvider{Endpoint: endpoint, RealtimeProtocol: "legacy"}).Open(context.Background(), SessionConfig{
		APIKey: "openai-key", Model: "gpt-realtime-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if got := session.Info().InputAudioFormat.SampleRate; got != 24000 {
		t.Fatalf("legacy provider input rate = %d, want 24000", got)
	}
}

func TestOpenAIEndpointAcceptsHTTPBaseURL(t *testing.T) {
	provider := OpenAIProvider{Endpoint: "https://api.example.test/v1/realtime"}
	got, err := provider.endpoint("gpt-realtime")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(got)
	if err != nil {
		t.Fatal(err)
	}
	if u.Scheme != "wss" || u.Query().Get("model") != "gpt-realtime" {
		t.Fatalf("endpoint = %q", got)
	}
}

func TestOpenAISessionUpdateUsesNegotiatedRates(t *testing.T) {
	update := buildOpenAISessionUpdate(SessionConfig{InputSampleRate: 16000, OutputSampleRate: 24000}, "gpt-realtime")
	if update.Session.Audio.Input.Format.Rate != 16000 || update.Session.Audio.Output.Format.Rate != 24000 {
		t.Fatalf("audio rates = %d/%d, want 16000/24000", update.Session.Audio.Input.Format.Rate, update.Session.Audio.Output.Format.Rate)
	}
}

func TestTextOutputOnlyRequestsTextModality(t *testing.T) {
	cfg := SessionConfig{TextOutputOnly: true}
	for name, update := range map[string]any{
		"openai ga":     buildOpenAISessionUpdateForProtocol(cfg, "gpt-realtime", "ga"),
		"openai legacy": buildOpenAISessionUpdateForProtocol(cfg, "gpt-realtime", "legacy"),
		"qwen":          buildQwenSessionUpdate(cfg),
		"xai":           buildXAISessionUpdate(cfg, "grok"),
	} {
		body, err := json.Marshal(update)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(body), `odalities":["text"]`) {
			t.Errorf("%s: text-only session still requests audio output: %s", name, body)
		}
	}
}

func TestOpenAILegacySessionUpdateUsesBetaFields(t *testing.T) {
	provider := OpenAIProvider{Endpoint: "wss://api.mixroute.ai/v1/realtime"}
	endpoint, err := provider.endpoint("gpt-realtime-2")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	if got := u.Query().Get("model"); got != "gpt-realtime-2" {
		t.Fatalf("model query = %q, want gpt-realtime-2", got)
	}

	threshold := 0.5
	update := buildOpenAISessionUpdateForProtocol(SessionConfig{
		Instructions: "be concise", Voice: "alloy", InputSampleRate: 16000, OutputSampleRate: 24000,
		TurnDetection: "server_vad", TurnDetectionThresh: &threshold, TurnDetectionSilenceMs: 550,
	}, "gpt-realtime-2", "legacy")
	body, err := json.Marshal(update)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Session map[string]any `json:"session"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		t.Fatal(err)
	}
	if model, ok := payload.Session["model"]; ok {
		t.Fatalf("legacy session update must select the model only through the URL, got model=%v: %s", model, body)
	}
	text := string(body)
	for _, forbidden := range []string{"output_modalities", `"audio":`, `"type":"realtime"`} {
		if strings.Contains(text, forbidden) {
			t.Fatalf("legacy payload contains GA field %q: %s", forbidden, text)
		}
	}
	for _, required := range []string{`"modalities":["audio","text"]`, `"input_audio_format":"pcm16"`, `"output_audio_format":"pcm16"`, fmt.Sprintf(`"input_audio_transcription":{"model":%q}`, DefaultOpenAIInputTranscriptionModel), `"silence_duration_ms":550`} {
		if !strings.Contains(text, required) {
			t.Fatalf("legacy payload missing %q: %s", required, text)
		}
	}
}

func TestOpenAIProtocolAliases(t *testing.T) {
	for _, tc := range []struct{ input, want string }{
		{"", "ga"}, {"ga", "ga"}, {"legacy", "legacy"}, {"beta", "legacy"}, {" LEGACY ", "legacy"},
	} {
		if got := normalizeRealtimeProtocol(tc.input); got != tc.want {
			t.Errorf("normalizeRealtimeProtocol(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestParseOpenAITranscriptionAcknowledgement(t *testing.T) {
	tests := []struct {
		name       string
		raw        string
		fallbackID string
		want       openAITranscriptionAcknowledgement
	}{
		{
			name:       "GA uses fallback session ID",
			raw:        `{"type":"session.updated","session":{"audio":{"input":{"transcription":{"model":"gpt-4o-transcribe","language":"zh"}}}}}`,
			fallbackID: "session-fallback",
			want:       openAITranscriptionAcknowledgement{SessionID: "session-fallback", Model: "gpt-4o-transcribe", Language: "zh", Present: true},
		},
		{
			name: "legacy",
			raw:  `{"type":"session.updated","session":{"id":"session-legacy","input_audio_transcription":{"model":"gpt-4o-transcribe"}}}`,
			want: openAITranscriptionAcknowledgement{SessionID: "session-legacy", Model: "gpt-4o-transcribe", Present: true},
		},
		{
			name:       "transcription absent",
			raw:        `{"type":"session.updated","session":{}}`,
			fallbackID: "session-fallback",
			want:       openAITranscriptionAcknowledgement{SessionID: "session-fallback"},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := parseOpenAITranscriptionAcknowledgement([]byte(tc.raw), tc.fallbackID)
			if !ok || !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("acknowledgement=%+v ok=%t, want %+v", got, ok, tc.want)
			}
		})
	}
	if _, ok := parseOpenAITranscriptionAcknowledgement([]byte(`{"type":"session.created","session":{"id":"session-1"}}`), ""); ok {
		t.Fatal("session.created was treated as a transcription acknowledgement")
	}
}

func TestOpenAIContextReplayUsesProtocolSpecificAssistantContentType(t *testing.T) {
	item := ContextItem{Type: "message", Role: "assistant", Content: "prior answer"}
	for _, tc := range []struct {
		name     string
		protocol string
		want     string
	}{
		{name: "GA", protocol: "", want: "output_text"},
		{name: "legacy", protocol: "legacy", want: "text"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			payload := openAIContextItemPayload(item, tc.protocol)
			content, ok := payload["content"].([]map[string]string)
			if !ok || len(content) != 1 || content[0]["type"] != tc.want || content[0]["text"] != item.Content {
				t.Fatalf("content = %#v, want type %q and text %q", payload["content"], tc.want, item.Content)
			}
		})
	}
}

func TestOpenAIContextReplayMapsOversizedToolCallIDs(t *testing.T) {
	clientEvents := make(chan map[string]any, 8)
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		if err := conn.WriteJSON(map[string]any{"type": "session.created", "session": map[string]any{"id": "sess_replay"}}); err != nil {
			t.Error(err)
			return
		}
		if _, _, err := conn.ReadMessage(); err != nil {
			t.Error(err)
			return
		}
		if err := conn.WriteJSON(map[string]any{"type": "session.updated", "session": map[string]any{"id": "sess_replay"}}); err != nil {
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

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http")
	session, err := (OpenAIProvider{Endpoint: endpoint, RealtimeProtocol: "legacy"}).Open(context.Background(), SessionConfig{
		APIKey: "openai-key", Model: "gpt-realtime-2",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	longID := "call-f0a496e2-c1ed-4734-bcc0-966147d84a16-0"
	otherLongID := "call-f0a496e2-c1ed-4734-bcc0-966147d84a16-1"
	shortID := "call_Ogehzbxbwabe7NGA"
	items := []ContextItem{
		{Type: "function_call", CallID: longID, Name: "get_current_time", Arguments: "{}"},
		{Type: "function_call_output", CallID: longID, Output: `{"datetime":"2026-09-07T14:28:28Z"}`},
		{Type: "function_call", CallID: otherLongID, Name: "get_current_time", Arguments: "{}"},
		{Type: "function_call_output", CallID: otherLongID, Output: `{"datetime":"2026-09-07T14:28:29Z"}`},
		{Type: "function_call", CallID: shortID, Name: "get_current_time", Arguments: "{}"},
		{Type: "function_call_output", CallID: shortID, Output: `{"datetime":"2026-09-07T14:28:28Z"}`},
	}
	if err := session.(ContextReplayer).ReplayContext(context.Background(), items); err != nil {
		t.Fatal(err)
	}

	var gotIDs []string
	for range items {
		select {
		case event := <-clientEvents:
			item, ok := event["item"].(map[string]any)
			if !ok {
				t.Fatalf("conversation item = %#v", event["item"])
			}
			callID, _ := item["call_id"].(string)
			gotIDs = append(gotIDs, callID)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for replayed context item")
		}
	}
	if gotIDs[0] == longID || len(gotIDs[0]) > 32 {
		t.Fatalf("oversized call ID mapped to %q (len=%d)", gotIDs[0], len(gotIDs[0]))
	}
	if gotIDs[1] != gotIDs[0] {
		t.Fatalf("tool call/result IDs differ after mapping: %q != %q", gotIDs[0], gotIDs[1])
	}
	if gotIDs[2] == otherLongID || len(gotIDs[2]) > 32 {
		t.Fatalf("second oversized call ID mapped to %q (len=%d)", gotIDs[2], len(gotIDs[2]))
	}
	if gotIDs[3] != gotIDs[2] {
		t.Fatalf("second tool call/result IDs differ after mapping: %q != %q", gotIDs[2], gotIDs[3])
	}
	if gotIDs[2] == gotIDs[0] {
		t.Fatalf("distinct oversized call IDs collided: %q", gotIDs[0])
	}
	if gotIDs[4] != shortID || gotIDs[5] != shortID {
		t.Fatalf("provider-safe call ID changed: %#v", gotIDs[4:])
	}

	if err := session.(ToolResultSender).SendToolResult(context.Background(), longID, `{"ok":true}`); err != nil {
		t.Fatal(err)
	}
	select {
	case event := <-clientEvents:
		item, ok := event["item"].(map[string]any)
		if !ok || item["call_id"] != longID {
			t.Fatalf("live tool result call ID changed: %#v", event["item"])
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for live tool result")
	}
}

func TestOpenAIOutputEventAliasesNormalize(t *testing.T) {
	cases := []struct {
		name string
		raw  string
		want EventKind
	}{
		{name: "text", raw: `{"type":"response.output_text.delta","delta":"hello"}`, want: EventTranscriptDelta},
		{name: "audio transcript delta", raw: `{"type":"response.output_audio_transcript.delta","delta":"hello"}`, want: EventTranscriptDelta},
		{name: "audio transcript done", raw: `{"type":"response.output_audio_transcript.done","transcript":"hello"}`, want: EventTranscriptFinal},
		{name: "audio", raw: `{"type":"response.output_audio.delta","delta":"AQI="}`, want: EventAudio},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			event, ok := translateOpenAIEvent([]byte(tc.raw))
			if !ok || event.Kind != tc.want {
				t.Fatalf("event = %+v, ok=%t, want kind %s", event, ok, tc.want)
			}
		})
	}
}

func TestOpenAIResponseDoneFailureIsError(t *testing.T) {
	for _, status := range []string{"failed", "incomplete"} {
		t.Run(status, func(t *testing.T) {
			event, ok := translateOpenAIEvent([]byte(`{"type":"response.done","response":{"id":"resp_1","status":"` + status + `"}}`))
			if !ok || event.Kind != EventError || event.Error == nil {
				t.Fatalf("event = %+v, ok=%t; want provider failure", event, ok)
			}
			if !strings.Contains(event.Error.Error(), status) {
				t.Fatalf("error = %v, want status %q", event.Error, status)
			}
		})
	}
}

// See the xAI counterpart: committed only confirms the audio that
// speech_stopped already ended, so it must not mutate turn state again.
func TestTranslateOpenAITurnSequenceDoesNotDuplicateSpeechStopped(t *testing.T) {
	raw := []string{
		`{"type":"input_audio_buffer.speech_stopped"}`,
		`{"type":"input_audio_buffer.committed"}`,
		`{"type":"response.created","response":{"id":"response-1"}}`,
	}
	var kinds []EventKind
	for _, body := range raw {
		if event, ok := translateOpenAIEvent([]byte(body)); ok {
			kinds = append(kinds, event.Kind)
		}
	}
	want := []EventKind{EventSpeechStopped, EventInputCommitted, EventResponseStarted}
	if !reflect.DeepEqual(kinds, want) {
		t.Fatalf("translated event kinds = %v, want %v", kinds, want)
	}
}

func TestTranslateOpenAIInputCorrelationEvents(t *testing.T) {
	tests := []struct {
		name  string
		raw   string
		kind  EventKind
		check func(Event) bool
	}{
		{name: "speech started", raw: `{"type":"input_audio_buffer.speech_started","item_id":"item-1","audio_start_ms":120}`, kind: EventSpeechStarted, check: func(event Event) bool { return event.AudioStartMS == 120 }},
		{name: "speech stopped", raw: `{"type":"input_audio_buffer.speech_stopped","item_id":"item-1","audio_end_ms":840}`, kind: EventSpeechStopped, check: func(event Event) bool { return event.AudioEndMS == 840 }},
		{name: "input committed", raw: `{"type":"input_audio_buffer.committed","item_id":"item-1","previous_item_id":"item-0"}`, kind: EventInputCommitted, check: func(event Event) bool { return event.PreviousItemID == "item-0" }},
		{name: "transcription failed", raw: `{"type":"conversation.item.input_audio_transcription.failed","item_id":"item-1","error":{"code":"audio_unintelligible","message":"could not transcribe"}}`, kind: EventTranscriptFailed, check: func(event Event) bool {
			return event.Error != nil && strings.Contains(event.Error.Error(), "audio_unintelligible") && strings.Contains(event.Error.Error(), "could not transcribe")
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			event, ok := translateOpenAIEvent([]byte(tc.raw))
			if !ok || event.Kind != tc.kind || event.ItemID != "item-1" || !tc.check(event) {
				t.Fatalf("event=%+v ok=%t, want kind=%s item=item-1 with diagnostic fields", event, ok, tc.kind)
			}
		})
	}
}
