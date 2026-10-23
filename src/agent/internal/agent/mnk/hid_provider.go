package mnk

import (
	"aico-agent/internal/agent/screen"
	"aico-agent/internal/logging"
	"context"
	"encoding/binary"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"
)

// HIDProvider implements Provider interface for USB HID device control.
// This bridges the MNK interface to the existing HID implementation in tools_hid.go.
type HIDProvider struct {
	// pointerDev is the HID device for pointer (mouse/touchscreen)
	pointerDev Device

	// pointerState tracks current pointer position
	pointerState *pointerState

	// touchscreen indicates if we're in touchscreen mode vs absolute mouse mode
	touchscreen bool

	// keyboardDev is the HID device for standard boot keyboard
	keyboardDev Device

	// androidKeyboardDev is the Android extension keyboard device (Consumer Control)
	androidKeyboardDev Device

	// screenState provides coordinate resolution with active_area awareness
	screenState *screen.ScreenState

	// keyboardLayout for text input (qwerty/azerty/qwertz)
	keyboardLayout string

	// gate optionally wraps keyboard/pointer writes for iOS absolute isolation.
	gate ProfileGate

	// dragMu protects the contact intentionally held across drag_start and
	// drag_release tool calls.
	dragMu     sync.Mutex
	dragActive bool
	dragButton string
	dragX      int
	dragY      int

	// Timing constants (internal, not exposed to callers)
	tapHoldMs              int // Default tap hold duration (60ms for iOS)
	swipeSteps             int // Interpolation steps (24)
	cursorSettleMs         int // Cursor settle delay for absolute mode (80ms)
	releaseRepeatCount     int // Touch release repetition (3)
	releaseRepeatDelayMs   int // Delay between release repeats (15ms)
	keyboardTapHoldMs      int // Default keyboard tap hold (50ms)
	keyboardModifierHoldMs int // Keyboard modifier hold (120ms)
}

// Default timing values based on iOS/Android HID requirements
const (
	defaultTapHoldMs                = 60               // iOS drops faster events
	defaultSwipeSteps               = 24               // Smooth interpolation
	defaultSwipeGestureHoldBeforeMs = 0                // Start moving immediately to avoid long-press recognition
	defaultCursorSettleMs           = 80               // iOS cursor animation
	defaultReleaseRepeatCount       = 3                // USB polling workaround
	defaultReleaseRepeatDelayMs     = 15               // Delay between releases
	defaultDoubleClickPauseMs       = 100              // Pause between double-click taps
	defaultKeyboardTapHoldMs        = 50               // Standard key hold
	defaultKeyboardModifierHoldMs   = 120              // Modifier chord hold
	absMouseMaxPos                  = 32767            // HID absolute coordinate max
	screenDimensionsStaleAfter      = 30 * time.Second // Max age for cached screen dimensions
)

// NewHIDProvider creates a new HID-based MNK provider.
// Devices should be shared with any ProfileGate that closes FDs across USB profile switches.
func NewHIDProvider(pointerDev, keyboardDev, androidKeyboardDev Device, screenState *screen.ScreenState, touchscreen bool, keyboardLayout string, gate ProfileGate) *HIDProvider {
	if keyboardLayout == "" {
		keyboardLayout = "qwerty"
	}

	return &HIDProvider{
		pointerDev:             pointerDev,
		pointerState:           &pointerState{},
		touchscreen:            touchscreen,
		keyboardDev:            keyboardDev,
		androidKeyboardDev:     androidKeyboardDev,
		screenState:            screenState,
		keyboardLayout:         keyboardLayout,
		gate:                   gate,
		tapHoldMs:              defaultTapHoldMs,
		swipeSteps:             defaultSwipeSteps,
		cursorSettleMs:         defaultCursorSettleMs,
		releaseRepeatCount:     defaultReleaseRepeatCount,
		releaseRepeatDelayMs:   defaultReleaseRepeatDelayMs,
		keyboardTapHoldMs:      defaultKeyboardTapHoldMs,
		keyboardModifierHoldMs: defaultKeyboardModifierHoldMs,
	}
}

// Click performs a press-hold-release at the specified position.
func (p *HIDProvider) Click(ctx context.Context, x, y float64, button string, holdMs int) error {
	if err := p.rejectActiveDrag("click"); err != nil {
		return err
	}
	return runPointerGate(p.gate, ctx, func() error {
		return p.clickLocked(x, y, button, holdMs)
	})
}

func (p *HIDProvider) clickLocked(x, y float64, button string, holdMs int) error {
	if err := p.validateCoordinate(x, y); err != nil {
		return err
	}

	absX, absY, err := p.normalizedToAbsolute(x, y)
	if err != nil {
		return err
	}

	buttonByte := p.mouseButtonByte(button)

	// Use caller-provided holdMs if specified, otherwise use default
	if holdMs <= 0 {
		holdMs = p.tapHoldMs
	}

	// Settle cursor if in absolute mouse mode (not needed for touchscreen)
	if !p.touchscreen {
		if err := p.settlePointer(absX, absY); err != nil {
			return err
		}
	}

	// Press
	if err := p.pressPointer(absX, absY, buttonByte); err != nil {
		return err
	}

	// Hold
	time.Sleep(time.Duration(holdMs) * time.Millisecond)

	// Release with repetition for touchscreen mode
	return p.releasePointerRepeated(absX, absY)
}

