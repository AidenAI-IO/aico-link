import asyncio
import base64
import io
import json
import sys
import threading
import time
import types
import urllib.error
import urllib.request
from enum import Enum

import pytest
from PIL import Image

from mobilegym.bridge.episode import BridgeEpisodeState, BridgeTaskRouter
from mobilegym.bridge.actions import action_to_dict
from mobilegym.bridge.server import (
    BridgeServer,
    DEFAULT_BRIDGE_REQUEST_TIMEOUT_SEC,
    SCREENSHOT_PROVIDER_TIMEOUT_SEC,
)


PNG_BYTES = b"\x89PNG\r\n\x1a\nmobilegym-png"
JPEG_BYTES = b"\xff\xd8\xff\xe0mobilegym-jpeg"


class FakeMobileGymActionType(str, Enum):
    CLICK = "CLICK"
    DOUBLE_TAP = "DOUBLE_TAP"
    LONG_PRESS = "LONG_PRESS"
    SWIPE = "SWIPE"
    DRAG = "DRAG"
    TYPE = "TYPE"
    ENTER = "ENTER"
    BACK = "BACK"
    HOME = "HOME"
    WAIT = "WAIT"


class FakeMobileGymAction:
    def __init__(self, action_type, data):
        self.action_type = action_type
        self.data = data


def install_mobilegym_action_classes(monkeypatch):
    bench_env = types.ModuleType("bench_env")
    env = types.ModuleType("bench_env.env")
    base = types.ModuleType("bench_env.env.base")
    base.Action = FakeMobileGymAction
    base.ActionType = FakeMobileGymActionType
    bench_env.env = env
    env.base = base
    monkeypatch.setitem(sys.modules, "bench_env", bench_env)
    monkeypatch.setitem(sys.modules, "bench_env.env", env)
    monkeypatch.setitem(sys.modules, "bench_env.env.base", base)


class OwnerLoop:
    def __enter__(self):
        self.loop = asyncio.new_event_loop()
        self.ready = threading.Event()
        self.thread = threading.Thread(target=self._run, name="mobilegym-owner-loop", daemon=True)
        self.thread.start()
        assert self.ready.wait(timeout=2)
        return self

    def __exit__(self, *exc):
        self.loop.call_soon_threadsafe(self.loop.stop)
        self.thread.join(timeout=2)
        self.loop.close()

    def _run(self):
        asyncio.set_event_loop(self.loop)
        self.ready.set()
        self.loop.run_forever()


class FakeObservation:
    screenshot = b"fake-screenshot"
    width = 1000
    height = 2000
    mime_type = "image/png"


class FakeObservationWithoutMime:
    def __init__(self, screenshot):
        self.screenshot = screenshot
        self.width = 1000
        self.height = 2000


class FakeStepResult:
    def __init__(self, observation):
        self.observation = observation


class RecordingKeyboard:
    def __init__(self):
        self.typed = []
        self.pressed = []

    async def type(self, text):
        self.typed.append(text)

    async def press(self, chord):
        self.pressed.append(chord)


class FakeEnv:
    def __init__(self, owner_loop):
        self.owner_loop = owner_loop
        self.actions = []
        self.state = {"os": {}, "apps": {}}
        self.route = {"app": "launcher", "path": "/"}
        self.reset_calls = 0
        self.reset_app_ids = []
        self.calls = []
        self.loop_matches = []
        self.threads = []
        self.active_steps = 0
        self.max_active_steps = 0
        self.step_delay = 0
        self.step_started = threading.Event()
        self.observation = FakeObservation()

    def _record(self, name):
        self.calls.append(name)
        self.loop_matches.append(asyncio.get_running_loop() is self.owner_loop)
        self.threads.append(threading.current_thread().name)

    async def step(self, action):
        self._record("step")
        self.active_steps += 1
        self.max_active_steps = max(self.max_active_steps, self.active_steps)
        self.step_started.set()
        if self.step_delay:
            await asyncio.sleep(self.step_delay)
        self.actions.append(action)
        self.active_steps -= 1
        return FakeStepResult(self.observation)

    async def get_observation(self):
        self._record("get_observation")
        return self.observation

    async def get_state(self, **kwargs):
        self._record("get_state")
        return self.state

    async def get_route(self):
        self._record("get_route")
        return self.route

    async def reset(self, app_ids=None):
        self._record("reset")
        self.reset_calls += 1
        self.reset_app_ids.append(app_ids)
        return FakeObservation()


