"""One frozen experiment, independent agent runs, paired attempt reporting."""
from __future__ import annotations

import dataclasses as dc
import hashlib
import html
import itertools
import json
import os
from pathlib import Path
import random
import re
import shutil
import statistics
import subprocess
import tempfile
import threading
import urllib.parse
from typing import Any
import uuid

from runner.harness import AidenAdapter, HarnessAdapter, OpenClawAdapter, PiAdapter
from runner.html_report import generate_report_html
from runner.judge import JudgeConfig, DEFAULT_JUDGE_API_KEY_ENV
from runner.models import HardAssertionFailure, HardAssertionResults, RubricVerdict, TaskResult
from runner.report import git_sha, now_iso, write_jsonl, write_manifest, write_metrics, write_summary
from runner.runtask import evaluate_task_history, skipped_task_result, _apply_environment_assertions
from runner.suite import Suite, load_suite


SAFE_ID = re.compile(r"[a-zA-Z0-9][a-zA-Z0-9_-]{0,79}\Z")
THINKING_LEVELS = {"off", "minimal", "low", "medium", "high", "xhigh", "max"}
PI_CUSTOM_FIELDS = {"base_url", "api", "api_key_env", "context_window", "max_tokens"}
PI_CUSTOM_APIS = {"openai-completions", "openai-responses", "anthropic-messages", "google-generative-ai"}
OPENCLAW_MIN_NODE = (24, 22, 1)


def openclaw_runtime() -> tuple[str, str, str]:
    """Resolve the installed CLI and a compatible Node without changing host PATH."""
    executable = shutil.which(os.environ.get("AIDEN_BENCHMARK_OPENCLAW_EXECUTABLE", "openclaw"))
    node = shutil.which(os.environ.get("AIDEN_BENCHMARK_OPENCLAW_NODE", "node"))
    if not executable or not node:
        raise ValueError("OpenClaw CLI and Node 24.22.1+ are required; set AIDEN_BENCHMARK_OPENCLAW_EXECUTABLE and AIDEN_BENCHMARK_OPENCLAW_NODE")
    version = subprocess.check_output([node, "--version"], text=True, timeout=10).strip()
    match = re.fullmatch(r"v(\d+)\.(\d+)\.(\d+)", version)
    version_tuple = tuple(map(int, match.groups())) if match else ()
    if not ((version_tuple[0] == 24 and version_tuple >= OPENCLAW_MIN_NODE)
            or version_tuple >= (26, 12, 1)):
        raise ValueError(f"OpenClaw requires Node 24.22.1+ or 26.12.1+; found {version}")
    cli_version = subprocess.check_output([node, executable, "--version"], text=True, timeout=20).strip()
    return executable, node, cli_version


def comparison_suite(path: Path) -> Suite:
    """Reuse original operation tasks and reject requirements we cannot preserve."""
    from runner.operations import OPERATION_TOOLS, preparation_steps
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
        if required - set(OPERATION_TOOLS):
            raise ValueError(f"{task.id}: required tools are not shared business tools: {sorted(required - set(OPERATION_TOOLS))}")
    return suite


