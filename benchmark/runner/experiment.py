"""One frozen experiment, independent agent runs, paired attempt reporting."""
from __future__ import annotations

import collections
import concurrent.futures
import dataclasses as dc
import hashlib
import html
import itertools
import json
import os
from pathlib import Path
import random
import re
import statistics
import subprocess
import tempfile
import threading
import urllib.parse
from typing import TYPE_CHECKING, Any
import uuid

from runner.harness import AidenAdapter
from runner.harnesses import HARNESSES, SAFE_ID, validate_agents
from runner.html_report import generate_report_html
from runner.judge import JudgeConfig, DEFAULT_JUDGE_API_KEY_ENV
from runner.models import HardAssertionFailure, HardAssertionResults, RubricVerdict, TaskResult
from runner.rejudge import execution_gate_blocks_rejudge, rejudge_run
from runner.report import git_sha, now_iso, write_jsonl, write_manifest, write_metrics, write_summary
from runner.runtask import evaluate_task_history, skipped_task_result, _apply_environment_assertions
from runner.suite import Suite, load_suite

if TYPE_CHECKING:
    from runner.operations import OperationHarness


# Suite prompts were written for Aiden, which knows it operates a device. Coding
# harnesses otherwise read "open Settings" as a request about their own host, so
# every agent, Aiden included, receives the same device context.
DEVICE_CONTEXT = ("The task refers to the connected mobile device. Operate that device only through "
                  "the provided device tools; do not act on the computer you are running on.")
# External harnesses receive only business tools, so native calls indicate a
# policy gap; a native call that addresses a local endpoint is flagged for review.
LOOPBACK_REFERENCE = re.compile(r"127\.0\.0\.1|localhost|0\.0\.0\.0|\[::1\]|host\.docker\.internal", re.IGNORECASE)


def tool_usage_metrics(history: list[dict], business_tools: set[str]) -> dict[str, int]:
    """Split one attempt's calls into shared business tools and native harness tools."""
    from runner.trace import extract_trace
    calls = extract_trace(history).tool_calls
    native = [call for call in calls if call.tool not in business_tools]
    return {"business_tool_calls": len(calls) - len(native), "native_tool_calls": len(native),
            "native_loopback_calls": sum(bool(LOOPBACK_REFERENCE.search(json.dumps(call.input, ensure_ascii=False)))
                                         for call in native)}


def comparison_suite(path: Path) -> Suite:
    """Reuse original operation tasks and reject requirements we cannot preserve."""
    from runner.operations import REQUIRED_BUSINESS_TOOLS, preparation_steps
    suite = load_suite(path)
    if not suite.tasks:
        raise ValueError("A comparison suite must contain at least one task")
    if suite.status != "active" or suite.mock_environment:
        raise ValueError("Comparison requires an active suite with a real device environment")
    preparation_steps(suite.global_reset)
    for task in suite.tasks:
        if not SAFE_ID.fullmatch(task.id):
            raise ValueError("Comparison task IDs must be safe directory names")
        if (task.mock_environment or task.memory_assertions is not None
                or task.expected_recalled_memory_ids or task.expected_recall_from_consolidation
                or task.consolidation_expectation or task.input_screenshot
                or task.category == "memory"):
            raise ValueError(f"{task.id}: agent-specific memory, mocks, or initial attachments are unsupported")
        preparation_steps(task.setup)
        required = set(task.hard_assertions.required_tools)
        required.update(call.tool for call in task.hard_assertions.required_tool_calls)
        if required - set(REQUIRED_BUSINESS_TOOLS):
            raise ValueError(f"{task.id}: required tools are not shared business tools: {sorted(required - set(REQUIRED_BUSINESS_TOOLS))}")
    return suite


def atomic_write(path: Path, write) -> None:
    """Publish a generated file only after `write(temporary_path)` completes."""
    temporary = path.with_suffix(".tmp")
    write(temporary)
    temporary.replace(path)


def atomic_json(path: Path, value: Any) -> None:
    atomic_write(path, lambda target: target.write_text(
        json.dumps(value, ensure_ascii=False, indent=2), encoding="utf-8"))


# Why an attempt was not scored, from the most to the least specific signal.
EXCLUSION_REASONS = {
    "platform": "Task requires another platform",
    "preparation": "Shared environment preparation failed",
    "network": "Model endpoint unreachable before the agent acted",
    "not_started": "Agent exited or stalled before acting",
    "harness": "Harness or infrastructure error",
    "evaluation": "Judge or state evaluation unavailable",
    "canceled": "Experiment canceled",
}


def exclusion_reason(row: TaskResult) -> str | None:
    """Classify an ineligible attempt; eligible attempts return None."""
    metrics = row.metrics
    if metrics.get("agent_eligible") is True:
        return None
    failure_class = metrics.get("failure_class")
    if row.status == "canceled" or failure_class == "canceled":
        return "canceled"
    if failure_class == "platform":
        return "platform"
    if failure_class == "adapter":
        return "preparation" if "preparation" in str(metrics.get("adapter_error") or "") else "harness"
    if failure_class == "evaluation" or row.status == "judge_error":
        return "evaluation"
    # Runtime marks attempts that never acted as unknown; retries show the endpoint was unreachable.
    return "network" if metrics.get("api_retries") else "not_started"


