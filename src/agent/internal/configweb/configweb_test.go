package configweb

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aiden-agent/internal/agent"
	"aiden-agent/internal/wifiproxy"
	"aiden-agent/internal/wifiregion"
)

type fakeStorageController struct {
	status           agent.StorageStatus
	reconfigured     agent.StorageConfig
	reconfigureCalls int
	reconfigureError error
}

type fakeStorageSnapshotLease struct {
	snapshot agent.StorageSnapshot
	err      error
	released atomic.Bool
}

func (l *fakeStorageSnapshotLease) Snapshot() agent.StorageSnapshot { return l.snapshot }
func (l *fakeStorageSnapshotLease) Validate(context.Context) error  { return l.err }
func (l *fakeStorageSnapshotLease) Release()                        { l.released.Store(true) }

func (f *fakeStorageController) Status() agent.StorageStatus { return f.status }
func (f *fakeStorageController) Reconfigure(cfg agent.StorageConfig) error {
	f.reconfigured = cfg
	f.reconfigureCalls++
	return f.reconfigureError
}
func (f *fakeStorageController) SafeEject() error {
	f.status.Card.Mounted = false
	return nil
}
func (f *fakeStorageController) StartFormat(fs, confirm string) error {
	if confirm != agent.StorageFormatConfirmToken {
		return os.ErrPermission
	}
	f.status.FormatJob = agent.StorageFormatJob{Status: agent.StorageFormatRunning, FS: fs}
	return nil
}
func (f *fakeStorageController) AcquireSnapshotLease(context.Context) (agent.StorageSnapshotLease, error) {
	if !f.status.Card.Mounted {
		return nil, errors.New("no readable and writable SD card is mounted")
	}
	return &fakeStorageSnapshotLease{snapshot: agent.StorageSnapshot{
		DevicePath: f.status.Card.Device, MountPoint: f.status.MountPoint,
		FilesystemUUID: "test-sd-uuid", MountID: "test-mount-id",
	}}, nil
}
func (f *fakeStorageController) Stop() {}

func TestUint64ValuePreservesJSONNumber(t *testing.T) {
	const value = "18446744073709551615"
	if got := uint64Value(json.Number(value)); got != ^uint64(0) {
		t.Fatalf("revision=%d, want %d", got, ^uint64(0))
	}
}

func testOptions(t *testing.T) Options {
	t.Helper()
	root := t.TempDir()
	webRoot := filepath.Join(root, "web")
	if err := os.MkdirAll(filepath.Join(webRoot, "assets", "js"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "index.html"), []byte("index"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(webRoot, "assets", "js", "app.js"), []byte("console.log(1)"), 0o644); err != nil {
		t.Fatal(err)
	}
	return Options{
		BindAddress:               "127.0.0.1",
		Port:                      8081,
		AgentConfigPath:           filepath.Join(root, "agent.toml"),
		WiFiConfigPath:            filepath.Join(root, "wpa_supplicant.conf"),
		WiFiConfigEnvironmentPath: filepath.Join(root, "wpa_supplicant-config.env"),
		WiFiInterface:             "wlan0",
		WiFiBackend:               "legacy",
		WiFiRegionStatePath:       filepath.Join(root, "wifi-region.json"),
		OTAStatePath:              filepath.Join(root, "ota-state.json"),
		CmdlinePath:               filepath.Join(root, "cmdline"),
		SystemEnvPath:             filepath.Join(root, "system.env"),
		WiFiProxyConfigPath:       filepath.Join(root, "wifi-proxies.json"),
		LocalProxyAddress:         wifiproxy.DefaultListenAddress,
		LocalProxyEnvironmentPath: filepath.Join(root, "proxy-env"),
		StorageStatePath:          filepath.Join(root, "storage.state"),
		WebRoot:                   webRoot,
		BackupUserdataRoot:        filepath.Join(root, "userdata"),
		BackupSDRoot:              filepath.Join(root, "sdcard"),
		MaintenanceLockPath:       filepath.Join(root, "run", "backup.lock"),
		BackupJobStateDir:         filepath.Join(root, "run", "jobs"),
		USBAddress:                "127.0.0.1",
		USBSubnet:                 "127.0.0.0/8",
		HardwareIDPath:            filepath.Join(root, "hardware-id"),
		SystemctlBinary:           "/bin/true",
		HybridUpdateService:       "aiden-hybrid-update.service",
		HybridUpdateMarkerPath:    filepath.Join(root, "hybrid-update.pending"),
		OTAUpdateLockPath:         filepath.Join(root, "ota.lock"),
		OTAUpdateLogPath:          filepath.Join(root, "ota-update.log"),
		AgentBinary:               "/bin/true",
		AgentHTTPBaseURL:          "http://127.0.0.1:1",
		AgentInitScript:           filepath.Join(root, "missing-init"),
		WiFiProxyInitScript:       "/bin/true",
	}
}

func TestOptionsRejectsInvalidLocalProxyPorts(t *testing.T) {
	for _, address := range []string{"127.0.0.1:0", "127.0.0.1:65536", "localhost:not-a-port"} {
		options := testOptions(t)
		options.LocalProxyAddress = address
		if err := options.Validate(); err == nil {
			t.Fatalf("LocalProxyAddress %q was accepted", address)
		}
	}
}

func TestOptionsRejectsUnknownWiFiBackend(t *testing.T) {
	options := testOptions(t)
	options.WiFiBackend = "unknown"
	if err := options.Validate(); err == nil {
		t.Fatal("unknown Wi-Fi backend was accepted")
	}
}

func TestOptionsAcceptsValidLocalProxyAddress(t *testing.T) {
	options := testOptions(t)
	options.LocalProxyAddress = "localhost:18080"
	if err := options.Validate(); err != nil {
		t.Fatalf("valid LocalProxyAddress rejected: %v", err)
	}
}

func TestWiFiWrongKeyEventOnlyMatchesTheAttemptedSSID(t *testing.T) {
	wrongKey := `<3>CTRL-EVENT-SSID-TEMP-DISABLED id=0 ssid="Office" auth_failures=1 duration=10 reason=WRONG_KEY`
	if !wifiWrongKeyEvent(wrongKey, "Office") {
		t.Fatal("WRONG_KEY event was not classified as a password failure")
	}
	if wifiWrongKeyEvent(wrongKey, "Guest") {
		t.Fatal("WRONG_KEY event for another SSID was classified as this attempt")
	}
	if wifiWrongKeyEvent(`<3>CTRL-EVENT-SSID-TEMP-DISABLED id=0 ssid="Office" reason=AUTH_FAILED`, "Office") {
		t.Fatal("a non-WRONG_KEY event was classified as a password failure")
	}
	if !wifiWrongKeyEvent(`<3>CTRL-EVENT-SSID-TEMP-DISABLED id=0 ssid="Alice's WiFi" reason=WRONG_KEY`, "Alice's WiFi") {
		t.Fatal("apostrophe in SSID was not matched")
	}
}

func TestWiFiConnectionFailureReasons(t *testing.T) {
	status := map[string]any{"connected": false, "ssid": "", "ip_address": ""}
	tests := []struct {
		name          string
		apply         commandResult
		wrongPassword bool
		contextError  error
		want          string
	}{
		{name: "wrong password", apply: commandResult{ExitCode: 1}, wrongPassword: true, want: wifiFailureWrongPassword},
		{name: "association", apply: commandResult{ExitCode: 1, FailureReason: wifiFailureAssociation}, want: wifiFailureAssociation},
		{name: "timeout", apply: commandResult{ExitCode: -1, TimedOut: true}, want: wifiFailureTimeout},
		{name: "unconfirmed", apply: commandResult{ExitCode: 0}, want: wifiFailureUnconfirmed},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := wifiConnectionFailureReason(test.apply, status, "Office", test.wrongPassword, test.contextError); got != test.want {
				t.Fatalf("reason=%q, want %q", got, test.want)
			}
		})
	}
}

func TestServerServesStaticAssetsAndRejectsTraversal(t *testing.T) {
	options := testOptions(t)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Body.String() != "index" {
		t.Fatalf("root response: status=%d body=%q", resp.Code, resp.Body.String())
	}
	if got := resp.Header().Get("Content-Type"); got != "text/html; charset=utf-8" {
		t.Fatalf("root content type=%q", got)
	}

	req = httptest.NewRequest(http.MethodGet, "/assets/js/app.js", nil)
	resp = httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusOK || resp.Body.String() != "console.log(1)" {
		t.Fatalf("asset response: status=%d body=%q", resp.Code, resp.Body.String())
	}

	for _, path := range []string{"/assets/../index.html", "/assets/%2e%2e/index.html", "/assets/js/%2fetc%2fpasswd"} {
		req = httptest.NewRequest(http.MethodGet, path, nil)
		resp = httptest.NewRecorder()
		server.ServeHTTP(resp, req)
		if resp.Code != http.StatusNotFound {
			t.Errorf("path %q returned status %d", path, resp.Code)
		}
	}

	link := filepath.Join(options.WebRoot, "assets", "js", "link.js")
	if err := os.Symlink("app.js", link); err != nil {
		t.Fatal(err)
	}
	req = httptest.NewRequest(http.MethodGet, "/assets/js/link.js", nil)
	resp = httptest.NewRecorder()
	server.ServeHTTP(resp, req)
	if resp.Code != http.StatusNotFound {
		t.Fatalf("symlink asset returned status %d", resp.Code)
	}
}

