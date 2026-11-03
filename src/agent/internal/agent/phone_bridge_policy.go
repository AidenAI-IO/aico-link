package agent

import (
	"fmt"
	"strings"
	"time"
)

const phoneBridgeBackgroundStateMaxAge = 15 * time.Second

const (
	phoneBridgeDisconnectedRecoveryGuidance = "If Phone Bridge recovery is unavailable, do not stop the task: call screenshot first to inspect the current phone state, then use visible app search or suitable HID/touch tools. Call request_user_action only after observation or input fallback is also unavailable, or the next step truly requires user action."
	phoneBridgeIOSOpenAppRecoveryGuidance   = "If a Dynamic Island entry is visible, tap it to reopen the companion app, wait for Phone Bridge to reconnect, then retry. " + phoneBridgeDisconnectedRecoveryGuidance
)

const (
	toolOpenApp            = "open_app"
	toolOpenURL            = "open_url"
	toolSearchLaunchApp    = "search_launch_app"
	toolBridgeOpenApp      = "bridge_open_app"
	toolBridgeClipboard    = "bridge_clipboard"
	toolBridgeCalendar     = "bridge_calendar"
	toolBridgeContacts     = "bridge_contacts"
	toolBridgeNotification = "bridge_notification"
)

var phoneBridgeBackgroundSafeCommandTypes = map[string]struct{}{
	"clipboard_read":    {},
	"clipboard_write":   {},
	"calendar_create":   {},
	"calendar_query":    {},
	"calendar_delete":   {},
	"contacts_query":    {},
	"contacts_create":   {},
	"contacts_update":   {},
	"notification_send": {},
}

// iOS BLE Wake runs commands during a short system-granted background window.
// Keep this narrower than the generic PiP/FGS background-safe set: UIKit does
// not provide reliable clipboard access there, and contacts_update currently
// reports success without a trustworthy update result.
var phoneBridgeBLEBackgroundCommandTypes = map[string]struct{}{
	"calendar_create":   {},
	"calendar_query":    {},
	"calendar_delete":   {},
	"contacts_query":    {},
	"contacts_create":   {},
	"notification_send": {},
}

func phoneBridgeReadyForCommand(status PhoneBridgeStatus, commandTypes ...string) bool {
	if !status.Connected {
		return false
	}
	commandType := ""
	if len(commandTypes) > 0 {
		commandType = strings.TrimSpace(commandTypes[0])
	}
	if phoneBridgeIsAndroid(status) {
		state := strings.ToLower(strings.TrimSpace(status.AppState))
		if state == "active" {
			return true
		}
		if phoneBridgeBackgroundSafeCommandType(commandType) {
			return true
		}
		return false
	}
	if !phoneBridgeIsIOS(status) {
		return true
	}
	state := strings.ToLower(strings.TrimSpace(status.AppState))
	return state == "" || state == "active"
}

func normalizeAppState(value string) (string, bool) {
	state := strings.ToLower(strings.TrimSpace(value))
	switch state {
	case "active", "background", "inactive":
		return state, true
	default:
		return "", false
	}
}

func phoneBridgeCanUsePiPBackground(status PhoneBridgeStatus, commandType string) bool {
	if !phoneBridgeBackgroundSafeCommandType(commandType) {
		return false
	}
	// pip_bridge_enabled is an explicit transport-mode signal from the iOS
	// companion app. Once PiP mode is enabled, it is the authoritative signal
	// for background-safe clipboard/data commands; app-state timestamps are
	// lifecycle observations and may stop refreshing while the app remains in
	// the system-managed PiP background mode.
	return phoneBridgePiPBackgroundEnabled(status)
}

func phoneBridgeCanUseFGSBackground(status PhoneBridgeStatus, commandType string) bool {
	if !phoneBridgeBackgroundSafeCommandType(commandType) {
		return false
	}
	if !phoneBridgeFGSBackgroundEnabled(status) {
		return false
	}
	if status.FgsBridgeUpdatedAt == nil || time.Since(*status.FgsBridgeUpdatedAt) > phoneBridgeBackgroundStateMaxAge {
		return false
	}
	return true
}

func phoneBridgeCanUseBLEBackground(status PhoneBridgeStatus, commandType string) bool {
	if !phoneBridgeIsIOS(status) || !phoneBridgeBLEBackgroundCommandType(commandType) {
		return false
	}
	state := strings.ToLower(strings.TrimSpace(status.AppState))
	return state != "active"
}

