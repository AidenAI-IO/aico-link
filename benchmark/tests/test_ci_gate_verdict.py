import json
from pathlib import Path

from ci.gate_verdict import judge, retry_matrix
from ci.plan import SuiteCase

CASE = SuiteCase(id="local", suite="suites/s.json", environment="isolated",
                 task_ids=["stable", "flaky", "broken", "unscored"])


def _write(runs_dir: Path, prefix: str, statuses: dict[str, str]) -> None:
    run_dir = runs_dir / f"{prefix}-local"
    run_dir.mkdir(parents=True)
    (run_dir / "results.jsonl").write_text(
        "".join(json.dumps({"task_id": t, "status": s}) + "\n" for t, s in statuses.items()),
        encoding="utf-8",
    )


def test_gate_passes_a_task_once_any_pass_succeeds_and_retries_only_the_rest(
    tmp_path: Path,
) -> None:
    _write(tmp_path, "first", {"stable": "passed", "flaky": "failed",
                               "broken": "failed", "unscored": "judge_error"})
    first = judge((CASE,), runs_dir=tmp_path, run_id_prefixes=["first"])
    assert retry_matrix((CASE,), first)["include"][0]["task_ids"] == [
        "flaky", "broken", "unscored",
    ]

    # The retry omits "unscored" entirely, as when its case fails to start.
    _write(tmp_path, "retry", {"flaky": "passed", "broken": "failed"})
    final = {v.task_id: v for v in judge((CASE,), runs_dir=tmp_path,
                                         run_id_prefixes=["first", "retry"])}

    assert final["stable"].statuses == ("passed",)
    assert final["flaky"].passed and final["flaky"].flaky
    assert not final["broken"].passed and final["broken"].kind == "regression"
    assert final["unscored"].statuses == ("judge_error", "missing")
    assert final["unscored"].kind == "unverified"
