"""Generate a benchmark dashboard and weekly report from Langfuse history."""

from __future__ import annotations

import argparse
import json
import os
from datetime import datetime, timedelta, timezone
from pathlib import Path
from typing import Any

from ci.fetch_langfuse_history import COMPARISON_FIELDS, _timestamp, comparison_key, fetch_benchmark_history
from ci.weekly_report import summarize_weekly, render_weekly
from ci.dashboard_metrics import METRIC_CHARTS


# A suite whose pass@1 drops by at least this much against its own previous run
# is called out as a regression. Benchmarks are noisy, so a threshold below this
# would flag ordinary run-to-run variance.
REGRESSION_THRESHOLD = 0.05


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
                       if (stamp := _timestamp(record.get("timestamp"))) is not None
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
        previous_run = points[-2] if len(points) > 1 else None
        metric_deltas = {metric: _delta(latest.get(metric), previous_run.get(metric) if previous_run else None)
                         for metric in ("pass_at_1", "pass_at_k", "pass_pow_k")}
        changes = [value for value in metric_deltas.values() if value is not None]
        increases = any(value > 0 for value in changes)
        decreases = any(value < 0 for value in changes)
        direction = ("mixed" if increases and decreases else "improved" if increases
                     else "declined" if decreases else "unchanged" if changes else "unavailable")
        comparison_status = (
            "No baseline" if previous_run is None else
            "Missing scores" if not changes else
            "Configuration unverified" if latest["comparison_key"] is None or previous_run["comparison_key"] is None else
            "Configuration changed" if latest["comparison_key"] != previous_run["comparison_key"] else
            "Comparable"
        )
        summaries.append({
            "suite": name,
            "latest": latest,
            "previous": previous,
            "previous_run": previous_run,
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
        "metricCharts": METRIC_CHARTS,
        "dataCompleteness": {
            "total": len(points),
            "measured": sum(bool(point.get("metrics_complete", all(point.get(field) is not None
                              for field in ("tasks", "eligible_tasks", "passed")))) for point in points),
        },
    }
    # json.dumps twice: the inner call is the data, the outer makes it a JS string
    # literal that JSON.parse reads back, so no value is ever parsed as code.
    embedded = json.dumps(json.dumps(payload, ensure_ascii=True)).replace("<", "\\u003c")

    return f"""<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="UTF-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>Benchmark Dashboard</title>
<script src="https://cdn.jsdelivr.net/npm/chart.js@4.4.0/dist/chart.umd.min.js"></script>
<style>
  :root {{
    --bg: #f5f5f7; --panel: #ffffff; --text: #1d1d1f; --muted: #6e6e73;
    --line: #e5e5ea; --accent: #0071e3; --bad: #d70015; --good: #1d8102;
  }}
  @media (prefers-color-scheme: dark) {{
    :root:not([data-theme="light"]) {{
      --bg: #161618; --panel: #1f1f22; --text: #f5f5f7; --muted: #9b9ba1;
      --line: #32323a; --accent: #4da3ff; --bad: #ff6961; --good: #5dd868;
    }}
  }}
  * {{ box-sizing: border-box; }}
  body {{
    margin: 0; padding-block: 32px; padding-inline: 20px;
    background: var(--bg); color: var(--text);
    font: 15px/1.5 -apple-system, BlinkMacSystemFont, "Segoe UI", Roboto, sans-serif;
  }}
  .wrap {{ max-width: 1100px; margin: 0 auto; }}
  h1 {{ font-size: 24px; margin: 0 0 4px; }}
  .sub {{ color: var(--muted); font-size: 13px; margin-bottom: 24px; }}
  .panel {{
    background: var(--panel); border: 1px solid var(--line);
    border-radius: 12px; padding: 20px; margin-bottom: 16px;
  }}
  h2 {{ font-size: 15px; margin: 0 0 14px; font-weight: 600; }}
  .cards {{ display: grid; grid-template-columns: repeat(auto-fit, minmax(160px, 1fr)); gap: 16px; }}
  .num {{ font-size: 30px; font-weight: 600; font-variant-numeric: tabular-nums; }}
  .cap {{ color: var(--muted); font-size: 12px; text-transform: uppercase; letter-spacing: .4px; }}
  .bad {{ color: var(--bad); }}
  .good {{ color: var(--good); }}
  table {{ width: 100%; border-collapse: collapse; font-size: 13px; }}
  th {{
    text-align: left; color: var(--muted); font-size: 11px; font-weight: 600;
    text-transform: uppercase; letter-spacing: .4px;
    padding: 0 10px 8px 0; border-bottom: 1px solid var(--line);
  }}
  td {{ padding: 9px 10px 9px 0; border-bottom: 1px solid var(--line); }}
  tr:last-child td {{ border-bottom: 0; }}
  th.n, td.n {{ text-align: right; font-variant-numeric: tabular-nums; white-space: nowrap; }}
  code {{
    font: 12px/1 ui-monospace, SFMono-Regular, Menlo, monospace;
    background: var(--bg); padding: 3px 6px; border-radius: 5px;
  }}
  a {{ color: var(--accent); text-decoration: none; }}
  a:hover {{ text-decoration: underline; }}
  .scroll {{ overflow-x: auto; }}
  .alert {{ border-left: 3px solid var(--bad); padding: 10px 0 10px 14px; margin-bottom: 10px; }}
  .alert:last-child {{ margin-bottom: 0; }}
  .alert b {{ font-weight: 600; }}
  .empty {{ color: var(--muted); font-size: 13px; }}
  canvas {{ max-width: 100%; }}
  .chart {{ position: relative; height: 300px; }}
  .toolbar {{ display: flex; flex-wrap: wrap; align-items: center; gap: 12px; margin-bottom: 16px; }}
  select {{ max-width: 100%; padding: 9px 12px; background: var(--panel); color: var(--text); border: 1px solid var(--line); border-radius: 6px; font: inherit; }}
  button.dataset {{ background: none; border: 0; padding: 0; color: var(--accent); font: inherit; text-align: left; cursor: pointer; }}
  button.dataset:hover {{ text-decoration: underline; }}
  button.dataset[aria-pressed="true"] {{ font-weight: 700; }}
  select:focus-visible, button:focus-visible {{ outline: 2px solid var(--accent); outline-offset: 3px; }}
  [hidden] {{ display: none !important; }}
  .tabs {{ display: flex; gap: 6px; padding: 5px; margin-bottom: 20px; width: fit-content; max-width: 100%; border: 1px solid var(--line); border-radius: 10px; background: var(--panel); }}
  .tabs button {{ border: 0; border-radius: 6px; padding: 10px 18px; font: inherit; font-weight: 600; cursor: pointer; background: transparent; color: var(--muted); }}
  .tabs button[aria-selected="true"] {{ background: var(--accent); color: white; }}
  .tabs button:hover:not([aria-selected="true"]) {{ background: var(--bg); color: var(--text); }}
  .cards {{ margin: 16px 0; }}
  .improved {{ border-left: 4px solid var(--good); }}
  .declined {{ border-left: 4px solid var(--bad); }}
  .mixed {{ border-left: 4px solid #b7791f; }}
  .delta {{ display: block; font-size: 11px; white-space: nowrap; }}
  .metric-grid {{ display: grid; grid-template-columns: repeat(2, minmax(0, 1fr)); gap: 16px; margin-bottom: 16px; }}
  .metric-grid .panel {{ margin-bottom: 0; min-width: 0; }}
  .metric-grid .chart {{ height: 260px; }}
  .metric-summary {{ margin-top: 12px; }}
  .metric-summary span {{ display: block; margin-top: 4px; }}
  @media (max-width: 850px) {{ .metric-grid {{ grid-template-columns: 1fr; }} }}
  summary {{ cursor: pointer; margin-top: 12px; }}
  .warnings {{ max-height: 320px; overflow-y: auto; margin-top: 12px; overflow-wrap: anywhere; }}
</style>
</head>
<body>
<div class="wrap">
  <h1>Benchmark Dashboard</h1>
  <div class="sub" id="sub"></div>
  <div class="tabs" id="dashboard-tabs" role="tablist" aria-label="Dashboard view">
    <button id="tab-latest" role="tab" type="button" aria-selected="true" aria-controls="view-latest" tabindex="0">Latest results</button>
    <button id="tab-trends" role="tab" type="button" aria-selected="false" aria-controls="view-trends" tabindex="-1">Weekly trends</button>
  </div>
  <section id="view-latest" role="tabpanel" aria-labelledby="tab-latest">
  <div class="panel"><h2>Dataset overview · latest results</h2><div id="change-summary" class="sub"></div><p class="empty">Latest versus previous run within the last 7 days. Changed datasets appear first. Click a dataset to view its history.</p><p id="comparison-note" class="empty"></p><div class="scroll"><table id="suites"></table></div></div>
  <div class="panel"><h2>Latest pass@1 by dataset</h2><p class="empty">Latest measured result within the last 7 days. Datasets use different tasks, so these scores are descriptive. Click a bar to view its history.</p><div class="chart" id="ranking-wrap"><canvas id="ranking" role="img" aria-label="Latest dataset pass at one scores"></canvas></div><p id="ranking-note" class="empty"></p></div>
  </section>
  <section id="view-trends" role="tabpanel" aria-labelledby="tab-trends" hidden>
  <div class="panel" id="dataset-panel">
    <h2>Dataset trends</h2>
    <div class="toolbar"><label for="dataset-select">Dataset</label><select id="dataset-select" aria-label="Dataset"></select></div>
    <div class="empty" id="dataset-context"></div>
    <div class="cards" id="latest"></div>
    <details><summary>Run details</summary><p id="comparison-details" class="empty"></p><div class="scroll"><table id="run-details"></table></div></details>
    <p class="empty">Each point is one run of the selected dataset. Click a legend label to show or hide a metric. pass@k: at least one of k attempts passes; pass^k: all k attempts pass.</p>
    <div class="chart"><canvas id="trend" aria-label="Selected dataset pass rates over time" role="img"></canvas></div>
    <p class="empty">Missing scores leave gaps. Hover for commit, model, k and task counts.</p>
  </div>
  <div id="metric-charts" class="metric-grid"></div><p id="measurement-note" class="empty"></p>
  <div class="panel"><h2>Latest failure stages</h2><p class="empty">Eligible failed attempts across all repeats, including unknown stages. These are evidence labels, not proven root causes; they cannot identify grounding, planner or keyboard failure percentages without supporting observations.</p>
    <div class="chart" id="failure-chart-wrap"><canvas id="failure-stages" role="img" aria-label="Latest eligible failure stages"></canvas></div>
    <p id="failure-summary" class="empty"></p>
  </div>
  <details class="panel"><summary id="history-title">Dataset run history</summary><div class="scroll"><table id="history"></table></div></details>
  </section>
  <div class="panel"><h2>Data availability</h2><div id="availability"></div></div>
</div>
<script>
const DATA = JSON.parse({embedded});
const esc = value => String(value ?? "\\u2014").replace(/[&<>"']/g, c => ({{
  "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;"
}}[c]));
const count = value => value === null || value === undefined ? "\\u2014" : value;
const pct = v => v === null || v === undefined ? "\\u2014" : (v * 100).toFixed(1) + "%";
const sha = s => (s || "unknown").slice(0, 8);
const commitLink = s => DATA.repo && s && s !== "unknown"
  ? `<a href="https://github.com/${{DATA.repo.split('/').map(encodeURIComponent).join('/')}}/commit/${{encodeURIComponent(s)}}" target="_blank" rel="noopener"><code>${{esc(sha(s))}}</code></a>`
  : `<code>${{esc(sha(s))}}</code>`;
const when = t => t ? new Date(t).toLocaleString() : "\\u2014";
const signed = d => (d > 0 ? "+" : "") + (d * 100).toFixed(1) + " pp";
const el = id => document.getElementById(id);

const runs = DATA.runs;

el("availability").innerHTML = `<div class="empty">Last 7 days: ${{DATA.dataCompleteness.measured}} / ${{DATA.dataCompleteness.total}} dataset runs have complete first-attempt counts. Historical publication failures remain unknown. Per-metric sample coverage is shown in the charts.</div>` +
  (DATA.warnings.length ? `<details><summary>${{DATA.warnings.length}} data availability warnings — expand to inspect</summary><div class="warnings">` +
    DATA.warnings.map(w => `<div class="alert">${{esc(w)}}</div>`).join("") + '</div></details>' : '');

el("sub").textContent = `Last 7 days · ${{when(DATA.windowStart)}} — ${{when(DATA.generatedAt)}} · ${{runs.length}} workflow runs · ${{DATA.suites.length}} datasets`;

const datasets = DATA.suites;
const metrics = [
  {{ key: "pass_at_1", label: "pass@1", color: "#0891b2" }},
  {{ key: "pass_at_k", label: "pass@k", color: "#ea580c" }},
  {{ key: "pass_pow_k", label: "pass^k", color: "#db2777" }},
];
let selected = null;
let chart = null;
let rankingChart = null;
let failureChart = null;
const measurementCharts = [];
const palette = ["#0891b2", "#ea580c", "#db2777", "#7c3aed", "#65a30d"];
const measurementValue = (point, series) => {{
  const dist = point.distributions?.[series.metric];
  return dist?.count === 0 ? null : dist?.[series.statistic] ?? null;
}};
const measurementText = (value, spec) => value == null ? "Unavailable" :
  (value * spec.scale).toLocaleString(undefined, {{ maximumFractionDigits: spec.unit === "USD" ? 4 : spec.unit === "tokens" ? 0 : 2 }}) + " " + spec.unit;
const sampleText = (point, series) => `${{count(point.distributions?.[series.metric]?.count)}} / ${{count(point.eligible_attempts)}} eligible attempts measured`;
el("metric-charts").innerHTML = DATA.metricCharts.map(spec => `<div class="panel" id="measurement-panel-${{spec.id}}">
  <h2 id="measurement-title-${{spec.id}}">${{esc(spec.title)}}</h2><p class="empty">${{esc(spec.description)}}</p>
  <div class="chart" id="measurement-wrap-${{spec.id}}"><canvas id="measurement-${{spec.id}}" role="img" aria-label="${{esc(spec.title)}}"></canvas></div>
  <p id="measurement-empty-${{spec.id}}" class="empty" hidden>No measurements available in the last 7 days. Missing data is not zero.</p>
  <div id="measurement-summary-${{spec.id}}" class="empty metric-summary"></div>
</div>`).join("");
const deltaClass = d => d == null || d === 0 ? "" : d > 0 ? "good" : "bad";
const deltaLabel = d => d == null ? "No comparison" : d === 0 ? "Unchanged" : `${{d > 0 ? "↑" : "↓"}} ${{signed(d)}}`;
const overviewRows = datasets.map((s, index) => ({{ ...s, index }}))
  .sort((a, b) => Math.max(...Object.values(b.metric_deltas).map(v => Math.abs(v || 0))) - Math.max(...Object.values(a.metric_deltas).map(v => Math.abs(v || 0))));
el("change-summary").textContent = ["improved", "declined", "mixed", "unchanged", "unavailable"]
  .map(direction => `${{datasets.filter(s => s.change_direction === direction).length}} ${{direction === "unavailable" ? "without comparison" : direction}}`).join(" · ");
const unverified = datasets.filter(s => s.comparison_status === "Configuration unverified").length;
const changedConfig = datasets.filter(s => s.comparison_status === "Configuration changed").length;
el("comparison-note").textContent = unverified || changedConfig
  ? `Observed score changes: ${{unverified}} datasets with unverified configurations; ${{changedConfig}} with changed configurations. Check Run details before treating a change as improvement or regression.`
  : "Changes compare the latest two runs. Missing scores or a previous run leave the change unavailable.";
el("dataset-select").innerHTML = datasets.length
  ? datasets.map((s, i) => `<option value="${{i}}">${{esc(s.suite)}}</option>`).join("")
  : '<option value="">No datasets available</option>';
el("dataset-select").disabled = !datasets.length;

el("suites").innerHTML = `
  <thead><tr><th>Dataset</th><th class="n">pass@1</th><th class="n">pass@k</th><th class="n">pass^k</th>
  <th>Latest run</th></tr></thead><tbody>` +
  (datasets.length ? overviewRows.map(s => {{
    return `<tr class="${{s.change_direction}}"><td><button class="dataset" data-dataset="${{s.index}}" aria-pressed="false">${{esc(s.suite)}}</button></td>
      ${{metrics.map(m => `<td class="n">${{pct(s.latest[m.key])}}<span class="delta ${{deltaClass(s.metric_deltas[m.key])}}">${{deltaLabel(s.metric_deltas[m.key])}}</span></td>`).join("")}}
      <td>${{when(s.latest.timestamp)}}</td></tr>`;
  }}).join("") : '<tr><td colspan="5" class="empty">No datasets.</td></tr>') + "</tbody>";

function selectDataset(index) {{
  selected = datasets[index];
  if (!selected) {{
    el("dataset-context").textContent = "No dataset history is available.";
    el("latest").innerHTML = '<div class="empty">Nothing to show yet.</div>';
    el("history").innerHTML = '<tr><td class="empty">No runs.</td></tr>';
    return;
  }}
  el("dataset-select").value = String(index);
  document.querySelectorAll("button[data-dataset]").forEach(button => {{
    button.setAttribute("aria-pressed", String(button.dataset.dataset === String(index)));
  }});
  const points = selected.points;
  const latest = selected.latest;
  el("dataset-context").textContent = `${{selected.suite}} · ${{points.length}} runs · ${{when(points[0].timestamp)}} — ${{when(latest.timestamp)}}`;
  el("latest").innerHTML = metrics.map(metric => `<div>
    <div class="num">${{pct(latest[metric.key])}}</div><div class="cap">Latest ${{metric.label}}</div>
    <span class="delta ${{deltaClass(selected.metric_deltas[metric.key])}}">${{deltaLabel(selected.metric_deltas[metric.key])}}</span>
  </div>`).join("") + `<div><div class="num" style="font-size:15px;padding-top:10px">${{commitLink(latest.commit_sha)}}</div>
    <div class="cap">Latest commit · k=${{esc(latest.metrics_k)}}</div></div>`;
  el("comparison-details").textContent = `${{selected.comparison_status}}. Comparison requires complete, matching workload and configuration metadata. Missing metadata or reported local Git changes make the configuration unverified; this is not a task failure.`;
  el("run-details").innerHTML = `<thead><tr><th>Run</th><th>When</th><th>Commit</th><th>Branch</th><th>Model</th><th>Platform</th><th class="n">k</th><th>Git changes reported</th></tr></thead><tbody>` +
    [["Latest", latest], ["Previous", selected.previous_run]].filter(([label, run]) => run).map(([label, run]) =>
      `<tr><td>${{label}}</td><td>${{when(run.timestamp)}}</td><td>${{commitLink(run.commit_sha)}}</td><td>${{esc(run.git_branch)}}</td><td>${{esc(run.agent_model)}}</td><td>${{esc(run.target_platform)}}</td><td class="n">${{esc(run.metrics_k)}}</td><td>${{run.git_dirty == null ? "Unknown" : run.git_dirty ? "Yes" : "No"}}</td></tr>`).join("") + "</tbody>";
  el("history-title").textContent = selected.suite + " · run history";
  el("history").innerHTML = `
    <thead><tr><th>When</th><th>Commit</th><th class="n">k</th>
    <th class="n">pass@1</th><th class="n">pass@k</th><th class="n">pass^k</th><th>Run</th></tr></thead><tbody>` +
    points.slice().reverse().map(r => `<tr>
      <td>${{when(r.timestamp)}}</td><td>${{commitLink(r.commit_sha)}}</td><td class="n">${{esc(r.metrics_k)}}</td>
      ${{metrics.map(m => `<td class="n">${{pct(r[m.key])}}</td>`).join("")}}
      <td>${{DATA.repo && r.workflow_run_id
        ? `<a href="https://github.com/${{DATA.repo.split('/').map(encodeURIComponent).join('/')}}/actions/runs/${{encodeURIComponent(r.workflow_run_id)}}" target="_blank" rel="noopener">#${{esc(r.workflow_run_number || r.workflow_run_id)}} / attempt ${{esc(r.workflow_run_attempt)}}</a>`
        : esc(r.run_id)}}</td>
    </tr>`).join("") + "</tbody>";
  if (chart) {{
    chart.data.labels = points.map(p => when(p.timestamp));
    chart.data.datasets.forEach((dataset, i) => {{
      dataset.data = points.map(p => p[metrics[i].key] == null ? null : p[metrics[i].key] * 100);
    }});
    chart.update();
  }}
  let visibleMeasurements = 0;
  for (const spec of DATA.metricCharts) {{
    const values = spec.series.map(series => points.map(point => {{
      const value = measurementValue(point, series);
      return value == null ? null : value * spec.scale;
    }}));
    const missing = values.every(series => series.every(value => value == null));
    if (!missing) visibleMeasurements++;
    el("measurement-empty-" + spec.id).hidden = !missing;
    el("measurement-summary-" + spec.id).innerHTML = "Latest run:" + spec.series.map((series, i) =>
      `<span>${{esc(series.label)}}: <b>${{measurementText(measurementValue(latest, series), spec)}}</b> · ${{sampleText(latest, series)}}${{values[i].every(value => value == null) ? " · No measurements in the last 7 days" : ""}}</span>`).join("");
    const instance = measurementCharts.find(item => item.id === spec.id)?.chart;
    if (instance) {{
      instance.data.labels = points.map(point => when(point.timestamp));
      instance.data.datasets.forEach((dataset, i) => {{
        dataset.data = values[i];
        dataset.label = spec.series[i].label + (values[i].some(value => value != null) ? "" : " (no data)");
      }});
      instance.resize(); instance.update("none");
    }}
  }}
  el("measurement-note").textContent = `${{visibleMeasurements}} / ${{DATA.metricCharts.length}} charts have measurements in this dataset's last 7 days. All metrics remain listed; missing measurements are unavailable, while measured zero values remain zero.`;
  const stages = Object.entries(latest.failure_stages || {{}}).filter(([name, value]) => value > 0).sort((a, b) => b[1] - a[1]);
  const observed = stages.reduce((total, [name, value]) => total + value, 0);
  el("failure-chart-wrap").hidden = !stages.length;
  el("failure-summary").textContent = latest.eligible_failures === 0 ? "No eligible failed attempts in this run." :
    !stages.length ? "Failure-stage measurements are unavailable for this run." :
    `${{observed}} stage counts reported / ${{count(latest.eligible_failures)}} eligible failures. Known-stage coverage: ${{pct(latest.failure_stage_coverage)}}. Unknown stages remain unclassified; counts are not a root-cause diagnosis.`;
  if (failureChart) {{
    failureChart.data.labels = stages.map(([name]) => name.replaceAll("_", " "));
    failureChart.data.datasets[0].data = stages.map(([name, value]) => value);
    failureChart.data.datasets[0].backgroundColor = stages.map(([name]) => name === "unknown" ? "#9ca3af" : "#db2777");
    failureChart.resize(); failureChart.update("none");
  }}
}}

if (datasets.length && typeof Chart !== "undefined") {{
  const ink = getComputedStyle(document.documentElement).getPropertyValue("--muted").trim();
  const line = getComputedStyle(document.documentElement).getPropertyValue("--line").trim();
  for (const spec of DATA.metricCharts) {{
    measurementCharts.push({{ id: spec.id, chart: new Chart(el("measurement-" + spec.id), {{
      type: "line",
      data: {{ labels: [], datasets: spec.series.map((series, i) => ({{
        label: series.label, data: [], borderColor: palette[i % palette.length], backgroundColor: palette[i % palette.length],
        borderWidth: 2, pointRadius: 2, pointHoverRadius: 5, tension: 0.15, spanGaps: false,
      }})) }},
      options: {{ responsive: true, maintainAspectRatio: false, interaction: {{ mode: "index", intersect: false }},
        plugins: {{ legend: {{ labels: {{ color: ink, usePointStyle: true }} }}, tooltip: {{ callbacks: {{
          title: items => {{ const point = selected.points[items[0].dataIndex]; return [selected.suite, `${{sha(point.commit_sha)}} · ${{when(point.timestamp)}}`]; }},
          label: item => {{ const point = selected.points[item.dataIndex], series = spec.series[item.datasetIndex];
            return `${{series.label}}: ${{measurementText(measurementValue(point, series), spec)}} · ${{sampleText(point, series)}}`; }},
        }} }} }},
        scales: {{ y: {{ beginAtZero: true, title: {{ display: true, text: spec.unit, color: ink }}, ticks: {{ color: ink }}, grid: {{ color: line }} }},
          x: {{ ticks: {{ color: ink, maxRotation: 0, maxTicksLimit: 4 }}, grid: {{ display: false }} }} }},
      }},
    }}) }});
  }}
  failureChart = new Chart(el("failure-stages"), {{
    type: "bar", data: {{ labels: [], datasets: [{{ label: "Eligible failed attempts", data: [], borderRadius: 4 }}] }},
    options: {{ indexAxis: "y", responsive: true, maintainAspectRatio: false,
      plugins: {{ legend: {{ display: false }}, tooltip: {{ callbacks: {{
        label: item => `${{item.raw}} attempts${{selected.latest.eligible_failures > 0 ? ` · ${{pct(item.raw / selected.latest.eligible_failures)}} of eligible failures` : ""}}`,
      }} }} }},
      scales: {{ x: {{ beginAtZero: true, ticks: {{ color: ink, precision: 0 }}, grid: {{ color: line }} }},
        y: {{ ticks: {{ color: ink }}, grid: {{ display: false }} }} }},
    }},
  }});
  chart = new Chart(el("trend"), {{
    type: "line",
    data: {{
      labels: [],
      datasets: metrics.map(metric => ({{
        label: metric.label, data: [], borderColor: metric.color,
        backgroundColor: metric.color, borderWidth: 2,
        pointRadius: 3, pointHoverRadius: 5, tension: 0.2, spanGaps: false,
      }})),
    }},
    options: {{
      responsive: true, maintainAspectRatio: false,
      interaction: {{ mode: "index", intersect: false }},
      plugins: {{
        legend: {{ display: true, labels: {{ color: ink, usePointStyle: true }} }},
        tooltip: {{ callbacks: {{
          title: items => {{
            const r = selected.points[items[0].dataIndex];
            return [selected.suite, `${{sha(r.commit_sha)}} · ${{when(r.timestamp)}}`,
              `Branch: ${{r.git_branch || "unknown"}} · Model: ${{r.agent_model || "unknown"}} · k=${{count(r.metrics_k)}}`];
          }},
          label: item => {{
            const r = selected.points[item.dataIndex];
            const metric = metrics[item.datasetIndex];
            const first = metric.key === "pass_at_1";
            const successes = first ? r.passed : r[metric.key + "_successes"];
            const eligible = first ? r.eligible_tasks : r[metric.key + "_eligible_tasks"];
            return ` ${{metric.label}} ${{pct(r[metric.key])}} (${{count(successes)}}/${{count(eligible)}} eligible tasks)`;
          }},
        }} }},
      }},
      scales: {{
        y: {{ min: 0, max: 100, ticks: {{ color: ink, callback: v => v + "%" }}, grid: {{ color: line }} }},
        x: {{ ticks: {{ color: ink, maxRotation: 0, autoSkipPadding: 16 }}, grid: {{ display: false }} }},
      }},
    }},
  }});
}}
const ranked = datasets.map((s, index) => ({{ ...s, index }})).filter(s => s.latest.pass_at_1 != null)
  .sort((a, b) => b.latest.pass_at_1 - a.latest.pass_at_1);
el("ranking-note").textContent = `${{ranked.length}} datasets with scores · ${{datasets.length - ranked.length}} without a latest score (not plotted).`;
el("ranking-wrap").style.height = Math.max(180, ranked.length * 30 + 40) + "px";
if (ranked.length && typeof Chart !== "undefined") {{
  const ink = getComputedStyle(document.documentElement).getPropertyValue("--muted").trim();
  rankingChart = new Chart(el("ranking"), {{
    type: "bar",
    data: {{ labels: ranked.map(s => s.suite), datasets: [{{
      label: "Latest pass@1", data: ranked.map(s => s.latest.pass_at_1 * 100),
      backgroundColor: ranked.map(s => s.metric_deltas.pass_at_1 > 0 ? "#1d8102" : s.metric_deltas.pass_at_1 < 0 ? "#d70015" : "#0891b2"),
      borderRadius: 4,
    }}] }},
    options: {{ indexAxis: "y", responsive: true, maintainAspectRatio: false,
      onClick: (event, items) => {{ if (items.length) openDataset(ranked[items[0].index].index); }},
      plugins: {{ legend: {{ display: false }}, tooltip: {{ callbacks: {{
        label: item => {{ const s = ranked[item.dataIndex]; return [`pass@1 ${{pct(s.latest.pass_at_1)}} · ${{count(s.latest.passed)}}/${{count(s.latest.eligible_tasks)}} eligible`, deltaLabel(s.metric_deltas.pass_at_1), s.comparison_status]; }},
      }} }} }},
      scales: {{ x: {{ min: 0, max: 100, ticks: {{ color: ink, callback: v => v + "%" }} }},
        y: {{ ticks: {{ color: ink }}, grid: {{ display: false }} }} }},
    }},
  }});
}}
const views = ["latest", "trends"];
function showView(view) {{
  if (!views.includes(view)) return;
  for (const name of views) {{
    const active = name === view;
    el("view-" + name).hidden = !active;
    el("tab-" + name).setAttribute("aria-selected", String(active));
    el("tab-" + name).tabIndex = active ? 0 : -1;
  }}
  // Charts created in a hidden panel need their visible container dimensions.
  for (const instance of view === "latest" ? [rankingChart] : [chart, failureChart, ...measurementCharts.map(item => item.chart)]) {{
    if (instance) {{ instance.resize(); instance.update("none"); }}
  }}
}}
for (const [index, view] of views.entries()) {{
  el("tab-" + view).addEventListener("click", () => showView(view));
  el("tab-" + view).addEventListener("keydown", event => {{
    const targets = {{ ArrowRight: (index + 1) % views.length, ArrowLeft: (index + views.length - 1) % views.length, Home: 0, End: views.length - 1 }};
    if (!(event.key in targets)) return;
    event.preventDefault();
    const target = views[targets[event.key]];
    showView(target);
    el("tab-" + target).focus();
  }});
}}
function openDataset(index) {{
  selectDataset(index);
  showView("trends");
  el("dataset-select").focus({{ preventScroll: true }});
  el("dashboard-tabs").scrollIntoView({{ behavior: "smooth", block: "start" }});
}}
el("dataset-select").addEventListener("change", event => selectDataset(Number(event.target.value)));
el("suites").addEventListener("click", event => {{
  const button = event.target.closest("button[data-dataset]");
  if (button) openDataset(Number(button.dataset.dataset));
}});
// Start with the most recently run dataset, not a pooled workflow rate.
const initial = datasets.reduce((best, dataset, i) =>
  dataset.latest.timestamp > datasets[best].latest.timestamp ? i : best, 0);
selectDataset(initial);

</script>
</body>
</html>
"""


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--history-json", type=Path, help="Reuse history from this JSON file")
    parser.add_argument("--output", type=Path, required=True, help="Output HTML file")
    parser.add_argument("--data-output", type=Path, help="Also write the dashboard data as JSON")
    parser.add_argument("--lookback-days", type=int, default=30, help="Days of Langfuse history to fetch")
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
        report_end = _timestamp(payload.get("generated_at")) or report_end
    else:
        result = fetch_benchmark_history(
            lookback_days=args.lookback_days,
            branch_filter=args.branch_filter,
            local_runs_dir=args.runs_dir,
            local_run_id_prefix=args.run_id_prefix,
            now=report_end,
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
