package agent

import (
	"context"
	"encoding/json"
	"strings"

	"aico-agent/internal/logging"
)

// handleTouchpadTurn runs before every touch_gesture on the iOS touchpad. In
// portrait, or in landscape once the direction is known, it sets the turn and
// lets the gesture through. In landscape with an unknown direction it runs an
// orientation_probe, or refuses any other gesture with instructions to probe
// first, because that gesture would land turned by a quarter.
func (t *TouchGestureTool) handleTouchpadTurn(ctx context.Context, input string) (string, bool) {
	var args struct {
		Type  string        `json:"type"`
		Point *pointerPoint `json:"point"`
	}
	_ = json.Unmarshal([]byte(input), &args)
	isProbe := strings.EqualFold(strings.TrimSpace(args.Type), touchpadOrientationProbeType)
	if t.turn == nil {
		if isProbe {
			return toolErrorResultf(ctx, CodeInvalidArguments, "orientation_probe is only used with the iOS touchpad; use tap"), true
		}
		return "", false
	}
	needsProbe := t.turn.prepare()
	if !isProbe {
		if !needsProbe {
			return "", false
		}
		return toolErrorResultf(ctx, CodeInvalidArguments,
			"not sent: the iPhone is in landscape and which way it is turned is not known yet, so this touch would land in the wrong place. "+
				`First call touch_gesture {"type":"orientation_probe","point":{...}} on a control that is safe to press and shows a press highlight, `+
				"such as a key or button that does not navigate, delete, send, or pay. The point mirrored through the screen centre (1000-x, 1000-y) "+
				"must also be safe to touch. Then repeat this gesture."), true
	}
	if !needsProbe {
		return `{"ok":true,"orientation":"` + t.turn.provider.TouchpadTurn().String() + `","probed":false,"note":"orientation already known; no touch was sent"}`, true
	}
	if args.Point == nil {
		return toolErrorResultf(ctx, CodeInvalidArguments, "orientation_probe needs point"), true
	}
	point := normalizedPoint{X: args.Point.X.Float64(), Y: args.Point.Y.Float64()}
	turn, err := t.turn.probe(ctx, point)
	if err != nil {
		logging.Warnf("agent", "touchpad_turn", "orientation probe failed: %v", err)
		return toolErrorResultf(ctx, CodeToolExecutionFailed,
			"%v. Pick a different control that visibly highlights while pressed, whose mirrored point (1000-x, 1000-y) is also safe, and probe again.", err), true
	}
	t.turn.remember(turn)
	t.turn.provider.SetTouchpadTurn(turn)
	logging.Infof("agent", "touchpad_turn", "landscape orientation learned: %s", turn)
	return `{"ok":true,"orientation":"` + turn.String() + `","probed":true,"note":"the probe pressed and released the chosen point once"}`, true
}
