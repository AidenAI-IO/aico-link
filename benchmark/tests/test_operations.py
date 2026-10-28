import dataclasses as dc
import json
from pathlib import Path
import subprocess
import threading

import pytest

from runner import operations
from runner.experiment import ExperimentManager, comparison_suite, run_experiment
from runner.harness import Execution, normalize_pi_events
from runner.trace import extract_trace
from runner.judge import JudgeConfig
from test_experiment import fake_judge


SUITE = Path(__file__).parents[1] / "suites" / "mobilegym_scroll_regression.json"


def successful_execution(state=None, digest="same"):
    return Execution(history=[
        {"type": "tool_call", "tool_name": "touch_gesture", "tool_input": '{"type":"swipe"}'},
        {"type": "tool_result", "content": "ok"},
        {"type": "assistant", "content": "Done"},
    ], episode={"events": [{"type": "tool_result", "tool_name": "touch_gesture", "is_error": False}]}, environment_state=state, metrics={"task_wall_ms": 10, "business_tools_sha256": digest})


@pytest.mark.parametrize("mode", ["success", "failure", "missing_state", "catalog_mismatch"])
def test_operations_score_device_state_and_require_identical_business_contract(tmp_path, mode):
    suite = comparison_suite(SUITE)
    suite = dc.replace(suite, tasks=suite.tasks[:1])
    class Agent:
        def __init__(self, number):
            self.number = number
        def execute_operation(self, prompt, timeout, artifacts, stop, task):
            assert prompt == f"{suite.prompt_prefix}\n\n{task.prompt}".strip()
            assert "environment_assertions" not in prompt
            state = {"route": {"app": "scroll_lab", "path": "/item/scroll-item-024"}, "apps": {"scroll_lab": {
                "selectedItemId": "scroll-item-024", "maxFirstVisibleOrdinal": 25 if mode == "failure" else 24}}}
            result = successful_execution(state if mode != "missing_state" else None,
                                          str(self.number) if mode == "catalog_mismatch" else "same")
            return result
    agents = [{"id": "aiden", "kind": "aiden"}, {"id": "pi", "kind": "pi"}]
    record = {"id": "test", "created_at": "now", "track": "operations"}
    run_experiment(suite, tmp_path, agents, {a["id"]: Agent(i) for i, a in enumerate(agents)},
                   1, 0, threading.Event(), record, JudgeConfig())
    rows = [json.loads((tmp_path / "runs" / a["id"] / "results.jsonl").read_text()) for a in agents]
    if mode == "catalog_mismatch":
        assert sum(r["metrics"]["agent_eligible"] is True for r in rows) == 1
        assert any("catalog changed" in r["metrics"].get("adapter_error", "") for r in rows)
    else:
        assert all(r["status"] == {"success": "passed", "failure": "failed", "missing_state": "judge_error"}[mode] for r in rows)
    assert "original suite assertions and rubric scoring" in (tmp_path / "report.html").read_text()


@pytest.mark.parametrize("stage", ["ok", "setup_error", "execute_error", "state_error", "release_error"])
def test_operation_attempt_resets_captures_and_releases_every_route(tmp_path, monkeypatch, stage):
    lifecycle = []
    routes = []
    def setup(url, **kwargs):
        lifecycle.append("setup")
        routes.append(kwargs["task_id"])
        if stage == "setup_error":
            raise RuntimeError("reset failed")
        return {"ok": True}
    def capture(url, path, **kwargs):
        lifecycle.append(path.name)
        assert kwargs["benchmark_task_id"] == routes[0]
        path.write_bytes(b"image")
    def state(url, **kwargs):
        assert kwargs["benchmark_task_id"] == routes[0]
        if "execute" in lifecycle and stage == "state_error":
            raise RuntimeError("state unavailable")
        return {"os": {"settings": {"global": {"wifiEnabled": "execute" not in lifecycle, "bluetoothEnabled": True}}}}
    def release(url, **kwargs):
        lifecycle.append("release")
        assert kwargs["task_id"] == routes[0]
        if stage == "release_error":
            raise RuntimeError("release unavailable")
    class Daemon:
        def execute(self, prompt, timeout, artifacts, stop, **kwargs):
            lifecycle.append("execute")
            assert kwargs["benchmark_task_id"] == routes[0]
            assert kwargs["tool_names"] == operations.OPERATION_TOOLS
            if stage == "execute_error":
                raise RuntimeError("daemon failed")
            return successful_execution()
    monkeypatch.setattr(operations, "call_environment_setup", setup)
    monkeypatch.setattr(operations, "call_environment_release", release)
    monkeypatch.setattr(operations, "take_environment_screenshot", capture)
    monkeypatch.setattr(operations, "read_environment_state", state)
    adapter = operations.OperationAdapter(daemon=Daemon(), environment_url="http://env")
    stop = threading.Event()
    if stage in {"setup_error", "execute_error"}:
        with pytest.raises((RuntimeError, ValueError)):
            adapter.execute_operation("task", 10, tmp_path, stop, comparison_suite(SUITE).tasks[0])
    else:
        result = adapter.execute_operation("task", 10, tmp_path, stop, comparison_suite(SUITE).tasks[0])
        assert result.pre_screenshot.exists() and result.post_screenshot.exists()
        assert bool(result.environment_state_error) is (stage == "state_error")
        assert stop.is_set() is (stage == "release_error")
    assert lifecycle[0] == "setup" and lifecycle[-1] == "release"


