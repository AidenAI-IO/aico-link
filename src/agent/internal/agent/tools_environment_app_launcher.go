package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// environmentAppLauncher opens apps through an environment bridge's own
// open_app tool. Bridges that own the device (the ADB Android bridge) can
// launch an app's launcher activity directly, which avoids the visual
// search-and-tap flow and its vision-model calls. Bridges without the tool
// answer 404, and the launcher reports that so open_app keeps its search route.
type environmentAppLauncher struct {
	endpoint string
	taskID   string
	client   *http.Client
}

// environmentLaunchResult is the bridge's decoded open_app answer.
type environmentLaunchResult struct {
	// Supported is false when the bridge has no open_app tool at all.
	Supported bool
	// Opened reports a successful launch.
	Opened bool
	// Output is the bridge's action output, returned to the model on success.
	Output string
	// Reason explains a declined launch, such as an unknown app name.
	Reason string
}

func newEnvironmentAppLauncher(endpoint, taskID string) *environmentAppLauncher {
	endpoint = strings.TrimRight(strings.TrimSpace(endpoint), "/")
	if endpoint == "" {
		return nil
	}
	return &environmentAppLauncher{
		endpoint: endpoint,
		taskID:   strings.TrimSpace(taskID),
		// Environment bridges are local; never route them through a host proxy.
		client: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{Proxy: nil}},
	}
}

func withEnvironmentAppLauncher(launcher *environmentAppLauncher) BuiltinToolSetOption {
	return func(options *builtinToolSetOptions) {
		options.environmentLauncher = launcher
	}
}

func (l *environmentAppLauncher) Launch(ctx context.Context, app string) (environmentLaunchResult, error) {
	if l == nil {
		return environmentLaunchResult{}, nil
	}
	input, err := json.Marshal(map[string]string{"app": app})
	if err != nil {
		return environmentLaunchResult{}, err
	}
	body, err := json.Marshal(map[string]string{"input": string(input)})
	if err != nil {
		return environmentLaunchResult{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, l.endpoint+"/api/tools/"+toolOpenApp, bytes.NewReader(body))
	if err != nil {
		return environmentLaunchResult{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	if l.taskID != "" {
		req.Header.Set("benchmark-task-id", l.taskID)
	}
	resp, err := l.client.Do(req)
	if err != nil {
		return environmentLaunchResult{}, fmt.Errorf("environment open_app: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return environmentLaunchResult{}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return environmentLaunchResult{Supported: true}, fmt.Errorf("environment open_app returned HTTP %d", resp.StatusCode)
	}
	var envelope struct {
		Output  string `json:"output"`
		IsError bool   `json:"is_error"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&envelope); err != nil {
		return environmentLaunchResult{Supported: true}, fmt.Errorf("decode environment open_app: %w", err)
	}
	if envelope.Error == "unknown_tool" {
		return environmentLaunchResult{}, nil
	}
	if envelope.IsError {
		return environmentLaunchResult{Supported: true, Reason: firstNonEmptyString([]string{envelope.Output, envelope.Error})}, nil
	}
	return decodeEnvironmentLaunchOutput(envelope.Output), nil
}

// decodeEnvironmentLaunchOutput reads either a post-action observation that
// wraps the launch result in action_output, or a bare {"ok":...} result.
func decodeEnvironmentLaunchOutput(output string) environmentLaunchResult {
	result := environmentLaunchResult{Supported: true, Output: output}
	var outer struct {
		ActionOutput *string `json:"action_output"`
		OK           *bool   `json:"ok"`
		Suggestion   string  `json:"suggestion"`
	}
	if err := json.Unmarshal([]byte(output), &outer); err != nil {
		result.Reason = "environment open_app returned invalid JSON"
		return result
	}
	if outer.ActionOutput != nil {
		var inner struct {
			OK         bool   `json:"ok"`
			Suggestion string `json:"suggestion"`
		}
		if err := json.Unmarshal([]byte(*outer.ActionOutput), &inner); err != nil {
			result.Reason = "environment open_app returned invalid action output"
			return result
		}
		result.Opened = inner.OK
		result.Output = *outer.ActionOutput
		result.Reason = inner.Suggestion
		return result
	}
	if outer.OK != nil {
		result.Opened = *outer.OK
		result.Reason = outer.Suggestion
	}
	return result
}
