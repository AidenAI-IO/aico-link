package agent

import (
	"reflect"
	"testing"
)

func TestConfigApplyLevel(t *testing.T) {
	base := Config{Device: DeviceConfig{DeviceType: "macOS"}, HID: HIDConfig{KeyboardLayout: "qwerty"}}
	tests := []struct {
		name        string
		edit        func(*Config)
		wantLevel   ApplyLevel
		wantReasons []string
	}{
		{"unrelated change", func(c *Config) { c.Locale = "en-US" }, ApplyLive, nil},
		// macOS and windows share absolute pointer mode, so no USB descriptor change.
		{"device type within pointer mode", func(c *Config) { c.Device.DeviceType = "windows" }, ApplyLive, nil},
		{"device type to iOS touchpad", func(c *Config) { c.Device.DeviceType = "iOS" }, ApplyReboot, []string{ApplyReasonPointerMode}},
		{"device type across pointer mode", func(c *Config) { c.Device.DeviceType = "Android" }, ApplyReboot, []string{ApplyReasonPointerMode}},
		{"keyboard layout", func(c *Config) { c.HID.KeyboardLayout = "azerty" }, ApplyReboot, []string{ApplyReasonKeyboardLayout}},
		{"both", func(c *Config) {
			c.Device.DeviceType = "Android"
			c.HID.KeyboardLayout = "azerty"
		}, ApplyReboot, []string{ApplyReasonPointerMode, ApplyReasonKeyboardLayout}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			next := base
			tt.edit(&next)
			level, reasons := ConfigApplyLevel(base, next)
			if level != tt.wantLevel || !reflect.DeepEqual(reasons, tt.wantReasons) {
				t.Fatalf("ConfigApplyLevel = %q %v, want %q %v", level, reasons, tt.wantLevel, tt.wantReasons)
			}
		})
	}
}

func TestMaxApplyLevel(t *testing.T) {
	if got := MaxApplyLevel(ApplyLive, ApplyAgentRestart); got != ApplyAgentRestart {
		t.Fatalf("live+agent_restart = %q", got)
	}
	if got := MaxApplyLevel(ApplyReboot, ApplyAgentRestart); got != ApplyReboot {
		t.Fatalf("a reboot must subsume an Agent restart, got %q", got)
	}
}
