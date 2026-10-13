import json
from pathlib import Path
import pytest

from runner import rejudge
from runner.models import RubricVerdict


@pytest.mark.parametrize("case", ["text", "hard_failure", "missing_image", "missing_memory", "judge_error"])
def test_rejudge_preserves_execution_gates_and_updates_metrics(tmp_path: Path, monkeypatch, case):
    attempt = tmp_path / "tasks" / "task" / "attempt_1"
    attempt.mkdir(parents=True)
    trace = {"final_response": "done"}
    if case == "missing_memory":
        trace["memory_assertions"] = {"expected_count": 1}
    (attempt / "trace.json").write_text(json.dumps(trace))
    row = {
        "task_id": "task", "attempt": 1, "status": "failed",
        "rubric_spec": [{"id": "ok", "check": "Completed"}], "rubric_total": 1,
        "metrics": {"success": False, "quality_score": 0.0, "agent_eligible": True},
    }
    if case == "hard_failure":
        row["hard_assertions"] = {"required_tools": False}
    if case == "missing_image":
        row["metrics"]["post_screenshot_file"] = True
    (tmp_path / "results.jsonl").write_text(json.dumps(row) + "\n")
    judged = []

    def judge(**kwargs):
        judged.append(kwargs)
        if case == "judge_error":
            raise RuntimeError("judge unavailable")
        return type("Verdict", (), {"verdicts": [RubricVerdict("ok", "yes", "Completed")]})()

    monkeypatch.setattr(rejudge, "judge_task", judge)
    rejudge.rejudge_run(tmp_path, "model", "https://judge.example/v1")
    result = json.loads((tmp_path / "results.rejudged.jsonl").read_text())
    if case == "hard_failure":
        assert result == row
        assert judged == []
    elif case == "text":
        assert result["status"] == "passed"
        assert result["metrics"]["success"] is True
        assert result["metrics"]["quality_score"] == 1.0
        assert judged[0]["pre_screenshot"] is None
        assert judged[0]["post_screenshot"] is None
    else:
        assert result["status"] == "judge_error"
        assert result["metrics"]["success"] is None
        assert result["metrics"]["quality_score"] is None
        assert result["metrics"]["agent_eligible"] is False
        if case != "judge_error":
            assert judged == []


def test_rejudge_uses_latest_legacy_step_screenshot_when_post_missing(tmp_path: Path, monkeypatch):
    run_dir = tmp_path / "run"
    attempt_dir = run_dir / "tasks" / "task-1"
    steps_dir = attempt_dir / "steps"
    steps_dir.mkdir(parents=True)
    (attempt_dir / "pre.jpg").write_bytes(b"\xff\xd8pre")
    (steps_dir / "001.jpg").write_bytes(b"\xff\xd8old")
    latest = steps_dir / "002.jpg"
    latest.write_bytes(b"\xff\xd8latest")
    (attempt_dir / "trace.json").write_text(json.dumps({"final_response": "done"}), encoding="utf-8")
    (run_dir / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task-1",
                "attempt": 1,
                "description_for_judge": "judge",
                "rubric_spec": [{"id": "ok", "check": "ok"}],
                "rubric_total": 1,
                "metrics": {},
            }
        )
        + "\n",
        encoding="utf-8",
    )
    captured = {}

    class Verdict:
        verdicts = [RubricVerdict(id="ok", verdict="yes", reason="ok")]

    def fake_judge_task(**kwargs):
        captured["post_screenshot"] = kwargs["post_screenshot"]
        return Verdict()

    monkeypatch.setattr(rejudge, "judge_task", fake_judge_task)

    assert rejudge.rejudge_run(run_dir, "judge-model", "https://judge.example/v1") == 0

    assert captured["post_screenshot"] == latest
    rows = [json.loads(line) for line in (run_dir / "results.rejudged.jsonl").read_text(encoding="utf-8").splitlines()]
    assert rows[0]["status"] == "passed"
