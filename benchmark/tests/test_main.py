import json
import subprocess
import sys
import threading
import time
import urllib.parse

import pytest

from runner.agent_client import ToolInvokeResult
from runner.analysis import AnalysisResult
from runner.models import HardAssertionFailure, HardAssertionResults, RubricVerdict, TaskResult
import runner.main as main
import runner.langfuse_reporter as langfuse_reporter
import runner.preflight as preflight_module
import runner.suite as suite_module
import runner.webui as webui


class FakeClockClient:
    def __init__(self, years):
        self.years = list(years)
        self.calls = []

    def invoke_tool(self, name, args):
        self.calls.append((name, args))
        year = self.years.pop(0)
        return ToolInvokeResult(output=f"{year}\n", is_error=False, duration_ms=1)


def test_publish_langfuse_cli_reports_experiment_url(monkeypatch, capsys):
    published = langfuse_reporter.PublishResult(
        dataset_name="aiden-benchmark:memory_v1",
        run_name="ci-123",
        dataset_run_id="run-1",
        dataset_run_url="http://langfuse.local/run-1",
        item_count=15,
    )
    def publish(*args, **kwargs):
        kwargs["progress"]("verification complete: dataset_run_id=run-1 items=15")
        return published

    monkeypatch.setattr(langfuse_reporter, "publish_run", publish)

    assert main.cli(["publish-langfuse", "--run-dir", "runs/ci-123"]) == 0

    output = capsys.readouterr().out
    assert "Langfuse publish: verification complete" in output
    assert "dataset=aiden-benchmark:memory_v1" in output
    assert "View experiment: http://langfuse.local/run-1" in output


def test_publish_langfuse_cli_returns_failure(monkeypatch, capsys):
    def fail(*args, **kwargs):
        raise langfuse_reporter.LangfusePublishError("score storage unavailable")

    monkeypatch.setattr(langfuse_reporter, "publish_run", fail)

    assert main.cli(["publish-langfuse", "--run-dir", "runs/ci-123"]) == 2
    assert "score storage unavailable" in capsys.readouterr().err


def test_runner_module_propagates_cli_exit_code(tmp_path):
    completed = subprocess.run(
        [
            sys.executable,
            "-m",
            "runner",
            "publish-langfuse",
            "--run-dir",
            str(tmp_path / "missing-run"),
        ],
        capture_output=True,
        text=True,
        check=False,
    )

    assert completed.returncode == 2
    assert "benchmark run directory does not exist" in completed.stderr


@pytest.mark.parametrize("error_status", ["judge_error", "timeout"])
def test_run_exit_code_allows_any_complete_task_result(error_status):
    benchmark_failure = {
        "tasks": 2,
        "passed": 1,
        "failed": 1,
        "skipped": 0,
        "judge_error": 0,
        "timeout": 0,
    }
    execution_error = {
        **benchmark_failure,
        "failed": 0,
        error_status: 1,
    }

    assert main._run_exit_code(benchmark_failure) == 0
    assert main._run_exit_code(execution_error) == 1


@pytest.mark.parametrize("target_platform", ["windows", "linux"])
def test_run_cli_accepts_desktop_target_platform(monkeypatch, target_platform):
    captured = {}

    def fake_cmd_run(args):
        captured["target_platform"] = args.target_platform
        return 0

    monkeypatch.setattr(main, "_cmd_run", fake_cmd_run)

    assert main.cli(["run", "--suite", "suite.json", "--target-platform", target_platform]) == 0
    assert captured["target_platform"] == target_platform


@pytest.mark.parametrize("device_type", ["windows", "linux"])
def test_start_agent_daemon_cli_accepts_desktop_device_type(monkeypatch, device_type):
    import runner.services as services

    captured = {}

    def fake_cmd_start_agent_daemon(args):
        captured["device_type"] = args.device_type
        return 0

    monkeypatch.setattr(services, "cmd_start_agent_daemon", fake_cmd_start_agent_daemon)

    assert main.cli(["start-agent-daemon", "--device-type", device_type]) == 0
    assert captured["device_type"] == device_type


def test_start_agent_daemon_cli_keeps_target_platform_as_compatibility_alias(monkeypatch):
    import runner.services as services

    captured = {}

    def fake_cmd_start_agent_daemon(args):
        captured["target_platform"] = args.target_platform
        return 0

    monkeypatch.setattr(services, "cmd_start_agent_daemon", fake_cmd_start_agent_daemon)

    assert main.cli(["start-agent-daemon", "--target-platform", "android"]) == 0
    assert captured["target_platform"] == "android"


def test_wait_for_agent_clock_retries_until_board_clock_is_current(monkeypatch):
    sleeps = []
    monkeypatch.setattr(time, "sleep", lambda seconds: sleeps.append(seconds))
    client = FakeClockClient([2021, 2021, 2026])

    assert hasattr(main, "wait_for_agent_clock")
    assert main.wait_for_agent_clock(client, min_year=2026, timeout_sec=10, poll_sec=2) is True

    assert [call[0] for call in client.calls] == ["shell", "shell", "shell"]
    assert all(call[1]["command"] == "date +%Y" for call in client.calls)
    assert sleeps == [2, 2]


def test_resolve_target_platform_infers_adb_android(monkeypatch):
    args = type("Args", (), {"target_platform": "auto", "environment_url": "http://127.0.0.1:18899"})()
    monkeypatch.setattr(main, "_read_environment_health", lambda environment_url: {"bridge_type": "adb_android"})

    assert main._resolve_target_platform(args) == "android"


def test_resolve_target_platform_infers_mobilegym_android(monkeypatch):
    args = type("Args", (), {"target_platform": "auto", "environment_url": "http://127.0.0.1:18899"})()
    monkeypatch.setattr(main, "_read_environment_health", lambda environment_url: {"bridge_type": "mobilegym"})

    assert main._resolve_target_platform(args) == "android"


def test_preflight_mobilegym_environment_exercises_setup_and_release(monkeypatch):
    calls = []
    monkeypatch.setattr(
        preflight_module,
        "read_environment_health",
        lambda environment_url: {"bridge_type": "mobilegym", "platform": "android"},
    )
    monkeypatch.setattr(
        preflight_module,
        "call_environment_setup",
        lambda environment_url, timeout, task_id: calls.append(
            ("setup", environment_url, timeout, task_id)
        ),
    )
    monkeypatch.setattr(
        preflight_module,
        "call_environment_release",
        lambda environment_url, timeout, task_id: calls.append(
            ("release", environment_url, timeout, task_id)
        ),
    )

    main._preflight_mobilegym_environment("http://127.0.0.1:19090", timeout=7)

    assert [call[0] for call in calls] == ["setup", "release"]
    assert calls[0][1:3] == ("http://127.0.0.1:19090", 7)
    assert calls[1][1:3] == ("http://127.0.0.1:19090", 7)
    assert calls[0][3] == calls[1][3]
    assert calls[0][3].startswith("benchmark-preflight:")


