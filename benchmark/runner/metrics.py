from __future__ import annotations
import json
import math
import statistics
from collections import Counter
from collections.abc import Iterable, Mapping
from typing import Any
from runner.models import TaskResult


FAILURE_CLASSES = ("agent", "environment", "evaluation", "skipped", "unknown")

DEVICE_ACTION_TOOLS = {
    "touch_gesture", "enter_text", "keyboard_text", "keyboard_tap", "mouse_move",
    "mouse_scroll", "quick_action", "open_app", "launch_app",
    "open_url", "search_launch_app", "bridge_open_app", "bridge_clipboard",
    "bridge_contacts", "bridge_calendar", "bridge_notification", "bridge_media",
    "swipe", "tap", "long_press", "drag", "press_key",
}

FAILURE_STAGES = {
    "setup", "perception", "planning", "action_selection", "device_execution",
    "state_verification", "recovery", "final_response", "evaluation", "unknown",
}


def _usage_value(usage: Mapping[str, Any], *names: str) -> int | None:
    for name in names:
        value = usage.get(name)
        if isinstance(value, (int, float)) and not isinstance(value, bool):
            return int(value)
    return None


def _tool_input(value: Any) -> Mapping[str, Any]:
    if isinstance(value, Mapping):
        return value
    if not isinstance(value, str):
        return {}
    try:
        parsed = json.loads(value)
    except (TypeError, json.JSONDecodeError):
        return {}
    return parsed if isinstance(parsed, Mapping) else {}


def _is_device_action(tool_name: str, tool_input: Any) -> bool:
    if tool_name not in DEVICE_ACTION_TOOLS:
        return False
    parsed_input = _tool_input(tool_input)
    if tool_name == "quick_action" and parsed_input.get("list") is True:
        return False
    action = str(parsed_input.get("action") or "").lower()
    return not (
        tool_name.startswith("bridge_")
        and action in {"query", "read", "get", "inspect", "list", "search"}
    )


def _has_image_attachment(message: Mapping[str, Any]) -> bool:
    return any(
        isinstance(item, Mapping) and str(item.get("kind") or "").lower() == "image"
        for item in (message.get("attachments") or [])
    )


def _is_screenshot_result(
    message: Mapping[str, Any], parsed: Mapping[str, Any] | None, raw: str
) -> bool:
    if str(message.get("tool_name") or "") == "screenshot":
        return True
    if "screenshot observation" in raw.lower():
        return True
    return bool(
        isinstance(parsed, Mapping)
        and isinstance(parsed.get("data"), str)
        and (parsed.get("width") is not None or parsed.get("height") is not None)
    )


