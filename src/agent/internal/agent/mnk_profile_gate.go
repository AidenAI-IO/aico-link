package agent

import (
	"context"

	"aico-agent/internal/agent/mnk"
)

// iosKeyboardIsolationProfileGate adapts iosKeyboardIsolationController to mnk.ProfileGate.
type iosKeyboardIsolationProfileGate struct {
	controller *iosKeyboardIsolationController
}

func newIOSKeyboardIsolationProfileGate(controller *iosKeyboardIsolationController) mnk.ProfileGate {
	if controller == nil {
		return nil
	}
	return &iosKeyboardIsolationProfileGate{controller: controller}
}

func (g *iosKeyboardIsolationProfileGate) WithKeyboard(ctx context.Context, isolate bool, fn func() error) error {
	if g == nil || g.controller == nil {
		return fn()
	}
	return g.controller.withKeyboard(ctx, isolate, fn)
}

func (g *iosKeyboardIsolationProfileGate) WithExtraKeys(ctx context.Context, fn func() error) error {
	if g == nil || g.controller == nil {
		return fn()
	}
	return g.controller.withExtraKeys(ctx, fn)
}

func (g *iosKeyboardIsolationProfileGate) WithPointer(ctx context.Context, fn func() error) error {
	if g == nil || g.controller == nil {
		return fn()
	}
	return g.controller.withPointerAction(ctx, func(context.Context) error {
		return fn()
	})
}

// asMNKDevice shares a HID device between isolation and HIDProvider. A nil
// device stays a nil interface so HIDProvider reports it as not configured.
func asMNKDevice(dev *mnk.HIDDevice) mnk.Device {
	if dev == nil {
		return nil
	}
	return dev
}
