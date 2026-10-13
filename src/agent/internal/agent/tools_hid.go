package agent

import (
	"aiden-agent/internal/agent/mnk"
	"aiden-agent/internal/agent/screen"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	absMouseMaxPos = 32767

	defaultHIDRefreshStatePath        = "/run/aiden_usb_ecm_watchdog.state"
	defaultUSBCompositeRefreshCommand = "/usr/lib/aiden/aiden-usb-ecm-watchdog"

	// defaultTapHoldMs is the dwell between a touch press and release so iOS
	// registers a tap rather than dropping the sub-millisecond event or
	// treating it as a long-press.
	defaultTapHoldMs = 60

	// defaultSwipeHoldBeforeMs is the dwell at the start of a swipe so iOS
	// recognises the touchdown frame before the touch begins moving. The
	// edge-back recogniser in particular rejects swipes that move on the same
	// frame as the press.
	defaultSwipeHoldBeforeMs = 80

	// defaultSwipeHoldAfterMs defaults to zero so a swipe releases as soon as
	// it reaches the destination. Holding at the end makes phone UIs look like
	// the touch never released and can leave the screen stuck in a dragged
	// state.
	defaultSwipeHoldAfterMs = 0

	defaultSwipeDurationMs = 700
	defaultSwipeSteps      = 24

	// defaultCursorSettleMs is the dwell between positioning the HID absolute
	// cursor and pressing a button at that position. iOS HID cursor mode
	// smoothly animates the cursor toward the target; if the press lands while
	// the cursor is still in flight the input is interpreted as a drag from
	// the previous cursor position rather than a tap, which triggers
	// long-press menus or off-target activations.
	defaultCursorSettleMs = 80

	// screenDimensionsStaleAfter bounds how long cached screenshot dimensions
	// are trusted for pixel-coordinate resolution. Past this age the cache may
	// not match the current HDMI capture resolution.
	screenDimensionsStaleAfter = 30 * time.Second

	// defaultHIDWriteTimeout bounds each HID report write. Linux hidg writes can
	// otherwise block forever when the USB host stops polling, which leaves the
	// last successful pressed report active and prevents swipe release reports
	// from being sent.
	defaultHIDWriteTimeout = 750 * time.Millisecond

	// touchReleaseReportCount repeats the button-up/touch-up report for touch-like
	// gestures. On phone USB HID hosts a single final release report can be missed
	// or coalesced with the final move, leaving the UI in a pressed/dragged state.
	// Repeating the release is harmless for normal hosts and gives the target
	// several polling frames to observe button-up.
	touchReleaseReportCount   = 3
	touchReleaseReportDelayMs = 15

	// defaultKeyboardTapHoldMs matches example_usb_hid's default tap duration.
	// Zero hold often fails on macOS/iOS hosts that miss sub-frame modifier chords.
	defaultKeyboardTapHoldMs = 50
	// keyboardModifierTapHoldMs keeps modifier chords pressed long enough for
	// macOS/iOS hosts to register shortcuts like Cmd+Q.
	keyboardModifierTapHoldMs = 120
)

var hidKeyboardMap = map[string]uint8{
	"a": 0x04, "b": 0x05, "c": 0x06, "d": 0x07,
	"e": 0x08, "f": 0x09, "g": 0x0a, "h": 0x0b,
	"i": 0x0c, "j": 0x0d, "k": 0x0e, "l": 0x0f,
	"m": 0x10, "n": 0x11, "o": 0x12, "p": 0x13,
	"q": 0x14, "r": 0x15, "s": 0x16, "t": 0x17,
	"u": 0x18, "v": 0x19, "w": 0x1a, "x": 0x1b,
	"y": 0x1c, "z": 0x1d,
	"1": 0x1e, "2": 0x1f, "3": 0x20, "4": 0x21,
	"5": 0x22, "6": 0x23, "7": 0x24, "8": 0x25,
	"9": 0x26, "0": 0x27,
	"enter": 0x28, "escape": 0x29, "backspace": 0x2a,
	"tab": 0x2b, "space": 0x2c, "minus": 0x2d,
	"equal": 0x2e, "leftbrace": 0x2f, "rightbrace": 0x30,
	"backslash": 0x31, "semicolon": 0x33, "apostrophe": 0x34,
	"grave": 0x35, "comma": 0x36, "dot": 0x37, "slash": 0x38,
	"capslock": 0x39,
	"f1":       0x3a, "f2": 0x3b, "f3": 0x3c, "f4": 0x3d,
	"f5": 0x3e, "f6": 0x3f, "f7": 0x40, "f8": 0x41,
	"f9": 0x42, "f10": 0x43, "f11": 0x44, "f12": 0x45,
	"printscreen": 0x46, "scrolllock": 0x47, "pause": 0x48,
	"insert": 0x49, "home": 0x4a, "pageup": 0x4b,
	"delete": 0x4c, "end": 0x4d, "pagedown": 0x4e,
	"right": 0x4f, "left": 0x50, "down": 0x51, "up": 0x52,
}

var hidModifierMap = map[string]uint8{
	"ctrl": 0x01, "lctrl": 0x01, "rctrl": 0x10,
	"shift": 0x02, "lshift": 0x02, "rshift": 0x20,
	"alt": 0x04, "lalt": 0x04, "ralt": 0x40,
	"meta": 0x08, "lmeta": 0x08, "rmeta": 0x80,
	"super": 0x08, "win": 0x08, "cmd": 0x08,
}

var androidExtensionUsageMap = map[string]uint16{
	"android_back":                0x0224,
	"android_home":                0x0223,
	"menu":                        0x0040,
	"search":                      0x0221,
	"power":                       0x0030,
	"sleep":                       0x0032,
	"volume_mute":                 0x00e2,
	"volumeup":                    0x00e9,
	"volume_up":                   0x00e9,
	"volumedown":                  0x00ea,
	"volume_down":                 0x00ea,
	"media_fast_forward":          0x00b3,
	"media_rewind":                0x00b4,
	"media_next":                  0x00b5,
	"media_previous":              0x00b6,
	"media_stop":                  0x00b7,
	"media_play_pause":            0x00cd,
	"screenshot":                  0x0065,
	"key_usage_screenshot":        0x0065,
	"window":                      0x0067,
	"key_usage_window":            0x0067,
	"brightness_up":               0x006f,
	"key_usage_brightness_up":     0x006f,
	"brightness_down":             0x0070,
	"key_usage_brightness_down":   0x0070,
	"dictate":                     0x00d8,
	"key_usage_dictate":           0x00d8,
	"emoji_picker":                0x00d9,
	"key_usage_emoji_picker":      0x00d9,
	"media_audio_track":           0x0173,
	"key_usage_media_audio_track": 0x0173,
	"profile_switch":              0x019c,
	"key_usage_profile_switch":    0x019c,
	"settings":                    0x019f,
	"key_usage_settings":          0x019f,
	"new":                         0x0201,
	"key_usage_new":               0x0201,
	"close":                       0x0203,
	"key_usage_close":             0x0203,
	"print":                       0x0208,
	"key_usage_print":             0x0208,
	"refresh":                     0x0227,
	"key_usage_refresh":           0x0227,
	"fullscreen":                  0x0232,
	"key_usage_fullscreen":        0x0232,
	"language_switch":             0x029d,
	"key_usage_language_switch":   0x029d,
	// AOSP Generic.kl checks HID usage codes before Linux scan codes.
	// 0x0c01A2 is mapped to ALL_APPS, while 0x0c029F is mapped to
	// RECENT_APPS / KEYCODE_APP_SWITCH.
	"app_switch": 0x029f,
}

var absolutePointerModeExtensionReports = map[string]uint16{
	"volume_mute":               1 << 0,
	"volumeup":                  1 << 1,
	"volume_up":                 1 << 1,
	"volumedown":                1 << 2,
	"volume_down":               1 << 2,
	"media_play_pause":          1 << 3,
	"media_stop":                1 << 4,
	"media_next":                1 << 5,
	"media_previous":            1 << 6,
	"media_rewind":              1 << 7,
	"media_fast_forward":        1 << 8,
	"screenshot":                1 << 9,
	"key_usage_screenshot":      1 << 9,
	"brightness_up":             1 << 10,
	"key_usage_brightness_up":   1 << 10,
	"brightness_down":           1 << 11,
	"key_usage_brightness_down": 1 << 11,
}

