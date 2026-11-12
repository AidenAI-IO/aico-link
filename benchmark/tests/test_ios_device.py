from __future__ import annotations

import base64
import asyncio
import io
import json
import urllib.error
import urllib.request
from contextlib import AsyncExitStack

import pytest
from PIL import Image

from ios_device.device import IOSDevice
from ios_device.permissions import PermissionPolicy, deduplicate_alert_buttons
from ios_device.server import IOSServer, execute_ios_mnk
from ios_device.transport import IOSDeviceError, USBTransport


class FakeWDA:
    def __init__(self):
        self.calls = []
        self.alert = None
        self.locked = False
        self.fail_selection = False
        self.fail_text = False
        self.fail_home = False

    def request(self, method, path, payload=None):
        self.calls.append((method, path, payload))
        if path == "/session":
            return {"value": {"sessionId": "test-session"}}
        if path.endswith("/wda/locked"):
            return {"value": self.locked}
        if path.endswith("/window/size"):
            return {"value": {"width": 390, "height": 844}}
        if path.endswith("/element/active"):
            return {"value": {"element-6066-11e4-a52e-4f735466cecf": "focused-field"}}
        if path.endswith("/wda/keys") and self.fail_text:
            raise IOSDeviceError("device_timeout", "delivery is ambiguous")
        if path == "/session/test-session/wda/pressButton":
            if self.fail_home:
                raise IOSDeviceError("device_timeout", "delivery is ambiguous")
            return {"value": None}
        if path.endswith("/alert/text") or path.endswith("/wda/alert/buttons"):
            if self.alert is None:
                raise IOSDeviceError("no_alert", "No alert is open")
            return {"value": self.alert["text"] if path.endswith("/alert/text") else self.alert["buttons"]}
        if path.endswith("/alert/accept"):
            if self.fail_selection:
                raise IOSDeviceError("device_timeout", "delivery is ambiguous")
            self.alert = None
        if path.endswith("/screenshot"):
            output = io.BytesIO()
            Image.new("RGB", (117, 253)).save(output, format="PNG")
            return {"value": base64.b64encode(output.getvalue()).decode()}
        if path == "/status" or any(path.endswith(route) for route in (
                "/wda/tap", "/wda/doubleTap", "/wda/touchAndHold", "/wda/dragfromtoforduration",
                "/wda/keys", "/clear", "/alert/accept", "/actions", "/source")):
            return {"value": {}}
        raise AssertionError(f"Unsupported WDA route: {method} {path}")


@pytest.fixture
def device():
    return IOSDevice(FakeWDA())


def signature():
    return {"text_contains": "Permission Probe", "buttons": ["Don't Allow", "Allow"], "button": "Allow"}


def test_native_coordinates_text_and_partial_delivery(device):
    device.click(250, 500)
    assert device.transport.calls[-1] == ("POST", "/session/test-session/wda/tap", {"x": 97.5, "y": 422.0})
    device.insert_text("中文🔋", replace=True)
    assert [path.rsplit("/", 1)[-1] for _, path, _ in device.transport.calls[-2:]] == ["clear", "keys"]
    assert device.transport.calls[-1][2] == {"value": ["中文🔋"]}
    device.transport.fail_text = True
    before = len(device.transport.calls)
    with pytest.raises(IOSDeviceError, match="ambiguous"):
        device.insert_text("X")
    assert sum(path.endswith("/wda/keys") for _, path, _ in device.transport.calls[before:]) == 1
    device.transport.locked = True
    with pytest.raises(IOSDeviceError, match="Unlock"):
        device.click(500, 500)
    assert device.check_device()["input_available"] is False


@pytest.mark.parametrize("payload", [
    {"operation": "insert_text", "insert_text": {"text": "\u0000"}},
    {"operation": "insert_text", "insert_text": {"text": "\ud800"}},
    {"operation": "insert_text", "insert_text": {"text": "🔋" * 2049}},
    {"operation": "insert_text", "insert_text": {"text": "hello", "replace": "true"}},
    {"operation": "system_action"},
    {"operation": "system_action", "system_action": {"action": "power"}},
    {"operation": "click", "click": {"x": 1001, "y": 500}},
    {"operation": "touch_actions", "touch_actions": [{"action": "touch_down", "point": {"x": 500, "y": 500}}]},
    {"operation": "touch_actions", "touch_actions": [{"action": "move_to", "point": {"x": 500, "y": 500}}]},
    {"operation": "touch_actions", "touch_actions": [{"action": "wait", "ms": 10001}]},
])
def test_invalid_requests_do_not_reach_the_device(payload):
    def unexpected(*_args):
        pytest.fail("invalid input reached WDA")
    assert execute_ios_mnk(payload, unexpected)[0] == 400


