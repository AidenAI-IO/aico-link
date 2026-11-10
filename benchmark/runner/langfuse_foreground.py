"""Rebuild the realtime foreground session of one benchmark attempt as a Langfuse trace.

The daemon exports each backend task run as an ``agent-run`` trace, but the
realtime foreground session (user turns, foreground replies and task tools,
result injection) is not exported. realtime-text attempts record it in
``collaboration-events.jsonl``; this module turns that evidence into OTLP spans
with their original timestamps. Spans are written through Langfuse's OTLP
endpoint, the same path the daemon uses, because the Python SDK cannot backdate
observations.
"""
from __future__ import annotations

import hashlib
import json
import re
import uuid
from collections.abc import Callable
from typing import Any

import httpx

from runner.collaboration_claims import response_key

TRACE_NAME = "realtime-foreground"
_OTLP_PATH = "/api/public/otel/v1/traces"


def foreground_trace_id(run_id: str, task_id: str, attempt: int) -> str:
    return uuid.uuid5(uuid.NAMESPACE_URL, f"aico-foreground:{run_id}:{task_id}:{attempt}").hex


def _timestamp_ns(value: Any) -> int | None:
    """Parse an RFC 3339 timestamp, keeping nanoseconds Python's parser drops."""
    match = re.fullmatch(r"(\d{4})-(\d\d)-(\d\d)T(\d\d):(\d\d):(\d\d)(?:\.(\d+))?Z", str(value or ""))
    if not match or match.group(1) == "0001":
        return None
    from datetime import datetime, timezone
    whole = datetime(*map(int, match.groups()[:6]), tzinfo=timezone.utc)
    return int(whole.timestamp()) * 1_000_000_000 + int((match.group(7) or "0")[:9].ljust(9, "0"))


def _json(value: Any) -> Any:
    if isinstance(value, str):
        try:
            return json.loads(value)
        except ValueError:
            return value
    return value