def test_preflight_mobilegym_environment_checks_every_pool_slot(monkeypatch):
    calls = []
    setup_barrier = threading.Barrier(3, timeout=1)
    monkeypatch.setattr(
        preflight_module,
        "read_environment_health",
        lambda environment_url: {
            "bridge_type": "mobilegym",
            "platform": "android",
            "env_count": 3,
        },
    )

    def setup(environment_url, timeout, task_id):
        calls.append(("setup", environment_url, timeout, task_id))
        setup_barrier.wait()

    monkeypatch.setattr(preflight_module, "call_environment_setup", setup)
    monkeypatch.setattr(
        preflight_module,
        "call_environment_release",
        lambda environment_url, timeout, task_id: calls.append(
            ("release", environment_url, timeout, task_id)
        ),
    )

    main._preflight_mobilegym_environment("http://127.0.0.1:19090")

    setup_calls = [call for call in calls if call[0] == "setup"]
    release_calls = [call for call in calls if call[0] == "release"]
    assert len(setup_calls) == 3
    assert len({call[3] for call in setup_calls}) == 3
    assert {call[3] for call in release_calls} == {call[3] for call in setup_calls}
    assert all(call[2] == 180 for call in calls)
    assert max(calls.index(call) for call in setup_calls) < min(
        calls.index(call) for call in release_calls
    )


def test_preflight_mobilegym_environment_rejects_setup_failure(monkeypatch):
    monkeypatch.setattr(
        preflight_module,
        "read_environment_health",
        lambda environment_url: {"bridge_type": "mobilegym", "platform": "android"},
    )

    def fail_setup(environment_url, timeout, task_id):
        raise main.ResetError("setup timed out")

    monkeypatch.setattr(preflight_module, "call_environment_setup", fail_setup)
    monkeypatch.setattr(preflight_module, "call_environment_release", lambda *args, **kwargs: None)

    with pytest.raises(RuntimeError, match="start a fresh MobileGym environment"):
        main._preflight_mobilegym_environment("http://127.0.0.1:19090", timeout=7)


def test_run_with_environment_url_runs_preflight_without_auto_agent_setup(
    monkeypatch, tmp_path, capsys
):
    monkeypatch.delenv(main.MOBILEGYM_PREFLIGHT_COMPLETE_ENV, raising=False)
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "suite",
                "tasks": [
                    {
                        "id": "t1",
                        "category": "diagnostic",
                        "prompt": "test",
                        "description_for_judge": "test",
                        "rubric": [{"id": "ok", "check": "ok"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )
    calls = []

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            pass

        def health(self):
            return False

        def close(self):
            pass

    monkeypatch.setattr(
        main,
        "_read_environment_health",
        lambda environment_url: {"bridge_type": "mobilegym", "platform": "android"},
    )
    monkeypatch.setattr(
        main,
        "_preflight_mobilegym_environment",
        lambda environment_url, health=None: calls.append(environment_url),
    )
    monkeypatch.setattr(main, "AgentClient", FakeClient)

    argv = [
        "run",
        "--suite",
        str(suite_path),
        "--environment-url",
        "http://127.0.0.1:19090",
        "--no-judge",
    ]
    rc = main.cli(argv)

    assert rc == 2
    assert calls == ["http://127.0.0.1:19090"]
    assert "not reachable" in capsys.readouterr().err

    monkeypatch.setenv(main.MOBILEGYM_PREFLIGHT_COMPLETE_ENV, "1")
    assert main.cli(argv) == 2
    assert calls == ["http://127.0.0.1:19090"]


def test_resolve_target_platform_reads_vphone_ios_health(monkeypatch):
    args = type("Args", (), {"target_platform": "auto", "environment_url": "http://127.0.0.1:8899"})()
    monkeypatch.setattr(
        main,
        "_read_environment_health",
        lambda environment_url: {"bridge_type": "vphone_ios", "platform": "ios"},
    )

    assert main._resolve_target_platform(args) == "ios"


def test_resolve_target_platform_infers_vphone_ios_for_legacy_health(monkeypatch):
    args = type("Args", (), {"target_platform": "auto", "environment_url": "http://127.0.0.1:8899"})()
    monkeypatch.setattr(main, "_read_environment_health", lambda environment_url: {"bridge_type": "vphone_ios"})

    assert main._resolve_target_platform(args) == "ios"


def test_resolve_target_platform_rejects_unknown_environment_platform(monkeypatch):
    args = type("Args", (), {"target_platform": "auto", "environment_url": "http://127.0.0.1:8899"})()
    monkeypatch.setattr(main, "_read_environment_health", lambda environment_url: {"bridge_type": "custom"})

    with pytest.raises(ValueError, match="supported platform"):
        main._resolve_target_platform(args, required=True)


def test_resolve_target_platform_accepts_windows_environment_health(monkeypatch):
    args = type("Args", (), {"target_platform": "auto", "environment_url": "http://127.0.0.1:8899"})()
    monkeypatch.setattr(
        main,
        "_read_environment_health",
        lambda environment_url: {"platform": "windows", "bridge_type": "mobilegym"},
    )

    assert main._resolve_target_platform(args) == "windows"


def test_resolve_target_platform_rejects_invalid_canonical_platform_without_legacy_fallback(monkeypatch):
    args = type("Args", (), {"target_platform": "auto", "environment_url": "http://127.0.0.1:8899"})()
    monkeypatch.setattr(
        main,
        "_read_environment_health",
        lambda environment_url: {"platform": "chromeos", "bridge_type": "mobilegym"},
    )

    with pytest.raises(ValueError, match="unsupported environment platform"):
        main._resolve_target_platform(args)


def test_resolve_target_platform_propagates_environment_health_failure(monkeypatch):
    args = type("Args", (), {"target_platform": "auto", "environment_url": "http://127.0.0.1:8899"})()

    def fail_health(environment_url):
        raise RuntimeError("health unavailable")

    monkeypatch.setattr(main, "_read_environment_health", fail_health)

    with pytest.raises(RuntimeError, match="health unavailable"):
        main._resolve_target_platform(args, required=True)


def test_resolve_target_platform_validates_explicit_platform_against_environment(monkeypatch):
    args = type("Args", (), {"target_platform": "ios", "environment_url": "http://127.0.0.1:8899"})()
    monkeypatch.setattr(main, "_read_environment_health", lambda environment_url: {"platform": "android"})

    with pytest.raises(ValueError, match="does not match"):
        main._resolve_target_platform(args, required=True)


def test_resolve_target_platform_rejects_explicit_mismatch_without_auto_setup(monkeypatch):
    args = type("Args", (), {"target_platform": "ios", "environment_url": "http://127.0.0.1:8899"})()
    monkeypatch.setattr(main, "_read_environment_health", lambda environment_url: {"platform": "android"})

    with pytest.raises(ValueError, match="does not match"):
        main._resolve_target_platform(args)


def test_resolve_target_platform_does_not_accept_pre_resolved_override(monkeypatch):
    args = type(
        "Args",
        (),
        {
            "target_platform": "auto",
            "environment_url": "http://127.0.0.1:8899",
        },
    )()
    monkeypatch.setattr(
        main,
        "_read_environment_health",
        lambda environment_url: {"platform": "android"},
    )

    assert main._resolve_target_platform(args) == "android"


def _task_result_with_details():
    return TaskResult(
        suite="suite",
        run_id="run-1",
        task_id="task-1",
        category="diagnostic",
        attempt=1,
        status="failed",
        rubric=[RubricVerdict(id="r1", verdict="no", reason="missing evidence")],
        rubric_pass_count=0,
        rubric_total=1,
        hard_assertions=HardAssertionResults(
            min_tool_calls=False,
            response_exists=False,
            required_tools=False,
            forbidden_tools=False,
        ),
        hard_assertion_failures=[
            HardAssertionFailure(
                id="required_tools",
                label="Required Tools",
                requirement="Must call: screenshot.",
                actual="Missing: screenshot. Used: none.",
            ),
            HardAssertionFailure(
                id="forbidden_tools",
                label="Forbidden Tools",
                requirement="Must not call: shell.",
                actual="Forbidden calls: shell at step 1. Used: shell.",
            ),
        ],
        metrics={"error": "boom", "agent_error": "agent boom", "judge_error": "judge boom"},
    )


def test_log_task_result_keeps_default_output_concise(capsys):
    main._log_task_result("task-1", 1, _task_result_with_details(), verbose=False)

    out = capsys.readouterr().out
    assert "FAILED" in out
    assert "Hard assertion failures" not in out
    assert "Error: boom" not in out
    assert "Agent Error" not in out
    assert "Judge Error" not in out
    assert "Rubric Details" not in out


def test_log_task_result_shows_details_in_verbose_mode(capsys):
    main._log_task_result("task-1", 1, _task_result_with_details(), verbose=True)

    out = capsys.readouterr().out
    assert "FAILED" in out
    assert "Hard assertion failures" in out
    assert "Required Tools" in out
    assert "Requirement: Must call: screenshot." in out
    assert "Actual: Missing: screenshot. Used: none." in out
    assert "Forbidden Tools" in out
    assert "Requirement: Must not call: shell." in out
    assert "Actual: Forbidden calls: shell at step 1. Used: shell." in out
    assert "Error: boom" in out
    assert "Agent Error" in out
    assert "Judge Error" in out
    assert "Rubric Details" in out


def test_run_manifest_records_agent_model(monkeypatch, tmp_path, capsys):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "empty_suite",
                "tasks": [],
            }
        ),
        encoding="utf-8",
    )
    original_suite = suite_path.read_bytes()

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            self.base_url = base_url
            self.benchmark_token = benchmark_token

        def health(self):
            suite_path.write_text('{"name":"changed","tasks":[]}', encoding="utf-8")
            return True

        def device_type(self):
            return "android"

        def close(self):
            pass

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "upload_report", lambda *args, **kwargs: False)
    monkeypatch.setattr(main, "generate_report_html", lambda run_dir: "<html></html>")

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "snapshot-run",
            "--agent-model",
            "qwen3.6-35b",
            "--no-judge",
        ]
    )

    assert rc == 0
    manifest_path = next((tmp_path / "runs").glob("*/manifest.json"))
    manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
    assert manifest["agent_model"] == "qwen3.6-35b"
    assert manifest["judge_config"] is None
    assert manifest["suite_snapshot_path"] == "suite.json"
    assert (manifest_path.parent / "suite.json").read_bytes() == original_suite

    assert main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "snapshot-run",
            "--no-judge",
        ]
    ) == 2
    assert "run directory already exists" in capsys.readouterr().err


