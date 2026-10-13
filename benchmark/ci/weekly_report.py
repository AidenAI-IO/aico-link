"""Summarize rolling seven-day benchmark windows using comparable configurations."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
import hashlib
from typing import Any

from ci.fetch_langfuse_history import _timestamp, comparison_key


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
        **{key: sum(r[key] for r in records) if records and all(r.get(key) is not None for r in records) else None
           for key in ("cost_usd_sum", "total_tokens_sum")},
    }


def summarize_weekly(history: list[dict[str, Any]], *, end: datetime | None = None) -> dict[str, Any]:
    """Compare the last seven days with the preceding seven days in UTC.

    The overall rates are descriptive; changes are calculated only within a
    suite/configuration present in both windows, avoiding changing suite mixes.
    """
    end = end or datetime.now(timezone.utc)
    start = end - timedelta(days=7)
    previous_start = start - timedelta(days=7)
    current = [r for r in history if start <= _timestamp(r["timestamp"]) < end]
    previous = [r for r in history if previous_start <= _timestamp(r["timestamp"]) < start]
    groups: dict[tuple[Any, ...], dict[str, Any]] = {}
    uncomparable = 0
    for window, records in (("current", current), ("previous", previous)):
        for record in records:
            key = comparison_key(record)
            if key is None:
                if window == "current":
                    uncomparable += 1
                continue
            group_key = (record["suite"], *key)
            group = groups.setdefault(group_key, {
                "suite": record["suite"], "agent_model": record.get("agent_model"),
                "git_branch": record.get("git_branch"),
                "configuration": hashlib.sha256(repr(group_key).encode()).hexdigest()[:10],
                "current": [], "previous": [],
            })
            group[window].append(record)
    suites = []
    for group in groups.values():
        if not group["current"]:
            continue
        current_totals = _totals(group["current"])
        previous_totals = _totals(group["previous"])
        before, after = previous_totals["pass_at_1"], current_totals["pass_at_1"]
        complete = (current_totals["measured_suite_runs"] == current_totals["suite_runs"]
                    and previous_totals["measured_suite_runs"] == previous_totals["suite_runs"])
        suites.append({**group, "current": current_totals, "previous": previous_totals,
                       "delta": after - before if complete and before is not None and after is not None else None})
    suites.sort(key=lambda s: (s["delta"] is None, s["delta"] or 0, s["suite"], s["configuration"]))
    return {
        "start": start.isoformat(), "end": end.isoformat(),
        "previous_start": previous_start.isoformat(),
        "current": _totals(current), "previous": _totals(previous),
        "uncomparable_suite_runs": uncomparable,
        "suites": suites,
    }


def _pct(value: float | None) -> str:
    return "—" if value is None else f"{value * 100:.1f}%"


def _cell(value: Any) -> str:
    # Report values are data, including in the Actions Markdown renderer.
    import html
    return html.escape(str(value)).replace("|", "&#124;").replace("\n", " ").replace("\r", " ")


def render_weekly(report: dict[str, Any], warnings: list[str]) -> str:
    lines = [
        "## Benchmark weekly report", "",
        f"UTC window: {report['start']} to {report['end']} (end exclusive).", "",
        "History source: Langfuse v4 experiments and scores; current-run local metrics when supplied.",
        "Only successfully published experiments are available as historical data; missing uploads cannot be counted.", "",
        "| Window | Suite runs | Measured runs | Measured eligible / total tasks | pass@1 | Cost (USD) | Tokens |",
        "| --- | ---: | ---: | ---: | ---: | ---: | ---: |",
    ]
    for label, totals in (("Last 7 days", report["current"]), ("Previous 7 days", report["previous"])):
        cost = "—" if totals["cost_usd_sum"] is None else f"{totals['cost_usd_sum']:.4f}"
        tokens = "—" if totals["total_tokens_sum"] is None else f"{totals['total_tokens_sum']:.0f}"
        lines.append(f"| {label} | {totals['suite_runs']} | {totals['measured_suite_runs']} | "
                     f"{totals['eligible_tasks']:.0f} / {totals['tasks']:.0f} | {_pct(totals['pass_at_1'])} | {cost} | {tokens} |")
    lines.extend(["", "Overall rates describe each window's task mix. Changes below require matching configurations.", "",
                  "| Suite | Branch | Model | Configuration | Last 7 days | Previous 7 days | Change |",
                  "| --- | --- | --- | --- | ---: | ---: | ---: |"])
    for suite in report["suites"]:
        delta = "—" if suite["delta"] is None else f"{suite['delta'] * 100:+.1f} pp"
        lines.append("| " + " | ".join([
            _cell(suite["suite"]), _cell(suite["git_branch"]), _cell(suite["agent_model"]),
            suite["configuration"], _pct(suite["current"]["pass_at_1"]),
            _pct(suite["previous"]["pass_at_1"]), delta,
        ]) + " |")
    if not report["suites"]:
        lines.append("| No comparable configuration metadata available | | | | — | — | — |")
    lines.extend(["", f"Current suite runs excluded from comparisons due to dirty or missing configuration metadata: {report['uncomparable_suite_runs']}.", ""])
    if warnings:
        lines.extend(["### Data availability", "", *[f"- {_cell(w)}" for w in warnings], ""])
    return "\n".join(lines)
