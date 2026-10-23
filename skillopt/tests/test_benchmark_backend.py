from __future__ import annotations

import dataclasses as dc
import json
import subprocess
from pathlib import Path

import pytest

from runner.models import TaskResult
from runner.suite import HardAssertions, Suite, TaskSpec


def _write_base_config(root: Path) -> Path:
    config = root / "config"
    (config / "skills" / "device-operator").mkdir(parents=True)
    (config / "agent.toml").write_text("[model]\nname = 'test'\n", encoding="utf-8")
    (config / "skills" / "device-operator" / "SKILL.md").write_text("base skill", encoding="utf-8")
    return config


def _write_shared_skills(tmp_path: Path) -> Path:
    shared = tmp_path / "shared-skills"
    (shared / "device-operator").mkdir(parents=True)
    (shared / "device-operator" / "SKILL.md").write_text("shared skill", encoding="utf-8")
    return shared


def _suite(tmp_path: Path) -> Suite:
    suite_path = tmp_path / "skillopt" / "suites" / "device-operator" / "device_operator_train.json"
    suite_path.parent.mkdir(parents=True)
    suite_path.write_text(json.dumps({"name": "device_operator_train", "tasks": []}), encoding="utf-8")
    return Suite(
        name="device_operator_train",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=suite_path,
    )


def _task(task_id: str = "case_one") -> TaskSpec:
    return TaskSpec(
        id=task_id,
        category="single_step",
        description_for_judge="desc",
        prompt="prompt",
        rubric=[],
        hard_assertions=HardAssertions(),
    )


