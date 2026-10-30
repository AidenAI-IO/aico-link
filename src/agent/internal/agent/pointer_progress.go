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
// when the input does not say (for example a swipe given only a direction).
// actionOutput tells how many points of a tap_sequence were tapped.
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
		case "tap", "double_tap", "long_press", "drag_start", "drag_release":
			if args.Point != nil {
				return finiteNormalizedPoint(args.Point.X.Float64(), args.Point.Y.Float64())
			}
		case "swipe":
			if args.End != nil {
				return finiteNormalizedPoint(args.End.X.Float64(), args.End.Y.Float64())
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
