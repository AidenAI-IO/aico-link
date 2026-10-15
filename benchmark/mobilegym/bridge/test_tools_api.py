"""Tests for unified /api/tools endpoint."""

import json
import time
from types import SimpleNamespace
from threading import Event, Thread
from urllib.request import Request, urlopen

import pytest

from .episode import BridgeEpisodeState, BridgeTaskRouter
from .actions import action_to_dict
from .server import BridgeServer


@pytest.fixture
def mock_env():
    """Mock MobileGym environment."""
    return mock_env_factory()


def mock_env_factory():
    class MockKeyboard:
        def __init__(self):
            self.typed = []
            self.pressed = []

        async def type(self, text):
            self.typed.append(text)

        async def press(self, keys):
            self.pressed.append(keys)

    class MockEnv:
        def __init__(self):
            self.page = SimpleNamespace(keyboard=MockKeyboard())
            self.last_action = None
            self.actions = []
            self.step_count = 0

        async def get_observation(self):
            # Return mock screenshot
            return MockObservation()

        async def step(self, action):
            self.last_action = action
            self.actions.append(action)
            self.step_count += 1
            return MockStepResult()

    class MockObservation:
        def __init__(self):
            self.screenshot_bytes = b"\xff\xd8\xff\xe0" + b"\x00" * 100  # Mock JPEG
            self.width = 1080
            self.height = 2400
            self.mime_type = "image/jpeg"

    class MockStepResult:
        def __init__(self):
            self.observation = MockObservation()

    return MockEnv()


@pytest.fixture
def bridge_server(mock_env):
    """Create and start bridge server."""
    import asyncio

    loop = asyncio.new_event_loop()
    thread = Thread(target=loop.run_forever, daemon=True)
    thread.start()

    state = BridgeEpisodeState(mock_env, loop)
    server = BridgeServer(state, host="127.0.0.1", port=0)
    base_url = server.start()

    yield server, base_url, state

    server.stop()
    loop.call_soon_threadsafe(loop.stop)
    thread.join(timeout=2)
    loop.close()


def test_get_tools_catalog(bridge_server):
    """Test GET /api/tools returns tool catalog."""
    server, base_url, state = bridge_server

    req = Request(f"{base_url}/api/tools", method="GET")
    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert "tools" in data
    tools = {tool["name"]: tool for tool in data["tools"]}
    assert set(tools) == {
        "touch_gesture",
        "keyboard_text",
        "keyboard_tap",
        "enter_text",
        "mouse_move",
        "mouse_scroll",
        "quick_action",
    }

    assert tools["touch_gesture"]["args_schema"]["additionalProperties"] is False
    touch_props = tools["touch_gesture"]["args_schema"]["properties"]
    assert touch_props["point"]["additionalProperties"] is False
    assert touch_props["point"]["required"] == ["x", "y"]
    assert "coord_space" not in touch_props
    assert touch_props["button"]["enum"] == ["left", "right", "middle"]
    assert touch_props["type"]["enum"] == ["tap", "double_tap", "long_press", "swipe", "drag", "drag_start", "drag_release"]
    assert touch_props["direction"]["enum"] == ["up", "down", "left", "right"]
    assert touch_props["speed"]["exclusiveMinimum"] == 0
    assert "hold_ms" in touch_props
    assert "pause_ms" in touch_props
    for removed in ("strength", "distance", "anchor"):
        assert removed not in touch_props
    assert "hold_before_ms" in touch_props
    assert "hold_after_ms" in touch_props
    assert "steps" in touch_props

    keyboard_tap_props = tools["keyboard_tap"]["args_schema"]["properties"]
    assert tools["keyboard_text"]["args_schema"]["additionalProperties"] is False
    assert tools["keyboard_tap"]["args_schema"]["additionalProperties"] is False
    assert "hold_ms" in keyboard_tap_props

    enter_text_props = tools["enter_text"]["args_schema"]["properties"]
    assert tools["enter_text"]["args_schema"]["additionalProperties"] is False
    assert "focus" in enter_text_props
    assert "send_after_commit" not in enter_text_props
    assert "segments" not in enter_text_props
    assert "max_attempts" not in enter_text_props
    assert "platform" not in enter_text_props
    assert enter_text_props["focus"]["additionalProperties"] is False
    assert "coord_space" not in enter_text_props["focus"]["properties"]

    assert tools["mouse_move"]["args_schema"]["additionalProperties"] is False
    assert tools["mouse_scroll"]["args_schema"]["additionalProperties"] is False
    assert "coord_space" not in tools["mouse_move"]["args_schema"]["properties"]
    assert tools["mouse_scroll"]["args_schema"]["properties"]["delta"]["minimum"] == -127
    assert tools["mouse_scroll"]["args_schema"]["properties"]["delta"]["maximum"] == 127

    quick_action_props = tools["quick_action"]["args_schema"]["properties"]
    assert tools["quick_action"]["args_schema"]["additionalProperties"] is False
    assert "platform" not in quick_action_props
    assert tools["quick_action"]["args_schema"]["anyOf"] == [
        {"required": ["action"]},
        {"required": ["list"], "properties": {"list": {"const": True}}},
    ]
    assert "alternative" in quick_action_props
    assert "alternative_index" in quick_action_props


