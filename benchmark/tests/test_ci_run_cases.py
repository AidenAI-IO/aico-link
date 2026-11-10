from __future__ import annotations

import json
import subprocess
import threading
from pathlib import Path

import pytest

from ci import run_case as run_case_module
from ci import run_cases as run_cases_module
from ci.plan import SuiteCase


def _case(case_id: str, environment: str) -> SuiteCase:
    return SuiteCase(
        id=case_id,
        suite=f"suites/{case_id}.json",
        environment=environment,
        target_platform="android" if environment == "mobilegym" else "auto",
    )


def test_mobilegym_environment_uses_the_selected_side_image(monkeypatch):
    captured = []
    def fake_json(command, **kwargs):
        captured.append(command)
        return {"environment_url": "http://localhost:1234"}
    monkeypatch.setattr(run_case_module, "_run_json", fake_json)
    for side in ("baseline", "candidate"):
        run_case_module._environment_url(_case("phone", "mobilegym"), run_id=side,
            environment={"MOBILEGYM_IMAGE": f"private:{side}", "BENCHMARK_CI_IMAGES_PREPARED": "1"})
    assert [c[c.index("--mobilegym-image") + 1] for c in captured] == ["private:baseline", "private:candidate"]


@pytest.mark.parametrize("repeats", ["1", "3"])
def test_run_case_forwards_configured_repeat_count(monkeypatch, tmp_path, repeats):
    commands: list[list[str]] = []

    def fake_run(command, **kwargs):
        assert kwargs["env"]["BENCHMARK_EXECUTION_ENVIRONMENT"] == "isolated"
        commands.append(command)
        return subprocess.CompletedProcess(command, 0)

    monkeypatch.setattr(run_case_module, "BENCHMARK_ROOT", tmp_path)
    monkeypatch.setattr(run_case_module, "_environment_url", lambda *args, **kwargs: ("", None))
    monkeypatch.setattr(run_case_module.subprocess, "run", fake_run)
    monkeypatch.setattr(run_case_module, "_effective_exit_code", lambda code, path: code)

    assert run_case_module.run_case(
        _case("isolated", "isolated"),
        run_id="ci-repeats",
        environment={"BENCHMARK_REPEATS": repeats},
    ) == 0

    assert len(commands) == 1
    assert commands[0][-2:] == ["--repeats", repeats]


def test_run_cases_starts_mobilegym_together_without_prepopulating_run_dirs(
    monkeypatch,
    tmp_path: Path,
) -> None:
    cases = (
        _case("isolated-first", "isolated"),
        _case("mobilegym-one", "mobilegym"),
        _case("isolated-second", "isolated"),
        _case("mobilegym-two", "mobilegym"),
    )
    started: list[str] = []
    started_lock = threading.Lock()
    first_pair_started = threading.Event()

    def fake_run_case(case, *, run_id, environment, log_path, timeout_seconds):
        target_run_dir = tmp_path / "runs" / run_id
        assert Path(log_path).parent != target_run_dir
        Path(log_path).parent.mkdir(parents=True, exist_ok=True)
        Path(log_path).write_text(f"log for {case.id}\n", encoding="utf-8")
        with started_lock:
            started.append(case.id)
            if len(started) == 2:
                first_pair_started.set()
        assert first_pair_started.wait(timeout=1)
        return 0

    monkeypatch.setattr(run_cases_module, "BENCHMARK_ROOT", tmp_path)
    monkeypatch.setattr(run_cases_module, "run_case", fake_run_case)

    outcomes = run_cases_module.run_cases(
        cases,
        run_id_prefix="ci-test",
        environment={},
        max_parallel=2,
    )

    assert set(started[:2]) == {"mobilegym-one", "mobilegym-two"}
    assert [outcome.case_id for outcome in outcomes] == [case.id for case in cases]
    for outcome in outcomes:
        assert outcome.log_path == tmp_path / "runs" / outcome.run_id / "ci-case.log"
        assert outcome.log_path.read_text(encoding="utf-8") == (
            f"log for {outcome.case_id}\n"
        )


def test_run_cases_emits_an_annotation_for_an_interrupted_run(
    monkeypatch,
    tmp_path: Path,
    capsys,
) -> None:
    case = _case("interrupted", "isolated")

    monkeypatch.setattr(run_cases_module, "BENCHMARK_ROOT", tmp_path)
    monkeypatch.setattr(run_cases_module, "run_case", lambda *args, **kwargs: 1)

    outcomes = run_cases_module.run_cases(
        (case,),
        run_id_prefix="ci-test",
        environment={},
        max_parallel=1,
    )

    assert outcomes[0].error_summary == "manifest_missing, runner_exit=1"
    assert (
        "::error title=Benchmark case interrupted::manifest_missing, runner_exit=1"
        in capsys.readouterr().out
    )


