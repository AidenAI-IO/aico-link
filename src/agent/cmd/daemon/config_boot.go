package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"aico-agent/internal/agent"
)

func configForRunningUSB(cfg agent.Config) (agent.Config, error) {
	if cfg.EnvironmentBridge.Enabled || cfg.DeviceTypeOverride != "" || cfg.HID.InputBackendADB() {
		return cfg, nil
	}
	descriptor, err := os.ReadFile("/sys/kernel/config/usb_gadget/aico_hid/functions/hid.usb1/report_desc")
	if os.IsNotExist(err) {
		return cfg, nil // Host development has no board USB gadget.
	}
	if err != nil {
		return cfg, err
	}
	mode := ""
	switch {
	case bytes.HasPrefix(descriptor, []byte{0x05, 0x0d, 0x09, 0x04}):
		mode = "touchscreen"
	case bytes.HasPrefix(descriptor, []byte{0x05, 0x01, 0x09, 0x02}):
		mode = "absolute"
	default:
		return cfg, fmt.Errorf("unknown bound USB pointer descriptor")
	}
	bootID, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return cfg, err
	}
	return agent.ConfigForUSBBoot(cfg, filepath.Join(cfg.ConfigDir, "cache", "usb-boot.json"), strings.TrimSpace(string(bootID)), mode)
}
