package agent

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"
)

var errIOSKeyboardDetachUsedCanceledContext = errors.New("iOS keyboard detach used canceled context")

func newTestIOSKeyboardIsolationController(events *[]string) *iosKeyboardIsolationController {
	return &iosKeyboardIsolationController{
		controlPath: "/test/control",
		run: func(_ context.Context, _ string, args ...string) ([]byte, error) {
			*events = append(*events, args[0])
			return nil, nil
		},
	}
}

func newCancellationSensitiveIOSKeyboardIsolationController(events *[]string) *iosKeyboardIsolationController {
	controller := newTestIOSKeyboardIsolationController(events)
	controller.run = func(ctx context.Context, _ string, args ...string) ([]byte, error) {
		*events = append(*events, args[0])
		if args[0] == "detach" && ctx.Err() != nil {
			return nil, errIOSKeyboardDetachUsedCanceledContext
		}
		return nil, ctx.Err()
	}
	return controller
}

func keyboardAction(events *[]string, name string) func() error {
	return func() error {
		*events = append(*events, name)
		return nil
	}
}

// The keyboard is attached on the first keyboard action of a run, reused by
// later ones (with or without modifiers), kept across pointer and media-key
// input, and detached once when the run ends. A run without keyboard input
// never re-enumerates.
func TestIOSKeyboardAttachScope(t *testing.T) {
	cases := []struct {
		name string
		run  func(c *iosKeyboardIsolationController, ctx context.Context, events *[]string) error
		want []string
	}{
		{
			name: "no keyboard input",
			run: func(c *iosKeyboardIsolationController, ctx context.Context, events *[]string) error {
				return c.withPointerAction(ctx, func(context.Context) error { *events = append(*events, "tap"); return nil })
			},
			want: []string{"tap"},
		},
		{
			name: "keyboard reused across pointer and extra keys",
			run: func(c *iosKeyboardIsolationController, ctx context.Context, events *[]string) error {
				if err := c.withKeyboard(ctx, false, keyboardAction(events, "type")); err != nil {
					return err
				}
				if err := c.withPointerAction(ctx, func(context.Context) error { *events = append(*events, "tap"); return nil }); err != nil {
					return err
				}
				if err := c.withExtraKeys(ctx, keyboardAction(events, "volume")); err != nil {
					return err
				}
				return c.withKeyboard(ctx, true, keyboardAction(events, "cmd+a"))
			},
			want: []string{"attach", "type", "tap", "volume", "cmd+a", "detach"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			c := newTestIOSKeyboardIsolationController(&events)
			err := c.withBatch(context.Background(), func(ctx context.Context) error {
				return tc.run(c, ctx, &events)
			})
			if err != nil {
				t.Fatalf("withBatch() error = %v", err)
			}
			if !reflect.DeepEqual(events, tc.want) {
				t.Fatalf("events = %v, want %v", events, tc.want)
			}
		})
	}
}

// A keyboard action outside any run gets its own attach/detach scope.
func TestIOSKeyboardAttachesForSingleCallOutsideRun(t *testing.T) {
	events := []string{}
	c := newTestIOSKeyboardIsolationController(&events)
	if err := c.withKeyboard(context.Background(), false, keyboardAction(&events, "type")); err != nil {
		t.Fatalf("withKeyboard() error = %v", err)
	}
	if want := []string{"attach", "type", "detach"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// A run that ends by error or cancellation still detaches the keyboard, and
// the detach does not use the canceled context.
func TestIOSKeyboardDetachesWhenRunEndsEarly(t *testing.T) {
	failure := errors.New("action failed")
	cases := []struct {
		name    string
		cancel  bool
		wantErr error
	}{
		{name: "action error", wantErr: failure},
		{name: "canceled", cancel: true, wantErr: context.Canceled},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			events := []string{}
			c := newCancellationSensitiveIOSKeyboardIsolationController(&events)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			err := c.withBatch(ctx, func(runCtx context.Context) error {
				if err := c.withKeyboard(runCtx, false, keyboardAction(&events, "type")); err != nil {
					return err
				}
				if tc.cancel {
					cancel()
					return runCtx.Err()
				}
				return failure
			})
			if !errors.Is(err, tc.wantErr) || errors.Is(err, errIOSKeyboardDetachUsedCanceledContext) {
				t.Fatalf("withBatch() error = %v, want %v", err, tc.wantErr)
			}
			if want := []string{"attach", "type", "detach"}; !reflect.DeepEqual(events, want) {
				t.Fatalf("events = %v, want %v", events, want)
			}
		})
	}
}

// A failed attach detaches right away, so a half-done switch never leaves the
// software keyboard hidden.
func TestIOSKeyboardDetachesWhenAttachFails(t *testing.T) {
	events := []string{}
	c := newTestIOSKeyboardIsolationController(&events)
	c.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		events = append(events, args[0])
		if args[0] == "attach" {
			return []byte("busy"), errors.New("exit 1")
		}
		return nil, nil
	}
	err := c.withBatch(context.Background(), func(ctx context.Context) error {
		return c.withKeyboard(ctx, false, keyboardAction(&events, "type"))
	})
	if err == nil {
		t.Fatal("withBatch() error = nil, want attach failure")
	}
	if want := []string{"attach", "detach"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// A detach that failed at the end of a run is retried before the next input
// once the cooldown has passed, and not before.
func TestIOSKeyboardRetriesFailedDetachAfterCooldown(t *testing.T) {
	events := []string{}
	c := newTestIOSKeyboardIsolationController(&events)
	failDetach := true
	c.run = func(_ context.Context, _ string, args ...string) ([]byte, error) {
		events = append(events, args[0])
		if args[0] == "detach" && failDetach {
			return nil, errors.New("exit 1")
		}
		return nil, nil
	}
	_ = c.withKeyboard(context.Background(), false, keyboardAction(&events, "type"))
	failDetach = false

	tap := func(context.Context) error { events = append(events, "tap"); return nil }
	if err := c.withPointerAction(context.Background(), tap); err == nil {
		t.Fatal("pointer input inside the cooldown should report the failed detach")
	}
	c.lastDetachFailure = time.Now().Add(-iosKeyboardDetachRetryCooldown - time.Second)
	if err := c.withPointerAction(context.Background(), tap); err != nil {
		t.Fatalf("withPointerAction() error = %v", err)
	}
	if want := []string{"attach", "type", "detach", "detach", "tap"}; !reflect.DeepEqual(events, want) {
		t.Fatalf("events = %v, want %v", events, want)
	}
}

// Every profile switch closes the shared HID descriptors, because the
// /dev/hidgN nodes are recreated by the re-enumeration.
func TestIOSKeyboardSwitchClosesHIDDevices(t *testing.T) {
	events := []string{}
	c := newTestIOSKeyboardIsolationController(&events)
	closes := 0
	dev := closerFunc(func() { closes++ })
	c.keyboardDev, c.pointerDev, c.extraKeysDev = dev, dev, dev
	if err := c.withKeyboard(context.Background(), false, keyboardAction(&events, "type")); err != nil {
		t.Fatalf("withKeyboard() error = %v", err)
	}
	if closes != 6 {
		t.Fatalf("Close() calls = %d, want 6 (three devices, attach and detach)", closes)
	}
}

type closerFunc func()

func (f closerFunc) Close() { f() }
