package mnk

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
)

const (
	// DefaultSwipeSpeed is the normalized gesture speed used when callers do
	// not provide speed or duration_ms.
	DefaultSwipeSpeed  = 2500.0
	MaxSwipeDurationMs = 10_000
	MaxSwipeHoldMs     = 10_000
	MaxSwipeSteps      = 1_000
)

// timedSwipeProvider is implemented by providers that can honor a caller's
// requested swipe duration. Provider keeps the historical Swipe method for
// internal callers that do not need timing control.
type timedSwipeProvider interface {
	SwipeWithDuration(context.Context, [][2]float64, string, int) error
}

// swipeOptionsProvider is implemented by providers that can honor the full
// swipe timing contract. It is optional so existing Provider implementations
// retain the duration-only compatibility path.
type swipeOptionsProvider interface {
	SwipeWithOptions(context.Context, [][2]float64, string, SwipeOptions) error
}

// TouchGestureToolAdapter 使用 MNK Provider 的 touch_gesture 工具适配器
type TouchGestureToolAdapter struct {
	provider Provider
}

// NewTouchGestureToolAdapter 创建适配器
func NewTouchGestureToolAdapter(provider Provider) *TouchGestureToolAdapter {
	return &TouchGestureToolAdapter{
		provider: provider,
	}
}

// Call 处理 touch_gesture 工具调用。
// 参数/配置类失败返回 *Error（InvalidArguments / ModuleUnavailable）；
// Provider 执行失败返回 ExecutionFailed。调用方应映射为 structured ToolError 并返回 (msg, nil)。
func (t *TouchGestureToolAdapter) Call(ctx context.Context, input string) (string, error) {
	// Detect the optional advanced atomic program before decoding the standard
	// gesture form so both envelopes remain unambiguous.
	var envelope struct {
		Actions json.RawMessage `json:"actions"`
	}
	if err := json.Unmarshal([]byte(input), &envelope); err == nil && envelope.Actions != nil {
		var rawActions []json.RawMessage
		if err := json.Unmarshal(envelope.Actions, &rawActions); err != nil {
			return "", InvalidArgumentsf("actions must be an array of atomic action objects: %v", err)
		}
		return t.callAtomic(ctx, rawActions)
	}

	var args struct {
		Type         string        `json:"type"`
		Point        *pointerPoint `json:"point"`
		Start        *pointerPoint `json:"start"`
		End          *pointerPoint `json:"end"`
		Direction    string        `json:"direction"`
		Button       string        `json:"button"`
		HoldMs       *int          `json:"hold_ms"`
		Speed        *float64      `json:"speed"`
		DurationMs   *int          `json:"duration_ms"`
		HoldBeforeMs *int          `json:"hold_before_ms"`
		HoldAfterMs  *int          `json:"hold_after_ms"`
		Steps        *int          `json:"steps"`
	}

	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return "", InvalidArgumentsf("invalid input: %v. Common mistakes: missing quotes around string values, incorrect comma placement, point/start/end must be objects with named keys like {\"x\":500,\"y\":300} not bare values. Example: {\"type\":\"tap\",\"point\":{\"x\":500,\"y\":500}}", err)
	}

	gestureType := strings.ToLower(strings.TrimSpace(args.Type))
	if gestureType == "" {
		return "", InvalidArguments("type is required")
	}

	button := args.Button
	if button == "" {
		button = ButtonLeft
	}

	switch gestureType {
	case "tap":
		if args.Point == nil {
			return "", InvalidArguments("point is required for tap")
		}
		if err := t.requireProvider(); err != nil {
			return "", err
		}
		holdMs := 0
		if args.HoldMs != nil && *args.HoldMs > 0 {
			holdMs = *args.HoldMs
		}
		return t.handleTap(ctx, args.Point, button, holdMs)

	case "long_press":
		if args.Point == nil {
			return "", InvalidArguments("point is required for long_press")
		}
		holdMs := 500
		if args.HoldMs != nil && *args.HoldMs > 0 {
			holdMs = *args.HoldMs
		}
		if err := t.requireProvider(); err != nil {
			return "", err
		}
		return t.handleTap(ctx, args.Point, button, holdMs)

	case "double_tap":
		if args.Point == nil {
			return "", InvalidArguments("point is required for double_tap")
		}
		if err := t.requireProvider(); err != nil {
			return "", err
		}
		return t.handleDoubleTap(ctx, args.Point, button)

	case "swipe":
		start, end, options, err := resolveSwipeArgsWithOptions(
			args.Start,
			args.End,
			args.Direction,
			args.Speed,
			args.DurationMs,
			args.HoldBeforeMs,
			args.HoldAfterMs,
			args.Steps,
		)
		if err != nil {
			return "", err
		}
		if err := t.requireProvider(); err != nil {
			return "", err
		}
		return t.handleSwipe(ctx, start, end, button, options)

	case "drag_start":
		if args.Point == nil {
			return "", InvalidArguments("point is required for drag_start")
		}
		if err := t.requireProvider(); err != nil {
			return "", err
		}
		return t.handleDragStart(ctx, args.Point, button)

	case "drag_release":
		if args.Point == nil {
			return "", InvalidArguments("point is required for drag_release")
		}
		if err := t.requireProvider(); err != nil {
			return "", err
		}
		return t.handleDragRelease(ctx, args.Point)

	default:
		return "", InvalidArgumentsf("unsupported gesture type: %q", args.Type)
	}
}

