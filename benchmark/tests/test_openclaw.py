import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import subprocess
import sys
import threading

import pytest

from runner.experiment import openclaw_runtime, validate_agents
from runner.harness import OpenClawAdapter
from runner.trace import extract_trace


def test_openclaw_requires_explicit_model_endpoint_and_supported_node(monkeypatch, tmp_path):
    with pytest.raises(ValueError, match="base_url"):
        validate_agents([{"kind": "openclaw", "provider": "apibest", "model": "vision"}])
    agent = validate_agents([{"kind": "openclaw", "provider": "apibest", "model": "vision",
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


def test_openclaw_mcp_server_preserves_images_errors_and_shared_schema(tmp_path):
    image = base64.b64encode(b"image bytes").decode()
    class ToolServer(BaseHTTPRequestHandler):
        def do_POST(self):
            body = json.loads(self.rfile.read(int(self.headers["Content-Length"])))
            assert self.path == "/api/tools/screenshot"
            assert body == {"input": {"source": "phone"}}
            output = {"width": 20, "height": 30, "format": "jpeg", "data": image,
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
    server = ThreadingHTTPServer(("127.0.0.1", 0), ToolServer)
    thread = threading.Thread(target=server.serve_forever)
    thread.start()
    config = tmp_path / "mcp.json"
    audit = tmp_path / "mcp-audit.jsonl"
    config.write_text(json.dumps({"endpoint": f"http://127.0.0.1:{server.server_port}",
        "audit_path": str(audit), "tools": [{"name": "screenshot", "description": "Capture",
            "args_schema": {"type": "object", "properties": {"source": {"type": "string"}}}}]}))
    try:
        requests = [
            {"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": {"protocolVersion": "2024-11-05"}},
            {"jsonrpc": "2.0", "method": "notifications/initialized"},
            {"jsonrpc": "2.0", "id": 2, "method": "tools/list"},
            {"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": {"name": "screenshot", "arguments": {"source": "phone"}}},
        ]
        result = subprocess.run([sys.executable, str(Path(__file__).parents[1] / "runner/openclaw_tools.py"), str(config)],
                                input="\n".join(map(json.dumps, requests)) + "\n", text=True,
                                capture_output=True, timeout=10, check=True)
    finally:
        server.shutdown()
        server.server_close()
        thread.join(timeout=3)
    responses = {row["id"]: row["result"] for row in map(json.loads, result.stdout.splitlines())}
    assert responses[2]["tools"][0]["name"] == "screenshot"
    assert responses[2]["tools"][0]["inputSchema"]["properties"]["source"]["type"] == "string"
    assert responses[3]["isError"] is True
    assert responses[3]["content"][1]["data"] == image
    assert json.loads((tmp_path / "mcp-audit.jsonl").read_text())["is_error"] is True
    assert (tmp_path / "step_1.jpg").read_bytes() == b"image bytes"


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
assert os.environ['AIDEN_BENCHMARK_OPENCLAW_KEY'] == 'fixture-secret'
assert 'aiden-openclaw-' in os.environ['HOME']
bridge = pathlib.Path(config['mcp']['servers']['benchmark']['args'][1])
audit = pathlib.Path(json.loads(bridge.read_text())['audit_path'])
audit.write_text(json.dumps({'tool_name':'screenshot','raw_input':'{}','is_error':False,
    'tool_error':None,'observation':'screenshot observation','has_screenshot':True}) + '\\n')
sys.stdin.read()
print(json.dumps({'ok':True,'status':'ok','final':'Done','toolSummary':{'calls':1}}))
""" % sys.executable)
    fake_node.chmod(0o755)
    adapter = OpenClawAdapter(executable=str(tmp_path / "openclaw.mjs"), node=str(fake_node),
                              provider="test", model="vision", thinking="medium",
                              base_url="https://example.test/v1", api_key_env="BENCHMARK_TEST_KEY")
    artifacts = tmp_path / "artifacts"
    result = adapter.execute("Open settings", 5, artifacts, threading.Event(),
                             tool_endpoint="http://127.0.0.1:9000",
                             tool_catalog=[{"name": "screenshot", "description": "Capture", "args_schema": {"type": "object"}}])
    assert "agent_error" not in result.metrics
    assert extract_trace(result.history).tool_calls[0].tool == "screenshot"
    assert extract_trace(result.history).tool_calls[0].has_screenshot
    assert extract_trace(result.history).final_response == "Done"
    assert result.episode["events"][1]["is_error"] is False
    assert "fixture-secret" not in (artifacts / "native-output.json").read_text()