func TestServerServesSettingsRoutesOnlyForKnownSections(t *testing.T) {
	server, err := NewServer(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]int{
		"/wifi":                 http.StatusOK,
		"/wifi/Home_2.4G":       http.StatusOK,
		"/model/providers/edit": http.StatusOK,
		"/firmware/log":         http.StatusOK,
		"/wifii":                http.StatusNotFound,
		"/favicon":              http.StatusNotFound,
		"/assets/css/tokens":    http.StatusNotFound,
		"/legacy":               http.StatusNotFound,
		"/api":                  http.StatusNotFound,
	} {
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
		if resp.Code != want {
			t.Errorf("GET %s: status=%d, want %d", path, resp.Code, want)
		}
		if want == http.StatusOK && resp.Body.String() != "index" {
			t.Errorf("GET %s: body=%q, want the settings shell", path, resp.Body.String())
		}
	}
	// POST /api reaches the API handler's 404, not the static 405.
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api", nil))
	if resp.Code != http.StatusNotFound {
		t.Errorf("POST /api: status=%d, want the API handler's 404", resp.Code)
	}
}

func TestRestartAndPlanRejectCrossSiteRequests(t *testing.T) {
	server, err := NewServer(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/agent/restart", "/api/device/reboot", "/api/config/plan"} {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{"config":{}}`))
		req.Header.Set("Origin", "https://evil.example")
		resp := httptest.NewRecorder()
		server.APIHandler().ServeHTTP(resp, req)
		if resp.Code != http.StatusForbidden {
			t.Errorf("cross-site POST %s: status=%d, want 403", path, resp.Code)
		}
	}
}

func TestServerStartsWithInvalidAgentConfigAndReportsFieldError(t *testing.T) {
	options := testOptions(t)
	config := `[voice_settings.mode]
input_mode = "stt"

[model_settings.model]
provider = "fake"
`
	if err := os.WriteFile(options.AgentConfigPath, []byte(config), 0o640); err != nil {
		t.Fatal(err)
	}
	fakeAgent := filepath.Join(t.TempDir(), "fake-agent")
	script := `#!/bin/sh
printf '%s\n' '{"agent":{"input_mode":"stt"},"model":{"provider":"fake"},"stt":{"provider":""}}'
`
	if err := os.WriteFile(fakeAgent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	options.AgentBinary = fakeAgent

	server, err := NewServer(options)
	if err != nil {
		t.Fatalf("NewServer() rejected semantically invalid config: %v", err)
	}
	defer server.currentStorage().Stop()

	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/device/snapshot", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var payload struct {
		ConfigValid  bool                          `json:"config_valid"`
		ConfigErrors []agent.ConfigValidationError `json:"config_errors"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ConfigValid || len(payload.ConfigErrors) != 1 {
		t.Fatalf("config validity payload=%+v", payload)
	}
	if payload.ConfigErrors[0].Field != "stt.provider" ||
		!strings.Contains(payload.ConfigErrors[0].Message, "stt.provider is required") {
		t.Fatalf("config error=%+v", payload.ConfigErrors[0])
	}
}

// Recovery mode exists so the page can repair the file, which means the state it
// reports has to follow that file: every request reads it again, a repaired file
// comes back valid without restarting the portal, and a later edit is reported
// with its own field. The save that writes the file goes out through the `agent
// config-update` subprocess, so this test leaves the file the way a completed
// save leaves it instead of faking that hop.
func TestRecoveryStateFollowsTheConfigFile(t *testing.T) {
	options := testOptions(t)
	fakeAgent := filepath.Join(t.TempDir(), "fake-agent")
	script := `#!/bin/sh
printf '%s\n' '{"agent":{"input_mode":"stt"},"model":{"provider":"fake"}}'
`
	if err := os.WriteFile(fakeAgent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	options.AgentBinary = fakeAgent

	// Config.Validate reports one error at a time, so the fixture keeps every
	// other requirement of stt mode satisfied: that isolates the field the page
	// highlights and keeps repairing it a single step.
	const header = `[model_settings.model]
provider = "fake"

[voice_settings.classic.tts]
provider = "minimax-cn"
`
	missingSTTProvider := header + `
[voice_settings.mode]
input_mode = "stt"
`
	if err := os.WriteFile(options.AgentConfigPath, []byte(missingSTTProvider), 0o640); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatalf("NewServer() rejected a recoverable config: %v", err)
	}
	defer server.currentStorage().Stop()

	state := func() (bool, agent.ConfigValidationError) {
		t.Helper()
		resp := httptest.NewRecorder()
		server.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/device/snapshot", nil))
		if resp.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
		}
		var payload struct {
			ConfigValid  bool                          `json:"config_valid"`
			ConfigErrors []agent.ConfigValidationError `json:"config_errors"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload.ConfigValid {
			return true, agent.ConfigValidationError{}
		}
		if len(payload.ConfigErrors) != 1 {
			t.Fatalf("invalid config reported errors=%+v", payload.ConfigErrors)
		}
		return false, payload.ConfigErrors[0]
	}

	if valid, failure := state(); valid || failure.Field != "stt.provider" {
		t.Fatalf("valid=%v error=%+v, want the missing stt provider to be flagged", valid, failure)
	}

	// What a successful save of the highlighted field leaves on disk.
	repaired := missingSTTProvider + `
[voice_settings.classic.stt]
provider = "openai-whisper"
`
	if err := os.WriteFile(options.AgentConfigPath, []byte(repaired), 0o640); err != nil {
		t.Fatal(err)
	}
	if valid, failure := state(); !valid {
		t.Fatalf("repaired config still reported invalid: %+v", failure)
	}

	// A later hand edit has to reach the page on the next request as well.
	if err := os.WriteFile(options.AgentConfigPath, []byte(header+`
[basic_settings.language_timezone]
locale = "fr-FR"
`), 0o640); err != nil {
		t.Fatal(err)
	}
	if valid, failure := state(); valid || failure.Field != "locale" {
		t.Fatalf("valid=%v error=%+v, want the new locale error after the file changed", valid, failure)
	}

	// A missing file is a state the page can repair: it renders the built-in
	// defaults and saving creates the file. The runtime treats an absent agent.toml
	// as "all defaults" rather than as a file that declared nothing, so this is a
	// valid configuration, not a recovery state.
	if err := os.Remove(options.AgentConfigPath); err != nil {
		t.Fatal(err)
	}
	if valid, failure := state(); !valid {
		t.Fatalf("valid=%v error=%+v, want an absent config reported as the built-in defaults", valid, failure)
	}
}

func TestServerRejectsDamagedAgentConfig(t *testing.T) {
	options := testOptions(t)
	if err := os.WriteFile(options.AgentConfigPath, []byte("[broken"), 0o640); err != nil {
		t.Fatal(err)
	}
	if _, err := NewServer(options); err == nil || !strings.Contains(err.Error(), "decode TOML config") {
		t.Fatalf("NewServer() error=%v, want damaged config failure", err)
	}
}

func TestConfigValidationStateMatchesAgentRuntimeFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.toml")
	config := `[voice_settings.mode]
input_mode = "realtime"

[model_settings.model]
provider = "fake"
`
	if err := os.WriteFile(path, []byte(config), 0o640); err != nil {
		t.Fatal(err)
	}
	valid, validationErrors := configValidationState(path)
	if !valid || len(validationErrors) != 0 {
		t.Fatalf("valid=%v errors=%+v, want Agent runtime fallback to text mode", valid, validationErrors)
	}
}

func TestDeviceStatusKeepsDeviceTypeWhileConfigIsInRecovery(t *testing.T) {
	options := testOptions(t)
	config := `[basic_settings.device]
device_type = "iOS"

[model_settings.model]
provider = ""
`
	if err := os.WriteFile(options.AgentConfigPath, []byte(config), 0o640); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatalf("NewServer() rejected a recoverable config: %v", err)
	}
	defer server.currentStorage().Stop()

	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/device/status", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var payload struct {
		DeviceType string `json:"device_type"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.DeviceType != "iOS" {
		t.Fatalf("device_type=%q, want the persisted value while the config is in recovery", payload.DeviceType)
	}
}

// Config Web owns the SD-card hardware, and Config.Validate() carries storage
// rules. Recovery mode starts the portal from a config the runtime rejected, so
// the manager must fall back to the built-in defaults instead of mounting,
// formatting, or cleaning up according to settings that failed validation.
func TestStorageManagerIgnoresRejectedStorageSettings(t *testing.T) {
	options := testOptions(t)
	// The manager publishes its state mirror from a background goroutine, so keep
	// that file out of the test's TempDir: cleanup would otherwise race the write.
	options.StorageStatePath = filepath.Join(os.TempDir(), "aiden-storage-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".state")
	defer os.Remove(options.StorageStatePath)
	config := `[model_settings.model]
provider = "fake"

[voice_settings.mode]
input_mode = "stt"

[storage_settings.storage]
monitor_enabled = true
mount_point = "/mnt/bogus"
warning_threshold_mb = 1
critical_threshold_mb = 50
emergency_threshold_mb = 5
`
	if err := os.WriteFile(options.AgentConfigPath, []byte(config), 0o640); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatalf("NewServer() rejected a recoverable config: %v", err)
	}
	storage := server.currentStorage()
	defer storage.Stop()

	wantMountPoint := agent.DefaultConfig().Storage.MountPointOrDefault()
	if got := storage.Status().MountPoint; got != wantMountPoint {
		t.Fatalf("storage mount point=%q, want the default %q while the persisted storage settings are rejected", got, wantMountPoint)
	}
}

func TestStorageStatusUsesAgentResponseContract(t *testing.T) {
	options := testOptions(t)
	if err := os.WriteFile(options.StorageStatePath, []byte("SD_PRESENT=1\nSD_MOUNTED=1\nSD_DEVICE=/dev/mmcblk2p1\nSD_MOUNTPOINT=/mnt/sdcard\nEFFECTIVE_MODE=2\nSD_TOTAL_BYTES=100\nSD_FREE_BYTES=40\nFORMAT_STATUS=running\nFORMAT_FS=ext4\nFORMAT_AUTO=1\nMIGRATE_STATUS=failed\nMIGRATE_ERROR=copy failed\nMIGRATE_MOVED_FILES=2\nMIGRATE_MOVED_BYTES=80\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server := &Server{options: options}
	payload := server.storageStatusValue()
	internal, ok := payload["internal"].(map[string]any)
	totalBytes, totalOK := internal["total_bytes"].(int64)
	if !ok || !totalOK || internal["available"] != true || totalBytes <= 0 {
		t.Fatalf("internal storage=%#v", payload["internal"])
	}
	if _, legacy := payload["sd_present"]; legacy {
		t.Fatalf("storage response still exposes legacy top-level fields: %#v", payload)
	}
	card, ok := payload["card"].(map[string]any)
	if !ok || card["present"] != true || card["mounted"] != true || card["total_bytes"] != int64(100) || card["free_bytes"] != int64(40) {
		t.Fatalf("card=%#v", payload["card"])
	}
	job, ok := payload["format_job"].(map[string]any)
	if !ok || job["status"] != "running" || job["fs"] != "ext4" || job["auto"] != true {
		t.Fatalf("format_job=%#v", payload["format_job"])
	}
	migration, ok := payload["migration"].(map[string]any)
	if !ok || migration["status"] != "failed" || migration["moved_files"] != 2 {
		t.Fatalf("migration=%#v", payload["migration"])
	}
}

func TestConfigBackupExportReturnsPersistedTOML(t *testing.T) {
	options := testOptions(t)
	config := []byte("[basic_settings.language_timezone]\nlocale = \"en-US\"\n")
	if err := os.WriteFile(options.AgentConfigPath, config, 0o640); err != nil {
		t.Fatal(err)
	}
	server := &Server{options: options}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/config/backup", nil))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Header().Get("Content-Disposition"), ".toml\"") || !strings.Contains(resp.Header().Get("Content-Type"), "application/toml") {
		t.Fatalf("status=%d headers=%v body=%s", resp.Code, resp.Header(), resp.Body.String())
	}
	if !bytes.Equal(resp.Body.Bytes(), config) {
		t.Fatalf("backup=%q, want %q", resp.Body.Bytes(), config)
	}
}

func TestConfigBackupImportValidatesPersistsAndReloads(t *testing.T) {
	options := testOptions(t)
	initial := []byte("[basic_settings.language_timezone]\nlocale = \"zh-CN\"\n")
	if err := os.WriteFile(options.AgentConfigPath, initial, 0o640); err != nil {
		t.Fatal(err)
	}
	var reloadRevision uint64
	reload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/api/internal/config/reload" {
			t.Fatalf("reload request=%s %s", r.Method, r.URL.Path)
		}
		var request struct {
			Revision uint64 `json:"revision"`
		}
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatal(err)
		}
		reloadRevision = request.Revision
		writeJSON(w, http.StatusAccepted, map[string]any{
			"ok": true, "applied": false, "pending": true,
			"revision": request.Revision, "state": "pending", "reboot_required": false,
		})
	}))
	defer reload.Close()
	options.AgentHTTPBaseURL = reload.URL
	server := &Server{options: options}
	restored := []byte("[basic_settings.language_timezone]\nlocale = \"en-US\"\n")
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPut, "/api/config/backup", bytes.NewReader(restored)))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"persisted":true`) || !strings.Contains(resp.Body.String(), `"pending":true`) {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if reloadRevision != configDataRevision(restored) {
		t.Fatalf("reload revision=%d, want %d", reloadRevision, configDataRevision(restored))
	}
	written, err := os.ReadFile(options.AgentConfigPath)
	if err != nil || !bytes.Equal(written, restored) {
		t.Fatalf("restored config=%q err=%v", written, err)
	}
}

func TestConfigBackupImportRejectsInvalidTOMLWithoutReplacingConfig(t *testing.T) {
	options := testOptions(t)
	initial := []byte("[basic_settings.language_timezone]\nlocale = \"zh-CN\"\n")
	if err := os.WriteFile(options.AgentConfigPath, initial, 0o640); err != nil {
		t.Fatal(err)
	}
	server := &Server{options: options}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPut, "/api/config/backup", strings.NewReader("[broken")))
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	written, err := os.ReadFile(options.AgentConfigPath)
	if err != nil || !bytes.Equal(written, initial) {
		t.Fatalf("config changed to %q err=%v", written, err)
	}
}

func TestConfigBackupImportRejectsFlatAndUnknownGroupedTables(t *testing.T) {
	for _, backup := range []string{
		"[model]\nprovider = \"openai\"\n",
		"[storage_settings.storge]\nmount_point = \"/mnt/sdcard\"\n",
		"# comments only\n",
	} {
		options := testOptions(t)
		initial := []byte("[basic_settings.language_timezone]\nlocale = \"zh-CN\"\n")
		if err := os.WriteFile(options.AgentConfigPath, initial, 0o640); err != nil {
			t.Fatal(err)
		}
		server := &Server{options: options}
		resp := httptest.NewRecorder()
		server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPut, "/api/config/backup", strings.NewReader(backup)))
		if resp.Code != http.StatusBadRequest {
			t.Errorf("backup=%q status=%d body=%s", backup, resp.Code, resp.Body.String())
		}
		written, err := os.ReadFile(options.AgentConfigPath)
		if err != nil || !bytes.Equal(written, initial) {
			t.Errorf("backup=%q changed config to %q err=%v", backup, written, err)
		}
	}
}

func TestConfigTestAcceptsRealtimeInputMode(t *testing.T) {
	options := testOptions(t)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	request := `{"section":"agent","values":{"input_mode":"realtime","vad_speech_threshold":0.5,"screen_stable_diff_threshold":6,"silence_ms":550,"min_speech_ms":300,"voice_followup_timeout_ms":1000,"voice_first_turn_timeout_ms":1000,"voice_max_turns":2,"voice_max_response_tokens":100,"screenshot_keep_n":3,"screenshot_prune_interval":2,"screen_stable_timeout_ms":2000,"screen_stable_ms":250,"max_iterations":-1}}`
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/config/test", strings.NewReader(request)))
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var payload struct {
		OK      bool `json:"ok"`
		Results []struct {
			Check  string `json:"check"`
			Passed bool   `json:"passed"`
			Detail string `json:"detail"`
		} `json:"results"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	for _, result := range payload.Results {
		if result.Check == "input_mode" {
			if !result.Passed || result.Detail != "got 'realtime', allowed: stt/realtime" {
				t.Fatalf("input_mode result=%+v", result)
			}
			return
		}
	}
	t.Fatalf("input_mode result missing: %+v", payload.Results)
}

