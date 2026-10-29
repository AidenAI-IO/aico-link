import base64
import json
import re
from pathlib import Path

from runner.agent_client import ToolInvokeResult
from runner.html_report import generate_report_html, upload_report


class RecordingClient:
    def __init__(self):
        self.calls = []

    def invoke_tool(self, name, args):
        self.calls.append((name, args))
        return ToolInvokeResult(output="", is_error=False, duration_ms=1)


def _report_tasks(html: str) -> list[dict]:
    match = re.search(r"const TASKS = (.*?);\nconst rows", html, re.S)
    assert match, "report TASKS payload missing"
    return json.loads(match.group(1))


def test_upload_report_uploads_run_artifacts_for_benchmark_page(tmp_path: Path):
    run_dir = tmp_path / "2026-05-28_091421"
    run_dir.mkdir()
    (run_dir / "manifest.json").write_text(
        json.dumps({"run_id": "2026-05-28_091421"}), encoding="utf-8"
    )
    client = RecordingClient()

    assert upload_report(client, "<html>report</html>", run_dir=run_dir) is True

    command = client.calls[0][1]["command"]
    assert "/userdata/agent/benchmark/runs/2026-05-28_091421" in command
    assert "/userdata/agent/benchmark/runs/2026-05-28_091421/report.html" in command
    assert "/userdata/agent/benchmark/runs/2026-05-28_091421/manifest.json" in command


def test_upload_report_returns_false_when_manifest_is_missing(tmp_path: Path):
    run_dir = tmp_path / "2026-05-28_091421"
    run_dir.mkdir()
    client = RecordingClient()

    assert upload_report(client, "<html>report</html>", run_dir=run_dir) is False


def test_upload_report_returns_false_when_manifest_is_invalid(tmp_path: Path):
    run_dir = tmp_path / "2026-05-28_091421"
    run_dir.mkdir()
    (run_dir / "manifest.json").write_text("not json", encoding="utf-8")
    client = RecordingClient()

    assert upload_report(client, "<html>report</html>", run_dir=run_dir) is False


def test_generate_report_marks_timeout_as_fail_and_escapes_drawer_chips(tmp_path: Path):
    run_dir = tmp_path / "2026-05-28_091421"
    run_dir.mkdir()
    (run_dir / "manifest.json").write_text(
        json.dumps(
            {
                "run_id": "2026-05-28_091421",
                "suite_path": "suite.json",
                "totals": {"tasks": 1, "passed": 0, "failed": 0, "skipped": 0, "timeout": 1},
            }
        ),
        encoding="utf-8",
    )
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task-1",
                "category": "<script>alert(1)</script>",
                "status": "timeout",
                "rubric_pass_count": 0,
                "rubric_total": 1,
                "metrics": {"tool_calls": "<b>2</b>", "wall_ms": "<i>9</i>"},
            }
        )
        + "\n",
        encoding="utf-8",
    )

    html = generate_report_html(run_dir)

    assert '<span class="badge fail">Timeout</span>' in html
    assert "'<span class=\"chip\">' + esc(t.category)" in html
    assert "'<span class=\"chip\">' + esc(t.status)" in html
    assert "esc(String(t.tool_calls_count))" in html
    assert "formatTime(t.wall_ms)" in html


def test_generate_report_handles_missing_tool_call_metric(tmp_path: Path):
    run_dir = tmp_path / "2026-05-28_091421"
    run_dir.mkdir()
    (run_dir / "manifest.json").write_text(
        json.dumps(
            {
                "run_id": "2026-05-28_091421",
                "suite_path": "suite.json",
                "totals": {"tasks": 1, "passed": 0, "failed": 0, "skipped": 1},
            }
        ),
        encoding="utf-8",
    )
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task-1",
                "category": "infrastructure",
                "status": "skipped",
                "rubric_pass_count": 0,
                "rubric_total": 0,
                "metrics": {"tool_calls": None},
            }
        )
        + "\n",
        encoding="utf-8",
    )

    html = generate_report_html(run_dir)

    assert '<td class="mono">0</td>' in html
    assert _report_tasks(html)[0]["tool_calls_count"] == 0


