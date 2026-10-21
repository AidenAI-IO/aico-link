package agent

import (
	"context"
	"testing"

	"aiden-agent/internal/agent/mnk"
	"aiden-agent/internal/agent/screen"
)

type testMNKOpts struct {
	pointer            *mnk.HIDDevice
	keyboard           *mnk.HIDDevice
	android            *mnk.HIDDevice
	screenState        *screen.ScreenState
	touchscreen        bool
	layout             string
	gate               mnk.ProfileGate
	adbRunner          *recordingADBRunner
	primeScreenMapping func(context.Context) error
}

func testMNKProvider(t testing.TB, opts testMNKOpts) mnk.Provider {
	t.Helper()
	if opts.adbRunner != nil {
		t.Setenv("AIDEN_ADB_PATH", "/fake/adb")
		t.Setenv("AIDEN_ADB_SERIAL", "serial123")
		return mnk.NewADBProvider(opts.screenState, nil, opts.adbRunner.run)
	}
	return mnk.NewHIDProvider(
		asMNKDevice(opts.pointer),
		asMNKDevice(opts.keyboard),
		asMNKDevice(opts.android),
		opts.screenState,
		opts.touchscreen,
		opts.layout,
		opts.gate,
	)
}

func testKeyboardTapTool(t testing.TB, opts testMNKOpts) *KeyboardTapTool {
	t.Helper()
	if opts.keyboard == nil && opts.pointer != nil {
		opts.keyboard = opts.pointer
	}
	return &KeyboardTapTool{mnkProvider: testMNKProvider(t, opts)}
}

func testTouchGestureTool(t testing.TB, opts testMNKOpts) *TouchGestureTool {
	t.Helper()
	if opts.pointer == nil && opts.keyboard != nil {
		opts.pointer = opts.keyboard
	}
	return &TouchGestureTool{
		mnkProvider:        testMNKProvider(t, opts),
		screen:             opts.screenState,
		touchscreen:        opts.touchscreen,
		primeScreenMapping: opts.primeScreenMapping,
	}
}

func testMouseMoveTool(t testing.TB, opts testMNKOpts) *MouseMoveTool {
	t.Helper()
	return &MouseMoveTool{mnkProvider: testMNKProvider(t, opts)}
}

func testMouseScrollTool(t testing.TB, opts testMNKOpts) *MouseScrollTool {
	t.Helper()
	return &MouseScrollTool{mnkProvider: testMNKProvider(t, opts)}
}
