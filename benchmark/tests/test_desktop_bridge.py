from __future__ import annotations

import base64
import json
import urllib.error
import urllib.request

import pytest

from desktop.bridge.device import DesktopDevice
from desktop.bridge.server import DesktopBridgeServer
from desktop.scripts import start_bridge
from desktop.bridge.state import DesktopBridgeState, NoBridgeEnvAvailableError
from desktop.bridge.tools_api import _read_body


class FakeDevice:
    platform = "linux"
    backend = "fake"

    def __init__(self):
        self.calls = []

    def check_device(self):
        return {"state": "online", "width": 1000, "height": 500, "backend": self.backend}

    def screenshot_jpeg(self, quality=85):
        # Minimal JPEG header is sufficient for protocol tests; dimensions come
        # from the fake device rather than image decoding.
        return b"\xff\xd8\xff\xd9", 1000, 500

    def click(self, *args, **kwargs): self.calls.append(("click", args, kwargs))
    def long_press(self, *args, **kwargs): self.calls.append(("long_press", args, kwargs))
    def drag(self, *args, **kwargs): self.calls.append(("drag", args, kwargs))
    def move(self, *args, **kwargs): self.calls.append(("move", args, kwargs))
    def scroll(self, *args, **kwargs): self.calls.append(("scroll", args, kwargs))
    def write(self, *args, **kwargs): self.calls.append(("write", args, kwargs))
    def press(self, *args, **kwargs): self.calls.append(("press", args, kwargs))
    def quick_action(self, *args, **kwargs): self.calls.append(("quick_action", args, kwargs))


@pytest.fixture()
def bridge():
    server = DesktopBridgeServer(device=FakeDevice(), host="127.0.0.1", port=0, action_settle_sec=0)
    server.start()
    try:
        yield server
    finally:
        server.stop()


def request(bridge, path, payload=None, method=None, headers=None):
    body = None if payload is None else json.dumps(payload).encode()
    req = urllib.request.Request(bridge.base_url + path, data=body, method=method or ("POST" if body else "GET"), headers=headers or {})
    try:
        with urllib.request.urlopen(req, timeout=3) as response:
            return response.status, json.loads(response.read())
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read())


def test_health_concurrent_and_screenshot(bridge):
    status, health = request(bridge, "/health")
    assert status == 200
    assert health["data"]["platform"] == "linux"
    assert request(bridge, "/api/concurrent")[1]["data"]["concurrent"] == 1
    owner = {"benchmark-task-id": "health-task"}
    assert request(bridge, "/api/setup", {}, headers=owner)[0] == 200
    status, payload = request(bridge, "/api/providers/screenshot", {}, headers=owner)
    assert status == 200
    frame = payload["data"]
    assert frame["meta"]["width"] == 1000
    assert base64.b64decode(frame["image"]) == b"\xff\xd8\xff\xd9"


def test_task_routing_and_mnk(bridge):
    owner = {"benchmark-task-id": "one"}
    assert request(bridge, "/api/setup", {}, headers=owner)[0] == 200
    assert request(bridge, "/api/providers/mnk", {"operation": "click", "click": {"x": 100, "y": 200}}, headers=owner)[0] == 200
    assert request(bridge, "/api/providers/screenshot", {}, headers={"benchmark-task-id": "two"})[0] == 429
    assert request(bridge, "/api/release", {}, headers=owner)[1]["data"]["released"] is True


def test_tools_catalog_and_keyboard_modifier(bridge):
    status, catalog = request(bridge, "/api/tools")
    assert status == 200
    assert {tool["name"] for tool in catalog["tools"]} >= {"touch_gesture", "keyboard_text", "keyboard_tap", "quick_action"}
    owner = {"benchmark-task-id": "tools-task"}
    assert request(bridge, "/api/setup", {}, headers=owner)[0] == 200
    status, result = request(bridge, "/api/tools/keyboard_tap", {"input": {"keys": ["ctrl", "l"]}}, headers=owner)
    assert status == 200
    assert result["is_error"] is False
    assert bridge.state.device.calls[-1][0] == "press"


def test_desktop_device_coordinate_validation_without_backend():
    device = DesktopDevice.__new__(DesktopDevice)
    device.system = "linux"
    device._pyautogui = None
    with pytest.raises(ValueError):
        device._pixels(-1, 500)


