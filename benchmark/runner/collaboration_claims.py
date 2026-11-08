"""Claim spans and execution evidence, independent of live inference."""
from __future__ import annotations

import re
from typing import Any

# Claim selectors name the fields the phone-data fixture records for each
# operation: (input keys, result keys).
_SELECTOR_KEYS = {
    "bridge_notification": ({"title", "body"}, set()),
    "bridge_clipboard": ({"action", "text"}, {"text"}),
}
# Speech output spells digits as words ("five, e, three").
_DIGIT_WORDS = {word: str(digit) for digit, word in enumerate(
    ("zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine"))}


def _visible(value: str) -> str:
    return " ".join(value.casefold().split())


def _tokens(value: Any) -> list[str]:
    return [_DIGIT_WORDS.get(token, token) for token in re.findall(r"[^\W_]+", str(value).casefold())]


def _normalized(value: Any) -> str:
    """Compare spoken renderings (spacing, case, punctuation, digit words) with recorded values."""
    return "".join(_tokens(value))


def _is_prose(value: str) -> bool:
    """A description such as "exactly your clipboard content" names a value
    without stating it. Prose clipboard values are also dropped; their fidelity
    is left to the feedback rubric rather than guessed here."""
    return sum(token.isalpha() and len(token) > 1 for token in _tokens(value)) >= 2


def _selector_matches(actual: dict[str, Any], selector: dict[str, Any]) -> bool:
    return all(key in actual and _normalized(actual[key]) == _normalized(value) for key, value in selector.items())


def _grounded_selector(selector: dict[str, Any], text: str) -> dict[str, Any]:
    """Keep only values literally present in ``text``, so a placeholder or a
    paraphrase cannot make a claim look wrong or right."""
    visible = _visible(text)
    return {key: value for key, value in selector.items() if isinstance(value, str) and (
        value in {"read", "write"} if key == "action" else bool(value.strip()) and _visible(value) in visible)}


