package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"aico-agent/internal/mcpclient"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/tmc/langchaingo/llms"
	"github.com/tmc/langchaingo/schema"
	langtools "github.com/tmc/langchaingo/tools"
)

func TestMCPClientManagementAndAgentExecution(t *testing.T) {
	remote := mcp.NewServer(&mcp.Implementation{Name: "typed", Version: "1"}, nil)
	remote.AddTool(&mcp.Tool{Name: "echo", InputSchema: map[string]any{"type": "object"}}, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: string(r.Params.Arguments)}}}, nil
	})
	h := httptest.NewServer(mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return remote }, &mcp.StreamableHTTPOptions{JSONResponse: true, Stateless: true}))
	defer h.Close()
	m, err := mcpclient.New(filepath.Join(t.TempDir(), "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()
	rt := NewRuntimeWithDeps(Config{}, nil, nil, &ToolSet{tools: map[string]langtools.Tool{}}, nil)
	rt.mcpClients = m
	s := &Server{runtime: rt}
	call := func(method, path, body string, local bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest(method, "http://127.0.0.1"+path, strings.NewReader(body))
		r.RemoteAddr = "127.0.0.1:1234"
		if !local {
			r.RemoteAddr = "192.0.2.1:1234"
		}
		w := httptest.NewRecorder()
		s.handleInternalMCPServers(w, r)
		return w
	}
	if w := call("POST", "/api/internal/mcp-servers", `{"mcpServers":{"typed":{"url":"`+h.URL+`"}}}`, true); w.Code != 200 {
		t.Fatalf("install: %d %s", w.Code, w.Body)
	}
	tools := rt.AvailableTools()
	if len(tools) != 1 {
		t.Fatalf("not in Agent catalog: %v", tools)
	}
	tool := tools[0]
	if _, ok := rt.Tool(tool.Name()); !ok {
		t.Fatal("runtime Tool lookup missed MCP tool")
	}
	if NewToolSpec(tool).HTTPExposed {
		t.Fatal("installed tool leaked to device HTTP tools")
	}
	for _, raw := range []string{`{"input":"hello"}`, `{"__arg1":"kept","id":9007199254740993}`} {
		a := &FunctionAgent{Tools: tools}
		actions, _, err := a.ParseOutput(&llms.ContentResponse{Choices: []*llms.ContentChoice{{ToolCalls: []llms.ToolCall{{ID: "call", Type: "function", FunctionCall: &llms.FunctionCall{Name: tool.Name(), Arguments: raw}}}}}})
		if err != nil || len(actions) != 1 || actions[0].ToolInput != raw {
			t.Fatalf("typed arguments altered: %v %v", actions, err)
		}
		result := executeToolCall(context.Background(), ToolCallExecution{Specs: NewToolSpecs(tools), Action: schema.AgentAction{Tool: tool.Name(), ToolInput: actions[0].ToolInput}})
		if result.Result.IsError() {
			t.Fatalf("execution: %+v", result.Result)
		}
		var response struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal([]byte(result.Result.Output), &response) != nil || len(response.Content) != 1 || response.Content[0].Text != raw {
			t.Fatalf("arguments lost in transit: %s", result.Result.Output)
		}
	}
	if w := call("DELETE", "/api/internal/mcp-servers/typed", "", false); w.Code != 404 {
		t.Fatal("non-loopback management accepted")
	}
	r := httptest.NewRequest("POST", "http://127.0.0.1/api/internal/mcp-servers", strings.NewReader(`{}`))
	r.RemoteAddr = "127.0.0.1:1234"
	r.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	s.handleInternalMCPServers(w, r)
	if w.Code != 403 {
		t.Fatal("cross-origin management accepted")
	}
	if w := call("PATCH", "/api/internal/mcp-servers/typed", `{"enabled":false}`, true); w.Code != 200 {
		t.Fatal(w.Body)
	}
	if len(rt.AvailableTools()) != 0 {
		t.Fatal("disabled server still visible to model")
	}
	if w := call("DELETE", "/api/internal/mcp-servers/typed", "", true); w.Code != 200 {
		t.Fatal(w.Body)
	}
}
