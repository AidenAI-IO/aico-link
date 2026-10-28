from __future__ import annotations

import json
from pathlib import Path
from typing import Any

from runner.judge import JudgeConfig
from runner.models import HardAssertionResults, TaskResult
from runner.runtask import evaluate_task_history
from runner.suite import Suite, TaskSpec
from skillopt.score import task_result_to_rollout
from skillopt.types import RolloutResult


class MobileGymResultError(RuntimeError):
    pass


def load_aico_suite_task_results(
    *,
    batch_dir: Path,
    suite: Suite,
    tasks: list[TaskSpec],
    run_id: str,
    phase_artifact_dir: Path,
    judge_cfg: JudgeConfig | None,
    judge_cache_dir: Path | None,
) -> list[TaskResult]:
    rows = _read_raw_rows(batch_dir)
    if not rows and _has_worker_failure(batch_dir):
        raise MobileGymResultError(f"MobileGym worker failed without task results: {batch_dir}")

    evidence = _read_task_evidence(batch_dir)
    by_full_id = {_row_task_id(row): row for row in rows if _row_task_id(row)}
    results: list[TaskResult] = []
    for task in tasks:
        full_id = f"{suite.name}.{task.id}"
        row = by_full_id.get(full_id)
        artifact_dir = phase_artifact_dir / task.id
        if row is None:
            results.append(_failed_task_result(
                suite=suite,
                task=task,
                run_id=run_id,
                artifact_dir=artifact_dir,
                status="failed",
                error="missing MobileGym result",
                mobilegym_status="missing",
            ))
            continue

        raw_status, raw_reason = _raw_status(row)
        if raw_status in {"failed", "error", "timeout", "unknown"}:
            results.append(_failed_task_result(
                suite=suite,
                task=task,
                run_id=run_id,
                artifact_dir=artifact_dir,
                status="timeout" if raw_status == "timeout" else "failed",
                error=raw_reason,
                mobilegym_status=raw_status,
            ))
            continue

        merged = {**evidence.get(full_id, {}), **row}
        metrics = {
            "mobilegym_result": row,
            "mobilegym_status": raw_status,
            "mobilegym_reason": raw_reason,
        }
        runtime_sec = _row_runtime_sec(row)
        if runtime_sec is not None:
            metrics["mobilegym_runtime_sec"] = runtime_sec
        timed_out = runtime_sec is not None and runtime_sec > task.hard_assertions.must_complete_within_sec

        history = merged.get("aico_last_chat_history")
        history_source = "aico_last_chat_history"
        if not isinstance(history, list):
            if _requires_full_aico_history(suite, task):
                history = None
            else:
                history = _history_from_execution_agent_message(merged)
                history_source = "mobilegym_execution_agent_message" if history else history_source
        if not isinstance(history, list):
            results.append(_failed_task_result(
                suite=suite,
                task=task,
                run_id=run_id,
                artifact_dir=artifact_dir,
                status="failed",
                error="missing aico_last_chat_history",
                mobilegym_status=raw_status,
            ))
            continue

        metrics["aico_history_source"] = history_source
        results.append(evaluate_task_history(
            suite=suite,
            task=task,
            history=history,
            attempt=1,
            artifact_dir=artifact_dir,
            judge_cfg=judge_cfg,
            judge_cache_dir=judge_cache_dir,
            run_id=run_id,
            timed_out=timed_out,
            metrics=metrics,
        ))
    return results


def load_aico_suite_rollouts(
    *,
    batch_dir: Path,
    suite: Suite,
    tasks: list[TaskSpec],
    run_id: str,
    phase_artifact_dir: Path,
    judge_cfg: JudgeConfig | None,
    judge_cache_dir: Path | None,
) -> list[RolloutResult]:
    return [
        task_result_to_rollout(result)
        for result in load_aico_suite_task_results(
            batch_dir=batch_dir,
            suite=suite,
            tasks=tasks,
            run_id=run_id,
            phase_artifact_dir=phase_artifact_dir,
            judge_cfg=judge_cfg,
            judge_cache_dir=judge_cache_dir,
        )
    ]


def _read_raw_rows(batch_dir: Path) -> list[dict[str, Any]]:
    by_id: dict[str, dict[str, Any]] = {}
    for path in sorted(batch_dir.glob("**/results.jsonl")):
        for row in _read_jsonl(path):
            task_id = _row_task_id(row)
            if task_id:
                by_id[task_id] = row
    for path in sorted(batch_dir.glob("**/errors.jsonl")):
        for row in _read_jsonl(path):
            task_id = _row_task_id(row)
            if task_id:
                row = dict(row)
                row["__mobilegym_source"] = "error"
                by_id[task_id] = row
    return list(by_id.values())


