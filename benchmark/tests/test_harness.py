import json
from pathlib import Path
import sys
import threading

import pytest

from runner.harness import AidenAdapter, PiAdapter, normalize_pi_events
from runner.metrics import derive_history_metrics
from runner.trace import extract_trace


def assistant(text="(b)", **overrides):
    return {"type": "message_end", "message": {
        "role": "assistant", "provider": "test", "model": "exact-model",
        "content": [{"type": "text", "text": text}], "stopReason": "stop",
        "usage": {"input": 10, "output": 2, "cacheRead": 3, "cacheWrite": 4,
                  "cost": {"total": 0.01}}, **overrides,
    }}


def test_pi_normalizes_parallel_calls_without_losing_errors_or_double_counting_usage():
    events = [
        {"type": "tool_execution_start", "toolCallId": "a", "toolName": "bash", "args": {"command": "false"}},
        {"type": "tool_execution_start", "toolCallId": "b", "toolName": "read", "args": {"path": "a"}},
        {"type": "tool_execution_end", "toolCallId": "b", "toolName": "read", "result": "ok", "isError": False},
        {"type": "tool_execution_end", "toolCallId": "a", "toolName": "bash", "result": "failed", "isError": True},
        assistant(),
        {"type": "agent_end", "messages": [assistant()["message"]]},
    ]
    result = normalize_pi_events(events)
    trace = extract_trace(result.history)
    assert [(call.tool, call.is_error, call.has_result) for call in trace.tool_calls] == [
        ("bash", True, True), ("read", False, True),
    ]
    assert trace.final_response == "(b)"
    metrics = derive_history_metrics(result.history)
    assert metrics["input_tokens"] == 17
    assert metrics["output_tokens"] == 2
    assert metrics["llm_calls"] == 1
    assert result.metrics["cost_usd"] == .01


def test_pi_missing_usage_stays_unknown_and_native_retry_can_recover():
    failed = assistant("", stopReason="error", errorMessage="temporary failure", usage=None)
    result = normalize_pi_events([failed, assistant()])
    assert "agent_error" not in result.metrics
    assert result.metrics["cost_usd"] is None
    assert normalize_pi_events([failed]).metrics["agent_error"] == "temporary failure"


def fake_pi(tmp_path: Path, body: str) -> PiAdapter:
    executable = tmp_path / "fake-pi"
    executable.write_text(f"#!{sys.executable}\n" + body, encoding="utf-8")
    executable.chmod(0o755)
    return PiAdapter(executable=str(executable), provider="test", model="exact-model", thinking="off")


def test_pi_subprocess_uses_stdin_fresh_workspace_and_collects_real_protocol(tmp_path):
    adapter = fake_pi(tmp_path, "import sys, json, os\n"
                      "prompt = sys.stdin.read()\n"
                      "assert prompt == '@file $(not-a-command) \\u2028 text'\n"
                      "assert os.path.basename(os.getcwd()) == 'workspace'\n"
                      "assert os.environ['PI_CODING_AGENT_DIR'] != os.path.expanduser('~/.pi/agent')\n"
                      "assert '--no-context-files' in sys.argv\n"
                      f"print({json.dumps(json.dumps(assistant()))})\n")
    artifacts = tmp_path / "artifacts"
    result = adapter.execute("@file $(not-a-command) \u2028 text", 5, artifacts, threading.Event())
    assert result.metrics["exit_code"] == 0
    assert extract_trace(result.history).final_response == "(b)"
    assert (artifacts / "native-events.jsonl").exists()
    assert (artifacts / "stderr.log").read_text() == ""


@pytest.mark.parametrize("cancel", [False, True])
def test_pi_timeout_and_cancel_stop_process_and_preserve_partial_evidence(tmp_path, cancel):
    adapter = fake_pi(tmp_path, "import time\n" + f"print({json.dumps(json.dumps(assistant()))}, flush=True)\n" + "time.sleep(60)\n")
    stop = threading.Event()
    timer = threading.Timer(.3, stop.set) if cancel else None
    if timer:
        timer.start()
    try:
        result = adapter.execute("task", 5 if cancel else .3, tmp_path / "artifacts", stop)
    finally:
        if timer:
            timer.cancel()
    assert result.canceled is cancel
    assert result.timed_out is not cancel
    assert result.metrics["exit_code"] is not None
    assert extract_trace(result.history).final_response == "(b)"


