from pathlib import Path
import io
import json
import logging
import subprocess
import threading
import tomllib

import pytest

from skillopt import webui
from skillopt.webui import INDEX_HTML, SkillOptJob, SkillOptWebApp, SkillOptWebUIConfig, build_skillopt_command


def test_build_skillopt_command_uses_bridge_backend_options(tmp_path: Path):
    cmd = build_skillopt_command(
        {
            "skill": "device-operator",
            "backend": "mobilegym",
            "environment_url": "http://127.0.0.1:50196",
            "train_suite": "skillopt/device-operator/device_operator_train",
            "validation_suite": "skillopt/device-operator/device_operator_verification",
            "budget": 3,
            "edit_budget": 2,
            "daemon_image": "aico-agent-daemon:local",
            "agent_config": str(tmp_path / "agent.toml"),
            "no_build_daemon_image": True,
            "no_judge": True,
        },
        run_id="skillopt-web-run",
        artifact_root=tmp_path,
    )

    assert cmd[:3] == [cmd[0], "-m", "skillopt"]
    assert cmd[cmd.index("--backend") + 1] == "mobilegym"
    assert cmd[cmd.index("--environment-url") + 1] == "http://127.0.0.1:50196"
    assert cmd[cmd.index("--daemon-image") + 1] == "aico-agent-daemon:local"
    assert cmd[cmd.index("--agent-config") + 1] == str(tmp_path / "agent.toml")
    assert "--no-build-daemon-image" in cmd
    assert "--no-judge" in cmd
    assert cmd[cmd.index("--run-id") + 1] == "skillopt-web-run"
    assert cmd[cmd.index("--output") + 1] == str(tmp_path / "skillopt-web-run" / "best_skill.md")


def test_default_webui_budget_is_five(tmp_path: Path):
    cmd = build_skillopt_command(
        {
            "skill": "device-operator",
            "backend": "mobilegym",
            "train_suite": "skillopt/device-operator/device_operator_train",
            "validation_suite": "skillopt/device-operator/device_operator_verification",
        },
        run_id="skillopt-web-run",
        artifact_root=tmp_path,
    )

    assert cmd[cmd.index("--budget") + 1] == "5"
    assert webui._default_webui_settings()["skillopt"]["budget"] == 5
    assert webui._normalize_webui_settings({})["skillopt"]["budget"] == 5
    assert 'id="budget" type="number" min="1" step="1" value="5"' in INDEX_HTML
    assert '<label for="budget">Max iterations</label>' in INDEX_HTML
    assert '<span class="muted">Max iterations</span><strong id="mBudget">10</strong>' in INDEX_HTML
    assert '<label for="budget">Budget</label>' not in INDEX_HTML
    assert '<label for="editBudget">Max edits / iteration</label>' in INDEX_HTML
    assert '<span class="muted">Max edits / iteration</span><strong id="mEditBudget">4</strong>' in INDEX_HTML
    assert '<label for="editBudget">Edit budget</label>' not in INDEX_HTML


def test_webui_model_settings_default_to_agent_config_fallback():
    settings = webui._default_webui_settings()
    normalized = webui._normalize_webui_settings({})

    assert settings["judge"]["model"] == ""
    assert settings["skillopt"]["optimizer_model"] == ""
    assert normalized["judge"]["model"] == ""
    assert normalized["skillopt"]["optimizer_model"] == ""


def test_save_webui_settings_persists_optimizer_model(tmp_path: Path):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))

    settings = app.save_webui_settings({"skillopt": {"optimizer_model": "model-a, model-b"}})

    assert settings["skillopt"]["optimizer_model"] == "model-a, model-b"


def test_load_historical_jobs_logs_malformed_job_dirs(tmp_path: Path, monkeypatch, caplog):
    runs_dir = tmp_path / "runs"
    job_dir = runs_dir / "skillopt-bad"
    job_dir.mkdir(parents=True)
    (job_dir / "skillopt.log").write_text("$ python -m skillopt\n", encoding="utf-8")

    def fail_reconstruct(self, job_dir):
        raise json.JSONDecodeError("bad json", "{", 0)

    monkeypatch.setattr(SkillOptWebApp, "_reconstruct_job_from_disk", fail_reconstruct)

    with caplog.at_level(logging.WARNING, logger="skillopt.webui"):
        app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=runs_dir))

    assert app.list_jobs() == []
    assert "skipping malformed job dir skillopt-bad" in caplog.text


