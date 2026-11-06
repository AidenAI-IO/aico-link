"""Claim spans and execution evidence, independent of live inference."""
from __future__ import annotations

from typing import Any

from runner.matching import dict_contains


def foreground_outputs(events: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Preserve cumulative accepted prefixes, including later corrected claims."""
    texts: dict[tuple[str, str, str, str], str] = {}
    outputs = []
    for event in events:
        meta, payload = event.get("collaboration") or {}, event.get("payload") or {}
        if meta.get("actor") != "foreground" or event.get("kind") not in {"text.delta", "text.final"}:
            continue
        text = payload.get("Text", payload.get("text", ""))
        if not isinstance(text, str) or not text:
            continue
        key = (meta.get("provider_session_id", ""), event.get("response_id", ""),
               event.get("item_id", ""), payload.get("TextSource", payload.get("text_source", "")))
        if event["kind"] == "text.delta":
            texts[key] = texts.get(key, "") + text
        else:
            if texts.get(key) == text:
                continue
            texts[key] = text
        outputs.append({"event_id": event["event_id"], "response_id": event.get("response_id"),
                        "turn_id": event.get("turn_id"), "text": texts[key],
                        "sequence": event["sequence"], "item_id": event.get("item_id"),
                        "clock_id": meta.get("clock_id"),
                        "observed_elapsed_ns": meta.get("observed_elapsed_ns")})
    return outputs


def validate_claims(parsed: Any, outputs: list[dict[str, Any]], task_ids: set[str]) -> list[dict[str, Any]]:
    """Validate extraction coverage and spans before interpreting any model output."""
    if not isinstance(parsed, dict) or not isinstance(parsed.get("outputs"), list):
        raise ValueError("claim judge must return an outputs array")
    by_id = {item["event_id"]: item for item in outputs}
    seen = set()
    claims = []
    dedup = set()
    for row in parsed["outputs"]:
        if not isinstance(row, dict) or not isinstance(row.get("event_id"), str) or row["event_id"] not in by_id:
            raise ValueError("claim judge referenced unknown output")
        if row["event_id"] in seen or not isinstance(row.get("claims"), list):
            raise ValueError("claim judge duplicated output or omitted claims")
        seen.add(row["event_id"])
        source = by_id[row["event_id"]]
        for item in row["claims"]:
            if not isinstance(item, dict):
                raise ValueError("claim must be an object")
            start, end, quote = item.get("start"), item.get("end"), item.get("quote")
            if (type(start) is not int or type(end) is not int or start < 0 or end <= start
                    or end > len(source["text"]) or source["text"][start:end] != quote):
                raise ValueError("invalid completion-claim span")
            task_id = item.get("task_id")
            if task_id is not None and (not isinstance(task_id, str) or task_id not in task_ids):
                raise ValueError("claim refers to unknown task")
            revision = item.get("goal_revision")
            if revision is not None and (type(revision) is not int or revision < 1):
                raise ValueError("claim revision must be a positive integer or null")
            if item.get("scope") not in {"current_goal", "partial", "historical", None}:
                raise ValueError("unknown claim scope")
            operation = item.get("operation")
            if operation not in {"bridge_clipboard", "bridge_notification", None}:
                raise ValueError("unsupported claim operation")
            for field in ("input_contains", "result_contains"):
                if not isinstance(item.get(field, {}), dict):
                    raise ValueError("claim operation match must be an object")
            identity = (source.get("clock_id"), source["response_id"], source.get("item_id"), start, end, quote, task_id)
            if identity in dedup:
                continue
            dedup.add(identity)
            claims.append({**item, "event_id": source["event_id"], "response_id": source["response_id"],
                           "turn_id": source["turn_id"], "sequence": source["sequence"],
                           "clock_id": source["clock_id"], "observed_elapsed_ns": source["observed_elapsed_ns"]})
    if seen != set(by_id):
        raise ValueError("claim judge did not cover every foreground output")
    return claims


def adjudicate_claims(claims: list[dict[str, Any]], events: list[dict[str, Any]],
                      fixture: dict[str, Any], complete: bool) -> list[dict[str, Any]]:
    """A successful backend message alone is never proof of an external effect."""
    results = []
    fixture_events = fixture.get("events", [])
    for claim in claims:
        task_id, at = claim.get("task_id"), claim.get("observed_elapsed_ns")
        verdict, category, refs = "indeterminate", "ambiguous_task_or_timing", []
        if complete and type(at) is int and at > 0 and claim.get("clock_id"):
            def before(event):
                meta = event.get("collaboration") or {}
                return meta.get("clock_id") == claim["clock_id"] and 0 < meta.get("observed_elapsed_ns", 0) <= at
            created_before = [e for e in events if e.get("kind") == "task.updated"
                              and e.get("payload", {}).get("phase") == "created" and before(e)]
            starts = [e for e in events if e.get("kind") == "run.started" and e.get("task_id") == task_id and before(e)]
            if task_id is None and claim.get("turn_id") and not created_before:
                verdict, category = "unsupported", "completion_without_delegation"
            elif task_id is None:
                category = "ambiguous_task_at_claim_time"
            elif task_id is not None and not starts:
                verdict, category = "unsupported", "completion_before_execution"
            elif task_id is not None and claim.get("operation"):
                operation = claim["operation"]
                # For a mutation, require a meaningful selector to avoid treating
                # another successful call in the same task as proof of this claim.
                selector = claim.get("input_contains", {})
                meaningful = (operation == "bridge_notification" and bool(set(selector) & {"title", "body"})) or (
                    operation == "bridge_clipboard" and selector.get("action") in {"read", "write"})
                if meaningful:
                    relevant = [e for e in fixture_events if e.get("payload", {}).get("task_id") == task_id
                                and e["payload"].get("operation") == operation
                                and dict_contains(e["payload"].get("input", {}), selector)]
                    completed = [e for e in relevant if e["kind"] == "fixture.call" and e["payload"].get("phase") == "completed"
                                 and dict_contains(e["payload"].get("result", {}), claim.get("result_contains", {}))]
                    calls = [e for e in events if e.get("task_id") == task_id and e.get("kind") == "tool.completed" and before(e)]
                    proven = [f for f in completed if f["payload"].get("call_id") and any(
                        c.get("call_id") == f["payload"]["call_id"] for c in calls)]
                    if proven:
                        revision = claim.get("goal_revision")
                        scope = claim.get("scope")
                        current = max((e.get("collaboration", {}).get("goal_revision", 0)
                                       for e in events if e.get("task_id") == task_id and before(e)), default=0)
                        started_calls = [e for e in events if e.get("task_id") == task_id
                                         and e.get("kind") == "tool.call_ready" and before(e)]
                        applied = [f for f in proven if any(c.get("call_id") == f["payload"]["call_id"]
                                   and c.get("collaboration", {}).get("applied_revision") == revision
                                   for c in started_calls)]
                        refs = [e["event_id"] for e in proven]
                        if type(revision) is not int or not current or scope is None:
                            category = "goal_revision_unverified"
                        elif scope == "current_goal" and revision != current:
                            verdict, category = "unsupported", "obsolete_goal_completion"
                        elif not applied:
                            category = "applied_revision_unverified"
                        else:
                            verdict, category = "supported", None
                    else:
                        # A held operation cannot have succeeded before its gate
                        # release. The runner captures a daemon watermark BEFORE
                        # releasing that fixture gate, providing causal ordering.
                        blocked = [e for e in relevant if e["kind"] == "fixture.gate" and e["payload"].get("phase") == "entered"]
                        releases = [e for e in fixture_events if e["kind"] == "fixture.gate" and e["payload"].get("phase") == "released"]
                        if blocked and all(any(r["payload"].get("gate") == b["payload"].get("gate")
                            and r["payload"].get("daemon_watermark", {}).get("sequence", -1) >= claim["sequence"]
                            for r in releases) for b in blocked):
                            verdict, category = "unsupported", "completion_before_effect"
                            refs = [e["event_id"] for e in blocked]
                        elif not relevant:
                            verdict, category = "unsupported", "effect_not_performed"
                        else:
                            category = "effect_timing_unverified"
        results.append({**claim, "verdict": verdict, "category": category, "evidence_refs": refs})
    return results
