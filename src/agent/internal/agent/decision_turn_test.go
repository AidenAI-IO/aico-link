package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"slices"
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

// Confidently relevant memories are added to the turn's state message in the
// same request; unsure ones and a failed request add nothing. A preference is
// never offered: the system prompt's user profile already carries it.
func TestStateHookTurnDecisionRecallsMemories(t *testing.T) {
	answers := func(screen, recall0, recall1 float64) string {
		return fmt.Sprintf(`{"answers":{"screen":{"type":"noul","noul":%g},"recall_memory_0":{"type":"noul","noul":%g},"recall_memory_1":{"type":"noul","noul":%g}}}`, screen, recall0, recall1)
	}
	cases := []struct {
		name           string
		body           string
		wantRecalled   []string
		wantScreenshot bool
	}{
		{name: "one relevant", body: answers(0.95, 0.9, 0.1), wantRecalled: []string{"Daughter"}, wantScreenshot: true},
		{name: "both relevant", body: answers(0.95, 0.9, 0.95), wantRecalled: []string{"Daughter", "Commute"}, wantScreenshot: true},
		{name: "recalled without the screen", body: answers(0.05, 0.9, 0.1), wantRecalled: []string{"Daughter"}},
		{name: "unsure recalls nothing", body: answers(0.95, 0.6, 0.5), wantScreenshot: true},
		{name: "failed request recalls nothing", body: `not json`, wantScreenshot: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := newDecisionServer(t, tc.body)
			store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
			memories := []struct{ title, kind string }{{"Daughter", "fact"}, {"Coffee", "preference"}, {"Commute", "procedure"}}
			for i, memory := range memories {
				if _, err := store.AddMemory(context.Background(), MemoryItem{
					ID: fmt.Sprintf("mem_170000000000000000%d_test", i), Type: memory.kind, Priority: 90 - i, Confidence: 1,
					Title: memory.title, Content: memory.title + " note", EvidenceExcerpts: []string{memory.title},
				}); err != nil {
					t.Fatal(err)
				}
			}
			_, manager := turnDecisionHookRuntime(t, true, server.URL, NewMemoryManager(t.TempDir(), WithLongTermMemoryStore(store)))
			state := turnStateMessage(t, manager, "what is my daughter's name")
			for _, memory := range memories {
				want := slices.Contains(tc.wantRecalled, memory.title)
				if got := strings.Contains(state.Content, memory.title+" note"); got != want {
					t.Fatalf("memory %q in state = %v, want %v; content %q", memory.title, got, want, state.Content)
				}
			}
			if got := len(state.Attachments) == 1; got != tc.wantScreenshot {
				t.Fatalf("screenshot attached = %v, want %v", got, tc.wantScreenshot)
			}
			server.mu.Lock()
			body, _ := json.Marshal(server.requests[0])
			server.mu.Unlock()
			if strings.Contains(string(body), "Coffee note") {
				t.Fatalf("a preference was offered for recall: %s", body)
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

// A turn with the user's own image takes no screenshot, but the same
// decision request still recalls memories for it.
func TestStateHookTurnDecisionRecallsForUserImage(t *testing.T) {
	server := newDecisionServer(t, `{"answers":{"screen":{"type":"noul","noul":0.95},"recall_memory_0":{"type":"noul","noul":0.9}}}`)
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	if _, err := store.AddMemory(context.Background(), MemoryItem{
		ID: "mem_1700000000000000000_test", Type: "fact", Priority: 90, Confidence: 1,
		Title: "Daughter", Content: "Daughter note", EvidenceExcerpts: []string{"Daughter"},
	}); err != nil {
		t.Fatal(err)
	}
	_, manager := turnDecisionHookRuntime(t, true, server.URL, NewMemoryManager(t.TempDir(), WithLongTermMemoryStore(store)))
	if err := manager.AppendMessage(messages.Message{
		Role:        messages.MessageRoleUser,
		Content:     "is this my daughter's school",
		Attachments: []messages.Attachment{{MIMEType: "image/jpeg"}},
	}); err != nil {
		t.Fatal(err)
	}
	list := manager.MessageListDump().Messages
	if len(list) < 2 || list[0].Role != messages.MessageRoleState || !strings.Contains(list[0].Content, "Daughter note") {
		t.Fatalf("state message = %#v, want the recalled memory", list[0])
	}
	if len(list[0].Attachments) != 0 {
		t.Fatalf("a screenshot was attached next to the user's own image")
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

// A memory search that outlasts the turn deadline, blocked here on the
// store's write lock, does not hold up the turn.
func TestStateHookTurnDecisionDoesNotWaitForBlockedMemorySearch(t *testing.T) {
	previous := turnDecisionTimeout
	turnDecisionTimeout = 100 * time.Millisecond
	t.Cleanup(func() { turnDecisionTimeout = previous })
	server := newDecisionServer(t, `{"answers":{"screen":{"type":"noul","noul":0.95}}}`)
	store := NewLongTermMemoryStore(filepath.Join(t.TempDir(), "long_term"))
	store.writeMu.Lock()
	t.Cleanup(store.writeMu.Unlock)
	_, manager := turnDecisionHookRuntime(t, true, server.URL, NewMemoryManager(t.TempDir(), WithLongTermMemoryStore(store)))

	started := time.Now()
	state := turnStateMessage(t, manager, "what is my daughter's name")
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("turn waited %v for a blocked memory search", elapsed)
	}
	if len(state.Attachments) != 1 {
		t.Fatalf("screenshot attached = %d, want 1", len(state.Attachments))
	}
}