def test_stop_job_terminates_process_group(tmp_path: Path, monkeypatch):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    job = SkillOptJob(
        id="job-1",
        command=["python", "-m", "skillopt"],
        log_path=str(tmp_path / "runs" / "job-1" / "skillopt.log"),
        run_dir=str(tmp_path / "runs" / "job-1"),
    )

    class FakeProcess:
        pid = 4321

        def poll(self):
            return None

        def terminate(self):
            raise AssertionError("terminate should not be used before process-group signal")

    job.process = FakeProcess()
    app._jobs[job.id] = job
    killed = []
    monkeypatch.setattr("skillopt.webui.os.name", "posix")
    monkeypatch.setattr("skillopt.webui.os.killpg", lambda pid, sig: killed.append((pid, sig)))

    payload = app.stop_job("job-1")

    assert payload["status"] == "stopping"
    assert killed == [(4321, 15)]


def test_run_job_does_not_spawn_after_pre_start_stop(tmp_path: Path, monkeypatch):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    run_dir = tmp_path / "runs" / "job-1"
    run_dir.mkdir(parents=True)
    job = SkillOptJob(
        id="job-1",
        command=["python", "-m", "skillopt"],
        log_path=str(run_dir / "skillopt.log"),
        run_dir=str(run_dir),
        status="stopping",
    )

    def fail_popen(*args, **kwargs):
        raise AssertionError("stopped job should not spawn a process")

    monkeypatch.setattr(subprocess, "Popen", fail_popen)

    app._run_job(job)

    assert job.status == "stopped"
    assert job.exit_code is None


def test_stop_job_leaves_completed_job_status_unchanged(tmp_path: Path):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    job = SkillOptJob(
        id="job-1",
        command=["python", "-m", "skillopt"],
        log_path=str(tmp_path / "runs" / "job-1" / "skillopt.log"),
        run_dir=str(tmp_path / "runs" / "job-1"),
        status="passed",
        exit_code=0,
    )
    app._jobs[job.id] = job

    payload = app.stop_job("job-1")

    assert payload["status"] == "passed"
    assert payload["exit_code"] == 0


def test_job_payload_omits_live_process_without_deepcopying_it(tmp_path: Path):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    job = SkillOptJob(
        id="job-1",
        command=["python", "-m", "skillopt"],
        log_path=str(tmp_path / "runs" / "job-1" / "skillopt.log"),
        run_dir=str(tmp_path / "runs" / "job-1"),
        status="running",
    )

    class ProcessLike:
        def __init__(self):
            self.lock = threading.Lock()

    job.process = ProcessLike()

    payload = app._job_payload(job)

    assert payload["id"] == "job-1"
    assert payload["status"] == "running"
    assert "process" not in payload


def test_job_payload_hides_missing_report_url(tmp_path: Path):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    run_dir = tmp_path / "runs" / "job-1"
    run_dir.mkdir(parents=True)
    log_path = run_dir / "skillopt.log"
    log_path.write_text("", encoding="utf-8")
    job = SkillOptJob(
        id="job-1",
        command=["python", "-m", "skillopt"],
        log_path=str(log_path),
        run_dir=str(run_dir),
        status="failed",
        report_url="/runs/job-1/report.html",
    )

    payload = app._job_payload(job)

    assert payload["report_url"] == ""


def test_run_job_flushes_command_before_process_wait(tmp_path: Path, monkeypatch):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    run_dir = tmp_path / "runs" / "job-1"
    run_dir.mkdir(parents=True)
    job = SkillOptJob(
        id="job-1",
        command=["python", "-m", "skillopt"],
        log_path=str(run_dir / "skillopt.log"),
        run_dir=str(run_dir),
    )
    observed = {}

    class FakeProcess:
        pid = 4321

        def __init__(self, *args, **kwargs):
            observed["log_during_spawn"] = Path(job.log_path).read_text(encoding="utf-8")
            observed["python_unbuffered"] = kwargs["env"].get("PYTHONUNBUFFERED")

        def poll(self):
            return 0

        def wait(self):
            return 0

    monkeypatch.setattr(subprocess, "Popen", FakeProcess)

    app._run_job(job)

    assert observed["log_during_spawn"].startswith("$ python -m skillopt\n")
    assert observed["python_unbuffered"] == "1"


def test_run_job_uses_agent_config_api_key_as_openrouter_fallback(tmp_path: Path, monkeypatch):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    run_dir = tmp_path / "runs" / "job-1"
    run_dir.mkdir(parents=True)
    agent_config = run_dir / "agent.toml"
    agent_config.write_text(
        '[model]\nprovider = "openrouter"\napi_key = "sk-agent-fallback"\n',
        encoding="utf-8",
    )
    job = SkillOptJob(
        id="job-1",
        command=["python", "-m", "skillopt", "--agent-config", str(agent_config)],
        log_path=str(run_dir / "skillopt.log"),
        run_dir=str(run_dir),
    )
    observed = {}

    class FakeProcess:
        pid = 4321

        def __init__(self, *args, **kwargs):
            observed["openrouter_api_key"] = kwargs["env"].get("OPENROUTER_API_KEY")
            observed["judge_api_key"] = kwargs["env"].get("AICO_BENCHMARK_JUDGE_API_KEY")

        def poll(self):
            return 0

        def wait(self):
            return 0

    monkeypatch.delenv("OPENROUTER_API_KEY", raising=False)
    monkeypatch.setattr(subprocess, "Popen", FakeProcess)

    app._run_job(job)

    assert observed["openrouter_api_key"] == "sk-agent-fallback"
    assert observed["judge_api_key"] == "sk-agent-fallback"


