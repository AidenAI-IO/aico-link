package mnk

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
)

type recordingHIDDevice struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (d *recordingHIDDevice) Write(b []byte) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	_, err := d.buf.Write(b)
	return err
}

func (d *recordingHIDDevice) Close() {}

func (d *recordingHIDDevice) bytes() []byte {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]byte(nil), d.buf.Bytes()...)
}

func TestAbsoluteModeAndroidExtensionUsesBitfieldReport(t *testing.T) {
	android := &recordingHIDDevice{}
	provider := NewHIDProvider(nil, nil, android, nil, false, "qwerty", nil)

	if err := provider.Keypress(context.Background(), []string{"KEYCODE_VOLUME_UP"}); err != nil {
		t.Fatalf("Keypress: %v", err)
	}
	got := android.bytes()
	if len(got) != 4 {
		t.Fatalf("wrote %d bytes, want 4", len(got))
	}
	press := uint16(got[0]) | uint16(got[1])<<8
	if want := absolutePointerModeExtensionReports["volume_up"]; press != want {
		t.Fatalf("absolute press report = 0x%04x, want bitfield 0x%04x", press, want)
	}
}

func TestTouchscreenModeAndroidExtensionUsesUsageLE(t *testing.T) {
	android := &recordingHIDDevice{}
	provider := NewHIDProvider(nil, nil, android, nil, true, "qwerty", nil)

	if err := provider.Keypress(context.Background(), []string{"volume_up"}); err != nil {
		t.Fatalf("Keypress: %v", err)
	}
	got := android.bytes()
	press := uint16(got[0]) | uint16(got[1])<<8
	if want := androidExtensionUsageMap["volume_up"]; press != want {
		t.Fatalf("touchscreen press report = 0x%04x, want usage 0x%04x", press, want)
	}
}

func TestAbsoluteModeRejectsAndroidNavigationExtensionKey(t *testing.T) {
	android := &recordingHIDDevice{}
	provider := NewHIDProvider(nil, nil, android, nil, false, "qwerty", nil)

	err := provider.Keypress(context.Background(), []string{"KEYCODE_BACK"})
	if err == nil {
		t.Fatal("expected absolute-mode rejection for KEYCODE_BACK")
	}
	msg := err.Error()
	if !strings.Contains(msg, `hid.pointer_mode="touchscreen"`) || !strings.Contains(msg, AbsolutePointerModeExtensionKeyList) {
		t.Fatalf("error = %q, want absolute allow-list message", msg)
	}
	if len(android.bytes()) != 0 {
		t.Fatalf("wrote %v on rejected key", android.bytes())
	}
}
