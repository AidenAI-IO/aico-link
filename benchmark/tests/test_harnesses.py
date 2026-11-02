import json
import os
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
import sys
import threading

import pytest

from runner.harness import ClaudeCodeAdapter, CodexAdapter
from runner.harnesses import HARNESSES, validate_agents
from runner.tool_gateway import AUDIT_FILE, DaemonToolBackend, ToolGateway
from test_openclaw import CATALOG, FakeDaemon, daemon


ENDPOINT = {"provider": "gateway", "model": "vision", "base_url": "https://gateway.example/v1"}


def fake_cli(tmp_path: Path, body: str) -> str:
    executable = tmp_path / "fake-cli"
    executable.write_text(f"#!{sys.executable}\n" + body, encoding="utf-8")
    executable.chmod(0o755)
    return str(executable)


def audited_artifacts(tmp_path: Path) -> Path:
    artifacts = tmp_path / "artifacts"
    artifacts.mkdir()
    # The tool gateway writes this audit while the harness runs.
    (artifacts / AUDIT_FILE).write_text(json.dumps({"tool_name": "screenshot", "raw_input": "{}", "is_error": False,
        "tool_error": None, "observation": "screenshot observation", "has_screenshot": True}) + "\n")
    return artifacts


def test_registry_validates_each_harness_protocol_and_endpoint():
    assert set(HARNESSES) == {"aiden", "pi", "openclaw", "claude-code", "codex"}
    claude = validate_agents([{"kind": "claude-code", **ENDPOINT}])[0]
    codex = validate_agents([{"kind": "codex", **ENDPOINT, "tool_layer": "raw"}])[0]
    assert (claude["api"], codex["api"], codex["tool_layer"]) == ("anthropic-messages", "openai-responses", "raw")
    with pytest.raises(ValueError, match="Claude Code supports API protocols: anthropic-messages"):
        validate_agents([{"kind": "claude-code", **ENDPOINT, "api": "openai-completions"}])
    with pytest.raises(ValueError, match="Codex comparison requires an explicit custom API base_url"):
        validate_agents([{"kind": "codex", "provider": "p", "model": "m"}])
    with pytest.raises(ValueError, match="Supported agents: aiden, pi, openclaw, claude-code, codex"):
        validate_agents([{"kind": "other"}])


def test_claude_code_runs_bare_with_only_mcp_business_tools(tmp_path, monkeypatch):
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "fixture-secret")
    executable = fake_cli(tmp_path, """import json, os, pathlib, sys
args = sys.argv[1:]
assert sys.stdin.read() == 'Open settings'
for flag in ('-p', '--bare', '--strict-mcp-config', '--no-session-persistence'):
    assert flag in args, flag
assert args[args.index('--tools') + 1] == ''
assert args[args.index('--allowedTools') + 1] == 'mcp__benchmark'
assert args[args.index('--effort') + 1] == 'low'
assert os.environ['ANTHROPIC_BASE_URL'] == 'https://gateway.example'
assert os.environ['ANTHROPIC_API_KEY'] == 'fixture-secret'
assert 'aiden-claude-code-' in os.environ['CLAUDE_CONFIG_DIR'] and 'aiden-claude-code-' in os.environ['HOME']
server = json.loads(pathlib.Path(args[args.index('--mcp-config') + 1]).read_text())['mcpServers']['benchmark']
assert json.loads(pathlib.Path(server['args'][1]).read_text())['endpoint'] == 'http://127.0.0.1:9000'
events = [
    {'type': 'system', 'subtype': 'init', 'tools': ['mcp__benchmark__screenshot']},
    {'type': 'assistant', 'message': {'content': [{'type': 'tool_use', 'name': 'mcp__benchmark__screenshot'},
                                                  {'type': 'tool_use', 'name': 'Bash'}]}},
    {'type': 'result', 'subtype': 'success', 'is_error': False, 'result': 'Done',
     'usage': {'input_tokens': 10, 'cache_read_input_tokens': 5, 'output_tokens': 3}},
]
print('\\n'.join(map(json.dumps, events)))
""")
    adapter = ClaudeCodeAdapter(executable=executable, model="vision", thinking="off",
                                base_url="https://gateway.example/v1", api_key_env="BENCHMARK_TEST_KEY")
    artifacts = audited_artifacts(tmp_path)
    result = adapter.execute("Open settings", 5, artifacts, threading.Event(),
                             tool_endpoint="http://127.0.0.1:9000", tool_catalog=CATALOG)
    assert "agent_error" not in result.metrics, (artifacts / "stderr.log").read_text()
    assert [item["type"] for item in result.history] == ["tool_call", "tool_result", "assistant"]
    assert result.history[-1]["usage"] == {"input_tokens": 15, "output_tokens": 3, "cached_input_tokens": 5}
    assert result.metrics["offered_tools"] == ["mcp__benchmark__screenshot"]
    assert (result.metrics["native_tool_calls_reported"], result.metrics["native_tools_reported"]) == (1, ["Bash"])
    assert "fixture-secret" not in (artifacts / "native-events.jsonl").read_text()


