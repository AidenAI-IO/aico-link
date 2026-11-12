package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/screen"

	langtools "github.com/tmc/langchaingo/tools"
)

// turnDecisionHookRuntime returns a runtime whose state hook captures a fixed
// screenshot and asks the decision model at serverURL when enabled.
func turnDecisionHookRuntime(t *testing.T, enabled bool, serverURL string, memories *MemoryManager) (*Runtime, *contextmanager.ContextManager) {
	t.Helper()
	const width, height = 60, 120
	jpegData, err := encodeJPEG(bytes.Repeat([]byte{240, 240, 240}, width*height), width, height, screenshotJPEGQuality)
	if err != nil {
		t.Fatal(err)
	}
	shot := &ScreenshotTool{
		client: &fakeScreenshotFrameClient{
			meta: frameMetadata{Width: width, Height: height, PixelFormat: "jpeg", Bytes: uint64(len(jpegData))},
			data: jpegData,
		},
		screen: &screen.ScreenState{},
	}
	tools := &ToolSet{tools: map[string]langtools.Tool{"screenshot": shot}}
	cfg := Config{}
	cfg.Decision = DecisionModelConfig{Enabled: enabled, APIKey: "ts-key", BaseURL: serverURL}
	runtime := NewRuntimeWithDeps(cfg, nil, memories, tools, NewSkillIndex())
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatal(err)
	}
	runtime.contextManager = manager
	manager.AddAppendMessageHook(runtime.getStateHook())
	return runtime, manager
}

// turnStateMessage appends a user turn and returns the state message the hook
// placed before it, or the zero message when there is none.
func turnStateMessage(t *testing.T, manager *contextmanager.ContextManager, utterance string) messages.Message {
	t.Helper()
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: utterance}); err != nil {
		t.Fatal(err)
	}
	list := manager.MessageListDump().Messages
	if len(list) > 1 && list[0].Role == messages.MessageRoleState {
		return list[0]
	}
	return messages.Message{}
}

// Only a confident "the screen is not needed" drops the turn screenshot.
func TestStateHookTurnDecisionScreenshot(t *testing.T) {
	answer := func(p float64) string { return fmt.Sprintf(`{"answers":{"screen":{"type":"noul","noul":%g}}}`, p) }
	cases := []struct {
		name           string
		enabled        bool
		body           string
		wantScreenshot bool
		wantAsked      int
	}{
		{name: "screen needed", enabled: true, body: answer(0.95), wantScreenshot: true, wantAsked: 1},
		{name: "screen not needed", enabled: true, body: answer(0.05), wantScreenshot: false, wantAsked: 1},
		{name: "unsure keeps screenshot", enabled: true, body: answer(0.5), wantScreenshot: true, wantAsked: 1},
		{name: "failed request keeps screenshot", enabled: true, body: `not json`, wantScreenshot: true, wantAsked: 1},
		{name: "out-of-range noul keeps screenshot", enabled: true, body: answer(-0.5), wantScreenshot: true, wantAsked: 1},
		{name: "missing answer keeps screenshot", enabled: true, body: `{"answers":{}}`, wantScreenshot: true, wantAsked: 1},
		{name: "disabled asks nothing", enabled: false, body: answer(0.05), wantScreenshot: true, wantAsked: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newDecisionServer(t, tc.body)
			_, manager := turnDecisionHookRuntime(t, tc.enabled, server.URL, nil)
			state := turnStateMessage(t, manager, "what is the weather today")
			if got := server.requestCount(); got != tc.wantAsked {
				t.Fatalf("decision requests = %d, want %d", got, tc.wantAsked)
			}
			if got := len(state.Attachments) == 1; got != tc.wantScreenshot {
				t.Fatalf("screenshot attached = %v, want %v (state %#v)", got, tc.wantScreenshot, state)
			}
		})
	}
}

// A follow-up is judged together with the exchange before it, so the request
// carries the previous user turn, the tools it used and the final reply, also
// after a tool run longer than the tool-name window.
func TestStateHookTurnDecisionSendsPreviousTurn(t *testing.T) {
	server := newDecisionServer(t, `{"answers":{"screen":{"type":"noul","noul":0.95}}}`)
	_, manager := turnDecisionHookRuntime(t, true, server.URL, nil)
	run := []messages.Message{{Role: messages.MessageRoleUser, Content: "open the calculator"}}
	for i := 0; i < previousTurnTailMessages; i++ {
		id := fmt.Sprintf("c%d", i)
		run = append(run,
			messages.Message{Role: messages.MessageRoleToolCall, ToolCalls: []messages.ToolCall{{ID: id, Name: "open_app", Arguments: `{}`}}},
			messages.Message{Role: messages.MessageRoleToolResult, ToolResults: []messages.ToolResult{{ToolCallID: id, Content: "ok"}}},
		)
	}
	run = append(run, messages.Message{Role: messages.MessageRoleAssistant, Content: "The calculator is open."})
	if err := manager.AppendMessages(run); err != nil {
		t.Fatal(err)
	}
	server.mu.Lock()
	server.requests = nil
	server.mu.Unlock()

	turnStateMessage(t, manager, "now press 7 times 8")
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.requests) != 1 {
		t.Fatalf("decision requests = %d, want 1", len(server.requests))
	}
	body, _ := json.Marshal(server.requests[0])
	for _, want := range []string{"open the calculator", "open_app", "The calculator is open."} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("request lacks %q: %s", want, body)
		}
	}
}

// A turn with the user's own image keeps it instead of a screenshot, so the
// decision model is not asked.
func TestStateHookTurnDecisionSkippedForUserImage(t *testing.T) {
	server := newDecisionServer(t, `{"answers":{"screen":{"type":"noul","noul":0.05}}}`)
	_, manager := turnDecisionHookRuntime(t, true, server.URL, nil)
	if err := manager.AppendMessage(messages.Message{
		Role:        messages.MessageRoleUser,
		Content:     "what is in this picture",
		Attachments: []messages.Attachment{{MIMEType: "image/jpeg"}},
	}); err != nil {
		t.Fatal(err)
	}
	if got := server.requestCount(); got != 0 {
		t.Fatalf("decision requests = %d, want 0", got)
	}
}

// A decision model slower than turnDecisionTimeout delays the turn by at most
// the timeout and keeps the screenshot.
func TestStateHookTurnDecisionTimeoutKeepsScreenshot(t *testing.T) {
	previous := turnDecisionTimeout
	turnDecisionTimeout = 100 * time.Millisecond
	t.Cleanup(func() { turnDecisionTimeout = previous })
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(server.Close)
	t.Cleanup(func() { close(release) })

	_, manager := turnDecisionHookRuntime(t, true, server.URL, nil)
	started := time.Now()
	state := turnStateMessage(t, manager, "what is the weather today")
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("turn waited %v for a stalled decision model", elapsed)
	}
	if len(state.Attachments) != 1 {
		t.Fatalf("screenshot attached = %d, want 1", len(state.Attachments))
	}
}
