package agent

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"aico-agent/internal/logging"
)

const (
	defaultIOSKeyboardIsolationControl  = "/usr/lib/aico/aico-dynamic-keyboard"
	iosKeyboardProfileSwitchTimeout     = 30 * time.Second
	iosKeyboardReenumerationWakeTimeout = 2 * time.Second
	iosKeyboardDetachRetryCooldown      = 5 * time.Second
)

type iosKeyboardIsolationCommand func(context.Context, string, ...string) ([]byte, error)

type iosKeyboardIsolationBatchContextKey struct{}

// iosKeyboardIsolationBatch is one Agent run (or one direct tool call) that
// owns the keyboard: the first keyboard action attaches it, later ones reuse
// it, and the end of the scope detaches it.
type iosKeyboardIsolationBatch struct {
	controller *iosKeyboardIsolationController
	attached   bool
}

func iosKeyboardIsolationControllerFromContext(ctx context.Context) *iosKeyboardIsolationController {
	if ctx == nil {
		return nil
	}
	batch, _ := ctx.Value(iosKeyboardIsolationBatchContextKey{}).(*iosKeyboardIsolationBatch)
	if batch == nil {
		return nil
	}
	return batch.controller
}

// iosKeyboardIsolationController attaches the USB keyboard on demand for the
// iOS touchpad composite.
//
// iOS hides its software keyboard while a hardware keyboard is attached, so
// the composite idles as media keys + touchpad + ECM. The first keyboard
// action of a run attaches the keyboard (a USB re-enumeration); the run keeps
// it until it ends, on success, failure, cancellation, or preemption, and the
// final detach always runs with a context-independent timeout. Pointer and
// media-key input never switch profiles. It also serializes access to the
// three HID devices across profile switches.
type iosKeyboardIsolationController struct {
	controlPath         string
	keyboardDev         hidDeviceCloser
	pointerDev          hidDeviceCloser
	extraKeysDev        hidDeviceCloser
	run                 iosKeyboardIsolationCommand
	notifyReenumeration func(context.Context) error
	gateOnce            sync.Once
	gate                chan struct{}
	// needsDetach is set while the keyboard may still be attached outside any
	// run, after a failed detach; the next acquire retries it.
	needsDetach       bool
	lastDetachErr     error
	lastDetachFailure time.Time
}

// hidDeviceCloser is closed across USB profile switches so writers reopen FDs.
type hidDeviceCloser interface {
	Close()
}

func newIOSKeyboardIsolationController(cfg HIDConfig, keyboardDev, pointerDev, extraKeysDev hidDeviceCloser) *iosKeyboardIsolationController {
	if cfg.InputBackendADB() || cfg.PointerModeOrDefault() != "touchpad" {
		return nil
	}
	info, err := os.Stat(defaultIOSKeyboardIsolationControl)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return nil
	}
	return &iosKeyboardIsolationController{
		controlPath:  defaultIOSKeyboardIsolationControl,
		keyboardDev:  keyboardDev,
		pointerDev:   pointerDev,
		extraKeysDev: extraKeysDev,
		notifyReenumeration: func(ctx context.Context) error {
			return defaultBLEWake(ctx, "usb_reenumeration")
		},
		run: func(ctx context.Context, path string, args ...string) ([]byte, error) {
			return exec.CommandContext(ctx, path, args...).CombinedOutput()
		},
	}
}

func (c *iosKeyboardIsolationController) closeHIDDevices() {
	if c == nil {
		return
	}
	for _, dev := range []hidDeviceCloser{c.keyboardDev, c.pointerDev, c.extraKeysDev} {
		if dev != nil {
			dev.Close()
		}
	}
}

func (c *iosKeyboardIsolationController) acquire(ctx context.Context) error {
	if c == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	c.gateOnce.Do(func() {
		c.gate = make(chan struct{}, 1)
		c.gate <- struct{}{}
	})
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-c.gate:
	}
	if err := ctx.Err(); err != nil {
		c.release()
		return err
	}
	return nil
}

func (c *iosKeyboardIsolationController) release() {
	c.gate <- struct{}{}
}

// switchProfile runs "attach" or "detach". The HID file descriptors are
// closed first, because the /dev/hidgN nodes are recreated by the switch.
func (c *iosKeyboardIsolationController) switchProfile(command string) error {
	c.closeHIDDevices()
	ctx, cancel := context.WithTimeout(context.Background(), iosKeyboardProfileSwitchTimeout)
	defer cancel()
	if c.notifyReenumeration != nil {
		// Best effort: the switch must still run when BLE is unavailable. The
		// App uses this notice to ignore the short, intentional ECM gap.
		notifyCtx, notifyCancel := context.WithTimeout(context.Background(), iosKeyboardReenumerationWakeTimeout)
		_ = c.notifyReenumeration(notifyCtx)
		notifyCancel()
	}
	started := time.Now()
	output, err := c.run(ctx, c.controlPath, command)
	logging.Infof("agent", "ios_keyboard", "keyboard %s ok=%t ms=%d", command, err == nil, time.Since(started).Milliseconds())
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("switch iOS HID profile with %s failed: %w", command, err)
	}
	return fmt.Errorf("switch iOS HID profile with %s failed: %w: %s", command, err, detail)
}

