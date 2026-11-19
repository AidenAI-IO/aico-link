package agent

import (
	"encoding/json"
	"fmt"
	"os"
)

// bootUSBConfig keeps reboot-only settings tied to a device boot, not an Agent
// process. It contains no credentials and is stored separately from agent.toml.
type bootUSBConfig struct {
	BootID         string `json:"boot_id"`
	DeviceType     string `json:"device_type"`
	KeyboardLayout string `json:"keyboard_layout"`
}

// ConfigForUSBBoot keeps the runtime consistent with the bound USB descriptor
// across Agent restarts. A new boot accepts the newly configured keyboard layout.
func ConfigForUSBBoot(cfg Config, statePath, bootID, pointerMode string) (Config, error) {
	if bootID == "" || (pointerMode != "absolute" && pointerMode != "touchscreen" && pointerMode != "touchpad") {
		return Config{}, fmt.Errorf("invalid USB boot identity")
	}
	state := bootUSBConfig{}
	data, err := os.ReadFile(statePath)
	if err == nil {
		if err := json.Unmarshal(data, &state); err != nil {
			return Config{}, fmt.Errorf("read USB boot settings: %w", err)
		}
	} else if !os.IsNotExist(err) {
		return Config{}, err
	}
	if state.BootID != bootID {
		state = bootUSBConfig{BootID: bootID, DeviceType: cfg.DeviceTypeOrDefault(), KeyboardLayout: cfg.HID.KeyboardLayoutOrDefault()}
	}
	// The descriptor is authoritative even on the first Agent start after an
	// installation, when the config may already differ from the running gadget.
	if (DeviceConfig{DeviceType: state.DeviceType}).PointerModeOrDefault() != pointerMode {
		switch pointerMode {
		case "touchscreen":
			state.DeviceType = "Android"
		case "absolute":
			state.DeviceType = "macOS"
		default:
			state.DeviceType = "iOS"
		}
	}
	encoded, err := json.Marshal(state)
	if err != nil {
		return Config{}, err
	}
	if err := writeFileAtomic(statePath, encoded, 0600); err != nil {
		return Config{}, fmt.Errorf("write USB boot settings: %w", err)
	}
	if cfg.PointerModeOrDefault() != pointerMode {
		cfg.Device.DeviceType = state.DeviceType
	}
	cfg.HID.PointerMode = pointerMode
	cfg.HID.KeyboardLayout = state.KeyboardLayout
	return cfg, nil
}