def test_run_job_prefers_explicit_judge_api_key_over_agent_config(tmp_path: Path, monkeypatch):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    run_dir = tmp_path / "runs" / "job-1"
    run_dir.mkdir(parents=True)
    agent_config = run_dir / "agent.toml"
    agent_config.write_text(
        '[model]\nprovider = "openrouter"\napi_key = "sk-agent-fallback"\n',
        encoding="utf-8",
    )
    job = SkillOptJob(
        id="job-1",
        command=["python", "-m", "skillopt", "--agent-config", str(agent_config)],
        log_path=str(run_dir / "skillopt.log"),
        run_dir=str(run_dir),
    )
    app._job_judge_api_keys[job.id] = "sk-explicit-judge"
    observed = {}

    class FakeProcess:
        pid = 4321

        def __init__(self, *args, **kwargs):
            observed["openrouter_api_key"] = kwargs["env"].get("OPENROUTER_API_KEY")
            observed["judge_api_key"] = kwargs["env"].get("AICO_BENCHMARK_JUDGE_API_KEY")

        def poll(self):
            return 0

        def wait(self):
            return 0

    monkeypatch.setattr(subprocess, "Popen", FakeProcess)

    app._run_job(job)

    assert observed["openrouter_api_key"] == "sk-explicit-judge"
    assert observed["judge_api_key"] == "sk-explicit-judge"


def test_running_job_payload_reports_benchmark_artifact_progress(tmp_path: Path):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    run_dir = tmp_path / "runs" / "skillopt-job"
    phase_dir = run_dir / "benchmark" / "skillopt-job-baseline_selection"
    for task_id, files in {
        "task_running_one": ["pre.jpg"],
        "task_done": ["pre.jpg", "post.jpg", "trace.json"],
        "task_running_two": ["pre.jpg"],
    }.items():
        task_dir = phase_dir / "tasks" / task_id
        task_dir.mkdir(parents=True, exist_ok=True)
        for name in files:
            task_dir.joinpath(name).write_text("{}", encoding="utf-8")
    log_path = run_dir / "skillopt.log"
    log_path.parent.mkdir(parents=True, exist_ok=True)
    log_path.write_text("", encoding="utf-8")
    job = SkillOptJob(
        id="skillopt-job",
        command=["python", "-m", "skillopt"],
        log_path=str(log_path),
        run_dir=str(run_dir),
        status="running",
    )

    payload = app._job_payload(job)

    assert payload["stage"] == "baseline"
    assert payload["current_suite"] == "baseline_selection"
    assert payload["progress"] == {
        "phase": "baseline_selection",
        "started_tasks": 3,
        "completed_tasks": 1,
        "total_tasks": 3,
        "running_tasks": ["task_running_one", "task_running_two"],
        "summary": "baseline_selection: 1/3 completed, 2 running (task_running_one, task_running_two)",
    }
    assert payload["log_tail"] == "baseline_selection: 1/3 completed, 2 running (task_running_one, task_running_two)"


def test_task_artifacts_complete_requires_trace_json(tmp_path: Path):
    task_dir = tmp_path / "task"
    task_dir.mkdir()
    (task_dir / "post.jpg").write_text("", encoding="utf-8")
    (task_dir / "judge.json").write_text("{}", encoding="utf-8")

    assert webui.task_artifacts_complete(task_dir) is False

    (task_dir / "trace.json").write_text("{}", encoding="utf-8")

    assert webui.task_artifacts_complete(task_dir) is True


def test_running_job_payload_prefers_skillopt_phase_records(tmp_path: Path):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    run_dir = tmp_path / "runs" / "skillopt-job"
    phase_dir = run_dir / "phases"
    phase_dir.mkdir(parents=True)
    (phase_dir / "step_01_train.json").write_text(json.dumps({
        "schema": "skillopt.phase.v1",
        "phase": "step_01_train",
        "kind": "train",
        "suite_name": "device_operator_train",
        "status": "running",
        "counts": {"total": 3, "passed": 1, "failed": 1, "running": 1},
        "tasks": [
            {"id": "task_passed", "category": "single_step", "status": "passed", "hard": 1, "soft": 1.0, "turns": 2, "reason": ""},
            {"id": "task_failed", "category": "single_step", "status": "failed", "hard": 0, "soft": 0.0, "turns": 0, "reason": "No tool calls."},
            {"id": "task_running", "category": "single_step", "status": "running", "hard": 0, "soft": 0.0, "turns": 1, "reason": ""},
        ],
    }), encoding="utf-8")
    log_path = run_dir / "skillopt.log"
    log_path.write_text("", encoding="utf-8")
    job = SkillOptJob(
        id="skillopt-job",
        command=["python", "-m", "skillopt"],
        log_path=str(log_path),
        run_dir=str(run_dir),
        status="running",
    )

    payload = app._job_payload(job)

    assert payload["stage"] == "train"
    assert payload["current_suite"] == "step_01_train"
    assert payload["progress"]["source"] == "skillopt_phase"
    assert payload["progress"]["phase"] == "step_01_train"
    assert payload["progress"]["iteration"] == 1
    assert payload["progress"]["completed_tasks"] == 2
    assert payload["progress"]["running_tasks"] == ["task_running"]
    assert payload["progress"]["failed_tasks"] == ["task_failed"]
    assert payload["progress"]["tasks"][1]["reason"] == "No tool calls."
    assert payload["log_tail"] == "step_01_train: 2/3 completed, 1 running (task_running), 1 failed (task_failed)"


