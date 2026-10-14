from __future__ import annotations
import json
from typing import Any
from runner.models import ToolCall, Trace


def _safe_loads(s: str) -> Any:
    try:
        return json.loads(s) if s else {}
    except (json.JSONDecodeError, TypeError):
        return None


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
                       "input": args, "has_screenshot": False}
        elif mtype == "tool_result" and pending is not None:
            raw = msg.get("content", "")
            content = _safe_loads(raw)
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
