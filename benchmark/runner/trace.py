from __future__ import annotations
import json
import re
import shlex
from collections.abc import Mapping
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


_TOOL_EXCHANGE_TYPES = {"tool_call", "tool_result"}


def _matches_tool_event(message: dict[str, Any], event: dict[str, Any]) -> bool:
    if message.get("type") != event.get("type") or message.get("tool_name") != event.get("tool_name"):
        return False
    if event.get("type") != "tool_call":
        return True
    history_input = message.get("tool_input", "")
    episode_input = event.get("tool_input", "")
    parsed_input = _safe_loads(history_input)
    return history_input == episode_input or (parsed_input is not None and parsed_input == _safe_loads(episode_input))


def tool_execution_history(
    history: list[dict[str, Any]], episode: dict[str, Any] | None,
) -> list[dict[str, Any]]:
    """Recover recorded executions missing from the public history snapshot."""
    events = episode.get("events") if isinstance(episode, dict) else None
    if not isinstance(events, list):
        return history
    events = [event for event in events if isinstance(event, dict)]
    last_fold = next((i for i in reversed(range(len(events))) if events[i].get("type") == "conact_fold"), None)
    positions = [i for i, message in enumerate(history) if message.get("type") in _TOOL_EXCHANGE_TYPES]
    if last_fold is None:
        recorded = [event for event in events if event.get("type") in _TOOL_EXCHANGE_TYPES]
        missing = len(recorded) - len(positions)
        # The recorder can finish one execution while the client takes its
        # history snapshot. Require an exact prefix and a complete final pair.
        if missing in (1, 2) and len(recorded) >= 2:
            call, result = recorded[-2:]
            if (call["type"] == "tool_call" and result["type"] == "tool_result"
                    and call.get("tool_name") and call["tool_name"] == result.get("tool_name")
                    and all(_matches_tool_event(history[i], event) for i, event in zip(positions, recorded))):
                return history + recorded[-missing:]
        return history

    before_fold = [event for event in events[:last_fold] if event.get("type") in _TOOL_EXCHANGE_TYPES]
    after_fold = [event for event in events[last_fold + 1:] if event.get("type") in _TOOL_EXCHANGE_TYPES]

    # The episode records completed executions. Keep a trailing unfinished call
    # from history, but only use history's completed exchanges when they align
    # with the episode suffix. This preserves surviving screenshot observations.
    pending = None
    if positions and history[positions[-1]].get("type") == "tool_call" and (
        not after_fold or after_fold[-1].get("type") == "tool_result"
    ):
        pending = positions.pop()
    if after_fold and len(positions) >= len(after_fold):
        suffix = positions[-len(after_fold):]
        if all(_matches_tool_event(history[i], event) for i, event in zip(suffix, after_fold)):
            return before_fold + history[suffix[0]:]

    # A fold marker is bookkeeping and does not represent a normal tool exchange.
    # Keep the assistant reply and rely on the episode for recorded executions.
    reply = [message for message in history if message.get("type") == "assistant"]
    # If the suffix could not be aligned, a pending history call could be one
    # of the executions already recorded in the episode.
    unfinished = [history[pending]] if not after_fold and pending is not None else []
    return before_fold + after_fold + unfinished + reply[-1:]


def extract_trace(history: list[dict[str, Any]]) -> Trace:
    tool_calls: list[ToolCall] = []
    final_response = ""
    step = 0
    pending: dict[str, Any] | None = None
    last_result: ToolCall | None = None
    for msg in history:
        mtype = msg.get("type")
        if mtype != "state":
            last_result = None
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
            last_result = tool_calls[-1]
            pending = None
        elif mtype == "state" and last_result is not None:
            # The daemon emits post-action images as state messages immediately
            # after the result. Initial/unrelated state images are not tool shots.
            caption = f"screenshot observation returned by the {last_result.tool} tool."
            attachments = msg.get("attachments") or []
            text = str(msg.get("content", ""))
            cli_caption = last_result.tool == "shell" and text.startswith(("Device CLI screenshot sheet.", "Device CLI screenshot."))
            if (caption in text or cli_caption) and any(
                isinstance(item, dict) and item.get("kind") == "image"
                for item in attachments
            ):
                last_result.has_screenshot = True
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