def test_completed_job_payload_reports_latest_skillopt_iteration(tmp_path: Path):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    run_dir = tmp_path / "runs" / "skillopt-job"
    phase_dir = run_dir / "phases"
    phase_dir.mkdir(parents=True)
    (phase_dir / "step_02_selection.json").write_text(json.dumps({
        "schema": "skillopt.phase.v1",
        "phase": "step_02_selection",
        "kind": "verification",
        "suite_name": "device_operator_verification",
        "status": "completed",
        "counts": {"total": 2, "passed": 2},
        "tasks": [
            {"id": "task_one", "category": "single_step", "status": "passed", "hard": 1, "soft": 1.0, "turns": 2, "reason": ""},
            {"id": "task_two", "category": "single_step", "status": "passed", "hard": 1, "soft": 1.0, "turns": 2, "reason": ""},
        ],
    }), encoding="utf-8")
    log_path = run_dir / "skillopt.log"
    log_path.write_text("", encoding="utf-8")
    job = SkillOptJob(
        id="skillopt-job",
        command=["python", "-m", "skillopt"],
        log_path=str(log_path),
        run_dir=str(run_dir),
        status="passed",
        exit_code=0,
    )

    payload = app._job_payload(job)

    assert payload["stage"] == "selection"
    assert payload["current_suite"] == "step_02_selection"
    assert payload["progress"]["iteration"] == 2
    assert payload["progress"]["completed_tasks"] == 2


def test_job_payload_reports_best_verification_score_from_phase_records(tmp_path: Path):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    run_dir = tmp_path / "runs" / "skillopt-job"
    phase_dir = run_dir / "phases"
    phase_dir.mkdir(parents=True)
    (phase_dir / "baseline_selection.json").write_text(json.dumps({
        "schema": "skillopt.phase.v1",
        "phase": "baseline_selection",
        "kind": "verification",
        "suite_name": "device_operator_verification",
        "status": "completed",
        "score": {"hard": 0.333, "soft": 0.5, "n": 6, "n_passed": 2},
        "tasks": [],
    }), encoding="utf-8")
    (phase_dir / "step_01_selection.json").write_text(json.dumps({
        "schema": "skillopt.phase.v1",
        "phase": "step_01_selection",
        "kind": "verification",
        "suite_name": "device_operator_verification",
        "status": "completed",
        "score": {"hard": 0.5, "soft": 0.75, "n": 6, "n_passed": 3},
        "tasks": [],
    }), encoding="utf-8")
    log_path = run_dir / "skillopt.log"
    log_path.write_text("", encoding="utf-8")
    job = SkillOptJob(
        id="skillopt-job",
        command=["python", "-m", "skillopt"],
        log_path=str(log_path),
        run_dir=str(run_dir),
        status="failed",
        exit_code=1,
    )

    payload = app._job_payload(job)

    assert payload["best_score"] == 0.5


def test_running_job_payload_enriches_running_phase_records_from_benchmark_artifacts(tmp_path: Path):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    run_dir = tmp_path / "runs" / "skillopt-job"
    phase_dir = run_dir / "phases"
    phase_dir.mkdir(parents=True)
    (phase_dir / "baseline_selection.json").write_text(json.dumps({
        "schema": "skillopt.phase.v1",
        "phase": "baseline_selection",
        "kind": "verification",
        "suite_name": "device_operator_verification",
        "status": "running",
        "counts": {"total": 3, "queued": 3},
        "tasks": [
            {"id": "task_running_one", "category": "single_step", "status": "queued"},
            {"id": "task_done", "category": "single_step", "status": "queued"},
            {"id": "task_not_started", "category": "single_step", "status": "queued"},
        ],
    }), encoding="utf-8")
    benchmark_phase = run_dir / "benchmark" / "skillopt-job-baseline_selection" / "tasks"
    (benchmark_phase / "task_running_one").mkdir(parents=True)
    (benchmark_phase / "task_running_one" / "pre.jpg").write_text("", encoding="utf-8")
    (benchmark_phase / "task_done").mkdir(parents=True)
    (benchmark_phase / "task_done" / "trace.json").write_text("{}", encoding="utf-8")
    log_path = run_dir / "skillopt.log"
    log_path.write_text("", encoding="utf-8")
    job = SkillOptJob(
        id="skillopt-job",
        command=["python", "-m", "skillopt"],
        log_path=str(log_path),
        run_dir=str(run_dir),
        status="running",
    )

    payload = app._job_payload(job)

    assert payload["progress"]["source"] == "skillopt_phase"
    assert payload["progress"]["completed_tasks"] == 1
    assert payload["progress"]["running_tasks"] == ["task_running_one"]
    assert [task["status"] for task in payload["progress"]["tasks"]] == ["running", "completed", "queued"]
    assert payload["log_tail"] == "baseline_selection: 1/3 completed, 1 running (task_running_one)"


