import json
from pathlib import Path

import pytest

from runner.assertions import evaluate_hard_assertions, evaluate_trace_observations
from runner.models import ToolCall, Trace
from runner.runtask import run_one_task
from runner.suite import HardAssertions, RubricItem, Suite, TaskSpec, TraceObservationSpec, SuiteValidationError, load_suite


def test_load_suite_parses_trace_observations(tmp_path: Path):
    fixture = {
        "name": "obs",
        "global_reset": {},
        "trace_observations": [
            {
                "id": "skill_read_device_operator",
                "description": "Loaded device-operator skill.",
                "skill_name": "device-operator",
            }
        ],
        "tasks": [
            {
                "id": "t1",
                "category": "single_step",
                "description_for_judge": "Do something.",
                "prompt": "go",
                "rubric": [{"id": "ok", "check": "ok"}],
                "hard_assertions": {"prohibited_actions": ["send", "checkout"]},
            }
        ],
    }
    path = tmp_path / "suite.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(path)

    assert len(suite.trace_observations) == 1
    assert suite.trace_observations[0].skill_name == "device-operator"
    assert suite.tasks[0].hard_assertions.prohibited_actions == ["send", "checkout"]


def test_load_suite_parses_generic_tool_trace_observations(tmp_path: Path):
    fixture = {
        "name": "obs",
        "global_reset": {},
        "trace_observations": [
            {
                "id": "used_search_launch_app",
                "description": "Used semantic app search.",
                "tool_name": "search_launch_app",
                "input_contains": {"app": "Clock"},
            }
        ],
        "tasks": [
            {
                "id": "t1",
                "category": "single_step",
                "description_for_judge": "Do something.",
                "prompt": "go",
                "rubric": [{"id": "ok", "check": "ok"}],
            }
        ],
    }
    path = tmp_path / "suite.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(path)

    assert len(suite.trace_observations) == 1
    assert suite.trace_observations[0].tool_name == "search_launch_app"
    assert suite.trace_observations[0].input_contains == {"app": "Clock"}


def test_load_suite_rejects_ambiguous_trace_observation_matchers(tmp_path: Path):
    fixture = {
        "name": "obs",
        "global_reset": {},
        "trace_observations": [
            {
                "id": "ambiguous",
                "description": "Ambiguous matcher.",
                "skill_name": "device-operator",
                "tool_name": "search_launch_app",
            }
        ],
        "tasks": [
            {
                "id": "t1",
                "category": "single_step",
                "description_for_judge": "Do something.",
                "prompt": "go",
                "rubric": [{"id": "ok", "check": "ok"}],
            }
        ],
    }
    path = tmp_path / "suite.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="specify only one of skill_name or tool_name"):
        load_suite(path)


def test_load_suite_rejects_duplicate_trace_observation_ids(tmp_path: Path):
    fixture = {
        "name": "obs",
        "global_reset": {},
        "trace_observations": [
            {
                "id": "used_search_launch_app",
                "description": "Used semantic app search.",
                "tool_name": "search_launch_app",
            },
            {
                "id": "used_search_launch_app",
                "description": "Loaded device-operator skill.",
                "skill_name": "device-operator",
            },
        ],
        "tasks": [
            {
                "id": "t1",
                "category": "single_step",
                "description_for_judge": "Do something.",
                "prompt": "go",
                "rubric": [{"id": "ok", "check": "ok"}],
            }
        ],
    }
    path = tmp_path / "suite.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="duplicate trace_observations id"):
        load_suite(path)


def test_phone_control_suite_defines_skill_read_observation():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "phone_control_v1.json"
    suite = load_suite(suite_path)

    assert any(
        obs.id == "skill_read_device_operator" and obs.skill_name == "device-operator"
        for obs in suite.trace_observations
    )


def test_evaluate_trace_observations_reports_skill_read():
    trace = Trace(
        tool_calls=[ToolCall(step=1, tool="skill_read", input={"name": "device-operator"})],
        final_response="",
        total_tool_calls=1,
        total_duration_ms=0,
    )
    specs = [
        TraceObservationSpec(
            id="skill_read_device_operator",
            description="Loaded skill.",
            skill_name="device-operator",
        )
    ]

    results = evaluate_trace_observations(trace, specs)

    assert len(results) == 1
    assert results[0].passed is True