def test_invoke_provider_screenshot(bridge_server):
    """Test POST /api/providers/screenshot."""
    server, base_url, state = bridge_server

    request_body = json.dumps({"format": "jpeg", "quality": 80}).encode()
    req = Request(
        f"{base_url}/api/providers/screenshot",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["ok"] is True
    output = data["data"]
    assert "image" in output
    assert output["meta"]["width"] == 1080
    assert output["meta"]["height"] == 2400


def test_invoke_touch_gesture_tap(bridge_server):
    """Test POST /api/tools/touch_gesture with tap."""
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-002"

    request_body = json.dumps({"input": {"type": "tap", "point": {"x": 500, "y": 800}}}).encode()
    req = Request(
        f"{base_url}/api/tools/touch_gesture",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is False
    output = json.loads(data["output"])
    assert "action_output" in output
    assert "data" in output  # Screenshot included
    assert action_to_dict(state.env.last_action) == {
        "action_type": "CLICK",
        "data": {"point": [500.0, 800.0]},
    }


def test_invoke_touch_gesture_tap_accepts_top_level_string_coordinates(bridge_server):
    """Regression test for Go environment bridge payloads that include top-level x/y."""
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-002b"

    request_body = json.dumps({"input": {"type": "tap", "x": "135", "y": "705"}}).encode()
    req = Request(
        f"{base_url}/api/tools/touch_gesture",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is False
    assert action_to_dict(state.env.last_action) == {
        "action_type": "CLICK",
        "data": {"point": [135.0, 705.0]},
    }


def test_invoke_touch_gesture_tap_accepts_list_point(bridge_server):
    """Regression test for agent traces that emit point as [x, y]."""
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-002c"

    request_body = json.dumps({"input": {"type": "tap", "point": [498, 828]}}).encode()
    req = Request(
        f"{base_url}/api/tools/touch_gesture",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is False
    assert action_to_dict(state.env.last_action) == {
        "action_type": "CLICK",
        "data": {"point": [498.0, 828.0]},
    }


def test_invoke_keyboard_text(bridge_server):
    """Test POST /api/tools/keyboard_text."""
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-003"

    request_body = json.dumps({"input": {"text": "hello world"}}).encode()
    req = Request(
        f"{base_url}/api/tools/keyboard_text",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is False
    assert state.env.page.keyboard.typed == ["hello world"]
    assert state.env.actions == []


def test_timed_out_keyboard_input_is_cancelled_before_later_actions():
    import asyncio
    from urllib.error import HTTPError

    started = Event()
    cancelled = Event()

    class BlockingKeyboard:
        def __init__(self):
            self.typed = []

        async def type(self, text):
            started.set()
            try:
                await asyncio.Event().wait()
            except asyncio.CancelledError:
                cancelled.set()
                raise
            self.typed.append(text)

    class BlockingEnv:
        def __init__(self):
            self.page = SimpleNamespace(keyboard=BlockingKeyboard())

    loop = asyncio.new_event_loop()
    thread = Thread(target=loop.run_forever, daemon=True)
    thread.start()
    state = BridgeEpisodeState(BlockingEnv(), loop)
    state.active_episode_id = "ep-timeout"
    server = BridgeServer(state, host="127.0.0.1", port=0, request_timeout_sec=0.05)
    base_url = server.start()
    try:
        req = Request(
            f"{base_url}/api/tools/keyboard_text",
            data=json.dumps({"input": {"text": "late"}}).encode(),
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        with pytest.raises(HTTPError) as exc_info:
            urlopen(req, timeout=2)
        assert exc_info.value.code == 504
        assert started.wait(timeout=1)
        assert cancelled.wait(timeout=1)
        assert state.env.page.keyboard.typed == []
        follow_up = asyncio.run_coroutine_threadsafe(state.run_env(lambda env: "ready"), loop)
        assert follow_up.result(timeout=1) == "ready"
    finally:
        server.stop()
        loop.call_soon_threadsafe(loop.stop)
        thread.join(timeout=2)
        loop.close()


def test_timed_out_drag_screenshot_releases_active_contact():
    import asyncio
    from urllib.error import HTTPError

    observing = Event()
    released = Event()

    class BlockingEnv:
        def __init__(self):
            self.page = SimpleNamespace(evaluate=self.evaluate)

        async def evaluate(self, _script, payload):
            if payload["operation"] == "cancel":
                released.set()

        async def get_observation(self):
            observing.set()
            await asyncio.Event().wait()

    loop = asyncio.new_event_loop()
    thread = Thread(target=loop.run_forever, daemon=True)
    thread.start()
    state = BridgeEpisodeState(BlockingEnv(), loop)
    state.active_episode_id = "ep-drag-timeout"
    server = BridgeServer(state, host="127.0.0.1", port=0, request_timeout_sec=0.05)
    base_url = server.start()
    try:
        req = Request(
            f"{base_url}/api/tools/touch_gesture",
            data=json.dumps({"input": {"type": "drag_start", "point": {"x": 200, "y": 300}}}).encode(),
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        with pytest.raises(HTTPError) as exc_info:
            urlopen(req, timeout=2)
        assert exc_info.value.code == 504
        assert observing.wait(timeout=1)
        assert released.wait(timeout=1)
        assert not state.input.contact_active
    finally:
        server.stop()
        loop.call_soon_threadsafe(loop.stop)
        thread.join(timeout=2)
        loop.close()


def test_timed_out_mobilegym_step_is_cancelled_before_later_actions():
    import asyncio
    from urllib.error import HTTPError

    started = Event()
    cancelled = Event()

    class BlockingEnv:
        def __init__(self):
            self.actions = []

        async def step(self, action):
            started.set()
            try:
                await asyncio.Event().wait()
            except asyncio.CancelledError:
                cancelled.set()
                raise
            self.actions.append(action)

    loop = asyncio.new_event_loop()
    thread = Thread(target=loop.run_forever, daemon=True)
    thread.start()
    state = BridgeEpisodeState(BlockingEnv(), loop)
    state.active_episode_id = "ep-step-timeout"
    server = BridgeServer(state, host="127.0.0.1", port=0, request_timeout_sec=0.05)
    base_url = server.start()
    try:
        req = Request(
            f"{base_url}/api/tools/keyboard_tap",
            data=json.dumps({"input": {"keys": ["enter"]}}).encode(),
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        with pytest.raises(HTTPError) as exc_info:
            urlopen(req, timeout=2)
        assert exc_info.value.code == 504
        assert started.wait(timeout=1)
        assert cancelled.wait(timeout=1)
        assert state.env.actions == []
        follow_up = asyncio.run_coroutine_threadsafe(state.run_env(lambda env: "ready"), loop)
        assert follow_up.result(timeout=1) == "ready"
    finally:
        server.stop()
        loop.call_soon_threadsafe(loop.stop)
        thread.join(timeout=2)
        loop.close()


def test_invoke_enter_text_maps_to_mobilegym_type_action(bridge_server):
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-enter-text"

    request_body = json.dumps(
        {
            "input": {
                "text": "微信读书",
                "focus": {"x": 500, "y": 120},
            }
        }
    ).encode()
    req = Request(
        f"{base_url}/api/tools/enter_text",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is False
    output = json.loads(data["output"])
    assert output["ok"] is True
    assert action_to_dict(state.env.last_action) == {
        "action_type": "TYPE",
        "data": {"value": "微信读书", "point": [500.0, 120.0]},
    }


def test_invoke_enter_text_supports_ascii(bridge_server):
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-enter-text-bridge"

    request_body = json.dumps(
        {
            "input": {
                "text": "Trip report",
                "focus": {"x": 250, "y": 800},
            }
        }
    ).encode()
    req = Request(
        f"{base_url}/api/tools/enter_text",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is False
    output = json.loads(data["output"])
    assert output["ok"] is True
    assert action_to_dict(state.env.last_action) == {
        "action_type": "TYPE",
        "data": {"value": "Trip report", "point": [250.0, 800.0]},
    }


def test_invoke_enter_text_preserves_exact_whitespace(bridge_server):
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-enter-text-whitespace"

    req = Request(
        f"{base_url}/api/tools/enter_text",
        data=json.dumps({"input": {"text": "  padded  ", "focus": {"x": 500, "y": 120}}}).encode(),
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is False
    output = json.loads(data["output"])
    assert output == {"ok": True}
    assert action_to_dict(state.env.last_action) == {
        "action_type": "TYPE",
        "data": {"value": "  padded  ", "point": [500.0, 120.0]},
    }


def test_invoke_quick_action_handles_mobilegym_common_actions(bridge_server):
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-quick-actions"

    for action, expected_type in [
        ("spotlight_search", "SWIPE"),
        ("search", "SWIPE"),
        ("search_launch_app", "SWIPE"),
        ("app_switch", "SWIPE"),
        ("send", "ENTER"),
    ]:
        req = Request(
            f"{base_url}/api/tools/quick_action",
            data=json.dumps({"input": {"action": action}}).encode(),
            method="POST",
            headers={"Content-Type": "application/json"},
        )

        with urlopen(req, timeout=5) as resp:
            assert resp.status == 200
            data = json.loads(resp.read().decode())

        assert data["is_error"] is False
        assert "unsupported quick_action" not in data["output"]
        assert action_to_dict(state.env.last_action)["action_type"] == expected_type

    no_action_count = state.env.step_count
    for action in ["select_all", "delete_backward", "copy", "paste", "undo", "find", "cut", "browser_refresh", "browser_new_tab"]:
        req = Request(
            f"{base_url}/api/tools/quick_action",
            data=json.dumps({"input": {"action": action}}).encode(),
            method="POST",
            headers={"Content-Type": "application/json"},
        )

        with urlopen(req, timeout=5) as resp:
            assert resp.status == 200
            data = json.loads(resp.read().decode())

        assert data["is_error"] is False
        output = json.loads(data["output"])
        assert output["ok"] is False
        assert output["status"] == "reserved"
        assert "unsupported quick_action" not in data["output"]
        assert state.env.step_count == no_action_count


def test_invoke_quick_action_rejects_legacy_platform_argument(bridge_server):
    _server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-legacy-platform"

    req = Request(
        f"{base_url}/api/tools/quick_action",
        data=json.dumps({"input": {"action": "home", "platform": "ios"}}).encode(),
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is True
    assert "unknown fields" in data["output"]


def test_invoke_keyboard_tap_keycode_app_switch_uses_swipe(bridge_server):
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-keyboard-app-switch"

    req = Request(
        f"{base_url}/api/tools/keyboard_tap",
        data=json.dumps({"input": {"keys": ["KEYCODE_APP_SWITCH"]}}).encode(),
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is False
    assert action_to_dict(state.env.last_action) == {
        "action_type": "SWIPE",
        "data": {"point1": [500.0, 1000.0], "point2": [500.0, 500.0], "duration": 900.0},
    }


@pytest.mark.parametrize(
    ("key_name", "action_type"),
    [("KEYCODE_BACK", "BACK"), ("KEYCODE_HOME", "HOME")],
)
def test_invoke_keyboard_tap_keycode_aliases_preserve_actions(bridge_server, key_name, action_type):
    server, base_url, state = bridge_server
    state.active_episode_id = f"test-episode-keyboard-{key_name.lower()}"

    req = Request(
        f"{base_url}/api/tools/keyboard_tap",
        data=json.dumps({"input": {"keys": [key_name]}}).encode(),
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is False
    assert action_to_dict(state.env.last_action) == {"action_type": action_type, "data": {}}


def test_invoke_keyboard_text_accepts_plain_text_fallback(bridge_server):
    """Matches the Go keyboard_text fallback for bare plain text input."""
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-003b"

    request_body = json.dumps({"raw_input": "plain text"}).encode()
    req = Request(
        f"{base_url}/api/tools/keyboard_text",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is False
    assert state.env.page.keyboard.typed == ["plain text"]
    assert state.env.actions == []


def test_invoke_enter_text_focuses_and_types_unicode(bridge_server):
    _server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-enter-text"

    request_body = json.dumps(
        {
            "input": {
                "text": "隐私",
                "focus": {"x": 500, "y": 80},
            }
        }
    ).encode()
    req = Request(
        f"{base_url}/api/tools/enter_text",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is False
    output = json.loads(data["output"])
    assert output == {"ok": True}
    assert action_to_dict(state.env.last_action) == {
        "action_type": "TYPE",
        "data": {"value": "隐私", "point": [500.0, 80.0]},
    }


def test_invoke_enter_text_rejects_removed_or_missing_arguments(bridge_server):
    _server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-enter-text-invalid"

    for tool_input in (
        {"text": "hello"},
        {"text": "hello", "focus": {"x": 500, "y": 80}, "max_attempts": 3},
        {"text": "hello", "focus": {"x": 500, "y": 80, "extra": True}},
    ):
        request_body = json.dumps({"input": tool_input}).encode()
        req = Request(
            f"{base_url}/api/tools/enter_text",
            data=request_body,
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        with urlopen(req, timeout=5) as resp:
            data = json.loads(resp.read().decode())
        assert data["is_error"] is False
        output = json.loads(data["output"])
        assert output["ok"] is False
        assert "suggestion" in output


def test_invoke_rejects_non_object_json_body(bridge_server):
    from urllib.error import HTTPError

    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-bad-body"

    req = Request(
        f"{base_url}/api/tools/touch_gesture",
        data=json.dumps([]).encode(),
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with pytest.raises(HTTPError) as exc_info:
        urlopen(req, timeout=5)
    assert exc_info.value.code == 400
    data = json.loads(exc_info.value.read().decode())
    assert data["is_error"] is True
    assert data["error"] == "bad_json"


def test_invoke_without_episode_returns_error(bridge_server):
    """Test tool invocation without active episode fails."""
    from urllib.error import HTTPError

    server, base_url, state = bridge_server

    request_body = json.dumps({"input": {"type": "tap", "point": {"x": 500, "y": 500}}}).encode()
    req = Request(
        f"{base_url}/api/tools/touch_gesture",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    try:
        with urlopen(req, timeout=5):
            pytest.fail("Expected HTTPError 409")
    except HTTPError as e:
        assert e.code == 409
        data = json.loads(e.read().decode())
        assert data["is_error"] is True
        assert "no active episode" in data["output"].lower()


def test_invoke_stale_episode_returns_conflict(bridge_server, monkeypatch):
    from urllib.error import HTTPError

    from .episode import StaleEpisodeError

    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-stale"

    def fail_require_active(episode_id):
        raise StaleEpisodeError("stale episode_id")

    monkeypatch.setattr(state, "require_active", fail_require_active)
    request_body = json.dumps({"input": {"type": "tap", "point": {"x": 500, "y": 500}}}).encode()
    req = Request(
        f"{base_url}/api/tools/touch_gesture",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with pytest.raises(HTTPError) as exc_info:
        urlopen(req, timeout=5)
    assert exc_info.value.code == 409
    data = json.loads(exc_info.value.read().decode())
    assert data["error"] == "stale_episode"


def test_touch_gesture_rejects_non_string_type(bridge_server):
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-bad-gesture"

    request_body = json.dumps({"input": {"type": {"kind": "tap"}, "point": {"x": 500, "y": 800}}}).encode()
    req = Request(
        f"{base_url}/api/tools/touch_gesture",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())
    assert data["is_error"] is True
    assert "type must be a string" in data["output"]


@pytest.mark.parametrize("duration_ms", [0, 10_001])
def test_touch_gesture_drag_rejects_out_of_range_duration(bridge_server, duration_ms):
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-bad-drag-duration"

    request_body = json.dumps({
        "input": {
            "type": "drag",
            "start": {"x": 200, "y": 300},
            "end": {"x": 700, "y": 600},
            "duration_ms": duration_ms,
        },
    }).encode()
    req = Request(
        f"{base_url}/api/tools/touch_gesture",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())
    assert data["is_error"] is True
    assert "duration_ms must be in range" in data["output"]
    assert state.env.last_action is None


@pytest.mark.parametrize("speed", [1e-308, 10 ** 400])
def test_touch_gesture_rejects_extreme_swipe_speed(bridge_server, speed):
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-extreme-swipe-speed"

    request_body = json.dumps({
        "input": {
            "type": "swipe",
            "start": {"x": 100, "y": 800},
            "end": {"x": 900, "y": 200},
            "speed": speed,
        },
    }).encode()
    req = Request(
        f"{base_url}/api/tools/touch_gesture",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())
    assert data["is_error"] is True
    assert "speed" in data["output"]
    assert state.env.last_action is None


@pytest.mark.parametrize(
    ("delta", "start_y", "end_y", "duration_ms"),
    [
        (-1, 800.0, 50.0, 300.0),
        (-3, 800.0, 50.0, 450.0),
        (3, 200.0, 950.0, 450.0),
        (127, 200.0, 950.0, 9750.0),
    ],
)
def test_mouse_scroll_preserves_delta_direction_and_magnitude(
    bridge_server, delta, start_y, end_y, duration_ms
):
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-scroll-magnitude"

    request_body = json.dumps({"input": {"delta": delta}}).encode()
    req = Request(
        f"{base_url}/api/tools/mouse_scroll",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())
    assert data["is_error"] is False
    assert action_to_dict(state.env.last_action) == {
        "action_type": "SWIPE",
        "data": {
            "point1": [500.0, start_y],
            "point2": [500.0, end_y],
            "duration": duration_ms,
        },
    }


def test_invoke_without_token_still_works(bridge_server):
    """Test tool invocation without token still works (auth removed)."""
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-004"

    request_body = json.dumps({"input": {"type": "tap", "point": {"x": 500, "y": 500}}}).encode()
    req = Request(
        f"{base_url}/api/tools/touch_gesture",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())
        assert data["is_error"] is False


def test_invoke_unknown_tool_returns_error(bridge_server):
    """Test invoking unknown tool returns error."""
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-005"

    request_body = json.dumps({"input": {"type": "tap", "point": {"x": 500, "y": 500}}}).encode()
    req = Request(
        f"{base_url}/api/tools/unknown_tool",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())

    assert data["is_error"] is True
    assert "unknown tool" in data["output"].lower()


def test_decode_tool_input_formats(bridge_server):
    """Test various input formats are decoded correctly."""
    server, base_url, state = bridge_server
    state.active_episode_id = "test-episode-006"

    # Test raw_input format
    request_body = json.dumps({"raw_input": '{"text": "test"}'}).encode()
    req = Request(
        f"{base_url}/api/tools/keyboard_text",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())
    assert data["is_error"] is False
    assert data["raw_input"] == '{"text": "test"}'

    # Test input as object
    request_body = json.dumps({"input": {"text": "test2"}}).encode()
    req = Request(
        f"{base_url}/api/tools/keyboard_text",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())
    assert data["is_error"] is False

    # Test input as JSON string
    request_body = json.dumps({"input": '{"text": "test3"}'}).encode()
    req = Request(
        f"{base_url}/api/tools/keyboard_text",
        data=request_body,
        method="POST",
        headers={"Content-Type": "application/json"},
    )

    with urlopen(req, timeout=5) as resp:
        assert resp.status == 200
        data = json.loads(resp.read().decode())
    assert data["is_error"] is False


def test_multi_env_tools_require_benchmark_task_id_header():
    import asyncio
    from urllib.error import HTTPError

    loop = asyncio.new_event_loop()
    thread = Thread(target=loop.run_forever, daemon=True)
    thread.start()

    envs = [mock_env_factory(), mock_env_factory()]
    states = [BridgeEpisodeState(env, loop) for env in envs]
    for index, state in enumerate(states):
        state.active_episode_id = f"ep-{index}"
    server = BridgeServer(BridgeTaskRouter(states), host="127.0.0.1", port=0)
    base_url = server.start()
    try:
        request_body = json.dumps({"input": {"type": "tap", "point": {"x": 500, "y": 500}}}).encode()
        req = Request(
            f"{base_url}/api/tools/touch_gesture",
            data=request_body,
            method="POST",
            headers={"Content-Type": "application/json"},
        )
        with pytest.raises(HTTPError) as exc_info:
            urlopen(req, timeout=5)
        assert exc_info.value.code == 400
        data = json.loads(exc_info.value.read().decode())
        assert data["error"] == "missing_benchmark_task_id"
    finally:
        server.stop()
        loop.call_soon_threadsafe(loop.stop)
        thread.join(timeout=2)
        loop.close()


def test_reset_episode_reuses_env_when_reset_succeeds():
    import asyncio

    class RestartableEnv:
        def __init__(self):
            self.calls = []

        async def close(self):
            self.calls.append("close")

        async def start(self):
            self.calls.append("start")
            return self

        async def reset(self, app_ids=None):
            self.calls.append(("reset", app_ids))

    async def run():
        env = RestartableEnv()
        state = BridgeEpisodeState(env, asyncio.get_running_loop())

        result = await state.reset_episode("episode-1", app_ids=[])

        assert result == {"episode_id": "episode-1", "reset": True}
        assert env.calls == [("reset", [])]

    asyncio.run(run())


def test_run_reset_bounds_synchronous_reset(monkeypatch):
    import asyncio
    import time

    from . import episode as episode_mod

    monkeypatch.setattr(episode_mod, "EPISODE_RESET_TIMEOUT_SEC", 0.01)

    def reset(app_ids=None):
        time.sleep(0.1)

    async def run():
        state = BridgeEpisodeState(object(), asyncio.get_running_loop())
        started = time.monotonic()
        with pytest.raises(asyncio.TimeoutError):
            await state._run_reset(reset, app_ids=[])
        return time.monotonic() - started

    assert asyncio.run(run()) < 0.05


def test_reset_episode_isolates_timed_out_synchronous_reset(monkeypatch):
    import asyncio

    from . import episode as episode_mod

    monkeypatch.setattr(episode_mod, "EPISODE_RESET_TIMEOUT_SEC", 0.01)
    monkeypatch.setattr(episode_mod, "EPISODE_RESTART_TIMEOUT_SEC", 0.01)

    class BlockingResetEnv:
        def __init__(self):
            self.allow_reset = Event()
            self.restart_calls = 0
            self.read_calls = 0

        def reset(self, app_ids=None):
            self.allow_reset.wait(timeout=1)

        async def restart(self):
            self.restart_calls += 1

        async def read(self):
            self.read_calls += 1
            return "ready"

    async def run():
        env = BlockingResetEnv()
        state = BridgeEpisodeState(env, asyncio.get_running_loop())

        with pytest.raises(TimeoutError):
            await state.reset_episode("episode-1", app_ids=[])

        restart_calls_before_release = env.restart_calls
        read_task = asyncio.create_task(state.run_env(lambda current: current.read()))
        await asyncio.sleep(0)
        read_finished_before_release = read_task.done()

        env.allow_reset.set()
        result = await read_task
        return (
            restart_calls_before_release,
            read_finished_before_release,
            env.restart_calls,
            result,
        )

    assert asyncio.run(run()) == (0, False, 1, "ready")


def test_cancelled_synchronous_reset_stays_isolated():
    import asyncio

    class BlockingResetEnv:
        def __init__(self):
            self.allow_reset = Event()
            self.reset_started = Event()
            self.read_calls = 0

        def reset(self, app_ids=None):
            self.reset_started.set()
            self.allow_reset.wait(timeout=1)

        async def read(self):
            self.read_calls += 1
            return "ready"

    async def run():
        env = BlockingResetEnv()
        state = BridgeEpisodeState(env, asyncio.get_running_loop())
        reset_task = asyncio.create_task(state._run_reset(env.reset, app_ids=[]))

        await asyncio.to_thread(env.reset_started.wait, 1)
        reset_task.cancel()
        with pytest.raises(asyncio.CancelledError):
            await reset_task

        read_task = asyncio.create_task(state.run_env(lambda current: current.read()))
        await asyncio.sleep(0)
        read_finished_before_release = read_task.done()

        env.allow_reset.set()
        result = await read_task
        return read_finished_before_release, env.read_calls, result

    assert asyncio.run(run()) == (False, 1, "ready")


def test_reset_episode_bounds_synchronous_restart(monkeypatch):
    import asyncio
    import time

    from . import episode as episode_mod

    monkeypatch.setattr(episode_mod, "EPISODE_RESTART_TIMEOUT_SEC", 0.01)

    class BlockingRestartEnv:
        async def reset(self, app_ids=None):
            raise TimeoutError("phase=__OS__ timeout")

        def restart(self):
            time.sleep(0.1)

    async def run():
        state = BridgeEpisodeState(BlockingRestartEnv(), asyncio.get_running_loop())
        started = time.monotonic()
        with pytest.raises(TimeoutError, match="restart timed out"):
            await state.reset_episode("episode-1", app_ids=[])
        return time.monotonic() - started

    assert asyncio.run(run()) < 0.05


def test_reset_episode_catches_builtin_timeout_error(monkeypatch):
    import asyncio

    from . import episode as episode_mod

    class DistinctAsyncioTimeoutError(Exception):
        pass

    monkeypatch.setattr(episode_mod.asyncio, "TimeoutError", DistinctAsyncioTimeoutError)

    class TimeoutEnv:
        def __init__(self):
            self.restart_calls = 0

        async def reset(self, app_ids=None):
            raise TimeoutError("phase=waitForData timeout")

        async def restart(self):
            self.restart_calls += 1

    async def run():
        env = TimeoutEnv()
        state = BridgeEpisodeState(env, asyncio.get_running_loop())

        with pytest.raises(TimeoutError, match="environment reset timed out"):
            await state.reset_episode("episode-1", app_ids=[])

        assert env.restart_calls == 2

    asyncio.run(run())


def test_reset_episode_retries_after_reset_timeout(monkeypatch):
    import asyncio
    from . import episode as episode_mod

    monkeypatch.setattr(episode_mod, "EPISODE_RESET_TIMEOUT_SEC", 0.01)

    class TimeoutThenSuccessEnv:
        def __init__(self):
            self.calls = []
            self.reset_calls = 0

        async def close(self):
            self.calls.append("close")

        async def start(self):
            self.calls.append("start")
            return self

        async def reset(self, app_ids=None):
            self.calls.append(("reset", app_ids))
            self.reset_calls += 1
            if self.reset_calls == 1:
                await asyncio.sleep(10)

    async def run():
        env = TimeoutThenSuccessEnv()
        state = BridgeEpisodeState(env, asyncio.get_running_loop())

        result = await state.reset_episode("episode-1", app_ids=["settings"])

        assert result == {"episode_id": "episode-1", "reset": True}
        assert env.calls == [
            ("reset", ["settings"]),
            "close",
            "start",
            ("reset", ["settings"]),
        ]

    asyncio.run(run())


def test_reset_episode_recreates_env_after_final_timeout(monkeypatch):
    import asyncio
    from . import episode as episode_mod

    monkeypatch.setattr(episode_mod, "EPISODE_RESET_TIMEOUT_SEC", 0.01)

    class AlwaysTimeoutEnv:
        def __init__(self):
            self.calls = []

        async def close(self):
            self.calls.append("close")

        async def start(self):
            self.calls.append("start")
            return self

        async def reset(self, app_ids=None):
            self.calls.append(("reset", app_ids))
            await asyncio.sleep(10)

    async def run():
        env = AlwaysTimeoutEnv()
        state = BridgeEpisodeState(env, asyncio.get_running_loop())

        with pytest.raises(TimeoutError, match="reset timed out"):
            await state.reset_episode("episode-1", app_ids=[])

        assert env.calls == [
            ("reset", []),
            "close",
            "start",
            ("reset", []),
            "close",
            "start",
        ]
        assert state.active_episode_id is None

    asyncio.run(run())


def test_reset_episode_bounds_environment_restart(monkeypatch):
    import asyncio
    from . import episode as episode_mod

    monkeypatch.setattr(episode_mod, "EPISODE_RESTART_TIMEOUT_SEC", 0.01)

    class HangingRestartEnv:
        async def reset(self, app_ids=None):
            raise TimeoutError("phase=__OS__ timeout")

        async def close(self):
            await asyncio.sleep(10)

        async def start(self):
            return self

    async def run():
        state = BridgeEpisodeState(HangingRestartEnv(), asyncio.get_running_loop())

        with pytest.raises(TimeoutError, match="restart timed out"):
            await state.reset_episode("episode-1", app_ids=[])

    asyncio.run(run())


def test_reset_episode_keeps_timed_out_restart_isolated_until_it_finishes(monkeypatch):
    import asyncio
    from . import episode as episode_mod

    monkeypatch.setattr(episode_mod, "EPISODE_RESTART_TIMEOUT_SEC", 0.01)

    class SlowRestartEnv:
        def __init__(self):
            self.allow_start = asyncio.Event()
            self.started = False
            self.closed = False
            self.reset_calls = 0
            self.read_calls = 0

        async def reset(self, app_ids=None):
            self.reset_calls += 1
            if self.closed:
                raise RuntimeError("Call start() first")
            if not self.started:
                raise TimeoutError("phase=__OS__ timeout")

        async def close(self):
            self.closed = True

        async def start(self):
            await self.allow_start.wait()
            self.started = True
            self.closed = False
            return self

        async def read(self):
            self.read_calls += 1
            if self.closed:
                raise RuntimeError("read raced with restart")
            return "ready"

    async def run():
        env = SlowRestartEnv()
        state = BridgeEpisodeState(env, asyncio.get_running_loop())

        with pytest.raises(TimeoutError, match="restart timed out"):
            await state.reset_episode("episode-1", app_ids=[])

        read_task = asyncio.create_task(state.run_env(lambda current: current.read()))
        await asyncio.sleep(0)
        assert env.read_calls == 0

        env.allow_start.set()
        assert await read_task == "ready"
        result = await state.reset_episode("episode-2", app_ids=[])

        assert result == {"episode_id": "episode-2", "reset": True}
        assert env.reset_calls == 2

    asyncio.run(run())


def test_setup_token_deduplicates_concurrent_and_completed_requests():
    import asyncio

    class BlockingResetEnv:
        def __init__(self):
            self.reset_calls = 0
            self.reset_entered = Event()
            self.allow_reset = Event()

        def reset(self, app_ids=None):
            self.reset_calls += 1
            self.reset_entered.set()
            self.allow_reset.wait(timeout=5)

    loop = asyncio.new_event_loop()
    loop_thread = Thread(target=loop.run_forever, daemon=True)
    loop_thread.start()
    env = BlockingResetEnv()
    state = BridgeEpisodeState(env, loop)
    server = BridgeServer(state, host="127.0.0.1", port=0)
    base_url = server.start()
    responses = []

    def setup(payload):
        request = Request(
            f"{base_url}/api/setup",
            data=json.dumps(payload).encode(),
            method="POST",
            headers={
                "Content-Type": "application/json",
                "benchmark-task-id": "task.setup-token",
            },
        )
        with urlopen(request, timeout=5) as response:
            responses.append((response.status, json.loads(response.read().decode())))

    first = Thread(target=setup, args=({"episode_id": "episode-1", "setup_token": "token-1"},))
    second = Thread(target=setup, args=({"episode_id": "episode-1", "setup_token": "token-1"},))
    try:
        first.start()
        assert env.reset_entered.wait(timeout=2)
        second.start()
        time.sleep(0.1)
        assert env.reset_calls == 1
        env.allow_reset.set()
        first.join(timeout=5)
        second.join(timeout=5)
        assert not first.is_alive()
        assert not second.is_alive()
        assert [status for status, _ in responses] == [200, 200]
        assert env.reset_calls == 1

        setup({"episode_id": "episode-1", "setup_token": "token-1"})
        assert env.reset_calls == 1

        setup({"episode_id": "episode-1", "setup_token": "token-2"})
        assert env.reset_calls == 2

        release = Request(
            f"{base_url}/api/release",
            data=b"{}",
            method="POST",
            headers={
                "Content-Type": "application/json",
                "benchmark-task-id": "task.setup-token",
            },
        )
        with urlopen(release, timeout=5) as response:
            assert response.status == 200
        setup({"episode_id": "episode-1", "setup_token": "token-2"})
        assert env.reset_calls == 3

        setup({"episode_id": "episode-1"})
        setup({"episode_id": "episode-1"})
        assert env.reset_calls == 5
    finally:
        env.allow_reset.set()
        server.stop()
        loop.call_soon_threadsafe(loop.stop)
        loop_thread.join(timeout=2)
        loop.close()


def test_multi_env_tools_route_by_benchmark_task_id_header():
    import asyncio

    loop = asyncio.new_event_loop()
    thread = Thread(target=loop.run_forever, daemon=True)
    thread.start()

    envs = [mock_env_factory(), mock_env_factory()]
    states = [BridgeEpisodeState(env, loop) for env in envs]
    for index, state in enumerate(states):
        state.active_episode_id = f"ep-{index}"
    server = BridgeServer(BridgeTaskRouter(states), host="127.0.0.1", port=0)
    base_url = server.start()
    try:
        for task_id, point in [
            ("task.alpha", {"x": 111, "y": 222}),
            ("task.beta", {"x": 333, "y": 444}),
        ]:
            request_body = json.dumps({"input": {"type": "tap", "point": point}}).encode()
            req = Request(
                f"{base_url}/api/tools/touch_gesture",
                data=request_body,
                method="POST",
                headers={"Content-Type": "application/json", "benchmark-task-id": task_id},
            )
            with urlopen(req, timeout=5) as resp:
                assert resp.status == 200
                data = json.loads(resp.read().decode())
                assert data["is_error"] is False

        assert action_to_dict(envs[0].last_action) == {
            "action_type": "CLICK",
            "data": {"point": [111.0, 222.0]},
        }
        assert action_to_dict(envs[1].last_action) == {
            "action_type": "CLICK",
            "data": {"point": [333.0, 444.0]},
        }
    finally:
        server.stop()
        loop.call_soon_threadsafe(loop.stop)
        thread.join(timeout=2)
        loop.close()


def test_multi_env_tools_return_capacity_error_until_task_released():
    import asyncio
    from urllib.error import HTTPError

    loop = asyncio.new_event_loop()
    thread = Thread(target=loop.run_forever, daemon=True)
    thread.start()

    envs = [mock_env_factory()]
    states = [BridgeEpisodeState(envs[0], loop)]
    states[0].active_episode_id = "ep-0"
    server = BridgeServer(BridgeTaskRouter(states), host="127.0.0.1", port=0)
    base_url = server.start()
    try:
        for task_id in ("task.alpha", "task.beta"):
            request_body = json.dumps({"input": {"type": "tap", "point": {"x": 500, "y": 500}}}).encode()
            req = Request(
                f"{base_url}/api/tools/touch_gesture",
                data=request_body,
                method="POST",
                headers={"Content-Type": "application/json", "benchmark-task-id": task_id},
            )
            if task_id == "task.alpha":
                with urlopen(req, timeout=5) as resp:
                    assert resp.status == 200
            else:
                with pytest.raises(HTTPError) as exc_info:
                    urlopen(req, timeout=5)
                assert exc_info.value.code == 429
                data = json.loads(exc_info.value.read().decode())
                assert data["error"] == "no_bridge_env_available"

        release_req = Request(
            f"{base_url}/api/release",
            data=b"{}",
            method="POST",
            headers={"Content-Type": "application/json", "benchmark-task-id": "task.alpha"},
        )
        with urlopen(release_req, timeout=5) as resp:
            assert resp.status == 200

        assert states[0].active_episode_id is None
        setup_req = Request(
            f"{base_url}/api/setup",
            data=json.dumps({"episode_id": "ep-beta"}).encode(),
            method="POST",
            headers={"Content-Type": "application/json", "benchmark-task-id": "task.beta"},
        )
        with urlopen(setup_req, timeout=5) as resp:
            assert resp.status == 200

        request_body = json.dumps({"input": {"type": "tap", "point": {"x": 500, "y": 500}}}).encode()
        req = Request(
            f"{base_url}/api/tools/touch_gesture",
            data=request_body,
            method="POST",
            headers={"Content-Type": "application/json", "benchmark-task-id": "task.beta"},
        )
        with urlopen(req, timeout=5) as resp:
            assert resp.status == 200
    finally:
        server.stop()
        loop.call_soon_threadsafe(loop.stop)
        thread.join(timeout=2)
        loop.close()