func phoneBridgeShouldNotifyBLEWake(status PhoneBridgeStatus, commandType string) bool {
	if !phoneBridgeBLEBackgroundCommandType(commandType) {
		return false
	}
	if platform := phoneBridgePlatform(status); platform != "" && platform != "ios" {
		return false
	}
	state := strings.ToLower(strings.TrimSpace(status.AppState))
	return state != "active"
}

func phoneBridgeBackgroundSafeCommandType(commandType string) bool {
	_, ok := phoneBridgeBackgroundSafeCommandTypes[strings.TrimSpace(commandType)]
	return ok
}

func phoneBridgeBLEBackgroundCommandType(commandType string) bool {
	_, ok := phoneBridgeBLEBackgroundCommandTypes[strings.TrimSpace(commandType)]
	return ok
}

func phoneBridgeUnsupportedBLEBackgroundCommand(status PhoneBridgeStatus, commandType string) bool {
	if !phoneBridgeIsIOS(status) || !phoneBridgeAppNeedsForeground(status) {
		return false
	}
	return !phoneBridgeBLEBackgroundCommandType(commandType) &&
		!phoneBridgeCanUsePiPBackground(status, commandType) &&
		!phoneBridgeCanRestoreFromReturnEntry(status)
}

func phoneBridgeUnsupportedBLEBackgroundError(status PhoneBridgeStatus, commandType string, bleWakeAvailable bool) *ToolError {
	if !bleWakeAvailable || !phoneBridgeUnsupportedBLEBackgroundCommand(status, commandType) {
		return nil
	}
	return NewToolErrorWithDetails(
		CodeAppBackgrounded,
		fmt.Sprintf("%s is unavailable through iOS BLE Wake; bring the companion app to the foreground and retry", commandType),
		map[string]any{
			"command_type": commandType,
			"transport":    "ios_ble_wake",
		},
	)
}

func phoneBridgeHTTPPollCommandAllowed(platform, appState string, pipBridgeEnabled, fgsBridgeEnabled bool, commandType string) bool {
	status := PhoneBridgeStatus{Platform: platform, AppState: appState}
	if !phoneBridgeAppNeedsForeground(status) {
		return true
	}
	if phoneBridgeIsIOS(status) {
		if pipBridgeEnabled {
			return phoneBridgeBackgroundSafeCommandType(commandType)
		}
		return phoneBridgeBLEBackgroundCommandType(commandType)
	}
	if phoneBridgeIsAndroid(status) && fgsBridgeEnabled {
		return phoneBridgeBackgroundSafeCommandType(commandType)
	}
	return true
}

func phoneBridgeCommandAvailable(status PhoneBridgeStatus, commandType string, bleWakeAvailable bool) bool {
	return phoneBridgeReadyForCommand(status, commandType) ||
		phoneBridgeCanUsePiPBackground(status, commandType) ||
		phoneBridgeCanUseFGSBackground(status, commandType) ||
		(phoneBridgeCanUseBLEBackground(status, commandType) && bleWakeAvailable) ||
		phoneBridgeCanRestoreFromReturnEntry(status)
}

func phoneBridgePiPBackgroundEnabled(status PhoneBridgeStatus) bool {
	if status.PipBridgeEnabled == nil || !*status.PipBridgeEnabled {
		return false
	}
	state := strings.ToLower(strings.TrimSpace(status.AppState))
	return state == "" || state == "background" || state == "inactive"
}

func phoneBridgeFGSBackgroundEnabled(status PhoneBridgeStatus) bool {
	if status.FgsBridgeEnabled == nil || !*status.FgsBridgeEnabled {
		return false
	}
	if !phoneBridgeIsAndroid(status) {
		return false
	}
	state := strings.ToLower(strings.TrimSpace(status.AppState))
	return state == "background" || state == "inactive"
}

func phoneBridgePlatform(status PhoneBridgeStatus) string {
	return strings.ToLower(strings.TrimSpace(status.Platform))
}

func phoneBridgeIsIOS(status PhoneBridgeStatus) bool {
	return phoneBridgePlatform(status) == "ios"
}

func phoneBridgeIsAndroid(status PhoneBridgeStatus) bool {
	return phoneBridgePlatform(status) == "android"
}