def test_original_overshoot_requirement_is_preserved():
    suite = comparison_suite(SUITE)
    assert len(suite.tasks) == 2
    assert suite.tasks[0].environment_assertions["apps.scroll_lab.maxFirstVisibleOrdinal"] == {"max": 24}
    assert suite.tasks[1].environment_assertions["apps.scroll_lab.maxFirstVisibleOrdinal"] == {"max": 83}
    assert all(task.rubric and task.hard_assertions.required_tools == ["touch_gesture"] for task in suite.tasks)


def test_operation_manager_requires_environment_and_freezes_pi_tool_server_config(tmp_path, monkeypatch):
    captured = []
    monkeypatch.setattr(ExperimentManager, "_run", lambda self, suite, root, manifest, config, stop, judge_cfg, judge_key: captured.append((manifest, config)))
    manager = ExperimentManager(runs_dir=tmp_path, suites_dir=SUITE.parent, base_config_dir=tmp_path,
                                daemon_image="digest", build_daemon_image=False, config_loader=lambda: "frozen config", judge_loader=lambda: {"api_key": "test-key"})
    config = {"suite": SUITE.name, "agents": [{"kind": "pi", "provider": "test", "model": "test"}]}
    with pytest.raises(ValueError, match="environment"):
        manager.start(config)
    manager.start({**config, "environment_url": "http://localhost:9090"})
    manager.shutdown()
    assert captured[0][1] == "frozen config"
    assert captured[0][0]["track"] == "operations"


def test_pi_screenshot_evidence_is_counted_without_duplicating_base64():
    raw = {"content": [{"type": "image", "mimeType": "image/jpeg", "data": "aW1hZ2U="}]}
    result = normalize_pi_events([
        {"type": "tool_execution_start", "toolCallId": "1", "toolName": "screenshot", "args": {}},
        {"type": "tool_execution_end", "toolCallId": "1", "toolName": "screenshot", "result": raw},
    ])
    assert extract_trace(result.history).tool_calls[0].has_screenshot
    assert "aW1hZ2U=" not in json.dumps(result.history)
    assert raw["content"][0]["data"] == "aW1hZ2U="


def test_real_extension_registers_schema_forwards_input_and_returns_image(tmp_path):
    extension = Path(operations.__file__).with_name("pi_operation_tools.mjs")
    config = tmp_path / "tools.json"
    config.write_text(json.dumps({"endpoint": "http://tool-server", "ready_file": str(tmp_path / "ready"),
                                  "tools": [{"name": "screenshot", "description": "Capture", "args_schema": {"type": "object", "properties": {}}}]}))
    script = r'''
import assert from 'node:assert/strict';
const {default: load, toolResult} = await import(process.argv[1]);
process.env.AIDEN_OPERATION_CONFIG = process.argv[2];
const tools = [], hooks = {};
load({registerTool: tool => tools.push(tool), on: (name, hook) => hooks[name] = hook});
assert.equal(tools.length, 1);
assert.equal(tools[0].parameters.type, 'object');
const abort = new AbortController();
globalThis.fetch = async (url, options) => {
  assert.equal(url, 'http://tool-server/api/tools/screenshot');
  assert.deepEqual(JSON.parse(options.body), {input: {}});
  assert.equal(options.signal, abort.signal);
  return {ok: true, json: async () => ({output: JSON.stringify({width: 10, height: 20, format: 'jpeg', data: 'aW1hZ2U=', screen_stable: false}), is_error: true})};
};
const result = await tools[0].execute('call', {}, abort.signal);
assert.equal(result.content[1].type, 'image');
assert.equal(result.content[1].data, 'aW1hZ2U=');
assert.equal(JSON.parse(result.content[0].text).screen_stable, false);
assert.equal(JSON.parse(result.content[0].text).data, undefined);
assert.deepEqual(hooks.tool_result({toolName: 'screenshot', details: result.details}), {isError: true});
assert.equal(toolResult({output: 'plain error'}).content[0].text, 'plain error');
for (const action of [{ok: false}, JSON.stringify({ok: false, suggestion: 'Refocus'})]) {
  const failed = toolResult({is_error: false, output: JSON.stringify({
    width: 10, height: 20, format: 'jpeg', data: 'aW1hZ2U=', action_output: action,
  })});
  assert.equal(failed.details.is_error, true);
  assert.equal(failed.content[1].data, 'aW1hZ2U=');
  assert.deepEqual(hooks.tool_result({toolName: 'screenshot', details: failed.details}), {isError: true});
}
assert.equal(toolResult({output: '{"ok":false}'}).details.is_error, true);
assert.equal(toolResult({output: '{"ok":true,"screen_stable":false,"screen_changed":false}'}).details.is_error, false);
assert.equal(toolResult({output: 'ok', tool_error: {code: 'failed'}}).details.is_error, true);
globalThis.fetch = async () => ({ok: false, status: 500});
await assert.rejects(tools[0].execute('call', {}, abort.signal), /HTTP 500/);
'''
    subprocess.run(["node", "--input-type=module", "-e", script, extension.as_uri(), str(config)], check=True, capture_output=True, text=True)
    assert json.loads((tmp_path / "ready").read_text()) == ["screenshot"]