def test_running_job_payload_enriches_completed_phase_tasks_from_benchmark_results(tmp_path: Path):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    run_dir = tmp_path / "runs" / "skillopt-job"
    phase_dir = run_dir / "phases"
    phase_dir.mkdir(parents=True)
    (phase_dir / "step_01_train.json").write_text(json.dumps({
        "schema": "skillopt.phase.v1",
        "phase": "step_01_train",
        "kind": "train",
        "suite_name": "device_operator_train",
        "status": "running",
        "counts": {"total": 2, "queued": 2},
        "tasks": [
            {"id": "task_passed", "category": "single_step", "status": "queued"},
            {"id": "task_skipped", "category": "single_step", "status": "queued"},
        ],
    }), encoding="utf-8")
    benchmark_phase = run_dir / "benchmark" / "skillopt-job-step_01_train"
    benchmark_phase.mkdir(parents=True)
    (benchmark_phase / "results.jsonl").write_text(
        "\n".join([
            json.dumps({
                "suite": "device_operator_train",
                "run_id": "skillopt-job-step_01_train",
                "task_id": "task_passed",
                "category": "single_step",
                "attempt": 1,
                "status": "passed",
                "rubric": [],
                "rubric_pass_count": 0,
                "rubric_total": 0,
                "metrics": {"tool_calls": 4},
                "artifact_dir": str(benchmark_phase / "tasks" / "task_passed"),
                "description_for_judge": "passed task",
            }),
            json.dumps({
                "suite": "device_operator_train",
                "run_id": "skillopt-job-step_01_train",
                "task_id": "task_skipped",
                "category": "single_step",
                "attempt": 1,
                "status": "skipped",
                "rubric": [],
                "rubric_pass_count": 0,
                "rubric_total": 0,
                "metrics": {"tool_calls": 0, "error": "setup: reset failed"},
                "artifact_dir": str(benchmark_phase / "tasks" / "task_skipped"),
                "description_for_judge": "skipped task",
            }),
        ]) + "\n",
        encoding="utf-8",
    )
    (benchmark_phase / "report.html").write_text("<html></html>", encoding="utf-8")
    log_path = run_dir / "skillopt.log"
    log_path.write_text("", encoding="utf-8")
    job = SkillOptJob(
        id="skillopt-job",
        command=["python", "-m", "skillopt"],
        log_path=str(log_path),
        run_dir=str(run_dir),
        status="running",
    )

    payload = app._job_payload(job)

    tasks = payload["progress"]["tasks"]
    assert tasks[0]["status"] == "passed"
    assert tasks[0]["turns"] == 4
    assert tasks[0]["artifact_dir"] == "benchmark/skillopt-job-step_01_train/tasks/task_passed"
    assert tasks[0]["raw_report"] == "benchmark/skillopt-job-step_01_train/report.html"
    assert tasks[1]["status"] == "skipped"
    assert tasks[1]["turns"] == 0
    assert tasks[1]["reason"] == "setup: reset failed"


def test_start_job_writes_benchmark_agent_config(tmp_path: Path, monkeypatch):
    base_config_dir = tmp_path / "config"
    base_config_dir.mkdir()
    (base_config_dir / "agent.toml").write_text(
        """
[model_settings.providers.benchmark]
type = "openrouter"
api_key = "sk-test"

[model_settings.model]
provider = "benchmark"
model = "openrouter/test-model"
""".lstrip(),
        encoding="utf-8",
    )
    app = SkillOptWebApp(SkillOptWebUIConfig(
        runs_dir=tmp_path / "runs",
        base_config_dir=base_config_dir,
    ))

    monkeypatch.setattr(app, "_run_job", lambda job: None)

    job = app.start_job({"backend": "device", "environment_url": "http://127.0.0.1:50196"})

    command = job["command"]
    agent_config = Path(command[command.index("--agent-config") + 1])
    assert agent_config == tmp_path / "runs" / job["id"] / "agent.toml"
    content = agent_config.read_text(encoding="utf-8")
    assert "voice_streaming_tts_enabled = false" in content
    assert "voice_tool_call_speech = false" in content
    assert "voice_progress_speech_enabled = false" in content
    assert 'provider = "openrouter"' in content
    assert 'api_key = "sk-test"' in content


