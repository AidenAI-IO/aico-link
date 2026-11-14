package realtimevoice

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
)

func TestMossProviderOpenAndFakePlaybackAck(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	acks := make(chan map[string]any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer moss-key" {
			t.Errorf("authorization = %q", r.Header.Get("Authorization"))
		}
		if got := r.URL.Query().Get("model"); got != DefaultMossLiveModel {
			t.Errorf("model query = %q", got)
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
		var attach map[string]any
		if err := json.Unmarshal(body, &attach); err != nil || attach["action"] != "attach_session" {
			t.Errorf("first event = %s", body)
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"action": "session_attached",
			"data":   map[string]any{"session_id": "sess-moss-1", "memory_scope": "session", "delegation": "none"},
		})

		_, body, err = conn.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		var update map[string]any
		if err := json.Unmarshal(body, &update); err != nil {
			t.Error(err)
			return
		}
		if update["action"] != "session.update" {
			t.Errorf("second event = %#v", update)
		}
		session, _ := update["session"].(map[string]any)
		if session["instructions"] != "be brief" {
			t.Errorf("instructions = %#v", session["instructions"])
		}
		if _, ok := session["persona"]; ok {
			t.Errorf("persona should be omitted for default, got %#v", session["persona"])
		}
		_ = conn.WriteJSON(map[string]any{"action": "session.updated", "data": map[string]any{"error": ""}})

		_, body, err = conn.ReadMessage()
		if err != nil {
			t.Error(err)
			return
		}
		var mic map[string]any
		if err := json.Unmarshal(body, &mic); err != nil || mic["action"] != "mic_state" || mic["enabled"] != true {
			t.Errorf("mic event = %s", body)
			return
		}

		_ = conn.WriteJSON(map[string]any{
			"action": "input.transcript.done",
			"data":   map[string]any{"text": "你好"},
		})
		_ = conn.WriteJSON(map[string]any{
			"action": "response.audio.context",
			"data":   map[string]any{"chunk_id": "c1", "run_id": "r1", "turn_id": "t1"},
		})
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte{1, 2, 3, 4}); err != nil {
			t.Error(err)
			return
		}
		_ = conn.WriteJSON(map[string]any{
			"action": "response.audio.done",
			"data":   map[string]any{"run_id": "r1", "turn_id": "t1"},
		})
		_ = conn.WriteJSON(map[string]any{
			"action": "output.transcript.done",
			"data":   map[string]any{"text": "你好呀", "run_id": "r1", "turn_id": "t1"},
		})
		_ = conn.WriteJSON(map[string]any{
			"action": "response.ended",
			"data":   map[string]any{"status": "completed", "run_id": "r1", "turn_id": "t1"},
		})

		for {
			typ, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if typ == websocket.BinaryMessage {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal(body, &event); err != nil {
				t.Error(err)
				return
			}
			switch event["action"] {
			case "playback.chunk.completed", "playback.completed":
				acks <- event
			}
		}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"
	session, err := (MossProvider{Endpoint: endpoint}).Open(context.Background(), SessionConfig{
		APIKey: "moss-key", Instructions: "be brief",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	if session.Info().ID != "sess-moss-1" {
		t.Fatalf("session id = %q", session.Info().ID)
	}
	if session.Info().InputSampleRate != mossInputSampleRate || session.Info().OutputSampleRate != mossOutputSampleRate {
		t.Fatalf("rates = %d/%d", session.Info().InputSampleRate, session.Info().OutputSampleRate)
	}

	if err := session.SendAudio(context.Background(), []byte{9, 9}); err != nil {
		t.Fatal(err)
	}

	deadline := time.After(3 * time.Second)
	var sawAudio, sawUserFinal, sawAssistantFinal, sawDone bool
	for !(sawAudio && sawUserFinal && sawAssistantFinal && sawDone) {
		select {
		case <-deadline:
			t.Fatalf("timeout events audio=%v user=%v asst=%v done=%v", sawAudio, sawUserFinal, sawAssistantFinal, sawDone)
		case event := <-session.Events():
			switch event.Kind {
			case EventAudio:
				if string(event.PCM) != string([]byte{1, 2, 3, 4}) {
					t.Fatalf("pcm = %v", event.PCM)
				}
				sawAudio = true
			case EventTranscriptFinal:
				if event.Role == "user" && event.Text == "你好" {
					sawUserFinal = true
				}
				if event.Role == "assistant" && event.Text == "你好呀" {
					sawAssistantFinal = true
				}
			case EventResponseDone:
				if event.ResponseID != "r1" || event.Status != "completed" {
					t.Fatalf("response done = %#v", event)
				}
				sawDone = true
			}
		case err := <-session.Errors():
			t.Fatalf("session error: %v", err)
		}
	}

	chunkAck := false
	completedAck := false
	ackDeadline := time.After(3 * time.Second)
	for !(chunkAck && completedAck) {
		select {
		case <-ackDeadline:
			t.Fatalf("timeout acks chunk=%v completed=%v", chunkAck, completedAck)
		case event := <-acks:
			switch event["action"] {
			case "playback.chunk.completed":
				if event["chunk_id"] != "c1" || event["run_id"] != "r1" || event["turn_id"] != "t1" {
					t.Fatalf("chunk ack = %#v", event)
				}
				chunkAck = true
			case "playback.completed":
				if event["run_id"] != "r1" || event["turn_id"] != "t1" {
					t.Fatalf("completed ack = %#v", event)
				}
				completedAck = true
			}
		}
	}
}

func TestMossTextDelta(t *testing.T) {
	if got := mossTextDelta("", "你好"); got != "你好" {
		t.Fatalf("got %q", got)
	}
	if got := mossTextDelta("你", "你好"); got != "好" {
		t.Fatalf("got %q", got)
	}
	if got := mossTextDelta("abc", "abc"); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestMossLateContextAfterInterruptDoesNotReack(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	outbound := make(chan map[string]any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{"action": "session_attached", "data": map[string]any{"session_id": "s-late"}})
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{"action": "session.updated", "data": map[string]any{"error": ""}})
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{
			"action": "response.interrupted",
			"data":   map[string]any{"run_id": "r-old", "turn_id": "t-old"},
		})
		_ = conn.WriteJSON(map[string]any{
			"action": "response.audio.context",
			"data":   map[string]any{"chunk_id": "late-1", "run_id": "r-old", "turn_id": "t-old"},
		})
		if err := conn.WriteMessage(websocket.BinaryMessage, []byte{9, 9}); err != nil {
			return
		}
		deadline := time.Now().Add(500 * time.Millisecond)
		for time.Now().Before(deadline) {
			_ = conn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
			typ, body, err := conn.ReadMessage()
			if err != nil {
				continue
			}
			if typ == websocket.BinaryMessage {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal(body, &event); err != nil {
				continue
			}
			outbound <- event
		}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"
	session, err := (MossProvider{Endpoint: endpoint}).Open(context.Background(), SessionConfig{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case <-deadline:
			goto check
		case event := <-session.Events():
			if event.Kind == EventInterruption {
				// drain a bit for late frames
				time.Sleep(200 * time.Millisecond)
				goto check
			}
		}
	}
check:
	for {
		select {
		case event := <-outbound:
			if event["action"] == "playback.chunk.completed" {
				t.Fatalf("late chunk ack after interrupt: %#v", event)
			}
		default:
			return
		}
	}
}

func TestMossInterruptedSendsPlaybackStopped(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	stopped := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage() // attach
		_ = conn.WriteJSON(map[string]any{"action": "session_attached", "data": map[string]any{"session_id": "s-int"}})
		_, _, _ = conn.ReadMessage() // update
		_ = conn.WriteJSON(map[string]any{"action": "session.updated", "data": map[string]any{"error": ""}})
		_, _, _ = conn.ReadMessage() // mic
		_ = conn.WriteJSON(map[string]any{
			"action": "response.interrupted",
			"data":   map[string]any{"run_id": "r-int", "turn_id": "t-int", "text": "半句"},
		})
		for {
			typ, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if typ == websocket.BinaryMessage {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal(body, &event); err != nil {
				t.Error(err)
				return
			}
			if event["action"] == "playback.stopped" {
				stopped <- event
				return
			}
		}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"
	session, err := (MossProvider{Endpoint: endpoint}).Open(context.Background(), SessionConfig{APIKey: "k"})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	deadline := time.After(3 * time.Second)
	var sawInterrupt bool
	for !sawInterrupt {
		select {
		case <-deadline:
			t.Fatal("timeout waiting for interruption event")
		case event := <-session.Events():
			if event.Kind == EventInterruption && event.ResponseID == "r-int" {
				sawInterrupt = true
			}
		}
	}
	select {
	case <-deadline:
		t.Fatal("timeout waiting for playback.stopped")
	case event := <-stopped:
		if event["run_id"] != "r-int" || event["turn_id"] != "t-int" {
			t.Fatalf("playback.stopped = %#v", event)
		}
	}
}

func TestBuildMossSessionUpdateOmitsEmptyPersona(t *testing.T) {
	payload, aliases := buildMossSessionUpdate(SessionConfig{Instructions: "x"})
	if aliases != nil {
		t.Fatalf("unexpected aliases: %#v", aliases)
	}
	session := payload["session"].(map[string]any)
	if _, ok := session["persona"]; ok {
		t.Fatalf("unexpected persona: %#v", session)
	}
	if session["instructions"] != "x" {
		t.Fatalf("instructions = %#v", session["instructions"])
	}
	if _, ok := session["tools"]; ok {
		t.Fatalf("unexpected tools: %#v", session["tools"])
	}
}

func TestBuildMossSessionUpdateIncludesTools(t *testing.T) {
	payload, aliases := buildMossSessionUpdate(SessionConfig{
		Instructions: "use tools",
		Tools: []Tool{{
			Name:        "open_app",
			Description: "Open an app",
			Parameters:  json.RawMessage(`{"type":"object"}`),
		}},
	})
	if aliases != nil {
		t.Fatalf("unexpected aliases: %#v", aliases)
	}
	session := payload["session"].(map[string]any)
	tools, ok := session["tools"].([]map[string]any)
	if !ok || len(tools) != 1 {
		t.Fatalf("tools = %#v", session["tools"])
	}
	if tools[0]["type"] != "function" || tools[0]["name"] != "open_app" {
		t.Fatalf("tool = %#v", tools[0])
	}
}

func TestMossWireToolNameAliasesReservedNames(t *testing.T) {
	tools, aliases := buildMossTools([]Tool{
		{Name: "web_search", Description: "Search the web"},
		{Name: "open_app", Description: "Open an app"},
	})
	if len(tools) != 2 {
		t.Fatalf("tools = %#v", tools)
	}
	if tools[0]["name"] != "aico_web_search" {
		t.Fatalf("web_search wire name = %#v", tools[0]["name"])
	}
	if tools[1]["name"] != "open_app" {
		t.Fatalf("open_app wire name = %#v", tools[1]["name"])
	}
	if aliases["aico_web_search"] != "web_search" {
		t.Fatalf("aliases = %#v", aliases)
	}
}

func TestMossBuildToolsTrimsToAPILimit(t *testing.T) {
	input := []Tool{
		{Name: "get_current_time"},
		{Name: "open_app"},
		{Name: "web_search"},
		{Name: "skill_mark_used"},
		{Name: "skill_manage"},
		{Name: "config_update"},
	}
	for i := 0; len(input) < mossMaxClientTools+3; i++ {
		input = append(input, Tool{Name: "tool_" + strconv.Itoa(i)})
	}
	tools, aliases := buildMossTools(input)
	if len(tools) != mossMaxClientTools {
		t.Fatalf("trimmed tools = %d, want %d", len(tools), mossMaxClientTools)
	}
	names := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		name, _ := tool["name"].(string)
		names[name] = struct{}{}
	}
	for _, required := range []string{"get_current_time", "open_app", "aico_web_search"} {
		if _, ok := names[required]; !ok {
			t.Fatalf("missing required tool %q in %#v", required, names)
		}
	}
	for _, dropped := range []string{"skill_mark_used", "skill_manage", "config_update"} {
		if _, ok := names[dropped]; ok {
			t.Fatalf("low-priority tool %q should be trimmed", dropped)
		}
	}
	if aliases["aico_web_search"] != "web_search" {
		t.Fatalf("aliases = %#v", aliases)
	}
}

func TestMossFunctionCallMapsAliasedToolName(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{"action": "session_attached", "data": map[string]any{"session_id": "s-alias"}})
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{"action": "session.updated", "data": map[string]any{"error": ""}})
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{
			"action": "response.function_call_arguments.done",
			"data": map[string]any{
				"call_id": "call-ws", "name": "aico_web_search", "arguments": `{"query":"aico"}`,
				"item_id": "item-ws", "response_id": "run-ws",
			},
		})
		select {}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"
	session, err := (MossProvider{Endpoint: endpoint}).Open(context.Background(), SessionConfig{
		APIKey: "k",
		Tools:  []Tool{{Name: "web_search", Description: "Search the web"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	deadline := time.After(2 * time.Second)
	for {
		select {
		case event := <-session.Events():
			if event.Kind == EventToolCall {
				if event.Name != "web_search" {
					t.Fatalf("mapped tool name = %q", event.Name)
				}
				return
			}
		case err := <-session.Errors():
			t.Fatal(err)
		case <-deadline:
			t.Fatal("timeout waiting for aliased tool call")
		}
	}
}

func TestMossOpenAttachesClientDelegationForTools(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	attachCh := make(chan map[string]any, 1)
	updateCh := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, body, err := conn.ReadMessage()
		if err != nil {
			return
		}
		var attach map[string]any
		_ = json.Unmarshal(body, &attach)
		attachCh <- attach
		_ = conn.WriteJSON(map[string]any{"action": "session_attached", "data": map[string]any{"session_id": "s-tools", "delegation": "client"}})
		_, body, err = conn.ReadMessage()
		if err != nil {
			return
		}
		var update map[string]any
		_ = json.Unmarshal(body, &update)
		updateCh <- update
		_ = conn.WriteJSON(map[string]any{"action": "session.updated", "data": map[string]any{"error": "", "tools": []string{"open_app"}}})
		_, _, _ = conn.ReadMessage() // mic
		select {}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"
	session, err := (MossProvider{Endpoint: endpoint}).Open(context.Background(), SessionConfig{
		APIKey: "k",
		Tools:  []Tool{{Name: "open_app", Description: "Open an app"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()
	if session.Info().Capabilities.ExplicitToolContinuation {
		t.Fatal("Moss continues tool runs itself; the daemon must not request continuation")
	}
	if _, ok := session.(TextSession); ok {
		t.Fatal("Moss must not advertise TextSession without text injection")
	}
	select {
	case attach := <-attachCh:
		delegation, _ := attach["delegation"].(map[string]any)
		if attach["action"] != "attach_session" || delegation["type"] != "client" {
			t.Fatalf("attach = %#v", attach)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout attach")
	}
	select {
	case update := <-updateCh:
		sessionPayload := update["session"].(map[string]any)
		tools, ok := sessionPayload["tools"].([]any)
		if !ok || len(tools) != 1 {
			t.Fatalf("update tools = %#v", sessionPayload["tools"])
		}
		tool, _ := tools[0].(map[string]any)
		if tool["name"] != "open_app" {
			t.Fatalf("tool = %#v", tool)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timeout update")
	}
}

// The documented sync tool flow keeps the run open: the client returns each
// result, waits for conversation.item.created, then sends response.create with
// the acknowledged call IDs. response.done repeats calls and is not terminal.
func TestMossToolResultContinuesRunWithResponseCreate(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	outbound := make(chan map[string]any, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{"action": "session_attached", "data": map[string]any{"session_id": "s-fc"}})
		_, _, _ = conn.ReadMessage()
		_ = conn.WriteJSON(map[string]any{"action": "session.updated", "data": map[string]any{"error": ""}})
		_, _, _ = conn.ReadMessage()
		for _, callID := range []string{"call-1", "call-2"} {
			_ = conn.WriteJSON(map[string]any{
				"action": "response.function_call_arguments.done",
				"data": map[string]any{"call_id": callID, "name": "get_time", "arguments": `{}`,
					"item_id": "item-" + callID, "response_id": "run-1"},
			})
		}
		_ = conn.WriteJSON(map[string]any{"action": "response.done", "data": map[string]any{"response": map[string]any{
			"id": "run-1", "status": "completed", "output": []any{
				map[string]any{"type": "function_call", "call_id": "call-1", "name": "get_time", "arguments": `{}`},
				map[string]any{"type": "function_call", "call_id": "call-2", "name": "get_time", "arguments": `{}`},
			}}}})
		for {
			typ, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			if typ == websocket.BinaryMessage {
				continue
			}
			var event map[string]any
			if err := json.Unmarshal(body, &event); err != nil {
				continue
			}
			outbound <- event
			if event["action"] == "conversation.item.create" {
				item := event["item"].(map[string]any)
				_ = conn.WriteJSON(map[string]any{"action": "conversation.item.created", "data": map[string]any{
					"item": map[string]any{"type": "function_call_output", "call_id": item["call_id"]}}})
			}
		}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"
	session, err := (MossProvider{Endpoint: endpoint}).Open(context.Background(), SessionConfig{
		APIKey: "k",
		Tools:  []Tool{{Name: "get_time"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer session.Close()

	var calls []string
	deadline := time.After(2 * time.Second)
	for len(calls) < 2 {
		select {
		case <-deadline:
			t.Fatalf("tool calls = %v", calls)
		case event := <-session.Events():
			switch event.Kind {
			case EventToolCall:
				calls = append(calls, event.CallID)
			case EventResponseDone, EventResponseCancelled:
				t.Fatal("response.done must not end the run")
			}
		}
	}
	sender := session.(ToolResultSender)
	nextAction := func() map[string]any {
		select {
		case event := <-outbound:
			return event
		case <-time.After(2 * time.Second):
			t.Fatal("timeout waiting for client action")
			return nil
		}
	}
	if err := sender.SendToolResult(context.Background(), "call-1", `{"ok":1}`); err != nil {
		t.Fatal(err)
	}
	if action := nextAction()["action"]; action != "conversation.item.create" {
		t.Fatalf("first action = %v", action)
	}
	if err := sender.SendToolResult(context.Background(), "call-2", `{"ok":2}`); err != nil {
		t.Fatal(err)
	}
	if action := nextAction()["action"]; action != "conversation.item.create" {
		t.Fatalf("second action = %v, want no response.create before every result is acknowledged", action)
	}
	create := nextAction()
	if create["action"] != "response.create" {
		t.Fatalf("continuation = %#v", create)
	}
	callIDs, _ := create["response"].(map[string]any)["call_ids"].([]any)
	if len(callIDs) != 2 || callIDs[0] != "call-1" || callIDs[1] != "call-2" {
		t.Fatalf("call_ids = %#v", callIDs)
	}
	select {
	case event := <-session.Events():
		if event.Kind == EventToolCall {
			t.Fatalf("duplicate tool call from response.done: %#v", event)
		}
	case <-time.After(100 * time.Millisecond):
	}

	// A call first announced by response.done must open its response too.
	fresh := &mossSession{}
	events := fresh.onJSON([]byte(`{"action":"response.done","data":{"response":{"id":"run-9","output":[` +
		`{"type":"function_call","call_id":"call-9","name":"get_time","arguments":"{}"}]}}}`))
	if len(events) != 2 || events[0].Kind != EventResponseStarted || events[0].ResponseID != "run-9" ||
		events[1].Kind != EventToolCall || events[1].CallID != "call-9" {
		t.Fatalf("response.done-only call events = %#v", events)
	}
}

func TestMossSessionAdvertisesToolResultSender(t *testing.T) {
	var session Session = &mossSession{}
	if _, ok := session.(ToolResultSender); !ok {
		t.Fatal("mossSession must implement ToolResultSender so managed Open accepts daemon tool catalogs")
	}
	if _, ok := session.(TextSession); ok {
		t.Fatal("mossSession must not advertise TextSession until Moss exposes text injection")
	}
}

func TestMossManagedOpenAcceptsToolCatalog(t *testing.T) {
	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, _, _ = conn.ReadMessage() // attach
		_ = conn.WriteJSON(map[string]any{"action": "session_attached", "data": map[string]any{"session_id": "s1"}})
		_, _, _ = conn.ReadMessage() // update
		_ = conn.WriteJSON(map[string]any{"action": "session.updated", "data": map[string]any{"error": ""}})
		_, _, _ = conn.ReadMessage() // mic
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}))
	defer server.Close()

	endpoint := "ws" + strings.TrimPrefix(server.URL, "http") + "/v1/realtime"
	registry := NewProviderRegistry()
	registry.Register(ProviderMoss, func(c ProviderConfig) Provider {
		return MossProvider{Endpoint: c.Endpoint}
	})
	conversation, err := registry.Open(context.Background(), ProviderMoss, ProviderConfig{Endpoint: endpoint}, SessionConfig{
		APIKey: "k",
		Tools:  []Tool{{Name: "get_current_time"}},
	}, DeviceMediaConfig{})
	if err != nil {
		t.Fatal(err)
	}
	defer conversation.Close()
	if !conversation.Info().Capabilities.CanSendToolResult {
		t.Fatal("expected CanSendToolResult after ToolResultSender stub")
	}
	if conversation.Info().Capabilities.CanSendText || conversation.TextSession != nil {
		t.Fatal("Moss must remain voice-only until text injection exists")
	}
}

func TestMossLiveSmokeAgainstAPI(t *testing.T) {
	key := strings.TrimSpace(os.Getenv("MOSS_API_KEY"))
	if key == "" {
		t.Skip("set MOSS_API_KEY to run live Moss smoke")
	}
	wavPath := os.Getenv("MOSS_SMOKE_WAV")
	if wavPath == "" {
		wavPath = "/tmp/moss_live_probe/speech_16k.wav"
	}
	pcm, err := os.ReadFile(wavPath)
	if err != nil {
		t.Skipf("speech wav missing (%s): %v", wavPath, err)
	}
	if len(pcm) > 44 && string(pcm[0:4]) == "RIFF" {
		pcm = pcm[44:]
	}
	ctx, cancel := context.WithTimeout(context.Background(), 55*time.Second)
	defer cancel()
	session, err := (MossProvider{}).Open(ctx, SessionConfig{
		APIKey: key, Instructions: "请用一句很短的中文回答。",
	})
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer session.Close()
	t.Logf("session_id=%s wav=%s", session.Info().ID, filepath.Base(wavPath))

	go func() {
		const frame = 640
		for i := 0; i < len(pcm); i += frame {
			end := i + frame
			if end > len(pcm) {
				end = len(pcm)
			}
			chunk := make([]byte, frame)
			copy(chunk, pcm[i:end])
			_ = session.SendAudio(ctx, chunk)
			time.Sleep(20 * time.Millisecond)
		}
		silence := make([]byte, frame)
		for i := 0; i < 350; i++ {
			select {
			case <-ctx.Done():
				return
			default:
			}
			_ = session.SendAudio(ctx, silence)
			time.Sleep(20 * time.Millisecond)
		}
	}()

	var gotAudio, gotUser, gotDone bool
	deadline := time.After(50 * time.Second)
	for !(gotAudio && gotUser && gotDone) {
		select {
		case <-deadline:
			t.Fatalf("timeout audio=%v user=%v done=%v", gotAudio, gotUser, gotDone)
		case err := <-session.Errors():
			t.Fatalf("session error: %v", err)
		case ev, ok := <-session.Events():
			if !ok {
				t.Fatal("events closed")
			}
			switch ev.Kind {
			case EventAudio:
				gotAudio = true
			case EventTranscriptFinal:
				if ev.Role == "user" {
					gotUser = true
					t.Logf("user=%q", ev.Text)
				} else {
					t.Logf("asst=%q", ev.Text)
				}
			case EventResponseDone, EventResponseCancelled:
				gotDone = true
				t.Logf("done status=%s id=%s", ev.Status, ev.ResponseID)
			case EventError:
				t.Fatalf("event error: %v", ev.Error)
			}
		}
	}
}

func TestTranslateMossErrorEvent(t *testing.T) {
	s := &mossSession{
		label:     "moss live",
		events:    make(chan Event, 4),
		errs:      make(chan error, 1),
		done:      make(chan struct{}),
		writeGate: make(chan struct{}, 1),
		ready:     make(chan struct{}),
		updated:   make(chan struct{}),
	}
	s.writeGate <- struct{}{}
	events := s.onJSON([]byte(`{"action":"error","data":{"type":"error","error":{"code":"memory_scope_requires_user","message":"request could not be accepted"}}}`))
	if len(events) != 1 || events[0].Kind != EventError {
		t.Fatalf("events = %#v", events)
	}
	if events[0].Error == nil || !strings.Contains(events[0].Error.Error(), "memory_scope_requires_user") {
		t.Fatalf("error = %v", events[0].Error)
	}
}