// TestConfigTestInputModeAcceptsUnsetAndRejectsRemovedModes covers the agent
// section smoke test. The handler sees only this section's values and has no
// provider context, so an unset mode has to pass here: voice providers are
// opt-in and Config.Validate() is what resolves the effective mode over the
// full candidate configuration.
func TestConfigTestInputModeAcceptsUnsetAndRejectsRemovedModes(t *testing.T) {
	for _, test := range []struct {
		name       string
		value      string
		wantPassed bool
		wantDetail string
	}{
		{"omitted", "", true, "unset (voice not configured; Web UI and HTTP API only)"},
		{"explicit empty", `"input_mode":"",`, true, "unset (voice not configured; Web UI and HTTP API only)"},
		{"stt", `"input_mode":"stt",`, true, "got 'stt', allowed: stt/realtime"},
		{"removed text", `"input_mode":"text",`, false, "invalid input_mode: text (text and audio modes have been removed; use stt or realtime)"},
		{"removed audio", `"input_mode":"audio",`, false, "invalid input_mode: audio (text and audio modes have been removed; use stt or realtime)"},
		{"unknown", `"input_mode":"bogus",`, false, "got 'bogus', allowed: stt/realtime"},
		{"wrong type", `"input_mode":5,`, false, "must be a string"},
	} {
		t.Run(test.name, func(t *testing.T) {
			server, err := NewServer(testOptions(t))
			if err != nil {
				t.Fatal(err)
			}
			request := `{"section":"agent","values":{` + test.value + `"vad_speech_threshold":0.5,"screen_stable_diff_threshold":6,"silence_ms":550,"min_speech_ms":300,"voice_followup_timeout_ms":1000,"voice_first_turn_timeout_ms":1000,"voice_max_turns":2,"voice_max_response_tokens":100,"screenshot_keep_n":3,"screenshot_prune_interval":2,"screen_stable_timeout_ms":2000,"screen_stable_ms":250,"max_iterations":-1}}`
			resp := httptest.NewRecorder()
			server.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/config/test", strings.NewReader(request)))
			if resp.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
			}
			var payload struct {
				Results []struct {
					Check  string `json:"check"`
					Passed bool   `json:"passed"`
					Detail string `json:"detail"`
				} `json:"results"`
			}
			if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			for _, result := range payload.Results {
				if result.Check != "input_mode" {
					continue
				}
				if result.Passed != test.wantPassed || result.Detail != test.wantDetail {
					t.Fatalf("input_mode result=%+v, want passed=%v detail=%q", result, test.wantPassed, test.wantDetail)
				}
				return
			}
			t.Fatalf("input_mode result missing: %+v", payload.Results)
		})
	}
}

