from __future__ import annotations

import time

from desktop.bridge.server import DesktopBridgeServer
from desktop.bridge.tools_api import DesktopToolsAPIHandler, _point, _duration
from mnk_provider import execute_mnk_request
from .device import validate_insert_text


class MirroringTools(DesktopToolsAPIHandler):
    def catalog(self):
        catalog = super().catalog()
        for tool in catalog:
            tool["description"] = tool["description"].replace("desktop", "mirrored iPhone")
            if tool["name"] == "touch_gesture":
                tool["args_schema"]["properties"]["type"]["enum"] = ["tap", "double_tap", "long_press", "swipe", "swipe_left", "swipe_right", "swipe_up", "swipe_down", "home"]
            if tool["name"] == "quick_action":
                tool["description"] = "Open the mirrored iPhone Home Screen, App Switcher, or Spotlight."
                tool["args_schema"]["properties"]["action"]["enum"] = ["home", "app_switch", "spotlight_search"]
        return catalog

    def _submit_tool_call(self, name, data):
        device = self.state.device
        if name == "quick_action":
            if data.get("list"):
                return {"actions": ["home", "app_switch", "spotlight_search"]}
            device.quick_action(str(data.get("action") or ""))
        elif name == "touch_gesture" and str(data.get("type", "")).startswith("swipe"):
            if data.get("button", "left") != "left":
                raise ValueError("swipes require the left button")
            gesture = data["type"]
            if gesture == "swipe":
                start, end = _point(data.get("start")), _point(data.get("end"))
            else:
                points = {"swipe_up": ((500, 750), (500, 250)), "swipe_down": ((500, 250), (500, 750)),
                          "swipe_left": ((750, 500), (250, 500)), "swipe_right": ((250, 500), (750, 500))}
                if gesture not in points:
                    raise ValueError("unsupported swipe")
                start, end = points[gesture]
            duration = _duration(data.get("duration_ms", 400)) / 1000
            device.swipe(start, end, duration)
        elif name == "mirror_scroll":
            device.scroll_axes(data["x"], data["y"])
        elif name == "insert_text":
            device.insert_text(data.get("text"), data.get("replace", False))
        elif name == "enter_text":
            validate_insert_text(data.get("text"))
            if data.get("focus") is not None:
                device.click(*_point(data["focus"]))
            device.insert_text(data["text"])
        else:
            return super()._submit_tool_call(name, data)
        if self.action_settle_sec:
            time.sleep(self.action_settle_sec)
        return {"ok": True}


def execute_mirror_mnk(payload, invoke):
    # Keep Unicode and horizontal scrolling local to this backend. Other
    # environment bridges retain their current protocol restrictions.
    operation = str(payload.get("operation", "")).strip().lower()
    if operation == "insert_text":
        params = payload.get(operation)
        text = params.get("text") if isinstance(params, dict) else None
        try:
            validate_insert_text(text)
        except ValueError as exc:
            return 400, {"error": str(exc)}
        replace = params.get("replace", False)
        if not isinstance(replace, bool):
            return 400, {"error": "replace must be a boolean"}
        result = invoke("insert_text", {"text": text, "replace": replace})
    elif operation == "keyboard_text":
        params = payload.get(operation)
        text = params.get("text") if isinstance(params, dict) else None
        if not isinstance(text, str) or not text or len(text.encode("utf-16-le")) > 8192:
            return 400, {"error": "keyboard_text requires 1–4096 UTF-16 units"}
        result = invoke("keyboard_text", {"text": text})
    elif operation == "scroll":
        params = payload.get(operation)
        if not isinstance(params, dict):
            return 400, {"error": "scroll params required"}
        x, y = params.get("scroll_x", 0), params.get("scroll_y", 0)
        if any(isinstance(v, bool) or not isinstance(v, int) or abs(v) > 127 for v in (x, y)):
            return 400, {"error": "scroll axes must be integers in [-127, 127]"}
        result = invoke("mirror_scroll", {"x": x, "y": y})
    else:
        return execute_mnk_request(payload, invoke)
    if result.get("is_error"):
        return 500, {"error": result.get("output", "mirror input failed")}
    return 200, {"success": True}


class MirroringServer(DesktopBridgeServer):
    def __init__(self, device, host="127.0.0.1", port=8897, action_settle_sec=0.3):
        super().__init__(device, host, port, action_settle_sec,
                         tools_factory=MirroringTools, mnk_executor=execute_mirror_mnk)