def test_unreachable_agent_does_not_reserve_run_directory(monkeypatch, tmp_path):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "health_failure_suite",
                "tasks": [
                    {
                        "id": "task-a",
                        "category": "diagnostic",
                        "prompt": "test",
                        "description_for_judge": "test",
                        "rubric": [{"id": "done", "check": "done"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )

    class UnreachableClient:
        def health(self):
            return False

        def close(self):
            pass

    monkeypatch.setattr(main, "_new_agent_client", lambda *args, **kwargs: UnreachableClient())
    run_dir = tmp_path / "runs" / "retryable-run"

    assert main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "retryable-run",
            "--no-judge",
        ]
    ) == 2
    assert not run_dir.exists()


def test_run_cli_defaults_use_benchmark_agent_and_judge_model_environment(monkeypatch):
    captured = {}
    monkeypatch.setenv("AIDEN_BENCHMARK_AGENT_MODEL", "agent-model")
    monkeypatch.setenv("AIDEN_BENCHMARK_JUDGE_MODEL", "judge-model")
    monkeypatch.setenv("AIDEN_MODEL", "legacy-aiden-model")
    monkeypatch.setenv("MODEL_NAME", "legacy-model")
    monkeypatch.setenv("OPENAI_MODEL", "legacy-openai-model")
    monkeypatch.setattr(main, "_cmd_run", lambda args: captured.update(vars(args)) or 0)

    assert main.cli(["run", "--suite", "unused.json"]) == 0

    assert captured["agent_model"] == "agent-model"
    assert captured["judge_model"] == "judge-model"


def test_run_state_file_records_incremental_totals(monkeypatch, tmp_path):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "progress_suite",
                "tasks": [
                    {
                        "id": "first",
                        "category": "diagnostic",
                        "prompt": "first",
                        "description_for_judge": "first",
                        "rubric": [{"id": "done", "check": "done"}],
                    },
                    {
                        "id": "second",
                        "category": "diagnostic",
                        "prompt": "second",
                        "description_for_judge": "second",
                        "rubric": [{"id": "done", "check": "done"}],
                    },
                ],
            }
        ),
        encoding="utf-8",
    )
    state_file = tmp_path / "state.json"
    observed_before_second_task = {}

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            self.base_url = base_url
            self.benchmark_token = benchmark_token

        def health(self):
            return True

        def device_type(self):
            return "android"

        def close(self):
            pass

    def fake_run_one_task(client, suite, task, attempt, artifact_dir, *args, **kwargs):
        if task.id == "second":
            observed_before_second_task.update(json.loads(state_file.read_text(encoding="utf-8")))
        status = "passed" if task.id == "first" else "failed"
        return TaskResult(
            suite=suite.name,
            run_id="progress-run",
            task_id=task.id,
            category=task.category,
            attempt=attempt,
            status=status,
            rubric=[],
            rubric_pass_count=0,
            rubric_total=0,
            artifact_dir=str(artifact_dir),
        )

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_ready", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "recover_agent_after_timeout", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "run_one_task", fake_run_one_task)
    monkeypatch.setattr(main, "generate_report_html", lambda run_dir: "<html></html>")
    monkeypatch.setattr(main, "upload_report", lambda *args, **kwargs: False)

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "progress-run",
            "--state-file",
            str(state_file),
            "--no-judge",
        ]
    )

    assert rc == 0
    assert observed_before_second_task["completed"] == 1
    assert observed_before_second_task["totals"] == {
        "tasks": 2,
        "passed": 1,
        "failed": 0,
        "skipped": 0,
        "judge_error": 0,
        "timeout": 0,
    }
    final_state = json.loads(state_file.read_text(encoding="utf-8"))
    assert final_state["status"] == "done"
    assert final_state["totals"] == {
        "tasks": 2,
        "passed": 1,
        "failed": 1,
        "skipped": 0,
        "judge_error": 0,
        "timeout": 0,
    }