// DoubleClick performs two clicks in rapid succession.
func (p *HIDProvider) DoubleClick(ctx context.Context, x, y float64, button string) error {
	if err := p.rejectActiveDrag("double_click"); err != nil {
		return err
	}
	return runPointerGate(p.gate, ctx, func() error {
		if err := p.clickLocked(x, y, button, 0); err != nil {
			return err
		}
		time.Sleep(time.Duration(defaultDoubleClickPauseMs) * time.Millisecond)
		return p.clickLocked(x, y, button, 0)
	})
}

// Swipe performs a short gesture without the pre-movement dwell used by Drag.
func (p *HIDProvider) Swipe(ctx context.Context, path [][2]float64, button string) error {
	return p.SwipeWithOptions(ctx, path, button, SwipeOptions{})
}

// SwipeWithDuration performs a swipe using the caller-selected motion time.
func (p *HIDProvider) SwipeWithDuration(ctx context.Context, path [][2]float64, button string, durationMs int) error {
	return p.SwipeWithOptions(ctx, path, button, SwipeOptions{DurationMs: durationMs})
}

// SwipeWithOptions performs a swipe with caller-selected timing and HID
// interpolation settings.
func (p *HIDProvider) SwipeWithOptions(ctx context.Context, path [][2]float64, button string, options SwipeOptions) error {
	if err := p.rejectActiveDrag("swipe"); err != nil {
		return err
	}
	return runPointerGate(p.gate, ctx, func() error {
		return p.swipeLockedWithOptions(ctx, path, button, options)
	})
}

// DragStart presses, holds for long-press recognition, performs a bounded
// 200-unit activation move at 500 normalized units/second, and deliberately
// leaves the contact down.
func (p *HIDProvider) DragStart(ctx context.Context, x, y float64, button string) error {
	p.dragMu.Lock()
	defer p.dragMu.Unlock()
	if p.dragActive {
		return InvalidArguments("drag_start is already active; call drag_release before starting another drag")
	}
	if err := p.validateCoordinate(x, y); err != nil {
		return err
	}
	start := Point{X: x, Y: y}
	activation := dragActivationPoint(start)
	startX, startY, err := p.normalizedToAbsolute(start.X, start.Y)
	if err != nil {
		return err
	}
	activationX, activationY, err := p.normalizedToAbsolute(activation.X, activation.Y)
	if err != nil {
		return err
	}
	if button == "" {
		button = ButtonLeft
	}

	err = runPointerGate(p.gate, ctx, func() error {
		if !p.touchscreen {
			if settleErr := p.settlePointer(startX, startY); settleErr != nil {
				return settleErr
			}
		}
		buttonByte := p.mouseButtonByte(button)
		if pressErr := p.pressPointer(startX, startY, buttonByte); pressErr != nil {
			return pressErr
		}
		currentX, currentY := startX, startY
		fail := func(cause error) error {
			if releaseErr := p.releasePointerRepeated(currentX, currentY); releaseErr != nil {
				p.dragActive = true
				p.dragButton = button
				p.dragX = currentX
				p.dragY = currentY
				return fmt.Errorf("%w; drag_start cleanup release failed: %v", cause, releaseErr)
			}
			return cause
		}
		if waitErr := waitForContext(ctx, dragStartHoldMs*time.Millisecond); waitErr != nil {
			return fail(waitErr)
		}
		var moveErr error
		currentX, currentY, moveErr = p.movePointerInterpolated(ctx, startX, startY, activationX, activationY, buttonByte, dragStartMoveDurationMs)
		if moveErr != nil {
			return fail(moveErr)
		}
		p.dragActive = true
		p.dragButton = button
		p.dragX = activationX
		p.dragY = activationY
		return nil
	})
	if err != nil && p.dragActive {
		if releaseErr := p.releasePointerRepeated(p.dragX, p.dragY); releaseErr == nil {
			p.clearDragLocked()
		} else {
			err = fmt.Errorf("%w; drag_start cleanup release failed: %v", err, releaseErr)
		}
	}
	return err
}

// DragRelease completes the active drag with one direct move, a 200ms dwell,
// and a repeated HID release sequence.
func (p *HIDProvider) DragRelease(ctx context.Context, x, y float64) error {
	p.dragMu.Lock()
	defer p.dragMu.Unlock()
	if !p.dragActive {
		return InvalidArguments("drag_release requires an active drag_start")
	}
	if err := p.validateCoordinate(x, y); err != nil {
		return err
	}
	targetX, targetY, err := p.normalizedToAbsolute(x, y)
	if err != nil {
		return err
	}

	return runPointerGate(p.gate, ctx, func() error {
		buttonByte := p.mouseButtonByte(p.dragButton)
		if moveErr := p.movePointer(targetX, targetY, buttonByte); moveErr != nil {
			if releaseErr := p.releasePointerRepeated(p.dragX, p.dragY); releaseErr == nil {
				p.clearDragLocked()
			} else {
				return fmt.Errorf("%w; drag_release cleanup release failed: %v", moveErr, releaseErr)
			}
			return moveErr
		}
		p.dragX, p.dragY = targetX, targetY
		waitErr := waitForContext(ctx, dragReleaseHoldMs*time.Millisecond)
		releaseErr := p.releasePointerRepeated(targetX, targetY)
		if releaseErr == nil {
			p.clearDragLocked()
		}
		if waitErr != nil {
			if releaseErr != nil {
				return fmt.Errorf("%w; drag_release release failed: %v", waitErr, releaseErr)
			}
			return waitErr
		}
		return releaseErr
	})
}

