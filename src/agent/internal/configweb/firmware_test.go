package configweb

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestFirmwareEndpointDoesNotProbeAgent(t *testing.T) {
	options := testOptions(t)
	marker := filepath.Join(t.TempDir(), "agent-probed")
	options.AgentInitScript = filepath.Join(t.TempDir(), "agent-status")
	if err := os.WriteFile(options.AgentInitScript, []byte("#!/bin/sh\ntouch '"+marker+"'\nexit 1\n"), 0700); err != nil {
		t.Fatal(err)
	}
	options.OTAConfigPath = filepath.Join(t.TempDir(), "ota-config.json")
	if err := os.WriteFile(options.OTAConfigPath, []byte(`{"factory_version":"factory-test"}`), 0600); err != nil {
		t.Fatal(err)
	}
	server := &Server{options: options}
	response := httptest.NewRecorder()
	server.APIHandler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/device/firmware", nil))
	var payload struct {
		Firmware struct {
			Version string `json:"version"`
		} `json:"firmware"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || payload.Firmware.Version != "factory-test" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("firmware read probed the Agent: %v", err)
	}
}

func TestFirmwareInfoBeforeFirstOTA(t *testing.T) {
	options := testOptions(t)
	options.BusinessPackageQueryBinary = filepath.Join(t.TempDir(), "dpkg-query")
	if err := os.WriteFile(options.BusinessPackageQueryBinary, []byte("#!/bin/sh\nprintf 'install ok installed\\t0.0.10-2\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	options.OTAConfigPath = filepath.Join(t.TempDir(), "ota-config.json")
	const config = `{"factory_version":"dev-v0.0.10","factory_build_time":"2026-09-29T01:00:00Z"}`
	if err := os.WriteFile(options.OTAConfigPath, []byte(config), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(options.CmdlinePath, []byte("aico.slot_suffix=_b"), 0600); err != nil {
		t.Fatal(err)
	}
	server := &Server{options: options}
	info := server.firmwareInfo()
	for key, want := range map[string]string{
		"version": "dev-v0.0.10", "current_version": "dev-v0.0.10",
		"build_time": "2026-09-29T01:00:00Z", "current_build_time": "2026-09-29T01:00:00Z",
		"business_version": "0.0.10-2",
		"phase":            "factory", "running_slot": "b", "health_status": "", "target_version": "",
	} {
		if got := info[key]; got != want {
			t.Errorf("%s=%v, want %q", key, got, want)
		}
	}
	components := info["components"].(map[string]string)
	if components["boot"] != "dev-v0.0.10" || components["rootfs"] != "dev-v0.0.10" {
		t.Fatalf("components=%v", components)
	}
	if _, err := os.Stat(options.OTAStatePath); !os.IsNotExist(err) {
		t.Fatalf("reading firmware information created OTA state: %v", err)
	}
	data, err := os.ReadFile(options.OTAConfigPath)
	if err != nil || string(data) != config {
		t.Fatalf("reading firmware information changed factory config: %q, %v", data, err)
	}

	// The same server must switch to OTA state as soon as an update creates it.
	const state = `{"phase":"committed","current_version":"dev-v0.0.11","current_build_time":"2026-09-30T01:00:00Z","active_slot":1,"slots":{"b":{"partitions":{"boot":{"version":"boot-v11"},"rootfs":{"version":"rootfs-v11"}}}}}`
	if err := os.WriteFile(options.OTAStatePath, []byte(state), 0600); err != nil {
		t.Fatal(err)
	}
	info = server.firmwareInfo()
	if info["version"] != "dev-v0.0.11" || info["build_time"] != "2026-09-30T01:00:00Z" || info["health_status"] != "success" || info["business_version"] != "0.0.10-2" {
		t.Fatalf("OTA state did not supersede factory baseline: %v", info)
	}
	components = info["components"].(map[string]string)
	if components["boot"] != "boot-v11" || components["rootfs"] != "rootfs-v11" {
		t.Fatalf("OTA components=%v", components)
	}
}

func TestBusinessPackageVersionRequiresInstalledState(t *testing.T) {
	options := testOptions(t)
	options.BusinessPackageQueryBinary = filepath.Join(t.TempDir(), "dpkg-query")
	server := &Server{options: options}
	for _, test := range []struct{ output, want string }{
		{"install ok installed\t0.0.78-1", "0.0.78-1"},
		{"hold ok installed\t0.0.78-1", "0.0.78-1"},
		{"install ok unpacked\t0.0.78-1", ""},
		{"hold ok unpacked\t0.0.78-1", ""},
		{"deinstall ok config-files\t0.0.77-1", ""},
		{"", ""},
	} {
		t.Run(test.output, func(t *testing.T) {
			if err := os.WriteFile(options.BusinessPackageQueryBinary, []byte("#!/bin/sh\nprintf '%s\\n' '"+test.output+"'\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			if version := server.businessPackageVersion(); version != test.want {
				t.Fatalf("state %q returned version %q, want %q", test.output, version, test.want)
			}
		})
	}
}

func TestFirmwareInfoDoesNotMaskInvalidStateWithFactoryVersion(t *testing.T) {
	for _, state := range []string{"", "{broken", "null", "{}"} {
		t.Run(state, func(t *testing.T) {
			options := testOptions(t)
			options.OTAConfigPath = filepath.Join(t.TempDir(), "ota-config.json")
			if err := os.WriteFile(options.OTAConfigPath, []byte(`{"factory_version":"old-factory"}`), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(options.OTAStatePath, []byte(state), 0600); err != nil {
				t.Fatal(err)
			}
			server := &Server{options: options}
			if info := server.firmwareInfo(); info["version"] != "" {
				t.Fatalf("invalid state shown as factory: %v", info)
			}
		})
	}
}

func TestFirmwareInfoWithoutFactoryVersion(t *testing.T) {
	for _, config := range []string{"missing", "", "{broken", "null", "{}", `{"factory_version":12}`, `{"factory_version":"  "}`} {
		t.Run(config, func(t *testing.T) {
			options := testOptions(t)
			options.OTAConfigPath = filepath.Join(t.TempDir(), "ota-config.json")
			if config != "missing" {
				if err := os.WriteFile(options.OTAConfigPath, []byte(config), 0600); err != nil {
					t.Fatal(err)
				}
			}
			server := &Server{options: options}
			info := server.firmwareInfo()
			if info["version"] != "" || info["phase"] != "" {
				t.Fatalf("invalid factory config shown as a version: %v", info)
			}
		})
	}
}