const absolutePointerModeExtensionKeyList = "KEYCODE_VOLUME_MUTE, KEYCODE_VOLUME_UP, KEYCODE_VOLUME_DOWN, KEYCODE_MEDIA_PLAY_PAUSE, KEYCODE_MEDIA_STOP, KEYCODE_MEDIA_NEXT, KEYCODE_MEDIA_PREVIOUS, KEYCODE_MEDIA_REWIND, KEYCODE_MEDIA_FAST_FORWARD, KEYCODE_SCREENSHOT, KEYCODE_BRIGHTNESS_UP, KEYCODE_BRIGHTNESS_DOWN"

type androidKeyboardTapAlias struct {
	Keycode           int
	Replacement       string
	UnsupportedReason string
}

// Keep Android framework keycodes separate from the generic HID usage table.
// They are routed through hid.usb2, which advertises a Consumer Control
// interface instead of the boot keyboard report used by hid.usb0.
var androidKeyboardTapAliases = map[string]androidKeyboardTapAlias{
	"keycode_call": {
		Keycode:           5,
		UnsupportedReason: "call pickup requires an Android telephony/media key path beyond the current hid.usb2 Consumer Control interface",
	},
	"keycode_endcall": {
		Keycode:           6,
		UnsupportedReason: "call hangup requires an Android telephony/media key path beyond the current hid.usb2 Consumer Control interface",
	},
	"keycode_home": {
		Keycode:     3,
		Replacement: "android_home",
	},
	"keycode_menu": {
		Keycode:     82,
		Replacement: "menu",
	},
	"keycode_back": {
		Keycode:     4,
		Replacement: "android_back",
	},
	"keycode_search": {
		Keycode:     84,
		Replacement: "search",
	},
	"keycode_camera": {
		Keycode:           27,
		UnsupportedReason: "camera shutter requires a camera/media key path beyond the current hid.usb2 Consumer Control interface",
	},
	"keycode_focus": {
		Keycode:           80,
		UnsupportedReason: "camera focus requires a camera/media key path beyond the current hid.usb2 Consumer Control interface",
	},
	"keycode_power": {
		Keycode:     26,
		Replacement: "power",
	},
	"keycode_sleep": {
		Keycode:     223,
		Replacement: "sleep",
	},
	"keycode_wakeup": {
		Keycode:           224,
		UnsupportedReason: "wakeup requires a Generic Desktop/System Control HID path beyond the current hid.usb2 Consumer Control interface",
	},
	"keycode_soft_sleep": {
		Keycode:           276,
		UnsupportedReason: "soft sleep has no verified standard Consumer Control usage on this gadget",
	},
	"keycode_notification": {
		Keycode:           83,
		UnsupportedReason: "notification center has no verified standard Consumer Control usage on this gadget; use quick_action notification_center or touch_gesture instead",
	},
	"keycode_mute": {
		Keycode:           91,
		UnsupportedReason: "KEYCODE_MUTE is microphone mute, not speaker mute; hid.usb2 only exposes system speaker/stream volume mute",
	},
	"keycode_volume_mute": {
		Keycode:     164,
		Replacement: "volume_mute",
	},
	"keycode_volume_up": {
		Keycode:     24,
		Replacement: "volume_up",
	},
	"keycode_volume_down": {
		Keycode:     25,
		Replacement: "volume_down",
	},
	"keycode_media_play_pause": {
		Keycode:     85,
		Replacement: "media_play_pause",
	},
	"keycode_media_stop": {
		Keycode:     86,
		Replacement: "media_stop",
	},
	"keycode_media_next": {
		Keycode:     87,
		Replacement: "media_next",
	},
	"keycode_media_previous": {
		Keycode:     88,
		Replacement: "media_previous",
	},
	"keycode_media_rewind": {
		Keycode:     89,
		Replacement: "media_rewind",
	},
	"keycode_media_fast_forward": {
		Keycode:     90,
		Replacement: "media_fast_forward",
	},
	"keycode_app_switch": {
		Keycode:     187,
		Replacement: "app_switch",
	},
	// HID-backed Android KeyEvent aliases. The API levels are from
	// android.view.KeyEvent; replacements keep the existing Consumer Control
	// usage values in androidExtensionUsageMap unchanged.
	"keycode_window": { // API 11
		Keycode:     171,
		Replacement: "window",
	},
	"keycode_settings": { // API 11
		Keycode:     176,
		Replacement: "settings",
	},
	"keycode_language_switch": { // API 14
		Keycode:     204,
		Replacement: "language_switch",
	},
	"keycode_brightness_down": { // API 18
		Keycode:     220,
		Replacement: "brightness_down",
	},
	"keycode_brightness_up": { // API 18
		Keycode:     221,
		Replacement: "brightness_up",
	},
	"keycode_media_audio_track": { // API 19
		Keycode:     222,
		Replacement: "media_audio_track",
	},
	"keycode_refresh": { // API 28
		Keycode:     285,
		Replacement: "refresh",
	},
	"keycode_profile_switch": { // API 29
		Keycode:     288,
		Replacement: "profile_switch",
	},
	"keycode_emoji_picker": { // API 35
		Keycode:     317,
		Replacement: "emoji_picker",
	},
	"keycode_screenshot": { // API 35
		Keycode:     318,
		Replacement: "screenshot",
	},
	"keycode_dictate": { // API 36
		Keycode:     319,
		Replacement: "dictate",
	},
	"keycode_new": { // API 36
		Keycode:     320,
		Replacement: "new",
	},
	"keycode_close": { // API 36
		Keycode:     321,
		Replacement: "close",
	},
	"keycode_print": { // API 36
		Keycode:     323,
		Replacement: "print",
	},
	"keycode_fullscreen": { // API 36
		Keycode:     325,
		Replacement: "fullscreen",
	},
}

// HIDDevice manages a single HID device file with lazy open and auto-reopen.
type HIDDevice struct {
	path         string
	mu           sync.Mutex
	file         io.WriteCloser
	open         func(string) (io.WriteCloser, error)
	writeTimeout time.Duration
	openedAt     time.Time
	refreshState string
}

type pointerState struct {
	mu    sync.Mutex
	x     int
	y     int
	valid bool
}

func (s *pointerState) Update(x, y int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.x = x
	s.y = y
	s.valid = true
}