def test_generate_report_shows_capability_metrics(tmp_path: Path):
    run_dir = tmp_path / "2026-05-28_091421"
    run_dir.mkdir()
    (run_dir / "manifest.json").write_text(
        json.dumps(
            {
                "run_id": "2026-05-28_091421",
                "suite_path": "suite.json",
                "totals": {"tasks": 1, "passed": 1, "failed": 0, "skipped": 0},
            }
        ),
        encoding="utf-8",
    )
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task-1",
                "category": "multi_step",
                "status": "passed",
                "rubric_pass_count": 1,
                "rubric_total": 1,
                "metrics": {"tool_calls": 1, "wall_ms": 9},
            }
        )
        + "\n",
        encoding="utf-8",
    )
    (run_dir / "metrics.json").write_text(
        json.dumps(
            {
                "metrics_k": 2,
                "aggregate": {
                    "metrics_k": 2,
                    "pass_at_1": {"value": 0.5, "eligible_tasks": 1, "total_tasks": 2},
                    "pass_at_k": {"value": 1.0, "eligible_tasks": 2, "total_tasks": 2},
                    "pass_pow_k": {"value": 0.5, "eligible_tasks": 1, "total_tasks": 2},
                    "oracle_best_score_at_k": {"value": 0.75, "eligible_tasks": 2},
                },
            }
        ),
        encoding="utf-8",
    )

    html = generate_report_html(run_dir)

    assert "Capability Metrics" in html
    assert "Pass@1" in html and "50.0%" in html
    assert "Pass@k" in html and "100.0%" in html
    assert "Pass^k" in html and "50.0%" in html
    assert "Oracle best score@k" in html and "0.75" in html
    assert "not executable best-of-k selection" in html
    # Check that metric help text is included
    assert "Success rate of first attempt per task" in html


def test_generate_report_includes_tool_hard_assertion_failures(tmp_path: Path):
    run_dir = tmp_path / "2026-05-28_091421"
    run_dir.mkdir()
    (run_dir / "manifest.json").write_text(
        json.dumps(
            {
                "run_id": "2026-05-28_091421",
                "suite_path": "suite.json",
                "totals": {"tasks": 1, "passed": 0, "failed": 1, "skipped": 0},
            }
        ),
        encoding="utf-8",
    )
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task-1",
                "category": "multi_step",
                "status": "failed",
                "rubric_pass_count": 0,
                "rubric_total": 1,
                "hard_assertion_failures": [
                    {
                        "id": "required_tools",
                        "label": "Required Tools",
                        "requirement": "Must call: screenshot, tap.",
                        "actual": "Missing: tap. Used: screenshot.",
                    },
                    {
                        "id": "forbidden_tools",
                        "label": "Forbidden Tools",
                        "requirement": "Must not call: shell.",
                        "actual": "Forbidden calls: shell at step 2. Used: screenshot, shell.",
                    },
                ],
                "metrics": {"tool_calls": 2, "wall_ms": 9},
            }
        )
        + "\n",
        encoding="utf-8",
    )

    html = generate_report_html(run_dir)

    assert "Required Tools" in html
    assert "Forbidden Tools" in html
    assert "Requirement" in html
    assert "Actual" in html
    task = _report_tasks(html)[0]
    assert task["hard_assertion_failures"] == [
        {
            "id": "required_tools",
            "label": "Required Tools",
            "requirement": "Must call: screenshot, tap.",
            "actual": "Missing: tap. Used: screenshot.",
        },
        {
            "id": "forbidden_tools",
            "label": "Forbidden Tools",
            "requirement": "Must not call: shell.",
            "actual": "Forbidden calls: shell at step 2. Used: screenshot, shell.",
        },
    ]


def test_generate_report_embeds_failed_task_error_log(tmp_path: Path):
    run_dir = tmp_path / "2026-05-28_091421"
    task_dir = run_dir / "tasks" / "task-1"
    task_dir.mkdir(parents=True)
    (run_dir / "manifest.json").write_text(
        json.dumps(
            {
                "run_id": "2026-05-28_091421",
                "suite_path": "suite.json",
                "totals": {"tasks": 1, "passed": 0, "failed": 1, "skipped": 0},
            }
        ),
        encoding="utf-8",
    )
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task-1",
                "category": "multi_step",
                "status": "failed",
                "rubric_pass_count": 0,
                "rubric_total": 1,
                "hard_assertion_failures": [
                    {
                        "id": "max_tool_calls",
                        "label": "Max Tool Calls",
                        "requirement": "Use at most 1 tool call(s).",
                        "actual": "Used 2 tool call(s).",
                    }
                ],
                "metrics": {"tool_calls": 2, "wall_ms": 9},
            }
        )
        + "\n",
        encoding="utf-8",
    )
    (task_dir / "trace.json").write_text(json.dumps({"final_response": "wrong", "events": ["TraceBoom"]}), encoding="utf-8")
    (task_dir / "history.json").write_text(json.dumps([{"type": "user", "content": "Do task"}]), encoding="utf-8")
    (task_dir / "judge.json").write_text(json.dumps({"reason": "JudgeBoom"}), encoding="utf-8")

    html = generate_report_html(run_dir)
    task = _report_tasks(html)[0]

    assert "Max Tool Calls" in task["error_log_detail"]
    assert "Requirement: Use at most 1 tool call(s)." in task["error_log_detail"]
    assert "Actual: Used 2 tool call(s)." in task["error_log_detail"]
    assert "JudgeBoom" not in task["error_log_detail"]
    assert "evidence_detail" not in task
    assert "tasks/task-1/trace.json" in task["artifacts_detail"]
    assert "tasks/task-1/history.json" in task["artifacts_detail"]
    assert "tasks/task-1/judge.json" in task["artifacts_detail"]
    assert "<strong>Error Log</strong>" in html


