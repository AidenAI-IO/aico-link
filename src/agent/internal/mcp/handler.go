package mcp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
)

const (
	serverName    = "aiden-agent"
	serverVersion = "1.0.0"
	maxBodyBytes  = 8 << 20
)

// Handler serves Streamable HTTP MCP at a single endpoint (POST /mcp).
// Dual-era: legacy initialize clients (no MCP-Protocol-Version) and modern
// 2026-07-28 clients (headers + params._meta) are both accepted.
type Handler struct {
	Backend Backend
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !validateOrigin(r) {
		writeJSON(w, http.StatusForbidden, errorResponse(nil, errInvalidRequest, "invalid Origin", nil))
		return
	}
	accept := r.Header.Get("Accept")
	if accept != "" && !acceptContains(accept, "application/json") && !acceptContains(accept, "text/event-stream") && !acceptContains(accept, "*/*") {
		http.Error(w, "Accept must include application/json and/or text/event-stream", http.StatusBadRequest)
		return
	}

	// MaxBytesReader closes the connection on overflow instead of letting
	// net/http drain the remainder for keep-alive reuse.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "failed to read body", http.StatusBadRequest)
		return
	}

	trimmed := bytes.TrimSpace(body)
	if len(trimmed) > 0 && trimmed[0] == '[' {
		// JSON-RPC batch is out of scope; do not advertise batch-era revisions.
		writeJSON(w, http.StatusBadRequest, errorResponse(nil, errInvalidRequest, "JSON-RPC batch is not supported", nil))
		return
	}

	var req jsonRPCRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse(nil, errParseError, "parse error", nil))
		return
	}
	if req.JSONRPC != "" && req.JSONRPC != jsonRPCVersion {
		writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, errInvalidRequest, "jsonrpc must be 2.0", nil))
		return
	}
	if strings.TrimSpace(req.Method) == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, errInvalidRequest, "method required", nil))
		return
	}

	if isNotification(req) {
		// Core Streamable HTTP has no required client notifications; accept silently.
		w.WriteHeader(http.StatusAccepted)
		return
	}

	if isModernRequest(r, req) {
		if verr := validateModernHeaders(r, req); verr != nil {
			writeJSON(w, http.StatusBadRequest, errorResponse(req.ID, verr.Code, verr.Message, verr.Data))
			return
		}
	}

	resp, release := h.dispatch(r, req)
	if release != nil {
		defer release()
	}
	status := http.StatusOK
	if resp.Error != nil && resp.Error.Code == errMethodNotFound {
		// Spec: unknown RPC → HTTP 404 with JSON-RPC -32601.
		status = http.StatusNotFound
	}
	writeJSON(w, status, resp)
}

func (h *Handler) dispatch(r *http.Request, req jsonRPCRequest) (jsonRPCResponse, func()) {
	switch req.Method {
	case "initialize":
		return h.handleInitialize(req), nil
	case "server/discover":
		return h.handleDiscover(req), nil
	case "ping":
		return resultResponse(req.ID, map[string]any{}), nil
	case "tools/list":
		return h.handleToolsList(req), nil
	case "tools/call":
		return h.handleToolsCall(r, req)
	case "notifications/initialized", "notifications/cancelled":
		// Should arrive as notifications (no id); if mis-sent as request, ack.
		return resultResponse(req.ID, map[string]any{}), nil
	default:
		return errorResponse(req.ID, errMethodNotFound, "Method not found", nil), nil
	}
}

func (h *Handler) handleInitialize(req jsonRPCRequest) jsonRPCResponse {
	type initParams struct {
		ProtocolVersion string         `json:"protocolVersion"`
		Capabilities    map[string]any `json:"capabilities"`
		ClientInfo      map[string]any `json:"clientInfo"`
	}
	params, err := decodeParams[initParams](req.Params)
	if err != nil {
		return errorResponse(req.ID, errInvalidParams, err.Error(), nil)
	}
	version := preferLegacyProtocol(strings.TrimSpace(params.ProtocolVersion))
	return resultResponse(req.ID, map[string]any{
		"protocolVersion": version,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"serverInfo": map[string]any{
			"name":    serverName,
			"version": serverVersion,
		},
	})
}

func (h *Handler) handleDiscover(req jsonRPCRequest) jsonRPCResponse {
	// 2026-07-28 DiscoverResult: supportedVersions + serverInfo under _meta.
	return resultResponse(req.ID, map[string]any{
		"resultType":        "complete",
		"supportedVersions": supportedProtocolVersions,
		"capabilities": map[string]any{
			"tools": map[string]any{},
		},
		"_meta": map[string]any{
			"io.modelcontextprotocol/serverInfo": map[string]any{
				"name":    serverName,
				"version": serverVersion,
			},
		},
		"ttlMs":      int64(3600000),
		"cacheScope": "public",
	})
}