func (s *pointerState) Current() (x, y int, ok bool) {
	if s == nil {
		return 0, 0, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.valid {
		return 0, 0, false
	}
	return s.x, s.y, true
}

// pointerController sends hid.usb1 reports in absolute mouse or touchscreen mode.
type pointerController struct {
	dev                  *HIDDevice
	state                *pointerState
	touchscreen          bool
	iosKeyboardIsolation *iosKeyboardIsolationController
}

func newPointerController(hid HIDConfig) *pointerController {
	return &pointerController{
		dev:         NewHIDDevice(hid.MouseDeviceOrDefault()),
		state:       &pointerState{},
		touchscreen: hid.PointerTouchscreen(),
	}
}

func withIOSPointerCall(ctx context.Context, pc *pointerController, action func(context.Context) (string, error)) (string, error) {
	if pc == nil || pc.iosKeyboardIsolation == nil {
		return action(ctx)
	}
	return pc.iosKeyboardIsolation.withPointerCall(ctx, action)
}

func NewHIDDevice(path string) *HIDDevice {
	return &HIDDevice{
		path:         path,
		writeTimeout: defaultHIDWriteTimeout,
		refreshState: defaultHIDRefreshStatePath,
		open: func(path string) (io.WriteCloser, error) {
			return os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
		},
	}
}

func (d *HIDDevice) Write(data []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeLocked(data, nil)
}

func (d *HIDDevice) writeLocked(data []byte, after func()) error {
	if err := d.ensureOpenLocked(); err != nil {
		return err
	}
	if err := d.reopenStaleFileLocked(); err != nil {
		return err
	}

	n, err := d.writeOnceLocked(data)
	if err != nil {
		d.closeLocked()
		if n == 0 && hidShouldRetryWrite(err) {
			if reopenErr := d.ensureOpenLocked(); reopenErr == nil {
				n2, retryErr := d.writeOnceLocked(data)
				if retryErr == nil && n2 == len(data) {
					if after != nil {
						after()
					}
					return nil
				}
				d.closeLocked()
				if retryErr != nil {
					return fmt.Errorf("write %s: %w", d.path, retryErr)
				}
				return fmt.Errorf("write %s: short write after retry (%d/%d bytes)", d.path, n2, len(data))
			}
		}
		return fmt.Errorf("write %s: %w", d.path, err)
	}
	if n != len(data) {
		d.closeLocked()
		return fmt.Errorf("write %s: short write (%d/%d bytes)", d.path, n, len(data))
	}
	if after != nil {
		after()
	}
	// Sync to ensure data is flushed to USB gadget driver
	if f, ok := d.file.(*os.File); ok {
		_ = f.Sync()
	}
	return nil
}

func (d *HIDDevice) writeOnceLocked(data []byte) (int, error) {
	if fdFile, ok := d.file.(interface{ Fd() uintptr }); ok {
		return d.writeFDLocked(int(fdFile.Fd()), data)
	}
	return d.file.Write(data)
}

func (d *HIDDevice) writeFDLocked(fd int, data []byte) (int, error) {
	timeout := d.writeTimeout
	if timeout <= 0 {
		timeout = defaultHIDWriteTimeout
	}
	deadline := time.Now().Add(timeout)

	for {
		n, err := unix.Write(fd, data)
		if err == nil {
			return n, nil
		}
		if err == unix.EINTR {
			continue
		}
		if !hidWriteWouldBlock(err) {
			return n, err
		}

		remaining := time.Until(deadline)
		if remaining <= 0 {
			return 0, fmt.Errorf("timed out after %s waiting to write HID report", timeout)
		}

		pollTimeoutMs := int(remaining.Milliseconds())
		if pollTimeoutMs < 1 {
			pollTimeoutMs = 1
		}
		fds := []unix.PollFd{{
			Fd:     int32(fd),
			Events: unix.POLLOUT | unix.POLLERR | unix.POLLHUP,
		}}
		ready, pollErr := unix.Poll(fds, pollTimeoutMs)
		if pollErr == unix.EINTR {
			continue
		}
		if pollErr != nil {
			return 0, pollErr
		}
		if ready == 0 {
			return 0, fmt.Errorf("timed out after %s waiting to write HID report", timeout)
		}
		if fds[0].Revents&(unix.POLLERR|unix.POLLHUP|unix.POLLNVAL) != 0 {
			return 0, syscall.EPIPE
		}
	}
}

func (d *HIDDevice) Close() {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closeLocked()
}

func (d *HIDDevice) ensureOpenLocked() error {
	if d.file != nil {
		return nil
	}
	opener := d.open
	if opener == nil {
		opener = func(path string) (io.WriteCloser, error) {
			return os.OpenFile(path, os.O_WRONLY, 0)
		}
	}
	f, err := opener(d.path)
	if err != nil {
		// ENXIO means the device node exists but the USB gadget function is not enabled
		// (e.g. USB host suspended the connection). Try to trigger a USB composite refresh.
		if errors.Is(err, syscall.ENXIO) {
			refreshErr := triggerUSBCompositeRefresh(d.refreshState)
			if refreshErr == nil {
				// Wait briefly for the gadget to rebind and reach configured state
				time.Sleep(2 * time.Second)
				f, err = opener(d.path)
			} else {
				// Propagate watchdog refresh failure so it remains diagnosable
				return fmt.Errorf("open %s: USB composite refresh failed: %w (original error: %v)", d.path, refreshErr, err)
			}
		}
		if err != nil {
			return fmt.Errorf("open %s: %w", d.path, err)
		}
	}
	d.file = f
	d.openedAt = time.Now()
	return nil
}

func (d *HIDDevice) closeLocked() {
	if d.file != nil {
		_ = d.file.Close()
		d.file = nil
		d.openedAt = time.Time{}
	}
}

func (d *HIDDevice) reopenStaleFileLocked() error {
	if d.file == nil {
		return nil
	}
	if !d.openedAt.IsZero() && d.refreshState != "" {
		if info, err := os.Stat(d.refreshState); err == nil && info.ModTime().After(d.openedAt) {
			d.closeLocked()
			return d.ensureOpenLocked()
		}
	}

	statter, ok := d.file.(interface{ Stat() (os.FileInfo, error) })
	if !ok {
		return nil
	}
	fdInfo, err := statter.Stat()
	if err != nil {
		d.closeLocked()
		return d.ensureOpenLocked()
	}
	pathInfo, err := os.Stat(d.path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		d.closeLocked()
		return d.ensureOpenLocked()
	}
	if !os.SameFile(fdInfo, pathInfo) {
		d.closeLocked()
		return d.ensureOpenLocked()
	}
	return nil
}

func usbCompositeRefreshCommand() string {
	if command := strings.TrimSpace(os.Getenv("AIDEN_USB_COMPOSITE_REFRESH_COMMAND")); command != "" {
		return command
	}
	return defaultUSBCompositeRefreshCommand
}

func triggerUSBCompositeRefresh(_ string) error {
	// Trigger the USB watchdog's refresh command to rebind the USB composite gadget.
	// This is needed when the USB host suspends the connection or the gadget
	// enters a stale state where HID device nodes exist but are not functional.
	// Use a bounded timeout to prevent a hung watchdog from blocking HID recovery.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, usbCompositeRefreshCommand(), "refresh")
	output, err := cmd.CombinedOutput()
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("USB composite refresh timed out after 10s (output: %s)", string(output))
		}
		return fmt.Errorf("USB composite refresh failed: %w (output: %s)", err, string(output))
	}
	return nil
}

func hidShouldRetryWrite(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ESHUTDOWN) || errors.Is(err, syscall.EPIPE) || errors.Is(err, syscall.ENOTCONN) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ENODEV) {
		return true
	}
	text := strings.ToLower(err.Error())
	return strings.Contains(text, "transport endpoint shutdown") ||
		strings.Contains(text, "broken pipe") ||
		strings.Contains(text, "connection reset")
}

func hidWriteWouldBlock(err error) bool {
	return errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EWOULDBLOCK)
}

// KeyboardTapTool sends a key press then release via HID.
type KeyboardTapTool struct {
	mnkProvider  mnk.Provider
	deviceTypeFn func() string
}

func (t *KeyboardTapTool) Name() string { return "keyboard_tap" }

func (t *KeyboardTapTool) SetDeviceTypeFunc(fn func() string) {
	if t != nil {
		t.deviceTypeFn = fn
	}
}

func (t *KeyboardTapTool) platform() string {
	if t != nil && t.deviceTypeFn != nil {
		if deviceType, ok := normalizeDeviceType(t.deviceTypeFn()); ok {
			return normalizeAgentToolPlatform(deviceTypePlatform(deviceType))
		}
	}
	return ""
}

func (t *KeyboardTapTool) fullAndroidExtensionKeysVisible() bool {
	platform := t.platform()
	return platform == "" || platform == "android"
}

func (t *KeyboardTapTool) extensionKeyDescription() string {
	if t.fullAndroidExtensionKeysVisible() {
		return " Android device_type also supports single-key Android KEYCODE_* aliases through the extension keyboard device; they cannot be combined with modifiers/chords."
	}
	return " This non-Android device_type exposes only the absolute pointer-mode KEYCODE subset through the extension keyboard device: " + absolutePointerModeExtensionKeyList + "."
}

func (t *KeyboardTapTool) Description() string {
	description := `Press and release literal keyboard keys (e.g. {"keys":["enter"]}). Use for simple keys such as enter, escape, tab, or arrows; for exact physical chords explicitly requested by the user; and for app-specific shortcuts not represented by quick_action. Do not use keyboard_tap to enter text by spelling words, sentences, pinyin, romanization, or other sequential characters one key at a time. For text entry in an editable field, use enter_text. If enter_text fails or is unavailable, do not fall back to per-character keyboard_tap. A single letter or digit tap is appropriate only when the user explicitly requests that literal physical key, not as part of text entry. For cataloged semantic actions—including copy, paste, cut, select_all, delete_backward, delete_forward, undo, redo, find, send, back, home, app switching, and browser actions—you MUST use quick_action and let runtime select the device-specific binding from global device_type state. A ctrl/meta chord fallback is allowed only after a quick_action result in the current run explicitly reports the action as reserved/unavailable before executing a binding. Do not infer unavailability from another tool's failure. Never replay an active quick_action binding as a raw chord after failure or no visible effect.`
	if t.fullAndroidExtensionKeysVisible() {
		description += ` On Android device_type, single-key Android KEYCODE_* aliases are available through the Android extension keyboard device.`
	} else {
		description += ` On this non-Android device_type, only media, volume, screenshot, and brightness KEYCODE_* aliases are available through the extension keyboard device.`
	}
	return description
}