def comparison(suite: Suite, agents: list[dict], rows: dict[str, list[TaskResult]], repeats: int) -> dict:
    planned = len(suite.tasks) * repeats
    summaries = []
    by_agent = {}
    for agent in agents:
        values = rows[agent["id"]]
        by_agent[agent["id"]] = {(row.task_id, row.attempt): row for row in values}
        eligible = [row for row in values if row.metrics.get("agent_eligible") is True]
        passed = sum(row.status == "passed" for row in eligible)
        durations = sorted(row.metrics["wall_ms"] for row in eligible
                           if row.status == "passed" and isinstance(row.metrics.get("wall_ms"), (int, float)))
        costs = [row.metrics.get("cost_usd") for row in values]
        complete_cost = len(values) == planned and all(isinstance(v, (float, int)) for v in costs)
        total_cost = sum(costs) if complete_cost else None
        summaries.append({
            "id": agent["id"], "tool_layer": agent.get("tool_layer", "aiden"),
            "planned": planned, "completed": len(values),
            "eligible": len(eligible), "passed": passed,
            "success_rate": passed / len(eligible) if eligible else None,
            "coverage": len(eligible) / planned,
            "median_wall_ms": statistics.median(durations) if durations else None,
            "total_cost_usd": total_cost,
            "cost_per_success_usd": total_cost / passed if total_cost is not None and passed else None,
            "error_attempts": sum(row.metrics.get("agent_eligible") is not True for row in values),
            "timeouts": sum(row.status == "timeout" for row in values),
            # Timeouts that count as failures; excluded attempts can also have timed out.
            "scored_timeouts": sum(row.status == "timeout" for row in eligible),
            "execution_errors": sum(bool(row.metrics.get("agent_error")) for row in values),
            "native_tool_calls": sum(row.metrics.get("native_tool_calls") or 0 for row in values),
            "api_retry_attempts": sum(bool(row.metrics.get("api_retries")) for row in values),
            "native_loopback_attempts": sum(bool(row.metrics.get("native_loopback_calls")) for row in values),
            "exclusions": dict(sorted(collections.Counter(
                reason for reason in map(exclusion_reason, values) if reason).items())),
        })
    paired = []
    for task in suite.tasks:
        for attempt in range(1, repeats + 1):
            cells = {}
            for agent in agents:
                row = by_agent[agent["id"]].get((task.id, attempt))
                cells[agent["id"]] = ({"status": row.status, "wall_ms": row.metrics.get("wall_ms"),
                                       "eligible": row.metrics.get("agent_eligible") is True,
                                       "execution_error": row.metrics.get("agent_error"),
                                       "failure_class": row.metrics.get("failure_class"),
                                       "exclusion": exclusion_reason(row)}
                                      if row else {"status": "not_run"})
            paired.append({"suite": suite.name, "task_id": task.id, "attempt": attempt, "agents": cells})
    pairwise = []
    for left, right in itertools.combinations(by_agent, 2):
        matched = [row["agents"] for row in paired
                   if all(row["agents"][key].get("eligible") for key in (left, right))]
        both_pass = [pair for pair in matched if all(pair[key]["status"] == "passed" for key in (left, right))]
        values = {}
        for key in (left, right):
            passed = sum(pair[key]["status"] == "passed" for pair in matched)
            durations = [pair[key]["wall_ms"] for pair in both_pass]
            values[key] = {"passed": passed, "success_rate": passed / len(matched) if matched else None,
                           "both_pass_median_wall_ms": statistics.median(durations)
                           if durations and all(type(v) in (int, float) for v in durations) else None}
        pairwise.append({"agent_ids": [left, right], "planned_pairs": planned,
                         "eligible_pairs": len(matched), "excluded_or_missing_pairs": planned - len(matched),
                         "both_pass": len(both_pass),
                         "both_fail": sum(all(pair[key]["status"] != "passed" for key in (left, right)) for pair in matched),
                         "left_only_pass": values[left]["passed"] - len(both_pass),
                         "right_only_pass": values[right]["passed"] - len(both_pass), "agents": values})
    from runner.operations import preparation_steps
    # Deterministic environment_tools seeding is checked against expected
    # state; only agent-driven steps can leave differing starting screens.
    preparation_tasks = [task.id for task in suite.tasks
                         if any(step["type"] == "agent_prompt" for step in
                                preparation_steps(suite.global_reset) + preparation_steps(task.setup))]
    return {"agents": summaries, "pairwise": pairwise, "paired_attempts": paired,
            "agent_preparation_tasks": preparation_tasks}


REPORT_STYLE = """
:root{--bg:#f6f7f9;--panel:#fff;--text:#1d2733;--muted:#5b6876;--line:#dfe3e8;--accent:#17654b;
--pass:#1f7a4d;--pass-bg:#e3f4ea;--fail:#a1322b;--fail-bg:#fbe7e5;--warn:#8a5a00;--warn-bg:#fdf1d8;
--skip:#5b6876;--skip-bg:#eef0f3;--even:#f1f3f5}
@media (prefers-color-scheme:dark){:root{--bg:#12161b;--panel:#1a2027;--text:#e4e8ec;--muted:#9aa6b2;
--line:#2c343d;--accent:#5fc49a;--pass:#7fd6a6;--pass-bg:#173a29;--fail:#f29a92;--fail-bg:#43201d;
--warn:#f0c46a;--warn-bg:#3d3014;--skip:#a4afba;--skip-bg:#262d35;--even:#232a32}}
*{box-sizing:border-box}body{margin:0;background:var(--bg);color:var(--text);font:15px/1.5 system-ui,sans-serif}
main{max-width:1180px;margin:0 auto;padding:24px 16px 48px}a{color:var(--accent)}
h1{font-size:26px;margin:4px 0}h2{font-size:18px;margin:0 0 4px}.muted{color:var(--muted)}
.meta{display:flex;flex-wrap:wrap;gap:6px 18px;margin:8px 0 0;color:var(--muted);font-size:14px}
.panel{background:var(--panel);border:1px solid var(--line);border-radius:10px;padding:18px;margin:18px 0}
.scroll{overflow-x:auto}table{border-collapse:collapse;width:100%;margin-top:10px}
th,td{padding:8px 10px;border-bottom:1px solid var(--line);text-align:left;white-space:nowrap}
th{font-size:13px;color:var(--muted);font-weight:600}td.num,th.num{text-align:right}
.bar{display:inline-block;vertical-align:middle;width:72px;height:6px;border-radius:3px;background:var(--even);margin-left:8px}
.bar>i{display:block;height:100%;border-radius:3px;background:var(--pass)}
.badge{display:inline-block;min-width:22px;padding:1px 7px;border-radius:10px;font-size:13px;text-align:center}
.pass{color:var(--pass);background:var(--pass-bg)}.fail{color:var(--fail);background:var(--fail-bg)}
.timeout{color:var(--warn);background:var(--warn-bg)}.excluded{color:var(--skip);background:var(--skip-bg)}
.small{font-size:12px}.chip{display:inline-block;margin:0 4px 2px 0;padding:0 7px;border-radius:9px;
background:var(--skip-bg);color:var(--skip);font-size:12px}
tr.unsolved td:first-child{box-shadow:inset 3px 0 var(--fail)}details{margin-top:8px}
pre{white-space:pre-wrap;overflow-wrap:anywhere;font-size:12px}
"""
STATUS_BADGE = {"passed": ("pass", "Pass"), "failed": ("fail", "Fail"), "timeout": ("timeout", "Timeout")}
EXCLUSION_SHORT = {"platform": "Platform", "preparation": "Setup", "network": "Network", "not_started": "No start",
                   "harness": "Harness", "evaluation": "Judge", "canceled": "Canceled"}


