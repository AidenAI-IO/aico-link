import base64
import html
import json
import threading
import time
import urllib.error
import urllib.request

import pytest

from adbandroid.bridge.adb import ADBCommandError
from adbandroid.bridge.server import ADBBridgeServer


class FakeADBAndroidDevice:
    """In-memory stand-in for ADBAndroidDevice; records calls."""

    def __init__(self, *, healthy: bool = True, width: int = 1080, height: int = 1920):
        self.healthy = healthy
        self.width = width
        self.height = height
        self.calls: list[tuple] = []
        self.serial = "127.0.0.1:6555"
        self.last_input_text = ""
        self.window_text_override: str | None = None

    def check_device(self):
        self.calls.append(("check_device",))
        if not self.healthy:
            raise ADBCommandError("device offline")
        return {"serial": self.serial, "state": "device"}

    def screen_size(self):
        return (self.width, self.height)

    def screenshot_jpeg(self):
        self.calls.append(("screenshot_jpeg",))
        return b"fake-jpeg-bytes", 720, 1280

    def tap(self, x, y):
        self.calls.append(("tap", x, y))

    def swipe(self, x1, y1, x2, y2, duration_ms):
        self.calls.append(("swipe", x1, y1, x2, y2, duration_ms))

    def keyevent(self, keycode):
        self.calls.append(("keyevent", keycode))

    def input_text(self, text):
        self.calls.append(("input_text", text))
        self.last_input_text = text

    def dump_window_xml(self):
        self.calls.append(("dump_window_xml",))
        text = self.window_text_override
        if text is None:
            text = self.last_input_text
        text = html.escape(text, quote=True)
        return (
            '<?xml version="1.0" encoding="UTF-8"?>'
            '<hierarchy>'
            f'<node class="android.widget.EditText" text="{text}" focused="true" focusable="true" />'
            "</hierarchy>"
        )

    def start_settings(self):
        self.calls.append(("start_settings",))

    def expand_notifications(self):
        self.calls.append(("expand_notifications",))

    def expand_settings(self):
        self.calls.append(("expand_settings",))

    def collapse_statusbar(self):
        self.calls.append(("collapse_statusbar",))

    def reset_home(self):
        self.calls.append(("reset_home",))

    # Launcher lookup, mirroring ADBAndroidDevice.launchable_activities().
    launchable = {
        "com.android.chrome": "com.android.chrome/com.google.android.apps.chrome.Main",
        "com.google.android.apps.maps": "com.google.android.apps.maps/com.google.android.maps.MapsActivity",
        "com.android.settings": "com.android.settings/.Settings",
    }

    def launchable_activities(self):
        self.calls.append(("launchable_activities",))
        return dict(self.launchable)

    def launch_app(self, package):
        component = self.launchable.get(package)
        if not component:
            raise ADBCommandError(f"no launcher activity for package {package!r}")
        self.calls.append(("launch_app", package))
        return component


@pytest.fixture()
def bridge():
    device = FakeADBAndroidDevice()
    server = ADBBridgeServer(device, host="127.0.0.1", port=0, action_settle_sec=0)
    base_url = server.start()
    try:
        yield server, device, base_url
    finally:
        server.stop()


