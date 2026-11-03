import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
import os
import struct
from pathlib import Path
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request

import pytest

from runner.harnesses import openclaw_runtime, validate_agents
from runner.harness import OpenClawAdapter, _gateway_execution, _gateway_history, refresh_gateway_evidence
from runner.tool_gateway import AUDIT_FILE, DaemonToolBackend, ToolGateway
from runner.trace import extract_trace


def test_openclaw_requires_explicit_model_endpoint_and_supported_node(monkeypatch, tmp_path):
    with pytest.raises(ValueError, match="base_url"):
        validate_agents([{"kind": "openclaw", "provider": "benchmark-custom", "model": "vision"}])
    agent = validate_agents([{"kind": "openclaw", "provider": "benchmark-custom", "model": "vision",
                              "base_url": "https://example.test/v1"}])[0]
    assert agent["api"] == "openai-completions"
    assert agent["api_key_env"] == "AIDEN_BENCHMARK_PI_API_KEY"
    node = tmp_path / "node"
    node.write_text("#!/bin/sh\necho v22.0.0\n")
    node.chmod(0o755)
    monkeypatch.setenv("AIDEN_BENCHMARK_OPENCLAW_NODE", str(node))
    monkeypatch.setenv("AIDEN_BENCHMARK_OPENCLAW_EXECUTABLE", str(node))
    with pytest.raises(ValueError, match="Node 24"):
        openclaw_runtime()
    node.write_text("#!/bin/sh\necho unknown\n")
    with pytest.raises(ValueError, match="found unknown"):
        openclaw_runtime()


class FakeDaemon(BaseHTTPRequestHandler):
    """Daemon tool API that also exposes a tool outside the shared catalog."""
    calls: list[str] = []

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        FakeDaemon.calls.append(self.path)
        assert body == {"input": {"source": "phone"}}
        output = {"width": 20, "height": 30, "format": "jpeg", "data": IMAGE,
                  "action_output": json.dumps({"ok": False, "suggestion": "Retry"})}
        data = json.dumps({"output": json.dumps(output), "raw_input": '{"source":"phone"}',
                           "is_error": False, "duration_ms": 15}).encode()
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(data)))
        self.end_headers()
        self.wfile.write(data)

    def log_message(self, *_):
        pass


IMAGE = base64.b64encode(b"image bytes").decode()
CATALOG = [{"name": "screenshot", "description": "Capture",
            "args_schema": {"type": "object", "properties": {"source": {"type": "string"}}}}]


@pytest.fixture
def daemon():
    FakeDaemon.calls = []
    server = ThreadingHTTPServer(("127.0.0.1", 0), FakeDaemon)
    thread = threading.Thread(target=server.serve_forever)
    thread.start()
    yield f"http://127.0.0.1:{server.server_port}"
    server.shutdown()
    server.server_close()
    thread.join(timeout=3)


def test_gateway_forwards_only_catalog_tools_and_audits_business_failures(tmp_path, daemon):
    with ToolGateway(DaemonToolBackend(daemon), CATALOG, tmp_path) as gateway:
        listed = json.load(urllib.request.urlopen(f"{gateway.url}/api/tools"))
        assert listed == {"tools": CATALOG}
        with pytest.raises(urllib.error.HTTPError) as rejected:
            urllib.request.urlopen(urllib.request.Request(
                f"{gateway.url}/api/tools/shell", data=b'{"input":{}}', method="POST"))
        assert rejected.value.code == 404
        response = json.load(urllib.request.urlopen(urllib.request.Request(
            f"{gateway.url}/api/tools/screenshot", data=json.dumps({"input": {"source": "phone"}}).encode(),
            headers={"Content-Type": "application/json"}, method="POST")))
    assert FakeDaemon.calls == ["/api/tools/screenshot"]
    assert response["is_error"] is True
    started, audit = map(json.loads, (tmp_path / AUDIT_FILE).read_text().splitlines())
    assert started["type"] == "tool_call" and started["call_id"] == audit["call_id"]
    assert audit["is_error"] is True and audit["screenshot_file"] == "step_1.jpg"
    assert (tmp_path / "step_1.jpg").read_bytes() == b"image bytes"


