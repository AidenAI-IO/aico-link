from __future__ import annotations

import base64
import http.client
import io
import json
import urllib.error
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from pathlib import Path

import pytest
from PIL import Image

from runner import mock_environment as mock_environment_module
from runner.mock_environment import MAX_REQUEST_BODY_BYTES, MockEnvironmentServer
from runner.suite import MockEnvironmentSpec, MockToolResponseSpec, load_suite


def _json_request(url: str, method: str = "GET", payload: dict | None = None) -> dict:
    data = None if payload is None else json.dumps(payload).encode("utf-8")
    req = urllib.request.Request(
        url,
        data=data,
        headers={"Content-Type": "application/json"},
        method=method,
    )
    with urllib.request.urlopen(req, timeout=5) as response:
        return json.loads(response.read().decode("utf-8"))


def _task_mock_environment(suite_name: str, task_id: str):
    suite_path = (
        Path(__file__).resolve().parents[1] / "suites" / "aiden_app" / suite_name
    )
    suite = load_suite(suite_path)
    task = next(task for task in suite.tasks if task.id == task_id)
    assert task.mock_environment is not None
    return suite_path, task.mock_environment


def test_mock_environment_wildcard_bind_advertises_loopback(tmp_path):
    spec = MockEnvironmentSpec(
        platform="ios",
        phone_bridge={},
        tools={"bridge_contacts": [MockToolResponseSpec(output={"ok": True})]},
    )
    server = MockEnvironmentServer(spec, tmp_path, host="0.0.0.0")
    base_url = server.start()
    try:
        assert server._httpd is not None
        assert server._httpd.server_address[0] == "0.0.0.0"
        parsed = urllib.parse.urlparse(base_url)
        assert parsed.hostname == "127.0.0.1"
        assert parsed.path == f"/_aiden_mock/{server.auth_token}"
        health = _json_request(f"{base_url}/health")
        assert health["ok"] is True
        assert health["data"]["platform"] == "ios"
        catalog = _json_request(f"{base_url}/api/tools")
        assert catalog["tools"][0]["http"]["path"].startswith(parsed.path)

        unprotected_url = urllib.parse.urlunparse(
            parsed._replace(path="/health", params="", query="", fragment="")
        )
        with pytest.raises(urllib.error.HTTPError) as exc_info:
            _json_request(unprotected_url)
        assert exc_info.value.code == 404
    finally:
        server.stop()


def test_mock_environment_returns_scripted_tool_result_and_updates_screen():
    suite_path, spec = _task_mock_environment(
        "notes_entry_policy_v1.json",
        "ios_pip_notes_icon_missing",
    )
    server = MockEnvironmentServer(spec, suite_path.parent)
    base_url = server.start()
    try:
        setup = _json_request(f"{base_url}/api/setup", "POST", {})
        assert setup["ok"] is True

        contacts = _json_request(
            f"{base_url}/api/tools/bridge_contacts",
            "POST",
            {"input": json.dumps({"action": "query", "query": "Biden"})},
        )
        output = json.loads(contacts["output"])
        assert output["contacts"][0]["phone_numbers"] == ["+1 202-555-0147"]

        blocked = _json_request(
            f"{base_url}/api/tools/enter_text",
            "POST",
            {
                "input": json.dumps(
                    {
                        "text": "+1 202-555-0147",
                        "focus": {"x": 500, "y": 360},
                    }
                )
            },
        )
        assert blocked["is_error"] is True

        opened = _json_request(
            f"{base_url}/api/tools/open_app",
            "POST",
            {"input": json.dumps({"app": "备忘录"})},
        )
        assert json.loads(opened["output"])["ok"] is True

        missing_focus = _json_request(
            f"{base_url}/api/tools/enter_text",
            "POST",
            {"input": json.dumps({"text": "+1 202-555-0147"})},
        )
        assert missing_focus["is_error"] is True

        entered = _json_request(
            f"{base_url}/api/tools/enter_text",
            "POST",
            {
                "input": json.dumps(
                    {
                        "text": "Biden: +1 202-555-0147",
                        "focus": {
                            "x": 500,
                            "y": 360,
                        },
                    }
                )
            },
        )
        assert json.loads(entered["output"]) == {"ok": True}

        state = _json_request(f"{base_url}/api/state")
        assert state["data"]["calls"][-1]["tool"] == "enter_text"
        assert "+1 202-555-0147" in state["data"]["screen_text"]

        screen = _json_request(f"{base_url}/api/providers/screenshot", "POST", {"format": "jpeg", "quality": 80})
        meta = screen["data"]["meta"]
        assert meta["width"] == 1170
        assert meta["seq"] == 1
        assert meta["bytes"] > 0
        assert len(base64.b64decode(screen["data"]["image"])) == meta["bytes"]
        screen2 = _json_request(f"{base_url}/api/providers/screenshot", "POST", {"format": "jpeg", "quality": 80})
        assert screen2["data"]["meta"]["seq"] == 2
    finally:
        server.stop()


