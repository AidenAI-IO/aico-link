package mnk

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestHIDDeviceWriteRetriesAfterEndpointShutdown(t *testing.T) {
	first := &fakeHIDWriteCloser{writeErr: syscall.ESHUTDOWN}
	second := &fakeHIDWriteCloser{}
	openCount := 0

	dev := &HIDDevice{
		path: "fake-hid",
		open: func(string) (io.WriteCloser, error) {
			openCount++
			if openCount == 1 {
				return first, nil
			}
			return second, nil
		},
	}

	if err := dev.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if openCount != 2 {
		t.Fatalf("openCount = %d, want 2", openCount)
	}
	if !first.closed {
		t.Fatalf("expected first writer to be closed after retryable failure")
	}
	if second.writeCount != 1 {
		t.Fatalf("second writer writeCount = %d, want 1", second.writeCount)
	}
}

func TestHIDDeviceWriteReopensWhenDeviceNodeRecreated(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hidg0")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create test hid device: %v", err)
	}
	dev := NewHIDDevice(path)
	dev.refreshState = ""

	if err := dev.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("first Write returned error: %v", err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove test hid device: %v", err)
	}
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("recreate test hid device: %v", err)
	}

	if err := dev.Write([]byte{4, 5, 6}); err != nil {
		t.Fatalf("second Write returned error: %v", err)
	}
	dev.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read recreated test hid device: %v", err)
	}
	if string(data) != string([]byte{4, 5, 6}) {
		t.Fatalf("recreated device data = %v, want only second report", data)
	}
}

func TestHIDDeviceWriteReopensAfterWatchdogRefreshState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hidg0")
	statePath := filepath.Join(dir, "aiden_usb_ecm_watchdog.state")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatalf("create test hid device: %v", err)
	}
	dev := NewHIDDevice(path)
	dev.refreshState = statePath

	if err := dev.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("first Write returned error: %v", err)
	}
	if err := os.WriteFile(statePath, []byte("last_refresh_result=ok\n"), 0o600); err != nil {
		t.Fatalf("write watchdog state: %v", err)
	}
	refreshTime := dev.openedAt.Add(time.Second)
	if err := os.Chtimes(statePath, refreshTime, refreshTime); err != nil {
		t.Fatalf("set watchdog state mtime: %v", err)
	}

	if err := dev.Write([]byte{4, 5, 6}); err != nil {
		t.Fatalf("second Write returned error: %v", err)
	}
	dev.Close()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read test hid device: %v", err)
	}
	if string(data) != string([]byte{4, 5, 6}) {
		t.Fatalf("device data = %v, want second report after reopen", data)
	}
}

func TestTriggerUSBCompositeRefreshUsesConfiguredCommand(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "usb-refresh")
	argsPath := filepath.Join(dir, "args")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$AIDEN_USB_REFRESH_TEST_ARGS\"\n"
	if err := os.WriteFile(commandPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write refresh command: %v", err)
	}
	t.Setenv("AIDEN_USB_COMPOSITE_REFRESH_COMMAND", commandPath)
	t.Setenv("AIDEN_USB_REFRESH_TEST_ARGS", argsPath)

	if err := triggerUSBCompositeRefresh(); err != nil {
		t.Fatalf("triggerUSBCompositeRefresh returned error: %v", err)
	}
	args, err := os.ReadFile(argsPath)
	if err != nil {
		t.Fatalf("read refresh arguments: %v", err)
	}
	if string(args) != "refresh\n" {
		t.Fatalf("refresh arguments = %q, want %q", args, "refresh\\n")
	}
}

