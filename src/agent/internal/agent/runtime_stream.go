package agent

import (
	"context"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/stream"

	"github.com/google/uuid"
)

const streamEventEmitTimeout = 250 * time.Millisecond

// streamEventEmitter adapts legacy runtime callbacks to the provider-neutral
// event contract. It owns event identity and ordering for one run; the legacy
// callbacks remain authoritative for existing callers.
type streamEventEmitter struct {
	sink  stream.EventSink
	scope stream.EventScope

	mu       sync.Mutex
	sequence uint64
}

func newStreamEventEmitter(sink stream.EventSink, scope stream.EventScope, requestID, runID, episodeID string) *streamEventEmitter {
	if sink == nil {
		return nil
	}
	if strings.TrimSpace(scope.ConversationID) == "" {
		scope.ConversationID = strings.TrimSpace(episodeID)
		if scope.ConversationID == "" {
			scope.ConversationID = strings.TrimSpace(requestID)
		}
	}
	if scope.SessionEpoch == 0 {
		scope.SessionEpoch = 1
	}
	if strings.TrimSpace(scope.RunID) == "" {
		scope.RunID = strings.TrimSpace(runID)
	}
	if strings.TrimSpace(scope.TurnID) == "" {
		scope.TurnID = strings.TrimSpace(requestID)
	}
	return &streamEventEmitter{sink: sink, scope: scope}
}

func (e *streamEventEmitter) emit(ctx context.Context, kind stream.EventKind, visibility stream.Visibility, payload any, mutate func(*stream.EventIdentity)) {
	if e == nil || e.sink == nil {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	e.mu.Lock()
	e.sequence++
	identity := stream.EventIdentity{
		EventID:        uuid.NewString(),
		Sequence:       e.sequence,
		ConversationID: e.scope.ConversationID,
		SessionEpoch:   e.scope.SessionEpoch,
		TurnID:         e.scope.TurnID,
		RunID:          e.scope.RunID,
		ResponseID:     e.scope.ResponseID,
		AttemptID:      e.scope.AttemptID,
		ItemID:         e.scope.ItemID,
		StreamID:       e.scope.StreamID,
		OutputEpoch:    e.scope.OutputEpoch,
		CallID:         e.scope.CallID,
		TaskID:         e.scope.TaskID,
		ParentID:       e.scope.ParentID,
	}
	e.mu.Unlock()
	if mutate != nil {
		mutate(&identity)
	}
	event := stream.NewEvent(identity, kind, payload)
	event.Visibility = visibility
	event.Origin = "agent.runtime"
	emitCtx, cancel := context.WithTimeout(ctx, streamEventEmitTimeout)
	defer cancel()
	if err := e.sink.Emit(emitCtx, event); err != nil {
		// Event projections are observers. A slow or failed observer must not
		// change the legacy runtime's model/tool result or playback behavior.
		return
	}
}

func (e *streamEventEmitter) emitRunEvent(ctx context.Context, event RunEvent) {
	if e == nil {
		return
	}
	var kind stream.EventKind
	visibility := stream.VisibilityInternal
	switch event.Type {
	case "assistant_output":
		kind, visibility = stream.EventTextFinal, stream.VisibilityUser
	case "role_output":
		kind, visibility = stream.EventTextFinal, stream.VisibilityUser
	case runEventToolCall:
		kind = stream.EventToolCallReady
	case runEventToolProgress:
		kind = stream.EventToolProgress
	case "tool_result":
		kind = stream.EventToolCompleted
		if event.IsError || event.ToolError != nil {
			kind = stream.EventToolFailed
		}
	case runEventReasoningDelta:
		kind = stream.EventReasoningDelta
	case runEventReasoningReset:
		kind = stream.EventReasoningReset
	case "steer":
		kind, visibility = stream.EventTurnCommitted, stream.VisibilityInternal
	default:
		return
	}
	e.emit(ctx, kind, visibility, event, func(identity *stream.EventIdentity) {
		identity.CallID = event.ToolCallID
		identity.ItemID = event.ToolName
	})
}

func (e *streamEventEmitter) emitTextDelta(ctx context.Context, text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	e.emit(ctx, stream.EventTextDelta, stream.VisibilityUser, map[string]string{"text": text}, nil)
}

func streamErrorString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
