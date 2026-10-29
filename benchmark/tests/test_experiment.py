import dataclasses as dc
import json
from pathlib import Path
import threading

import pytest

from runner.experiment import (ExperimentManager, atomic_json, comparison, comparison_suite,
                               retry_experiment_judging, run_experiment, validate_agents)
from runner.harness import Execution
from runner.models import TaskResult, RubricVerdict
from runner.judge import JudgeConfig, JudgeOutput
from runner import runtask
from runner.report import now_iso


SUITE_PATH = Path(__file__).resolve().parents[1] / "suites" / "mobilegym_basic.json"


@pytest.fixture(autouse=True)
def fake_judge(monkeypatch):
    def judge(**kwargs):
        return JudgeOutput(verdicts=[RubricVerdict(id=item.id, verdict="yes", reason="Fixture evidence")
                                    for item in kwargs["rubric"]], overall_notes="", cache_key="fake", raw_response="")
    monkeypatch.setattr(runtask, "judge_task", judge)


class FakeAgent:
    def __init__(self, calls, name, *, fail=False, stop=None):
        self.calls, self.name, self.fail, self.stop = calls, name, fail, stop

    def execute_operation(self, prompt, timeout, artifacts, stop, task):
        self.calls.append((self.name, prompt))
        if self.fail:
            raise RuntimeError("adapter unavailable")
        if self.stop:
            self.stop.set()
        return Execution(history=[
            {"type": "tool_call", "tool_name": "screenshot", "tool_input": "{}"},
            {"type": "tool_result", "content": "screen captured"},
            {"type": "assistant", "content": "Done"},
        ], metrics={"task_wall_ms": 10, "cost_usd": .01, "business_tools_sha256": "same"})


def tiny_suite():
    suite = comparison_suite(SUITE_PATH)
    return dc.replace(suite, tasks=suite.tasks[:1])


def manifest():
    return {"id": "exp-test", "status": "queued", "created_at": now_iso()}


def test_interleaved_runs_have_identical_inputs_independent_artifacts_and_paired_report(tmp_path):
    suite = tiny_suite()
    agents = [{"id": "aiden", "kind": "aiden"}, {"id": "pi", "kind": "pi"}]
    calls = []
    adapters = {a["id"]: FakeAgent(calls, a["id"]) for a in agents}
    result = run_experiment(suite, tmp_path, agents, adapters, 2, 42, threading.Event(), manifest(), JudgeConfig())
    assert result["status"] == "completed"
    assert result["completed"] == 4
    assert len({prompt for _, prompt in calls}) == 1
    assert {name for name, _ in calls[:2]} == {"pi", "aiden"}
    report = json.loads((tmp_path / "comparison.json").read_text())
    assert len(report["paired_attempts"]) == 2
    assert all(a["passed"] == 2 and a["coverage"] == 1 for a in report["agents"])
    for agent in agents:
        root = tmp_path / "runs" / agent["id"]
        rows = [json.loads(line) for line in (root / "results.jsonl").read_text().splitlines()]
        assert [r["attempt"] for r in rows] == [1, 2]
        assert all(Path(row["artifact_dir"]).is_relative_to(root) for row in rows)
        assert (root / "report.html").exists()
        assert json.loads((root / "manifest.json").read_text())["suite_sha256"] == suite.sha256


