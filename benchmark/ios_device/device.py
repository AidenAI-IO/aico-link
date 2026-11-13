from __future__ import annotations

import base64
import io
import threading
import time
from urllib.parse import quote

from .permissions import PermissionPolicy, alert_snapshot, deduplicate_alert_buttons
from .text import TYPING_FREQUENCY, validate_ios_text
from .transport import IOSDeviceError
from .touch import validate_touch_actions, wda_actions


class IOSDevice:
    platform = "ios"
    backend = "ios-device"
    bridge_type = "ios_device"
    mnk_capabilities = {"insert_text": True, "system_actions": ["home"]}

    def __init__(self, transport, policy=None):
        self.transport = transport
        self.policy = policy or PermissionPolicy()
        self.lock = threading.RLock()
        self.auto_attempted = set()
        caps = {"shouldTerminateApp": False, "defaultAlertAction": ""}
        response = transport.request("POST", "/session", {"capabilities": {"alwaysMatch": caps}})
        value = response.get("value")
        self.session = response.get("sessionId") or (value.get("sessionId") if isinstance(value, dict) else None)
        if not isinstance(self.session, str) or not self.session:
            raise IOSDeviceError("wda_unavailable", "WDA did not create a session.")

    def _call(self, method, path, payload=None):
        result = self.transport.request(method, f"/session/{quote(self.session, safe='')}{path}", payload)
        return result.get("value")

    def check_device(self):
        with self.lock:
            self.transport.request("GET", "/status")
            locked = self._call("GET", "/wda/locked")
            return {"backend": self.backend, "connected": True, "locked": bool(locked),
                    "input_available": not locked, "permission_policy_enabled": bool(self.policy.rules)}

    def _unlocked(self):
        if self._call("GET", "/wda/locked"):
            raise IOSDeviceError("device_locked", "Unlock the iPhone to use the direct iOS bridge.")

    def read_alert(self):
        with self.lock:
            try:
                text = self._call("GET", "/alert/text")
                buttons = self._call("GET", "/wda/alert/buttons")
                if isinstance(buttons, list) and len(buttons) != len(set(buttons)):
                    buttons = deduplicate_alert_buttons(text, buttons, self._call("GET", "/source"))
                # Reading two endpoints is not atomic; reject a changed prompt.
                if text != self._call("GET", "/alert/text"):
                    raise IOSDeviceError("alert_changed", "The alert changed while it was being read; inspect it again.")
                return alert_snapshot(text, buttons)
            except IOSDeviceError as exc:
                if exc.code != "no_alert":
                    raise
                self.auto_attempted.clear()
                return {"present": False}

    def select_alert(self, alert_id, button):
        if not isinstance(alert_id, str) or not alert_id or not isinstance(button, str) or not button:
            raise ValueError("alert_id and button are required")
        with self.lock:
            self._unlocked()
            before = self.read_alert()
            if not before.get("present") or before["alert_id"] != alert_id:
                raise IOSDeviceError("alert_changed", "The alert changed; inspect it again before selecting a button.")
            if before["buttons"].count(button) != 1:
                raise ValueError("button must exactly match one displayed alert button")
            self._call("POST", "/alert/accept", {"name": button})
            deadline = time.monotonic() + 3
            while True:
                after = self.read_alert()
                if after.get("alert_id") != alert_id:
                    return {"selected": button, "dismissed": True, "next_alert": after}
                if time.monotonic() >= deadline:
                    raise IOSDeviceError("selection_unverified", "The button was sent but the alert remains; inspect it before retrying.")
                time.sleep(0.15)

    def apply_permission_policy(self):
        if not self.policy.rules:
            return None
        with self.lock:
            alert = self.read_alert()
            button = self.policy.choose(alert)
            if button is None or alert["alert_id"] in self.auto_attempted:
                return None
            # Never replay a selection after a timeout or partial delivery.
            self.auto_attempted.add(alert["alert_id"])
            return self.select_alert(alert["alert_id"], button)

    def screenshot_jpeg(self, quality=85):
        from PIL import Image

        with self.lock:
            self.apply_permission_policy()
            encoded = self._call("GET", "/screenshot")
            try:
                image = Image.open(io.BytesIO(base64.b64decode(encoded, validate=True)))
                image.load()
            except (ValueError, TypeError, OSError) as exc:
                raise IOSDeviceError("capture_failed", "WDA did not return a valid screenshot.") from exc
            output = io.BytesIO()
            image.convert("RGB").save(output, format="JPEG", quality=max(1, min(100, quality)))
            return output.getvalue(), image.width, image.height

    def _point(self, x, y):
        if any(isinstance(v, bool) or not isinstance(v, (int, float)) or not 0 <= v <= 1000 for v in (x, y)):
            raise ValueError("coordinates must be in [0, 1000]")
        size = self._call("GET", "/window/size")
        return {"x": min(x * size["width"] / 1000, size["width"] - 1),
                "y": min(y * size["height"] / 1000, size["height"] - 1)}

    def touch_actions(self, actions):
        validate_touch_actions(actions)
        with self.lock:
            self._unlocked()
            size = self._call("GET", "/window/size")
            self._call("POST", "/actions", wda_actions(actions, size))

    def click(self, x, y, *, button="left", clicks=1, interval=0.12):
        if button != "left" or clicks not in (1, 2):
            raise ValueError("iOS supports single/double left taps")
        with self.lock:
            self._unlocked()
            point = self._point(x, y)
            path = "/wda/tap" if clicks == 1 else "/wda/doubleTap"
            self._call("POST", path, point)

    def long_press(self, x, y, hold_sec=0.5, button="left"):
        if button != "left" or not 0 <= hold_sec <= 10:
            raise ValueError("long press requires the left button and a duration in [0, 10]")
        with self.lock:
            self._unlocked()
            self._call("POST", "/wda/touchAndHold", {**self._point(x, y), "duration": hold_sec})

    def drag(self, start, end, duration=0.4, button="left"):
        if button != "left" or not 0 <= duration <= 10:
            raise ValueError("drag requires the left button and a duration in [0, 10]")
        # WDA's dragfromtoforduration interprets duration as the initial hold,
        # whereas the MNK protocol specifies movement time. W3C actions retain it.
        self.touch_actions([
            {"action": "touch_down", "point": dict(zip(("x", "y"), start))},
            {"action": "move_to", "point": dict(zip(("x", "y"), end)), "ms": round(duration * 1000)},
            {"action": "touch_up"}])

    def scroll(self, delta):
        distance = max(-700, min(700, delta * 20))
        self.drag((500, 500 + distance / 2), (500, 500 - distance / 2))

    def move(self, x, y):
        raise ValueError("iOS has no mouse pointer; use a tap or drag")

    def write(self, text):
        self.insert_text(text)

    def insert_text(self, text, replace=False):
        validate_ios_text(text)
        if not isinstance(replace, bool):
            raise ValueError("replace must be a boolean")
        with self.lock:
            self._unlocked()
            if replace:
                element = self._call("GET", "/element/active")
                identifier = element.get("element-6066-11e4-a52e-4f735466cecf") or element.get("ELEMENT")
                if not identifier:
                    raise IOSDeviceError("field_unavailable", "No focused field could be identified before replacement.")
                self._call("POST", f"/element/{quote(identifier, safe='')}/clear", {})
            self._call("POST", "/wda/keys", {"value": [text], "frequency": TYPING_FREQUENCY})

    def press(self, keys):
        values = {"enter": "\n", "return": "\n", "tab": "\t", "backspace": "\b", "delete": "\b"}
        if len(keys) != 1 or keys[0].lower() not in values:
            raise ValueError("WDA keyboard supports enter, tab, and backspace; use native text insertion for text")
        with self.lock:
            self._unlocked()
            self._call("POST", "/wda/keys", {"value": [values[keys[0].lower()]]})

    def quick_action(self, action):
        with self.lock:
            self._unlocked()
            if action == "home":
                # Native Home also dismisses Spotlight. Activating SpringBoard
                # alone leaves search open; edge swipes may scroll the app.
                self._call("POST", "/wda/pressButton", {"name": "home"})
            elif action == "app_switch":
                self.touch_actions([
                    {"action": "touch_down", "point": {"x": 500, "y": 1000}},
                    {"action": "move_to", "point": {"x": 500, "y": 500}, "ms": 350},
                    {"action": "wait", "ms": 250}, {"action": "touch_up"}])
            elif action == "spotlight_search":
                self.quick_action("home")
                time.sleep(0.3)
                self.drag((500, 250), (500, 750))
            else:
                raise ValueError(f"unsupported iOS quick action: {action}")
