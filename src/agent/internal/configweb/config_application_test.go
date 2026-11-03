package configweb

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aico-agent/internal/agent"
)

func TestConfigSavesSerializeThroughReloadAndKeepStatusAvailable(t *testing.T) {
	o := testOptions(t)
	o.AgentBinary = filepath.Join(t.TempDir(), "fake-agent")
	if err := os.WriteFile(o.AgentBinary, []byte("#!/bin/sh\ncat >/dev/null\nprintf '%s' '{\"ok\":true,\"config\":{},\"changed_paths\":[],\"persisted\":true,\"revision\":7}'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	var posts atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if posts.Add(1) == 1 {
				close(entered)
				<-release
				writeJSON(w, 409, map[string]any{"ok": false, "error": "stale revision"})
				return
			}
			writeJSON(w, 202, map[string]any{"ok": true, "applied": false, "pending": true, "state": "pending"})
			return
		}
		writeJSON(w, 200, map[string]any{"state": "applied", "applied": true, "revision": 7})
	}))
	defer upstream.Close()
	defer once.Do(func() { close(release) })
	o.AgentHTTPBaseURL = upstream.URL
	s, err := NewServer(o)
	if err != nil {
		t.Fatal(err)
	}
	save := func() *httptest.ResponseRecorder {
		resp := httptest.NewRecorder()
		s.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"config":{}}`)))
		return resp
	}
	first, second := make(chan *httptest.ResponseRecorder, 1), make(chan *httptest.ResponseRecorder, 1)
	go func() { first <- save() }()
	<-entered
	go func() { second <- save() }()
	select {
	case <-second:
		t.Fatal("later save passed an unfinished reload")
	case <-time.After(30 * time.Millisecond):
	}
	status := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/config/application", nil))
	if !strings.Contains(status.Body.String(), `"pending":true`) {
		t.Fatalf("status during reload=%s", status.Body.String())
	}
	once.Do(func() { close(release) })
	if resp := <-first; resp.Code != 503 {
		t.Fatalf("first=%d", resp.Code)
	}
	if resp := <-second; resp.Code != 200 {
		t.Fatalf("second=%s", resp.Body.String())
	}
	status = httptest.NewRecorder()
	s.APIHandler().ServeHTTP(status, httptest.NewRequest(http.MethodGet, "/api/config/application", nil))
	if !strings.Contains(status.Body.String(), `"state":"applied"`) {
		t.Fatalf("latest success hidden: %s", status.Body.String())
	}
}

func TestConfigRejectsObsoleteEnvelope(t *testing.T) {
	s, err := NewServer(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{}`, `{"config":{},"apply_wifi":false}`, `{"config":{},"wifi":{}}`} {
		resp := httptest.NewRecorder()
		s.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(body)))
		if resp.Code != 400 {
			t.Errorf("accepted %s: %d", body, resp.Code)
		}
	}
}

func TestEnvironmentSaveRequiresExplicitApplyAndSurvivesPortalRestart(t *testing.T) {
	o := testOptions(t)
	if err := os.WriteFile(o.SystemEnvPath, []byte("OPENAI_API_KEY=one\n"), 0600); err != nil {
		t.Fatal(err)
	}
	appliedRevision, err := agent.SystemEnvironmentRevision(o.SystemEnvPath)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		writeJSON(w, 200, map[string]any{"environment_revision": appliedRevision})
	}))
	defer upstream.Close()
	o.AgentHTTPBaseURL = upstream.URL
	o.WiFiProxyInitScript = filepath.Join(t.TempDir(), "proxy-init")
	if err := os.WriteFile(o.WiFiProxyInitScript, []byte("#!/bin/sh\nexit 0\n"), 0700); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(t.TempDir(), "restarted")
	o.AgentInitScript = filepath.Join(t.TempDir(), "init")
	if err := os.WriteFile(o.AgentInitScript, []byte("#!/bin/sh\ntouch '"+marker+"'\n"), 0700); err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(o)
	if err != nil {
		t.Fatal(err)
	}
	read := func(s *Server) string {
		resp := httptest.NewRecorder()
		s.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/system/environment", nil))
		return resp.Body.String()
	}
	if body := read(s); !strings.Contains(body, `"agent_restart_required":false`) {
		t.Fatal(body)
	}
	resp := httptest.NewRecorder()
	s.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPut, "/api/system/environment", strings.NewReader(`{"system_env":"OPENAI_API_KEY=two\n"}`)))
	if resp.Code != 200 || !strings.Contains(resp.Body.String(), `"agent_restart_required":true`) {
		t.Fatal(resp.Body.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatal("save restarted Agent")
	}
	s, err = NewServer(o)
	if err != nil {
		t.Fatal(err)
	}
	if body := read(s); !strings.Contains(body, `"agent_restart_required":true`) {
		t.Fatal("portal restart lost pending environment: " + body)
	}
	resp = httptest.NewRecorder()
	s.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/system/environment/apply", nil))
	if resp.Code != 202 {
		t.Fatal(resp.Body.String())
	}
	if err := s.waitForAgentRestart(time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatal("explicit apply did not restart")
	}
	mu.Lock()
	appliedRevision, err = agent.SystemEnvironmentRevision(o.SystemEnvPath)
	mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if body := read(s); !strings.Contains(body, `"agent_restart_required":false`) {
		t.Fatal("restart did not clear environment requirement: " + body)
	}
}
