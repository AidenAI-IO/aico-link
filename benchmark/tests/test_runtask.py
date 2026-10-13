import json
from pathlib import Path

from PIL import Image
import pytest

from runner.agent_client import AgentRequestError, AgentTimeoutError, ChatResponse, ToolInvokeResult
from runner.judge import JudgeConfig, JudgeOutput
from runner.metrics import aggregate
from runner.models import RubricVerdict
from runner.reset import SetupAssertionError
from runner.runtask import run_one_task
from runner.suite import (
    HardAssertions,
    MockEnvironmentSpec,
    RubricItem,
    Suite,
    TaskSpec,
    TraceObservationSpec,
)
import runner.runtask as runtask_mod


class FakeClient:
    def __init__(self, response="ok"):
        self.response = response
        self.messages = []
        self.attachments = []
        self.skill_requests = []

    def health(self):
        return True

    def clear_history(self, timeout=30):
        pass

    def recover_after_timeout(self, timeout_sec=90, poll_sec=3.0):
        return True

    def invoke_tool(self, name, args):
        raise AssertionError(f"unexpected tool invoke: {name}")

    def chat(self, message, timeout_sec=None, attachments=None, skills=None):
        self.messages.append(message)
        self.attachments.append(attachments)
        self.skill_requests.append(list(skills or []))
        return ChatResponse(
            response=self.response,
            history=[{"type": "assistant", "content": self.response}],
        )


@pytest.mark.parametrize("behavior", ["replace", "append", "read_error", "missing_fixture", "timeout"])
def test_memory_update_is_verified_from_storage(tmp_path: Path, monkeypatch, behavior):
    old = {"id": "old", "type": "preference", "content": "dark mode"}
    new = {"id": "new", "type": "preference", "content": "light mode"}
    observation = {"query": {"limit": 100}, "expected_count": 1}
    suite = Suite(name="memory", global_reset={}, tasks=[], sha256="sha", source_path=tmp_path / "suite.json")
    task = TaskSpec(
        id="update", category="memory", description_for_judge="Update theme", prompt="Use light mode",
        rubric=[RubricItem(id="current", check="Only light mode is current")],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=1),
        setup=[
            {"type": "seed_memory", "memories": [old]},
            {"type": "assert_memory", **observation, "expected": [{"id": "old"}]},
        ],
        memory_assertions={**observation, "expected": [{"content_contains": "light mode"}]},
    )

    class MemoryClient(FakeClient):
        def __init__(self):
            super().__init__()
            self.memories = []
            self.reads = 0

        def seed_memory(self, memory, timeout=30):
            if behavior != "missing_fixture":
                self.memories.append(dict(memory))

        def invoke_tool(self, name, args, timeout=30):
            assert name == "recall_memory"
            self.reads += 1
            if behavior == "read_error" and self.messages:
                raise AgentRequestError("storage unavailable")
            return ToolInvokeResult(json.dumps({"results": self.memories}), False, 1)

        def chat(self, *args, **kwargs):
            super().chat(*args, **kwargs)
            if behavior == "timeout":
                raise AgentTimeoutError("task timed out")
            self.memories = [*self.memories, new] if behavior == "append" else [new]
            return ChatResponse(response="Updated", history=[
                {"type": "tool_call", "tool_name": "save_memory", "tool_input": json.dumps(new)},
                {"type": "tool_result", "tool_name": "save_memory", "content": json.dumps({"id": "new"})},
                {"type": "assistant", "content": "Updated"},
            ])

    judged = []

    def judge(**kwargs):
        judged.append(kwargs)
        return JudgeOutput([RubricVerdict("current", "yes", "Stored light mode")], "", "key", "")

    monkeypatch.setattr(runtask_mod, "judge_task", judge)
    client = MemoryClient()
    result = run_one_task(client, suite, task, 1, tmp_path / "artifacts", JudgeConfig(), None, "run")

    if behavior == "missing_fixture":
        assert client.messages == []
        assert result.metrics["first_failure_stage"] == "setup"
        assert result.metrics["agent_eligible"] is False
    elif behavior == "timeout":
        assert result.status == "timeout"
        assert client.reads == 1
    elif behavior == "read_error":
        assert result.status == "judge_error"
        assert result.metrics["success"] is None
        assert result.metrics["agent_eligible"] is False
        assert result.metrics["failure_class"] == "evaluation"
    else:
        assert result.status == ("passed" if behavior == "replace" else "failed")
        assert result.hard_assertions.memory_state is (behavior == "replace")
        assert result.metrics["tool_calls"] == 1
        assert client.reads == 2
        trace = json.loads((tmp_path / "artifacts" / "trace.json").read_text())
        assert trace["memory_state"] == client.memories
        assert "pre_screenshot_error" not in result.metrics
        assert "post_screenshot_error" not in result.metrics
    if behavior == "replace":
        assert judged[0]["trace"]["memory_state"] == [new]
        assert json.loads(judged[0]["trace"]["tool_results"][0]["content"]) == {"id": "new"}
    else:
        assert judged == []


def test_run_one_task_marks_setup_assertion_as_failed(tmp_path: Path, monkeypatch):
    suite = Suite(
        name="memory",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="memory_update",
        category="memory",
        description_for_judge="Update memory.",
        prompt="check memory",
        rubric=[],
        hard_assertions=HardAssertions(),
    )

    def fail_setup(*args, **kwargs):
        raise SetupAssertionError("expected revision 2")

    monkeypatch.setattr(runtask_mod, "prepare_task_isolation", fail_setup)

    result = run_one_task(
        FakeClient(), suite, task, 1, tmp_path / "artifacts", None, None, "run-1"
    )

    assert result.status == "failed"
    assert result.metrics["error"] == "setup assertion: expected revision 2"
    assert result.metrics["failure_event_ref"] is None


