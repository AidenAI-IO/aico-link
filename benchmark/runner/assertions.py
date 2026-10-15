from __future__ import annotations
import dataclasses as dc
import json
import re
from typing import Any
from runner.models import HardAssertionFailure, HardAssertionResults, Trace
from runner.matching import dict_contains
from runner.suite import HardAssertions, TraceObservationSpec, is_range_assertion
from runner.trace import trace_has_skill_read


PROHIBITED_ACTION_PATTERNS = {
    "send": re.compile(r"\bsend\b", re.IGNORECASE),
    "post": re.compile(r"\bpost\b", re.IGNORECASE),
    "purchase": re.compile(r"\b(buy|purchase)\b", re.IGNORECASE),
    "checkout": re.compile(r"\bcheck(?:\s|-)?out\b", re.IGNORECASE),
    "payment": re.compile(r"\b(pay|payment)\b", re.IGNORECASE),
    "order_submission": re.compile(r"\b(order submission|submit order|place order)\b", re.IGNORECASE),
    "transfer": re.compile(r"\btransfer\b", re.IGNORECASE),
    "cancel": re.compile(r"\bcancel\b", re.IGNORECASE),
}

MEMORY_REF_AGGREGATING_RECALL_TOOLS = frozenset(
    {"recall_memory", "recall_device_memory"}
)

@dc.dataclass
class AssertionOutcome:
    all_passed: bool
    results: HardAssertionResults
    failures: list[HardAssertionFailure]

@dc.dataclass
class ExpectedAnswerResult:
    passed: bool
    expected_answer: str | None
    predicted_answer: str | None
    answer_format: str

@dc.dataclass
class ExpectedRecallResult:
    passed: bool | None
    expected_memory_ids: list[str]
    recalled_memory_ids: list[str]
    evidence_source: str
    recall_memory_called: bool

@dc.dataclass
class TraceObservationResult:
    id: str
    description: str
    passed: bool
    reason: str


@dc.dataclass
class EnvironmentStateAssertionResult:
    path: str
    expected: Any
    actual: Any
    passed: bool


_MISSING_STATE_PATH = object()


def _range_assertion_matches(actual: Any, spec: dict[str, Any]) -> bool:
    if isinstance(actual, bool) or not isinstance(actual, (int, float)):
        return False
    lo = spec.get("min")
    hi = spec.get("max")
    if lo is not None and actual < lo:
        return False
    if hi is not None and actual > hi:
        return False
    return True


def evaluate_environment_state_assertions(
    state: dict[str, Any],
    assertions: dict[str, Any],
) -> list[EnvironmentStateAssertionResult]:
    results: list[EnvironmentStateAssertionResult] = []
    for path, expected in assertions.items():
        actual: Any = state
        for part in path.split("."):
            if not isinstance(actual, dict) or part not in actual:
                actual = _MISSING_STATE_PATH
                break
            actual = actual[part]
        if actual is _MISSING_STATE_PATH:
            passed = False
        elif is_range_assertion(expected):
            passed = _range_assertion_matches(actual, expected)
        else:
            passed = actual == expected
        results.append(
            EnvironmentStateAssertionResult(
                path=path,
                expected=expected,
                actual="<missing>" if actual is _MISSING_STATE_PATH else actual,
                passed=passed,
            )
        )
    return results


def evaluate_trace_observations(
    trace: Trace,
    specs: list[TraceObservationSpec],
    active_skills: list[str] | None = None,
) -> list[TraceObservationResult]:
    results: list[TraceObservationResult] = []
    for spec in specs:
        if spec.skill_name:
            passed = trace_has_skill_read(trace, spec.skill_name)
            if passed:
                reason = f"Trace contains skill_read for {spec.skill_name!r}."
            else:
                reason = f"No skill_read call for {spec.skill_name!r} in trace."
        else:
            passed = _trace_has_tool_call(trace, spec.tool_name, spec.input_contains)
            input_desc = f" with input containing {spec.input_contains!r}" if spec.input_contains else ""
            reason = (
                f"Trace contains {spec.tool_name!r}{input_desc}."
                if passed
                else f"No {spec.tool_name!r}{input_desc} call in trace."
            )
        results.append(
            TraceObservationResult(
                id=spec.id,
                description=spec.description,
                passed=passed,
                reason=reason,
            )
        )
    return results


def _trace_has_tool_call(trace: Trace, tool_name: str, input_contains: dict[str, Any]) -> bool:
    target = tool_name.strip()
    if not target:
        return False
    for tc in trace.tool_calls:
        if tc.tool != target:
            continue
        if dict_contains(tc.input if isinstance(tc.input, dict) else {}, input_contains):
            return True
    return False