def test_run_skips_tasks_outside_target_platform(monkeypatch, tmp_path):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "platform_suite",
                "tasks": [
                    {
                        "id": "android_task",
                        "platforms": ["android"],
                        "category": "diagnostic",
                        "prompt": "android",
                        "description_for_judge": "android",
                        "rubric": [{"id": "done", "check": "done"}],
                    },
                    {
                        "id": "ios_task",
                        "platforms": ["ios"],
                        "category": "diagnostic",
                        "prompt": "ios",
                        "description_for_judge": "ios",
                        "rubric": [{"id": "done", "check": "done"}],
                    },
                ],
            }
        ),
        encoding="utf-8",
    )
    called_tasks = []

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            self.base_url = base_url

        def health(self):
            return True

        def device_type(self):
            return "android"

        def close(self):
            pass

    def fake_run_one_task(client, suite, task, attempt, artifact_dir, *args, **kwargs):
        called_tasks.append(task.id)
        return TaskResult(
            suite=suite.name,
            run_id="platform-run",
            task_id=task.id,
            category=task.category,
            attempt=attempt,
            status="passed",
            rubric=[],
            rubric_pass_count=0,
            rubric_total=0,
            artifact_dir=str(artifact_dir),
        )

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_ready", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "run_one_task", fake_run_one_task)
    monkeypatch.setattr(main, "generate_report_html", lambda run_dir: "<html></html>")
    monkeypatch.setattr(main, "upload_report", lambda *args, **kwargs: False)

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "platform-run",
            "--target-platform",
            "android",
            "--no-judge",
            "--inter-task-cooldown-sec",
            "0",
        ]
    )

    assert rc == 0
    assert called_tasks == ["android_task"]
    manifest = json.loads((tmp_path / "runs" / "platform-run" / "manifest.json").read_text(encoding="utf-8"))
    assert manifest["target_platform"] == "android"
    assert manifest["totals"] == {
        "tasks": 2,
        "passed": 1,
        "failed": 0,
        "skipped": 1,
        "judge_error": 0,
        "timeout": 0,
    }


def test_run_all_platform_skipped_tasks_uses_daemon_platform(monkeypatch, tmp_path):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "platform_suite",
                "tasks": [
                    {
                        "id": "ios_task",
                        "platforms": ["ios"],
                        "category": "diagnostic",
                        "prompt": "ios",
                        "description_for_judge": "ios",
                        "rubric": [{"id": "done", "check": "done"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            self.base_url = base_url

        def health(self):
            return True

        def device_type(self):
            return "android"

        def close(self):
            pass

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "generate_report_html", lambda run_dir: "<html></html>")

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "all-skipped-run",
            "--target-platform",
            "android",
            "--no-judge",
        ]
    )

    assert rc == 0
    manifest = json.loads((tmp_path / "runs" / "all-skipped-run" / "manifest.json").read_text(encoding="utf-8"))
    assert manifest["totals"]["skipped"] == 1
    assert manifest["target_platform"] == "android"


def test_run_all_environment_filtered_tasks_still_validates_daemon_platform(
    monkeypatch, tmp_path, capsys
):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "platform_suite",
                "tasks": [
                    {
                        "id": "ios_task",
                        "platforms": ["ios"],
                        "category": "diagnostic",
                        "prompt": "ios",
                        "description_for_judge": "ios",
                        "rubric": [{"id": "done", "check": "done"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            pass

        def health(self):
            return True

        def device_type(self):
            return "ios"

        def close(self):
            pass

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "_read_environment_health", lambda url: {"platform": "android"})
    monkeypatch.setattr(main, "clear_stale_adb_android_owner", lambda url: None)

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--environment-url",
            "http://127.0.0.1:19090",
            "--no-judge",
        ]
    )

    assert rc == 2
    assert "expected android, reported ios" in capsys.readouterr().err


def test_run_validates_cli_constraint_against_daemon_before_platform_skips(
    monkeypatch, tmp_path, capsys
):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "platform_suite",
                "tasks": [
                    {
                        "id": "ios_task",
                        "platforms": ["ios"],
                        "category": "diagnostic",
                        "prompt": "ios",
                        "description_for_judge": "ios",
                        "rubric": [{"id": "done", "check": "done"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            pass

        def health(self):
            return True

        def device_type(self):
            return "ios"

        def close(self):
            pass

    monkeypatch.setattr(main, "AgentClient", FakeClient)

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--target-platform",
            "android",
            "--no-judge",
        ]
    )

    assert rc == 2
    assert "expected android, reported ios" in capsys.readouterr().err


def test_run_triggers_llm_analysis_when_enabled(monkeypatch, tmp_path):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(json.dumps({"name": "empty_suite", "tasks": []}), encoding="utf-8")
    calls = []

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            self.base_url = base_url
            self.benchmark_token = benchmark_token

        def health(self):
            return True

        def device_type(self):
            return "android"

        def close(self):
            pass

    def fake_analyze(run_dir, repo_root, cfg):
        calls.append((run_dir, repo_root, cfg))
        (run_dir / "llm_analysis.md").write_text("analysis", encoding="utf-8")
        return AnalysisResult(ok=True, markdown_path=run_dir / "llm_analysis.md")

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "upload_report", lambda *args, **kwargs: False)
    monkeypatch.setattr(main, "analyze_run", fake_analyze)

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--no-judge",
            "--judge-model",
            "bytedance-seed/seed-2.0-lite",
            "--llm-analysis",
        ]
    )

    assert rc == 0
    assert calls and calls[0][2].enabled is True
    assert calls[0][2].model == "bytedance-seed/seed-2.0-lite"


def test_run_llm_analysis_env_limits_fall_back_on_invalid_values(monkeypatch, tmp_path):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(json.dumps({"name": "empty_suite", "tasks": []}), encoding="utf-8")
    calls = []

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            self.base_url = base_url
            self.benchmark_token = benchmark_token

        def health(self):
            return True

        def device_type(self):
            return "android"

        def close(self):
            pass

    def fake_analyze(run_dir, repo_root, cfg):
        calls.append(cfg)
        return AnalysisResult(ok=True, markdown_path=run_dir / "llm_analysis.md")

    monkeypatch.setenv("AIDEN_BENCHMARK_ANALYSIS_MAX_LOG_BYTES", "not-an-int")
    monkeypatch.setenv("AIDEN_BENCHMARK_ANALYSIS_MAX_CODE_BYTES", "not-an-int")
    monkeypatch.setenv("AIDEN_BENCHMARK_ANALYSIS_TIMEOUT_SEC", "not-an-int")
    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "upload_report", lambda *args, **kwargs: False)
    monkeypatch.setattr(main, "analyze_run", fake_analyze)

    rc = main.cli(
        ["run", "--suite", str(suite_path), "--out", str(tmp_path / "runs"), "--no-judge", "--llm-analysis"]
    )

    assert rc == 0
    assert calls[0].max_log_bytes == 64 * 1024
    assert calls[0].max_code_bytes == 128 * 1024
    assert calls[0].timeout_sec == 180


def test_run_keeps_exit_code_when_analysis_fails(monkeypatch, tmp_path):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(json.dumps({"name": "empty_suite", "tasks": []}), encoding="utf-8")

    class FakeClient:
        def __init__(self, base_url):
            pass

        def health(self):
            return True

        def device_type(self):
            return "android"

        def close(self):
            pass

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "upload_report", lambda *args, **kwargs: False)
    monkeypatch.setattr(
        main,
        "analyze_run",
        lambda run_dir, repo_root, cfg: AnalysisResult(ok=False, warning="boom"),
    )

    rc = main.cli(
        ["run", "--suite", str(suite_path), "--out", str(tmp_path / "runs"), "--no-judge", "--llm-analysis"]
    )

    assert rc == 0


def test_read_optional_token_fails_fast_for_missing_file(tmp_path):
    with pytest.raises(ValueError, match="unable to read benchmark token file"):
        main._read_optional_token(tmp_path / "missing-token")


def test_read_optional_token_fails_fast_for_empty_file(tmp_path):
    token_file = tmp_path / "control_token"
    token_file.write_text("  \n", encoding="utf-8")

    with pytest.raises(ValueError, match=r"benchmark token file .* is empty"):
        main._read_optional_token(token_file)


