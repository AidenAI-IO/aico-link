package executor

import (
	"aico-agent/internal/agent/contextmanager"
	"aico-agent/internal/agent/messages"
	"context"
	"errors"
	"fmt"

	"github.com/tmc/langchaingo/llms"
)

// LLMCallError marks failures that occurred while requesting or validating an
// LLM response. Callers can distinguish these from local runtime, persistence,
// skill, and configuration failures without matching error strings.
type LLMCallError struct {
	err error
}

func MarkLLMCallError(err error) error {
	if err == nil {
		return nil
	}
	var existing *LLMCallError
	if errors.As(err, &existing) {
		return err
	}
	return &LLMCallError{err: err}
}

func (e *LLMCallError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *LLMCallError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *LLMCallError) IsLLMTurnFailureSource() {}

type LLMExecutor struct {
	model          llms.Model
	contextManager *contextmanager.ContextManager
	transforms     []OutboundMessageTransform
}

// contextMessageContentModel can consume persisted context directly when a
// wire protocol needs provider-specific opaque items in addition to the common
// LangChain message parts.
type contextMessageContentModel interface {
	GenerateContentFromMessageList(context.Context, []messages.Message, ...llms.CallOption) (*llms.ContentResponse, error)
}

func NewLLMExecutor(model llms.Model, contextManager *contextmanager.ContextManager, transforms ...OutboundMessageTransform) *LLMExecutor {
	return &LLMExecutor{
		model:          model,
		contextManager: contextManager,
		transforms:     transforms,
	}
}

func (e *LLMExecutor) ContextManager() *contextmanager.ContextManager {
	return e.contextManager
}

func (e *LLMExecutor) ReplaceContextManager(contextManager *contextmanager.ContextManager) {
	e.contextManager = contextManager
}

func (e *LLMExecutor) AppendMessage(message messages.Message) error {
	return e.contextManager.AppendMessage(message)
}

func (e *LLMExecutor) AppendMessages(messagesToAppend []messages.Message) error {
	return e.contextManager.AppendMessages(messagesToAppend)
}

func (e *LLMExecutor) GenerateContent(ctx context.Context, options ...llms.CallOption) (*llms.ContentResponse, error) {
	messageList := e.contextManager.CloneMessageList()
	for _, transform := range e.transforms {
		if transform == nil {
			continue
		}
		messageList = transform.Transform(messageList)
	}
	var contentResponse *llms.ContentResponse
	var err error
	if model, ok := e.model.(contextMessageContentModel); ok {
		contentResponse, err = model.GenerateContentFromMessageList(ctx, messageList, options...)
	} else {
		standard := messages.ConvertMessageList(messageList)
		contentResponse, err = e.model.GenerateContent(ctx, standard, options...)
	}
	if err != nil {
		return nil, MarkLLMCallError(err)
	}
	if contentResponse == nil || len(contentResponse.Choices) == 0 || contentResponse.Choices[0] == nil {
		return contentResponse, MarkLLMCallError(fmt.Errorf("model returned no choices"))
	}
	return contentResponse, nil
}

func (e *LLMExecutor) Generate(ctx context.Context, options ...llms.CallOption) (messages.Message, *llms.ContentResponse, error) {
	contentResponse, err := e.GenerateContent(ctx, options...)
	if err != nil {
		return messages.Message{}, contentResponse, err
	}
	response := messages.ConvertChoiceToContextManagerMessage(*contentResponse.Choices[0])
	if err := e.contextManager.AppendMessage(response); err != nil {
		return messages.Message{}, contentResponse, err
	}
	return response, contentResponse, nil
}

func (e *LLMExecutor) Execute(ctx context.Context, message messages.Message, options ...llms.CallOption) (messages.Message, *llms.ContentResponse, error) {
	if err := e.contextManager.AppendMessage(message); err != nil {
		return messages.Message{}, nil, err
	}
	return e.Generate(ctx, options...)
}
