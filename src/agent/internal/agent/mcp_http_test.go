package agent

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestMCPDisabledReturnsNotFound(t *testing.T) {
	disabled := false
	runtime := &Runtime{config: Config{MCPEnabled: &disabled}}
	server := &Server{runtime: runtime}
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader([]byte(`{"jsonrpc":"2.0","id":1,"method":"ping"}`)))
	req.Header.Set("Accept", "application/json, text/event-stream")
	rec := httptest.NewRecorder()
	server.handleMCP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d want 404", rec.Code)
	}
}

func TestMCPEnabledInitialize(t *testing.T) {
	enabled := true
	runtime := NewRuntimeWithDeps(Config{MCPEnabled: &enabled, MCPShellEnabled: boolPtr(false)}, nil, nil, nil, NewSkillIndex())
	t.Cleanup(func() { runtime.Close() })
	server := &Server{runtime: runtime}
	body, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "initialize",
		"params": map[string]any{
			"protocolVersion": "2025-03-26",
			"capabilities":    map[string]any{},
			"clientInfo":      map[string]any{"name": "test", "version": "0"},
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	server.handleMCP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte(`"aiden-agent"`)) {
		t.Fatalf("body=%s", rec.Body.String())
	}
}

func TestMCPEnabledOrDefault(t *testing.T) {
	if !(Config{}).MCPEnabledOrDefault() {
		t.Fatal("default mcp_enabled want true")
	}
	if (Config{}).MCPShellEnabledOrDefault() {
		t.Fatal("default mcp_shell_enabled want false")
	}
	off := false
	if (Config{MCPEnabled: &off}).MCPEnabledOrDefault() {
		t.Fatal("explicit false")
	}
}

func TestValidateMCPConfigWebBaseURL(t *testing.T) {
	if err := validateMCPConfigWebBaseURL("http://127.0.0.1"); err != nil {
		t.Fatal(err)
	}
	if err := validateMCPConfigWebBaseURL("http://localhost:80"); err != nil {
		t.Fatal(err)
	}
	if err := validateMCPConfigWebBaseURL("http://192.168.1.1"); err == nil {
		t.Fatal("non-loopback should fail")
	}
	if err := validateMCPConfigWebBaseURL("https://127.0.0.1"); err == nil {
		t.Fatal("https should fail")
	}
}
