package usbgadget

import (
	"os"
	"path/filepath"
	"testing"
)

func TestKeyboardOnDemandOnlyForTouchpad(t *testing.T) {
	dir := t.TempDir()
	for name, tc := range map[string]struct {
		descriptor []byte
		want       bool
	}{
		"touchpad":    {[]byte{0x05, 0x0d, 0x09, 0x05, 0xa1}, true},
		"touchscreen": {[]byte{0x05, 0x0d, 0x09, 0x04, 0xa1}, false},
		"absolute":    {[]byte{0x05, 0x01, 0x09, 0x02, 0xa1}, false},
		"unknown":     {[]byte{0x05, 0x01, 0x09, 0x06}, false},
	} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, tc.descriptor, 0600); err != nil {
			t.Fatal(err)
		}
		if got := KeyboardOnDemand(path); got != tc.want {
			t.Errorf("%s: KeyboardOnDemand=%v, want %v", name, got, tc.want)
		}
	}
	if KeyboardOnDemand(filepath.Join(dir, "missing")) {
		t.Error("missing gadget must keep the keyboard required")
	}
}
