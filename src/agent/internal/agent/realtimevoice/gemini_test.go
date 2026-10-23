package realtimevoice

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"net/http/httptest"
)

func TestGeminiProviderNormalizesLiveSession(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	clientContents := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.URL.Query().Get("key"); got != "gemini-key" {
			t.Errorf("api key query = %q", got)
		}
		if got := r.URL.Query().Get("model"); got != "" {
			t.Errorf("unexpected model query = %q", got)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, body, err := conn.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		var setup map[string]any
		if err := json.Unmarshal(body, &setup); err != nil {
			t.Error(err)
			return
		}
		payload, ok := setup["setup"].(map[string]any)
		if !ok || payload["model"] != "models/gemini-3.1-flash-live-preview" {
			t.Errorf("setup = %#v", setup)
		}
		if _, ok := payload["tools"]; !ok {
			t.Errorf("setup missing tools = %#v", setup)
		}
		if err := conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}}); err != nil {
			t.Error(err)
			return
		}
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
			switch {
			case event["realtimeInput"] != nil:
				input := event["realtimeInput"].(map[string]any)
				audio := input["audio"].(map[string]any)
				if audio["mimeType"] != "audio/pcm;rate=16000" {
					t.Errorf("audio = %#v", audio)
				}
				_ = conn.WriteJSON(map[string]any{"serverContent": map[string]any{
					"modelTurn":           map[string]any{"parts": []any{map[string]any{"inlineData": map[string]any{"mimeType": "audio/pcm;rate=24000", "data": base64.StdEncoding.EncodeToString([]byte{4, 5})}}}},
					"outputTranscription": map[string]any{"text": "hello", "finished": false},
				}})
				_ = conn.WriteJSON(map[string]any{"toolCall": map[string]any{"functionCalls": []any{map[string]any{"id": "call_1", "name": "clock", "args": map[string]any{"timezone": "UTC"}}}}})
			case event["toolResponse"] != nil:
				response := event["toolResponse"].(map[string]any)
				if len(response["functionResponses"].([]any)) != 1 {
					t.Errorf("tool response = %#v", event)
				}
				_ = conn.WriteJSON(map[string]any{"serverContent": map[string]any{"turnComplete": true}, "usageMetadata": map[string]any{"promptTokenCount": 3, "responseTokenCount": 4, "totalTokenCount": 7}})
			case event["clientContent"] != nil:
				clientContents <- event["clientContent"].(map[string]any)
				_ = conn.WriteJSON(map[string]any{"serverContent": map[string]any{"turnComplete": true}})
			}
		}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/live"
	session, err := (GeminiProvider{Endpoint: endpoint}).Open(context.Background(), SessionConfig{
		APIKey: "gemini-key", Model: "gemini-3.1-flash-live-preview", Voice: "Puck", Instructions: "be concise",
		Tools: []Tool{{Name: "clock", Parameters: json.RawMessage(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if got := session.Info(); got.ID != "" || got.InputSampleRate != 16000 || got.OutputSampleRate != 24000 || got.InputAudioFormat != (AudioFormat{Encoding: "pcm_s16le", SampleRate: 16000, Channels: 1, BitDepth: 16}) || got.OutputAudioFormat != (AudioFormat{Encoding: "pcm_s16le", SampleRate: 24000, Channels: 1, BitDepth: 16}) {
		t.Fatalf("session info = %+v", got)
	}
	if err := session.SendAudio(context.Background(), []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	if err := session.(ToolResultSender).SendToolResult(context.Background(), "call_1", `{"ok":true}`); err != nil {
		t.Fatal(err)
	}

	want := []EventKind{EventResponseStarted, EventTranscriptDelta, EventAudio, EventToolCall, EventUsage, EventResponseDone}
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
	replayer, ok := session.(ContextReplayer)
	if !ok {
		t.Fatal("Gemini session does not support context replay")
	}
	if err := replayer.ReplayContext(context.Background(), []ContextItem{
		{Type: "message", Role: "user", Content: "question"},
		{Type: "message", Role: "assistant", Content: "answer"},
		{Type: "function_call", CallID: "call_old", Name: "clock", Arguments: `{"timezone":"UTC"}`},
		{Type: "function_call_output", CallID: "call_old", Output: `{"time":"12:00"}`},
	}); err != nil {
		t.Fatal(err)
	}
	select {
	case content := <-clientContents:
		turns, ok := content["turns"].([]any)
		if !ok || len(turns) != 4 || content["turnComplete"] != false {
			t.Fatalf("replayed client content = %#v", content)
		}
		if turns[0].(map[string]any)["role"] != "user" || turns[1].(map[string]any)["role"] != "model" {
			t.Fatalf("replayed message roles = %#v", turns)
		}
		functionCall := turns[2].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionCall"].(map[string]any)
		if functionCall["id"] != "call_old" || functionCall["name"] != "clock" {
			t.Fatalf("replayed function call = %#v", functionCall)
		}
		functionResponse := turns[3].(map[string]any)["parts"].([]any)[0].(map[string]any)["functionResponse"].(map[string]any)
		if functionResponse["id"] != "call_old" || functionResponse["name"] != "clock" {
			t.Fatalf("replayed function response = %#v", functionResponse)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for replayed Gemini context")
	}
}

func TestGeminiSessionUsesServerAuthoritativeCapabilities(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		if _, _, err := conn.ReadMessage(); err != nil {
			return
		}
		_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/live"
	for _, tc := range []struct {
		model             string
		wantInterruption  bool
		wantTurnDetection bool
	}{
		{model: Gemini38ThinkingModel, wantInterruption: true, wantTurnDetection: true},
		{model: Gemini38LiveModel, wantInterruption: true},
		{model: DefaultGeminiLiveModel, wantInterruption: true},
	} {
		t.Run(tc.model, func(t *testing.T) {
			session, err := (GeminiProvider{Endpoint: endpoint}).Open(context.Background(), SessionConfig{APIKey: "test", Model: tc.model})
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()
			if got := session.Info().Capabilities.ServerAuthoritativeInterruption; got != tc.wantInterruption {
				t.Fatalf("ServerAuthoritativeInterruption = %t, want %t", got, tc.wantInterruption)
			}
			if got := session.Info().Capabilities.ServerAuthoritativeTurnDetection; got != tc.wantTurnDetection {
				t.Fatalf("ServerAuthoritativeTurnDetection = %t, want %t", got, tc.wantTurnDetection)
			}
		})
	}
}

func TestGemini38ExtendedThinkingModelClassificationIsExact(t *testing.T) {
	for _, model := range []string{
		Gemini38ThinkingModel,
		"models/" + Gemini38ThinkingModel,
	} {
		if !IsGemini38ExtendedThinkingModel(model) {
			t.Fatalf("model %q was not classified as Gemini 3.8 Extended Thinking", model)
		}
	}
	for _, model := range []string{
		Gemini38LiveModel,
		DefaultGeminiLiveModel,
		Gemini38ThinkingModel + "-preview",
	} {
		if IsGemini38ExtendedThinkingModel(model) {
			t.Fatalf("model %q was incorrectly classified as Gemini 3.8 Extended Thinking", model)
		}
	}
}

func TestGeminiVertexUsesOAuthHeaderAndVertexModelResource(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer vertex-token" {
			t.Errorf("authorization = %q", got)
		}
		if r.URL.Query().Get("key") != "" || r.URL.Query().Get("access_token") != "" {
			t.Errorf("Vertex credential leaked into query: %s", r.URL.RawQuery)
		}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer conn.Close()
		_, body, err := conn.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		var setup map[string]any
		if err := json.Unmarshal(body, &setup); err != nil {
			t.Error(err)
			return
		}
		payload := setup["setup"].(map[string]any)
		wantModel := "projects/project-1/locations/us-central1/publishers/google/models/gemini-live"
		if payload["model"] != wantModel {
			t.Errorf("Vertex model = %q, want %q", payload["model"], wantModel)
		}
		_ = conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})
		// Hold the connection open until the client closes it. Returning here
		// drops the socket without a close frame, and the client may observe
		// that abnormal closure before setupComplete.
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/ws/google.cloud.aiplatform.v1beta1.LlmBidiService/BidiGenerateContent"
	session, err := (GeminiProvider{
		Endpoint: endpoint, AuthMode: "vertex", ProjectID: "project-1", Location: "us-central1",
	}).Open(context.Background(), SessionConfig{APIKey: "vertex-token", Model: "gemini-live"})
	if err != nil {
		t.Fatal(err)
	}
	_ = session.Close()

	provider := GeminiProvider{AuthMode: "vertex", ProjectID: "project-1", Location: "europe-west4"}
	got, err := provider.endpoint("gemini-live", "vertex-token")
	if err != nil {
		t.Fatal(err)
	}
	if got != "wss://europe-west4-aiplatform.googleapis.com/ws/google.cloud.aiplatform.v1beta1.LlmBidiService/BidiGenerateContent" {
		t.Fatalf("default Vertex endpoint = %q", got)
	}
}

func TestGeminiSetupUsesJSONSchemaToolParameterField(t *testing.T) {
	inputSchema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"point":{"type":"object","additionalProperties":false,"properties":{"x":{"type":"number"},"y":{"type":"number"}},"required":["x","y"]}},"required":["point"]}`)
	setup := buildGeminiSetup(SessionConfig{Tools: []Tool{{Name: "touch", Parameters: inputSchema}}}, "gemini-3.1-flash-live-preview")
	encoded, err := json.Marshal(setup)
	if err != nil {
		t.Fatalf("marshal setup: %v", err)
	}
	var decoded struct {
		Setup struct {
			Tools []struct {
				FunctionDeclarations []struct {
					Parameters           json.RawMessage `json:"parameters"`
					ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
				} `json:"functionDeclarations"`
			} `json:"tools"`
		} `json:"setup"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode setup: %v", err)
	}
	declaration := decoded.Setup.Tools[0].FunctionDeclarations[0]
	if len(declaration.Parameters) != 0 {
		t.Fatalf("restricted parameters field must not be used: %s", declaration.Parameters)
	}
	if string(declaration.ParametersJSONSchema) != string(inputSchema) {
		t.Fatalf("parametersJsonSchema = %s, want %s", declaration.ParametersJSONSchema, inputSchema)
	}
}

