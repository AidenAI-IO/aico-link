import time

import pytest

from runner.agent_client import AgentRequestError, AgentTimeoutError
from runner.recovery import prepare_task_isolation, wait_for_agent_ready
from runner.reset import ResetError, SetupAssertionError
from runner.suite import HardAssertions, RubricItem, Suite, TaskSpec


class FlakyHealthClient:
    def __init__(self, ready_after: int):
        self.attempts = 0
        self.ready_after = ready_after
        self.clears = 0

    def health(self) -> bool:
        self.attempts += 1
        return self.attempts > self.ready_after

    def clear_history(self, timeout: int = 30) -> None:
        self.clears += 1

    def recover_after_timeout(self, timeout_sec: int = 90, poll_sec: float = 3.0) -> bool:
        return True

    def invoke_tool(self, name, args):
        raise AssertionError("unexpected tool call")


def test_wait_for_agent_ready_polls_until_health(monkeypatch):
    sleeps = []
    monkeypatch.setattr(time, "sleep", lambda seconds: sleeps.append(seconds))
    client = FlakyHealthClient(ready_after=2)

    assert wait_for_agent_ready(client, timeout_sec=10, poll_sec=1) is True
    assert client.attempts == 3
    assert sleeps == [1, 1]


def test_wait_for_agent_ready_times_out(monkeypatch):
    monkeypatch.setattr(time, "sleep", lambda _seconds: None)
    client = FlakyHealthClient(ready_after=99)

    assert wait_for_agent_ready(client, timeout_sec=0, poll_sec=1) is False


class SetupClient:
    def __init__(self, fail_clears: int = 0):
        self.fail_clears = fail_clears
        self.clears = 0
        self.chats = []
        self.seeded_memories = []

    def health(self) -> bool:
        return True

    def recover_after_timeout(self, timeout_sec: int = 90, poll_sec: float = 3.0) -> bool:
        return True

    def clear_history(self, timeout: int = 30) -> None:
        self.clears += 1
        if self.clears <= self.fail_clears:
            raise AgentTimeoutError("clear timed out")

    def invoke_tool(self, name, args):
        raise AssertionError("unexpected agent-side tool call")

    def chat(self, message, timeout_sec=None):
        self.chats.append((message, timeout_sec))

    def seed_memory(self, memory, timeout=30):
        self.seeded_memories.append((memory, timeout))
        return {"status": "seeded", "id": memory["id"]}


def test_prepare_task_isolation_retries_clear(monkeypatch):
    sleeps = []
    monkeypatch.setattr(time, "sleep", lambda seconds: sleeps.append(seconds))
    client = SetupClient(fail_clears=1)
    suite = Suite(
        name="phone",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=__import__("pathlib").Path("suite.json"),
    )
    task = TaskSpec(
        id="open_settings",
        category="single_step",
        description_for_judge="Open settings.",
        prompt="open settings",
        rubric=[RubricItem(id="ok", check="ok")],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=3),
    )

    prepare_task_isolation(client, suite, task, ready_timeout_sec=10, setup_attempts=3)

    assert client.clears == 2


def test_prepare_task_isolation_retries_clear_before_environment_setup(monkeypatch):
    setup_calls = []
    monkeypatch.setattr(time, "sleep", lambda _seconds: None)
    monkeypatch.setattr(
        "runner.recovery.call_environment_setup",
        lambda *args, **kwargs: setup_calls.append((args, kwargs)),
    )
    client = SetupClient(fail_clears=1)
    suite = Suite(
        name="phone",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=__import__("pathlib").Path("suite.json"),
    )
    task = TaskSpec(
        id="open_settings",
        category="single_step",
        description_for_judge="Open settings.",
        prompt="open settings",
        rubric=[RubricItem(id="ok", check="ok")],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=3),
    )

    prepare_task_isolation(
        client,
        suite,
        task,
        environment_url="http://127.0.0.1:9090",
        ready_timeout_sec=10,
        setup_attempts=3,
    )

    assert client.clears == 2
    assert len(setup_calls) == 1