def test_auto_agent_setup_injects_environment_url_as_bridge_endpoint(monkeypatch, tmp_path):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "mobile_suite",
                "tasks": [
                    {
                        "id": "open_clock",
                        "category": "diagnostic",
                        "prompt": "open clock",
                        "description_for_judge": "open clock",
                        "rubric": [{"id": "done", "check": "done"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )

    captured = {}
    stale_clears = []

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            self.base_url = base_url
            captured["client_base_url"] = base_url
            self.benchmark_token = benchmark_token
            captured["client_benchmark_token"] = benchmark_token

        def close(self):
            pass

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_ready", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "_read_environment_health", lambda url: {"bridge_type": "mobilegym"})
    monkeypatch.setattr(main, "generate_report_html", lambda run_dir: "<html></html>")
    monkeypatch.setattr(preflight_module, "call_environment_setup", lambda *args, **kwargs: None)
    monkeypatch.setattr(preflight_module, "call_environment_release", lambda *args, **kwargs: None)
    monkeypatch.setattr(main, "call_environment_release", lambda *args, **kwargs: None)
    monkeypatch.setattr(main, "clear_stale_adb_android_owner", lambda url: stale_clears.append(url))
    monkeypatch.setattr(webui, "ensure_daemon_image", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "read_environment_bridge_concurrency", lambda *args, **kwargs: 1)
    def fake_prepare_run_config(base_config_dir, config_dir, **kwargs):
        captured["prepare_config_kwargs"] = kwargs
        config_dir.mkdir(parents=True, exist_ok=True)
        (config_dir / "control_token").write_text("test-token", encoding="utf-8")

    monkeypatch.setattr(webui, "prepare_run_config", fake_prepare_run_config)
    monkeypatch.setattr(webui, "docker_published_port", lambda container_id, container_port: 18081)
    monkeypatch.setattr(webui, "start_daemon_logs", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "stop_daemon_compose", lambda *args, **kwargs: None)

    def fake_start_daemon_compose(job, **kwargs):
        captured["job"] = job
        captured["kwargs"] = kwargs
        return "container-id"

    monkeypatch.setattr(webui, "start_daemon_compose", fake_start_daemon_compose)

    def fake_run_one_task(client, suite, task, attempt, artifact_dir, *args, **kwargs):
        captured["task_kwargs"] = kwargs
        return TaskResult(
            suite=suite.name,
            run_id="auto-run",
            task_id=task.id,
            category=task.category,
            attempt=attempt,
            status="passed",
            rubric=[],
            rubric_pass_count=0,
            rubric_total=0,
            artifact_dir=str(artifact_dir),
        )

    monkeypatch.setattr(main, "run_one_task", fake_run_one_task)

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "auto-run",
            "--environment-url",
            "http://127.0.0.1:19090",
            "--auto-agent-setup",
            "--skill",
            "device-operator",
            "--no-judge",
        ]
    )

    assert rc == 0
    assert captured["job"].endpoint == "http://127.0.0.1:19090"
    assert captured["job"].docker_endpoint == "http://host.docker.internal:19090"
    assert captured["job"].agent_url == "http://127.0.0.1:18081"
    assert captured["client_base_url"] == "http://127.0.0.1:18081"
    assert captured["client_benchmark_token"] == "test-token"
    assert captured["kwargs"]["host_port"] == 0
    assert captured["kwargs"]["environment_bridge_endpoint"] == "http://host.docker.internal:19090"
    assert captured["kwargs"]["environment_bridge_mode"] is True
    assert captured["task_kwargs"]["active_skills"] == ["device-operator"]
    assert "device_type" not in captured["prepare_config_kwargs"]
    assert captured["kwargs"]["device_type"] == "android"
    assert stale_clears == ["http://127.0.0.1:19090"]


def test_auto_agent_setup_runs_memory_suite_without_environment_bridge(monkeypatch, tmp_path):
    suite_path = tmp_path / "memory-suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "memory_suite",
                "tasks": [
                    {
                        "id": "recall_memory",
                        "category": "memory",
                        "prompt": "recall memory",
                        "description_for_judge": "recall memory",
                        "rubric": [{"id": "done", "check": "done"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )
    captured = {}

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            captured["client_base_url"] = base_url
            captured["benchmark_token"] = benchmark_token

        def close(self):
            pass

    def fake_prepare_run_config(base_config_dir, config_dir, **kwargs):
        config_dir.mkdir(parents=True, exist_ok=True)
        (config_dir / "control_token").write_text("memory-token", encoding="utf-8")

    def fake_start_daemon_compose(job, **kwargs):
        captured["job"] = job
        captured["daemon_kwargs"] = kwargs
        return "container-id"

    def fake_run_one_task(client, suite, task, attempt, artifact_dir, *args, **kwargs):
        captured["task_kwargs"] = kwargs
        return TaskResult(
            suite=suite.name,
            run_id="memory-run",
            task_id=task.id,
            category=task.category,
            attempt=attempt,
            status="passed",
            rubric=[],
            rubric_pass_count=0,
            rubric_total=0,
            artifact_dir=str(artifact_dir),
        )

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_ready", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "generate_report_html", lambda run_dir: "<html></html>")
    monkeypatch.setattr(webui, "ensure_daemon_image", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "prepare_run_config", fake_prepare_run_config)
    monkeypatch.setattr(webui, "docker_published_port", lambda *args, **kwargs: 18082)
    monkeypatch.setattr(webui, "start_daemon_compose", fake_start_daemon_compose)
    monkeypatch.setattr(webui, "start_daemon_logs", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "stop_daemon_compose", lambda *args, **kwargs: None)
    monkeypatch.setattr(main, "run_one_task", fake_run_one_task)

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "memory-run",
            "--auto-agent-setup",
            "--no-judge",
        ]
    )

    assert rc == 0
    assert captured["job"].endpoint == ""
    assert captured["job"].docker_endpoint == ""
    assert captured["daemon_kwargs"]["environment_bridge_endpoint"] == ""
    assert captured["daemon_kwargs"]["environment_bridge_mode"] is False
    assert captured["task_kwargs"]["environment_url"] is None
    assert captured["benchmark_token"] == "memory-token"


def test_auto_agent_setup_surfaces_daemon_boot_failure(monkeypatch, tmp_path):
    # A daemon container that exits at startup makes `docker port` fail with
    # the misleading "no public port published" message. The skip reason must
    # carry the container state and logs instead of the raw docker error, and
    # daemon logs must start streaming before the port probe.
    suite_path = tmp_path / "memory-suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "memory_suite",
                "tasks": [
                    {
                        "id": "recall_memory",
                        "category": "memory",
                        "prompt": "recall memory",
                        "description_for_judge": "recall memory",
                        "rubric": [{"id": "done", "check": "done"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )
    calls: list[str] = []

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            pass

        def close(self):
            pass

    def fake_prepare_run_config(base_config_dir, config_dir, **kwargs):
        config_dir.mkdir(parents=True, exist_ok=True)
        (config_dir / "control_token").write_text("memory-token", encoding="utf-8")

    def fake_start_daemon_compose(job, **kwargs):
        return "container-id"

    def failing_docker_published_port(container_id, container_port):
        calls.append("port-probe")
        raise subprocess.CalledProcessError(1, ["docker", "port", container_id, "8080/tcp"])

    def fake_start_daemon_logs(*args, **kwargs):
        calls.append("logs-stream")
        return None

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_ready", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "generate_report_html", lambda run_dir: "<html></html>")
    monkeypatch.setattr(webui, "ensure_daemon_image", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "prepare_run_config", fake_prepare_run_config)
    monkeypatch.setattr(webui, "docker_published_port", failing_docker_published_port)
    monkeypatch.setattr(webui, "container_boot_failure_detail", lambda container_id: (
        "container state: exited (exit 1)\n"
        "last container logs:\n"
        "config_load_failed invalid input_mode"
    ))
    monkeypatch.setattr(webui, "start_daemon_compose", fake_start_daemon_compose)
    monkeypatch.setattr(webui, "start_daemon_logs", fake_start_daemon_logs)
    monkeypatch.setattr(webui, "stop_daemon_compose", lambda *args, **kwargs: None)

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "memory-run",
            "--auto-agent-setup",
            "--no-judge",
        ]
    )

    assert rc == 0
    assert calls == ["logs-stream", "port-probe"]
    results = [
        json.loads(line)
        for line in (tmp_path / "runs" / "memory-run" / "results.jsonl").read_text(
            encoding="utf-8"
        ).splitlines()
    ]
    assert len(results) == 1
    assert results[0]["status"] == "skipped"
    error = results[0]["metrics"]["error"]
    assert "container state: exited (exit 1)" in error
    assert "config_load_failed invalid input_mode" in error


def test_run_rejects_external_daemon_platform_mismatch(monkeypatch, tmp_path, capsys):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "mobile_suite",
                "tasks": [
                    {
                        "id": "open_clock",
                        "category": "diagnostic",
                        "prompt": "open clock",
                        "description_for_judge": "open clock",
                        "rubric": [{"id": "done", "check": "done"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )

    class FakeClient:
        def __init__(self, base_url):
            self.base_url = base_url

        def health(self):
            return True

        def device_type(self):
            return "ios"

        def close(self):
            pass

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "_read_environment_health", lambda url: {"platform": "android"})
    monkeypatch.setattr(main, "clear_stale_adb_android_owner", lambda url: None)

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--environment-url",
            "http://127.0.0.1:19090",
            "--skip-clock-wait",
            "--no-judge",
        ]
    )

    assert rc == 2
    assert "expected android, reported ios" in capsys.readouterr().err


def test_mock_environment_suite_requires_auto_agent_setup(tmp_path, capsys):
    suite_path = tmp_path / "mock-suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "mock_suite",
                "mock_environment": {
                    "phone_bridge": {"platform": "ios"},
                    "tools": {},
                },
                "tasks": [],
            }
        ),
        encoding="utf-8",
    )

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--no-judge",
        ]
    )

    assert rc == 2
    assert "mock_environment require --auto-agent-setup" in capsys.readouterr().err


