import io
from pathlib import Path

from PIL import Image

from runner.suite import load_suite
from vphone.scripts.validate_bridge import validate_bridge

from tests.test_vphone_bridge import FakeVPhoneDevice
from vphone.bridge.server import VPhoneBridgeServer


def test_vphone_ios_basic_suite_loads():
    suite = load_suite(Path(__file__).parents[1] / "suites" / "vphone_ios_basic.json")
    assert suite.name == "vphone_ios_basic"
    # The first task is a warmup that absorbs the daemon cold-start
    # "agent not ready" skip, so the seven scored tasks run once the daemon
    # is ready. It is a diagnostic task with an empty rubric.
    assert [task.id for task in suite.tasks] == [
        "warmup",
        "screenshot_home",
        "go_home",
        "open_settings",
        "swipe_screen",
        "clock_count_alarms",
        "settings_read_ethernet_ipv4",
        "open_app_library",
    ]
    warmup = suite.tasks[0]
    assert warmup.category == "diagnostic"
    assert warmup.hard_assertions.required_tools == ["screenshot"]
    assert all(task.repeats == 1 for task in suite.tasks)

    ethernet = next(task for task in suite.tasks if task.id == "settings_read_ethernet_ipv4")
    # This task has no min_tool_calls floor: the agent can reach the Ethernet
    # detail page and screenshot it in as few as 2 calls, and the number of
    # calls drifts with VM state, so a floor randomly fails correct runs.
    # Correctness is enforced by required_tools + the judge/rubric instead.
    assert ethernet.hard_assertions.min_tool_calls == 0
    assert ethernet.hard_assertions.required_tools == ["screenshot"]
    assert {item.id for item in ethernet.rubric} >= {
        "opened_ethernet_interface",
        "captured_ipv4_details",
        "reported_ipv4_details",
        "did_not_modify",
    }


def test_live_validation_flow_matches_runner_contract(tmp_path):
    class ProbeDevice(FakeVPhoneDevice):
        def screenshot_jpeg(self):
            self.calls.append(("screenshot_jpeg",))
            output = io.BytesIO()
            Image.new("RGB", (72, 156), color=(25, 90, 160)).save(output, format="JPEG")
            return output.getvalue(), 72, 156, self.width, self.height

    server = VPhoneBridgeServer(ProbeDevice(), port=0, action_settle_sec=0)
    endpoint = server.start()
    screenshot = tmp_path / "probe.jpg"
    try:
        result = validate_bridge(endpoint, screenshot_path=screenshot)
    finally:
        server.stop()
    assert result["ok"] is True
    assert result["checks"] == [
        "health",
        "concurrent=1",
        "screenshot-provider",
        "setup-home",
        "ownership-429",
        "tool-catalog",
        "touch-gesture",
        "release",
    ]
    with Image.open(screenshot) as image:
        assert image.format == "JPEG"
        assert image.mode == "RGB"
        assert image.size == (72, 156)
