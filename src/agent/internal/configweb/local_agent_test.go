package configweb

import (
	"archive/tar"
	"compress/gzip"
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
	"time"

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

func TestLocalAgentContainerSettingsRoundTrip(t *testing.T) {
	resetCalls := 0
	runtime := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			fmt.Fprint(w, `{"ok":true,"data":{"status":"ok","bridge_type":"go-agent","device_type":"Android"}}`)
			return
		}
		if r.Method == http.MethodPost && r.URL.Path == "/api/clear-all" {
			resetCalls++
		}
		fmt.Fprint(w, `{"ok":true,"applied":false,"pending":true,"state":"pending"}`)
	}))
	defer runtime.Close()
	base := localTestServer(t, runtime.URL)
	options := base.options
	options.LocalAgent = false
	options.ContainerAgent = true
	options.BindAddress = "0.0.0.0"
	options.AgentInitScript = "/bin/true"
	s, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Shutdown(context.Background()) })
	if s.storage != nil || s.services != nil || s.usbHTTP != nil || s.lanAccess != nil {
		t.Fatal("container settings initialized board services")
	}
	if err := os.WriteFile(filepath.Join(options.WebRoot, "container.html"), []byte("PC settings"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AICO_PUBLIC_AGENT_WEB_PORT", "51278")
	t.Setenv("AICO_ENABLE_TERMINAL", "0")
	t.Setenv("AICO_CLASSIC_VOICE", "0")
	w := localRequest(s, "GET", "/api/device/status", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"url":"http://127.0.0.1:51278"`) || !strings.Contains(w.Body.String(), `"device_type":"Android"`) ||
		!strings.Contains(w.Body.String(), `"classic_voice_enabled":false`) || strings.Contains(w.Body.String(), `"terminal_url"`) {
		t.Fatalf("container status: %d %s", w.Code, w.Body)
	}
	t.Setenv("AICO_ENABLE_TERMINAL", "1")
	t.Setenv("AICO_CLASSIC_VOICE", "1")
	w = localRequest(s, "GET", "/api/device/status", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"terminal_url":"http://127.0.0.1:51278/webtty/"`) ||
		!strings.Contains(w.Body.String(), `"classic_voice_enabled":true`) {
		t.Fatalf("enabled container capabilities: %d %s", w.Code, w.Body)
	}
	for _, path := range []string{"/", "/voice/model", "/model/main", "/memory"} {
		if w := localRequest(s, "GET", path, ""); w.Code != 200 || w.Body.String() != "PC settings" {
			t.Fatalf("container route %s: %d %s", path, w.Code, w.Body)
		}
	}
	patch := `{"config":{"agent":{"input_mode":"realtime"},"voice_model":{"provider":"browser"},"voice_model_providers":{"browser":{"type":"openai","api_key":"voice-test-secret","model":"gpt-realtime"}}}}`
	w = localRequest(s, "PATCH", "/api/config", patch)
	if w.Code != 200 || strings.Contains(w.Body.String(), "voice-test-secret") {
		t.Fatalf("voice settings: %d %s", w.Code, w.Body)
	}
	cfg, err := agent.LoadRuntimeConfig(options.AgentConfigPath)
	if err != nil || cfg.VoiceModel.APIKey != "voice-test-secret" {
		t.Fatalf("voice settings did not persist: %v", err)
	}
	patch = `{"config":{"agent":{"input_mode":"stt"},"stt":{"provider":"speech","language":"en"},"stt_providers":{"speech":{"type":"openai-whisper","api_key":"stt-test-secret"}},"tts":{"provider":"voice","speed":1.2},"tts_providers":{"voice":{"type":"minimax-cn","api_key":"tts-test-secret"}},"quick_capture":{"screen_memory_ttl":"30d"},"voice_notifications":{"retention_days":14}}}`
	w = localRequest(s, "PATCH", "/api/config", patch)
	if w.Code != 200 {
		t.Fatalf("Classic and memory settings: %d %s", w.Code, w.Body)
	}
	w = localRequest(s, "PATCH", "/api/config", `{"config":{"stt":{"language":"zh"}}}`)
	if w.Code != 200 {
		t.Fatalf("Classic preference update: %d %s", w.Code, w.Body)
	}
	cfg, err = agent.LoadRuntimeConfig(options.AgentConfigPath)
	if err != nil || cfg.InputMode != "stt" || cfg.STT.Language != "zh" || cfg.STT.APIKey != "stt-test-secret" ||
		cfg.TTS.APIKey != "tts-test-secret" || cfg.TTS.Speed != 1.2 || cfg.QuickCapture.ScreenMemoryTTL != "30d" || cfg.VoiceNotifications.RetentionDays != 14 ||
		cfg.Device.DeviceType != "iOS" {
		t.Fatalf("Classic/memory settings or preserved device settings did not persist: %v", err)
	}
	for _, path := range []string{"/api/device/snapshot", "/api/config", "/api/config/schema"} {
		w := localRequest(s, "GET", path, "")
		if w.Code != 200 || !strings.Contains(w.Body.String(), `"voice_model"`) || strings.Contains(w.Body.String(), "test-secret") {
			t.Fatalf("container voice configuration %s: %d %s", path, w.Code, w.Body)
		}
		var payload struct {
			Config   map[string]json.RawMessage `json:"config"`
			Sections []struct {
				Name   string `json:"name"`
				Fields []struct {
					Key string `json:"key"`
				} `json:"fields"`
			} `json:"sections"`
		}
		if err := json.Unmarshal(w.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		for _, forbidden := range []string{"device", "audio", "storage"} {
			if _, exposed := payload.Config[forbidden]; exposed {
				t.Fatalf("container configuration %s exposes %s", path, forbidden)
			}
			for _, section := range payload.Sections {
				if section.Name == forbidden {
					t.Fatalf("container schema exposes %s", forbidden)
				}
			}
		}
		for _, required := range []string{"stt", "stt_providers", "tts", "tts_providers", "quick_capture", "voice_notifications"} {
			if path != "/api/config/schema" {
				if _, exposed := payload.Config[required]; !exposed {
					t.Fatalf("container configuration %s omits %s", path, required)
				}
			} else {
				found := false
				for _, section := range payload.Sections {
					if section.Name == required {
						found = true
						for _, field := range section.Fields {
							if (required == "quick_capture" && (field.Key == "enabled" || field.Key == "gpio_pin")) ||
								(required == "voice_notifications" && field.Key == "enabled") {
								t.Fatalf("container schema exposes hardware fields in %s", required)
							}
						}
					}
				}
				if !found {
					t.Fatalf("container schema omits %s", required)
				}
			}
		}
		if data, ok := payload.Config["quick_capture"]; ok {
			var fields map[string]json.RawMessage
			_ = json.Unmarshal(data, &fields)
			if fields["screen_memory_ttl"] == nil || fields["enabled"] != nil || fields["gpio_pin"] != nil {
				t.Fatal("memory retention exposes board quick-capture fields")
			}
		}
	}
	for _, patch := range []string{`{"agent":{"input_mode":"invalid"}}`, `{"device":{"device_type":"iOS"}}`, `{"audio":{"volume":50}}`,
		`{"quick_capture":{"enabled":false}}`, `{"quick_capture":{"gpio_pin":0}}`, `{"voice_notifications":{"enabled":false}}`,
		`{"stt_providers":{"speech":{"gpio_pin":1}}}`} {
		w := localRequest(s, "PATCH", "/api/config", `{"config":`+patch+`}`)
		if w.Code != 400 {
			t.Fatalf("accepted board settings %s: %d", patch, w.Code)
		}
	}
	w = localRequest(s, "POST", "/api/memory/reset", "")
	if w.Code != 200 || resetCalls != 1 || !strings.Contains(w.Body.String(), `"agent_restart_scheduled":true`) {
		t.Fatalf("container memory reset: %d %s (calls=%d)", w.Code, w.Body, resetCalls)
	}
	if err := s.waitForAgentRestart(time.Second); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(s.llmLogDir(), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.llmLogDir(), "agent.log"), []byte("container agent log"), 0600); err != nil {
		t.Fatal(err)
	}
	w = localRequest(s, "GET", "/api/logs/support", "")
	if w.Code != 200 {
		t.Fatalf("container support archive: %d", w.Code)
	}
	gz, err := gzip.NewReader(w.Body)
	if err != nil {
		t.Fatal(err)
	}
	defer gz.Close()
	archive := tar.NewReader(gz)
	foundAgent := false
	for {
		header, err := archive.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if header.Name == "agent.log" {
			data, _ := io.ReadAll(archive)
			foundAgent = string(data) == "container agent log"
		}
		if strings.HasPrefix(header.Name, "services/") || strings.HasPrefix(header.Name, "diagnostics/") || header.Name == "dmesg.log" {
			t.Fatalf("container archive includes board diagnostics: %s", header.Name)
		}
	}
	if !foundAgent {
		t.Fatal("container archive omits the Agent log")
	}
	for _, path := range []string{"/wifi", "/firmware", "/storage", "/api/system/environment", "/api/services"} {
		if w := localRequest(s, "GET", path, ""); w.Code != 404 {
			t.Fatalf("container exposes %s: %d", path, w.Code)
		}
	}
	request := httptest.NewRequest("GET", "http://attacker.example/api/config", nil)
	w = httptest.NewRecorder()
	s.ServeHTTP(w, request)
	if w.Code != 403 {
		t.Fatal("container settings accepted a non-loopback browser host")
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