func (p *HIDProvider) rejectActiveDrag(operation string) error {
	p.dragMu.Lock()
	defer p.dragMu.Unlock()
	if p.dragActive {
		return InvalidArgumentsf("drag_start is active; call drag_release before %s", operation)
	}
	return nil
}

func (p *HIDProvider) clearDragLocked() {
	p.dragActive = false
	p.dragButton = ""
	p.dragX = 0
	p.dragY = 0
}

// TouchActions executes a sequence of low-level pointer primitives without
// releasing the contact between actions. This is the primitive path used by
// the atomic touch_gesture syntax; callers control movement and dwell timing
// explicitly with touch_down, move_to, wait, and touch_up actions.
func (p *HIDProvider) TouchActions(ctx context.Context, actions []TouchAction) error {
	if err := p.rejectActiveDrag("atomic touch actions"); err != nil {
		return err
	}
	if err := validateTouchActionLimits(actions); err != nil {
		return err
	}
	actions = withTouchReleaseTails(actions)
	if err := validateTouchActionLimits(actions); err != nil {
		return InvalidArgumentsf("profiled touch program exceeds limits: %v", err)
	}
	return runPointerGate(p.gate, ctx, func() error {
		active := false
		activeButton := ButtonLeft
		currentX, currentY := p.getCurrentPosition()
		releaseOnError := func(err error) error {
			if active {
				_ = p.releasePointerRepeated(currentX, currentY)
				active = false
			}
			return err
		}
		for index, action := range actions {
			if err := ctx.Err(); err != nil {
				return releaseOnError(err)
			}
			actionType := strings.ToLower(strings.TrimSpace(action.Type))
			button := action.Button
			if button == "" {
				button = activeButton
			}

			switch actionType {
			case "wait":
				if err := waitForContext(ctx, time.Duration(action.DurationMs)*time.Millisecond); err != nil {
					return releaseOnError(err)
				}

			case "move_to":
				if action.Point == nil {
					return releaseOnError(InvalidArgumentsf("touch action %d move_to requires point", index))
				}
				absX, absY, err := p.normalizedToAbsolute(action.Point.X, action.Point.Y)
				if err != nil {
					return releaseOnError(InvalidArgumentsf("touch action %d: %v", index, err))
				}
				buttons := uint8(0)
				if active {
					buttons = p.mouseButtonByte(activeButton)
				}
				var moveErr error
				// Atomic programs that keep a contact down are still continuous
				// finger gestures. Apply the same controlled acceleration and
				// braking as the standard swipe path when they specify a duration.
				smooth := active && action.DurationMs > 0 && !action.releaseTail && !action.linear
				currentX, currentY, moveErr = p.movePointerInterpolatedWithProfile(ctx, currentX, currentY, absX, absY, buttons, action.DurationMs, smooth)
				if moveErr != nil {
					return releaseOnError(moveErr)
				}

			case "touch_down":
				if active {
					return releaseOnError(InvalidArgumentsf("touch action %d touch_down while a contact is already active", index))
				}
				absX, absY := currentX, currentY
				if action.Point != nil {
					var err error
					absX, absY, err = p.normalizedToAbsolute(action.Point.X, action.Point.Y)
					if err != nil {
						return releaseOnError(InvalidArgumentsf("touch action %d: %v", index, err))
					}
				}
				if !p.touchscreen {
					if err := p.settlePointer(absX, absY); err != nil {
						return releaseOnError(err)
					}
				}
				if err := p.pressPointer(absX, absY, p.mouseButtonByte(button)); err != nil {
					return releaseOnError(err)
				}
				currentX, currentY = absX, absY
				activeButton = button
				active = true

			case "touch_up":
				if !active {
					return releaseOnError(InvalidArgumentsf("touch action %d touch_up without an active contact", index))
				}
				if action.Point != nil {
					absX, absY, err := p.normalizedToAbsolute(action.Point.X, action.Point.Y)
					if err != nil {
						return releaseOnError(InvalidArgumentsf("touch action %d: %v", index, err))
					}
					if err := p.movePointer(absX, absY, p.mouseButtonByte(activeButton)); err != nil {
						return releaseOnError(err)
					}
					currentX, currentY = absX, absY
				}
				if err := p.releasePointerRepeated(currentX, currentY); err != nil {
					return releaseOnError(err)
				}
				active = false

			default:
				return releaseOnError(InvalidArgumentsf("touch action %d has unsupported action %q", index, action.Type))
			}
		}

		if active {
			return releaseOnError(InvalidArguments("touch action sequence ended with an active contact; add touch_up"))
		}
		return nil
	})
}

