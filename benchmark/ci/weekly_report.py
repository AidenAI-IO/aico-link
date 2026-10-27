"""Summarize the last seven days of benchmark runs against the seven before."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import Any

from ci.fetch_langfuse_history import COMPARISON_FIELDS, parse_timestamp


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
    # eligible_tasks sums every run in the window. task_count is the distinct
    # task count (each suite's latest run) and run_count the workflow runs, so
    # readers can tell many tasks from many repetitions.
    latest: dict[str, dict[str, Any]] = {}
    for r in measured:
        if r["suite"] not in latest or parse_timestamp(r["timestamp"]) > parse_timestamp(latest[r["suite"]]["timestamp"]):
            latest[r["suite"]] = r
    return {
        "suite_runs": len(records),
        "measured_suite_runs": len(measured),
        "tasks": sum(r["tasks"] for r in measured),
        "eligible_tasks": eligible,
        "task_count": sum(r["eligible_tasks"] for r in latest.values()),
        "run_count": len({(r["workflow_run_id"], r.get("workflow_run_attempt")) if r.get("workflow_run_id")
                          else (r.get("run_id") or r["timestamp"], None) for r in measured}),
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
    "judge_prompt_version": "judge prompt", "judge_provider": "judge provider",
    "suite_sha256": "suite", "workload_sha256": "workload",
    "metrics_schema_version": "metrics schema", "metrics_k": "k",
    "target_platform": "platform", "active_skills": "skills", "git_branch": "branch",
    "execution_environment": "environment",
}
assert sorted(CONFIG_LABELS) == sorted(COMPARISON_FIELDS)


def _config_value(record: dict[str, Any], field: str) -> str:
    value = record.get(field)
    if isinstance(value, list):
        value = ", ".join(str(item) for item in value)
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


# Primary test objectives, in report order. Tool calls are a mechanism shared
# across areas, while execution environments are separate run metadata.
CAPABILITY_SUITES = {
    "Memory": ("memory_v1", "episode_memory_v1", "notification_memory_v1",
               "personamem_lt_recall_v1", "quick_capture_v1", "episode_memory_before_v1",
               "episode_memory_after_v1", "episode_memory_legacy_v1"),
    "Experience reflection": ("episode_reflection_v1", "episode_reflection_v2"),
    "Visual grounding": ("perception_v1",),
    "UI operation": ("mobilegym_basic", "mobilegym_scroll_regression", "mobilegym_scroll_sweep",
                     "phone_control_v1", "adb_android_basic", "adb_android_text_editing", "vphone_ios_basic",
                     "app_workflow_v1", "quick_action_v1"),
    "Device integration": ("aico_app_connection_capabilities_v1",
                           "aico_app_android_bridge_capabilities_v1",
                           "aico_app_phone_bridge_data_policy_v1", "aico_app_notes_entry_policy_v1"),
    "Shell utilities": ("shell_utility_v1",),
}
DIAGNOSTIC_SUITES = ("skill_discovery_v1", "tool_recovery_v1")
DIAGNOSTIC_AREA = "Diagnostics"
OTHER_CAPABILITY = "Other"
_CAPABILITY_BY_SUITE = {suite: group for group, suites in CAPABILITY_SUITES.items() for suite in suites}
AREA_ORDER = (*CAPABILITY_SUITES, DIAGNOSTIC_AREA, OTHER_CAPABILITY)
ENVIRONMENT_LABELS = {
    "scripted": "Scripted", "mobilegym": "Simulator", "isolated": "Isolated",
    "adb": "ADB device", "adb_aidenbridge": "ADB + AidenBridge",
    "external": "External device", "mixed": "Mixed",
}


def capability_of(suite: str) -> str:
    if suite in DIAGNOSTIC_SUITES:
        return DIAGNOSTIC_AREA
    return _CAPABILITY_BY_SUITE.get(suite, OTHER_CAPABILITY)


def _by_capability(records: list[dict[str, Any]]) -> dict[str, list[dict[str, Any]]]:
    groups: dict[str, list[dict[str, Any]]] = {}
    for record in records:
        if record["suite"] in DIAGNOSTIC_SUITES:
            continue
        groups.setdefault(capability_of(record["suite"]), []).append(record)
    return groups


def environment_of(record: dict[str, Any]) -> str:
    """Use recorded execution metadata; never infer an environment from a suite name."""
    return ENVIRONMENT_LABELS.get(record.get("execution_environment"), "Unknown")


def skill_discovery_totals(records: list[dict[str, Any]]) -> dict[str, Any]:
    counts = [record.get("skill_discovery") or {} for record in records]
    complete = bool(counts) and all(
        isinstance(c.get("observed"), (int, float)) and isinstance(c.get("read"), (int, float))
        and 0 <= c["read"] <= c["observed"] and float(c["read"]).is_integer()
        and float(c["observed"]).is_integer() for c in counts)
    observed = sum(c["observed"] for c in counts) if complete else None
    read = sum(c["read"] for c in counts) if complete else None
    return {"observed": observed, "read": read, "rate": read / observed if observed else None}


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
    diagnostics = []
    for name, group in _by_suite(current).items():
        totals = _totals(group)
        prior = previous_by_suite.get(name, [])
        before = _totals(prior)["pass_at_1"]
        if name in DIAGNOSTIC_SUITES:
            observation = skill_discovery_totals(group)
            previous_observation = skill_discovery_totals(prior)
            diagnostics.append({"suite": name, **observation,
                                "change": _change(observation["rate"], previous_observation["rate"])})
            continue
        suites.append({"suite": name, **totals, "previous_pass_at_1": before,
                       "change": _change(totals["pass_at_1"], before),
                       "config_changes": _config_changes(group, prior)})
    # Weakest suites first; unknown rates last.
    suites.sort(key=lambda s: (s["pass_at_1"] is None, s["pass_at_1"] or 0.0, s["suite"]))
    previous_by_capability = _by_capability(previous)
    capabilities = []
    current_by_capability = _by_capability(current)
    order = AREA_ORDER
    for name in sorted(current_by_capability, key=order.index):
        group = current_by_capability[name]
        totals = _totals(group)
        before = _totals(previous_by_capability.get(name, []))["pass_at_1"]
        capabilities.append({"capability": name, **totals, "previous_pass_at_1": before,
                             "change": _change(totals["pass_at_1"], before),
                             "suites": sorted({r["suite"] for r in group})})
    totals = _totals([r for r in current if r["suite"] not in DIAGNOSTIC_SUITES])
    previous_totals = _totals([r for r in previous if r["suite"] not in DIAGNOSTIC_SUITES])
    return {"start": start.isoformat(), "end": end.isoformat(), "previous_start": previous_start.isoformat(),
            "totals": totals, "previous_totals": previous_totals,
            "change": _change(totals["pass_at_1"], previous_totals["pass_at_1"]),
            "capabilities": capabilities, "suites": suites, "diagnostics": diagnostics}


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
        "### By test area", "",
        "| Test area | Suites | Tasks | Runs | Eligible first attempts | pass@1 | Previous 7 days | Change |",
        "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: |",
        *[f"| {_cell(c['capability'])} | {_cell(', '.join(c['suites']))} | {c['task_count']:.0f} | "
          f"{c['run_count']} | {c['eligible_tasks']:.0f} | "
          f"{_pct(c['pass_at_1'])} | {_pct(c['previous_pass_at_1'])} | {_pp(c['change'])} |"
          for c in report.get("capabilities", [])],
        "", "### By suite", "",
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
    if report.get("diagnostics"):
        lines.extend(["", "### Diagnostics", "", "Skill discovery is an observation, not UI task pass@1. Excluded from test-area and overall pass rates.", ""])
        for diagnostic in report["diagnostics"]:
            lines.append(f"- {_cell(diagnostic['suite'])}: skill read {_pct(diagnostic['rate'])} ({_pp(diagnostic['change'])}); "
                         + (f"{diagnostic['read']:.0f}/{diagnostic['observed']:.0f} observed attempts."
                            if diagnostic["observed"] is not None else "observation data unavailable."))
    lines.append("")
    if warnings:
        lines.extend(["### Data availability", "", *[f"- {_cell(w)}" for w in warnings], ""])
    return "\n".join(lines)