def derive_history_metrics(history: list[Mapping[str, Any]]) -> dict[str, Any]:
    """Extract metrics available from the public Agent history."""
    input_tokens = output_tokens = total_tokens = 0
    cached_tokens = reasoning_tokens = 0
    input_complete = output_complete = total_complete = True
    cached_complete = reasoning_complete = True
    usage_seen = False
    llm_calls = 0
    llm_durations: list[float] = []
    llm_duration_complete = True
    vision_llm_calls = 0
    vision_llm_durations: list[float] = []
    vision_duration_complete = True
    screenshot_since_model = False
    tool_errors = 0
    device_actions = 0
    last_tool_inputs: dict[str, Any] = {}
    first_error_ref = None
    first_error_stage = None
    for index, message in enumerate(history):
        message_type = str(message.get("type") or "")
        if _has_image_attachment(message):
            screenshot_since_model = True
        if message_type == "tool_result":
            raw = message.get("content")
            is_error = bool(message.get("is_error"))
            if isinstance(raw, str):
                try:
                    parsed = json.loads(raw)
                except (TypeError, json.JSONDecodeError):
                    parsed = None
                if isinstance(parsed, Mapping):
                    is_error = is_error or bool(parsed.get("is_error")) or parsed.get("ok") is False
                if _is_screenshot_result(message, parsed, raw):
                    screenshot_since_model = True
            tool_errors += int(is_error)
            if is_error and first_error_ref is None:
                tool_name = str(message.get("tool_name") or "")
                tool_input = message.get("tool_input", last_tool_inputs.get(tool_name))
                first_error_ref = f"history.json#/{index}"
                first_error_stage = (
                    "device_execution"
                    if _is_device_action(tool_name, tool_input)
                    else "unknown"
                )
        elif message_type == "tool_call":
            tool_name = str(message.get("tool_name") or "")
            last_tool_inputs[tool_name] = message.get("tool_input")
            if _is_device_action(tool_name, message.get("tool_input")):
                device_actions += 1
        if message_type in {"assistant", "tool_call"}:
            usage = message.get("usage")
            if isinstance(usage, Mapping):
                usage_seen = True
                llm_calls += 1
                is_vision_call = screenshot_since_model or _has_image_attachment(message)
                input_value = _usage_value(usage, "input_tokens", "prompt_tokens")
                output_value = _usage_value(usage, "output_tokens", "completion_tokens")
                total_value = _usage_value(usage, "total_tokens")
                if total_value is None and input_value is not None and output_value is not None:
                    total_value = input_value + output_value
                input_complete = input_complete and input_value is not None
                output_complete = output_complete and output_value is not None
                total_complete = total_complete and total_value is not None
                input_tokens += input_value or 0
                output_tokens += output_value or 0
                total_tokens += total_value or 0
                cached_value = _usage_value(usage, "cached_input_tokens", "cached_tokens")
                reasoning_value = _usage_value(usage, "reasoning_tokens")
                cached_complete = cached_complete and cached_value is not None
                reasoning_complete = reasoning_complete and reasoning_value is not None
                cached_tokens += cached_value or 0
                reasoning_tokens += reasoning_value or 0
                duration = message.get("duration_ms")
                if isinstance(duration, (int, float)) and duration >= 0:
                    llm_durations.append(float(duration))
                    if is_vision_call:
                        vision_llm_durations.append(float(duration))
                else:
                    llm_duration_complete = False
                    if is_vision_call:
                        vision_duration_complete = False
                if is_vision_call:
                    vision_llm_calls += 1
                screenshot_since_model = False
    metrics: dict[str, Any] = {
        "llm_calls": llm_calls if usage_seen else None,
        "input_tokens": input_tokens if usage_seen and input_complete else None,
        "output_tokens": output_tokens if usage_seen and output_complete else None,
        "total_tokens": total_tokens if usage_seen and total_complete else None,
        "cached_input_tokens": cached_tokens if usage_seen and cached_complete else None,
        "reasoning_tokens": reasoning_tokens if usage_seen and reasoning_complete else None,
        "device_actions": device_actions,
        "tool_errors": tool_errors,
        "retry_count": None,
        "replan_count": None,
        "vision_llm_calls": vision_llm_calls if usage_seen else None,
        "llm_time_ms": (
            sum(llm_durations) if llm_calls and llm_duration_complete else None
        ),
        "vision_llm_time_ms": (
            sum(vision_llm_durations)
            if (
                vision_llm_calls > 0
                and vision_duration_complete
                and len(vision_llm_durations) == vision_llm_calls
            )
            else None
        ),
    }
    if first_error_ref is not None:
        metrics["failure_event_ref"] = first_error_ref
        metrics["first_failure_stage"] = first_error_stage
    return metrics