def test_run_one_task_does_not_count_pre_execution_agent_error_as_agent_failure(
    tmp_path: Path,
):
    suite = Suite(
        name="provider_error",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="open_settings",
        category="single_step",
        description_for_judge="Open Settings.",
        prompt="Open Settings.",
        rubric=[],
        hard_assertions=HardAssertions(),
    )

    class BrokenClient(FakeClient):
        def chat(self, *args, **kwargs):
            raise RuntimeError("missing provider API key")

    result = run_one_task(
        BrokenClient(), suite, task, 1, tmp_path / "artifacts", None, None, "run-1"
    )

    assert result.status == "failed"
    assert result.metrics["success"] is None
    assert result.metrics["agent_eligible"] is False
    assert result.metrics["failure_class"] == "unknown"


def test_run_one_task_applies_environment_state_assertions(tmp_path: Path, monkeypatch):
    suite = Suite(
        name="mobilegym",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="find_target",
        category="multi_step",
        description_for_judge="Open the exact target.",
        prompt="open target",
        rubric=[],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=0),
        environment_assertions={
            "route.path": "/item/scroll-item-083",
            "apps.scroll_lab.selectedItemId": "scroll-item-083",
        },
    )
    monkeypatch.setattr(runtask_mod, "prepare_task_isolation", lambda *args, **kwargs: None)
    monkeypatch.setattr(runtask_mod, "take_environment_screenshot", lambda *args, **kwargs: (360, 800))
    monkeypatch.setattr(
        runtask_mod,
        "read_environment_state",
        lambda *args, **kwargs: {
            "route": {"path": "/item/scroll-item-083"},
            "apps": {"scroll_lab": {"selectedItemId": "scroll-item-024"}},
        },
    )

    result = run_one_task(
        FakeClient(),
        suite,
        task,
        1,
        tmp_path / "artifacts",
        None,
        None,
        "run-1",
        environment_url="http://environment.test",
    )

    assert result.status == "failed"
    assert result.hard_assertions.environment_state is False
    assert result.metrics["success"] is False
    assert result.metrics["agent_eligible"] is True
    assert result.metrics["quality_score"] == 0.0
    assert aggregate([result])["pass_at_1"]["value"] == 0.0
    assert [failure.id for failure in result.hard_assertion_failures] == [
        "environment_state:apps.scroll_lab.selectedItemId"
    ]
    assert (tmp_path / "artifacts" / "environment_state.json").exists()


def test_missing_environment_state_invalidates_success_metrics():
    task = TaskSpec(
        id="missing_state", category="multi_step", description_for_judge="", prompt="",
        rubric=[], hard_assertions=HardAssertions(),
        environment_assertions={"route.path": "/item/scroll-item-083"},
    )
    result = runtask_mod.TaskResult(
        suite="mobilegym", run_id="run", task_id=task.id, category=task.category,
        attempt=1, status="passed", rubric=[],
        metrics={"success": True, "agent_eligible": True, "quality_score": 1.0},
    )
    runtask_mod._apply_environment_assertions(result, task, None, "state unavailable")
    assert result.status == "judge_error"
    assert result.metrics["success"] is None
    assert result.metrics["agent_eligible"] is False
    assert result.metrics["failure_class"] == "evaluation"
    assert result.metrics["quality_score"] is None


def test_run_one_task_includes_static_screenshot_dimensions(tmp_path: Path):
    screenshot_dir = tmp_path / "screenshots"
    screenshot_dir.mkdir()
    screenshot_path = screenshot_dir / "screen.jpg"
    Image.new("RGB", (37, 91), "white").save(screenshot_path, format="JPEG")

    suite = Suite(
        name="perception",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="tap_target",
        category="perception",
        description_for_judge="Tap the target.",
        prompt="tap the target",
        rubric=[],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=0),
        input_screenshot="screenshots/screen.jpg",
    )
    client = FakeClient("done")

    result = run_one_task(
        client, suite, task, 1, tmp_path / "artifacts", None, None, "run-1"
    )

    assert result.status == "passed"
    assert client.attachments[0] is not None
    attachment = client.attachments[0][0]
    assert attachment["width"] == 37
    assert attachment["height"] == 91


def test_run_one_task_uses_mock_screenshot_without_static_attachment(
    tmp_path: Path,
    monkeypatch,
):
    screenshot_dir = tmp_path / "screenshots"
    screenshot_dir.mkdir()
    fixture_path = screenshot_dir / "fixture.jpg"
    Image.new("RGB", (37, 91), "white").save(fixture_path, format="JPEG")
    suite = Suite(
        name="perception",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="tap_target",
        category="perception",
        description_for_judge="Tap the target.",
        prompt="tap the target",
        rubric=[],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=0),
        input_screenshot="screenshots/fixture.jpg",
        mock_environment=MockEnvironmentSpec(
            platform="ios",
            phone_bridge={},
            tools={},
            screen="screenshots/fixture.jpg",
            single_frame=True,
        ),
    )
    capture_calls = []

    def capture_fixture(environment_url, out_path, benchmark_task_id=None):
        capture_calls.append((environment_url, benchmark_task_id))
        out_path.write_bytes(fixture_path.read_bytes())
        return 37, 91

    monkeypatch.setattr(runtask_mod, "take_environment_screenshot", capture_fixture)
    client = FakeClient("done")
    artifact_dir = tmp_path / "artifacts"

    result = run_one_task(
        client,
        suite,
        task,
        1,
        artifact_dir,
        None,
        None,
        "run-1",
        environment_url="http://mock-environment.test",
    )

    assert result.status == "passed"
    assert client.attachments == [None]
    assert capture_calls == [("http://mock-environment.test", None)]
    with Image.open(artifact_dir / "pre.jpg") as image:
        assert image.size == (37, 91)
    assert not (artifact_dir / "post.jpg").exists()


