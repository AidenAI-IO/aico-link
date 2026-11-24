package realtimevoice

import (
	"context"
	"encoding/base64"
	"fmt"
	"sync"
	"time"
)

// jsonRealtimeSession owns the client events shared by JSON realtime
// providers. Endpoint authentication, session configuration, response
// interruption, and server-event dialects remain provider adapter concerns.
type jsonRealtimeSession struct {
	*jsonWebSocketTransport
	info   SessionInfo
	infoMu sync.RWMutex
}

func newJSONRealtimeSession(transport *jsonWebSocketTransport, info SessionInfo) *jsonRealtimeSession {
	return &jsonRealtimeSession{jsonWebSocketTransport: transport, info: info}
}

func (s *jsonRealtimeSession) Info() SessionInfo {
	s.infoMu.RLock()
	defer s.infoMu.RUnlock()
	return s.info
}

func (s *jsonRealtimeSession) waitReady(ctx context.Context, count int) error {
	timer := time.NewTimer(15 * time.Second)
	defer timer.Stop()
	for count > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("%s: timed out waiting for session ready: %w", s.label, context.DeadlineExceeded)
		case err, ok := <-s.errs:
			if !ok {
				return fmt.Errorf("%s: event stream closed", s.label)
			}
			if err != nil {
				return err
			}
		case event, ok := <-s.events:
			if !ok {
				return fmt.Errorf("%s: event stream closed", s.label)
			}
			if event.Kind == EventError {
				return event.Error
			}
			if event.Kind != EventReady {
				continue
			}
			if event.SessionID != "" {
				s.infoMu.Lock()
				s.info.ID = event.SessionID
				s.infoMu.Unlock()
			}
			count--
		}
	}
	return nil
}

func (s *jsonRealtimeSession) SendAudio(ctx context.Context, pcm []byte) error {
	if len(pcm) == 0 {
		return nil
	}
	return s.writeJSON(ctx, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(pcm),
	})
}

func (s *jsonRealtimeSession) Commit(ctx context.Context) error {
	return s.writeJSON(ctx, map[string]any{"type": "input_audio_buffer.commit"})
}

func (s *jsonRealtimeSession) cancelResponse(ctx context.Context) error {
	return s.writeJSON(ctx, map[string]any{"type": "response.cancel"})
}

func (s *jsonRealtimeSession) SendToolResult(ctx context.Context, id, output string) error {
	return s.writeJSON(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{"type": "function_call_output", "call_id": id, "output": output},
	})
}

func (s *jsonRealtimeSession) SendText(ctx context.Context, text string) error {
	return s.writeJSON(ctx, map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]string{{"type": "input_text", "text": text}},
		},
	})
}

func (s *jsonRealtimeSession) CreateResponse(ctx context.Context) error {
	return s.writeJSON(ctx, map[string]any{"type": "response.create"})
}

func (s *jsonRealtimeSession) ReplayContext(ctx context.Context, items []ContextItem) error {
	for _, item := range items {
		if err := s.writeJSON(ctx, map[string]any{
			"type": "conversation.item.create",
			"item": contextItemPayload(item),
		}); err != nil {
			return err
		}
	}
	return nil
}