def derive_episode_metrics(episode: Mapping[str, Any] | None) -> dict[str, Any]:
    if not isinstance(episode, Mapping):
        return {}
    extra = episode.get("extra") if isinstance(episode.get("extra"), Mapping) else {}
    metrics: dict[str, Any] = {}
    mappings = {
        "prompt_tokens": "input_tokens",
        "completion_tokens": "output_tokens",
        "total_tokens": "total_tokens",
        "cached_prompt_tokens": "cached_input_tokens",
        "reasoning_tokens": "reasoning_tokens",
        "first_token_time_ms": "time_to_first_token_ms",
        "llm_calls": "llm_calls",
        "llm_time_ms": "llm_time_ms",
    }
    for source, target in mappings.items():
        value = extra.get(source)
        if (isinstance(value, (int, float)) and not isinstance(value, bool)
                and math.isfinite(value) and value >= 0):
            metrics[target] = value
    # Episode totals cover calls absent from public history (e.g. compaction).
    # A missing/invalid cost is unknown, including when only some calls were priced.
    cost = extra.get("cost_usd")
    billed_calls = extra.get("cost_reported_calls")
    calls = metrics.get("llm_calls")
    complete_billing = billed_calls is None or (
        isinstance(billed_calls, (int, float)) and not isinstance(billed_calls, bool)
        and calls is not None and calls > 0 and float(calls).is_integer()
        and billed_calls == calls
    )
    if (isinstance(cost, (int, float)) and not isinstance(cost, bool)
            and math.isfinite(cost) and cost >= 0 and complete_billing):
        metrics["cost_usd"] = cost
        metrics["cost_source"] = extra.get("cost_source") or "episode_reported"
    events = episode.get("events") if isinstance(episode.get("events"), list) else []
    device_execution_ms = 0
    device_actions_seen = 0
    device_durations_seen = 0
    tool_errors = 0
    replan_count = 0
    replan_seen = False
    first_error_ref = None
    first_error_stage = None
    last_tool_inputs: dict[str, Any] = {}
    for index, event in enumerate(events):
        if not isinstance(event, Mapping):
            continue
        if "needs_replan" in event:
            replan_seen = True
            replan_count += int(bool(event.get("needs_replan")))
        tool_name = str(event.get("tool_name") or "")
        if event.get("type") == "tool_call":
            last_tool_inputs[tool_name] = event.get("tool_input")
        if event.get("type") != "tool_result":
            continue
        duration = event.get("duration_ms")
        tool_input = event.get("tool_input")
        if tool_input is None:
            tool_input = last_tool_inputs.get(tool_name)
        is_device_action = _is_device_action(tool_name, tool_input)
        if is_device_action:
            device_actions_seen += 1
            if isinstance(duration, (int, float)) and not isinstance(duration, bool):
                device_execution_ms += max(0, duration)
                device_durations_seen += 1
        if event.get("is_error"):
            tool_errors += 1
            if first_error_ref is None:
                first_error_ref = f"episode.json#/events/{index}"
                first_error_stage = "device_execution" if is_device_action else "unknown"
    metrics["device_execution_ms"] = (
        device_execution_ms
        if events and device_actions_seen == device_durations_seen
        else None
    )
    if events:
        metrics["tool_errors"] = tool_errors
    if replan_seen:
        metrics["replan_count"] = replan_count
    if first_error_ref is not None:
        metrics["failure_event_ref"] = first_error_ref
        metrics["first_failure_stage"] = first_error_stage
    return metrics


def _percentile(values: list[float], pct: float) -> float:
    if not values:
        return 0.0
    s = sorted(values)
    k = (len(s) - 1) * pct / 100
    f = int(k)
    c = min(f + 1, len(s) - 1)
    return s[f] + (s[c] - s[f]) * (k - f)


def _wilson(successes: int, trials: int, z: float = 1.959963984540054) -> dict[str, float | None]:
    if trials <= 0:
        return {"lower": None, "upper": None}
    p = successes / trials
    denominator = 1 + z * z / trials
    centre = (p + z * z / (2 * trials)) / denominator
    margin = z * ((p * (1 - p) / trials + z * z / (4 * trials * trials)) ** 0.5) / denominator
    return {"lower": max(0.0, centre - margin), "upper": min(1.0, centre + margin)}


def _status_fields(row: Mapping[str, Any]) -> tuple[bool | None, str | None, bool]:
    status = str(row.get("status") or "").strip().lower()
    metrics = row.get("metrics") if isinstance(row.get("metrics"), Mapping) else {}
    success = metrics.get("success")
    if not isinstance(success, bool):
        success = True if status == "passed" else False if status in {"failed", "timeout"} else None
    failure_class = metrics.get("failure_class")
    if failure_class not in FAILURE_CLASSES:
        failure_class = {
            "failed": "agent",
            "timeout": "agent",
            "judge_error": "evaluation",
            "skipped": "skipped",
        }.get(status)
    eligible = metrics.get("agent_eligible")
    if not isinstance(eligible, bool):
        eligible = status in {"passed", "failed", "timeout"}
        if failure_class in {"environment", "evaluation", "skipped"}:
            eligible = False
    return success, failure_class, eligible


def _metric_number(row: Mapping[str, Any], key: str) -> float | None:
    metrics = row.get("metrics") if isinstance(row.get("metrics"), Mapping) else {}
    value = metrics.get(key)
    return float(value) if isinstance(value, (int, float)) and not isinstance(value, bool) else None