def build_foreground_spans(
    events: list[dict[str, Any]],
    *,
    trace_id: str,
    trace_context: dict[str, Any],
    evaluation: dict[str, Any] | None = None,
    foreground_model: str | None = None,
    backend_trace: Callable[[str], dict[str, Any]] | None = None,
) -> list[dict[str, Any]]:
    """Return Langfuse-shaped spans for one attempt's foreground session.

    ``backend_trace`` maps a backend episode ID to link metadata (trace ID and
    URL) so each backend run points at its exported ``agent-run`` trace.
    """
    events = sorted((e for e in events if _timestamp_ns(e.get("received_at")) is not None),
                    key=lambda e: e["sequence"])
    if not events:
        return []
    at = {e["event_id"]: _timestamp_ns(e["received_at"]) for e in events}
    first, last = at[events[0]["event_id"]], at[events[-1]["event_id"]]
    spans: dict[str, dict[str, Any]] = {}

    def span_id(key: str) -> str:
        return hashlib.sha256(f"{trace_id}:{key}".encode()).hexdigest()[:16]

    def add(key: str, *, name: str, kind: str, start: int, parent: str | None, **fields: Any) -> dict[str, Any]:
        span = {"span_id": span_id(key), "parent_id": parent, "name": name, "type": kind,
                "start": start, "end": fields.pop("end", start), "metadata": {}, **fields}
        spans[key] = span
        return span

    def actor(event: dict[str, Any]) -> str | None:
        return (event.get("collaboration") or {}).get("actor")

    root = add("root", name=TRACE_NAME, kind="agent", start=first, end=last, parent=None)
    root["metadata"].update({"foreground_model": foreground_model})
    turns: dict[str, dict[str, Any]] = {}
    responses: dict[str, dict[str, Any]] = {}
    # One response can contain several output items; keep them apart.
    texts: dict[str, dict[str, str]] = {}
    tools: dict[str, dict[str, Any]] = {}
    tasks: dict[str, dict[str, Any]] = {}
    runs: dict[str, dict[str, Any]] = {}
    for event in events:
        kind, payload, when = event["kind"], event.get("payload") or {}, at[event["event_id"]]
        meta = event.get("collaboration") or {}
        if actor(event) == "foreground":
            turn_id, response_id = event.get("turn_id"), response_key(event)
            if kind == "turn.committed" and turn_id:
                turns[turn_id] = add(f"turn:{turn_id}", name="user-turn", kind="span", start=when,
                                     end=last, parent=root["span_id"], input=payload.get("text"))
                root.setdefault("input", payload.get("text"))
            elif kind == "turn.settled" and turn_id in turns:
                turns[turn_id].update(end=when, output=payload.get("text") or payload.get("error"))
                turns[turn_id]["metadata"]["status"] = payload.get("status")
                if payload.get("status") == "failed":
                    turns[turn_id].update(level="ERROR", status_message=payload.get("error"))
            elif kind == "response.started" and response_id:
                parent = turns[turn_id]["span_id"] if turn_id in turns else root["span_id"]
                responses[response_id] = add(
                    f"response:{response_id}", name="foreground-response", kind="generation",
                    start=when, end=last, parent=parent, model=foreground_model)
                responses[response_id]["metadata"].update(
                    response_id=response_id, response_request_id=meta.get("response_request_id"),
                    result_update_ids=meta.get("update_ids"),
                    trigger="user_turn" if turn_id in turns else "task_result")
            elif kind in {"text.delta", "text.final"} and response_id in responses:
                text = payload.get("Text") or ""
                items = texts.setdefault(response_id, {})
                item = event.get("item_id") or ""
                items[item] = items.get(item, "") + text if kind == "text.delta" else text
                output = "\n\n".join(part for part in items.values() if part)
                responses[response_id]["output"] = output
                # The trace output is the last thing the foreground told the
                # user, which for delegated work is the result feedback.
                root["output"] = output
            elif kind == "response.terminal" and response_id in responses:
                usage = payload.get("Usage") or {}
                responses[response_id].update(end=when, usage={
                    "input": usage.get("InputTokens", 0), "output": usage.get("OutputTokens", 0),
                    "total": usage.get("TotalTokens", 0)} if usage else None)
            elif kind == "tool.call_ready":
                call_id = event.get("call_id") or payload.get("CallID")
                parent = responses[response_id]["span_id"] if response_id in responses else root["span_id"]
                tools[call_id] = add(f"tool:{call_id}", name=payload.get("Name") or "tool", kind="tool",
                                     start=when, parent=parent, input=_json(payload.get("Arguments")))
            elif kind == "tool.completed" and payload.get("call_id") in tools:
                tools[payload["call_id"]].update(end=when, output=_json(payload.get("output")))
            elif kind == "task.result_injected":
                add(f"inject:{event['event_id']}", name="task-result-injected", kind="event", start=when,
                    parent=root["span_id"], input=payload.get("updates"))
        task_id = event.get("task_id")
        if kind == "task.updated" and task_id:
            phase, task = payload.get("phase"), payload.get("task") or {}
            if task_id not in tasks:
                tasks[task_id] = add(f"task:{task_id}", name="backend-task", kind="chain", start=when,
                                     end=last, parent=root["span_id"], input=task.get("prompt"))
                tasks[task_id]["metadata"].update(task_id=task_id, episode_ids=[], backend_trace_ids=[])
            add(f"task-event:{event['event_id']}", name=f"task.{phase}", kind="event", start=when,
                parent=tasks[task_id]["span_id"], input=task.get("prompt") if phase == "goal_updated" else None,
                metadata={"status": payload.get("status"), "goal_revision": meta.get("goal_revision")})
            if phase == "terminal":
                tasks[task_id].update(end=when, output={key: task.get(key) for key in ("status", "result", "error")})
                if payload.get("status") == "failed":
                    tasks[task_id].update(level="ERROR", status_message=task.get("error"))
        elif kind == "run.started" and task_id in tasks:
            episode = payload.get("episode_id")
            link = backend_trace(episode) if backend_trace and episode else {}
            runs[event.get("run_id") or episode] = add(
                f"run:{event.get('run_id') or episode}", name="backend-run", kind="span", start=when, end=last,
                parent=tasks[task_id]["span_id"], metadata={"episode_id": episode, **link})
            tasks[task_id]["metadata"]["episode_ids"].append(episode)
            if link.get("backend_trace_id"):
                tasks[task_id]["metadata"]["backend_trace_ids"].append(link["backend_trace_id"])
        elif kind == "run.terminal" and (event.get("run_id") in runs):
            runs[event["run_id"]].update(end=when, output=payload)
            if payload.get("error"):
                runs[event["run_id"]].update(level="ERROR", status_message=payload.get("error"))
    for claim in (evaluation or {}).get("claims", []):
        when = at.get(claim.get("event_id"))
        if when is None:
            continue
        response = responses.get(claim.get("response_id"))
        add(f"claim:{claim['event_id']}:{claim.get('start')}:{claim.get('operation')}", name="completion-claim",
            kind="event", start=when, parent=(response or root)["span_id"], input=claim.get("quote"),
            output={"verdict": claim.get("verdict"), "category": claim.get("category")},
            level="WARNING" if claim.get("verdict") == "unsupported" else None,
            metadata={"task_id": claim.get("task_id"), "operation": claim.get("operation"),
                      "goal_revision": claim.get("goal_revision")})
    if evaluation:
        root["metadata"]["benchmark_outcome"] = evaluation.get("outcome")
    root["metadata"]["backend_trace_ids"] = [
        trace for task in tasks.values() for trace in task["metadata"]["backend_trace_ids"]]
    return [{**span, "trace_id": trace_id, "trace": trace_context} for span in spans.values()]