def test_claude_code_failed_result_is_an_agent_error(tmp_path, monkeypatch):
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "key")
    executable = fake_cli(tmp_path, """import json, sys
sys.stdin.read()
print(json.dumps({'type': 'result', 'subtype': 'error_during_execution', 'is_error': True, 'result': 'API error'}))
""")
    result = ClaudeCodeAdapter(executable=executable, model="m", thinking="medium", base_url="https://g.example",
                               api_key_env="BENCHMARK_TEST_KEY").execute(
        "task", 5, tmp_path / "artifacts", threading.Event(), tool_endpoint="http://127.0.0.1:9", tool_catalog=CATALOG)
    assert result.metrics["agent_error"] == "API error"


def test_codex_disables_native_tools_and_reads_its_event_stream(tmp_path, monkeypatch):
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "fixture-secret")
    executable = fake_cli(tmp_path, """import json, os, sys
args = sys.argv[1:]
assert sys.stdin.read() == 'Open settings' and args[-1] == '-'
for flag in ('exec', '--json', '--ephemeral', '--ignore-user-config', '--ignore-rules', '--skip-git-repo-check'):
    assert flag in args, flag
assert args[args.index('-s') + 1] == 'read-only'
disabled = {args[i + 1] for i, arg in enumerate(args) if arg == '--disable'}
assert {'shell_tool', 'unified_exec', 'computer_use', 'browser_use'} <= disabled
assert any('default_tools_approval_mode="approve"' in item for item in [args[i + 1] for i, arg in enumerate(args) if arg == '-c'])
config = [args[i + 1] for i, arg in enumerate(args) if arg == '-c']
assert 'web_search="disabled"' in config and 'model_reasoning_effort="high"' in config
assert any('base_url="https://gateway.example/v1"' in item and 'wire_api="responses"' in item for item in config)
assert os.environ['AIDEN_BENCHMARK_CODEX_KEY'] == 'fixture-secret'
assert 'aiden-codex-' in os.environ['CODEX_HOME']
events = [
    {'type': 'thread.started'},
    {'type': 'item.completed', 'item': {'type': 'error', 'message': 'Model metadata not found'}},
    {'type': 'item.completed', 'item': {'type': 'command_execution', 'command': 'ls'}},
    {'type': 'item.completed', 'item': {'type': 'mcp_tool_call', 'server': 'benchmark', 'tool': 'screenshot'}},
    {'type': 'item.completed', 'item': {'type': 'agent_message', 'text': 'Done'}},
    {'type': 'turn.completed', 'usage': {'input_tokens': 20, 'cached_input_tokens': 4, 'output_tokens': 2}},
]
print('\\n'.join(map(json.dumps, events)))
""")
    adapter = CodexAdapter(executable=executable, model="vision", thinking="high",
                           base_url="https://gateway.example/v1", api_key_env="BENCHMARK_TEST_KEY")
    artifacts = audited_artifacts(tmp_path)
    result = adapter.execute("Open settings", 5, artifacts, threading.Event(),
                             tool_endpoint="http://127.0.0.1:9000", tool_catalog=CATALOG)
    assert "agent_error" not in result.metrics, (artifacts / "stderr.log").read_text()
    assert result.history[-1] == {"type": "assistant", "content": "Done",
                                  "usage": {"input_tokens": 20, "output_tokens": 2, "cached_input_tokens": 4}}
    assert (result.metrics["native_tool_calls_reported"], result.metrics["native_tools_reported"]) == (
        1, ["command_execution"])


