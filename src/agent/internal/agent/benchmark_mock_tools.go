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

	langtools "github.com/tmc/langchaingo/tools"
)

// benchmarkMockTools replaces device execution only. Descriptions, schemas,
// availability filtering, and post-action screenshot verification stay native;
// the verification screenshots come from the fixture's screen provider.
// Real device suites never enable this mode.
func benchmarkMockTools(cfg Config, tools []langtools.Tool) []langtools.Tool {
	if !cfg.Benchmark.MockTools {
		return tools
	}
	endpoint := strings.TrimRight(cfg.EnvironmentBridge.Endpoint, "/")
	result := make([]langtools.Tool, len(tools))
	for i, tool := range tools {
		result[i] = tool
		// Device-facing tools are the input and bridge categories; a new device
		// tool joins the mock automatically when its spec declares one of them.
		switch builtInToolSpecMetadata[tool.Name()].Category {
		case "input", "bridge":
			if action, ok := tool.(*postActionScreenshotTool); ok {
				decorated := *action
				decorated.inner = &benchmarkMockTool{Tool: action.inner, endpoint: endpoint, taskID: cfg.EnvironmentBridge.BenchmarkTaskID}
				result[i] = &decorated
				continue
			}
			result[i] = &benchmarkMockTool{Tool: tool, endpoint: endpoint, taskID: cfg.EnvironmentBridge.BenchmarkTaskID}
		}
	}
	return result
}

// Local fixtures must not inherit a host proxy or redirect outside their URL.
var benchmarkMockClient = &http.Client{
	Timeout:       30 * time.Second,
	Transport:     &http.Transport{Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type benchmarkMockTool struct {
	langtools.Tool
	endpoint string
	taskID   string
}

func (t *benchmarkMockTool) ArgsSchema() map[string]any {
	if structured, ok := t.Tool.(structuredInputTool); ok {
		return structured.ArgsSchema()
	}
	return genericToolParameters()
}

func (t *benchmarkMockTool) Call(ctx context.Context, input string) (string, error) {
	body, err := json.Marshal(map[string]string{"input": input})
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.endpoint+"/api/tools/"+t.Name(), bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if t.taskID != "" {
		req.Header.Set("benchmark-task-id", t.taskID)
	}
	resp, err := benchmarkMockClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("benchmark mock tool %s: %w", t.Name(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("benchmark mock tool %s returned HTTP %d", t.Name(), resp.StatusCode)
	}
	var result struct {
		Output  string `json:"output"`
		IsError bool   `json:"is_error"`
		Error   string `json:"error"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1024*1024)).Decode(&result); err != nil {
		return "", fmt.Errorf("decode benchmark mock tool %s: %w", t.Name(), err)
	}
	if result.IsError {
		te := NewToolError(CodeToolExecutionFailed, firstNonEmptyString([]string{result.Error, result.Output, "mock tool failed"}))
		SetToolError(ctx, te)
		return toolErrorString(te), nil
	}
	return result.Output, nil
}

// benchmarkHeadlessTools keeps unavailable hardware services out of memory/shell
// evaluations. This is explicit container configuration, never inferred on devices.
func benchmarkHeadlessTools(cfg Config, tools []langtools.Tool) []langtools.Tool {
	if !cfg.Benchmark.Headless || cfg.Benchmark.Token == "" {
		return tools
	}
	result := make([]langtools.Tool, 0, len(tools))
	for _, tool := range tools {
		switch builtInToolSpecMetadata[tool.Name()].Category {
		case "input", "bridge", "observation":
			continue
		}
		result = append(result, tool)
	}
	return result
}