func (p *HIDProvider) movePointerInterpolated(ctx context.Context, fromX, fromY, toX, toY int, buttons uint8, durationMs int) (int, int, error) {
	return p.movePointerInterpolatedWithProfile(ctx, fromX, fromY, toX, toY, buttons, durationMs, false)
}

func (p *HIDProvider) movePointerInterpolatedWithProfile(ctx context.Context, fromX, fromY, toX, toY int, buttons uint8, durationMs int, smooth bool) (int, int, error) {
	lastX, lastY := fromX, fromY
	if durationMs <= 0 || (fromX == toX && fromY == toY) {
		if err := p.movePointer(toX, toY, buttons); err != nil {
			return lastX, lastY, err
		}
		return toX, toY, nil
	}
	steps := p.swipeSteps
	if steps < 1 {
		steps = 1
	}
	distance := math.Sqrt(float64((toX-fromX)*(toX-fromX) + (toY-fromY)*(toY-fromY)))
	if distance < float64(steps) {
		steps = int(math.Max(1, math.Round(distance)))
	}
	if smooth {
		return p.moveAlongPathWithSteps(ctx, [][2]int{{fromX, fromY}, {toX, toY}}, buttons, durationMs, steps, true)
	}
	// Emit the first move immediately, then span the full requested duration
	// across the remaining interpolation reports.
	intervals := steps - 1
	if intervals < 1 {
		intervals = 1
	}
	stepDelay := time.Duration(durationMs) * time.Millisecond / time.Duration(intervals)
	for step := 1; step <= steps; step++ {
		if err := ctx.Err(); err != nil {
			return lastX, lastY, err
		}
		if (step > 1 || steps == 1) && stepDelay > 0 {
			if err := waitForContext(ctx, stepDelay); err != nil {
				return lastX, lastY, err
			}
		}
		progress := float64(step) / float64(steps)
		x := int(math.Round(float64(fromX) + float64(toX-fromX)*progress))
		y := int(math.Round(float64(fromY) + float64(toY-fromY)*progress))
		if err := p.movePointer(x, y, buttons); err != nil {
			return lastX, lastY, err
		}
		lastX, lastY = x, y
	}
	return lastX, lastY, nil
}