def test_prepare_task_isolation_runs_agent_prompt_after_environment_setup(monkeypatch):
    setup_calls = []

    def fake_environment_setup(environment_url, task_id=None, timeout=30, app_ids=None, foreground_app_id=None,
                               initial_environment_state=None):
        setup_calls.append((environment_url, task_id, timeout, app_ids, foreground_app_id,
                            initial_environment_state))

    monkeypatch.setattr(
        "runner.recovery.call_environment_setup",
        fake_environment_setup,
    )
    client = SetupClient()
    suite = Suite(
        name="phone",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=__import__("pathlib").Path("suite.json"),
        prompt_prefix="ADB benchmark rules",
    )
    task = TaskSpec(
        id="open_settings",
        category="single_step",
        description_for_judge="Open settings.",
        prompt="open settings",
        rubric=[RubricItem(id="ok", check="ok")],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=3),
        setup={"type": "agent_prompt", "prompt": "open a settings sub-page", "timeout_sec": 45},
        app_ids=["settings"],
        foreground_app_id="settings",
        initial_environment_state={"apps": {"settings": {"searchQuery": ""}}},
    )

    prepare_task_isolation(
        client,
        suite,
        task,
        environment_url="http://127.0.0.1:9090",
        benchmark_task_id="suite.json:open_settings",
        ready_timeout_sec=10,
        setup_attempts=1,
    )

    assert setup_calls == [
        ("http://127.0.0.1:9090", "suite.json:open_settings", 180, ["settings"], "settings",
         task.initial_environment_state)
    ]
    assert client.chats == [("ADB benchmark rules\n\nopen a settings sub-page", 45)]
    assert client.clears == 2


def test_prepare_task_isolation_rebuilds_environment_after_agent_prompt_timeout(monkeypatch):
    events = []

    def fake_environment_setup(environment_url, task_id=None, timeout=30, app_ids=None, foreground_app_id=None):
        events.append(("environment_setup", task_id))

    class TimeoutThenSuccessClient(SetupClient):
        def __init__(self):
            super().__init__()
            self.chat_attempts = 0

        def recover_after_timeout(self, timeout_sec=90, poll_sec=3.0):
            events.append(("recover", timeout_sec))
            return True

        def clear_history(self, timeout=30):
            super().clear_history(timeout=timeout)
            events.append(("clear_history", self.clears))

        def chat(self, message, timeout_sec=None):
            self.chat_attempts += 1
            events.append(("chat", self.chat_attempts))
            if self.chat_attempts == 1:
                error = AgentTimeoutError("setup timed out")
                error.request_id = "setup-req-1"
                raise error
            self.chats.append((message, timeout_sec))

    monkeypatch.setattr("runner.recovery.call_environment_setup", fake_environment_setup)
    monkeypatch.setattr(time, "sleep", lambda _seconds: None)
    client = TimeoutThenSuccessClient()
    suite = Suite(
        name="phone",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=__import__("pathlib").Path("suite.json"),
    )
    task = TaskSpec(
        id="open_settings",
        category="single_step",
        description_for_judge="Open settings.",
        prompt="open settings",
        rubric=[RubricItem(id="ok", check="ok")],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=3),
        setup={"type": "agent_prompt", "prompt": "open a settings sub-page"},
    )

    prepare_task_isolation(
        client,
        suite,
        task,
        environment_url="http://127.0.0.1:9090",
        benchmark_task_id="suite.json:open_settings",
        ready_timeout_sec=10,
        setup_attempts=2,
    )

    assert events == [
        ("recover", 10),
        ("clear_history", 1),
        ("environment_setup", "suite.json:open_settings"),
        ("chat", 1),
        ("recover", 15),
        ("clear_history", 2),
        ("environment_setup", "suite.json:open_settings"),
        ("chat", 2),
        ("clear_history", 3),
    ]


def test_prepare_task_isolation_runs_seed_memory_with_environment_setup(monkeypatch):
    setup_calls = []

    def fake_environment_setup(environment_url, task_id=None, timeout=30, app_ids=None, foreground_app_id=None):
        setup_calls.append((environment_url, task_id, timeout, app_ids))

    monkeypatch.setattr(
        "runner.recovery.call_environment_setup",
        fake_environment_setup,
    )
    client = SetupClient()
    suite = Suite(
        name="memory",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=__import__("pathlib").Path("personamem.json"),
    )
    task = TaskSpec(
        id="recall_fact",
        category="memory",
        description_for_judge="Recall a seeded memory.",
        prompt="what do I like?",
        rubric=[RubricItem(id="ok", check="ok")],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=3),
        setup={
            "type": "seed_memory",
            "memories": [
                {
                    "id": "mem-1",
                    "content": "The user likes flashcards.",
                    "tags": ["study"],
                }
            ],
            "timeout_sec": 7,
        },
    )

    prepare_task_isolation(
        client,
        suite,
        task,
        environment_url="http://127.0.0.1:9090",
        benchmark_task_id="personamem.json:recall_fact",
        ready_timeout_sec=10,
        setup_attempts=1,
    )

    assert setup_calls == [
        ("http://127.0.0.1:9090", "personamem.json:recall_fact", 180, [])
    ]
    assert client.seeded_memories == [
        ({"id": "mem-1", "content": "The user likes flashcards.", "tags": ["study"]}, 7)
    ]


