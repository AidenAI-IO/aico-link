"""Behavior assertions in setup must not be mistaken for broken fixtures."""
import json
from pathlib import Path

import pytest

from runner.agent_client import AgentRequestError, AgentSemanticError, ToolInvokeResult
from runner.mock_environment import MockEnvironmentServer
from runner.reset import per_task_setup
from runner.runtask import evaluate_task_history, run_one_task
from runner.suite import HardAssertions, Suite, SuiteValidationError, TaskSpec, load_suite
import runner.runtask as runtask

SUITES = Path(__file__).resolve().parents[1] / "suites"


class SetupClient:
    def __init__(self, failure):
        self.failure = failure
        self.processed = False

    def seed_episode(self, episode, timeout=90):
        if self.failure == "seed":
            raise AgentRequestError("fixture injection unavailable")

    def process_episode_memory(self, episode_id, timeout=90):
        self.processed = True
        if self.failure == "transport":
            raise AgentRequestError("connection lost")
        if self.failure == "semantic":
            raise AgentSemanticError("invalid model proposal", status_code=422)
        return {"status": "done", "assessment": {"goal_result": "achieved"}, "memory_ids": ["learned"]}

    def invoke_tool(self, name, args, timeout=90):
        if self.failure == "read":
            raise AgentRequestError("memory evidence unavailable")
        return ToolInvokeResult(output=json.dumps({"results": [{"id": "learned", "content": "retained"}]}),
                                is_error=False, duration_ms=1)


def run_setup(tmp_path, monkeypatch, setup, client):
    task = TaskSpec(id="case", category="memory", prompt="unreachable", rubric=[],
                    description_for_judge="Memory behavior", hard_assertions=HardAssertions(), setup=setup)
    suite = Suite(name="memory", global_reset={}, tasks=[task], sha256="sha", source_path=tmp_path / "suite.json")
    monkeypatch.setattr(runtask, "prepare_task_isolation", lambda *a, **kw: per_task_setup(client, setup))
    return run_one_task(client, suite, task, 1, tmp_path, None, None, "run")


@pytest.mark.parametrize("evaluate", [False, True])
@pytest.mark.parametrize("failure", ["assertion", "seed", "transport", "semantic", "read"])
def test_evaluated_consolidation_distinguishes_behavior_from_infrastructure(tmp_path, monkeypatch, evaluate, failure):
    step = {"type": "seed_episode", "evaluate": evaluate, "consolidate": True,
            "episode": {"id": "ep", "user_goal": "Observe a transient value"},
            "consolidation_expectation": {"max_memory_ids": 0, "allow_empty_memory": True}}
    if failure == "read":
        step["consolidation_expectation"] = {"required_memory_types": ["procedure"]}
    # Sequence wrapping must preserve eligibility AND the consolidation payload.
    result = run_setup(tmp_path, monkeypatch, [step], SetupClient(failure))
    eligible = evaluate and failure in {"assertion", "semantic"}
    assert result.metrics["agent_eligible"] is eligible
    assert result.metrics["first_failure_stage"] == "setup"
    if eligible:
        assert result.status == "failed"
        assert result.metrics["failure_class"] == "agent"
        assert result.metrics["quality_score"] == 0
        assert result.hard_assertion_failures[0].id == "setup_evaluation"
    if failure == "assertion":
        artifact = json.loads((tmp_path / "setup_failure.json").read_text())
        assert artifact["consolidation"]["memory_ids"] == ["learned"]
        assert artifact["setup_step"] == step
        assert result.metrics["consolidation_memory_count"] == 1


@pytest.mark.parametrize("evaluate", [False, True])
def test_memory_state_failure_keeps_observed_records_and_fixture_boundary(tmp_path, monkeypatch, evaluate):
    setup = [{"type": "assert_memory", "evaluate": evaluate, "expected_count": 0}]
    result = run_setup(tmp_path, monkeypatch, setup, SetupClient("assertion"))
    assert result.metrics["agent_eligible"] is evaluate
    evidence = json.loads((tmp_path / "setup_failure.json").read_text())
    assert evidence["memory_state"] == [{"id": "learned", "content": "retained"}]


@pytest.mark.parametrize("evaluate,consolidate", [("true", True), (1, True), (True, False)])
def test_suite_rejects_invalid_evaluation_marker(tmp_path, evaluate, consolidate):
    data = json.loads((SUITES / "episode_reflection_v1.json").read_text())
    data["tasks"][0]["setup"].update(evaluate=evaluate, consolidate=consolidate)
    path = tmp_path / "invalid.json"
    path.write_text(json.dumps(data))
    with pytest.raises(SuiteValidationError, match="evaluate"):
        load_suite(path)


def test_perception_fixture_rejects_malformed_calls_but_accepts_wrong_targets():
    path = SUITES / "perception/perception_v1.json"
    suite = load_suite(path)
    for task in suite.tasks:
        server = MockEnvironmentServer(task.mock_environment, path.parent)
        # The captured task_386 input must fail even though the fixture says ok.
        assert server.invoke("touch_gesture", {"point": {"x": 935, "y": 87}}).is_error
        assert server.invoke("touch_gesture", {"type": "tap", "point": {"x": "935", "y": 87}}).is_error
        assert server.invoke("touch_gesture", {"type": "tap"}).is_error
        # Keep execution independent of the target label; the evaluator scores aim.
        assert not server.invoke("touch_gesture", {"type": "tap", "point": {"x": 500, "y": 500}}).is_error