func (t *KeyboardTapTool) ArgsSchema() map[string]any {
	keysDescription := "Literal keys pressed simultaneously, e.g. [\"enter\"] or [\"shift\",\"tab\"]. Cataloged semantic actions MUST use quick_action. Raw ctrl/meta chords are allowed only for explicitly requested physical input, uncataloged app-specific shortcuts, or after a quick_action result in the current run explicitly reports the matching action as reserved/unavailable before execution. Do not infer unavailability or replay an active binding after failure or no visible effect. " +
		"Standard boot-keyboard keys: a-z, 0-9, f1-f12, enter, escape, backspace, tab, space, delete, arrows, home, end, pageup/down, insert, printscreen; modifiers ctrl, shift, alt, meta/super/win/cmd; modifier-only taps allowed. " +
		"backspace is backward-delete before the cursor; delete is forward-delete after the cursor. For semantic deletion, use quick_action delete_backward/delete_forward unless the user explicitly requests the literal physical key or the evidence-gated fallback above applies."
	keysDescription += t.extensionKeyDescription()
	keysSchema := stringArrayArgSchema(keysDescription, []string{"enter"}, []string{"shift", "tab"})
	keysSchema["minItems"] = 1
	keysSchema["maxItems"] = 6

	return objectArgsSchema(map[string]any{
		"keys": keysSchema,
	}, "keys")
}

func (t *KeyboardTapTool) Call(ctx context.Context, input string) (string, error) {
	adapter := mnk.NewKeyboardTapToolAdapter(t.mnkProvider)
	output, err := adapter.Call(ctx, input)
	return mapMNKAdapterResult(ctx, output, err)
}

// KeyboardTextTool types a string character by character via HID.
type KeyboardTextTool struct {
	textProvider         mnk.TextProvider
	dev                  *HIDDevice
	adb                  *ADBInputController
	keyboardLayout       string
	iosKeyboardIsolation *iosKeyboardIsolationController
}

// KeyboardText serves the provider endpoint through the same layout-aware
// primitive used by enter_text, without focusing or observing the device.
func (t *KeyboardTextTool) KeyboardText(ctx context.Context, text string) error {
	input, err := json.Marshal(map[string]string{"text": text})
	if err != nil {
		return err
	}
	ctx, _ = WithToolError(ctx)
	if _, err := t.Call(ctx, string(input)); err != nil {
		return err
	}
	if toolErr := ToolErrorFromContext(ctx); toolErr != nil {
		return fmt.Errorf("keyboard input failed: %s", toolErr.Message)
	}
	return nil
}

func (t *KeyboardTextTool) Name() string { return "keyboard_text" }

func (t *KeyboardTextTool) Description() string {
	return `Configured-layout ASCII text input only via USB HID physical keyboard (not the on-screen soft keyboard). ` +
		`The physical keyboard layout comes from hid.keyboard_layout (qwerty, azerty, or qwertz). ` +
		`Allowed characters: a-z, A-Z, 0-9, space, and common ASCII punctuation. ` +
		`For model/tool calls, pass JSON only, for example {"text":"App Store"}; do not pass a bare string. ` +
		`Do NOT pass non-ASCII text, emoji, or spaced romanization — use enter_text for input box entry. ` +
		`Do not transliterate Chinese/CJK targets to pinyin or guessed ASCII keywords; if enter_text is unavailable, report the blocker instead. ` +
		`If a Chinese IME is active but the target text is English, switch to the English/Latin keyboard first, commonly with the globe/input-method key; do not leave English text in Chinese IME preedit/candidate state. ` +
		`Do not use keyboard_text for picker/wheel values, even if tapping the selected row appears to expose edit mode; use touch_gesture swipes and verify each returned screenshot instead. ` +
		`keyboard_text remains for simple standalone ASCII typing outside the enter_text workflow. ` +
		`Bare plain text is accepted only as a legacy compatibility fallback.`
}

func (t *KeyboardTextTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"text": stringArgSchema("ASCII text to type using the configured physical keyboard layout."),
	}, "text")
}

func keyboardTextUsesModifier(layout, text string) bool {
	for _, ch := range text {
		stroke, ok := keyboardLayoutKeyStroke(layout, byte(ch))
		if ok && stroke.modifier != 0 {
			return true
		}
	}
	return false
}

func (t *KeyboardTextTool) Call(ctx context.Context, input string) (string, error) {
	text, errText := parseKeyboardTextInput(input)
	if errText != "" {
		return toolErrorResultString(ctx, CodeInvalidArguments, errText), nil
	}

	if unsupported := unsupportedKeyboardTextRunes(t.keyboardLayout, text); len(unsupported) > 0 {
		return toolErrorResultf(
			ctx,
			CodeInvalidArguments,
			"keyboard_text supports only ASCII characters available on the configured physical keyboard layout; unsupported characters: %q. Use enter_text for this target.",
			string(unsupported),
		), nil
	}
	if looksLikeSpacedRomanizationBlob(text) {
		return toolErrorResultString(ctx, CodeInvalidArguments, "keyboard_text received spaced romanization; use enter_text instead."), nil
	}

	if t.textProvider != nil {
		if err := t.textProvider.KeyboardText(ctx, text); err != nil {
			return toolErrorResultf(ctx, CodeToolExecutionFailed, "remote keyboard input failed: %v", err), nil
		}
		return "ok", nil
	}

	if t.adb != nil {
		if err := t.adb.Text(ctx, text); err != nil {
			return toolErrorResultf(ctx, adbInputToolErrorCode(err), "%v", err), nil
		}
		return "ok", nil
	}

	typeText := func() error {
		releaseReport := make([]byte, 8)
		for _, ch := range text {
			stroke, ok := keyboardLayoutKeyStroke(t.keyboardLayout, byte(ch))
			if !ok {
				continue
			}
			report := make([]byte, 8)
			report[0] = stroke.modifier
			report[2] = stroke.usage
			// Press then release immediately, same as keyboard_tap.
			if err := t.dev.Write(report); err != nil {
				return err
			}
			if err := t.dev.Write(releaseReport); err != nil {
				return err
			}
		}
		return nil
	}
	var err error
	if t.iosKeyboardIsolation != nil {
		err = t.iosKeyboardIsolation.withKeyboard(ctx, keyboardTextUsesModifier(t.keyboardLayout, text), typeText)
	} else {
		err = typeText()
	}
	if err != nil {
		return toolErrorResultf(ctx, CodeToolExecutionFailed, "%v", err), nil
	}

	return "ok", nil
}

// MouseMoveTool moves the mouse to coordinates without clicking.
type MouseMoveTool struct {
	mnkProvider mnk.Provider
}

func (t *MouseMoveTool) Name() string { return "mouse_move" }

func (t *MouseMoveTool) Description() string {
	return `Move the mouse without clicking. Use normalized coordinates (0-1000) from the latest screenshot, where (500,500) is center.`
}

func (t *MouseMoveTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"x": coordinateSchema("Normalized 0-1000 X coordinate.", 500),
		"y": coordinateSchema("Normalized 0-1000 Y coordinate.", 300),
	}, "x", "y")
}

func (t *MouseMoveTool) Call(ctx context.Context, input string) (string, error) {
	adapter := mnk.NewMouseMoveToolAdapter(t.mnkProvider)
	output, err := adapter.Call(ctx, input)
	return mapMNKAdapterResult(ctx, output, err)
}

// TouchGestureTool executes touch-like pointer gestures for mobile UI control.
type TouchGestureTool struct {
	mnkProvider        mnk.Provider
	screen             *screen.ScreenState
	touchscreen        bool
	primeScreenMapping func(context.Context) error
	deviceTypeFn       func() string
}

func (t *TouchGestureTool) Name() string { return "touch_gesture" }

func (t *TouchGestureTool) SetDeviceTypeFunc(fn func() string) {
	if t != nil {
		t.deviceTypeFn = fn
	}
}

func (t *TouchGestureTool) platform() string {
	if t != nil && t.deviceTypeFn != nil {
		if deviceType, ok := normalizeDeviceType(t.deviceTypeFn()); ok {
			return normalizeAgentToolPlatform(deviceTypePlatform(deviceType))
		}
	}
	return ""
}