def _task_metric(rows: list[Mapping[str, Any]], key: str) -> dict[str, float | int | None]:
    values = [value for row in rows if (value := _metric_number(row, key)) is not None]
    return {
        "count": len(values),
        "sum": sum(values) if values else None,
        "mean": (sum(values) / len(values)) if values else None,
        "p50": _percentile(values, 50) if values else None,
        "p90": _percentile(values, 90) if values else None,
        "p95": _percentile(values, 95) if values else None,
    }


def aggregate_rows(rows: list[Mapping[str, Any]], k: int = 1) -> dict[str, Any]:
    """Aggregate JSON result rows, including fixed-k task-level metrics."""
    k = max(1, int(k))
    ordered: dict[str, dict[int, Mapping[str, Any]]] = {}
    for row in rows:
        task_id = str(row.get("task_id") or "")
        try:
            attempt = int(row.get("attempt") or 1)
        except (TypeError, ValueError):
            attempt = 1
        ordered.setdefault(task_id, {})[attempt] = row

    all_attempts = list(rows)
    failure_classes: Counter[str] = Counter()
    failure_stages: Counter[str] = Counter()
    eligible_attempts = 0
    eligible_failures = 0
    for row in all_attempts:
        success, failure_class, eligible = _status_fields(row)
        if failure_class:
            failure_classes[failure_class] += 1
        if eligible and success is False:
            eligible_failures += 1
            metrics = row.get("metrics") if isinstance(row.get("metrics"), Mapping) else {}
            stage = str(metrics.get("first_failure_stage") or "unknown")
            if stage not in FAILURE_STAGES:
                stage = "unknown"
            failure_stages[stage] += 1
        if eligible:
            eligible_attempts += 1

    task_groups = list(ordered.values())
    eligible_groups: list[list[Mapping[str, Any]]] = []
    eligible_prefix_groups: list[list[Mapping[str, Any]]] = []
    first_attempt_groups: list[list[Mapping[str, Any]]] = []
    for attempts in task_groups:
        first = attempts.get(1)
        if first is not None:
            _, _, first_eligible = _status_fields(first)
            if first_eligible:
                first_attempt_groups.append([first])
        group = [attempts[index] for index in range(1, k + 1) if index in attempts]
        if len(group) == k and all(_status_fields(row)[2] for row in group):
            eligible_groups.append(group)
        prefix = []
        for index in range(1, max(attempts, default=0) + 1):
            row = attempts.get(index)
            if row is None or not _status_fields(row)[2]:
                break
            prefix.append(row)
        if prefix:
            eligible_prefix_groups.append(prefix)

    def task_metric(groups: list[list[Mapping[str, Any]]], predicate) -> dict[str, Any]:
        successes = sum(1 for group in groups if predicate(group))
        trials = len(groups)
        return {
            "successes": successes,
            "eligible_tasks": trials,
            "total_tasks": len(task_groups),
            "coverage": (trials / len(task_groups)) if task_groups else 0.0,
            "value": successes / trials if trials else None,
            "ci95": _wilson(successes, trials),
        }

    pass_at_k = task_metric(eligible_groups, lambda group: any(_status_fields(row)[0] is True for row in group))
    pass_pow_k = task_metric(eligible_groups, lambda group: all(_status_fields(row)[0] is True for row in group))
    pass_at_1 = task_metric(first_attempt_groups, lambda group: _status_fields(group[0])[0] is True)
    best_scores = []
    first_success_attempts: list[int] = []
    for group in eligible_groups:
        scores = [_metric_number(row, "quality_score") for row in group]
        scores = [score for score in scores if score is not None]
        if scores:
            best_scores.append(max(scores))
    for group in eligible_prefix_groups:
        for row in group:
            if _status_fields(row)[0] is True:
                try:
                    attempt_number = int(row.get("attempt") or 1)
                except (TypeError, ValueError):
                    attempt_number = 1
                first_success_attempts.append(attempt_number)
                break

    eligible_attempts_rows = [row for row in all_attempts if _status_fields(row)[2]]
    metrics = {
        key: _task_metric(all_attempts, key)
        if key == "setup_ms"
        else _task_metric(eligible_attempts_rows, key)
        for key in (
            "task_wall_ms", "wall_ms", "setup_ms", "llm_time_ms",
            "vision_llm_time_ms", "time_to_first_token_ms", "device_execution_ms",
            "screenshot_capture_ms", "tool_calls", "device_actions", "llm_calls",
            "vision_llm_calls", "tool_errors", "replan_count", "retry_count",
            "input_tokens", "output_tokens", "total_tokens", "cached_input_tokens",
            "reasoning_tokens", "cost_usd",
        )
    }
    attempt_successes = sum(
        1 for row in all_attempts if _status_fields(row)[2] and _status_fields(row)[0] is True
    )
    known_failure_stages = eligible_failures - failure_stages.get("unknown", 0)
    out: dict[str, Any] = {
        "unique_tasks": len(task_groups),
        "attempts": len(all_attempts),
        "metrics_k": k,
        "pass_at_1": pass_at_1,
        "pass_at_k": pass_at_k,
        "pass_pow_k": pass_pow_k,
        "oracle_best_score_at_k": {
            "value": (sum(best_scores) / len(best_scores)) if best_scores else None,
            "count": len(best_scores),
            "eligible_tasks": len(eligible_groups),
        },
        "agent_eligible_attempts": eligible_attempts,
        "failure_classes": dict(sorted(failure_classes.items())),
        "failure_stages": dict(sorted(failure_stages.items())),
        "failure_stage_coverage": {
            "classified": known_failure_stages,
            "eligible_failures": eligible_failures,
            "value": known_failure_stages / eligible_failures if eligible_failures else None,
        },
        "invalid_attempts": len(all_attempts) - eligible_attempts,
        "attempt_success_rate": {
            "successes": attempt_successes,
            "eligible_attempts": eligible_attempts,
            "total_attempts": len(all_attempts),
            "coverage": eligible_attempts / len(all_attempts) if all_attempts else 0.0,
            "value": attempt_successes / eligible_attempts if eligible_attempts else None,
            "ci95": _wilson(attempt_successes, eligible_attempts),
        },
        "first_success_attempt": {
            "count": len(first_success_attempts),
            "mean": (sum(first_success_attempts) / len(first_success_attempts)) if first_success_attempts else None,
            "p50": _percentile([float(value) for value in first_success_attempts], 50) if first_success_attempts else None,
        },
    }
    cost_to_first_success: dict[str, dict[str, float | int | None]] = {}
    for field in ("total_tokens", "cost_usd", "task_wall_ms"):
        values: list[float] = []
        for group in eligible_prefix_groups:
            cumulative = 0.0
            complete = True
            for row in group:
                value = _metric_number(row, field)
                if value is None:
                    complete = False
                else:
                    cumulative += value
                if _status_fields(row)[0] is True:
                    if complete:
                        values.append(cumulative)
                    break
        cost_to_first_success[field] = {
            "count": len(values),
            "p50": _percentile(values, 50) if values else None,
            "mean": (sum(values) / len(values)) if values else None,
        }
    out["cost_to_first_success"] = cost_to_first_success

    # Per-task metrics
    per_task: dict[str, dict[str, Any]] = {}
    for task_id, attempts_dict in ordered.items():
        attempts = [attempts_dict[i] for i in sorted(attempts_dict.keys())]
        eligible_attempts = [a for a in attempts if _status_fields(a)[2]]

        if not eligible_attempts:
            continue

        passed_count = sum(1 for a in eligible_attempts if _status_fields(a)[0] is True)
        total_attempts = len(eligible_attempts)

        # Quality scores
        quality_scores = [
            _metric_number(a, "quality_score")
            for a in eligible_attempts
            if _metric_number(a, "quality_score") is not None
        ]

        # Wall times
        wall_times = [
            _metric_number(a, "task_wall_ms")
            for a in eligible_attempts
            if _metric_number(a, "task_wall_ms") is not None
        ]

        per_task[task_id] = {
            "task_id": task_id,
            "total_attempts": len(attempts),
            "eligible_attempts": total_attempts,
            "passed": passed_count,
            "failed": total_attempts - passed_count,
            "pass_rate": passed_count / total_attempts if total_attempts > 0 else 0.0,
            "first_attempt_passed": _status_fields(eligible_attempts[0])[0] is True if eligible_attempts else None,
            "best_quality_score": max(quality_scores) if quality_scores else None,
            "avg_quality_score": (sum(quality_scores) / len(quality_scores)) if quality_scores else None,
            "avg_wall_ms": (sum(wall_times) / len(wall_times)) if wall_times else None,
        }

    out["per_task"] = per_task
    out.update(metrics)
    return out

