from __future__ import annotations
import json
from typing import Any
from runner.models import ToolCall, Trace


def _safe_loads(s: str) -> Any:
    try:
        return json.loads(s) if s else {}
    except (json.JSONDecodeError, TypeError):
        return None


def tool_result_failed(message: dict[str, Any], content: Any = None) -> bool:
    """Return whether a history tool_result reports failure.

    Shared by trace matching and history metrics so both count the same results.
    """
    if message.get("is_error") or message.get("tool_error") is not None:
        return True
    if content is None:
        content = _safe_loads(message.get("content", ""))
    return isinstance(content, dict) and (bool(content.get("is_error")) or content.get("ok") is False)


def extract_trace(history: list[dict[str, Any]]) -> Trace:
    tool_calls: list[ToolCall] = []
    final_response = ""
    step = 0
    pending: dict[str, Any] | None = None
    for msg in history:
        mtype = msg.get("type")
        if mtype == "tool_call":
            if pending is not None:
                tool_calls.append(ToolCall(**pending))
            step += 1
            args = _safe_loads(msg.get("tool_input", "")) or {}
            if not isinstance(args, dict):
                args = {}
            pending = {"step": step, "tool": msg.get("tool_name", ""),
                       "input": args, "has_screenshot": False, "has_result": False}
        elif mtype == "tool_result" and pending is not None:
            raw = msg.get("content", "")
            content = _safe_loads(raw)
            pending["has_result"] = True
            pending["is_error"] = tool_result_failed(msg, content)
            if isinstance(content, dict) and content.get("data"):
                pending["has_screenshot"] = True
            elif isinstance(raw, str) and "screenshot observation" in raw.lower():
                # New agent history omits base64 image data and emits a text
                # summary instead (e.g. "... returned a screenshot observation ...").
                pending["has_screenshot"] = True
            tool_calls.append(ToolCall(**pending))
            pending = None
        elif mtype == "assistant":
            final_response = msg.get("content", "")
    if pending is not None:
        tool_calls.append(ToolCall(**pending))
    return Trace(
        tool_calls=tool_calls,
        final_response=final_response,
        total_tool_calls=len(tool_calls),
        total_duration_ms=0,
    )


def apply_episode_tool_evidence(trace: Trace, episode: dict[str, Any] | None) -> bool:
    """Apply episode-only tool evidence to the history-derived trace.

    Public history omits error status and records the model's raw arguments.
    The episode records each result's error status and, when the model uses
    pixel coordinates, the normalized arguments the device received. A result
    whose tool_error marks action_completed means the device action succeeded
    and only post-action verification failed, so the call still counts as
    executed. Results pair with calls in order; a count or name mismatch leaves
    the trace as-is.

    Returns whether the trace now carries episode evidence for every completed
    call. False means error status and device-facing arguments are unverified.
    """
    completed = [tc for tc in trace.tool_calls if tc.has_result]
    if not completed:
        return True
    events = episode.get("events") if isinstance(episode, dict) else None
    if not isinstance(events, list):
        return False
    results: list[tuple[dict[str, Any], dict[str, Any] | None]] = []
    mapping: tuple[str, dict[str, Any]] | None = None
    for event in events:
        if not isinstance(event, dict):
            continue
        if event.get("type") == "visual_coordinate_mapping":
            # Recorded during execution, before that call's result.
            metadata = event.get("metadata") if isinstance(event.get("metadata"), dict) else {}
            normalized = metadata.get("normalized_input")
            if isinstance(normalized, str):
                normalized = _safe_loads(normalized)
            mapping = (str(metadata.get("tool") or ""), normalized) if isinstance(normalized, dict) else None
        elif event.get("type") == "tool_result":
            tool_name = str(event.get("tool_name") or "")
            results.append((event, mapping[1] if mapping and mapping[0] == tool_name else None))
            mapping = None
    # Only calls whose result reached history can pair with episode results; a
    # trailing unfinished call must not discard evidence for completed ones.
    if len(results) != len(completed):
        return False
    if any(str(r.get("tool_name") or "") != tc.tool for (r, _), tc in zip(results, completed)):
        return False
    for (result, normalized), tool_call in zip(results, completed):
        tool_error = result.get("tool_error")
        details = tool_error.get("details") if isinstance(tool_error, dict) else None
        if isinstance(details, dict) and details.get("action_completed") is True:
            # The device action succeeded; only post-action verification
            # (stable-screen wait or screenshot) failed.
            tool_call.is_error = False
        elif result.get("is_error") or tool_error is not None:
            tool_call.is_error = True
        if normalized is not None:
            tool_call.input = normalized
    return True


def trace_has_skill_read(trace: Trace, skill_name: str) -> bool:
    target = skill_name.strip()
    if not target:
        return False
    for tc in trace.tool_calls:
        if tc.tool != "skill_read":
            continue
        inp = tc.input if isinstance(tc.input, dict) else {}
        name = str(inp.get("name") or inp.get("skill") or "").strip()
        if name == target:
            return True
    return False


def extract_step_screenshots(history: list[dict[str, Any]]) -> list[tuple[str, str]]:
    """Returns list of (tool_name, base64_jpeg) pairs from tool_result messages."""
    result: list[tuple[str, str]] = []
    last_tool_name = ""
    for msg in history:
        if msg.get("type") == "tool_call":
            last_tool_name = msg.get("tool_name", "")
        elif msg.get("type") == "tool_result":
            content = _safe_loads(msg.get("content", ""))
            if isinstance(content, dict):
                data = content.get("data")
                if data:
                    result.append((last_tool_name or msg.get("tool_name", ""), data))
    return result


def tool_result_evidence(history: list[dict[str, Any]]) -> list[dict[str, Any]]:
    """Keep execution outcomes for judging, without duplicating inline images."""
    results = []
    last_tool_name = ""
    for msg in history:
        if msg.get("type") == "tool_call":
            last_tool_name = msg.get("tool_name", "")
        if msg.get("type") != "tool_result":
            continue
        result = dict(msg)
        content = _safe_loads(msg.get("content", ""))
        if isinstance(content, dict) and isinstance(content.get("data"), str) and (
            "width" in content or "format" in content
            or (msg.get("tool_name") or last_tool_name) in {"screenshot", "touch_gesture"}
        ):
            content = {key: value for key, value in content.items() if key != "data"}
            result["content"] = json.dumps(content, ensure_ascii=False)
        results.append(result)
    return results


def human_handoff_requested(history: list[dict[str, Any]]) -> bool:
    for msg in history:
        if msg.get("type") != "tool_result" or msg.get("tool_name") != "request_user_action":
            continue
        content = _safe_loads(msg.get("content", ""))
        if isinstance(content, dict) and content.get("status") == "HUMAN_HANDOFF_REQUESTED":
            return True
    return False
