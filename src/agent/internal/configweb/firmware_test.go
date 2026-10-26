package configweb

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFirmwareInfoBeforeFirstOTA(t *testing.T) {
	options := testOptions(t)
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
		"phase": "factory", "running_slot": "b", "health_status": "", "target_version": "",
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
	if info["version"] != "dev-v0.0.11" || info["build_time"] != "2026-09-30T01:00:00Z" || info["health_status"] != "success" {
		t.Fatalf("OTA state did not supersede factory baseline: %v", info)
	}
	components = info["components"].(map[string]string)
	if components["boot"] != "boot-v11" || components["rootfs"] != "rootfs-v11" {
		t.Fatalf("OTA components=%v", components)
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
