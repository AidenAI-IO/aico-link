from __future__ import annotations
import dataclasses as dc
import json
from pathlib import Path
from runner.judge import JudgeConfig, judge_task
from runner.suite import RubricItem
from runner.report import now_iso


def _post_screenshot_path(attempt_dir: Path) -> Path:
    post = attempt_dir / "post.jpg"
    if post.exists():
        return post
    legacy_steps = sorted((attempt_dir / "steps").glob("*.jpg"))
    return legacy_steps[-1] if legacy_steps else post


def rejudge_run(run_dir: Path, judge_model: str, judge_base_url: str) -> int:
    cfg = JudgeConfig(model=judge_model, base_url=judge_base_url)
    cache = run_dir / "_judge_cache"
    new_results = []
    for line in (run_dir / "results.jsonl").read_text("utf-8").splitlines():
        row = json.loads(line)
        metrics = row.setdefault("metrics", {})
        # Rejudging changes semantic verdicts, never recorded execution gates.
        if (
            row.get("status") in {"timeout", "skipped"}
            or row.get("hard_assertion_failures")
            or any(value is False for value in (row.get("hard_assertions") or {}).values())
            or metrics.get("agent_error")
            or metrics.get("first_failure_stage") == "setup"
        ):
            new_results.append(row)
            continue
        td = run_dir / "tasks" / row["task_id"]
        attempt_dir = td / f"attempt_{row['attempt']}" if (td / f"attempt_{row['attempt']}").exists() else td
        pre = attempt_dir / "pre.jpg"
        post = _post_screenshot_path(attempt_dir)
        if (metrics.get("pre_screenshot_file") and not pre.exists()) or (
            metrics.get("post_screenshot_file") and not post.exists()
        ):
            row["status"] = "judge_error"
            row["metrics"] = {**row.get("metrics", {}), "rejudge_error": "missing artifacts"}
            new_results.append(row)
            continue
        try:
            trace = json.loads((attempt_dir / "trace.json").read_text("utf-8"))
        except Exception as e:
            row["status"] = "judge_error"
            row.setdefault("metrics", {})["rejudge_error"] = f"trace load: {e}"
            new_results.append(row)
            continue
        rubric = [RubricItem(id=r["id"], check=r["check"]) for r in row.get("rubric_spec", [])]
        if not rubric:
            row["status"] = "judge_error"
            row["metrics"] = {**row.get("metrics", {}), "rejudge_error": "missing rubric_spec"}
            new_results.append(row)
            continue
        if trace.get("memory_assertions") is not None and trace.get("memory_state") is None:
            row["status"] = "judge_error"
            metrics["rejudge_error"] = "missing memory state evidence"
            new_results.append(row)
            continue
        try:
            verdict = judge_task(
                description=row.get("description_for_judge", ""),
                rubric=rubric, pre_screenshot=pre if pre.exists() else None,
                post_screenshot=post if post.exists() else None,
                trace=trace, final_response=trace.get("final_response", ""),
                cfg=cfg, cache_dir=cache,
            )
            row["rubric"] = [dc.asdict(v) for v in verdict.verdicts]
        except Exception as e:
            row["status"] = "judge_error"
            row.setdefault("metrics", {})["rejudge_error"] = str(e)
            new_results.append(row)
            continue
        row["rubric_total"] = len(rubric)
        row["rubric_pass_count"] = sum(1 for v in verdict.verdicts if v.verdict == "yes")
        row["status"] = "passed" if row["rubric_pass_count"] == row["rubric_total"] else "failed"
        metrics.pop("rejudge_error", None)
        metrics.pop("judge_error", None)
        metrics.update(
            success=row["status"] == "passed",
            agent_eligible=True,
            quality_score=row["rubric_pass_count"] / row["rubric_total"],
            failure_class=None if row["status"] == "passed" else "agent",
            first_failure_stage=None if row["status"] == "passed" else "unknown",
        )
        row["finished_at"] = now_iso()
        new_results.append(row)
    for row in new_results:
        if row["status"] == "judge_error":
            row.setdefault("metrics", {}).update(
                success=None, agent_eligible=False, quality_score=None,
                failure_class="evaluation", first_failure_stage="evaluation",
            )
    out = run_dir / "results.rejudged.jsonl"
    out.write_text("\n".join(json.dumps(r, ensure_ascii=False, sort_keys=True) for r in new_results) + "\n",
                   encoding="utf-8")
    print(f"wrote {out}")
    return 0