def test_pi_nonzero_exit_and_bad_protocol_do_not_pass(tmp_path):
    adapter = fake_pi(tmp_path, "import sys\nsys.exit(3)\n")
    result = adapter.execute("task", 5, tmp_path / "one", threading.Event())
    assert "agent_error" in result.metrics
    adapter = fake_pi(tmp_path, "print('not json')\n")
    with pytest.raises(ValueError, match="malformed JSON"):
        adapter.execute("task", 5, tmp_path / "two", threading.Event())


def test_pi_rejects_unpaired_tool_results():
    with pytest.raises(ValueError, match="unknown tool call"):
        normalize_pi_events([{"type": "tool_execution_end", "toolCallId": "missing"}])


def test_pi_environment_isolates_home_and_provider_credentials(tmp_path, monkeypatch):
    from runner.harness import pi_environment
    monkeypatch.setenv("OPENROUTER_API_KEY", "selected-provider-key")
    monkeypatch.setenv("AWS_SECRET_ACCESS_KEY", "unrelated-key")
    monkeypatch.setenv("DATABASE_PASSWORD", "unrelated-secret")
    env = pi_environment("/opt/pi/bin/pi", "openrouter", tmp_path)
    assert env["OPENROUTER_API_KEY"] == "selected-provider-key"
    assert env["HOME"] == str(tmp_path / "home")
    assert "AWS_SECRET_ACCESS_KEY" not in env
    assert "DATABASE_PASSWORD" not in env


@pytest.mark.parametrize("vision", [False, True])
def test_custom_pi_endpoint_generates_isolated_config_and_keeps_unknown_cost(tmp_path, monkeypatch, vision):
    monkeypatch.setenv("MY_GATEWAY_KEY", "endpoint-test-secret")
    monkeypatch.setenv("OPENAI_API_KEY", "unrelated-provider-secret")
    body = """import json, os, sys
from pathlib import Path
sys.stdin.read()
config = json.loads((Path(os.environ['PI_CODING_AGENT_DIR']) / 'models.json').read_text())
provider = config['providers']['openai']
assert provider['baseUrl'] == 'http://127.0.0.1:9000/v1'
assert provider['api'] == 'openai-completions'
assert provider['apiKey'] == '$AIDEN_BENCHMARK_PI_CUSTOM_KEY'
assert os.environ['AIDEN_BENCHMARK_PI_CUSTOM_KEY'] == 'endpoint-test-secret'
assert 'OPENAI_API_KEY' not in os.environ
assert 'endpoint-test-secret' not in json.dumps(config)
model = provider['models'][0]
assert model['contextWindow'] == 32000 and model['maxTokens'] == 2000
assert model['reasoning'] is False
if 'AIDEN_OPERATION_CONFIG' in os.environ:
    assert model['input'] == ['text', 'image']
    tools = json.loads(Path(os.environ['AIDEN_OPERATION_CONFIG']).read_text())
    Path(tools['ready_file']).write_text('[]')
else:
    assert model['input'] == ['text']
""" + f"print({json.dumps(json.dumps(assistant()))})\n"
    adapter = fake_pi(tmp_path, body)
    adapter.provider, adapter.base_url = "openai", "http://127.0.0.1:9000/v1"
    adapter.api_key_env, adapter.context_window, adapter.max_tokens = "MY_GATEWAY_KEY", 32000, 2000
    artifacts = tmp_path / "artifacts"
    kwargs = {"tool_endpoint": "http://127.0.0.1:1", "tool_catalog": []} if vision else {}
    result = adapter.execute("task", 5, artifacts, threading.Event(), **kwargs)
    assert result.metrics["exit_code"] == 0, (artifacts / "stderr.log").read_text()
    assert result.metrics["cost_usd"] is None
    assert result.metrics["cost_source"] is None
    assert not list(artifacts.rglob("models.json"))
    assert all("endpoint-test-secret" not in path.read_text() for path in artifacts.iterdir())


def test_custom_endpoint_missing_key_does_not_start_pi(tmp_path, monkeypatch):
    monkeypatch.delenv("MY_MISSING_KEY", raising=False)
    adapter = PiAdapter(executable="must-not-be-executed", provider="custom", model="vision", thinking="off",
                        base_url="http://localhost:9000/v1", api_key_env="MY_MISSING_KEY")
    with pytest.raises(ValueError, match="Set MY_MISSING_KEY"):
        adapter.execute("task", 10, tmp_path / "artifacts", threading.Event())


