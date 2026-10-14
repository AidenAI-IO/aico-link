"""Read task-level benchmark evidence without inferring failure root causes."""

from __future__ import annotations

import json
from typing import Any

from runner.metrics import _status_fields


def _object(value: Any) -> dict[str, Any]:
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except (ValueError, TypeError):
            return {}
    return value if isinstance(value, dict) else {}


def _text(value: Any) -> str:
    if value is None:
        return ""
    return value if isinstance(value, str) else json.dumps(value, ensure_ascii=True)


def normalize_failure(input: Any, output: Any, *, trace_id: str | None = None,
                      trace_url: str | None = None) -> dict[str, Any] | None:
    """Keep failed/excluded attempts; successful attempts need no detail row."""
    input, output = _object(input), _object(output)
    success, failure_class, eligible = _status_fields(output)
    if success is True:
        return None
    metrics = _object(output.get("metrics"))
    evidence = []
    if metrics.get("error"):
        evidence.append({"kind": "Recorded error", "label": "",
                         "reason": _text(metrics["error"])})
    for check in output.get("hard_assertion_failures") or []:
        if isinstance(check, dict):
            evidence.append({"kind": "Hard assertion", "label": _text(check.get("label") or check.get("id")),
                             "reason": "", "expected": _text(check.get("requirement")),
                             "actual": _text(check.get("actual"))})
    for verdict in output.get("rubric") or []:
        if isinstance(verdict, dict) and verdict.get("verdict") == "no":
            evidence.append({"kind": "Rubric", "label": _text(verdict.get("id")),
                             "reason": _text(verdict.get("reason"))})
    for check in metrics.get("trace_observations") or []:
        if isinstance(check, dict) and check.get("passed") is False:
            evidence.append({"kind": "Trace check", "label": _text(check.get("description") or check.get("id")),
                             "reason": _text(check.get("reason"))})
    for key, label in (("expected_answer_match", "Expected answer"),
                       ("expected_recalled_memory_match", "Expected recalled memory")):
        if metrics.get(key) is False:
            evidence.append({"kind": "Match check", "label": label, "reason": "Did not match"})
    return {
        "task_id": _text(input.get("task_id")) or None,
        "attempt": input.get("attempt"),
        "status": output.get("status"), "agent_eligible": eligible,
        "failure_class": failure_class, "first_failure_stage": metrics.get("first_failure_stage"),
        "evidence": evidence, "final_response": _text(output.get("final_response")),
        "failure_event_ref": _text(metrics.get("failure_event_ref")),
        "trace_id": trace_id, "trace_url": trace_url,
    }


def _benchmark_output(value: Any) -> bool:
    value = _object(value)
    return bool(value.get("status")) and isinstance(value.get("metrics"), dict)


def fetch_failure_details(client: Any, experiment: Any) -> dict[str, Any]:
    """Inspect every item, following its exact evaluation observation or trace.

    Native execution attaches the evaluation to a child observation. Reading
    only the Agent trace root would lose the rubric and hard-assertion results.
    An inaccessible item never becomes a successful attempt or zero failures.
    """
    details, items, errors = [], {}, {}
    def unavailable(reason: str) -> None:
        errors[reason] = errors.get(reason, 0) + 1

    def read_error(exc: Exception) -> str:
        status = getattr(exc, "status_code", None)
        return type(exc).__name__ + (f" (HTTP {status})" if status else "")

    cursor, listing_complete = None, False
    try:
        while True:
            response = client.api.experiments.list_items(
                from_start_time=experiment.start_time, to_start_time=experiment.end_time,
                experiment_id=experiment.id, fields="core,dataset,io,itemMetadata",
                limit=100, cursor=cursor, request_options={"max_retries": 2})
            for item in response.data:
                items.setdefault(item.id, item)
            cursor = response.meta.cursor
            if not cursor:
                listing_complete = True
                break
    except Exception as exc:
        # Preserve inspected evidence from earlier pages and mark it partial.
        unavailable(f"Item listing: {read_error(exc)}")
    inspected = 0
    for item in items.values():
        try:
            input, output = item.input, item.output
            if not _benchmark_output(output):
                unavailable("Benchmark output unavailable")
                continue
            input = _object(input)
            if not input.get("task_id"):
                metadata = item.experiment_item_metadata or {}
                input = _object(metadata.get("input"))
            if not input.get("task_id"):
                unavailable("Task identity unavailable")
                continue
            trace_url = _object(output).get("aiden_trace_url")
            if not trace_url:
                get_url = getattr(client, "get_trace_url", None)
                if callable(get_url):
                    try:
                        trace_url = get_url(trace_id=item.trace_id)
                    except Exception:
                        pass
            detail = normalize_failure(input, output, trace_id=item.trace_id, trace_url=trace_url)
            if detail is not None:
                details.append(detail)
            inspected += 1
        except Exception as exc:
            unavailable(f"Attempt output: {read_error(exc)}")
            continue
    expected = experiment.item_count
    complete = listing_complete and inspected == len(items) == expected
    return {
        "failure_details": details,
        "failure_details_status": "available" if complete else "partial" if inspected else "unavailable",
        "failure_details_inspected": inspected, "failure_details_expected": expected,
        "failure_details_errors": errors,
    }
