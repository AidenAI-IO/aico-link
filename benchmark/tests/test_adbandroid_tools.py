import base64
import json
import urllib.error
import urllib.request

import pytest

from adbandroid.bridge.server import ADBBridgeServer
from adbandroid.bridge.tools_api import _normalized_point_arg, _to_pixels

from tests.test_adbandroid_bridge import FakeADBAndroidDevice, _request


@pytest.fixture()
def bridge():
    device = FakeADBAndroidDevice()  # screen 1080x1920
    server = ADBBridgeServer(device, host="127.0.0.1", port=0, action_settle_sec=0)
    base_url = server.start()
    server.state.acquire("suite:task")  # active episode for tool calls
    try:
        yield server, device, base_url
    finally:
        server.stop()


def _invoke(base_url, tool, tool_input, *, task_id="suite:task"):
    headers = {"Content-Type": "application/json", "benchmark-task-id": task_id}
    raw = tool_input if isinstance(tool_input, str) else json.dumps(tool_input)
    data = json.dumps({"input": raw}).encode("utf-8")
    req = urllib.request.Request(
        f"{base_url}/api/tools/{tool}", data=data, headers=headers, method="POST"
    )
    try:
        with urllib.request.urlopen(req, timeout=5) as resp:
            return resp.status, json.loads(resp.read().decode("utf-8"))
    except urllib.error.HTTPError as exc:
        return exc.code, json.loads(exc.read().decode("utf-8"))


def _post_action_output(body):
    assert body["is_error"] is False, body
    return json.loads(body["output"])


def _text_entry_output(body):
    output = _post_action_output(body)
    assert "action_output" in output
    return json.loads(output["action_output"]), output


# ---- coordinate handling ----------------------------------------------------


def test_normalized_point_converts_to_pixels():
    point = _normalized_point_arg({"point": {"x": 500, "y": 500}})
    assert _to_pixels(point, 1080, 1920) == (540, 960)


def test_normalized_point_rejects_out_of_range_coordinates():
    with pytest.raises(ValueError, match="normalized 0-1000"):
        _normalized_point_arg({"point": {"x": 1500, "y": 500}})


# ---- touch_gesture ----------------------------------------------------------


def test_touch_gesture_tap(bridge):
    _, device, base_url = bridge
    status, body = _invoke(base_url, "touch_gesture", {"type": "tap", "point": {"x": 500, "y": 500}})
    assert status == 200
    output = _post_action_output(body)
    assert output["action_output"] == "ok"
    assert ("tap", 540, 960) in device.calls


def test_touch_gesture_rejects_retired_coord_space(bridge):
    _, _, base_url = bridge
    status, body = _invoke(
        base_url,
        "touch_gesture",
        {"type": "tap", "point": {"x": 540, "y": 960}, "coord_space": "pixel"},
    )
    assert status == 200
    assert body["is_error"] is True
    assert "coord_space" in body["output"]


def test_touch_gesture_swipe(bridge):
    _, device, base_url = bridge
    status, body = _invoke(
        base_url,
        "touch_gesture",
        {"type": "swipe", "start": {"x": 500, "y": 800}, "end": {"x": 500, "y": 200}, "duration_ms": 400},
    )
    assert status == 200
    assert body["is_error"] is False
    assert ("swipe", 540, 1536, 540, 384, 400) in device.calls


def test_touch_gesture_directional_swipe(bridge):
    _, device, base_url = bridge
    status, body = _invoke(
        base_url,
        "touch_gesture",
        {"type": "swipe", "start": {"x": 500, "y": 800}, "direction": "up", "speed": 2500, "duration_ms": 200},
    )
    assert status == 200
    assert body["is_error"] is False
    swipes = [call for call in device.calls if call[0] == "swipe"]
    assert len(swipes) == 1
    _, x1, y1, x2, y2, duration = swipes[0]
    assert x1 == x2 == 540  # start.x 500 normalized
    assert y1 > y2  # upward
    assert duration == 200


def test_touch_gesture_rejects_retired_navigation_types(bridge):
    _, device, base_url = bridge
    for gesture_type in ("home", "back", "swipe_up"):
        status, body = _invoke(base_url, "touch_gesture", {"type": gesture_type})
        assert status == 200 and body["is_error"] is True
    assert all(call[0] != "keyevent" for call in device.calls)


