package agent

import (
	"context"
	"strings"
	"testing"

	"aiden-agent/internal/agent/contextmanager"
	"aiden-agent/internal/agent/executor"
	"aiden-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/llms"
)

type pruneRecordingModel struct {
	last []llms.MessageContent
}

func (m *pruneRecordingModel) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	m.last = messages
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: "ok"}}}, nil
}

func (m *pruneRecordingModel) Call(ctx context.Context, prompt string, options ...llms.CallOption) (string, error) {
	return "ok", nil
}

func newScreenshotObservationManager(t *testing.T, count int) *contextmanager.ContextManager {
	t.Helper()
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	for i := 0; i < count; i++ {
		stored, err := manager.StoreAttachment("image/jpeg", []byte("img"))
		if err != nil {
			t.Fatalf("StoreAttachment() error = %v", err)
		}
		stored.Source = messages.AttachmentSourceScreenshotObservation
		if err := manager.AppendMessage(messages.Message{
			Role:        messages.MessageRoleUser,
			Content:     "shot",
			Attachments: []messages.Attachment{stored},
		}); err != nil {
			t.Fatalf("AppendMessage() error = %v", err)
		}
	}
	return manager
}

func countBinaryAndOmitted(messages []llms.MessageContent) (binaryCount, omitted int) {
	for _, msg := range messages {
		for _, part := range msg.Parts {
			switch p := part.(type) {
			case llms.BinaryContent:
				binaryCount++
			case llms.TextContent:
				if strings.Contains(p.Text, "[Image omitted]") {
					omitted++
				}
			}
		}
	}
	return binaryCount, omitted
}

func TestAgentLoopExecutorPrunesScreenshotsForEveryProvider(t *testing.T) {
	// Pruning used to be enabled for Anthropic models only, so every other
	// provider resent each turn's screenshot for the whole conversation.
	for _, cfg := range []ModelConfig{
		{Provider: "anthropic", Model: "claude-test"},
		{Provider: "openrouter", Model: "anthropic/claude-test"},
		{Provider: "openrouter", Model: "openai/gpt-4o"},
		{Provider: "openai", Model: "gpt-5.5"},
		{Provider: "deepseek", Model: "deepseek-flash"},
		{Provider: "gemini", Model: "gemini-3.7-flash"},
	} {
		t.Run(cfg.Provider+"/"+cfg.Model, func(t *testing.T) {
			loop := &AgentLoop{
				Model:             &ModelManager{config: cfg},
				ScreenshotPruning: executor.ScreenshotPruningConfig{}.WithDefaults(),
			}
			manager := newScreenshotObservationManager(t, 6)
			model := &pruneRecordingModel{}
			exec := executor.NewLLMExecutor(model, manager, loop.outboundTransforms()...)
			if _, err := exec.GenerateContent(context.Background()); err != nil {
				t.Fatal(err)
			}
			binaryCount, omitted := countBinaryAndOmitted(model.last)
			if binaryCount != 4 || omitted != 2 {
				t.Fatalf("binary=%d omitted=%d messages=%#v", binaryCount, omitted, model.last)
			}
			if got := len(manager.MessageListDump().Messages); got != 6 {
				t.Fatalf("persisted message count = %d", got)
			}
			for _, msg := range manager.MessageListDump().Messages {
				if len(msg.Attachments) != 1 {
					t.Fatalf("persisted attachments mutated: %#v", msg)
				}
			}
		})
	}
}

func TestAgentLoopOutboundTransformsUseConfiguredScreenshotPruning(t *testing.T) {
	want := executor.ScreenshotPruningConfig{KeepN: 5, Interval: 4}
	loop := &AgentLoop{
		Model:             &ModelManager{config: ModelConfig{Provider: "deepseek", Model: "deepseek-flash"}},
		ScreenshotPruning: want,
	}
	transforms := loop.outboundTransforms()
	pruner, ok := transforms[0].(executor.ScreenshotPruner)
	if !ok || pruner.Config != want {
		t.Fatalf("transforms = %#v", transforms)
	}
}
