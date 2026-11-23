package mcpclient

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func testServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	s.AddTool(&mcp.Tool{Name: "echo", Description: "Echo typed input", InputSchema: map[string]any{
		"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []string{"input"},
	}}, func(_ context.Context, r *mcp.CallToolRequest) (*mcp.CallToolResult, error) {
		var input map[string]any
		if err := json.Unmarshal(r.Params.Arguments, &input); err != nil {
			return nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprint(input["input"])}}, StructuredContent: input, IsError: input["input"] == "fail"}, nil
	})
	return s
}

func TestMCPStdioHelper(t *testing.T) {
	if os.Getenv("AICO_MCP_TEST_HELPER") != "1" {
		return
	}
	if err := testServer().Run(context.Background(), &mcp.StdioTransport{}); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

func newTestManager(t *testing.T) *Manager {
	t.Helper()
	m, err := New(filepath.Join(t.TempDir(), "mcp-servers.json"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(m.Close)
	return m
}

func TestMCPTransportsAndLifecycle(t *testing.T) {
	for _, kind := range []string{"stdio", "json-http", "sse-http"} {
		t.Run(kind, func(t *testing.T) {
			cfg := Config{Command: os.Args[0], Args: []string{"-test.run=^TestMCPStdioHelper$"}, Env: map[string]string{"AICO_MCP_TEST_HELPER": "1"}}
			if kind != "stdio" {
				s := testServer()
				handler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return s }, &mcp.StreamableHTTPOptions{JSONResponse: kind == "json-http", Stateless: kind == "json-http"})
				h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Header.Get("Authorization") != "Bearer private-test-token" {
						w.WriteHeader(401)
						return
					}
					handler.ServeHTTP(w, r)
				}))
				t.Cleanup(h.Close)
				cfg = Config{URL: h.URL, Headers: map[string]string{"Authorization": "Bearer private-test-token"}}
			}
			m := newTestManager(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			if err := m.Install(ctx, Document{Servers: map[string]Config{"demo": cfg}}); err != nil {
				t.Fatal(err)
			}
			tools := m.Tools()
			if len(tools) != 1 {
				t.Fatalf("tools: %v", tools)
			}
			// Installation may finish before the background startup reconnect.
			// The original handle must remain usable after Start.
			m.Start()
			if got := tools[0].ArgsSchema()["properties"].(map[string]any)["input"].(map[string]any)["type"]; got != "string" {
				t.Fatalf("schema lost: %v", got)
			}
			out, err := tools[0].Call(ctx, `{"input":"hello"}`)
			if err != nil || !strings.Contains(out, `"input":"hello"`) {
				t.Fatalf("call: %s %v", out, err)
			}
			out, err = tools[0].Call(ctx, `{"input":"fail"}`)
			if err == nil || !strings.Contains(out, `"isError":true`) || !strings.Contains(err.Error(), `"input":"fail"`) {
				t.Fatalf("tool error lost: %s %v", out, err)
			}
			if _, err := tools[0].Call(ctx, `[]`); err == nil {
				t.Fatal("non-object accepted")
			}
			canceled, stop := context.WithCancel(ctx)
			stop()
			if _, err := tools[0].Call(canceled, `{}`); err != context.Canceled {
				t.Fatalf("cancellation lost: %v", err)
			}
			info, err := os.Stat(m.path)
			if err != nil || info.Mode().Perm() != 0600 {
				t.Fatalf("private file: %v %v", info, err)
			}
			status, _ := json.Marshal(m.Status())
			if strings.Contains(string(status), "private-test-token") {
				t.Fatal("credential leaked in status")
			}
			if err := m.Install(ctx, Document{Servers: map[string]Config{"demo": cfg}}); err == nil {
				t.Fatal("duplicate replaced existing server")
			}
			disabled := false
			if err := m.Change(ctx, "demo", &disabled); err != nil {
				t.Fatal(err)
			}
			if len(m.Tools()) != 0 {
				t.Fatal("disabled tools still published")
			}
			if _, err := tools[0].Call(ctx, `{}`); err == nil {
				t.Fatal("stale handle still usable")
			}
			enabled := true
			if err := m.Change(ctx, "demo", &enabled); err != nil {
				t.Fatal(err)
			}
			loaded, err := New(m.path)
			if err != nil {
				t.Fatal(err)
			}
			defer loaded.Close()
			loaded.Start()
			if len(loaded.Tools()) != 1 {
				t.Fatalf("restart failed: %v", loaded.Status())
			}
			if err := m.Change(ctx, "demo", nil); err != nil {
				t.Fatal(err)
			}
			if len(m.Tools()) != 0 || len(m.Status()) != 0 {
				t.Fatal("remove did not clear catalog")
			}
			empty, err := New(m.path)
			if err != nil {
				t.Fatal(err)
			}
			empty.Close()
		})
	}
}