func TestGeminiExtendedThinkingSetupUsesNonBlockingTools(t *testing.T) {
	inputSchema := json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"timezone":{"type":"string"},"offset":{"type":"number"}},"required":["timezone"]}`)
	setup := buildGeminiSetup(SessionConfig{Tools: []Tool{{
		Name:       "clock",
		Parameters: inputSchema,
	}}}, "gemini-3.8-live-extended-thinking")
	if setup.Setup.GenerationConfig.ThinkingConfig == nil || setup.Setup.GenerationConfig.ThinkingConfig.ThinkingLevel != "LOW" {
		t.Fatalf("thinking config = %+v, want LOW", setup.Setup.GenerationConfig.ThinkingConfig)
	}
	declaration := setup.Setup.Tools[0].FunctionDeclarations[0]
	if declaration.Behavior != "NON_BLOCKING" {
		t.Fatalf("tool behavior = %q, want NON_BLOCKING", declaration.Behavior)
	}
	encoded, err := json.Marshal(declaration)
	if err != nil {
		t.Fatal(err)
	}
	var fields struct {
		Parameters           map[string]any  `json:"parameters"`
		ParametersJSONSchema json.RawMessage `json:"parametersJsonSchema"`
	}
	if err := json.Unmarshal(encoded, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields.ParametersJSONSchema) != 0 {
		t.Fatalf("extended-thinking declaration used parametersJsonSchema: %s", fields.ParametersJSONSchema)
	}
	if fields.Parameters["type"] != "OBJECT" {
		t.Fatalf("parameters.type = %#v, want OBJECT", fields.Parameters["type"])
	}
	if _, ok := fields.Parameters["additionalProperties"]; ok {
		t.Fatalf("parameters retained unsupported additionalProperties: %#v", fields.Parameters)
	}
	properties, ok := fields.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatalf("parameters.properties = %#v", fields.Parameters["properties"])
	}
	if properties["timezone"].(map[string]any)["type"] != "STRING" || properties["offset"].(map[string]any)["type"] != "NUMBER" {
		t.Fatalf("parameter property types were not converted to Gemini enums: %#v", properties)
	}

	for _, model := range []string{Gemini38LiveModel, DefaultGeminiLiveModel} {
		regular := buildGeminiSetup(SessionConfig{Tools: []Tool{{Name: "clock"}}}, model)
		if regular.Setup.GenerationConfig.ThinkingConfig != nil {
			t.Fatalf("Gemini model %q unexpectedly received thinking config: %+v", model, regular.Setup.GenerationConfig.ThinkingConfig)
		}
		if behavior := regular.Setup.Tools[0].FunctionDeclarations[0].Behavior; behavior != "" {
			t.Fatalf("Gemini model %q tool behavior = %q, want provider default", model, behavior)
		}
	}
}

func TestGeminiSetupRequestsBothTranscriptions(t *testing.T) {
	// Gemini Live only returns transcripts when these keys are present, and the
	// request for defaults is an empty object. With omitempty the empty map was
	// dropped, so no inputTranscription ever arrived and spoken user turns were
	// missing from history while assistant turns still worked.
	encoded, err := json.Marshal(buildGeminiSetup(SessionConfig{}, "gemini-3.1-flash-live-preview"))
	if err != nil {
		t.Fatalf("marshal setup: %v", err)
	}
	var decoded struct {
		Setup map[string]json.RawMessage `json:"setup"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatalf("decode setup: %v", err)
	}
	for _, key := range []string{"inputAudioTranscription", "outputAudioTranscription"} {
		raw, present := decoded.Setup[key]
		if !present {
			t.Fatalf("setup is missing %s: %s", key, encoded)
		}
		if string(raw) != "{}" {
			t.Fatalf("%s = %s, want {}", key, raw)
		}
	}
	if got := decoded.Setup["model"]; string(got) != `"models/gemini-3.1-flash-live-preview"` {
		t.Fatalf("model = %s", got)
	}
}