def test_touch_gesture_out_of_range_is_error(bridge):
    _, _, base_url = bridge
    status, body = _invoke(
        base_url,
        "touch_gesture",
        {"type": "tap", "point": {"x": 1500, "y": 500}},
    )
    assert status == 200
    assert body["is_error"] is True
    assert "normalized 0-1000" in body["output"]


# ---- keyboard tools ----------------------------------------------------------


def test_keyboard_tap_keys_array_semantics(bridge):
    _, device, base_url = bridge
    status, body = _invoke(base_url, "keyboard_tap", {"keys": ["enter"]})
    assert status == 200 and body["is_error"] is False
    assert ("keyevent", 66) in device.calls

    _invoke(base_url, "keyboard_tap", {"keys": ["keycode_back"]})
    assert ("keyevent", "KEYCODE_BACK") in device.calls

    _invoke(base_url, "keyboard_tap", {"keys": ["meta", "h"]})
    assert ("keyevent", "KEYCODE_HOME") in device.calls

    _invoke(base_url, "keyboard_tap", {"keys": ["backspace"]})
    assert ("keyevent", 67) in device.calls

    _invoke(base_url, "keyboard_tap", {"keys": ["delete"]})
    assert device.calls[-1][0:2] == ("screenshot_jpeg",)
    assert ("keyevent", 67) in device.calls

    status, body = _invoke(base_url, "keyboard_tap", {"keys": ["f5"]})
    assert body["is_error"] is True

    status, body = _invoke(base_url, "keyboard_tap", {"keys": []})
    assert body["is_error"] is True


def test_keyboard_text_accepts_dict_and_plain_string(bridge):
    _, device, base_url = bridge
    status, body = _invoke(base_url, "keyboard_text", {"text": "hello android"})
    assert status == 200 and body["is_error"] is False
    assert ("input_text", "hello android") in device.calls

    device.calls.clear()
    status, body = _invoke(base_url, "keyboard_text", "plain text input")
    assert status == 200 and body["is_error"] is False
    assert ("input_text", "plain text input") in device.calls


def test_keyboard_text_rejects_non_ascii(bridge):
    _, _, base_url = bridge
    status, body = _invoke(base_url, "keyboard_text", {"text": "你好"})
    assert status == 200
    assert body["is_error"] is True
    assert "US-keyboard ASCII" in body["output"]


def test_enter_text_taps_focus_then_types(bridge):
    _, device, base_url = bridge
    status, body = _invoke(
        base_url,
        "enter_text",
        {"text": "hello android", "focus": {"x": 500, "y": 100}},
    )
    assert status == 200
    output, screenshot = _text_entry_output(body)
    assert output == {"ok": True}
    assert screenshot["width"] == 720
    tap_index = device.calls.index(("tap", 540, 192))
    text_index = device.calls.index(("input_text", "hello android"))
    verify_index = device.calls.index(("dump_window_xml",))
    assert tap_index < text_index
    assert text_index < verify_index


def test_enter_text_does_not_claim_success_on_mismatch(bridge):
    _, device, base_url = bridge
    device.window_text_override = "hello一"
    status, body = _invoke(
        base_url,
        "enter_text",
        {"text": "hello-aico", "focus": {"x": 500, "y": 100}},
    )
    assert status == 200
    output, screenshot = _text_entry_output(body)
    assert output["ok"] is False
    assert set(output) == {"ok", "suggestion"}
    assert "English/Latin keyboard" in output["suggestion"]
    assert screenshot["format"] == "jpeg"


def test_enter_text_verifies_only_the_new_text_suffix(bridge):
    _, device, base_url = bridge
    device.window_text_override = "existing: hello android"
    status, body = _invoke(
        base_url,
        "enter_text",
        {"text": "hello android", "focus": {"x": 500, "y": 100}},
    )
    assert status == 200
    output, _ = _text_entry_output(body)
    assert output == {"ok": True}


def test_enter_text_rejects_non_suffix_match(bridge):
    _, device, base_url = bridge
    device.window_text_override = "hello android trailing"
    status, body = _invoke(
        base_url,
        "enter_text",
        {"text": "hello android", "focus": {"x": 500, "y": 100}},
    )
    assert status == 200
    output, _ = _text_entry_output(body)
    assert output["ok"] is False