def test_generate_report_accepts_relative_run_dir_with_task_artifacts(tmp_path: Path, monkeypatch):
    workspace = tmp_path / "benchmark"
    run_dir = workspace / "runs" / "2026-05-28_091421"
    task_dir = run_dir / "tasks" / "task-1"
    task_dir.mkdir(parents=True)
    (run_dir / "manifest.json").write_text(
        json.dumps(
            {
                "run_id": "2026-05-28_091421",
                "suite_path": "suite.json",
                "totals": {"tasks": 1, "passed": 0, "failed": 1, "skipped": 0},
            }
        ),
        encoding="utf-8",
    )
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task-1",
                "category": "multi_step",
                "status": "failed",
                "rubric_pass_count": 0,
                "rubric_total": 1,
                "metrics": {"tool_calls": 2, "wall_ms": 9},
            }
        )
        + "\n",
        encoding="utf-8",
    )
    (task_dir / "trace.json").write_text(json.dumps({"final_response": "wrong"}), encoding="utf-8")
    (task_dir / "history.json").write_text(json.dumps([{"type": "user", "content": "Do task"}]), encoding="utf-8")
    (task_dir / "judge.json").write_text(json.dumps({"reason": "JudgeBoom"}), encoding="utf-8")

    monkeypatch.chdir(workspace)
    html = generate_report_html(Path("runs") / "2026-05-28_091421")
    task = _report_tasks(html)[0]

    assert "tasks/task-1/trace.json" in task["artifacts_detail"]
    assert "tasks/task-1/judge.json" in task["artifacts_detail"]
    assert "JudgeBoom" not in task["error_log_detail"]


def test_generate_report_includes_full_trace_with_screenshots(tmp_path: Path):
    run_dir = tmp_path / "2026-05-28_091421"
    task_dir = run_dir / "tasks" / "task-1"
    steps_dir = task_dir / "steps"
    steps_dir.mkdir(parents=True)
    pre = b"\x89PNG\r\n\x1a\npre-shot"
    post = b"\xff\xd8post-shot"
    step = b"\xff\xd8step-shot"
    (task_dir / "pre.jpg").write_bytes(pre)
    (task_dir / "post.jpg").write_bytes(post)
    (steps_dir / "step_01_screenshot.jpg").write_bytes(step)
    (task_dir / "step_1.jpg").write_bytes(step)
    image_payload = base64.b64encode(b"history-shot").decode("ascii")
    (task_dir / "history.json").write_text(
        json.dumps(
            [
                {"type": "user", "content": "open settings"},
                {"type": "tool_call", "tool_name": "screenshot", "tool_input": "{}"},
                {
                    "type": "tool_result",
                    "tool_name": "screenshot",
                    "content": json.dumps({"data": image_payload, "width": 10, "height": 20, "format": "jpeg"}),
                },
                {"type": "assistant", "content": "done"},
            ]
        ),
        encoding="utf-8",
    )
    (task_dir / "trace.json").write_text(
        json.dumps(
            {
                "tool_calls": [{"step": 1, "tool": "screenshot", "input": {}, "has_screenshot": True}],
                "final_response": "done",
                "total_tool_calls": 1,
            }
        ),
        encoding="utf-8",
    )
    (run_dir / "manifest.json").write_text(
        json.dumps(
            {
                "run_id": "2026-05-28_091421",
                "suite_path": "suite.json",
                "totals": {"tasks": 1, "passed": 1, "failed": 0, "skipped": 0},
            }
        ),
        encoding="utf-8",
    )
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task-1",
                "category": "diagnostic",
                "status": "passed",
                "rubric_pass_count": 1,
                "rubric_total": 1,
                "rubric": [{"id": "ok", "reason": "looks correct", "verdict": "yes"}],
                "metrics": {"tool_calls": 1, "wall_ms": 9, "screenshots_taken": 1},
            }
        )
        + "\n",
        encoding="utf-8",
    )

    html = generate_report_html(run_dir)

    assert "View full trace" in html
    assert "Pre screenshot" in html
    assert "Post screenshot" in html
    assert "Tool call #1: screenshot" in html
    assert "<base64 image data" in html
    assert f"data:image/png;base64,{base64.b64encode(pre).decode('ascii')}" in html
    assert f"data:image/jpeg;base64,{base64.b64encode(post).decode('ascii')}" in html
    assert f"data:image/jpeg;base64,{base64.b64encode(step).decode('ascii')}" in html
    assert image_payload not in html


