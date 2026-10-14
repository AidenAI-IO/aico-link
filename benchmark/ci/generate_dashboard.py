"""Generate a benchmark dashboard and weekly report from Langfuse history."""

from __future__ import annotations

import argparse
import json
import os
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from ci.fetch_langfuse_history import COMPARISON_FIELDS, parse_timestamp, comparison_key, fetch_benchmark_history
from ci.weekly_report import summarize_weekly, render_weekly
from ci.dashboard_metrics import METRIC_CHARTS


# A suite whose pass@1 drops by at least this much against its own previous run
# is called out as a regression. Benchmarks are noisy, so a threshold below this
# would flag ordinary run-to-run variance.
REGRESSION_THRESHOLD = 0.05

# Plain HTML (no Python format braces); the JSON payload replaces one marker.
_TEMPLATE_PATH = Path(__file__).with_name("dashboard_template.html")


def generate_dashboard(
    *,
    history: list[dict[str, Any]],
    output_file: Path,
    data_output: Path | None = None,
    repo: str = "",
    warnings: list[str] | None = None,
    weekly_output: Path | None = None,
    report_end: datetime | None = None,
) -> None:
    """
    Generate the dashboard HTML from benchmark history.

    Args:
        history: Per-suite run records from fetch_benchmark_history
        output_file: Path to write the HTML dashboard to
        data_output: Optional path to also write the underlying JSON data
        repo: GitHub repository ("owner/repo") used to build commit links
    """
    report_end = report_end or datetime.now(timezone.utc)
    display_start = report_end - timedelta(days=7)
    display_history = [record for record in history
                       if (stamp := parse_timestamp(record.get("timestamp"))) is not None
                       and display_start <= stamp < report_end]
    runs = _group_into_runs(display_history)
    regressions = _find_regressions(runs)
    suites = _summarize_suites(runs)
    warnings = warnings or []
    weekly = summarize_weekly(history, end=report_end)
    if weekly_output is not None:
        weekly_output.parent.mkdir(parents=True, exist_ok=True)
        weekly_output.write_text(render_weekly(weekly, warnings), encoding="utf-8")

    if data_output is not None:
        data_output.parent.mkdir(parents=True, exist_ok=True)
        data_output.write_text(
            json.dumps(
                {
                    "generated_at": report_end.isoformat(),
                    "repo": repo,
                    "runs": runs,
                    "suites": suites,
                    "regressions": regressions,
                    "history": history,
                    "warnings": warnings,
                    "weekly": weekly,
                },
                indent=2,
            ),
            encoding="utf-8",
        )

    output_file.parent.mkdir(parents=True, exist_ok=True)
    output_file.write_text(
        _render_html(runs=runs, suites=suites, regressions=regressions, repo=repo,
                     warnings=warnings, report_end=report_end),
        encoding="utf-8",
    )

    print(f"Dashboard generated: {output_file}")
    print(f"  Workflow runs: {len(runs)} ({len(history)} suite records)")
    if runs:
        latest = runs[-1]
        print(f"  Latest: {latest['commit_sha'][:8]} on {latest['git_branch']}")
        print(f"    pass@1 {_percent(latest['pass_at_1'])}"
              f" ({latest['passed']}/{latest['eligible_tasks']} eligible first attempts)")
    if regressions:
        print(f"  Regressions: {len(regressions)}")
        for item in regressions:
            print(f"    {item['suite']}: {_percent(item['previous'])}"
                  f" -> {_percent(item['current'])}")


