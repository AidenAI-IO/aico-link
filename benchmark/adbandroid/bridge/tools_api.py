"""Unified /api/tools endpoint for the ADB Android environment bridge.

Mirrors the input semantics of benchmark/mobilegym/bridge/tools_api.py (the Go
agent defines the tool schemas and forwards matching calls verbatim), then
executes each tool through adb instead of a MobileGym env.

Differences from the MobileGym bridge, kept deliberately small:
- quick_action executes Android navigation through keyevents / `cmd statusbar`
  / activity intents.
"""

from __future__ import annotations

import json
import math
import time
import xml.etree.ElementTree as ET
from http.server import BaseHTTPRequestHandler
from typing import Any, Callable

from .adb import (
    ADBCommandError,
    KEYCODE_APP_SWITCH,
    KEYCODE_DEL,
    KEYCODE_ENTER,
    KEYCODE_TAB,
    unsupported_adb_text_chars,
)
from .protocol import encode_screenshot
from .state import (
    ADBBridgeState,
    NoBridgeEnvAvailableError,
    benchmark_task_id_from_headers,
)


DEFAULT_SWIPE_SPEED = 2500.0
MAX_SWIPE_DURATION_MS = 10_000
MAX_SWIPE_HOLD_MS = 10_000
MAX_SWIPE_STEPS = 1_000
ADB_RESERVED_QUICK_ACTIONS = {
    "browser_refresh",
    "browser_new_tab",
    "copy",
    "cut",
    "delete_backward",
    "find",
    "paste",
    "select_all",
    "undo",
    "spotlight_search",
    "search_launch_app",
}
DEFAULT_DOUBLE_TAP_PAUSE_MS = 120
DEFAULT_LONG_PRESS_MS = 500
# Upper bound for caller-supplied durations (swipe/long_press/pause). All
# actions run under the single shared device lock, and the adb subprocess
# timeout scales with the gesture duration — an unclamped value from a bad
# LLM tool call could block every other request against the device.
MAX_ACTION_DURATION_MS = 10_000
MAX_REQUEST_BODY_BYTES = 10 * 1024 * 1024  # 10MB
FOCUS_SETTLE_SEC = 0.3
# Wait after an action before the post-action screenshot: adb commands (e.g.
# `am start`, keyevents) return before the UI transition finishes, so an
# immediate screencap would show the previous screen.
DEFAULT_ACTION_SETTLE_SEC = 0.6