class RunningBridge:
    def __enter__(self):
        self.owner = OwnerLoop().__enter__()
        self.env = FakeEnv(self.owner.loop)
        self.state = BridgeEpisodeState(self.env, owner_loop=self.owner.loop)
        self.server = BridgeServer(self.state, host="127.0.0.1", port=0)
        self.server.start()
        self.base_url = self.server.base_url
        return self

    def __exit__(self, *exc):
        self.server.stop()
        OwnerLoop.__exit__(self.owner, *exc)


def request_json(base_url, method, path, payload=None, timeout=2):
    data = None
    headers = {}
    if method != "GET":
        data = json.dumps(payload or {}).encode("utf-8")
        headers["Content-Type"] = "application/json"
    req = urllib.request.Request(f"{base_url}{path}", data=data, headers=headers, method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def request_text(base_url, method, path, timeout=2):
    req = urllib.request.Request(f"{base_url}{path}", method=method)
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            return resp.status, resp.read().decode("utf-8")
    except urllib.error.HTTPError as exc:
        return exc.code, exc.read().decode("utf-8")


def start_episode(bridge, episode_id="ep1"):
    return request_json(
        bridge.base_url,
        "POST",
        "/episode/start",
        {"episode_id": episode_id},
    )


def expected_screenshot(payload=b"fake-screenshot", fmt="png"):
    return {
        "width": 1000,
        "height": 2000,
        "format": fmt,
        "size": len(payload),
        "data": base64.b64encode(payload).decode("ascii"),
    }


def test_bridge_base_url_uses_public_host_override():
    with OwnerLoop() as owner:
        env = FakeEnv(owner.loop)
        state = BridgeEpisodeState(env, owner_loop=owner.loop)
        server = BridgeServer(
            state,
            host="127.0.0.1",
            port=0,
            public_host="bridge-container",
        )
        try:
            base_url = server.start()
            assert base_url.startswith("http://bridge-container:")
        finally:
            server.stop()


def test_bridge_server_default_request_timeout_covers_slow_mobilegym_actions():
    assert DEFAULT_BRIDGE_REQUEST_TIMEOUT_SEC >= 180
    assert SCREENSHOT_PROVIDER_TIMEOUT_SEC == 30
    with OwnerLoop() as owner:
        env = FakeEnv(owner.loop)
        state = BridgeEpisodeState(env, owner_loop=owner.loop)
        server = BridgeServer(state, host="127.0.0.1", port=0)

    assert server.request_timeout_sec == DEFAULT_BRIDGE_REQUEST_TIMEOUT_SEC


def test_bridge_base_url_resolves_container_ip_when_bound_to_wildcard(monkeypatch):
    from mobilegym.bridge import server as server_module

    monkeypatch.setattr(server_module, "_get_container_ip", lambda: "10.20.30.40")
    with OwnerLoop() as owner:
        env = FakeEnv(owner.loop)
        state = BridgeEpisodeState(env, owner_loop=owner.loop)
        server = BridgeServer(
            state,
            host="0.0.0.0",
            port=0,
        )
        try:
            base_url = server.start()
            assert base_url.startswith("http://10.20.30.40:")
        finally:
            server.stop()


def test_bridge_base_url_falls_back_to_hostname_when_container_ip_unavailable(monkeypatch):
    from mobilegym.bridge import server as server_module

    monkeypatch.setattr(server_module, "_get_container_ip", lambda: None)
    monkeypatch.setattr(server_module.socket, "gethostname", lambda: "fallback-host")
    with OwnerLoop() as owner:
        env = FakeEnv(owner.loop)
        state = BridgeEpisodeState(env, owner_loop=owner.loop)
        server = BridgeServer(
            state,
            host="0.0.0.0",
            port=0,
        )
        try:
            base_url = server.start()
            assert base_url.startswith("http://fallback-host:")
        finally:
            server.stop()


def test_health_and_runner_endpoints_do_not_require_authentication():
    with RunningBridge() as bridge:
        status, body = request_json(bridge.base_url, "GET", "/health")
        assert status == 200
        assert body["data"]["status"] == "ok"
        assert body["data"]["platform"] == "android"
        assert body["data"]["concurrent"] == 1
        assert "/api/concurrent" in body["data"]["interfaces"]
        assert "/state" in body["data"]["interfaces"]
        assert "/route" in body["data"]["interfaces"]

        status, body = request_json(bridge.base_url, "GET", "/api/concurrent")
        assert status == 200
        assert body["data"]["bridge_type"] == "mobilegym"
        assert body["data"]["concurrent"] == 1
        assert body["data"]["env_count"] == 1

        status, _ = request_json(bridge.base_url, "POST", "/episode/start", {"episode_id": "ep1"})
        assert status == 200

        status, body = request_json(bridge.base_url, "POST", "/api/setup", {"episode_id": "reset-ep1"})
        assert status == 200
        assert body["data"] == {"episode_id": "reset-ep1", "reset": True}
        assert bridge.state.active_episode_id == "reset-ep1"
        assert bridge.env.reset_calls == 1
        assert bridge.env.reset_app_ids == [[]]

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/setup",
            {"episode_id": "reset-ep2", "app_ids": ["settings"]},
        )
        assert status == 200
        assert body["data"] == {"episode_id": "reset-ep2", "reset": True}
        assert bridge.env.reset_app_ids == [[], ["settings"]]

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/providers/screenshot",
            {"format": "jpeg", "quality": 80},
        )
        assert status == 200
        assert body["ok"] is True
        assert body["data"]["meta"]["pixel_format"] in {"jpeg", "png"}
        assert body["data"]["capture_info"]["capture_backend"] == "mobilegym"
        assert body["data"]["image"]

        status, body = request_json(bridge.base_url, "POST", "/state", {})
        assert status == 200
        assert body["data"] == bridge.env.state

        status, body = request_json(bridge.base_url, "POST", "/route", {})
        assert status == 200
        assert body["data"] == bridge.env.route

        status, _ = request_json(bridge.base_url, "POST", "/episode/end", {"episode_id": "reset-ep2"})
        assert status == 200

        assert bridge.env.loop_matches and all(bridge.env.loop_matches)
        assert bridge.env.threads and set(bridge.env.threads) == {"mobilegym-owner-loop"}