func TestGeminiFinalizesUserTranscriptAtTurnBoundary(t *testing.T) {
	// Gemini Live never sets finished on inputTranscription, so a final user
	// transcript can only be produced at the turn boundary. Frame shapes here
	// match what api.google.com sent on hardware.
	s := &geminiSession{toolNames: map[string]string{}}
	for _, frame := range []string{
		`{"serverContent":{"inputTranscription":{"text":"今天"}}}`,
		`{"serverContent":{"inputTranscription":{"text":"是几号？"}}}`,
	} {
		for _, event := range s.translate([]byte(frame)) {
			if event.Kind == EventTranscriptFinal {
				t.Fatalf("no final transcript is possible before the turn ends: %+v", event)
			}
		}
	}
	events := s.translate([]byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"hi"}]}}}`))
	var finalAt, startedAt = -1, -1
	for i, event := range events {
		switch {
		case event.Kind == EventTranscriptFinal && event.Role == "user":
			if event.Text != "今天是几号？" {
				t.Fatalf("final user transcript = %q, want the joined deltas", event.Text)
			}
			finalAt = i
		case event.Kind == EventResponseStarted:
			startedAt = i
		}
	}
	if finalAt < 0 {
		t.Fatalf("modelTurn did not finalize the user transcript: %+v", events)
	}
	if startedAt < 0 || finalAt > startedAt {
		t.Fatalf("user transcript must precede EventResponseStarted, got final=%d started=%d", finalAt, startedAt)
	}
	if _, ok := s.finalUserTranscriptEvent(); ok {
		t.Fatal("accumulated transcript must be cleared after finalizing")
	}
}

func TestGeminiIgnoresLateTranscriptRefinementDuringResponse(t *testing.T) {
	// modelTurn repeats per audio chunk and Gemini keeps refining the transcript
	// after the answer starts. Observed on hardware: one spoken sentence produced
	// two user records, the second a wrong-language re-transcription.
	s := &geminiSession{toolNames: map[string]string{}}
	finals := 0
	countFinals := func(events []Event) {
		for _, event := range events {
			if event.Kind == EventTranscriptFinal && event.Role == "user" {
				finals++
				if event.Text != "我说的是中文" {
					t.Fatalf("final user transcript = %q", event.Text)
				}
			}
		}
	}
	countFinals(s.translate([]byte(`{"serverContent":{"inputTranscription":{"text":"我说的是中文"}}}`)))
	countFinals(s.translate([]byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"a"}]}}}`)))
	countFinals(s.translate([]byte(`{"serverContent":{"inputTranscription":{"text":"o assunto é chinês"}}}`)))
	countFinals(s.translate([]byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"b"}]}}}`)))
	countFinals(s.translate([]byte(`{"serverContent":{"turnComplete":true}}`)))
	if finals != 1 {
		t.Fatalf("got %d final user transcripts for one spoken turn, want 1", finals)
	}
}

func TestGeminiInterruptionStartsNewUserTurn(t *testing.T) {
	// Speech that cuts off the model must still be recorded, so an interruption
	// has to clear the in-flight response and let accumulation resume.
	s := &geminiSession{toolNames: map[string]string{}}
	s.translate([]byte(`{"serverContent":{"inputTranscription":{"text":"first"}}}`))
	s.translate([]byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"answer"}]}}}`))
	s.translate([]byte(`{"serverContent":{"interrupted":true}}`))
	s.translate([]byte(`{"serverContent":{"inputTranscription":{"text":"cut in"}}}`))
	var got string
	for _, event := range s.translate([]byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"next"}]}}}`)) {
		if event.Kind == EventTranscriptFinal && event.Role == "user" {
			got = event.Text
		}
	}
	if got != "cut in" {
		t.Fatalf("interrupting speech was not recorded as a new turn, got %q", got)
	}
}

func TestGeminiSameFrameInterruptionKeepsInputTranscript(t *testing.T) {
	s := &geminiSession{toolNames: map[string]string{}}
	s.translate([]byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"answer"}]}}}`))
	events := s.translate([]byte(`{"serverContent":{"interrupted":true,"inputTranscription":{"text":"cut in"}}}`))
	if len(events) != 2 || events[0].Kind != EventInterruption || events[1].Kind != EventTranscriptDelta {
		t.Fatalf("same-frame events = %+v, want interruption then user transcript", events)
	}
	var got string
	for _, event := range s.translate([]byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"next"}]}}}`)) {
		if event.Kind == EventTranscriptFinal && event.Role == "user" {
			got = event.Text
		}
	}
	if got != "cut in" {
		t.Fatalf("interrupting transcript = %q, want cut in", got)
	}
}

func TestGeminiToolCallFlushesUserTranscriptFirst(t *testing.T) {
	s := &geminiSession{toolNames: map[string]string{}}
	s.translate([]byte(`{"serverContent":{"inputTranscription":{"text":"what time is it"}}}`))
	events := s.translate([]byte(`{"toolCall":{"functionCalls":[{"id":"call_1","name":"clock","args":{}}]}}`))
	if len(events) != 3 {
		t.Fatalf("tool-call events = %+v", events)
	}
	if events[0].Kind != EventTranscriptFinal || events[0].Role != "user" || events[1].Kind != EventResponseStarted || events[2].Kind != EventToolCall {
		t.Fatalf("tool-call event order = %+v", events)
	}
}

func TestGeminiToolCancellationPreservesCallIDsAndResponse(t *testing.T) {
	s := &geminiSession{responseActive: true, toolNames: map[string]string{"a": "clock", "b": "search", "c": "clock"}}
	events := s.translate([]byte(`{"toolCallCancellation":{"ids":["a","c"]}}`))
	if len(events) != 2 || events[0].Kind != EventToolCallCancelled || events[0].CallID != "a" || events[1].Kind != EventToolCallCancelled || events[1].CallID != "c" {
		t.Fatalf("cancellation events = %+v", events)
	}
	if !s.responseActive || len(s.toolNames) != 1 || s.toolNames["b"] != "search" {
		t.Fatalf("cancellation changed unrelated state: active=%t tools=%v", s.responseActive, s.toolNames)
	}
	for _, payload := range []string{`{"toolCallCancellation":{}}`, `{"toolCallCancellation":{"ids":[""]}}`} {
		if events := s.translate([]byte(payload)); len(events) != 0 {
			t.Fatalf("empty cancellation emitted events: %+v", events)
		}
	}
}

func TestGeminiGoAwayReportsRotationNotFailure(t *testing.T) {
	// Gemini Live caps session lifetime and announces the cutoff. Reporting a
	// generic error made a scheduled handover look like a fault, so long
	// conversations ended with no reconnect.
	s := &geminiSession{toolNames: map[string]string{}}
	events := s.translate([]byte(`{"goAway":{"timeLeft":"50s"}}`))
	if len(events) != 1 || events[0].Kind != EventError {
		t.Fatalf("events = %+v", events)
	}
	err := events[0].Error
	if !errors.Is(err, ErrSessionRotated) {
		t.Fatalf("goAway must wrap ErrSessionRotated so callers can reconnect, got %v", err)
	}
	if !strings.Contains(err.Error(), "50s") {
		t.Fatalf("error should keep the announced budget, got %v", err)
	}
}

func TestGeminiInterruptedTurnCompleteIsCancelled(t *testing.T) {
	for _, sameFrame := range []bool{false, true} {
		s := &geminiSession{toolNames: map[string]string{}}
		s.translate([]byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"answer"}]}}}`))
		var events []Event
		if sameFrame {
			events = s.translate([]byte(`{"serverContent":{"interrupted":true,"turnComplete":true}}`))
		} else {
			s.translate([]byte(`{"serverContent":{"interrupted":true}}`))
			events = s.translate([]byte(`{"serverContent":{"turnComplete":true}}`))
		}
		last := events[len(events)-1]
		if last.Kind != EventResponseCancelled || last.Status != "cancelled" {
			t.Fatalf("sameFrame=%t: terminal=%+v", sameFrame, last)
		}
		s.translate([]byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"next"}]}}}`))
		events = s.translate([]byte(`{"serverContent":{"turnComplete":true}}`))
		if events[len(events)-1].Kind != EventResponseDone {
			t.Fatal("cancellation leaked into next turn")
		}
	}
}

func TestGeminiInterruptSendsClientContentWithoutStartingNewTurn(t *testing.T) {
	received := make(chan map[string]any, 1)
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		var setup map[string]any
		if conn.ReadJSON(&setup) != nil {
			return
		}
		conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})
		var message map[string]any
		if conn.ReadJSON(&message) != nil {
			return
		}
		received <- message
		conn.WriteJSON(map[string]any{"serverContent": map[string]any{"interrupted": true}})
		conn.WriteJSON(map[string]any{"serverContent": map[string]any{"turnComplete": true}})
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	session, err := (GeminiProvider{Endpoint: server.URL}).Open(ctx, SessionConfig{APIKey: "test"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if timeout := session.(InterruptionAckTimeoutProvider).InterruptionAckTimeout(); timeout != 3*time.Second {
		t.Fatalf("interruption acknowledgement timeout=%s, want 3s", timeout)
	}
	if err := session.(ResponseInterrupter).Interrupt(ctx, ResponseInterruption{}); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-received:
		data, _ := json.Marshal(message)
		if string(data) != `{"clientContent":{"turnComplete":false}}` {
			t.Fatalf("interrupt wire payload=%s", data)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for _, want := range []EventKind{EventInterruption, EventResponseCancelled} {
		select {
		case event := <-session.Events():
			if event.Kind != want {
				t.Fatalf("event=%+v want=%s", event, want)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
func TestGeminiExtendedThinkingLifecycleUsesModelCapability(t *testing.T) {
	s := &geminiSession{toolNames: map[string]string{}, extendedThinking: true}
	started := s.translate([]byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"checking"}]}}}`))
	if len(started) == 0 || started[0].Kind != EventResponseStarted {
		t.Fatalf("filler events = %+v, want started", started)
	}
	inProgress := s.translate([]byte(`{"serverContent":{"turnComplete":true,"interactionStatus":"IN_PROGRESS"}}`))
	if len(inProgress) != 0 {
		t.Fatalf("IN_PROGRESS emitted terminal events: %+v", inProgress)
	}
	missingStatus := s.translate([]byte(`{"serverContent":{"turnComplete":true}}`))
	if len(missingStatus) != 0 {
		t.Fatalf("missing interactionStatus emitted terminal events: %+v", missingStatus)
	}
	requiresAction := s.translate([]byte(`{"serverContent":{"turnComplete":true,"interactionStatus":"REQUIRES_ACTION"}}`))
	if requiresAction[len(requiresAction)-1].Kind != EventResponseDone {
		t.Fatalf("REQUIRES_ACTION terminal = %+v, want done", requiresAction)
	}
	if s.responseActive || s.responseInterrupted {
		t.Fatal("REQUIRES_ACTION left the response open")
	}
}

