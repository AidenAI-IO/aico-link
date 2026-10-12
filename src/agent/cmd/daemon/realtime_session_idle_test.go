package main

import (
	"fmt"
	"testing"

	"aiden-agent/internal/agent"
	"aiden-agent/internal/agent/realtimevoice"
)

// A provider idle close must stay announceable and reach the voice layer as an
// idle timeout rather than a generic service outage. Both shapes below were
// observed on hardware: a bare error frame and a 1007 close frame.
func TestRealtimeIdleCloseAnnouncesIdleTimeout(t *testing.T) {
	const reason = "Your session was closed because no response was generated for 180 seconds."
	cases := map[string]error{
		"error frame": fmt.Errorf("%w: %s", realtimevoice.ErrSessionIdleTimeout, reason),
		"1007 close frame": fmt.Errorf("%w: websocket: close 1007 (invalid payload data): %s",
			realtimevoice.ErrSessionIdleTimeout, reason),
	}
	for name, providerErr := range cases {
		t.Run(name, func(t *testing.T) {
			sessionErr := markRealtimeProviderFailure(providerErr)
			if !shouldAnnounceRealtimeSessionFailure(sessionErr) {
				t.Fatal("idle close must be announced; the failed session cannot voice its own error")
			}
			failure := agent.TurnFailureFromError(sessionErr)
			if failure == nil {
				t.Fatal("TurnFailureFromError returned nil for an idle close")
			}
			if failure.Code != agent.TurnFailureSessionIdle {
				t.Errorf("Code = %q, want %q", failure.Code, agent.TurnFailureSessionIdle)
			}
		})
	}
}

// Rotation keeps its existing reconnect handling and must not be reclassified.
func TestRealtimeRotationIsNotIdleTimeout(t *testing.T) {
	rotated := fmt.Errorf("%w: gemini live ends this session in 50s", realtimevoice.ErrSessionRotated)
	failure := agent.TurnFailureFromError(rotated)
	if failure != nil && failure.Code == agent.TurnFailureSessionIdle {
		t.Error("session rotation must not be classified as an idle timeout")
	}
}
