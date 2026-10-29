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

func TestIOSKeyboardIsolationProfileGateBatchSticky(t *testing.T) {
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
	provider := mnk.NewHIDProvider(pointer, keyboard, nil, nil, false, "qwerty", gate)

	err := controller.withBatch(context.Background(), func(batchCtx context.Context) error {
		if err := provider.Keypress(batchCtx, []string{"ctrl", "a"}); err != nil {
			return err
		}
		if pointer.closeCount() == 0 {
			t.Fatalf("expected shared pointer device Close during isolate")
		}
		closesAfterFirst := pointer.closeCount()
		if err := provider.Keypress(batchCtx, []string{"ctrl", "c"}); err != nil {
			return err
		}
		if pointer.closeCount() != closesAfterFirst {
			t.Fatalf("sticky isolate should not Close again between modifier keypresses")
		}
		if want := []string{"isolate"}; !reflect.DeepEqual(events, want) {
			t.Fatalf("profile events mid-batch = %v, want %v", events, want)
		}
		if err := provider.Click(batchCtx, 100, 100, "left", 0); err != nil {
			return err
		}
		if pointer.closeCount() <= closesAfterFirst {
			t.Fatalf("expected shared pointer Close on restore before Click")
		}
		if want := []string{"isolate", "restore"}; !reflect.DeepEqual(events, want) {
			t.Fatalf("profile events after Click = %v, want %v", events, want)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("withBatch: %v", err)
	}
	if want := []string{"isolate", "restore"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("final profile events = %v, want %v (no extra restore when already normal)", events, want)
	}
}
