package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// environmentBridgeStub answers /api/tools/open_app like the ADB bridge does.
func environmentBridgeStub(t *testing.T, handler func(app string) (int, map[string]any)) (*httptest.Server, *[]string) {
	t.Helper()
	var requests []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/tools/open_app" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("benchmark-task-id"); got != "suite:task" {
			t.Errorf("benchmark-task-id = %q, want suite:task", got)
		}
		body, _ := io.ReadAll(r.Body)
		var envelope struct {
			Input string `json:"input"`
		}
		if err := json.Unmarshal(body, &envelope); err != nil {
			t.Errorf("decode request: %v", err)
		}
		var input struct {
			App string `json:"app"`
		}
		_ = json.Unmarshal([]byte(envelope.Input), &input)
		requests = append(requests, input.App)
		status, payload := handler(input.App)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_ = json.NewEncoder(w).Encode(payload)
	}))
	t.Cleanup(server.Close)
	return server, &requests
}

func launchedObservation(app string) map[string]any {
	inner, _ := json.Marshal(map[string]any{"ok": true, "target": app, "package": "com.google.android.apps.maps"})
	outer, _ := json.Marshal(map[string]any{"action_output": string(inner)})
	return map[string]any{"output": string(outer), "is_error": false}
}

func TestOpenAppLaunchesThroughEnvironmentBridgeBeforeVisualSearch(t *testing.T) {
	server, requests := environmentBridgeStub(t, func(app string) (int, map[string]any) {
		return http.StatusOK, launchedObservation(app)
	})
	search := &recordingTextInputTool{name: toolSearchLaunchApp, out: `{"ok":true}`}
	tool := NewOpenAppTool(nil, nil, search)
	tool.environment = newEnvironmentAppLauncher(server.URL, "suite:task")
	var logs []string
	tool.logf = func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) }

	ctx, _ := WithToolError(context.Background())
	out, err := tool.Call(ctx, `{"app":"Maps"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if te := ToolErrorFromContext(ctx); te != nil {
		t.Fatalf("unexpected ToolError: %+v", te)
	}
	if len(search.calls) != 0 {
		t.Fatalf("search calls = %#v, want the environment launch to win", search.calls)
	}
	if got := strings.Join(*requests, ","); got != "Maps" {
		t.Fatalf("environment requests = %q, want Maps", got)
	}
	var result map[string]any
	if err := json.Unmarshal([]byte(out), &result); err != nil || result["ok"] != true || result["package"] != "com.google.android.apps.maps" {
		t.Fatalf("output = %s, want the bridge launch result: %v", out, err)
	}
	if got := strings.Join(logs, "\n"); !strings.Contains(got, "selected=environment_open_app") || strings.Contains(got, "selected=search_launch_app") {
		t.Fatalf("route logs = %q, want only the environment route", got)
	}
}

func TestOpenAppFallsBackToSearchWhenEnvironmentDeclines(t *testing.T) {
	server, _ := environmentBridgeStub(t, func(string) (int, map[string]any) {
		declined, _ := json.Marshal(map[string]any{"ok": false, "suggestion": "No installed launcher app matches this name."})
		return http.StatusOK, map[string]any{"output": string(declined), "is_error": false}
	})
	search := &recordingTextInputTool{name: toolSearchLaunchApp, out: `{"ok":true,"target":"Snapchat"}`}
	tool := NewOpenAppTool(nil, nil, search)
	tool.environment = newEnvironmentAppLauncher(server.URL, "suite:task")

	out, err := tool.Call(context.Background(), `{"app":"Snapchat"}`)
	if err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if out != search.out || len(search.calls) != 1 {
		t.Fatalf("output = %s calls = %#v, want one search fallback", out, search.calls)
	}
}

func TestOpenAppFallsBackToSearchWhenEnvironmentHasNoLauncher(t *testing.T) {
	for name, respond := range map[string]func(string) (int, map[string]any){
		"http 404": func(string) (int, map[string]any) { return http.StatusNotFound, map[string]any{} },
		"unknown tool": func(string) (int, map[string]any) {
			return http.StatusOK, map[string]any{"output": "unknown tool: open_app", "is_error": true, "error": "unknown_tool"}
		},
		"server error": func(string) (int, map[string]any) { return http.StatusInternalServerError, map[string]any{} },
	} {
		t.Run(name, func(t *testing.T) {
			server, _ := environmentBridgeStub(t, respond)
			search := &recordingTextInputTool{name: toolSearchLaunchApp, out: `{"ok":true}`}
			tool := NewOpenAppTool(nil, nil, search)
			tool.environment = newEnvironmentAppLauncher(server.URL, "suite:task")

			ctx, _ := WithToolError(context.Background())
			out, err := tool.Call(ctx, `{"app":"Maps"}`)
			if err != nil {
				t.Fatalf("Call returned err: %v", err)
			}
			if te := ToolErrorFromContext(ctx); te != nil {
				t.Fatalf("environment failure leaked a ToolError: %+v", te)
			}
			if out != search.out || len(search.calls) != 1 {
				t.Fatalf("output = %s calls = %#v, want the search fallback", out, search.calls)
			}
		})
	}
}

func TestOpenAppWithoutEnvironmentLauncherKeepsSearchRoute(t *testing.T) {
	search := &recordingTextInputTool{name: toolSearchLaunchApp, out: `{"ok":true}`}
	tool := NewOpenAppTool(nil, nil, search)

	if _, err := tool.Call(context.Background(), `{"app":"Maps"}`); err != nil {
		t.Fatalf("Call returned err: %v", err)
	}
	if len(search.calls) != 1 {
		t.Fatalf("search calls = %#v, want one", search.calls)
	}
}

func TestEnvironmentAppLauncherIsOnlyWiredInEnvironmentBridgeMode(t *testing.T) {
	if newEnvironmentAppLauncher("  ", "task") != nil {
		t.Fatal("empty endpoint produced a launcher")
	}
	cfg := Config{}
	if set := NewBuiltinToolSetFromConfig(cfg, ProxyConfig{}); set.environmentLauncher != nil {
		t.Fatal("hardware tool set gained an environment launcher")
	}
	cfg.EnvironmentBridge = EnvironmentBridgeConfig{Enabled: true, Endpoint: "http://bridge.invalid/", BenchmarkTaskID: "suite:task"}
	set := NewBuiltinToolSetFromConfig(cfg, ProxyConfig{})
	if set.environmentLauncher == nil || set.environmentLauncher.endpoint != "http://bridge.invalid" || set.environmentLauncher.taskID != "suite:task" {
		t.Fatalf("environment launcher = %#v, want the bridge endpoint and task", set.environmentLauncher)
	}
}
