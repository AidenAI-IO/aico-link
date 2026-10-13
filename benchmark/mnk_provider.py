from __future__ import annotations

import math
from collections.abc import Callable
from typing import Any


class MNKRequestError(ValueError):
    pass


ToolCall = tuple[str, dict[str, Any]]
ToolInvoker = Callable[[str, dict[str, Any]], dict[str, Any]]
MAX_MOTION_DURATION_MS = 10_000


def execute_mnk_request(
    payload: dict[str, Any], invoke: ToolInvoker
) -> tuple[int, dict[str, Any]]:
    try:
        calls = mnk_tool_calls(payload)
    except MNKRequestError as exc:
        return 400, {"error": str(exc)}

    for tool_name, tool_input in calls:
        result = invoke(tool_name, tool_input)
        if bool(result.get("is_error", False)):
            message = str(result.get("output") or result.get("error") or "MNK operation failed")
            return 500, {"error": message}
    return 200, {"success": True}


def mnk_tool_calls(payload: dict[str, Any]) -> list[ToolCall]:
    operation = payload.get("operation")
    if not isinstance(operation, str) or not operation.strip():
        raise MNKRequestError("operation is required")
    operation = operation.strip().lower()

    if operation == "click":
        params = _params(payload, "click")
        point = _point(params.get("x"), params.get("y"))
        hold_ms = _non_negative_int(params.get("hold_ms", 0), "hold_ms")
        _touch_button(params.get("button", "left"))
        gesture: dict[str, Any] = {
            "type": "long_press" if hold_ms > 0 else "tap",
            "point": point,
        }
        if hold_ms > 0:
            gesture["hold_ms"] = hold_ms
        return [("touch_gesture", gesture)]

    if operation == "double_click":
        params = _params(payload, "double_click")
        _touch_button(params.get("button", "left"))
        return [("touch_gesture", {
            "type": "double_tap",
            "point": _point(params.get("x"), params.get("y")),
        })]

    if operation in {"swipe", "drag"}:
        params = _params(payload, operation)
        path = _path(params.get("path"))
        _touch_button(params.get("button", "left"))
        if len(path) != 2:
            raise MNKRequestError(
                f"{operation} path must contain exactly 2 points; multi-point paths are unsupported"
            )
        start, end = path
        gesture = {
            "type": operation,
            "start": {"x": start[0], "y": start[1]},
            "end": {"x": end[0], "y": end[1]},
        }
        duration_ms = _non_negative_int(params.get("duration_ms", 0), "duration_ms")
        if duration_ms > MAX_MOTION_DURATION_MS:
            raise MNKRequestError(f"duration_ms must be in range [0, {MAX_MOTION_DURATION_MS}]")
        # The Go provider uses zero/omitted duration for backend defaults.
        if duration_ms > 0:
            gesture["duration_ms"] = duration_ms
        return [("touch_gesture", gesture)]

    if operation == "keyboard_text":
        params = _params(payload, operation)
        text = params.get("text")
        if not isinstance(text, str) or not text or not text.isascii():
            raise MNKRequestError("keyboard_text requires non-empty ASCII text")
        return [("keyboard_text", {"text": text})]

    if operation in {"drag_start", "drag_release"}:
        params = _params(payload, operation)
        _touch_button(params.get("button", "left"))
        return [("touch_gesture", {
            "type": operation,
            "point": _point(params.get("x"), params.get("y")),
        })]

    if operation == "keypress":
        params = _params(payload, "keypress")
        keys = params.get("keys")
        if not isinstance(keys, list) or not keys or not all(isinstance(key, str) and key.strip() for key in keys):
            raise MNKRequestError("keypress keys must be a non-empty string array")
        return [("keyboard_tap", {"keys": keys})]

    if operation == "move":
        params = _params(payload, "move")
        point = _point(params.get("x"), params.get("y"))
        return [("mouse_move", point)]

    if operation == "scroll":
        params = _params(payload, "scroll")
        scroll_x = _int(params.get("scroll_x", 0), "scroll_x")
        scroll_y = _int(params.get("scroll_y", 0), "scroll_y")
        if scroll_x != 0:
            raise MNKRequestError("horizontal scroll is unsupported")
        return [("mouse_scroll", {"delta": scroll_y})]

    raise MNKRequestError(f"unknown operation: {operation!r}")


def _params(payload: dict[str, Any], name: str) -> dict[str, Any]:
    value = payload.get(name)
    if not isinstance(value, dict):
        raise MNKRequestError(f"{name} params required")
    return value


def _point(x: Any, y: Any) -> dict[str, float]:
    return {"x": _coordinate(x, "x"), "y": _coordinate(y, "y")}


def _path(value: Any) -> list[tuple[float, float]]:
    if not isinstance(value, list) or len(value) < 2:
        raise MNKRequestError("path must contain at least 2 points")
    path: list[tuple[float, float]] = []
    for index, point in enumerate(value):
        if not isinstance(point, (list, tuple)) or len(point) != 2:
            raise MNKRequestError(f"path point {index} must contain x and y")
        path.append((_coordinate(point[0], f"path[{index}].x"), _coordinate(point[1], f"path[{index}].y")))
    return path


def _coordinate(value: Any, name: str) -> float:
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise MNKRequestError(f"{name} must be a number")
    number = float(value)
    if not math.isfinite(number) or number < 0 or number > 1000:
        raise MNKRequestError(f"{name} must be in range [0, 1000]")
    return number


def _button(value: Any) -> str:
    if not isinstance(value, str):
        raise MNKRequestError("button must be a string")
    button = value.strip().lower() or "left"
    if button not in {"left", "right", "middle"}:
        raise MNKRequestError("button must be left, right, or middle")
    return button


def _touch_button(value: Any) -> str:
    button = _button(value)
    if button != "left":
        raise MNKRequestError(f"touch operations do not support button {button!r}")
    return button


def _int(value: Any, name: str) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise MNKRequestError(f"{name} must be an integer")
    return value


def _non_negative_int(value: Any, name: str) -> int:
    number = _int(value, name)
    if number < 0:
        raise MNKRequestError(f"{name} must be non-negative")
    return number
