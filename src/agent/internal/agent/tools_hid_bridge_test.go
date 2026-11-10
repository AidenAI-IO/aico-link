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

func TestKeyboardTextPacesHoldAndGap(t *testing.T) {
	dev, w := newTimedHIDDevice()
	tool := &KeyboardTextTool{dev: dev, keyboardLayout: "qwerty"}

	if _, err := tool.Call(context.Background(), `{"text":"ab"}`); err != nil {
		t.Fatalf("Call() error = %v", err)
	}
	times := w.writeTimes()
	if len(times) != 4 {
		t.Fatalf("HID writes = %d, want 4", len(times))
	}
	if hold := times[1].Sub(times[0]); hold < keyboardTextHoldDelay {
		t.Fatalf("hold = %s, want >= %s", hold, keyboardTextHoldDelay)
	}
	if gap := times[2].Sub(times[1]); gap < keyboardTextGapDelay {
		t.Fatalf("gap = %s, want >= %s", gap, keyboardTextGapDelay)
	}
}

func TestKeyboardTextReleasesKeyWhenCancelledDuringHold(t *testing.T) {
	dev, w := newTimedHIDDevice()
	tool := &KeyboardTextTool{dev: dev, keyboardLayout: "qwerty"}
	ctx, cancel := context.WithTimeout(context.Background(), keyboardTextHoldDelay/4)
	defer cancel()

	_, _ = tool.Call(ctx, `{"text":"ab"}`)
	if got := len(w.writeTimes()); got != 2 {
		t.Fatalf("HID writes = %d, want press and release of the first key only", got)
	}
}