func phoneBridgeCanRestoreFromReturnEntry(status PhoneBridgeStatus) bool {
	platform := phoneBridgePlatform(status)
	if platform != "" && platform != "ios" {
		return false
	}
	if phoneBridgePiPBackgroundEnabled(status) {
		return false
	}
	if status.ReturnEntryAvailable == nil || !*status.ReturnEntryAvailable {
		return false
	}
	entry := strings.ToLower(strings.TrimSpace(status.ReturnEntry))
	return entry == "dynamic_island"
}

func phoneBridgeAppNeedsForeground(status PhoneBridgeStatus) bool {
	appState := strings.ToLower(strings.TrimSpace(status.AppState))
	return appState == "background" || appState == "inactive"
}

func phoneBridgeHasReturnEntry(status PhoneBridgeStatus) bool {
	if phoneBridgePiPBackgroundEnabled(status) {
		return false
	}
	if status.ReturnEntryAvailable != nil {
		return *status.ReturnEntryAvailable
	}
	entry := strings.ToLower(strings.TrimSpace(status.ReturnEntry))
	return entry != "" && entry != "none"
}

func phoneBridgeRecoveryGuidance(status PhoneBridgeStatus) string {
	if phoneBridgeCanRestoreFromReturnEntry(status) {
		return "Retry the companion app tool; it can reopen the companion app through the Dynamic Island entry, wait for Phone Bridge to reconnect, then send the command. Use home-screen search or HID fallback only if restore fails."
	}
	if phoneBridgeIsAndroid(status) {
		return "Use screenshot plus HID/touch fallback; Android FGS Bridge mode only supports background-safe data tools."
	}
	if phoneBridgeIsIOS(status) {
		return "Use screenshot plus HID/touch fallback; only visible Dynamic Island return entries are auto-tapped, and lock-screen Live Activity or PiP-hidden entries need visual confirmation."
	}
	return "Use screenshot plus HID/touch fallback; retry companion app tools only after Phone Bridge reconnects or a visible platform return entry is confirmed."
}

func phoneBridgeOpenAppRecoveryGuidance(status PhoneBridgeStatus) string {
	if phoneBridgeIsIOS(status) || phoneBridgeCanRestoreFromReturnEntry(status) {
		return phoneBridgeIOSOpenAppRecoveryGuidance
	}
	if phoneBridgeIsAndroid(status) {
		return "On Android, keep the companion app open in the foreground and wait for Phone Bridge to reconnect, then retry. " + phoneBridgeDisconnectedRecoveryGuidance
	}
	return "Open the companion app and wait for Phone Bridge to reconnect, then retry. " + phoneBridgeDisconnectedRecoveryGuidance
}

func phoneBridgeRestoreUnavailableError(status PhoneBridgeStatus) error {
	if phoneBridgeFGSBackgroundEnabled(status) {
		state := strings.TrimSpace(status.AppState)
		if state == "" {
			state = "background"
		}
		return fmt.Errorf("phone bridge app is %s with Android FGS Bridge enabled, but this command requires the companion app in foreground", state)
	}
	if phoneBridgePiPBackgroundEnabled(status) {
		state := strings.TrimSpace(status.AppState)
		if state == "" {
			state = "background"
		}
		return fmt.Errorf("phone bridge app is %s with PiP Bridge mode enabled, but this command requires the companion app in foreground", state)
	}
	if status.Connected {
		state := strings.TrimSpace(status.AppState)
		if state == "" {
			return fmt.Errorf("phone bridge is connected but not ready for foreground command")
		}
		if phoneBridgeIsAndroid(status) {
			return fmt.Errorf("phone bridge app is %s; Android bridge commands require a connected foreground companion app", state)
		}
		if !phoneBridgeIsIOS(status) {
			return fmt.Errorf("phone bridge app is %s and no supported foreground return entry is available", state)
		}
		return fmt.Errorf("phone bridge app is %s and no supported Dynamic Island return entry is available", state)
	}
	if phoneBridgeIsAndroid(status) {
		return fmt.Errorf("phone bridge not connected for Android companion app")
	}
	if !phoneBridgeIsIOS(status) {
		return fmt.Errorf("phone bridge not connected")
	}
	return fmt.Errorf("phone bridge not connected and no supported Dynamic Island return entry is available")
}