func (t *TouchGestureTool) Description() string {
	return `Perform touch/pointer gestures. Provide either type or actions. Use the type form for normal interaction: tap, double_tap, long_press, swipe, drag_start, or drag_release. Atomic actions are a low-frequency advanced option only for an uninterrupted custom contact sequence that cannot be expressed by those standard gestures. Do not use actions for ordinary taps, long presses, swipes, scrolling, or moving draggable UI targets. Atomic actions execute in order and keep the contact pressed until touch_up. Atomic move_to accepts speed in normalized coordinate units per second; duration_ms overrides speed, and omitting both preserves immediate movement. ` +
		`Base coordinates on the latest screenshot using normalized 0-1000 coordinates where (500,500) is center. For taps and long presses, aim at the visual center of the target. Point, start, and end never accept screenshot pixels: convert a target measured at (pixel_x,pixel_y) in the latest image with x=pixel_x/max(image_width-1,1)*1000 and y=pixel_y/max(image_height-1,1)*1000 before calling. The tool returns a post-action screenshot. Swipe direction names describe finger movement, not content scroll. ` +
		`Many scroll controls continue receiving the same gesture after the initial press, even when the pointer moves outside their visible bounds. For browsing, searching, or picker scrolling, use type:swipe with explicit start and end. Start inside the intended scrollable region near the appropriate edge, with a small margin chosen from the latest screenshot. The end may extend beyond that region along the scroll direction while remaining inside the screen with a margin from physical screen edges. Use the available travel for larger movements and shorter travel near the target. Inspect the returned screenshot to confirm that the intended control moved; behavior can vary by control. Leave timing at defaults; the tool handles acceleration, braking, and release. ` +
		`For swipe, provide start and either end or direction (up/down/left/right). Speed is normalized coordinate units per second and defaults to 2500; duration_ms may be supplied to override the calculated duration. For HID swipes starting outside the outer 1% screen border with hold_after_ms=0, duration_ms controls main motion with a 180ms minimum and excludes an additional 100ms low-speed release tail, in both absolute-mouse and touchscreen modes. hold_before_ms and hold_after_ms optionally dwell after press and before release, and steps controls HID interpolation (provider default 24). A direction-only swipe travels toward the corresponding screen edge when duration_ms is omitted, or travels speed*duration_ms/1000 normalized units when duration_ms is supplied. ` +
		`Decide whether the requested target is draggable before choosing a gesture form. When moving an app icon, card, widget, list item, or any other draggable UI target, never use actions; always use exactly this sequence: call drag_start with the target's current point. drag_start internally waits for the screen to stabilize before returning its final screenshot; do not call wait_for_stable_screen separately in the normal drag flow. Confirm screen_stable=true, then inspect that returned stable screenshot to identify and confirm the final destination point and call drag_release with it. Never determine or guess the destination from an intermediate or screen_stable=false result. When drag_start returns screen_stable=false, it has automatically moved back to the original point and released the contact; inspect the returned screenshot and retry the complete drag flow instead of calling drag_release. drag_start presses for 500ms, then moves 200 normalized units at 500 normalized units per second (a 400ms interpolated move) in a bounded direction to activate dragging, and keeps the contact down only when its internal wait succeeds. drag_release moves directly to the destination, holds for 200ms, and releases. Never use the removed drag type or perform unrelated input while drag_start is active. ` +
		`For a picker/wheel column, use type:"swipe" and read the selected CENTER value plus adjacent row ordering. A desired row above center needs finger-down (end.y > start.y); a row below needs finger-up (end.y < start.y). For values increasing downward, 08 to 07 needs finger-down. Choose each swipe's start INSIDE the intended column from the latest screenshot: near its upper inner edge for finger-down or lower inner edge for finger-up. The end may extend beyond the column while staying inside the screen. Keep X aligned with the column. Estimate remaining rows from the visible value step and use the last observed movement to scale travel: if 120 normalized units moved three rows and one remains, try about 40, not another 120. Use longer travel when many rows remain and one-row corrections near the target. Keep the requested target fixed; verify the centered value after every swipe, even when screen_changed=false. Reset movement estimates when the column or UI changes. Use default timing and omit hold_after_ms. Do not use atomic actions, drag_start, keyboard entry, or taps on picker rows for this interaction.`
}

func (t *TouchGestureTool) ArgsSchema() map[string]any {
	typeDescription := "Gesture type. tap, double_tap, long_press, drag_start, and drag_release require point. swipe requires start and either end or direction."
	speedSchema := numberArgSchema("Swipe speed in normalized coordinate units per second. Defaults to 2500.", 2500)
	speedSchema["exclusiveMinimum"] = 0
	schema := objectArgsSchema(map[string]any{
		"actions": map[string]any{
			"type":        "array",
			"minItems":    1,
			"maxItems":    128,
			"description": "Low-frequency advanced touch program for an uninterrupted custom contact sequence that standard gesture types cannot express. Never use actions for a draggable UI target; use drag_start, which internally waits for screen stability, determine the destination from its returned screen_stable=true screenshot, then use drag_release. A screen_stable=false drag_start is automatically canceled at its original point and must be retried instead of released. Each action is one of touch_down, move_to, wait, or touch_up. move_to and touch_down require point; wait requires ms. move_to may use speed or duration_ms for interpolated movement, with duration_ms taking precedence. A program must end with touch_up.",
			"items": objectArgsSchema(map[string]any{
				"action":      stringEnumArgSchema("Atomic action to execute.", "touch_down", "move_to", "wait", "touch_up"),
				"point":       pointSchema("Normalized point for move_to or touch_down."),
				"x":           coordinateSchema("Normalized X coordinate; use point for new programs."),
				"y":           coordinateSchema("Normalized Y coordinate; use point for new programs."),
				"ms":          rangedIntegerArgSchema("Wait duration in milliseconds.", 0, 30000),
				"duration_ms": rangedIntegerArgSchema("Optional movement duration for move_to in milliseconds.", 0, 30000),
				"speed":       speedSchema,
				"button":      stringEnumArgSchema("Pointer button held during the contact.", "left", "right", "middle"),
			}, "action"),
		},
		"type":           stringEnumArgSchema(typeDescription, "tap", "double_tap", "long_press", "drag_start", "drag_release", "swipe"),
		"point":          pointSchema(`Required for tap, double_tap, long_press, drag_start, and drag_release. drag_start uses the target's current point; drag_release uses the destination confirmed from the screen_stable=true screenshot returned by drag_start after its internal stability wait. Must be a JSON object containing both named keys "x" and "y"; do not use an array, bare value, or positional shorthand.`),
		"start":          pointSchema(`Required start point for swipe. For scrolling and pickers, choose each swipe's start inside the intended visible region from the latest screenshot. Use its upper inner edge for finger-down or lower inner edge for finger-up, with a margin from the current screenshot. Must be a JSON object containing both named keys "x" and "y"; do not use an array, bare value, or positional shorthand.`),
		"end":            pointSchema(`End point for swipe. After starting inside the intended scrollable region, the end may extend beyond that region along the scroll direction while staying inside the screen with a small margin from physical edges. Many controls continue receiving the gesture after the initial press. Use longer travel when far from the target and shorter travel for fine adjustment; verify movement in the returned screenshot. Swipe may provide direction instead of end.`),
		"direction":      stringEnumArgSchema("Swipe direction when end is omitted. The gesture starts at start and moves toward that direction.", "up", "down", "left", "right"),
		"button":         stringEnumArgSchema("Mouse button for pointer gestures.", "left", "right", "middle"),
		"hold_ms":        nonNegativeIntegerSchema("Tap or long-press hold duration in milliseconds."),
		"speed":          speedSchema,
		"duration_ms":    rangedIntegerArgSchema("Optional swipe main-motion duration in milliseconds. With end, it overrides timing calculated from speed; with direction, speed × duration determines travel. For HID swipes starting outside the outer 1% screen border with hold_after_ms=0, duration_ms controls main motion with a 180ms minimum and excludes an additional 100ms low-speed release tail, in both absolute-mouse and touchscreen modes.", 1, mnk.MaxSwipeDurationMs),
		"hold_before_ms": rangedIntegerArgSchema("Optional swipe dwell after pressing and before movement.", 0, mnk.MaxSwipeHoldMs),
		"hold_after_ms":  rangedIntegerArgSchema("Optional swipe dwell at the end before release.", 0, mnk.MaxSwipeHoldMs),
		"steps":          rangedIntegerArgSchema("Optional HID interpolation step count; larger values produce smoother motion. Defaults to the provider default (24).", 1, mnk.MaxSwipeSteps),
	})
	schema["description"] = `JSON object for one standard gesture or, rarely, an advanced atomic touch program. Provide either type or actions; a call with neither is rejected. Use type for normal interaction. Reserve actions for uninterrupted custom contact timing that tap, double_tap, long_press, swipe, drag_start, or drag_release cannot express. Never use actions to move a draggable target: call drag_start at its current point, let its internal stability wait finish, determine the destination from its returned screen_stable=true screenshot, then call drag_release at the confirmed point. A screen_stable=false drag_start automatically returns to its original point and releases; inspect its screenshot and retry drag_start rather than calling drag_release. Do not call wait_for_stable_screen separately in the normal drag flow. Unknown fields are ignored. Coordinate fields point, start, and end use named objects containing both x and y. For scrolling, start inside the intended scrollable region near its appropriate edge. Many controls continue receiving the gesture after the initial press, so the explicit end may extend beyond that region while staying inside the screen with a margin from physical edges. Use the available travel for larger movements and shorten it near the target; verify the result after every swipe and leave timing at defaults. Swipe accepts either start+end or start+direction; speed defaults to 2500 normalized coordinate units per second and duration_ms overrides calculated timing. For HID swipes starting outside the outer 1% screen border with hold_after_ms=0, duration_ms controls main motion with a 180ms minimum and excludes an additional 100ms low-speed release tail, in both absolute-mouse and touchscreen modes. hold_before_ms, hold_after_ms, and steps are optional swipe timing controls.`
	schema["examples"] = []map[string]any{
		{"type": "tap", "point": map[string]any{"x": 500, "y": 500}},
		{"type": "drag_start", "point": map[string]any{"x": 400, "y": 500}},
		{"type": "drag_release", "point": map[string]any{"x": 750, "y": 500}},
	}
	return schema
}