@pytest.mark.parametrize("mode", ["success", "timeout", "runner_deadline", "cancel", "startup_error"])
def test_aiden_owned_daemon_always_stops_and_episode_is_retained(tmp_path, monkeypatch, mode):
    import time
    from runner import harness, webui
    from runner.agent_client import ChatResponse, AgentTimeoutError
    stop = threading.Event()
    lifecycle = []
    monkeypatch.setattr(webui, "prepare_run_config", lambda *args, **kwargs: None)
    def start(*args, **kwargs):
        lifecycle.append("start")
        if mode == "startup_error":
            raise RuntimeError("startup failed")
        return "container-id"
    monkeypatch.setattr(webui, "start_daemon_compose", start)
    monkeypatch.setattr(webui, "stop_daemon_compose", lambda job: lifecycle.append("stop"))
    monkeypatch.setattr(webui, "docker_published_port", lambda *args: 8080)
    class Client:
        def __init__(self, url):
            assert url == "http://127.0.0.1:8080"
        def health(self):
            return True
        def chat(self, prompt, timeout_sec):
            assert prompt == "task"
            if mode == "timeout":
                raise AgentTimeoutError("deadline")
            if mode == "runner_deadline":
                time.sleep(.3)
            if mode == "cancel":
                stop.set()
                time.sleep(.3)
            return ChatResponse("(b)", [{"type": "assistant", "content": "(b)"}], "episode-id")
        def get_history(self):
            return [{"type": "assistant", "content": "partial"}]
        def get_episode(self, episode_id):
            assert episode_id == "episode-id"
            return {"events": [], "extra": {"cost_usd": .02}}
        def close(self):
            lifecycle.append("close")
    monkeypatch.setattr(harness, "AgentClient", Client)
    adapter = AidenAdapter(image="digest", prepared_config=tmp_path)
    if mode == "startup_error":
        with pytest.raises(RuntimeError, match="startup failed"):
            adapter.execute("task", 5, tmp_path / "artifacts", stop)
    else:
        result = adapter.execute("task", .05 if mode == "runner_deadline" else 5, tmp_path / "artifacts", stop)
        assert result.timed_out is (mode in {"timeout", "runner_deadline"})
        assert result.canceled is (mode == "cancel")
        if mode == "success":
            assert result.episode["extra"]["cost_usd"] == .02
        if mode in {"timeout", "runner_deadline", "cancel"}:
            assert result.history[0]["content"] == "partial"
            assert json.loads((tmp_path / "artifacts/native-history.json").read_text()) == result.history
    assert lifecycle.count("start") == lifecycle.count("stop") == 1


def test_pi_operations_use_daemon_tools_without_delegating_to_aiden_chat(tmp_path, monkeypatch):
    from runner import harness, webui
    from runner.harness import Execution
    from runner.operations import OPERATION_TOOLS
    lifecycle = []
    catalog = [{"name": name, "description": name, "args_schema": {"type": "object"}}
               for name in OPERATION_TOOLS]
    monkeypatch.setattr(webui, "prepare_run_config", lambda *a, **k: None)
    def start(*args, **kwargs):
        assert kwargs["benchmark_task_id"] == "route-id"
        assert kwargs["environment_bridge_mode"] is True
        assert kwargs["device_type"] == "android"
        assert "host.docker.internal" in kwargs["environment_bridge_endpoint"]
        return "container"
    monkeypatch.setattr(webui, "start_daemon_compose", start)
    monkeypatch.setattr(webui, "stop_daemon_compose", lambda job: lifecycle.append("stopped"))
    monkeypatch.setattr(webui, "docker_published_port", lambda *a: 8080)
    class Client:
        def __init__(self, url):
            self.base_url = url
        def health(self):
            return True
        def _get(self, path):
            assert path == "/api/tools"
            return 200, json.dumps({"tools": catalog}).encode()
        def chat(self, *a, **k):
            pytest.fail("Pi must never ask the Aiden agent to execute its task")
        def close(self):
            pass
    class Pi:
        def execute(self, prompt, timeout, artifacts, stop, **kwargs):
            assert kwargs["tool_catalog"] == catalog
            assert kwargs["tool_endpoint"] == "http://127.0.0.1:8080"
            return Execution(metrics={"cost_usd": 0.01})
    monkeypatch.setattr(harness, "AgentClient", Client)
    result = AidenAdapter(image="digest", prepared_config=tmp_path).execute(
        "task", 10, tmp_path / "artifacts", threading.Event(),
        environment_url="http://127.0.0.1:9090", benchmark_task_id="route-id", pi=Pi(), tool_names=OPERATION_TOOLS)
    assert len(result.metrics["business_tools_sha256"]) == 64
    assert result.metrics["cost_usd"] == 0.01
    assert lifecycle == ["stopped"]