def test_run_one_task_falls_back_to_fixture_for_mock_pre_artifact(
    tmp_path: Path,
    monkeypatch,
):
    screenshot_dir = tmp_path / "screenshots"
    screenshot_dir.mkdir()
    fixture_path = screenshot_dir / "fixture.jpg"
    Image.new("RGB", (37, 91), "white").save(fixture_path, format="JPEG")
    suite = Suite(
        name="perception",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="tap_target",
        category="perception",
        description_for_judge="Tap the target.",
        prompt="tap the target",
        rubric=[],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=0),
        input_screenshot="screenshots/fixture.jpg",
        mock_environment=MockEnvironmentSpec(
            platform="ios",
            phone_bridge={},
            tools={},
            screen="screenshots/fixture.jpg",
            single_frame=True,
        ),
    )

    def fail_capture(*args, **kwargs):
        raise RuntimeError("provider unavailable")

    monkeypatch.setattr(runtask_mod, "take_environment_screenshot", fail_capture)
    client = FakeClient("done")
    artifact_dir = tmp_path / "artifacts"

    result = run_one_task(
        client,
        suite,
        task,
        1,
        artifact_dir,
        None,
        None,
        "run-1",
        environment_url="http://mock-environment.test",
    )

    assert result.status == "passed"
    assert client.attachments == [None]
    assert result.metrics["pre_screenshot_error"] == "provider unavailable"
    assert (artifact_dir / "pre.jpg").read_bytes() == fixture_path.read_bytes()
    assert not (artifact_dir / "post.jpg").exists()


def test_run_one_task_keeps_dynamic_mock_screenshot_attachment_and_post_artifact(
    tmp_path: Path,
    monkeypatch,
):
    screenshot_dir = tmp_path / "screenshots"
    screenshot_dir.mkdir()
    fixture_path = screenshot_dir / "fixture.jpg"
    Image.new("RGB", (37, 91), "white").save(fixture_path, format="JPEG")
    suite = Suite(
        name="dynamic-mock",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="tap_target",
        category="perception",
        description_for_judge="Tap the target.",
        prompt="tap the target",
        rubric=[],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=0),
        input_screenshot="screenshots/fixture.jpg",
        mock_environment=MockEnvironmentSpec(
            platform="ios",
            phone_bridge={},
            tools={},
            screen="screenshots/fixture.jpg",
        ),
    )
    capture_calls = []

    def capture_post(environment_url, out_path, benchmark_task_id=None):
        capture_calls.append((environment_url, out_path.name, benchmark_task_id))
        Image.new("RGB", (37, 91), "black").save(out_path, format="JPEG")
        return 37, 91

    monkeypatch.setattr(runtask_mod, "take_environment_screenshot", capture_post)
    client = FakeClient("done")
    artifact_dir = tmp_path / "artifacts"

    result = run_one_task(
        client,
        suite,
        task,
        1,
        artifact_dir,
        None,
        None,
        "run-1",
        environment_url="http://mock-environment.test",
    )

    assert result.status == "passed"
    attachment = client.attachments[0][0]
    assert attachment["width"] == 37
    assert attachment["height"] == 91
    assert capture_calls == [
        ("http://mock-environment.test", "post.jpg", None),
    ]
    assert (artifact_dir / "pre.jpg").read_bytes() == fixture_path.read_bytes()
    assert (artifact_dir / "post.jpg").exists()


def test_run_one_task_passes_without_judge_when_hard_assertions_pass(tmp_path: Path):
    suite = Suite(
        name="smoke",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="chat_smoke",
        category="diagnostic",
        description_for_judge="Smoke chat task.",
        prompt="ping",
        rubric=[RubricItem(id="responds", check="Agent responds.")],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=0),
    )

    result = run_one_task(
        FakeClient(), suite, task, 1, tmp_path / "artifacts", None, None, "run-1"
    )

    assert result.status == "passed"
    assert result.hard_assertions.response_exists is True


def test_run_one_task_writes_consolidation_artifact_from_setup(
    tmp_path: Path, monkeypatch
):
    suite = Suite(
        name="reflection",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="reflection_case",
        category="memory",
        description_for_judge="Reflection contract.",
        prompt="confirm",
        rubric=[RubricItem(id="ok", check="responds")],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=0),
        setup={"type": "seed_episode"},
    )

    monkeypatch.setattr(
        runtask_mod,
        "prepare_task_isolation",
        lambda *args, **kwargs: {
            "type": "seed_episode",
            "episode_id": "ep-1",
            "consolidated": True,
            "consolidation": {
                "episode_id": "ep-1",
                "status": "done",
                "assessment": {"goal_result": "unknown"},
                "memory_ids": [],
            },
        },
    )

    artifact_dir = tmp_path / "artifacts"
    result = run_one_task(
        FakeClient("done"), suite, task, 1, artifact_dir, None, None, "run-1"
    )

    assert result.status == "passed"
    assert json.loads((artifact_dir / "consolidation.json").read_text())["assessment"]["goal_result"] == "unknown"
    assert not (artifact_dir / "setup.json").exists()


def test_run_one_task_preserves_empty_consolidation_artifact_before_recall_failure(
    tmp_path: Path, monkeypatch
):
    suite = Suite(
        name="reflection",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="reflection_recall_case",
        category="memory",
        description_for_judge="Recall a consolidated lesson.",
        prompt="confirm",
        rubric=[RubricItem(id="ok", check="responds")],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=1),
        setup={"type": "seed_episode"},
        expected_recall_from_consolidation=True,
        expected_recalled_memory_tool="recall_device_memory",
    )
    consolidation = {
        "episode_id": "ep-1",
        "status": "done",
        "assessment": {"goal_result": "unknown"},
        "memory_ids": [],
    }
    monkeypatch.setattr(
        runtask_mod,
        "prepare_task_isolation",
        lambda *args, **kwargs: {
            "type": "seed_episode",
            "episode_id": "ep-1",
            "consolidated": True,
            "consolidation": consolidation,
        },
    )

    artifact_dir = tmp_path / "artifacts"
    result = run_one_task(
        FakeClient("done"), suite, task, 1, artifact_dir, None, None, "run-1"
    )

    assert result.status == "failed"
    assert json.loads((artifact_dir / "consolidation.json").read_text()) == consolidation
    assert result.metrics["consolidation_goal_result"] == "unknown"
    assert result.metrics["consolidation_memory_count"] == 0
    assert "non-empty consolidation memory_ids" in result.metrics["error"]


