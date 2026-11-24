package agent

import (
	"context"
	"reflect"
	"sync"
	"testing"

	"aico-agent/internal/agent/mnk"
)

type closeCountingDevice struct {
	mu     sync.Mutex
	closes int
}

func (d *closeCountingDevice) Write([]byte) error { return nil }
func (d *closeCountingDevice) Close() {
	d.mu.Lock()
	d.closes++
	d.mu.Unlock()
}

func (d *closeCountingDevice) closeCount() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.closes
}

// Through the MNK provider, the first keypress of a run attaches the keyboard
// (closing the shared descriptors for the switch), later keypresses and taps
// reuse it, and the run's end detaches it once.
func TestIOSKeyboardIsolationProfileGateKeepsKeyboardForRun(t *testing.T) {
	events := []string{}
	pointer := &closeCountingDevice{}
	keyboard := &closeCountingDevice{}
	controller := &iosKeyboardIsolationController{
		controlPath: "/test/control",
		keyboardDev: keyboard,
		pointerDev:  pointer,
		run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			events = append(events, args[0])
			return nil, nil
		},
	}
	gate := newIOSKeyboardIsolationProfileGate(controller)
	provider := mnk.NewTouchpadHIDProvider(pointer, keyboard, nil, nil, "qwerty", gate)

	err := controller.withBatch(context.Background(), func(batchCtx context.Context) error {
		if err := provider.Keypress(batchCtx, []string{"a"}); err != nil {
			return err
		}
		if pointer.closeCount() == 0 {
			t.Fatalf("expected shared pointer device Close during attach")
		}
		closesAfterAttach := pointer.closeCount()
		if err := provider.Keypress(batchCtx, []string{"ctrl", "c"}); err != nil {
			return err
		}
		if err := provider.Click(batchCtx, 100, 100, "left", 0); err != nil {
			return err
		}
		if pointer.closeCount() != closesAfterAttach {
			t.Fatalf("keyboard and tap input must not switch profiles again mid-run")
		}
		if want := []string{"attach"}; !reflect.DeepEqual(events, want) {
			t.Fatalf("profile events mid-run = %v, want %v", events, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withBatch: %v", err)
	}
	if want := []string{"attach", "detach"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("final profile events = %v, want %v", events, want)
	}
}
