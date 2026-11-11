from __future__ import annotations

import base64
import json
import subprocess
import urllib.error
import urllib.request
from pathlib import Path

import pytest

from desktop.bridge.device import DesktopDeviceError
from iphone_mirroring.device import MirroringDevice, parse_insets
from iphone_mirroring.server import MirroringServer, execute_mirror_mnk


class FakeNative(MirroringDevice):
    def __init__(self):
        super().__init__(Path("unused"))
        self.calls = []
        self.size = (286, 619)

    def _call(self, command, **params):
        self.calls.append((command, params))
        if command == "status":
            return {"width": self.size[0], "height": self.size[1], "screen_recording": True, "accessibility": True, "window_visible": True}
        if command == "capture":
            return {"image": base64.b64encode(b"jpeg-frame").decode(), "width": 572, "height": 1238}
        return {}


def request(bridge, path, payload=None, task="mirror"):
    req = urllib.request.Request(bridge.base_url + path,
                                 data=None if payload is None else json.dumps(payload).encode(),
                                 headers={"benchmark-task-id": task, "Content-Type": "application/json"})
    try:
        with urllib.request.urlopen(req, timeout=3) as response:
            return response.status, json.load(response)
    except urllib.error.HTTPError as exc:
        return exc.code, json.load(exc)


@pytest.fixture
def mirror():
    bridge = MirroringServer(FakeNative(), port=0, action_settle_sec=0)
    bridge.start()
    try:
        assert request(bridge, "/api/setup", {})[0] == 200
        yield bridge
    finally:
        bridge.stop()


def test_mirror_http_capture_and_exclusive_input(mirror):
    health = request(mirror, "/health")[1]["data"]
    assert health["platform"] == "ios" and health["input_available"]
    # A visible window is not evidence of a connected, unlocked mirror session.
    assert health["connection_verified"] is False
    status, frame = request(mirror, "/api/providers/screenshot", {})
    assert status == 200
    assert (frame["data"]["meta"]["width"], frame["data"]["meta"]["height"]) == (572, 1238)
    assert base64.b64decode(frame["data"]["image"]) == b"jpeg-frame"
    click = {"operation": "click", "click": {"x": 123, "y": 456}}
    before = len(mirror.state.device.calls)
    assert request(mirror, "/api/providers/mnk", click, task="another-task")[0] == 429
    assert len(mirror.state.device.calls) == before
    assert request(mirror, "/api/providers/mnk", click)[0] == 200
    assert mirror.state.device.calls[-1] == ("click", {"x": 123.0, "y": 456.0, "clicks": 1})


def test_mirror_unicode_horizontal_scroll_and_shortcuts(mirror):
    assert request(mirror, "/api/providers/mnk")[1]["capabilities"]["insert_text"] is True
    insertion = {"operation": "insert_text", "insert_text": {"text": "中文ABC🔋\n第二行", "replace": True}}
    before = len(mirror.state.device.calls)
    assert request(mirror, "/api/providers/mnk", insertion, task="another-task")[0] == 429
    assert len(mirror.state.device.calls) == before
    assert request(mirror, "/api/providers/mnk", insertion)[0] == 200
    assert mirror.state.device.calls[-1] == ("insert_text", {"text": "中文ABC🔋\n第二行", "replace": True})
    assert request(mirror, "/api/providers/mnk", {"operation": "keyboard_text", "keyboard_text": {"text": "电池🔋"}})[0] == 200
    assert mirror.state.device.calls[-1] == ("text", {"text": "电池🔋"})
    assert request(mirror, "/api/providers/mnk", {"operation": "scroll", "scroll": {"scroll_x": -4}})[0] == 200
    assert mirror.state.device.calls[-1] == ("scroll", {"dx": -80, "dy": 0})
    assert request(mirror, "/api/tools/quick_action", {"input": {"action": "home"}})[1]["is_error"] is False
    assert mirror.state.device.calls[-1] == ("keypress", {"keys": ["meta", "1"]})


def test_swipes_use_current_window_size_and_never_mouse_drag(mirror):
    device = mirror.state.device
    device.size = (400, 800)
    swipe = {"operation": "swipe", "swipe": {"path": [[500, 750], [500, 250]], "duration_ms": 600}}
    assert request(mirror, "/api/providers/mnk", swipe)[0] == 200
    assert device.calls[-1] == ("scroll", {"x": 500.0, "y": 750.0, "dx": 0.0, "dy": -400.0, "duration": 0.6})
    before = len(device.calls)
    swipe["operation"] = "drag"
    swipe["drag"] = swipe.pop("swipe")
    assert request(mirror, "/api/providers/mnk", swipe)[0] == 500
    assert len(device.calls) == before


@pytest.mark.parametrize("payload", [
    {"operation": "insert_text", "insert_text": {"text": "\u0000"}},
    {"operation": "insert_text", "insert_text": {"text": "\u007f"}},
    {"operation": "insert_text", "insert_text": {"text": "\u0085"}},
    {"operation": "insert_text", "insert_text": {"text": "\ud800"}},
    {"operation": "insert_text", "insert_text": {"text": "🔋" * 2049}},
    {"operation": "insert_text", "insert_text": {"text": "中文", "replace": "true"}},
    {"operation": "click", "click": {"x": 1001, "y": 0}},
    {"operation": "scroll", "scroll": {"scroll_y": True}},
    {"operation": "scroll", "scroll": {"scroll_x": 128}},
    {"operation": "keyboard_text", "keyboard_text": {"text": ""}},
    {"operation": "keyboard_text", "keyboard_text": {"text": "🔋" * 2049}},
    {"operation": "touch_actions", "touch_actions": []},
])
def test_invalid_or_unsupported_mnk_never_dispatches(payload):
    def unexpected(*args):
        pytest.fail("invalid request reached input backend")
    assert execute_mirror_mnk(payload, unexpected)[0] == 400


def test_native_permission_error_is_not_reported_as_success(monkeypatch):
    def run(*args, **kwargs):
        assert json.loads(kwargs["input"])["command"] == "click"
        return subprocess.CompletedProcess(args[0], 0, '{"ok":false,"error":"Accessibility permission required"}', "")
    monkeypatch.setattr(subprocess, "run", run)
    with pytest.raises(DesktopDeviceError, match="Accessibility"):
        MirroringDevice(Path("helper")).click(100, 200)


def test_permissions_are_rechecked_and_probe_works_without_a_window(monkeypatch):
    device = MirroringDevice(Path("helper"))
    status = {"window_visible": False, "screen_recording": False, "accessibility": False}
    monkeypatch.setattr(device, "_call", lambda *args, **kwargs: dict(status))
    assert device.diagnostics()["input_available"] is False
    with pytest.raises(DesktopDeviceError, match="request-screen-access"):
        device.check_device()
    status.update(window_visible=True, screen_recording=True, accessibility=True)
    assert device.check_device()["input_available"] is True
    status["accessibility"] = False
    assert device.check_device()["input_available"] is False
    status["window_visible"] = False
    with pytest.raises(DesktopDeviceError, match="No visible"):
        device.check_device()


@pytest.mark.parametrize("value", ["0,0,0", "0,nan,0,0", "0,-1,0,0", "0,inf,0,0"])
def test_invalid_crop_is_rejected(value):
    with pytest.raises(ValueError):
        parse_insets(value)