def test_run_one_task_fails_without_judge_when_expected_answer_is_wrong(tmp_path: Path):
    suite = Suite(
        name="persona",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="personamem_case",
        category="memory",
        description_for_judge="Choose the personalized option.",
        prompt="Choose one option.",
        rubric=[RubricItem(id="chooses_correct", check="Agent chooses correct option.")],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=0),
        expected_answer="(c)",
        answer_format="option_letter",
    )

    result = run_one_task(
        FakeClient("I choose <final_answer>(b)</final_answer>"),
        suite,
        task,
        1,
        tmp_path / "artifacts",
        None,
        None,
        "run-1",
    )

    assert result.status == "failed"
    assert result.metrics["expected_answer_match"] is False
    assert result.metrics["predicted_answer"] == "(b)"
    assert result.hard_assertion_failures[-1].id == "expected_answer"
    assert result.hard_assertion_failures[-1].requirement == "Final answer must be (c)."
    assert result.hard_assertion_failures[-1].actual == "Predicted answer was (b)."


def test_evaluate_task_history_applies_hard_assertions_and_expected_answer(tmp_path: Path):
    from runner.runtask import evaluate_task_history

    suite = Suite(
        name="s",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "s.json",
    )
    task = TaskSpec(
        id="answer_case",
        category="single_step",
        description_for_judge="Answer C.",
        prompt="Pick C",
        rubric=[],
        hard_assertions=HardAssertions(
            min_tool_calls=0,
            max_tool_calls=2,
            response_required=True,
        ),
        expected_answer="(c)",
        answer_format="option_letter",
    )
    history = [{"type": "assistant", "content": "<final_answer>(c)</final_answer>"}]

    result = evaluate_task_history(
        suite=suite,
        task=task,
        history=history,
        attempt=1,
        artifact_dir=tmp_path / "artifacts",
        judge_cfg=None,
        judge_cache_dir=None,
        run_id="run-1",
        timed_out=False,
        metrics={},
    )

    assert result.status == "passed"
    assert result.hard_assertions is not None
    assert result.hard_assertions.expected_answer is True
    assert (tmp_path / "artifacts" / "history.json").exists()
    assert (tmp_path / "artifacts" / "trace.json").exists()


def test_evaluate_task_history_persists_episode_for_failure_evidence(tmp_path: Path):
    from runner.runtask import evaluate_task_history

    suite = Suite(
        name="s",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "s.json",
    )
    task = TaskSpec(
        id="tool_failure",
        category="single_step",
        description_for_judge="Use contacts.",
        prompt="Use contacts.",
        rubric=[],
        hard_assertions=HardAssertions(required_tools=["bridge_contacts"]),
    )
    episode = {
        "events": [
            {
                "type": "tool_result",
                "tool_name": "touch_gesture",
                "tool_input": '{"type":"tap"}',
                "duration_ms": 10,
                "is_error": True,
            }
        ]
    }

    result = evaluate_task_history(
        suite=suite,
        task=task,
        history=[
            {"type": "tool_call", "tool_name": "touch_gesture", "tool_input": '{"type":"tap"}'},
            {"type": "tool_result", "tool_name": "touch_gesture", "content": "failed"},
        ],
        episode=episode,
        attempt=1,
        artifact_dir=tmp_path / "artifacts",
        judge_cfg=None,
        judge_cache_dir=None,
        run_id="run-1",
        timed_out=False,
    )

    assert result.metrics["failure_event_ref"] == "episode.json#/events/0"
    assert json.loads((tmp_path / "artifacts" / "episode.json").read_text()) == episode


def test_evaluate_task_history_records_expected_memory_failure_details(tmp_path: Path):
    from runner.runtask import evaluate_task_history

    suite = Suite(
        name="s",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "s.json",
    )
    task = TaskSpec(
        id="memory_case",
        category="memory",
        description_for_judge="Recall a memory.",
        prompt="What do I prefer?",
        rubric=[],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=2),
        expected_recalled_memory_ids=["personamem_solo_travel"],
    )
    history = [
        {
            "type": "tool_call",
            "tool_name": "recall_memory",
            "tool_input": "{}",
        },
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": json.dumps({"results": [{"id": "personamem_campfire_storytelling"}]}),
        },
        {"type": "assistant", "content": "I found a different memory."},
    ]

    result = evaluate_task_history(
        suite=suite,
        task=task,
        history=history,
        attempt=1,
        artifact_dir=tmp_path / "artifacts",
        judge_cfg=None,
        judge_cache_dir=None,
        run_id="run-1",
        timed_out=False,
        metrics={},
    )

    assert result.status == "failed"
    assert result.hard_assertion_failures[-1].id == "expected_recalled_memory"
    assert result.hard_assertion_failures[-1].requirement == "Must recall memory id(s): personamem_solo_travel."
    assert (
        result.hard_assertion_failures[-1].actual
        == "Missing: personamem_solo_travel. Recalled: personamem_campfire_storytelling."
    )