def _read_task_evidence(batch_dir: Path) -> dict[str, dict[str, Any]]:
    evidence: dict[str, dict[str, Any]] = {}
    for path in sorted(batch_dir.glob("**/trajectory/*/meta.json")):
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            continue
        if not isinstance(payload, dict):
            continue
        task_id = str(payload.get("task_id") or "")
        if task_id:
            evidence[task_id] = payload
    return evidence


def _read_jsonl(path: Path) -> list[dict[str, Any]]:
    try:
        lines = path.read_text(encoding="utf-8").splitlines()
    except OSError:
        return []
    rows: list[dict[str, Any]] = []
    for line in lines:
        if not line.strip():
            continue
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            continue
        if isinstance(row, dict):
            rows.append(row)
    return rows


def _history_from_execution_agent_message(row: dict[str, Any]) -> list[dict[str, Any]] | None:
    execution = row.get("execution")
    message = execution.get("agent_message") if isinstance(execution, dict) else None
    if message is None:
        message = row.get("agent_message") or row.get("aico_last_response")
    text = str(message or "").strip()
    if not text:
        return None
    return [{"type": "assistant", "content": text}]


def _requires_full_aico_history(suite: Suite, task: TaskSpec) -> bool:
    assertions = task.hard_assertions
    return bool(
        suite.trace_observations
        or assertions.min_tool_calls > 0
        or assertions.required_tools
        or assertions.forbidden_tools
        or task.expected_recalled_memory_ids
    )


def _has_worker_failure(batch_dir: Path) -> bool:
    for path in batch_dir.glob("**/shard.json"):
        try:
            payload = json.loads(path.read_text(encoding="utf-8"))
        except (OSError, json.JSONDecodeError):
            continue
        try:
            if int(payload.get("exit_code") or 0) != 0:
                return True
        except (TypeError, ValueError):
            return True
    return False


def _row_task_id(row: dict[str, Any]) -> str:
    for key in ("id", "task_id", "name"):
        if row.get(key):
            return str(row[key])
    return ""


def _raw_status(row: dict[str, Any]) -> tuple[str, str]:
    if row.get("__mobilegym_source") == "error":
        reason = _row_error(row) or "errors.jsonl"
        if _looks_like_timeout(reason):
            return "timeout", reason
        return "error", reason

    stop_reason = _stop_reason(row)
    if stop_reason.lower() in {"timeout", "overdue_termination"}:
        return "timeout", stop_reason
    if row.get("is_error") is True:
        return "error", _row_error(row) or "is_error"
    if row.get("is_success") is True:
        return "passed", "is_success"
    if row.get("is_success") is False:
        return "failed", "is_success false"
    if row.get("status") == "passed":
        return "passed", "status"
    if row.get("status") == "failed":
        return "failed", "status"
    if row.get("success") is True or row.get("passed") is True:
        return "passed", "fallback"
    if row.get("success") is False or row.get("passed") is False:
        return "failed", "fallback false"
    return "unknown", "unrecognized result"


def _stop_reason(row: dict[str, Any]) -> str:
    execution = row.get("execution")
    if isinstance(execution, dict) and execution.get("stop_reason"):
        return str(execution["stop_reason"])
    return str(row.get("stop_reason") or "")


def _row_runtime_sec(row: dict[str, Any]) -> float | None:
    execution = row.get("execution")
    candidates: list[Any] = []
    if isinstance(execution, dict):
        candidates.extend([execution.get("runtime_s"), execution.get("duration_s")])
    candidates.extend([row.get("runtime_s"), row.get("duration_s")])
    for value in candidates:
        if value is None:
            continue
        try:
            return float(value)
        except (TypeError, ValueError):
            continue
    return None


def _row_error(row: dict[str, Any]) -> str:
    execution = row.get("execution")
    if isinstance(execution, dict) and execution.get("error"):
        return str(execution["error"])
    return str(row.get("error") or row.get("message") or "")


def _looks_like_timeout(reason: str) -> bool:
    return "timeout" in reason.lower() or "overdue_termination" in reason.lower()


def _failed_task_result(
    *,
    suite: Suite,
    task: TaskSpec,
    run_id: str,
    artifact_dir: Path,
    status: str,
    error: str,
    mobilegym_status: str,
) -> TaskResult:
    artifact_dir.mkdir(parents=True, exist_ok=True)
    return TaskResult(
        suite=suite.name,
        run_id=run_id,
        task_id=task.id,
        category=task.category,
        attempt=1,
        status=status,
        rubric=[],
        rubric_pass_count=0,
        rubric_total=len(task.rubric),
        hard_assertions=HardAssertionResults(timeout=status != "timeout", response_exists=False),
        metrics={"error": error, "mobilegym_status": mobilegym_status},
        artifact_dir=str(artifact_dir),
        description_for_judge=task.description_for_judge,
        rubric_spec=[{"id": item.id, "check": item.check} for item in task.rubric],
    )