def comparison_html(manifest: dict, data: dict) -> str:
    """Render the experiment overview, task results, exclusions, and diagnostics."""
    escape = lambda value: html.escape(str(value))
    seconds = lambda ms: f"{ms / 1000:.1f}s" if isinstance(ms, (int, float)) else "—"
    agents = data["agents"]
    config = {agent["id"]: agent for agent in manifest.get("agents", [])}
    ids = [agent["id"] for agent in agents]
    headers = "".join(f"<th>{escape(agent_id)}</th>" for agent_id in ids)

    models = sorted({str(agent.get("model")) for agent in config.values() if agent.get("model")})
    judge = (manifest.get("judge_config") or {}).get("model")
    started = str(manifest.get("created_at", ""))[:16].replace("T", " ")
    meta = "".join(f"<span>{item}</span>" for item in (
        f"Suite <b>{escape(manifest.get('suite', ''))}</b>", f"Status <b>{escape(manifest.get('status', ''))}</b>",
        f"{manifest.get('repeats', 1)} repeat(s)", f"{manifest.get('concurrency', 1)} in parallel",
        f"Model {escape(', '.join(models) or '—')}", f"Judge {escape(judge or '—')}", f"Started {escape(started)} UTC"))

    overview = []
    for agent in agents:
        kind = config.get(agent["id"], {}).get("kind", "")
        rate = agent["success_rate"]
        chips = "".join(f"<span class='chip' title='{escape(EXCLUSION_REASONS.get(reason, reason))}'>"
                        f"{escape(EXCLUSION_SHORT.get(reason, reason))} {count}</span>"
                        for reason, count in agent["exclusions"].items())
        overview.append(
            f"<tr><td><a href='runs/{escape(agent['id'])}/report.html'>{escape(agent['id'])}</a></td>"
            f"<td>{escape(HARNESSES[kind].label if kind in HARNESSES else kind)}</td>"
            f"<td>{escape(agent.get('tool_layer', 'aiden'))}</td><td class='num'>{agent['passed']}/{agent['eligible']}</td>"
            f"<td>{f'{rate:.0%}' if rate is not None else '—'}"
            f"<span class='bar'><i style='width:{(rate or 0) * 100:.0f}%'></i></span></td>"
            f"<td class='num'>{agent['coverage']:.0%}</td><td class='num'>{seconds(agent['median_wall_ms'])}</td>"
            f"<td class='num'>{agent.get('scored_timeouts', agent['timeouts'])}</td><td>{sum(agent['exclusions'].values())} {chips}</td></tr>")

    tasks = []
    for row in data["paired_attempts"]:
        cells, scored, passed = [], 0, 0
        for agent_id in ids:
            cell = row["agents"].get(agent_id, {"status": "not_run"})
            if cell.get("eligible"):
                scored += 1
                passed += cell["status"] == "passed"
                tone, label = STATUS_BADGE.get(cell["status"], ("fail", cell["status"]))
            elif cell["status"] == "not_run":
                tone, label = "excluded", "Not run"
            else:
                tone, label = "excluded", EXCLUSION_SHORT.get(cell.get("exclusion") or "harness", "Excluded")
            cells.append(f"<td><span class='badge {tone}'>{escape(label)}</span></td>")
        name = row["task_id"] + (f" #{row['attempt']}" if manifest.get("repeats", 1) > 1 else "")
        css = "unsolved" if scored and not passed else ""
        tasks.append(f"<tr class='{css}'><td>{escape(name)}</td>{''.join(cells)}<td class='num'>{passed}/{scored}</td></tr>")

    reasons = [reason for reason in EXCLUSION_REASONS if any(agent["exclusions"].get(reason) for agent in agents)]
    exclusion_rows = "".join(
        f"<tr><td>{escape(EXCLUSION_SHORT[reason])}</td><td class='muted'>{escape(EXCLUSION_REASONS[reason])}</td>"
        + "".join(f"<td class='num'>{agent['exclusions'].get(reason, 0) or ''}</td>" for agent in agents) + "</tr>"
        for reason in reasons)
    exclusion_headers = "".join(f"<th class='num'>{escape(agent_id)}</th>" for agent_id in ids)
    exclusions = ("<section class='panel'><h2>Exclusions</h2><p class='muted'>Excluded attempts are not scored. "
                  "Uneven exclusions change which tasks each agent is scored on.</p><div class='scroll'><table>"
                  f"<thead><tr><th>Reason</th><th>Meaning</th>{exclusion_headers}</tr></thead>"
                  f"<tbody>{exclusion_rows}</tbody></table></div></section>") if reasons else ""

    diagnostics = "".join(
        f"<tr><td>{escape(agent['id'])}</td><td class='num'>{agent['completed']}/{agent['planned']}</td>"
        f"<td class='num'>{agent['execution_errors']}</td><td class='num'>{agent['api_retry_attempts']}</td>"
        f"<td class='num'>{agent['native_tool_calls']} ({agent['native_loopback_attempts']} flagged)</td>"
        f"<td class='num'>{escape(agent['total_cost_usd']) if agent['total_cost_usd'] is not None else '—'}</td></tr>"
        for agent in agents)
    preparation = (f"{len(data['agent_preparation_tasks'])} task(s) use agent-driven preparation, which does not "
                   "guarantee identical starting screens; inspect pre-screenshots."
                   if data["agent_preparation_tasks"] else "No task uses agent-driven preparation.")
    return f"""<!doctype html><html lang="en"><head><meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1"><title>Agent comparison</title>
<style>{REPORT_STYLE}</style></head><body><main>
<a href="/experiments">Experiments</a><h1>Agent comparison</h1>
<div class="muted">{escape(manifest.get('id', ''))}</div><div class="meta">{meta}</div>
<section class="panel"><h2>Overview</h2>
<p class="muted">Success counts scored attempts only; coverage is the share of planned attempts that could be scored.
Median time covers passed attempts, and timeouts count scored attempts that ran out of time.</p>
<div class="scroll"><table><thead><tr><th>Agent</th><th>Harness</th><th>Tool layer</th><th class="num">Passed</th>
<th>Success</th><th class="num">Coverage</th><th class="num">Median time</th><th class="num">Timeouts</th><th>Excluded</th>
</tr></thead><tbody>{''.join(overview)}</tbody></table></div></section>
<section class="panel"><h2>Task results</h2>
<p class="muted">The last column counts passes among scored agents. Rows marked red were passed by no scored agent.
Gray badges are excluded attempts.</p>
<div class="scroll"><table><thead><tr><th>Task</th>{headers}<th class="num">Passed</th></tr></thead>
<tbody>{''.join(tasks)}</tbody></table></div></section>
{exclusions}
<section class="panel"><h2>Diagnostics and scope</h2>
<p class="muted">Every agent receives the same device context and the device UI tools of its labeled tool layer;
planning, memory, and context stay agent-owned. Scoring uses the original suite assertions and rubric scoring.
Aiden runs in its daemon container; external harnesses run on the
host with fresh configuration and only the business tools. Each attempt resets the MobileGym environment.
{escape(preparation)} Model retries are failed model requests or reconnects that pi, Claude Code, or Codex reported.
Native tool calls are harness tools outside the business catalog; flagged ones referenced a local endpoint.
Missing cost is unknown.</p>
<div class="scroll"><table><thead><tr><th>Agent</th><th class="num">Recorded</th><th class="num">Execution errors</th>
<th class="num">Attempts with model retries</th><th class="num">Native tool calls</th><th class="num">Total USD</th>
</tr></thead><tbody>{diagnostics}</tbody></table></div>
<details><summary>Frozen experiment configuration</summary><pre>{escape(json.dumps(manifest, indent=2))}</pre></details>
</section></main></body></html>"""


