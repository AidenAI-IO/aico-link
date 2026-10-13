from runner.models import ToolCall, Trace
from runner.trace import extract_trace, extract_step_screenshots, trace_has_skill_read

HISTORY = [
    {"type": "user", "content": "请打开设置"},
    {"type": "tool_call", "tool_name": "screenshot", "tool_input": "{}"},
    {"type": "tool_result", "tool_name": "screenshot",
     "content": '{"width":1080,"height":1920,"format":"jpeg","size":4,"data":"AAAA"}'},
    {"type": "tool_call", "tool_name": "touch_gesture", "tool_input": '{"type":"tap","point":{"x":540,"y":1200}}'},
    {"type": "tool_result", "tool_name": "touch_gesture",
     "content": '{"width":1080,"height":1920,"format":"jpeg","size":4,"data":"BBBB","action_output":"ok"}'},
    {"type": "assistant", "content": "已打开。"},
]

def test_extract_trace_collects_tool_calls_in_order():
    trace = extract_trace(HISTORY)
    assert trace.total_tool_calls == 2
    assert trace.tool_calls[0].tool == "screenshot"
    assert trace.tool_calls[1].tool == "touch_gesture"
    assert trace.tool_calls[1].input == {"type": "tap", "point": {"x": 540, "y": 1200}}
    assert trace.final_response == "已打开。"

def test_extract_trace_marks_has_screenshot_when_data_present():
    trace = extract_trace(HISTORY)
    assert trace.tool_calls[0].has_screenshot is True
    assert trace.tool_calls[1].has_screenshot is True


def test_extract_trace_detects_case_insensitive_screenshot_observation_text():
    history = [
        {"type": "tool_call", "tool_name": "touch_gesture", "tool_input": "{}"},
        {"type": "tool_result", "tool_name": "touch_gesture", "content": "Returned a Screenshot Observation after settling."},
    ]

    trace = extract_trace(history)

    assert trace.tool_calls[0].has_screenshot is True

def test_extract_step_screenshots_returns_base64_payloads():
    shots = extract_step_screenshots(HISTORY)
    assert len(shots) == 2
    assert shots[0] == ("screenshot", "AAAA")
    assert shots[1] == ("touch_gesture", "BBBB")

def test_extract_trace_handles_malformed_input_gracefully():
    history = [
        {"type": "tool_call", "tool_name": "screenshot", "tool_input": "not-json"},
        {"type": "tool_result", "tool_name": "screenshot", "content": "also-not-json"},
        {"type": "assistant", "content": ""},
    ]
    trace = extract_trace(history)
    assert trace.total_tool_calls == 1
    assert trace.tool_calls[0].input == {}
    assert trace.tool_calls[0].has_screenshot is False

def test_extract_trace_keeps_tool_call_without_result_before_next_call():
    history = [
        {"type": "tool_call", "tool_name": "enter_plan_mode", "tool_input": '{"reason":"complex"}'},
        {"type": "tool_call", "tool_name": "commit_plan", "tool_input": '{"plan":["step"]}'},
        {"type": "tool_result", "tool_name": "commit_plan", "content": '{"status":"committed"}'},
        {"type": "assistant", "content": "done"},
    ]

    trace = extract_trace(history)

    assert [tc.tool for tc in trace.tool_calls] == ["enter_plan_mode", "commit_plan"]
    assert trace.tool_calls[0].input == {"reason": "complex"}
    assert trace.total_tool_calls == 2


def test_trace_has_skill_read_detects_matching_tool_call():
    trace = Trace(
        tool_calls=[
            ToolCall(step=1, tool="skill_list", input={}),
            ToolCall(step=2, tool="skill_read", input={"name": "device-operator"}),
        ],
        final_response="ok",
        total_tool_calls=2,
        total_duration_ms=0,
    )

    assert trace_has_skill_read(trace, "device-operator") is True
    assert trace_has_skill_read(trace, "other-skill") is False


def test_tool_result_evidence_keeps_failures_without_image_payloads():
    from runner.trace import tool_result_evidence
    result = tool_result_evidence([{
        "type": "tool_result", "tool_name": "touch_gesture", "is_error": True,
        "content": '{"data":"BASE64_IMAGE","width":1080,"action_output":"unknown operation: drag_start"}',
    }])
    assert result[0]["is_error"] is True
    assert "unknown operation: drag_start" in result[0]["content"]
    assert "BASE64_IMAGE" not in result[0]["content"]


def test_handoff_is_detected_from_result_not_just_attempted_call():
    from runner.trace import human_handoff_requested
    assert not human_handoff_requested([{"type": "tool_call", "tool_name": "request_user_action"}])
    assert human_handoff_requested([{"type": "tool_result", "tool_name": "request_user_action",
        "content": '{"status":"HUMAN_HANDOFF_REQUESTED","reason":"stuck"}'}])