func waitForContext(ctx context.Context, duration time.Duration) error {
	if duration <= 0 {
		return ctx.Err()
	}
	timer := time.NewTimer(duration)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (p *HIDProvider) swipeLockedWithOptions(ctx context.Context, path [][2]float64, button string, options SwipeOptions) error {
	if len(path) < 2 {
		return InvalidArgumentsf("swipe path must contain at least 2 points, got %d", len(path))
	}
	if options.DurationMs <= 0 {
		options.DurationMs = defaultSwipeGestureDurationMs
	}
	if options.HoldBeforeMs == 0 {
		options.HoldBeforeMs = defaultSwipeGestureHoldBeforeMs
	}
	if options.HoldBeforeMs < 0 {
		options.HoldBeforeMs = 0
	}
	if options.HoldAfterMs < 0 {
		options.HoldAfterMs = 0
	}
	if options.Steps <= 0 {
		options.Steps = p.swipeSteps
	}
	if options.Steps <= 0 {
		options.Steps = defaultSwipeSteps
	}

	// Validate all points first
	for i, point := range path {
		if err := p.validateCoordinate(point[0], point[1]); err != nil {
			return InvalidArgumentsf("invalid point %d in path: %v", i, err)
		}
	}

	// Convert path to absolute coordinates
	absPath := make([][2]int, len(path))
	for i, point := range path {
		absX, absY, err := p.normalizedToAbsolute(point[0], point[1])
		if err != nil {
			return InvalidArgumentsf("failed to convert point %d: %v", i, err)
		}
		absPath[i] = [2]int{absX, absY}
	}
	if pathLength(absPath) == 0 {
		return InvalidArguments("swipe requires distinct start and end points")
	}

	buttonByte := p.mouseButtonByte(button)

	// Settle cursor at start position
	if !p.touchscreen {
		if err := p.settlePointer(absPath[0][0], absPath[0][1]); err != nil {
			return err
		}
	}

	// Press at start
	if err := p.pressPointer(absPath[0][0], absPath[0][1], buttonByte); err != nil {
		return err
	}

	// iOS AssistiveTouch retains fling velocity through an unchanged-coordinate
	// hold. Reserve a tiny part of the path for real low-speed motion instead.
	// Use the same trajectory for mouse and touchscreen reports; preserve
	// explicit end holds and edge gestures.
	start := path[0]
	controlledRelease := controlledReleaseAllowed(Point{start[0], start[1]}, options.HoldAfterMs)
	endpoint := absPath[len(absPath)-1]
	if controlledRelease {
		// Skip duplicate trailing points when locating the final moving segment.
		last := len(path) - 1
		previous := last - 1
		for previous > 0 && path[previous] == path[last] {
			previous--
		}
		tailStart := releaseTailStart(Point{path[previous][0], path[previous][1]}, Point{path[last][0], path[last][1]})
		x, y, convertErr := p.normalizedToAbsolute(tailStart.X, tailStart.Y)
		if convertErr != nil {
			_ = p.releasePointerRepeated(absPath[0][0], absPath[0][1])
			return convertErr
		}
		absPath = absPath[:previous+2]
		absPath[len(absPath)-1] = [2]int{x, y}
	}
	if err := waitForContext(ctx, time.Duration(options.HoldBeforeMs)*time.Millisecond); err != nil {
		_ = p.releasePointerRepeated(absPath[0][0], absPath[0][1])
		return err
	}
	motionDurationMs := options.DurationMs
	if controlledRelease && motionDurationMs < defaultSwipeSmoothMotionMinMs {
		motionDurationMs = defaultSwipeSmoothMotionMinMs
	}
	motionStarted := time.Now()
	x, y, err := p.moveAlongPathWithSteps(ctx, absPath, buttonByte, motionDurationMs, options.Steps, controlledRelease)
	if err != nil {
		_ = p.releasePointerRepeated(x, y)
		return err
	}
	motionFinished := time.Now()
	if controlledRelease {
		x, y, err = p.movePointerInterpolated(ctx, x, y, endpoint[0], endpoint[1], buttonByte, defaultSwipeReleaseTailMs)
	} else {
		err = waitForContext(ctx, time.Duration(options.HoldAfterMs)*time.Millisecond)
	}
	releaseStarted := time.Now()
	// Cancellation/write failure must release at the last successful position.
	releaseErr := p.releasePointerRepeated(x, y)
	logging.Infof("agent", "mnk", "swipe_release controlled=%t touchscreen=%t start_norm=(%.1f,%.1f) end_norm=(%.1f,%.1f) requested_motion_ms=%d motion_ms=%.1f tail_or_hold_ms=%.1f tail_error=%v release_error=%v",
		controlledRelease, p.touchscreen, start[0], start[1], path[len(path)-1][0], path[len(path)-1][1], options.DurationMs,
		float64(motionFinished.Sub(motionStarted))/float64(time.Millisecond), float64(releaseStarted.Sub(motionFinished))/float64(time.Millisecond), err, releaseErr)
	if err != nil {
		return err
	}
	return releaseErr
}

func (p *HIDProvider) moveAlongPathWithSteps(ctx context.Context, absPath [][2]int, buttonByte uint8, durationMs, steps int, smoothRelease bool) (int, int, error) {
	lastX, lastY := absPath[0][0], absPath[0][1]
	// Calculate total path length for timing distribution
	totalLength := pathLength(absPath)
	if totalLength == 0 {
		return lastX, lastY, ctx.Err()
	}
	motionStarted := time.Now()
	motionDuration := time.Duration(durationMs) * time.Millisecond
	completedLength := 0.0

	// Distribute steps proportionally across segments
	for i := 1; i < len(absPath); i++ {
		start := absPath[i-1]
		end := absPath[i]

		dx := float64(end[0] - start[0])
		dy := float64(end[1] - start[1])
		segmentLength := math.Sqrt(dx*dx + dy*dy)

		// Proportional step count and duration for this segment
		segmentSteps := int(math.Round(float64(steps) * segmentLength / totalLength))
		if segmentSteps < 1 {
			segmentSteps = 1
		}
		segmentDurationMs := int(math.Round(float64(durationMs) * segmentLength / totalLength))

		// Interpolate along this segment
		stepDelayMs := 0
		if segmentSteps > 0 {
			stepDelayMs = segmentDurationMs / segmentSteps
		}

		for step := 1; step <= segmentSteps; step++ {
			if err := ctx.Err(); err != nil {
				return lastX, lastY, err
			}
			progress := float64(step) / float64(segmentSteps)
			if smoothRelease {
				// Schedule every movement interval against one clock, including
				// the first step and segment boundaries, without rounding to ms.
				fraction := (completedLength + segmentLength*progress) / totalLength
				due := time.Duration(float64(motionDuration) * fraction)
				if i == len(absPath)-1 && step == segmentSteps {
					due = motionDuration
				}
				if err := waitForContext(ctx, time.Until(motionStarted.Add(due))); err != nil {
					return lastX, lastY, err
				}
			}
			if smoothRelease && i == len(absPath)-1 {
				// Quintic smoothstep: gently accelerate and brake; zero velocity
				// and acceleration at the ends of the main movement.
				progress = quinticSmoothStep(progress)
			}
			x := start[0] + int(math.Round(dx*progress))
			y := start[1] + int(math.Round(dy*progress))

			if err := p.movePointer(x, y, buttonByte); err != nil {
				return lastX, lastY, err
			}
			lastX, lastY = x, y

			if !smoothRelease && step < segmentSteps && stepDelayMs > 0 {
				if err := waitForContext(ctx, time.Duration(stepDelayMs)*time.Millisecond); err != nil {
					return lastX, lastY, err
				}
			}
		}
		completedLength += segmentLength
	}

	return lastX, lastY, nil
}

func pathLength(path [][2]int) float64 {
	total := 0.0
	for i := 1; i < len(path); i++ {
		dx := float64(path[i][0] - path[i-1][0])
		dy := float64(path[i][1] - path[i-1][1])
		total += math.Sqrt(dx*dx + dy*dy)
	}
	return total
}

// Keypress sends one or more keys simultaneously.
func (p *HIDProvider) Keypress(ctx context.Context, keys []string) error {
	if len(keys) == 0 {
		return InvalidArguments("keypress requires at least one key")
	}

	// Resolve keys into modifiers and key codes
	resolved, err := p.resolveKeys(keys)
	if err != nil {
		return err
	}

	// Check if this is an Android extension key
	if resolved.androidExtensionKey != "" {
		return runExtraKeysGate(p.gate, ctx, func() error {
			return p.tapAndroidExtension(resolved.androidExtensionKey, resolved.androidUsage)
		})
	}

	isolate := resolved.modifier != 0
	return runKeyboardGate(p.gate, ctx, isolate, func() error {
		return p.tapKeyboardChord(resolved.modifier, resolved.keys)
	})
}

// Move positions the pointer without pressing any button.
func (p *HIDProvider) Move(ctx context.Context, x, y float64) error {
	if err := p.rejectActiveDrag("mouse_move"); err != nil {
		return err
	}
	return runPointerGate(p.gate, ctx, func() error {
		if err := p.validateCoordinate(x, y); err != nil {
			return err
		}

		if p.touchscreen {
			return InvalidArguments("move is unsupported in touchscreen mode (no hover capability)")
		}

		absX, absY, err := p.normalizedToAbsolute(x, y)
		if err != nil {
			return err
		}

		return p.movePointer(absX, absY, 0)
	})
}

// Scroll sends wheel/scroll input.
func (p *HIDProvider) Scroll(ctx context.Context, scrollX, scrollY int) error {
	if err := p.rejectActiveDrag("mouse_scroll"); err != nil {
		return err
	}
	if scrollX != 0 {
		return InvalidArguments("horizontal scroll is unsupported by the absolute mouse HID descriptor; use scroll_y")
	}
	if p.touchscreen {
		return InvalidArguments("mouse_scroll is unsupported when pointer_mode is touchscreen; use touch_gesture")
	}

	return runPointerGate(p.gate, ctx, func() error {
		// Absolute mouse mode: send wheel events
		if scrollY != 0 {
			if err := p.scrollPointer(scrollY); err != nil {
				return err
			}
		}

		return nil
	})
}

// ============================================================================
// Low-level HID primitives (delegating to existing tools_hid.go implementations)
// ============================================================================

// ============================================================================
// Coordinate conversion and validation
// ============================================================================

func (p *HIDProvider) validateCoordinate(x, y float64) error {
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
		return InvalidArguments("coordinates must be finite")
	}
	if x < 0 || x > 1000 || y < 0 || y > 1000 {
		return InvalidArgumentsf("coordinates must use the normalized 0-1000 scale, got x=%.2f y=%.2f", x, y)
	}
	return nil
}