def write_experiment_results(suite: Suite, root: Path, agents: list[dict],
                             rows: dict[str, list[TaskResult]], repeats: int, manifest: dict,
                             *, changed_agent: str | None = None) -> None:
    for agent in agents:
        run_id = agent["id"]
        if changed_agent is not None and run_id != changed_agent:
            continue
        directory = root / "runs" / run_id
        directory.mkdir(parents=True, exist_ok=True)
        if not (directory / "suite.json").exists():
            (directory / "suite.json").write_bytes(suite.source_bytes)
        run_manifest = {
            "experiment_id": manifest["id"], "run_id": run_id, "suite": suite.name,
            "suite_sha256": suite.sha256, "agent_model": agent.get("model", agent["kind"]),
            "agent": agent, "repeats": repeats, "metrics_k": repeats,
            "judge_config": manifest.get("judge_config"), "started_at": manifest["created_at"],
            "experiment_track": manifest.get("track", "native"), "status": manifest["status"],
            "suite_path": str(suite.source_path), "finished_at": manifest.get("finished_at", ""),
            "totals": {"tasks": len(rows[run_id]), **{
                status: sum(row.status == status for row in rows[run_id])
                for status in ("passed", "failed", "skipped", "timeout", "judge_error")
            }},
        }
        atomic_write(directory / "manifest.json", lambda path: write_manifest(path, run_manifest))
        atomic_write(directory / "results.jsonl", lambda path: write_jsonl(path, rows[run_id]))
        atomic_write(directory / "metrics.json",
                     lambda path: write_metrics(path, suite.name, run_manifest, rows[run_id]))
        atomic_write(directory / "summary.md",
                     lambda path: write_summary(path, suite.name, run_manifest, rows[run_id]))
        atomic_write(directory / "report.html",
                     lambda path: path.write_text(generate_report_html(directory), encoding="utf-8"))
    data = comparison(suite, agents, rows, repeats)
    manifest["completed"] = sum(len(items) for items in rows.values())
    atomic_json(root / "manifest.json", manifest)
    atomic_json(root / "comparison.json", data)
    atomic_write(root / "report.html",
                 lambda path: path.write_text(comparison_html(manifest, data), encoding="utf-8"))


def _result_from_record(record: dict) -> TaskResult:
    row = dict(record)
    row["rubric"] = [RubricVerdict(**item) for item in row.get("rubric", [])]
    if isinstance(row.get("hard_assertions"), dict):
        row["hard_assertions"] = HardAssertionResults(**row["hard_assertions"])
    row["hard_assertion_failures"] = [HardAssertionFailure(**item)
                                      for item in row.get("hard_assertion_failures", [])]
    return TaskResult(**row)


def _retryable_judge_error(row: dict) -> bool:
    return (row.get("status") == "judge_error" and row.get("rubric_total", 0) > 0
            and not execution_gate_blocks_rejudge(row))


def _has_judge_errors(root: Path, agents: list[dict]) -> bool:
    return any(
        any(_retryable_judge_error(json.loads(line)) for line in
            (root / "runs" / agent["id"] / "results.jsonl").read_text(encoding="utf-8").splitlines())
        for agent in agents
    )