def test_evaluate_task_history_requires_inline_recall_for_consolidation_ids(tmp_path: Path):
    from runner.runtask import evaluate_task_history

    suite = Suite(
        name="s",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "s.json",
    )
    task = TaskSpec(
        id="consolidation_recall_case",
        category="memory",
        description_for_judge="Recall the newly consolidated memory.",
        prompt="Recall it.",
        rubric=[],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=2),
        expected_recalled_memory_ids=["memory-created-by-consolidation"],
        expected_recalled_memory_tool="recall_device_memory",
        expected_recall_from_consolidation=True,
    )
    history = [
        {
            "type": "tool_call",
            "tool_name": "recall_device_memory",
            "tool_input": "{}",
        },
        {
            "type": "tool_result",
            "tool_name": "recall_device_memory",
            "content": "[Large tool result omitted from public history (8406 chars)]",
        },
        {"type": "assistant", "content": "I could not verify the recalled memory."},
    ]

    result = evaluate_task_history(
        suite=suite,
        task=task,
        history=history,
        attempt=1,
        artifact_dir=tmp_path / "artifacts",
        judge_cfg=None,
        judge_cache_dir=None,
        run_id="run-1",
        timed_out=False,
        metrics={},
        episode={"retrieved_memory_refs": ["memory-created-by-consolidation"]},
    )

    assert result.status == "judge_error"
    assert result.metrics["memory_recall_evidence_source"] == "unavailable"
    assert result.hard_assertions.expected_recalled_memory is None


def _memory_suite_and_task(tmp_path: Path):
    suite = Suite(
        name="persona",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="personamem_case",
        category="memory",
        description_for_judge="Recall the expected memory.",
        prompt="What do I prefer?",
        rubric=[RubricItem(id="uses_memory", check="Uses the memory.")],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=3),
        expected_recalled_memory_ids=["personamem_music_expression"],
    )
    return suite, task


def _compressed_recall_history(episode_id: str = "ep-1"):
    return [
        {
            "type": "tool_call",
            "tool_name": "recall_memory",
            "tool_input": "{}",
            "episode_id": episode_id,
        },
        {
            "type": "tool_result",
            "tool_name": "recall_memory",
            "content": "[Large tool result omitted from public history (8406 chars)]",
            "episode_id": episode_id,
        },
        {
            "type": "assistant",
            "content": "I used the stored preference.",
            "episode_id": episode_id,
        },
    ]


def _compressed_recall_and_device_history(episode_id: str = "ep-1"):
    recall_history = _compressed_recall_history(episode_id)
    return recall_history[:-1] + [
        {
            "type": "tool_call",
            "tool_name": "recall_device_memory",
            "tool_input": "{}",
            "episode_id": episode_id,
        },
        {
            "type": "tool_result",
            "tool_name": "recall_device_memory",
            "content": '{"results":[]}',
            "episode_id": episode_id,
        },
        recall_history[-1],
    ]


def test_compressed_recall_with_episode_continues_to_judge_and_persists_evidence(
    tmp_path: Path,
    monkeypatch,
):
    from runner.runtask import evaluate_task_history

    suite, task = _memory_suite_and_task(tmp_path)
    judge_called = []

    def fake_judge_task(**kwargs):
        judge_called.append(kwargs)
        return JudgeOutput(
            verdicts=[RubricVerdict(id="uses_memory", verdict="yes", reason="used")],
            overall_notes="",
            cache_key="k",
            raw_response="{}",
        )

    monkeypatch.setattr(runtask_mod, "judge_task", fake_judge_task)

    result = evaluate_task_history(
        suite=suite,
        task=task,
        history=_compressed_recall_history(),
        episode={
            "id": "ep-1",
            "retrieved_memory_refs": ["personamem_music_expression"],
        },
        attempt=1,
        artifact_dir=tmp_path / "artifacts",
        judge_cfg=JudgeConfig(),
        judge_cache_dir=None,
        run_id="run-1",
        timed_out=False,
    )

    assert result.status == "passed"
    assert len(judge_called) == 1
    assert result.metrics["recalled_memory_ids"] == ["personamem_music_expression"]
    assert result.metrics["memory_recall_evidence_source"] == "episode"
    assert result.metrics["expected_recalled_memory_match"] is True
    trace = json.loads((tmp_path / "artifacts" / "trace.json").read_text())
    assert trace["recalled_memory_ids"] == ["personamem_music_expression"]
    assert trace["memory_recall_evidence_source"] == "episode"
    assert trace["expected_recalled_memory_match"] is True


def test_episode_missing_expected_memory_is_real_failure(tmp_path: Path):
    from runner.runtask import evaluate_task_history

    suite, task = _memory_suite_and_task(tmp_path)
    result = evaluate_task_history(
        suite=suite,
        task=task,
        history=_compressed_recall_history(),
        episode={"id": "ep-1", "retrieved_memory_refs": ["other-memory"]},
        attempt=1,
        artifact_dir=tmp_path / "artifacts",
        judge_cfg=None,
        judge_cache_dir=None,
        run_id="run-1",
        timed_out=False,
    )

    assert result.status == "failed"
    assert result.metrics["memory_recall_evidence_source"] == "episode"
    assert result.metrics["expected_recalled_memory_match"] is False


def test_episode_authoritative_empty_memory_refs_is_real_failure(tmp_path: Path):
    from runner.runtask import evaluate_task_history

    suite, task = _memory_suite_and_task(tmp_path)
    result = evaluate_task_history(
        suite=suite,
        task=task,
        history=_compressed_recall_history(),
        episode={"id": "ep-1"},
        attempt=1,
        artifact_dir=tmp_path / "artifacts",
        judge_cfg=None,
        judge_cache_dir=None,
        run_id="run-1",
        timed_out=False,
    )

    assert result.status == "failed"
    assert result.metrics["recalled_memory_ids"] == []
    assert result.metrics["memory_recall_evidence_source"] == "episode"
    assert result.metrics["expected_recalled_memory_match"] is False


def test_ambiguous_episode_empty_memory_refs_is_real_failure(tmp_path: Path):
    from runner.runtask import evaluate_task_history

    suite, task = _memory_suite_and_task(tmp_path)
    result = evaluate_task_history(
        suite=suite,
        task=task,
        history=_compressed_recall_and_device_history(),
        episode={"id": "ep-1", "retrieved_memory_refs": []},
        attempt=1,
        artifact_dir=tmp_path / "artifacts",
        judge_cfg=None,
        judge_cache_dir=None,
        run_id="run-1",
        timed_out=False,
    )

    assert result.status == "failed"
    assert result.metrics["recalled_memory_ids"] == []
    assert result.metrics["memory_recall_evidence_source"] == "episode"
    assert result.metrics["expected_recalled_memory_match"] is False