def test_policy_requires_an_unambiguous_prompt_and_complete_button_set(device):
    rule = signature()
    device.transport.alert = {"text": '"Permission Probe" Would Like to Send Notifications', "buttons": ["Don't Allow", "Allow"]}
    alert = device.read_alert()
    assert PermissionPolicy([rule]).choose(alert) == "Allow"
    assert PermissionPolicy([rule, rule]).choose(alert) is None
    assert PermissionPolicy([{**rule, "text_contains": "Different App"}]).choose(alert) is None
    assert PermissionPolicy([{**rule, "buttons": ["Allow Once", "Allow"]}]).choose(alert) is None
    # WDA exposes the location precision toggle twice; keep its multiplicity.
    device.transport.alert["buttons"] = ["Precise", "Precise", "Allow", "Don't Allow"]
    location_alert = device.read_alert()
    assert PermissionPolicy([{**rule, "buttons": location_alert["buttons"]}]).choose(location_alert) == "Allow"
    assert PermissionPolicy([{**rule, "buttons": ["Precise", "Allow", "Don't Allow"]}]).choose(location_alert) is None
    for invalid in [{**rule, "text_contains": ""}, {**rule, "button": "Missing"}, {**rule, "buttons": ["Allow", "Allow"]}]:
        with pytest.raises(ValueError):
            PermissionPolicy([invalid])


def test_atomic_touch_keeps_one_contact_and_validates_before_delivery(device):
    actions = [{"action": "touch_down", "point": {"x": 500, "y": 1000}},
               {"action": "move_to", "point": {"x": 500, "y": 500}, "ms": 350},
               {"action": "wait", "ms": 250}, {"action": "touch_up"}]
    status, _ = execute_ios_mnk({"operation": "touch_actions", "touch_actions": actions},
                              lambda name, params: (device.touch_actions(params["actions"]) or {}))
    assert status == 200
    method, path, payload = device.transport.calls[-1]
    assert method == "POST" and path.endswith("/actions")
    program = payload["actions"][0]["actions"]
    assert [step["type"] for step in program] == ["pointerMove", "pointerDown", "pointerMove", "pause", "pointerUp"]
    assert program[0]["y"] == 843 and program[2]["duration"] == 350
    device.drag((500, 1000), (500, 200), duration=0.35)
    program = device.transport.calls[-1][2]["actions"][0]["actions"]
    assert program[2]["type"] == "pointerMove" and program[2]["duration"] == 350
    assert not any(step["type"] == "pause" for step in program)


def test_alert_aliases_require_matching_identity_and_geometry():
    source = '''<root><XCUIElementTypeAlert label="Camera?">
      <XCUIElementTypeButton label="Deny" name="deny" x="10" y="20" width="40" height="30">
        <XCUIElementTypeButton label="Deny" name="deny" x="10" y="20" width="40" height="30"/>
      </XCUIElementTypeButton><XCUIElementTypeButton label="Allow" name="allow" x="60" y="20" width="40" height="30"/>
    </XCUIElementTypeAlert></root>'''
    buttons = ["Deny", "Deny", "Allow"]
    assert deduplicate_alert_buttons("Camera?\nPurpose", buttons, source) == ["Deny", "Allow"]
    assert deduplicate_alert_buttons("Different prompt", buttons, source) == buttons
    assert deduplicate_alert_buttons("Camera?", buttons, source.replace('name="deny" x="10"', 'name="other" x="11"', 1)) == buttons


@pytest.mark.parametrize("message", ["No alert is open", "no such alert", "An attempt was made to operate on a modal dialog when one was not open"])
def test_transport_maps_absent_alert_without_hiding_other_wda_errors(message):
    class Client:
        async def _request_json(self, *_args):
            raise RuntimeError(message)
    transport = object.__new__(USBTransport)
    transport.client, transport.runner, transport.timeout = Client(), None, 1
    with pytest.raises(IOSDeviceError) as error:
        asyncio.run(transport._request("GET", "/session/test/alert/text", None))
    assert error.value.code == "no_alert"
    with pytest.raises(IOSDeviceError) as error:
        asyncio.run(transport._request("POST", "/session/test/wda/keys", {}))
    assert error.value.code == "wda_request_failed"