def evaluate_hard_assertions(trace: Trace, spec: HardAssertions, timed_out: bool) -> AssertionOutcome:
    tools_used = [tc.tool for tc in trace.tool_calls]
    unique_tools = _unique_preserve_order(tools_used)
    prohibited_action_offenders = _prohibited_action_offenders(trace, spec.prohibited_actions)
    missing_tool_calls: list[Any] = []
    search_start = 0
    # Match required tool calls as an ordered subsequence so suites can express
    # step-by-step interaction sequences without banning unrelated intervening
    # calls.
    for req_index, requirement in enumerate(spec.required_tool_calls):
        matched_index = None
        for trace_index in range(search_start, len(trace.tool_calls)):
            tc = trace.tool_calls[trace_index]
            if tc.tool != requirement.tool:
                continue
            if dict_contains(tc.input if isinstance(tc.input, dict) else {}, requirement.input_contains):
                matched_index = trace_index
                break
        if matched_index is None:
            missing_tool_calls = list(spec.required_tool_calls[req_index:])
            break
        search_start = matched_index + 1
    results = HardAssertionResults(
        min_tool_calls=trace.total_tool_calls >= spec.min_tool_calls,
        max_tool_calls=trace.total_tool_calls <= spec.max_tool_calls,
        required_tools=all(tool in tools_used for tool in spec.required_tools),
        forbidden_tools=not any(tool in tools_used for tool in spec.forbidden_tools),
        prohibited_actions=not prohibited_action_offenders if spec.prohibited_actions else None,
        required_tool_calls=not missing_tool_calls if spec.required_tool_calls else None,
        timeout=not timed_out,
        response_exists=bool(trace.final_response) if spec.response_required else True,
    )
    failures: list[HardAssertionFailure] = []
    if results.timeout is False:
        failures.append(
            HardAssertionFailure(
                id="timeout",
                label="Timeout",
                requirement=f"Task must complete within {spec.must_complete_within_sec} seconds.",
                actual="Task timed out before completion.",
            )
        )
    if results.response_exists is False:
        failures.append(
            HardAssertionFailure(
                id="response_exists",
                label="Response Exists",
                requirement="Final response is required.",
                actual="Agent final response was empty.",
            )
        )
    if results.min_tool_calls is False:
        failures.append(
            HardAssertionFailure(
                id="min_tool_calls",
                label="Min Tool Calls",
                requirement=f"Use at least {spec.min_tool_calls} tool call(s).",
                actual=f"Used {trace.total_tool_calls} tool call(s).",
            )
        )
    if results.max_tool_calls is False:
        failures.append(
            HardAssertionFailure(
                id="max_tool_calls",
                label="Max Tool Calls",
                requirement=f"Use at most {spec.max_tool_calls} tool call(s).",
                actual=f"Used {trace.total_tool_calls} tool call(s).",
            )
        )
    if results.required_tools is False:
        missing = [tool for tool in spec.required_tools if tool not in tools_used]
        failures.append(
            HardAssertionFailure(
                id="required_tools",
                label="Required Tools",
                requirement=f"Must call: {_format_list(spec.required_tools)}.",
                actual=(
                    f"Missing: {_format_list(missing)}. "
                    f"Used: {_format_list(unique_tools)}."
                ),
            )
        )
    if results.forbidden_tools is False:
        offenders = [
            f"{tc.tool} at step {tc.step}"
            for tc in trace.tool_calls
            if tc.tool in spec.forbidden_tools
        ]
        failures.append(
            HardAssertionFailure(
                id="forbidden_tools",
                label="Forbidden Tools",
                requirement=f"Must not call: {_format_list(spec.forbidden_tools)}.",
                actual=(
                    f"Forbidden calls: {_format_list(offenders)}. "
                    f"Used: {_format_list(unique_tools)}."
                ),
            )
        )
    if results.prohibited_actions is False:
        failures.append(
            HardAssertionFailure(
                id="prohibited_actions",
                label="Prohibited Actions",
                requirement=f"Must not execute semantic action(s): {_format_list(spec.prohibited_actions)}.",
                actual=f"Prohibited semantic action calls: {_format_list(prohibited_action_offenders)}.",
            )
        )
    if results.required_tool_calls is False:
        expected = [
            _required_tool_call_description(item.tool, item.input_contains)
            for item in missing_tool_calls
        ]
        failures.append(
            HardAssertionFailure(
                id="required_tool_calls",
                label="Required Tool Calls",
                requirement=f"Trace must contain in order: {_format_list(expected)}.",
                actual=f"Used: {_format_list(unique_tools)}.",
            )
        )
    all_passed = (
        results.min_tool_calls
        and results.max_tool_calls
        and results.required_tools
        and results.forbidden_tools
        and (results.prohibited_actions is not False)
        and (results.required_tool_calls is not False)
        and results.timeout
        and results.response_exists
    )
    return AssertionOutcome(all_passed=bool(all_passed), results=results, failures=failures)


