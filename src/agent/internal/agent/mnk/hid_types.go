package mnk

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

const (
	// defaultHIDWriteTimeout bounds each HID report write. Linux hidg writes can
	// otherwise block forever when the USB host stops polling, which leaves the
	// last successful pressed report active and prevents swipe release reports
	// from being sent.
	defaultHIDWriteTimeout = 750 * time.Millisecond

	defaultHIDRefreshStatePath        = "/run/aico_usb_ecm_watchdog.state"
	defaultUSBCompositeRefreshCommand = "/usr/lib/aico/aico-usb-ecm-watchdog"
	usbCompositeRefreshTimeout        = 10 * time.Second
	// usbCompositeRefreshWaitDelay bounds the wait for the refresh command's
	// output after it exits or times out. The watchdog resets the gadget in a
	// subshell that outlives a killed parent and keeps the pipes open.
	usbCompositeRefreshWaitDelay = time.Second
)

// usbCompositeRebindDelay is how long ensureOpenLocked waits after a USB
// composite refresh for the gadget to rebind and reach configured state.
var usbCompositeRebindDelay = 2 * time.Second

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

// NewHIDDevice creates a lazily opened HID device for a hidg node.
func NewHIDDevice(path string) *HIDDevice {
	return NewHIDDeviceWithOpener(path, func(path string) (io.WriteCloser, error) {
		return os.OpenFile(path, os.O_WRONLY|syscall.O_NONBLOCK, 0)
	})
}

// NewHIDDeviceWithOpener creates an HID device that opens path through open
// instead of os.OpenFile, so callers can substitute an in-memory writer.
func NewHIDDeviceWithOpener(path string, open func(string) (io.WriteCloser, error)) *HIDDevice {
	return &HIDDevice{
		path:         path,
		open:         open,
		writeTimeout: defaultHIDWriteTimeout,
		refreshState: defaultHIDRefreshStatePath,
	}
}

// Path returns the device node the HID device writes to.
func (d *HIDDevice) Path() string {
	return d.path
}

// Write sends a HID report to the device.
func (d *HIDDevice) Write(data []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.writeLocked(data)
}

func (d *HIDDevice) writeLocked(data []byte) error {
	if err := d.ensureOpenLocked(); err != nil {
		return err
	}
	if err := d.reopenStaleFileLocked(); err != nil {
		return err
	}

	n, err := d.writeOnceLocked(data)
	if err != nil {
		d.closeLocked()
		// unix.Write returns n == -1 with the errno, so n <= 0 means nothing
		// was written and the report can be resent on a fresh descriptor.
		if n <= 0 && hidShouldRetryWrite(err) {
			if reopenErr := d.ensureOpenLocked(); reopenErr == nil {
				n2, retryErr := d.writeOnceLocked(data)
				if retryErr == nil && n2 == len(data) {
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
			refreshErr := triggerUSBCompositeRefresh()
			if refreshErr == nil {
				// Wait briefly for the gadget to rebind and reach configured state
				time.Sleep(usbCompositeRebindDelay)
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
	if command := strings.TrimSpace(os.Getenv("AICO_USB_COMPOSITE_REFRESH_COMMAND")); command != "" {
		return command
	}
	return defaultUSBCompositeRefreshCommand
}

func triggerUSBCompositeRefresh() error {
	// Trigger the USB watchdog's refresh command to rebind the USB composite gadget.
	// This is needed when the USB host suspends the connection or the gadget
	// enters a stale state where HID device nodes exist but are not functional.
	// Use a bounded timeout to prevent a hung watchdog from blocking HID recovery.
	ctx, cancel := context.WithTimeout(context.Background(), usbCompositeRefreshTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, usbCompositeRefreshCommand(), "refresh")
	cmd.WaitDelay = usbCompositeRefreshWaitDelay
	output, err := cmd.CombinedOutput()
	if errors.Is(err, exec.ErrWaitDelay) {
		// The command succeeded; only a child it left behind held the output open.
		err = nil
	}
	if err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return fmt.Errorf("USB composite refresh timed out after %s (output: %s)", usbCompositeRefreshTimeout, string(output))
		}
		return fmt.Errorf("USB composite refresh failed: %w (output: %s)", err, string(output))
	}
	return nil
}

func hidShouldRetryWrite(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, syscall.ESHUTDOWN) || errors.Is(err, syscall.EPIPE) ||
		errors.Is(err, syscall.ENOTCONN) || errors.Is(err, syscall.ECONNRESET) ||
		errors.Is(err, syscall.ENODEV) {
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

// PointerState tracks the last absolute pointer position written to a device.
type PointerState struct {
	mu    sync.Mutex
	x     int
	y     int
	valid bool
}

func (s *PointerState) Update(x, y int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.x = x
	s.y = y
	s.valid = true
}

func (s *PointerState) Current() (x, y int, ok bool) {
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

// HID keyboard usage codes from USB HID Usage Tables specification
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

// HID keyboard modifier bits
var hidModifierMap = map[string]uint8{
	"ctrl": 0x01, "lctrl": 0x01, "rctrl": 0x10,
	"shift": 0x02, "lshift": 0x02, "rshift": 0x20,
	"alt": 0x04, "lalt": 0x04, "ralt": 0x40,
	"meta": 0x08, "lmeta": 0x08, "rmeta": 0x80,
	"super": 0x08, "win": 0x08, "cmd": 0x08,
}

// Android extension keyboard (Consumer Control) usage codes
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
