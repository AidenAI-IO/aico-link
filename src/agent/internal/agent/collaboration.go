package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"sync"
	"time"

	"aico-agent/internal/stream"
)

// CollaborationRecorder mirrors observations without consuming task notifications.
// Its bounded queue keeps model and task execution independent of HTTP clients.
type CollaborationRecorder struct {
	start      time.Time
	clockID    string
	events     *semanticEventBroadcaster
	queue      chan stream.Event
	mu         sync.Mutex
	incomplete bool
	closed     bool
	accepted   uint64
	processed  uint64
	changed    chan struct{}
	done       chan struct{}
}

func NewCollaborationRecorder(clockID string) *CollaborationRecorder {
	r := &CollaborationRecorder{
		start: time.Now(), clockID: clockID,
		events: newSemanticEventBroadcaster(4096), queue: make(chan stream.Event, 2048),
		changed: make(chan struct{}), done: make(chan struct{}),
	}
	go r.run()
	return r
}

func (r *CollaborationRecorder) run() {
	defer close(r.done)
	for event := range r.queue {
		// Validate JSON while outside execution locks. Freeze the payload before
		// exposing it to replay and subscribers; encoding errors invalidate evidence.
		data, err := json.Marshal(event)
		if err == nil {
			err = json.Unmarshal(data, &event)
		}
		if err == nil {
			err = r.events.Publish(event)
		}
		r.mu.Lock()
		if err != nil {
			r.incomplete = true
		}
		r.processed++
		close(r.changed)
		r.changed = make(chan struct{})
		r.mu.Unlock()
	}
}

func (r *CollaborationRecorder) Observe(event stream.Event, actor string) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return
	}
	metadata := stream.Collaboration{Version: 1}
	if event.Collaboration != nil {
		metadata = *event.Collaboration
	}
	metadata.UpdateIDs = append([]string(nil), metadata.UpdateIDs...)
	event.Collaboration = &metadata
	event.Collaboration.Actor = actor
	event.Collaboration.ClockID = r.clockID
	event.Collaboration.ObservedElapsedNS = time.Since(r.start).Nanoseconds()
	select {
	case r.queue <- event:
		r.accepted++
	default:
		r.incomplete = true
	}
}

func (r *CollaborationRecorder) Emit(_ context.Context, event stream.Event) error {
	if r == nil {
		return nil
	}
	actor := "foreground"
	if event.Collaboration != nil && event.Collaboration.Actor != "" {
		actor = event.Collaboration.Actor
	}
	r.Observe(event, actor)
	return nil
}

func (r *CollaborationRecorder) Record(kind stream.EventKind, actor, taskID, turnID string, payload any, metadata *stream.Collaboration) {
	if r == nil {
		return
	}
	event := stream.NewEvent(stream.EventIdentity{ConversationID: "agent", SessionEpoch: 1, TaskID: taskID, TurnID: turnID}, kind, payload)
	event.Origin = "collaboration"
	event.Visibility = stream.VisibilityDiagnostic
	event.Collaboration = metadata
	r.Observe(event, actor)
}

func (r *CollaborationRecorder) Cursor() stream.ReplayCursor {
	if r == nil {
		return stream.ReplayCursor{}
	}
	return r.events.Cursor()
}

func (r *CollaborationRecorder) Status() map[string]any {
	r.mu.Lock()
	complete := !r.incomplete
	pending := r.accepted - r.processed
	r.mu.Unlock()
	// Publish locks the broadcaster before updating processed under r.mu.
	// Read the cursor only after releasing r.mu to keep lock order consistent.
	return map[string]any{
		"complete": complete, "pending": pending,
		"cursor": r.events.Cursor(), "contract_version": 1,
	}
}

// Barrier waits for all observations accepted before this call, not for future work.
func (r *CollaborationRecorder) Barrier(ctx context.Context) error {
	r.mu.Lock()
	target := r.accepted
	for r.processed < target {
		changed := r.changed
		r.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
		r.mu.Lock()
	}
	r.mu.Unlock()
	return nil
}

func (r *CollaborationRecorder) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	if !r.closed {
		r.closed = true
		close(r.queue)
	}
	r.mu.Unlock()
	<-r.done
}

func (s *Server) CollaborationRecorder() *CollaborationRecorder {
	if s == nil {
		return nil
	}
	return s.collaboration
}

func (s *Server) handleCollaboration(w http.ResponseWriter, req *http.Request) {
	if req.Method != http.MethodGet {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !s.authorizeBenchmarkRequest(req) {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}
	cfg := s.runtime.ConfigSnapshot()
	if s.collaboration == nil || !cfg.Benchmark.RealtimeText {
		http.Error(w, "collaboration recorder unavailable", http.StatusServiceUnavailable)
		return
	}
	if req.URL.Path == "/api/benchmark/collaboration/events" {
		serveSemanticEvents(w, req, s.collaboration.events)
		return
	}
	if req.URL.Path != "/api/benchmark/collaboration" {
		http.NotFound(w, req)
		return
	}
	if req.URL.Query().Get("barrier") == "1" {
		ctx, cancel := context.WithTimeout(req.Context(), 5*time.Second)
		defer cancel()
		if err := s.collaboration.Barrier(ctx); err != nil {
			http.Error(w, err.Error(), http.StatusGatewayTimeout)
			return
		}
	}
	status := s.collaboration.Status()
	status["execution_mode"] = "realtime-text"
	status["handler_ready"] = s.realtimeChatHandlerSnapshot() != nil
	status["input_mode"] = cfg.InputModeOrDefault()
	status["use_backend_agent"] = cfg.VoiceModel.UseBackendAgent
	status["foreground_provider"] = cfg.VoiceModel.Provider
	status["foreground_model"] = cfg.VoiceModel.Model
	status["backend_provider"] = cfg.Model.Provider
	status["backend_model"] = cfg.Model.Model
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(status)
}