def test_enter_text_rejects_removed_or_missing_arguments(bridge):
    _, device, base_url = bridge
    for tool_input in (
        {"text": "hello"},
        {"text": "hello", "focus": {"x": 500, "y": 100}, "segments": ["hello"]},
        {"text": "hello", "focus": {"x": 500, "y": 100, "extra": True}},
    ):
        status, body = _invoke(base_url, "enter_text", tool_input)
        assert status == 200
        output = json.loads(body["output"])
        assert output["ok"] is False
        assert "suggestion" in output
    assert all(call[0] != "input_text" for call in device.calls)


def test_enter_text_reports_unsupported_text_without_typing(bridge):
    _, device, base_url = bridge
    status, body = _invoke(
        base_url,
        "enter_text",
        {"text": "你好", "focus": {"x": 500, "y": 100}},
    )
    assert status == 200
    assert body["is_error"] is False
    output = json.loads(body["output"])
    assert output["ok"] is False
    assert "US-keyboard ASCII" in output["suggestion"]
    assert all(call[0] != "input_text" for call in device.calls)


# ---- mouse tools --------------------------------------------------------------


def test_touch_gesture_tap_taps(bridge):
    _, device, base_url = bridge
    status, body = _invoke(
        base_url,
        "touch_gesture",
        {"type": "tap", "point": {"x": 500, "y": 500}},
    )
    assert status == 200 and body["is_error"] is False
    assert ("tap", 540, 960) in device.calls


def test_mouse_move_is_noop_with_screenshot(bridge):
    _, device, base_url = bridge
    status, body = _invoke(base_url, "mouse_move", {"x": 500, "y": 500})
    assert status == 200 and body["is_error"] is False
    output = json.loads(body["output"])
    assert output["action_output"] == "ok"
    assert all(call[0] in ("screenshot_jpeg",) for call in device.calls)


def test_mouse_scroll_swipes_vertically(bridge):
    _, device, base_url = bridge
    status, body = _invoke(base_url, "mouse_scroll", {"delta": -3})
    assert status == 200 and body["is_error"] is False
    swipes = [call for call in device.calls if call[0] == "swipe"]
    assert len(swipes) == 1
    _, _, y1, _, y2, _ = swipes[0]
    assert y1 > y2  # delta < 0 scrolls up


# ---- quick_action --------------------------------------------------------------


def test_quick_action_uses_android_platform_from_environment(bridge):
    _, device, base_url = bridge
    status, body = _invoke(base_url, "quick_action", {"action": "home"})
    assert status == 200 and body["is_error"] is False
    assert ("keyevent", "KEYCODE_HOME") in device.calls


def test_quick_action_list_returns_catalog(bridge):
    _, _, base_url = bridge
    status, body = _invoke(base_url, "quick_action", {"list": True})
    assert status == 200 and body["is_error"] is False
    output = json.loads(body["output"])
    ids = {item["id"] for item in output["actions"]}
    assert {"back", "home", "app_switch", "open_settings", "notification_center", "send"} <= ids


def test_quick_action_rejects_legacy_platform_argument(bridge):
    _, _, base_url = bridge
    status, body = _invoke(base_url, "quick_action", {"platform": "ios", "action": "home"})
    assert status == 200 and body["is_error"] is True
    assert "unknown fields" in body["output"]


def test_quick_action_open_settings(bridge):
    _, device, base_url = bridge
    status, body = _invoke(base_url, "quick_action", {"action": "open_settings"})
    assert status == 200 and body["is_error"] is False
    assert ("start_settings",) in device.calls

    # Alias resolution: "settings" maps to open_settings.
    device.calls.clear()
    _invoke(base_url, "quick_action", {"action": "settings"})
    assert ("start_settings",) in device.calls


def test_quick_action_home_back_send(bridge):
    _, device, base_url = bridge
    _invoke(base_url, "quick_action", {"action": "home"})
    _invoke(base_url, "quick_action", {"action": "back"})
    _invoke(base_url, "quick_action", {"action": "send"})
    assert ("keyevent", "KEYCODE_HOME") in device.calls
    assert ("keyevent", "KEYCODE_BACK") in device.calls
    assert ("keyevent", 66) in device.calls


def test_quick_action_statusbar_actions(bridge):
    _, device, base_url = bridge
    _invoke(base_url, "quick_action", {"action": "notification_center"})
    assert ("expand_notifications",) in device.calls
    _invoke(base_url, "quick_action", {"action": "control_center"})
    assert ("expand_settings",) in device.calls
    _invoke(base_url, "quick_action", {"action": "dismiss_panel"})
    assert ("collapse_statusbar",) in device.calls


