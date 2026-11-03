package configweb

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHybridUpdateQueuesSystemdAndRejectsDuplicateAcrossRestart(t *testing.T) {
	options := testOptions(t)
	root := t.TempDir()
	options.SystemctlBinary = filepath.Join(root, "systemctl")
	t.Setenv("TEST_HYBRID_MARKER", options.HybridUpdateMarkerPath)
	t.Setenv("TEST_SYSTEMCTL_ARGS", filepath.Join(root, "args"))
	script := "#!/bin/sh\n[ -s \"$TEST_HYBRID_MARKER\" ] || exit 4\nprintf '%s\\n' \"$@\" > \"$TEST_SYSTEMCTL_ARGS\"\n"
	if err := os.WriteFile(options.SystemctlBinary, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/ota/updates", nil))
	if response.Code != http.StatusAccepted {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	args, err := os.ReadFile(filepath.Join(root, "args"))
	if err != nil || string(args) != "start\n--no-block\naico-hybrid-update.service\n" {
		t.Fatalf("systemctl args=%q err=%v", args, err)
	}
	server, err = NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	if !server.otaUpdateRunning() {
		t.Fatal("queued update disappeared after Config Web restart")
	}
	response = httptest.NewRecorder()
	server.APIHandler().ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/ota/updates", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("duplicate status=%d", response.Code)
	}
}

func TestHybridUpdateStartupFailureClearsRequest(t *testing.T) {
	options := testOptions(t)
	options.SystemctlBinary = "/bin/false"
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.handleOTAUpdate(response, httptest.NewRequest(http.MethodPost, "/api/ota/updates", nil))
	if response.Code != http.StatusServiceUnavailable || server.otaUpdateRunning() {
		t.Fatalf("status=%d running=%v body=%s", response.Code, server.otaUpdateRunning(), response.Body.String())
	}
	log, err := os.ReadFile(options.OTAUpdateLogPath)
	if err != nil || !strings.Contains(string(log), "update_exited exit_code=1") {
		t.Fatalf("log=%q err=%v", log, err)
	}
}

func TestHybridUpdateUsesLockBeforePublishingRequest(t *testing.T) {
	options := testOptions(t)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := server.acquireOTALock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	response := httptest.NewRecorder()
	server.handleOTAUpdate(response, httptest.NewRequest(http.MethodPost, "/api/ota/updates", nil))
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(options.HybridUpdateMarkerPath); !os.IsNotExist(err) {
		t.Fatalf("request created while another updater holds lock: %v", err)
	}
}

func TestHybridStatusDistinguishesNewBootFromRebootRequest(t *testing.T) {
	options := testOptions(t)
	options.BootIDPath = filepath.Join(t.TempDir(), "boot-id")
	if err := os.WriteFile(options.HybridUpdateMarkerPath, []byte(`{"phase":"post-ota","boot_id":"old-boot"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	server := &Server{options: options}
	for _, tc := range []struct{ boot, phase string }{{"old-boot", "post-ota"}, {"new-boot", "waiting-health"}} {
		if err := os.WriteFile(options.BootIDPath, []byte(tc.boot+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if got := server.hybridUpdatePhase(); got != tc.phase {
			t.Fatalf("phase=%s, want %s", got, tc.phase)
		}
	}
}