def test_auto_agent_setup_starts_mock_environment_and_injects_phone_state(
    monkeypatch, tmp_path
):
    suite_path = tmp_path / "mock-suite.json"
    phone_state = {
        "connected": False,
        "platform": "ios",
        "app_state": "background",
        "pip_bridge_enabled": True,
    }
    suite_path.write_text(
        json.dumps(
            {
                "name": "mock_suite",
                "tasks": [
                    {
                        "id": "policy",
                        "category": "diagnostic",
                        "prompt": "test policy",
                        "description_for_judge": "test policy",
                        "rubric": [{"id": "done", "check": "done"}],
                        "mock_environment": {
                            "phone_bridge": phone_state,
                            "screen_text": "mock home screen",
                            "tools": {"screenshot": {"output": {"ok": True}}},
                        },
                    }
                ],
            }
        ),
        encoding="utf-8",
    )
    captured = {}

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            captured["client_base_url"] = base_url
            captured["benchmark_token"] = benchmark_token

        def set_phone_bridge_state(self, state):
            captured["phone_state"] = state
            return {"ok": True}

        def close(self):
            pass

    def fake_prepare_run_config(base_config_dir, config_dir, **kwargs):
        captured["prepare_config_kwargs"] = kwargs
        config_dir.mkdir(parents=True, exist_ok=True)
        (config_dir / "control_token").write_text("mock-token", encoding="utf-8")

    def fake_start_daemon_compose(job, **kwargs):
        captured["job"] = job
        captured["daemon_kwargs"] = kwargs
        return "container-id"

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_ready", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "generate_report_html", lambda run_dir: "<html></html>")
    monkeypatch.setattr(webui, "ensure_daemon_image", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "prepare_run_config", fake_prepare_run_config)
    monkeypatch.setattr(
        webui, "docker_published_port", lambda container_id, container_port: 18081
    )
    monkeypatch.setattr(webui, "start_daemon_compose", fake_start_daemon_compose)
    monkeypatch.setattr(webui, "start_daemon_logs", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "stop_daemon_compose", lambda *args, **kwargs: None)
    monkeypatch.setattr(
        main,
        "run_one_task",
        lambda client, suite, task, attempt, artifact_dir, *args, **kwargs: TaskResult(
            suite=suite.name,
            run_id="mock-run",
            task_id=task.id,
            category=task.category,
            attempt=attempt,
            status="passed",
            rubric=[],
            rubric_pass_count=0,
            rubric_total=0,
            artifact_dir=str(artifact_dir),
        ),
    )

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "mock-run",
            "--auto-agent-setup",
            "--no-judge",
        ]
    )

    assert rc == 0
    assert captured["phone_state"] == {
        key: value for key, value in phone_state.items() if key != "platform"
    }
    assert captured["benchmark_token"] == "mock-token"
    assert captured["job"].endpoint.startswith("http://127.0.0.1:")
    assert captured["job"].docker_endpoint.startswith(
        "http://host.docker.internal:"
    )
    public_endpoint = urllib.parse.urlparse(captured["job"].endpoint)
    docker_endpoint = urllib.parse.urlparse(captured["job"].docker_endpoint)
    assert public_endpoint.path.startswith("/_aiden_mock/")
    assert docker_endpoint.path == public_endpoint.path
    assert captured["daemon_kwargs"]["environment_bridge_endpoint"] == captured[
        "job"
    ].docker_endpoint
    assert "device_type" not in captured["prepare_config_kwargs"]
    assert captured["daemon_kwargs"]["device_type"] == "ios"
    manifest = json.loads(
        (tmp_path / "runs" / "mock-run" / "manifest.json").read_text(
            encoding="utf-8"
        )
    )
    assert manifest["mock_environment"]["default"] is None
    mock_spec = manifest["mock_environment"]["tasks"]["policy"]
    assert mock_spec["platform"] == "ios"
    assert mock_spec["phone_bridge"] == {
        key: value for key, value in phone_state.items() if key != "platform"
    }
    assert mock_spec["single_frame"] is False
    assert manifest["environment_url"].endswith("/_aiden_mock/REDACTED")
    assert manifest["target_platform"] == "ios"


def test_auto_agent_setup_filters_mock_tasks_by_target_platform(tmp_path, capsys):
    suite_path = tmp_path / "mock-suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "mock_suite",
                "tasks": [
                    {
                        "id": "policy",
                        "category": "diagnostic",
                        "prompt": "test policy",
                        "description_for_judge": "test policy",
                        "rubric": [{"id": "done", "check": "done"}],
                        "mock_environment": {
                            "phone_bridge": {"platform": "android"},
                            "tools": {"screenshot": {"output": {"ok": True}}},
                        },
                    }
                ],
            }
        ),
        encoding="utf-8",
    )

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "filtered-run",
            "--auto-agent-setup",
            "--target-platform",
            "ios",
            "--no-judge",
        ]
    )

    assert rc == 0
    assert "does not match" not in capsys.readouterr().err
    manifest = json.loads(
        (tmp_path / "runs" / "filtered-run" / "manifest.json").read_text(encoding="utf-8")
    )
    assert manifest["target_platform"] == "ios"
    assert manifest["totals"]["skipped"] == 1


