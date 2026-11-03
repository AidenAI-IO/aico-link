package agent

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"aico-agent/internal/agent/agentpath"
	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
	"aico-agent/internal/agent/model"

	"github.com/tmc/langchaingo/llms"
	langtools "github.com/tmc/langchaingo/tools"
)

// screenshotStateConversation builds finished turns that each start with a
// <state> carrying a screenshot, the shape the state hook leaves behind.
func screenshotStateConversation(t *testing.T, folder string, turns int) *contextmanager.ContextManager {
	t.Helper()
	manager, err := contextmanager.NewContextManagerFromMessageList(folder, []messages.Message{
		{Role: messages.MessageRoleSystem, Content: "Answer directly."},
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < turns; i++ {
		shot, err := manager.StoreAttachment("image/jpeg", []byte("jpeg"))
		if err != nil {
			t.Fatal(err)
		}
		shot.Source = messages.AttachmentSourceScreenshotObservation
		for _, message := range []messages.Message{
			{Role: messages.MessageRoleState, Content: fmt.Sprintf("app_state: turn-%d", i), Attachments: []messages.Attachment{shot}},
			{Role: messages.MessageRoleUser, Content: fmt.Sprintf("question %d", i)},
			{Role: messages.MessageRoleAssistant, Content: fmt.Sprintf("answer %d", i)},
		} {
			if err := manager.AppendMessage(message); err != nil {
				t.Fatal(err)
			}
		}
	}
	return manager
}

func TestScreenshotStateCountCountsOnlyStatesWithScreenshots(t *testing.T) {
	manager := screenshotStateConversation(t, t.TempDir(), 3)
	shot, err := manager.StoreAttachment("image/jpeg", []byte("jpeg"))
	if err != nil {
		t.Fatal(err)
	}
	shot.Source = messages.AttachmentSourceScreenshotObservation
	for _, message := range []messages.Message{
		{Role: messages.MessageRoleState, Content: "app_state: text only"},
		{Role: messages.MessageRoleUser, Content: "user image", Attachments: []messages.Attachment{shot}},
	} {
		if err := manager.AppendMessage(message); err != nil {
			t.Fatal(err)
		}
	}
	if got := manager.ScreenshotStateCount(); got != 3 {
		t.Fatalf("ScreenshotStateCount() = %d, want 3", got)
	}
}

func runWithScreenshotStates(t *testing.T, turns, limit int) (before, after string, states int) {
	t.Helper()
	configDir := ensureTestConfigDir(t, t.TempDir())
	manager := screenshotStateConversation(t, agentpath.ContextManagerSessionFolder(configDir), turns)
	runtime := NewRuntimeWithDeps(
		Config{ConfigDir: configDir, ContextPruneStateLimit: limit, Model: ModelConfig{Provider: "fake", MaxResponseTokens: 256}, Instruction: "Answer directly.", MaxIterations: 1},
		&testModelResolver{
			model: &scriptedModel{responses: []*llms.ContentResponse{contentResponse("ok")}},
			// A large window keeps token usage far below the token trigger, so
			// only the screenshot count can start a prune.
			spec: model.ModelSpec{Provider: "fake", Name: "large-window", ContextWindow: 1_000_000, MaxOutput: 256},
		},
		NewMemoryManager(""),
		&ToolSet{tools: map[string]langtools.Tool{}},
		NewSkillIndex(),
	)
	runtime.contextManager = manager
	runtime.logger = nil
	before = manager.GetSessionID()
	if _, err := runtime.Run(context.Background(), RunRequest{Input: "new question"}); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	return before, runtime.contextManager.GetSessionID(), runtime.contextManager.ScreenshotStateCount()
}

func TestRuntimeRunPrunesScreenshotStatesOverLimitBelowTokenTrigger(t *testing.T) {
	before, after, states := runWithScreenshotStates(t, defaultContextPruneStateLimit+1, 0)
	if after == before {
		t.Fatal("runtime did not switch to a state-pruned context revision")
	}
	if states != 0 {
		t.Fatalf("screenshot states after prune = %d, want 0", states)
	}
}

func TestRuntimeRunKeepsScreenshotStatesAtLimit(t *testing.T) {
	before, after, states := runWithScreenshotStates(t, defaultContextPruneStateLimit, 0)
	if after != before {
		t.Fatal("runtime pruned at the screenshot state limit; it should prune only above it")
	}
	if states != defaultContextPruneStateLimit {
		t.Fatalf("screenshot states = %d, want %d", states, defaultContextPruneStateLimit)
	}
}

func TestRuntimeRunHonorsConfiguredScreenshotStateLimit(t *testing.T) {
	before, after, states := runWithScreenshotStates(t, 4, 3)
	if after == before || states != 0 {
		t.Fatalf("context_prune_state_limit = 3 did not prune 4 screenshot states: session changed=%v states=%d", after != before, states)
	}
}

func TestContextPruneStateLimitDefaultsAndValidation(t *testing.T) {
	if got := (Config{}).ContextPruneStateLimitOrDefault(); got != defaultContextPruneStateLimit {
		t.Fatalf("unset limit = %d, want %d", got, defaultContextPruneStateLimit)
	}
	if got := (Config{ContextPruneStateLimit: 9}).ContextPruneStateLimitOrDefault(); got != 9 {
		t.Fatalf("configured limit = %d, want 9", got)
	}
	cfg := DefaultConfig()
	cfg.ContextPruneStateLimit = -1
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "context_prune_state_limit") {
		t.Fatalf("Validate() error = %v, want context_prune_state_limit error", err)
	}
}