class ADBToolsAPIHandler:
    """Handler for the ADB bridge /api/tools endpoint."""

    def __init__(
        self,
        state: ADBBridgeState,
        request_timeout_sec: float = 30,
        action_settle_sec: float = DEFAULT_ACTION_SETTLE_SEC,
    ):
        self.state = state
        self.request_timeout_sec = request_timeout_sec
        self.action_settle_sec = max(0.0, action_settle_sec)

    def handle_request(self, handler: BaseHTTPRequestHandler, path: str) -> None:
        """Route /api/tools requests to catalog or invocation."""
        if path == "/api/tools":
            if handler.command == "GET":
                self._handle_catalog(handler)
            else:
                handler.send_error(405, "Method not allowed")
        elif path.startswith("/api/tools/"):
            if handler.command == "POST":
                tool_name = path[len("/api/tools/"):]
                self._handle_invoke(handler, tool_name)
            else:
                handler.send_error(405, "Method not allowed")
        else:
            handler.send_error(404, "Not found")

    def _handle_catalog(self, handler: BaseHTTPRequestHandler) -> None:
        """GET /api/tools - return tool catalog."""
        enter_text_focus_schema = {
            "type": "object",
            "additionalProperties": False,
            "properties": {
                "x": {"type": "number"},
                "y": {"type": "number"},
            },
            "required": ["x", "y"],
            "description": "Input field coordinates in normalized 0-1000 units.",
        }
        tools = [
            {
                "name": "touch_gesture",
                "description": "Perform touch gestures on the Android device (tap, swipe, drag, long_press, etc.).",
                "args_schema": {
                    "type": "object",
                    "additionalProperties": False,
                    "properties": {
                        "type": {
                            "type": "string",
                            "enum": ["tap", "double_tap", "long_press", "swipe", "drag"],
                            "description": "Type of gesture to perform",
                        },
                        "point": {
                            "type": "object",
                            "additionalProperties": False,
                            "properties": {"x": {"type": "number"}, "y": {"type": "number"}},
                            "required": ["x", "y"],
                            "description": "Point for tap/double_tap/long_press (normalized 0-1000)",
                        },
                        "start": {
                            "type": "object",
                            "additionalProperties": False,
                            "properties": {"x": {"type": "number"}, "y": {"type": "number"}},
                            "required": ["x", "y"],
                            "description": "Start point for swipe/drag",
                        },
                        "end": {
                            "type": "object",
                            "additionalProperties": False,
                            "properties": {"x": {"type": "number"}, "y": {"type": "number"}},
                            "required": ["x", "y"],
                            "description": "End point for swipe/drag",
                        },
                        "duration_ms": {"type": "integer", "minimum": 1, "maximum": MAX_SWIPE_DURATION_MS, "description": "Optional swipe duration in milliseconds"},
                        "hold_before_ms": {"type": "integer", "minimum": 0, "maximum": MAX_SWIPE_HOLD_MS, "description": "Optional dwell after pressing before a swipe begins."},
                        "hold_after_ms": {"type": "integer", "minimum": 0, "maximum": MAX_SWIPE_HOLD_MS, "description": "Optional dwell at the destination before release."},
                        "hold_ms": {"type": "integer", "minimum": 0, "maximum": 10000, "description": "Tap or long-press hold duration in milliseconds."},
                        "pause_ms": {"type": "integer", "minimum": 0, "maximum": 10000, "description": "Pause between taps for double_tap."},
                        "steps": {"type": "integer", "minimum": 1, "maximum": MAX_SWIPE_STEPS, "description": "Accepted for compatibility but ignored by this backend; adb device.swipe uses duration-based movement."},
                        "direction": {"type": "string", "enum": ["up", "down", "left", "right"], "description": "Swipe direction when end is omitted"},
                        "speed": {"type": "number", "exclusiveMinimum": 0, "description": "Optional swipe speed in normalized coordinate units per second; defaults to 2500"},
                        "button": {"type": "string", "enum": ["left", "right", "middle"]},
                    },
                    "required": ["type"],
                },
            },
            {
                "name": "keyboard_text",
                "description": "Type text on the Android device via adb input text (US-keyboard ASCII only).",
                "args_schema": {
                    "type": "object",
                    "additionalProperties": False,
                    "properties": {
                        "text": {"type": "string", "description": "Text to type"},
                    },
                    "required": ["text"],
                },
            },
            {
                "name": "keyboard_tap",
                "description": "Press keyboard keys on the Android device (e.g., enter, back, home).",
                "args_schema": {
                    "type": "object",
                    "additionalProperties": False,
                    "properties": {
                        "keys": {
                            "type": "array",
                            "minItems": 1,
                            "maxItems": 6,
                            "items": {"type": "string"},
                            "description": "Keys to press (e.g., ['enter'], ['meta', 'h'])",
                        },
                        "hold_ms": {"type": "integer", "minimum": 0, "maximum": 10000, "description": "Accepted but ignored on the adb path: adb `input keyevent` cannot hold a key for an arbitrary duration."},
                    },
                    "required": ["keys"],
                },
            },
            {
                "name": "enter_text",
                "description": "Enter target text into an input field on the Android device: taps the focus point, types via adb input text, verifies the editable field with Android UIAutomator, and returns a post-action screenshot.",
                "args_schema": {
                    "type": "object",
                    "additionalProperties": False,
                    "properties": {
                        "text": {"type": "string", "description": "Exact text that must appear in the field."},
                        "focus": enter_text_focus_schema,
                    },
                    "required": ["text", "focus"],
                },
            },
            {
                "name": "mouse_move",
                "description": "Move the pointer. Android has no hover state, so this is accepted as a no-op and returns a screenshot.",
                "args_schema": {
                    "type": "object",
                    "additionalProperties": False,
                    "properties": {
                        "x": {"type": "number"},
                        "y": {"type": "number"},
                    },
                    "required": ["x", "y"],
                },
            },
            {
                "name": "mouse_scroll",
                "description": "Scroll using an adb swipe approximation.",
                "args_schema": {
                    "type": "object",
                    "additionalProperties": False,
                    "properties": {"delta": {"type": "integer", "minimum": -127, "maximum": 127}},
                    "required": ["delta"],
                },
            },
            {
                "name": "quick_action",
                "description": "Execute common Android navigation actions such as back, home, or open_settings.",
                "args_schema": {
                    "type": "object",
                    "additionalProperties": False,
                    "properties": {
                        "action": {"type": "string"},
                        "list": {"type": "boolean"},
                        "alternative": {"type": "boolean"},
                        "alternative_index": {"type": "integer", "minimum": 1},
                    },
                    "anyOf": [
                        {"required": ["action"]},
                        {"required": ["list"], "properties": {"list": {"const": True}}},
                    ],
                },
            },
        ]

        response = {"tools": tools}
        self._send_json(handler, 200, response)

    def _handle_invoke(self, handler: BaseHTTPRequestHandler, tool_name: str) -> None:
        """POST /api/tools/{tool_name} - invoke a tool."""
        try:
            content_length = int(handler.headers.get("Content-Length", "0") or "0")
        except ValueError:
            self._send_json(handler, 400, {"error": "bad_header", "output": "invalid Content-Length", "is_error": True})
            return
        if content_length < 0:
            self._send_json(handler, 400, {"error": "bad_header", "output": "Content-Length must be non-negative", "is_error": True})
            return
        if content_length > MAX_REQUEST_BODY_BYTES:
            self._send_json(handler, 413, {"error": "request_too_large", "output": f"request body exceeds {MAX_REQUEST_BODY_BYTES} bytes", "is_error": True})
            return

        raw_body = handler.rfile.read(content_length) if content_length else b"{}"
        try:
            request_body = json.loads(raw_body.decode("utf-8"))
        except json.JSONDecodeError:
            self._send_json(handler, 400, {"error": "bad_json", "output": "invalid JSON body", "is_error": True})
            return
        if not isinstance(request_body, dict):
            self._send_json(handler, 400, {"error": "bad_json", "output": "JSON body must be an object", "is_error": True})
            return

        # Decode input (matches Go agent's decodeToolInvokeInput)
        raw_input = _decode_tool_input(request_body)

        # Parse input as JSON (tools expect JSON object input). keyboard_text
        # intentionally mirrors the Go tool's plain-text compatibility fallback.
        try:
            if raw_input.strip() in ("", "null", "{}"):
                tool_input = {}
            else:
                parsed_input = json.loads(raw_input)
                if tool_name == "keyboard_text" and isinstance(parsed_input, str):
                    tool_input = {"text": parsed_input}
                elif isinstance(parsed_input, dict):
                    tool_input = parsed_input
                else:
                    self._send_json(
                        handler,
                        400,
                        {"error": "invalid_input", "output": "tool input must be a JSON object", "is_error": True},
                    )
                    return
        except json.JSONDecodeError:
            if tool_name == "keyboard_text":
                tool_input = {"text": raw_input.strip()}
            else:
                self._send_json(
                    handler,
                    400,
                    {"error": "invalid_input", "output": f"tool input must be valid JSON: {raw_input}", "is_error": True},
                )
                return

        if not isinstance(tool_input, dict):
            self._send_json(
                handler,
                400,
                {"error": "invalid_input", "output": "tool input must be a JSON object", "is_error": True},
            )
            return

        # Single-device routing: empty task id falls through to the one state;
        # a different owning task id gets 429 (mirrors the MobileGym router).
        task_id = benchmark_task_id_from_headers(handler.headers)
        try:
            self.state.check_task_access(task_id)
        except NoBridgeEnvAvailableError as exc:
            self._send_json(
                handler,
                429,
                {"error": "no_bridge_env_available", "output": str(exc), "is_error": True},
            )
            return
        if not self.state.active_episode_id:
            self._send_json(
                handler,
                409,
                {"error": "no_active_episode", "output": "no active episode; call /api/setup first", "is_error": True},
            )
            return

        # Execute tool
        started_at = time.time()
        try:
            result = self._submit_tool_call(tool_name, tool_input)
            duration_ms = int((time.time() - started_at) * 1000)

            response = {
                "tool": {"name": tool_name},
                "raw_input": raw_input,
                "output": result.get("output", "ok"),
                "is_error": result.get("is_error", False),
                "duration_ms": duration_ms,
            }
            if result.get("error"):
                response["error"] = result["error"]

            self._send_json(handler, 200, response)

        except ADBCommandError as exc:
            duration_ms = int((time.time() - started_at) * 1000)
            self._send_json(
                handler,
                500,
                {
                    "output": str(exc),
                    "is_error": True,
                    "error": "adb_error",
                    "duration_ms": duration_ms,
                },
            )
        except Exception as exc:
            duration_ms = int((time.time() - started_at) * 1000)
            self._send_json(
                handler,
                500,
                {
                    "output": str(exc),
                    "is_error": True,
                    "error": str(exc),
                    "duration_ms": duration_ms,
                },
            )

    def _submit_tool_call(self, tool_name: str, tool_input: dict[str, Any]) -> dict[str, Any]:
        """Dispatch tool call to the ADB device."""
        unknown = _unknown_tool_fields(tool_name, tool_input)
        if unknown:
            return {"output": f"error: unknown fields: {unknown!r}", "is_error": True}
        if tool_name == "touch_gesture":
            return self._call_touch_gesture(tool_input)
        elif tool_name == "keyboard_text":
            return self._call_keyboard_text(tool_input)
        elif tool_name == "keyboard_tap":
            return self._call_keyboard_tap(tool_input)
        elif tool_name == "enter_text":
            return self._call_enter_text(tool_input)
        elif tool_name == "mouse_move":
            return self._call_mouse_move(tool_input)
        elif tool_name == "mouse_scroll":
            return self._call_mouse_scroll(tool_input)
        elif tool_name == "quick_action":
            return self._call_quick_action(tool_input)
        else:
            return {"output": f"unknown tool: {tool_name}", "is_error": True, "error": "unknown_tool"}

    def _call_touch_gesture(
        self,
        tool_input: dict[str, Any],
        *,
        log_tool_name: str = "touch_gesture",
        log_tool_input: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        raw_gesture_type = tool_input.get("type", "")
        if not isinstance(raw_gesture_type, str):
            return {"output": "error: type must be a string", "is_error": True}
        gesture_type = raw_gesture_type.strip().lower()
        if not gesture_type:
            return {"output": "error: type is required", "is_error": True}

        device = self.state.device
        log_input = tool_input if log_tool_input is None else log_tool_input

        try:
            if gesture_type in ("tap", "double_tap", "long_press"):
                width, height = device.screen_size()
                point = _normalized_point_arg(tool_input)
                x, y = _to_pixels(point, width, height)
                if gesture_type == "tap":
                    return self._execute_device(
                        lambda: device.tap(x, y),
                        tool_name=log_tool_name,
                        tool_input=log_input,
                        adb_summary=f"input tap {x} {y}",
                    )
                if gesture_type == "double_tap":
                    pause_ms = _duration_ms_arg(
                        tool_input.get("pause_ms"), DEFAULT_DOUBLE_TAP_PAUSE_MS, minimum=0
                    )

                    def double_tap() -> None:
                        device.tap(x, y)
                        time.sleep(max(0, pause_ms) / 1000)
                        device.tap(x, y)

                    return self._execute_device(
                        double_tap,
                        tool_name=log_tool_name,
                        tool_input=log_input,
                        adb_summary=f"input tap {x} {y} (x2)",
                    )
                hold_ms = _duration_ms_arg(
                    tool_input.get("duration_ms") or tool_input.get("hold_ms"),
                    DEFAULT_LONG_PRESS_MS,
                )
                return self._execute_device(
                    lambda: device.swipe(x, y, x, y, hold_ms),
                    tool_name=log_tool_name,
                    tool_input=log_input,
                    adb_summary=f"input swipe {x} {y} {x} {y} {hold_ms} (long_press)",
                )
            elif gesture_type in ("swipe", "drag"):
                width, height = device.screen_size()
                start = _normalized_point_arg(
                    tool_input,
                    field="start",
                    x_key="start_x",
                    y_key="start_y",
                )
                if gesture_type == "swipe":
                    start, end, duration_ms = _resolve_swipe(start, tool_input)
                else:
                    end = _normalized_point_arg(
                        tool_input,
                        field="end",
                        x_key="end_x",
                        y_key="end_y",
                    )
                    duration_ms = _duration_ms_arg(tool_input.get("duration_ms"), 700)
                timing = _swipe_timing_options(tool_input)
                x1, y1 = _to_pixels(start, width, height)
                x2, y2 = _to_pixels(end, width, height)
                def swipe() -> None:
                    if timing.get("hold_before_ms", 0):
                        time.sleep(timing["hold_before_ms"] / 1000)
                    device.swipe(x1, y1, x2, y2, duration_ms)
                    if timing.get("hold_after_ms", 0):
                        time.sleep(timing["hold_after_ms"] / 1000)
                return self._execute_device(
                    swipe,
                    tool_name=log_tool_name,
                    tool_input=log_input,
                    adb_summary=f"input swipe {x1} {y1} {x2} {y2} {duration_ms}",
                )
            else:
                return {"output": f"error: unsupported gesture type: {gesture_type}", "is_error": True}
        except (TypeError, ValueError) as exc:
            return {"output": f"error: {exc}", "is_error": True}

    def _call_mouse_move(self, tool_input: dict[str, Any]) -> dict[str, Any]:
        """Validate mouse_move input and return a screenshot (no adb action)."""
        try:
            width, height = self.state.device.screen_size()
            _normalized_point_arg(tool_input)
        except (TypeError, ValueError) as exc:
            return {"output": f"error: {exc}", "is_error": True}
        return self._call_noop_with_screenshot()

    def _call_mouse_scroll(self, tool_input: dict[str, Any]) -> dict[str, Any]:
        try:
            delta = int(tool_input.get("delta", 0))
        except (TypeError, ValueError) as exc:
            return {"output": f"error: invalid delta: {exc}", "is_error": True}
        if delta == 0:
            return self._call_noop_with_screenshot()
        return self._call_touch_gesture(
            {
                "type": "swipe",
                "start": {"x": 500, "y": 800 if delta < 0 else 200},
                "direction": "up" if delta < 0 else "down",
                "duration_ms": 300,
            },
            log_tool_name="mouse_scroll",
            log_tool_input=tool_input,
        )

    def _call_quick_action(self, tool_input: dict[str, Any]) -> dict[str, Any]:
        platform = "android"
        action = _quick_action_id(tool_input)
        if bool(tool_input.get("list")) or action == "list":
            output = {"ok": True, "platform": platform, "actions": _adb_quick_action_catalog()}
            return {"output": json.dumps(output), "is_error": False}

        if bool(tool_input.get("alternative")):
            return _adb_reserved_quick_action(action, platform, "adb bridge does not define alternative bindings")

        device = self.state.device
        if action == "back":
            return self._execute_device(
                lambda: device.keyevent("KEYCODE_BACK"),
                tool_name="quick_action",
                tool_input=tool_input,
                adb_summary="input keyevent KEYCODE_BACK",
            )
        if action == "home":
            return self._execute_device(
                lambda: device.keyevent("KEYCODE_HOME"),
                tool_name="quick_action",
                tool_input=tool_input,
                adb_summary="input keyevent KEYCODE_HOME",
            )
        if action == "app_switch":
            return self._execute_device(
                lambda: device.keyevent(KEYCODE_APP_SWITCH),
                tool_name="quick_action",
                tool_input=tool_input,
                adb_summary=f"input keyevent {KEYCODE_APP_SWITCH}",
            )
        if action == "send":
            return self._execute_device(
                lambda: device.keyevent(KEYCODE_ENTER),
                tool_name="quick_action",
                tool_input=tool_input,
                adb_summary=f"input keyevent {KEYCODE_ENTER}",
            )
        if action == "open_settings":
            return self._execute_device(
                device.start_settings,
                tool_name="quick_action",
                tool_input=tool_input,
                adb_summary="am start -a android.settings.SETTINGS",
            )
        if action == "notification_center":
            return self._execute_device(
                lambda: _statusbar_with_fallback(device, device.expand_notifications, _top_swipe(device)),
                tool_name="quick_action",
                tool_input=tool_input,
                adb_summary="cmd statusbar expand-notifications",
            )
        if action == "control_center":
            def expand_quick_settings() -> None:
                _statusbar_with_fallback(device, device.expand_settings, _top_swipe(device, repeat=2))

            return self._execute_device(
                expand_quick_settings,
                tool_name="quick_action",
                tool_input=tool_input,
                adb_summary="cmd statusbar expand-settings",
            )
        if action == "dismiss_panel":
            return self._execute_device(
                lambda: _statusbar_with_fallback(
                    device, device.collapse_statusbar, lambda: device.keyevent("KEYCODE_BACK")
                ),
                tool_name="quick_action",
                tool_input=tool_input,
                adb_summary="cmd statusbar collapse",
            )
        if action == "quit_app":
            return self._execute_device(
                lambda: device.keyevent("KEYCODE_HOME"),
                tool_name="quick_action",
                tool_input=tool_input,
                adb_summary="input keyevent KEYCODE_HOME",
            )
        if action in ADB_RESERVED_QUICK_ACTIONS:
            return _adb_reserved_quick_action(
                action,
                platform,
                "adb bridge cannot faithfully execute this shortcut; use direct touch or text-entry tools",
            )

        return {"output": f"error: unsupported quick_action: {tool_input.get('action')!r}", "is_error": True}

    def _call_enter_text(self, tool_input: dict[str, Any]) -> dict[str, Any]:
        """Execute enter_text through adb input text."""
        unknown = sorted(set(tool_input) - {"text", "focus"})
        if unknown:
            output = {"ok": False, "suggestion": f"Remove unsupported enter_text arguments: {unknown!r}."}
            return {"output": json.dumps(output), "is_error": False}
        text = tool_input.get("text", "")
        if not isinstance(text, str):
            return {"output": "error: text must be a string", "is_error": True}
        if text == "":
            output = {"ok": False, "suggestion": "Provide non-empty text, then retry enter_text."}
            return {"output": json.dumps(output), "is_error": False}
        unsupported = unsupported_adb_text_chars(text)
        if unsupported:
            output = {
                "ok": False,
                "suggestion": f"adb input text supports only US-keyboard ASCII characters; unsupported: {unsupported!r}",
            }
            return {"output": json.dumps(output, ensure_ascii=False), "is_error": False}

        device = self.state.device
        focus = tool_input.get("focus")
        focus_pixels: tuple[int, int] | None = None
        if not isinstance(focus, dict):
            output = {"ok": False, "suggestion": "Provide focus as an object with x and y coordinates, then retry enter_text."}
            return {"output": json.dumps(output), "is_error": False}
        unknown_focus = sorted(set(focus) - {"x", "y"})
        if unknown_focus:
            output = {"ok": False, "suggestion": f"Remove unsupported focus arguments: {unknown_focus!r}."}
            return {"output": json.dumps(output), "is_error": False}
        point_input: dict[str, Any] = {"focus": focus}
        try:
            width, height = device.screen_size()
            point = _normalized_point_arg(point_input, field="focus")
        except (TypeError, ValueError) as exc:
            output = {"ok": False, "suggestion": f"Correct the focus coordinates: {exc}"}
            return {"output": json.dumps(output), "is_error": False}
        focus_pixels = _to_pixels(point, width, height)

        def enter_text() -> None:
            if focus_pixels is not None:
                device.tap(*focus_pixels)
                time.sleep(FOCUS_SETTLE_SEC)
            device.input_text(text)

        with self.state.lock:
            started = time.monotonic()
            try:
                enter_text()
            except ValueError as exc:
                return {"output": f"error: {exc}", "is_error": True}
            duration_ms = int((time.monotonic() - started) * 1000)
            if self.action_settle_sec > 0:
                time.sleep(self.action_settle_sec)
            xml_text = ""
            xml_error = ""
            try:
                xml_text = device.dump_window_xml()
            except (ADBCommandError, AttributeError) as exc:
                xml_error = str(exc)
            jpeg, width, height = device.screenshot_jpeg()

        committed, _, wrong_ime, verify_reason, _ = _verify_adb_text_entry(
            xml_text,
            text,
            xml_error=xml_error,
        )
        text_output: dict[str, Any] = {"ok": committed}
        if not committed:
            suggestion = verify_reason
            if wrong_ime and "English/Latin keyboard" not in suggestion:
                suggestion += "; switch to the English/Latin keyboard before retrying"
            text_output["suggestion"] = suggestion
        screenshot = encode_screenshot(jpeg, "image/jpeg", width, height)
        self.state.log_action(
            tool_name="enter_text",
            tool_input=tool_input,
            adb_summary="input text",
            duration_ms=duration_ms,
            screenshot=screenshot,
        )
        output_data = {
            "action_output": json.dumps(text_output, ensure_ascii=False),
            "data": screenshot["data"],
            "width": screenshot["width"],
            "height": screenshot["height"],
            "format": screenshot.get("format", "jpeg"),
            "size": screenshot.get("size", 0),
        }
        return {"output": json.dumps(output_data, ensure_ascii=False), "is_error": False}

    def _call_keyboard_text(self, tool_input: dict[str, Any]) -> dict[str, Any]:
        text = tool_input.get("text", "")
        if not isinstance(text, str):
            return {"output": "error: text must be a string", "is_error": True}
        if text == "":
            return {"output": "error: text is required", "is_error": True}
        unsupported = unsupported_adb_text_chars(text)
        if unsupported:
            return {
                "output": f"error: keyboard_text supports only US-keyboard ASCII characters; unsupported characters: {unsupported!r}",
                "is_error": True,
            }
        device = self.state.device
        return self._execute_device(
            lambda: device.input_text(text),
            tool_name="keyboard_text",
            tool_input=tool_input,
            adb_summary="input text",
        )

    def _call_keyboard_tap(self, tool_input: dict[str, Any]) -> dict[str, Any]:
        keys = tool_input.get("keys", [])
        if not keys:
            return {"output": "error: keys array is required", "is_error": True}
        if not isinstance(keys, list):
            return {"output": "error: keys must be an array", "is_error": True}

        # Reject hold_ms explicitly: adb keyevent cannot hold keys for a duration.
        if "hold_ms" in tool_input and tool_input.get("hold_ms") not in (None, 0):
            return {
                "output": "error: adb keyboard_tap does not support hold_ms; adb input keyevent cannot hold a key for an arbitrary duration",
                "is_error": True,
            }

        normalized_keys = [str(k).strip().lower() for k in keys if str(k).strip()]
        if not normalized_keys:
            return {"output": "error: at least one key or modifier is required", "is_error": True}
        alias_map = {
            "keycode_back": "back",
            "keycode_home": "home",
            "keycode_app_switch": "app_switch",
            "return": "enter",
            "escape": "back",
            "esc": "back",
            "delete_backward": "backspace",
            "delete": "backspace",
            "del": "backspace",
        }
        normalized_keys = [alias_map.get(k, k) for k in normalized_keys]
        has_meta = any(k in ("meta", "cmd", "super", "win") for k in normalized_keys)
        # The Go text engine uses these chords to clear fields and undo probes.
        # Never degrade a chord to sequential/plain keyevents on older Android.
        if len(normalized_keys) == 2 and "ctrl" in normalized_keys:
            key = next(k for k in normalized_keys if k != "ctrl") if normalized_keys.count("ctrl") == 1 else ""
            if key in {"a", "z"}:
                keycodes = ["KEYCODE_CTRL_LEFT", f"KEYCODE_{key.upper()}"]
                return self._execute_device(
                    lambda: self.state.device.keycombination(keycodes),
                    tool_name="keyboard_tap",
                    tool_input=tool_input,
                    adb_summary="input keycombination " + " ".join(keycodes),
                )
        # Other modifier combinations remain explicitly unsupported.
        if any(k in ("ctrl", "alt", "shift") for k in normalized_keys):
            return {
                "output": "error: adb keyboard_tap supports ctrl/alt/shift modifiers only for ctrl+a and ctrl+z on devices with input keycombination; use the bare key, keyboard_text, or quick_action instead",
                "is_error": True,
            }
        non_modifiers = [k for k in normalized_keys if k not in ("meta", "cmd", "super", "win")]

        keycode_map: dict[str, str | int] = {
            "enter": KEYCODE_ENTER,
            "home": "KEYCODE_HOME",
            "back": "KEYCODE_BACK",
            "app_switch": KEYCODE_APP_SWITCH,
            "backspace": KEYCODE_DEL,
            "tab": KEYCODE_TAB,
        }

        if has_meta and len(non_modifiers) == 0:
            keycode: str | int = "KEYCODE_HOME"
            summary_key = "home"
        elif len(non_modifiers) == 0:
            return self._call_noop_with_screenshot()
        elif has_meta and len(non_modifiers) == 1 and non_modifiers[0] == "h":
            keycode = "KEYCODE_HOME"
            summary_key = "home"
        elif len(non_modifiers) == 1:
            key = non_modifiers[0]
            if key not in keycode_map:
                return {"output": f"error: adb keyboard_tap does not support key: {key!r}", "is_error": True}
            keycode = keycode_map[key]
            summary_key = key
        else:
            return {"output": "error: adb keyboard_tap supports one non-modifier key at a time", "is_error": True}

        device = self.state.device
        return self._execute_device(
            lambda: device.keyevent(keycode),
            tool_name="keyboard_tap",
            tool_input=tool_input,
            adb_summary=f"input keyevent {keycode} ({summary_key})",
        )

    # ---- execution helpers --------------------------------------------------

    def _execute_device(
        self,
        fn: Callable[[], None],
        *,
        tool_name: str,
        tool_input: dict[str, Any],
        adb_summary: str,
    ) -> dict[str, Any]:
        """Run a device action, then capture a post-action screenshot."""
        with self.state.lock:
            started = time.monotonic()
            try:
                fn()
            except ValueError as exc:
                return {"output": f"error: {exc}", "is_error": True}
            duration_ms = int((time.monotonic() - started) * 1000)
            if self.action_settle_sec > 0:
                time.sleep(self.action_settle_sec)
            jpeg, width, height = self.state.device.screenshot_jpeg()
        screenshot = encode_screenshot(jpeg, "image/jpeg", width, height)
        self.state.log_action(
            tool_name=tool_name,
            tool_input=tool_input,
            adb_summary=adb_summary,
            duration_ms=duration_ms,
            screenshot=screenshot,
        )
        output_data = {
            "action_output": "ok",
            "data": screenshot["data"],
            "width": screenshot["width"],
            "height": screenshot["height"],
            "format": screenshot.get("format", "jpeg"),
        }
        return {"output": json.dumps(output_data), "is_error": False}

    def _call_noop_with_screenshot(self) -> dict[str, Any]:
        with self.state.lock:
            jpeg, width, height = self.state.device.screenshot_jpeg()
        screenshot = encode_screenshot(jpeg, "image/jpeg", width, height)
        output_data = {
            "action_output": "ok",
            "data": screenshot["data"],
            "width": screenshot["width"],
            "height": screenshot["height"],
            "format": screenshot.get("format", "jpeg"),
        }
        return {"output": json.dumps(output_data), "is_error": False}

    def _send_json(self, handler: BaseHTTPRequestHandler, status: int, payload: dict[str, Any]) -> None:
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        handler.send_response(status)
        handler.send_header("Content-Type", "application/json")
        handler.send_header("Content-Length", str(len(data)))
        handler.end_headers()
        handler.wfile.write(data)


# ---- shared helpers (mirroring mobilegym/bridge/tools_api.py) ---------------


def _decode_tool_input(request_body: dict[str, Any]) -> str:
    """Decode tool input from request body (matches Go decodeToolInvokeInput)."""
    if "raw_input" in request_body and request_body["raw_input"] is not None:
        return str(request_body["raw_input"])

    if "input" in request_body:
        input_value = request_body["input"]
        if input_value is None or input_value == "":
            return ""
        if isinstance(input_value, str):
            trimmed = input_value.strip()
            if trimmed in ("", "null"):
                return ""
            # If it's a quoted string, unwrap it
            if trimmed.startswith('"') and trimmed.endswith('"'):
                try:
                    return json.loads(trimmed)
                except json.JSONDecodeError:
                    pass
            return trimmed
        # input is already a dict/list, re-serialize it
        return json.dumps(input_value)

    return ""


def _statusbar_with_fallback(device: Any, primary: Callable[[], None], fallback: Callable[[], None]) -> None:
    try:
        primary()
    except ADBCommandError:
        fallback()


def _top_swipe(device: Any, *, repeat: int = 1) -> Callable[[], None]:
    """Return a fallback that swipes down from the top edge of the screen."""

    def swipe_from_top() -> None:
        width, height = device.screen_size()
        for index in range(max(1, repeat)):
            device.swipe(width // 2, 1, width // 2, int(height * 0.7), 300)
            if index + 1 < repeat:
                time.sleep(0.3)

    return swipe_from_top


def _resolve_swipe(
    start: dict[str, float], tool_input: dict[str, Any]
) -> tuple[dict[str, float], dict[str, float], int]:
    has_end = "end" in tool_input or ("end_x" in tool_input and "end_y" in tool_input)
    direction = str(tool_input.get("direction", "") or "").strip().lower()
    if has_end and direction:
        raise ValueError("swipe accepts either end or direction, not both")

    speed = _swipe_speed_arg(tool_input.get("speed"))
    duration_value = tool_input.get("duration_ms")
    duration_ms = None if duration_value is None else _swipe_duration_arg(duration_value)

    if has_end:
        end = _normalized_point_arg(tool_input, field="end", x_key="end_x", y_key="end_y")
    else:
        if not direction:
            raise ValueError("end or direction is required for swipe")
        if direction not in {"up", "down", "left", "right"}:
            raise ValueError(f"unsupported swipe direction: {direction!r}; use up, down, left, or right")
        travel = _distance_to_edge(start, direction)
        if duration_ms is not None:
            travel = speed * duration_ms / 1000
        end = _direction_end(start, direction, travel)
        if not 0 <= end["x"] <= 1000 or not 0 <= end["y"] <= 1000:
            raise ValueError("speed and duration_ms move the swipe past the screen edge")

    distance = math.hypot(end["x"] - start["x"], end["y"] - start["y"])
    if distance == 0:
        raise ValueError("swipe requires distinct start and end points")
    if duration_ms is None:
        duration_ms = max(1, int(math.floor(distance / speed * 1000 + 0.5)))
        if duration_ms > MAX_SWIPE_DURATION_MS:
            raise ValueError(
                f"speed is too low for this swipe; calculated duration_ms={duration_ms} exceeds {MAX_SWIPE_DURATION_MS}"
            )
    return start, end, duration_ms


def _swipe_duration_arg(value: Any) -> int:
    if isinstance(value, bool) or not isinstance(value, int):
        raise ValueError("duration_ms must be an integer")
    duration_ms = value
    if duration_ms < 1 or duration_ms > MAX_SWIPE_DURATION_MS:
        raise ValueError(f"duration_ms must be in range [1, {MAX_SWIPE_DURATION_MS}]")
    return duration_ms


def _swipe_speed_arg(value: Any) -> float:
    if value is None:
        return DEFAULT_SWIPE_SPEED
    if isinstance(value, bool) or not isinstance(value, (int, float)):
        raise ValueError("speed must be a positive finite number")
    speed = float(value)
    if not math.isfinite(speed) or speed <= 0:
        raise ValueError("speed must be a positive finite number")
    return speed


def _distance_to_edge(start: dict[str, float], direction: str) -> float:
    return {
        "up": start["y"],
        "down": 1000 - start["y"],
        "left": start["x"],
        "right": 1000 - start["x"],
    }[direction]


def _direction_end(start: dict[str, float], direction: str, travel: float) -> dict[str, float]:
    end = dict(start)
    axis, sign = {
        "up": ("y", -1),
        "down": ("y", 1),
        "left": ("x", -1),
        "right": ("x", 1),
    }[direction]
    end[axis] += sign * travel
    return end


def _duration_ms_arg(value: Any, default: int, *, minimum: int = 1) -> int:
    """Clamp a caller-supplied duration to [minimum, MAX_ACTION_DURATION_MS].

    Bad or missing values fall back to the default; out-of-range values are
    clamped rather than rejected for non-swipe gesture timing.
    """
    try:
        parsed = int(value) if value not in (None, "") else int(default)
    except (TypeError, ValueError):
        parsed = int(default)
    return max(minimum, min(MAX_ACTION_DURATION_MS, parsed))


def _point_arg(
    tool_input: dict[str, Any],
    *,
    field: str = "point",
    x_key: str = "x",
    y_key: str = "y",
) -> dict[str, float]:
    point = tool_input.get(field)
    if isinstance(point, dict):
        unknown = sorted(set(point) - {"x", "y"})
        if unknown:
            raise ValueError(f"unknown {field} fields: {unknown!r}")
        if "x" not in point or "y" not in point:
            raise ValueError(f"{field}.x and {field}.y are required")
        return {"x": _finite_float(point["x"], f"{field}.x"), "y": _finite_float(point["y"], f"{field}.y")}
    if isinstance(point, (list, tuple)):
        if len(point) != 2:
            raise ValueError(f"{field} must contain [x, y]")
        return {"x": _finite_float(point[0], f"{field}[0]"), "y": _finite_float(point[1], f"{field}[1]")}

    if x_key in tool_input and y_key in tool_input:
        return {"x": _finite_float(tool_input[x_key], x_key), "y": _finite_float(tool_input[y_key], y_key)}

    raise ValueError(f"{field} is required")


def _normalized_point_arg(
    tool_input: dict[str, Any],
    *,
    field: str = "point",
    x_key: str = "x",
    y_key: str = "y",
) -> dict[str, float]:
    """Return a strictly validated normalized 0-1000 point."""
    point = _point_arg(tool_input, field=field, x_key=x_key, y_key=y_key)
    if not 0.0 <= point["x"] <= 1000.0 or not 0.0 <= point["y"] <= 1000.0:
        raise ValueError("coordinates must use the normalized 0-1000 scale")
    return point


def _unknown_tool_fields(tool_name: str, tool_input: dict[str, Any]) -> list[str]:
    allowed = {
        "touch_gesture": {
            "type", "point", "start", "end", "x", "y", "start_x", "start_y",
            "end_x", "end_y", "direction", "speed", "duration_ms", "hold_before_ms", "hold_after_ms",
            "hold_ms", "pause_ms", "steps", "button",
        },
        "mouse_move": {"x", "y"},
        "quick_action": {"action", "list", "alternative", "alternative_index"},
    }.get(tool_name)
    return [] if allowed is None else sorted(set(tool_input) - allowed)


def _swipe_timing_options(tool_input: dict[str, Any]) -> dict[str, int]:
    options: dict[str, int] = {}
    for name in ("hold_before_ms", "hold_after_ms"):
        value = tool_input.get(name)
        if value is None:
            continue
        if isinstance(value, bool) or not isinstance(value, int) or not 0 <= value <= MAX_SWIPE_HOLD_MS:
            raise ValueError(f"{name} must be an integer in range [0, {MAX_SWIPE_HOLD_MS}]")
        options[name] = value
    value = tool_input.get("steps")
    if value is not None:
        if isinstance(value, bool) or not isinstance(value, int) or not 1 <= value <= MAX_SWIPE_STEPS:
            raise ValueError(f"steps must be an integer in range [1, {MAX_SWIPE_STEPS}]")
        options["steps"] = value
    return options


def _to_pixels(point: dict[str, float], width: int, height: int) -> tuple[int, int]:
    """Convert a normalized 0-1000 point to device pixel coordinates."""
    x = min(width - 1, max(0, round(point["x"] / 1000.0 * width)))
    y = min(height - 1, max(0, round(point["y"] / 1000.0 * height)))
    return int(x), int(y)


def _finite_float(value: Any, name: str) -> float:
    try:
        parsed = float(value)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{name} must be a number") from exc
    if math.isnan(parsed) or math.isinf(parsed):
        raise ValueError(f"{name} must be a finite number")
    return parsed


def _verify_adb_text_entry(
    xml_text: str,
    target_text: str,
    *,
    xml_error: str = "",
) -> tuple[bool, str, bool, str, list[str]]:
    """Verify adb text entry from Android's focused/editable UIAutomator nodes."""
    steps: list[str] = []
    if xml_error:
        steps.append(f"uiautomator dump failed: {xml_error}")
        return (
            False,
            "",
            False,
            "adb input text sent; field verification unavailable, inspect the attached screenshot before retrying or reporting success",
            steps,
        )

    editable_texts, visible_texts, parse_error = _uiautomator_texts(xml_text)
    if parse_error:
        steps.append(f"uiautomator parse failed: {parse_error}")
        return (
            False,
            "",
            False,
            "adb input text sent; field verification unavailable, inspect the attached screenshot before retrying or reporting success",
            steps,
        )
    if not editable_texts:
        steps.append("uiautomator verification found no focused/editable text field")
        return (
            False,
            "",
            _looks_like_wrong_ascii_ime(target_text, "", visible_texts),
            "adb input text sent; no editable field text was available, inspect the attached screenshot before retrying or reporting success",
            steps,
        )

    for field_text in editable_texts:
        if _field_text_matches(field_text, target_text):
            steps.append(f"uiautomator verified editable field text {field_text!r}")
            return (
                True,
                field_text,
                False,
                "adb input text sent; verified in editable field",
                steps,
            )

    field_text = _best_observed_field_text(editable_texts, target_text)
    wrong_ime = _looks_like_wrong_ascii_ime(target_text, field_text, visible_texts)
    steps.append(f"uiautomator editable field mismatch: field={field_text!r}")
    reason = "adb input text sent; editable field text did not match target"
    if wrong_ime:
        reason += "; Chinese IME may be active, switch to the English/Latin keyboard before retrying"
    return False, field_text, wrong_ime, reason, steps


def _uiautomator_texts(xml_text: str) -> tuple[list[str], list[str], str]:
    xml_text = _extract_uiautomator_xml(xml_text)
    if not xml_text:
        return [], [], "uiautomator output did not contain hierarchy XML"
    try:
        root = ET.fromstring(xml_text)
    except ET.ParseError as exc:
        return [], [], str(exc)

    focused_editable: list[str] = []
    other_editable: list[str] = []
    visible: list[str] = []
    for node in root.iter("node"):
        text = str(node.attrib.get("text", "") or "").strip()
        content_desc = str(node.attrib.get("content-desc", "") or "").strip()
        for value in (text, content_desc):
            if value:
                visible.append(value)
        class_name = str(node.attrib.get("class", "") or "")
        focused = str(node.attrib.get("focused", "") or "").lower() == "true"
        is_editable = "EditText" in class_name or "AutoCompleteTextView" in class_name
        if is_editable and text:
            if focused:
                focused_editable.append(text)
            else:
                other_editable.append(text)
    return _unique_texts(focused_editable + other_editable), _unique_texts(visible), ""


def _extract_uiautomator_xml(raw: str) -> str:
    raw = str(raw or "")
    start = raw.find("<?xml")
    if start < 0:
        start = raw.find("<hierarchy")
    if start < 0:
        return ""
    end = raw.rfind("</hierarchy>")
    if end >= 0:
        return raw[start : end + len("</hierarchy>")]
    return raw[start:]


def _unique_texts(values: list[str]) -> list[str]:
    out: list[str] = []
    seen: set[str] = set()
    for value in values:
        value = value.strip()
        if not value or value in seen:
            continue
        seen.add(value)
        out.append(value)
    return out


def _field_text_matches(field_text: str, target_text: str) -> bool:
    field_text = field_text.strip()
    target_text = target_text.strip()
    return field_text.lower().endswith(target_text.lower())


def _best_observed_field_text(editable_texts: list[str], target_text: str) -> str:
    target_text = target_text.strip()
    if not editable_texts:
        return ""
    for text in editable_texts:
        if text and text in target_text:
            return text
    return editable_texts[0]


def _looks_like_wrong_ascii_ime(target_text: str, field_text: str, visible_texts: list[str]) -> bool:
    if any(ord(ch) > 127 for ch in target_text):
        return False
    if _field_text_matches(field_text, target_text):
        return False
    if any(ord(ch) > 127 for ch in field_text):
        return True
    indicators = ("拼音", "中文", "简体", "候选")
    return any(any(indicator in text for indicator in indicators) for text in visible_texts)


def _quick_action_id(tool_input: dict[str, Any]) -> str:
    action = str(tool_input.get("action", "") or "").strip().lower().replace("-", "_")
    aliases = {
        "返回": "back",
        "go_back": "back",
        "navigate_back": "back",
        "recents": "app_switch",
        "switch_app": "app_switch",
        "task_switcher": "app_switch",
        "主屏": "home",
        "go_home": "home",
        "home_screen": "home",
        "spotlight": "spotlight_search",
        "global_search": "spotlight_search",
        "search": "spotlight_search",
        "search_launch_app": "spotlight_search",
        "搜索": "spotlight_search",
        "notifications": "notification_center",
        "notification_shade": "notification_center",
        "通知": "notification_center",
        "quick_settings": "control_center",
        "quick_setting": "control_center",
        "快捷设置": "control_center",
        "close_panel": "dismiss_panel",
        "enter": "send",
        "press_enter": "send",
        "send_message": "send",
        "submit": "send",
        "refresh": "browser_refresh",
        "reload": "browser_refresh",
        "backspace": "delete_backward",
        "backward_delete": "delete_backward",
        "退格": "delete_backward",
        "settings": "open_settings",
        "system_settings": "open_settings",
        "设置": "open_settings",
    }
    return aliases.get(action, action)


def _adb_quick_action_catalog() -> list[dict[str, str]]:
    return [
        {"id": "back", "status": "active", "tool": "keyboard_tap"},
        {"id": "home", "status": "active", "tool": "keyboard_tap"},
        {"id": "app_switch", "status": "active", "tool": "keyboard_tap"},
        {"id": "notification_center", "status": "active", "tool": "quick_action"},
        {"id": "control_center", "status": "active", "tool": "quick_action"},
        {"id": "dismiss_panel", "status": "active", "tool": "quick_action"},
        {"id": "send", "status": "active", "tool": "keyboard_tap"},
        {"id": "open_settings", "status": "active", "tool": "quick_action"},
        {"id": "quit_app", "status": "active", "tool": "quick_action"},
        {"id": "browser_refresh", "status": "reserved", "tool": "keyboard_tap"},
        {"id": "copy", "status": "reserved", "tool": "keyboard_tap"},
        {"id": "paste", "status": "reserved", "tool": "keyboard_tap"},
    ]


def _adb_reserved_quick_action(action: str, platform: str, reason: str) -> dict[str, Any]:
    output = {"ok": False, "action": action, "platform": platform, "status": "reserved", "reason": reason}
    return {"output": json.dumps(output), "is_error": False}
