package configweb

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"aico-agent/internal/agent"
)

func TestConfigApplicationObservesDiskIndependentlyOfRuntime(t *testing.T) {
	o := testOptions(t)
	for _, offline := range []bool{false, true} {
		t.Run(fmt.Sprint(offline), func(t *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if offline {
					http.Error(w, "offline", http.StatusServiceUnavailable)
					return
				}
				writeJSON(w, http.StatusOK, agent.ConfigApplyStatus{Revision: 7, State: "applied", Applied: true})
			}))
			defer upstream.Close()
			o.AgentHTTPBaseURL = upstream.URL
			s := &Server{options: o}
			for _, mode := range []string{"stt", "realtime"} {
				if err := os.WriteFile(o.AgentConfigPath, []byte("[voice_settings.mode]\ninput_mode = \""+mode+"\"\n"), 0600); err != nil {
					t.Fatal(err)
				}
				response := httptest.NewRecorder()
				s.handleConfigApplication(response, httptest.NewRequest(http.MethodGet, "/api/config/application", nil))
				var status struct {
					Revision          uint64 `json:"revision"`
					PersistedRevision uint64 `json:"persisted_revision"`
				}
				if err := json.Unmarshal(response.Body.Bytes(), &status); err != nil {
					t.Fatal(err)
				}
				if status.PersistedRevision != agent.ConfigFileRevision(o.AgentConfigPath) || status.PersistedRevision == 0 {
					t.Fatalf("file revision missing: %s", response.Body.String())
				}
				if !offline && (response.Code != http.StatusOK || status.Revision != 7) {
					t.Fatalf("runtime revision changed: %s", response.Body.String())
				}
				if offline && response.Code != http.StatusServiceUnavailable {
					t.Fatal("Agent failure was hidden")
				}
			}
		})
	}
}

func TestAgentEndpointSharesDiscoveryAndRecoversAfterFailure(t *testing.T) {
	o := testOptions(t)
	o.AgentHTTPBaseURL = ""
	root := t.TempDir()
	calls, address := filepath.Join(root, "calls"), filepath.Join(root, "address")
	t.Setenv("POLL_TEST_CALLS", calls)
	t.Setenv("POLL_TEST_ADDRESS", address)
	o.AgentInitScript = filepath.Join(root, "init")
	if err := os.WriteFile(o.AgentInitScript, []byte("#!/bin/sh\necho call >> \"$POLL_TEST_CALLS\"\nprintf 'agent=running pid=1 addr=%s\\n' \"$(cat \"$POLL_TEST_ADDRESS\")\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]any{}) }))
	defer upstream.Close()
	setAddress := func(value string) {
		t.Helper()
		if err := os.WriteFile(address, []byte(strings.TrimPrefix(value, "http://")), 0600); err != nil {
			t.Fatal(err)
		}
	}
	setAddress(upstream.URL)
	s := &Server{options: o}
	var group sync.WaitGroup
	for i := 0; i < 5; i++ {
		group.Add(1)
		go func() {
			defer group.Done()
			base, err := s.agentBaseURL()
			if err != nil || base.String() != upstream.URL {
				t.Errorf("endpoint=%v err=%v", base, err)
				return
			}
			base.Path = "/per-request-path"
		}()
	}
	group.Wait()
	count := func(want int) {
		t.Helper()
		data, err := os.ReadFile(calls)
		if err != nil || strings.Count(string(data), "call\n") != want {
			t.Fatalf("discoveries=%q err=%v, want %d", data, err, want)
		}
	}
	count(1)
	upstream.Close()
	if _, _, err := s.fetchAgentApplicationStatus(context.Background()); err == nil {
		t.Fatal("closed endpoint succeeded")
	}
	next := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { writeJSON(w, 200, map[string]any{}) }))
	defer next.Close()
	setAddress(next.URL)
	base, err := s.agentBaseURL()
	if err != nil || base.String() != next.URL {
		t.Fatalf("endpoint did not recover: %v %v", base, err)
	}
	count(2)
	s.agentEndpointExpires = time.Now().Add(-time.Second)
	if _, err := s.agentBaseURL(); err != nil {
		t.Fatal(err)
	}
	count(3)
}

func TestConfigSchemaWithoutAgentBinary(t *testing.T) {
	o := testOptions(t)
	o.AgentBinary = filepath.Join(t.TempDir(), "missing-agent")
	s := &Server{options: o}
	response := httptest.NewRecorder()
	s.handleConfigMeta(response, httptest.NewRequest(http.MethodGet, "/api/config/schema", nil))
	var metadata agent.ConfigMetadata
	if err := json.Unmarshal(response.Body.Bytes(), &metadata); err != nil || response.Code != 200 || len(metadata.Sections) == 0 {
		t.Fatalf("metadata unavailable without CLI: %d %v", response.Code, err)
	}
}

func TestOTALightStatusOmitsLogsAndStillObservesUpdates(t *testing.T) {
	o := testOptions(t)
	if err := os.WriteFile(o.OTAUpdateLogPath, []byte("previous update output"), 0600); err != nil {
		t.Fatal(err)
	}
	s := &Server{options: o}
	for _, query := range []string{"", "?logs=0"} {
		response := httptest.NewRecorder()
		s.handleOTALogs(response, httptest.NewRequest(http.MethodGet, "/api/ota/status"+query, nil))
		var value map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
			t.Fatal(err)
		}
		_, hasLog := value["ota_log"]
		if hasLog != (query == "") || value["ota_update_running"] != false {
			t.Fatalf("unexpected idle response: %v", value)
		}
	}
	if err := os.WriteFile(o.HybridUpdateMarkerPath, []byte(`{"phase":"post-ota"}`), 0600); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	s.handleOTALogs(response, httptest.NewRequest(http.MethodGet, "/api/ota/status?logs=0", nil))
	if !strings.Contains(response.Body.String(), `"ota_update_running":true`) || !strings.Contains(response.Body.String(), `"hybrid_update_phase":"post-ota"`) {
		t.Fatal(response.Body.String())
	}
}