@pytest.mark.parametrize("duplicates", [False, True])
def test_notification_reinforcement_scores_memory_state_not_repeated_speech(tmp_path, duplicates):
    suite = load_suite(SUITES / "notification_memory_v1.json")
    task = next(t for t in suite.tasks if t.id == "notification_explicit_reinforce")
    item = {"id": "tmp_reinforce_calendar", "memory_scope": "temporary", "revision": 2,
            "content": "The meeting is tomorrow at 10:00.",
            "source_refs": [{"type": "notification", "id": "fixture-reinforce-old", "event_ids": ["old-event"]},
                            {"type": "notification", "id": "1", "event_ids": ["reinforce-calendar-event-1"]}]}
    state = [item, {**item, "id": "duplicate"}] if duplicates else [item]
    history = [{"type": "tool_call", "tool_name": "recall_memory", "tool_input": "{}"},
               {"type": "tool_result", "tool_name": "recall_memory", "content": json.dumps({"results": state})},
               {"type": "assistant", "content": "<tts>The meeting is tomorrow at 10:00.</tts>The meeting is tomorrow at 10:00."}]
    result = evaluate_task_history(suite=suite, task=task, history=history, attempt=1,
                                  artifact_dir=tmp_path, judge_cfg=None, judge_cache_dir=None,
                                  run_id="run", timed_out=False, memory_state=state)
    assert result.hard_assertions.memory_state is (not duplicates)
    assert result.status == ("failed" if duplicates else "passed")
    assert "does_not_duplicate" not in {r.id for r in task.rubric}


def test_later_evaluation_marker_does_not_score_a_broken_fixture(tmp_path, monkeypatch):
    client = SetupClient("assertion")
    setup = [{"type": "assert_memory", "expected_count": 0},
             {"type": "seed_episode", "evaluate": True, "consolidate": True,
              "episode": {"id": "ep", "user_goal": "Learn"}}]
    result = run_setup(tmp_path, monkeypatch, setup, client)
    assert result.metrics["agent_eligible"] is False
    assert client.processed is False


@pytest.mark.parametrize("failure", ["count", "semantic", "seed", "transport"])
def test_evaluated_notification_failure_boundary(tmp_path, monkeypatch, failure):
    class Client(SetupClient):
        def seed_notification(self, events, timeout=90):
            if failure == "seed":
                raise AgentRequestError("fixture failed")
            return {"context_ids": ["1"]}

        def process_notification_memory(self, timeout=90):
            if failure == "semantic":
                raise AgentSemanticError("invalid scope", status_code=422)
            if failure == "transport":
                raise AgentRequestError("connection lost")
            return {"memory_cursor": "1", "memory_ids": ["unexpected"]}

    setup = {"type": "seed_notification", "evaluate": True, "consolidate": True,
             "events": [{"title": "Verification code"}], "expected_memory_count": 0}
    result = run_setup(tmp_path, monkeypatch, setup, Client(failure))
    assert result.metrics["agent_eligible"] is (failure in {"count", "semantic"})
    if failure == "count":
        artifact = json.loads((tmp_path / "setup_failure.json").read_text())
        assert artifact["consolidation"]["memory_ids"] == ["unexpected"]


def test_invalid_consolidation_response_is_preserved_without_crashing(tmp_path, monkeypatch):
    class Client(SetupClient):
        def process_episode_memory(self, episode_id, timeout=90):
            return {"status": "done", "assessment": "invalid", "memory_ids": None}

    setup = {"type": "seed_episode", "evaluate": True, "consolidate": True,
             "episode": {"id": "ep", "user_goal": "Learn"}}
    result = run_setup(tmp_path, monkeypatch, setup, Client("invalid"))
    assert result.status == "failed"
    assert result.metrics["agent_eligible"] is True
    assert result.metrics["consolidation_memory_count"] is None
    assert "invalid memory_ids" in result.metrics["error"]


@pytest.mark.parametrize("payload,eligible", [
    ({}, False),
    (None, False),
    ([], False),
    ({"results": {}}, False),
    ({"results": "invalid"}, False),
    ({"results": [None]}, False),
    ({"results": []}, True),
    ({"results": None}, True),
    ({"results": [{"id": "other"}]}, True),
])
def test_evaluated_consolidation_recall_separates_missing_from_empty_evidence(
    tmp_path, monkeypatch, payload, eligible,
):
    class Client(SetupClient):
        def invoke_tool(self, name, args, timeout=90):
            return ToolInvokeResult(json.dumps(payload), False, 1)

    step = {"type": "seed_episode", "evaluate": True, "consolidate": True,
            "episode": {"id": "ep", "user_goal": "Learn"},
            "consolidation_expectation": {"required_memory_types": ["procedure"]}}
    result = run_setup(tmp_path, monkeypatch, [step], Client("recall"))
    assert result.metrics["agent_eligible"] is eligible
    assert result.metrics["first_failure_stage"] == "setup"
    if eligible:
        assert result.status == "failed"
        assert result.metrics["failure_class"] == "agent"
        assert result.metrics["quality_score"] == 0
        evidence = json.loads((tmp_path / "setup_failure.json").read_text())
        assert evidence["consolidation"]["memory_ids"] == ["learned"]
    else:
        assert result.metrics["quality_score"] is None
        assert "recall results" in result.metrics["error"]