def test_run_rejects_unfiltered_mixed_mock_and_real_tasks(tmp_path, capsys):
    suite_path = tmp_path / "mixed-mock-real.json"
    base_task = {
        "category": "diagnostic",
        "prompt": "test",
        "description_for_judge": "test",
        "rubric": [{"id": "done", "check": "done"}],
    }
    suite_path.write_text(
        json.dumps(
            {
                "name": "mixed_mock_real",
                "tasks": [
                    {
                        **base_task,
                        "id": "mocked",
                        "mock_environment": {
                            "platform": "ios",
                            "phone_bridge": {},
                            "tools": {},
                        },
                    },
                    {**base_task, "id": "real"},
                ],
            }
        ),
        encoding="utf-8",
    )

    rc = main.cli(["run", "--suite", str(suite_path), "--auto-agent-setup", "--no-judge"])

    assert rc == 2
    assert "cannot mix mock_environment tasks with real-environment tasks" in capsys.readouterr().err


def test_auto_agent_setup_resolves_mock_platform_per_task(monkeypatch, tmp_path):
    suite_path = tmp_path / "mock-suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "mixed_mock_suite",
                "tasks": [
                    {
                        "id": platform,
                        "category": "diagnostic",
                        "prompt": f"test {platform}",
                        "description_for_judge": f"test {platform}",
                        "rubric": [{"id": "done", "check": "done"}],
                        "mock_environment": {
                            "platform": platform,
                            "phone_bridge": {},
                            "tools": {"screenshot": {"output": {"ok": True}}},
                        },
                    }
                    for platform in ("ios", "android")
                ],
            }
        ),
        encoding="utf-8",
    )
    daemon_platforms = {}

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            pass

        def set_phone_bridge_state(self, state):
            return {"ok": True}

        def close(self):
            pass

    def fake_prepare_run_config(base_config_dir, config_dir, **kwargs):
        config_dir.mkdir(parents=True, exist_ok=True)
        (config_dir / "control_token").write_text("mock-token", encoding="utf-8")

    def fake_start_daemon_compose(job, **kwargs):
        task_id = kwargs["benchmark_task_id"].rsplit(":", 1)[-1]
        daemon_platforms[task_id] = kwargs["device_type"]
        return f"container-{task_id}"

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_ready", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "generate_report_html", lambda run_dir: "<html></html>")
    monkeypatch.setattr(webui, "ensure_daemon_image", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "prepare_run_config", fake_prepare_run_config)
    monkeypatch.setattr(
        webui, "docker_published_port", lambda container_id, container_port: 18081
    )
    monkeypatch.setattr(webui, "start_daemon_compose", fake_start_daemon_compose)
    monkeypatch.setattr(webui, "start_daemon_logs", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "stop_daemon_compose", lambda *args, **kwargs: None)
    monkeypatch.setattr(
        main,
        "run_one_task",
        lambda client, suite, task, attempt, artifact_dir, *args, **kwargs: TaskResult(
            suite=suite.name,
            run_id="mixed-run",
            task_id=task.id,
            category=task.category,
            attempt=attempt,
            status="passed",
            rubric=[],
            rubric_pass_count=0,
            rubric_total=0,
            artifact_dir=str(artifact_dir),
        ),
    )

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "mixed-run",
            "--auto-agent-setup",
            "--max-concurrency",
            "1",
            "--no-judge",
        ]
    )

    assert rc == 0
    assert daemon_platforms == {"ios": "ios", "android": "android"}
    manifest = json.loads(
        (tmp_path / "runs" / "mixed-run" / "manifest.json").read_text(
            encoding="utf-8"
        )
    )
    assert manifest["target_platform"] == "mixed"


def test_mock_task_platform_uses_task_override(tmp_path):
    suite_path = tmp_path / "mock-suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "mock_suite",
                "mock_environment": {
                    "phone_bridge": {"platform": "android"},
                    "tools": {"screenshot": {"output": {"ok": True}}},
                },
                "tasks": [
                    {
                        "id": task_id,
                        "category": "diagnostic",
                        "prompt": "test policy",
                        "description_for_judge": "test policy",
                        "rubric": [{"id": "done", "check": "done"}],
                        "mock_environment": {
                            "phone_bridge": {"platform": "ios"},
                            "tools": {"screenshot": {"output": {"ok": True}}},
                        },
                    }
                    for task_id in ("one", "two")
                ],
            }
        ),
        encoding="utf-8",
    )

    suite = main.load_suite(suite_path)

    assert {
        suite_module.resolve_mock_task_platform(suite, task)
        for task in suite.tasks
    } == {suite_module.TargetPlatform.IOS}


def test_mock_suite_loading_rejects_effective_spec_without_platform(tmp_path):
    suite_path = tmp_path / "mock-suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "mock_suite",
                "tasks": [
                    {
                        "id": "ios",
                        "category": "diagnostic",
                        "prompt": "test policy",
                        "description_for_judge": "test policy",
                        "rubric": [{"id": "done", "check": "done"}],
                        "mock_environment": {
                            "phone_bridge": {"platform": "ios"},
                            "tools": {"screenshot": {"output": {"ok": True}}},
                        },
                    },
                    {
                        "id": "missing",
                        "category": "diagnostic",
                        "prompt": "test policy",
                        "description_for_judge": "test policy",
                        "rubric": [{"id": "done", "check": "done"}],
                        "mock_environment": {
                            "phone_bridge": {},
                            "tools": {"screenshot": {"output": {"ok": True}}},
                        },
                    },
                ],
            }
        ),
        encoding="utf-8",
    )

    with pytest.raises(ValueError, match="declare a target platform"):
        main.load_suite(suite_path)


def test_auto_agent_setup_caps_environment_concurrency(monkeypatch, tmp_path):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "mobile_suite",
                "tasks": [
                    {
                        "id": f"task_{i}",
                        "category": "diagnostic",
                        "prompt": "open",
                        "description_for_judge": "open",
                        "rubric": [{"id": "done", "check": "done"}],
                    }
                    for i in range(3)
                ],
            }
        ),
        encoding="utf-8",
    )

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            self.base_url = base_url
            self.benchmark_token = benchmark_token

        def close(self):
            pass

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "_read_environment_health", lambda url: {"platform": "android"})
    monkeypatch.setattr(main, "wait_for_agent_ready", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "generate_report_html", lambda run_dir: "<html></html>")
    monkeypatch.setattr(main, "call_environment_release", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "ensure_daemon_image", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "read_environment_bridge_concurrency", lambda *args, **kwargs: 5)
    def fake_prepare_run_config(base_config_dir, config_dir, **kwargs):
        config_dir.mkdir(parents=True, exist_ok=True)
        (config_dir / "control_token").write_text("test-token", encoding="utf-8")

    monkeypatch.setattr(webui, "prepare_run_config", fake_prepare_run_config)
    monkeypatch.setattr(webui, "docker_published_port", lambda container_id, container_port: 18081)
    monkeypatch.setattr(webui, "start_daemon_compose", lambda *args, **kwargs: "container-id")
    monkeypatch.setattr(webui, "start_daemon_logs", lambda *args, **kwargs: None)
    monkeypatch.setattr(webui, "stop_daemon_compose", lambda *args, **kwargs: None)
    monkeypatch.setattr(
        main,
        "run_one_task",
        lambda client, suite, task, attempt, artifact_dir, *args, **kwargs: TaskResult(
            suite=suite.name,
            run_id="capped-run",
            task_id=task.id,
            category=task.category,
            attempt=attempt,
            status="passed",
            rubric=[],
            rubric_pass_count=0,
            rubric_total=0,
            artifact_dir=str(artifact_dir),
        ),
    )

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "capped-run",
            "--environment-url",
            "http://127.0.0.1:19090",
            "--auto-agent-setup",
            "--max-concurrency",
            "2",
            "--no-judge",
        ]
    )

    assert rc == 0
    manifest = json.loads((tmp_path / "runs" / "capped-run" / "manifest.json").read_text(encoding="utf-8"))
    assert manifest["concurrency"] == 2