def retry_experiment_judging(root: Path, judge_cfg: JudgeConfig, *,
                             api_key_env: str = DEFAULT_JUDGE_API_KEY_ENV) -> dict:
    """Re-score recorded judge errors and refresh both run and paired reports."""
    manifest = json.loads((root / "manifest.json").read_text(encoding="utf-8"))
    if manifest.get("status") not in {"completed", "canceled"}:
        raise ValueError("Only finished experiments can retry Judge scoring")
    if not manifest.get("judge_config"):
        raise ValueError("This experiment does not use Judge scoring")
    suite = load_suite(root / "suite.json")
    agents = manifest["agents"]
    rows: dict[str, list[TaskResult]] = {}
    original_errors = 0
    remaining_errors = 0
    for agent in agents:
        run_dir = root / "runs" / agent["id"]
        source = run_dir / "results.jsonl"
        if not source.exists():
            raise ValueError(f"Missing run results for {agent['id']}")
        recorded = [json.loads(line) for line in source.read_text(encoding="utf-8").splitlines()]
        original_errors += sum(_retryable_judge_error(row) for row in recorded)
        rows[agent["id"]] = [_result_from_record(row) for row in recorded]
    if not original_errors:
        return manifest
    for agent in agents:
        run_dir = root / "runs" / agent["id"]
        if not any(_retryable_judge_error(dc.asdict(row)) for row in rows[agent["id"]]):
            continue
        rejudge_run(run_dir, judge_cfg.model, judge_cfg.base_url,
                    judge_api_key_env=api_key_env, only_judge_errors=True)
        path = run_dir / "results.rejudged.jsonl"
        rows[agent["id"]] = [_result_from_record(json.loads(line))
                             for line in path.read_text(encoding="utf-8").splitlines()]
        remaining_errors += sum(_retryable_judge_error(dc.asdict(row)) for row in rows[agent["id"]])
    manifest["judge_retry"] = {"attempted_at": now_iso(), "recovered": original_errors - remaining_errors,
                               "remaining": remaining_errors}
    for agent in agents:
        run_dir = root / "runs" / agent["id"]
        original = run_dir / "results.original.jsonl"
        if not original.exists():
            original.write_bytes((run_dir / "results.jsonl").read_bytes())
    write_experiment_results(suite, root, agents, rows, manifest["repeats"], manifest)
    return manifest


def _task_for_tool_layer(task, layer: str, metrics: dict):
    """Raw tools have their own names, so suite assertions on Aiden tool names do not apply."""
    from runner.raw_tools import TOOL_LAYER as RAW_TOOL_LAYER
    assertions = task.hard_assertions
    if layer != RAW_TOOL_LAYER or not (assertions.required_tools or assertions.required_tool_calls):
        return task
    metrics["tool_name_assertions"] = "not_applicable"
    return dc.replace(task, hard_assertions=dc.replace(assertions, required_tools=[], required_tool_calls=[]))


def run_experiment(suite: Suite, root: Path, agents: list[dict], adapters: dict[str, OperationHarness],
                   repeats: int, seed: int, stop: threading.Event, manifest: dict,
                   judge_cfg: JudgeConfig | None = None, concurrency: int = 1) -> dict:
    """Run paired blocks in schedule order, up to `concurrency` attempts at a time.

    Each attempt owns a fresh daemon, tool gateway, and environment route, so
    attempts can overlap on a bridge with that many parallel environments.
    """
    if any(task.rubric for task in suite.tasks) and judge_cfg is None:
        raise ValueError("This suite requires a configured Judge; rubric scoring cannot be disabled")
    rows: dict[str, list[TaskResult]] = {agent["id"]: [] for agent in agents}
    rng = random.Random(seed)
    schedule = []
    for task in suite.tasks:
        for attempt in range(1, repeats + 1):
            order = list(adapters)
            rng.shuffle(order)
            schedule.extend((task, attempt, agent_id) for agent_id in order)
    manifest.update(status="running", comparison_policy="matched-eligible-v2", concurrency=concurrency,
                    schedule=[{"task_id": t.id, "attempt": n, "agent_id": a} for t, n, a in schedule])
    task_order = {task.id: index for index, task in enumerate(suite.tasks)}
    lock = threading.Lock()
    active: dict[int, dict] = {}

    def save(changed_agent: str | None = None) -> None:
        # Callers hold the lock. Completion order varies with concurrency.
        for values in rows.values():
            values.sort(key=lambda row: (task_order.get(row.task_id, 0), row.attempt))
        write_experiment_results(suite, root, agents, rows, repeats, manifest, changed_agent=changed_agent)

    def publish_active() -> None:
        manifest["current_attempts"] = list(active.values())
        if active:
            manifest["current_attempt"] = active[max(active)]
        atomic_json(root / "manifest.json", manifest)

    def run_attempt(index: int, task, attempt: int, agent_id: str) -> None:
        if stop.is_set():
            return
        with lock:
            active[index] = {"agent_id": agent_id, "task_id": task.id, "attempt": attempt, "started_at": now_iso()}
            manifest["stage"] = "Executing task"
            publish_active()
        artifacts = root / "runs" / agent_id / "tasks" / task.id / f"attempt_{attempt}"
        artifacts.mkdir(parents=True, exist_ok=True)
        prompt = "\n\n".join(part for part in (DEVICE_CONTEXT, suite.prompt_prefix.strip(), task.prompt) if part)
        try:
            adapter = adapters[agent_id]
            if task.environment_bridge_types and "mobilegym" not in task.environment_bridge_types:
                row = skipped_task_result(suite, task, attempt, artifacts, agent_id,
                                          f"Task requires bridges {task.environment_bridge_types}; environment bridge is mobilegym")
                row.metrics.update(agent_eligible=False, failure_class="platform")
            elif task.platforms and "android" not in task.platforms:
                row = skipped_task_result(suite, task, attempt, artifacts, agent_id,
                                          f"Task requires {task.platforms}; environment platform is android")
                row.metrics.update(agent_eligible=False, failure_class="platform")
            else:
                row = _execute_and_score(suite, task, attempt, agent_id, adapter, prompt, artifacts, stop,
                                         manifest, lock, judge_cfg, root)
        except Exception as exc:
            row = skipped_task_result(suite, task, attempt, artifacts, agent_id, str(exc))
            row.metrics.update(adapter_error=str(exc), agent_eligible=False, failure_class="adapter")
        with lock:
            rows[agent_id].append(row)
            active.pop(index, None)
            save(agent_id)
            publish_active()

    with lock:
        save()
    # One worker keeps the original sequential schedule; more overlap blocks in order.
    with concurrent.futures.ThreadPoolExecutor(max_workers=max(1, concurrency),
                                               thread_name_prefix="experiment") as executor:
        for future in [executor.submit(run_attempt, index, *item) for index, item in enumerate(schedule)]:
            future.result()
    with lock:
        manifest.pop("current_attempt", None)
        manifest.pop("current_attempts", None)
        manifest["stage"] = "Finished"
        manifest["status"] = "canceled" if stop.is_set() else "completed"
        manifest["finished_at"] = now_iso()
        save()
    return manifest


