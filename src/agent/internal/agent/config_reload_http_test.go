package agent

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestInternalConfigReloadAppliesLoopbackRevision(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.toml"), []byte("[basic_settings.language_timezone]\nlocale=\"en-US\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{config: Config{ConfigDir: dir}}
	server := &Server{runtime: runtime}
	req := httptest.NewRequest(http.MethodPost, "/api/internal/config/reload", strings.NewReader(fmt.Sprintf(`{"revision":%d}`, configFileRevision(filepath.Join(dir, "agent.toml")))))
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	server.handleInternalConfigReload(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var payload map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload["pending"] != true {
		t.Fatalf("payload=%s", rec.Body.String())
	}
	waitForConfigApplied(t, runtime)
	if runtime.ConfigSnapshot().LocaleOrDefault() != "en-US" {
		t.Fatalf("runtime locale=%q", runtime.ConfigSnapshot().LocaleOrDefault())
	}
}

func TestInternalConfigReloadKeepsUnknownGeminiTemperatureUnset(t *testing.T) {
	dir := t.TempDir()
	configPath := filepath.Join(dir, "agent.toml")
	contents := `[model_settings.model]
provider = "gemini"
model = "gemini-2.5-flash"
`
	if err := os.WriteFile(configPath, []byte(contents), 0o640); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{config: Config{ConfigDir: dir}}
	defer runtime.Close()
	server := &Server{runtime: runtime}
	req := httptest.NewRequest(http.MethodPost, "/api/internal/config/reload", strings.NewReader(fmt.Sprintf(`{"revision":%d}`, configFileRevision(configPath))))
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	server.handleInternalConfigReload(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	waitForConfigApplied(t, runtime)
	if temperature := runtime.ConfigSnapshot().Model.Temperature; temperature != nil {
		t.Fatalf("reloaded model.temperature = %v, want nil", formatFloatPtr(temperature))
	}
}

func TestRuntimeConfigSnapshotIsSafeDuringReload(t *testing.T) {
	runtime := &Runtime{config: Config{ConfigDir: t.TempDir()}}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				cfg := runtime.ConfigSnapshot()
				cfg.Locale = []string{"en-US", "zh-CN"}[index%2]
				_ = runtime.ApplyConfigSnapshot(cfg)
			}
		}(i)
	}
	wg.Wait()
}

func TestInternalConfigReloadRejectsRemoteRequest(t *testing.T) {
	server := &Server{runtime: &Runtime{config: Config{ConfigDir: t.TempDir()}}}
	req := httptest.NewRequest(http.MethodPost, "/api/internal/config/reload", strings.NewReader(`{}`))
	req.RemoteAddr = "192.0.2.10:1234"
	req.Header.Set("X-Aico-Internal", "config-web")
	rec := httptest.NewRecorder()
	server.handleInternalConfigReload(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestInternalConfigReloadRejectsTruncatedJSON(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.toml"), []byte("[basic_settings.language_timezone]\nlocale=\"en-US\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	server := &Server{runtime: &Runtime{config: Config{ConfigDir: dir}}}
	req := httptest.NewRequest(http.MethodPost, "/api/internal/config/reload", strings.NewReader(`{"revision":1`))
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	server.handleInternalConfigReload(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestAgentHandlerDoesNotExposeConfigWebCapabilities(t *testing.T) {
	server := &Server{}
	h := server.Handler()
	for _, path := range []string{
		"/api/models?provider=openai",
		"/api/config-test/stt/start",
		"/api/config-test/stt/stop",
		"/api/storage/status",
		"/api/storage/format",
		"/api/storage/eject",
	} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusNotFound {
			t.Errorf("path %s returned status=%d", path, rec.Code)
		}
	}
}

func TestInternalConfigReloadPreservesDeviceCLIOverride(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "agent.toml"), []byte("[basic_settings.device]\ndevice_type=\"Android\"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := DefaultConfig()
	cfg.ConfigDir = dir
	if err := cfg.OverrideDeviceType("macOS"); err != nil {
		t.Fatal(err)
	}
	runtime := &Runtime{config: cfg}
	defer runtime.Close()
	server := &Server{runtime: runtime}
	req := httptest.NewRequest(http.MethodPost, "/api/internal/config/reload", strings.NewReader(fmt.Sprintf(`{"revision":%d}`, configFileRevision(filepath.Join(dir, "agent.toml")))))
	req.RemoteAddr = "127.0.0.1:1234"
	rec := httptest.NewRecorder()
	server.handleInternalConfigReload(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	status := waitForConfigApplied(t, runtime)
	if status.RebootRequired || runtime.ConfigSnapshot().DeviceTypeOrDefault() != "macOS" {
		t.Fatalf("lost CLI override: %+v", status)
	}
}

// TestInternalConfigReloadReportsWhyARequestWasRejected keeps the rejection
// message actionable: "requires a nonzero revision" is wrong when the caller
// did send one and the body failed to decode instead.
func TestInternalConfigReloadReportsWhyARequestWasRejected(t *testing.T) {
	server := &Server{runtime: &Runtime{config: Config{ConfigDir: t.TempDir()}}}
	cases := []struct{ name, body, want string }{
		{"empty body", "", "invalid reload request"},
		{"truncated json", "{\"revision\":1", "invalid reload request"},
		{"unknown field", "{\"revision\":1,\"apply\":true}", "unknown field"},
		{"trailing document", "{\"revision\":1} {}", "single JSON object"},
		{"trailing garbage", "{\"revision\":1} {", "invalid reload request"},
		{"missing revision", "{}", "nonzero revision"},
		{"zero revision", "{\"revision\":0}", "nonzero revision"},
	}
	for _, testCase := range cases {
		req := httptest.NewRequest(http.MethodPost, "/api/internal/config/reload", strings.NewReader(testCase.body))
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		server.handleInternalConfigReload(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: status=%d body=%s", testCase.name, rec.Code, rec.Body.String())
			continue
		}
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Errorf("%s: invalid json %s", testCase.name, rec.Body.String())
			continue
		}
		message, _ := payload["error"].(string)
		if !strings.Contains(message, testCase.want) {
			t.Errorf("%s: error=%q, want substring %q", testCase.name, message, testCase.want)
		}
	}
}

func TestInternalConfigReloadRejectsObsoleteRequests(t *testing.T) {
	server := &Server{runtime: &Runtime{config: Config{ConfigDir: t.TempDir()}}}
	for _, body := range []string{"", `{}`, `{"revision":0}`, `{"revision":1,"apply":true}`, `{"revision":1} {}`} {
		req := httptest.NewRequest(http.MethodPost, "/api/internal/config/reload", strings.NewReader(body))
		req.RemoteAddr = "127.0.0.1:1234"
		rec := httptest.NewRecorder()
		server.handleInternalConfigReload(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("accepted %q: %d %s", body, rec.Code, rec.Body.String())
		}
	}
}