def test_auto_agent_setup_rejects_negative_max_concurrency_before_setup(monkeypatch, tmp_path, capsys):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "mobile_suite",
                "tasks": [
                    {
                        "id": "task_1",
                        "category": "diagnostic",
                        "prompt": "open",
                        "description_for_judge": "open",
                        "rubric": [{"id": "done", "check": "done"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )

    def fail_if_setup_runs(*args, **kwargs):
        raise AssertionError("daemon setup should not run for invalid max-concurrency")

    monkeypatch.setattr(webui, "ensure_daemon_image", fail_if_setup_runs)
    monkeypatch.setattr(webui, "read_environment_bridge_concurrency", fail_if_setup_runs)

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "bad-concurrency-run",
            "--environment-url",
            "http://127.0.0.1:19090",
            "--auto-agent-setup",
            "--max-concurrency",
            "-1",
            "--no-judge",
        ]
    )

    assert rc == 2
    assert "max-concurrency must be non-negative" in capsys.readouterr().err


def test_task_route_id_keeps_explicit_id_verbatim_across_attempts(tmp_path):
    import argparse
    from pathlib import Path

    from runner.suite import Suite

    suite = Suite(
        name="suite",
        global_reset={},
        tasks=[],
        sha256="",
        source_path=Path(tmp_path / "suite.json"),
    )
    derived = argparse.Namespace(benchmark_task_id="")
    explicit = argparse.Namespace(benchmark_task_id="webui:job-test")

    # Without an explicit id every attempt gets its own route, so parallel
    # environments can hand each attempt a separate device.
    assert main._task_route_id(derived, suite, "t1", 1, 2) == "suite.json:t1:attempt-1"
    assert main._task_route_id(derived, suite, "t1", 2, 2) == "suite.json:t1:attempt-2"
    # With an explicit id the caller already started its daemon under that id;
    # suffixing attempts here would break ownership on the second repeat.
    assert main._task_route_id(explicit, suite, "t1", 1, 2) == "webui:job-test"
    assert main._task_route_id(explicit, suite, "t1", 2, 2) == "webui:job-test"


def test_run_releases_environment_route_per_non_auto_attempt(monkeypatch, tmp_path):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps(
            {
                "name": "mobile_suite",
                "tasks": [
                    {
                        "id": "open_clock",
                        "category": "diagnostic",
                        "prompt": "open clock",
                        "description_for_judge": "open clock",
                        "rubric": [{"id": "done", "check": "done"}],
                    }
                ],
            }
        ),
        encoding="utf-8",
    )
    route_ids = []
    releases = []
    stale_clears = []

    class FakeClient:
        def __init__(self, base_url):
            self.base_url = base_url

        def health(self):
            return True

        def device_type(self):
            return "android"

        def close(self):
            pass

    def fake_run_one_task(client, suite, task, attempt, artifact_dir, *args, **kwargs):
        route_ids.append(kwargs["benchmark_task_id"])
        return TaskResult(
            suite=suite.name,
            run_id="route-run",
            task_id=task.id,
            category=task.category,
            attempt=attempt,
            status="passed",
            rubric=[],
            rubric_pass_count=0,
            rubric_total=0,
            artifact_dir=str(artifact_dir),
        )

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_ready", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(main, "run_one_task", fake_run_one_task)
    monkeypatch.setattr(main, "generate_report_html", lambda run_dir: "<html></html>")
    monkeypatch.setattr(main, "upload_report", lambda *args, **kwargs: False)
    monkeypatch.setattr(
        main,
        "call_environment_release",
        lambda environment_url, task_id=None, **kwargs: releases.append((environment_url, task_id)),
    )
    monkeypatch.setattr(main, "clear_stale_adb_android_owner", lambda url: stale_clears.append(url))
    monkeypatch.setattr(main, "_read_environment_health", lambda url: {"platform": "android"})

    rc = main.cli(
        [
            "run",
            "--suite",
            str(suite_path),
            "--out",
            str(tmp_path / "runs"),
            "--run-id",
            "route-run",
            "--environment-url",
            "http://127.0.0.1:19090",
            "--repeats",
            "2",
            "--inter-task-cooldown-sec",
            "0",
            "--no-judge",
        ]
    )

    assert rc == 0
    assert route_ids == ["suite.json:open_clock:attempt-1", "suite.json:open_clock:attempt-2"]
    manifest = json.loads(
        (tmp_path / "runs" / "route-run" / "manifest.json").read_text(encoding="utf-8")
    )
    assert manifest["metrics_k"] == 2
    assert releases == [
        ("http://127.0.0.1:19090", "suite.json:open_clock:attempt-1"),
        ("http://127.0.0.1:19090", "suite.json:open_clock:attempt-2"),
    ]
    assert stale_clears == ["http://127.0.0.1:19090"]


def test_manifest_metrics_k_uses_common_repeat_prefix(monkeypatch, tmp_path):
    suite_path = tmp_path / "suite.json"
    suite_path.write_text(
        json.dumps({
            "name": "mixed_repeats",
            "tasks": [
                {"id": "short", "category": "diagnostic", "description_for_judge": "short", "prompt": "short", "rubric": [{"id": "done", "check": "done"}], "repeats": 1},
                {"id": "long", "category": "diagnostic", "description_for_judge": "long", "prompt": "long", "rubric": [{"id": "done", "check": "done"}], "repeats": 3},
            ],
        }),
        encoding="utf-8",
    )

    class FakeClient:
        def __init__(self, base_url, benchmark_token=""):
            pass
        def health(self): return True
        def device_type(self): return "android"
        def close(self): pass

    monkeypatch.setattr(main, "AgentClient", FakeClient)
    monkeypatch.setattr(main, "wait_for_agent_clock", lambda *args, **kwargs: True)
    monkeypatch.setattr(
        main,
        "run_one_task",
        lambda client, suite, task, attempt, artifact_dir, *args, **kwargs: TaskResult(
            suite=suite.name,
            run_id="mixed",
            task_id=task.id,
            category=task.category,
            attempt=attempt,
            status="passed",
            rubric=[],
            artifact_dir=str(artifact_dir),
            metrics={"success": True, "agent_eligible": True},
        ),
    )
    monkeypatch.setattr(main, "generate_report_html", lambda run_dir: "<html></html>")
    monkeypatch.setattr(main, "upload_report", lambda *args, **kwargs: False)

    assert main.cli(["run", "--suite", str(suite_path), "--out", str(tmp_path / "runs"), "--run-id", "mixed", "--no-judge"]) == 0
    manifest = json.loads((tmp_path / "runs" / "mixed" / "manifest.json").read_text(encoding="utf-8"))
    assert manifest["metrics_k"] == 1