func TestGeminiRegularModelIgnoresExtendedThinkingStatus(t *testing.T) {
	s := &geminiSession{toolNames: map[string]string{}}
	started := s.translate([]byte(`{"serverContent":{"modelTurn":{"parts":[{"text":"answering"}]}}}`))
	if len(started) == 0 || started[0].Kind != EventResponseStarted {
		t.Fatalf("response events = %+v, want started", started)
	}
	done := s.translate([]byte(`{"serverContent":{"turnComplete":true,"interactionStatus":"IN_PROGRESS"}}`))
	if done[len(done)-1].Kind != EventResponseDone {
		t.Fatalf("regular model terminal = %+v, want done", done)
	}
	if s.responseActive || s.responseInterrupted {
		t.Fatal("regular model left the response open")
	}
}

func TestGeminiThinkingConfigDependsOnModelCapability(t *testing.T) {
	for _, level := range []string{"", "MEDIUM", "low", "High"} {
		setup := buildGeminiSetup(SessionConfig{ThinkingLevel: level, Tools: []Tool{}}, "gemini-3.8-live-extended-thinking")
		want := strings.ToUpper(level)
		if want == "" {
			want = "LOW"
		}
		if got := setup.Setup.GenerationConfig.ThinkingConfig; got == nil || got.ThinkingLevel != want {
			t.Fatalf("level=%q got=%+v want=%s", level, got, want)
		}
	}
	setup := buildGeminiSetup(SessionConfig{ThinkingLevel: "MEDIUM", Tools: []Tool{}}, "gemini-3.8-live")
	if setup.Setup.GenerationConfig.ThinkingConfig != nil {
		t.Fatal("regular Gemini model received thinking config")
	}
}