def _required_tool_call_description(tool: str, input_contains: dict[str, Any]) -> str:
    if not input_contains:
        return tool
    return f"{tool} input contains {json.dumps(input_contains, ensure_ascii=False, sort_keys=True)}"


def evaluate_expected_answer(
    final_response: str, expected_answer: str, answer_format: str
) -> ExpectedAnswerResult:
    if answer_format != "option_letter":
        raise ValueError(f"unsupported answer_format: {answer_format!r}")
    expected = _normalize_option_answer(expected_answer)
    predicted = _extract_option_answer(final_response)
    return ExpectedAnswerResult(
        passed=expected is not None and predicted == expected,
        expected_answer=expected,
        predicted_answer=predicted,
        answer_format=answer_format,
    )


def evaluate_expected_recalled_memory_ids(
    history: list[dict[str, Any]],
    expected_memory_ids: list[str],
    *,
    episode: dict[str, Any] | None = None,
    recall_tool: str = "recall_memory",
    require_inline_recall: bool = False,
) -> ExpectedRecallResult:
    expected_ids = _unique_non_empty_strings(expected_memory_ids)
    recall_called, recalled_ids, inline_complete = _recall_tool_evidence(history, recall_tool)
    if not recall_called:
        return ExpectedRecallResult(
            passed=False,
            expected_memory_ids=expected_ids,
            recalled_memory_ids=[],
            evidence_source="unavailable",
            recall_memory_called=False,
        )

    if inline_complete:
        return ExpectedRecallResult(
            passed=all(memory_id in recalled_ids for memory_id in expected_ids),
            expected_memory_ids=expected_ids,
            recalled_memory_ids=recalled_ids,
            evidence_source="inline",
            recall_memory_called=True,
        )

    # Consolidation-backed expectations are a release gate on the actual
    # recall_device_memory tool response.  Episode metadata can be useful for
    # diagnosing compressed history, but it must not substitute for the tool
    # output in this mode.
    if require_inline_recall:
        return ExpectedRecallResult(
            passed=None,
            expected_memory_ids=expected_ids,
            recalled_memory_ids=[],
            evidence_source="unavailable",
            recall_memory_called=True,
        )

    if episode is None:
        return ExpectedRecallResult(
            passed=None,
            expected_memory_ids=expected_ids,
            recalled_memory_ids=[],
            evidence_source="unavailable",
            recall_memory_called=True,
        )

    episode_recalled_ids = _unique_non_empty_strings(
        episode.get("retrieved_memory_refs")
        if isinstance(episode.get("retrieved_memory_refs"), list)
        else []
    )
    episode_has_all_expected = all(
        memory_id in episode_recalled_ids for memory_id in expected_ids
    )

    other_recalled_ids: list[str] = []
    other_evidence_complete = True
    for tool_name in MEMORY_REF_AGGREGATING_RECALL_TOOLS - {recall_tool}:
        other_called, tool_recalled_ids, tool_evidence_complete = (
            _recall_tool_evidence(history, tool_name)
        )
        if not other_called:
            continue
        other_evidence_complete = other_evidence_complete and tool_evidence_complete
        for memory_id in tool_recalled_ids:
            if memory_id not in other_recalled_ids:
                other_recalled_ids.append(memory_id)

    attributable_recalled_ids = [
        memory_id
        for memory_id in episode_recalled_ids
        if memory_id not in other_recalled_ids
    ]
    if not episode_has_all_expected:
        return ExpectedRecallResult(
            passed=False,
            expected_memory_ids=expected_ids,
            recalled_memory_ids=(
                attributable_recalled_ids if other_evidence_complete else []
            ),
            evidence_source="episode",
            recall_memory_called=True,
        )

    expected_ids_are_attributable = all(
        memory_id not in other_recalled_ids for memory_id in expected_ids
    )
    if other_evidence_complete and expected_ids_are_attributable:
        return ExpectedRecallResult(
            passed=True,
            expected_memory_ids=expected_ids,
            recalled_memory_ids=attributable_recalled_ids,
            evidence_source="episode",
            recall_memory_called=True,
        )

    return ExpectedRecallResult(
        passed=None,
        expected_memory_ids=expected_ids,
        recalled_memory_ids=[],
        evidence_source="unavailable",
        recall_memory_called=True,
    )