def test_materialize_benchmark_config_preserves_comments_and_rewrites_both_references():
    content = """
[model_settings.providers.benchmark] # selected by Benchmark WebUI
type = "openrouter"
api_key = "sk-test"

[model_settings.model] # active model
provider = "benchmark" # provider alias
model = "openrouter/test-model"
""".lstrip()

    materialized = webui.materialize_benchmark_agent_config(content)
    parsed = tomllib.loads(materialized)

    assert parsed["model_settings"]["model"]["provider"] == "openrouter"
    assert parsed["model_settings"]["providers"]["openrouter"]["api_key"] == "sk-test"
    assert "[model_settings.providers.openrouter] # selected by Benchmark WebUI" in materialized
    assert 'provider = "openrouter" # provider alias' in materialized
    assert "model_settings.providers.benchmark" not in materialized


def test_materialize_benchmark_config_rejects_existing_provider_collision():
    content = """
[model_settings.providers.benchmark]
type = "openrouter"
api_key = "sk-benchmark"

[model_settings.providers.openrouter]
type = "openrouter"
api_key = "sk-existing"

[model_settings.model]
provider = "benchmark"
model = "openrouter/test-model"
""".lstrip()

    with pytest.raises(ValueError, match="already contains"):
        webui.materialize_benchmark_agent_config(content)


def test_grouped_agent_config_reports_provider_api_key():
    content = '''
[model_settings.providers.benchmark]
type = "openrouter"
api_key = "sk-grouped"

[model_settings.model]
provider = "benchmark"
model = "openrouter/test-model"
'''

    assert webui.agent_config_has_api_key(content) is True


def test_grouped_agent_config_reports_missing_provider_api_key():
    content = '''
[model_settings.providers.benchmark]
type = "openrouter"
api_key = ""

[model_settings.model]
provider = "benchmark"
model = "openrouter/test-model"
'''

    assert webui.agent_config_has_api_key(content) is False


def test_legacy_agent_config_does_not_report_api_key():
    content = '''
[model]
provider = "openrouter"
api_key = "sk-legacy"
'''

    assert webui.agent_config_has_api_key(content) is False


def test_start_mobilegym_job_does_not_inject_legacy_instruction(tmp_path: Path, monkeypatch):
    base_config_dir = tmp_path / "benchmark" / "config"
    base_config_dir.mkdir(parents=True)
    base_config_dir.joinpath("agent.toml").write_text(
        """
[model_settings.providers.benchmark]
type = "openrouter"
api_key = "sk-test"

[model_settings.model]
provider = "benchmark"
model = "openrouter/test-model"
""".lstrip(),
        encoding="utf-8",
    )
    mobilegym_config_dir = tmp_path / "benchmark" / "mobilegym" / "config"
    mobilegym_config_dir.mkdir(parents=True)
    mobilegym_config_dir.joinpath("agent.toml").write_text(
        """
[model_settings.providers.fake]
type = "fake"
api_key = ""

[model_settings.model]
provider = "fake"
model = "fake"
""".lstrip(),
        encoding="utf-8",
    )
    monkeypatch.setattr(webui, "REPO_ROOT", tmp_path)
    app = SkillOptWebApp(SkillOptWebUIConfig(
        runs_dir=tmp_path / "runs",
        base_config_dir=base_config_dir,
    ))

    monkeypatch.setattr(app, "_run_job", lambda job: None)

    job = app.start_job({"backend": "mobilegym", "environment_url": "http://127.0.0.1:50196"})

    command = job["command"]
    agent_config = Path(command[command.index("--agent-config") + 1])
    content = agent_config.read_text(encoding="utf-8")
    assert "instruction" not in tomllib.loads(content)
    assert 'api_key = "sk-test"' in content


def test_list_targets_discovers_skill_suite_pairs(tmp_path: Path):
    suites_dir = tmp_path / "suites"
    skill_dir = suites_dir / "device-operator"
    skill_dir.mkdir(parents=True)
    (skill_dir / "device_operator_train.json").write_text(json.dumps({
        "name": "device_operator_train",
        "tasks": [{"id": "train_one"}, {"id": "train_two"}],
    }), encoding="utf-8")
    (skill_dir / "device_operator_verification.json").write_text(json.dumps({
        "name": "device_operator_verification",
        "tasks": [{"id": "verify_one"}],
    }), encoding="utf-8")
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs", suites_dir=suites_dir))

    assert app.list_targets() == [{
        "id": "device-operator",
        "skill": "device-operator",
        "name": "device-operator",
        "train_suite": "skillopt/device-operator/device_operator_train",
        "validation_suite": "skillopt/device-operator/device_operator_verification",
        "train_task_count": 2,
        "validation_task_count": 1,
    }]


