"""Neutral raw UI tools on the environment bridge, without Aiden's tool layer.

The raw layer gives external harnesses the primitives a generic computer-use
agent expects: pixel coordinates in the latest screenshot, one input per call,
and a screenshot after each action taken after a fixed settle delay. It does not
wait for a stable screen, detect screen changes, map semantic shortcuts, or
recover app launches; those belong to the Aiden tool layer being compared.
"""
from __future__ import annotations

import json
import time
from typing import Any
import urllib.error
import urllib.request

from runner.capture import CaptureError, read_environment_frame
from runner.environment_endpoint import EnvironmentEndpoint
from runner.tool_gateway import action_failed


TOOL_LAYER = "raw"
SETTLE_MS = 500
BRIDGE_TOOL_TIMEOUT_SEC = 90
KEYS = ("back", "home", "enter", "backspace")

_POINT = {"x": {"type": "integer", "minimum": 0, "description": "Pixel x in the latest screenshot"},
          "y": {"type": "integer", "minimum": 0, "description": "Pixel y in the latest screenshot"}}


def _tool(name: str, description: str, properties: dict, required: list[str]) -> dict[str, Any]:
    return {"name": name, "description": description,
            "args_schema": {"type": "object", "properties": properties, "required": required,
                            "additionalProperties": False}}


# Descriptions name the device: task prompts may not, and a harness must know
# these tools operate a phone rather than its own host.
RAW_TOOL_CATALOG = [
    _tool("screenshot", "Capture the current screen of the connected mobile device. Other tools take "
          "pixel coordinates in the latest screenshot.", {}, []),
    _tool("tap", "Tap a point on the connected mobile device's touchscreen. Returns a screenshot taken "
          "shortly after the tap.", _POINT, ["x", "y"]),
    _tool("long_press", "Press and hold a point on the connected mobile device's touchscreen. Returns a "
          "screenshot taken shortly after release.",
          {**_POINT, "duration_ms": {"type": "integer", "minimum": 100, "maximum": 5000, "default": 800}},
          ["x", "y"]),
    _tool("swipe", "Swipe on the connected mobile device's touchscreen from a start point to an end point. "
          "Returns a screenshot taken shortly after.",
          {"start_x": _POINT["x"], "start_y": _POINT["y"], "end_x": _POINT["x"], "end_y": _POINT["y"],
           "duration_ms": {"type": "integer", "minimum": 50, "maximum": 5000, "default": 300}},
          ["start_x", "start_y", "end_x", "end_y"]),
    _tool("type_text", "Tap the text field at a point on the connected mobile device and type text into it. "
          "Returns a screenshot.", {**_POINT, "text": {"type": "string", "minLength": 1}}, ["x", "y", "text"]),
    _tool("press_key", "Press one key of the connected mobile device. Returns a screenshot.",
          {"key": {"type": "string", "enum": list(KEYS)}}, ["key"]),
    _tool("wait", "Wait, then return a screenshot of the connected mobile device.",
          {"seconds": {"type": "number", "minimum": 0.1, "maximum": 10}}, ["seconds"]),
]


class RawToolBackend:
    """Translate raw tools into environment bridge tool calls for one route."""

    def __init__(self, environment_url: str, route: str, *, settle_ms: int = SETTLE_MS):
        self.environment_url = environment_url
        self.tools_url = EnvironmentEndpoint(environment_url).base + "/api/tools/"
        self.route = route
        self.settle_ms = settle_ms
        self._size: tuple[int, int] | None = None

    def invoke(self, name: str, tool_input: dict[str, Any]) -> tuple[int, dict]:
        try:
            if name != "screenshot":
                if name == "wait":
                    time.sleep(_number(tool_input, "seconds", 0.1, 10))
                else:
                    self._act(name, tool_input)
                    time.sleep(self.settle_ms / 1000)
            frame = self._frame()
        except (ValueError, CaptureError, OSError) as exc:
            return 200, {"output": json.dumps({"ok": False, "error": str(exc)}), "is_error": True}
        output = {"ok": True, **frame} if name != "screenshot" else frame
        return 200, {"output": json.dumps(output), "is_error": False}

    def _frame(self) -> dict[str, Any]:
        frame = read_environment_frame(self.environment_url, benchmark_task_id=self.route)
        if not frame["width"] or not frame["height"]:
            raise CaptureError("environment bridge did not report the screenshot size")
        self._size = (int(frame["width"]), int(frame["height"]))
        return {"width": self._size[0], "height": self._size[1], "format": frame["format"], "data": frame["data"]}

    def _point(self, tool_input: dict[str, Any], x: str = "x", y: str = "y") -> dict[str, float]:
        if self._size is None:
            self._frame()
        width, height = self._size
        return {"x": round(_number(tool_input, x, 0, width - 1) * 1000 / width, 2),
                "y": round(_number(tool_input, y, 0, height - 1) * 1000 / height, 2)}

    def _act(self, name: str, tool_input: dict[str, Any]) -> None:
        if name == "tap":
            call = ("touch_gesture", {"type": "tap", "point": self._point(tool_input)})
        elif name == "long_press":
            call = ("touch_gesture", {"type": "long_press", "point": self._point(tool_input),
                                      "hold_ms": int(_number(tool_input, "duration_ms", 100, 5000, 800))})
        elif name == "swipe":
            call = ("touch_gesture", {"type": "swipe", "start": self._point(tool_input, "start_x", "start_y"),
                                      "end": self._point(tool_input, "end_x", "end_y"),
                                      "duration_ms": int(_number(tool_input, "duration_ms", 50, 5000, 300))})
        elif name == "type_text":
            text = tool_input.get("text")
            if not isinstance(text, str) or not text:
                raise ValueError("text must be a non-empty string")
            call = ("enter_text", {"text": text, "focus": self._point(tool_input)})
        elif name == "press_key":
            if tool_input.get("key") not in KEYS:
                raise ValueError(f"key must be one of {', '.join(KEYS)}")
            call = ("keyboard_tap", {"keys": [tool_input["key"]]})
        else:
            raise ValueError(f"Unknown raw tool: {name}")
        self._bridge_tool(*call)

    def _bridge_tool(self, name: str, tool_input: dict[str, Any]) -> None:
        request = urllib.request.Request(self.tools_url + name, data=json.dumps({"input": tool_input}).encode(),
                                         headers={"Content-Type": "application/json",
                                                  "benchmark-task-id": self.route}, method="POST")
        try:
            with urllib.request.urlopen(request, timeout=BRIDGE_TOOL_TIMEOUT_SEC) as response:
                result = json.load(response)
        except urllib.error.HTTPError as exc:
            raise ValueError(f"{name} failed HTTP {exc.code}: {exc.read()[:200]!r}") from exc
        output = result.get("output")
        if result.get("is_error") or action_failed(output):
            raise ValueError(f"{name} failed: {_summary(output)}")


def _number(tool_input: dict[str, Any], key: str, low: float, high: float, default: float | None = None) -> float:
    value = tool_input.get(key, default)
    if type(value) not in (int, float) or not low <= value <= high:
        raise ValueError(f"{key} must be a number in [{low}, {high}]")
    return value


def _decoded(output: Any) -> Any:
    try:
        return json.loads(output) if isinstance(output, str) else output
    except ValueError:
        return output


def _summary(output: Any) -> str:
    """Error text without any screenshot bytes the bridge attached."""
    decoded = _decoded(output)
    if isinstance(decoded, dict):
        decoded = {key: value for key, value in decoded.items() if key != "data"}
    return (decoded if isinstance(decoded, str) else json.dumps(decoded, ensure_ascii=False))[:500]