func (p *HIDProvider) normalizedToAbsolute(x, y float64) (int, int, error) {
	// Uses the same logic as tools_hid.go:normalizedToAbsolutePointForSurface
	// Handles active_area for de-blackbarring and touchscreen vs absolute mode
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
		return 0, 0, InvalidArguments("coordinates must be finite")
	}
	if x < 0 || x > 1000 || y < 0 || y > 1000 {
		return 0, 0, InvalidArgumentsf("coordinates must use the normalized 0-1000 scale, got x=%.2f y=%.2f", x, y)
	}

	// Try to use active_area from screen state
	if p.screenState != nil {
		if width, height, active, age, ok := p.screenState.ActiveAreaWithAge(); ok && age < screenDimensionsStaleAfter && active.Valid {
			activePixelX := (clampFloat(x, 0, 1000) / 1000.0) * float64(active.Width-1)
			activePixelY := (clampFloat(y, 0, 1000) / 1000.0) * float64(active.Height-1)

			if p.touchscreen {
				// Touchscreen mode: project to active_area, then scale against full frame
				fullFramePixelX := float64(active.X) + activePixelX
				fullFramePixelY := float64(active.Y) + activePixelY
				absX := scalePixelToAbsolute(fullFramePixelX, width)
				absY := scalePixelToAbsolute(fullFramePixelY, height)
				return absX, absY, nil
			}

			// Absolute mouse mode: scale within active_area coordinate system
			absX := activeLocalAxisToAbsolute(activePixelX, active.X, active.Width, width)
			absY := activeLocalAxisToAbsolute(activePixelY, active.Y, active.Height, height)
			return absX, absY, nil
		}
	}

	// Fallback: simple linear mapping
	absX := int(math.Round(clampFloat(x, 0, 1000) / 1000.0 * absMouseMaxPos))
	absY := int(math.Round(clampFloat(y, 0, 1000) / 1000.0 * absMouseMaxPos))
	return absX, absY, nil
}

const nearlyFullSourceAxisRatio = 0.9

func activeLocalAxisToAbsolute(local float64, activeStart, activeSize, sourceSize int) int {
	if activeSize <= 0 || sourceSize <= 0 {
		return 0
	}
	// If active area covers nearly the whole source, preserve offset to cancel jitter
	if float64(activeSize)/float64(sourceSize) >= nearlyFullSourceAxisRatio {
		return scalePixelToAbsolute(float64(activeStart)+local, sourceSize)
	}
	return scalePixelToAbsolute(local, activeSize)
}

func scalePixelToAbsolute(value float64, size int) int {
	if size <= 1 {
		return 0
	}
	maxPixel := float64(size - 1)
	clamped := clampFloat(value, 0, maxPixel)
	return int(math.Round((clamped / maxPixel) * absMouseMaxPos))
}

