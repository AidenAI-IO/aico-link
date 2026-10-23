from __future__ import annotations
import dataclasses as dc
import json
from pathlib import Path
from runner.judge import JudgeConfig, judge_task
from runner.suite import RubricItem, load_suite
from runner.report import now_iso
from runner.trace import extract_trace, apply_episode_tool_evidence


def _refresh_screenshot_evidence(trace: dict, attempt_dir: Path) -> bool:
    """Repair old screenshot flags without rewriting recorded execution gates."""
    history_path = attempt_dir / "history.json"
    if not history_path.exists():
        return False
    refreshed = extract_trace(json.loads(history_path.read_text("utf-8")))
    episode_path = attempt_dir / "episode.json"
    if episode_path.exists():
        apply_episode_tool_evidence(refreshed, json.loads(episode_path.read_text("utf-8")))
    recorded = trace.get("tool_calls", [])
    if len(recorded) != len(refreshed.tool_calls) or any(
        old.get("tool") != new.tool for old, new in zip(recorded, refreshed.tool_calls)
    ):
        return False
    for old, new in zip(recorded, refreshed.tool_calls):
        old["has_screenshot"] = bool(old.get("has_screenshot") or new.has_screenshot)
    return True


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
    suite_path = run_dir / "suite.json"
    suite = load_suite(suite_path) if suite_path.exists() else None
    prompts = {t.id: f"{suite.prompt_prefix.rstrip()}\n\n{t.prompt}".strip() for t in suite.tasks} if suite else {}
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
        # Judge-free tasks are decided by recorded assertions; keep them as-is.
        if row.get("rubric_total") == 0:
            new_results.append(row)
            continue
        # Semantic rejudging cannot recover missing execution evidence.
        if (
            metrics.get("episode_tool_evidence") != "applied"
            and (row.get("hard_assertions") or {}).get("required_tool_calls") is not None
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
            if _refresh_screenshot_evidence(trace, attempt_dir):
                metrics["screenshots_taken"] = sum(bool(call.get("has_screenshot")) for call in trace.get("tool_calls", []))
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
                task_prompt=row.get("task_prompt") or prompts.get(row["task_id"], ""),
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
