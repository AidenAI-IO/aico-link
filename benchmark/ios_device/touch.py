"""Validate complete contact programs before sending any input to WDA."""
from mnk_provider import _point


def validate_touch_actions(actions):
    if not isinstance(actions, list) or not 1 <= len(actions) <= 128:
        raise ValueError("touch_actions requires 1–128 actions")
    active, total = False, 0
    for action in actions:
        if not isinstance(action, dict) or action.get("button", "left") not in ("", "left"):
            raise ValueError("touch actions require objects and the left button")
        kind = action.get("action")
        duration = action.get("ms", 0)
        if isinstance(duration, bool) or not isinstance(duration, int) or not 0 <= duration <= 10000:
            raise ValueError("touch action ms must be in [0, 10000]")
        total += duration
        if total > 10000:
            raise ValueError("touch program must not exceed 10000 ms")
        point = action.get("point")
        if point is not None:
            if not isinstance(point, dict):
                raise ValueError("touch point requires x and y")
            _point(point.get("x"), point.get("y"))
        if kind == "touch_down":
            if active or point is None:
                raise ValueError("touch_down requires a point and no active contact")
            active = True
        elif kind == "move_to":
            if not active or point is None:
                raise ValueError("move_to requires a point and an active contact")
        elif kind == "touch_up":
            if not active:
                raise ValueError("touch_up requires an active contact")
            active = False
        elif kind != "wait":
            raise ValueError("unsupported touch action")
    if active:
        raise ValueError("touch program must end with touch_up")


def wda_actions(actions, size):
    validate_touch_actions(actions)
    result = []
    for action in actions:
        kind, point = action["action"], action.get("point")
        duration = action.get("ms", 0)
        if point is not None and kind != "wait":
            result.append({"type": "pointerMove", "origin": "viewport",
                           "x": min(point["x"] * size["width"] / 1000, size["width"] - 1),
                           "y": min(point["y"] * size["height"] / 1000, size["height"] - 1),
                           "duration": duration if kind == "move_to" else 0})
        if kind in ("touch_down", "touch_up"):
            result.append({"type": "pointerDown" if kind == "touch_down" else "pointerUp", "button": 0})
        if kind == "wait" or (kind in ("touch_down", "touch_up") and duration):
            result.append({"type": "pause", "duration": duration})
    return {"actions": [{"type": "pointer", "id": "finger", "parameters": {"pointerType": "touch"}, "actions": result}]}