def test_setup_rejects_invalid_app_ids():
    with RunningBridge() as bridge:
        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/setup",
            {"app_ids": "settings"},
        )

        assert status == 400
        assert body["error"]["code"] == "bad_request"
        assert bridge.env.reset_calls == 0


def test_setup_treats_null_app_ids_as_empty():
    with RunningBridge() as bridge:
        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/setup",
            {"app_ids": None},
        )

        assert status == 200
        assert body["data"]["reset"] is True
        assert bridge.env.reset_app_ids == [[]]


def test_setup_foregrounds_only_explicit_app_id():
    class Page:
        def __init__(self):
            self.calls = []

        async def evaluate(self, script, app_id):
            self.calls.append(app_id)
            if app_id != "scroll_lab":
                raise RuntimeError(f"app is not installed: {app_id}")

    with RunningBridge() as bridge:
        bridge.env.page = Page()
        status, body = request_json(
            bridge.base_url, "POST", "/api/setup",
            {"app_ids": ["settings"], "foreground_app_id": "scroll_lab"},
        )
        assert status == 200
        assert body["ok"] is True
        assert bridge.env.reset_app_ids == [["settings"]]
        assert bridge.env.page.calls == ["scroll_lab"]


def test_setup_fails_when_foreground_app_cannot_open():
    with RunningBridge() as bridge:
        status, body = request_json(
            bridge.base_url, "POST", "/api/setup",
            {"foreground_app_id": "scroll_lab"},
        )
        assert status == 500
        assert body["ok"] is False
        assert "browser page" in body["error"]["message"]


def test_setup_token_cache_includes_foreground_app_id():
    class Page:
        def __init__(self):
            self.calls = []

        async def evaluate(self, script, app_id):
            self.calls.append(app_id)

    with RunningBridge() as bridge:
        bridge.env.page = Page()
        for app_id in ("scroll_lab", "settings"):
            status, body = request_json(
                bridge.base_url,
                "POST",
                "/api/setup",
                {"setup_token": "same-token", "foreground_app_id": app_id},
            )
            assert status == 200
            assert body["ok"] is True

        assert bridge.env.reset_calls == 2
        assert bridge.env.page.calls == ["scroll_lab", "settings"]


@pytest.mark.parametrize("value", ["", " scroll_lab ", 42, []])
def test_setup_rejects_invalid_foreground_app_id(value):
    with RunningBridge() as bridge:
        status, body = request_json(bridge.base_url, "POST", "/api/setup", {"foreground_app_id": value})
        assert status == 400
        assert body["error"]["code"] == "bad_request"
        assert bridge.env.reset_calls == 0


