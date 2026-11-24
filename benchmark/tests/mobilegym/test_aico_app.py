import asyncio
import base64
import json
from types import SimpleNamespace
import os
import socket
import struct
import threading
import urllib.parse

import pytest

from mobilegym.bridge.aico_app import (
    AicoAppSession,
    BACKGROUND_COMMAND_TYPES,
    build_operation,
    CommandError,
    parse_aico_app_config,
)
from mobilegym.bridge.input import SimulatorInput
from mobilegym.bridge.relay_websocket import OP_CLOSE, OP_TEXT, accept_key
from tests.mobilegym.test_bridge_server import RunningBridge, request_json


def session(**config):
    return AicoAppSession(parse_aico_app_config(config))


def run_ok(operation):
    return {"data": {"echo": operation["type"]}}


@pytest.mark.parametrize(
    ("config", "command_type", "expected_code"),
    [
        ({"connected": False}, "clipboard_read", "bridge_not_connected"),
        ({"app_state": "background"}, "open_app", "app_backgrounded"),
        ({"permissions": {"contacts": "denied"}}, "contacts_query", "permission_denied"),
        ({"faults": [{"command": "calendar_query", "error": "bridge_timeout"}]}, "calendar_query", "bridge_timeout"),
        ({}, "sms_send", "invalid_arguments"),
    ],
)
def test_session_rejects_commands_the_app_cannot_run(config, command_type, expected_code):
    app = session(**config)
    calls = []

    response = app.execute(
        {"id": "c1", "type": command_type, "app": "Clock", "payload": {"from": "2026-06-02T00:00:00+08:00", "to": "2026-06-03T00:00:00+08:00"}},
        lambda op: calls.append(op) or {},
    )

    assert response["error"]["code"] == expected_code
    assert calls == []
    failed = app.snapshot()["failed_command_counts"]
    assert failed[command_type] == 1
    assert sum(failed.values()) == 1


def test_background_app_still_runs_data_commands():
    app = session(app_state="background", fgs_bridge_enabled=True)

    assert app.execute({"id": "c1", "type": "clipboard_read"}, run_ok) == {
        "id": "c1",
        "data": {"echo": "clipboard_read"},
    }
    status = app.status()
    assert status["app_state"] == "background"
    assert status["fgs_bridge_enabled"] is True
    assert "fgs_bridge_updated_at" in status
    assert "open_app" not in BACKGROUND_COMMAND_TYPES


def test_faults_are_consumed_and_disconnect_drops_the_connection():
    app = session(faults=[
        {"command": "*", "error": "app_launch_failed"},
        {"command": "clipboard_read", "error": "disconnect"},
    ])

    first = app.execute({"id": "c1", "type": "clipboard_write", "payload": {"text": "a"}}, run_ok)
    second = app.execute({"id": "c2", "type": "clipboard_write", "payload": {"text": "a"}}, run_ok)
    third = app.execute({"id": "c3", "type": "clipboard_read"}, run_ok)
    fourth = app.execute({"id": "c4", "type": "clipboard_read"}, run_ok)

    assert first["error"]["code"] == "app_launch_failed"
    assert "error" not in second
    assert third["error"]["code"] == "bridge_connection_closed"
    assert fourth["error"]["code"] == "bridge_not_connected"
    assert app.status() == {
        "connected": False,
        "platform": "android",
        "device_type": "Android",
        "phone_id": "mobilegym-aico-app",
    }


def test_duplicate_command_id_returns_the_cached_result_without_rerunning():
    app = session()
    calls = []

    def run(operation):
        calls.append(operation)
        return {"method": "clipboard_write"}

    command = {"id": "c1", "type": "clipboard_write", "payload": {"text": "hello"}}
    assert app.execute(command, run) == app.execute(command, run) == {"id": "c1", "method": "clipboard_write"}
    assert len(calls) == 1