def test_perception_mock_environment_exposes_fixture_and_accepts_tap():
    suite_path = (
        Path(__file__).resolve().parents[1]
        / "suites"
        / "perception"
        / "perception_v1.json"
    )
    suite = load_suite(suite_path)
    task = next(task for task in suite.tasks if task.id == "task_4657")
    assert task.mock_environment is not None
    server = MockEnvironmentServer(task.mock_environment, suite_path.parent)
    base_url = server.start()
    try:
        before = _json_request(
            f"{base_url}/api/providers/screenshot",
            "POST",
            {"format": "jpeg", "quality": 80},
        )
        before_meta = before["data"]["meta"]
        assert (before_meta["width"], before_meta["height"]) == (447, 972)
        before_image = base64.b64decode(before["data"]["image"])

        tap = _json_request(
            f"{base_url}/api/tools/touch_gesture",
            "POST",
            {
                "input": json.dumps(
                    {"type": "tap", "point": {"x": 940, "y": 95}}
                )
            },
        )
        assert tap["is_error"] is False
        assert json.loads(tap["output"]) == {"ok": True}

        mnk_tap = _json_request(
            f"{base_url}/api/providers/mnk",
            "POST",
            {
                "operation": "click",
                "click": {"x": 940, "y": 95, "button": "left", "hold_ms": 0},
            },
        )
        assert mnk_tap == {"success": True}

        assert before_image
    finally:
        server.stop()


def test_mock_environment_requires_visible_icon_click_before_text_entry():
    suite_path, spec = _task_mock_environment(
        "notes_entry_policy_v1.json",
        "ios_pip_notes_icon_visible",
    )
    server = MockEnvironmentServer(spec, suite_path.parent)
    base_url = server.start()
    try:
        blocked = _json_request(
            f"{base_url}/api/tools/enter_text",
            "POST",
            {
                "input": json.dumps(
                    {
                        "text": "+1 202-555-0147",
                        "focus": {"x": 500, "y": 360},
                    }
                )
            },
        )
        assert blocked["is_error"] is True

        clicked = _json_request(
            f"{base_url}/api/tools/touch_gesture",
            "POST",
            {
                "input": json.dumps(
                    {
                        "type": "tap",
                        "point": {"x": 180, "y": 310},
                    }
                )
            },
        )
        assert json.loads(clicked["output"])["clicked"] is True

        entered = _json_request(
            f"{base_url}/api/tools/enter_text",
            "POST",
            {
                "input": json.dumps(
                    {
                        "text": "+1 202-555-0147",
                        "focus": {"x": 500, "y": 360},
                    }
                )
            },
        )
        assert json.loads(entered["output"]) == {"ok": True}
    finally:
        server.stop()


def test_mock_environment_allows_direct_entry_when_notes_is_already_open():
    suite_path, spec = _task_mock_environment(
        "notes_entry_policy_v1.json",
        "ios_pip_notes_already_open",
    )
    server = MockEnvironmentServer(spec, suite_path.parent)
    base_url = server.start()
    try:
        entered = _json_request(
            f"{base_url}/api/tools/enter_text",
            "POST",
            {
                "input": json.dumps(
                    {
                        "text": "+1 202-555-0147",
                        "focus": {"x": 500, "y": 360},
                    }
                )
            },
        )
        assert json.loads(entered["output"]) == {"ok": True}
    finally:
        server.stop()