def _execute_and_score(suite: Suite, task, attempt: int, agent_id: str, adapter: OperationHarness, prompt: str,
                       artifacts: Path, stop: threading.Event, manifest: dict, lock: threading.Lock,
                       judge_cfg: JudgeConfig | None, root: Path) -> TaskResult:
    execution = adapter.execute_operation(prompt, task.hard_assertions.must_complete_within_sec, artifacts, stop, task)
    digest = execution.metrics.get("business_tools_sha256")
    scored_task = task
    if not execution.canceled:
        if not digest:
            raise ValueError("Operation attempt did not record its shared tool catalog")
        layer = execution.metrics.get("tool_layer", "aiden")
        with lock:
            frozen = manifest.setdefault("business_tool_catalogs", {}).setdefault(
                layer, {"sha256": digest, "tools": execution.metrics.get("business_tools")})
        if digest != frozen["sha256"]:
            raise ValueError("Shared business tool catalog changed between attempts")
        scored_task = _task_for_tool_layer(task, layer, execution.metrics)
    row = evaluate_task_history(
        suite=suite, task=scored_task, history=execution.history, episode=execution.episode,
        attempt=attempt, artifact_dir=artifacts, run_id=agent_id, timed_out=execution.timed_out,
        judge_cfg=judge_cfg, judge_cache_dir=root / "judge-cache", metrics=execution.metrics,
        pre_screenshot=execution.pre_screenshot, post_screenshot=execution.post_screenshot,
    )
    row.metrics.update(execution.metrics)
    if not execution.canceled:
        row = _apply_environment_assertions(row, task, execution.environment_state, execution.environment_state_error)
        usage = tool_usage_metrics(execution.history, set(execution.metrics.get("business_tools") or ()))
        usage["native_tool_calls"] = max(usage["native_tool_calls"],
                                         execution.metrics.get("native_tool_calls_reported") or 0)
        row.metrics.update(usage)
    if execution.canceled:
        row.status = "canceled"
        row.metrics.update(agent_eligible=False, success=None, failure_class="canceled")
    return row