def test_generate_report_uses_result_artifact_dir_for_attempt_trace(tmp_path: Path):
    run_dir = tmp_path / "2026-05-28_091421"
    base_dir = run_dir / "tasks" / "task-1"
    attempt_dir = base_dir / "attempt_2"
    base_dir.mkdir(parents=True)
    attempt_dir.mkdir(parents=True)
    base_post = b"\xff\xd8base-post"
    attempt_pre = b"\xff\xd8attempt-pre"
    attempt_post = b"\xff\xd8attempt-post"
    (base_dir / "post.jpg").write_bytes(base_post)
    (attempt_dir / "pre.jpg").write_bytes(attempt_pre)
    (attempt_dir / "post.jpg").write_bytes(attempt_post)
    (attempt_dir / "history.json").write_text(
        json.dumps([{"type": "user", "content": "attempt prompt"}]),
        encoding="utf-8",
    )
    (attempt_dir / "trace.json").write_text(
        json.dumps({"tool_calls": [], "final_response": "attempt response"}),
        encoding="utf-8",
    )
    (run_dir / "manifest.json").write_text(
        json.dumps(
            {
                "run_id": "2026-05-28_091421",
                "suite_path": "suite.json",
                "totals": {"tasks": 1, "passed": 1, "failed": 0, "skipped": 0},
            }
        ),
        encoding="utf-8",
    )
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task-1",
                "category": "diagnostic",
                "attempt": 2,
                "status": "passed",
                "rubric_pass_count": 1,
                "rubric_total": 1,
                "metrics": {"tool_calls": 0, "wall_ms": 9},
                "artifact_dir": str(attempt_dir),
            }
        )
        + "\n",
        encoding="utf-8",
    )

    task = _report_tasks(generate_report_html(run_dir))[0]

    assert task["prompt"] == "attempt prompt"
    assert task["response"] == "attempt response"
    assert f"data:image/jpeg;base64,{base64.b64encode(attempt_pre).decode('ascii')}" == task["full_trace"]["pre_screenshot"]
    assert f"data:image/jpeg;base64,{base64.b64encode(attempt_post).decode('ascii')}" == task["full_trace"]["post_screenshot"]
    assert base64.b64encode(base_post).decode("ascii") not in json.dumps(task)
    assert "tasks/task-1/attempt_2/trace.json" in task["artifacts_detail"]


def test_generate_report_includes_llm_analysis_section(tmp_path: Path):
    run_dir = tmp_path / "run"
    run_dir.mkdir()
    (run_dir / "manifest.json").write_text(
        json.dumps({"run_id": "run", "totals": {"tasks": 0, "passed": 0, "failed": 0}}),
        encoding="utf-8",
    )
    (run_dir / "results.jsonl").write_text("", encoding="utf-8")
    (run_dir / "llm_analysis.md").write_text(
        "# LLM 基准分析\n\nRoot cause summary", encoding="utf-8"
    )

    html = generate_report_html(run_dir)

    assert "LLM 分析" in html
    assert "Root cause summary" in html


def test_generate_report_includes_all_task_error_fields(tmp_path: Path):
    run_dir = tmp_path / "run"
    run_dir.mkdir()
    (run_dir / "manifest.json").write_text(
        json.dumps({"run_id": "run", "totals": {"tasks": 1, "passed": 1}}),
        encoding="utf-8",
    )
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task-1",
                "status": "passed",
                "metrics": {
                    "environment_state_error": "bridge unavailable",
                    "pre_screenshot_error": "provider unavailable",
                    "episode_error": "episode unavailable",
                },
            }
        )
        + "\n",
        encoding="utf-8",
    )

    html = generate_report_html(run_dir)

    assert "Environment State Error" in html
    assert "Pre Screenshot Error" in html
    assert "Episode Error" in html


def test_upload_report_uploads_analysis_artifacts(tmp_path: Path):
    run_dir = tmp_path / "run-1"
    run_dir.mkdir()
    (run_dir / "manifest.json").write_text('{"run_id":"run-1"}', encoding="utf-8")
    (run_dir / "llm_analysis.md").write_text("analysis md", encoding="utf-8")
    (run_dir / "llm_analysis.json").write_text('{"summary":"analysis"}', encoding="utf-8")
    client = RecordingClient()

    assert upload_report(client, "<html></html>", run_dir) is True
    command = client.calls[0][1]["command"]
    assert "llm_analysis.md" in command
    assert "llm_analysis.json" in command