func pointSchema(description string) map[string]any {
	schema := objectArgsSchema(map[string]any{
		"x": coordinateSchema("Normalized 0-1000 X coordinate, not a screenshot pixel X value."),
		"y": coordinateSchema("Normalized 0-1000 Y coordinate, not a screenshot pixel Y value."),
	}, "x", "y")
	schema["description"] = description + ` Coordinates always use normalized 0-1000 values, never screenshot pixels.`
	return schema
}

func (t *TouchGestureTool) Call(ctx context.Context, input string) (string, error) {
	if err := t.ensureTouchscreenMapping(ctx); err != nil {
		return toolErrorResultf(ctx, CodeToolExecutionFailed, "touchscreen mapping unavailable: %v", err), nil
	}
	adapter := mnk.NewTouchGestureToolAdapter(t.mnkProvider)
	output, err := adapter.Call(ctx, input)
	return mapMNKAdapterResult(ctx, output, err)
}

func (t *TouchGestureTool) recoverAfterUnstableScreen(ctx context.Context, input string) error {
	var args struct {
		Type  string          `json:"type"`
		Point json.RawMessage `json:"point"`
	}
	if err := json.Unmarshal([]byte(input), &args); err != nil || !strings.EqualFold(strings.TrimSpace(args.Type), "drag_start") {
		return nil
	}
	if len(args.Point) == 0 || string(args.Point) == "null" {
		return fmt.Errorf("drag_start recovery requires the original point")
	}
	releaseInput, err := json.Marshal(struct {
		Type  string          `json:"type"`
		Point json.RawMessage `json:"point"`
	}{Type: "drag_release", Point: args.Point})
	if err != nil {
		return fmt.Errorf("encode drag_start recovery: %w", err)
	}
	_, err = mnk.NewTouchGestureToolAdapter(t.mnkProvider).Call(ctx, string(releaseInput))
	return err
}

func (t *TouchGestureTool) ensureTouchscreenMapping(ctx context.Context) error {
	if t == nil || !t.touchscreen || t.screen == nil || t.primeScreenMapping == nil {
		return nil
	}
	if t.screen.FreshActiveArea(screenDimensionsStaleAfter) {
		return nil
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("touch_gesture prime mapping before input mapping_before={%s}", t.screen.Format())
	}
	if err := t.primeScreenMapping(ctx); err != nil {
		return err
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("touch_gesture prime mapping succeeded mapping_after={%s}", t.screen.Format())
	}
	return nil
}

func sameResolvedPointerPoint(first, second resolvedPointerPoint) bool {
	return first.x == second.x && first.y == second.y
}

// MouseScrollTool sends mouse wheel events.
type MouseScrollTool struct {
	mnkProvider mnk.Provider
}

func (t *MouseScrollTool) Name() string { return "mouse_scroll" }

func (t *MouseScrollTool) Description() string {
	return `Scroll the mouse wheel. This is a wheel event, not equivalent to a mobile swipe gesture; use touch_gesture for swipes. Unsupported when pointer_mode is touchscreen.`
}

func (t *MouseScrollTool) ArgsSchema() map[string]any {
	return objectArgsSchema(map[string]any{
		"delta": rangedIntegerArgSchema("Mouse wheel delta. Positive scrolls up, negative scrolls down.", -127, 127),
	}, "delta")
}

func (t *MouseScrollTool) Call(ctx context.Context, input string) (string, error) {
	adapter := mnk.NewMouseScrollToolAdapter(t.mnkProvider)
	output, err := adapter.Call(ctx, input)
	return mapMNKAdapterResult(ctx, output, err)
}

// mapMNKAdapterResult converts mnk adapter failures into the agent tool contract:
// structured ToolError via toolErrorResult*, with a nil Go error. This keeps
// executeToolCall / quick_action.delegate on the recoverable observation path.
func mapMNKAdapterResult(ctx context.Context, output string, err error) (string, error) {
	if err == nil {
		return output, nil
	}
	if mnkErr := mnk.AsError(err); mnkErr != nil {
		switch mnkErr.Kind {
		case mnk.ErrInvalidArguments:
			return toolErrorResultString(ctx, CodeInvalidArguments, mnkErr.Error()), nil
		case mnk.ErrModuleUnavailable:
			return toolErrorResultString(ctx, CodeModuleUnavailable, mnkErr.Error()), nil
		case mnk.ErrExecutionFailed:
			return toolErrorResultString(ctx, CodeToolExecutionFailed, mnkErr.Error()), nil
		}
	}
	return toolErrorResultf(ctx, CodeToolExecutionFailed, "%v", err), nil
}

// writeAbsMouseReport writes an absolute mouse report:
// [buttons, x_lo, x_hi, y_lo, y_hi, wheel].
func writeAbsMouseReport(dev *HIDDevice, state *pointerState, x, y int, buttons uint8, wheel int8) error {
	absX := clampUint16(x, absMouseMaxPos)
	absY := clampUint16(y, absMouseMaxPos)

	report := make([]byte, 6)
	report[0] = buttons
	binary.LittleEndian.PutUint16(report[1:3], absX)
	binary.LittleEndian.PutUint16(report[3:5], absY)
	report[5] = byte(wheel)

	var after func()
	if state != nil {
		after = func() {
			state.Update(int(absX), int(absY))
		}
	}

	dev.mu.Lock()
	defer dev.mu.Unlock()
	return dev.writeLocked(report, after)
}

// writeTouchscreenReport writes a single-contact touch report:
// [flags, contact_id, x_lo, x_hi, y_lo, y_hi]. flags bit0=tip switch, bit1=in range.
func writeTouchscreenReport(dev *HIDDevice, state *pointerState, x, y int, touching bool) error {
	absX := clampUint16(x, absMouseMaxPos)
	absY := clampUint16(y, absMouseMaxPos)
	report := make([]byte, 6)
	if touching {
		report[0] = 0x03
	}
	report[1] = 0x01
	binary.LittleEndian.PutUint16(report[2:4], absX)
	binary.LittleEndian.PutUint16(report[4:6], absY)

	var after func()
	if state != nil {
		after = func() {
			state.Update(int(absX), int(absY))
		}
	}

	path := "<nil>"
	if dev != nil {
		path = dev.path
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf(
			"hid touchscreen report write start path=%s requested=(%d,%d) clamped=(%d,%d) touching=%v flags=0x%02x report=% x",
			path,
			x,
			y,
			absX,
			absY,
			touching,
			report[0],
			report,
		)
	}
	dev.mu.Lock()
	defer dev.mu.Unlock()
	err := dev.writeLocked(report, after)
	if err != nil {
		if touchscreenRCADebugEnabledCached() {
			touchscreenRCALogf("hid touchscreen report write error path=%s clamped=(%d,%d) touching=%v err=%v", path, absX, absY, touching, err)
		}
		return err
	}
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("hid touchscreen report write ok path=%s clamped=(%d,%d) touching=%v", path, absX, absY, touching)
	}
	return nil
}