def test_setup_timeout_preserves_mobilegym_phase_diagnostic():
    with RunningBridge() as bridge:
        async def timeout_reset(app_ids=None):
            raise TimeoutError("phase=waitForData timeout after 40000ms")

        bridge.env.reset = timeout_reset

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/setup",
            {"app_ids": []},
        )

        assert status == 504
        assert body["error"]["code"] == "timeout"
        assert "phase=waitForData" in body["error"]["message"]


def test_submit_to_state_cancels_work_after_wait_timeout():
    with RunningBridge() as bridge:
        cancelled = threading.Event()

        async def wait_forever():
            try:
                await asyncio.Future()
            except asyncio.CancelledError:
                cancelled.set()
                raise

        with pytest.raises(TimeoutError, match="bridge request timed out"):
            bridge.server.submit_to_state(bridge.state, wait_forever(), timeout=0.01)

        assert cancelled.wait(timeout=1)


@pytest.mark.parametrize("failure", ["error", "timeout"])
def test_release_frees_task_route_when_episode_end_fails(failure):
    with OwnerLoop() as owner:
        state = BridgeEpisodeState(FakeEnv(owner.loop), owner_loop=owner.loop)
        router = BridgeTaskRouter([state])
        server = BridgeServer(router, host="127.0.0.1", port=0, request_timeout_sec=0.05)
        server.start()
        cancelled = threading.Event()

        def post(path, task_id, payload):
            request = urllib.request.Request(
                f"{server.base_url}{path}",
                data=json.dumps(payload).encode("utf-8"),
                headers={"Content-Type": "application/json", "benchmark-task-id": task_id},
                method="POST",
            )
            with urllib.request.urlopen(request, timeout=2) as response:
                return response.status, json.loads(response.read())

        try:
            status, _ = post("/api/setup", "task.alpha", {"episode_id": "ep-alpha", "setup_token": "token"})
            assert status == 200

            async def failed_end(episode_id):
                if failure == "error":
                    raise RuntimeError("episode cleanup failed")
                try:
                    await asyncio.Event().wait()
                except asyncio.CancelledError:
                    cancelled.set()
                    raise

            state.end_episode = failed_end
            status, result = post("/api/release", "task.alpha", {})
            assert status == 200
            assert result["data"]["released"] is True
            assert router.task_map() == {}
            assert server.setup_tokens.clear_completed_for_task("task.alpha") == 0
            if failure == "timeout":
                assert cancelled.wait(timeout=1)

            status, _ = post("/api/setup", "task.beta", {"episode_id": "ep-beta"})
            assert status == 200
        finally:
            server.stop()


def test_api_screen_is_removed():
    with RunningBridge() as bridge:
        status, _ = request_text(bridge.base_url, "GET", "/screen")
        assert status == 404
        status, body = request_json(bridge.base_url, "GET", "/api/screen")
        assert status == 404
        assert body["ok"] is False


def test_provider_screenshot_converts_png_observation_to_jpeg():
    png_buf = io.BytesIO()
    Image.new("RGB", (2, 1), "red").save(png_buf, format="PNG")
    png_bytes = png_buf.getvalue()

    with OwnerLoop() as owner:
        env = FakeEnv(owner.loop)
        env.observation = types.SimpleNamespace(
            screenshot=png_bytes, width=2, height=1, mime_type="image/png"
        )
        state = BridgeEpisodeState(env, owner_loop=owner.loop)
        server = BridgeServer(state, host="127.0.0.1", port=0)
        try:
            server.start()
            status, body = request_json(
                server.base_url,
                "POST",
                "/api/providers/screenshot",
                {"format": "jpeg", "quality": 80},
            )
        finally:
            server.stop()

    assert status == 200
    meta = body["data"]["meta"]
    image = base64.b64decode(body["data"]["image"])
    assert meta["pixel_format"] == "jpeg"
    assert image.startswith(b"\xff\xd8")
    assert meta["bytes"] == len(image)