def test_shutdown_releases_stack_and_drains_a_slow_runner():
    async def scenario():
        release = asyncio.Event()
        events = []

        async def runner():
            try:
                await asyncio.Event().wait()
            finally:
                await release.wait()
                events.append("runner_closed")

        async def close_stack():
            events.append("stack_closed")
            release.set()

        transport = object.__new__(USBTransport)
        transport.stack = AsyncExitStack()
        transport.stack.push_async_callback(close_stack)
        transport.runner = asyncio.create_task(runner())
        await asyncio.sleep(0)
        # _close owns its event loop and drains every other task on that loop.
        await transport._close()
        assert transport.runner.done()
        assert events == ["stack_closed", "runner_closed"]

    asyncio.run(scenario())


def test_auto_policy_verifies_selection_and_does_not_replay_a_timeout(device):
    device.policy = PermissionPolicy([signature()])
    device.transport.alert = {"text": '"Permission Probe" Would Like to Send Notifications', "buttons": ["Don't Allow", "Allow"]}
    device.transport.fail_selection = True
    with pytest.raises(IOSDeviceError, match="ambiguous"):
        device.screenshot_jpeg()
    frame, width, height = device.screenshot_jpeg()
    assert frame.startswith(b"\xff\xd8") and (width, height) == (117, 253)
    assert sum(path.endswith("/alert/accept") for _, path, _ in device.transport.calls) == 1
    # A newly observed interval without a prompt permits a subsequent fresh request.
    device.transport.alert = None
    assert device.read_alert() == {"present": False}
    device.transport.alert = {"text": '"Permission Probe" Would Like to Send Notifications', "buttons": ["Don't Allow", "Allow"]}
    device.transport.fail_selection = False
    result = device.apply_permission_policy()
    assert result["dismissed"] and result["selected"] == "Allow"
    assert device.read_alert() == {"present": False}


def test_http_alert_selection_obeys_task_lease_and_rejects_stale_prompts(device):
    bridge = IOSServer(device, port=0, action_settle_sec=0)
    bridge.start()

    def request(path, payload=None, task="test"):
        req = urllib.request.Request(bridge.base_url + path,
            data=None if payload is None else json.dumps(payload).encode(),
            headers={"benchmark-task-id": task, "Content-Type": "application/json"})
        try:
            with urllib.request.urlopen(req, timeout=3) as response:
                return response.status, json.load(response)
        except urllib.error.HTTPError as exc:
            return exc.code, json.load(exc)

    try:
        assert request("/api/setup", {})[0] == 200
        assert request("/api/ios/alert", task="other")[0] == 429
        device.transport.alert = {"text": "Allow camera?", "buttons": ["Don't Allow", "Allow"]}
        status, response = request("/api/ios/alert")
        assert status == 200
        selection = {"alert_id": response["data"]["alert_id"], "button": "Allow"}
        assert request("/api/ios/alert", selection, task="other")[0] == 429
        device.transport.alert["text"] = "Allow location?"
        assert request("/api/ios/alert", selection)[0] == 409
        assert not any(path.endswith("/alert/accept") for _, path, _ in device.transport.calls)
        selection["alert_id"] = request("/api/ios/alert")[1]["data"]["alert_id"]
        assert request("/api/ios/alert", selection)[1]["data"]["dismissed"]
        frame = request("/api/providers/screenshot", {})[1]["data"]
        assert (frame["meta"]["width"], frame["meta"]["height"]) == (117, 253)
        assert request("/api/providers/mnk")[1]["capabilities"]["insert_text"]
        assert request("/api/providers/mnk")[1]["capabilities"]["system_actions"] == ["home"]
        home = {"operation": "system_action", "system_action": {"action": "home"}}
        assert request("/api/providers/mnk", home, task="other")[0] == 429
        assert request("/api/providers/mnk", home)[1]["success"]
        assert device.transport.calls[-1] == ("POST", "/session/test-session/wda/pressButton", {"name": "home"})
        before = len(device.transport.calls)
        device.transport.fail_home = True
        assert request("/api/providers/mnk", home)[0] == 500
        sent = [(method, path) for method, path, _ in device.transport.calls[before:] if method == "POST"]
        assert sent == [("POST", "/session/test-session/wda/pressButton")]
    finally:
        bridge.stop()
