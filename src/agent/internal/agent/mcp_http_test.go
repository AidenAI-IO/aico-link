package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
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

func TestMCPContentFromToolOutputImage(t *testing.T) {
	blocks := mcpContentFromToolOutput(`{"width":10,"height":8,"format":"jpeg","size":4,"data":"ZmFrZQ=="}`)
	if len(blocks) != 2 {
		t.Fatalf("blocks=%d want 2", len(blocks))
	}
	if blocks[0].Type != "image" || blocks[0].MIMEType != "image/jpeg" || blocks[0].Data != "ZmFrZQ==" {
		t.Fatalf("image block=%+v", blocks[0])
	}
	if blocks[1].Type != "text" || !bytes.Contains([]byte(blocks[1].Text), []byte(`"width":10`)) {
		t.Fatalf("text block=%+v", blocks[1])
	}
	if bytes.Contains([]byte(blocks[1].Text), []byte("ZmFrZQ==")) || bytes.Contains([]byte(blocks[1].Text), []byte(`"data"`)) {
		t.Fatalf("metadata must not include data: %s", blocks[1].Text)
	}
}

func TestMCPContentFromToolOutputPlainText(t *testing.T) {
	blocks := mcpContentFromToolOutput("not-json")
	if len(blocks) != 1 || blocks[0].Type != "text" || blocks[0].Text != "not-json" {
		t.Fatalf("blocks=%+v", blocks)
	}
}

func TestSupportBundleRejectsWhenBusy(t *testing.T) {
	mcpSupportBundleSlots <- struct{}{}
	t.Cleanup(func() { <-mcpSupportBundleSlots })

	_, _, err := (mcpServerBackend{}).SupportBundle(context.Background())
	if err == nil || !strings.Contains(err.Error(), "busy") {
		t.Fatalf("err=%v want busy", err)
	}
}