def test_episode_missing_one_expected_memory_reports_attributable_recall(tmp_path: Path):
    from runner.runtask import evaluate_task_history

    suite, task = _memory_suite_and_task(tmp_path)
    task.expected_recalled_memory_ids = ["memory-a", "memory-b"]
    result = evaluate_task_history(
        suite=suite,
        task=task,
        history=_compressed_recall_and_device_history(),
        episode={"id": "ep-1", "retrieved_memory_refs": ["memory-a"]},
        attempt=1,
        artifact_dir=tmp_path / "artifacts",
        judge_cfg=None,
        judge_cache_dir=None,
        run_id="run-1",
        timed_out=False,
    )

    assert result.status == "failed"
    assert result.metrics["recalled_memory_ids"] == ["memory-a"]
    assert result.metrics["memory_recall_evidence_source"] == "episode"
    assert result.metrics["expected_recalled_memory_match"] is False


def test_compressed_recall_without_episode_is_judge_error(tmp_path: Path):
    from runner.runtask import evaluate_task_history

    suite, task = _memory_suite_and_task(tmp_path)
    result = evaluate_task_history(
        suite=suite,
        task=task,
        history=_compressed_recall_history(),
        episode=None,
        attempt=1,
        artifact_dir=tmp_path / "artifacts",
        judge_cfg=None,
        judge_cache_dir=None,
        run_id="run-1",
        timed_out=False,
    )

    assert result.status == "judge_error"
    assert result.hard_assertions.expected_recalled_memory is None
    assert result.metrics["memory_recall_evidence_source"] == "unavailable"
    assert result.metrics["expected_recalled_memory_match"] is None
    trace = json.loads((tmp_path / "artifacts" / "trace.json").read_text())
    assert trace["recalled_memory_ids"] == []
    assert trace["memory_recall_evidence_source"] == "unavailable"
    assert trace["expected_recalled_memory_match"] is None


def test_missing_recall_call_fails_even_when_episode_contains_expected_id(tmp_path: Path):
    from runner.runtask import evaluate_task_history

    suite, task = _memory_suite_and_task(tmp_path)
    task.hard_assertions.min_tool_calls = 0
    history = [
        {
            "type": "assistant",
            "content": "I recalled personamem_music_expression.",
            "episode_id": "ep-1",
        }
    ]
    result = evaluate_task_history(
        suite=suite,
        task=task,
        history=history,
        episode={"retrieved_memory_refs": ["personamem_music_expression"]},
        attempt=1,
        artifact_dir=tmp_path / "artifacts",
        judge_cfg=None,
        judge_cache_dir=None,
        run_id="run-1",
        timed_out=False,
    )

    assert result.status == "failed"
    assert result.metrics["recalled_memory_ids"] == []
    assert result.metrics["expected_recalled_memory_match"] is False
    assert "No recall_memory call" in result.hard_assertion_failures[-1].actual


class EpisodeClient(FakeClient):
    def __init__(self, *, inline_content, episode=None, episode_error=None):
        super().__init__("done")
        self.inline_content = inline_content
        self.episode = episode
        self.episode_error = episode_error
        self.episode_requests = []

    def chat(self, message, timeout_sec=None, attachments=None, skills=None):
        self.messages.append(message)
        return ChatResponse(
            response="done",
            episode_id="ep/one",
            history=[
                {
                    "type": "tool_call",
                    "tool_name": "recall_memory",
                    "tool_input": "{}",
                    "episode_id": "ep/one",
                },
                {
                    "type": "tool_result",
                    "tool_name": "recall_memory",
                    "content": self.inline_content,
                    "episode_id": "ep/one",
                },
                {"type": "assistant", "content": "done", "episode_id": "ep/one"},
            ],
        )

    def get_episode(self, episode_id):
        self.episode_requests.append(episode_id)
        if self.episode_error is not None:
            raise self.episode_error
        return self.episode


def test_run_one_task_fetches_unique_episode_and_saves_it(tmp_path: Path):
    suite, task = _memory_suite_and_task(tmp_path)
    episode = {
        "id": "ep/one",
        "retrieved_memory_refs": ["personamem_music_expression"],
    }
    client = EpisodeClient(
        inline_content="[Large tool result omitted from public history (8406 chars)]",
        episode=episode,
    )

    result = run_one_task(
        client,
        suite,
        task,
        1,
        tmp_path / "artifacts",
        None,
        None,
        "run-1",
    )

    assert result.status == "passed"
    assert client.episode_requests == ["ep/one"]
    assert json.loads((tmp_path / "artifacts" / "episode.json").read_text()) == episode
    assert result.metrics["episode_id"] == "ep/one"
    assert result.metrics["memory_recall_evidence_source"] == "episode"


def test_run_one_task_uses_response_episode_with_context_backed_history(tmp_path: Path):
    suite, task = _memory_suite_and_task(tmp_path)

    class ContextClient(EpisodeClient):
        def chat(self, *args, **kwargs):
            response = super().chat(*args, **kwargs)
            for message in response.history:
                message.pop("episode_id", None)
            response.episode_id = "ep-current"
            return response

    client = ContextClient(
        inline_content="[Large tool result omitted from public history (8406 chars)]",
        episode={"id": "ep-current", "retrieved_memory_refs": ["personamem_music_expression"]},
    )
    result = run_one_task(
        client, suite, task, 1, tmp_path / "artifacts", None, None, "run-1"
    )
    assert result.status == "passed"
    assert client.episode_requests == ["ep-current"]
    assert result.metrics["episode_id"] == "ep-current"


