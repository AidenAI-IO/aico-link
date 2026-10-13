import json
from pathlib import Path

from runner.models import TaskResult
from runner.report import write_metrics, write_summary


def test_write_summary_describes_skill_activation_or_read(tmp_path: Path):
    result = TaskResult(
        suite="phone",
        run_id="run-1",
        task_id="open_settings",
        category="single_step",
        attempt=1,
        status="passed",
        rubric=[],
        rubric_pass_count=0,
        rubric_total=0,
        metrics={
            "trace_observations": [
                {
                    "id": "skill_read_device_operator",
                    "passed": True,
                    "reason": "Trace contains skill_read for 'device-operator'.",
                }
            ]
        },
        description_for_judge="Open Settings.",
    )

    write_summary(
        tmp_path / "summary.md",
        "phone",
        {"run_id": "run-1", "agent_model": "test-model"},
        [result],
    )

    text = (tmp_path / "summary.md").read_text(encoding="utf-8")
    assert "Agent: test-model" in text
    assert "device-operator skill_read calls: 1/1 tasks" in text
    assert "device-operator skill_read:" not in text


def test_write_metrics_writes_recomputable_p0_aggregate(tmp_path: Path):
    results = [
        TaskResult(
            suite="phone",
            run_id="run-1",
            task_id="open_settings",
            category="single_step",
            attempt=1,
            status="passed",
            rubric=[],
            metrics={"success": True, "agent_eligible": True, "task_wall_ms": 100},
        ),
        TaskResult(
            suite="phone",
            run_id="run-1",
            task_id="open_settings",
            category="single_step",
            attempt=2,
            status="failed",
            rubric=[],
            metrics={"success": False, "agent_eligible": True, "task_wall_ms": 300},
        ),
    ]
    manifest = {"run_id": "run-1", "metrics_schema_version": "p0-v1", "metrics_k": 2}

    write_metrics(tmp_path / "metrics.json", "phone", manifest, results)

    payload = json.loads((tmp_path / "metrics.json").read_text(encoding="utf-8"))
    assert payload["schema_version"] == "p0-v1"
    assert payload["suite"] == "phone"
    assert payload["run_id"] == "run-1"
    assert payload["aggregate"]["pass_at_k"]["value"] == 1.0
    assert payload["aggregate"]["pass_pow_k"]["value"] == 0.0
    assert payload["aggregate"]["task_wall_ms"]["p50"] == 200


def test_write_summary_includes_oracle_best_score(tmp_path: Path):
    results = [
        TaskResult(
            suite="phone",
            run_id="run-1",
            task_id="open_settings",
            category="single_step",
            attempt=1,
            status="failed",
            rubric=[],
            metrics={"success": False, "agent_eligible": True, "quality_score": 0.25},
        ),
        TaskResult(
            suite="phone",
            run_id="run-1",
            task_id="open_settings",
            category="single_step",
            attempt=2,
            status="passed",
            rubric=[],
            metrics={"success": True, "agent_eligible": True, "quality_score": 1.0},
        ),
    ]

    write_summary(
        tmp_path / "summary.md",
        "phone",
        {"run_id": "run-1", "metrics_k": 2},
        results,
    )

    assert "oracle best score@k=1" in (tmp_path / "summary.md").read_text(encoding="utf-8")


def test_write_summary_includes_task_error_details(tmp_path: Path):
    result = TaskResult(
        suite="phone",
        run_id="run-1",
        task_id="open_settings",
        category="single_step",
        attempt=1,
        status="judge_error",
        rubric=[],
        metrics={
            "error": "setup failed",
            "agent_error": "request reset",
            "judge_error": "malformed JSON",
        },
    )

    write_summary(
        tmp_path / "summary.md",
        "phone",
        {"run_id": "run-1"},
        [result],
    )

    text = (tmp_path / "summary.md").read_text(encoding="utf-8")
    assert (
        "- **open_settings** (judge_error) — Error: setup failed; "
        "Agent Error: request reset; Judge Error: malformed JSON"
    ) in text


def test_write_summary_includes_environment_and_artifact_errors(tmp_path: Path):
    result = TaskResult(
        suite="phone",
        run_id="run-1",
        task_id="open_settings",
        category="single_step",
        attempt=1,
        status="passed",
        rubric=[],
        metrics={
            "environment_state_error": "bridge unavailable",
            "pre_screenshot_error": "provider unavailable",
            "episode_error": "episode unavailable",
        },
    )

    write_summary(tmp_path / "summary.md", "phone", {"run_id": "run-1"}, [result])

    text = (tmp_path / "summary.md").read_text(encoding="utf-8")
    assert "Environment State Error: bridge unavailable" in text
    assert "Pre Screenshot Error: provider unavailable" in text
    assert "Episode Error: episode unavailable" in text