def test_mcp_bridge_preserves_images_errors_and_shared_schema(tmp_path, daemon):
    config = tmp_path / "mcp.json"
    with ToolGateway(DaemonToolBackend(daemon), CATALOG, tmp_path) as gateway:
        config.write_text(json.dumps({"endpoint": gateway.url, "tools": CATALOG}))
        requests = [
            {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2024-11-05"}},
            {"jsonrpc": "2.0", "method": "notifications/initialized"},
            {"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
            {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "screenshot", "arguments": {"source": "phone"}}},
            {"jsonrpc": "2.0", "id": 4, "method": "tools/call", "params": {"name": "shell", "arguments": {}}},
        ]
        result = subprocess.run([sys.executable, str(Path(__file__).parents[1] / "runner/mcp_operation_tools.py"), str(config)],
                                input="\n".join(map(json.dumps, requests)) + "\n", text=True,
                                capture_output=True, timeout=10, check=True)
    rows = {row["id"]: row for row in map(json.loads, result.stdout.splitlines())}
    assert rows[2]["result"]["tools"][0]["name"] == "screenshot"
    assert rows[2]["result"]["tools"][0]["inputSchema"]["properties"]["source"]["type"] == "string"
    assert rows[3]["result"]["isError"] is True
    assert rows[3]["result"]["content"][1]["data"] == IMAGE
    assert "Unknown business tool" in rows[4]["error"]["message"]
    assert FakeDaemon.calls == ["/api/tools/screenshot"]


def test_openclaw_adapter_isolates_config_and_reconstructs_tool_evidence(tmp_path, monkeypatch):
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "fixture-secret")
    fake_node = tmp_path / "fake-node"
    fake_node.write_text("""#!%s
import json, os, pathlib, sys
args = sys.argv
config = json.loads(pathlib.Path(args[args.index('--config') + 1]).read_text())
assert config['models']['providers']['test']['apiKey'] == '${AIDEN_BENCHMARK_OPENCLAW_KEY}'
assert config['tools']['allow'] == ['benchmark__*']
assert config['tools']['toolSearch'] is False
assert args[args.index('--timeout') + 1] == '35'
assert os.environ['AIDEN_BENCHMARK_OPENCLAW_KEY'] == 'fixture-secret'
assert 'aiden-openclaw-' in os.environ['HOME']
bridge = json.loads(pathlib.Path(config['mcp']['servers']['benchmark']['args'][1]).read_text())
assert bridge == {'endpoint': 'http://127.0.0.1:9000', 'tools': [{'name': 'screenshot', 'description': 'Capture', 'args_schema': {'type': 'object'}}]}
sys.stdin.read()
print(json.dumps({'ok':True,'status':'ok','final':'Done','toolSummary':{'calls':2,'tools':['benchmark__screenshot','exec']}}))
""" % sys.executable)
    fake_node.chmod(0o755)
    adapter = OpenClawAdapter(executable=str(tmp_path / "openclaw.mjs"), node=str(fake_node),
                              provider="test", model="vision", thinking="medium",
                              base_url="https://example.test/v1", api_key_env="BENCHMARK_TEST_KEY")
    artifacts = tmp_path / "artifacts"
    artifacts.mkdir()
    # The tool gateway writes this audit while the harness runs.
    (artifacts / AUDIT_FILE).write_text(json.dumps({"tool_name": "screenshot", "raw_input": "{}", "is_error": False,
        "tool_error": None, "observation": "screenshot observation", "has_screenshot": True}) + "\n")
    result = adapter.execute("Open settings", 5, artifacts, threading.Event(),
                             tool_endpoint="http://127.0.0.1:9000",
                             tool_catalog=[{"name": "screenshot", "description": "Capture", "args_schema": {"type": "object"}}])
    assert "agent_error" not in result.metrics
    assert extract_trace(result.history).tool_calls[0].tool == "screenshot"
    assert extract_trace(result.history).tool_calls[0].has_screenshot
    assert extract_trace(result.history).final_response == "Done"
    assert result.episode["events"][1]["is_error"] is False
    assert "fixture-secret" not in (artifacts / "native-output.json").read_text()
    # A native call missing from the gateway audit is still counted from OpenClaw's summary.
    assert result.metrics["native_tool_calls_reported"] == 1
    assert result.metrics["native_tools_reported"] == ["exec"]


