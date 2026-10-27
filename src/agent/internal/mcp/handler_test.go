package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

type stubBackend struct {
	shell bool
	tools map[string]ToolInfo
	calls []string
}

func (s *stubBackend) ShellEnabled() bool { return s.shell }

func (s *stubBackend) DescribeRuntimeTool(name string) (ToolInfo, bool) {
	info, ok := s.tools[name]
	return info, ok
}

func (s *stubBackend) CallRuntimeTool(_ context.Context, name string, _ json.RawMessage) (CallResult, error) {
	s.calls = append(s.calls, name)
	return CallResult{Content: []ContentBlock{{Type: "text", Text: "ok:" + name}}}, nil
}

func (s *stubBackend) PhoneBridgeStatus(context.Context) (any, error) {
	return map[string]any{"connected": true}, nil
}

func (s *stubBackend) DeviceStatus(context.Context) (any, error) {
	return map[string]any{"ok": true}, nil
}

func (s *stubBackend) SupportBundle(context.Context) (string, string, int, func(), error) {
	return "Z3ppcA==", "application/gzip", 4, nil, nil // base64("gzip")
}

type releaseTrackingBundleBackend struct {
	stubBackend
	released *bool
}

func (s *releaseTrackingBundleBackend) SupportBundle(context.Context) (string, string, int, func(), error) {
	return "Z3ppcA==", "application/gzip", 4, func() { *s.released = true }, nil
}

func postMCP(t *testing.T, h http.Handler, body any, origin string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Host = "192.168.1.10:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if origin != "" {
		req.Header.Set("Origin", origin)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func TestInitializeAndToolsList(t *testing.T) {
	backend := &stubBackend{
		tools: map[string]ToolInfo{
			"screenshot": {Name: "screenshot", Description: "shot", InputSchema: map[string]any{"type": "object"}},
			"shell":      {Name: "shell", Description: "sh", InputSchema: map[string]any{"type": "object"}},
		},
	}
	h := &Handler{Backend: backend}

	initRec := postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "0"},
		},
	}, "")
	if initRec.Code != http.StatusOK {
		t.Fatalf("initialize status=%d body=%s", initRec.Code, initRec.Body.String())
	}

	listRec := postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	}, "")
	if listRec.Code != http.StatusOK {
		t.Fatalf("tools/list status=%d body=%s", listRec.Code, listRec.Body.String())
	}
	var listResp struct {
		Result struct {
			Tools []ToolInfo `json:"tools"`
		} `json:"result"`
	}
	if err := json.Unmarshal(listRec.Body.Bytes(), &listResp); err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tool := range listResp.Result.Tools {
		names[tool.Name] = true
	}
	if !names["screenshot"] || !names["phone_bridge_status"] {
		t.Fatalf("tools=%v", names)
	}
	if names["shell"] {
		t.Fatal("shell should be hidden when ShellEnabled=false")
	}
}

func TestToolsCallAllowlistAndShellGate(t *testing.T) {
	backend := &stubBackend{
		shell: false,
		tools: map[string]ToolInfo{
			"screenshot": {Name: "screenshot", InputSchema: map[string]any{"type": "object"}},
			"shell":      {Name: "shell", InputSchema: map[string]any{"type": "object"}},
		},
	}
	h := &Handler{Backend: backend}

	call := postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "tools/call",
		"params":  map[string]any{"name": "screenshot", "arguments": map[string]any{}},
	}, "")
	if call.Code != http.StatusOK || !strings.Contains(call.Body.String(), "ok:screenshot") {
		t.Fatalf("screenshot call: %s", call.Body.String())
	}

	shell := postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      4,
		"method":  "tools/call",
		"params":  map[string]any{"name": "shell", "arguments": map[string]any{"command": "id"}},
	}, "")
	if !strings.Contains(shell.Body.String(), "shell is disabled") {
		t.Fatalf("shell gate: %s", shell.Body.String())
	}

	denied := postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      5,
		"method":  "tools/call",
		"params":  map[string]any{"name": "web_search", "arguments": map[string]any{}},
	}, "")
	if !strings.Contains(denied.Body.String(), "allowlist") {
		t.Fatalf("allowlist: %s", denied.Body.String())
	}
}

