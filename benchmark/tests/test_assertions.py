import runner.assertions as assertions
from runner.assertions import evaluate_hard_assertions, AssertionOutcome
from runner.suite import HardAssertions, RequiredToolCallSpec
from runner.models import Trace, ToolCall

def make_trace(n: int, response: str = "ok") -> Trace:
    return Trace(
        tool_calls=[ToolCall(step=i+1, tool="x", input={}) for i in range(n)],
        final_response=response, total_tool_calls=n, total_duration_ms=0,
    )

def test_within_bounds_passes():
    spec = HardAssertions(min_tool_calls=1, max_tool_calls=10)
    out = evaluate_hard_assertions(make_trace(3), spec, timed_out=False)
    assert out.all_passed is True

def test_below_min_tool_calls_fails():
    spec = HardAssertions(min_tool_calls=2, max_tool_calls=10)
    out = evaluate_hard_assertions(make_trace(1), spec, timed_out=False)
    assert out.all_passed is False
    assert out.results.min_tool_calls is False
    assert out.failures[0].id == "min_tool_calls"
    assert out.failures[0].requirement == "Use at least 2 tool call(s)."
    assert out.failures[0].actual == "Used 1 tool call(s)."

def test_above_max_tool_calls_fails():
    spec = HardAssertions(min_tool_calls=1, max_tool_calls=2)
    out = evaluate_hard_assertions(make_trace(5), spec, timed_out=False)
    assert out.all_passed is False
    assert out.results.max_tool_calls is False

def test_timeout_fails():
    spec = HardAssertions(min_tool_calls=0, max_tool_calls=10)
    out = evaluate_hard_assertions(make_trace(3), spec, timed_out=True)
    assert out.all_passed is False
    assert out.results.timeout is False

def test_missing_response_fails_when_required():
    spec = HardAssertions(min_tool_calls=0, max_tool_calls=10, response_required=True)
    out = evaluate_hard_assertions(make_trace(1, response=""), spec, timed_out=False)
    assert out.all_passed is False
    assert out.results.response_exists is False

def test_required_tools_pass_when_all_present():
    trace = Trace(
        tool_calls=[
            ToolCall(step=1, tool="enter_plan_mode", input={}),
            ToolCall(step=2, tool="commit_plan", input={}),
            ToolCall(step=3, tool="shell", input={"command": "printf 1"}),
        ],
        final_response="ok",
        total_tool_calls=3,
        total_duration_ms=0,
    )
    spec = HardAssertions(required_tools=["enter_plan_mode", "commit_plan"])

    out = evaluate_hard_assertions(trace, spec, timed_out=False)

    assert out.all_passed is True
    assert out.results.required_tools is True

def test_required_tools_fail_when_missing():
    spec = HardAssertions(required_tools=["enter_plan_mode"])

    out = evaluate_hard_assertions(make_trace(1), spec, timed_out=False)

    assert out.all_passed is False
    assert out.results.required_tools is False
    assert out.failures[0].id == "required_tools"
    assert out.failures[0].requirement == "Must call: enter_plan_mode."
    assert out.failures[0].actual == "Missing: enter_plan_mode. Used: x."


def test_forbidden_tools_fail_when_present():
    trace = Trace(
        tool_calls=[ToolCall(step=1, tool="screenshot", input={})],
        final_response="ok",
        total_tool_calls=1,
        total_duration_ms=0,
    )
    spec = HardAssertions(forbidden_tools=["screenshot"])

    out = evaluate_hard_assertions(trace, spec, timed_out=False)

    assert out.all_passed is False
    assert out.results.forbidden_tools is False
    assert out.failures[0].id == "forbidden_tools"
    assert out.failures[0].requirement == "Must not call: screenshot."
    assert out.failures[0].actual == "Forbidden calls: screenshot at step 1. Used: screenshot."


def test_required_tool_calls_match_nested_input_subset():
    trace = Trace(
        tool_calls=[
            ToolCall(
                step=1,
                tool="enter_text",
                input={
                    "text": "+1 202-555-0147",
                    "platform": "ios",
                    "focus": {"x": 500, "y": 360},
                },
            )
        ],
        final_response="ok",
        total_tool_calls=1,
        total_duration_ms=0,
    )
    spec = HardAssertions(
        required_tool_calls=[
            RequiredToolCallSpec(
                tool="enter_text",
                input_contains={"text": "+1 202-555-0147", "focus": {"x": 500}},
            )
        ]
    )

    out = evaluate_hard_assertions(trace, spec, timed_out=False)

    assert out.all_passed is True
    assert out.results.required_tool_calls is True