class ExperimentManager:
    def __init__(self, *, runs_dir: Path, suites_dir: Path, base_config_dir: Path,
                 daemon_image: str, build_daemon_image: bool, config_loader, judge_loader=None):
        # Absolute, so recorded artifact paths resolve from any working directory.
        self.root = runs_dir.resolve() / "experiments"
        self.root.mkdir(parents=True, exist_ok=True)
        self.suites_dir = suites_dir
        self.base_config_dir = base_config_dir
        self.daemon_image = daemon_image
        self.build_daemon_image = build_daemon_image
        self.config_loader = config_loader
        self.judge_loader = judge_loader or (lambda: {})
        self._lock = threading.Lock()
        self._active: tuple[str, threading.Event, threading.Thread] | None = None

    def settings(self) -> dict:
        path = self.root.parent / "experiment-settings.json"
        with self._lock:
            return json.loads(path.read_text(encoding="utf-8")) if path.exists() else {}

    def save_settings(self, payload: dict) -> dict:
        if not isinstance(payload, dict) or set(payload) != {"pi"}:
            raise ValueError("Settings require a pi configuration")
        pi = validate_agents([payload["pi"]])[0]
        if pi["kind"] != "pi":
            raise ValueError("Settings require kind=pi")
        settings = {"pi": pi}
        with self._lock:
            atomic_json(self.root.parent / "experiment-settings.json", settings)
        return settings

    def suites(self) -> list[dict]:
        result = []
        for path in sorted(self.suites_dir.rglob("*.json")):
            try:
                suite = comparison_suite(path)
                result.append({"key": path.relative_to(self.suites_dir).as_posix(), "name": suite.name,
                               "tasks": len(suite.tasks), "track": "operations", "compatible": True,
                               "requires_judge": any(task.rubric for task in suite.tasks)})
            except (ValueError, KeyError) as exc:
                result.append({"key": path.relative_to(self.suites_dir).as_posix(), "name": path.stem,
                               "compatible": False, "reason": str(exc)})
        return result

    def list(self) -> list[dict]:
        result = []
        with self._lock:
            active = self._active[0] if self._active and self._active[2].is_alive() else None
        for path in sorted(self.root.glob("*/manifest.json"), reverse=True):
            item = json.loads(path.read_text(encoding="utf-8"))
            if item["status"] in {"queued", "preparing", "running"} and item["id"] != active:
                item["status"] = "interrupted"
            item["judge_errors"] = sum(
                sum(_retryable_judge_error(json.loads(line)) for line in results.read_text(encoding="utf-8").splitlines())
                for agent in item.get("agents", [])
                if (results := path.parent / "runs" / agent["id"] / "results.jsonl").is_file()
            )
            item["report_url"] = f"/experiments/{item['id']}/report.html" if (path.parent / "report.html").exists() else ""
            item["setup_log_url"] = f"/experiments/{item['id']}/setup.log" if (path.parent / "setup.log").exists() else ""
            result.append(item)
        return sorted(result, key=lambda item: (item["id"] == active, item.get("created_at", "")), reverse=True)

    def start(self, payload: dict) -> dict:
        if not isinstance(payload, dict):
            raise ValueError("Experiment configuration must be an object")
        allowed = {"suite", "agents", "repeats", "seed", "environment_url", "concurrency"}
        if set(payload) - allowed:
            raise ValueError("Unknown experiment configuration fields")
        path = (self.suites_dir / str(payload.get("suite", ""))).resolve()
        if self.suites_dir.resolve() not in path.parents or not path.is_file():
            raise ValueError("Choose a suite inside the configured suites directory")
        suite = comparison_suite(path)
        agents = validate_agents(payload.get("agents"))
        for agent in agents:
            if agent.get("base_url") and not os.environ.get(agent["api_key_env"], "").strip():
                raise ValueError(f"Set {agent['api_key_env']} in the benchmark server environment before running {agent['kind']}")
        environment_url = str(payload.get("environment_url") or "").strip()
        from runner.environment_endpoint import EnvironmentEndpoint
        environment_url = EnvironmentEndpoint(environment_url).base
        if urllib.parse.urlsplit(environment_url).username is not None:
            raise ValueError("Do not embed credentials in the environment URL")
        judge_settings = dict(self.judge_loader())
        judge_cfg = None
        judge_key = ""
        if any(task.rubric for task in suite.tasks):
            if not judge_settings.get("enabled", True):
                raise ValueError("Enable Judge in WebUI settings; this suite requires rubric scoring")
            judge_cfg = JudgeConfig()
            for name in ("model", "base_url"):
                if judge_settings.get(name):
                    setattr(judge_cfg, name, str(judge_settings[name]))
            judge_key = str(judge_settings.get("api_key") or os.environ.get(DEFAULT_JUDGE_API_KEY_ENV, "")).strip()
            if not judge_key:
                raise ValueError(f"Configure Judge credentials in WebUI settings or {DEFAULT_JUDGE_API_KEY_ENV}")
        repeats, seed = payload.get("repeats", 3), payload.get("seed", 0)
        if type(repeats) is not int or not 1 <= repeats <= 20 or type(seed) is not int:
            raise ValueError("repeats must be 1–20 and seed must be an integer")
        concurrency = payload.get("concurrency", 1)
        if type(concurrency) is not int or not 1 <= concurrency <= 8:
            raise ValueError("concurrency must be 1–8 parallel attempts")
        with self._lock:
            if self._active and self._active[2].is_alive():
                raise ValueError("An experiment is already running; stop it or wait for completion")
            config_text = self.config_loader()
            experiment_id = "exp-" + uuid.uuid4().hex[:16]
            root = self.root / experiment_id
            root.mkdir()
            (root / "suite.json").write_bytes(suite.source_bytes)
            sha, dirty = git_sha(Path(__file__).resolve().parents[2])
            manifest = {"schema_version": 1, "id": experiment_id, "status": "queued",
                        "created_at": now_iso(), "suite": suite.name, "suite_sha256": suite.sha256,
                        "track": "operations", "agents": agents, "repeats": repeats, "seed": seed,
                        "concurrency": concurrency,
                        "environment_url": environment_url,
                        "scoring": "original-suite-v1", "device_context": DEVICE_CONTEXT,
                        "preparation": "shared-aiden-preparer-fresh-evaluated-agent",
                        "judge_config": dc.asdict(judge_cfg) if judge_cfg else None, "git_sha": sha, "git_dirty": dirty,
                        "total": len(suite.tasks) * repeats * len(agents), "completed": 0}
            atomic_json(root / "manifest.json", manifest)
            stop = threading.Event()
            thread = threading.Thread(target=self._run, args=(suite, root, manifest, config_text, stop, judge_cfg, judge_key), daemon=True)
            self._active = (experiment_id, stop, thread)
            thread.start()
            return dict(manifest)

    def _run(self, suite: Suite, root: Path, manifest: dict, config_text: str | None, stop: threading.Event,
             judge_cfg: JudgeConfig | None, judge_key: str) -> None:
        credential_env = "AIDEN_EXPERIMENT_JUDGE_" + uuid.uuid4().hex
        if judge_cfg is not None:
            judge_cfg = dc.replace(judge_cfg, api_key_env=credential_env)
            os.environ[credential_env] = judge_key
        try:
            manifest.update(status="preparing", stage="Checking environment")
            atomic_json(root / "manifest.json", manifest)
            with tempfile.TemporaryDirectory(prefix="aiden-experiment-config-") as scratch:
                adapters = {}
                prepared = Path(scratch) / "config"
                from runner.platform import read_environment_health
                from runner.operations import OperationAdapter
                from runner.webui import ensure_daemon_image, prepare_run_config
                health = read_environment_health(manifest["environment_url"])
                data = health.get("data", health)
                if data.get("bridge_type") != "mobilegym":
                    raise ValueError("Operation experiments require a resettable MobileGym environment")
                concurrency = manifest.get("concurrency", 1)
                if concurrency > int(data.get("env_count") or 1):
                    raise ValueError(f"concurrency {concurrency} exceeds the environment's "
                                     f"{data.get('env_count') or 1} parallel MobileGym environments")
                manifest["environment_health"] = health
                prepare_run_config(self.base_config_dir, prepared, agent_config_text=config_text)
                manifest["stage"] = "Building daemon image" if self.build_daemon_image else "Checking daemon image"
                atomic_json(root / "manifest.json", manifest)
                ensure_daemon_image(self.daemon_image, self.build_daemon_image, root / "setup.log", stop_requested=stop.is_set)
                manifest["stage"] = "Preparing agent adapters"
                atomic_json(root / "manifest.json", manifest)
                image_id = subprocess.check_output(["docker", "image", "inspect", "--format", "{{.Id}}", self.daemon_image], text=True).strip()
                manifest["tool_server_image"] = image_id
                # Record the shared tool/preparer identity even for pi-only runs.
                config_bytes = (prepared / "agent.toml").read_bytes()
                try:
                    import tomllib
                except ImportError:
                    import tomli as tomllib
                model = tomllib.loads(config_bytes.decode())["model_settings"]["model"]
                redacted_config = re.sub(r'(?m)^(\s*api_key\s*=\s*).+$', r'\1"<redacted>"', config_bytes.decode())
                config_sha = hashlib.sha256(redacted_config.encode()).hexdigest()
                manifest["tool_server_config_sha256"] = config_sha
                manifest["preparer_model"] = model.get("model", "")
                for agent in manifest["agents"]:
                    if stop.is_set():
                        break
                    harness = HARNESSES[agent["kind"]]
                    if harness.external:
                        agent.update(harness.runtime(agent, Path(scratch)))
                        external = harness.build(agent)
                    else:
                        agent.update(model=model.get("model", ""), provider=model.get("provider", ""),
                                     image_id=image_id, config_sha256=config_sha)
                        external = None
                    adapters[agent["id"]] = OperationAdapter(
                        # External harnesses use this daemon only as their Aiden-layer tool server.
                        daemon=AidenAdapter(image=image_id, prepared_config=prepared),
                        environment_url=manifest["environment_url"], external=external,
                        tool_layer=agent["tool_layer"],
                        global_reset=suite.global_reset, prompt_prefix=suite.prompt_prefix,
                    )
                if stop.is_set():
                    manifest["status"] = "canceled"
                    atomic_json(root / "manifest.json", manifest)
                    return
                run_experiment(suite, root, manifest["agents"], adapters, manifest["repeats"], manifest["seed"], stop,
                               manifest, judge_cfg, concurrency=manifest.get("concurrency", 1))
                if judge_cfg is not None and not stop.is_set() and _has_judge_errors(root, manifest["agents"]):
                    manifest["stage"] = "Retrying Judge errors"
                    atomic_json(root / "manifest.json", manifest)
                    try:
                        manifest = retry_experiment_judging(root, judge_cfg, api_key_env=credential_env)
                        manifest["stage"] = "Finished"
                        atomic_json(root / "manifest.json", manifest)
                    except Exception as exc:
                        manifest = json.loads((root / "manifest.json").read_text(encoding="utf-8"))
                        manifest["stage"] = "Judge retry failed"
                        manifest["judge_retry_error"] = str(exc)
                        atomic_json(root / "manifest.json", manifest)
        except Exception as exc:
            manifest.update(status="canceled" if stop.is_set() else "failed", error=str(exc), finished_at=now_iso())
            atomic_json(root / "manifest.json", manifest)
        finally:
            os.environ.pop(credential_env, None)

    def stop(self, experiment_id: str) -> bool:
        with self._lock:
            if self._active and self._active[0] == experiment_id and self._active[2].is_alive():
                self._active[1].set()
                return True
        return False

    def retry_judge(self, experiment_id: str) -> dict:
        if not SAFE_ID.fullmatch(experiment_id):
            raise ValueError("Invalid experiment ID")
        root = self.root / experiment_id
        path = root / "manifest.json"
        if not path.is_file():
            raise ValueError("Experiment not found")
        manifest = json.loads(path.read_text(encoding="utf-8"))
        if manifest.get("status") not in {"completed", "canceled"} or not manifest.get("judge_config"):
            raise ValueError("Only finished Judge experiments can retry scoring")
        if not _has_judge_errors(root, manifest["agents"]):
            raise ValueError("This experiment has no Judge errors to retry")
        settings = dict(self.judge_loader())
        key = str(settings.get("api_key") or os.environ.get(DEFAULT_JUDGE_API_KEY_ENV, "")).strip()
        if not key:
            raise ValueError("Configure Judge credentials before retrying scoring")
        judge_cfg = JudgeConfig(**manifest["judge_config"])
        with self._lock:
            if self._active and self._active[2].is_alive():
                raise ValueError("An experiment is already running")
            stop = threading.Event()
            thread = threading.Thread(target=self._retry_judge, args=(root, judge_cfg, key), daemon=True)
            self._active = (experiment_id, stop, thread)
            thread.start()
        return {"retrying": True, "id": experiment_id}

    def _retry_judge(self, root: Path, judge_cfg: JudgeConfig, key: str) -> None:
        credential_env = "AIDEN_EXPERIMENT_JUDGE_" + uuid.uuid4().hex
        os.environ[credential_env] = key
        try:
            manifest = json.loads((root / "manifest.json").read_text(encoding="utf-8"))
            manifest["stage"] = "Retrying Judge errors"
            atomic_json(root / "manifest.json", manifest)
            manifest = retry_experiment_judging(root, judge_cfg, api_key_env=credential_env)
            manifest["stage"] = "Finished"
            manifest.pop("judge_retry_error", None)
            atomic_json(root / "manifest.json", manifest)
        except Exception as exc:
            manifest = json.loads((root / "manifest.json").read_text(encoding="utf-8"))
            manifest["stage"] = "Judge retry failed"
            manifest["judge_retry_error"] = str(exc)
            atomic_json(root / "manifest.json", manifest)
        finally:
            os.environ.pop(credential_env, None)

    def wait(self, experiment_id: str) -> None:
        """Block until the active experiment with this ID finishes."""
        with self._lock:
            active = self._active
        if active and active[0] == experiment_id:
            while active[2].is_alive():
                active[2].join(timeout=0.5)

    def shutdown(self) -> None:
        with self._lock:
            active = self._active
        if active:
            active[1].set()
            active[2].join(timeout=15)