func (pc *pointerController) currentXY() (int, int) {
	if pc.state != nil {
		if x, y, ok := pc.state.Current(); ok {
			return x, y
		}
	}
	return absMouseMaxPos / 2, absMouseMaxPos / 2
}

func (pc *pointerController) moveTo(x, y int, buttons uint8) error {
	if pc.touchscreen {
		return writeTouchscreenReport(pc.dev, pc.state, x, y, buttons != 0)
	}
	return writeAbsMouseReport(pc.dev, pc.state, x, y, buttons, 0)
}

func (pc *pointerController) scroll(delta int) error {
	if delta == 0 {
		return nil
	}
	if delta < -127 {
		delta = -127
	} else if delta > 127 {
		delta = 127
	}
	if pc.touchscreen {
		return nil
	}
	x, y := pc.currentXY()
	return writeAbsMouseReport(pc.dev, pc.state, x, y, 0, int8(delta))
}

type pointerPoint struct {
	X pointerCoordinate `json:"x"`
	Y pointerCoordinate `json:"y"`
}

func (p *pointerPoint) UnmarshalJSON(data []byte) error {
	// Accept array format [x, y] as fallback for models that pass coordinates as arrays.
	var arr []json.RawMessage
	if json.Unmarshal(data, &arr) == nil && len(arr) == 2 {
		if err := json.Unmarshal(arr[0], &p.X); err != nil {
			return fmt.Errorf("coordinate must be a number or numeric string")
		}
		if err := json.Unmarshal(arr[1], &p.Y); err != nil {
			return fmt.Errorf("coordinate must be a number or numeric string")
		}
		return nil
	}
	// Normal object format {"x":N,"y":M}
	type plain pointerPoint
	return decodeStrictJSONObject(string(data), (*plain)(p))
}

type pointerCoordinate float64

func (c *pointerCoordinate) UnmarshalJSON(data []byte) error {
	var number float64
	if err := json.Unmarshal(data, &number); err == nil {
		return c.setFinite(number)
	}

	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		value, parseErr := strconv.ParseFloat(strings.TrimSpace(text), 64)
		if parseErr != nil {
			return fmt.Errorf("parse coordinate %q: %w", text, parseErr)
		}
		return c.setFinite(value)
	}

	return fmt.Errorf("coordinate must be a number or numeric string")
}

func (c *pointerCoordinate) setFinite(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return fmt.Errorf("coordinate must be a finite number")
	}
	*c = pointerCoordinate(value)
	return nil
}

func (c pointerCoordinate) Float64() float64 {
	return float64(c)
}

type resolvedPointerPoint struct {
	x int
	y int
}

func resolveRequiredPoint(screen *screen.ScreenState, touchscreen bool, point *pointerPoint) (resolvedPointerPoint, error) {
	if point == nil {
		return resolvedPointerPoint{}, fmt.Errorf("point is required")
	}

	x, y, err := resolvePointerPositionForSurface(screen, touchscreen, point.X.Float64(), point.Y.Float64())
	if err != nil {
		return resolvedPointerPoint{}, err
	}
	return resolvedPointerPoint{x: x, y: y}, nil
}

func resolvePointOrDefaultNormalized(screen *screen.ScreenState, touchscreen bool, point *pointerPoint, defaultX, defaultY float64) (resolvedPointerPoint, error) {
	if point != nil {
		return resolveRequiredPoint(screen, touchscreen, point)
	}

	x, y, err := resolvePointerPositionForSurface(screen, touchscreen, defaultX, defaultY)
	if err != nil {
		return resolvedPointerPoint{}, err
	}
	return resolvedPointerPoint{x: x, y: y}, nil
}

func resolvePointerPosition(screen *screen.ScreenState, x, y float64) (int, int, error) {
	return resolvePointerPositionForSurface(screen, false, x, y)
}

func resolvePointerPositionForSurface(screen *screen.ScreenState, touchscreen bool, x, y float64) (int, int, error) {
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
		return 0, 0, fmt.Errorf("coordinates must be finite")
	}
	if x < 0 || x > 1000 || y < 0 || y > 1000 {
		return 0, 0, fmt.Errorf("coordinates must use the normalized 0-1000 scale, got x=%.2f y=%.2f", x, y)
	}
	return normalizedToAbsolutePointForSurface(screen, touchscreen, x, y)
}

func normalizedToAbsolutePointForSurface(screen *screen.ScreenState, touchscreen bool, x, y float64) (int, int, error) {
	// Normalized coordinates are always interpreted within active_area:
	// 0-1000 maps to the mirrored phone touch region inside the HDMI frame.
	//
	// pointer_mode absolute:
	// Scale within active_area's own coordinate system because the absolute
	// HID cursor surface tracks the de-blackbarred phone region.
	//
	// pointer_mode touchscreen:
	// First project the normalized point into the active_area inside the HDMI
	// frame, then scale against the full frame because the touchscreen HID
	// surface covers the complete mirrored frame.
	if screen != nil {
		if width, height, active, age, ok := screen.ActiveAreaWithAge(); ok && age < screenDimensionsStaleAfter && active.Valid {
			activePixelX := (clampFloat(x, 0, 1000) / 1000.0) * float64(active.Width-1)
			activePixelY := (clampFloat(y, 0, 1000) / 1000.0) * float64(active.Height-1)
			if touchscreen {
				fullFramePixelX := float64(active.X) + activePixelX
				fullFramePixelY := float64(active.Y) + activePixelY
				absX := scalePixelToAbsolute(fullFramePixelX, width)
				absY := scalePixelToAbsolute(fullFramePixelY, height)
				if touchscreenRCADebugEnabledCached() {
					touchscreenRCALogf(
						"normalizedToAbsolute touchscreen using active_area input_norm=(%.2f,%.2f) active_pixel=(%.2f,%.2f) full_frame_pixel=(%.2f,%.2f) source=%dx%d active=%s age_ms=%d absolute=(%d,%d)",
						x,
						y,
						activePixelX,
						activePixelY,
						fullFramePixelX,
						fullFramePixelY,
						width,
						height,
						active.Format(),
						age.Milliseconds(),
						absX,
						absY,
					)
				}
				return absX, absY, nil
			}
			absX := activeLocalAxisToAbsolute(activePixelX, active.X, active.Width, width)
			absY := activeLocalAxisToAbsolute(activePixelY, active.Y, active.Height, height)
			if touchscreenRCADebugEnabledCached() {
				touchscreenRCALogf(
					"normalizedToAbsolute absolute_mouse using active_area input_norm=(%.2f,%.2f) active_pixel=(%.2f,%.2f) source=%dx%d active=%s age_ms=%d absolute=(%d,%d)",
					x,
					y,
					activePixelX,
					activePixelY,
					width,
					height,
					active.Format(),
					age.Milliseconds(),
					absX,
					absY,
				)
			}
			return absX, absY, nil
		}
	}
	absX, absY := normalizedToAbsolutePoint(x, y)
	if touchscreenRCADebugEnabledCached() {
		touchscreenRCALogf("normalizedToAbsolute fallback input_norm=(%.2f,%.2f) touchscreen=%v absolute=(%d,%d) mapping={%s}", x, y, touchscreen, absX, absY, screen.Format())
	}
	return absX, absY, nil
}

func normalizedToAbsolutePoint(x, y float64) (int, int) {
	return int(math.Round(clampFloat(x, 0, 1000) / 1000.0 * absMouseMaxPos)), int(math.Round(clampFloat(y, 0, 1000) / 1000.0 * absMouseMaxPos))
}

const nearlyFullSourceAxisRatio = 0.9