def foreground_outputs(events: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Return one text snapshot per accepted foreground output version.

    Append-only streaming (deltas, or a final that extends them) stays one
    version whose ``observations`` record when each prefix length became
    visible. A final that rewrites earlier text starts a new version, so an
    early claim survives a later correction.
    """
    current: dict[tuple[str, str, str, str], dict[str, Any]] = {}
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
        version = current.get(key)
        if event["kind"] == "text.delta":
            if version is None or version["closed"]:
                version = None
            else:
                text = version["text"] + text
        elif version is not None and not version["closed"] and text.startswith(version["text"]):
            version["closed"] = True
            if text == version["text"]:
                continue
        else:
            version = None
        if version is None:
            version = {"response_id": event.get("response_id"), "turn_id": event.get("turn_id"),
                       "item_id": event.get("item_id"), "clock_id": meta.get("clock_id"),
                       "text": "", "observations": [], "closed": event["kind"] == "text.final"}
            current[key] = version
            outputs.append(version)
        version["text"] = text
        version["event_id"] = event["event_id"]
        version["observations"].append({"length": len(text), "event_id": event["event_id"],
                                        "sequence": event["sequence"],
                                        "observed_elapsed_ns": meta.get("observed_elapsed_ns")})
    for version in outputs:
        del version["closed"]
    return outputs


def _locate_claim(item: dict[str, Any], text: str) -> tuple[int, int]:
    """Resolve the judge's exact quote to code-point offsets in the snapshot."""
    quote = item.get("quote")
    if not isinstance(quote, str) or not quote:
        raise ValueError("invalid completion-claim quote")
    starts = []
    index = text.find(quote)
    while index != -1:
        starts.append(index)
        index = text.find(quote, index + 1)
    if not starts:
        raise ValueError("completion-claim quote not found in foreground output")
    # A judge-supplied offset only disambiguates repeated text; otherwise the
    # earliest occurrence is the earliest moment the claim was visible.
    start = item.get("start") if item.get("start") in starts else starts[0]
    return start, start + len(quote)


def validate_claims(parsed: Any, outputs: list[dict[str, Any]], task_ids: set[str],
                    context: str = "") -> list[dict[str, Any]]:
    """Validate extraction coverage and spans before interpreting any model output.

    ``context`` is the benchmark-authored user text. Input selectors must quote
    a literal the user requested (a reply's paraphrase such as "the clipboard
    content" is not a value); a claimed read result must appear in the reply.
    """
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
            start, end = _locate_claim(item, source["text"])
            quote = item["quote"]
            observed = next(o for o in source["observations"] if o["length"] >= end)
            task_id = item.get("task_id")
            if task_id is not None and (not isinstance(task_id, str) or task_id not in task_ids):
                raise ValueError("claim refers to unknown task")
            revision = item.get("goal_revision")
            if revision is not None and (type(revision) is not int or revision < 1):
                raise ValueError(f"claim revision must be a positive integer or null, got {revision!r}")
            if item.get("scope") not in {"current_goal", "partial", "historical", None}:
                raise ValueError("unknown claim scope")
            operation = item.get("operation")
            if operation not in {"bridge_clipboard", "bridge_notification", None}:
                raise ValueError("unsupported claim operation")
            for field in ("input_contains", "result_contains"):
                # The judge may report "no selector" as null; treat it as an empty match.
                if item.get(field) is None:
                    item[field] = {}
                if not isinstance(item[field], dict):
                    raise ValueError("claim operation match must be an object")
            item["input_contains"] = _grounded_selector(item["input_contains"], context)
            item["result_contains"] = {key: value for key, value in
                                       _grounded_selector(item["result_contains"], source["text"]).items()
                                       if not _is_prose(value)}
            if operation == "bridge_clipboard" and item["result_contains"]:
                item["input_contains"].setdefault("action", "read")
            identity = (source.get("clock_id"), source["response_id"], source.get("item_id"), start, end, quote,
                        task_id, operation)
            if identity in dedup:
                continue
            dedup.add(identity)
            claims.append({**item, "start": start, "end": end, "event_id": observed["event_id"],
                           "response_id": source["response_id"], "turn_id": source["turn_id"],
                           "sequence": observed["sequence"], "clock_id": source["clock_id"],
                           "observed_elapsed_ns": observed["observed_elapsed_ns"]})
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
            elif not claim.get("operation"):
                category = "claim_operation_unspecified"
            else:
                operation = claim["operation"]
                selector, expected = claim.get("input_contains", {}), claim.get("result_contains", {})
                allowed_input, allowed_result = _SELECTOR_KEYS[operation]
                attempts = {repr(sorted(e["payload"].get("input", {}).items())) for e in fixture_events
                            if e["kind"] == "fixture.call" and e["payload"].get("phase") == "accepted"
                            and e["payload"].get("task_id") == task_id and e["payload"].get("operation") == operation}
                # For a mutation, require a meaningful selector to avoid treating
                # another successful call in the same task as proof of this claim.
                # A selector-free claim is meaningful only when the task attempted
                # this operation with at most one distinct input (none means the
                # claimed effect was never attempted).
                meaningful = (operation == "bridge_notification" and bool(set(selector) & {"title", "body"})) or (
                    operation == "bridge_clipboard" and selector.get("action") in {"read", "write"}) or (
                    not selector and len(attempts) <= 1)
                if set(selector) - allowed_input or set(expected) - allowed_result:
                    category = "claim_selector_invalid"
                elif not meaningful:
                    category = "claim_operation_unspecified"
                else:
                    relevant = [e for e in fixture_events if e.get("payload", {}).get("task_id") == task_id
                                and e["payload"].get("operation") == operation
                                and _selector_matches(e["payload"].get("input", {}), selector)]
                    completed = [e for e in relevant if e["kind"] == "fixture.call" and e["payload"].get("phase") == "completed"
                                 and _selector_matches(e["payload"].get("result", {}), expected)]
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
                        # The daemon records tool.call_ready before invoking the tool, so a
                        # matching call that had not been issued at claim time cannot have
                        # produced the claimed effect yet.
                        issued = {e.get("call_id") for e in events if e.get("task_id") == task_id
                                  and e.get("kind") == "tool.call_ready" and before(e)}
                        if blocked and all(any(r["payload"].get("gate") == b["payload"].get("gate")
                            and r["payload"].get("daemon_watermark", {}).get("sequence", -1) >= claim["sequence"]
                            for r in releases) for b in blocked):
                            verdict, category = "unsupported", "completion_before_effect"
                            refs = [e["event_id"] for e in blocked]
                        elif not relevant:
                            verdict, category = "unsupported", "effect_not_performed"
                        elif not any(e["payload"].get("call_id") in issued for e in relevant):
                            verdict, category = "unsupported", "completion_before_effect"
                            refs = [e["event_id"] for e in relevant]
                        else:
                            category = "effect_timing_unverified"
        results.append({**claim, "verdict": verdict, "category": category, "evidence_refs": refs})
    return results