type atomicTouchActionInput struct {
	Action     string        `json:"action"`
	Type       string        `json:"type"`
	Point      *pointerPoint `json:"point"`
	X          *float64      `json:"x"`
	Y          *float64      `json:"y"`
	Ms         *int          `json:"ms"`
	DurationMs *int          `json:"duration_ms"`
	Speed      *float64      `json:"speed"`
	Button     string        `json:"button"`
}

func (t *TouchGestureToolAdapter) callAtomic(ctx context.Context, rawActions []json.RawMessage) (string, error) {
	if len(rawActions) == 0 {
		return "", InvalidArguments("actions must contain at least one atomic action")
	}
	if len(rawActions) > 128 {
		return "", InvalidArguments("actions must contain at most 128 atomic actions")
	}
	if t == nil || t.provider == nil {
		return "", ModuleUnavailable("atomic touch actions are not configured")
	}
	atomic, ok := t.provider.(TouchActionProvider)
	if !ok || atomic == nil {
		return "", ModuleUnavailable("atomic touch actions are not supported by this input provider")
	}

	actions := make([]TouchAction, 0, len(rawActions))
	contactActive := false
	totalWaitMs := 0
	var currentPoint *pointerPoint
	for index, raw := range rawActions {
		var parsed atomicTouchActionInput
		if err := json.Unmarshal(raw, &parsed); err != nil {
			return "", InvalidArgumentsf("actions[%d] must be an object: %v", index, err)
		}
		actionType := strings.ToLower(strings.TrimSpace(parsed.Action))
		if actionType == "" {
			actionType = strings.ToLower(strings.TrimSpace(parsed.Type))
		}
		if actionType == "" {
			return "", InvalidArgumentsf("actions[%d] action is required", index)
		}
		if parsed.Button == "" {
			parsed.Button = ButtonLeft
		}
		action := TouchAction{Type: actionType, Button: parsed.Button}
		duration := parsed.Ms
		if duration == nil {
			duration = parsed.DurationMs
		}
		if duration != nil {
			if *duration < 0 || *duration > 30000 {
				return "", InvalidArgumentsf("actions[%d] %s duration must be between 0 and 30000 ms", index, actionType)
			}
			if strings.EqualFold(actionType, "wait") {
				totalWaitMs += *duration
				if totalWaitMs > 60000 {
					return "", InvalidArguments("total wait time in actions must not exceed 60000 ms")
				}
			}
			action.DurationMs = *duration
		}

		point := parsed.Point
		if point == nil && (parsed.X != nil || parsed.Y != nil) {
			if parsed.X == nil || parsed.Y == nil {
				return "", InvalidArgumentsf("actions[%d] x and y must be provided together", index)
			}
			point = &pointerPoint{X: pointerCoordinate(*parsed.X), Y: pointerCoordinate(*parsed.Y)}
		}
		if point != nil {
			converted := Point{X: point.X.Float64(), Y: point.Y.Float64()}
			action.Point = &converted
		}
		switch actionType {
		case "touch_down":
			if action.Point == nil {
				return "", InvalidArgumentsf("actions[%d] touch_down requires point (x/y)", index)
			}
			if contactActive {
				return "", InvalidArgumentsf("actions[%d] touch_down requires touch_up before starting another contact", index)
			}
			contactActive = true
			currentPoint = point
		case "move_to":
			if action.Point == nil {
				return "", InvalidArgumentsf("actions[%d] %s requires point (x/y)", index, actionType)
			}
			if parsed.Speed != nil {
				if math.IsNaN(*parsed.Speed) || math.IsInf(*parsed.Speed, 0) || *parsed.Speed <= 0 {
					return "", InvalidArgumentsf("actions[%d] speed must be a positive finite number", index)
				}
				// Explicit timing retains the same precedence as legacy swipe:
				// duration_ms (or its ms alias) overrides speed. Otherwise,
				// derive movement time from normalized 0-1000 coordinates.
				if duration == nil {
					if currentPoint == nil {
						return "", InvalidArgumentsf("actions[%d] move_to speed requires a preceding touch_down or move_to point", index)
					}
					calculatedDuration := swipeDurationForDistance(currentPoint, point, *parsed.Speed)
					if calculatedDuration > 30000 {
						return "", InvalidArgumentsf("actions[%d] speed is too low for move_to; calculated duration_ms=%d exceeds 30000", index, calculatedDuration)
					}
					action.DurationMs = calculatedDuration
				}
			}
			currentPoint = point
		case "touch_up":
			// point is optional: release at the current contact location.
			if !contactActive {
				return "", InvalidArgumentsf("actions[%d] touch_up requires an active contact", index)
			}
			contactActive = false
			if point != nil {
				currentPoint = point
			}
		case "wait":
			if duration == nil {
				return "", InvalidArgumentsf("actions[%d] wait requires ms", index)
			}
		default:
			return "", InvalidArgumentsf("actions[%d] has unsupported action %q; use touch_down, move_to, wait, or touch_up", index, actionType)
		}
		actions = append(actions, action)
	}
	if contactActive {
		return "", InvalidArguments("actions must end with touch_up")
	}

	if err := atomic.TouchActions(ctx, actions); err != nil {
		return "", WrapExecutionFailed(err)
	}
	return "ok", nil
}

