"""Summarize the last seven days of benchmark runs."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import Any

from ci.fetch_langfuse_history import parse_timestamp


def _complete_resource_sum(records: list[dict[str, Any]], metric: str) -> float | None:
    # Run sums contain only measured eligible attempts. A present sum alone
    # does not establish full coverage of the window's attempts.
    key = f"{metric}_sum"
    if not records or any(
        r.get(key) is None or r.get("eligible_attempts") is None
        or r.get("distributions", {}).get(metric, {}).get("count") != r["eligible_attempts"]
        for r in records
    ):
        return None
    return sum(r[key] for r in records)


def _totals(records: list[dict[str, Any]]) -> dict[str, Any]:
    measured = [r for r in records if not r.get("missing_metrics")
                and r.get("eligible_tasks") is not None and r.get("passed") is not None]
    eligible = sum(r["eligible_tasks"] for r in measured)
    return {
        "suite_runs": len(records),
        "measured_suite_runs": len(measured),
        "tasks": sum(r["tasks"] for r in measured),
        "eligible_tasks": eligible,
        "passed": sum(r["passed"] for r in measured),
        "pass_at_1": sum(r["passed"] for r in measured) / eligible if eligible else None,
        # Missing cost/token measurements must not silently become zero.
        **{f"{metric}_sum": _complete_resource_sum(records, metric)
           for metric in ("cost_usd", "total_tokens")},
    }


def summarize_weekly(history: list[dict[str, Any]], *, end: datetime | None = None) -> dict[str, Any]:
    """Summarize the last seven days in UTC, overall and per suite.

    Overall and per-suite rates are descriptive totals over each window's
    task mix; they are not comparisons between runs or configurations.
    """
    end = end or datetime.now(timezone.utc)
    start = end - timedelta(days=7)
    records = [r for r in history if start <= parse_timestamp(r["timestamp"]) < end]
    by_suite: dict[str, list[dict[str, Any]]] = {}
    for record in records:
        by_suite.setdefault(record["suite"], []).append(record)
    suites = [{"suite": name, **_totals(group)} for name, group in by_suite.items()]
    # Weakest suites first; unknown rates last.
    suites.sort(key=lambda s: (s["pass_at_1"] is None, s["pass_at_1"] or 0.0, s["suite"]))
    return {"start": start.isoformat(), "end": end.isoformat(), "totals": _totals(records), "suites": suites}


def _pct(value: float | None) -> str:
    return "—" if value is None else f"{value * 100:.1f}%"


def _cell(value: Any) -> str:
    # Report values are data, including in the Actions Markdown renderer.
    import html
    return html.escape(str(value)).replace("|", "&#124;").replace("\n", " ").replace("\r", " ")


def _resources(totals: dict[str, Any]) -> tuple[str, str]:
    cost = "—" if totals["cost_usd_sum"] is None else f"{totals['cost_usd_sum']:.6g}"
    tokens = "—" if totals["total_tokens_sum"] is None else f"{totals['total_tokens_sum']:.0f}"
    return cost, tokens


def render_weekly(report: dict[str, Any], warnings: list[str]) -> str:
    totals = report["totals"]
    cost, tokens = _resources(totals)
    lines = [
        "## Benchmark weekly report", "",
        f"UTC window: {report['start']} to {report['end']} (end exclusive).", "",
        "History source: Langfuse v4 experiments and scores.",
        "Only successfully published experiments are available as historical data; missing uploads cannot be counted.", "",
        "| Suite runs | Measured runs | Measured eligible / total tasks | pass@1 | Cost (USD) | Tokens |",
        "| ---: | ---: | ---: | ---: | ---: | ---: |",
        f"| {totals['suite_runs']} | {totals['measured_suite_runs']} | {totals['eligible_tasks']:.0f} / {totals['tasks']:.0f} | "
        f"{_pct(totals['pass_at_1'])} | {cost} | {tokens} |",
        "", "Rates are totals over each suite's runs in the window, not comparisons between runs.", "",
        "| Suite | Runs | Eligible tasks | pass@1 | Cost (USD) |",
        "| --- | ---: | ---: | ---: | ---: |",
    ]
    for suite in report["suites"]:
        lines.append(f"| {_cell(suite['suite'])} | {suite['suite_runs']} | {suite['eligible_tasks']:.0f} | "
                     f"{_pct(suite['pass_at_1'])} | {_resources(suite)[0]} |")
    if not report["suites"]:
        lines.append("| No benchmark runs in this window | | | — | — |")
    lines.append("")
    if warnings:
        lines.extend(["### Data availability", "", *[f"- {_cell(w)}" for w in warnings], ""])
    return "\n".join(lines)