def test_provider_screenshot_routes_by_task_id():
    with OwnerLoop() as owner:
        envs = [FakeEnv(owner.loop), FakeEnv(owner.loop)]
        envs[0].observation = types.SimpleNamespace(
            screenshot=b"alpha-frame", width=111, height=222, mime_type="image/jpeg"
        )
        envs[1].observation = types.SimpleNamespace(
            screenshot=b"beta-frame", width=333, height=444, mime_type="image/jpeg"
        )
        states = [BridgeEpisodeState(env, owner_loop=owner.loop) for env in envs]
        states[0].active_episode_id = "ep-alpha"
        states[1].active_episode_id = "ep-beta"
        server = BridgeServer(BridgeTaskRouter(states), host="127.0.0.1", port=0)
        try:
            server.start()
            status, body = request_json(server.base_url, "GET", "/api/concurrent")
            assert status == 200
            assert body["data"]["concurrent"] == 2
            assert body["data"]["env_count"] == 2
            assert body["data"]["active_routes"] == {}

            server.router.state_for_task_id("task.alpha")
            server.router.state_for_task_id("task.beta")
            status, body = request_json(server.base_url, "GET", "/api/concurrent")
            assert status == 200
            assert body["data"]["active_routes"] == {"task.alpha": 0, "task.beta": 1}

            status, body = request_json(
                server.base_url,
                "POST",
                "/api/providers/screenshot?benchmark-task-id=task.alpha",
                {"format": "jpeg"},
            )
            assert status == 200
            assert body["data"]["meta"]["width"] == 111
            assert base64.b64decode(body["data"]["image"]) == b"alpha-frame"

            status, body = request_json(
                server.base_url,
                "POST",
                "/api/providers/screenshot?benchmark-task-id=task.beta",
                {"format": "jpeg"},
            )
            assert status == 200
            assert body["data"]["meta"]["width"] == 333
            assert base64.b64decode(body["data"]["image"]) == b"beta-frame"
        finally:
            server.stop()


@pytest.mark.parametrize("operation", ["swipe", "drag"])
@pytest.mark.parametrize("duration_ms", [None, 0, 1, 120, 2400, 10_000])
def test_provider_mnk_preserves_motion_duration_to_environment(operation, duration_ms):
    with RunningBridge() as bridge:
        status, _ = start_episode(bridge)
        assert status == 200
        params = {"path": [[500, 800], [500, 400]], "button": "left"}
        if duration_ms is not None:
            params["duration_ms"] = duration_ms

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/providers/mnk",
            {"operation": operation, operation: params},
        )

        assert status == 200
        assert body == {"success": True}
        assert len(bridge.env.actions) == 1
        # Legacy callers without a positive duration keep the backend default.
        expected_duration = duration_ms or (160 if operation == "swipe" else 700)
        assert action_to_dict(bridge.env.actions[0]) == {
            "action_type": operation.upper(),
            "data": {
                "point1": [500.0, 800.0],
                "point2": [500.0, 400.0],
                "duration": expected_duration,
            },
        }


def test_tools_api_touch_gestures_use_active_reset_episode_and_normalized_coordinates():
    with RunningBridge() as bridge:
        status, body = request_json(bridge.base_url, "POST", "/api/setup", {"episode_id": "reset-ep1"})
        assert status == 200
        assert body["data"] == {"episode_id": "reset-ep1", "reset": True}

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/touch_gesture",
            {"input": {"type": "tap", "point": {"x": 500, "y": 800}}},
        )
        assert status == 200
        assert body["is_error"] is False
        assert action_to_dict(bridge.env.actions[-1]) == {
            "action_type": "CLICK",
            "data": {"point": [500.0, 800.0]},
        }

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/touch_gesture",
            {"input": {"type": "tap", "x": "135", "y": "705"}},
        )
        assert status == 200
        assert body["is_error"] is False
        assert action_to_dict(bridge.env.actions[-1]) == {
            "action_type": "CLICK",
            "data": {"point": [135.0, 705.0]},
        }

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/touch_gesture",
            {"input": {"type": "swipe", "start": {"x": 600, "y": 700}, "direction": "up", "speed": 1000, "duration_ms": 200}},
        )
        assert status == 200
        assert body["is_error"] is False
        assert action_to_dict(bridge.env.actions[-1]) == {
            "action_type": "SWIPE",
            "data": {"point1": [600.0, 700.0], "point2": [600.0, 500.0], "duration": 200.0},
        }

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/touch_gesture",
            {"input": {"type": "swipe", "start": {"x": 500, "y": 500}, "direction": "diagonal"}},
        )
        assert status == 200
        assert body["is_error"] is True
        assert "unsupported swipe direction" in body["output"]