def test_codex_failed_turn_is_an_agent_error(tmp_path, monkeypatch):
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "key")
    executable = fake_cli(tmp_path, """import json, sys
sys.stdin.read()
print(json.dumps({'type': 'turn.failed', 'error': {'message': 'stream disconnected'}}))
""")
    result = CodexAdapter(executable=executable, model="m", thinking="off", base_url="https://g.example/v1",
                          api_key_env="BENCHMARK_TEST_KEY").execute(
        "task", 5, tmp_path / "artifacts", threading.Event(), tool_endpoint="http://127.0.0.1:9", tool_catalog=CATALOG)
    assert result.metrics["agent_error"] == "stream disconnected"


class ToolCallingModel(BaseHTTPRequestHandler):
    """Fake model: requests one business tool call, then answers after its result."""
    offered: list[str] = []
    protocol = "anthropic"

    def do_POST(self):
        body = json.loads(self.rfile.read(int(self.headers.get("Content-Length") or 0)) or b"{}")
        text = json.dumps(body)
        if self.protocol == "anthropic":
            ToolCallingModel.offered.extend(tool["name"] for tool in body.get("tools", []))
            answered = '"tool_result"' in text
            block = ({"type": "text", "text": "Done"} if answered else
                     {"type": "tool_use", "id": "toolu_1", "name": "mcp__benchmark__screenshot", "input": {"source": "phone"}})
            events = [("message_start", {"type": "message_start", "message": {
                "id": "m", "type": "message", "role": "assistant", "model": body.get("model"), "content": [],
                "stop_reason": None, "usage": {"input_tokens": 1, "output_tokens": 1}}}),
                ("content_block_start", {"type": "content_block_start", "index": 0,
                                         "content_block": {**block, "input": {}} if block["type"] == "tool_use"
                                         else {"type": "text", "text": ""}}),
                ("content_block_delta", {"type": "content_block_delta", "index": 0, "delta":
                    {"type": "input_json_delta", "partial_json": json.dumps(block["input"])}
                    if block["type"] == "tool_use" else {"type": "text_delta", "text": "Done"}}),
                ("content_block_stop", {"type": "content_block_stop", "index": 0}),
                ("message_delta", {"type": "message_delta", "delta": {
                    "stop_reason": "end_turn" if answered else "tool_use"}, "usage": {"output_tokens": 1}}),
                ("message_stop", {"type": "message_stop"})]
        else:
            for tool in body.get("tools", []):
                ToolCallingModel.offered.extend(f"{tool['name']}/{item['name']}" for item in tool.get("tools", []))
                if tool.get("type") != "namespace":
                    ToolCallingModel.offered.append(tool.get("name") or tool.get("type"))
            answered = "function_call_output" in text
            item = ({"type": "message", "role": "assistant", "id": "m", "content": [{"type": "output_text", "text": "Done"}]}
                    if answered else {"type": "function_call", "id": "fc", "call_id": "c1", "namespace": "mcp__benchmark",
                                      "name": "screenshot", "arguments": json.dumps({"source": "phone"})})
            events = [(name, payload) for name, payload in (
                ("response.created", {"type": "response.created", "response": {"id": "r"}}),
                ("response.output_item.done", {"type": "response.output_item.done", "output_index": 0, "item": item}),
                ("response.completed", {"type": "response.completed", "response": {
                    "id": "r", "usage": {"input_tokens": 1, "output_tokens": 1, "total_tokens": 2}}}))]
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.end_headers()
        for name, payload in events:
            self.wfile.write(f"event: {name}\ndata: {json.dumps(payload)}\n\n".encode())

    def log_message(self, *_):
        pass