def test_judge_retry_updates_original_reports_without_reexecuting_agents(tmp_path, monkeypatch):
    from runner import rejudge
    suite = comparison_suite(SUITE_PATH)
    agents = [{"id": "aiden", "kind": "aiden"}, {"id": "pi", "kind": "pi"}]
    calls = []
    (tmp_path / "suite.json").write_bytes(suite.source_bytes)
    record = {**manifest(), "agents": agents, "repeats": 1,
              "judge_config": dc.asdict(JudgeConfig())}
    def unavailable(**kwargs):
        raise RuntimeError("Judge transport unavailable")
    monkeypatch.setattr(runtask, "judge_task", unavailable)
    run_experiment(suite, tmp_path, agents,
                   {agent["id"]: FakeAgent(calls, agent["id"]) for agent in agents},
                   1, 0, threading.Event(), record, JudgeConfig())
    assert len(calls) == len(suite.tasks) * len(agents)
    assert all(agent["eligible"] == 0 for agent in json.loads((tmp_path / "comparison.json").read_text())["agents"])
    judged = []
    def recovered(**kwargs):
        judged.append(kwargs["task_prompt"])
        return JudgeOutput(verdicts=[RubricVerdict(item.id, "yes", "Recorded evidence")
                                     for item in kwargs["rubric"]], overall_notes="", cache_key="", raw_response="")
    monkeypatch.setattr(rejudge, "judge_task", recovered)
    refreshed = retry_experiment_judging(tmp_path, JudgeConfig())
    assert refreshed["judge_retry"]["recovered"] == 4
    assert refreshed["judge_retry"]["remaining"] == 0
    assert len(judged) == 4
    assert len(calls) == len(suite.tasks) * len(agents)
    paired = json.loads((tmp_path / "comparison.json").read_text())
    assert paired["pairwise"][0]["eligible_pairs"] == 2
    assert all(agent["passed"] == 2 for agent in paired["agents"])
    for agent in agents:
        run_dir = tmp_path / "runs" / agent["id"]
        assert [json.loads(line)["status"] for line in
                (run_dir / "results.original.jsonl").read_text().splitlines()] == ["judge_error", "judge_error", "judge_error"]
        assert [json.loads(line)["status"] for line in
                (run_dir / "results.jsonl").read_text().splitlines()] == ["passed", "passed", "judge_error"]
        assert '"status": "passed"' in (run_dir / "report.html").read_text()


def test_adapter_failure_does_not_abort_other_agents_or_inflate_coverage(tmp_path):
    agents = [{"id": "broken", "kind": "pi"}, {"id": "working", "kind": "aiden"}]
    run_experiment(tiny_suite(), tmp_path, agents,
                   {"broken": FakeAgent([], "broken", fail=True), "working": FakeAgent([], "working")},
                   1, 0, threading.Event(), manifest(), JudgeConfig())
    report = json.loads((tmp_path / "comparison.json").read_text())
    broken, working = report["agents"]
    assert broken["coverage"] == 0 and broken["success_rate"] is None
    assert broken["total_cost_usd"] is None
    assert working["passed"] == 1
    assert report["pairwise"][0]["eligible_pairs"] == 0
    assert report["pairwise"][0]["agents"]["working"]["success_rate"] is None


def test_paired_scores_use_same_denominator_and_latency_uses_both_passes():
    suite = tiny_suite()
    agents = [{"id": key, "kind": key} for key in ("aiden", "pi")]
    def row(attempt, status, wall, eligible=True):
        return TaskResult(suite=suite.name, run_id="test", task_id=suite.tasks[0].id,
                          category="test", attempt=attempt, status=status, rubric=[],
                          metrics={"agent_eligible": eligible, "wall_ms": wall})
    data = comparison(suite, agents, {
        "aiden": [row(1, "passed", 10), row(2, "passed", 100), row(3, "skipped", None, False)],
        "pi": [row(1, "passed", 20), row(2, "timeout", 90000), row(3, "passed", 1)],
    }, 4)
    pair = data["pairwise"][0]
    assert pair["eligible_pairs"] == 2
    assert pair["excluded_or_missing_pairs"] == 2
    assert pair["both_pass"] == 1 and pair["left_only_pass"] == 1
    assert pair["right_only_pass"] == pair["both_fail"] == 0
    assert pair["agents"]["aiden"]["success_rate"] == 1
    assert pair["agents"]["pi"]["success_rate"] == .5
    assert pair["agents"]["aiden"]["both_pass_median_wall_ms"] == 10
    assert pair["agents"]["pi"]["both_pass_median_wall_ms"] == 20


