package main

import (
	"context"
	"sync"

	"aico-agent/internal/agent"
	"aico-agent/internal/agenttask"
	"aico-agent/internal/stream"
)

// runtimeAgentTaskRunner adapts the legacy agent Runtime to the narrow
// orchestration boundary owned by internal/agenttask.
type runtimeAgentTaskRunner struct {
	runtime *agent.Runtime
	// activity shows each task's steps on the phone's Live Activity; nil
	// leaves it alone.
	activity *agent.BackgroundTaskActivity
}

func newRealtimeAgentTaskManager(cfg agent.Config, runtime *agent.Runtime, activity *agent.BackgroundTaskActivity) *agenttask.Manager {
	if !cfg.UseBackendAgentEffective() {
		return nil
	}
	manager := agenttask.NewManager(runtimeAgentTaskRunner{runtime: runtime, activity: activity})
	if activity != nil {
		manager.SetLifecycleObserver(activity.Observe)
	}
	return manager
}

func (r runtimeAgentTaskRunner) Run(ctx context.Context, prompt string) (string, error) {
	// Run events advance the task's Live Activity step; the manager's
	// lifecycle observer starts and settles it.
	taskID := ""
	if observation, ok := agenttask.TaskObservationFromContext(ctx); ok {
		taskID = observation.Task.ID
	}
	var actionHandler agent.UserActionHandler
	if handler := agenttask.UserActionHandlerFromContext(ctx); handler != nil {
		actionHandler = func(_ context.Context, req agent.HumanHandoffRequest) error {
			handler(agenttask.UserAction{Reason: req.Reason, Details: req.Details, SuggestedAction: req.SuggestedAction})
			return nil
		}
	}
	request := agent.RunRequest{
		WaitForIdle:             true,
		Input:                   prompt,
		Turn:                    agent.NewTextTurnInput(prompt, nil),
		AsyncEpisodeMaintenance: true,
		UserActionHandler:       actionHandler,
	}
	if observation, ok := agenttask.TaskObservationFromContext(ctx); ok {
		request.StreamScope = stream.EventScope{TaskID: observation.Task.ID}
		request.TaskCreatedAt = observation.Task.CreatedAt
		if observation.StreamSink != nil {
			request.StreamSink = &backendCollaborationSink{sink: observation.StreamSink, taskContext: ctx}
		}
	}
	var suppliedMu sync.Mutex
	var suppliedSteer agenttask.SteerMessage
	// Successful tool results, so a cancelled run can report effects it
	// already produced instead of an empty result.
	var completed []agenttask.CompletedStep
	if provider := agenttask.SteerProviderFromContext(ctx); provider != nil {
		request.SteerProvider = func(ctx context.Context) (agent.RunSteerMessage, bool) {
			message, ok := provider(ctx)
			if !ok {
				return agent.RunSteerMessage{}, false
			}
			suppliedMu.Lock()
			suppliedSteer = message
			suppliedMu.Unlock()
			return agent.RunSteerMessage{
				ID:        message.ID,
				Content:   message.Content,
				Timestamp: message.Timestamp,
			}, true
		}
	}
	acknowledge := agenttask.SteerAcknowledgerFromContext(ctx)
	recordStep := agenttask.StepRecorderFromContext(ctx)
	request.EventHandler = func(event agent.RunEvent) {
		r.activity.Event(taskID, event)
		if recordStep != nil {
			if step, ok := agent.TaskTimelineStep(event); ok {
				recordStep(step)
			}
		}
		switch event.Type {
		case "tool_result":
			if !event.IsError {
				// The summary reports which steps already took effect. Private
				// phone data is left out: it also reaches the task timeline.
				output := event.Content
				if agent.PrivateToolOutput(event.ToolName) {
					output = ""
				}
				suppliedMu.Lock()
				completed = append(completed, agenttask.CompletedStep{
					Tool: event.ToolName, Input: event.ToolInput, Output: output})
				suppliedMu.Unlock()
			}
		case "steer":
			if acknowledge == nil {
				return
			}
			suppliedMu.Lock()
			message := suppliedSteer
			suppliedMu.Unlock()
			if message.ID != "" {
				acknowledge(message)
			}
		}
	}
	request.SteerInterrupt = agenttask.SteerInterruptFromContext(ctx)
	result, err := r.runtime.Run(ctx, request)
	if err != nil && ctx.Err() != nil {
		suppliedMu.Lock()
		defer suppliedMu.Unlock()
		return agenttask.CancelledResult(completed), err
	}
	return result.Output, err
}

type backendCollaborationSink struct {
	sink        stream.EventSink
	taskContext context.Context
	mu          sync.Mutex
	episodeID   string
}

func (s *backendCollaborationSink) Emit(ctx context.Context, event stream.Event) error {
	observation, _ := agenttask.TaskObservationFromContext(s.taskContext)
	s.mu.Lock()
	defer s.mu.Unlock()
	if event.Kind == stream.EventRunStarted {
		if payload, ok := event.Payload.(map[string]string); ok {
			s.episodeID = payload["episode_id"]
		}
	}
	event.Collaboration = &stream.Collaboration{Version: 1, Actor: "backend",
		GoalRevision: observation.GoalRevision, AppliedRevision: observation.AppliedRevision,
		EpisodeID: s.episodeID}
	return s.sink.Emit(ctx, event)
}
