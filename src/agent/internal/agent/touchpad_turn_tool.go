package agent

import (
	"context"
	"encoding/json"
	"strings"

	"aico-agent/internal/logging"
)

const (
	// touchpadOrientationSetType reports where the last touch reacted.
	touchpadOrientationSetType = "orientation_set"
	// touchpadTurnCentreNote follows a check touch at the screen centre,
	// which lands on the same spot with either turn.
	touchpadTurnCentreNote = `ok. This mirror_ok touch was at the screen centre, which is its own mirror (1000-x, 1000-y), so its reaction does not show the turn direction; do not call orientation_set for it. To check the direction, touch an off-centre point whose mirror is harmless, or use a tap_sequence that first taps the centre to reveal controls and then taps an off-centre one.`
	// touchpadTurnPendingNote follows an unconfirmed landscape touch.
	touchpadTurnPendingNote = `ok. This mirror_ok touch on the landscape iPhone checks its turn direction. The red ring marks where this touch was aimed; the blue ring marks its mirror (1000-x, 1000-y), where it lands if the phone is turned the other way. For a tap_sequence the rings mark its last tap. Find the touch's reaction in the screenshot. If it reacted at the red ring, call touch_gesture {"type":"orientation_set","landed":"expected"}. If it reacted at the blue ring, call {"type":"orientation_set","landed":"mirror"} and then redo the intended touch. If no reaction is visible, continue with taps whose mirror is also harmless.`
)

// handleTouchpadTurn runs before every touch_gesture on the iOS touchpad. It
// sets the turn for the latest screenshot and answers orientation_set and
// orientation_probe. While the landscape direction is unconfirmed it lets
// through only point touches the model marked mirror_ok, because they may land
// at their mirror. check reports a mirror_ok touch on a landscape screen,
// which runs as a direction check.
func (t *TouchGestureTool) handleTouchpadTurn(ctx context.Context, input string) (out string, handled, check bool) {
	var args struct {
		Type     string          `json:"type"`
		Point    *pointerPoint   `json:"point"`
		Landed   string          `json:"landed"`
		MirrorOK bool            `json:"mirror_ok"`
		Actions  json.RawMessage `json:"actions"`
	}
	_ = json.Unmarshal([]byte(input), &args)
	gestureType := strings.ToLower(strings.TrimSpace(args.Type))
	if len(args.Actions) > 0 && string(args.Actions) != "null" {
		// The adapter runs actions in place of type, so the touch checked
		// here would not be the one sent.
		gestureType = "actions"
	}
	if t.turn == nil {
		if gestureType == touchpadOrientationProbeType || gestureType == touchpadOrientationSetType {
			return toolErrorResultf(ctx, CodeInvalidArguments, "%s is only used with the iOS touchpad", gestureType), true, false
		}
		return "", false, false
	}
	pending := t.turn.prepare()
	switch gestureType {
	case "drag_release":
		// A held contact must always be releasable; it was pressed with the
		// turn now in use.
		return "", false, false
	case touchpadOrientationSetType:
		return t.setTouchpadTurn(ctx, args.Landed), true, false
	case touchpadOrientationProbeType:
		return t.probeTouchpadTurn(ctx, args.Point, pending), true, false
	}
	checkable := gestureType == "tap" || gestureType == "double_tap" || gestureType == "long_press" || gestureType == "tap_sequence"
	if !pending {
		// A confirmed direction can still be checked: the phone may have
		// turned straight to the other landscape direction.
		return "", false, checkable && args.MirrorOK && t.turn.landscapeActive()
	}
	switch {
	case checkable:
		if args.MirrorOK {
			return "", false, true
		}
		return toolErrorResultf(ctx, CodeInvalidArguments,
			"not sent: the iPhone is in landscape and its turn direction is not confirmed, so this touch may land at its mirror (1000-x, 1000-y) instead. "+
				"Check that the mirror point of every touched point is also harmless and resend with \"mirror_ok\":true, or first tap a point whose mirror is harmless; "+
				"the screen centre (500,500) is its own mirror. When the target only appears after a tap, such as video controls that hide again, "+
				"send both taps in one tap_sequence with \"mirror_ok\":true, for example points [{\"x\":500,\"y\":500}, target]."), true, false
	default:
		return toolErrorResultf(ctx, CodeInvalidArguments,
			"not sent: the iPhone is in landscape and its turn direction is not confirmed, so a %s would run mirrored. "+
				"First confirm the direction with a tap whose mirror (1000-x, 1000-y) is harmless, sent with \"mirror_ok\":true, then orientation_set.", gestureType), true, false
	}
}

// landscapeTurnCheck tells the post-action observation that input is a
// direction check: a mirror_ok touch on a landscape screen, required while
// the direction is unconfirmed. It is captured early and its mirror marked.
func (t *TouchGestureTool) landscapeTurnCheck(input string) bool {
	if t == nil || t.turn == nil || !turnCheckTouchInput(input) {
		return false
	}
	// prepare first: the observation runs before Call, and the first touch
	// on a new landscape screen must already see the landscape turn.
	t.turn.prepare()
	return t.turn.landscapeActive()
}

func (t *TouchGestureTool) setTouchpadTurn(ctx context.Context, landed string) string {
	var mirrored bool
	switch strings.ToLower(strings.TrimSpace(landed)) {
	case "expected":
	case "mirror":
		mirrored = true
	default:
		return toolErrorResultf(ctx, CodeInvalidArguments, `orientation_set needs "landed": "expected" or "mirror"`)
	}
	turn, err := t.turn.confirm(mirrored)
	if err != nil {
		return toolErrorResultf(ctx, CodeInvalidArguments, "%v", err)
	}
	logging.Infof("agent", "touchpad_turn", "landscape orientation set: %s (landed=%s)", turn, landed)
	note := "touches now land where they are aimed"
	if mirrored {
		note = "later touches are turned the other way; redo the touch that reacted at its mirror"
	}
	return `{"ok":true,"orientation":"` + turn.String() + `","note":"` + note + `"}`
}

func (t *TouchGestureTool) probeTouchpadTurn(ctx context.Context, point *pointerPoint, pending bool) string {
	if !pending {
		return `{"ok":true,"orientation":"` + t.turn.provider.TouchpadTurn().String() + `","probed":false,"note":"no unconfirmed landscape direction; no touch was sent"}`
	}
	if point == nil {
		return toolErrorResultf(ctx, CodeInvalidArguments, "orientation_probe needs point")
	}
	turn, err := t.turn.probe(ctx, normalizedPoint{X: point.X.Float64(), Y: point.Y.Float64()})
	if err != nil {
		// The probe pressed with another turn; go back to the one in use.
		t.turn.prepare()
		logging.Warnf("agent", "touchpad_turn", "orientation probe failed: %v", err)
		return toolErrorResultf(ctx, CodeToolExecutionFailed,
			"%v. Confirm the direction with an ordinary mirror_ok tap instead, or probe a control that visibly highlights while pressed.", err)
	}
	t.turn.remember(turn)
	logging.Infof("agent", "touchpad_turn", "landscape orientation learned: %s", turn)
	return `{"ok":true,"orientation":"` + turn.String() + `","probed":true,"note":"the probe pressed and released the chosen point once"}`
}