def test_concurrent_duplicate_command_ids_execute_once():
    app = session()
    started = threading.Event()
    release = threading.Event()
    calls = []

    def run(operation):
        calls.append(operation)
        started.set()
        assert release.wait(timeout=2)
        return {"data": {"contact_id": "c_new"}}

    command = {"id": "c1", "type": "contacts_create", "payload": {"name": "Morgan Lee"}}
    responses = []
    first = threading.Thread(target=lambda: responses.append(app.execute(command, run)))
    first.start()
    assert started.wait(timeout=2)
    second = threading.Thread(target=lambda: responses.append(app.execute(command, run)))
    second.start()
    release.set()
    first.join(timeout=2)
    second.join(timeout=2)

    assert len(calls) == 1
    assert responses == [{"id": "c1", "data": {"contact_id": "c_new"}}] * 2


def test_page_errors_and_timeouts_map_to_tool_errors():
    app = session()

    def timeout(_operation):
        raise TimeoutError("slow page")

    assert app.execute({"id": "c1", "type": "clipboard_read"}, timeout)["error"] == {
        "category": "transient",
        "code": "bridge_timeout",
        "message": "Aico App command timed out",
    }
    not_found = app.execute(
        {"id": "c2", "type": "calendar_delete", "payload": {"event_id": "missing"}},
        lambda _op: {"error": {"code": "invalid_arguments", "message": "Calendar event not found", "details": {"native_code": "not_found"}}},
    )
    assert not_found["error"]["details"] == {"native_code": "not_found"}


def test_scheduled_notification_is_recorded_instead_of_posted():
    app = session()
    command = {
        "id": "n1",
        "type": "notification_send",
        "payload": {"title": "Take medicine", "schedule_at": "2026-06-04T18:00:00+08:00"},
    }

    response = app.execute(command, lambda _op: {"data": {"notification_id": "n_aico_1"}, "scheduled": True})

    assert response == {"id": "n1", "data": {"notification_id": "n_aico_1"}}
    assert app.snapshot()["scheduled_notifications"] == [{
        "notification_id": "n_aico_1",
        "title": "Take medicine",
        "body": None,
        "schedule_at": "2026-06-04T18:00:00+08:00",
        "schedule_ts": 1780567200000,
    }]


@pytest.mark.parametrize(
    ("raw", "message"),
    [
        ([], "must be an object"),
        ({"platform": "ios"}, "unsupported keys"),
        ({"app_state": "inactive"}, "app_state"),
        ({"permissions": {"camera": "denied"}}, "unknown permission"),
        ({"faults": [{"command": "open_app", "error": "permission_denied"}]}, "error must be one of"),
        ({"faults": [{"command": "open_app", "error": "disconnect", "count": 0}]}, "positive integer"),
    ],
)
def test_config_rejects_invalid_values(raw, message):
    with pytest.raises(ValueError, match=message):
        parse_aico_app_config(raw)


@pytest.mark.parametrize(
    ("command", "expected"),
    [
        (
            {"type": "calendar_create", "payload": {"title": "Dentist", "start_at": "2026-06-02T15:00:00+08:00", "end_at": "2026-06-02T16:00:00Z"}},
            {"type": "calendar_create", "title": "Dentist", "start_ts": 1780383600000, "end_ts": 1780416000000, "all_day": False, "location": None, "notes": None},
        ),
        ({"type": "open_app", "url": "tel:10086"}, {"type": "open_app", "url": "tel:10086", "scheme": "tel"}),
        ({"type": "open_app", "payload": {"phone_number": "10086"}}, {"type": "open_app", "url": "tel:10086", "scheme": "tel"}),
        ({"type": "contacts_query", "payload": {"limit": 999}}, {"type": "contacts_query", "query": "", "limit": 200}),
        # The Agent's bridge_contacts tool sends unchanged fields as "" or null.
        (
            {"type": "contacts_update", "payload": {"contact_id": "c_lisi", "name": "", "phone_numbers": None, "emails": None, "organization": "Aiden Robotics", "notes": ""}},
            {"type": "contacts_update", "contact_id": "c_lisi", "organization": "Aiden Robotics"},
        ),
    ],
)
def test_build_operation_normalizes_arguments(command, expected):
    assert build_operation(command) == expected