def test_tools_api_actions_are_logged_on_episode_end():
    with RunningBridge() as bridge:
        status, body = request_json(bridge.base_url, "POST", "/api/setup", {"episode_id": "reset-ep1"})
        assert status == 200
        assert body["data"] == {"episode_id": "reset-ep1", "reset": True}

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/touch_gesture",
            {"input": {"type": "tap", "point": {"x": 321, "y": 654}}},
        )
        assert status == 200
        assert body["is_error"] is False

        status, body = request_json(bridge.base_url, "POST", "/episode/end", {"episode_id": "reset-ep1"})
        assert status == 200
        action_log = body["data"]["action_log"]
        assert len(action_log) == 1
        assert action_log[0]["tool_name"] == "touch_gesture"
        assert action_log[0]["tool_input"] == {
            "type": "tap",
            "point": {"x": 321, "y": 654},
        }


def test_tools_api_pointer_and_quick_action_inputs_map_to_mobilegym_actions():
    with RunningBridge() as bridge:
        status, body = request_json(bridge.base_url, "POST", "/api/setup", {"episode_id": "reset-ep1"})
        assert status == 200
        assert body["data"] == {"episode_id": "reset-ep1", "reset": True}

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/touch_gesture",
            {"input": {"type": "tap", "point": {"x": 321, "y": 654}}},
        )
        assert status == 200
        assert body["is_error"] is False
        assert action_to_dict(bridge.env.actions[-1]) == {
            "action_type": "CLICK",
            "data": {"point": [321.0, 654.0]},
        }

        before = len(bridge.env.actions)
        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/mouse_move",
            {"input": {"x": 111, "y": 222}},
        )
        assert status == 200
        assert body["is_error"] is False
        assert len(bridge.env.actions) == before

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/mouse_move",
            {"input": {"y": 222}},
        )
        assert status == 200
        assert body["is_error"] is True
        assert "point is required" in body["output"]
        assert len(bridge.env.actions) == before

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/mouse_scroll",
            {"input": {"delta": -3}},
        )
        assert status == 200
        assert body["is_error"] is False
        assert action_to_dict(bridge.env.actions[-1])["action_type"] == "SWIPE"

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/quick_action",
            {"input": {"action": "back"}},
        )
        assert status == 200
        assert body["is_error"] is False
        assert action_to_dict(bridge.env.actions[-1]) == {
            "action_type": "SWIPE",
            "data": {"point1": [1.0, 500.0], "point2": [750.0, 500.0], "duration": 700.0},
        }

        before = len(bridge.env.actions)
        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/quick_action",
            {"input": {"action": "list"}},
        )
        assert status == 200
        assert body["is_error"] is False
        output = json.loads(body["output"])
        assert output["ok"] is True
        assert output["platform"] == "android"
        assert len(bridge.env.actions) == before


@pytest.mark.parametrize("home_keys", [["meta"], ["meta", "h"], ["cmd", "h"], ["super", "h"], ["win", "h"]])
def test_tools_api_keyboard_inputs_match_agent_proxy_contract(home_keys):
    with RunningBridge() as bridge:
        bridge.env.page = types.SimpleNamespace(keyboard=RecordingKeyboard())
        status, body = request_json(bridge.base_url, "POST", "/api/setup", {"episode_id": "reset-ep1"})
        assert status == 200
        assert body["data"] == {"episode_id": "reset-ep1", "reset": True}

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/keyboard_text",
            {"raw_input": "plain text"},
        )
        assert status == 200
        assert body["is_error"] is False
        assert bridge.env.page.keyboard.typed == ["plain text"]
        assert not bridge.env.actions  # No value-setting TYPE shortcut.

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/keyboard_text",
            {"input": {"text": "中文"}},
        )
        assert status == 200
        assert body["is_error"] is True
        assert "US-keyboard ASCII" in body["output"]

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/keyboard_tap",
            {"input": {"keys": home_keys, "hold_ms": 120}},
        )
        assert status == 200
        assert body["is_error"] is False
        assert action_to_dict(bridge.env.actions[-1]) == {"action_type": "HOME", "data": {}}
        assert bridge.env.page.keyboard.pressed == []

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/keyboard_tap",
            {"input": {"keys": ["ctrl", "c"]}},
        )
        assert status == 200
        assert body["is_error"] is False
        assert bridge.env.page.keyboard.pressed == ["Control+c"]