func TestOriginValidation(t *testing.T) {
	h := &Handler{Backend: &stubBackend{}}
	ping := map[string]any{"jsonrpc": "2.0", "id": 1, "method": "ping"}

	bad := postMCP(t, h, ping, "http://evil.example")
	if bad.Code != http.StatusForbidden {
		t.Fatalf("cross-origin status=%d want 403", bad.Code)
	}

	// DNS rebinding: Origin and Host both carry the attacker name — must reject.
	raw, _ := json.Marshal(ping)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Host = "attacker.example:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Origin", "http://attacker.example:8080")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("rebinding status=%d want 403 body=%s", rec.Code, rec.Body.String())
	}

	good := postMCP(t, h, ping, "http://192.168.1.10:8080")
	if good.Code != http.StatusOK {
		t.Fatalf("IP origin status=%d body=%s", good.Code, good.Body.String())
	}

	localhostRaw, _ := json.Marshal(ping)
	localhostReq := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(localhostRaw))
	localhostReq.Host = "localhost:8080"
	localhostReq.Header.Set("Content-Type", "application/json")
	localhostReq.Header.Set("Accept", "application/json, text/event-stream")
	localhostReq.Header.Set("Origin", "http://localhost:8080")
	localhostRec := httptest.NewRecorder()
	h.ServeHTTP(localhostRec, localhostReq)
	if localhostRec.Code != http.StatusOK {
		t.Fatalf("localhost origin status=%d body=%s", localhostRec.Code, localhostRec.Body.String())
	}

	noOrigin := postMCP(t, h, ping, "")
	if noOrigin.Code != http.StatusOK {
		t.Fatalf("missing Origin status=%d body=%s", noOrigin.Code, noOrigin.Body.String())
	}
}

func TestJSONRPCBatchRejected(t *testing.T) {
	h := &Handler{Backend: &stubBackend{}}
	raw := []byte(`[{"jsonrpc":"2.0","id":1,"method":"ping"},{"jsonrpc":"2.0","id":2,"method":"tools/list"}]`)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Host = "192.168.1.10:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "batch is not supported") {
		t.Fatalf("body=%s", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":null`) {
		t.Fatalf("want id null: %s", rec.Body.String())
	}
}

func TestParseErrorIncludesNullID(t *testing.T) {
	h := &Handler{Backend: &stubBackend{}}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{`)))
	req.Host = "192.168.1.10:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"id":null`) {
		t.Fatalf("want id null: %s", rec.Body.String())
	}
}

func TestServerDiscoverShape(t *testing.T) {
	h := &Handler{Backend: &stubBackend{}}
	rec := postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      3,
		"method":  "server/discover",
	}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result["resultType"] != "complete" {
		t.Fatalf("resultType=%v", envelope.Result["resultType"])
	}
	if _, ok := envelope.Result["supportedVersions"]; !ok {
		t.Fatalf("missing supportedVersions: %v", envelope.Result)
	}
	if _, ok := envelope.Result["protocolVersions"]; ok {
		t.Fatalf("legacy protocolVersions key must not be present: %v", envelope.Result)
	}
	meta, _ := envelope.Result["_meta"].(map[string]any)
	info, _ := meta["io.modelcontextprotocol/serverInfo"].(map[string]any)
	if info["name"] != "aico-agent" {
		t.Fatalf("serverInfo=%v", info)
	}
	if _, ok := envelope.Result["serverInfo"]; ok {
		t.Fatalf("top-level serverInfo must not be present: %v", envelope.Result)
	}
}

func TestInitializeNegotiatesUnknownVersionToLegacy25(t *testing.T) {
	h := &Handler{Backend: &stubBackend{}}
	rec := postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "t", "version": "0"},
		},
	}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var envelope struct {
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.Result["protocolVersion"] != protocolLegacy25 {
		t.Fatalf("protocolVersion=%v want %s", envelope.Result["protocolVersion"], protocolLegacy25)
	}
	if envelope.Result["resultType"] != "complete" {
		t.Fatalf("resultType=%v", envelope.Result["resultType"])
	}
}

func TestSupportBundleEmbeddedResource(t *testing.T) {
	h := &Handler{Backend: &stubBackend{}}
	rec := postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      7,
		"method":  "tools/call",
		"params":  map[string]any{"name": "support_bundle", "arguments": map[string]any{}},
	}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, `"type":"resource"`) || !strings.Contains(body, `"blob"`) || !strings.Contains(body, `"uri":"aico://support-bundle"`) {
		t.Fatalf("want embedded resource blob: %s", body)
	}
	if strings.Contains(body, "encoding=base64\n") {
		t.Fatalf("should not put archive in text body: %s", body)
	}
}