@pytest.mark.parametrize("ready", ["0", "1", None])
def test_failed_aidenbridge_setup_loses_only_bridge_cases(monkeypatch, tmp_path: Path, ready) -> None:
    bridge = SuiteCase(id="bridge", suite="suites/bridge.json", environment="adb",
                       target_platform="android", android_phone_bridge=True)
    suite_path = tmp_path / bridge.suite
    suite_path.parent.mkdir(parents=True)
    suite_path.write_text(json.dumps({"name": "bridge", "tasks": [
        {"id": "selected", "category": "single_step", "prompt": "Act", "description_for_judge": "Act",
         "rubric": [{"id": "done", "check": "Action completed"}]},
        {"id": "unselected", "category": "single_step", "prompt": "Act", "description_for_judge": "Act",
         "rubric": [{"id": "done", "check": "Action completed"}]},
    ]}))
    bridge = SuiteCase(**{**bridge.matrix_entry(), "task_ids": ["selected"]})
    started: list[str] = []
    monkeypatch.setattr(run_cases_module, "BENCHMARK_ROOT", tmp_path)
    monkeypatch.setattr(run_cases_module, "run_case", lambda case, **kwargs: started.append(case.id) or 0)

    outcomes = run_cases_module.run_cases(
        (bridge, _case("plain-adb", "adb"), _case("memory", "isolated")),
        run_id_prefix="ci-test",
        environment={"BENCHMARK_REPEATS": "2", **({} if ready is None else {"AIDENBRIDGE_READY": ready})},
        max_parallel=1,
    )

    by_id = {outcome.case_id: outcome for outcome in outcomes}
    if ready == "0":
        assert sorted(started) == ["memory", "plain-adb"]
        assert not by_id["bridge"].ok and by_id["bridge"].error_summary == "aidenbridge_setup_failed"
        from ci.fetch_langfuse_history import _read_local_runs
        from ci.generate_dashboard import _group_into_runs
        from ci.weekly_report import _totals
        from runner.langfuse_reporter import _load_run_artifacts, _experiment_metadata, _run_scores
        run_dir = tmp_path / "runs" / by_id["bridge"].run_id
        artifacts = _load_run_artifacts(run_dir)
        assert [(row["task_id"], row["attempt"]) for row in artifacts.results] == [
            ("selected", 1), ("selected", 2)]
        assert all(row["status"] == "skipped" and row["metrics"]["agent_eligible"] is False
                   and row["metrics"]["failure_class"] == "environment" for row in artifacts.results)
        assert _experiment_metadata(artifacts)["execution_environment"] == "adb_aidenbridge"
        assert {s["name"]: s["value"] for s in _run_scores(artifacts.metrics["aggregate"])}[
            "coverage.agent_eligible_attempts"] == 0
        warnings = []
        records = _read_local_runs(tmp_path / "runs", "ci-test", None, warnings)
        assert len(records) == 1
        assert records[0]["missing_metrics"]
        healthy = {**records[0], "run_id": "healthy", "missing_metrics": [],
                   "eligible_tasks": 1, "passed": 1, "pass_at_1": 1.0}
        records[0]["workflow_run_id"] = healthy["workflow_run_id"] = "123"
        run, = _group_into_runs([healthy, records[0]])
        assert run["metrics_complete"] is False and run["pass_at_1"] is None
        assert _totals([healthy, records[0]])["pass_at_1"] is None
    else:
        assert sorted(started) == ["bridge", "memory", "plain-adb"]
    assert by_id["plain-adb"].ok and by_id["memory"].ok


def test_setup_report_failure_does_not_abort_independent_cases(monkeypatch, tmp_path):
    bridge = SuiteCase(id="broken", suite="broken.json", environment="adb",
                       android_phone_bridge=True)
    (tmp_path / bridge.suite).write_text(json.dumps({"tasks": [
        {"id": "broken", "category": []},
    ]}))
    monkeypatch.setattr(run_cases_module, "BENCHMARK_ROOT", tmp_path)
    started = []
    monkeypatch.setattr(run_cases_module, "run_case",
                        lambda case, **kwargs: started.append(case.id) or 0)
    outcomes = run_cases_module.run_cases(
        (bridge, _case("memory", "isolated")), run_id_prefix="ci-test",
        environment={"AIDENBRIDGE_READY": "0"}, max_parallel=1,
    )
    assert started == ["memory"]
    assert not outcomes[0].ok
    assert outcomes[0].error_summary == "aidenbridge_setup_failed, setup_report_failed"
    assert outcomes[1].ok


