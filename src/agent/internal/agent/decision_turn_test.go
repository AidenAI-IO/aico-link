package agent

import (
	"bytes"
	"fmt"
	"testing"

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
