package agent

import (
	"aico-agent/internal/agent/mnk"
	"encoding/json"
	"image"
	"math"
	"strings"
	"sync"
)

// pointerExclusionFraction sizes the area around an absolute-mode pointer,
// as a share of the screenshot width, that progress comparison ignores. The
// iOS pointer circle and its hover effect fit inside it.
const pointerExclusionFraction = 0.05

// pointerTracker remembers where the absolute-mode pointer was left by the
// last pointer action, so the next action's progress comparison can ignore
// the pointer leaving that spot as well as arriving at the new one. Moving
// the pointer changes a dozen or more grid blocks even when a tap missed.
type pointerTracker struct {
	mu   sync.Mutex
	last *normalizedPoint
}

func (p *pointerTracker) current() *normalizedPoint {
	if p == nil {
		return nil
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.last == nil {
		return nil
	}
	point := *p.last
	return &point
}

// set records the pointer position after an action; nil means unknown.
func (p *pointerTracker) set(point *normalizedPoint) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = point
}

// pointerFinalPoint returns where a pointer action leaves the pointer, or nil
// when it cannot be told from the input. Swipes and drag_start resolve their
// end the same way execution does; actionOutput tells how many points of a
// tap_sequence were tapped.
func pointerFinalPoint(toolName, input, actionOutput string) *normalizedPoint {
	switch strings.ToLower(strings.TrimSpace(toolName)) {
	case "mouse_move":
		var args struct {
			X *pointerCoordinate `json:"x"`
			Y *pointerCoordinate `json:"y"`
		}
		if json.Unmarshal([]byte(input), &args) != nil || args.X == nil || args.Y == nil {
			return nil
		}
		return finiteNormalizedPoint(args.X.Float64(), args.Y.Float64())
	case "touch_gesture":
		var args struct {
			Type   string         `json:"type"`
			Point  *pointerPoint  `json:"point"`
			End    *pointerPoint  `json:"end"`
			Points []pointerPoint `json:"points"`
		}
		if json.Unmarshal([]byte(input), &args) != nil {
			return nil
		}
		switch strings.ToLower(strings.TrimSpace(args.Type)) {
		case "tap", "double_tap", "long_press", "drag_release":
			if args.Point != nil {
				return finiteNormalizedPoint(args.Point.X.Float64(), args.Point.Y.Float64())
			}
		case "drag_start":
			// drag_start keeps the contact down after moving it away from
			// the pressed point to activate dragging.
			if args.Point != nil {
				end := mnk.DragActivationPoint(args.Point.X.Float64(), args.Point.Y.Float64())
				return finiteNormalizedPoint(end.X, end.Y)
			}
		case "swipe":
			if end, ok := mnk.SwipeEndPoint(input); ok {
				return finiteNormalizedPoint(end.X, end.Y)
			}
		case "tap_sequence":
			var result mnk.TapSequenceResult
			if json.Unmarshal([]byte(actionOutput), &result) == nil && result.Executed > 0 && result.Executed <= len(args.Points) {
				last := args.Points[result.Executed-1]
				return finiteNormalizedPoint(last.X.Float64(), last.Y.Float64())
			}
		}
	}
	return nil
}

// dragStartRecoveredPoint is where an unstable drag_start leaves the pointer
// after it is moved back to its pressed point and released.
func dragStartRecoveredPoint(input string) *normalizedPoint {
	var args struct {
		Type  string        `json:"type"`
		Point *pointerPoint `json:"point"`
	}
	if json.Unmarshal([]byte(input), &args) != nil || !strings.EqualFold(strings.TrimSpace(args.Type), "drag_start") || args.Point == nil {
		return nil
	}
	return finiteNormalizedPoint(args.Point.X.Float64(), args.Point.Y.Float64())
}

func finiteNormalizedPoint(x, y float64) *normalizedPoint {
	if !finiteNormalizedCoordinate(x) || !finiteNormalizedCoordinate(y) {
		return nil
	}
	return &normalizedPoint{X: x, Y: y}
}