func (t *TouchGestureToolAdapter) requireProvider() error {
	if t == nil || t.provider == nil {
		return ModuleUnavailable("touch_gesture is not configured")
	}
	return nil
}

func (t *TouchGestureToolAdapter) handleTap(ctx context.Context, point *pointerPoint, button string, holdMs int) (string, error) {
	if err := t.provider.Click(ctx, point.X.Float64(), point.Y.Float64(), button, holdMs); err != nil {
		return "", WrapExecutionFailed(err)
	}
	return "ok", nil
}

func (t *TouchGestureToolAdapter) handleDoubleTap(ctx context.Context, point *pointerPoint, button string) (string, error) {
	if err := t.provider.DoubleClick(ctx, point.X.Float64(), point.Y.Float64(), button); err != nil {
		return "", WrapExecutionFailed(err)
	}
	return "ok", nil
}

func (t *TouchGestureToolAdapter) handleSwipe(ctx context.Context, start, end *pointerPoint, button string, options SwipeOptions) (string, error) {
	path := [][2]float64{
		{start.X.Float64(), start.Y.Float64()},
		{end.X.Float64(), end.Y.Float64()},
	}
	if err := swipeWithOptions(ctx, t.provider, path, button, options); err != nil {
		return "", WrapExecutionFailed(err)
	}
	return "ok", nil
}

func (t *TouchGestureToolAdapter) handleDragStart(ctx context.Context, point *pointerPoint, button string) (string, error) {
	if err := t.provider.DragStart(ctx, point.X.Float64(), point.Y.Float64(), button); err != nil {
		return "", WrapExecutionFailed(err)
	}
	return "ok", nil
}