class RecordingModel(BaseHTTPRequestHandler):
    """OpenAI-compatible model that records offered tools and answers immediately."""
    offered: list[str] = []

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
        RecordingModel.offered.extend(tool["function"]["name"] for tool in body.get("tools", []))
        chunk = {"id": "x", "object": "chat.completion.chunk", "created": 0, "model": "probe",
                 "choices": [{"index": 0, "delta": {"role": "assistant", "content": "Done"}, "finish_reason": "stop"}]}
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        self.wfile.write(f"data: {json.dumps(chunk)}\n\ndata: [DONE]\n\n".encode())

    def log_message(self, *_):
        pass


@pytest.mark.skipif(not (os.environ.get("AIDEN_BENCHMARK_OPENCLAW_EXECUTABLE")
                         and os.environ.get("AIDEN_BENCHMARK_OPENCLAW_NODE")),
                    reason="set AIDEN_BENCHMARK_OPENCLAW_EXECUTABLE and _NODE to verify an installed OpenClaw")
def test_installed_openclaw_offers_only_business_tools(tmp_path, monkeypatch):
    RecordingModel.offered = []
    model = ThreadingHTTPServer(("127.0.0.1", 0), RecordingModel)
    thread = threading.Thread(target=model.serve_forever)
    thread.start()
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "probe")
    try:
        adapter = OpenClawAdapter(executable=os.environ["AIDEN_BENCHMARK_OPENCLAW_EXECUTABLE"],
                                  node=os.environ["AIDEN_BENCHMARK_OPENCLAW_NODE"], provider="probe", model="probe",
                                  thinking="off", base_url=f"http://127.0.0.1:{model.server_port}/v1",
                                  api_key_env="BENCHMARK_TEST_KEY")
        result = adapter.execute("Say done.", 60, tmp_path / "artifacts", threading.Event(),
                                 tool_endpoint="http://127.0.0.1:9", tool_catalog=CATALOG)
    finally:
        model.shutdown()
        model.server_close()
        thread.join(timeout=3)
    assert "agent_error" not in result.metrics, (tmp_path / "artifacts/stderr.log").read_text()
    assert set(RecordingModel.offered) == {"benchmark__screenshot"}


def test_openclaw_business_calls_missing_from_audit_are_not_native(tmp_path, monkeypatch):
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "key")
    fake_node = tmp_path / "fake-node"
    fake_node.write_text(f"#!{sys.executable}\n" + """import json, sys
sys.stdin.read()
print(json.dumps({'ok': True, 'status': 'ok', 'final': 'Done',
                  'toolSummary': {'calls': 3, 'tools': ['benchmark__screenshot']}}))
""")
    fake_node.chmod(0o755)
    adapter = OpenClawAdapter(executable=str(tmp_path / "openclaw.mjs"), node=str(fake_node), provider="test",
                              model="vision", thinking="medium", base_url="https://example.test/v1",
                              api_key_env="BENCHMARK_TEST_KEY")
    result = adapter.execute("task", 5, tmp_path / "artifacts", threading.Event(),
                             tool_endpoint="http://127.0.0.1:9", tool_catalog=CATALOG)
    assert (result.metrics["native_tool_calls_reported"], result.metrics["native_tools_reported"]) == (0, [])


def test_gateway_ignores_a_harness_that_disconnects_before_the_reply(tmp_path, daemon, capfd):
    import socket
    with ToolGateway(DaemonToolBackend(daemon), CATALOG, tmp_path) as gateway:
        host, port = gateway.url.removeprefix("http://").split(":")
        body = json.dumps({"input": {"source": "phone"}}).encode()
        client = socket.create_connection((host, int(port)))
        # Ask the kernel to reset rather than close politely, as a killed process would.
        client.setsockopt(socket.SOL_SOCKET, socket.SO_LINGER, struct.pack("ii", 1, 0))
        client.sendall(b"POST /api/tools/screenshot HTTP/1.1\r\nHost: x\r\nContent-Type: application/json\r\n"
                       + f"Content-Length: {len(body)}\r\n\r\n".encode() + body)
        client.close()
        for _ in range(50):
            if (tmp_path / AUDIT_FILE).exists():
                break
            threading.Event().wait(0.1)
        threading.Event().wait(0.3)
    assert [json.loads(line)["tool_name"] for line in (tmp_path / AUDIT_FILE).read_text().splitlines()] == [
        "screenshot", "screenshot"]
    assert "Traceback" not in capfd.readouterr().err