def validate_agents(value: Any) -> list[dict[str, Any]]:
    if not isinstance(value, list) or not 1 <= len(value) <= 8:
        raise ValueError("agents must contain between 1 and 8 configurations")
    result = []
    ids = set()
    for item in value:
        if not isinstance(item, dict) or set(item) - ({"id", "kind", "provider", "model", "thinking"} | PI_CUSTOM_FIELDS):
            raise ValueError("Unknown agent configuration fields")
        agent = {key: str(val).strip() for key, val in item.items()}
        if agent.get("kind") not in {"aiden", "pi", "openclaw"}:
            raise ValueError("Supported agents: aiden, pi, openclaw")
        agent.setdefault("id", agent["kind"])
        if not SAFE_ID.fullmatch(agent["id"]) or agent["id"] in ids:
            raise ValueError("Agent IDs must be unique safe directory names")
        if agent["kind"] in {"pi", "openclaw"}:
            if not agent.get("provider") or not agent.get("model"):
                raise ValueError(f"{agent['kind']} requires an explicit provider and model")
            agent.setdefault("thinking", "medium")
            if agent["thinking"] not in THINKING_LEVELS:
                raise ValueError("Unsupported pi thinking level")
            if agent.get("base_url"):
                url = urllib.parse.urlsplit(agent["base_url"])
                try:
                    port = url.port
                except ValueError:
                    raise ValueError("pi base_url has an invalid port") from None
                if (url.scheme not in {"http", "https"} or not url.hostname or url.username is not None
                        or url.query or url.fragment or port == 0
                        or any(char.isspace() for char in agent["base_url"])):
                    raise ValueError("pi base_url must be an HTTP(S) base URL without credentials, query or fragment")
                agent["base_url"] = agent["base_url"].rstrip("/")
                agent.setdefault("api", "openai-completions")
                if agent["api"] not in PI_CUSTOM_APIS:
                    raise ValueError("Unsupported pi custom API protocol")
                agent.setdefault("api_key_env", "AIDEN_BENCHMARK_PI_API_KEY")
                if not re.fullmatch(r"[A-Za-z_][A-Za-z0-9_]*", agent["api_key_env"]):
                    raise ValueError("api_key_env must be an environment variable name, not a key or command")
                for name, default in (("context_window", 128000), ("max_tokens", 16384)):
                    number = item.get(name, default)
                    if type(number) is not int or number <= 0:
                        raise ValueError(f"pi {name} must be a positive integer")
                    agent[name] = number
                if agent["max_tokens"] > agent["context_window"]:
                    raise ValueError("pi max_tokens must not exceed context_window")
            elif set(item) & PI_CUSTOM_FIELDS:
                raise ValueError("pi custom endpoint fields require base_url")
            if agent["kind"] == "openclaw" and not agent.get("base_url"):
                raise ValueError("OpenClaw comparison requires an explicit custom API base_url")
        elif set(agent) - {"id", "kind"}:
            raise ValueError("Aiden uses the frozen Agent Config; set its model there")
        ids.add(agent["id"])
        result.append(agent)
    return result


def atomic_json(path: Path, value: Any) -> None:
    temporary = path.with_suffix(".tmp")
    temporary.write_text(json.dumps(value, ensure_ascii=False, indent=2), encoding="utf-8")
    temporary.replace(path)


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
            "id": agent["id"], "planned": planned, "completed": len(values),
            "eligible": len(eligible), "passed": passed,
            "success_rate": passed / len(eligible) if eligible else None,
            "coverage": len(eligible) / planned,
            "median_wall_ms": statistics.median(durations) if durations else None,
            "total_cost_usd": total_cost,
            "cost_per_success_usd": total_cost / passed if total_cost is not None and passed else None,
            "error_attempts": sum(row.metrics.get("agent_eligible") is not True for row in values),
            "timeouts": sum(row.status == "timeout" for row in values),
            "execution_errors": sum(bool(row.metrics.get("agent_error")) for row in values),
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
    preparation_tasks = [task.id for task in suite.tasks
                         if preparation_steps(suite.global_reset) or preparation_steps(task.setup)]
    return {"agents": summaries, "pairwise": pairwise, "paired_attempts": paired,
            "agent_preparation_tasks": preparation_tasks}