def test_desktop_device_clamps_normalized_screen_edges():
    device = DesktopDevice.__new__(DesktopDevice)
    device.system = "linux"
    device._pyautogui = None
    device.screen_size = lambda: (100, 50)
    assert device._pixels(1000, 1000) == (99, 49)


def test_desktop_device_uses_system_profiler_fallback_on_macos(monkeypatch):
    device = DesktopDevice.__new__(DesktopDevice)
    device.system = "darwin"
    device._pyautogui = None
    captured = {}

    monkeypatch.setattr("desktop.bridge.device.shutil.which", lambda name: None)
    monkeypatch.setattr(
        "desktop.bridge.device.os.path.exists",
        lambda path: path == "/usr/sbin/system_profiler",
    )

    def fake_run(command, capture_output, text, timeout):
        captured["command"] = command

        class Result:
            stdout = "Resolution: 1440 x 900\n"

        return Result()

    monkeypatch.setattr("desktop.bridge.device.subprocess.run", fake_run)

    assert device.screen_size() == (1440, 900)
    assert captured["command"][0] == "/usr/sbin/system_profiler"


def test_task_access_requires_matching_active_lease():
    state = DesktopBridgeState(device=FakeDevice())
    with pytest.raises(NoBridgeEnvAvailableError):
        state.check_task_access("")
    with pytest.raises(NoBridgeEnvAvailableError):
        state.check_task_access("task")
    with pytest.raises(ValueError):
        state.acquire("")

    episode, newly_acquired = state.acquire("task")
    assert (episode, newly_acquired) == ("task", True)
    with pytest.raises(NoBridgeEnvAvailableError):
        state.check_task_access("other")
    state.check_task_access("task")


def test_protected_http_calls_require_task_lease(bridge):
    assert request(bridge, "/api/setup", {})[0] == 400
    assert request(bridge, "/api/providers/screenshot", {})[0] == 429
    assert request(bridge, "/api/tools/keyboard_tap", {"input": {"keys": ["ctrl", "l"]}})[0] == 429


def test_read_body_rejects_negative_content_length():
    class Handler:
        headers = {"Content-Length": "-1"}

        class Body:
            def read(self, length):
                raise AssertionError("body must not be read")

        rfile = Body()

    with pytest.raises(ValueError, match="non-negative"):
        _read_body(Handler())


@pytest.mark.parametrize(
    ("host", "allow_private_bind"),
    [
        ("0.0.0.0", False),
        ("0.0.0.0", True),
        ("172.17.0.1", False),
        ("8.8.8.8", True),
        ("::1", True),
        ("example.com", True),
    ],
)
def test_start_bridge_rejects_disallowed_host(monkeypatch, host, allow_private_bind):
    monkeypatch.setattr(start_bridge, "DesktopBridgeServer", lambda **kwargs: pytest.fail("server must not be constructed"))
    argv = ["--bridge-host", host] + (["--allow-private-bind"] if allow_private_bind else [])
    with pytest.raises(SystemExit) as excinfo:
        start_bridge.main(argv)
    assert excinfo.value.code == 2


@pytest.mark.parametrize(
    ("host", "allow_private_bind"),
    [
        ("127.0.0.1", False),
        ("localhost", False),
        ("172.17.0.1", True),
        ("192.168.65.254", True),
        ("10.0.0.5", True),
        ("100.64.1.1", True),
        ("169.254.1.1", True),
    ],
)
def test_start_bridge_accepts_allowed_host(monkeypatch, host, allow_private_bind):
    bound_hosts = []

    def fake_server(*, device, host, port):
        bound_hosts.append(host)
        raise OSError("stop before binding")

    class FakeDesktopDevice:
        permission_hint = ""

        def __init__(self, **kwargs):
            pass

    monkeypatch.setattr(start_bridge, "DesktopDevice", FakeDesktopDevice)
    monkeypatch.setattr(start_bridge, "DesktopBridgeServer", fake_server)
    argv = ["--bridge-host", host] + (["--allow-private-bind"] if allow_private_bind else [])
    assert start_bridge.main(argv) == 1
    assert bound_hosts == [host]


@pytest.mark.parametrize(
    ("system", "expected"),
    [("darwin", "Screen Recording"), ("windows", "桌面应用"), ("linux", "图形桌面")],
)
def test_permission_hint_is_platform_specific(system, expected):
    device = DesktopDevice.__new__(DesktopDevice)
    device.system = system
    assert expected in device.permission_hint
    assert "重启终端" in device.permission_hint