func TestMCPInstallRollbackAndTimeout(t *testing.T) {
	m := newTestManager(t)
	release := make(chan struct{})
	defer close(release)
	h := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	t.Cleanup(h.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	err := m.Install(ctx, Document{Servers: map[string]Config{"pending": {URL: h.URL}}})
	if err == nil || len(m.Status()) != 0 {
		t.Fatalf("failed install persisted: %v %v", m.Status(), err)
	}
	if _, err := os.Stat(m.path); !os.IsNotExist(err) {
		t.Fatalf("config written on failure: %v", err)
	}
	// A persistence failure must also leave the live catalog unchanged.
	m.path = filepath.Join(t.TempDir(), "file", "servers.json")
	if err := os.WriteFile(filepath.Dir(m.path), []byte("block directory"), 0600); err != nil {
		t.Fatal(err)
	}
	err = m.Install(context.Background(), Document{Servers: map[string]Config{"disabled": {Command: "not-started", Disabled: true}}})
	if err == nil || len(m.Status()) != 0 {
		t.Fatal("persistence failure changed live state")
	}
}

func TestMCPConfigValidation(t *testing.T) {
	for _, raw := range []string{
		`null`, `{"mcpServers":{}}`, `{"mcpServers":{"../escape":{"command":"x"}}}`,
		`{"mcpServers":{"demo":{"type":"sse","url":"https://example.com"}}}`,
		`{"mcpServers":{"demo":{"url":"file:///etc/passwd"}}}`,
		`{"mcpServers":{"demo":{"url":"https://user:secret@example.com/mcp"}}}`,
		`{"mcpServers":{"demo":{"command":"x","url":"https://example.com"}}}`,
		`{"mcpServers":{"demo":{"url":"https://example.com","headers":{"MCP-Session-Id":"x"}}}}`,
		`{"mcpServers":{"demo":{"command":"x","unknown":true}}}`,
		`{"mcpServers":{"demo":{"command":"x"}}} {}`,
		strings.Repeat(" ", MaxConfigBytes+1),
	} {
		if _, err := Decode(strings.NewReader(raw)); err == nil {
			t.Fatalf("accepted %s", raw[:min(len(raw), 100)])
		}
	}
}

// Opt-in integration against independently implemented reference servers. The
// caller installs pinned npm packages outside the checkout; CI unit tests stay
// offline and deterministic.
func TestMCPReferenceServers(t *testing.T) {
	root := os.Getenv("AICO_MCP_REFERENCE_ROOT")
	if root == "" {
		t.Skip("set AICO_MCP_REFERENCE_ROOT to the npm install prefix")
	}
	m := newTestManager(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("MCP filesystem verified"), 0600); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, pkg, tool, input, want string
		args                         []string
		env                          map[string]string
	}{
		{name: "filesystem", pkg: "server-filesystem", tool: "read_file", args: []string{dir}, input: fmt.Sprintf(`{"path":%q}`, filepath.Join(dir, "hello.txt")), want: "MCP filesystem verified"},
		{name: "memory", pkg: "server-memory", tool: "create_entities", env: map[string]string{"MEMORY_FILE_PATH": filepath.Join(dir, "memory.jsonl")}, input: `{"entities":[{"name":"MCP test","entityType":"validation","observations":["Agent installed memory MCP"]}]}`, want: "Agent installed memory MCP"},
		{name: "thinking", pkg: "server-sequential-thinking", tool: "sequentialthinking", env: map[string]string{"DISABLE_THOUGHT_LOGGING": "true"}, input: `{"thought":"Confirm a short plan","nextThoughtNeeded":false,"thoughtNumber":1,"totalThoughts":1}`, want: "thoughtNumber"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Config{Command: "node", Args: append([]string{filepath.Join(root, "node_modules", "@modelcontextprotocol", tc.pkg, "dist", "index.js")}, tc.args...), Env: tc.env}
			if err := m.Install(context.Background(), Document{Servers: map[string]Config{tc.name: cfg}}); err != nil {
				t.Fatal(err)
			}
			var found bool
			for _, tool := range m.Tools() {
				if tool.server == tc.name && tool.remote == tc.tool {
					found = true
					out, err := tool.Call(context.Background(), tc.input)
					if err != nil || !strings.Contains(out, tc.want) {
						t.Fatalf("call: %s %v", out, err)
					}
					for _, status := range m.Status() {
						if status.Name == tc.name {
							t.Logf("%s: discovered %d tools, %s succeeded", tc.name, len(status.Tools), tc.tool)
						}
					}
				}
			}
			if !found {
				t.Fatalf("reference tool %s missing", tc.tool)
			}
		})
	}
}