def test_evaluate_trace_observations_does_not_trust_requested_skill():
    trace = Trace(tool_calls=[], final_response="", total_tool_calls=0, total_duration_ms=0)
    specs = [
        TraceObservationSpec(
            id="skill_read_device_operator",
            description="Loaded skill.",
            skill_name="device-operator",
        )
    ]

    results = evaluate_trace_observations(trace, specs, active_skills=["device-operator"])

    assert results[0].passed is False
    assert "No skill_read call" in results[0].reason


def test_evaluate_hard_assertions_fails_prohibited_quick_action_send():
    trace = Trace(
        tool_calls=[ToolCall(step=1, tool="quick_action", input={"action": "send"})],
        final_response="sent",
        total_tool_calls=1,
        total_duration_ms=0,
    )

    outcome = evaluate_hard_assertions(
        trace,
        HardAssertions(min_tool_calls=0, max_tool_calls=5, prohibited_actions=["send"]),
        timed_out=False,
    )

    assert outcome.all_passed is False
    assert outcome.results.prohibited_actions is False
    assert outcome.failures[-1].id == "prohibited_actions"
    assert "quick_action action=send at step 1" in outcome.failures[-1].actual


def test_evaluate_hard_assertions_fails_prohibited_checkout_trace_input():
    trace = Trace(
        tool_calls=[ToolCall(step=1, tool="browser_action", input={"label": "Check-out now"})],
        final_response="done",
        total_tool_calls=1,
        total_duration_ms=0,
    )

    outcome = evaluate_hard_assertions(
        trace,
        HardAssertions(min_tool_calls=0, max_tool_calls=5, prohibited_actions=["checkout"]),
        timed_out=False,
    )

    assert outcome.all_passed is False
    assert outcome.results.prohibited_actions is False
    assert "browser_action input.label=Check-out now at step 1" in outcome.failures[-1].actual


def test_evaluate_trace_observations_reports_generic_tool_with_input_subset():
    trace = Trace(
        tool_calls=[ToolCall(step=1, tool="search_launch_app", input={"app": "Clock", "platform": "android"})],
        final_response="",
        total_tool_calls=1,
        total_duration_ms=0,
    )
    specs = [
        TraceObservationSpec(
            id="used_search_launch_app",
            description="Used app search.",
            tool_name="search_launch_app",
            input_contains={"app": "Clock"},
        )
    ]

    results = evaluate_trace_observations(trace, specs)

    assert len(results) == 1
    assert results[0].passed is True


class ObservingClient:
    def health(self):
        return True

    def clear_history(self, timeout=30):
        pass

    def recover_after_timeout(self, timeout_sec=90, poll_sec=3.0):
        return True

    def invoke_tool(self, name, args):
        raise AssertionError("unexpected tool call")

    def chat(self, message, timeout_sec=None, attachments=None, skills=None):
        from runner.agent_client import ChatResponse

        return ChatResponse(
            response="done",
            history=[
                {"type": "tool_call", "tool_name": "skill_read", "tool_input": '{"name":"device-operator"}'},
                {"type": "tool_result", "tool_name": "skill_read", "content": "{}"},
                {"type": "assistant", "content": "done"},
            ],
        )


def test_run_one_task_records_trace_observations_without_affecting_pass(tmp_path: Path):
    suite = Suite(
        name="phone",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
        trace_observations=[
            TraceObservationSpec(
                id="skill_read_device_operator",
                description="Loaded skill.",
                skill_name="device-operator",
            )
        ],
    )
    task = TaskSpec(
        id="open_settings",
        category="single_step",
        description_for_judge="Open settings.",
        prompt="请打开系统设置。",
        rubric=[RubricItem(id="ok", check="ok")],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=5),
    )

    result = run_one_task(
        ObservingClient(), suite, task, 1, tmp_path / "artifacts", None, None, "run-1"
    )

    assert result.status == "passed"
    assert result.metrics["trace_observations"][0]["passed"] is True