def test_android_fgs_mock_environment_returns_background_calendar_result():
    suite_path, spec = _task_mock_environment(
        "phone_bridge_data_policy_v1.json",
        "android_fgs_calendar_query",
    )
    server = MockEnvironmentServer(spec, suite_path.parent)
    base_url = server.start()
    try:
        calendar = _json_request(
            f"{base_url}/api/tools/bridge_calendar",
            "POST",
            {
                "input": json.dumps(
                    {
                        "action": "query",
                        "from": "2026-07-25T00:00:00+08:00",
                        "to": "2026-07-26T00:00:00+08:00",
                    }
                )
            },
        )
        output = json.loads(calendar["output"])
        assert output["delivery"] == "fgs_background_queue"
        assert output["events"][0]["title"] == "Android FGS 例会"
        state = _json_request(f"{base_url}/api/state")
        assert state["data"]["phone_bridge"]["fgs_bridge_enabled"] is True
    finally:
        server.stop()


def test_mock_environment_rejects_unconfigured_input_when_no_default_matches(tmp_path):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "mock",
                "mock_environment": {
                    "phone_bridge": {"platform": "ios"},
                    "tools": {
                        "bridge_contacts": {
                            "input_contains": {"query": "Biden"},
                            "output": {"ok": True},
                        }
                    },
                },
                "tasks": [
                    {
                        "id": "t1",
                        "category": "diagnostic",
                        "description_for_judge": "test",
                        "prompt": "test",
                        "rubric": [{"id": "done", "check": "done"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )
    suite = load_suite(suite_path)
    assert suite.mock_environment is not None
    server = MockEnvironmentServer(suite.mock_environment, tmp_path)
    base_url = server.start()
    try:
        result = _json_request(
            f"{base_url}/api/tools/bridge_contacts",
            "POST",
            {"input": json.dumps({"query": "Alice"})},
        )
        assert result["is_error"] is True
        assert "no mock response" in result["error"]
    finally:
        server.stop()


def test_mock_environment_activate_switches_task_fixture(tmp_path):
    ios = MockEnvironmentSpec(
        platform="ios",
        phone_bridge={},
        tools={
            "bridge_contacts": [
                MockToolResponseSpec(output={"ok": True, "platform": "ios"})
            ]
        },
        screen_text="iOS fixture",
    )
    android = MockEnvironmentSpec(
        platform="android",
        phone_bridge={},
        tools={
            "bridge_calendar": [
                MockToolResponseSpec(output={"ok": True, "platform": "android"})
            ]
        },
        screen_text="Android fixture",
    )
    server = MockEnvironmentServer(ios, tmp_path)
    base_url = server.start()
    try:
        ios_tools = _json_request(f"{base_url}/api/tools")
        assert [tool["name"] for tool in ios_tools["tools"]] == [
            "bridge_contacts",
        ]

        server.activate(android)

        android_tools = _json_request(f"{base_url}/api/tools")
        assert [tool["name"] for tool in android_tools["tools"]] == [
            "bridge_calendar",
        ]
        state = _json_request(f"{base_url}/api/state")
        assert state["data"]["platform"] == "android"
        assert "platform" not in state["data"]["phone_bridge"]
        assert state["data"]["screen_text"] == "Android fixture"
    finally:
        server.stop()


def test_mock_environment_missing_screen_falls_back_to_placeholder(tmp_path):
    spec = MockEnvironmentSpec(
        platform="ios",
        phone_bridge={},
        tools={},
        screen="missing-screen.jpg",
        screen_text="Fallback screen",
    )
    server = MockEnvironmentServer(spec, tmp_path)

    payload = server.screenshot_payload()

    assert payload["format"] == "jpeg"
    assert payload["data"]
    assert payload["description"] == "Fallback screen"


def test_mock_environment_normalizes_fixture_screen_metadata(tmp_path):
    fixture = tmp_path / "screen.png"
    image = Image.new("RGBA", (320, 640), (12, 34, 56, 128))
    image.save(fixture, format="PNG")
    spec = MockEnvironmentSpec(
        platform="ios",
        phone_bridge={},
        tools={},
        screen=fixture.name,
    )
    server = MockEnvironmentServer(spec, tmp_path)

    payload = server.screenshot_payload()
    raw = base64.b64decode(payload["data"])
    with Image.open(io.BytesIO(raw)) as normalized:
        assert normalized.format == "JPEG"
        assert normalized.size == (320, 640)
    assert payload["format"] == "jpeg"
    assert payload["width"] == 320
    assert payload["height"] == 640


@pytest.mark.parametrize("content_length", [-1, MAX_REQUEST_BODY_BYTES + 1])
def test_mock_environment_rejects_invalid_content_length(tmp_path, content_length):
    spec = MockEnvironmentSpec(platform="ios", phone_bridge={}, tools={})
    server = MockEnvironmentServer(spec, tmp_path)
    base_url = server.start()
    parsed = urllib.parse.urlparse(base_url)
    connection = http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=5)
    try:
        connection.putrequest("POST", f"{parsed.path}/api/providers/screenshot")
        connection.putheader("Content-Length", str(content_length))
        connection.endheaders()
        response = connection.getresponse()
        payload = json.loads(response.read().decode("utf-8"))

        assert response.status == 400
        assert payload["error"]["code"] == "bad_request"
    finally:
        connection.close()
        server.stop()


def test_mock_environment_times_out_incomplete_request_body(tmp_path, monkeypatch):
    monkeypatch.setattr(mock_environment_module, "REQUEST_TIMEOUT_SECONDS", 0.05)
    spec = MockEnvironmentSpec(platform="ios", phone_bridge={}, tools={})
    server = MockEnvironmentServer(spec, tmp_path)
    base_url = server.start()
    parsed = urllib.parse.urlparse(base_url)
    connection = http.client.HTTPConnection(parsed.hostname, parsed.port, timeout=1)
    try:
        connection.putrequest("POST", f"{parsed.path}/api/providers/screenshot")
        connection.putheader("Content-Length", "10")
        connection.endheaders(b"{}")
        response = connection.getresponse()
        payload = json.loads(response.read().decode("utf-8"))

        assert response.status == 400
        assert payload["error"]["code"] == "bad_request"
    finally:
        connection.close()
        server.stop()


def test_phone_bridge_proxy_status_uses_current_fixture_and_native_shape(tmp_path):
    _, spec = _task_mock_environment("phone_bridge_data_policy_v1.json", "ios_pip_contacts_query")
    server = MockEnvironmentServer(spec, tmp_path)
    url = server.start()
    try:
        status = _json_request(f"{url}/api/phone-bridge/status")
        assert status["platform"] == "ios"
        assert status["pip_bridge_enabled"] is True
        assert status["connected"] is False
        assert "data" not in status
        _, android = _task_mock_environment("phone_bridge_data_policy_v1.json", "android_fgs_calendar_query")
        server.activate(android)
        status = _json_request(f"{url}/api/phone-bridge/status")
        assert status["platform"] == "android"
        assert status["fgs_bridge_enabled"] is True
        assert status["pip_bridge_enabled"] is False
        heartbeat = datetime.fromisoformat(status["fgs_bridge_updated_at"])
        assert 0 <= (datetime.now(timezone.utc) - heartbeat).total_seconds() < 15
        assert "fgs_bridge_updated_at" not in android.phone_bridge
        # An explicit stale heartbeat must still exercise the runtime's gate.
        android.phone_bridge["fgs_bridge_updated_at"] = "2000-01-01T00:00:00+00:00"
        server.activate(android)
        status = _json_request(f"{url}/api/phone-bridge/status")
        assert status["fgs_bridge_updated_at"] == "2000-01-01T00:00:00+00:00"
        with pytest.raises(urllib.error.HTTPError) as exc:
            _json_request(url.replace(server.base_path, "") + "/api/phone-bridge/status")
        assert exc.value.code == 404
    finally:
        server.stop()
