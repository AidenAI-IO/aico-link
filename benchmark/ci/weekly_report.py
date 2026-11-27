"""Summarize the last seven days of benchmark runs against the seven before."""

from __future__ import annotations

from datetime import datetime, timedelta, timezone
from typing import Any

from ci.fetch_langfuse_history import COMPARISON_FIELDS, parse_timestamp


def _is_count(value: Any) -> bool:
    return (isinstance(value, (int, float)) and not isinstance(value, bool)
            and value >= 0 and float(value).is_integer())


def _has_result_counts(record: dict[str, Any]) -> bool:
    # An evaluation or fixture error makes the run partial. The publisher still
    # counts its usable first attempts, and the reader validates those counts.
    if set(record.get("missing_metrics") or ()) - {"evaluation_coverage"}:
        return False
    passed, eligible, tasks = (record.get(key) for key in ("passed", "eligible_tasks", "tasks"))
    return all(_is_count(value) for value in (passed, eligible, tasks)) and passed <= eligible <= tasks


def _failure_counts(records: list[dict[str, Any]]) -> dict[str, float | None]:
    kinds = ("evaluation", "environment", "skipped")
    if not records or not all(_has_result_counts(r) for r in records):
        return dict.fromkeys(kinds)
    counts = {}
    for kind in kinds:
        # Older publishers omitted zero-count classes.
        values = [(r.get("failure_classes") or {}).get(kind, 0) for r in records]
        counts[kind] = sum(values) if all(_is_count(value) for value in values) else None
    return counts


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
    measured = [r for r in records if _has_result_counts(r)]
    eligible = sum(r["eligible_tasks"] for r in measured)
    passed = sum(r["passed"] for r in measured)
    tasks = sum(r["tasks"] for r in records) if all(_is_count(r.get("tasks")) for r in records) else None
    # Count repeated first attempts across the window. For distinct tasks,
    # use each dataset's latest eligible-task count.
    latest: dict[str, dict[str, Any]] = {}
    for r in measured:
        key = r.get("dataset", r["suite"])
        if key not in latest or parse_timestamp(r["timestamp"]) > parse_timestamp(latest[key]["timestamp"]):
            latest[key] = r
    return {
        "suite_runs": len(records),
        "measured_suite_runs": len(measured),
        "tasks": tasks,
        "eligible_tasks": eligible,
        "task_count": sum(r["eligible_tasks"] for r in latest.values()),
        "run_count": len({(r["workflow_run_id"], r.get("workflow_run_attempt")) if r.get("workflow_run_id")
                          else (r.get("run_id") or r["timestamp"], None) for r in records}),
        "passed": passed,
        "pass_at_1": passed / eligible if eligible else None,
        "coverage": eligible / tasks if tasks else None,
        # Missing cost/token measurements must not silently become zero.
        **{f"{metric}_sum": _complete_resource_sum(records, metric)
           for metric in ("cost_usd", "total_tokens")},
    }


def _by_suite(records: list[dict[str, Any]]) -> dict[str, list[dict[str, Any]]]:
    groups: dict[str, list[dict[str, Any]]] = {}
    for record in records:
        groups.setdefault(record["dataset"], []).append(record)
    return groups