func TestConfigTestRejectsInvalidTelemetryURL(t *testing.T) {
	server, err := NewServer(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	request := `{"section":"telemetry","values":{"enabled":true,"base_url":"--output=/tmp/leak","public_key":"pk","secret_key":"sk","upload_timeout_sec":1,"max_retry":1}}`
	resp := httptest.NewRecorder()
	server.ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/config/test", strings.NewReader(request)))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), "base_url must be an http:// or https:// URL with a host") {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestModelsSTTAndStorageAreOwnedByConfigWeb(t *testing.T) {
	server, err := NewServer(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	server.storage = &fakeStorageController{status: agent.StorageStatus{
		EffectiveMode: agent.StorageModeDual,
		Card:          agent.StorageCardStatus{Present: true, Mounted: true},
		MountPoint:    "/mnt/sdcard",
		FormatJob:     agent.StorageFormatJob{Status: agent.StorageFormatIdle},
		Migration:     agent.StorageMigrationJob{Status: agent.StorageFormatIdle},
	}}
	for _, test := range []struct{ method, path string }{
		{http.MethodGet, "/api/models?provider=openai&locale=zh-CN"},
		{http.MethodPost, "/api/config-test/stt/start"},
		{http.MethodPost, "/api/config-test/stt/stop"},
		{http.MethodGet, "/api/storage/status"},
		{http.MethodPost, "/api/storage/format"},
		{http.MethodPost, "/api/storage/eject"},
		{http.MethodGet, "/api/config/backup"},
		{http.MethodPut, "/api/config/backup"},
	} {
		resp := httptest.NewRecorder()
		body := `{}`
		if test.path == "/api/storage/format" {
			body = `{"fs":"ext4","confirm":"format-sd-card"}`
		}
		server.ServeHTTP(resp, httptest.NewRequest(test.method, test.path, strings.NewReader(body)))
		if resp.Code == http.StatusNotFound {
			t.Errorf("Config Web route %s %s returned 404", test.method, test.path)
		}
	}
}

func TestModelsEndpointReturnsLocalizedCatalog(t *testing.T) {
	server, err := NewServer(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	fetch := func(locale string) []agent.LocalizedModelInfo {
		t.Helper()
		resp := httptest.NewRecorder()
		path := "/api/models?provider=openai&locale=" + locale
		server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
		if resp.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
		}
		var payload struct {
			Models []agent.LocalizedModelInfo `json:"models"`
		}
		if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload.Models
	}
	zh := fetch("zh-CN")
	en := fetch("en-US")
	if len(zh) == 0 || len(en) == 0 || zh[0].ID != en[0].ID || zh[0].Description == en[0].Description {
		t.Fatalf("zh=%+v en=%+v", zh, en)
	}
	if zh[0].Spec == nil || zh[0].Spec.Reasoning == nil {
		t.Fatalf("catalog model is missing capability metadata: %+v", zh[0])
	}
}

func TestModelsEndpointReturnsGeminiCatalog(t *testing.T) {
	server, err := NewServer(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet,
		"/api/models?provider=gemini&locale=en-US", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Models []agent.LocalizedModelInfo `json:"models"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Models) != 3 || payload.Models[0].ID != "gemini-3.8-flash" || !payload.Models[0].Recommended {
		t.Fatalf("Gemini catalog = %+v", payload.Models)
	}
}

func TestModelsEndpointReturnsRequestedModelSpec(t *testing.T) {
	options := testOptions(t)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(options.AgentConfigPath, []byte("[model_settings.model]\nprovider = \"anthropic\"\nmodel = \"claude-sonnet-4-6\"\n"), 0o640); err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet,
		"/api/models?provider=anthropic&model=claude-sonnet-4-6&locale=en-US", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var payload struct {
		Spec *struct {
			Provider  string `json:"provider"`
			Name      string `json:"name"`
			APIShape  string `json:"api_shape"`
			Reasoning *struct {
				Supported bool     `json:"supported"`
				Mode      string   `json:"mode"`
				Efforts   []string `json:"efforts"`
			} `json:"reasoning"`
		} `json:"spec"`
	}
	if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Spec == nil || payload.Spec.Provider != "anthropic" || payload.Spec.Name != "claude-sonnet-4-6" || payload.Spec.APIShape != "messages" {
		t.Fatalf("spec=%+v", payload.Spec)
	}
	if payload.Spec.Reasoning == nil || !payload.Spec.Reasoning.Supported || payload.Spec.Reasoning.Mode != "effort" || len(payload.Spec.Reasoning.Efforts) == 0 {
		t.Fatalf("reasoning=%+v", payload.Spec.Reasoning)
	}
}

func TestWiFiConnectionRunsAsBoundedBackgroundTask(t *testing.T) {
	options := testOptions(t)
	binDir := t.TempDir()
	ifconfig := filepath.Join(binDir, "ifconfig")
	if err := os.WriteFile(ifconfig, []byte("#!/bin/sh\n/bin/sleep 1\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	server.wifiRegionDriver = &fakeWiFiRegionDriver{country: "CN"}
	startedAt := time.Now()
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPut, "/api/network/wifi/connection", strings.NewReader(`{"ssid":"test-network","psk":"secret"}`)))
	if resp.Code != http.StatusAccepted || time.Since(startedAt) > 500*time.Millisecond {
		t.Fatalf("status=%d elapsed=%s body=%s", resp.Code, time.Since(startedAt), resp.Body.String())
	}
	var started map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &started); err != nil {
		t.Fatal(err)
	}
	taskID, _ := started["task_id"].(string)
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodPut, "/api/network/wifi/connection", strings.NewReader(`{"ssid":"another-network","psk":"secret"}`)),
		httptest.NewRequest(http.MethodDelete, "/api/network/wifi/connection?ssid=test-network", nil),
	} {
		conflict := httptest.NewRecorder()
		server.APIHandler().ServeHTTP(conflict, request)
		if conflict.Code != http.StatusConflict {
			t.Fatalf("concurrent Wi-Fi operation: status=%d body=%s", conflict.Code, conflict.Body.String())
		}
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		statusResp := httptest.NewRecorder()
		path := "/api/network/wifi/connection?task_id=" + taskID
		server.APIHandler().ServeHTTP(statusResp, httptest.NewRequest(http.MethodGet, path, nil))
		var status map[string]any
		if err := json.Unmarshal(statusResp.Body.Bytes(), &status); err != nil {
			t.Fatal(err)
		}
		if status["status"] != "running" {
			if status["status"] != "failed" || status["wifi_rollback"] == nil {
				t.Fatalf("status payload=%#v", status)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("Wi-Fi task did not finish: %#v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestSystemdWiFiConnectionStartsWithCandidateConfig(t *testing.T) {
	options := testOptions(t)
	options.WiFiBackend = "systemd-networkd"
	binDir := t.TempDir()
	candidate := options.WiFiConfigPath + ".candidate"
	t.Setenv("AIDEN_TEST_WIFI_SELECTOR", options.WiFiConfigEnvironmentPath)
	t.Setenv("AIDEN_TEST_WIFI_CANDIDATE", candidate)
	commands := map[string]string{
		"ip":         "#!/bin/sh\nexit 0\n",
		"networkctl": "#!/bin/sh\nexit 0\n",
		"wpa_cli":    "#!/bin/sh\nprintf '%s\\n' 'wpa_state=COMPLETED' 'ssid=qtum' 'ip_address=192.0.2.10'\n",
		"systemctl": `#!/bin/sh
set -eu
[ "${1:-}" = restart ]
[ "${2:-}" = wpa_supplicant@wlan0.service ]
/usr/bin/grep -Fqx "AIDEN_WPA_SUPPLICANT_CONFIG=\"$AIDEN_TEST_WIFI_CANDIDATE\"" "$AIDEN_TEST_WIFI_SELECTOR"
`,
	}
	for name, script := range commands {
		if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", binDir)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	server.wifiRegionDriver = &fakeWiFiRegionDriver{country: "CN"}
	psk := "secret"
	result := server.runWiFiConnection(context.Background(), wifiConnectionRequest{SSID: "qtum", PSK: &psk})
	if result["ok"] != true {
		t.Fatalf("result=%#v", result)
	}
	if _, err := os.Stat(options.WiFiConfigEnvironmentPath); !os.IsNotExist(err) {
		t.Fatalf("runtime config selector remains after restart: %v", err)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("verified candidate was not retained: %v", err)
	}
	saved, err := loadWiFiConfig(options.WiFiConfigPath)
	if err != nil || findWiFiNetwork(saved, "qtum") < 0 {
		t.Fatalf("saved Wi-Fi config=%#v err=%v", saved.publicValue(), err)
	}
}

func TestConfigUpdatesAreSerialized(t *testing.T) {
	options := testOptions(t)
	fakeAgent := filepath.Join(t.TempDir(), "fake-agent")
	guardDir := filepath.Join(t.TempDir(), "active")
	overlapPath := filepath.Join(t.TempDir(), "overlap")
	t.Setenv("AIDEN_TEST_CONFIG_GUARD", guardDir)
	t.Setenv("AIDEN_TEST_CONFIG_OVERLAP", overlapPath)
	script := `#!/bin/sh
cat >/dev/null
if mkdir "$AIDEN_TEST_CONFIG_GUARD" 2>/dev/null; then
  /bin/sleep 0.15
  rmdir "$AIDEN_TEST_CONFIG_GUARD"
else
  : > "$AIDEN_TEST_CONFIG_OVERLAP"
fi
printf '%s\n' '{"ok":true,"config":{},"changed_paths":[],"reboot_required":false,"persisted":true}'
`
	if err := os.WriteFile(fakeAgent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	reload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "applied": false, "pending": true})
	}))
	defer reload.Close()
	options.AgentHTTPBaseURL = reload.URL
	options.AgentBinary = fakeAgent
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}

	start := make(chan struct{})
	responses := make(chan *httptest.ResponseRecorder, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			resp := httptest.NewRecorder()
			server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"config":{"agent":{"locale":"en-US"}}}`)))
			responses <- resp
		}()
	}
	close(start)
	wg.Wait()
	close(responses)
	for resp := range responses {
		if resp.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
		}
	}
	if _, err := os.Stat(overlapPath); !os.IsNotExist(err) {
		t.Fatalf("config-update commands overlapped: %v", err)
	}
}