@pytest.mark.parametrize("bridge_enabled", [False, True])
@pytest.mark.parametrize("cleanup_fails", [False, True])
def test_run_case_terminates_the_runner_process_group_on_timeout(
    monkeypatch,
    tmp_path: Path,
    bridge_enabled: bool,
    cleanup_fails: bool,
) -> None:
    case = SuiteCase(
        id="timeout", suite="suites/timeout.json",
        environment="adb" if bridge_enabled else "isolated",
        android_phone_bridge=bridge_enabled,
    )
    captured: dict[str, object] = {}
    cleanup_events = []

    def remove_network(serial):
        assert captured.get("terminated") is not None
        cleanup_events.append(("network", serial))
        if cleanup_fails:
            raise RuntimeError("emulator unavailable")

    monkeypatch.setattr(run_case_module, "remove_board_network", remove_network)
    monkeypatch.setattr(run_case_module, "_environment_url", lambda *args, **kwargs: ("", None))
    monkeypatch.setattr(run_case_module, "_stop_service", lambda service: cleanup_events.append("service"))
    worker_dir = tmp_path / "runs" / "ci-timeout" / "workers" / "worker-token"
    worker_dir.mkdir(parents=True)

    class FakeProcess:
        pid = 4321

        def wait(self, timeout=None):
            raise subprocess.TimeoutExpired(["runner"], timeout)

    def fake_popen(command, **kwargs):
        captured["command"] = command
        captured["kwargs"] = kwargs
        return FakeProcess()

    def fake_terminate(process):
        captured["terminated"] = process

    monkeypatch.setattr(run_case_module, "BENCHMARK_ROOT", tmp_path)
    monkeypatch.setattr(run_case_module.subprocess, "Popen", fake_popen)
    monkeypatch.setattr(
        run_case_module.subprocess,
        "run",
        lambda command, **kwargs: captured.setdefault("cleanup_command", command)
        or subprocess.CompletedProcess(command, 0),
    )
    monkeypatch.setattr(
        run_case_module,
        "terminate_process_tree",
        fake_terminate,
        raising=False,
    )

    returncode = run_case_module.run_case(
        case,
        run_id="ci-timeout",
        environment={"ANDROID_SERIAL": "emulator-5554"},
        log_path=tmp_path / "case.log",
        timeout_seconds=1,
    )

    assert returncode == 1
    assert captured["kwargs"]["start_new_session"] is True
    assert captured["terminated"] is not None
    assert cleanup_events == (
        [("network", "emulator-5554"), "service"] if bridge_enabled else ["service"]
    )
    assert captured["cleanup_command"] == [
        "docker",
        "compose",
        "-f",
        str(tmp_path / "docker" / "docker-compose.agent-daemon.yml"),
        "-p",
        "aico-benchmark-agent-ci-timeout-worker-token",
        "down",
        "--volumes",
        "--remove-orphans",
    ]


def test_run_json_terminates_environment_startup_process_group_on_timeout(
    monkeypatch,
) -> None:
    captured: dict[str, object] = {}

    class FakeProcess:
        pid = 4321
        returncode = None

        def communicate(self, timeout=None):
            captured["timeout"] = timeout
            raise subprocess.TimeoutExpired(["runner", "start-mobilegym-env"], timeout)

    def fake_popen(command, **kwargs):
        captured["command"] = command
        captured["kwargs"] = kwargs
        return FakeProcess()

    def fake_terminate(process):
        captured["terminated"] = process

    monkeypatch.setattr(run_case_module.subprocess, "Popen", fake_popen)
    monkeypatch.setattr(run_case_module, "terminate_process_tree", fake_terminate)

    with pytest.raises(subprocess.TimeoutExpired):
        run_case_module._run_json(
            ["runner", "start-mobilegym-env"],
            environment={},
            timeout_seconds=1,
        )

    assert captured["timeout"] == 1
    assert captured["kwargs"]["start_new_session"] is True
    assert captured["terminated"] is not None