def test_run_one_task_does_not_replace_missing_request_id_with_old_episode(tmp_path: Path):
    suite, task = _memory_suite_and_task(tmp_path)

    class UncorrelatedClient(EpisodeClient):
        def chat(self, *args, **kwargs):
            response = super().chat(*args, **kwargs)
            response.episode_id = None
            return response

    client = UncorrelatedClient(inline_content="{}")
    result = run_one_task(
        client, suite, task, 1, tmp_path / "artifacts", None, None, "run-1"
    )
    assert client.episode_requests == []
    assert not result.metrics.get("episode_id")


def test_run_one_task_correlates_timeout_before_recovery(tmp_path: Path):
    suite, task = _memory_suite_and_task(tmp_path)
    events = []

    class TimeoutClient(EpisodeClient):
        def chat(self, *args, **kwargs):
            events.append("chat")
            raise AgentTimeoutError("timed out", request_id="req-running")

        def get_history(self):
            return []

        def chat_episode_id(self, request_id):
            assert request_id == "req-running"
            events.append("correlate")
            return "ep-running"

        def recover_after_timeout(self, **kwargs):
            events.append("recover")
            return True

    client = TimeoutClient(inline_content="", episode={"id": "ep-running"})
    result = run_one_task(
        client, suite, task, 1, tmp_path / "artifacts", None, None, "run-1"
    )
    # Isolation recovers before starting chat; timeout correlation must happen
    # before the second recovery, which can clear the active request evidence.
    assert events == ["recover", "chat", "correlate", "recover"]
    assert client.episode_requests == ["ep-running"]
    assert result.metrics["episode_id"] == "ep-running"
    assert result.metrics["recovery_succeeded"] is True


def test_run_one_task_complete_inline_result_still_fetches_episode_metrics(tmp_path: Path):
    suite, task = _memory_suite_and_task(tmp_path)
    client = EpisodeClient(
        inline_content=json.dumps(
            {"results": [{"id": "personamem_music_expression"}]}
        ),
        episode_error=RuntimeError("episode unavailable"),
    )

    result = run_one_task(
        client,
        suite,
        task,
        1,
        tmp_path / "artifacts",
        None,
        None,
        "run-1",
    )

    assert result.status == "passed"
    assert result.metrics["memory_recall_evidence_source"] == "inline"
    assert client.episode_requests == ["ep/one"]
    assert "episode unavailable" in result.metrics["episode_error"]
    assert not (tmp_path / "artifacts" / "episode.json").exists()


def test_run_one_task_without_memory_id_assertion_fetches_episode_metrics(
    tmp_path: Path,
):
    suite, task = _memory_suite_and_task(tmp_path)
    task.expected_recalled_memory_ids = []
    client = EpisodeClient(
        inline_content="[Large tool result omitted from public history (8406 chars)]",
        episode={"id": "ep/one", "retrieved_memory_refs": []},
    )

    result = run_one_task(
        client,
        suite,
        task,
        1,
        tmp_path / "artifacts",
        None,
        None,
        "run-1",
    )

    assert result.status == "passed"
    assert client.episode_requests == ["ep/one"]
    assert json.loads((tmp_path / "artifacts" / "episode.json").read_text())["id"] == "ep/one"


def test_run_one_task_merges_authoritative_episode_telemetry(tmp_path: Path):
    suite, task = _memory_suite_and_task(tmp_path)
    task.expected_recalled_memory_ids = []
    client = EpisodeClient(
        inline_content="{}",
        episode={
            "id": "ep/one",
            "extra": {
                "prompt_tokens": 101,
                "completion_tokens": 22,
                "total_tokens": 123,
                "first_token_time_ms": 45,
            },
            "events": [
                {
                    "type": "tool_result",
                    "tool_name": "touch_gesture",
                    "tool_input": '{"type":"tap"}',
                    "duration_ms": 17,
                    "is_error": True,
                }
            ],
        },
    )

    result = run_one_task(
        client,
        suite,
        task,
        1,
        tmp_path / "artifacts",
        None,
        None,
        "run-1",
    )

    assert result.metrics["input_tokens"] == 101
    assert result.metrics["output_tokens"] == 22
    assert result.metrics["total_tokens"] == 123
    assert result.metrics["time_to_first_token_ms"] == 45
    assert result.metrics["device_execution_ms"] == 17
    assert result.metrics["tool_errors"] == 1
    assert result.metrics["first_failure_stage"] is None
    assert result.metrics["failure_event_ref"] is None


def test_run_one_task_episode_fetch_failure_with_compressed_result_is_judge_error(
    tmp_path: Path,
):
    suite, task = _memory_suite_and_task(tmp_path)
    client = EpisodeClient(
        inline_content="[Large tool result omitted from public history (8406 chars)]",
        episode_error=RuntimeError("episode unavailable"),
    )

    result = run_one_task(
        client,
        suite,
        task,
        1,
        tmp_path / "artifacts",
        None,
        None,
        "run-1",
    )

    assert result.status == "judge_error"
    assert result.metrics["memory_recall_evidence_source"] == "unavailable"
    assert result.metrics["expected_recalled_memory_match"] is None
    assert "episode unavailable" in result.metrics["episode_error"]


def test_run_one_task_applies_suite_prompt_prefix(tmp_path: Path):
    suite = Suite(
        name="persona",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
        prompt_prefix="You must call recall_memory before answering.",
    )
    task = TaskSpec(
        id="personamem_case",
        category="memory",
        description_for_judge="Choose the personalized option.",
        prompt="Choose one option.",
        rubric=[RubricItem(id="chooses_correct", check="Agent chooses correct option.")],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=0),
    )
    client = FakeClient("ok")

    result = run_one_task(
        client, suite, task, 1, tmp_path / "artifacts", None, None, "run-1"
    )

    assert result.status == "passed"
    assert client.messages == [
        "You must call recall_memory before answering.\n\nChoose one option."
    ]


