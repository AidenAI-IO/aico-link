package mnk

import "context"

const (
	defaultSwipeGestureDurationMs = 300
	dragStartHoldMs               = 500
	dragStartMoveDistance         = 200.0
	dragStartMoveSpeed            = 500.0
	dragStartMoveDurationMs       = int(dragStartMoveDistance / dragStartMoveSpeed * 1000)
	dragReleaseHoldMs             = 200
)

// SwipeOptions controls the timing and interpolation of a swipe. A zero
// duration uses the provider's default swipe duration; a zero Steps uses the
// provider's default interpolation step count. Hold durations are optional
// and default to zero; HID content swipes use an internal
// low-speed release tail when HoldAfterMs is zero.
type SwipeOptions struct {
	DurationMs   int
	HoldBeforeMs int
	HoldAfterMs  int
	Steps        int
}

// Provider defines a minimal set of mouse/keyboard primitives for device input.
// This interface isolates tools from device-specific implementations (HID, ADB, etc).
//
// Coordinate system: Normalized 0-1000 scale where (0,0) is top-left, (1000,1000) is bottom-right.
// The provider implementation handles conversion to device-specific absolute coordinates.
//
// Context: Callers must pass the request/tool context so platform gates (e.g. iOS
// keyboard isolation batch) can observe sticky isolate/restore across a run.
type Provider interface {
	// Click performs a press-hold-release at the specified position.
	// For touchscreen mode: simulates finger tap (button parameter ignored).
	// For absolute mouse mode: performs button click with proper button state.
	//
	// Parameters:
	//   x, y: Normalized coordinates (0-1000)
	//   button: "left" | "right" | "middle" (ignored in touchscreen mode)
	//   holdMs: Duration to hold before release. Use higher values (500+) for long-press.
	//           Default timing is handled by implementation (typically 60ms for tap).
	Click(ctx context.Context, x, y float64, button string, holdMs int) error

	// DoubleClick performs two clicks in rapid succession.
	// The pause between clicks is handled by implementation (typically 100ms).
	DoubleClick(ctx context.Context, x, y float64, button string) error

	// Swipe performs a short gesture along a path of points.
	// Unlike Drag, Swipe should begin moving immediately and complete quickly
	// enough to avoid triggering long-press behavior.
	Swipe(ctx context.Context, path [][2]float64, button string) error

	// DragStart presses at a normalized point, holds for 500ms, then moves 200
	// normalized units at 500 units/second in a bounded activation direction
	// without releasing.
	DragStart(ctx context.Context, x, y float64, button string) error

	// DragRelease moves the active drag contact directly to a normalized point,
	// holds for 200ms, then releases it.
	DragRelease(ctx context.Context, x, y float64) error

	// Keypress sends one or more keys simultaneously.
	// Supports modifier+key combinations as a single chord.
	//
	// Parameters:
	//   keys: Array of key names to press simultaneously, e.g.:
	//         ["enter"]                    - single key
	//         ["ctrl", "a"]                - Ctrl+A chord
	//         ["ctrl", "shift", "escape"]  - three-key combination
	//
	// Supported key names:
	//   - Letters: "a"-"z"
	//   - Numbers: "0"-"9"
	//   - Modifiers: "ctrl", "shift", "alt", "meta"/"cmd" (with l/r variants: "lctrl", "rshift", etc.)
	//   - Function: "f1"-"f12"
	//   - Navigation: "up", "down", "left", "right", "home", "end", "pageup", "pagedown"
	//   - Edit: "enter", "escape", "backspace", "tab", "space", "delete"
	//   - Android extensions: "volume_up", "volume_down", "android_back", "android_home", "power", etc.
	//
	// Implementation handles proper HID report construction with modifier byte + key array.
	Keypress(ctx context.Context, keys []string) error

	// Move positions the pointer without pressing any button.
	// In touchscreen mode, this is typically unsupported (no hover).
	// In absolute mouse mode, moves the cursor to the specified position.
	Move(ctx context.Context, x, y float64) error

	// Scroll sends wheel/scroll input.
	//
	// Parameters:
	//   scrollX: Horizontal scroll delta (positive = right)
	//   scrollY: Vertical scroll delta (positive = up)
	//
	// In touchscreen mode, implementation may convert to swipe gestures.
	// In absolute mouse mode, sends wheel events.
	Scroll(ctx context.Context, scrollX, scrollY int) error
}

