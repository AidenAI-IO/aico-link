"""Unified /api/tools endpoint for environment bridge implementations.

This module implements a tool catalog and invocation API that matches the Go agent's
/api/tools interface, making the MobileGym bridge server usable as an environment bridge.
"""

from __future__ import annotations

import asyncio
import json
import math
import time
from collections.abc import Awaitable, Callable
from concurrent.futures import TimeoutError as FutureTimeoutError
from http.server import BaseHTTPRequestHandler
from typing import Any

from .actions import action_to_dict, build_action
from .episode import (
    BridgeEpisodeState,
    BridgeTaskRouter,
    MissingBenchmarkTaskIDError,
    NoBridgeEnvAvailableError,
    StaleEpisodeError,
)
from .protocol import encode_screenshot


DEFAULT_SWIPE_SPEED = 2500.0
MAX_SWIPE_DURATION_MS = 10_000
MAX_SWIPE_HOLD_MS = 10_000
MAX_SWIPE_STEPS = 1_000
MAX_MOUSE_SCROLL_DELTA = 127
MOUSE_SCROLL_BASE_DURATION_MS = 300
MOUSE_SCROLL_DURATION_STEP_MS = 75

US_KEYBOARD_TEXT_CHARS = set(
    "abcdefghijklmnopqrstuvwxyz"
    "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
    "0123456789"
    " \n\r\t"
    "-=[]\\;'`,./"
    "!@#$%^&*()_+{}|:\"~<>?"
)
MOBILEGYM_RESERVED_QUICK_ACTIONS = {
    "browser_refresh",
    "browser_new_tab",
    "copy",
    "cut",
    "delete_backward",
    "find",
    "paste",
    "select_all",
    "undo",
}


def _mobilegym_app_switch_action() -> Any:
    return build_action(
        "swipe",
        {"start_x": 500, "start_y": 1000, "end_x": 500, "end_y": 500, "duration_ms": 900},
    )