def test_run_one_task_requests_active_skill_and_marks_trace_observation(tmp_path: Path):
    suite = Suite(
        name="phone",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
        trace_observations=[
            TraceObservationSpec(
                id="skill_read_device_operator",
                description="Loaded device-operator skill.",
                skill_name="device-operator",
            )
        ],
    )
    task = TaskSpec(
        id="open_settings",
        category="single_step",
        description_for_judge="Open Settings.",
        prompt="open settings",
        rubric=[RubricItem(id="done", check="Done.")],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=0),
    )
    client = FakeClient("ok")

    result = run_one_task(
        client,
        suite,
        task,
        1,
        tmp_path / "artifacts",
        None,
        None,
        "run-1",
        active_skills=["device-operator"],
    )

    assert client.skill_requests == [["device-operator"]]
    assert result.metrics["active_skills"] == ["device-operator"]
    obs = result.metrics["trace_observations"][0]
    assert obs["id"] == "skill_read_device_operator"
    assert obs["passed"] is True
    assert "requested active skill" in obs["reason"]


class TimeoutClient(FakeClient):
    def __init__(self):
        super().__init__()
        self.history = [
            {"type": "tool_call", "tool_name": "screenshot", "tool_input": "{}"},
            {"type": "tool_result", "content": "{}"},
        ]

    def chat(self, message, timeout_sec=None, attachments=None, skills=None):
        raise AgentTimeoutError("deadline exceeded")

    def get_history(self):
        return self.history


def test_run_one_task_preserves_history_after_timeout(tmp_path: Path):
    suite = Suite(
        name="phone",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="slow_task",
        category="phone",
        description_for_judge="Times out after using a tool.",
        prompt="do something slow",
        rubric=[RubricItem(id="done", check="Done.")],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=4),
    )

    result = run_one_task(
        TimeoutClient(), suite, task, 1, tmp_path / "artifacts", None, None, "run-1"
    )

    assert result.status == "timeout"
    assert result.metrics["tool_calls"] == 1
    assert result.metrics["recovery_attempted"] is True
    assert result.metrics["recovery_succeeded"] is True
    history = json.loads((tmp_path / "artifacts" / "history.json").read_text())
    assert history[0]["tool_name"] == "screenshot"


def test_run_one_task_excludes_timeout_without_execution_evidence(tmp_path: Path):
    class EmptyTimeoutClient(TimeoutClient):
        def __init__(self):
            super().__init__()
            self.history = []

    suite = Suite(
        name="phone",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="provider_timeout",
        category="phone",
        description_for_judge="Provider times out before execution.",
        prompt="do something",
        rubric=[],
        hard_assertions=HardAssertions(),
    )

    result = run_one_task(
        EmptyTimeoutClient(), suite, task, 1, tmp_path / "artifacts", None, None, "run-1"
    )

    assert result.status == "timeout"
    assert result.metrics["success"] is None
    assert result.metrics["agent_eligible"] is False
    assert result.metrics["failure_class"] == "unknown"


class SummaryHistoryClient(FakeClient):
    """Mimics the current agent: history omits base64 image data and the
    tool_result content is a plain text summary instead."""

    def chat(self, message, timeout_sec=None, attachments=None, skills=None):
        self.messages.append(message)
        return ChatResponse(
            response="done",
            history=[
                {"type": "tool_call", "tool_name": "screenshot", "tool_input": "{}"},
                {"type": "tool_result", "tool_name": "screenshot",
                 "content": ("screenshot returned a screenshot observation: "
                             "format=jpeg width=1080 height=2400 size=167770 bytes. "
                             "Image data omitted from text summary.")},
                {"type": "assistant", "content": "done"},
            ],
        )


def test_run_one_task_sends_live_post_screenshot_to_judge(tmp_path: Path, monkeypatch):
    suite = Suite(
        name="phone",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=tmp_path / "suite.json",
    )
    task = TaskSpec(
        id="open_settings",
        category="phone",
        description_for_judge="Open the Settings app.",
        prompt="open settings",
        rubric=[RubricItem(id="settings_open", check="Settings app is open.")],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=4),
    )

    captured: dict = {}

    def fake_judge_task(*, post_screenshot, **kwargs):
        captured["post_screenshot"] = post_screenshot
        return JudgeOutput(
            verdicts=[RubricVerdict(id="settings_open", verdict="yes", reason="visible")],
            overall_notes="",
            cache_key="k",
            raw_response="{}",
        )

    monkeypatch.setattr(runtask_mod, "judge_task", fake_judge_task)
    monkeypatch.setattr(runtask_mod, "prepare_task_isolation", lambda *args, **kwargs: None)

    capture_calls = []

    def fake_take_environment_screenshot(
        environment_url,
        out_path,
        benchmark_task_id=None,
    ):
        capture_calls.append((environment_url, out_path.name, benchmark_task_id))
        out_path.parent.mkdir(parents=True, exist_ok=True)
        out_path.write_bytes(b"jpeg")
        return (1, 1)

    monkeypatch.setattr(
        runtask_mod,
        "take_environment_screenshot",
        fake_take_environment_screenshot,
    )

    result = run_one_task(
        SummaryHistoryClient(), suite, task, 1, tmp_path / "artifacts",
        JudgeConfig(), None, "run-1",
        environment_url="http://127.0.0.1:19090",
        benchmark_task_id="suite.json:open_settings",
    )

    # The judge must receive a real, existing post-screenshot file even though
    # the history carried no embedded image data.
    assert captured["post_screenshot"] is not None
    assert captured["post_screenshot"].exists()
    assert captured["post_screenshot"] == tmp_path / "artifacts" / "post.jpg"
    assert capture_calls == [
        ("http://127.0.0.1:19090", "pre.jpg", "suite.json:open_settings"),
        ("http://127.0.0.1:19090", "post.jpg", "suite.json:open_settings"),
    ]
    assert result.status == "passed"
    assert result.rubric_pass_count == 1
