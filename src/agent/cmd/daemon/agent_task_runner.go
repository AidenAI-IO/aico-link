package main

import (
	"context"
	"sync"

	"aiden-agent/internal/agent"
	"aiden-agent/internal/agenttask"
)

// runtimeAgentTaskRunner adapts the legacy agent Runtime to the narrow
// orchestration boundary owned by internal/agenttask.
type runtimeAgentTaskRunner struct {
	runtime *agent.Runtime
}

func newRealtimeAgentTaskManager(cfg agent.Config, runtime *agent.Runtime) *agenttask.Manager {
	if !cfg.VoiceModel.UseBackendAgent {
		return nil
	}
	return agenttask.NewManager(runtimeAgentTaskRunner{runtime: runtime})
}

func (r runtimeAgentTaskRunner) Run(ctx context.Context, prompt string) (string, error) {
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
	var suppliedMu sync.Mutex
	var suppliedSteer agenttask.SteerMessage
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
	if acknowledge := agenttask.SteerAcknowledgerFromContext(ctx); acknowledge != nil {
		request.EventHandler = func(event agent.RunEvent) {
			if event.Type != "steer" {
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
	return result.Output, err
}