func TestConfigPatchReconfiguresStorageOwner(t *testing.T) {
	options := testOptions(t)
	fakeAgent := filepath.Join(t.TempDir(), "fake-agent")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"ok\":true,\"config\":{},\"changed_paths\":[\"storage_settings.storage.mount_point\"],\"reboot_required\":false,\"persisted\":true,\"revision\":11}'\n"
	if err := os.WriteFile(fakeAgent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	reload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "applied": false, "pending": true, "revision": 11, "state": "pending"})
	}))
	defer reload.Close()
	options.AgentBinary = fakeAgent
	options.AgentHTTPBaseURL = reload.URL
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	storage := &fakeStorageController{}
	server.storage = storage
	config := "[storage_settings.storage]\nmount_point = \"/mnt/new-card\"\ndevice = \"mmcblk9\"\nmin_card_free_mb = 128\n"
	if err := os.WriteFile(options.AgentConfigPath, []byte(config), 0o640); err != nil {
		t.Fatal(err)
	}

	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"config":{"storage":{"mount_point":"/mnt/new-card"}}}`)))
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if storage.reconfigureCalls != 1 || storage.reconfigured.MountPointOrDefault() != "/mnt/new-card" || storage.reconfigured.DeviceOrDefault() != "mmcblk9" || storage.reconfigured.MinCardFreeMBOrDefault() != 128 {
		t.Fatalf("storage reconfigure calls=%d config=%+v", storage.reconfigureCalls, storage.reconfigured)
	}
}

func TestAPIRouteHeaders(t *testing.T) {
	server, err := NewServer(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "canonical", path: "/api/device/status"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, test.path, nil))
			if resp.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
			}
			if got := resp.Header().Get("X-Aiden-API-Version"); got != "1" {
				t.Fatalf("API version header=%q", got)
			}
		})
	}
	for _, test := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/api/agent/status"},
		{http.MethodPost, "/api/config"},
		{http.MethodPost, "/api/wifi/scan"},
		{http.MethodPost, "/api/ota/update"},
		{http.MethodGet, "/api/llm-logs"},
	} {
		resp := httptest.NewRecorder()
		server.APIHandler().ServeHTTP(resp, httptest.NewRequest(test.method, test.path, nil))
		if resp.Code != http.StatusNotFound {
			t.Errorf("retired route %s %s returned status=%d", test.method, test.path, resp.Code)
		}
	}
}

func TestConfigPatchReportsPersistedAndPendingRevision(t *testing.T) {
	options := testOptions(t)
	fakeAgent := filepath.Join(t.TempDir(), "fake-agent")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"ok\":true,\"config\":{\"agent\":{\"locale\":\"en-US\"}},\"changed_paths\":[\"agent.locale\"],\"reboot_required\":false,\"persisted\":true,\"revision\":7}'\n"
	if err := os.WriteFile(fakeAgent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	reload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/internal/config/reload" || r.Method != http.MethodPost {
			t.Fatalf("reload request=%s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "applied": false, "pending": true, "revision": 7, "state": "pending"})
	}))
	defer reload.Close()
	options.AgentBinary = fakeAgent
	options.AgentHTTPBaseURL = reload.URL
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"config":{"agent":{"locale":"en-US"}}}`)))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"persisted":true`) || !strings.Contains(resp.Body.String(), `"pending":true`) {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestConfigPatchDoesNotRestartWhenRuntimeReloadRejectsChange(t *testing.T) {
	options := testOptions(t)
	fakeAgent := filepath.Join(t.TempDir(), "fake-agent")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"ok\":true,\"config\":{\"agent\":{\"locale\":\"zh-CN\"}},\"changed_paths\":[\"agent.locale\"],\"reboot_required\":false,\"persisted\":true,\"revision\":9}'\n"
	if err := os.WriteFile(fakeAgent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	reload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{
			"ok": false, "applied": false,
			"error": "config file unavailable",
		})
	}))
	defer reload.Close()
	options.AgentBinary = fakeAgent
	options.AgentHTTPBaseURL = reload.URL
	options.AgentInitScript = "/bin/true"
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"config":{"agent":{"locale":"zh-CN"}}}`)))
	if resp.Code != http.StatusServiceUnavailable ||
		!strings.Contains(resp.Body.String(), `"persisted":true`) ||
		!strings.Contains(resp.Body.String(), `"applied":false`) ||
		server.agentRestartPending() {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestConfigPatchUsesUpdateHandler(t *testing.T) {
	options := testOptions(t)
	fakeAgent := filepath.Join(t.TempDir(), "fake-agent")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"ok\":true,\"config\":{\"agent\":{\"locale\":\"en-US\"}},\"changed_paths\":[],\"reboot_required\":false,\"persisted\":true}'\n"
	if err := os.WriteFile(fakeAgent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	reload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "applied": false, "pending": true})
	}))
	defer reload.Close()
	options.AgentHTTPBaseURL = reload.URL
	options.AgentBinary = fakeAgent
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}

	body := strings.NewReader(`{"config":{"agent":{"locale":"en-US"}}}`)
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPatch, "/api/config", body))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"locale":"en-US"`) {
		t.Fatalf("config patch: status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestConfigPatchRejectsMissingPersistenceState(t *testing.T) {
	options := testOptions(t)
	fakeAgent := filepath.Join(t.TempDir(), "fake-agent")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"ok\":true,\"config\":{},\"changed_paths\":[],\"reboot_required\":false}'\n"
	if err := os.WriteFile(fakeAgent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	options.AgentBinary = fakeAgent
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"config":{}}`)))
	if resp.Code != http.StatusServiceUnavailable || !strings.Contains(resp.Body.String(), "omitted persisted state") {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestSystemEnvironmentGet(t *testing.T) {
	options := testOptions(t)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}

	read := func() map[string]any {
		t.Helper()
		resp := httptest.NewRecorder()
		server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/system/environment", nil))
		if resp.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
		}
		var payload map[string]any
		if err := json.Unmarshal(resp.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		return payload
	}

	if got := read()["system_env"]; got != "" {
		t.Fatalf("missing env content=%q", got)
	}
	if err := os.WriteFile(options.SystemEnvPath, []byte("HTTP_PROXY=http://proxy.example\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := read()["system_env"]; got != "HTTP_PROXY=http://proxy.example\n" {
		t.Fatalf("env content=%q", got)
	}
}

func TestSystemEnvironmentRejectsVariablesDroppedByDebianGenerator(t *testing.T) {
	options := testOptions(t)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPut, "/api/system/environment", strings.NewReader(`{"system_env":"OPENAI_API_KEY=live\nLD_PRELOAD=/tmp/inject.so\n"}`)))
	if resp.Code != http.StatusBadRequest || !strings.Contains(resp.Body.String(), "environment variable is not approved: LD_PRELOAD") {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if _, err := os.Stat(options.SystemEnvPath); !os.IsNotExist(err) {
		t.Fatalf("rejected environment was persisted: %v", err)
	}
}

func TestSystemEnvironmentAllowsProviderCredentialsAndWiFiProxyBypass(t *testing.T) {
	for _, key := range []string{"DASHSCOPE_API_KEY", "SPEKO_KEY", "VERTEX_TOKEN", "CUSTOM_SECRET", "AIDEN_WIFI_PROXY_ENABLED"} {
		if !isAllowedSystemEnvKey(key) {
			t.Fatalf("expected %s to be allowed", key)
		}
	}
}

func TestSystemEnvironmentWriteFailureReturnsServerError(t *testing.T) {
	options := testOptions(t)
	blockedParent := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedParent, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	options.SystemEnvPath = filepath.Join(blockedParent, "system.env")
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPut, "/api/system/environment", strings.NewReader(`{"system_env":"OPENAI_API_KEY=1\n"}`)))
	if resp.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestSystemEnvironmentReportsRestartLaunchFailure(t *testing.T) {
	options := testOptions(t)
	options.AgentInitScript = ""
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/system/environment/apply", nil))
	if resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestLLMLogRoutesPreserveEncodedName(t *testing.T) {
	options := testOptions(t)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(server.llmLogDir(), 0o755); err != nil {
		t.Fatal(err)
	}
	name := "llm-http-a+b.log"
	if err := os.WriteFile(filepath.Join(server.llmLogDir(), name), []byte("entry"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name string
		path string
	}{
		{name: "canonical", path: "/api/logs/llm/llm-http-a%2Bb.log"},
	} {
		t.Run(test.name, func(t *testing.T) {
			resp := httptest.NewRecorder()
			server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, test.path, nil))
			if resp.Code != http.StatusOK || resp.Body.String() != "entry" {
				t.Fatalf("status=%d body=%q", resp.Code, resp.Body.String())
			}
		})
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/llm-logs/export/llm-http-a%2Bb.log", nil))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("legacy LLM log route returned status=%d", resp.Code)
	}

	resp = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/logs/llm/llm-http-upload%2B1.log", strings.NewReader("uploaded"))
	server.APIHandler().ServeHTTP(resp, request)
	if resp.Code != http.StatusOK {
		t.Fatalf("import status=%d body=%s", resp.Code, resp.Body.String())
	}
	data, err := os.ReadFile(filepath.Join(server.llmLogDir(), "llm-http-upload+1.log"))
	if err != nil || string(data) != "uploaded" {
		t.Fatalf("imported data=%q err=%v", data, err)
	}
}

func TestLLMLogImportRejectsOversizedBody(t *testing.T) {
	options := testOptions(t)
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPut, "/api/logs/llm/llm-http-oversized.log", strings.NewReader(strings.Repeat("x", maxRequestBodySize+1)))
	server.APIHandler().ServeHTTP(resp, request)
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if _, err := os.Stat(filepath.Join(server.llmLogDir(), "llm-http-oversized.log")); !os.IsNotExist(err) {
		t.Fatalf("oversized target exists or stat failed unexpectedly: %v", err)
	}
}