@pytest.fixture
def model():
    ToolCallingModel.offered = []
    server = ThreadingHTTPServer(("127.0.0.1", 0), ToolCallingModel)
    thread = threading.Thread(target=server.serve_forever)
    thread.start()
    yield f"http://127.0.0.1:{server.server_port}"
    server.shutdown()
    server.server_close()
    thread.join(timeout=3)


@pytest.mark.parametrize("variable,adapter_type,protocol,expected_tools", [
    ("AIDEN_BENCHMARK_CLAUDE_CODE_EXECUTABLE", ClaudeCodeAdapter, "anthropic", {"mcp__benchmark__screenshot"}),
    ("AIDEN_BENCHMARK_CODEX_EXECUTABLE", CodexAdapter, "responses", {"mcp__benchmark/screenshot"}),
])
def test_installed_cli_calls_business_tools_through_gateway(
        tmp_path, monkeypatch, daemon, model, variable, adapter_type, protocol, expected_tools):
    if not os.environ.get(variable):
        pytest.skip(f"set {variable} to verify an installed CLI")
    monkeypatch.setattr(ToolCallingModel, "protocol", protocol)
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "probe")
    adapter = adapter_type(executable=os.environ[variable], model="probe", thinking="low",
                           base_url=f"{model}/v1", api_key_env="BENCHMARK_TEST_KEY")
    artifacts = tmp_path / "artifacts"
    artifacts.mkdir()
    with ToolGateway(DaemonToolBackend(daemon), CATALOG, artifacts) as gateway:
        result = adapter.execute("Take a screenshot.", 90, artifacts, threading.Event(),
                                 tool_endpoint=gateway.url, tool_catalog=CATALOG)
    assert "agent_error" not in result.metrics, (artifacts / "stderr.log").read_text()[-2000:]
    business = {name for name in ToolCallingModel.offered if "benchmark" in name}
    assert business == expected_tools
    if protocol == "anthropic":
        assert set(ToolCallingModel.offered) == expected_tools
    else:
        # Codex keeps MCP resource helpers and request_user_input, which cannot act on the host.
        assert set(ToolCallingModel.offered) - expected_tools <= {
            "list_mcp_resources", "list_mcp_resource_templates", "read_mcp_resource", "request_user_input"}
    assert FakeDaemon.calls == ["/api/tools/screenshot"]
    assert result.history[0]["tool_name"] == "screenshot" and result.history[-1]["content"] == "Done"
    assert result.metrics["native_tool_calls_reported"] == 0


def test_codex_reconnects_are_retries_not_failures_when_the_turn_completes(tmp_path, monkeypatch):
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "key")
    executable = fake_cli(tmp_path, """import json, sys
sys.stdin.read()
events = [{'type': 'error', 'message': 'Reconnecting... waiting for network'},
          {'type': 'error', 'message': 'Reconnecting... waiting for network'},
          {'type': 'item.completed', 'item': {'type': 'agent_message', 'text': 'Done'}},
          {'type': 'turn.completed', 'usage': {'input_tokens': 1, 'output_tokens': 1}}]
print('\\n'.join(map(json.dumps, events)))
""")
    result = CodexAdapter(executable=executable, model="m", thinking="low", base_url="https://g.example/v1",
                          api_key_env="BENCHMARK_TEST_KEY").execute(
        "task", 5, tmp_path / "artifacts", threading.Event(), tool_endpoint="http://127.0.0.1:9", tool_catalog=CATALOG)
    assert "agent_error" not in result.metrics
    assert result.metrics["api_retries"] == 2