def test_required_tool_calls_support_string_contains_matcher():
    trace = Trace(
        tool_calls=[
            ToolCall(
                step=1,
                tool="enter_text",
                input={"text": "Biden: +1 202-555-0147", "platform": "ios"},
            )
        ],
        final_response="ok",
        total_tool_calls=1,
        total_duration_ms=0,
    )
    spec = HardAssertions(
        required_tool_calls=[
            RequiredToolCallSpec(
                tool="enter_text",
                input_contains={
                    "text": {"$contains": "+1 202-555-0147"},
                    "platform": "ios",
                },
            )
        ]
    )

    out = evaluate_hard_assertions(trace, spec, timed_out=False)

    assert out.all_passed is True
    assert out.results.required_tool_calls is True


def test_required_tool_calls_support_list_contains_matcher():
    trace = Trace(
        tool_calls=[
            ToolCall(
                step=1,
                tool="recall_memory",
                input={"tags": ["办公城市", "杭州"], "types": ["fact"]},
            )
        ],
        final_response="ok",
        total_tool_calls=1,
        total_duration_ms=0,
    )
    spec = HardAssertions(
        required_tool_calls=[
            RequiredToolCallSpec(
                tool="recall_memory",
                input_contains={"tags": {"$contains": "办公城市"}},
            )
        ]
    )

    out = evaluate_hard_assertions(trace, spec, timed_out=False)

    assert out.all_passed is True
    assert out.results.required_tool_calls is True


def test_required_tool_calls_respect_order():
    trace = Trace(
        tool_calls=[
            ToolCall(step=1, tool="save_memory", input={"content": "我的办公城市是杭州"}),
            ToolCall(step=2, tool="save_memory", input={"content": "我的办公城市是深圳"}),
            ToolCall(step=3, tool="save_memory", input={"content": "我的办公城市是成都"}),
            ToolCall(step=4, tool="recall_memory", input={"tags": ["办公城市"]}),
        ],
        final_response="ok",
        total_tool_calls=4,
        total_duration_ms=0,
    )
    spec = HardAssertions(
        required_tool_calls=[
            RequiredToolCallSpec(tool="save_memory", input_contains={"content": {"$contains": "杭州"}}),
            RequiredToolCallSpec(tool="save_memory", input_contains={"content": {"$contains": "深圳"}}),
            RequiredToolCallSpec(tool="save_memory", input_contains={"content": {"$contains": "成都"}}),
            RequiredToolCallSpec(tool="recall_memory", input_contains={"tags": {"$contains": "办公城市"}}),
        ]
    )

    out = evaluate_hard_assertions(trace, spec, timed_out=False)

    assert out.all_passed is True
    assert out.results.required_tool_calls is True


def test_required_tool_calls_fail_when_out_of_order():
    trace = Trace(
        tool_calls=[
            ToolCall(step=1, tool="recall_memory", input={"tags": ["办公城市"]}),
            ToolCall(step=2, tool="save_memory", input={"content": "我的办公城市是杭州"}),
        ],
        final_response="ok",
        total_tool_calls=2,
        total_duration_ms=0,
    )
    spec = HardAssertions(
        required_tool_calls=[
            RequiredToolCallSpec(tool="save_memory", input_contains={"content": {"$contains": "杭州"}}),
            RequiredToolCallSpec(tool="recall_memory", input_contains={"tags": {"$contains": "办公城市"}}),
        ]
    )

    out = evaluate_hard_assertions(trace, spec, timed_out=False)

    assert out.all_passed is False
    assert out.results.required_tool_calls is False
    assert out.failures[0].id == "required_tool_calls"


def test_required_tool_calls_report_missing_input_match():
    trace = Trace(
        tool_calls=[ToolCall(step=1, tool="bridge_contacts", input={"action": "query", "query": "Alice"})],
        final_response="ok",
        total_tool_calls=1,
        total_duration_ms=0,
    )
    spec = HardAssertions(
        required_tool_calls=[
            RequiredToolCallSpec(
                tool="bridge_contacts",
                input_contains={"action": "query", "query": "Biden"},
            )
        ]
    )

    out = evaluate_hard_assertions(trace, spec, timed_out=False)

    assert out.all_passed is False
    assert out.results.required_tool_calls is False
    assert out.failures[0].id == "required_tool_calls"

def test_expected_option_answer_matches_tagged_final_answer():
    result = assertions.evaluate_expected_answer(
        "Reasoning text. <final_answer>(c)</final_answer>", "(C)", "option_letter"
    )

    assert result.passed is True
    assert result.predicted_answer == "(c)"

def test_expected_option_answer_fails_for_wrong_answer():
    result = assertions.evaluate_expected_answer("I choose (b).", "(c)", "option_letter")

    assert result.passed is False
    assert result.predicted_answer == "(b)"