@pytest.mark.parametrize("evidence", ["none", "text", "tool", "tokens"])
@pytest.mark.parametrize("kind", ["aiden", "pi"])
def test_connection_errors_are_scored_by_execution_evidence_not_retry_records(tmp_path, kind, evidence):
    from runner.harness import normalize_pi_events
    execution = Execution(metrics={"agent_error": "EOF"})
    if kind == "pi":
        execution = normalize_pi_events([{"type": "message_end", "message": {
            "role": "assistant", "content": [], "stopReason": "error", "errorMessage": "Connection error.",
            "usage": {"input": 0, "output": 0, "cacheRead": 0, "cacheWrite": 0},
        }}] * 4)
    if evidence == "text":
        execution.history.insert(0, {"type": "assistant", "content": "Opening settings"})
    elif evidence == "tool":
        execution.history.append({"type": "tool_call", "tool_name": "screenshot", "tool_input": "{}"})
    elif evidence == "tokens":
        execution.history.append({"type": "assistant", "content": "", "usage": {"output_tokens": 20}})
    suite = tiny_suite()
    row = runtask.evaluate_task_history(suite=suite, task=suite.tasks[0], history=execution.history,
        episode=execution.episode, metrics=execution.metrics, attempt=1, artifact_dir=tmp_path,
        run_id=kind, timed_out=False, judge_cfg=None, judge_cache_dir=None)
    assert row.status == "failed"
    assert row.metrics["agent_eligible"] is (evidence != "none")
    assert row.metrics["execution_started"] is (evidence != "none")


def test_cancel_preserves_attempts_and_leaves_unrun_cells_explicit(tmp_path):
    stop = threading.Event()
    run_experiment(tiny_suite(), tmp_path, [{"id": "pi", "kind": "pi"}],
                   {"pi": FakeAgent([], "pi", stop=stop)}, 3, 0, stop, manifest(), JudgeConfig())
    report = json.loads((tmp_path / "comparison.json").read_text())
    assert report["agents"][0]["completed"] == 1
    assert report["agents"][0]["total_cost_usd"] is None
    assert report["paired_attempts"][1]["agents"]["pi"]["status"] == "not_run"
    assert json.loads((tmp_path / "manifest.json").read_text())["status"] == "canceled"


@pytest.mark.parametrize("change", [
    {"setup": {"type": "seed_memory", "memories": [{"id": "x", "content": "x"}]}},
    {"hard_assertions": {"required_tools": ["save_memory"]}},
    {"input_screenshot": "screen.jpg"},
])
def test_comparison_suite_rejects_agent_specific_requirements(tmp_path, change):
    data = json.loads(SUITE_PATH.read_text())
    data["tasks"][0].update(change)
    path = tmp_path / "suite.json"
    path.write_text(json.dumps(data))
    with pytest.raises(ValueError):
        comparison_suite(path)


def test_suite_requiring_unshared_tools_is_not_accepted():
    with pytest.raises(ValueError, match="shared business tools"):
        comparison_suite(SUITE_PATH.parent / "shell_utility_v1.json")


@pytest.mark.parametrize("agents", [
    [], [{"kind": "pi"}], [{"kind": "other"}],
    [{"id": "../escape", "kind": "aiden"}],
    [{"kind": "aiden"}, {"kind": "aiden"}],
    [{"kind": "pi", "provider": "x", "model": "x", "command": "arbitrary"}],
    [{"kind": "aiden", "model": "silently-ignored"}],
])
def test_invalid_agent_configs_rejected(agents):
    with pytest.raises(ValueError):
        validate_agents(agents)


@pytest.mark.parametrize("api", ["openai-completions", "openai-responses", "anthropic-messages", "google-generative-ai"])
def test_custom_pi_endpoint_is_normalized_with_explicit_model_limits(api):
    agent = validate_agents([{"kind": "pi", "provider": "custom", "model": "vision",
                              "base_url": "https://gateway.example/v1/", "api": api,
                              "api_key_env": "MY_GATEWAY_KEY", "context_window": 32000, "max_tokens": 2000}])[0]
    assert agent["base_url"] == "https://gateway.example/v1"
    assert agent["api"] == api
    assert agent["context_window"] == 32000 and agent["max_tokens"] == 2000