def test_list_targets_discovers_multiple_skill_suite_pairs(tmp_path: Path):
    suites_dir = tmp_path / "suites"
    for skill, train_count, validation_count in [
        ("device-operator", 2, 1),
        ("memory-curator", 3, 2),
    ]:
        skill_dir = suites_dir / skill
        skill_dir.mkdir(parents=True)
        (skill_dir / f"{skill.replace('-', '_')}_train.json").write_text(json.dumps({
            "tasks": [{"id": f"train_{idx}"} for idx in range(train_count)],
        }), encoding="utf-8")
        (skill_dir / f"{skill.replace('-', '_')}_verification.json").write_text(json.dumps({
            "tasks": [{"id": f"verify_{idx}"} for idx in range(validation_count)],
        }), encoding="utf-8")

    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs", suites_dir=suites_dir))

    assert [target["skill"] for target in app.list_targets()] == ["device-operator", "memory-curator"]
    assert app.list_targets()[1]["train_suite"] == "skillopt/memory-curator/memory_curator_train"
    assert app.list_targets()[1]["validation_task_count"] == 2


def test_start_job_resolves_target_and_running_environment(tmp_path: Path, monkeypatch):
    suites_dir = tmp_path / "suites"
    skill_dir = suites_dir / "device-operator"
    skill_dir.mkdir(parents=True)
    (skill_dir / "device_operator_train.json").write_text(json.dumps({"tasks": [{"id": "train_one"}]}), encoding="utf-8")
    (skill_dir / "device_operator_verification.json").write_text(json.dumps({"tasks": [{"id": "verify_one"}]}), encoding="utf-8")
    base_config_dir = tmp_path / "config"
    base_config_dir.mkdir()
    (base_config_dir / "agent.toml").write_text('[model]\napi_key = "sk-test"\n', encoding="utf-8")
    app = SkillOptWebApp(SkillOptWebUIConfig(
        runs_dir=tmp_path / "runs",
        suites_dir=suites_dir,
        base_config_dir=base_config_dir,
    ))

    # Inject a running environment directly into env_manager
    from runner.environment import MobileGymEnvironment
    env = MobileGymEnvironment(
        id="mg-1",
        name="MobileGym",
        endpoint="http://host.docker.internal:50196",
        public_endpoint="http://127.0.0.1:50196",
        web_url="http://127.0.0.1:50197",
        status="running",
    )
    app.env_manager._environments["mg-1"] = env

    monkeypatch.setattr(app, "_run_job", lambda job: None)

    job = app.start_job({"target_id": "device-operator", "environment_id": "mg-1", "budget": 1, "edit_budget": 1})

    command = job["command"]
    assert command[command.index("--skill") + 1] == "device-operator"
    assert command[command.index("--train-suite") + 1] == "skillopt/device-operator/device_operator_train"
    assert command[command.index("--validation-suite") + 1] == "skillopt/device-operator/device_operator_verification"
    assert command[command.index("--environment-url") + 1] == "http://127.0.0.1:50196"


def test_start_mobilegym_environment_proxies_to_benchmark_webui(tmp_path: Path, monkeypatch):
    """SkillOpt now manages MobileGym environments directly via env_manager."""
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))
    calls = []

    from runner.environment import MobileGymEnvironment

    def fake_start(name, parallel_envs=5):
        calls.append({"name": name, "parallel_envs": parallel_envs})
        env = MobileGymEnvironment(
            id="mg-1",
            name=name,
            endpoint="http://host.docker.internal:50196",
            public_endpoint="http://127.0.0.1:50196",
            web_url="http://127.0.0.1:50197",
            status="running",
            parallel_envs=parallel_envs,
        )
        app.env_manager._environments[env.id] = env
        return env

    monkeypatch.setattr(app.env_manager, "start_mobilegym", fake_start)

    environment = app.start_mobilegym_environment({"name": "SkillOpt", "parallel_envs": 3})

    assert environment["id"] == "mg-1"
    assert environment["status"] == "running"
    assert calls == [{"name": "SkillOpt", "parallel_envs": 3}]


def test_start_mobilegym_environment_rejects_nonpositive_parallel_envs(tmp_path: Path, monkeypatch):
    app = SkillOptWebApp(SkillOptWebUIConfig(runs_dir=tmp_path / "runs"))

    def fail(*args, **kwargs):
        raise AssertionError("should not start environment")

    monkeypatch.setattr(app.env_manager, "start_mobilegym", fail)

    try:
        app.start_mobilegym_environment({"parallel_envs": 0})
    except ValueError as exc:
        assert "parallel_envs must be positive" in str(exc)
    else:
        raise AssertionError("nonpositive parallel_envs should be rejected")