def _label_datasets(history: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Name each record's dataset: its suite, plus its environment when the
    same suite also ran in another environment, so those results never merge."""
    environments: dict[str, set[str]] = {}
    for record in history:
        environments.setdefault(record["suite"], set()).add(record.get("execution_environment") or "unknown")
    labelled = []
    for record in history:
        suite = record["suite"]
        environment = record.get("execution_environment") or "unknown"
        split = len(environments[suite]) > 1
        labelled.append({**record, "dataset": f"{suite} [{environment}]" if split else suite})
    return labelled


def _comparison(current: list[dict[str, Any]], previous: list[dict[str, Any]]) -> dict[str, Any]:
    """Descriptive change over the datasets measured in both windows.

    A dataset that is new or retired would otherwise move the total through the
    task mix alone."""
    def measured(records: list[dict[str, Any]]) -> set[str]:
        return {dataset for dataset, group in _by_suite(records).items()
                if _totals(group)["pass_at_1"] is not None}

    common = measured(current) & measured(previous)
    now = _totals([r for r in current if r["dataset"] in common])
    before = _totals([r for r in previous if r["dataset"] in common])
    change = _change(now["pass_at_1"], before["pass_at_1"])
    return {"change": change, "compared_datasets": len(common),
            "compared_pass_at_1": {"current": now["pass_at_1"], "previous": before["pass_at_1"]}}


def _change(current: float | None, previous: float | None) -> float | None:
    return current - previous if current is not None and previous is not None else None


# Settings that explain a score change, in reading order; hashes are shortened.
CONFIG_LABELS = {
    "agent_model": "agent model", "foreground_model": "foreground model",
    "execution_mode": "execution mode", "judge_model": "judge model",
    "judge_prompt_version": "judge prompt", "judge_provider": "judge provider",
    "suite_sha256": "suite", "workload_sha256": "workload",
    "metrics_schema_version": "metrics schema", "metrics_k": "k",
    "target_platform": "platform", "skills_fingerprint": "skills", "git_branch": "branch",
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
    """List settings with different values across the two windows."""
    if not current or not previous:
        return []
    def values(records: list[dict[str, Any]], field: str) -> list[str]:
        return sorted({_config_value(record, field) for record in records})

    changes = []
    for field, label in CONFIG_LABELS.items():
        before, now = values(previous, field), values(current, field)
        if before != now:
            changes.append({"field": label, "previous": ", ".join(before), "current": ", ".join(now)})
    return changes


# Primary test objectives, in report order. Tool calls are a mechanism shared
# across areas, while execution environments are separate run metadata.
CAPABILITY_SUITES = {
    "Memory": ("memory_v1", "memory_admission_v1", "notification_memory_v1",
               "personamem_lt_recall_v1", "quick_capture_v1", "episode_memory_before_v1",
               "episode_memory_after_v1", "episode_memory_legacy_v1"),
    "Experience reflection": ("episode_reflection_v1", "episode_reflection_v2"),
    "Visual grounding": ("perception_v1",),
    "UI operation": ("mobilegym_basic", "mobilegym_scroll_regression", "mobilegym_scroll_sweep",
                     "mobilegym_touch_v1",
                     "phone_control_v1", "adb_android_basic", "adb_android_text_editing",
                     "adb_android_builtin_apps", "vphone_ios_basic",
                     "app_workflow_v1", "quick_action_v1"),
    "Device integration": ("aico_app_connection_capabilities_v1",
                           "aico_app_android_bridge_capabilities_v1",
                           "aico_app_phone_bridge_data_policy_v1", "aico_app_notes_entry_policy_v1",
                           "mobilegym_aico_app_v1"),
    "Shell utilities": ("shell_utility_v1",),
    "Realtime collaboration": ("realtime_collaboration_v1",),
}
DIAGNOSTIC_SUITES = ("skill_discovery_v1", "tool_recovery_v1")
DIAGNOSTIC_AREA = "Diagnostics"
OTHER_CAPABILITY = "Other"
_CAPABILITY_BY_SUITE = {suite: group for group, suites in CAPABILITY_SUITES.items() for suite in suites}
AREA_ORDER = (*CAPABILITY_SUITES, DIAGNOSTIC_AREA, OTHER_CAPABILITY)
ENVIRONMENT_LABELS = {
    "scripted": "Scripted", "mobilegym": "Simulator", "isolated": "Isolated",
    "adb": "ADB device", "adb_aidenbridge": "ADB + Aico App",
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

    Rates are descriptive totals over each window's datasets. A change compares
    only datasets measured in both windows, so a new or retired dataset cannot
    move it. Changes are descriptive, not statistical significance claims.
    """
    end = end or datetime.now(timezone.utc)
    start = end - timedelta(days=7)
    previous_start = start - timedelta(days=7)
    history = _label_datasets(history)
    current = [r for r in history if start <= parse_timestamp(r["timestamp"]) < end]
    earlier = [r for r in history if parse_timestamp(r["timestamp"]) < start]
    previous = [r for r in earlier if previous_start <= parse_timestamp(r["timestamp"])]
    return {"start": start.isoformat(), "end": end.isoformat(), "previous_start": previous_start.isoformat(),
            **_summarize(current, previous, earlier)}


def _run_key(record: dict[str, Any]) -> tuple[Any, Any]:
    if record.get("workflow_run_id"):
        return record["workflow_run_id"], record.get("workflow_run_attempt")
    return record.get("run_id") or record["timestamp"], None


def summarize_latest_run(history: list[dict[str, Any]], *, end: datetime | None = None) -> dict[str, Any] | None:
    """Summarize the newest workflow run before `end` against the run before it.

    The result has the weekly summary's shape, so both views read the same way.
    A dataset missing from the previous run has no change.
    """
    end = end or datetime.now(timezone.utc)
    history = _label_datasets([r for r in history if parse_timestamp(r["timestamp"]) < end])
    runs: dict[tuple[Any, Any], list[dict[str, Any]]] = {}
    for record in history:
        runs.setdefault(_run_key(record), []).append(record)
    if not runs:
        return None
    ordered = sorted(runs.values(), key=lambda run: min(parse_timestamp(r["timestamp"]) for r in run))
    current = ordered[-1]
    previous = ordered[-2] if len(ordered) > 1 else []
    first = min(current, key=lambda r: parse_timestamp(r["timestamp"]))
    return {
        "start": first["timestamp"],
        "previous_start": (min(previous, key=lambda r: parse_timestamp(r["timestamp"]))["timestamp"]
                           if previous else None),
        **{field: first.get(field) for field in (
            "workflow_run_id", "workflow_run_number", "workflow_run_attempt", "commit_sha", "git_branch")},
        "agent_models": sorted({(r.get("agent_model") or "").strip() for r in current}),
        **_summarize(current, previous, [r for run in ordered[:-1] for r in run]),
    }


def _summarize(current: list[dict[str, Any]], previous: list[dict[str, Any]],
               earlier: list[dict[str, Any]]) -> dict[str, Any]:
    """Totals, test areas and datasets of `current`, with changes from `previous`.

    `earlier` holds every record before `current`, for first appearances and
    the last usable result of a dataset that has none now.
    """
    previous_by_suite = _by_suite(previous)
    earlier_by_suite = _by_suite(earlier)
    suites = []
    diagnostics = []
    for name, group in _by_suite(current).items():
        totals = _totals(group)
        prior = previous_by_suite.get(name, [])
        if group[0]["suite"] in DIAGNOSTIC_SUITES:
            observation = skill_discovery_totals(group)
            previous_observation = skill_discovery_totals(prior)
            diagnostics.append({"suite": name, **observation,
                                "change": _change(observation["rate"], previous_observation["rate"])})
            continue
        past = earlier_by_suite.get(name, [])
        last_valid = None
        if totals["pass_at_1"] is None:
            last = max((r for r in past if _has_result_counts(r) and r["eligible_tasks"]),
                       key=lambda r: parse_timestamp(r["timestamp"]), default=None)
            if last is not None:
                last_valid = {"timestamp": last["timestamp"], "pass_at_1": last["passed"] / last["eligible_tasks"]}
        suites.append({"suite": name, **totals, "previous_pass_at_1": _totals(prior)["pass_at_1"],
                       "new": not past, "last_valid": last_valid,
                       **_comparison(group, prior),
                       "config_changes": _config_changes(group, prior)})
    # Weakest suites first; unknown rates last.
    suites.sort(key=lambda s: (s["pass_at_1"] is None, s["pass_at_1"] or 0.0, s["suite"]))
    previous_by_capability = _by_capability(previous)
    capabilities = []
    current_by_capability = _by_capability(current)
    order = AREA_ORDER
    for name in sorted(current_by_capability, key=order.index):
        group = current_by_capability[name]
        prior = previous_by_capability.get(name, [])
        capabilities.append({"capability": name, **_totals(group), "previous_pass_at_1": _totals(prior)["pass_at_1"],
                             **_comparison(group, prior),
                             "suites": sorted({r["dataset"] for r in group})})
    scored_current = [r for r in current if r["suite"] not in DIAGNOSTIC_SUITES]
    scored_previous = [r for r in previous if r["suite"] not in DIAGNOSTIC_SUITES]
    totals = {**_totals(scored_current), "failure_counts": _failure_counts(scored_current)}
    previous_totals = _totals(scored_previous)
    return {"totals": totals, "previous_totals": previous_totals,
            **_comparison(scored_current, scored_previous),
            "capabilities": capabilities, "suites": suites, "diagnostics": diagnostics}


def _pct(value: float | None) -> str:
    return "—" if value is None else f"{value * 100:.1f}%"


def _pp(value: float | None) -> str:
    return "—" if value is None else f"{value * 100:+.1f} pp"


def _count(value: float | None) -> str:
    return "—" if value is None else f"{value:.0f}"


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
        "| Suite runs | Runs with usable counts | Eligible / recorded first attempts | Coverage | pass@1 | Matched change | Cost (USD) | Tokens |",
        "| ---: | ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
        f"| {totals['suite_runs']} | {totals['measured_suite_runs']} | {totals['eligible_tasks']:.0f} / {_count(totals['tasks'])} | "
        f"{_pct(totals['coverage'])} | {_pct(totals['pass_at_1'])} | {_pp(report['change'])} | {cost} | {tokens} |",
        "", "Pass@1 uses eligible first attempts, including usable results from partially evaluated runs. "
        "Coverage is eligible first attempts divided by all recorded first attempts.",
        "Matched changes compare only datasets measured in both windows;",
        "changes are descriptive, not statistical significance claims.",
        "Configuration changes compare all recorded values in the two windows.", "",
        "### By test area", "",
        "| Test area | Suites | Tasks | Runs | Eligible first attempts | pass@1 | Previous 7 days | Matched change | Coverage |",
        "| --- | --- | ---: | ---: | ---: | ---: | ---: | ---: | ---: |",
        *[f"| {_cell(c['capability'])} | {_cell(', '.join(c['suites']))} | {c['task_count']:.0f} | "
          f"{c['run_count']} | {c['eligible_tasks']:.0f} | "
          f"{_pct(c['pass_at_1'])} | {_pct(c['previous_pass_at_1'])} | {_pp(c['change'])} | {_pct(c['coverage'])} |"
          for c in report.get("capabilities", [])],
        "", "### By suite", "",
        "| Suite | Runs | Eligible tasks | pass@1 | Previous 7 days | Change | Configuration changes | Cost (USD) | Coverage |",
        "| --- | ---: | ---: | ---: | ---: | ---: | --- | ---: | ---: |",
    ]
    for suite in report["suites"]:
        lines.append(f"| {_cell(suite['suite'])} | {suite['suite_runs']} | {suite['eligible_tasks']:.0f} | "
                     f"{_pct(suite['pass_at_1'])} | {_pct(suite['previous_pass_at_1'])} | {_pp(suite['change'])} | "
                     f"{_cell(describe_config_changes(suite.get('config_changes') or [])) or '—'} | "
                     f"{_resources(suite)[0]} | {_pct(suite['coverage'])} |")
    if not report["suites"]:
        lines.append("| No benchmark runs in this window | | | — | — | — | — | — | — |")
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