@pytest.mark.parametrize("failed", [False, True])
def test_original_swipe_assertion_uses_pi_tool_execution_evidence(tmp_path, failed):
    from runner.runtask import evaluate_task_history
    suite = comparison_suite(SUITE.with_name("mobilegym_basic.json"))
    task = suite.tasks[2]
    execution = normalize_pi_events([
        {"type": "tool_execution_start", "toolCallId": "1", "toolName": "touch_gesture", "args": {"type": "tap"}},
        {"type": "tool_execution_end", "toolCallId": "1", "toolName": "touch_gesture", "isError": failed,
         "result": {"content": [{"type": "text", "text": "result"}], "details": {
             "source": "aiden-tool-api", "raw_input": {"type": "swipe"}, "is_error": failed}}},
        {"type": "message_end", "message": {"role": "assistant", "content": [{"type": "text", "text": "Visible item"}]}}
    ])
    row = evaluate_task_history(suite=suite, task=task, history=execution.history, episode=execution.episode,
                                attempt=1, artifact_dir=tmp_path, run_id="pi", timed_out=False,
                                judge_cfg=JudgeConfig(), judge_cache_dir=None, metrics={"task_wall_ms": 1})
    assert row.status == ("failed" if failed else "passed")
    assert row.metrics["episode_tool_evidence"] == "applied"


def test_rubric_only_task_does_not_require_environment_state_endpoint(tmp_path, monkeypatch):
    monkeypatch.setattr(operations, "call_environment_setup", lambda *a, **kw: {})
    monkeypatch.setattr(operations, "call_environment_release", lambda *a, **kw: {})
    monkeypatch.setattr(operations, "take_environment_screenshot", lambda url, path, **kw: path.write_bytes(b"image"))
    def unexpected_state(*args, **kwargs):
        pytest.fail("Rubric-only tasks do not require internal environment state")
    monkeypatch.setattr(operations, "read_environment_state", unexpected_state)
    class Daemon:
        def execute(self, *args, **kwargs):
            return successful_execution()
    task = comparison_suite(SUITE.with_name("mobilegym_basic.json")).tasks[0]
    result = operations.OperationAdapter(daemon=Daemon(), environment_url="http://env").execute_operation(
        task.prompt, 10, tmp_path, threading.Event(), task)
    assert result.pre_screenshot.exists() and result.post_screenshot.exists()


def test_shared_preparation_precedes_evaluated_agent_and_is_not_in_task_history(tmp_path, monkeypatch):
    lifecycle = []
    monkeypatch.setattr(operations, "call_environment_setup", lambda *a, **kw: lifecycle.append("reset"))
    monkeypatch.setattr(operations, "call_environment_release", lambda *a, **kw: lifecycle.append("release"))
    monkeypatch.setattr(operations, "take_environment_screenshot", lambda url, path, **kw: (lifecycle.append(path.name), path.write_bytes(b"image")))
    pi = object()
    class Daemon:
        def execute(self, prompt, timeout, artifacts, stop, **kwargs):
            artifacts.mkdir(parents=True, exist_ok=True)
            lifecycle.append(prompt)
            assert (kwargs.get("pi") is pi) == (prompt == "task")
            return successful_execution()
    task = comparison_suite(SUITE.with_name("phone_control_v1.json")).tasks[0]
    task.setup = {"type": "agent_prompt", "prompt": "Open settings", "expected_response": "Done"}
    adapter = operations.OperationAdapter(daemon=Daemon(), environment_url="http://env", pi=pi,
                                          global_reset={"type": "agent_prompt", "prompt": "Home"})
    result = adapter.execute_operation("task", 10, tmp_path, threading.Event(), task)
    assert lifecycle == ["reset", "Home", "Open settings", "pre.jpg", "task", "post.jpg", "release"]
    assert len(result.history) == 3
    assert (tmp_path / "preparation/1/history.json").exists()
