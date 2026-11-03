package agent

import (
	"aico-agent/internal/agent/mnk"
	"context"
	"io"
	"testing"
)

func TestKeyboardTextWithoutIOSIsolationUsesConfiguredHID(t *testing.T) {
	writer := &fakeHIDWriteCloser{}
	dev := mnk.NewHIDDeviceWithOpener("bridge-hid", func(string) (io.WriteCloser, error) {
		return writer, nil
	})
	tool := &KeyboardTextTool{dev: dev, keyboardLayout: "qwerty"}

	out, err := tool.Call(context.Background(), `{"text":"A"}`)
	if err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	if out != "ok" {
		t.Fatalf("Call() output = %q, want ok", out)
	}
	if writer.writeCount != 2 {
		t.Fatalf("HID writes = %d, want press and release", writer.writeCount)
	}
}