@pytest.mark.parametrize("fields", [
    {"base_url": "file:///tmp/models.json"},
    {"base_url": "https://user:secret@gateway.example/v1"},
    {"base_url": "https://gateway.example/v1?api_key=secret"},
    {"base_url": "https://gateway.example/v1#secret"},
    {"base_url": "http://gateway.example:99999"},
    {"base_url": "http://gateway.example:0"},
    {"base_url": "https://gateway.example/with space"},
    {"base_url": "https://gateway.example", "api": "unknown"},
    {"base_url": "https://gateway.example", "api_key_env": "!cat secret"},
    {"base_url": "https://gateway.example", "api_key_env": "${KEY}"},
    {"base_url": "https://gateway.example", "context_window": True},
    {"base_url": "https://gateway.example", "max_tokens": 0},
    {"base_url": "https://gateway.example", "max_tokens": "1000"},
    {"base_url": "https://gateway.example", "context_window": 1000, "max_tokens": 2000},
    {"api": "openai-completions"},
    {"api_key": "do-not-store-raw-secrets"},
])
def test_invalid_custom_endpoint_settings_fail_before_execution(fields):
    with pytest.raises(ValueError):
        validate_agents([{"kind": "pi", "provider": "custom", "model": "vision", **fields}])


def test_custom_endpoint_requires_key_before_creating_experiment(tmp_path, monkeypatch):
    monkeypatch.delenv("AIDEN_BENCHMARK_PI_API_KEY", raising=False)
    manager = ExperimentManager(runs_dir=tmp_path, suites_dir=SUITE_PATH.parent,
                                base_config_dir=tmp_path, daemon_image="unused", build_daemon_image=False,
                                config_loader=lambda: "", judge_loader=lambda: {"api_key": "test-key"})
    with pytest.raises(ValueError, match="Set AIDEN_BENCHMARK_PI_API_KEY"):
        manager.start({"suite": SUITE_PATH.name, "agents": [{"kind": "pi", "provider": "custom", "model": "vision", "base_url": "http://127.0.0.1:9000/v1"}]})
    assert not list(manager.root.iterdir())


def test_manager_blocks_path_traversal_and_reports_interrupted_runs(tmp_path):
    manager = ExperimentManager(runs_dir=tmp_path, suites_dir=SUITE_PATH.parent,
                                base_config_dir=tmp_path, daemon_image="unused", build_daemon_image=False,
                                config_loader=lambda: "", judge_loader=lambda: {"api_key": "test-key"})
    with pytest.raises(ValueError, match="inside"):
        manager.start({"suite": "../manual.md", "agents": [{"kind": "aiden"}]})
    directory = manager.root / "exp-old"
    directory.mkdir()
    atomic_json(directory / "manifest.json", {"id": "exp-old", "status": "running"})
    assert manager.list()[0]["status"] == "interrupted"


def test_manager_freezes_suite_before_background_execution(tmp_path, monkeypatch):
    suite_path = tmp_path / "suite.json"
    suite_path.write_bytes(SUITE_PATH.read_bytes())
    entered, release = threading.Event(), threading.Event()
    captured = []
    def fake_run(self, suite, root, record, config_text, stop, judge_cfg, judge_key):
        entered.set()
        release.wait(5)
        captured.append(suite.source_bytes)
    monkeypatch.setattr(ExperimentManager, "_run", fake_run)
    manager = ExperimentManager(runs_dir=tmp_path / "runs", suites_dir=tmp_path,
                                base_config_dir=tmp_path, daemon_image="unused", build_daemon_image=False,
                                config_loader=lambda: "", judge_loader=lambda: {"api_key": "test-key"})
    try:
        manager.start({"suite": "suite.json", "environment_url": "http://env", "agents": [{"kind": "pi", "provider": "test", "model": "test"}]})
        assert entered.wait(3)
        suite_path.write_text("{}")
        with pytest.raises(ValueError):
            manager.start({"suite": "suite.json", "environment_url": "http://env", "agents": [{"kind": "aiden"}]})
    finally:
        release.set()
        manager.shutdown()
    assert captured == [SUITE_PATH.read_bytes()]