def test_tools_api_mobilegym_text_entry_tools_do_not_depend_on_hid_devices():
    with RunningBridge() as bridge:
        status, body = request_json(bridge.base_url, "POST", "/api/setup", {"episode_id": "reset-ep1"})
        assert status == 200
        assert body["data"] == {"episode_id": "reset-ep1", "reset": True}

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/enter_text",
            {
                "input": {
                    "text": "微信读书",
                    "focus": {"x": 500, "y": 120},
                }
            },
        )
        assert status == 200
        assert body["is_error"] is False
        output = json.loads(body["output"])
        assert output == {"ok": True}
        assert "hidg" not in body["output"]
        assert action_to_dict(bridge.env.actions[-1]) == {
            "action_type": "TYPE",
            "data": {"value": "微信读书", "point": [500.0, 120.0]},
        }

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/api/tools/enter_text",
            {
                "input": {
                    "text": "Camera note",
                    "focus": {"x": 400, "y": 700},
                }
            },
        )
        assert status == 200
        assert body["is_error"] is False
        output = json.loads(body["output"])
        assert output == {"ok": True}
        assert "hidg" not in body["output"]
        assert action_to_dict(bridge.env.actions[-1]) == {
            "action_type": "TYPE",
            "data": {"value": "Camera note", "point": [400.0, 700.0]},
        }


def test_device_endpoints_require_active_episode_without_authentication():
    with RunningBridge() as bridge:
        assert start_episode(bridge)[0] == 200

        status, _ = request_json(
            bridge.base_url,
            "POST",
            "/tap",
            {"episode_id": "old", "x": 500, "y": 250},
        )
        assert status == 409
        assert bridge.env.actions == []

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/screenshot",
            {"episode_id": "ep1"},
        )
        assert status == 200
        assert body == expected_screenshot()

        action_requests = [
            ("/tap", {"episode_id": "ep1", "x": 100, "y": 200}, "CLICK"),
            ("/tap", {"episode_id": "ep1", "x": 100, "y": 200, "count": 2}, "DOUBLE_TAP"),
            (
                "/tap",
                {"episode_id": "ep1", "x": 100, "y": 200, "kind": "long_press", "duration_ms": 777},
                "LONG_PRESS",
            ),
            (
                "/swipe",
                {
                    "episode_id": "ep1",
                    "start_x": 100,
                    "start_y": 200,
                    "end_x": 800,
                    "end_y": 900,
                    "duration_ms": 300,
                },
                "SWIPE",
            ),
            (
                "/drag",
                {"episode_id": "ep1", "start_x": 200, "start_y": 300, "end_x": 700, "end_y": 600},
                "DRAG",
            ),
            ("/type_text", {"episode_id": "ep1", "text": "hello"}, "TYPE"),
            ("/key", {"episode_id": "ep1", "key": "enter"}, "ENTER"),
            ("/back", {"episode_id": "ep1"}, "BACK"),
            ("/home", {"episode_id": "ep1"}, "HOME"),
            ("/wait", {"episode_id": "ep1", "duration_ms": 10}, "WAIT"),
        ]
        for index, (path, payload, _) in enumerate(action_requests, start=1):
            status, body = request_json(bridge.base_url, "POST", path, payload)
            assert status == 200
            assert body == {
                "ok": True,
                "message": "ok",
                "action_id": f"ep1:{index:04d}",
                "screenshot": expected_screenshot(),
            }

        assert len(bridge.env.actions) == len(action_requests)
        assert [action_to_dict(action)["action_type"] for action in bridge.env.actions] == [
            expected_type for _, _, expected_type in action_requests
        ]
        assert all(bridge.env.loop_matches)
        assert set(bridge.env.threads) == {"mobilegym-owner-loop"}


def test_key_endpoint_supports_mobilegym_action_type_without_key_and_rejects_unsupported_before_step(monkeypatch):
    install_mobilegym_action_classes(monkeypatch)

    with RunningBridge() as bridge:
        assert start_episode(bridge)[0] == 200

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/key",
            {"episode_id": "ep1", "key": "enter"},
        )
        assert status == 200
        assert body["ok"] is True
        assert len(bridge.env.actions) == 1
        assert action_to_dict(bridge.env.actions[0]) == {"action_type": "ENTER", "data": {}}

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/key",
            {"episode_id": "ep1", "key": "volume_up"},
        )
        assert status == 400
        assert body["ok"] is False
        assert body["error"]["code"] == "bad_request"
        assert body["error"]["message"] == "unsupported key: volume_up"
        assert len(bridge.env.actions) == 1