func (t *TouchGestureToolAdapter) handleDragRelease(ctx context.Context, point *pointerPoint) (string, error) {
	if err := t.provider.DragRelease(ctx, point.X.Float64(), point.Y.Float64()); err != nil {
		return "", WrapExecutionFailed(err)
	}
	return "ok", nil
}

func swipeWithOptions(ctx context.Context, provider Provider, path [][2]float64, button string, options SwipeOptions) error {
	if configured, ok := provider.(swipeOptionsProvider); ok {
		return configured.SwipeWithOptions(ctx, path, button, options)
	}
	if options.DurationMs > 0 {
		if timed, ok := provider.(timedSwipeProvider); ok {
			return timed.SwipeWithDuration(ctx, path, button, options.DurationMs)
		}
	}
	return provider.Swipe(ctx, path, button)
}

func resolveSwipeArgsWithOptions(start, end *pointerPoint, direction string, speed *float64, durationMs, holdBeforeMs, holdAfterMs, steps *int) (*pointerPoint, *pointerPoint, SwipeOptions, error) {
	options := SwipeOptions{}
	if holdBeforeMs != nil {
		if *holdBeforeMs < 0 || *holdBeforeMs > MaxSwipeHoldMs {
			return nil, nil, options, InvalidArgumentsf("hold_before_ms must be in range [0, %d]", MaxSwipeHoldMs)
		}
		options.HoldBeforeMs = *holdBeforeMs
	}
	if holdAfterMs != nil {
		if *holdAfterMs < 0 || *holdAfterMs > MaxSwipeHoldMs {
			return nil, nil, options, InvalidArgumentsf("hold_after_ms must be in range [0, %d]", MaxSwipeHoldMs)
		}
		options.HoldAfterMs = *holdAfterMs
	}
	if steps != nil {
		if *steps < 1 || *steps > MaxSwipeSteps {
			return nil, nil, options, InvalidArgumentsf("steps must be in range [1, %d]", MaxSwipeSteps)
		}
		options.Steps = *steps
	}

	resolvedStart, resolvedEnd, resolvedDuration, err := resolveSwipeGeometry(start, end, direction, speed, durationMs)
	if err != nil {
		return nil, nil, options, err
	}
	options.DurationMs = resolvedDuration
	return resolvedStart, resolvedEnd, options, nil
}

func resolveSwipeGeometry(start, end *pointerPoint, direction string, speed *float64, durationMs *int) (*pointerPoint, *pointerPoint, int, error) {
	if start == nil {
		return nil, nil, 0, InvalidArguments("start is required for swipe")
	}
	if err := validateSwipePoint("start", start); err != nil {
		return nil, nil, 0, err
	}
	if end != nil && strings.TrimSpace(direction) != "" {
		return nil, nil, 0, InvalidArguments("swipe accepts either end or direction, not both")
	}

	resolvedSpeed := DefaultSwipeSpeed
	if speed != nil {
		if math.IsNaN(*speed) || math.IsInf(*speed, 0) || *speed <= 0 {
			return nil, nil, 0, InvalidArguments("speed must be a positive finite number")
		}
		resolvedSpeed = *speed
	}
	resolvedDuration := 0
	if durationMs != nil {
		if *durationMs <= 0 || *durationMs > MaxSwipeDurationMs {
			return nil, nil, 0, InvalidArgumentsf("duration_ms must be in range [1, %d]", MaxSwipeDurationMs)
		}
		resolvedDuration = *durationMs
	}

	if end == nil {
		direction = strings.ToLower(strings.TrimSpace(direction))
		if direction == "" {
			return nil, nil, 0, InvalidArguments("end or direction is required for swipe")
		}
		if direction != "up" && direction != "down" && direction != "left" && direction != "right" {
			return nil, nil, 0, InvalidArgumentsf("unsupported swipe direction: %q; use up, down, left, or right", direction)
		}

		travel := distanceToEdge(start, direction)
		if resolvedDuration > 0 {
			travel = resolvedSpeed * float64(resolvedDuration) / 1000.0
		}
		end = directionEnd(start, direction, travel)
		if err := validateSwipePoint("derived end", end); err != nil {
			return nil, nil, 0, InvalidArgumentsf("speed and duration_ms move the swipe past the screen edge: %v", err)
		}
		if samePointerPoint(start, end) {
			return nil, nil, 0, InvalidArguments("swipe direction does not move from start before reaching the screen edge")
		}
		if resolvedDuration == 0 {
			resolvedDuration = swipeDurationForDistance(start, end, resolvedSpeed)
			if resolvedDuration > MaxSwipeDurationMs {
				return nil, nil, 0, InvalidArgumentsf("speed is too low for this swipe; calculated duration_ms=%d exceeds %d", resolvedDuration, MaxSwipeDurationMs)
			}
		}
		return start, end, resolvedDuration, nil
	}

	if err := validateSwipePoint("end", end); err != nil {
		return nil, nil, 0, err
	}
	if samePointerPoint(start, end) {
		return nil, nil, 0, InvalidArguments("swipe requires distinct start and end points")
	}
	if resolvedDuration == 0 {
		resolvedDuration = swipeDurationForDistance(start, end, resolvedSpeed)
		if resolvedDuration > MaxSwipeDurationMs {
			return nil, nil, 0, InvalidArgumentsf("speed is too low for this swipe; calculated duration_ms=%d exceeds %d", resolvedDuration, MaxSwipeDurationMs)
		}
	}
	return start, end, resolvedDuration, nil
}

