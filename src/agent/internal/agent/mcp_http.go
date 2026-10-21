package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"

	"aiden-agent/internal/logging"
	"aiden-agent/internal/mcp"

	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

const defaultMCPConfigWebBase = "http://127.0.0.1"

// Cap concurrent MCP support_bundle work (config-web fetch + base64 encode +
// response write). Reject extras instead of queueing so an unauthenticated
// flood cannot multiply large Agent buffers while slow clients drain.
const maxMCPSupportBundleConcurrent = 1

var (
	mcpConfigWebHTTPClient     = &http.Client{Timeout: 30 * time.Second}
	mcpSupportBundleHTTPClient = &http.Client{Timeout: 2 * time.Minute}
	mcpSupportBundleSlots      = make(chan struct{}, maxMCPSupportBundleConcurrent)
)

func (s *Server) handleMCP(w http.ResponseWriter, r *http.Request) {
	if s.runtime == nil || !s.runtime.ConfigSnapshot().MCPEnabledOrDefault() {
		http.NotFound(w, r)
		return
	}
	handler := &mcp.Handler{Backend: mcpServerBackend{server: s}}
	handler.ServeHTTP(w, r)
}

type mcpServerBackend struct {
	server *Server
}

func (b mcpServerBackend) ShellEnabled() bool {
	if b.server == nil || b.server.runtime == nil {
		return false
	}
	return b.server.runtime.ConfigSnapshot().MCPShellEnabledOrDefault()
}

func (b mcpServerBackend) DescribeRuntimeTool(name string) (mcp.ToolInfo, bool) {
	if b.server == nil {
		return mcp.ToolInfo{}, false
	}
	spec, ok := b.server.lookupOwnedToolSpec(name)
	if !ok {
		return mcp.ToolInfo{}, false
	}
	desc := spec.Descriptor()
	schemaMap := desc.ArgsSchema
	if schemaMap == nil {
		schemaMap = map[string]any{"type": "object", "properties": map[string]any{}}
	}
	return mcp.ToolInfo{
		Name:        desc.Name,
		Description: desc.Description,
		InputSchema: schemaMap,
	}, true
}

func (b mcpServerBackend) CallRuntimeTool(ctx context.Context, name string, arguments json.RawMessage) (mcp.CallResult, error) {
	if b.server == nil {
		return mcp.CallResult{}, fmt.Errorf("server unavailable")
	}
	spec, ok := b.server.lookupOwnedToolSpec(name)
	if !ok {
		return mcp.CallResult{
			IsError: true,
			Content: []mcp.ContentBlock{{Type: "text", Text: "unknown tool: " + name}},
		}, nil
	}
	rawInput := "{}"
	if len(arguments) > 0 && string(arguments) != "null" {
		rawInput = string(arguments)
	}
	executionCtx := ctx
	cancel := context.CancelFunc(func() {})
	if httpToolExecutionSurvivesClientDisconnect(name) {
		executionCtx, cancel = detachedHTTPToolExecutionContext(ctx)
	}
	defer cancel()
	execution := executeToolCall(executionCtx, ToolCallExecution{
		Specs:  NewToolSpecs([]langtools.Tool{spec.Tool}),
		Action: schema.AgentAction{Tool: spec.Name, ToolInput: rawInput},
	})
	isError := execution.Result.IsError() || execution.Error != nil
	output := execution.Result.Output
	// /mcp is unauthenticated: never return raw execution errors (paths, etc.).
	if execution.Error != nil {
		logging.Warnf("agent", "mcp", "tool %s failed: %v", name, execution.Error)
		output = "tool execution failed"
		isError = true
	} else if execution.Result.Error != nil {
		logging.Warnf("agent", "mcp", "tool %s error: %s", name, execution.Result.Error.Message)
		if strings.TrimSpace(output) == "" || output == execution.Result.Error.Message {
			output = "tool execution failed"
		}
		isError = true
	}
	return mcp.CallResult{
		IsError: isError,
		Content: mcpContentFromToolOutput(output),
	}, nil
}