def test_benchmark_runner_backend_invokes_current_runner_and_reads_rollouts(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    from skillopt.benchmark_backend import BenchmarkRunnerBackend

    benchmark_root = tmp_path / "benchmark"
    base_config = _write_base_config(benchmark_root)
    shared_skills = _write_shared_skills(tmp_path)
    captured: dict[str, object] = {}

    def fake_run(cmd, cwd=None, env=None, text=None, stdout=None, stderr=None, check=None):
        captured["cmd"] = list(cmd)
        captured["cwd"] = cwd
        out_dir = Path(cmd[cmd.index("--out") + 1])
        run_id = cmd[cmd.index("--run-id") + 1]
        child_run = out_dir / run_id
        child_run.mkdir(parents=True)
        row = TaskResult(
            suite="device_operator_train",
            run_id=run_id,
            task_id="case_one",
            category="single_step",
            attempt=1,
            status="passed",
            rubric=[],
            rubric_pass_count=0,
            rubric_total=0,
            metrics={"tool_calls": 2},
            artifact_dir=str(child_run / "tasks" / "case_one"),
            description_for_judge="desc",
        )
        (child_run / "results.jsonl").write_text(json.dumps(dc.asdict(row)) + "\n", encoding="utf-8")
        (child_run / "report.html").write_text("<html>report</html>", encoding="utf-8")
        return subprocess.CompletedProcess(cmd, 0)

    monkeypatch.setattr(subprocess, "run", fake_run)

    backend = BenchmarkRunnerBackend(
        benchmark_root=benchmark_root,
        base_config_dir=base_config,
        shared_skills_dir=shared_skills,
        environment_url="http://127.0.0.1:50196",
        daemon_image="aico-agent-daemon:local",
        build_daemon_image=False,
        agent_config_path=tmp_path / "agent.toml",
    )

    rollouts = backend.run_rollout(
        suite=_suite(tmp_path),
        tasks=[_task()],
        skill_name="device-operator",
        skill_path=shared_skills / "device-operator" / "SKILL.md",
        skill_text="candidate skill",
        phase="step_01_train",
        run_id="skillopt-run",
        run_root=tmp_path / "runs" / "skillopt-run",
        judge_cfg=None,
    )

    cmd = captured["cmd"]
    assert captured["cwd"] == benchmark_root
    assert "--auto-agent-setup" in cmd
    assert cmd[cmd.index("--environment-url") + 1] == "http://127.0.0.1:50196"
    assert cmd[cmd.index("--daemon-image") + 1] == "aico-agent-daemon:local"
    assert cmd[cmd.index("--agent-config") + 1] == str(tmp_path / "agent.toml")
    assert "--no-build-daemon-image" in cmd
    assert "--no-judge" in cmd
    assert cmd[cmd.index("--skill") + 1] == "device-operator"
    assert cmd[cmd.index("--task-id") + 1] == "case_one"
    phase_config = Path(cmd[cmd.index("--base-config-dir") + 1])
    assert (phase_config / "skills" / "device-operator" / "SKILL.md").read_text(encoding="utf-8") == "candidate skill"
    assert rollouts[0].id == "case_one"
    assert rollouts[0].hard == 1
    assert rollouts[0].n_turns == 2
    assert rollouts[0].extras["benchmark_run_id"] == "skillopt-run-step_01_train"


def test_benchmark_runner_backend_rejects_skill_name_escape(tmp_path: Path):
    from skillopt.benchmark_backend import BenchmarkRunnerBackend

    benchmark_root = tmp_path / "benchmark"
    base_config = _write_base_config(benchmark_root)
    shared_skills = _write_shared_skills(tmp_path)
    backend = BenchmarkRunnerBackend(
        benchmark_root=benchmark_root,
        base_config_dir=base_config,
        shared_skills_dir=shared_skills,
        environment_url="http://127.0.0.1:50196",
    )

    with pytest.raises(ValueError, match="invalid skill_name"):
        backend.prepare_phase_config(tmp_path / "phase-config", "../evil", "candidate skill")

    assert not (tmp_path / "evil" / "SKILL.md").exists()


def test_mobilegym_runner_command_does_not_cap_environment_concurrency(tmp_path: Path):
    from skillopt.benchmark_backend import BenchmarkRunnerBackend

    benchmark_root = tmp_path / "benchmark"
    base_config = _write_base_config(benchmark_root)
    shared_skills = _write_shared_skills(tmp_path)
    backend = BenchmarkRunnerBackend(
        benchmark_root=benchmark_root,
        base_config_dir=base_config,
        shared_skills_dir=shared_skills,
        environment_url="http://127.0.0.1:50196",
        environment_profile="mobilegym",
    )

    cmd = backend._runner_command(
        suite=_suite(tmp_path),
        tasks=[_task()],
        skill_name="device-operator",
        child_runs_root=tmp_path / "children",
        child_run_id="child-run",
        phase_config=tmp_path / "phase-config",
        judge_cfg=None,
    )

    assert "--max-concurrency" not in cmd


def test_benchmark_runner_backend_applies_mobilegym_profile(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    from skillopt import benchmark_backend
    from skillopt.benchmark_backend import BenchmarkRunnerBackend

    benchmark_root = tmp_path / "benchmark"
    base_config = _write_base_config(benchmark_root)
    shared_skills = _write_shared_skills(tmp_path)
    profiles_root = tmp_path / "profiles"
    profile_dir = profiles_root / "mobilegym" / "device-operator"
    profile_dir.mkdir(parents=True)
    (profile_dir / "SKILL.md").write_text("MobileGym simulator profile", encoding="utf-8")
    monkeypatch.setattr(benchmark_backend, "PROFILES_ROOT", profiles_root)
    backend = BenchmarkRunnerBackend(
        benchmark_root=benchmark_root,
        base_config_dir=base_config,
        shared_skills_dir=shared_skills,
        environment_url="http://127.0.0.1:50196",
        environment_profile="mobilegym",
    )

    dest = backend.prepare_phase_config(tmp_path / "phase-config", "device-operator", "candidate skill")

    skill = (dest / "skills" / "device-operator" / "SKILL.md").read_text(encoding="utf-8")
    assert "candidate skill" in skill
    assert "MobileGym simulator profile" in skill
    agent_config = (dest / "agent.toml").read_text(encoding="utf-8")
    assert "force_simple_loop" not in agent_config

def test_mobilegym_profile_is_inserted_before_long_base_skill_body(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    from skillopt import benchmark_backend
    from skillopt.benchmark_backend import apply_environment_profile

    profiles_root = tmp_path / "profiles"
    profile_dir = profiles_root / "mobilegym" / "device-operator"
    profile_dir.mkdir(parents=True)
    (profile_dir / "SKILL.md").write_text("## MobileGym Environment Profile\n\nDo not use frame service recovery.", encoding="utf-8")
    monkeypatch.setattr(benchmark_backend, "PROFILES_ROOT", profiles_root)
    base_skill = "---\nname: device-operator\n---\n\n## Core Loop\n\n" + ("filler\n" * 800) + "## Screenshot Failure Recovery\n"

    skill = apply_environment_profile(base_skill, "device-operator", "mobilegym")

    assert skill.startswith("---\nname: device-operator\n---\n")
    assert skill.index("## MobileGym Environment Profile") < skill.index("## Core Loop")
    assert skill.index("## MobileGym Environment Profile") < skill.index("## Screenshot Failure Recovery")


def test_load_benchmark_task_results_handles_failed_runner_exit_with_results(tmp_path: Path):
    from skillopt.benchmark_backend import load_benchmark_task_results

    run_dir = tmp_path / "child"
    run_dir.mkdir()
    row = TaskResult(
        suite="device_operator_train",
        run_id="child",
        task_id="case_one",
        category="single_step",
        attempt=1,
        status="failed",
        rubric=[],
        rubric_pass_count=0,
        rubric_total=0,
        metrics={"tool_calls": 1},
        description_for_judge="desc",
    )
    (run_dir / "results.jsonl").write_text(json.dumps(dc.asdict(row)) + "\n", encoding="utf-8")

    results = load_benchmark_task_results(run_dir)

    assert results[0].task_id == "case_one"
    assert results[0].status == "failed"
    assert results[0].metrics["tool_calls"] == 1


def test_load_benchmark_task_results_ignores_unknown_hard_assertion_fields(tmp_path: Path):
    from skillopt.benchmark_backend import load_benchmark_task_results

    run_dir = tmp_path / "child"
    run_dir.mkdir()
    row = {
        "suite": "crossapp_train",
        "run_id": "child",
        "task_id": "case_one",
        "category": "multi_step",
        "attempt": 1,
        "status": "failed",
        "rubric": [],
        "rubric_pass_count": 0,
        "rubric_total": 0,
        "hard_assertions": {
            "timeout": True,
            "response_exists": True,
            "future_runner_field": False,
        },
        "hard_assertion_failures": [],
    }
    (run_dir / "results.jsonl").write_text(json.dumps(row) + "\n", encoding="utf-8")

    results = load_benchmark_task_results(run_dir)

    assert results[0].task_id == "case_one"
    assert results[0].hard_assertions is not None
    assert results[0].hard_assertions.timeout is True


def test_bare_docker_mention_is_not_environment_setup_failure():
    from skillopt.benchmark_backend import is_environment_setup_failure

    result = TaskResult(
        suite="device_operator_train",
        run_id="child",
        task_id="case_one",
        category="single_step",
        attempt=1,
        status="skipped",
        rubric=[],
        rubric_pass_count=0,
        rubric_total=0,
        metrics={"error": "setup: task instructions mention Docker Desktop, but setup completed"},
        description_for_judge="desc",
    )

    assert is_environment_setup_failure(result) is False


def test_benchmark_runner_backend_rejects_environment_setup_failure_phase(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    from skillopt.benchmark_backend import BenchmarkRolloutError, BenchmarkRunnerBackend

    benchmark_root = tmp_path / "benchmark"
    base_config = _write_base_config(benchmark_root)
    shared_skills = _write_shared_skills(tmp_path)

    def fake_run(cmd, cwd=None, env=None, text=None, stdout=None, stderr=None, check=None):
        out_dir = Path(cmd[cmd.index("--out") + 1])
        run_id = cmd[cmd.index("--run-id") + 1]
        child_run = out_dir / run_id
        child_run.mkdir(parents=True)
        rows = []
        for task_id in ("case_one", "case_two"):
            rows.append(TaskResult(
                suite="device_operator_train",
                run_id=run_id,
                task_id=task_id,
                category="single_step",
                attempt=1,
                status="skipped",
                rubric=[],
                rubric_pass_count=0,
                rubric_total=0,
                metrics={"error": "setup: setup endpoint timed out: timed out"},
                artifact_dir=str(child_run / "tasks" / task_id),
                description_for_judge="desc",
            ))
        (child_run / "results.jsonl").write_text("".join(json.dumps(dc.asdict(row)) + "\n" for row in rows), encoding="utf-8")
        return subprocess.CompletedProcess(cmd, 1, stdout="setup failed")

    monkeypatch.setattr(subprocess, "run", fake_run)
    backend = BenchmarkRunnerBackend(
        benchmark_root=benchmark_root,
        base_config_dir=base_config,
        shared_skills_dir=shared_skills,
        environment_url="http://127.0.0.1:50196",
        build_daemon_image=False,
    )

    with pytest.raises(BenchmarkRolloutError, match="environment setup failed") as exc_info:
        backend.run_rollout(
            suite=_suite(tmp_path),
            tasks=[_task("case_one"), _task("case_two")],
            skill_name="device-operator",
            skill_path=shared_skills / "device-operator" / "SKILL.md",
            skill_text="candidate skill",
            phase="step_01_train",
            run_id="skillopt-run",
            run_root=tmp_path / "runs" / "skillopt-run",
            judge_cfg=None,
        )

    assert [rollout.id for rollout in exc_info.value.rollouts] == ["case_one", "case_two"]
    assert exc_info.value.rollouts[0].extras["benchmark_run_id"] == "skillopt-run-step_01_train"
    assert "setup endpoint timed out" in exc_info.value.rollouts[0].fail_reason