def _attribute(key: str, value: Any) -> dict[str, Any] | None:
    if value is None or value == "" or value == [] or value == {}:
        return None
    if isinstance(value, bool):
        return {"key": key, "value": {"boolValue": value}}
    if isinstance(value, int):
        return {"key": key, "value": {"intValue": str(value)}}
    if isinstance(value, list) and all(isinstance(item, str) for item in value):
        return {"key": key, "value": {"arrayValue": {"values": [{"stringValue": item} for item in value]}}}
    text = value if isinstance(value, str) else json.dumps(value, ensure_ascii=False)
    return {"key": key, "value": {"stringValue": text}}


def otlp_payload(spans: list[dict[str, Any]]) -> dict[str, Any]:
    """Encode spans with Langfuse's OTLP attribute conventions."""
    encoded = []
    for span in spans:
        trace = span["trace"]
        pairs = [("langfuse.trace.name", trace.get("name")), ("langfuse.session.id", trace.get("session_id")),
                 ("langfuse.environment", trace.get("environment")), ("langfuse.release", trace.get("release")),
                 ("langfuse.trace.tags", trace.get("tags"))]
        pairs += [(f"langfuse.trace.metadata.{k}", v) for k, v in (trace.get("metadata") or {}).items()]
        pairs += [("langfuse.observation.type", span["type"]), ("langfuse.observation.input", span.get("input")),
                  ("langfuse.observation.output", span.get("output")), ("langfuse.observation.level", span.get("level")),
                  ("langfuse.observation.status_message", span.get("status_message")),
                  ("langfuse.observation.model.name", span.get("model")),
                  ("langfuse.observation.usage_details", span.get("usage"))]
        pairs += [(f"langfuse.observation.metadata.{k}", v) for k, v in span["metadata"].items()]
        item = {"traceId": span["trace_id"], "spanId": span["span_id"], "name": span["name"], "kind": 1,
                "startTimeUnixNano": str(span["start"]), "endTimeUnixNano": str(max(span["end"], span["start"])),
                "attributes": [a for a in (_attribute(k, v) for k, v in pairs) if a]}
        if span.get("parent_id"):
            item["parentSpanId"] = span["parent_id"]
        if span.get("level") == "ERROR":
            item["status"] = {"code": 2, "message": span.get("status_message") or ""}
        encoded.append(item)
    return {"resourceSpans": [{
        "resource": {"attributes": [{"key": "service.name", "value": {"stringValue": "aico-benchmark"}}]},
        "scopeSpans": [{"scope": {"name": "aico.benchmark.realtime_foreground"}, "spans": encoded}]}]}


def otlp_exporter(public_key: str, secret_key: str, base_url: str) -> Callable[[list[dict[str, Any]]], None]:
    def export(spans: list[dict[str, Any]]) -> None:
        response = httpx.post(base_url.rstrip("/") + _OTLP_PATH, json=otlp_payload(spans),
                              auth=(public_key, secret_key), timeout=30,
                              headers={"x-langfuse-ingestion-version": "4"})
        response.raise_for_status()
        rejected = ((response.json() if response.content else {}).get("partialSuccess") or {}).get("rejectedSpans")
        if rejected and int(rejected) > 0:
            raise RuntimeError(f"Langfuse rejected {rejected} foreground span(s)")
    return export
