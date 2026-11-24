// Package usbgadget reads the state of the board's bound USB HID gadget.
package usbgadget

import (
	"bytes"
	"fmt"
	"os"
)

// PointerDescriptorPath is the report descriptor of the bound pointer function.
const PointerDescriptorPath = "/sys/kernel/config/usb_gadget/aico_hid/functions/hid.usb1/report_desc"

// PointerModeFromDescriptor maps a hid.usb1 report descriptor to its pointer
// mode: "touchscreen", "touchpad" or "absolute".
func PointerModeFromDescriptor(descriptor []byte) (string, error) {
	switch {
	case bytes.HasPrefix(descriptor, []byte{0x05, 0x0d, 0x09, 0x04}):
		return "touchscreen", nil
	case bytes.HasPrefix(descriptor, []byte{0x05, 0x0d, 0x09, 0x05}):
		return "touchpad", nil
	case bytes.HasPrefix(descriptor, []byte{0x05, 0x01, 0x09, 0x02}):
		return "absolute", nil
	default:
		return "", fmt.Errorf("unknown bound USB pointer descriptor")
	}
}

// ReadPointerMode returns the pointer mode of the gadget described at path.
// A missing file is returned as an os.IsNotExist error.
func ReadPointerMode(path string) (string, error) {
	descriptor, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return PointerModeFromDescriptor(descriptor)
}

// KeyboardOnDemand reports whether the gadget at path boots without the HID
// keyboard: in touchpad mode /dev/hidg0 exists only while the Agent types.
func KeyboardOnDemand(path string) bool {
	mode, err := ReadPointerMode(path)
	return err == nil && mode == "touchpad"
}