class ToolsAPIHandler:
    """Handler for the environment bridge /api/tools endpoint."""

    def __init__(self, state: BridgeEpisodeState | BridgeTaskRouter, request_timeout_sec: float = 30):
        self.router = BridgeTaskRouter.from_state(state)
        self.state = self.router.default_state
        self.request_timeout_sec = request_timeout_sec

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
                "description": "Perform touch gestures on the MobileGym simulator (tap, swipe, drag, long_press, etc.).",
                "args_schema": {
                    "type": "object",
                    "additionalProperties": False,
                    "properties": {
                        "type": {
                            "type": "string",
                            "enum": ["tap", "double_tap", "long_press", "swipe", "drag", "drag_start", "drag_release"],
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
                        "hold_ms": {"type": "integer", "minimum": 0, "description": "Tap or long-press hold duration in milliseconds."},
                        "pause_ms": {"type": "integer", "minimum": 0, "description": "Pause between taps for double_tap."},
                        "steps": {"type": "integer", "minimum": 1, "maximum": MAX_SWIPE_STEPS, "description": "Number of movement steps for swipe or drag."},
                        "direction": {"type": "string", "enum": ["up", "down", "left", "right"], "description": "Swipe direction when end is omitted"},
                        "speed": {"type": "number", "exclusiveMinimum": 0, "description": "Optional swipe speed in normalized coordinate units per second; defaults to 2500"},
                        "button": {"type": "string", "enum": ["left", "right", "middle"]},
                    },
                    "required": ["type"],
                },
            },
            {
                "name": "keyboard_text",
                "description": "Type text into the MobileGym simulator.",
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
                "description": "Press keyboard keys in the MobileGym simulator (e.g., enter, back, home).",
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
                        "hold_ms": {"type": "integer", "minimum": 0, "description": "Optional press duration before release."},
                    },
                    "required": ["keys"],
                },
            },
            {
                "name": "enter_text",
                "description": "Enter target text into an input field in MobileGym using the simulator TYPE action.",
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
                "description": "Move the pointer. MobileGym has no hover state, so this is accepted as a no-op and returns a screenshot.",
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
                "description": "Scroll using a MobileGym swipe approximation.",
                "args_schema": {
                    "type": "object",
                    "additionalProperties": False,
                    "properties": {"delta": {"type": "integer", "minimum": -MAX_MOUSE_SCROLL_DELTA, "maximum": MAX_MOUSE_SCROLL_DELTA}},
                    "required": ["delta"],
                },
            },
            {
                "name": "quick_action",
                "description": "Execute common Android navigation actions such as back or home.",
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
        # Read request body
        try:
            content_length = int(handler.headers.get("Content-Length", "0") or "0")
        except ValueError:
            self._send_json(handler, 400, {"error": "bad_header", "output": "invalid Content-Length", "is_error": True})
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
        raw_input = self._decode_tool_input(request_body)

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

        # Check active episode
        try:
            state = self.router.state_for_headers(handler.headers)
        except MissingBenchmarkTaskIDError as exc:
            self._send_json(
                handler,
                400,
                {"error": "missing_benchmark_task_id", "output": str(exc), "is_error": True},
            )
            return
        except NoBridgeEnvAvailableError as exc:
            self._send_json(
                handler,
                429,
                {"error": "no_bridge_env_available", "output": str(exc), "is_error": True},
            )
            return
        session, ok = self._get_active_session(state, tool_input)
        if not ok:
            self._send_json(
                handler,
                409,
                {"error": "no_active_episode", "output": "no active episode; call /api/setup first", "is_error": True},
            )
            return

        # Execute tool
        started_at = time.time()
        try:
            result = self._submit_tool_call(state, tool_name, tool_input)
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

        except TimeoutError:
            duration_ms = int((time.time() - started_at) * 1000)
            self._send_json(
                handler,
                504,
                {
                    "output": "tool execution timed out",
                    "is_error": True,
                    "error": "timeout",
                    "duration_ms": duration_ms,
                },
            )
        except StaleEpisodeError:
            duration_ms = int((time.time() - started_at) * 1000)
            self._send_json(
                handler,
                409,
                {
                    "output": "stale episode",
                    "is_error": True,
                    "error": "stale_episode",
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

    def _decode_tool_input(self, request_body: dict[str, Any]) -> str:
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

    def _get_active_session(self, state: BridgeEpisodeState, tool_input: dict[str, Any]) -> tuple[dict[str, str], bool]:
        """Get active episode session."""
        episode_id = state.active_episode_id
        if not episode_id:
            return {}, False
        return {"episode_id": episode_id}, True

    def _submit_tool_call(self, state: BridgeEpisodeState, tool_name: str, tool_input: dict[str, Any]) -> dict[str, Any]:
        """Submit tool call to MobileGym environment."""
        unknown = _unknown_tool_fields(tool_name, tool_input)
        if unknown:
            return {"output": f"error: unknown fields: {unknown!r}", "is_error": True}
        episode_id = state.active_episode_id

        if tool_name == "touch_gesture":
            return self._call_touch_gesture(state, tool_input, episode_id)
        elif tool_name == "keyboard_text":
            return self._call_keyboard_text(state, tool_input, episode_id)
        elif tool_name == "keyboard_tap":
            return self._call_keyboard_tap(state, tool_input, episode_id)
        elif tool_name == "enter_text":
            return self._call_enter_text(state, tool_input, episode_id)
        elif tool_name == "mouse_move":
            return self._call_mouse_move(state, tool_input, episode_id)
        elif tool_name == "mouse_scroll":
            return self._call_mouse_scroll(state, tool_input, episode_id)
        elif tool_name == "quick_action":
            return self._call_quick_action(state, tool_input, episode_id)
        else:
            return {"output": f"unknown tool: {tool_name}", "is_error": True, "error": "unknown_tool"}

    def _call_touch_gesture(
        self,
        state: BridgeEpisodeState,
        tool_input: dict[str, Any],
        episode_id: str,
        *,
        log_tool_name: str = "touch_gesture",
        log_tool_input: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        """Execute touch_gesture tool."""
        raw_gesture_type = tool_input.get("type", "")
        if not isinstance(raw_gesture_type, str):
            return {"output": "error: type must be a string", "is_error": True}
        gesture_type = raw_gesture_type.strip().lower()
        if not gesture_type:
            return {"output": "error: type is required", "is_error": True}

        # Map gesture type to bridge action
        try:
            if gesture_type in {"drag_start", "drag_release"}:
                point = _normalized_point_arg(tool_input)
                return self._execute_input(state, episode_id, "touch_gesture", tool_input,
                    lambda: state.input.contact(gesture_type, point))
            if gesture_type == "tap":
                point = _normalized_point_arg(tool_input)
                action = build_action("tap", point)
            elif gesture_type == "double_tap":
                point = _normalized_point_arg(tool_input)
                action = build_action("tap", {**point, "count": 2})
            elif gesture_type == "long_press":
                point = _normalized_point_arg(tool_input)
                action = build_action(
                    "tap",
                    {
                        **point,
                        "kind": "long_press",
                        "duration_ms": tool_input.get("duration_ms", tool_input.get("hold_ms", 500)),
                    },
                )
            elif gesture_type in ("swipe", "drag"):
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
                    duration_value = tool_input.get("duration_ms")
                    duration_ms = 700 if duration_value is None else _swipe_duration_arg(duration_value)
                timing = _swipe_timing_options(tool_input)
                action = build_action(
                    gesture_type,
                    {
                        "start_x": start["x"],
                        "start_y": start["y"],
                        "end_x": end["x"],
                        "end_y": end["y"],
                        "duration_ms": duration_ms,
                        **timing,
                    },
                )
            else:
                return {"output": f"error: unsupported gesture type: {gesture_type}", "is_error": True}
        except (TypeError, ValueError) as exc:
            return {"output": f"error: {exc}", "is_error": True}

        return self._execute_action(
            state,
            action,
            episode_id,
            tool_name=log_tool_name,
            tool_input=tool_input if log_tool_input is None else log_tool_input,
        )

    def _call_mouse_move(self, state: BridgeEpisodeState, tool_input: dict[str, Any], episode_id: str) -> dict[str, Any]:
        """Move an active drag contact; otherwise retain the legacy hover no-op."""
        try:
            _normalized_point_arg(tool_input)
        except (TypeError, ValueError) as exc:
            return {"output": f"error: {exc}", "is_error": True}
        if state.input.contact_active:
            return self._execute_input(state, episode_id, "mouse_move", tool_input,
                lambda: state.input.contact("move", _normalized_point_arg(tool_input)))
        return self._call_noop_with_screenshot(state, episode_id)

    def _call_mouse_scroll(self, state: BridgeEpisodeState, tool_input: dict[str, Any], episode_id: str) -> dict[str, Any]:
        """Execute mouse_scroll as an approximate vertical swipe."""
        try:
            delta = int(tool_input.get("delta", 0))
        except (TypeError, ValueError) as exc:
            return {"output": f"error: invalid delta: {exc}", "is_error": True}
        if delta == 0:
            return self._call_noop_with_screenshot(state, episode_id)
        magnitude = abs(delta)
        if magnitude > MAX_MOUSE_SCROLL_DELTA:
            return {"output": f"error: delta must be in range [-{MAX_MOUSE_SCROLL_DELTA}, {MAX_MOUSE_SCROLL_DELTA}]", "is_error": True}
        start_y = 800 if delta < 0 else 200
        end_y = 50 if delta < 0 else 950
        duration_ms = MOUSE_SCROLL_BASE_DURATION_MS + (magnitude - 1) * MOUSE_SCROLL_DURATION_STEP_MS
        return self._call_touch_gesture(
            state,
            {"type": "swipe", "start": {"x": 500, "y": start_y}, "end": {"x": 500, "y": end_y}, "duration_ms": duration_ms},
            episode_id,
            log_tool_name="mouse_scroll",
            log_tool_input=tool_input,
        )

    def _call_quick_action(self, state: BridgeEpisodeState, tool_input: dict[str, Any], episode_id: str) -> dict[str, Any]:
        """Execute a small MobileGym-compatible quick_action subset."""
        platform = "android"
        action = _quick_action_id(tool_input)
        if bool(tool_input.get("list")) or action == "list":
            output = {"ok": True, "platform": platform, "actions": _mobilegym_quick_action_catalog()}
            return {"output": json.dumps(output), "is_error": False}

        if bool(tool_input.get("alternative")):
            return _mobilegym_reserved_quick_action(action, platform, "mobilegym quick_action does not define alternative bindings")

        if action == "back":
            return self._call_touch_gesture(
                state,
                {"type": "swipe", "start": {"x": 1, "y": 500}, "end": {"x": 750, "y": 500}, "duration_ms": 700},
                episode_id,
                log_tool_name="quick_action",
                log_tool_input=tool_input,
            )
        if action == "home":
            return self._call_touch_gesture(
                state,
                {"type": "swipe", "start": {"x": 500, "y": 999}, "end": {"x": 500, "y": 180}, "duration_ms": 700},
                episode_id,
                log_tool_name="quick_action",
                log_tool_input=tool_input,
            )
        if action == "app_switch":
            action = _mobilegym_app_switch_action()
            return self._execute_action(state, action, episode_id, tool_name="quick_action", tool_input=tool_input)
        if action == "spotlight_search":
            return self._call_touch_gesture(
                state,
                {"type": "swipe", "start": {"x": 500, "y": 250}, "end": {"x": 500, "y": 750}, "duration_ms": 300},
                episode_id,
                log_tool_name="quick_action",
                log_tool_input=tool_input,
            )
        if action == "notification_center":
            return self._call_touch_gesture(
                state,
                {"type": "swipe", "start": {"x": 500, "y": 250}, "end": {"x": 500, "y": 750}, "duration_ms": 300},
                episode_id,
                log_tool_name="quick_action",
                log_tool_input=tool_input,
            )
        if action == "control_center":
            action = build_action(
                "swipe",
                {"start_x": 850, "start_y": 0, "end_x": 850, "end_y": 700, "duration_ms": 500},
            )
            return self._execute_action(state, action, episode_id, tool_name="quick_action", tool_input=tool_input)
        if action == "dismiss_panel":
            return self._call_touch_gesture(
                state,
                {"type": "swipe", "start": {"x": 500, "y": 750}, "end": {"x": 500, "y": 250}, "duration_ms": 300},
                episode_id,
                log_tool_name="quick_action",
                log_tool_input=tool_input,
            )
        if action == "send":
            action = build_action("key", {"key": "enter"})
            return self._execute_action(state, action, episode_id, tool_name="quick_action", tool_input=tool_input)
        if action == "quit_app":
            action = build_action("home", {})
            return self._execute_action(state, action, episode_id, tool_name="quick_action", tool_input=tool_input)
        if action in MOBILEGYM_RESERVED_QUICK_ACTIONS:
            return _mobilegym_reserved_quick_action(
                action,
                platform,
                "mobilegym cannot faithfully execute this keyboard shortcut; use direct touch or text-entry tools",
            )

        return {"output": f"error: unsupported quick_action: {tool_input.get('action')!r}", "is_error": True}

    def _call_enter_text(
        self,
        state: BridgeEpisodeState,
        tool_input: dict[str, Any],
        episode_id: str,
    ) -> dict[str, Any]:
        """Execute enter_text through MobileGym's TYPE action."""
        # Accept Agent proxy text-entry hints for API compatibility, but do
        # not advertise or forward them because MobileGym's TYPE action only
        # needs the final text and focus point.
        unknown = sorted(set(tool_input) - {"text", "focus", "platform", "mode", "segments"})
        if unknown:
            output = {"ok": False, "suggestion": f"Remove unsupported enter_text arguments: {unknown!r}."}
            return {"output": json.dumps(output), "is_error": False}
        text = tool_input.get("text", "")
        if not isinstance(text, str):
            return {"output": "error: text must be a string", "is_error": True}
        if text == "":
            output = {"ok": False, "suggestion": "Provide non-empty text, then retry enter_text."}
            return {"output": json.dumps(output), "is_error": False}

        action_input: dict[str, Any] = {"text": text}
        focus = tool_input.get("focus")
        if not isinstance(focus, dict):
            output = {"ok": False, "suggestion": "Provide focus as an object with x and y coordinates, then retry enter_text."}
            return {"output": json.dumps(output), "is_error": False}
        unknown_focus = sorted(set(focus) - {"x", "y"})
        if unknown_focus:
            output = {"ok": False, "suggestion": f"Remove unsupported focus arguments: {unknown_focus!r}."}
            return {"output": json.dumps(output), "is_error": False}
        point_input: dict[str, Any] = {"focus": focus}
        try:
            action_input["point"] = _normalized_point_arg(point_input, field="focus")
        except (TypeError, ValueError) as exc:
            output = {"ok": False, "suggestion": f"Correct the focus coordinates: {exc}"}
            return {"output": json.dumps(output), "is_error": False}

        try:
            action = build_action("type_text", action_input)
        except (TypeError, ValueError) as exc:
            return {"output": f"error: {exc}", "is_error": True}

        result = self._execute_action(state, action, episode_id, tool_name="enter_text", tool_input=tool_input)
        if result.get("is_error"):
            return result

        output = {"ok": True}
        return {"output": json.dumps(output, ensure_ascii=False), "is_error": False}

    def _call_keyboard_text(self, state: BridgeEpisodeState, tool_input: dict[str, Any], episode_id: str) -> dict[str, Any]:
        """Execute keyboard_text tool."""
        text = tool_input.get("text", "")
        if not isinstance(text, str):
            return {"output": "error: text must be a string", "is_error": True}
        if text == "":
            return {"output": "error: text is required", "is_error": True}
        unsupported = _unsupported_keyboard_text_chars(text)
        if unsupported:
            return {
                "output": f"error: keyboard_text supports only US-keyboard ASCII characters; unsupported characters: {unsupported!r}",
                "is_error": True,
            }
        return self._execute_input(state, episode_id, "keyboard_text", tool_input,
            lambda: state.input.text(text))

    def _call_keyboard_tap(self, state: BridgeEpisodeState, tool_input: dict[str, Any], episode_id: str) -> dict[str, Any]:
        """Execute keyboard_tap tool."""
        keys = tool_input.get("keys", [])
        if not keys:
            return {"output": "error: keys array is required", "is_error": True}
        if not isinstance(keys, list):
            return {"output": "error: keys must be an array", "is_error": True}

        # Handle meta+h -> home
        normalized_keys = [str(k).strip().lower() for k in keys if str(k).strip()]
        if not normalized_keys:
            return {"output": "error: at least one key or modifier is required", "is_error": True}
        alias_map = {
            "keycode_back": "back",
            "keycode_home": "home",
            "keycode_app_switch": "app_switch",
        }
        normalized_keys = [alias_map.get(k, k) for k in normalized_keys]
        has_meta = any(k in ("meta", "cmd", "super", "win") for k in normalized_keys)
        non_modifiers = [k for k in normalized_keys if k not in ("meta", "cmd", "super", "win", "ctrl", "alt", "shift")]

        native_keys = {"backspace", "delete", "tab", "space", "left", "right", "up", "down", "end", "pageup", "pagedown"}
        home_chord = has_meta and non_modifiers == ["h"] and len(normalized_keys) == 2
        if not home_chord and (any(k in ("ctrl", "control", "shift", "alt") for k in normalized_keys)
                or (has_meta and non_modifiers != ["h"] and non_modifiers)
                or any(k in native_keys or (len(k) == 1 and k.isascii()) for k in non_modifiers)):
            return self._execute_input(state, episode_id, "keyboard_tap", tool_input,
                lambda: state.input.keypress(normalized_keys))

        try:
            if has_meta and len(non_modifiers) == 0:
                action = build_action("home", {})
            elif len(non_modifiers) == 0:
                return self._call_noop_with_screenshot(state, episode_id)
            elif has_meta and len(non_modifiers) == 1 and non_modifiers[0] == "h":
                action = build_action("home", {})
            elif len(non_modifiers) == 1:
                key = non_modifiers[0]
                if key in ("enter", "return"):
                    action = build_action("key", {"key": "enter"})
                elif key in ("home",):
                    action = build_action("key", {"key": "home"})
                elif key in ("back", "escape", "esc"):
                    action = build_action("key", {"key": "back"})
                elif key in ("app_switch",):
                    action = _mobilegym_app_switch_action()
                else:
                    return {"output": f"error: mobilegym keyboard_tap does not support key: {key!r}", "is_error": True}
            else:
                return {"output": "error: mobilegym keyboard_tap supports one non-modifier key at a time", "is_error": True}
        except (TypeError, ValueError) as exc:
            return {"output": f"error: {exc}", "is_error": True}

        return self._execute_action(state, action, episode_id, tool_name="keyboard_tap", tool_input=tool_input)

    def _execute_input(
        self,
        state: BridgeEpisodeState,
        episode_id: str,
        tool_name: str,
        tool_input: dict[str, Any],
        invoke: Callable[[], Awaitable[None]],
    ) -> dict[str, Any]:
        async def execute(env: Any) -> dict[str, Any]:
            state.require_active(episode_id)
            started = time.monotonic()
            try:
                try:
                    await invoke()
                except Exception as exc:
                    state.log_action(
                        tool_name=tool_name, tool_input=tool_input,
                        mobilegym_action={"type": "input"},
                        duration_ms=int((time.monotonic() - started) * 1000),
                        error=str(exc), episode_id=episode_id,
                    )
                    return {"output": str(exc), "is_error": True}
                screenshot = _encode_observation_screenshot(await _maybe_await(env.get_observation()))
                state.log_action(
                    tool_name=tool_name, tool_input=tool_input,
                    mobilegym_action={"type": "input"},
                    duration_ms=int((time.monotonic() - started) * 1000),
                    error=None, episode_id=episode_id, screenshot=screenshot,
                )
                output = {
                    "action_output": "ok", "data": screenshot["data"],
                    "width": screenshot["width"], "height": screenshot["height"],
                    "format": screenshot.get("format", "jpeg"),
                }
                return {"output": json.dumps(output), "is_error": False}
            except asyncio.CancelledError:
                if state.input.contact_active:
                    await state.input.release()
                raise
        future = asyncio.run_coroutine_threadsafe(state.run_env(execute), state.owner_loop)
        try:
            return future.result(timeout=self.request_timeout_sec)
        except FutureTimeoutError:
            if not future.done():
                future.cancel()
            raise

    def _execute_action(
        self,
        state: BridgeEpisodeState,
        action: Any,
        episode_id: str,
        *,
        tool_name: str,
        tool_input: dict[str, Any],
    ) -> dict[str, Any]:
        """Execute a MobileGym action and return result with screenshot."""
        action_payload = action_to_dict(action)

        async def step_env(env: Any) -> dict[str, Any]:
            state.require_active(episode_id)
            started = time.monotonic()
            step_result = await _maybe_await(env.step(action))
            duration_ms = int((time.monotonic() - started) * 1000)

            observation = _observation_value(step_result, "observation")
            if observation is None:
                observation = await _maybe_await(env.get_observation())

            screenshot = _encode_observation_screenshot(observation)
            state.log_action(
                tool_name=tool_name,
                tool_input=tool_input,
                mobilegym_action=action_payload,
                duration_ms=duration_ms,
                error=None,
                episode_id=episode_id,
                screenshot=screenshot,
            )

            # Format output as post-action screenshot result
            output_data = {
                "action_output": "ok",
                "data": screenshot["data"],
                "width": screenshot["width"],
                "height": screenshot["height"],
                "format": screenshot.get("format", "jpeg"),
            }

            return {"output": json.dumps(output_data), "is_error": False}

        future = asyncio.run_coroutine_threadsafe(state.run_env(step_env), state.owner_loop)
        return future.result(timeout=self.request_timeout_sec)

    def _call_noop_with_screenshot(self, state: BridgeEpisodeState, episode_id: str) -> dict[str, Any]:
        async def get_screenshot(env: Any) -> dict[str, Any]:
            state.require_active(episode_id)
            observation = await _maybe_await(env.get_observation())
            screenshot = _encode_observation_screenshot(observation)
            output_data = {
                "action_output": "ok",
                "data": screenshot["data"],
                "width": screenshot["width"],
                "height": screenshot["height"],
                "format": screenshot.get("format", "jpeg"),
            }
            return {"output": json.dumps(output_data), "is_error": False}

        future = asyncio.run_coroutine_threadsafe(state.run_env(get_screenshot), state.owner_loop)
        return future.result(timeout=self.request_timeout_sec)

    def _send_json(self, handler: BaseHTTPRequestHandler, status: int, payload: dict[str, Any]) -> None:
        """Send JSON response."""
        data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
        handler.send_response(status)
        handler.send_header("Content-Type", "application/json")
        handler.send_header("Content-Length", str(len(data)))
        handler.end_headers()
        handler.wfile.write(data)


async def _maybe_await(value: Any) -> Any:
    if asyncio.iscoroutine(value):
        return await value
    return value


def _encode_observation_screenshot(observation: Any) -> dict[str, Any]:
    payload = _observation_value(observation, "screenshot")
    if payload is None:
        payload = _observation_value(observation, "screenshot_bytes")
    if isinstance(payload, str):
        payload = payload.encode("utf-8")
    if not isinstance(payload, bytes):
        raise ValueError("observation does not contain screenshot bytes")
    mime_type = _observation_value(observation, "mime_type") or _infer_screenshot_mime_type(payload)
    width = _observation_int(observation, "width") or _observation_int(observation, "screenshot_width")
    height = _observation_int(observation, "height") or _observation_int(observation, "screenshot_height")
    return encode_screenshot(payload, mime_type=str(mime_type), width=width, height=height)


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
        try:
            derived_duration_ms = distance / speed * 1000
        except OverflowError as exc:
            raise ValueError("speed is too low for this swipe; calculated duration is not finite") from exc
        if not math.isfinite(derived_duration_ms):
            raise ValueError("speed is too low for this swipe; calculated duration is not finite")
        if derived_duration_ms >= MAX_SWIPE_DURATION_MS + 0.5:
            raise ValueError(
                f"speed is too low for this swipe; calculated duration exceeds {MAX_SWIPE_DURATION_MS} ms"
            )
        duration_ms = max(1, int(math.floor(derived_duration_ms + 0.5)))
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
    try:
        speed = float(value)
    except (OverflowError, ValueError) as exc:
        raise ValueError("speed must be a positive finite number") from exc
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


def _finite_float(value: Any, name: str) -> float:
    try:
        parsed = float(value)
    except (TypeError, ValueError) as exc:
        raise ValueError(f"{name} must be a number") from exc
    if math.isnan(parsed) or math.isinf(parsed):
        raise ValueError(f"{name} must be a finite number")
    return parsed


def _unsupported_keyboard_text_chars(text: str) -> str:
    return "".join(ch for ch in text if ch not in US_KEYBOARD_TEXT_CHARS)


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
    }
    return aliases.get(action, action)


def _mobilegym_quick_action_catalog() -> list[dict[str, str]]:
    return [
        {"id": "back", "status": "active", "tool": "touch_gesture"},
        {"id": "home", "status": "active", "tool": "touch_gesture"},
        {"id": "app_switch", "status": "active", "tool": "touch_gesture"},
        {"id": "spotlight_search", "status": "active", "tool": "touch_gesture"},
        {"id": "search_launch_app", "status": "active", "tool": "touch_gesture"},
        {"id": "notification_center", "status": "active", "tool": "touch_gesture"},
        {"id": "control_center", "status": "active", "tool": "touch_gesture"},
        {"id": "dismiss_panel", "status": "active", "tool": "touch_gesture"},
        {"id": "send", "status": "active", "tool": "keyboard_tap"},
        {"id": "quit_app", "status": "active", "tool": "touch_gesture"},
        {"id": "browser_refresh", "status": "reserved", "tool": "keyboard_tap"},
        {"id": "browser_new_tab", "status": "reserved", "tool": "keyboard_tap"},
        {"id": "copy", "status": "reserved", "tool": "keyboard_tap"},
        {"id": "cut", "status": "reserved", "tool": "keyboard_tap"},
        {"id": "delete_backward", "status": "reserved", "tool": "keyboard_tap"},
        {"id": "find", "status": "reserved", "tool": "keyboard_tap"},
        {"id": "paste", "status": "reserved", "tool": "keyboard_tap"},
        {"id": "select_all", "status": "reserved", "tool": "keyboard_tap"},
        {"id": "undo", "status": "reserved", "tool": "keyboard_tap"},
    ]


def _mobilegym_reserved_quick_action(action: str, platform: str, reason: str) -> dict[str, Any]:
    output = {"ok": False, "action": action, "platform": platform, "status": "reserved", "reason": reason}
    return {"output": json.dumps(output), "is_error": False}


def _infer_screenshot_mime_type(payload: bytes) -> str:
    if payload.startswith(b"\x89PNG\r\n\x1a\n"):
        return "image/png"
    if payload.startswith(b"\xff\xd8"):
        return "image/jpeg"
    return "application/octet-stream"


def _observation_int(observation: Any, name: str) -> int | None:
    value = _observation_value(observation, name)
    if value is None:
        return None
    return int(value)


def _observation_value(observation: Any, name: str) -> Any:
    if isinstance(observation, dict):
        value = observation.get(name)
    else:
        value = getattr(observation, name, None)
    if callable(value):
        return value()
    return value