@pytest.mark.parametrize(
    "command",
    [
        {"type": "calendar_create", "payload": {"title": "x", "start_at": "2026-06-02T15:00:00", "end_at": "2026-06-02T16:00:00"}},
        {"type": "calendar_query", "payload": {"from": "2026-06-03T00:00:00Z", "to": "2026-06-02T00:00:00Z"}},
        {"type": "open_app", "app": "Clock", "url": "https://example.com"},
        {"type": "open_app", "url": "ftp://example.com"},
        {"type": "contacts_update", "payload": {"name": "x"}},
    ],
)
def test_build_operation_rejects_invalid_arguments(command):
    with pytest.raises(CommandError) as exc:
        build_operation(command)
    assert exc.value.code == "invalid_arguments"


class FakePage:
    def __init__(self):
        self.operations = []

    async def evaluate(self, _script, operation):
        self.operations.append(operation)
        if "type" not in operation:
            return None
        if operation["type"] == "clipboard_read":
            return {"data": {"text": "copied"}}
        return {"method": operation["type"]}


class AicoBridge(RunningBridge):
    def __enter__(self):
        super().__enter__()
        self.env.page = FakePage()
        self.env.APP_NAME_MAP = {"Clock": "clock"}
        return self


def websocket_exchange(base_url, message, headers=None):
    parsed = urllib.parse.urlparse(base_url)
    with socket.create_connection((parsed.hostname, parsed.port), timeout=2) as conn:
        key = base64.b64encode(os.urandom(16)).decode("ascii")
        extra = "".join(f"{name}: {value}\r\n" for name, value in (headers or {}).items())
        conn.sendall(
            (
                "GET /api/phone-bridge?benchmark_relay=1 HTTP/1.1\r\n"
                f"Host: {parsed.netloc}\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n"
                f"Sec-WebSocket-Key: {key}\r\nSec-WebSocket-Version: 13\r\n{extra}\r\n"
            ).encode("ascii")
        )
        stream = conn.makefile("rb")
        status_line = stream.readline()
        response_headers = {}
        while (line := stream.readline()) not in (b"\r\n", b""):
            name, value = line.decode("ascii").split(":", 1)
            response_headers[name.strip().lower()] = value.strip()
        assert status_line.startswith(b"HTTP/1.1 101")
        assert response_headers["sec-websocket-accept"] == accept_key(key)

        def send(opcode, payload):
            mask = os.urandom(4)
            masked = bytes(byte ^ mask[index % 4] for index, byte in enumerate(payload))
            conn.sendall(bytes([0x80 | opcode, 0x80 | len(payload)]) + mask + masked)

        def receive():
            first, second = stream.read(2)
            length = second & 0x7F
            if length == 126:
                (length,) = struct.unpack("!H", stream.read(2))
            return first & 0x0F, stream.read(length)

        send(OP_TEXT, json.dumps(message).encode("utf-8"))
        opcode, payload = receive()
        assert opcode == OP_TEXT
        send(OP_CLOSE, struct.pack("!H", 1000))
        assert receive()[0] == OP_CLOSE
        return json.loads(payload)