func TestSupportLogsExportIncludesServiceLogsAndDmesg(t *testing.T) {
	options := testOptions(t)
	root := filepath.Dir(options.AgentConfigPath)
	dmesg := filepath.Join(root, "dmesg")
	if err := os.WriteFile(dmesg, []byte("#!/bin/sh\nprintf 'kernel message\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	options.DmesgBinary = dmesg
	options.OTAHealthLogPath = filepath.Join(root, "ota.log")
	if err := os.MkdirAll(filepath.Dir(options.OTAHealthLogPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(options.OTAHealthLogPath, []byte("ota health\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}

	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/logs/support", nil))
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	gz, err := gzip.NewReader(bytes.NewReader(resp.Body.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	files := map[string]string{}
	for {
		header, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(tr)
		if err != nil {
			t.Fatal(err)
		}
		files[header.Name] = string(data)
	}
	if files["dmesg.log"] != "kernel message\n" {
		t.Fatalf("dmesg.log=%q", files["dmesg.log"])
	}
	if files["services/frame_service.log"] == "" {
		t.Fatal("frame service log was not included")
	}
	if files["services/ota.log"] != "ota health\n" {
		t.Fatalf("services/ota.log=%q", files["services/ota.log"])
	}
}

func TestRunDmesgCommandTimeoutKillsWrapperAndChild(t *testing.T) {
	wrapper := filepath.Join(t.TempDir(), "dmesg-wrapper")
	if err := os.WriteFile(wrapper, []byte("#!/bin/sh\nsleep 30 &\nwait\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result := runDmesgCommand(50*time.Millisecond, wrapper)
	if !result.TimedOut {
		t.Fatalf("TimedOut=%t ExitCode=%d Output=%q, want timeout", result.TimedOut, result.ExitCode, result.Output)
	}
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("dmesg wrapper shutdown took %s after timeout", elapsed)
	}
}

func TestAPIRouteCatalogHasNoDuplicates(t *testing.T) {
	seen := map[routeVariant]apiEndpoint{}
	for _, route := range apiRoutes {
		if previous, exists := seen[route.canonical]; exists {
			t.Fatalf("duplicate route %s %s for endpoints %d and %d", route.canonical.method, route.canonical.path, previous, route.endpoint)
		}
		seen[route.canonical] = route.endpoint
	}
}

func TestMemoryResetProxiesAgentClearAllAndSchedulesRestart(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Method != http.MethodPost || r.URL.Path != "/api/clear-all" {
			t.Fatalf("upstream request = %s %s", r.Method, r.URL.Path)
		}
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	}))
	defer upstream.Close()

	options := testOptions(t)
	options.AgentHTTPBaseURL = upstream.URL
	options.AgentInitScript = "/bin/true"
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/memory/reset", nil))
	if resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"agent_restart_scheduled":true`) {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	if calls.Load() != 1 {
		t.Fatalf("clear-all calls = %d, want 1", calls.Load())
	}
}

func TestMemoryResetRejectsCrossOriginBrowserRequests(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	}))
	defer upstream.Close()

	options := testOptions(t)
	options.AgentHTTPBaseURL = upstream.URL
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	for _, headers := range []map[string]string{
		{"Origin": "https://attacker.example"},
		{"Sec-Fetch-Site": "cross-site"},
	} {
		req := httptest.NewRequest(http.MethodPost, "http://device.local/api/memory/reset", nil)
		for name, value := range headers {
			req.Header.Set(name, value)
		}
		resp := httptest.NewRecorder()
		server.APIHandler().ServeHTTP(resp, req)
		if resp.Code != http.StatusForbidden {
			t.Fatalf("headers=%v status=%d body=%s", headers, resp.Code, resp.Body.String())
		}
	}
	if calls.Load() != 0 {
		t.Fatalf("clear-all calls = %d, want 0", calls.Load())
	}
}

func TestUnknownAPIRouteReturnsNotFound(t *testing.T) {
	server, err := NewServer(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/unknown", nil))
	if resp.Code != http.StatusNotFound {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
}

func TestVersionedAndRetiredSchemaRoutesReturnNotFound(t *testing.T) {
	server, err := NewServer(testOptions(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/v1/config", "/api/config-meta", "/api/config/meta"} {
		resp := httptest.NewRecorder()
		server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, path, nil))
		if resp.Code != http.StatusNotFound {
			t.Errorf("%s returned status=%d body=%s", path, resp.Code, resp.Body.String())
		}
	}
}

func TestReadFileLimitedRejectsOversizedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "large")
	if err := os.WriteFile(path, []byte("12345"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readFileLimited(path, 4); err == nil {
		t.Fatal("oversized file was accepted")
	}
	data, err := readFileLimited(path, 5)
	if err != nil || string(data) != "12345" {
		t.Fatalf("exact-sized file read: data=%q err=%v", data, err)
	}
}

func TestSystemEnvParserAndWiFiConfigDefaults(t *testing.T) {
	assignments, err := parseSystemEnv("export HTTP_PROXY='http://proxy.example:8080'\nAIDEN_TEST=ok\n")
	if err != nil || len(assignments) != 2 || assignments[0].Value != "http://proxy.example:8080" {
		t.Fatalf("parsed assignments=%#v err=%v", assignments, err)
	}
	if _, err := parseSystemEnv("BROKEN='unterminated\n"); err == nil {
		t.Fatal("unterminated env value was accepted")
	}

	path := filepath.Join(t.TempDir(), "wpa.conf")
	contents := "country=US\nnetwork={\n ssid=\"demo\"\n psk=\"secret\"\n}\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	config, err := loadWiFiConfig(path)
	if err != nil || len(config.Networks) != 1 || config.Networks[0].SSID != "demo" || !config.Networks[0].ScanSSID {
		t.Fatalf("wifi config=%#v err=%v", config, err)
	}
}

func TestAgentCommandEnvironmentUsesStableLocalProxy(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(key, "")
	}
	options := testOptions(t)
	if err := os.WriteFile(options.SystemEnvPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := server.agentCommandEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, item := range environment {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	if values["HTTP_PROXY"] != "http://127.0.0.1:18080" || values["HTTPS_PROXY"] != "http://127.0.0.1:18080" {
		t.Fatalf("proxy environment=%#v", values)
	}
	if values["NO_PROXY"] != agent.DefaultNoProxy || values["no_proxy"] != agent.DefaultNoProxy {
		t.Fatalf("NO_PROXY=%q no_proxy=%q", values["NO_PROXY"], values["no_proxy"])
	}
}

func TestAgentCommandEnvironmentPreservesSOCKS5LocalProxyScheme(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(key, "")
	}
	options := testOptions(t)
	if err := os.WriteFile(options.SystemEnvPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	generated := "HTTP_PROXY=socks5h://127.0.0.1:18080\nHTTPS_PROXY=socks5h://127.0.0.1:18080\nALL_PROXY=socks5h://127.0.0.1:18080\nNO_PROXY=\nno_proxy=\n"
	if err := os.WriteFile(options.LocalProxyEnvironmentPath, []byte(generated), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := server.agentCommandEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, item := range environment {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy"} {
		if values[key] != "socks5h://127.0.0.1:18080" {
			t.Fatalf("%s=%q, want SOCKS5 local proxy", key, values[key])
		}
	}
}

func TestAgentCommandEnvironmentIgnoresStaleWiFiNoProxyWhenDisabled(t *testing.T) {
	for _, key := range []string{"HTTP_PROXY", "http_proxy", "HTTPS_PROXY", "https_proxy", "ALL_PROXY", "all_proxy", "NO_PROXY", "no_proxy"} {
		t.Setenv(key, "")
	}
	options := testOptions(t)
	systemEnv := "HTTP_PROXY=http://proxy.example:8080\nAIDEN_WIFI_PROXY_ENABLED=0\n"
	if err := os.WriteFile(options.SystemEnvPath, []byte(systemEnv), 0o600); err != nil {
		t.Fatal(err)
	}
	generated := "HTTP_PROXY=socks5h://127.0.0.1:18080\nNO_PROXY=stale.example\n"
	if err := os.WriteFile(options.LocalProxyEnvironmentPath, []byte(generated), 0o600); err != nil {
		t.Fatal(err)
	}
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	environment, err := server.agentCommandEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	values := make(map[string]string)
	for _, item := range environment {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	if values["HTTP_PROXY"] != "http://proxy.example:8080" {
		t.Fatalf("HTTP_PROXY=%q", values["HTTP_PROXY"])
	}
	if values["NO_PROXY"] != agent.DefaultNoProxy || values["no_proxy"] != agent.DefaultNoProxy {
		t.Fatalf("stale Wi-Fi NO_PROXY was retained: %#v", values)
	}
}

func TestRestoreWiFiPersistenceRestoresBothSnapshots(t *testing.T) {
	root := t.TempDir()
	wifiPath := filepath.Join(root, "wifi.conf")
	proxyPath := filepath.Join(root, "wifi-proxies.json")
	if err := os.WriteFile(wifiPath, []byte("old wifi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wifiPath, 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyPath, []byte("old proxy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(proxyPath, 0o644); err != nil {
		t.Fatal(err)
	}
	wifiSnapshot, err := captureFileSnapshot(wifiPath)
	if err != nil {
		t.Fatal(err)
	}
	proxySnapshot, err := captureFileSnapshot(proxyPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wifiPath, []byte("new wifi"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(proxyPath, []byte("new proxy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(wifiPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(proxyPath, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := restoreWiFiPersistence(wifiSnapshot, proxySnapshot); err != nil {
		t.Fatal(err)
	}
	if data, _ := os.ReadFile(wifiPath); string(data) != "old wifi" {
		t.Fatalf("Wi-Fi config=%q", data)
	}
	if data, _ := os.ReadFile(proxyPath); string(data) != "old proxy" {
		t.Fatalf("proxy config=%q", data)
	}
	for path, want := range map[string]os.FileMode{wifiPath: 0o640, proxyPath: 0o644} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := info.Mode().Perm(); got != want {
			t.Errorf("%s mode=%#o, want %#o", filepath.Base(path), got, want)
		}
	}
}

func TestRestoreWiFiPersistenceReportsIncompleteRecoveryAndContinues(t *testing.T) {
	root := t.TempDir()
	blockingPath := filepath.Join(root, "not-a-directory")
	if err := os.WriteFile(blockingPath, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	proxyPath := filepath.Join(root, "wifi-proxies.json")
	if err := os.WriteFile(proxyPath, []byte("new proxy"), 0o600); err != nil {
		t.Fatal(err)
	}
	wifiSnapshot := fileSnapshot{path: filepath.Join(blockingPath, "wifi.conf"), data: []byte("old wifi"), mode: 0o600, existed: true}
	proxySnapshot := fileSnapshot{path: proxyPath, data: []byte("old proxy"), mode: 0o600, existed: true}
	err := restoreWiFiPersistence(wifiSnapshot, proxySnapshot)
	if err == nil || !strings.Contains(err.Error(), "restore Wi-Fi config") {
		t.Fatalf("restore error=%v", err)
	}
	if data, _ := os.ReadFile(proxyPath); string(data) != "old proxy" {
		t.Fatalf("proxy restore was skipped after Wi-Fi restore failure: %q", data)
	}
}

func TestWiFiPublicValueUsesNetworkCollectionOnly(t *testing.T) {
	payload := (wiFiConfig{Country: "US", Networks: []wiFiNetwork{{SSID: "demo", PSK: "secret", Priority: 1}}}).publicValue()
	if _, exists := payload["ssid"]; exists {
		t.Fatalf("flat ssid field remains: %#v", payload)
	}
	if _, exists := payload["has_psk"]; exists {
		t.Fatalf("flat has_psk field remains: %#v", payload)
	}
	if _, exists := payload["networks"]; !exists {
		t.Fatalf("network collection missing: %#v", payload)
	}
}

func TestParseWiFiScanOutputDecodesEscapedUTF8SSID(t *testing.T) {
	text := `BSS aa:bb:cc:dd:ee:ff(on wlan0)
	SSID: \xe9\xa3\x9e\xe5\x88\xa9\xe7\x8c\xab\xe5\x93\x81\xe7\x89\x8cWiFi
	Cell 02 - Address: 11:22:33:44:55:66
		ESSID:"\xe9\xa3\x9e\xe5\x88\xa9\xe7\x8c\xab\xe5\x93\x81\xe7\x89\x8cWiFi"
	`
	want := []string{"飞利猫品牌WiFi"}
	if got := parseWiFiScanOutput(text).SSIDs; len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("parseWiFiScanOutput()=%q, want %q", got, want)
	}
}

func TestParseWiFiScanDetailsIncludesSecurityAndSignal(t *testing.T) {
	text := `BSS aa:bb:cc:dd:ee:ff(on wlan0)
	capability: ESS Privacy (0x0411)
	signal: -48.00 dBm
	SSID: Office
BSS aa:bb:cc:dd:ee:01(on wlan0)
	capability: ESS Privacy (0x0411)
	signal: -78.00 dBm
	SSID: Office
Cell 02 - Address: 11:22:33:44:55:66
		Quality=70/70  Signal level=-39 dBm
		Encryption key:off
		ESSID:"Guest"
`
	details := parseWiFiScanDetails(text)
	if len(details) != 2 {
		t.Fatalf("details=%#v", details)
	}
	if details[0].SSID != "Office" || !details[0].Secured || details[0].SignalDBM == nil || *details[0].SignalDBM != -48 {
		t.Fatalf("Office details=%#v", details[0])
	}
	if details[1].SSID != "Guest" || details[1].Secured || details[1].SignalDBM == nil || *details[1].SignalDBM != -39 {
		t.Fatalf("Guest details=%#v", details[1])
	}
}

func TestDecodeWiFiSSIDPreservesInvalidEscapes(t *testing.T) {
	for _, value := range []string{`Office\xZZ`, `Office\xE9`, `Office\x41\xZZ`} {
		if got := decodeWiFiSSID(value); got != value {
			t.Errorf("decodeWiFiSSID(%q)=%q, want unchanged", value, got)
		}
	}
}

func TestWiFiProxyRequestValidationAndPasswordProtection(t *testing.T) {
	customURL := "http://alice:secret@proxy.example:7890"
	noProxy := "localhost,.example.com"
	request := wifiConnectionRequest{SSID: "Office", ProxyMode: "proxy", ProxyURL: &customURL, NoProxy: &noProxy}
	if err := validateWiFiProxyRequest(request); err != nil {
		t.Fatal(err)
	}
	config := wifiproxy.EmptyConfig()
	if err := applyWiFiProxyRequest(&config, request); err != nil {
		t.Fatal(err)
	}
	public := (wiFiConfig{Networks: []wiFiNetwork{{SSID: "Office"}}}).publicValue(config)
	network := public["networks"].([]map[string]any)[0]
	// The detail page shows the saved proxy, but never its password.
	if network["proxy_mode"] != "proxy" || network["proxy_url"] != "http://alice:xxxxx@proxy.example:7890" || network["no_proxy"] != noProxy {
		t.Fatalf("public proxy=%#v", network)
	}

	// Saving the redacted form back (here with a new port) keeps the password.
	edited := "http://alice:xxxxx@proxy.example:8080"
	if err := applyWiFiProxyRequest(&config, wifiConnectionRequest{SSID: "Office", ProxyMode: "proxy", ProxyURL: &edited}); err != nil {
		t.Fatal(err)
	}
	editedSaved := "http://alice:secret@proxy.example:8080"
	if got := config.Networks["Office"].ProxyURL; got != editedSaved {
		t.Fatalf("saved proxy after redacted edit=%q", got)
	}

	blank := ""
	if err := applyWiFiProxyRequest(&config, wifiConnectionRequest{SSID: "Office", ProxyMode: "proxy", ProxyURL: &blank}); err != nil {
		t.Fatalf("blank saved proxy should preserve existing value: %v", err)
	}
	// A blank URL keeps whatever is stored, which is now the edited proxy.
	if got := config.Networks["Office"].ProxyURL; got != editedSaved {
		t.Fatalf("preserved proxy=%q, want %q", got, editedSaved)
	}
	emptyNoProxy := ""
	if err := applyWiFiProxyRequest(&config, wifiConnectionRequest{SSID: "Office", ProxyMode: "proxy", ProxyURL: &blank, NoProxy: &emptyNoProxy}); err != nil {
		t.Fatalf("clear NO_PROXY: %v", err)
	}
	if got := config.Networks["Office"].NoProxy; got != "" {
		t.Fatalf("NO_PROXY was not cleared: %q", got)
	}
	if err := applyWiFiProxyRequest(&config, wifiConnectionRequest{SSID: "New", ProxyMode: "proxy", ProxyURL: &blank}); err == nil {
		t.Fatal("blank proxy URL was accepted for a new custom proxy")
	}

	invalidURL := "ftp://proxy.example:21"
	if err := validateWiFiProxyRequest(wifiConnectionRequest{SSID: "Office", ProxyMode: "proxy", ProxyURL: &invalidURL}); err == nil {
		t.Fatal("unsupported proxy scheme was accepted")
	}
	if err := validateWiFiProxyRequest(wifiConnectionRequest{SSID: "Office", ProxyMode: "unknown"}); err == nil {
		t.Fatal("unsupported proxy mode was accepted")
	}
	if err := validateWiFiProxyRequest(wifiConnectionRequest{SSID: "Office", ProxyMode: "system", NoProxy: &noProxy}); err == nil {
		t.Fatal("Wi-Fi NO_PROXY was accepted for the system-default proxy")
	}
	if err := applyWiFiProxyRequest(&config, wifiConnectionRequest{SSID: "Office", ProxyMode: "system"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := config.Networks["Office"]; ok {
		t.Fatalf("system-default proxy retained Wi-Fi-specific settings: %#v", config.Networks["Office"])
	}
	if err := validateWiFiProxyRequest(wifiConnectionRequest{SSID: " Office ", ProxyMode: "direct"}); err == nil {
		t.Fatal("Wi-Fi proxy validation accepted an SSID with surrounding whitespace")
	}
	if err := applyWiFiProxyRequest(&config, wifiConnectionRequest{SSID: " Office ", ProxyMode: "direct"}); err == nil {
		t.Fatal("Wi-Fi proxy update accepted an SSID with surrounding whitespace")
	}
}

func TestWiFiForgetRemovesProxyMapping(t *testing.T) {
	options := testOptions(t)
	wifi := wiFiConfig{Country: "US", Networks: []wiFiNetwork{{SSID: "Office", PSK: "secret", Priority: 1}}}
	if err := saveWiFiConfig(options.WiFiConfigPath, wifi); err != nil {
		t.Fatal(err)
	}
	proxy := wifiproxy.EmptyConfig()
	proxy.Networks["Office"] = wifiproxy.Network{Mode: wifiproxy.ModeProxy, ProxyURL: "http://proxy.example:7890"}
	if err := wifiproxy.Save(options.WiFiProxyConfigPath, proxy); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", t.TempDir())
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodDelete, "/api/network/wifi/connection?ssid=Office", nil)
	server.APIHandler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	savedWiFi, err := loadWiFiConfig(options.WiFiConfigPath)
	if err != nil || len(savedWiFi.Networks) != 0 {
		t.Fatalf("saved Wi-Fi=%#v err=%v", savedWiFi, err)
	}
	savedProxy, err := wifiproxy.Load(options.WiFiProxyConfigPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, exists := savedProxy.Networks["Office"]; exists {
		t.Fatalf("forgotten proxy mapping remains: %#v", savedProxy)
	}
}

func TestWiFiCountryValidation(t *testing.T) {
	// An unusable code must be reported as such. Substituting a default here is
	// how a device ends up transmitting under rules nobody chose, so the parser
	// reports no country and callers resolve one explicitly.
	for input, want := range map[string]string{
		"us":            "US",
		" CN ":          "CN",
		"USA":           "",
		"U1":            "",
		"ZZ":            "",
		"US\nnetwork={": "",
	} {
		got, ok := wifiregion.Normalize(input)
		if got != want || ok != (want != "") {
			t.Errorf("wifiregion.Normalize(%q)=(%q,%v), want (%q,%v)", input, got, ok, want, want != "")
		}
	}
	if _, err := renderWiFiConfig(wiFiConfig{Country: "US\nnetwork={"}); err == nil {
		t.Fatal("renderWiFiConfig accepted an invalid country")
	}
	rendered, err := renderWiFiConfig(wiFiConfig{Country: "us"})
	if err != nil {
		t.Fatalf("renderWiFiConfig: %v", err)
	}
	if !strings.Contains(rendered, "country=US\n") {
		t.Fatalf("rendered country not normalized: %q", rendered)
	}
}

func TestEmptyFirmwareInfoMatchesSuccessShape(t *testing.T) {
	info := emptyFirmwareInfo()
	for _, key := range []string{"version", "build_time", "current_version", "current_build_time", "target_version", "target_build_time", "previous_version", "previous_build_time", "phase", "running_slot", "target_slot", "health_status", "health_error"} {
		if value, exists := info[key]; !exists || value != "" {
			t.Errorf("%s=%#v exists=%v", key, value, exists)
		}
	}
	components, ok := info["components"].(map[string]string)
	if !ok || components["boot"] != "" || components["rootfs"] != "" {
		t.Fatalf("components=%#v", info["components"])
	}
}

func TestFirmwareComponentVersionsUsesSelectedSlot(t *testing.T) {
	state := map[string]any{
		"slots": map[string]any{
			"b": map[string]any{
				"partitions": map[string]any{
					"boot":   map[string]any{"version": "boot-v2"},
					"rootfs": map[string]any{"version": "rootfs-v2"},
				},
			},
		},
	}
	got := firmwareComponentVersions(state, "b")
	if got["boot"] != "boot-v2" || got["rootfs"] != "rootfs-v2" {
		t.Fatalf("components=%#v", got)
	}
}

func TestRunHelpReturnsSuccess(t *testing.T) {
	if code := Run([]string{"--help"}); code != 0 {
		t.Fatalf("Run(--help)=%d, want 0", code)
	}
}

func TestRunRejectsRetiredWiFiIfaceFlag(t *testing.T) {
	if code := Run([]string{"--wifi-iface", "wlan1"}); code != 1 {
		t.Fatalf("Run(--wifi-iface)=%d, want 1", code)
	}
}

func TestFrameConfigRestartsOnlyFrameServiceAndRetriesFailures(t *testing.T) {
	options := testOptions(t)
	socketDir, err := os.MkdirTemp("", "frame-ready-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "f.sock")
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := os.WriteFile(options.AgentConfigPath, []byte("[advanced_settings.hardware.hid]\nframe_socket = "+strconv.Quote(socket)+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	marker := filepath.Join(dir, "restart")
	fakeAgent := filepath.Join(dir, "agent")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"ok\":true,\"config\":{},\"changed_paths\":[\"advanced_settings.hardware.frame_service.keep_streamon\"],\"reboot_required\":false,\"persisted\":true,\"revision\":11}'\n"
	if err := os.WriteFile(fakeAgent, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	init := filepath.Join(dir, "frame-init")
	t.Setenv("AIDEN_CONFIG_TEST_RESTART_MARKER", marker)
	if err := os.WriteFile(init, []byte("#!/bin/sh\nprintf '%s' frame >\"$AIDEN_CONFIG_TEST_RESTART_MARKER\"\nexit 1\n"), 0755); err != nil {
		t.Fatal(err)
	}
	var reloads atomic.Int64
	reload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reloads.Add(1)
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "applied": false, "pending": true})
	}))
	defer reload.Close()
	options.AgentBinary = fakeAgent
	options.AgentHTTPBaseURL = reload.URL
	options.FrameServiceInitScript = init
	options.AgentInitScript = "/must-not-execute"
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	save := func() *httptest.ResponseRecorder {
		resp := httptest.NewRecorder()
		server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"config":{"frame_service":{"keep_streamon":true}}}`)))
		return resp
	}
	failed := save()
	if failed.Code != http.StatusServiceUnavailable || reloads.Load() != 0 {
		t.Fatalf("failed service restart was applied: %d %s", failed.Code, failed.Body.String())
	}
	if err := os.WriteFile(init, []byte("#!/bin/sh\nprintf '%s' frame >\"$AIDEN_CONFIG_TEST_RESTART_MARKER\"\n"), 0755); err != nil {
		t.Fatal(err)
	}
	// An unchanged retry still has to finish the failed frame restart.
	if err := os.WriteFile(fakeAgent, []byte(strings.ReplaceAll(script, `["advanced_settings.hardware.frame_service.keep_streamon"]`, `[]`)), 0755); err != nil {
		t.Fatal(err)
	}
	applied := save()
	if applied.Code != http.StatusOK || !strings.Contains(applied.Body.String(), `"pending":true`) || !strings.Contains(applied.Body.String(), `"agent_restart_scheduled":false`) {
		t.Fatalf("status=%d body=%s", applied.Code, applied.Body.String())
	}
	if data, err := os.ReadFile(marker); err != nil || string(data) != "frame" {
		t.Fatalf("frame restart missing: %s %v", data, err)
	}
	if server.agentRestartPending() {
		t.Fatal("Agent restart was scheduled")
	}
}

func TestFrameServiceReadinessWaitsForListener(t *testing.T) {
	dir, err := os.MkdirTemp("", "frame-start-")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	socket := filepath.Join(dir, "f.sock")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- waitForFrameService(ctx, socket) }()
	select {
	case err := <-done:
		t.Fatalf("returned before listener: %v", err)
	case <-time.After(20 * time.Millisecond):
	}
	listener, err := net.Listen("unix", socket)
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	canceled, stop := context.WithCancel(context.Background())
	stop()
	if err := waitForFrameService(canceled, filepath.Join(dir, "missing.sock")); err == nil {
		t.Fatal("missing service reported ready")
	}
}

func TestConfigApplicationDropsReloadErrorAfterAgentRestart(t *testing.T) {
	options := testOptions(t)
	fakeAgent := filepath.Join(t.TempDir(), "fake-agent")
	script := "#!/bin/sh\ncat >/dev/null\nprintf '%s\\n' '{\"ok\":true,\"config\":{},\"changed_paths\":[],\"reboot_required\":false,\"persisted\":true,\"revision\":7}'\n"
	if err := os.WriteFile(fakeAgent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	agentID := "agent-one"
	failReload := false
	reload := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			mu.Lock()
			id := agentID
			mu.Unlock()
			revision := 6
			if id == "agent-two" {
				revision = 7
			}
			writeJSON(w, http.StatusOK, map[string]any{"state": "applied", "applied": true, "pending": false, "runtime_id": id, "revision": revision})
			return
		}
		mu.Lock()
		defer mu.Unlock()
		if failReload {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": "agent busy"})
			return
		}
		writeJSON(w, http.StatusAccepted, map[string]any{"ok": true, "applied": false, "pending": true, "runtime_id": agentID})
	}))
	defer reload.Close()
	options.AgentBinary = fakeAgent
	options.AgentHTTPBaseURL = reload.URL
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	save := func() *httptest.ResponseRecorder {
		resp := httptest.NewRecorder()
		server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPatch, "/api/config", strings.NewReader(`{"config":{}}`)))
		return resp
	}
	status := func() *httptest.ResponseRecorder {
		resp := httptest.NewRecorder()
		server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/config/application", nil))
		return resp
	}
	// A successful save records the answering Agent process.
	if resp := save(); resp.Code != http.StatusOK {
		t.Fatalf("first save: %d %s", resp.Code, resp.Body.String())
	}
	// The next reload request fails and the error is attributed to that process.
	mu.Lock()
	failReload = true
	mu.Unlock()
	if resp := save(); resp.Code != http.StatusServiceUnavailable {
		t.Fatalf("failing save: %d %s", resp.Code, resp.Body.String())
	}
	if resp := status(); resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"state":"failed"`) || !strings.Contains(resp.Body.String(), "agent busy") {
		t.Fatalf("same-process status: %d %s", resp.Code, resp.Body.String())
	}
	// A restarted Agent booted the persisted configuration: the stored error
	// is stale and must be dropped in favor of the live status.
	mu.Lock()
	agentID = "agent-two"
	mu.Unlock()
	if resp := status(); resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"state":"applied"`) || strings.Contains(resp.Body.String(), "agent busy") {
		t.Fatalf("restarted-agent status: %d %s", resp.Code, resp.Body.String())
	}
	// The error stays cleared for subsequent polls.
	if resp := status(); resp.Code != http.StatusOK || !strings.Contains(resp.Body.String(), `"state":"applied"`) {
		t.Fatalf("cleared status: %d %s", resp.Code, resp.Body.String())
	}
}