func clampFloat(value, min, max float64) float64 {
	if value < min {
		return min
	}
	if value > max {
		return max
	}
	return value
}

func (p *HIDProvider) mouseButtonByte(button string) uint8 {
	normalized := strings.ToLower(strings.TrimSpace(button))
	switch normalized {
	case "right":
		return 0x02
	case "middle":
		return 0x04
	default:
		return 0x01 // left
	}
}

// ============================================================================
// Pointer operations (touchscreen and absolute mouse)
// ============================================================================

func (p *HIDProvider) settlePointer(x, y int) error {
	// Move cursor and wait for animation to complete (absolute mouse mode only)
	if err := p.movePointer(x, y, 0); err != nil {
		return err
	}
	time.Sleep(time.Duration(p.cursorSettleMs) * time.Millisecond)
	return nil
}

func (p *HIDProvider) pressPointer(x, y int, button uint8) error {
	return p.movePointer(x, y, button)
}

func (p *HIDProvider) releasePointerRepeated(x, y int) error {
	// Repeat release multiple times for touchscreen USB polling workaround
	var firstErr error
	released := false
	for i := 0; i < p.releaseRepeatCount; i++ {
		if err := p.movePointer(x, y, 0); err != nil {
			if firstErr == nil {
				firstErr = err
			}
		} else {
			released = true
		}
		if i < p.releaseRepeatCount-1 {
			time.Sleep(time.Duration(p.releaseRepeatDelayMs) * time.Millisecond)
		}
	}
	if released {
		return nil
	}
	return firstErr
}

func (p *HIDProvider) movePointer(x, y int, buttons uint8) error {
	if p.touchscreen {
		return p.writeTouchscreenReport(x, y, buttons != 0)
	}
	return p.writeAbsMouseReport(x, y, buttons, 0)
}

func (p *HIDProvider) scrollPointer(delta int) error {
	if p.touchscreen {
		return nil // Scroll not supported in touchscreen mode
	}
	if delta == 0 {
		return nil
	}
	if delta < -127 {
		delta = -127
	} else if delta > 127 {
		delta = 127
	}

	// Get current position
	x, y := p.getCurrentPosition()
	return p.writeAbsMouseReport(x, y, 0, int8(delta))
}

func (p *HIDProvider) getCurrentPosition() (int, int) {
	if p.pointerState != nil {
		if x, y, ok := p.pointerState.Current(); ok {
			return x, y
		}
	}
	return absMouseMaxPos / 2, absMouseMaxPos / 2
}

// ============================================================================
// HID report writing
// ============================================================================

func writeDevice(dev Device, report []byte) error {
	if dev == nil {
		return ModuleUnavailable("hid device is not configured")
	}
	return dev.Write(report)
}

// writeAbsMouseReport writes a 6-byte absolute mouse report:
// [buttons, x_lo, x_hi, y_lo, y_hi, wheel]
func (p *HIDProvider) writeAbsMouseReport(x, y int, buttons uint8, wheel int8) error {
	absX := clampUint16(x, absMouseMaxPos)
	absY := clampUint16(y, absMouseMaxPos)

	report := make([]byte, 6)
	report[0] = buttons
	binary.LittleEndian.PutUint16(report[1:3], absX)
	binary.LittleEndian.PutUint16(report[3:5], absY)
	report[5] = uint8(wheel)

	// Update state after successful write
	if err := writeDevice(p.pointerDev, report); err != nil {
		return err
	}

	if p.pointerState != nil {
		p.pointerState.Update(int(absX), int(absY))
	}
	return nil
}

// writeTouchscreenReport writes a 6-byte single-contact touch report:
// [flags, contact_id, x_lo, x_hi, y_lo, y_hi]
// flags: bit0=tip switch (touching), bit1=in range
func (p *HIDProvider) writeTouchscreenReport(x, y int, touching bool) error {
	absX := clampUint16(x, absMouseMaxPos)
	absY := clampUint16(y, absMouseMaxPos)

	report := make([]byte, 6)
	if touching {
		report[0] = 0x03 // tip switch + in range
	}
	report[1] = 0x01 // contact_id
	binary.LittleEndian.PutUint16(report[2:4], absX)
	binary.LittleEndian.PutUint16(report[4:6], absY)

	// Update state after successful write
	if err := writeDevice(p.pointerDev, report); err != nil {
		return err
	}

	if p.pointerState != nil {
		p.pointerState.Update(int(absX), int(absY))
	}
	return nil
}

func clampUint16(val, max int) uint16 {
	if val < 0 {
		return 0
	}
	if val > max {
		return uint16(max)
	}
	return uint16(val)
}

// ============================================================================
// Keyboard operations
// ============================================================================

type resolvedKeys struct {
	modifier            uint8
	keys                []uint8
	androidExtensionKey string
	androidUsage        uint16
}