// mcpContentFromToolOutput turns Runtime tool JSON that includes a screenshot
// payload into an MCP image block plus metadata text (without the base64).
func mcpContentFromToolOutput(output string) []mcp.ContentBlock {
	var obj map[string]any
	if err := json.Unmarshal([]byte(output), &obj); err != nil {
		return []mcp.ContentBlock{{Type: "text", Text: output}}
	}
	data, _ := obj["data"].(string)
	format, _ := obj["format"].(string)
	data = strings.TrimSpace(data)
	format = strings.ToLower(strings.TrimSpace(format))
	if data == "" || format == "" {
		return []mcp.ContentBlock{{Type: "text", Text: output}}
	}
	mime, ok := mcpImageMIME(format)
	if !ok {
		return []mcp.ContentBlock{{Type: "text", Text: output}}
	}
	delete(obj, "data")
	meta, err := json.Marshal(obj)
	if err != nil {
		return []mcp.ContentBlock{{Type: "image", MIMEType: mime, Data: data}}
	}
	return []mcp.ContentBlock{
		{Type: "image", MIMEType: mime, Data: data},
		{Type: "text", Text: string(meta)},
	}
}

func mcpImageMIME(format string) (string, bool) {
	switch format {
	case "jpeg", "jpg":
		return "image/jpeg", true
	case "png":
		return "image/png", true
	case "webp":
		return "image/webp", true
	case "gif":
		return "image/gif", true
	default:
		return "", false
	}
}

func (b mcpServerBackend) PhoneBridgeStatus(ctx context.Context) (any, error) {
	_ = ctx
	if b.server == nil {
		return nil, fmt.Errorf("server unavailable")
	}
	status := PhoneBridgeStatus{}
	if b.server.bridge != nil {
		status = b.server.bridge.getStatus()
	}
	if b.server.runtime != nil {
		status.DeviceType = b.server.runtime.deviceTypeFromState()
		status.PointerMode = b.server.runtime.devicePointerModeFromState()
	}
	return status, nil
}

func (b mcpServerBackend) DeviceStatus(ctx context.Context) (any, error) {
	base, err := b.configWebBase()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/device/status", nil)
	if err != nil {
		return nil, err
	}
	resp, err := mcpConfigWebHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("config-web device status: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("config-web device status: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, err
	}
	return payload, nil
}

func (b mcpServerBackend) SupportBundle(ctx context.Context) (string, string, int, func(), error) {
	select {
	case mcpSupportBundleSlots <- struct{}{}:
	default:
		return "", "", 0, nil, fmt.Errorf("support_bundle busy; try again later")
	}
	release := func() { <-mcpSupportBundleSlots }
	fail := func(err error) (string, string, int, func(), error) {
		release()
		return "", "", 0, nil, err
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	base, err := b.configWebBase()
	if err != nil {
		return fail(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(base, "/")+"/api/logs/support", nil)
	if err != nil {
		return fail(err)
	}
	resp, err := mcpSupportBundleHTTPClient.Do(req)
	if err != nil {
		return fail(fmt.Errorf("config-web support bundle: %w", err))
	}
	defer resp.Body.Close()
	const maxSupportBundleBytes = 32 << 20
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSupportBundleBytes+1))
	if err != nil {
		return fail(err)
	}
	if len(body) > maxSupportBundleBytes {
		return fail(fmt.Errorf("config-web support bundle: exceeds %d byte limit", maxSupportBundleBytes))
	}
	if resp.StatusCode != http.StatusOK {
		return fail(fmt.Errorf("config-web support bundle: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(body))))
	}
	mime := resp.Header.Get("Content-Type")
	if mime == "" {
		mime = "application/gzip"
	}
	// Encode while holding the slot; caller releases after writeJSON so slow
	// readers cannot accumulate multiple large responses.
	blob := base64.StdEncoding.EncodeToString(body)
	return blob, mime, len(body), release, nil
}

func (b mcpServerBackend) configWebBase() (string, error) {
	base := defaultMCPConfigWebBase
	if b.server != nil && b.server.runtime != nil {
		if configured := strings.TrimSpace(b.server.runtime.ConfigSnapshot().MCPConfigWebBaseURL); configured != "" {
			base = configured
		}
	}
	if err := validateMCPConfigWebBaseURL(base); err != nil {
		return "", err
	}
	return base, nil
}

// validateMCPConfigWebBaseURL keeps MCP→config-web fetches on loopback HTTP only.
func validateMCPConfigWebBaseURL(raw string) error {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return fmt.Errorf("mcp_config_web_base_url: %w", err)
	}
	if parsed.Scheme != "http" {
		return fmt.Errorf("mcp_config_web_base_url: only http loopback is allowed")
	}
	host := parsed.Hostname()
	if host != "127.0.0.1" && !strings.EqualFold(host, "localhost") {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return fmt.Errorf("mcp_config_web_base_url: host must be loopback")
		}
	}
	if parsed.User != nil || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("mcp_config_web_base_url: invalid URL")
	}
	return nil
}