func TestGeminiToolResultSchedulingOnlyForExtendedThinking(t *testing.T) {
	for _, model := range []string{Gemini38ThinkingModel, DefaultGeminiLiveModel} {
		received := make(chan map[string]any, 1)
		upgrader := websocket.Upgrader{}
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			conn, err := upgrader.Upgrade(w, r, nil)
			if err != nil {
				return
			}
			defer conn.Close()
			var setup map[string]any
			if conn.ReadJSON(&setup) != nil {
				return
			}
			conn.WriteJSON(map[string]any{"setupComplete": map[string]any{}})
			var message map[string]any
			if conn.ReadJSON(&message) != nil {
				return
			}
			received <- message
			for {
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
			}
		}))
		defer server.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		session, err := (GeminiProvider{Endpoint: server.URL}).Open(ctx, SessionConfig{APIKey: "test", Model: model})
		if err != nil {
			t.Fatal(err)
		}
		defer session.Close()
		if err := session.(ToolResultSender).SendToolResult(ctx, "call_1", `{"temperature_c":27}`); err != nil {
			t.Fatal(err)
		}
		select {
		case message := <-received:
			data, _ := json.Marshal(message)
			if IsGemini38ExtendedThinkingModel(model) {
				if string(data) != `{"toolResponse":{"functionResponses":[{"id":"call_1","response":{"result":{"temperature_c":27},"scheduling":"INTERRUPT"}}]}}` {
					t.Fatalf("model=%s wire payload=%s", model, data)
				}
			} else if strings.Contains(string(data), "scheduling") {
				t.Fatalf("model=%s sent unexpected scheduling: %s", model, data)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}
