package agent

import (
	"os"
	"path/filepath"
	"testing"
)

func TestUSBBootConfigSurvivesAgentRestart(t *testing.T) {
	cfg := DefaultConfig()
	cfg.ConfigDir = t.TempDir()
	path := filepath.Join(cfg.ConfigDir, "cache", "usb-boot.json")
	if err := os.WriteFile(filepath.Join(cfg.ConfigDir, "agent.toml"), []byte("[device]\ndevice_type='iOS'\n"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ConfigForUSBBoot(cfg, path, "boot-one", "touchpad"); err != nil {
		t.Fatal(err)
	}
	next := cfg
	next.Device.DeviceType = "Android"
	next.HID.KeyboardLayout = "azerty"
	next.MaxIterations = 23
	active, err := ConfigForUSBBoot(next, path, "boot-one", "touchpad")
	if err != nil {
		t.Fatal(err)
	}
	if active.DeviceTypeOrDefault() != "iOS" || active.HID.KeyboardLayoutOrDefault() != "qwerty" || active.MaxIterations != 23 {
		t.Fatalf("active=%+v", active)
	}
	r := &Runtime{config: active}
	r.InitializeConfigApplication(next)
	if status := r.ConfigApplyStatus(); status.State != "reboot_required" || status.Applied {
		t.Fatalf("restarted Agent lost reboot: %+v", status)
	}
	// Saving the persisted values again must keep the reboot requirement.
	r.QueueConfig(next, 4)
	if !waitForConfigApplied(t, r).RebootRequired {
		t.Fatal("resave lost reboot")
	}
	r.StopConfigReloads()
	// A real boot has new descriptors and accepts the new layout.
	active, err = ConfigForUSBBoot(next, path, "boot-two", "touchscreen")
	if err != nil {
		t.Fatal(err)
	}
	if configRequiresReboot(active, next) {
		t.Fatal("device reboot did not apply USB settings")
	}
	// Non-Android device type changes remain hot reloadable.
	cfg.Device.DeviceType = "macOS"
	active, err = ConfigForUSBBoot(cfg, path, "boot-three", "absolute")
	if err != nil || active.DeviceTypeOrDefault() != "macOS" {
		t.Fatalf("desktop type lost: %v", err)
	}
}

func TestUSBBootConfigUsesBoundDescriptorOnFirstStart(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Device.DeviceType = "Android"
	active, err := ConfigForUSBBoot(cfg, filepath.Join(t.TempDir(), "usb.json"), "boot-one", "absolute")
	if err != nil || active.PointerModeOrDefault() != "absolute" {
		t.Fatalf("descriptor mismatch: %v", err)
	}
}