def test_gateway_records_inflight_calls_and_drains_before_finalizing(tmp_path, monkeypatch):
    from runner.runtask import evaluate_task_history
    from runner.suite import load_suite

    entered, release, closing, closed = (threading.Event() for _ in range(4))
    class SlowBackend:
        def invoke(self, name, args):
            entered.set()
            assert release.wait(5)
            return 200, {"output": json.dumps({"width": 20, "height": 30, "format": "jpeg", "data": IMAGE})}

    gateway = ToolGateway(SlowBackend(), CATALOG, tmp_path).__enter__()
    close_server = gateway._server.server_close
    def record_close():
        closing.set()
        close_server()
        closed.set()
    monkeypatch.setattr(gateway._server, "server_close", record_close)
    def request():
        with urllib.request.urlopen(urllib.request.Request(
            f"{gateway.url}/api/tools/screenshot", data=b'{"input":{}}', method="POST"), timeout=5) as response:
            response.read()
    client = threading.Thread(target=request)
    closer = threading.Thread(target=gateway.__exit__)
    client.start()
    try:
        assert entered.wait(3)
        execution = _gateway_execution(tmp_path, "codex-tool-gateway-audit", started=time.monotonic(),
                                       returncode=-15, timed_out=True, canceled=False, model="fixture")
        trace = extract_trace(execution.history)
        assert len(trace.tool_calls) == 1 and not trace.tool_calls[0].has_result
        suite = load_suite(Path(__file__).resolve().parents[1] / "suites/mobilegym_basic.json")
        row = evaluate_task_history(suite=suite, task=suite.tasks[0], history=execution.history,
            episode=execution.episode, metrics=execution.metrics, attempt=1, artifact_dir=tmp_path,
            run_id="codex", timed_out=True, judge_cfg=None, judge_cache_dir=None)
        assert row.status == "timeout" and row.metrics["agent_eligible"] is True
        closer.start()
        assert closing.wait(3)
        assert not closed.is_set()
    finally:
        release.set()
        client.join(timeout=5)
        if closer.ident is None:
            closer.start()
        closer.join(timeout=5)
    assert closed.is_set() and not client.is_alive() and not closer.is_alive()
    execution.history.append({"type": "assistant", "content": "Partial answer", "usage": {"output_tokens": 2}})
    refresh_gateway_evidence(execution, tmp_path)
    trace = extract_trace(execution.history)
    assert len(trace.tool_calls) == 1 and trace.tool_calls[0].has_result
    assert trace.tool_calls[0].has_screenshot
    assert trace.final_response == "Partial answer"
    assert execution.history[-1]["usage"] == {"output_tokens": 2}
    assert (tmp_path / "step_1.jpg").read_bytes() == b"image bytes"


def test_gateway_pairs_parallel_results_in_call_order(tmp_path):
    records = [
        {"type": "tool_call", "call_id": 1, "tool_name": "screenshot", "raw_input": "{}"},
        {"type": "tool_call", "call_id": 2, "tool_name": "touch_gesture", "raw_input": '{"type":"tap"}'},
        {"type": "tool_result", "call_id": 2, "tool_name": "touch_gesture", "raw_input": '{"type":"tap"}',
         "is_error": False, "observation": "second finished first"},
    ]
    (tmp_path / AUDIT_FILE).write_text("".join(json.dumps(record) + "\n" for record in records))
    history, evidence = _gateway_history(tmp_path)
    trace = extract_trace(history)
    assert [(call.tool, call.has_result) for call in trace.tool_calls] == [
        ("screenshot", False), ("touch_gesture", True)]
    assert [event["tool_name"] for event in evidence if event["type"] == "tool_result"] == ["touch_gesture"]