func distanceToEdge(start *pointerPoint, direction string) float64 {
	switch direction {
	case "up":
		return start.Y.Float64()
	case "down":
		return 1000 - start.Y.Float64()
	case "left":
		return start.X.Float64()
	case "right":
		return 1000 - start.X.Float64()
	default:
		return 0
	}
}

func directionEnd(start *pointerPoint, direction string, travel float64) *pointerPoint {
	x := start.X.Float64()
	y := start.Y.Float64()
	switch direction {
	case "up":
		y -= travel
	case "down":
		y += travel
	case "left":
		x -= travel
	case "right":
		x += travel
	}
	return &pointerPoint{
		X: pointerCoordinate(x),
		Y: pointerCoordinate(y),
	}
}

func validateSwipePoint(name string, point *pointerPoint) error {
	x := point.X.Float64()
	y := point.Y.Float64()
	if math.IsNaN(x) || math.IsInf(x, 0) || math.IsNaN(y) || math.IsInf(y, 0) {
		return InvalidArgumentsf("%s coordinates must be finite", name)
	}
	if x < 0 || x > 1000 || y < 0 || y > 1000 {
		return InvalidArgumentsf("%s coordinates must use the normalized 0-1000 scale, got x=%.2f y=%.2f", name, x, y)
	}
	return nil
}

func swipeDurationForDistance(start, end *pointerPoint, speed float64) int {
	dx := end.X.Float64() - start.X.Float64()
	dy := end.Y.Float64() - start.Y.Float64()
	distance := math.Hypot(dx, dy)
	duration := int(math.Round(distance / speed * 1000.0))
	if duration < 1 {
		return 1
	}
	return duration
}

// KeyboardTapToolAdapter 使用 MNK Provider 的 keyboard_tap 工具适配器
type KeyboardTapToolAdapter struct {
	provider Provider
}

// NewKeyboardTapToolAdapter 创建适配器
func NewKeyboardTapToolAdapter(provider Provider) *KeyboardTapToolAdapter {
	return &KeyboardTapToolAdapter{
		provider: provider,
	}
}

// Call 处理 keyboard_tap 工具调用。
func (t *KeyboardTapToolAdapter) Call(ctx context.Context, input string) (string, error) {
	var args struct {
		Keys []string `json:"keys"`
	}

	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return "", InvalidArgumentsf("invalid input: %v. Expected JSON format: {\"keys\": [\"enter\"]}. Common mistakes: missing quotes around key names, incorrect comma placement in array", err)
	}

	if len(args.Keys) == 0 {
		return "", InvalidArguments("keys array is required")
	}

	if t == nil || t.provider == nil {
		if _, err := ResolveKeypressKeys(args.Keys, ""); err != nil {
			return "", err
		}
		return "", ModuleUnavailable("keyboard_tap is not configured")
	}

	if err := t.provider.Keypress(ctx, args.Keys); err != nil {
		return "", WrapExecutionFailed(err)
	}

	return "ok", nil
}

