package agent

// ApplyLevel names what a saved change needs before it takes effect. It is the
// single definition every settings surface reads: Config Web chooses between
// "Save" and "Save and restart" from it, and the companion app mirrors that
// choice on its native navigation bar.
type ApplyLevel string

const (
	// ApplyLive changes are hot-reloaded by the running Agent.
	ApplyLive ApplyLevel = "live"
	// ApplyAgentRestart changes are read only at Agent start, such as the
	// system environment file. The Config Web page and the USB link stay up.
	ApplyAgentRestart ApplyLevel = "agent_restart"
	// ApplyReboot changes alter the USB gadget the phone enumerated, so they
	// wait for a device reboot; until then the running Agent keeps the old value.
	ApplyReboot ApplyLevel = "reboot"
)

// Apply-level reasons, reported alongside the level so a client can explain it.
const (
	ApplyReasonPointerMode    = "pointer_mode"
	ApplyReasonKeyboardLayout = "keyboard_layout"
)

var applyLevelRank = map[ApplyLevel]int{ApplyLive: 0, ApplyAgentRestart: 1, ApplyReboot: 2}

// MaxApplyLevel returns the heavier of two levels. A reboot restarts the Agent
// too, so it subsumes an Agent restart.
func MaxApplyLevel(a, b ApplyLevel) ApplyLevel {
	if applyLevelRank[b] > applyLevelRank[a] {
		return b
	}
	return a
}

// ConfigApplyLevel classifies moving the running config from current to next.
//
// It never returns ApplyAgentRestart: the Agent hot-reloads every agent.toml
// field it reads, so a config change is either live or waits for the USB
// gadget to re-enumerate. Only the system environment file, which the Agent
// reads at start, needs a restart (see configweb's systemEnvironmentApply).
//
// The device type only matters through the pointer mode it derives: macOS,
// Windows and Linux share absolute pointer mode, so switching among them is
// live, while switching to or from iOS (touchpad) or Android (touchscreen)
// changes the HID descriptor.
// Both checks read resolved runtime values, so a spelling alias in the source
// file cannot masquerade as a USB/HID behaviour change.
func ConfigApplyLevel(current, next Config) (ApplyLevel, []string) {
	var reasons []string
	if current.PointerModeOrDefault() != next.PointerModeOrDefault() {
		reasons = append(reasons, ApplyReasonPointerMode)
	}
	if current.HID.KeyboardLayoutOrDefault() != next.HID.KeyboardLayoutOrDefault() {
		reasons = append(reasons, ApplyReasonKeyboardLayout)
	}
	if len(reasons) > 0 {
		return ApplyReboot, reasons
	}
	return ApplyLive, nil
}
