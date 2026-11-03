package executor

import (
	"context"
	"errors"
	"testing"

	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"

	"github.com/tmc/langchaingo/llms"
)

type recordingModel struct {
	last []llms.MessageContent
}

type nilResponseModel struct{}

func (nilResponseModel) GenerateContent(context.Context, []llms.MessageContent, ...llms.CallOption) (*llms.ContentResponse, error) {
	return nil, nil
}

func (nilResponseModel) Call(context.Context, string, ...llms.CallOption) (string, error) {
	return "", nil
}

func (m *recordingModel) GenerateContent(ctx context.Context, messages []llms.MessageContent, options ...llms.CallOption) (*llms.ContentResponse, error) {
	m.last = messages
	return &llms.ContentResponse{Choices: []*llms.ContentChoice{{Content: "ok"}}}, nil
}

func (m *recordingModel) Call(ctx context.Context, prompt string, options ...llms.CallOption) (string, error) {
	return "ok", nil
}

type dropUserTransform struct{}

func (dropUserTransform) Transform(messageList []messages.Message) []messages.Message {
	out := make([]messages.Message, 0, len(messageList))
	for _, msg := range messageList {
		if msg.Role == messages.MessageRoleUser {
			continue
		}
		out = append(out, msg)
	}
	return out
}

func TestGenerateContentAppliesOutboundTransformsWithoutMutatingStore(t *testing.T) {
	sessionFolder := t.TempDir()
	manager, err := contextmanager.NewContextManagerFromMessageList(sessionFolder, nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleSystem, Content: "sys"}); err != nil {
		t.Fatalf("append system: %v", err)
	}
	if err := manager.AppendMessage(messages.Message{Role: messages.MessageRoleUser, Content: "user"}); err != nil {
		t.Fatalf("append user: %v", err)
	}

	model := &recordingModel{}
	exec := NewLLMExecutor(model, manager, dropUserTransform{})
	if _, err := exec.GenerateContent(context.Background()); err != nil {
		t.Fatalf("GenerateContent() error = %v", err)
	}
	if len(model.last) != 1 || model.last[0].Role != llms.ChatMessageTypeSystem {
		t.Fatalf("model messages = %#v, want only system", model.last)
	}
	dump := manager.MessageListDump()
	if len(dump.Messages) != 2 {
		t.Fatalf("persisted messages = %d, want 2", len(dump.Messages))
	}
}

func TestGenerateContentMarksNilModelResponseAsLLMCallError(t *testing.T) {
	manager, err := contextmanager.NewContextManagerFromMessageList(t.TempDir(), nil)
	if err != nil {
		t.Fatalf("NewContextManagerFromMessageList() error = %v", err)
	}
	exec := NewLLMExecutor(nilResponseModel{}, manager)

	response, err := exec.GenerateContent(context.Background())
	if response != nil {
		t.Fatalf("GenerateContent() response = %#v, want nil", response)
	}
	var llmErr *LLMCallError
	if !errors.As(err, &llmErr) {
		t.Fatalf("GenerateContent() error = %T %v, want LLMCallError", err, err)
	}
}