func (c *iosKeyboardIsolationController) detachLocked() error {
	err := c.switchProfile("detach")
	c.needsDetach = err != nil
	c.lastDetachErr = err
	if err != nil {
		c.lastDetachFailure = time.Now()
	} else {
		c.lastDetachFailure = time.Time{}
	}
	return err
}

// ensureDetachedLocked retries a detach that failed outside a run, so the
// software keyboard comes back even if the end of a run could not detach.
func (c *iosKeyboardIsolationController) ensureDetachedLocked() error {
	if !c.needsDetach {
		return nil
	}
	if c.lastDetachErr != nil && time.Since(c.lastDetachFailure) < iosKeyboardDetachRetryCooldown {
		return c.lastDetachErr
	}
	return c.detachLocked()
}

func (c *iosKeyboardIsolationController) batchFromContext(ctx context.Context) *iosKeyboardIsolationBatch {
	if c == nil || ctx == nil {
		return nil
	}
	batch, _ := ctx.Value(iosKeyboardIsolationBatchContextKey{}).(*iosKeyboardIsolationBatch)
	if batch == nil || batch.controller != c {
		return nil
	}
	return batch
}

func (b *iosKeyboardIsolationBatch) attach() error {
	if b == nil || b.controller == nil || b.attached {
		return nil
	}
	c := b.controller
	if err := c.switchProfile("attach"); err != nil {
		// A half-done switch may have left the keyboard linked.
		return errors.Join(err, c.detachLocked())
	}
	c.needsDetach = true
	b.attached = true
	return nil
}

func (b *iosKeyboardIsolationBatch) detach() error {
	if b == nil || b.controller == nil || !b.attached {
		return nil
	}
	err := b.controller.detachLocked()
	if err == nil {
		b.attached = false
	}
	return err
}

// withBatch scopes one Agent run. Nested composite tools reuse the outer
// scope. The keyboard is attached lazily by the first keyboard action and the
// scope always detaches it at the end, including cancellation and errors.
func (c *iosKeyboardIsolationController) withBatch(ctx context.Context, action func(context.Context) error) (err error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if c == nil {
		return action(ctx)
	}
	if c.batchFromContext(ctx) != nil {
		return action(ctx)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.ensureDetachedLocked(); err != nil {
		return err
	}
	batch := &iosKeyboardIsolationBatch{controller: c}
	batchCtx := context.WithValue(ctx, iosKeyboardIsolationBatchContextKey{}, batch)
	defer func() {
		err = errors.Join(err, batch.detach())
	}()
	return action(batchCtx)
}

func withIOSKeyboardIsolationBatchCall(
	ctx context.Context,
	controller *iosKeyboardIsolationController,
	action func(context.Context) (string, error),
) (string, error) {
	if controller == nil {
		return action(ctx)
	}
	var output string
	err := controller.withBatch(ctx, func(batchCtx context.Context) error {
		var callErr error
		output, callErr = action(batchCtx)
		return callErr
	})
	return output, err
}

// withKeyboard runs a keyboard action with the keyboard attached. Every
// keyboard action needs it, with or without modifiers, so the modifier flag
// is ignored. Outside a run the action gets its own attach/detach scope.
func (c *iosKeyboardIsolationController) withKeyboard(ctx context.Context, _ bool, action func() error) error {
	if c == nil {
		return action()
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if batch := c.batchFromContext(ctx); batch != nil {
		if err := batch.attach(); err != nil {
			return err
		}
		return action()
	}
	return c.withBatch(ctx, func(batchCtx context.Context) error {
		return c.withKeyboard(batchCtx, true, action)
	})
}

func (c *iosKeyboardIsolationController) withPointerCall(ctx context.Context, action func(context.Context) (string, error)) (string, error) {
	var output string
	err := c.withPointerAction(ctx, func(callCtx context.Context) error {
		var callErr error
		output, callErr = action(callCtx)
		return callErr
	})
	return output, err
}

// withPointerAction serializes pointer input with profile switches. The
// touchpad is linked in both profiles, so the keyboard stays as it is.
func (c *iosKeyboardIsolationController) withPointerAction(ctx context.Context, action func(context.Context) error) error {
	return c.withDevices(ctx, func() error { return action(ctx) })
}

func (c *iosKeyboardIsolationController) withExtraKeys(ctx context.Context, action func() error) error {
	return c.withDevices(ctx, action)
}

func (c *iosKeyboardIsolationController) withDevices(ctx context.Context, action func() error) error {
	if c == nil {
		return action()
	}
	if c.batchFromContext(ctx) != nil {
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		return action()
	}
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	if err := c.ensureDetachedLocked(); err != nil {
		return err
	}
	return action()
}