@pytest.mark.parametrize(("payload", "fmt"), [(PNG_BYTES, "png"), (JPEG_BYTES, "jpeg")])
def test_screenshot_infers_format_from_bytes_when_mime_type_is_absent(payload, fmt):
    with RunningBridge() as bridge:
        bridge.env.observation = FakeObservationWithoutMime(payload)
        assert start_episode(bridge)[0] == 200

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/screenshot",
            {"episode_id": "ep1"},
        )

        assert status == 200
        assert body == expected_screenshot(payload, fmt)


def test_episode_end_returns_action_log_entries_with_screenshots():
    with RunningBridge() as bridge:
        assert start_episode(bridge)[0] == 200

        status, action_body = request_json(
            bridge.base_url,
            "POST",
            "/tap",
            {"episode_id": "ep1", "x": 100, "y": 200},
        )
        assert status == 200

        status, end_body = request_json(
            bridge.base_url,
            "POST",
            "/episode/end",
            {"episode_id": "ep1"},
        )

        assert status == 200
        assert end_body["data"]["action_log"][0]["screenshot"] == action_body["screenshot"]


def test_episode_end_waits_for_in_flight_env_work_before_returning():
    with RunningBridge() as bridge:
        bridge.env.step_delay = 0.2
        assert start_episode(bridge)[0] == 200
        action_done = threading.Event()
        end_done = threading.Event()

        def call_wait():
            request_json(
                bridge.base_url,
                "POST",
                "/wait",
                {"episode_id": "ep1", "duration_ms": 1},
                timeout=3,
            )
            action_done.set()

        def call_end():
            status, _ = request_json(
                bridge.base_url,
                "POST",
                "/episode/end",
                {"episode_id": "ep1"},
                timeout=3,
            )
            assert status == 200
            end_done.set()

        action_thread = threading.Thread(target=call_wait, daemon=True)
        action_thread.start()
        assert bridge.env.step_started.wait(timeout=1)

        end_thread = threading.Thread(target=call_end, daemon=True)
        end_thread.start()
        time.sleep(0.05)
        assert not end_done.is_set()

        action_thread.join(timeout=3)
        end_thread.join(timeout=3)
        assert action_done.is_set()
        assert end_done.is_set()
        assert len(bridge.env.actions) == 1


def test_env_work_is_serialized_and_bridge_serves_screenshot_while_chat_is_blocked():
    with RunningBridge() as bridge:
        bridge.env.step_delay = 0.05
        assert start_episode(bridge)[0] == 200

        def call_wait():
            status, _ = request_json(
                bridge.base_url,
                "POST",
                "/wait",
                {"episode_id": "ep1", "duration_ms": 1},
                timeout=3,
            )
            assert status == 200

        first = threading.Thread(target=call_wait, daemon=True)
        second = threading.Thread(target=call_wait, daemon=True)
        first.start()
        second.start()
        first.join(timeout=3)
        second.join(timeout=3)
        assert bridge.env.max_active_steps == 1

        chat_blocked = threading.Event()
        release_chat = threading.Event()

        def fake_blocked_chat():
            chat_blocked.set()
            release_chat.wait(timeout=2)

        chat_thread = threading.Thread(target=fake_blocked_chat, daemon=True)
        chat_thread.start()
        assert chat_blocked.wait(timeout=1)

        status, body = request_json(
            bridge.base_url,
            "POST",
            "/screenshot",
            {"episode_id": "ep1"},
            timeout=2,
        )
        assert status == 200
        assert body == expected_screenshot()

        release_chat.set()
        chat_thread.join(timeout=1)
        assert not chat_thread.is_alive()


def test_persistent_drag_cleans_up_on_reset_and_release():
    with RunningBridge() as bridge:
        operations = []

        async def evaluate(script, args):
            operations.append(args["operation"])

        bridge.env.page = types.SimpleNamespace(evaluate=evaluate)
        request_json(bridge.base_url, "POST", "/api/setup", {"episode_id": "first"})
        def drag(operation):
            return request_json(bridge.base_url, "POST", "/api/providers/mnk",
                {"operation": operation, operation: {"x": 148, "y": 114}})
        assert drag("drag_start")[0] == 200
        # Starting twice must fail, not silently lift the contact.
        assert drag("drag_start")[0] == 500
        assert bridge.state.input.contact_active
        request_json(bridge.base_url, "POST", "/api/setup", {"episode_id": "second"})
        assert operations == ["drag_start", "cancel"]
        assert drag("drag_release")[0] == 500
        assert drag("drag_start")[0] == 200
        assert drag("drag_release")[0] == 200
        assert operations[-2:] == ["drag_start", "drag_release"]
        assert not bridge.state.input.contact_active