def test_claude_code_counts_api_retries(tmp_path, monkeypatch):
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "key")
    executable = fake_cli(tmp_path, """import json, sys
sys.stdin.read()
print(json.dumps({'type': 'system', 'subtype': 'api_retry', 'attempt': 1}))
print(json.dumps({'type': 'result', 'subtype': 'success', 'is_error': False, 'result': 'Done'}))
""")
    result = ClaudeCodeAdapter(executable=executable, model="m", thinking="low", base_url="https://g.example",
                               api_key_env="BENCHMARK_TEST_KEY").execute(
        "task", 5, tmp_path / "artifacts", threading.Event(), tool_endpoint="http://127.0.0.1:9", tool_catalog=CATALOG)
    assert "agent_error" not in result.metrics and result.metrics["api_retries"] == 1


def test_external_cli_finds_node_outside_its_install_directory(tmp_path, monkeypatch):
    # CI installs the CLIs into their own prefix while setup-node provides `node`.
    node_dir = tmp_path / "node-bin"
    node_dir.mkdir()
    node = node_dir / "node"
    node.write_text(f"#!{sys.executable}\nimport sys\nexec(open(sys.argv[1]).read())\n")
    node.chmod(0o755)
    monkeypatch.setenv("PATH", f"{node_dir}{os.pathsep}{os.environ['PATH']}")
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "key")
    cli_dir = tmp_path / "cli-bin"
    cli_dir.mkdir()
    executable = cli_dir / "codex"
    executable.write_text("""#!/usr/bin/env node
import json, sys
sys.stdin.read()
print(json.dumps({'type': 'item.completed', 'item': {'type': 'agent_message', 'text': 'Done'}}))
print(json.dumps({'type': 'turn.completed', 'usage': {'input_tokens': 1, 'output_tokens': 1}}))
""")
    executable.chmod(0o755)
    result = CodexAdapter(executable=str(executable), model="m", thinking="low", base_url="https://g.example/v1",
                          api_key_env="BENCHMARK_TEST_KEY").execute(
        "task", 5, tmp_path / "artifacts", threading.Event(), tool_endpoint="http://127.0.0.1:9", tool_catalog=CATALOG)
    assert result.metrics["exit_code"] == 0, (tmp_path / "artifacts/stderr.log").read_text()
    assert result.history[-1]["content"] == "Done"


def test_cli_that_exits_before_reading_its_prompt_is_an_agent_error(tmp_path, monkeypatch):
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "key")
    executable = fake_cli(tmp_path, "import sys\nsys.stdout.close()\nsys.exit(127)\n")
    # A prompt larger than the pipe buffer guarantees the write hits the closed pipe.
    result = CodexAdapter(executable=executable, model="m", thinking="low", base_url="https://g.example/v1",
                          api_key_env="BENCHMARK_TEST_KEY").execute(
        "x" * 1_000_000, 5, tmp_path / "artifacts", threading.Event(),
        tool_endpoint="http://127.0.0.1:9", tool_catalog=CATALOG)
    assert result.metrics["exit_code"] == 127
    assert "exited with code 127" in result.metrics["agent_error"]


def test_claude_code_empty_result_falls_back_to_last_assistant_text(tmp_path, monkeypatch):
    monkeypatch.setenv("BENCHMARK_TEST_KEY", "key")
    executable = fake_cli(tmp_path, """import json, sys
sys.stdin.read()
print(json.dumps({'type': 'assistant', 'message': {'content': [{'type': 'text', 'text': 'Settings is open.'}]}}))
print(json.dumps({'type': 'result', 'subtype': 'success', 'is_error': False, 'result': ''}))
""")
    result = ClaudeCodeAdapter(executable=executable, model="m", thinking="low", base_url="https://g.example",
                               api_key_env="BENCHMARK_TEST_KEY").execute(
        "task", 5, tmp_path / "artifacts", threading.Event(), tool_endpoint="http://127.0.0.1:9", tool_catalog=CATALOG)
    assert "agent_error" not in result.metrics
    assert result.history[-1]["content"] == "Settings is open."