def test_start_job_rejects_missing_benchmark_agent_api_key(tmp_path: Path, monkeypatch):
    base_config_dir = tmp_path / "config"
    base_config_dir.mkdir()
    (base_config_dir / "agent.toml").write_text('[model]\napi_key = ""\n', encoding="utf-8")
    app = SkillOptWebApp(SkillOptWebUIConfig(
        runs_dir=tmp_path / "runs",
        base_config_dir=base_config_dir,
    ))

    monkeypatch.setattr(app, "_run_job", lambda job: (_ for _ in ()).throw(AssertionError("job should not start")))

    try:
        app.start_job({"environment_url": "http://127.0.0.1:50196"})
    except ValueError as exc:
        assert "Benchmark agent.toml does not contain a model api_key" in str(exc)
    else:
        raise AssertionError("missing api key should reject the SkillOpt job")


def test_webui_settings_use_agent_config_key_as_judge_key_fallback(tmp_path: Path):
    base_config_dir = tmp_path / "config"
    base_config_dir.mkdir()
    (base_config_dir / "agent.toml").write_text(
        '[model]\nprovider = "openrouter"\napi_key = "sk-agent-fallback"\n',
        encoding="utf-8",
    )
    app = SkillOptWebApp(SkillOptWebUIConfig(
        runs_dir=tmp_path / "runs",
        base_config_dir=base_config_dir,
    ))

    settings = app.get_webui_settings()

    assert settings["judge"]["has_api_key"] is True


def test_index_html_reuses_benchmark_webui_shell():
    assert '<header class="topbar">' in INDEX_HTML
    assert 'class="brand-title">Aico SkillOpt' in INDEX_HTML
    assert 'class="tile"' in INDEX_HTML
    assert 'id="suiteRows"' in INDEX_HTML
    assert 'id="runBtn"' in INDEX_HTML
    assert 'id="agentConfigStatus"' in INDEX_HTML
    assert 'id="envRows"' in INDEX_HTML
    assert 'id="startMobileGym"' in INDEX_HTML
    assert 'id="mobilegymParallelEnvs"' in INDEX_HTML
    assert 'id="activeStopJob"' in INDEX_HTML
    assert 'id="jobRows"' in INDEX_HTML
    assert 'id="phaseTaskRows"' in INDEX_HTML
    assert 'id="logBox"' in INDEX_HTML
    assert 'id="judgeEnabled"' in INDEX_HTML
    assert 'id="judgeModel"' in INDEX_HTML
    assert 'id="judgeApiKey"' in INDEX_HTML
    assert 'placeholder="AICO_BENCHMARK_JUDGE_API_KEY"' in INDEX_HTML
    assert 'id="optimizerModel"' in INDEX_HTML
    assert 'id="runEnvDialog"' in INDEX_HTML
    assert 'id="skill"' not in INDEX_HTML
    assert 'id="environmentUrl"' not in INDEX_HTML
    assert 'id="agentUrl"' not in INDEX_HTML


def test_index_html_renders_skillopt_phase_task_records():
    assert "function renderPhaseTasks" in INDEX_HTML
    assert "const progress = job && job.progress" in INDEX_HTML
    assert "progress.tasks" in INDEX_HTML
    assert "phaseTaskRows" in INDEX_HTML
    assert "renderJobsTable" not in INDEX_HTML


def test_index_html_omits_stage_prefix_from_job_rows():
    assert "stageLabel" not in INDEX_HTML
    assert "`${stageLabel} ${suitesLabel}`" not in INDEX_HTML
    assert "const targetInfo = suitesLabel || targetLabel;" in INDEX_HTML


def test_index_html_syncs_progress_bar_to_phase_task_progress():
    assert "progressWidth(job)" in INDEX_HTML
    assert "job.progress.completed_tasks" in INDEX_HTML
    assert "job.progress.total_tasks" in INDEX_HTML
    assert "return '62%'" not in INDEX_HTML


def test_index_html_syncs_iterations_to_phase_progress():
    assert "job.progress.iteration" in INDEX_HTML


def test_index_html_progress_metrics_use_score_and_report_not_exit_counts():
    assert "Best score" in INDEX_HTML
    assert "formatBestScore(job.best_score)" in INDEX_HTML
    assert '<span class="muted">Report</span>' in INDEX_HTML
    assert "mReportLink" in INDEX_HTML
    assert '<span class="muted">Exit</span>' not in INDEX_HTML
    assert '<span class="muted">Reports</span>' not in INDEX_HTML
    assert "mIterations" in INDEX_HTML


def test_index_html_does_not_skip_daemon_image_build_by_default():
    assert "no_build_daemon_image: true" not in INDEX_HTML


def test_index_html_labels_phase_task_turns_as_tool_calls():
    assert "Tool calls" in INDEX_HTML
    assert ">Turns<" not in INDEX_HTML
