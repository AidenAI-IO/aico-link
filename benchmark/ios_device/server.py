from __future__ import annotations

from desktop.bridge.server import DesktopBridgeServer, _handler_for
from desktop.bridge.state import NoBridgeEnvAvailableError, benchmark_task_id_from_headers
from desktop.bridge.tools_api import DesktopToolsAPIHandler
from mnk_provider import execute_mnk_request
from .text import validate_ios_text
from .transport import IOSDeviceError
from .touch import validate_touch_actions


class IOSTools(DesktopToolsAPIHandler):
    def catalog(self):
        tools = [tool for tool in super().catalog() if tool["name"] != "mouse_move"]
        for tool in tools:
            tool["description"] = tool["description"].replace("desktop", "iPhone").replace("mouse", "touch")
            if tool["name"] == "touch_gesture":
                tool["args_schema"]["properties"]["type"]["enum"] = ["tap", "double_tap", "long_press", "swipe", "drag", "swipe_left", "swipe_right", "swipe_up", "swipe_down", "home"]
                tool["args_schema"]["properties"]["button"]["enum"] = ["left"]
            if tool["name"] == "keyboard_tap":
                tool["description"] = "Send Enter, Tab, or Backspace/Delete to the focused iPhone field; modifier chords are unsupported."
        tools.append({"name": "permission_dialog", "description": "Inspect an iOS alert or select an exact displayed button from a previously inspected alert.",
                      "args_schema": {"type": "object", "properties": {"alert_id": {"type": "string"}, "button": {"type": "string"}}}})
        return tools

    def _submit_tool_call(self, name, data):
        if name == "system_action":
            if data.get("action") != "home":
                raise ValueError("unsupported system action")
            self.state.device.quick_action("home")
            return {"ok": True}
        if name == "touch_actions":
            self.state.device.touch_actions(data["actions"])
            return {"ok": True}
        if name == "permission_dialog":
            if "button" in data:
                return self.state.device.select_alert(data.get("alert_id"), data["button"])
            return self.state.device.read_alert()
        if name == "insert_text":
            self.state.device.insert_text(data["text"], data.get("replace", False))
            return {"ok": True}
        if name == "quick_action" and data.get("list"):
            return {"actions": ["home", "app_switch", "spotlight_search"]}
        return super()._submit_tool_call(name, data)


def execute_ios_mnk(payload, invoke):
    operation = payload.get("operation")
    if operation not in ("insert_text", "touch_actions", "system_action"):
        return execute_mnk_request(payload, invoke)
    params = payload.get(operation)
    try:
        if operation == "touch_actions":
            validate_touch_actions(params)
            params = {"actions": params}
        elif operation == "system_action":
            if not isinstance(params, dict) or params.get("action") != "home":
                raise ValueError("system_action requires action home")
        else:
            if not isinstance(params, dict):
                raise ValueError("insert_text params required")
            validate_ios_text(params.get("text"))
            if not isinstance(params.get("replace", False), bool):
                raise ValueError("replace must be a boolean")
    except ValueError as exc:
        return 400, {"error": str(exc)}
    result = invoke(operation, params)
    return (500, {"error": result["output"]}) if result.get("is_error") else (200, {"success": True})


def ios_handler(bridge):
    class Handler(_handler_for(bridge)):
        def _alert(self):
            try:
                bridge.state.check_task_access(benchmark_task_id_from_headers(self.headers))
                if self.command == "GET":
                    result = bridge.state.device.read_alert()
                else:
                    payload = self._read_json()
                    result = bridge.state.device.select_alert(payload.get("alert_id"), payload.get("button"))
                self._send(200, {"ok": True, "data": result})
            except NoBridgeEnvAvailableError as exc:
                self._send_error(429, "no_bridge_env_available", str(exc))
            except IOSDeviceError as exc:
                self._send_error(409 if exc.code == "alert_changed" else 503, exc.code, str(exc))
            except ValueError as exc:
                self._send_error(400, "bad_request", str(exc))
            except Exception as exc:
                self._send_error(500, "bridge_error", str(exc))

        def do_GET(self):
            if self.path.split("?", 1)[0] == "/api/ios/alert":
                return self._alert()
            return super().do_GET()

        def do_POST(self):
            if self.path.split("?", 1)[0] == "/api/ios/alert":
                return self._alert()
            return super().do_POST()

    return Handler


class IOSServer(DesktopBridgeServer):
    def __init__(self, device, host="127.0.0.1", port=8896, action_settle_sec=0.2):
        super().__init__(device, host, port, action_settle_sec, tools_factory=IOSTools,
                         mnk_executor=execute_ios_mnk, handler_factory=ios_handler)