def comparison_html(manifest: dict, data: dict) -> str:
    escape = lambda value: html.escape(str(value))
    summaries = []
    for agent in data["agents"]:
        rate = f"{agent['success_rate']:.1%}" if agent["success_rate"] is not None else "—"
        duration = f"{agent['median_wall_ms'] / 1000:.2f}s" if agent["median_wall_ms"] is not None else "—"
        summaries.append(f"<tr><td><a href='runs/{escape(agent['id'])}/report.html'>{escape(agent['id'])}</a></td>"
                         f"<td>{agent['passed']}/{agent['eligible']} ({rate})</td>"
                         f"<td>{agent['eligible']}/{agent['planned']}</td><td>{agent['completed']}/{agent['planned']}</td>"
                         f"<td>{duration}</td><td>{agent['timeouts']}</td><td>{agent['execution_errors']}</td>"
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
    scope = ("Shared screen/input tools; native agent harnesses; original suite assertions and rubric scoring. "
             "Each attempt resets the MobileGym environment. Memory, planning and context remain agent-owned. "
             "This is a cold-start operation comparison; model and native harness settings may differ.")
    preparation = (f"{len(data['agent_preparation_tasks'])} tasks use independent agent-driven preparation. "
                   "Matching setup instructions do not guarantee identical starting states; inspect pre-screenshots."
                   if data["agent_preparation_tasks"] else "No agent-driven preparation is required by this suite.")
    return f"""<!doctype html><html lang="en"><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Agent comparison</title><style>body{{font:16px system-ui;max-width:1100px;margin:40px auto;padding:0 20px;color:#202a35}}table{{border-collapse:collapse;width:100%;margin:24px 0}}th,td{{text-align:left;padding:12px;border-bottom:1px solid #ddd}}.scroll{{overflow:auto}}a{{color:#17654b}}pre{{white-space:pre-wrap;overflow-wrap:anywhere}}</style>
<a href="/experiments">Experiments</a><h1>{escape(manifest['id'])}</h1>
<p>Status: {escape(manifest['status'])}. {scope}</p>
<p>Aiden runs in Docker; pi and OpenClaw run on the host with fresh configuration and workspace. Coverage excludes infrastructure/evaluation errors. Missing cost is unknown; pi costs are model-pricing estimates.</p>
<p>{preparation}</p>
<h2>Matched comparison</h2><p>Compare the same task and repeat only when both attempts are eligible. Latency uses only pairs where both passed. Exclusions can bias the remaining sample; report coverage alongside success.</p>
<div class="scroll"><table><thead><tr><th>Pair</th><th>Agent</th><th>Matched success</th><th>Paired coverage</th><th>Both-pass median execution</th></tr></thead><tbody>{''.join(pairwise)}</tbody></table></div>
<h2>Individual run diagnostics</h2><p>These success rates may have different denominators. Execution errors can overlap timeouts and remain failures when execution already started; they do not establish an agent-caused failure.</p>
<div class="scroll"><table><thead><tr><th>Agent run</th><th>Success</th><th>Coverage</th><th>Recorded</th><th>Median execution</th><th>Timeouts</th><th>Execution errors</th><th>Exclusions</th><th>Total USD</th></tr></thead><tbody>{''.join(summaries)}</tbody></table></div>
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
        write_manifest(directory / "manifest.tmp", run_manifest)
        (directory / "manifest.tmp").replace(directory / "manifest.json")
        write_jsonl(directory / "results.tmp", rows[run_id])
        (directory / "results.tmp").replace(directory / "results.jsonl")
        write_metrics(directory / "metrics.tmp", suite.name, run_manifest, rows[run_id])
        (directory / "metrics.tmp").replace(directory / "metrics.json")
        write_summary(directory / "summary.tmp", suite.name, run_manifest, rows[run_id])
        (directory / "summary.tmp").replace(directory / "summary.md")
        run_report = directory / "report.tmp"
        run_report.write_text(generate_report_html(directory), encoding="utf-8")
        run_report.replace(directory / "report.html")
    data = comparison(suite, agents, rows, repeats)
    manifest["completed"] = sum(len(items) for items in rows.values())
    atomic_json(root / "manifest.json", manifest)
    atomic_json(root / "comparison.json", data)
    report = root / "report.tmp"
    report.write_text(comparison_html(manifest, data), encoding="utf-8")
    report.replace(root / "report.html")


def _result_from_record(record: dict) -> TaskResult:
    row = dict(record)
    row["rubric"] = [RubricVerdict(**item) for item in row.get("rubric", [])]
    if isinstance(row.get("hard_assertions"), dict):
        row["hard_assertions"] = HardAssertionResults(**row["hard_assertions"])
    row["hard_assertion_failures"] = [HardAssertionFailure(**item)
                                      for item in row.get("hard_assertion_failures", [])]
    return TaskResult(**row)


def _retryable_judge_error(row: dict) -> bool:
    metrics = row.get("metrics") or {}
    return (row.get("status") == "judge_error" and row.get("rubric_total", 0) > 0
            and not row.get("hard_assertion_failures")
            and not any(value is False for value in (row.get("hard_assertions") or {}).values())
            and not metrics.get("agent_error") and metrics.get("first_failure_stage") != "setup")


def _has_judge_errors(root: Path, agents: list[dict]) -> bool:
    return any(
        any(_retryable_judge_error(json.loads(line)) for line in
            (root / "runs" / agent["id"] / "results.jsonl").read_text(encoding="utf-8").splitlines())
        for agent in agents
    )


def retry_experiment_judging(root: Path, judge_cfg: JudgeConfig, *,
                             api_key_env: str = DEFAULT_JUDGE_API_KEY_ENV) -> dict:
    """Re-score recorded judge errors and refresh both run and paired reports."""
    from runner.rejudge import rejudge_run

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


def run_experiment(suite: Suite, root: Path, agents: list[dict], adapters: dict[str, HarnessAdapter],
                   repeats: int, seed: int, stop: threading.Event, manifest: dict,
                   judge_cfg: JudgeConfig | None = None) -> dict:
    """Sequential paired blocks avoid sharing a mutable environment across agents."""
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
    manifest.update(status="running", comparison_policy="matched-eligible-v2",
                    schedule=[{"task_id": t.id, "attempt": n, "agent_id": a} for t, n, a in schedule])

    def save() -> None:
        write_experiment_results(suite, root, agents, rows, repeats, manifest)

    save()
    for task, attempt, agent_id in schedule:
        if stop.is_set():
            break
        manifest.update(stage="Executing task", current_attempt={"agent_id": agent_id, "task_id": task.id,
                                                                 "attempt": attempt, "started_at": now_iso()})
        atomic_json(root / "manifest.json", manifest)
        artifacts = root / "runs" / agent_id / "tasks" / task.id / f"attempt_{attempt}"
        artifacts.mkdir(parents=True, exist_ok=True)
        prompt = f"{suite.prompt_prefix}\n\n{task.prompt}".strip()
        try:
            adapter = adapters[agent_id]
            if task.platforms and "android" not in task.platforms:
                row = skipped_task_result(suite, task, attempt, artifacts, agent_id,
                                          f"Task requires {task.platforms}; environment platform is android")
                row.metrics.update(agent_eligible=False, failure_class="platform")
                rows[agent_id].append(row)
                save()
                continue
            execution = adapter.execute_operation(prompt, task.hard_assertions.must_complete_within_sec, artifacts, stop, task)
            digest = execution.metrics.get("business_tools_sha256")
            if not execution.canceled:
                if not digest:
                    raise ValueError("Operation attempt did not record its shared tool catalog")
                expected = manifest.setdefault("business_tools_sha256", digest)
                if digest != expected:
                    raise ValueError("Shared business tool catalog changed between attempts")
            manifest["stage"] = "Evaluating task"
            atomic_json(root / "manifest.json", manifest)
            row = evaluate_task_history(
                suite=suite, task=task, history=execution.history, episode=execution.episode,
                attempt=attempt, artifact_dir=artifacts, run_id=agent_id, timed_out=execution.timed_out,
                judge_cfg=judge_cfg, judge_cache_dir=root / "judge-cache", metrics=execution.metrics,
                pre_screenshot=execution.pre_screenshot, post_screenshot=execution.post_screenshot,
            )
            row.metrics.update(execution.metrics)
            if not execution.canceled:
                row = _apply_environment_assertions(row, task, execution.environment_state, execution.environment_state_error)
                from runner.trace import extract_trace
                from runner.operations import OPERATION_TOOLS
                row.metrics["business_tool_calls"] = sum(
                    call.tool in OPERATION_TOOLS for call in extract_trace(execution.history).tool_calls)
            if execution.canceled:
                row.status = "canceled"
                row.metrics.update(agent_eligible=False, success=None, failure_class="canceled")
        except Exception as exc:
            row = skipped_task_result(suite, task, attempt, artifacts, agent_id, str(exc))
            row.metrics.update(adapter_error=str(exc), agent_eligible=False, failure_class="adapter")
        rows[agent_id].append(row)
        save()
    manifest.pop("current_attempt", None)
    manifest["stage"] = "Finished"
    manifest["status"] = "canceled" if stop.is_set() else "completed"
    manifest["finished_at"] = now_iso()
    save()
    return manifest


class ExperimentManager:
    def __init__(self, *, runs_dir: Path, suites_dir: Path, base_config_dir: Path,
                 daemon_image: str, build_daemon_image: bool, config_loader, judge_loader=None):
        self.root = runs_dir / "experiments"
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
        allowed = {"suite", "agents", "repeats", "seed", "environment_url"}
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
                        "environment_url": environment_url,
                        "scoring": "original-suite-v1",
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
                    if agent["kind"] == "aiden":
                        agent.update(model=model.get("model", ""), provider=model.get("provider", ""),
                                     image_id=image_id, config_sha256=config_sha)
                        adapters[agent["id"]] = AidenAdapter(image=image_id, prepared_config=prepared)
                    elif agent["kind"] == "pi":
                        executable = shutil.which(os.environ.get("AIDEN_BENCHMARK_PI_EXECUTABLE", "pi"))
                        if not executable:
                            raise ValueError("pi executable not found; install pi or set AIDEN_BENCHMARK_PI_EXECUTABLE")
                        from runner.harness import pi_environment
                        env = pi_environment(executable, agent["provider"], Path(scratch) / "pi-probe")
                        version = subprocess.check_output([executable, "--version"], env=env, text=True, timeout=15).strip()
                        agent.update(version=version, executable=executable)
                        adapters[agent["id"]] = PiAdapter(
                            executable=executable, provider=agent["provider"], model=agent["model"],
                            thinking=agent["thinking"], **{key: agent[key] for key in PI_CUSTOM_FIELDS if key in agent},
                        )
                    else:
                        executable, node, version = openclaw_runtime()
                        agent.update(version=version, executable=executable, node_version=subprocess.check_output(
                            [node, "--version"], text=True).strip())
                        adapters[agent["id"]] = OpenClawAdapter(
                            executable=executable, node=node, provider=agent["provider"],
                            model=agent["model"], thinking=agent["thinking"],
                            **{key: agent[key] for key in PI_CUSTOM_FIELDS if key in agent},
                        )
                    native = adapters[agent["id"]]
                    adapters[agent["id"]] = OperationAdapter(
                        daemon=native if agent["kind"] == "aiden" else AidenAdapter(image=image_id, prepared_config=prepared),
                        environment_url=manifest["environment_url"],
                        pi=native if agent["kind"] == "pi" else None,
                        openclaw=native if agent["kind"] == "openclaw" else None,
                        global_reset=suite.global_reset, prompt_prefix=suite.prompt_prefix,
                    )
                if stop.is_set():
                    manifest["status"] = "canceled"
                    atomic_json(root / "manifest.json", manifest)
                    return
                run_experiment(suite, root, manifest["agents"], adapters, manifest["repeats"], manifest["seed"], stop, manifest, judge_cfg)
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
        assert manager._active is not None
        while manager._active[2].is_alive():
            manager._active[2].join(timeout=0.5)
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
