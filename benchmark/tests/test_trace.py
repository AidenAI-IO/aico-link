import json

import pytest

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


def test_extract_trace_captures_tool_error_status():
    history = [
        {"type": "tool_call", "tool_name": "bridge_contacts", "tool_input": '{"action":"query"}'},
        {"type": "tool_result", "tool_name": "bridge_contacts", "content": "Error: permission denied", "is_error": True},
        {"type": "tool_call", "tool_name": "screenshot", "tool_input": "{}"},
        {"type": "tool_result", "tool_name": "screenshot", "content": "timeout", "tool_error": {"code": "timeout"}},
        {"type": "tool_call", "tool_name": "enter_text", "tool_input": '{"text":"test"}'},
        {"type": "tool_result", "tool_name": "enter_text", "content": '{"ok":true}'},
        {"type": "assistant", "content": "Entered text after error"},
    ]
    trace = extract_trace(history)
    assert [tc.is_error for tc in trace.tool_calls] == [True, True, False]


def test_extract_trace_reads_failed_payload_and_missing_results():
    history = [
        {"type": "tool_call", "tool_name": "bridge_contacts", "tool_input": '{"action":"query"}'},
        {"type": "tool_result", "tool_name": "bridge_contacts", "content": '{"ok": false, "error": "denied"}'},
        {"type": "tool_call", "tool_name": "enter_text", "tool_input": '{"text":"x"}'},
        {"type": "tool_call", "tool_name": "touch_gesture", "tool_input": '{"type":"tap"}'},
    ]
    trace = extract_trace(history)
    # An ok:false payload fails like an error flag; calls without a result stay unfinished.
    assert [tc.is_error for tc in trace.tool_calls] == [True, False, False]
    assert [tc.has_result for tc in trace.tool_calls] == [True, False, False]


def test_apply_episode_tool_evidence_uses_episode_result_flags():
    from runner.trace import apply_episode_tool_evidence

    history = [
        {"type": "tool_call", "tool_name": "touch_gesture", "tool_input": '{"type":"tap"}'},
        {"type": "tool_result", "tool_name": "touch_gesture", "content": "ok"},
        {"type": "tool_call", "tool_name": "open_app", "tool_input": '{"app":"Settings"}'},
        {"type": "tool_result", "tool_name": "open_app", "content": "no mock response configured"},
    ]
    episode = {"events": [
        {"type": "tool_call", "tool_name": "touch_gesture"},
        {"type": "tool_result", "tool_name": "touch_gesture"},
        {"type": "tool_call", "tool_name": "open_app"},
        {"type": "tool_result", "tool_name": "open_app", "is_error": True},
    ]}
    trace = extract_trace(history)
    apply_episode_tool_evidence(trace, episode)
    assert [tc.is_error for tc in trace.tool_calls] == [False, True]

    # A misaligned episode is ignored instead of flagging the wrong call.
    trace = extract_trace(history)
    apply_episode_tool_evidence(trace, {"events": episode["events"][1:2]})
    assert [tc.is_error for tc in trace.tool_calls] == [False, False]
    apply_episode_tool_evidence(trace, None)
    assert [tc.is_error for tc in trace.tool_calls] == [False, False]


def test_perception_scores_normalized_coordinates_from_pixel_model_episode():
    # A pixel-coordinate model taps Settings on a 1179x2556 screenshot. History keeps
    # the raw pixels; the device received the normalized point recorded in the episode.
    from pathlib import Path

    from runner.assertions import evaluate_hard_assertions
    from runner.suite import load_suite
    from runner.trace import apply_episode_tool_evidence

    pixel = '{"type":"tap","point":{"x":996,"y":1219}}'
    normalized = {"type": "tap", "point": {"x": 845.5, "y": 477.1}}
    history = [
        {"type": "tool_call", "tool_name": "touch_gesture", "tool_input": pixel},
        {"type": "tool_result", "tool_name": "touch_gesture", "content": '{"ok": true}'},
    ]
    episode = {"events": [
        {"type": "visual_coordinate_mapping", "metadata": {
            "tool": "touch_gesture", "image_width": 1179, "image_height": 2556,
            "pixel_input": json.loads(pixel), "normalized_input": normalized,
        }},
        {"type": "tool_call", "tool_name": "touch_gesture", "tool_input": pixel},
        {"type": "tool_result", "tool_name": "touch_gesture"},
    ]}
    suite = load_suite(Path(__file__).resolve().parents[1] / "suites" / "perception" / "perception_v1.json")
    task = next(task for task in suite.tasks if task.id == "find_settings_iphone")

    raw = extract_trace(history)
    assert evaluate_hard_assertions(raw, task.hard_assertions, timed_out=False).all_passed is False
    trace = extract_trace(history)
    apply_episode_tool_evidence(trace, episode)
    assert trace.tool_calls[0].input == normalized
    assert evaluate_hard_assertions(trace, task.hard_assertions, timed_out=False).all_passed is True


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


@pytest.mark.parametrize("tool", ["screenshot", "touch_gesture"])
@pytest.mark.parametrize("result_has_tool_name", [False, True])
def test_tool_result_evidence_removes_data_only_images(tool, result_has_tool_name):
    from runner.trace import tool_result_evidence

    result = {"type": "tool_result", "content": '{"data":"BASE64_IMAGE","action_output":"ok"}'}
    if result_has_tool_name:
        result["tool_name"] = tool
    history = [{"type": "tool_call", "tool_name": tool}, result]

    evidence = tool_result_evidence(history)

    assert json.loads(evidence[0]["content"]) == {"action_output": "ok"}
    assert "BASE64_IMAGE" in history[1]["content"]


def test_tool_result_evidence_preserves_non_image_data():
    from runner.trace import tool_result_evidence

    history = [{"type": "tool_result", "tool_name": "skill_read", "content": '{"data":"skill contents"}'}]

    assert tool_result_evidence(history) == history