// ResolveKeypressKeys validates and expands keyboard_tap / Keypress key names
// (including Android KEYCODE_* aliases) without requiring a configured device.
// layout selects physical HID usages for letter/symbol taps (qwerty/azerty/qwertz).
func ResolveKeypressKeys(keys []string, layout string) (*resolvedKeys, error) {
	resolved := &resolvedKeys{keys: make([]uint8, 0, 6)}
	androidKeys := make([]string, 0, 1)

	for _, key := range keys {
		normalized := strings.ToLower(strings.TrimSpace(key))
		if normalized == "" {
			continue
		}

		if alias, ok := androidKeyboardTapAliases[normalized]; ok {
			if alias.UnsupportedReason != "" {
				return nil, InvalidArgumentsf("android-only key %q (keycode %d) is not supported by keyboard_tap: %s", normalized, alias.Keycode, alias.UnsupportedReason)
			}
			normalized = alias.Replacement
		}

		if usage, ok := androidExtensionUsageMap[normalized]; ok {
			androidKeys = append(androidKeys, normalized)
			resolved.androidUsage = usage
			continue
		}

		if mod, ok := hidModifierMap[normalized]; ok {
			resolved.modifier |= mod
			continue
		}

		if stroke, ok := keyboardLayoutTapKeyStroke(layout, normalized); ok {
			resolved.modifier |= stroke.modifier
			resolved.keys = append(resolved.keys, stroke.usage)
			continue
		}

		return nil, InvalidArgumentsf("unknown key: %q", normalized)
	}

	if len(androidKeys) > 1 {
		return nil, InvalidArgumentsf("keypress supports one Android extension key at a time, got %v", androidKeys)
	}
	if len(androidKeys) == 1 {
		if len(resolved.keys) > 0 || resolved.modifier != 0 {
			return nil, InvalidArgumentsf("Android extension key %q cannot be combined with standard keyboard keys or modifiers", androidKeys[0])
		}
		resolved.androidExtensionKey = androidKeys[0]
		return resolved, nil
	}

	if len(resolved.keys) == 0 && resolved.modifier == 0 {
		return nil, InvalidArguments("at least one key or modifier is required")
	}
	if len(resolved.keys) > 6 {
		return nil, InvalidArguments("keypress supports at most 6 simultaneous keys")
	}

	return resolved, nil
}

func (p *HIDProvider) resolveKeys(keys []string) (*resolvedKeys, error) {
	layout := ""
	if p != nil {
		layout = p.keyboardLayout
	}
	return ResolveKeypressKeys(keys, layout)
}

func (p *HIDProvider) androidExtensionPressReport(key string, usage uint16) ([]byte, error) {
	// Touchscreen gadgets accept standard Consumer Control usage LE reports.
	if p != nil && p.touchscreen {
		return []byte{byte(usage), byte(usage >> 8)}, nil
	}
	// Absolute pointer-mode hid.usb2 uses a packed bitfield report and only
	// exposes a media/volume/brightness/screenshot subset.
	report, ok := absolutePointerModeExtensionReports[key]
	if !ok {
		return nil, InvalidArgumentsf("android extension key is unavailable in the configured pointer mode: %q requires hid.pointer_mode=\"touchscreen\"; hid.pointer_mode=\"absolute\" only exposes these hid.usb2 keys: %s", key, absolutePointerModeExtensionKeyList)
	}
	return []byte{byte(report), byte(report >> 8)}, nil
}

func (p *HIDProvider) tapAndroidExtension(key string, usage uint16) error {
	if p.androidKeyboardDev == nil {
		return ModuleUnavailablef("android extension keyboard device is not configured; ensure hid.android_keyboard_device exists to use %s", key)
	}

	report, err := p.androidExtensionPressReport(key, usage)
	if err != nil {
		return err
	}

	if err := writeDevice(p.androidKeyboardDev, report); err != nil {
		return err
	}

	holdMs := p.keyboardTapHoldMs
	time.Sleep(time.Duration(holdMs) * time.Millisecond)

	return writeDevice(p.androidKeyboardDev, []byte{0x00, 0x00})
}

func (p *HIDProvider) tapKeyboardChord(modifier uint8, keys []uint8) error {
	// 8-byte HID boot keyboard report: [modifier, reserved, key1..key6]
	report := make([]byte, 8)
	report[0] = modifier
	for i := 0; i < len(keys) && i < 6; i++ {
		report[2+i] = keys[i]
	}

	// Press
	if err := writeDevice(p.keyboardDev, report); err != nil {
		return err
	}

	// Hold (longer for modifier chords)
	holdMs := p.keyboardTapHoldMs
	if modifier != 0 {
		holdMs = p.keyboardModifierHoldMs
	}
	time.Sleep(time.Duration(holdMs) * time.Millisecond)

	// Release (all zeros)
	return writeDevice(p.keyboardDev, make([]byte, 8))
}

// Close releases cached file descriptors after the owning tool generation drains.
func (p *HIDProvider) Close() {
	for _, device := range []Device{p.pointerDev, p.keyboardDev, p.androidKeyboardDev} {
		if closer, ok := device.(interface{ Close() }); ok {
			closer.Close()
		}
	}
}

// A drag may intentionally span HTTP requests. Keep its device generation
// alive until the user completes it instead of abandoning a pressed contact.
func (p *HIDProvider) CheckConfigReload() error {
	p.dragMu.Lock()
	defer p.dragMu.Unlock()
	if p.dragActive {
		return fmt.Errorf("finish drag_release before applying input configuration")
	}
	return nil
}