def aggregate(results: list[TaskResult], k: int | None = None) -> dict[str, object]:
    if not results:
        out: dict[str, object] = {
            "tasks": 0,
            "passed": 0,
            "by_status": {},
            "by_category": {},
            "wall_ms_median": None,
            "wall_ms_p95": None,
            "tool_calls_median": None,
            "tool_calls_p95": None,
        }
        out.update(aggregate_rows([], k=k or 1))
        return out
    by_status: Counter[str] = Counter(r.status for r in results)
    by_category: dict[str, dict[str, int]] = {}
    for r in results:
        cat = by_category.setdefault(r.category, {"passed": 0, "total": 0,
                                                    "rubric_pass": 0, "rubric_total": 0})
        cat["total"] += 1
        if r.status == "passed":
            cat["passed"] += 1
        cat["rubric_pass"] += r.rubric_pass_count
        cat["rubric_total"] += r.rubric_total
    judge_eligible = [r for r in results if r.status not in {"judge_error", "skipped"}]
    pass_count = sum(1 for r in judge_eligible if r.status == "passed")
    walls = [r.metrics.get("wall_ms", 0) for r in results if r.metrics.get("wall_ms")]
    tool_counts = [r.metrics.get("tool_calls", 0) for r in results
                   if r.metrics.get("tool_calls") is not None]
    trace_observations = aggregate_trace_observation_metrics(r.metrics for r in results)
    out = {
        "tasks": len(results),
        "passed": pass_count,
        "by_status": dict(by_status),
        "by_category": by_category,
        "wall_ms_median": int(statistics.median(walls)) if walls else None,
        "wall_ms_p95": int(_percentile(walls, 95)) if walls else None,
        "tool_calls_median": int(statistics.median(tool_counts)) if tool_counts else None,
        "tool_calls_p95": int(_percentile(tool_counts, 95)) if tool_counts else None,
        "trace_observations": trace_observations,
    }
    row_metrics = aggregate_rows(
        [
            {
                "task_id": r.task_id,
                "attempt": r.attempt,
                "status": r.status,
                "metrics": r.metrics,
            }
            for r in results
        ],
        k=k or max((r.attempt for r in results), default=1),
    )
    out.update(row_metrics)
    out["wall_ms_median"] = row_metrics["wall_ms"]["p50"]
    out["wall_ms_p95"] = row_metrics["wall_ms"]["p95"]
    out["tool_calls_median"] = row_metrics["tool_calls"]["p50"]
    out["tool_calls_p95"] = row_metrics["tool_calls"]["p95"]
    if "skill_read_device_operator" in trace_observations:
        device_obs = trace_observations["skill_read_device_operator"]
        # Preserve the historical summary key while deriving it from generic buckets.
        out["skill_read_device_operator"] = {
            "tasks_with_skill_read": device_obs["tasks_with_observation"],
            "tasks_observed": device_obs["tasks_observed"],
        }
    return out

def aggregate_trace_observation_metrics(metrics_rows: Iterable[Mapping[str, Any]]) -> dict[str, dict[str, int]]:
    observed: dict[str, dict[str, int]] = {}
    for metrics in metrics_rows:
        seen_for_task: set[str] = set()
        passed_for_task: set[str] = set()
        for obs in metrics.get("trace_observations") or []:
            obs_id = str(obs.get("id") or "").strip()
            if not obs_id:
                continue
            seen_for_task.add(obs_id)
            if obs.get("passed"):
                passed_for_task.add(obs_id)
        for obs_id in seen_for_task:
            bucket = observed.setdefault(obs_id, {"tasks_with_observation": 0, "tasks_observed": 0})
            bucket["tasks_observed"] += 1
            if obs_id in passed_for_task:
                bucket["tasks_with_observation"] += 1
    return dict(sorted(observed.items()))
