import base64
import dataclasses as dc
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import threading
import urllib.request

import pytest

from runner import operations
from runner.experiment import comparison_suite, run_experiment, validate_agents
from runner.harness import Execution
from runner.judge import JudgeConfig
from runner.raw_tools import RAW_TOOL_CATALOG, RawToolBackend
from runner.report import now_iso
from runner.tool_gateway import AUDIT_FILE
from test_experiment import fake_judge
from test_operations import SUITE


FRAME = base64.b64encode(b"frame").decode()


class FakeBridge(BaseHTTPRequestHandler):
    """Environment bridge with a 400x800 screen and the shared /api/tools primitives."""
    calls: list[tuple[str, dict, str]] = []
    fail = False

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])) or b"{}")
        route = self.headers.get("benchmark-task-id", "")
        if self.path == "/api/providers/screenshot":
            payload = {"ok": True, "data": {"image": FRAME, "meta": {"width": 400, "height": 800,
                                                                     "pixel_format": "jpeg"}}}
        else:
            FakeBridge.calls.append((self.path.rsplit("/", 1)[-1], body["input"], route))
            output = json.dumps({"ok": False, "suggestion": "Refocus", "data": "bytes"}) if FakeBridge.fail else "ok"
            payload = {"output": output, "is_error": False}
        data = json.dumps(payload).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_):
        pass


@pytest.fixture
def bridge():
    FakeBridge.calls, FakeBridge.fail = [], False
    server = ThreadingHTTPServer(("127.0.0.1", 0), FakeBridge)
    thread = threading.Thread(target=server.serve_forever)
    thread.start()
    yield f"http://127.0.0.1:{server.server_port}"
    server.shutdown()
    server.server_close()
    thread.join(timeout=3)


def invoke(backend, name, tool_input):
    status, payload = backend.invoke(name, tool_input)
    assert status == 200
    return payload, json.loads(payload["output"])


def test_raw_actions_map_pixels_to_bridge_coordinates_and_return_a_screenshot(bridge):
    backend = RawToolBackend(bridge, "route-1", settle_ms=0)
    payload, frame = invoke(backend, "screenshot", {})
    assert payload["is_error"] is False and frame == {"width": 400, "height": 800, "format": "jpeg", "data": FRAME}
    _, result = invoke(backend, "tap", {"x": 100, "y": 400})
    assert result["ok"] is True and result["data"] == FRAME
    invoke(backend, "swipe", {"start_x": 200, "start_y": 600, "end_x": 200, "end_y": 200})
    invoke(backend, "type_text", {"x": 0, "y": 80, "text": "你好 hello"})
    invoke(backend, "press_key", {"key": "back"})
    assert FakeBridge.calls == [
        ("touch_gesture", {"type": "tap", "point": {"x": 250.0, "y": 500.0}}, "route-1"),
        ("touch_gesture", {"type": "swipe", "start": {"x": 500.0, "y": 750.0}, "end": {"x": 500.0, "y": 250.0},
                           "duration_ms": 300}, "route-1"),
        ("enter_text", {"text": "你好 hello", "focus": {"x": 0.0, "y": 100.0}}, "route-1"),
        ("keyboard_tap", {"keys": ["back"]}, "route-1"),
    ]


def test_raw_tool_descriptions_name_the_device_they_operate():
    # Task prompts may not mention the phone; generic "screen" tools read as host tools.
    assert all("connected mobile device" in tool["description"] for tool in RAW_TOOL_CATALOG)


def test_raw_tools_report_invalid_input_and_business_failures_as_errors(bridge):
    backend = RawToolBackend(bridge, "route", settle_ms=0)
    payload, result = invoke(backend, "tap", {"x": 400, "y": 10})
    assert payload["is_error"] is True and "x must be" in result["error"]
    payload, result = invoke(backend, "press_key", {"key": "volume_up"})
    assert payload["is_error"] is True
    FakeBridge.fail = True
    payload, result = invoke(backend, "tap", {"x": 1, "y": 1})
    assert payload["is_error"] is True and "Refocus" in result["error"] and "bytes" not in result["error"]


