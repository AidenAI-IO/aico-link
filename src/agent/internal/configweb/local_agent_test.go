package configweb

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"aico-agent/internal/agent"
	"aico-agent/internal/configupdate"
)

// Exercise the HTTP -> subprocess -> real config-update persistence boundary
// without building a second daemon inside the test suite.
func TestLocalAgentCLIHelper(t *testing.T) {
	if os.Getenv("AICO_TEST_LOCAL_CLI") != "1" {
		return
	}
	args := os.Args
	for len(args) > 0 && args[0] != "--" {
		args = args[1:]
	}
	args = args[1:]
	var path string
	for _, arg := range args {
		if strings.HasPrefix(arg, "--config=") {
			path = strings.TrimPrefix(arg, "--config=")
		}
	}
	var value any
	var err error
	switch args[0] {
	case "config":
		var cfg agent.Config
		cfg, err = agent.LoadResolvedConfigForUpdate(path)
		value = configupdate.FromAgentConfig(cfg)
	case "config-meta":
		value = agent.ConfigMeta()
	case "config-update":
		body, _ := io.ReadAll(os.Stdin)
		if strings.Contains(strings.Join(args, " "), "--dry-run") {
			value, err = configupdate.NewService().Plan(path, body)
		} else {
			value, err = configupdate.NewService().Update(path, body)
		}
	}
	if err != nil {
		_ = json.NewEncoder(os.Stdout).Encode(map[string]any{"ok": false, "error": err.Error()})
		os.Exit(1)
	}
	_ = json.NewEncoder(os.Stdout).Encode(value)
	os.Exit(0)
}

func localTestServer(t *testing.T, agentURL string) *Server {
	t.Helper()
	t.Setenv("AICO_TEST_LOCAL_CLI", "1")
	options := testOptions(t)
	options.LocalAgent = true
	options.AgentHTTPBaseURL = agentURL
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	options.AgentBinary = filepath.Join(t.TempDir(), "agent")
	script := "#!/bin/sh\nexec '" + strings.ReplaceAll(executable, "'", "'\\''") + "' -test.run=^TestLocalAgentCLIHelper$ -- \"$@\"\n"
	if err := os.WriteFile(options.AgentBinary, []byte(script), 0700); err != nil {
		t.Fatal(err)
	}
	const config = `[basic_settings.device]
device_type = "iOS"
[model_settings.providers.main]
type = "openai"
api_key = "local-test-secret"
[model_settings.model]
provider = "main"
model = "gpt-5.5"
[basic_settings.device.hid]
keyboard_layout = "qwerty" # preserve device settings
`
	if err := os.WriteFile(options.AgentConfigPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(options.WebRoot, "local.html"), []byte("local settings"), 0600); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = server.Shutdown(context.Background()) })
	return server
}

func localRequest(server *Server, method, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "http://127.0.0.1:8000"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	server.ServeHTTP(w, req)
	return w
}

func TestLocalAgentConfigRoundTrip(t *testing.T) {
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, `{"ok":true,"data":{"status":"ok","bridge_type":"go-agent","device_type":"iOS"}}`)
		case "/api/internal/config/reload":
			fmt.Fprint(w, `{"ok":true,"applied":false,"pending":true,"state":"pending"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer runtime.Close()
	s := localTestServer(t, runtime.URL)
	if s.storage != nil || s.maintenance != nil || s.services != nil || s.usbHTTP != nil || s.usbTerminal != nil || s.lanAccess != nil {
		t.Fatal("local portal initialized board services")
	}
	for _, path := range []string{"/api/device/snapshot", "/api/config", "/api/config/schema"} {
		w := localRequest(s, "GET", path, "")
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", path, w.Code, w.Body)
		}
		for _, forbidden := range []string{"local-test-secret", "keyboard_layout", "tap_wakeup_enabled", "wifi_status", "system_env", `"tts"`, `"storage"`} {
			if strings.Contains(w.Body.String(), forbidden) {
				t.Fatalf("%s exposes %s", path, forbidden)
			}
		}
		if path == "/api/device/snapshot" && !strings.Contains(w.Body.String(), `"process_running":true`) {
			t.Fatal("Agent health was ignored")
		}
	}
	patch := `{"config":{"log":{"level":"debug"},"model_providers":{"main":{"base_url":"https://example.com/v1"}}}}`
	for _, step := range []struct{ method, path string }{{"POST", "/api/config/plan"}, {"PATCH", "/api/config"}} {
		w := localRequest(s, step.method, step.path, patch)
		if w.Code != 200 {
			t.Fatalf("%s: %d %s", step.path, w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "local-test-secret") || strings.Contains(w.Body.String(), "keyboard_layout") {
			t.Fatal("save leaked excluded configuration")
		}
	}
	saved, err := os.ReadFile(s.options.AgentConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`level = "debug"`, "local-test-secret", "# preserve device settings"} {
		if !strings.Contains(string(saved), want) {
			t.Fatalf("save lost %q", want)
		}
	}
	runtime.Close()
	w := localRequest(s, "GET", "/api/device/snapshot", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"state":"offline"`) {
		t.Fatal("offline Agent prevents configuration access")
	}
	w = localRequest(s, "PATCH", "/api/config", `{"config":{"log":{"level":"info"}}}`)
	if w.Code != 503 || !strings.Contains(w.Body.String(), `"persisted":true`) || !strings.Contains(w.Body.String(), `"applied":false`) {
		t.Fatalf("offline save hid pending application: %d %s", w.Code, w.Body)
	}
}