// activeLocalAxisToAbsolute maps one coordinate axis from a cropped screenshot
// to the absolute HID surface. A narrow active axis represents real HDMI
// letter/pillar-boxing and is de-barred. An active axis covering nearly the
// whole source often comes from dark UI pixels being mistaken for a small
// black bar; preserve its source offset so frame-to-frame crop jitter cancels
// out instead of shifting the touch point.
func activeLocalAxisToAbsolute(local float64, activeStart, activeSize, sourceSize int) int {
	if activeSize <= 0 || sourceSize <= 0 {
		return 0
	}
	if float64(activeSize)/float64(sourceSize) >= nearlyFullSourceAxisRatio {
		return scalePixelToAbsolute(float64(activeStart)+local, sourceSize)
	}
	return scalePixelToAbsolute(local, activeSize)
}

func scalePixelToAbsolute(value float64, size int) int {
	if size <= 1 {
		return 0
	}
	maxPixel := float64(size - 1)
	clamped := clampFloat(value, 0, maxPixel)
	return int(math.Round((clamped / maxPixel) * absMouseMaxPos))
}

func clampFloat(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func tapPointer(pc *pointerController, x, y int, button uint8) error {
	return tapPointerWithHold(pc, x, y, button, defaultTapHoldMs)
}

func tapPointerWithHold(pc *pointerController, x, y int, button uint8, holdMs int) error {
	touchscreenRCALogf("tapPointerWithHold start pointer_mode=%s absolute=(%d,%d) button=0x%02x hold_ms=%d", touchscreenRCAPointerMode(pc), x, y, button, holdMs)
	if err := settlePointer(pc, x, y); err != nil {
		return err
	}
	if err := pressPointer(pc, x, y, button); err != nil {
		return err
	}
	sleepMs(holdMs)
	err := releasePointerRepeated(pc, x, y, touchReleaseReportCount, touchReleaseReportDelayMs)
	if err != nil {
		touchscreenRCALogf("tapPointerWithHold error pointer_mode=%s absolute=(%d,%d) err=%v", touchscreenRCAPointerMode(pc), x, y, err)
		return err
	}
	touchscreenRCALogf("tapPointerWithHold completed pointer_mode=%s absolute=(%d,%d)", touchscreenRCAPointerMode(pc), x, y)
	return nil
}

func settlePointer(pc *pointerController, x, y int) error {
	if pc.touchscreen {
		return nil
	}
	if err := movePointer(pc, x, y, 0); err != nil {
		return err
	}
	sleepMs(defaultCursorSettleMs)
	return nil
}

func positionPointer(pc *pointerController, x, y int, buttons uint8) error {
	return movePointer(pc, x, y, buttons)
}

func pressPointer(pc *pointerController, x, y int, button uint8) error {
	return movePointer(pc, x, y, button)
}

func releasePointer(pc *pointerController, x, y int) error {
	return movePointer(pc, x, y, 0)
}

func releasePointerRepeated(pc *pointerController, x, y int, count int, delayMs int) error {
	if count < 1 {
		count = 1
	}

	var firstErr error
	released := false
	for i := 0; i < count; i++ {
		if err := releasePointer(pc, x, y); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			released = true
		}
		if i+1 < count {
			sleepMs(delayMs)
		}
	}
	if released {
		return nil
	}
	return firstErr
}

func movePointer(pc *pointerController, x, y int, buttons uint8) error {
	return pc.moveTo(x, y, buttons)
}

func scrollPointer(pc *pointerController, delta int) error {
	return pc.scroll(delta)
}

func runSwipeLikeGesture(pc *pointerController, start, end resolvedPointerPoint, button uint8, durationMs, holdBeforeMs, holdAfterMs, steps int) error {
	return dragPointer(pc, start, end, button, durationMs, holdBeforeMs, holdAfterMs, steps)
}

func runPositionedDragGesture(pc *pointerController, start, end resolvedPointerPoint, button uint8, durationMs, holdBeforeMs, holdAfterMs, steps int) error {
	return dragPointer(pc, start, end, button, durationMs, holdBeforeMs, holdAfterMs, steps)
}

func dragPointer(pc *pointerController, start, end resolvedPointerPoint, button uint8, durationMs, holdBeforeMs, holdAfterMs, steps int) (dragErr error) {
	if steps < 1 {
		steps = 1
	}
	if durationMs < 0 {
		durationMs = 0
	}
	if holdBeforeMs < 0 {
		holdBeforeMs = 0
	}
	if holdAfterMs < 0 {
		holdAfterMs = 0
	}

	touchscreenRCALogf(
		"dragPointer start pointer_mode=%s start_abs=(%d,%d) end_abs=(%d,%d) button=0x%02x duration_ms=%d hold_before_ms=%d hold_after_ms=%d steps=%d",
		touchscreenRCAPointerMode(pc),
		start.x,
		start.y,
		end.x,
		end.y,
		button,
		durationMs,
		holdBeforeMs,
		holdAfterMs,
		steps,
	)
	if err := settlePointer(pc, start.x, start.y); err != nil {
		return err
	}
	if err := pressPointer(pc, start.x, start.y, button); err != nil {
		return err
	}

	defer func() {
		relErr := releasePointerRepeated(pc, end.x, end.y, touchReleaseReportCount, touchReleaseReportDelayMs)
		if dragErr == nil {
			dragErr = relErr
		}
	}()

	sleepMs(holdBeforeMs)

	stepDelay := 0
	if steps > 0 {
		stepDelay = durationMs / steps
	}
	for i := 1; i <= steps; i++ {
		progress := float64(i) / float64(steps)
		x := interpolateInt(start.x, end.x, progress)
		y := interpolateInt(start.y, end.y, progress)
		if err := movePointer(pc, x, y, button); err != nil {
			return err
		}
		if i < steps {
			sleepMs(stepDelay)
		}
	}

	sleepMs(holdAfterMs)
	touchscreenRCALogf("dragPointer completed pointer_mode=%s start_abs=(%d,%d) end_abs=(%d,%d)", touchscreenRCAPointerMode(pc), start.x, start.y, end.x, end.y)
	return nil
}

func interpolateInt(start, end int, progress float64) int {
	return int(math.Round(float64(start) + (float64(end-start) * progress)))
}

var sleepMs = realSleepMs

func realSleepMs(ms int) {
	if ms <= 0 {
		return
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
}

func intOrDefault(value *int, fallback int) int {
	if value == nil {
		return fallback
	}
	return *value
}

func positiveIntOrDefault(value *int, fallback int) int {
	if value == nil || *value <= 0 {
		return fallback
	}
	return *value
}

func clampUint16(val, max int) uint16 {
	if val < 0 {
		return 0
	}
	if val > max {
		return uint16(max)
	}
	return uint16(val)
}

func clampInt(val, min, max int) int {
	if val < min {
		return min
	}
	if val > max {
		return max
	}
	return val
}

func absInt(val int) int {
	if val < 0 {
		return -val
	}
	return val
}

func mouseButtonByte(button string) uint8 {
	switch strings.ToLower(strings.TrimSpace(button)) {
	case "right":
		return 0x02
	case "middle":
		return 0x04
	default:
		return 0x01 // left
	}
}

func parseKeyboardTextInput(input string) (string, string) {
	trimmed := strings.TrimSpace(input)
	if trimmed == "" {
		return "", "text is required"
	}

	if strings.HasPrefix(trimmed, "{") {
		var args struct {
			Text string `json:"text"`
		}
		if err := json.Unmarshal([]byte(trimmed), &args); err != nil {
			return "", fmt.Sprintf("invalid input: %v", err)
		}
		if args.Text == "" {
			return "", "text is required"
		}
		return args.Text, ""
	}

	if strings.HasPrefix(trimmed, `"`) {
		var text string
		if err := json.Unmarshal([]byte(trimmed), &text); err != nil {
			return "", fmt.Sprintf("invalid input: %v", err)
		}
		if text == "" {
			return "", "text is required"
		}
		return text, ""
	}

	return trimmed, ""
}

func unsupportedKeyboardTextRunes(layout, text string) []rune {
	unsupported := make([]rune, 0)
	for _, ch := range text {
		if ch > 0x7F {
			unsupported = append(unsupported, ch)
			continue
		}
		if _, ok := keyboardLayoutKeyStroke(layout, byte(ch)); !ok {
			unsupported = append(unsupported, ch)
		}
	}
	return unsupported
}
