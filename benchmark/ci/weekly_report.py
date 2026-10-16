"""Summarize the last seven days of benchmark runs against the seven before."""

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


def _by_suite(records: list[dict[str, Any]]) -> dict[str, list[dict[str, Any]]]:
    groups: dict[str, list[dict[str, Any]]] = {}
    for record in records:
        groups.setdefault(record["suite"], []).append(record)
    return groups


def _change(current: float | None, previous: float | None) -> float | None:
    return current - previous if current is not None and previous is not None else None


# Settings that explain a score change, in reading order; hashes are shortened.
CONFIG_LABELS = {
    "agent_model": "agent model", "judge_model": "judge model",
    "judge_prompt_version": "judge prompt", "suite_sha256": "suite",
    "workload_sha256": "workload", "metrics_k": "k", "target_platform": "platform",
}


def _config_value(record: dict[str, Any], field: str) -> str:
    value = record.get(field)
    text = "—" if value in (None, "") else str(value)
    return text[:10] if field.endswith("_sha256") and text != "—" else text


def _config_changes(current: list[dict[str, Any]], previous: list[dict[str, Any]]) -> list[dict[str, str]]:
    """List settings whose latest value differs between the two windows."""
    if not current or not previous:
        return []
    def latest(records: list[dict[str, Any]]) -> dict[str, Any]:
        return max(records, key=lambda record: parse_timestamp(record["timestamp"]))
    now, before = latest(current), latest(previous)
    return [{"field": label, "previous": _config_value(before, field), "current": _config_value(now, field)}
            for field, label in CONFIG_LABELS.items()
            if _config_value(before, field) != _config_value(now, field)]


def describe_config_changes(changes: list[dict[str, str]]) -> str:
    return "; ".join(f"{c['field']}: {c['previous']} → {c['current']}" for c in changes)


def summarize_weekly(history: list[dict[str, Any]], *, end: datetime | None = None) -> dict[str, Any]:
    """Summarize the last seven days in UTC and their change from the seven before.

    Rates are descriptive totals over each window's task mix. A suite's change
    compares its own two windows; it is unknown when either window has no rate.
    """
    end = end or datetime.now(timezone.utc)
    start = end - timedelta(days=7)
    previous_start = start - timedelta(days=7)
    current = [r for r in history if start <= parse_timestamp(r["timestamp"]) < end]
    previous = [r for r in history if previous_start <= parse_timestamp(r["timestamp"]) < start]
    previous_by_suite = _by_suite(previous)
    suites = []
    for name, group in _by_suite(current).items():
        totals = _totals(group)
        prior = previous_by_suite.get(name, [])
        before = _totals(prior)["pass_at_1"]
        suites.append({"suite": name, **totals, "previous_pass_at_1": before,
                       "change": _change(totals["pass_at_1"], before),
                       "config_changes": _config_changes(group, prior)})
    # Weakest suites first; unknown rates last.
    suites.sort(key=lambda s: (s["pass_at_1"] is None, s["pass_at_1"] or 0.0, s["suite"]))
    totals, previous_totals = _totals(current), _totals(previous)
    return {"start": start.isoformat(), "end": end.isoformat(), "previous_start": previous_start.isoformat(),
            "totals": totals, "previous_totals": previous_totals,
            "change": _change(totals["pass_at_1"], previous_totals["pass_at_1"]), "suites": suites}


def _pct(value: float | None) -> str:
    return "—" if value is None else f"{value * 100:.1f}%"


def _pp(value: float | None) -> str:
    return "—" if value is None else f"{value * 100:+.1f} pp"


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
        "| Suite runs | Measured runs | Measured eligible / total tasks | pass@1 | Change vs previous 7 days | Cost (USD) | Tokens |",
        "| ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
        f"| {totals['suite_runs']} | {totals['measured_suite_runs']} | {totals['eligible_tasks']:.0f} / {totals['tasks']:.0f} | "
        f"{_pct(totals['pass_at_1'])} | {_pp(report['change'])} | {cost} | {tokens} |",
        "", "Rates are totals over each window's runs; changes compare a suite's last 7 days with its previous 7.",
        "Configuration changes list settings that differ between the two windows' latest runs.", "",
        "| Suite | Runs | Eligible tasks | pass@1 | Previous 7 days | Change | Configuration changes | Cost (USD) |",
        "| --- | ---: | ---: | ---: | ---: | ---: | --- | ---: |",
    ]
    for suite in report["suites"]:
        lines.append(f"| {_cell(suite['suite'])} | {suite['suite_runs']} | {suite['eligible_tasks']:.0f} | "
                     f"{_pct(suite['pass_at_1'])} | {_pct(suite['previous_pass_at_1'])} | {_pp(suite['change'])} | "
                     f"{_cell(describe_config_changes(suite.get('config_changes') or [])) or '—'} | "
                     f"{_resources(suite)[0]} |")
    if not report["suites"]:
        lines.append("| No benchmark runs in this window | | | — | — | — | — | — |")
    lines.append("")
    if warnings:
        lines.extend(["### Data availability", "", *[f"- {_cell(w)}" for w in warnings], ""])
    return "\n".join(lines)