// MouseMoveToolAdapter 使用 MNK Provider 的 mouse_move 工具适配器
type MouseMoveToolAdapter struct {
	provider Provider
}

// NewMouseMoveToolAdapter 创建适配器
func NewMouseMoveToolAdapter(provider Provider) *MouseMoveToolAdapter {
	return &MouseMoveToolAdapter{
		provider: provider,
	}
}

// Call 处理 mouse_move 工具调用。
func (t *MouseMoveToolAdapter) Call(ctx context.Context, input string) (string, error) {
	var args struct {
		X pointerCoordinate `json:"x"`
		Y pointerCoordinate `json:"y"`
	}

	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return "", InvalidArgumentsf("invalid input: %v. Expected JSON format: {\"x\": 500, \"y\": 300}. Coordinates always use the normalized 0-1000 scale", err)
	}

	if t == nil || t.provider == nil {
		return "", ModuleUnavailable("mouse_move is not configured")
	}

	if err := t.provider.Move(ctx, args.X.Float64(), args.Y.Float64()); err != nil {
		return "", WrapExecutionFailed(err)
	}

	return "ok", nil
}

// MouseScrollToolAdapter 使用 MNK Provider 的 mouse_scroll 工具适配器
type MouseScrollToolAdapter struct {
	provider Provider
}

// NewMouseScrollToolAdapter 创建适配器
func NewMouseScrollToolAdapter(provider Provider) *MouseScrollToolAdapter {
	return &MouseScrollToolAdapter{
		provider: provider,
	}
}

// Call 处理 mouse_scroll 工具调用。
func (t *MouseScrollToolAdapter) Call(ctx context.Context, input string) (string, error) {
	var args struct {
		Delta int `json:"delta"`
	}

	if err := json.Unmarshal([]byte(input), &args); err != nil {
		return "", InvalidArgumentsf("invalid input: %v. Expected JSON format: {\"delta\": -3}. Delta must be a number between -127 and 127", err)
	}
	if args.Delta == 0 {
		return "ok", nil
	}
	if args.Delta < -127 || args.Delta > 127 {
		return "", InvalidArguments("delta must be between -127 and 127")
	}

	if t == nil || t.provider == nil {
		return "", ModuleUnavailable("mouse_scroll is not configured")
	}

	if err := t.provider.Scroll(ctx, 0, args.Delta); err != nil {
		return "", WrapExecutionFailed(err)
	}

	return "ok", nil
}

// pointerPoint 和 pointerCoordinate 辅助类型（从 tools_hid.go 复制）
type pointerPoint struct {
	X pointerCoordinate `json:"x"`
	Y pointerCoordinate `json:"y"`
}

type pointerCoordinate float64

func (c pointerCoordinate) Float64() float64 {
	return float64(c)
}

func (c *pointerCoordinate) UnmarshalJSON(data []byte) error {
	var number float64
	if err := json.Unmarshal(data, &number); err == nil {
		*c = pointerCoordinate(number)
		return nil
	}

	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		value, parseErr := parseFloat(strings.TrimSpace(text))
		if parseErr != nil {
			return fmt.Errorf("parse coordinate %q: %w", text, parseErr)
		}
		*c = pointerCoordinate(value)
		return nil
	}

	return fmt.Errorf("coordinate must be a number or numeric string")
}

func parseFloat(s string) (float64, error) {
	value, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, err
	}
	if math.IsNaN(value) || math.IsInf(value, 0) {
		return 0, fmt.Errorf("coordinate must be a finite number, got %v", s)
	}
	return value, nil
}

func samePointerPoint(first, second *pointerPoint) bool {
	return first != nil && second != nil && first.X == second.X && first.Y == second.Y
}