def test_expected_option_answer_ignores_bare_article_after_parenthesized_choice():
    result = assertions.evaluate_expected_answer(
        "**(b) not achieved with a guard**", "(b)", "option_letter"
    )

    assert result.passed is True
    assert result.predicted_answer == "(b)"

def test_expected_option_answer_rejects_invalid_expected_answer():
    result = assertions.evaluate_expected_answer("No option selected.", "z", "option_letter")

    assert result.passed is False
    assert result.expected_answer is None

def test_expected_option_answer_rejects_ambiguous_predicted_answer():
    result = assertions.evaluate_expected_answer("I think (a), but maybe (c).", "(c)", "option_letter")

    assert result.passed is False
    assert result.predicted_answer is None

def test_expected_recalled_memory_ids_pass_when_tool_result_contains_id():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": '{"results":[{"id":"personamem_solo_travel"}]}',
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(history, ["personamem_solo_travel"])

    assert result.passed is True
    assert result.recalled_memory_ids == ["personamem_solo_travel"]
    assert result.evidence_source == "inline"
    assert result.recall_memory_called is True

def test_expected_recalled_memory_ids_fail_when_expected_id_absent():
    history = [
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": '{"results":[{"id":"personamem_campfire_storytelling"}]}',
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(history, ["personamem_solo_travel"])

    assert result.passed is False
    assert result.recalled_memory_ids == ["personamem_campfire_storytelling"]
    assert result.evidence_source == "inline"

def test_expected_recalled_memory_ids_ignores_non_object_json_payload():
    history = [
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": "[]",
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(history, ["personamem_solo_travel"])

    assert result.passed is None
    assert result.recalled_memory_ids == []
    assert result.evidence_source == "unavailable"


def test_expected_recalled_memory_ids_prefers_episode_over_compressed_history():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": "[Large tool result omitted from public history (8406 chars)]",
        },
    ]
    episode = {
        "id": "ep-1",
        "retrieved_memory_refs": ["personamem_music_expression"],
    }

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["personamem_music_expression"],
        episode=episode,
    )

    assert result.passed is True
    assert result.recalled_memory_ids == ["personamem_music_expression"]
    assert result.evidence_source == "episode"


def test_expected_recalled_memory_ids_does_not_use_episode_for_consolidation_gate():
    history = [
        {"type": "tool_call", "tool_name": "recall_device_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_device_memory",
            "content": "[Large tool result omitted from public history (8406 chars)]",
        },
    ]
    episode = {
        "id": "ep-consolidation",
        "retrieved_memory_refs": ["memory-created-by-consolidation"],
    }

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["memory-created-by-consolidation"],
        episode=episode,
        recall_tool="recall_device_memory",
        require_inline_recall=True,
    )

    assert result.passed is None
    assert result.recalled_memory_ids == []
    assert result.evidence_source == "unavailable"


def test_expected_recalled_memory_ids_episode_missing_expected_id_is_failure():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": "[Large tool result omitted from public history (5000 chars)]",
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["personamem_music_expression"],
        episode={"retrieved_memory_refs": ["personamem_music_software"]},
    )

    assert result.passed is False
    assert result.recalled_memory_ids == ["personamem_music_software"]
    assert result.evidence_source == "episode"


def test_expected_recalled_memory_ids_prefers_complete_inline_result_over_episode():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": '{"results":[{"id":"personamem_music_expression"}]}',
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["personamem_music_expression"],
        episode={"id": "ep-1"},
    )

    assert result.passed is True
    assert result.recalled_memory_ids == ["personamem_music_expression"]
    assert result.evidence_source == "inline"


def test_expected_recalled_memory_ids_deduplicates_multiple_recalls_and_uses_all_of():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": '{"results":[{"id":"memory-a"},{"id":"memory-a"}]}',
        },
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": '{"results":[{"id":"memory-b"},{"id":"memory-a"}]}',
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["memory-a", "memory-b"],
    )

    assert result.passed is True
    assert result.recalled_memory_ids == ["memory-a", "memory-b"]
    assert result.evidence_source == "inline"


def test_expected_recalled_memory_ids_does_not_use_final_answer_claim():
    history = [
        {"type": "assistant", "content": "I recalled personamem_music_expression."},
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["personamem_music_expression"],
        episode={"retrieved_memory_refs": ["personamem_music_expression"]},
    )

    assert result.passed is False
    assert result.recalled_memory_ids == []
    assert result.evidence_source == "unavailable"
    assert result.recall_memory_called is False


def test_expected_recalled_memory_ids_compressed_history_without_episode_is_unavailable():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": "[Large tool result omitted from public history (8406 chars)]",
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["personamem_music_expression"],
    )

    assert result.passed is None
    assert result.recalled_memory_ids == []
    assert result.evidence_source == "unavailable"
    assert result.recall_memory_called is True


