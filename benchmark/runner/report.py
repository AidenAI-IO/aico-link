from __future__ import annotations
import dataclasses as dc
import json
import subprocess
from datetime import datetime, timezone
from pathlib import Path
from typing import Any
from runner.models import TaskResult
from runner.metrics import aggregate


_ERROR_METRIC_LABELS = {
    "error": "Error",
    "agent_error": "Agent Error",
    "judge_error": "Judge Error",
    "environment_state_error": "Environment State Error",
    "pre_screenshot_error": "Pre Screenshot Error",
    "post_screenshot_error": "Post Screenshot Error",
    "episode_error": "Episode Error",
    "rejudge_error": "Rejudge Error",
}


def task_error_details(metrics: dict[str, Any]) -> list[tuple[str, Any]]:
    """Return every concrete task error in a stable, human-readable order."""
    keys = list(_ERROR_METRIC_LABELS)
    keys.extend(
        sorted(
            key
            for key in metrics
            if key.endswith("_error") and key not in _ERROR_METRIC_LABELS
        )
    )
    return [
        (
            _ERROR_METRIC_LABELS.get(
                key,
                key.removesuffix("_error").replace("_", " ").title() + " Error",
            ),
            metrics[key],
        )
        for key in keys
        if metrics.get(key)
    ]


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat()

def git_sha(repo_root: Path) -> tuple[str, bool]:
    try:
        sha = subprocess.check_output(
            ["git", "rev-parse", "HEAD"], cwd=repo_root, text=True
        ).strip()
        dirty = bool(subprocess.check_output(
            ["git", "status", "--porcelain"], cwd=repo_root, text=True
        ).strip())
        return sha, dirty
    except Exception:
        return "", False

def write_jsonl(path: Path, results: list[TaskResult]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("w", encoding="utf-8") as fp:
        for r in results:
            fp.write(json.dumps(dc.asdict(r), ensure_ascii=False, sort_keys=True) + "\n")

def write_manifest(path: Path, manifest: dict[str, Any]) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(manifest, ensure_ascii=False, indent=2, sort_keys=True),
        encoding="utf-8",
    )


def write_metrics(
    path: Path,
    suite_name: str,
    manifest: dict[str, Any],
    results: list[TaskResult],
) -> None:
    payload = {
        "schema_version": manifest.get("metrics_schema_version", "p0-v1"),
        "suite": suite_name,
        "run_id": manifest.get("run_id", ""),
        "suite_sha256": manifest.get("suite_sha256"),
        "metrics_k": _manifest_k(manifest),
        "aggregate": aggregate(results, k=_manifest_k(manifest)),
    }
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(
        json.dumps(payload, ensure_ascii=False, indent=2, sort_keys=True),
        encoding="utf-8",
    )

