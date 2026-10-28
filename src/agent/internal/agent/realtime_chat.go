package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// handleRealtimeChatAsync starts a realtime run and retains its result for
// polling after the run releases its cancellation state.
func (s *Server) handleRealtimeChatAsync(w http.ResponseWriter, req ChatRequest, input TurnInput) {
	requestID := req.RequestID
	episodeID := s.runtime.NewEpisodeID()
	userMsg := messageFromTurnInput(input, episodeID, requestID, nil, time.Now())
	pending := &chatPendingResult{messages: make([]Message, 0), history: []Message{userMsg}}

	s.pendingResultsMu.Lock()
	if _, exists := s.pendingResults[requestID]; exists {
		s.pendingResultsMu.Unlock()
		http.Error(w, "request_id already in use", http.StatusConflict)
		return
	}
	s.pendingResults[requestID] = pending
	s.pendingResultsMu.Unlock()

	runCtx, cancel := context.WithCancel(context.Background())
	if !s.registerActiveRun(requestID, cancel) {
		cancel()
		s.pendingResultsMu.Lock()
		delete(s.pendingResults, requestID)
		s.pendingResultsMu.Unlock()
		http.Error(w, "request_id already in use", http.StatusConflict)
		return
	}
	s.publishMessage(userMsg)
	if s.liveActivity != nil {
		s.liveActivity.StartTask(requestID, input.InputText, s.liveActivityPhoneID(req))
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(map[string]string{"request_id": requestID})

	handler := s.realtimeChatHandlerSnapshot()
	go func() {
		defer s.unregisterActiveRun(requestID)
		defer func() {
			time.AfterFunc(60*time.Second, func() {
				s.pendingResultsMu.Lock()
				delete(s.pendingResults, requestID)
				s.pendingResultsMu.Unlock()
			})
		}()

		var response strings.Builder
		finish := func(event RealtimeChatEvent) {
			if event.Response != "" {
				response.Reset()
				response.WriteString(event.Response)
			}
			content := response.String()
			assistant := Message{
				Type:             "assistant",
				EpisodeID:        episodeID,
				RequestID:        requestID,
				Content:          content,
				ReasoningContent: event.ReasoningContent,
				Timestamp:        time.Now(),
			}
			if normalized, ok := s.publishMessage(assistant); ok {
				pending.mu.Lock()
				pending.history = append(pending.history, normalized)
				pending.messages = append(pending.messages, normalized)
				// The request's own user and assistant messages are its turn;
				// the shared context may already hold another one.
				pending.turn = append([]Message(nil), pending.history...)
				pending.done = true
				pending.mu.Unlock()
			} else {
				pending.mu.Lock()
				pending.turn = append([]Message(nil), pending.history...)
				pending.done = true
				pending.mu.Unlock()
			}

			if s.liveActivity != nil {
				s.liveActivity.CompleteTask(requestID, content)
			}
		}
		fail := func(message string) {
			if strings.TrimSpace(message) == "" {
				message = "realtime chat failed"
			}
			pending.mu.Lock()
			pending.err = message
			pending.done = true
			pending.mu.Unlock()
			if s.liveActivity != nil {
				s.liveActivity.FailTask(requestID, message)
			}
		}

		if handler == nil {
			fail("realtime voice session is unavailable")
			return
		}
		events, err := handler(runCtx, RealtimeChatRequest{RequestID: requestID, Message: input.InputText})
		if err != nil {
			fail(err.Error())
			return
		}
		for {
			select {
			case <-runCtx.Done():
				fail("request canceled")
				return
			case event, ok := <-events:
				if !ok {
					fail("realtime chat ended without a response")
					return
				}
				switch event.Type {
				case RealtimeChatEventDelta:
					response.WriteString(event.Delta)
				case RealtimeChatEventDone:
					finish(event)
					return
				case RealtimeChatEventError:
					fail(event.Error)
					return
				}
			}
		}
	}()
}

// handleRealtimeChatStream keeps a realtime run registered until the event
// stream ends so cancellation can find and stop it.
func (s *Server) handleRealtimeChatStream(w http.ResponseWriter, r *http.Request, req ChatRequest, input TurnInput) {
	stream, ok := newChatStreamWriter(w)
	if !ok {
		http.Error(w, "Streaming unsupported", http.StatusInternalServerError)
		return
	}
	episodeID := s.runtime.NewEpisodeID()
	userMessage := messageFromTurnInput(input, episodeID, req.RequestID, nil, time.Now())
	runCtx, cancel := context.WithCancel(context.Background())
	if !s.registerActiveRun(req.RequestID, cancel) {
		cancel()
		http.Error(w, "request_id already in use", http.StatusConflict)
		return
	}
	defer func() {
		s.unregisterActiveRun(req.RequestID)
		cancel()
	}()
	s.publishMessage(userMessage)
	if s.liveActivity != nil {
		s.liveActivity.StartTask(req.RequestID, input.InputText, s.liveActivityPhoneID(req))
	}

	handler := s.realtimeChatHandlerSnapshot()
	if handler == nil {
		stream.Write(ChatStreamEvent{Type: RealtimeChatEventError, RequestID: req.RequestID, EpisodeID: episodeID, Error: "realtime voice session is unavailable", History: s.webHistoryTurnSnapshot()})
		return
	}
	events, err := handler(runCtx, RealtimeChatRequest{RequestID: req.RequestID, Message: input.InputText})
	if err != nil {
		stream.Write(ChatStreamEvent{Type: RealtimeChatEventError, RequestID: req.RequestID, EpisodeID: episodeID, Error: err.Error(), History: s.webHistoryTurnSnapshot()})
		return
	}
	var response strings.Builder
	var reasoningContent string
	for {
		select {
		case <-runCtx.Done():
			stream.Write(ChatStreamEvent{Type: RealtimeChatEventError, RequestID: req.RequestID, EpisodeID: episodeID, Error: "request canceled", History: s.webHistoryTurnSnapshot()})
			return
		case event, ok := <-events:
			if !ok {
				stream.Write(ChatStreamEvent{Type: RealtimeChatEventError, RequestID: req.RequestID, EpisodeID: episodeID, Error: "realtime chat ended without a response", History: s.webHistoryTurnSnapshot()})
				return
			}
			switch event.Type {
			case RealtimeChatEventDelta:
				response.WriteString(event.Delta)
				stream.Write(ChatStreamEvent{Type: "assistant_delta", RequestID: req.RequestID, EpisodeID: episodeID, Delta: event.Delta})
			case RealtimeChatEventDone:
				if event.Response != "" {
					response.Reset()
					response.WriteString(event.Response)
				}
				if event.ReasoningContent != "" {
					reasoningContent = event.ReasoningContent
				}
				assistant := Message{
					Type:             "assistant",
					EpisodeID:        episodeID,
					RequestID:        req.RequestID,
					Content:          response.String(),
					ReasoningContent: reasoningContent,
					Timestamp:        time.Now(),
				}
				if normalized, ok := s.publishMessage(assistant); ok {
					history := s.webHistoryTurnSnapshot()
					if s.liveActivity != nil {
						s.liveActivity.CompleteTask(req.RequestID, normalized.Content)
					}
					stream.Write(ChatStreamEvent{Type: "done", RequestID: req.RequestID, EpisodeID: episodeID, Response: normalized.Content, History: history})
				} else {
					stream.Write(ChatStreamEvent{Type: "done", RequestID: req.RequestID, EpisodeID: episodeID, Response: response.String(), History: s.webHistoryTurnSnapshot()})
				}
				return
			case RealtimeChatEventError:
				stream.Write(ChatStreamEvent{Type: RealtimeChatEventError, RequestID: req.RequestID, EpisodeID: episodeID, Error: event.Error, History: s.webHistoryTurnSnapshot()})
				return
			}
		}
	}
}
