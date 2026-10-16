package agent

import (
	"context"
	"encoding/json"
	langtools "github.com/tmc/langchaingo/tools"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

type mockPolicyTestTool struct{ name string }

func (t mockPolicyTestTool) Name() string        { return t.name }
func (t mockPolicyTestTool) Description() string { return "native description" }
func (t mockPolicyTestTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{"text": stringArgSchema("Text")}, "text")
}
func (t mockPolicyTestTool) Call(context.Context, string) (string, error) {
	panic("native execution must not run")
}

func TestBenchmarkMockToolsRequireExplicitAuthenticatedMode(t *testing.T) {
	tools := []langtools.Tool{mockPolicyTestTool{"enter_text"}}
	cfg := Config{EnvironmentBridge: EnvironmentBridgeConfig{Enabled: true, Endpoint: "http://fixture"}, Benchmark: BenchmarkConfig{Token: "token", MockTools: true}}
	for _, disable := range []func(*Config){func(c *Config) { c.Benchmark.MockTools = false }, func(c *Config) { c.Benchmark.Token = "" }, func(c *Config) { c.EnvironmentBridge.Enabled = false }, func(c *Config) { c.EnvironmentBridge.Endpoint = "" }} {
		c := cfg
		disable(&c)
		if _, wrapped := benchmarkMockTools(c, tools)[0].(*benchmarkMockTool); wrapped {
			t.Fatal("mock enabled without all prerequisites")
		}
	}
}

func TestBenchmarkMockToolsPreserveSchemasAndRejectFixtureFailures(t *testing.T) {
	fail := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/fixture/api/tools/touch_gesture" || r.Header.Get("benchmark-task-id") != "task-1" {
			t.Errorf("unexpected request: %s", r.URL)
		}
		var body map[string]string
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["input"] != `{"type":"tap","point":{"x":180,"y":310}}` {
			t.Errorf("input=%v", body)
		}
		json.NewEncoder(w).Encode(map[string]any{"output": `{"ok":true}`, "is_error": fail, "error": "fixture mismatch"})
	}))
	defer server.Close()
	cfg := Config{EnvironmentBridge: EnvironmentBridgeConfig{Enabled: true, Endpoint: server.URL + "/fixture", BenchmarkTaskID: "task-1"}, Benchmark: BenchmarkConfig{Token: "token", MockTools: true}}
	native := mockPolicyTestTool{"touch_gesture"}
	tools := benchmarkMockTools(cfg, []langtools.Tool{native, mockPolicyTestTool{"shell"}})
	wrapped := tools[0].(*benchmarkMockTool)
	if !reflect.DeepEqual(wrapped.ArgsSchema(), native.ArgsSchema()) || wrapped.Description() != native.Description()+"\n\n"+benchmarkMockExecutionDescription {
		t.Fatal("model schema changed")
	}
	if _, ok := tools[1].(*benchmarkMockTool); ok {
		t.Fatal("shell must execute normally")
	}
	input := `{"type":"tap","point":{"x":180,"y":310}}`
	if output, err := wrapped.Call(context.Background(), input); err != nil || output != `{"ok":true}` {
		t.Fatalf("output=%s err=%v", output, err)
	}
	fail = true
	if output, err := wrapped.Call(context.Background(), input); err != nil || output == `{"ok":true}` {
		t.Fatalf("fixture error swallowed: %s %v", output, err)
	}
}

func TestBenchmarkMockToolsDoNotPromiseNativePostActionScreenshots(t *testing.T) {
	base := mockPolicyTestTool{"touch_gesture"}
	native := newPostActionScreenshotTool(base, nil, 0)
	cfg := Config{EnvironmentBridge: EnvironmentBridgeConfig{Enabled: true, Endpoint: "http://fixture"}, Benchmark: BenchmarkConfig{Token: "token", MockTools: true}}
	wrapped := benchmarkMockTools(cfg, []langtools.Tool{native})[0]
	if wrapped.Description() != base.Description()+"\n\n"+benchmarkMockExecutionDescription {
		t.Fatalf("scripted action still promises native screenshot behavior: %s", wrapped.Description())
	}
	if !reflect.DeepEqual(wrapped.(structuredInputTool).ArgsSchema(), base.ArgsSchema()) {
		t.Fatal("base action schema changed")
	}
	cfg.Benchmark.MockTools = false
	if got := benchmarkMockTools(cfg, []langtools.Tool{native})[0]; got != native {
		t.Fatal("real-device tool lost its post-action screenshot wrapper")
	}
}

func TestBenchmarkMockRuntimeRetainsBridgeAvailabilityAndNativeSchemas(t *testing.T) {
	pip := false
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/phone-bridge/status" {
			json.NewEncoder(w).Encode(map[string]any{"connected": false, "platform": "ios", "app_state": "background", "pip_bridge_enabled": pip})
			return
		}
		if r.URL.Path != "/api/tools/bridge_contacts" {
			t.Errorf("unexpected path %s", r.URL.Path)
		}
		json.NewEncoder(w).Encode(map[string]any{"output": `{"ok":true,"contacts":[{"name":"Biden"}]}`})
	}))
	defer server.Close()
	cfg := Config{EnvironmentBridge: EnvironmentBridgeConfig{Enabled: true, Endpoint: server.URL}, Benchmark: BenchmarkConfig{Token: "token", MockTools: true}}
	runtime := newRuntimeWithTextEntryToolsWithConfig(cfg)
	bridge := NewPhoneBridgeProxy(server.URL, "task-1", nil)
	t.Cleanup(func() { bridge.queue.Stop() })
	runtime.tools.RegisterPhoneBridge(bridge)
	if _, exists := toolNameSet(runtime.AvailableTools())["bridge_contacts"]; exists {
		t.Fatal("unavailable contacts exposed")
	}
	pip = true
	for _, tool := range runtime.AvailableTools() {
		if tool.Name() != "bridge_contacts" {
			continue
		}
		if _, ok := tool.(*benchmarkMockTool); !ok {
			t.Fatal("native device transport still selected")
		}
		schema := tool.(structuredInputTool).ArgsSchema()
		if schema["properties"] == nil {
			t.Fatal("native schema lost")
		}
		output, err := tool.Call(context.Background(), `{"action":"query","query":"Biden"}`)
		if err != nil || output != `{"ok":true,"contacts":[{"name":"Biden"}]}` {
			t.Fatalf("mock call=%s %v", output, err)
		}
		return
	}
	t.Fatal("PiP contacts tool unavailable")
}