def episode_device_cli_calls(event: Mapping[str, Any], step: int = 0) -> list[ToolCall]:
    metadata = event.get("metadata")
    if event.get("type") != "tool_result" or event.get("tool_name") != "shell" or not isinstance(metadata, dict):
        return []
    calls = metadata.get("device_cli_calls")
    if not isinstance(calls, list):
        return []
    parsed = []
    for item in calls:
        if not (isinstance(item, dict) and isinstance(item.get("tool"), str)
                and isinstance(item.get("input"), dict) and isinstance(item.get("is_error"), bool)):
            continue
        duration = item.get("duration_ms")
        parsed.append(ToolCall(
            step=step, tool=item["tool"], input=item["input"],
            is_error=item["is_error"] or item.get("not_dispatched") is True,
            not_dispatched=item.get("not_dispatched") is True,
            duration_ms=duration if type(duration) is int and duration >= 0 else None,
        ))
    return parsed


# Only literal device-cli commands joined by && qualify. Never parse or
# exempt arbitrary shell programs, even if they dispatched a CLI call.
_CLI_LITERAL = r"(?:[a-zA-Z0-9_./,:+@%=-]+|'[^']*'|\"[^\"$`\\]*\")"
_CLI_COMMAND = re.compile(rf"device-cli(?:[ \t]+{_CLI_LITERAL})+")
_CLI_CHAIN = re.compile(rf"{_CLI_COMMAND.pattern}(?:[ \t]+&&[ \t]+{_CLI_COMMAND.pattern})*")
_APP_DATA_VERBS = {
    "clipboard": {"read", "write"}, "calendar": {"query", "create", "delete"},
    "contacts": {"query", "create", "update"}, "notification": {"send"},
}


def is_device_cli_shell(tc: ToolCall) -> bool:
    if tc.tool != "shell" or not isinstance(tc.input, dict):
        return False
    command = tc.input.get("command")
    return (bool(tc.device_cli_calls) and isinstance(command, str)
            and not tc.input.get("background") and "action" not in tc.input
            and _CLI_CHAIN.fullmatch(command.strip()) is not None)


_CLI_TOOL_NAMES = {
    **dict.fromkeys(("tap", "double-tap", "long-press", "swipe", "drag-start", "drag-release"), "touch_gesture"),
    **dict.fromkeys(("home", "back", "quick"), "quick_action"),
    **dict.fromkeys(("key", "enter"), "keyboard_tap"),
    "type": "enter_text", "open": "open_app", "url": "open_url", "look": "screenshot",
    "scroll": "mouse_scroll", "move": "mouse_move", "wait": "device_cli_wait",
}


def _app_flag_args(domain: str, action: str, args: list[str]) -> dict[str, Any] | None:
    allowed = {
        ("calendar", "query"): {"from", "to"},
        ("calendar", "create"): {"title", "start-at", "end-at", "all-day", "location", "notes", "alarm-minutes-before"},
        ("calendar", "delete"): {"event-id"},
        ("contacts", "query"): {"query", "limit"},
        ("contacts", "create"): {"name", "phone-number", "email", "organization", "notes"},
        ("contacts", "update"): {"contact-id", "name", "phone-number", "email", "organization", "notes"},
        ("notification", "send"): {"title", "body", "schedule-at", "sound", "badge"},
    }.get((domain, action), set())
    if not allowed:
        return None
    fields: dict[str, Any] = {}
    pos = 0
    while pos < len(args):
        flag, inline, value = args[pos].partition("=")
        pos += 1
        if not flag.startswith("--"):
            return None
        name = flag.removeprefix("--")
        if (domain, action) == ("calendar", "query"):
            name = {"start": "from", "end": "to"}.get(name, name)
        key = name.replace("-", "_")
        if name not in allowed or key in fields:
            return None
        if name in {"all-day", "sound"}:
            if inline and value not in {"true", "false"}:
                return None
            fields[key] = not inline or value == "true"
            continue
        if not inline:
            if pos == len(args) or args[pos].startswith("--"):
                return None
            value = args[pos]
            pos += 1
        if name in {"alarm-minutes-before", "limit", "badge"}:
            if not re.fullmatch(r"\+?[0-9]{1,19}", value):
                return None
            n = int(value)
            if name == "limit" and n == 0:
                return None
            fields[key] = n
        elif name in {"phone-number", "email"}:
            if not value.strip():
                return None
            key = "phone_numbers" if name == "phone-number" else "emails"
            fields.setdefault(key, []).append(value)
        else:
            fields[key] = value
    return fields


