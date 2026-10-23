package agent

import (
	"context"
	"fmt"

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

// mnkDevice adapts agent HIDDevice to mnk.Device so isolation and HIDProvider share FDs.
type mnkDevice struct {
	dev *HIDDevice
}

func asMNKDevice(dev *HIDDevice) mnk.Device {
	if dev == nil {
		return nil
	}
	return mnkDevice{dev: dev}
}

func (d mnkDevice) Write(data []byte) error {
	if d.dev == nil {
		return fmt.Errorf("hid device is not configured")
	}
	return d.dev.Write(data)
}

func (d mnkDevice) Close() {
	if d.dev != nil {
		d.dev.Close()
	}
}