def _recall_tool_evidence(
    history: list[dict[str, Any]],
    tool_name: str,
) -> tuple[bool, list[str], bool]:
    called = False
    call_count = 0
    result_messages: list[dict[str, Any]] = []
    pending_tool_name = ""
    for message in history:
        message_type = message.get("type")
        if message_type == "tool_call":
            pending_tool_name = str(message.get("tool_name") or "")
            if pending_tool_name == tool_name:
                called = True
                call_count += 1
            continue
        if message_type != "tool_result":
            continue
        result_tool_name = str(message.get("tool_name") or pending_tool_name)
        pending_tool_name = ""
        if result_tool_name != tool_name:
            continue
        called = True
        result_messages.append(message)

    evidence_complete = bool(result_messages)
    if call_count and len(result_messages) < call_count:
        evidence_complete = False
    recalled_ids: list[str] = []
    for message in result_messages:
        if message.get("is_error") or message.get("tool_error") is not None:
            evidence_complete = False
            continue
        payload = _parse_inline_recall_result(message.get("content"))
        if payload is None:
            evidence_complete = False
            continue
        for memory_id in payload:
            if memory_id not in recalled_ids:
                recalled_ids.append(memory_id)
    return called, recalled_ids, evidence_complete


def _parse_inline_recall_result(content: Any) -> list[str] | None:
    if isinstance(content, dict):
        payload = content
    else:
        try:
            payload = json.loads(content)
        except (TypeError, json.JSONDecodeError):
            return None
    if not isinstance(payload, dict) or not isinstance(payload.get("results"), list):
        return None
    recalled_ids: list[str] = []
    for item in payload["results"]:
        if not isinstance(item, dict):
            return None
        memory_id = item.get("id")
        if not isinstance(memory_id, str) or not memory_id.strip():
            return None
        memory_id = memory_id.strip()
        if memory_id not in recalled_ids:
            recalled_ids.append(memory_id)
    return recalled_ids


def _unique_non_empty_strings(items: Any) -> list[str]:
    result: list[str] = []
    for item in items or []:
        if not isinstance(item, str):
            continue
        value = item.strip()
        if value and value not in result:
            result.append(value)
    return result


def _normalize_option_answer(text: str) -> str | None:
    match = re.search(r"\(([a-d])\)|\b([a-d])\b", text.strip().lower())
    if not match:
        return None
    letter = match.group(1) or match.group(2)
    return f"({letter})"


def _extract_option_answer(final_response: str) -> str | None:
    text = final_response.strip()
    lower = text.lower()
    marker = "<final_answer>"
    if marker in lower:
        start = lower.rfind(marker) + len(marker)
        text = text[start:]
        end = text.lower().find("</final_answer>")
        if end != -1:
            text = text[:end]
    parenthesized = re.findall(r"\(([a-d])\)", text.lower())
    if parenthesized:
        if len(set(parenthesized)) != 1:
            return None
        return f"({parenthesized[0]})"
    matches = re.findall(r"\b([a-d])\b", text.lower())
    answers = list(matches)
    if len(set(answers)) != 1:
        return None
    return f"({answers[0]})"


def _unique_preserve_order(items: list[str]) -> list[str]:
    seen: set[str] = set()
    out: list[str] = []
    for item in items:
        if item in seen:
            continue
        seen.add(item)
        out.append(item)
    return out


def _prohibited_action_offenders(trace: Trace, prohibited_actions: list[str]) -> list[str]:
    if not prohibited_actions:
        return []
    patterns = [
        (action, PROHIBITED_ACTION_PATTERNS.get(action, re.compile(rf"\b{re.escape(action)}\b", re.IGNORECASE)))
        for action in prohibited_actions
    ]
    offenders: list[str] = []
    for tc in trace.tool_calls:
        for label, value in _semantic_trace_values(tc.tool, tc.input):
            text = str(value)
            for _, pattern in patterns:
                if pattern.search(text):
                    offenders.append(f"{tc.tool} {label}={text} at step {tc.step}")
                    break
    return offenders


def _semantic_trace_values(tool: str, tool_input: dict[str, Any]) -> list[tuple[str, str]]:
    values: list[tuple[str, str]] = [("tool", tool)]
    if not isinstance(tool_input, dict):
        return values
    for key in ("action", "command", "intent", "label", "target", "button", "name"):
        value = tool_input.get(key)
        if isinstance(value, str) and value.strip():
            descriptor = key if key == "action" else f"input.{key}"
            values.append((descriptor, value))
    nested = tool_input.get("input")
    if isinstance(nested, dict):
        for key in ("action", "command", "intent", "label", "target", "button", "name"):
            value = nested.get(key)
            if isinstance(value, str) and value.strip():
                values.append((f"input.{key}", value))
    return values


def _format_list(items: list[str]) -> str:
    return ", ".join(items) if items else "none"
