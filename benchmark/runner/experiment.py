"""One frozen experiment, independent agent runs, paired attempt reporting."""
from __future__ import annotations

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
                           if isinstance(row.metrics.get("wall_ms"), (int, float)))
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
            "execution_errors": sum(bool(row.metrics.get("agent_error")) for row in values),
            "native_tool_calls": sum(row.metrics.get("native_tool_calls") or 0 for row in values),
            "api_retry_attempts": sum(bool(row.metrics.get("api_retries")) for row in values),
            "native_loopback_attempts": sum(bool(row.metrics.get("native_loopback_calls")) for row in values),
            "exclusions": {reason: sum(row.metrics.get("agent_eligible") is not True
                                       and (row.metrics.get("failure_class") or "unknown") == reason
                                       for row in values)
                           for reason in sorted({row.metrics.get("failure_class") or "unknown"
                                                 for row in values if row.metrics.get("agent_eligible") is not True})},
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
                                       "failure_class": row.metrics.get("failure_class")}
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


def comparison_html(manifest: dict, data: dict) -> str:
    escape = lambda value: html.escape(str(value))
    summaries = []
    for agent in data["agents"]:
        rate = f"{agent['success_rate']:.1%}" if agent["success_rate"] is not None else "—"
        duration = f"{agent['median_wall_ms'] / 1000:.2f}s" if agent["median_wall_ms"] is not None else "—"
        summaries.append(f"<tr><td><a href='runs/{escape(agent['id'])}/report.html'>{escape(agent['id'])}</a>"
                         f" ({escape(agent.get('tool_layer', 'aiden'))} tools)</td>"
                         f"<td>{agent['passed']}/{agent['eligible']} ({rate})</td>"
                         f"<td>{agent['eligible']}/{agent['planned']}</td><td>{agent['completed']}/{agent['planned']}</td>"
                         f"<td>{duration}</td><td>{agent['timeouts']}</td><td>{agent['execution_errors']}</td>"
                         f"<td>{agent['native_tool_calls']} ({agent['native_loopback_attempts']} flagged)</td>"
                         f"<td>{agent['api_retry_attempts']}</td>"
                         f"<td>{escape(json.dumps(agent['exclusions']))}</td>"
                         f"<td>{escape(agent['total_cost_usd']) if agent['total_cost_usd'] is not None else '—'}</td></tr>")
    pairwise = []
    for pair in data["pairwise"]:
        for agent_id in pair["agent_ids"]:
            value = pair["agents"][agent_id]
            rate = f"{value['success_rate']:.1%}" if value['success_rate'] is not None else "—"
            wall = value["both_pass_median_wall_ms"]
            duration = f"{wall / 1000:.2f}s" if wall is not None else "—"
            pairwise.append(f"<tr><td>{escape(' / '.join(pair['agent_ids']))}</td><td>{escape(agent_id)}</td>"
                            f"<td>{value['passed']}/{pair['eligible_pairs']} ({rate})</td>"
                            f"<td>{pair['eligible_pairs']}/{pair['planned_pairs']}</td>"
                            f"<td>{duration} ({pair['both_pass']} pairs)</td></tr>")
    cells = []
    for row in data["paired_attempts"]:
        values = "".join(f"<td>{escape(value['status'])}"
                         f"{'' if value.get('eligible') else ' (excluded or not run)'}</td>"
                         for value in row["agents"].values())
        cells.append(f"<tr><td>{escape(row['task_id'])} / {row['attempt']}</td>{values}</tr>")
    headers = "".join(f"<th>{escape(agent['id'])}</th>" for agent in data["agents"])
    scope = ("Shared device UI tools from the Aiden tool layer or the neutral raw layer, as labeled per agent; "
             "native agent harnesses; original suite assertions and rubric scoring. "
             "Each attempt resets the MobileGym environment. Memory, planning and context remain agent-owned. "
             "This is a cold-start operation comparison; model and native harness settings may differ.")
    preparation = (f"{len(data['agent_preparation_tasks'])} tasks use independent agent-driven preparation. "
                   "Matching setup instructions do not guarantee identical starting states; inspect pre-screenshots."
                   if data["agent_preparation_tasks"] else "No agent-driven preparation is required by this suite.")
    return f"""<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Agent comparison</title><style>body{{font:16px system-ui;max-width:1100px;margin:40px auto;padding:0 20px;color:#202a35}}table{{border-collapse:collapse;width:100%;margin:24px 0}}th,td{{text-align:left;padding:12px;border-bottom:1px solid #ddd}}.scroll{{overflow:auto}}a{{color:#17654b}}pre{{white-space:pre-wrap;overflow-wrap:anywhere}}</style>
<a href="/experiments">Experiments</a><h1>{escape(manifest['id'])}</h1>
<p>Status: {escape(manifest['status'])}. {scope}</p>
<p>Aiden runs in Docker; external harnesses run on the host with fresh configuration and workspace and only the business tools. Coverage excludes infrastructure/evaluation errors. Missing cost is unknown; pi costs are model-pricing estimates.</p>
<p>{preparation}</p>
<h2>Matched comparison</h2><p>Compare the same task and repeat only when both attempts are eligible. Latency uses only pairs where both passed. Exclusions can bias the remaining sample; report coverage alongside success.</p>
<div class="scroll"><table><thead><tr><th>Pair</th><th>Agent</th><th>Matched success</th><th>Paired coverage</th><th>Both-pass median execution</th></tr></thead><tbody>{''.join(pairwise)}</tbody></table></div>
<h2>Individual run diagnostics</h2><p>These success rates may have different denominators. Execution errors can overlap timeouts and remain failures when execution already started; they do not establish an agent-caused failure. Native tool calls are harness tools outside the shared business catalog; flagged attempts referenced a local endpoint and should be reviewed in their native transcript. Model retries count reconnects or failed model requests that pi, Claude Code, or Codex reported; a timeout in such an attempt may reflect the network rather than the harness.</p>
<div class="scroll"><table><thead><tr><th>Agent run</th><th>Success</th><th>Coverage</th><th>Recorded</th><th>Median execution</th><th>Timeouts</th><th>Execution errors</th><th>Native tool calls</th><th>Attempts with model retries</th><th>Exclusions</th><th>Total USD</th></tr></thead><tbody>{''.join(summaries)}</tbody></table></div>
<h2>Paired attempts</h2><div class="scroll"><table><thead><tr><th>Task / repeat</th>{headers}</tr></thead><tbody>{''.join(cells)}</tbody></table></div>
<details><summary>Frozen experiment configuration</summary><pre>{escape(json.dumps(manifest, indent=2))}</pre></details></html>"""


def write_experiment_results(suite: Suite, root: Path, agents: list[dict],
                             rows: dict[str, list[TaskResult]], repeats: int, manifest: dict) -> None:
    for agent in agents:
        run_id = agent["id"]
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

    def save() -> None:
        # Callers hold the lock. Completion order varies with concurrency.
        for values in rows.values():
            values.sort(key=lambda row: (task_order.get(row.task_id, 0), row.attempt))
        write_experiment_results(suite, root, agents, rows, repeats, manifest)

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
            if task.platforms and "android" not in task.platforms:
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
            save()
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
        return result

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