def test_quick_action_reserved_and_alternative(bridge):
    _, _, base_url = bridge
    status, body = _invoke(base_url, "quick_action", {"action": "copy"})
    assert status == 200 and body["is_error"] is False
    output = json.loads(body["output"])
    assert output["status"] == "reserved"

    status, body = _invoke(
        base_url, "quick_action", {"action": "home", "alternative": True}
    )
    output = json.loads(body["output"])
    assert output["status"] == "reserved"


# ---- screenshot ------------------------------------------------------------------


def test_screenshot_output_shape(bridge):
    _, _, base_url = bridge
    status, body = _request(
        base_url,
        "/api/providers/screenshot",
        method="POST",
        payload={"format": "jpeg", "quality": 80},
    )
    assert status == 200 and body["ok"] is True
    data = body["data"]
    assert set(data) == {"meta", "capture_info", "image"}
    assert data["meta"]["width"] == 720 and data["meta"]["height"] == 1280
    assert data["meta"]["pixel_format"] == "jpeg"
    assert data["capture_info"]["capture_backend"] == "adb"
    assert base64.b64decode(data["image"]) == b"fake-jpeg-bytes"


def test_tool_response_envelope_matches_mobilegym(bridge):
    _, _, base_url = bridge
    status, body = _invoke(base_url, "touch_gesture", {"type": "tap", "point": {"x": 500, "y": 500}})
    assert status == 200
    assert set(body) >= {"tool", "raw_input", "output", "is_error", "duration_ms"}
    assert body["tool"] == {"name": "touch_gesture"}


# ---- duration clamping ---------------------------------------------------------


def test_swipe_duration_out_of_range_is_rejected(bridge):
    _, device, base_url = bridge
    status, body = _invoke(
        base_url,
        "touch_gesture",
        {"type": "swipe", "start": {"x": 500, "y": 800}, "end": {"x": 500, "y": 200}, "duration_ms": 99999999},
    )
    assert status == 200 and body["is_error"] is True
    assert not [call for call in device.calls if call[0] == "swipe"]


def test_long_press_duration_is_clamped(bridge):
    _, device, base_url = bridge
    status, body = _invoke(
        base_url,
        "touch_gesture",
        {"type": "long_press", "point": {"x": 500, "y": 500}, "duration_ms": 3600000},
    )
    assert status == 200 and body["is_error"] is False
    swipes = [call for call in device.calls if call[0] == "swipe"]
    assert swipes[0][5] == 10000


def test_directional_swipe_duration_out_of_range_is_rejected(bridge):
    _, device, base_url = bridge
    status, body = _invoke(
        base_url,
        "touch_gesture",
        {"type": "swipe", "start": {"x": 500, "y": 800}, "direction": "up", "duration_ms": -5},
    )
    assert status == 200 and body["is_error"] is True
    assert not [call for call in device.calls if call[0] == "swipe"]


def test_nonnumeric_duration_is_rejected(bridge):
    _, device, base_url = bridge
    status, body = _invoke(
        base_url,
        "touch_gesture",
        {"type": "swipe", "start": {"x": 500, "y": 800}, "end": {"x": 500, "y": 200}, "duration_ms": "abc"},
    )
    assert status == 200 and body["is_error"] is True
    assert not [call for call in device.calls if call[0] == "swipe"]


def test_keyboard_tap_rejects_ctrl_alt_shift_combos(bridge):
    _, device, base_url = bridge
    # Silently executing plain Enter for ctrl+enter would record an action
    # that diverges from what actually ran on the device — must error instead.
    for combo in (["ctrl", "enter"], ["alt", "tab"], ["shift", "enter"]):
        status, body = _invoke(base_url, "keyboard_tap", {"keys": combo})
        assert status == 200
        assert body["is_error"] is True
        assert "ctrl/alt/shift" in body["output"]
    assert all(call[0] != "keyevent" for call in device.calls)

    # meta-based home shortcuts keep working (MobileGym-compatible semantics).
    status, body = _invoke(base_url, "keyboard_tap", {"keys": ["meta", "h"]})
    assert status == 200 and body["is_error"] is False
    assert ("keyevent", "KEYCODE_HOME") in device.calls