def test_raw_layer_serves_external_harness_without_starting_a_daemon(tmp_path, bridge, monkeypatch):
    monkeypatch.setattr(operations, "call_environment_setup", lambda *a, **kw: {"ok": True})
    monkeypatch.setattr(operations, "call_environment_release", lambda *a, **kw: None)
    monkeypatch.setattr(operations, "take_environment_screenshot", lambda url, path, **kw: path.write_bytes(b"x"))

    class Daemon:
        def execute(self, *args, **kwargs):
            pytest.fail("The raw tool layer must not start the Aiden daemon")

    class External:
        def execute(self, prompt, timeout, artifacts, stop, *, tool_endpoint, tool_catalog):
            assert tool_catalog == RAW_TOOL_CATALOG
            request = json.dumps({"input": {"x": 10, "y": 10}}).encode()
            urllib.request.urlopen(urllib.request.Request(f"{tool_endpoint}/api/tools/tap", data=request,
                                                          method="POST"), timeout=10).read()
            return Execution(history=[{"type": "assistant", "content": "Done"}])

    task = comparison_suite(SUITE.with_name("mobilegym_basic.json")).tasks[0]
    adapter = operations.OperationAdapter(daemon=Daemon(), environment_url=bridge, external=External(),
                                          tool_layer="raw")
    result = adapter.execute_operation("task", 10, tmp_path, threading.Event(), task)
    assert result.metrics["tool_layer"] == "raw"
    assert result.metrics["business_tools"] == [tool["name"] for tool in RAW_TOOL_CATALOG]
    audit = json.loads((tmp_path / AUDIT_FILE).read_text().splitlines()[-1])
    assert audit["tool_name"] == "tap" and audit["has_screenshot"] is True
    with pytest.raises(ValueError, match="external"):
        operations.OperationAdapter(daemon=Daemon(), environment_url=bridge, tool_layer="raw")


def test_tool_layers_freeze_separate_catalogs_and_raw_skips_aiden_tool_name_assertions(tmp_path):
    suite = comparison_suite(SUITE.with_name("mobilegym_basic.json"))
    task = next(t for t in suite.tasks if t.hard_assertions.required_tool_calls)
    suite = dc.replace(suite, tasks=[task])

    class Agent:
        def __init__(self, layer):
            self.layer = layer

        def execute_operation(self, prompt, timeout, artifacts, stop, task):
            name = "swipe" if self.layer == "raw" else "touch_gesture"
            return Execution(history=[
                {"type": "tool_call", "tool_name": name, "tool_input": '{"type":"swipe"}'},
                {"type": "tool_result", "content": "ok"},
                {"type": "assistant", "content": "Done"},
            ], metrics={"task_wall_ms": 10, "business_tools_sha256": self.layer, "tool_layer": self.layer,
                        "business_tools": [name]})

    agents = [{"id": "pi", "kind": "pi", "tool_layer": "aiden"}, {"id": "pi-raw", "kind": "pi", "tool_layer": "raw"}]
    record = {"id": "exp", "created_at": now_iso()}
    run_experiment(suite, tmp_path, agents, {"pi": Agent("aiden"), "pi-raw": Agent("raw")},
                   1, 0, threading.Event(), record, JudgeConfig())
    assert set(record["business_tool_catalogs"]) == {"aiden", "raw"}
    rows = {agent["id"]: json.loads((tmp_path / "runs" / agent["id"] / "results.jsonl").read_text())
            for agent in agents}
    assert "tool_name_assertions" not in rows["pi"]["metrics"]
    assert rows["pi-raw"]["metrics"]["tool_name_assertions"] == "not_applicable"
    assert not rows["pi-raw"]["hard_assertion_failures"]
    assert rows["pi-raw"]["metrics"]["business_tool_calls"] == 1
    # The overview labels each agent's tool layer.
    report = (tmp_path / "report.html").read_text()
    assert "report.html'>pi-raw</a>" in report and "<td>raw</td>" in report


def test_tool_layer_is_validated_per_agent():
    assert validate_agents([{"kind": "aiden"}])[0]["tool_layer"] == "aiden"
    agent = validate_agents([{"kind": "pi", "provider": "p", "model": "m", "tool_layer": "raw"}])[0]
    assert agent["tool_layer"] == "raw"
    with pytest.raises(ValueError, match="Aiden uses its own tool layer"):
        validate_agents([{"kind": "aiden", "tool_layer": "raw"}])
    with pytest.raises(ValueError, match="tool_layer must be one of"):
        validate_agents([{"kind": "pi", "provider": "p", "model": "m", "tool_layer": "hybrid"}])
