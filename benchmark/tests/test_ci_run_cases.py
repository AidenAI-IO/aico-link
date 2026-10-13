from __future__ import annotations

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


@pytest.mark.parametrize("repeats", ["1", "3"])
def test_run_case_forwards_configured_repeat_count(monkeypatch, tmp_path, repeats):
    commands: list[list[str]] = []

    def fake_run(command, **kwargs):
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


def test_run_case_terminates_the_runner_process_group_on_timeout(
    monkeypatch,
    tmp_path: Path,
) -> None:
    case = _case("isolated", "isolated")
    captured: dict[str, object] = {}
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
        environment={},
        log_path=tmp_path / "case.log",
        timeout_seconds=1,
    )

    assert returncode == 1
    assert captured["kwargs"]["start_new_session"] is True
    assert captured["terminated"] is not None
    assert captured["cleanup_command"] == [
        "docker",
        "compose",
        "-f",
        str(tmp_path / "docker" / "docker-compose.agent-daemon.yml"),
        "-p",
        "aiden-benchmark-agent-ci-timeout-worker-token",
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