func TestLocalAgentUnreadableConfigReportsRepairState(t *testing.T) {
	s := localTestServer(t, "http://127.0.0.1:1")
	if err := os.WriteFile(s.options.AgentConfigPath, []byte("[broken"), 0600); err != nil {
		t.Fatal(err)
	}
	w := localRequest(s, "GET", "/api/device/snapshot", "")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d: %s", w.Code, w.Body)
	}
	var response struct {
		Code  string            `json:"code"`
		Paths map[string]string `json:"paths"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != "config_read_failed" || response.Paths["agent_config"] != s.options.AgentConfigPath {
		t.Fatalf("missing repair state: %s", w.Body)
	}
}

func TestLocalAgentRejectsBoardOperations(t *testing.T) {
	s := localTestServer(t, "http://127.0.0.1:1")
	before, _ := os.ReadFile(s.options.AgentConfigPath)
	for _, patch := range []string{`{"hid":{"keyboard_layout":"azerty"}}`, `{"agent":{"input_mode":"stt"}}`, `{"agent":null}`, `{"model_providers":null}`, `{"model":{"gpio_pin":1}}`, `{"model_providers":{"main":{"unknown":true}}}`, `{"agent":{"timezone":"UTC"},"storage":{}}`, `{"_provider_renames":{"tts_providers":{}}}`} {
		for _, step := range []struct{ method, path string }{{"PATCH", "/api/config"}, {"POST", "/api/config/plan"}} {
			w := localRequest(s, step.method, step.path, `{"config":`+patch+`}`)
			if w.Code != 400 {
				t.Fatalf("accepted %s: %d %s", patch, w.Code, w.Body)
			}
		}
	}
	for _, route := range []struct{ method, path string }{{"POST", "/api/device/reboot"}, {"POST", "/api/agent/restart"}, {"PUT", "/api/config/backup"}, {"GET", "/api/system/environment"}, {"POST", "/api/storage/format"}, {"POST", "/api/config-test/stt/start"}} {
		if w := localRequest(s, route.method, route.path, `{}`); w.Code != 404 {
			t.Fatalf("board API %s: %d", route.path, w.Code)
		}
	}
	if w := localRequest(s, "POST", "/api/config/test", `{"section":"tts","values":{}}`); w.Code != 400 {
		t.Fatal("voice test was exposed")
	}
	for _, path := range []string{"/wifi", "/firmware", "/advanced/config", "/basic", "/voice"} {
		if w := localRequest(s, "GET", path, ""); w.Code != 404 {
			t.Fatalf("board page %s: %d", path, w.Code)
		}
	}
	if w := localRequest(s, "GET", "/model/main", ""); w.Code != 200 || w.Body.String() != "local settings" {
		t.Fatal("local deep link did not serve the host shell")
	}
	after, _ := os.ReadFile(s.options.AgentConfigPath)
	if string(before) != string(after) {
		t.Fatal("rejected operations changed the configuration")
	}
}

func TestLocalAgentLoopbackBoundaryAndEnvironment(t *testing.T) {
	s := localTestServer(t, "http://127.0.0.1:1")
	for _, endpoint := range []string{"https://example.com", "http://example.com", "http://user:password@localhost", "http://127.0.0.1/other"} {
		options := s.options
		options.AgentHTTPBaseURL = endpoint
		if _, err := NewServer(options); err == nil {
			t.Fatalf("accepted %s", endpoint)
		}
	}
	options := s.options
	options.BindAddress = "0.0.0.0"
	if _, err := NewServer(options); err == nil {
		t.Fatal("local settings bound to LAN")
	}
	for _, item := range []struct{ host, origin string }{{"attacker.example", ""}, {"127.0.0.1:8000", "https://attacker.example"}} {
		r := httptest.NewRequest("PATCH", "http://"+item.host+"/api/config", strings.NewReader(`{"config":{"log":{"level":"debug"}}}`))
		r.Header.Set("Origin", item.origin)
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("accepted cross-origin request: %d", w.Code)
		}
	}
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1234")
	if err := os.WriteFile(s.options.SystemEnvPath, []byte("invalid board env"), 0600); err != nil {
		t.Fatal(err)
	}
	env, err := s.agentCommandEnvironment()
	if err != nil || !strings.Contains(strings.Join(env, "\n"), "HTTP_PROXY=http://127.0.0.1:1234") {
		t.Fatal("host proxy environment was replaced")
	}
}

func TestLocalAgentMirroringProxy(t *testing.T) {
	for _, upstreamStatus := range []int{200, 409, 503} {
		t.Run(fmt.Sprint(upstreamStatus), func(t *testing.T) {
			calls := 0
			runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if r.Method != "POST" || r.URL.Path != "/api/internal/iphone-mirroring" || r.URL.RawQuery != "" {
					t.Errorf("unexpected proxy request %s %s", r.Method, r.URL)
				}
				body, _ := io.ReadAll(r.Body)
				if string(body) != `{}` {
					t.Error("browser input forwarded to Agent")
				}
				w.WriteHeader(upstreamStatus)
				if upstreamStatus == 200 {
					fmt.Fprint(w, `{"ok":true,"supported":true,"restored":true,"screenshot_available":true}`)
				} else {
					fmt.Fprint(w, `{"ok":false,"code":"session_busy","error":"busy"}`)
				}
			}))
			defer runtime.Close()
			s := localTestServer(t, runtime.URL)
			w := localRequest(s, "POST", "/api/device/iphone-mirroring/reconnect?endpoint=other", `{"reset":true}`)
			if w.Code != upstreamStatus || calls != 1 {
				t.Fatalf("status=%d calls=%d", w.Code, calls)
			}
			s.options.LocalAgent = false
			w = localRequest(s, "POST", "/api/device/iphone-mirroring/reconnect", `{}`)
			if w.Code != 404 || calls != 1 {
				t.Fatal("board mode exposes mirroring reconnect")
			}
		})
	}
}
