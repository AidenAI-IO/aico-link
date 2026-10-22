package mcp

import "errors"

// ErrSupportBundleBusy is returned when another support_bundle call holds the
// concurrency slot. It is safe to surface to unauthenticated MCP clients.
var ErrSupportBundleBusy = errors.New("support_bundle busy; try again later")

// Runtime tool names that map 1:1 to Agent HTTP tools.
var runtimeToolNames = []string{
	"screenshot",
	"wait_for_stable_screen",
	"touch_gesture",
	"enter_text",
	"keyboard_tap",
	"open_app",
	"open_url",
	"quick_action",
	"bridge_clipboard",
	"shell",
}

// Status / config-web MCP tools (not Agent Runtime tools).
const (
	toolPhoneBridgeStatus = "phone_bridge_status"
	toolDeviceStatus      = "device_status"
	toolSupportBundle     = "support_bundle"
)

func statusToolNames() []string {
	return []string{toolPhoneBridgeStatus, toolDeviceStatus, toolSupportBundle}
}

func isRuntimeTool(name string) bool {
	for _, n := range runtimeToolNames {
		if n == name {
			return true
		}
	}
	return false
}

func isStatusTool(name string) bool {
	for _, n := range statusToolNames() {
		if n == name {
			return true
		}
	}
	return false
}