def _group_into_runs(history: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """
    Collapse per-suite records into one entry per workflow run.

    A single workflow run executes every suite in the profile, so the dashboard's
    headline rate weights suites by eligible first attempts. Unavailable metrics
    make the overall rate unknown instead of silently excluding a suite.

    Args:
        history: Per-suite run records, sorted oldest first

    Returns:
        One record per workflow run, sorted oldest first
    """
    grouped: dict[Any, dict[str, Any]] = {}
    for record in history:
        key = (record.get("workflow_run_id"), record.get("workflow_run_attempt")) if record.get("workflow_run_id") else record.get("run_id")
        run = grouped.get(key)
        if run is None:
            run = {
                "workflow_run_id": record.get("workflow_run_id"),
                "workflow_run_number": record.get("workflow_run_number"),
                "workflow_run_attempt": record.get("workflow_run_attempt"),
                "timestamp": record.get("timestamp"),
                "commit_sha": record.get("commit_sha") or "unknown",
                "git_branch": record.get("git_branch"),
                "agent_model": record.get("agent_model"),
                "tasks": 0,
                "passed": 0,
                "eligible_tasks": 0,
                "metrics_complete": True,
                "suites": [],
                "failure_classes": {},
            }
            grouped[key] = run

        tasks = record.get("tasks")
        passed = record.get("passed")
        eligible = record.get("eligible_tasks")
        complete = not record.get("missing_metrics") and all(v is not None for v in (tasks, passed, eligible))
        run["metrics_complete"] = run["metrics_complete"] and complete
        if complete:
            run["tasks"] += int(tasks)
            run["passed"] += int(passed)
            run["eligible_tasks"] += int(eligible)
        run["timestamp"] = min(run["timestamp"], record["timestamp"])
        run["suites"].append({
            **{field: record.get(field) for field in COMPARISON_FIELDS},
            "git_dirty": record.get("git_dirty"),
            "comparison_key": comparison_key(record),
            "suite": record.get("suite") or "unknown",
            "timestamp": record.get("timestamp"),
            "commit_sha": record.get("commit_sha") or "unknown",
            "run_id": record.get("run_id"),
            "experiment_id": record.get("experiment_id"),
            "tasks": tasks,
            "passed": passed,
            "eligible_tasks": eligible,
            "pass_at_1": record.get("pass_at_1"),
            "metrics_complete": complete,
            "distributions": record.get("distributions") or {},
            "failure_stages": record.get("failure_stages") or {},
            **{field: record.get(field) for field in (
                "failure_details", "failure_details_status", "failure_details_inspected",
                "failure_details_expected", "failure_details_errors")},
            **{field: record.get(field) for field in (
                "total_attempts", "eligible_attempts", "failure_stage_coverage",
                "classified_failures", "eligible_failures")},
            **{f"{metric}{suffix}": record.get(f"{metric}{suffix}")
               for metric in ("pass_at_k", "pass_pow_k")
               for suffix in ("", "_successes", "_eligible_tasks", "_total_tasks")},
            "wall_ms_p50": record.get("wall_ms_p50"),
            **{field: record.get(field) for field in (
                "cost_usd_sum", "total_tokens_sum", "total_tokens_p50", "cost_usd_p50")},
            "failure_classes": record.get("failure_classes") or {},
        })
        for name, count in (record.get("failure_classes") or {}).items():
            run["failure_classes"][name] = run["failure_classes"].get(name, 0) + count

    runs = sorted(grouped.values(), key=lambda run: run["timestamp"] or "")
    for run in runs:
        run["suites"].sort(key=lambda suite: suite["suite"])
        run["pass_at_1"] = run["passed"] / run["eligible_tasks"] if run["eligible_tasks"] and run["metrics_complete"] else None
        # A pooled k-attempt rate only has a meaning when all suites use the
        # same known k. Its denominator differs from first-attempt eligibility.
        k_values = {str(suite.get("metrics_k")) for suite in run["suites"]}
        try:
            normalized_k = {float(k) for k in k_values}
            k = next(iter(normalized_k))
            run["metrics_k"] = int(k) if len(normalized_k) == 1 and k > 0 and k.is_integer() else None
        except (ValueError, OverflowError):
            run["metrics_k"] = None
        for metric in ("pass_at_k", "pass_pow_k"):
            counts = [(suite.get(f"{metric}_successes"), suite.get(f"{metric}_eligible_tasks"))
                      for suite in run["suites"]]
            complete = run["metrics_k"] is not None and all(
                successes is not None and eligible is not None and 0 <= successes <= eligible
                and (not eligible or suite.get(metric) is not None)
                for suite, (successes, eligible) in zip(run["suites"], counts)
            )
            successes = sum(pair[0] for pair in counts) if complete else None
            eligible = sum(pair[1] for pair in counts) if complete else None
            run[f"{metric}_successes"] = successes
            run[f"{metric}_eligible_tasks"] = eligible
            run[metric] = successes / eligible if eligible else None
        if not run["metrics_complete"]:
            run["tasks"] = run["passed"] = run["eligible_tasks"] = None
    return runs


def _summarize_suites(runs: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """
    Build each dataset's history, preserving all metrics and run identities.

    Args:
        runs: Workflow runs, sorted oldest first

    Returns:
        Suite summaries sorted worst-performing first
    """
    series: dict[str, list[dict[str, Any]]] = {}
    for run in runs:
        for suite in run["suites"]:
            series.setdefault(suite["suite"], []).append({
                **suite,
                "timestamp": suite.get("timestamp") or run["timestamp"],
                "commit_sha": suite.get("commit_sha") or run["commit_sha"],
                **{field: run.get(field) for field in (
                    "workflow_run_id", "workflow_run_number", "workflow_run_attempt")},
            })

    summaries = []
    for name, points in series.items():
        points.sort(key=lambda point: point["timestamp"] or "")
        latest = points[-1]
        previous = next((p for p in reversed(points[:-1])
                         if latest["comparison_key"] is not None
                         and p["comparison_key"] == latest["comparison_key"]), None)
        # Metric deltas and change direction require matching configurations.
        metric_deltas = {metric: _delta(latest.get(metric), previous.get(metric) if previous else None)
                         for metric in ("pass_at_1", "pass_at_k", "pass_pow_k")}
        changes = [value for value in metric_deltas.values() if value is not None]
        increases = any(value > 0 for value in changes)
        decreases = any(value < 0 for value in changes)
        direction = ("mixed" if increases and decreases else "improved" if increases
                     else "declined" if decreases else "unchanged" if changes else "unavailable")
        comparison_status = (
            "No baseline" if len(points) < 2 else
            "Configuration unverified" if latest["comparison_key"] is None else
            "Configuration changed" if previous is None else
            "Missing scores" if not changes else
            "Comparable"
        )
        summaries.append({
            "suite": name,
            "latest": latest,
            "previous": previous,
            "metric_deltas": metric_deltas,
            "change_direction": direction,
            "comparison_status": comparison_status,
            "delta": _delta(latest["pass_at_1"], previous["pass_at_1"] if previous else None),
            "points": points,
        })

    # Unknown rates sort last; among known ones the weakest suite needs attention
    # first, which is what someone opening the dashboard is looking for.
    summaries.sort(key=lambda item: (
        item["latest"]["pass_at_1"] is None,
        item["latest"]["pass_at_1"] or 0.0,
    ))
    return summaries


def _find_regressions(runs: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """
    Find suites whose pass@1 fell meaningfully in the most recent run.

    Args:
        runs: Workflow runs, sorted oldest first

    Returns:
        Regressions, largest drop first
    """
    if len(runs) < 2:
        return []

    latest = runs[-1]

    regressions = []
    for suite in latest["suites"]:
        current = suite["pass_at_1"]
        previous = next((run for run in reversed(runs[:-1]) if any(
            candidate["suite"] == suite["suite"] and suite["comparison_key"] is not None
            and candidate["comparison_key"] == suite["comparison_key"] for candidate in run["suites"])), None)
        prior = next((candidate["pass_at_1"] for candidate in previous["suites"]
                      if candidate["suite"] == suite["suite"] and candidate["comparison_key"] == suite["comparison_key"]), None) if previous else None
        if current is None or prior is None:
            continue
        if prior - current >= REGRESSION_THRESHOLD - 1e-12:
            regressions.append({
                "suite": suite["suite"],
                "current": current,
                "previous": prior,
                "delta": current - prior,
                "passed": suite["passed"],
                "tasks": suite["tasks"],
                "eligible_tasks": suite["eligible_tasks"],
                "commit_sha": latest["commit_sha"],
                "previous_commit_sha": previous["commit_sha"],
            })

    regressions.sort(key=lambda item: item["delta"])
    return regressions


def _delta(current: float | None, previous: float | None) -> float | None:
    """Return current minus previous when both values are known."""
    if current is None or previous is None:
        return None
    return current - previous


def _percent(value: float | None) -> str:
    """Format a 0..1 rate as a percentage, or a dash when unknown."""
    if value is None:
        return "—"
    return f"{value * 100:.1f}%"


def _render_html(
    *,
    runs: list[dict[str, Any]],
    suites: list[dict[str, Any]],
    regressions: list[dict[str, Any]],
    repo: str,
    warnings: list[str],
    report_end: datetime | None = None,
) -> str:
    """
    Render the dashboard HTML with its data embedded as JSON.

    Every string in the payload reaches the page through JSON.parse of a
    JSON-encoded literal, and `<` is escaped so that a branch name or commit
    subject can never close the script tag or inject markup.

    Args:
        runs: Workflow runs, sorted oldest first
        suites: Per-suite summaries
        regressions: Detected regressions
        repo: GitHub repository ("owner/repo") for commit links

    Returns:
        A self-contained HTML document
    """
    report_end = report_end or datetime.now(timezone.utc)
    points = [point for suite in suites for point in suite["points"]]
    visible_run_ids = {point.get("run_id") for point in points}
    # API-wide failures remain visible; record-specific warnings follow the
    # same seven-day window as the plotted records, not the fetch lookback.
    display_warnings = [warning for warning in warnings
                        if ": missing or invalid metrics:" not in warning
                        or warning.split(": missing or invalid metrics:", 1)[0] in visible_run_ids]
    payload = {
        "generatedAt": report_end.isoformat(),
        "windowStart": (report_end - timedelta(days=7)).isoformat(),
        "repo": repo,
        "runs": runs,
        "suites": suites,
        "regressions": regressions,
        "warnings": display_warnings,
        "metricCharts": sorted(METRIC_CHARTS, key=lambda spec:
                               ("latency", "timings", "calls", "reliability", "tokens", "cost").index(spec["id"])),
        "dataCompleteness": {
            "total": len(points),
            "measured": sum(bool(point.get("metrics_complete", all(point.get(field) is not None
                              for field in ("tasks", "eligible_tasks", "passed")))) for point in points),
        },
    }
    # json.dumps twice: the inner call is the data, the outer makes it a JS string
    # literal that JSON.parse reads back, so no value is ever parsed as code.
    embedded = json.dumps(json.dumps(payload, ensure_ascii=True)).replace("<", "\\u003c")

    return _TEMPLATE_PATH.read_text(encoding="utf-8").replace("__DASHBOARD_DATA__", embedded)


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--history-json", type=Path, help="Reuse history from this JSON file")
    parser.add_argument("--output", type=Path, required=True, help="Output HTML file")
    parser.add_argument("--data-output", type=Path, help="Also write the dashboard data as JSON")
    parser.add_argument("--lookback-days", type=int, default=14, help="Days of Langfuse history to fetch")
    parser.add_argument("--weekly-output", type=Path, help="Also write the rolling weekly report as Markdown")
    parser.add_argument("--branch-filter", help="Only include runs from this branch")
    parser.add_argument("--repo", default="", help="owner/repo for commit links; auto-detected when omitted")
    parser.add_argument(
        "--runs-dir", type=Path,
        help="Directory holding the in-progress run's case directories, so the "
             "current run appears in its own dashboard",
    )
    parser.add_argument(
        "--run-id-prefix",
        help="Case-directory prefix of the in-progress run, for example ci-123-1",
    )
    args = parser.parse_args(argv)

    if args.lookback_days <= 0 or (args.weekly_output and args.lookback_days < 14):
        parser.error("lookback-days must be positive, and at least 14 for a weekly report")
    if args.runs_dir and not args.run_id_prefix:
        parser.error("--run-id-prefix is required with --runs-dir")
    repo = args.repo or os.environ.get("GITHUB_REPOSITORY", "")
    warnings = []
    report_end = datetime.now(timezone.utc)

    if args.history_json:
        payload = json.loads(args.history_json.read_text(encoding="utf-8"))
        history = payload["history"]
        warnings = payload.get("warnings", [])
        report_end = parse_timestamp(payload.get("generated_at")) or report_end
    else:
        result = fetch_benchmark_history(
            lookback_days=args.lookback_days,
            branch_filter=args.branch_filter,
            local_runs_dir=args.runs_dir,
            local_run_id_prefix=args.run_id_prefix,
            now=report_end,
            include_failure_details=True,
        )
        history, warnings = result.records, result.warnings
    for warning in warnings:
        print(f"Warning: {warning}")

    generate_dashboard(
        history=history,
        output_file=args.output,
        data_output=args.data_output,
        repo=repo,
        warnings=warnings,
        weekly_output=args.weekly_output,
        report_end=report_end,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(cli())