def write_summary(path: Path, suite_name: str, manifest: dict[str, Any],
                  results: list[TaskResult]) -> None:
    agg = aggregate(results, k=_manifest_k(manifest))
    agent_label = manifest.get("agent_model") or manifest.get("agent_url") or "unknown"
    lines = [
        f"# {suite_name} — {manifest.get('run_id', '')}",
        "",
        f"Agent: {agent_label}",
        f"Judge: {(manifest.get('judge_config') or {}).get('provider', 'none')}"
        f" / {(manifest.get('judge_config') or {}).get('model', 'none')}",
        f"Attempts: {agg['passed']}/{agg['tasks']} passed across {agg.get('unique_tasks', 0)} tasks",
        f"Eligible attempts: {agg.get('agent_eligible_attempts', 0)}/{agg.get('attempts', 0)}",
        "",
        "## Capability Metrics",
        "",
        f"k={agg.get('metrics_k', 1)}; pass@1={_format_rate(agg.get('pass_at_1'))}; "
        f"pass@k={_format_rate(agg.get('pass_at_k'))}; pass^k={_format_rate(agg.get('pass_pow_k'))}",
        f"pass@k coverage={_format_coverage(agg.get('pass_at_k'))}; "
        f"attempt success={_format_rate(agg.get('attempt_success_rate'))}",
        f"first success attempt p50={_format_number((agg.get('first_success_attempt') or {}).get('p50'))}",
        f"oracle best score@k={_format_number((agg.get('oracle_best_score_at_k') or {}).get('value'))}",
        f"cost to first success: tokens p50={_format_number(((agg.get('cost_to_first_success') or {}).get('total_tokens') or {}).get('p50'))}; "
        f"wall p50={_format_number(((agg.get('cost_to_first_success') or {}).get('task_wall_ms') or {}).get('p50'))} ms; "
        f"cost p50={_format_number(((agg.get('cost_to_first_success') or {}).get('cost_usd') or {}).get('p50'))}",
        "",
        "| failure class | count |",
        "|---|---:|",
    ]
    for failure_class, count in (agg.get("failure_classes") or {}).items():
        lines.append(f"| {failure_class} | {count} |")
    lines += [
        "",
        "## By category",
        "",
        "| category | passed | total | rubric step % |",
        "|---|---|---|---|",
    ]
    for cat, c in agg["by_category"].items():
        pct = (100.0 * c["rubric_pass"] / c["rubric_total"]) if c["rubric_total"] else 0
        lines.append(f"| {cat} | {c['passed']} | {c['total']} | {pct:.0f}% |")
    lines += [
        "",
        "## Efficiency",
        "",
        f"task wall: p50 {_format_number((agg.get('task_wall_ms') or {}).get('p50'))} ms"
        f"    p90 {_format_number((agg.get('task_wall_ms') or {}).get('p90'))} ms",
        f"tool calls: p50 {_format_number((agg.get('tool_calls') or {}).get('p50'))}"
        f"    p90 {_format_number((agg.get('tool_calls') or {}).get('p90'))}",
        f"LLM calls: p50 {_format_number((agg.get('llm_calls') or {}).get('p50'))}"
        f"    device actions: p50 {_format_number((agg.get('device_actions') or {}).get('p50'))}",
        f"LLM / vision / device / screenshot p50: "
        f"{_format_number((agg.get('llm_time_ms') or {}).get('p50'))} / "
        f"{_format_number((agg.get('vision_llm_time_ms') or {}).get('p50'))} / "
        f"{_format_number((agg.get('device_execution_ms') or {}).get('p50'))} / "
        f"{_format_number((agg.get('screenshot_capture_ms') or {}).get('p50'))} ms",
        f"tokens: input {_format_number((agg.get('input_tokens') or {}).get('sum'))}"
        f" / output {_format_number((agg.get('output_tokens') or {}).get('sum'))}"
        f" / cached {_format_number((agg.get('cached_input_tokens') or {}).get('sum'))}",
        f"cost: {_format_number((agg.get('cost_usd') or {}).get('sum'))}",
        "",
    ]
    if agg.get("failure_stages"):
        coverage = agg.get("failure_stage_coverage") or {}
        lines += ["## First Failure Stages", "", "| stage | count |", "|---|---:|"]
        for stage, count in agg["failure_stages"].items():
            lines.append(f"| {stage} | {count} |")
        lines += [
            "",
            f"Classified-stage coverage: {coverage.get('classified', 0)}/{coverage.get('eligible_failures', 0)} eligible failures",
            "",
        ]
    trace_observations = agg.get("trace_observations") or {}
    if trace_observations:
        lines += [
            "## Trace Observations (informational)",
            "",
            "| observation | hit / observed |",
            "|---|---:|",
        ]
        for obs_id, obs in trace_observations.items():
            hits = obs.get("tasks_with_observation", 0)
            total = obs.get("tasks_observed", 0)
            lines.append(f"| {obs_id} | {hits}/{total} |")
        skill_obs = trace_observations.get("skill_read_device_operator") or {}
        if skill_obs.get("tasks_observed"):
            hits = skill_obs.get("tasks_with_observation", 0)
            total = skill_obs.get("tasks_observed", 0)
            lines += [
                "",
                f"device-operator skill activation/read: {hits}/{total} tasks",
            ]
        lines += [
            "",
        ]

    # Per-task results table
    per_task_data = agg.get("per_task", {})
    if per_task_data:
        lines += [
            "## Per-Task Results",
            "",
            "| Task ID | Status | Pass Rate | 1st✓ | Avg Score | Best Score | Avg Time |",
            "|---|---|---|---|---|---|---|",
        ]
        for task_id, task_metrics in sorted(per_task_data.items(), key=lambda x: x[1].get("pass_rate", 0)):
            pass_rate = task_metrics.get("pass_rate", 0.0)
            passed = task_metrics.get("passed", 0)
            eligible = task_metrics.get("eligible_attempts", 0)
            first_passed = task_metrics.get("first_attempt_passed")
            best_score = task_metrics.get("best_quality_score")
            avg_score = task_metrics.get("avg_quality_score")
            avg_wall = task_metrics.get("avg_wall_ms")

            pass_rate_str = f"{pass_rate * 100:.1f}%"
            status_str = f"{passed}/{eligible}"
            first_icon = "✓" if first_passed is True else ("✗" if first_passed is False else "-")
            score_str = f"{avg_score:.2f}" if avg_score is not None else "n/a"
            best_score_str = f"{best_score:.2f}" if best_score is not None else "n/a"
            wall_str = f"{avg_wall / 1000:.1f}s" if avg_wall is not None else "n/a"

            lines.append(f"| {task_id} | {status_str} | {pass_rate_str} | {first_icon} | {score_str} | {best_score_str} | {wall_str} |")

        lines += [""]

    lines += [
        "## Task Errors and Failures",
        "",
    ]
    for r in results:
        if r.status == "passed" and not task_error_details(r.metrics):
            continue
        reasons = _failure_details(r)
        lines.append(f"- **{r.task_id}** ({r.status}) — {reasons}")
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text("\n".join(lines) + "\n", encoding="utf-8")


def _failure_details(result: TaskResult) -> str:
    details = [
        f"{label}: {' '.join(str(value).split())}"
        for label, value in task_error_details(result.metrics)
    ]
    details.extend(
        f"{verdict.id}: {verdict.reason}"
        for verdict in result.rubric
        if verdict.verdict == "no"
    )
    return "; ".join(details) or result.status


def _format_number(value: Any) -> str:
    if value is None:
        return "n/a"
    if isinstance(value, float) and value.is_integer():
        return str(int(value))
    return f"{value:.2f}" if isinstance(value, float) else str(value)


def _format_rate(metric: Any) -> str:
    if not isinstance(metric, dict) or metric.get("value") is None:
        return "n/a"
    value = float(metric["value"]) * 100
    ci = metric.get("ci95") or {}
    if ci.get("lower") is None:
        return f"{value:.1f}%"
    return f"{value:.1f}% [{float(ci['lower']) * 100:.1f}%, {float(ci['upper']) * 100:.1f}%]"


def _format_coverage(metric: Any) -> str:
    if not isinstance(metric, dict):
        return "n/a"
    return f"{metric.get('eligible_tasks', 0)}/{metric.get('total_tasks', 0)} tasks"


def _manifest_k(manifest: dict[str, Any]) -> int:
    value = manifest.get("metrics_k", 1)
    try:
        return max(1, int(value))
    except (TypeError, ValueError):
        return 1