def test_original_suites_are_accepted_without_altering_tasks():
    from runner.suite import load_suite
    for name in ("mobilegym_basic.json", "mobilegym_scroll_regression.json", "mobilegym_scroll_sweep.json",
                 "phone_control_v1.json", "app_workflow_v1.json"):
        path = SUITE_PATH.parent / name
        assert comparison_suite(path) == load_suite(path)


def test_missing_judge_cannot_silently_pass_rubric_tasks(tmp_path):
    with pytest.raises(ValueError, match="Judge"):
        run_experiment(tiny_suite(), tmp_path, [], {}, 1, 0, threading.Event(), manifest())


@pytest.mark.parametrize("settings", [{"enabled": False}, {}])
def test_manager_requires_judge_before_launch(tmp_path, monkeypatch, settings):
    monkeypatch.delenv("AIDEN_BENCHMARK_JUDGE_API_KEY", raising=False)
    manager = ExperimentManager(runs_dir=tmp_path, suites_dir=SUITE_PATH.parent,
                                base_config_dir=tmp_path, daemon_image="unused", build_daemon_image=False,
                                config_loader=lambda: "", judge_loader=lambda: settings)
    with pytest.raises(ValueError, match="Judge"):
        manager.start({"suite": SUITE_PATH.name, "environment_url": "http://env", "agents": [{"kind": "aiden"}]})
    assert not list(manager.root.iterdir())


def test_incompatible_platform_keeps_planned_attempt_but_does_not_execute(tmp_path):
    suite = tiny_suite()
    suite.tasks[0].platforms = ["ios"]
    calls = []
    run_experiment(suite, tmp_path, [{"id": "pi", "kind": "pi"}], {"pi": FakeAgent(calls, "pi")},
                   1, 0, threading.Event(), manifest(), JudgeConfig())
    assert calls == []
    row = json.loads((tmp_path / "runs/pi/results.jsonl").read_text())
    assert row["status"] == "skipped" and row["metrics"]["failure_class"] == "platform"


def test_unsupported_suites_remain_visible_with_reason(tmp_path):
    manager = ExperimentManager(runs_dir=tmp_path, suites_dir=SUITE_PATH.parent,
                                base_config_dir=tmp_path, daemon_image="unused", build_daemon_image=False,
                                config_loader=lambda: "")
    suites = {item["key"]: item for item in manager.suites()}
    assert suites["mobilegym_basic.json"]["compatible"]
    assert not suites["memory_v1.json"]["compatible"]
    assert suites["memory_v1.json"]["reason"]


def test_running_attempt_is_visible_before_agent_returns(tmp_path):
    class ProgressAgent(FakeAgent):
        def execute_operation(self, prompt, timeout, artifacts, stop, task):
            record = json.loads((tmp_path / 'manifest.json').read_text())
            assert record['stage'] == 'Executing task'
            assert record['current_attempt']['agent_id'] == 'pi'
            assert record['current_attempt']['task_id'] == task.id
            assert record['current_attempt']['started_at']
            assert record.get('completed', 0) == 0
            return super().execute_operation(prompt, timeout, artifacts, stop, task)
    run_experiment(tiny_suite(), tmp_path, [{'id':'pi','kind':'pi'}], {'pi':ProgressAgent([], 'pi')},
                   1, 0, threading.Event(), manifest(), JudgeConfig())
    record = json.loads((tmp_path / 'manifest.json').read_text())
    assert record['completed'] == 1 and 'current_attempt' not in record