def test_bridge_serves_the_phone_bridge_protocol_for_a_configured_task():
    with AicoBridge() as bridge:
        assert request_json(bridge.base_url, "GET", "/api/phone-bridge/status") == (200, {"connected": False})
        assert websocket_exchange(bridge.base_url, {"id": "c0", "type": "clipboard_read"})["error"]["code"] == (
            "bridge_not_connected"
        )

        status, _ = request_json(bridge.base_url, "POST", "/api/setup", {"aico_app": {"fgs_bridge_enabled": True}})
        assert status == 200
        status, body = request_json(bridge.base_url, "GET", "/api/phone-bridge/status")
        assert (status, body["connected"], body["app_state"]) == (200, True, "active")

        assert websocket_exchange(bridge.base_url, {"id": "c1", "type": "clipboard_read", "timeout_ms": 5000}) == {
            "id": "c1",
            "data": {"text": "copied"},
        }
        assert bridge.env.page.operations[-1]["app_names"] == {"clock": "clock"}

        command = {"id": "q1", "type": "clipboard_write", "payload": {"text": "hi"}}
        status, body = request_json(bridge.base_url, "POST", "/api/phone-bridge/commands", {"command": command})
        assert (status, body["command_id"]) == (202, "q1")
        assert request_json(bridge.base_url, "POST", "/api/phone-bridge/commands", {"command": command})[0] == 409
        assert request_json(bridge.base_url, "GET", "/api/phone-bridge/results/q1") == (
            200,
            {"command_id": "q1", "status": "completed", "result": {"id": "q1", "method": "clipboard_write"}},
        )
        assert request_json(bridge.base_url, "DELETE", "/api/phone-bridge/commands/q1")[0] == 200
        assert request_json(bridge.base_url, "GET", "/api/phone-bridge/results/q1")[0] == 404

        status, body = request_json(bridge.base_url, "POST", "/state", {})
        counts = body["data"]["aico_app"]["command_counts"]
        assert (counts["clipboard_read"], counts["clipboard_write"], counts["open_app"]) == (1, 1, 0)

        assert request_json(bridge.base_url, "POST", "/api/setup", {})[0] == 200
        assert request_json(bridge.base_url, "GET", "/api/phone-bridge/status") == (200, {"connected": False})
        status, body = request_json(bridge.base_url, "POST", "/state", {})
        assert "aico_app" not in body["data"]


def test_setup_seeds_initial_state_after_reset():
    patch = {"apps": {"calendar": {"events": [{"id": "evt_seed", "title": "Standup"}]}}}
    with AicoBridge() as bridge:
        status, _ = request_json(bridge.base_url, "POST", "/api/setup", {"initial_state": patch})
        assert request_json(bridge.base_url, "POST", "/api/setup", {"initial_state": {"apps": []}})[0] == 400

    assert status == 200
    assert bridge.env.reset_calls == 1
    assert bridge.env.page.operations == [patch]


def test_setup_rejects_an_invalid_aico_app_config():
    with AicoBridge() as bridge:
        status, body = request_json(bridge.base_url, "POST", "/api/setup", {"aico_app": {"app_state": "gone"}})

    assert status == 400
    assert "app_state" in body["error"]["message"]


@pytest.mark.parametrize(
    ("keys", "clipboard", "inserted", "pressed"),
    [
        (["ctrl", "v"], "王小明", ["王小明"], []),
        (["meta", "v"], "王小明", ["王小明"], []),
        (["ctrl", "v"], "", [], []),
        (["ctrl", "a"], "王小明", [], ["Control+a"]),
    ],
)
def test_keyboard_paste_reads_the_simulator_clipboard(keys, clipboard, inserted, pressed):
    # The Aico App writes MobileGym's clipboard; the browser's native paste
    # would read the host clipboard instead.
    calls = {"inserted": [], "pressed": []}

    async def evaluate(_script):
        return clipboard

    async def insert_text(text):
        calls["inserted"].append(text)

    async def press(chord):
        calls["pressed"].append(chord)

    page = SimpleNamespace(evaluate=evaluate, keyboard=SimpleNamespace(insert_text=insert_text, press=press))
    asyncio.run(SimulatorInput(SimpleNamespace(page=page)).keypress(keys))

    assert calls == {"inserted": inserted, "pressed": pressed}