// TextProvider types ASCII keystrokes through a remote input backend. It is
// optional so local HID/ADB typing retains its existing layout-aware behavior.
// This primitive does not focus, clear, paste, or verify a field.
type TextProvider interface {
	KeyboardText(ctx context.Context, text string) error
}

// TouchAction is one low-level pointer/touch primitive. Coordinates use the
// same normalized 0-1000 space as Provider. A point is optional for
// touch_up (the current contact position is used) and required for the other
// coordinate-bearing actions by the tool parser.
type TouchAction struct {
	Type       string `json:"action"`
	Point      *Point `json:"point,omitempty"`
	DurationMs int    `json:"ms,omitempty"`
	Button     string `json:"button,omitempty"`
	// steps is set internally by ADB SwipeWithOptions; atomic callers use the
	// provider default. It does not add a field to the public atomic protocol.
	steps int
	// releaseTail is internal: this move must remain slow and linear, rather
	// than starting another acceleration/braking cycle.
	releaseTail bool
	// linear preserves travel through intermediate swipe waypoints and swipes
	// that intentionally omit braking. Atomic moves use the default profile.
	linear bool
}

// validateTouchActionLimits bounds both caller programs and their profiled
// expansion before any input is sent. Count all declared durations so optional
// backend-specific release holds cannot bypass the program budget.
func validateTouchActionLimits(actions []TouchAction) error {
	if len(actions) == 0 {
		return InvalidArguments("touch actions must contain at least one atomic action")
	}
	if len(actions) > 128 {
		return InvalidArguments("touch actions must contain at most 128 atomic actions")
	}
	totalDurationMs := 0
	for i, action := range actions {
		if action.DurationMs < 0 || action.DurationMs > 30000 {
			return InvalidArgumentsf("touch action %d duration must be between 0 and 30000 ms", i)
		}
		totalDurationMs += action.DurationMs
		if totalDurationMs > 60000 {
			return InvalidArguments("total duration in touch actions must not exceed 60000 ms")
		}
	}
	return nil
}

// TouchActionProvider executes a validated sequence of atomic touch actions
// while keeping the pointer profile and contact state alive for the whole
// sequence. Providers should return ModuleUnavailable when the selected
// backend or device cannot represent an independent touch contact.
type TouchActionProvider interface {
	TouchActions(ctx context.Context, actions []TouchAction) error
}

// Point represents a normalized coordinate point.
type Point struct {
	X float64 `json:"x"` // Normalized X coordinate (0-1000)
	Y float64 `json:"y"` // Normalized Y coordinate (0-1000)
}

// dragActivationPoint picks the axis direction with the most room and moves
// exactly 200 normalized units. This guarantees a real movement at every valid
// start point without asking the caller to guess an activation coordinate.
func dragActivationPoint(start Point) Point {
	type candidate struct {
		room float64
		dx   float64
		dy   float64
	}
	candidates := []candidate{
		{room: 1000 - start.X, dx: dragStartMoveDistance},
		{room: 1000 - start.Y, dy: dragStartMoveDistance},
		{room: start.X, dx: -dragStartMoveDistance},
		{room: start.Y, dy: -dragStartMoveDistance},
	}
	selected := candidates[0]
	for _, current := range candidates[1:] {
		if current.room > selected.room {
			selected = current
		}
	}
	return Point{X: start.X + selected.dx, Y: start.Y + selected.dy}
}

// Button constants for click/drag operations.
const (
	ButtonLeft   = "left"
	ButtonRight  = "right"
	ButtonMiddle = "middle"
)

// Common key name constants for Keypress.
const (
	// Modifiers
	KeyCtrl  = "ctrl"
	KeyShift = "shift"
	KeyAlt   = "alt"
	KeyMeta  = "meta"
	KeyCmd   = "cmd"

	// Common keys
	KeyEnter     = "enter"
	KeyEscape    = "escape"
	KeyBackspace = "backspace"
	KeyTab       = "tab"
	KeySpace     = "space"
	KeyDelete    = "delete"

	// Navigation
	KeyUp       = "up"
	KeyDown     = "down"
	KeyLeft     = "left"
	KeyRight    = "right"
	KeyHome     = "home"
	KeyEnd      = "end"
	KeyPageUp   = "pageup"
	KeyPageDown = "pagedown"

	// Android extensions
	KeyVolumeUp       = "volume_up"
	KeyVolumeDown     = "volume_down"
	KeyAndroidBack    = "android_back"
	KeyAndroidHome    = "android_home"
	KeyPower          = "power"
	KeyMediaPlayPause = "media_play_pause"
)