func TestSupportBundleReleasesSlotAfterWrite(t *testing.T) {
	released := false
	h := &Handler{Backend: &releaseTrackingBundleBackend{released: &released}}
	rec := postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      8,
		"method":  "tools/call",
		"params":  map[string]any{"name": "support_bundle", "arguments": map[string]any{}},
	}, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !released {
		t.Fatal("support_bundle slot release was not called after writeJSON")
	}
}

func TestRequestBodyTooLarge(t *testing.T) {
	h := &Handler{Backend: &stubBackend{}}
	oversized := bytes.Repeat([]byte("a"), maxBodyBytes+1)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(oversized))
	req.Host = "192.168.1.10:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d want 413 body=%s", rec.Code, rec.Body.String())
	}
}

func TestMethodNotFoundHTTP404(t *testing.T) {
	h := &Handler{Backend: &stubBackend{}}
	rec := postMCP(t, h, map[string]any{
		"jsonrpc": "2.0",
		"id":      9,
		"method":  "resources/list",
	}, "")
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404 body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "-32601") {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestModernHeadersRequired(t *testing.T) {
	h := &Handler{Backend: &stubBackend{
		tools: map[string]ToolInfo{"screenshot": {Name: "screenshot", InputSchema: map[string]any{"type": "object"}}},
	}}
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "screenshot",
			"arguments": map[string]any{},
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion":    "2026-07-28",
				"io.modelcontextprotocol/clientInfo":         map[string]any{"name": "t", "version": "0"},
				"io.modelcontextprotocol/clientCapabilities": map[string]any{},
			},
		},
	}
	missing := postMCP(t, h, body, "")
	if missing.Code != http.StatusBadRequest || !strings.Contains(missing.Body.String(), "-32020") {
		t.Fatalf("missing headers: status=%d body=%s", missing.Code, missing.Body.String())
	}

	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Host = "192.168.1.10:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(headerProtocolVersion, protocolModern)
	req.Header.Set(headerMethod, "tools/call")
	req.Header.Set(headerName, "screenshot")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok:screenshot") {
		t.Fatalf("modern call: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestModernProtocolVersionHeaderOnlyToolsList(t *testing.T) {
	h := &Handler{Backend: &stubBackend{
		tools: map[string]ToolInfo{"screenshot": {Name: "screenshot", InputSchema: map[string]any{"type": "object"}}},
	}}
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      2,
		"method":  "tools/list",
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Host = "192.168.1.10:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(headerProtocolVersion, protocolModern)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), `"screenshot"`) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestModernHeadersWithoutBodyMetaToolsCall(t *testing.T) {
	h := &Handler{Backend: &stubBackend{
		tools: map[string]ToolInfo{"screenshot": {Name: "screenshot", InputSchema: map[string]any{"type": "object"}}},
	}}
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "screenshot",
			"arguments": map[string]any{},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Host = "192.168.1.10:8080"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(headerProtocolVersion, protocolModern)
	req.Header.Set(headerMethod, "tools/call")
	req.Header.Set(headerName, "screenshot")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok:screenshot") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestLegacyProtocolVersionHeaderSkipsModernValidation(t *testing.T) {
	h := &Handler{Backend: &stubBackend{
		tools: map[string]ToolInfo{"screenshot": {Name: "screenshot", InputSchema: map[string]any{"type": "object"}}},
	}}
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "screenshot",
			"arguments": map[string]any{},
		},
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Host = "192.168.1.10:8080"
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(headerProtocolVersion, protocolLegacy03)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "ok:screenshot") {
		t.Fatalf("legacy header call: status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestModernHeaderMismatch(t *testing.T) {
	h := &Handler{Backend: &stubBackend{}}
	body := map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name": "screenshot",
			"_meta": map[string]any{
				"io.modelcontextprotocol/protocolVersion": "2026-07-28",
			},
		},
	}
	raw, _ := json.Marshal(body)
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(raw))
	req.Host = "192.168.1.10:8080"
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set(headerProtocolVersion, protocolModern)
	req.Header.Set(headerMethod, "tools/call")
	req.Header.Set(headerName, "other_tool")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "Header mismatch") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}