func TestHIDDeviceWriteReturnsNonRetryableError(t *testing.T) {
	dev := &HIDDevice{
		path: "fake-hid",
		open: func(string) (io.WriteCloser, error) {
			return &fakeHIDWriteCloser{writeErr: errors.New("permission denied")}, nil
		},
	}

	err := dev.Write([]byte{1})
	if err == nil {
		t.Fatal("expected write error")
	}
	if !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestHIDDeviceWriteRetriesFailedFDWrite(t *testing.T) {
	// unix.Write reports n == -1 alongside the errno, so a failed fd write
	// wrote nothing and must still take the reopen-and-retry path.
	brokenRead, broken, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	brokenRead.Close()
	healthyRead, healthy, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe: %v", err)
	}
	t.Cleanup(func() {
		healthyRead.Close()
		healthy.Close()
	})

	openCount := 0
	dev := NewHIDDeviceWithOpener("pipe-hid", func(string) (io.WriteCloser, error) {
		openCount++
		if openCount == 1 {
			return broken, nil
		}
		return healthy, nil
	})
	dev.refreshState = ""

	if err := dev.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if openCount != 2 {
		t.Fatalf("openCount = %d, want 2 (EPIPE, then reopen)", openCount)
	}
	got := make([]byte, 3)
	if _, err := io.ReadFull(healthyRead, got); err != nil {
		t.Fatalf("read retried report: %v", err)
	}
	if string(got) != string([]byte{1, 2, 3}) {
		t.Fatalf("retried report = %v, want [1 2 3]", got)
	}
}

func TestHIDDeviceOpenRefreshesUSBCompositeOnENXIO(t *testing.T) {
	dir := t.TempDir()
	commandPath := filepath.Join(dir, "usb-refresh")
	markerPath := filepath.Join(dir, "refreshed")
	script := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$AIDEN_USB_REFRESH_TEST_MARKER\"\n"
	if err := os.WriteFile(commandPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write refresh command: %v", err)
	}
	t.Setenv("AIDEN_USB_COMPOSITE_REFRESH_COMMAND", commandPath)
	t.Setenv("AIDEN_USB_REFRESH_TEST_MARKER", markerPath)
	setUSBCompositeRebindDelay(t, 0)

	writer := &fakeHIDWriteCloser{}
	openCount := 0
	dev := NewHIDDeviceWithOpener("enxio-hid", func(path string) (io.WriteCloser, error) {
		openCount++
		if openCount == 1 {
			return nil, &os.PathError{Op: "open", Path: path, Err: syscall.ENXIO}
		}
		return writer, nil
	})

	if err := dev.Write([]byte{1, 2, 3}); err != nil {
		t.Fatalf("Write returned error: %v", err)
	}
	if openCount != 2 {
		t.Fatalf("openCount = %d, want 2 (ENXIO, then reopen after refresh)", openCount)
	}
	if args, err := os.ReadFile(markerPath); err != nil || string(args) != "refresh\n" {
		t.Fatalf("refresh command args = %q, err = %v; want %q", args, err, "refresh\n")
	}
	if writer.writeCount != 1 {
		t.Fatalf("writeCount = %d, want 1", writer.writeCount)
	}
}

func TestHIDDeviceOpenReportsUSBCompositeRefreshFailure(t *testing.T) {
	t.Setenv("AIDEN_USB_COMPOSITE_REFRESH_COMMAND", filepath.Join(t.TempDir(), "missing-refresh"))
	setUSBCompositeRebindDelay(t, 0)

	openCount := 0
	dev := NewHIDDeviceWithOpener("enxio-hid", func(path string) (io.WriteCloser, error) {
		openCount++
		return nil, &os.PathError{Op: "open", Path: path, Err: syscall.ENXIO}
	})

	err := dev.Write([]byte{1})
	if err == nil || !strings.Contains(err.Error(), "USB composite refresh failed") || !strings.Contains(err.Error(), syscall.ENXIO.Error()) {
		t.Fatalf("Write error = %v, want refresh failure that keeps the original ENXIO", err)
	}
	if openCount != 1 {
		t.Fatalf("openCount = %d, want 1 (no reopen after failed refresh)", openCount)
	}
}

func setUSBCompositeRebindDelay(t *testing.T, delay time.Duration) {
	t.Helper()
	previous := usbCompositeRebindDelay
	usbCompositeRebindDelay = delay
	t.Cleanup(func() { usbCompositeRebindDelay = previous })
}

type fakeHIDWriteCloser struct {
	closed     bool
	writeCount int
	writeErr   error
}

func (f *fakeHIDWriteCloser) Write(p []byte) (int, error) {
	f.writeCount++
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}

func (f *fakeHIDWriteCloser) Close() error {
	f.closed = true
	return nil
}
