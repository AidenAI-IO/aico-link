package main

import (
	"os"
	"path/filepath"
	"strings"

	"aico-agent/internal/agent"
	"aico-agent/internal/usbgadget"
)

func configForRunningUSB(cfg agent.Config) (agent.Config, error) {
	if cfg.EnvironmentBridge.Enabled || cfg.DeviceTypeOverride != "" || cfg.HID.InputBackendADB() {
		return cfg, nil
	}
	mode, err := usbgadget.ReadPointerMode(usbgadget.PointerDescriptorPath)
	if os.IsNotExist(err) {
		return cfg, nil // Host development has no board USB gadget.
	}
	if err != nil {
		return cfg, err
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return cfg, err
	}
	return agent.ConfigForUSBBoot(cfg, filepath.Join(cfg.ConfigDir, "cache", "usb-boot.json"), strings.TrimSpace(string(bootID)), mode)
}