// pointerExclusionRects converts normalized pointer positions into squares on
// a screenshot of the given size.
func pointerExclusionRects(bounds image.Rectangle, points ...*normalizedPoint) []image.Rectangle {
	radius := int(math.Round(float64(bounds.Dx()) * pointerExclusionFraction))
	var rects []image.Rectangle
	for _, point := range points {
		if point == nil || radius <= 0 {
			continue
		}
		x := bounds.Min.X + int(math.Round(point.X/1000*float64(max(bounds.Dx()-1, 0))))
		y := bounds.Min.Y + int(math.Round(point.Y/1000*float64(max(bounds.Dy()-1, 0))))
		rects = append(rects, image.Rect(x-radius, y-radius, x+radius+1, y+radius+1))
	}
	return rects
}

// A tap's own feedback (a selection ring, a placed piece, a checkbox mark,
// a popover anchored at the touch) usually fits inside the pointer exclusion
// square, so the block grid cannot see it once the pointer is excluded. The
// iOS pointer, however, is a small disc: on the board it changes luminance
// only within about 12 pixels of its centre, by under 1 in mean absolute
// difference outside that, including while hovering over buttons. So the
// ring around the tap point, outside the pointer disc and inside the
// exclusion square, is compared directly.
//
// Measured on 498x1080 iPhone screenshots: pointer-only moves and hovers,
// over flat backgrounds, a textured board and buttons, changed this ring by
// at most 0.4. Taps with feedback around the touch changed it by 7.7 for a
// thin touch-indicator ring, 20 to 46 for a piece preview, confirm bubble or
// placed piece, and 60 or more for a new page or sheet.
const (
	// tapFeedbackInnerFraction is the pointer disc left out of the ring,
	// as a share of the screenshot width (15 px at 498 px).
	tapFeedbackInnerFraction = 0.03
	// tapFeedbackOuterFraction is the outer edge of the ring (25 px at
	// 498 px), inside the 5% pointer exclusion square.
	tapFeedbackOuterFraction = 0.05
	// tapFeedbackLumaThreshold is the mean absolute luminance difference
	// (0-255) in the ring that counts as feedback at the tap point, ten
	// times the pointer's own and half the faintest feedback measured.
	tapFeedbackLumaThreshold = 4.0
)

// pointerTapPoint returns the single point a tap-like gesture touched, or nil
// for gestures whose feedback is not centred on one point (swipes, drags,
// tap sequences) and for mouse_move, which touches nothing.
func pointerTapPoint(toolName, input string) *normalizedPoint {
	if !strings.EqualFold(strings.TrimSpace(toolName), "touch_gesture") {
		return nil
	}
	var args struct {
		Type  string        `json:"type"`
		Point *pointerPoint `json:"point"`
	}
	if json.Unmarshal([]byte(input), &args) != nil || args.Point == nil {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(args.Type)) {
	case "tap", "double_tap", "long_press":
		return finiteNormalizedPoint(args.Point.X.Float64(), args.Point.Y.Float64())
	}
	return nil
}

// tapFeedbackDiff returns the mean absolute luminance difference between
// before and after in the ring around point that the pointer disc does not
// reach. ok is false when the frames cannot be compared there.
func tapFeedbackDiff(before, after image.Image, point *normalizedPoint) (float64, bool) {
	if before == nil || after == nil || point == nil || before.Bounds() != after.Bounds() {
		return 0, false
	}
	bounds := after.Bounds()
	inner := int(math.Round(float64(bounds.Dx()) * tapFeedbackInnerFraction))
	outer := int(math.Round(float64(bounds.Dx()) * tapFeedbackOuterFraction))
	if inner <= 0 || outer <= inner {
		return 0, false
	}
	b, a := imageLuma(before), imageLuma(after)
	if !b.valid() || !a.valid() {
		return 0, false
	}
	cx := int(math.Round(point.X / 1000 * float64(max(bounds.Dx()-1, 0))))
	cy := int(math.Round(point.Y / 1000 * float64(max(bounds.Dy()-1, 0))))
	sum, count := 0, 0
	for y := cy - outer; y <= cy+outer; y++ {
		if y < 0 || y >= b.height {
			continue
		}
		for x := cx - outer; x <= cx+outer; x++ {
			if x < 0 || x >= b.width || max(absInt(x-cx), absInt(y-cy)) <= inner {
				continue
			}
			sum += absInt(int(b.y[y*b.stride+x]) - int(a.y[y*a.stride+x]))
			count++
		}
	}
	if count == 0 {
		return 0, false
	}
	return float64(sum) / float64(count), true
}