# Recognize simple shell diagnostics around CLI commands for argument recovery.
# This is deliberately separate from is_device_cli_shell's transport policy.
_SHELL_TOKEN = re.compile(rf"(?P<word>(?:{_CLI_LITERAL})+)|(?P<status>\"[^\"$`\\]*\$\?[^\"$`\\]*\")|(?P<op>&&|>>|[;|<>])")
_DIAGNOSTIC_COMMANDS = {"echo", "wc", "od", "head"}


def _literal_cli_commands(command: str) -> list[list[str]]:
    commands = []
    words: list[str] = []
    redirect = False
    piped = False
    pos = 0
    command = command.strip()
    while pos < len(command):
        if command[pos] in " \t":
            pos += 1
            continue
        token = _SHELL_TOKEN.match(command, pos)
        if token is None:
            return []
        pos = token.end()
        op = token.group("op")
        if op in {"<", ">", ">>"}:
            if not words or redirect:
                return []
            redirect = True
        elif op:
            if not words or redirect:
                return []
            if words[0] == "device-cli" and not piped:
                commands.append(words[1:])
            elif words[0] not in _DIAGNOSTIC_COMMANDS:
                return []
            words = []
            piped = op == "|"
        else:
            # Reject partially tokenized words, including shell expansions.
            if pos < len(command) and command[pos] not in " \t;|<>&":
                return []
            if token.group("status") and (not words or words[0] != "echo" or redirect):
                return []
            if redirect:
                redirect = False
            else:
                words.extend(shlex.split(token[0]))
    if redirect:
        return []
    if words and words[0] == "device-cli" and not piped:
        commands.append(words[1:])
    elif words and words[0] not in _DIAGNOSTIC_COMMANDS:
        return []
    return commands


def _restore_app_data_args(tc: ToolCall) -> None:
    # Match the recorded sequence before assigning any redacted inputs. Shell
    # text cannot establish execution or success, including a short-circuited tail.
    command = tc.input.get("command")
    if (tc.tool != "shell" or not isinstance(command, str) or tc.input.get("background")
            or "action" in tc.input or not any(not call.input for call in tc.device_cli_calls)):
        return
    parsed: list[tuple[str, dict[str, Any]]] = []
    for args in _literal_cli_commands(command):
        if not args:
            return
        if args[0] == "drag":
            parsed.extend([("touch_gesture", {}), ("touch_gesture", {})])
        elif args[:2] == ["app", "type"] and len(args) == 3:
            parsed.append(("enter_text", {"route": "app", "text": args[2]}))
        elif args[0] in _CLI_TOOL_NAMES:
            parsed.append((_CLI_TOOL_NAMES[args[0]], {}))
        elif (args[0] == "app" and len(args) >= 3 and args[1] in {"calendar", "contacts", "notification"}
              and (len(args) == 3 or args[3].startswith("--"))):
            fields = _app_flag_args(args[1], args[2], args[3:])
            if fields is None:
                return
            parsed.append(("bridge_" + args[1], {**fields, "action": args[2]}))
        elif args[0] == "app" and len(args) in (3, 4):
            domain, verb = args[1:3]
            if verb not in _APP_DATA_VERBS.get(domain, ()):
                return
            if domain == "clipboard":
                if len(args) != (3 if verb == "read" else 4):
                    return
                fields = {} if verb == "read" else {"text": args[3]}
            else:
                fields = _safe_loads(args[3]) if len(args) == 4 else None
                if not isinstance(fields, dict) or any(k.lower() == "action" for k in fields):
                    return
            parsed.append(("bridge_" + domain, {**fields, "action": verb}))
        else:
            return
    recorded = tc.device_cli_calls
    if [name for name, _ in parsed] != [call.tool for call in recorded]:
        return
    for (_, fields), call in zip(parsed, recorded):
        if not call.input:
            call.input = fields


def effective_tool_calls(trace: Trace) -> list[ToolCall]:
    """Keep outer calls for shell policy and expand recorded actions in order."""
    return [call for outer in trace.tool_calls for call in (outer, *outer.device_cli_calls)]


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
        tool_call.device_cli_calls = episode_device_cli_calls(result, tool_call.step)
        _restore_app_data_args(tool_call)
        if isinstance(result.get("screenshot_ref"), str) and result["screenshot_ref"].strip():
            tool_call.has_screenshot = True
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