def experiment_cli(args) -> int:
    from runner.config import AgentConfigManager
    config = AgentConfigManager(
        base_config_dir=Path(args.base_config_dir),
        config_path=Path(args.agent_config) if args.agent_config else Path(args.out) / "experiment-agent.toml",
    )
    manager = ExperimentManager(
        runs_dir=Path(args.out), suites_dir=Path(args.suites_dir),
        base_config_dir=Path(args.base_config_dir), daemon_image=args.daemon_image,
        build_daemon_image=not args.no_build_daemon_image,
        config_loader=lambda: config.get_config()[0],
        judge_loader=lambda: {"model": args.judge_model, "base_url": args.judge_base_url},
    )
    try:
        record = manager.start(json.loads(Path(args.config).read_text(encoding="utf-8")))
        print(f"Experiment: {record['id']}", flush=True)
        manager.wait(record["id"])
        final = next(item for item in manager.list() if item["id"] == record["id"])
        print(f"Status: {final['status']}", flush=True)
        if final.get("error"):
            print(final["error"], flush=True)
        print(f"Artifacts: {manager.root / record['id']}", flush=True)
        return 0 if final["status"] == "completed" else 1
    except KeyboardInterrupt:
        return 130
    except (ValueError, OSError) as exc:
        print(f"Error: {exc}", flush=True)
        return 2
    finally:
        manager.shutdown()