func (h *Handler) handleToolsList(req jsonRPCRequest) jsonRPCResponse {
	if h.Backend == nil {
		return errorResponse(req.ID, errInternal, "mcp backend unavailable", nil)
	}
	tools := make([]ToolInfo, 0, len(runtimeToolNames)+3)
	for _, name := range runtimeToolNames {
		if name == "shell" && !h.Backend.ShellEnabled() {
			continue
		}
		info, ok := h.Backend.DescribeRuntimeTool(name)
		if !ok {
			continue
		}
		tools = append(tools, info)
	}
	tools = append(tools,
		ToolInfo{
			Name:        toolPhoneBridgeStatus,
			Description: "Phone Bridge connection and device status from the Agent.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		ToolInfo{
			Name:        toolDeviceStatus,
			Description: "Device / firmware status from config-web.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
		ToolInfo{
			Name:        toolSupportBundle,
			Description: "Download a support log archive (gzip tar) from config-web; returns an embedded resource blob.",
			InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
		},
	)
	return resultResponse(req.ID, map[string]any{
		"tools": tools,
	})
}

func (h *Handler) handleToolsCall(r *http.Request, req jsonRPCRequest) (jsonRPCResponse, func()) {
	if h.Backend == nil {
		return errorResponse(req.ID, errInternal, "mcp backend unavailable", nil), nil
	}
	type callParams struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	params, err := decodeParams[callParams](req.Params)
	if err != nil {
		return errorResponse(req.ID, errInvalidParams, err.Error(), nil), nil
	}
	name := strings.TrimSpace(params.Name)
	if name == "" {
		return errorResponse(req.ID, errInvalidParams, "tool name required", nil), nil
	}
	if name == "shell" && !h.Backend.ShellEnabled() {
		return resultResponse(req.ID, CallResult{
			IsError: true,
			Content: []ContentBlock{{Type: "text", Text: "shell is disabled (mcp_shell_enabled=false)"}},
		}), nil
	}
	if !isRuntimeTool(name) && !isStatusTool(name) {
		return resultResponse(req.ID, CallResult{
			IsError: true,
			Content: []ContentBlock{{Type: "text", Text: "tool not in MCP allowlist: " + name}},
		}), nil
	}

	ctx := r.Context()
	switch {
	case isRuntimeTool(name):
		result, callErr := h.Backend.CallRuntimeTool(ctx, name, params.Arguments)
		if callErr != nil {
			return resultResponse(req.ID, CallResult{
				IsError: true,
				Content: []ContentBlock{{Type: "text", Text: callErr.Error()}},
			}), nil
		}
		return resultResponse(req.ID, result), nil
	case name == toolPhoneBridgeStatus:
		status, statusErr := h.Backend.PhoneBridgeStatus(ctx)
		if statusErr != nil {
			return resultResponse(req.ID, CallResult{
				IsError: true,
				Content: []ContentBlock{{Type: "text", Text: statusErr.Error()}},
			}), nil
		}
		return resultResponse(req.ID, jsonResult(status)), nil
	case name == toolDeviceStatus:
		status, statusErr := h.Backend.DeviceStatus(ctx)
		if statusErr != nil {
			return resultResponse(req.ID, CallResult{
				IsError: true,
				Content: []ContentBlock{{Type: "text", Text: statusErr.Error()}},
			}), nil
		}
		return resultResponse(req.ID, jsonResult(status)), nil
	case name == toolSupportBundle:
		blob, mime, rawBytes, release, bundleErr := h.Backend.SupportBundle(ctx)
		if bundleErr != nil {
			msg := "support_bundle failed"
			if errors.Is(bundleErr, ErrSupportBundleBusy) {
				msg = ErrSupportBundleBusy.Error()
			}
			return resultResponse(req.ID, CallResult{
				IsError: true,
				Content: []ContentBlock{{Type: "text", Text: msg}},
			}), nil
		}
		if mime == "" {
			mime = "application/gzip"
		}
		// Hold the backend slot through writeJSON so slow readers cannot
		// accumulate multiple large base64 responses after fetch completes.
		return resultResponse(req.ID, CallResult{
			Content: []ContentBlock{
				{Type: "text", Text: fmt.Sprintf("support_bundle mime=%s bytes=%d", mime, rawBytes)},
				{
					Type: "resource",
					Resource: &ResourceContents{
						URI:      "aiden://support-bundle",
						MIMEType: mime,
						Blob:     blob,
					},
				},
			},
		}), release
	default:
		return errorResponse(req.ID, errInternal, "unhandled tool", nil), nil
	}
}

func jsonResult(v any) CallResult {
	raw, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return CallResult{IsError: true, Content: []ContentBlock{{Type: "text", Text: err.Error()}}}
	}
	return CallResult{
		Content:           []ContentBlock{{Type: "text", Text: string(raw)}},
		StructuredContent: v,
	}
}

func writeJSON(w http.ResponseWriter, status int, payload any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

func acceptContains(accept, want string) bool {
	for _, part := range strings.Split(accept, ",") {
		media := strings.TrimSpace(strings.Split(part, ";")[0])
		if strings.EqualFold(media, want) {
			return true
		}
	}
	return false
}
