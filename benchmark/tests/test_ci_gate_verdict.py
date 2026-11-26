import json
from pathlib import Path

from ci.gate_verdict import judge, retry_matrix
from ci.plan import SuiteCase

CASE = SuiteCase(id="local", suite="suites/s.json", environment="isolated",
                 task_ids=["stable", "flaky", "broken", "unscored", "text_call"])


def _write(runs_dir: Path, prefix: str, statuses: dict[str, str]) -> None:
    run_dir = runs_dir / f"{prefix}-local"
    run_dir.mkdir(parents=True)
    rows = []
    for task_id, status in statuses.items():
        reply = "<parcel_call><default_api:touch_gesture/></parcel_call>" if task_id == "text_call" else "done"
        task_dir = run_dir / "tasks" / task_id
        task_dir.mkdir(parents=True)
        (task_dir / "trace.json").write_text(
            json.dumps({"tool_calls": [], "final_response": reply}), encoding="utf-8"
        )
        rows.append({"task_id": task_id, "status": status,
                     "artifact_dir": f"/runner/benchmark/runs/{run_dir.name}/tasks/{task_id}"})
    (run_dir / "results.jsonl").write_text(
        "".join(json.dumps(row) + "\n" for row in rows), encoding="utf-8"
    )


def test_gate_passes_a_task_once_any_pass_succeeds_and_retries_only_the_rest(
    tmp_path: Path,
) -> None:
    _write(tmp_path, "first", {"stable": "passed", "flaky": "failed",
                               "broken": "failed", "unscored": "judge_error",
                               "text_call": "failed"})
    first = judge((CASE,), runs_dir=tmp_path, run_id_prefixes=["first"])
    assert retry_matrix((CASE,), first)["include"][0]["task_ids"] == [
        "flaky", "broken", "unscored", "text_call",
    ]

    # The retry omits "unscored" entirely, as when its case fails to start.
    _write(tmp_path, "retry", {"flaky": "passed", "broken": "failed", "text_call": "failed"})
    final = {v.task_id: v for v in judge((CASE,), runs_dir=tmp_path,
                                         run_id_prefixes=["first", "retry"])}

    assert final["stable"].statuses == ("passed",)
    assert final["flaky"].passed and final["flaky"].flaky
    assert not final["broken"].passed and final["broken"].kind == "regression"
    assert final["unscored"].statuses == ("judge_error", "missing")
    assert final["unscored"].kind == "unverified"
    assert final["broken"].blocking and final["unscored"].blocking
    assert final["text_call"].statuses == ("model_format", "model_format")
    assert final["text_call"].kind == "model_format" and not final["text_call"].blocking