def test_prepare_task_isolation_does_not_retry_setup_assertion(monkeypatch):
    monkeypatch.setattr(time, "sleep", lambda _seconds: None)
    setup_calls = []

    def fail_assertion(*args, **kwargs):
        setup_calls.append((args, kwargs))
        raise SetupAssertionError("expected memory is absent")

    monkeypatch.setattr("runner.recovery.per_task_setup", fail_assertion)
    client = SetupClient()
    suite = Suite(
        name="memory",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=__import__("pathlib").Path("memory.json"),
    )
    task = TaskSpec(
        id="memory_update",
        category="memory",
        description_for_judge="Update memory.",
        prompt="check memory",
        rubric=[RubricItem(id="ok", check="ok")],
        hard_assertions=HardAssertions(),
        setup={"type": "assert_memory", "expected_count": 1},
    )

    with pytest.raises(SetupAssertionError, match="expected memory is absent"):
        prepare_task_isolation(
            client,
            suite,
            task,
            ready_timeout_sec=10,
            setup_attempts=3,
        )

    assert len(setup_calls) == 1
    assert client.clears == 1


def test_prepare_task_isolation_raises_after_exhausting_retries(monkeypatch):
    monkeypatch.setattr(time, "sleep", lambda _seconds: None)
    client = SetupClient(fail_clears=3)
    suite = Suite(
        name="phone",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=__import__("pathlib").Path("suite.json"),
    )
    task = TaskSpec(
        id="open_settings",
        category="single_step",
        description_for_judge="Open settings.",
        prompt="open settings",
        rubric=[RubricItem(id="ok", check="ok")],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=3),
    )

    with pytest.raises(AgentTimeoutError, match="clear timed out"):
        prepare_task_isolation(client, suite, task, ready_timeout_sec=10, setup_attempts=2)


def test_prepare_task_isolation_does_not_retry_consolidation_contract_failure(
    monkeypatch,
):
    setup_attempts = 0

    def fail_consolidation_contract(*args, **kwargs):
        nonlocal setup_attempts
        setup_attempts += 1
        error = ResetError("consolidation contract failed")
        error.consolidation = {"status": "done", "memory_ids": []}
        raise error

    monkeypatch.setattr("runner.recovery.per_task_setup", fail_consolidation_contract)
    client = SetupClient()
    suite = Suite(
        name="memory",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=__import__("pathlib").Path("suite.json"),
    )
    task = TaskSpec(
        id="reflection_contract",
        category="memory",
        description_for_judge="Validate reflection.",
        prompt="confirm",
        rubric=[RubricItem(id="ok", check="ok")],
        hard_assertions=HardAssertions(min_tool_calls=0, max_tool_calls=0),
        setup={"type": "seed_episode"},
    )

    with pytest.raises(ResetError, match="consolidation contract failed"):
        prepare_task_isolation(
            client,
            suite,
            task,
            ready_timeout_sec=10,
            setup_attempts=3,
        )

    assert setup_attempts == 1
    assert client.clears == 1


def test_prepare_task_isolation_does_not_repeat_failed_environment_setup(monkeypatch):
    setup_calls = 0

    def fail_environment_setup(environment_url, task_id=None, timeout=30, app_ids=None, foreground_app_id=None):
        nonlocal setup_calls
        setup_calls += 1
        raise ResetError("environment reset failed")

    monkeypatch.setattr("runner.recovery.call_environment_setup", fail_environment_setup)
    monkeypatch.setattr(time, "sleep", lambda _seconds: None)
    client = SetupClient()
    suite = Suite(
        name="phone",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=__import__("pathlib").Path("suite.json"),
    )
    task = TaskSpec(
        id="open_settings",
        category="single_step",
        description_for_judge="Open settings.",
        prompt="open settings",
        rubric=[RubricItem(id="ok", check="ok")],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=3),
    )

    with pytest.raises(ResetError, match="environment reset failed"):
        prepare_task_isolation(
            client,
            suite,
            task,
            environment_url="http://127.0.0.1:9090",
            ready_timeout_sec=10,
            setup_attempts=3,
        )

    assert setup_calls == 1


def test_prepare_task_isolation_raises_when_agent_never_ready(monkeypatch):
    monkeypatch.setattr(time, "sleep", lambda _seconds: None)

    class DownClient:
        def health(self) -> bool:
            return False

    suite = Suite(
        name="phone",
        global_reset={},
        tasks=[],
        sha256="sha",
        source_path=__import__("pathlib").Path("suite.json"),
    )
    task = TaskSpec(
        id="open_settings",
        category="single_step",
        description_for_judge="Open settings.",
        prompt="open settings",
        rubric=[RubricItem(id="ok", check="ok")],
        hard_assertions=HardAssertions(min_tool_calls=1, max_tool_calls=3),
    )

    with pytest.raises(ResetError, match="agent not ready"):
        prepare_task_isolation(DownClient(), suite, task, ready_timeout_sec=0)