def _request(base_url, path, *, method="GET", payload=None, task_id=None):
    headers = {"Content-Type": "application/json"}
    if task_id:
        headers["benchmark-task-id"] = task_id
    data = json.dumps(payload or {}).encode("utf-8") if method == "POST" else None
    req = urllib.request.Request(f"{base_url}{path}", data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def test_health_reports_ok(bridge):
    _, _, base_url = bridge
    status, body = _request(base_url, "/health")
    assert status == 200
    assert body["ok"] is True
    assert body["data"]["bridge_type"] == "adb_android"
    assert body["data"]["platform"] == "android"
    assert body["data"]["concurrent"] == 1


def test_health_reports_503_when_device_offline():
    device = FakeADBAndroidDevice(healthy=False)
    server = ADBBridgeServer(device, host="127.0.0.1", port=0, action_settle_sec=0)
    base_url = server.start()
    try:
        status, body = _request(base_url, "/health")
    finally:
        server.stop()
    assert status == 503
    assert body["ok"] is False
    assert body["error"]["code"] == "device_unavailable"


def test_concurrent_returns_data_concurrent(bridge):
    _, _, base_url = bridge
    status, body = _request(base_url, "/api/concurrent")
    assert status == 200
    assert body["data"]["concurrent"] == 1
    assert body["data"]["env_count"] == 1


def test_setup_with_empty_task_id_creates_episode(bridge):
    server, device, base_url = bridge
    status, body = _request(base_url, "/api/setup", method="POST")
    assert status == 200
    assert body["ok"] is True
    assert body["data"]["episode_id"].startswith("reset-")
    assert ("reset_home",) in device.calls
    assert server.state.active_task_id == ""


def test_setup_with_task_id_takes_ownership_and_is_idempotent(bridge):
    server, device, base_url = bridge
    status, body = _request(base_url, "/api/setup", method="POST", task_id="suite:task-1")
    assert status == 200
    assert body["data"]["episode_id"] == "suite:task-1"
    assert server.state.active_task_id == "suite:task-1"
    status, body = _request(base_url, "/health")
    assert status == 200
    assert body["data"]["active_task_id"] == "suite:task-1"
    assert body["data"]["active_task_lease_state"] == "active"

    status, _ = _request(base_url, "/api/setup", method="POST", task_id="suite:task-1")
    assert status == 200
    assert device.calls.count(("reset_home",)) == 2


def test_setup_token_deduplicates_concurrent_and_completed_requests():
    class BlockingResetDevice(FakeADBAndroidDevice):
        def __init__(self):
            super().__init__()
            self.reset_entered = threading.Event()
            self.allow_reset = threading.Event()

        def reset_home(self):
            self.calls.append(("reset_home",))
            self.reset_entered.set()
            self.allow_reset.wait(timeout=5)

    device = BlockingResetDevice()
    server = ADBBridgeServer(device, host="127.0.0.1", port=0, action_settle_sec=0)
    base_url = server.start()
    responses = []

    def setup():
        responses.append(
            _request(
                base_url,
                "/api/setup",
                method="POST",
                task_id="suite:task-token",
                payload={"setup_token": "setup-token-1"},
            )
        )

    first = threading.Thread(target=setup)
    second = threading.Thread(target=setup)
    try:
        first.start()
        assert device.reset_entered.wait(timeout=2)
        second.start()
        time.sleep(0.1)
        assert device.calls.count(("reset_home",)) == 1
        device.allow_reset.set()
        first.join(timeout=5)
        second.join(timeout=5)
        assert not first.is_alive()
        assert not second.is_alive()
        assert [status for status, _ in responses] == [200, 200]
        assert device.calls.count(("reset_home",)) == 1

        status, _ = _request(
            base_url,
            "/api/setup",
            method="POST",
            task_id="suite:task-token",
            payload={"setup_token": "setup-token-1"},
        )
        assert status == 200
        assert device.calls.count(("reset_home",)) == 1

        status, _ = _request(
            base_url,
            "/api/setup",
            method="POST",
            task_id="suite:task-token",
            payload={"setup_token": "setup-token-2"},
        )
        assert status == 200
        assert device.calls.count(("reset_home",)) == 2

        status, _ = _request(
            base_url,
            "/api/release",
            method="POST",
            task_id="suite:task-token",
        )
        assert status == 200
        status, _ = _request(
            base_url,
            "/api/setup",
            method="POST",
            task_id="suite:task-token",
            payload={"setup_token": "setup-token-2"},
        )
        assert status == 200
        assert device.calls.count(("reset_home",)) == 3
    finally:
        device.allow_reset.set()
        server.stop()


def test_setup_token_cache_is_scoped_to_bridge_process():
    device = FakeADBAndroidDevice()
    payload = {"setup_token": "setup-token-restart"}

    for _ in range(2):
        server = ADBBridgeServer(device, host="127.0.0.1", port=0, action_settle_sec=0)
        base_url = server.start()
        try:
            status, _ = _request(
                base_url,
                "/api/setup",
                method="POST",
                task_id="suite:task-restart",
                payload=payload,
            )
            assert status == 200
        finally:
            server.stop()

    assert device.calls.count(("reset_home",)) == 2


def test_health_reports_expired_task_lease_not_active(bridge):
    server, _, base_url = bridge
    status, _ = _request(base_url, "/api/setup", method="POST", task_id="suite:task-1")
    assert status == 200
    with server.state.lock:
        server.state.active_task_expires_at = time.monotonic() - 1

    status, body = _request(base_url, "/health")

    assert status == 200
    assert body["data"]["active_task_id"] == "suite:task-1"
    assert body["data"]["active_task_lease_state"] == "expired"


def test_anonymous_setup_preserves_active_task_lease_expiry(bridge):
    server, _, base_url = bridge
    status, _ = _request(base_url, "/api/setup", method="POST", task_id="suite:task-1")
    assert status == 200
    expires_at = time.monotonic() + 5
    with server.state.lock:
        server.state.active_task_expires_at = expires_at

    status, body = _request(base_url, "/api/setup", method="POST")

    assert status == 200
    assert body["data"]["episode_id"].startswith("reset-")
    with server.state.lock:
        assert server.state.active_task_id == "suite:task-1"
        assert server.state.active_task_expires_at == expires_at


def test_setup_with_different_task_id_returns_429(bridge):
    _, _, base_url = bridge
    status, _ = _request(base_url, "/api/setup", method="POST", task_id="suite:task-1")
    assert status == 200
    status, body = _request(base_url, "/api/setup", method="POST", task_id="suite:task-2")
    assert status == 429
    assert body["error"]["code"] == "no_bridge_env_available"


def test_release_semantics(bridge):
    server, _, base_url = bridge
    _request(base_url, "/api/setup", method="POST", task_id="suite:task-1")

    # Mismatched release keeps ownership.
    status, body = _request(base_url, "/api/release", method="POST", task_id="suite:other")
    assert status == 200
    assert body["data"]["released"] is False
    assert server.state.active_task_id == "suite:task-1"

    # Empty release keeps ownership.
    status, body = _request(base_url, "/api/release", method="POST")
    assert body["data"]["released"] is False

    # Owner release frees the device for the next task.
    status, body = _request(base_url, "/api/release", method="POST", task_id="suite:task-1")
    assert body["data"]["released"] is True
    status, _ = _request(base_url, "/api/setup", method="POST", task_id="suite:task-2")
    assert status == 200


def test_tool_call_requires_active_episode(bridge):
    _, _, base_url = bridge
    status, body = _request(base_url, "/api/tools/touch_gesture", method="POST", payload={"input": {"type": "tap", "point": {"x": 500, "y": 500}}})
    assert status == 409
    assert body["error"] == "no_active_episode"


def test_tool_call_with_empty_task_id_uses_single_state(bridge):
    # WebUI serial mode: setup carries a per-task route id while daemon tool
    # calls carry no benchmark-task-id header at all. Both must hit the state.
    _, device, base_url = bridge
    _request(base_url, "/api/setup", method="POST", task_id="suite:task-1")
    status, body = _request(base_url, "/api/tools/touch_gesture", method="POST", payload={"input": {"type": "tap", "point": {"x": 500, "y": 500}}})
    assert status == 200
    assert body["is_error"] is False
    assert ("tap", 540, 960) in device.calls


def test_provider_screenshot_returns_frame_metadata(bridge):
    _, device, base_url = bridge
    status, body = _request(
        base_url,
        "/api/providers/screenshot",
        method="POST",
        payload={"format": "jpeg", "quality": 80},
    )
    assert status == 200
    assert body["ok"] is True
    data = body["data"]
    assert data["meta"]["width"] == 720
    assert data["meta"]["height"] == 1280
    assert data["meta"]["pixel_format"] == "jpeg"
    assert data["capture_info"]["capture_backend"] == "adb"
    assert base64.b64decode(data["image"]) == b"fake-jpeg-bytes"
    assert ("screenshot_jpeg",) in device.calls


def test_mnk_provider_executes_click_and_drag(bridge):
    _, device, base_url = bridge
    status, _ = _request(base_url, "/api/setup", method="POST", task_id="suite:mnk")
    assert status == 200

    status, body = _request(
        base_url,
        "/api/providers/mnk",
        method="POST",
        task_id="suite:mnk",
        payload={
            "operation": "click",
            "click": {"x": 500, "y": 250, "button": "left", "hold_ms": 0},
        },
    )
    assert status == 200 and body == {"success": True}
    assert ("tap", 540, 480) in device.calls

    status, body = _request(
        base_url,
        "/api/providers/mnk",
        method="POST",
        task_id="suite:mnk",
        payload={
            "operation": "drag",
            "drag": {"path": [[100, 200], [900, 800]], "button": "left"},
        },
    )
    assert status == 200 and body == {"success": True}
    assert ("swipe", 108, 384, 972, 1536, 700) in device.calls


def test_mnk_provider_skips_the_post_action_screenshot(bridge):
    """MNK returns a flat body, so a captured frame would only be dropped."""
    _, device, base_url = bridge
    assert _request(base_url, "/api/setup", method="POST", task_id="suite:mnk-noshot")[0] == 200
    device.calls.clear()

    status, _ = _request(
        base_url,
        "/api/providers/mnk",
        method="POST",
        task_id="suite:mnk-noshot",
        payload={"operation": "click", "click": {"x": 500, "y": 250, "button": "left", "hold_ms": 0}},
    )

    assert status == 200
    assert ("tap", 540, 480) in device.calls
    assert ("screenshot_jpeg",) not in device.calls


def test_mnk_provider_returns_flat_error_for_adb_failure(bridge):
    _, device, base_url = bridge
    status, _ = _request(base_url, "/api/setup", method="POST", task_id="suite:mnk-error")
    assert status == 200

    def fail_tap(x, y):
        raise ADBCommandError(f"tap failed at {x},{y}")

    device.tap = fail_tap
    status, body = _request(
        base_url,
        "/api/providers/mnk",
        method="POST",
        task_id="suite:mnk-error",
        payload={
            "operation": "click",
            "click": {"x": 500, "y": 250, "button": "left", "hold_ms": 0},
        },
    )

    assert status == 500
    assert body == {"error": "tap failed at 540,480"}


def test_mnk_provider_enforces_task_access(bridge):
    _, _, base_url = bridge
    _request(base_url, "/api/setup", method="POST", task_id="suite:mnk")
    status, body = _request(
        base_url,
        "/api/providers/mnk",
        method="POST",
        task_id="suite:other",
        payload={"operation": "move", "move": {"x": 500, "y": 500}},
    )
    assert status == 429
    assert body["error"]["code"] == "no_bridge_env_available"


def test_tool_call_with_mismatched_task_id_returns_429(bridge):
    _, _, base_url = bridge
    _request(base_url, "/api/setup", method="POST", task_id="suite:task-1")
    status, body = _request(
        base_url, "/api/tools/touch_gesture", method="POST", payload={"input": {"type": "tap", "point": {"x": 500, "y": 500}}}, task_id="suite:other"
    )
    assert status == 429
    assert body["error"] == "no_bridge_env_available"


def test_provider_screenshot_works_without_setup(bridge):
    _, _, base_url = bridge
    status, body = _request(
        base_url,
        "/api/providers/screenshot",
        method="POST",
        payload={"format": "jpeg", "quality": 80},
    )
    assert status == 200
    assert body["ok"] is True
    assert base64.b64decode(body["data"]["image"]) == b"fake-jpeg-bytes"


def test_provider_screenshot_rejects_conflicting_task_id(bridge):
    _, _, base_url = bridge
    _request(base_url, "/api/setup", method="POST", task_id="suite:task-1")
    status, body = _request(
        base_url,
        "/api/providers/screenshot",
        method="POST",
        payload={"format": "jpeg", "quality": 80},
        task_id="suite:other",
    )
    assert status == 429
    assert body["error"]["code"] == "no_bridge_env_available"


def test_tools_catalog_lists_expected_tools(bridge):
    _, _, base_url = bridge
    status, body = _request(base_url, "/api/tools")
    assert status == 200
    names = {tool["name"] for tool in body["tools"]}
    assert names == {
        "touch_gesture",
        "keyboard_text",
        "keyboard_tap",
        "enter_text",
        "mouse_move",
        "mouse_scroll",
        "open_app",
        "quick_action",
    }
    quick_action = next(tool for tool in body["tools"] if tool["name"] == "quick_action")
    assert "platform" not in quick_action["args_schema"]["properties"]
    assert quick_action["args_schema"]["anyOf"] == [
        {"required": ["action"]},
        {"required": ["list"], "properties": {"list": {"const": True}}},
    ]


def test_request_handler_applies_socket_timeout():
    # A stalled client (e.g. partial request body) must not pin its
    # connection thread forever: the handler class carries the configured
    # request timeout, which StreamRequestHandler applies to the socket.
    device = FakeADBAndroidDevice()
    server = ADBBridgeServer(device, host="127.0.0.1", port=0, request_timeout_sec=42)
    server.start()
    try:
        assert server._httpd.RequestHandlerClass.timeout == 42
    finally:
        server.stop()


def test_failed_setup_rolls_back_new_ownership(bridge):
    server, device, base_url = bridge
    device.healthy = False

    status, body = _request(base_url, "/api/setup", method="POST", task_id="suite:task-1")
    assert status == 500
    assert body["error"]["code"] == "adb_error"
    # Ownership must not leak: the device stays free for the next task.
    assert server.state.active_task_id == ""

    device.healthy = True
    status, _ = _request(base_url, "/api/setup", method="POST", task_id="suite:task-2")
    assert status == 200


def test_failed_idempotent_resetup_keeps_existing_ownership(bridge):
    server, device, base_url = bridge
    status, _ = _request(base_url, "/api/setup", method="POST", task_id="suite:task-1")
    assert status == 200

    # A transient adb failure during the owner's re-setup must NOT release
    # ownership, or another task could steal the device mid-run.
    device.healthy = False
    status, _ = _request(base_url, "/api/setup", method="POST", task_id="suite:task-1")
    assert status == 500
    assert server.state.active_task_id == "suite:task-1"

    status, body = _request(base_url, "/api/setup", method="POST", task_id="suite:task-2")
    assert status == 429
    assert body["error"]["code"] == "no_bridge_env_available"