def test_expected_recalled_memory_ids_does_not_attribute_device_recall_to_empty_memory_recall():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": '{"results":[]}',
        },
        {"type": "tool_call", "tool_name": "recall_device_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_device_memory",
            "content": '{"results":[{"id":"personamem_music_expression"}]}',
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["personamem_music_expression"],
        episode={"retrieved_memory_refs": ["personamem_music_expression"]},
    )

    assert result.passed is False
    assert result.recalled_memory_ids == []
    assert result.evidence_source == "inline"


def test_expected_recalled_memory_ids_does_not_attribute_device_recall_to_compressed_memory_recall():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": "[Large tool result omitted from public history (8406 chars)]",
        },
        {"type": "tool_call", "tool_name": "recall_device_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_device_memory",
            "content": '{"results":[{"id":"personamem_music_expression"}]}',
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["personamem_music_expression"],
        episode={"retrieved_memory_refs": ["personamem_music_expression"]},
    )

    assert result.passed is None
    assert result.recalled_memory_ids == []
    assert result.evidence_source == "unavailable"


def test_expected_recalled_memory_ids_attributes_episode_when_device_recall_is_empty():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": "[Large tool result omitted from public history (8406 chars)]",
        },
        {"type": "tool_call", "tool_name": "recall_device_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_device_memory",
            "content": '{"results":[]}',
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["personamem_music_expression"],
        episode={"retrieved_memory_refs": ["personamem_music_expression"]},
    )

    assert result.passed is True
    assert result.recalled_memory_ids == ["personamem_music_expression"]
    assert result.evidence_source == "episode"


def test_expected_recalled_memory_ids_attributes_episode_when_device_ids_are_unrelated():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": "[Large tool result omitted from public history (8406 chars)]",
        },
        {"type": "tool_call", "tool_name": "recall_device_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_device_memory",
            "content": '{"results":[{"id":"device-only"}]}',
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["personamem_music_expression"],
        episode={
            "retrieved_memory_refs": [
                "personamem_music_expression",
                "device-only",
            ]
        },
    )

    assert result.passed is True
    assert result.recalled_memory_ids == ["personamem_music_expression"]
    assert result.evidence_source == "episode"


def test_expected_recalled_memory_ids_keeps_episode_ambiguous_after_device_error():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": "[Large tool result omitted from public history (8406 chars)]",
        },
        {"type": "tool_call", "tool_name": "recall_device_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_device_memory",
            "content": "device memory unavailable",
            "is_error": True,
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["personamem_music_expression"],
        episode={"retrieved_memory_refs": ["personamem_music_expression"]},
    )

    assert result.passed is None
    assert result.recalled_memory_ids == []
    assert result.evidence_source == "unavailable"


def test_expected_recalled_memory_ids_uses_empty_aggregate_as_negative_episode_evidence():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": "[Large tool result omitted from public history (8406 chars)]",
        },
        {"type": "tool_call", "tool_name": "recall_device_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_device_memory",
            "content": '{"results":[]}',
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["personamem_music_expression"],
        episode={"retrieved_memory_refs": []},
    )

    assert result.passed is False
    assert result.recalled_memory_ids == []
    assert result.evidence_source == "episode"


def test_expected_recalled_memory_ids_uses_partial_aggregate_as_negative_episode_evidence():
    history = [
        {"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": "[Large tool result omitted from public history (8406 chars)]",
        },
        {"type": "tool_call", "tool_name": "recall_device_memory", "tool_input": "{}"},
        {
            "type": "tool_result",
            "tool_name": "recall_device_memory",
            "content": '{"results":[{"id":"memory-a"}]}',
        },
    ]

    result = assertions.evaluate_expected_recalled_memory_ids(
        history,
        ["memory-a", "memory-b"],
        episode={"retrieved_memory_refs": ["memory-a"]},
    )

    assert result.passed is False
    assert result.recalled_memory_ids == []
    assert result.evidence_source == "episode"


def test_null_device_recall_is_complete_negative_evidence():
    history = [
        {"type": "tool_call", "tool_name": "recall_device_memory", "tool_input": "{}"},
        {"type": "tool_result", "tool_name": "recall_device_memory", "content": '{"results":null}'},
    ]
    result = assertions.evaluate_expected_recalled_memory_ids(
        history, ["expected-procedure"], recall_tool="recall_device_memory",
        require_inline_recall=True, episode={"retrieved_memory_refs": ["expected-procedure"]},
    )
    assert result.passed is False
    assert result.evidence_source == "inline"
    assert result.recalled_memory_ids == []