func TestConfigPlanRunsDryRunAndReportsApplyLevel(t *testing.T) {
	options := testOptions(t)
	fakeAgent := filepath.Join(t.TempDir(), "fake-agent")
	argsPath := filepath.Join(t.TempDir(), "args")
	t.Setenv("AIDEN_TEST_CONFIG_ARGS", argsPath)
	script := `#!/bin/sh
cat >/dev/null
printf '%s\n' "$@" > "$AIDEN_TEST_CONFIG_ARGS"
printf '%s\n' '{"ok":true,"config":{},"changed_paths":["basic_settings.device.hid.keyboard_layout"],"reboot_required":true,"persisted":false,"apply":"reboot","apply_reasons":["keyboard_layout"]}'
`
	if err := os.WriteFile(fakeAgent, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	options.AgentBinary = fakeAgent
	server, err := NewServer(options)
	if err != nil {
		t.Fatal(err)
	}
	resp := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(resp, httptest.NewRequest(http.MethodPost, "/api/config/plan", strings.NewReader(`{"config":{"hid":{"keyboard_layout":"azerty"}}}`)))
	if resp.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.Code, resp.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(resp.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["apply"] != "reboot" || body["reboot_required"] != true {
		t.Fatalf("plan body = %v", body)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--dry-run") {
		t.Fatalf("plan must not persist; config-update args were:\n%s", args)
	}

	bad := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(bad, httptest.NewRequest(http.MethodPost, "/api/config/plan", strings.NewReader(`{"patch":{}}`)))
	if bad.Code != http.StatusBadRequest {
		t.Fatalf("a malformed plan request must be rejected, status=%d", bad.Code)
	}
}
