from __future__ import annotations

import base64
import json
import math
import subprocess
import threading
from pathlib import Path

from desktop.bridge.device import DesktopDeviceError
from mnk_provider import validate_insert_text


def parse_insets(value: str) -> tuple[float, ...]:
    try:
        result = tuple(float(part) for part in value.split(","))
    except ValueError as exc:
        raise ValueError("content insets must be left,top,right,bottom in macOS points") from exc
    if len(result) != 4 or any(not math.isfinite(part) or part < 0 for part in result):
        raise ValueError("content insets must contain four finite, non-negative values")
    return result


class MirroringDevice:
    platform = "ios"
    backend = "iphone-mirroring"
    bridge_type = "iphone_mirroring"
    mnk_capabilities = {"insert_text": True}

    def __init__(self, helper: Path, insets=(8.0, 38.0, 8.0, 8.0)):
        self.helper = Path(helper)
        self.insets = insets
        self.lock = threading.RLock()

    def _call(self, command: str, **params) -> dict:
        with self.lock:
            try:
                result = subprocess.run(
                    [str(self.helper)], input=json.dumps({"command": command, "insets": self.insets, **params}),
                    text=True, capture_output=True, timeout=30, check=True,
                )
                response = json.loads(result.stdout)
            except (OSError, subprocess.SubprocessError, ValueError) as exc:
                raise DesktopDeviceError(f"iPhone Mirroring helper failed: {exc}") from exc
            if not response.get("ok"):
                raise DesktopDeviceError(response.get("error", "iPhone Mirroring helper failed"))
            return response["data"]

    def diagnostics(self) -> dict:
        result = self._call("status")
        return {"state": "window_visible" if result["window_visible"] else "window_unavailable", "backend": self.backend, **result,
                "connection_verified": False,
                "input_available": result["accessibility"],
                "permission_hint": "For input permission, run python3 -m iphone_mirroring --request-input-access and approve the responsible app in Privacy & Security > Accessibility (Device Control and Data Access). Keep iPhone Mirroring connected and the iPhone locked."}

    def check_device(self) -> dict:
        result = self.diagnostics()
        if not result["screen_recording"]:
            raise DesktopDeviceError("Screen Recording permission is missing; run python3 -m iphone_mirroring --request-screen-access, approve the responsible app, then restart the bridge")
        if not result["window_visible"]:
            raise DesktopDeviceError(result.get("window_error", "No visible iPhone Mirroring window"))
        return result

    def screenshot_jpeg(self, quality=85):
        result = self._call("capture", quality=max(1, min(100, quality)) / 100)
        return base64.b64decode(result["image"], validate=True), int(result["width"]), int(result["height"])

    def click(self, x, y, *, button="left", clicks=1, interval=0.12):
        if button != "left":
            raise ValueError("iPhone Mirroring supports left clicks only")
        self._call("click", x=x, y=y, clicks=clicks)

    def long_press(self, x, y, hold_sec=0.5, button="left"):
        if button != "left" or not 0 <= hold_sec <= 5:
            raise ValueError("long press requires the left button and a duration of at most 5 seconds")
        self._call("click", x=x, y=y, hold_sec=hold_sec)

    def move(self, x, y):
        self._call("move", x=x, y=y)

    def swipe(self, start, end, duration=0.4):
        with self.lock:
            info = self._call("status")
            if not info["window_visible"]:
                raise DesktopDeviceError(info.get("window_error", "No visible iPhone Mirroring window"))
            # Finger movement maps to continuous pixel scroll, not mouse drag.
            self._call("scroll", x=start[0], y=start[1],
                       dx=(end[0] - start[0]) * info["width"] / 1000,
                       dy=(end[1] - start[1]) * info["height"] / 1000,
                       duration=duration)

    def drag(self, *args, **kwargs):
        raise ValueError("persistent drag and multi-touch are not supported by this mirror adapter")

    def scroll(self, delta):
        self.scroll_axes(0, delta)

    def scroll_axes(self, x, y):
        # Agent scroll primitives are wheel units; native helper uses pixels.
        self._call("scroll", dx=x * 20, dy=y * 20)

    def write(self, text):
        if not isinstance(text, str) or not text or len(text.encode("utf-16-le")) > 8192:
            raise ValueError("text must contain 1–4096 UTF-16 units")
        self._call("text", text=text)

    def press(self, keys):
        self._call("keypress", keys=keys)

    def insert_text(self, text, replace=False):
        validate_insert_text(text)
        if not isinstance(replace, bool):
            raise ValueError("replace must be a boolean")
        self._call("insert_text", text=text, replace=replace)

    def quick_action(self, action):
        key = {"home": "1", "app_switch": "2", "spotlight_search": "3"}.get(action)
        if key is None:
            raise ValueError(f"unsupported iPhone Mirroring quick action: {action}")
        self.press(["meta", key])
