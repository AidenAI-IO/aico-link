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
)

const (
	defaultIOSKeyboardIsolationControl  = "/usr/lib/aiden/aiden-dynamic-keyboard"
	iosKeyboardProfileSwitchTimeout     = 30 * time.Second
	iosKeyboardReenumerationWakeTimeout = 2 * time.Second
	iosKeyboardRestoreRetryCooldown     = 5 * time.Second
)

type iosKeyboardIsolationCommand func(context.Context, string, ...string) ([]byte, error)

type iosKeyboardIsolationBatchContextKey struct{}

type iosKeyboardIsolationBatch struct {
	controller *iosKeyboardIsolationController
	isolated   bool
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

// iosKeyboardIsolationController serializes access to the three HID devices.
// Keyboard actions whose HID reports contain modifiers temporarily use a
// pointer-free USB profile. Unmodified text, pointer input, and Consumer
// Control actions do not trigger profile switching.
type iosKeyboardIsolationController struct {
	controlPath         string
	keyboardDev         hidDeviceCloser
	pointerDev          hidDeviceCloser
	extraKeysDev        hidDeviceCloser
	run                 iosKeyboardIsolationCommand
	notifyReenumeration func(context.Context) error
	gateOnce            sync.Once
	gate                chan struct{}
	needsRestore        bool
	lastRestoreErr      error
	lastRestoreFailure  time.Time
}

// hidDeviceCloser is closed across USB profile switches so writers reopen FDs.
type hidDeviceCloser interface {
	Close()
}

func newIOSKeyboardIsolationController(cfg HIDConfig, keyboardDev, pointerDev, extraKeysDev hidDeviceCloser) *iosKeyboardIsolationController {
	if cfg.InputBackendADB() || cfg.PointerModeOrDefault() != "absolute" {
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
	if c.keyboardDev != nil {
		c.keyboardDev.Close()
	}
	if c.pointerDev != nil {
		c.pointerDev.Close()
	}
	if c.extraKeysDev != nil {
		c.extraKeysDev.Close()
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

func (c *iosKeyboardIsolationController) switchProfile(command string) error {
	ctx, cancel := context.WithTimeout(context.Background(), iosKeyboardProfileSwitchTimeout)
	defer cancel()
	if c.notifyReenumeration != nil {
		// Best effort: the profile switch must still run when BLE is unavailable.
		// The App uses this advance notice only to ignore the short, intentional
		// ECM interruption caused by rebinding the single USB gadget controller.
		notifyCtx, notifyCancel := context.WithTimeout(
			context.Background(),
			iosKeyboardReenumerationWakeTimeout,
		)
		_ = c.notifyReenumeration(notifyCtx)
		notifyCancel()
	}
	output, err := c.run(ctx, c.controlPath, command)
	if err == nil {
		return nil
	}
	detail := strings.TrimSpace(string(output))
	if detail == "" {
		return fmt.Errorf("switch iOS HID profile with %s failed: %w", command, err)
	}
	return fmt.Errorf("switch iOS HID profile with %s failed: %w: %s", command, err, detail)
}

func (c *iosKeyboardIsolationController) ensureNormalProfileLocked() error {
	if !c.needsRestore {
		return nil
	}
	if c.lastRestoreErr != nil && time.Since(c.lastRestoreFailure) < iosKeyboardRestoreRetryCooldown {
		return c.lastRestoreErr
	}
	err := c.switchProfile("restore")
	c.recordRestoreResult(err)
	return err
}

func (c *iosKeyboardIsolationController) recordRestoreResult(err error) {
	c.needsRestore = err != nil
	c.lastRestoreErr = err
	if err != nil {
		c.lastRestoreFailure = time.Now()
	} else {
		c.lastRestoreFailure = time.Time{}
	}
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

func (b *iosKeyboardIsolationBatch) isolate() error {
	if b == nil || b.controller == nil || b.isolated {
		return nil
	}
	c := b.controller
	c.closeHIDDevices()
	if isolateErr := c.switchProfile("isolate"); isolateErr != nil {
		restoreErr := c.switchProfile("restore")
		c.recordRestoreResult(restoreErr)
		return errors.Join(isolateErr, restoreErr)
	}
	c.needsRestore = true
	b.isolated = true
	return nil
}

func (b *iosKeyboardIsolationBatch) restore() error {
	if b == nil || b.controller == nil || !b.isolated {
		return nil
	}
	c := b.controller
	c.closeHIDDevices()
	err := c.switchProfile("restore")
	c.recordRestoreResult(err)
	if err == nil {
		b.isolated = false
	}
	return err
}

// restoreForPointer ends keyboard isolation in the current batch ahead of a
// pointer action, so callers can observe the screen after the USB profile
// switch instead of acting on coordinates captured before it.
func (c *iosKeyboardIsolationController) restoreForPointer(ctx context.Context) (restored bool, err error) {
	batch := c.batchFromContext(ctx)
	if batch == nil || !batch.isolated {
		return false, nil
	}
	return true, batch.restore()
}

// withBatch keeps consecutive keyboard operations in one pointer-free profile.
// Nested composite tools reuse the same batch. Pointer input restores the normal
// profile before it runs, and this outer scope always attempts a final restore
// with a context-independent timeout, including cancellation and error paths.
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
	if err := c.ensureNormalProfileLocked(); err != nil {
		return err
	}

	batch := &iosKeyboardIsolationBatch{controller: c}
	batchCtx := context.WithValue(ctx, iosKeyboardIsolationBatchContextKey{}, batch)
	defer func() {
		err = errors.Join(err, batch.restore())
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

func (c *iosKeyboardIsolationController) withKeyboard(ctx context.Context, isolate bool, action func() error) (err error) {
	if c == nil {
		return action()
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if batch := c.batchFromContext(ctx); batch != nil {
		if !isolate {
			return action()
		}
		if err := batch.isolate(); err != nil {
			return err
		}
		return action()
	}

	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return err
		}
	}
	if err := c.ensureNormalProfileLocked(); err != nil {
		return err
	}
	if !isolate {
		return action()
	}

	c.closeHIDDevices()
	if isolateErr := c.switchProfile("isolate"); isolateErr != nil {
		restoreErr := c.switchProfile("restore")
		c.recordRestoreResult(restoreErr)
		return errors.Join(isolateErr, restoreErr)
	}
	c.needsRestore = true
	defer func() {
		c.closeHIDDevices()
		restoreErr := c.switchProfile("restore")
		c.recordRestoreResult(restoreErr)
		err = errors.Join(err, restoreErr)
	}()
	return action()
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

func (c *iosKeyboardIsolationController) withPointerAction(ctx context.Context, action func(context.Context) error) error {
	if c == nil {
		return action(ctx)
	}
	if batch := c.batchFromContext(ctx); batch != nil {
		if err := batch.restore(); err != nil {
			return err
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return err
			}
		}
		return action(ctx)
	}
	if err := c.acquire(ctx); err != nil {
		return err
	}
	defer c.release()
	if err := c.ensureNormalProfileLocked(); err != nil {
		return err
	}
	return action(ctx)
}

func (c *iosKeyboardIsolationController) withExtraKeys(ctx context.Context, action func() error) error {
	if c == nil {
		return action()
	}
	if batch := c.batchFromContext(ctx); batch != nil {
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
	if err := c.ensureNormalProfileLocked(); err != nil {
		return err
	}
	return action()
}
