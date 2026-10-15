package agent

import (
	"encoding/json"
	"strings"
)

func steerHumanMessageContent(steer RunSteerMessage) string {
	content := strings.TrimSpace(steer.Content)
	if content == "" {
		content = "(empty steering message)"
	}
	return content
}

func actionOutputFromScreenshotObservation(observation string) (string, bool) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(observation), &fields); err != nil {
		return "", false
	}
	raw, ok := fields["action_output"]
	if !ok {
		return "", false
	}
	var output string
	if err := json.Unmarshal(raw, &output); err != nil {
		return "", false
	}
	return output, true
}

func compactScreenshotObservation(_ string, observation string) (string, bool) {
	var result postActionScreenshotResult
	if err := json.Unmarshal([]byte(observation), &result); err != nil {
		return "", false
	}
	if (result.Data == "" && strings.TrimSpace(result.ScreenshotRef) == "") || result.Width <= 0 || result.Height <= 0 {
		return "", false
	}
	if actionOutput, ok := actionOutputFromScreenshotObservation(observation); ok {
		return actionOutput, true
	}
	return observation, true
}

func (o observedWorldState) IsEmpty() bool {
	return strings.TrimSpace(o.AppName) == "" &&
		strings.TrimSpace(o.PageName) == "" &&
		strings.TrimSpace(o.Platform) == "" &&
		len(uniqueNonEmpty(o.VisibleText)) == 0 &&
		len(uniqueNonEmpty(o.Dialogs)) == 0
}
