package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"

	"aico-agent/internal/agent/screen"
)

// newScreenToolResultObserver projects successful visual tool observations
// into the shared local ScreenState. Tool execution invokes it for both local
// and environment-bridge calls, so callers do not need separate bridge state
// synchronization logic.
func newScreenToolResultObserver(state *screen.ScreenState) ToolResultObserver {
	return func(_ context.Context, call ToolCall, result ToolResult) {
		if state == nil || result.IsError() || !returnsVisualObservation(call.Spec.Tool) {
			return
		}
		observation, ok := screenObservationFromToolOutput(result.Output)
		if !ok {
			return
		}

		width, height := observation.SourceWidth, observation.SourceHeight
		active := screen.ScreenActiveArea{}
		if observation.ActiveArea != nil {
			active = *observation.ActiveArea
		}
		if width <= 0 || height <= 0 {
			width, height = observation.Width, observation.Height
			active = screen.ScreenActiveArea{
				X: 0, Y: 0, Width: width, Height: height, Valid: width > 0 && height > 0,
			}
		}
		state.UpdateActiveArea(width, height, active)

		if observation.Data == "" {
			return
		}
		jpegData, err := base64.StdEncoding.DecodeString(observation.Data)
		if err == nil {
			state.UpdateScreenshot(jpegData, observation.Width, observation.Height)
		}
	}
}

// screenObservationFromToolOutput accepts the direct visual observation format
// and the common {"screenshot": {...}} envelope. Post-action observations
// embed screenshotResult directly, so they are covered by the direct form.
func screenObservationFromToolOutput(output string) (screenshotResult, bool) {
	var result screenshotResult
	if err := json.Unmarshal([]byte(output), &result); err == nil && result.Width > 0 && result.Height > 0 {
		return result, true
	}

	var envelope struct {
		Screenshot json.RawMessage `json:"screenshot"`
	}
	if err := json.Unmarshal([]byte(output), &envelope); err != nil || len(envelope.Screenshot) == 0 {
		return screenshotResult{}, false
	}
	if err := json.Unmarshal(envelope.Screenshot, &result); err != nil || result.Width <= 0 || result.Height <= 0 {
		return screenshotResult{}, false
	}
	return result, true
}
