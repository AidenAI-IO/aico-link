import json
from types import SimpleNamespace

import pytest

from runner.agent_client import AgentRequestError, AgentTimeoutError, ChatResponse, ToolInvokeResult
from runner.reset import (
    ResetError,
    SetupAssertionError,
    call_environment_release,
    call_environment_setup,
    clear_stale_adb_android_owner,
    per_task_setup,
)


class FailingChatClient:
    def chat(self, message, timeout_sec=None):
        raise AgentRequestError("HTTP 500: missing auth")


class FailingClearHistoryClient:
    def chat(self, message, timeout_sec=None):
        return None

    def clear_history(self):
        raise AgentRequestError("HTTP 500: clear failed")


class TimeoutChatClient:
    def chat(self, message, timeout_sec=None):
        raise AgentTimeoutError("timed out")


class RecordingSetupClient:
    def __init__(self):
        self.calls = []

    def chat(self, message, timeout_sec=None):
        self.calls.append(("chat", message, timeout_sec))
        return ChatResponse(response="READY", history=[])

    def clear_history(self):
        self.calls.append(("clear_history",))

    def seed_episode(self, episode, timeout=30):
        self.calls.append(("seed_episode", episode, timeout))
        return {"status": "seeded", "id": episode["id"]}

    def seed_session_chunk(self, chunk, timeout=30):
        self.calls.append(("seed_session_chunk", chunk, timeout))
        self.seeded_chunk = dict(chunk)
        return {"status": "seeded", "session_id": chunk["session_id"]}

    def seed_memory(self, memory, timeout=30):
        self.calls.append(("seed_memory", memory, timeout))
        return {"status": "seeded", "id": memory["id"]}

    def process_episode_memory(self, episode_id, timeout=90):
        self.calls.append(("process_episode_memory", episode_id, timeout))
        return {"episode_id": episode_id, "status": "done", "memory_ids": ["devmem-1"]}

    def seed_notification(self, events, timeout=30):
        self.calls.append(("seed_notification", events, timeout))
        return {"status": "seeded", "context_ids": [str(index + 1) for index in range(len(events))]}

    def process_notification_memory(self, timeout=90):
        self.calls.append(("process_notification_memory", timeout))
        return {"memory_cursor": "1", "memory_ids": ["tmp_notification_1"]}

    def invoke_tool(self, name, args, timeout=90):
        self.calls.append(("invoke_tool", name, args, timeout))
        from runner.agent_client import ToolInvokeResult

        if name == "recall_session_chunks":
            return ToolInvokeResult(
                output=json.dumps({"results": [self.seeded_chunk]}),
                is_error=False,
                duration_ms=0,
            )
        return ToolInvokeResult(
            output='{"results":[{"id":"tmp_notification_1","memory_scope":"temporary"}]}',
            is_error=False,
            duration_ms=0,
        )


class TimeoutClearHistoryClient(RecordingSetupClient):
    def clear_history(self):
        raise AgentTimeoutError("clear timed out")


def test_agent_prompt_setup_wraps_chat_errors_as_reset_error():
    setup = {"type": "agent_prompt", "prompt": "remember this", "timeout_sec": 5}

    with pytest.raises(ResetError, match="setup agent_prompt failed"):
        per_task_setup(FailingChatClient(), setup)


def test_notification_setup_seeds_and_processes_fixture():
    client = RecordingSetupClient()
    setup = {
        "type": "seed_notification",
        "events": [{"title": "Package", "message": "Tomorrow"}],
        "consolidate": True,
        "expected_memory_count": 1,
        "expected_memory_scope": "temporary",
    }

    per_task_setup(client, setup)

    assert client.calls == [
        ("seed_notification", setup["events"], 90),
        ("process_notification_memory", 90),
        ("invoke_tool", "recall_memory", {"limit": 20}, 90),
    ]


def test_notification_scope_setup_uses_configured_query_and_covers_all_memory_ids():
    client = RecordingSetupClient()
    setup = {
        "type": "seed_notification",
        "events": [{"title": "Package", "message": "Tomorrow"}],
        "consolidate": True,
        "expected_memory_count": 25,
        "expected_memory_scope": "long_term",
        "expected_memory_query": {"types": ["fact"], "limit": 3},
    }

    def process_notification_memory(timeout=90):
        client.calls.append(("process_notification_memory", timeout))
        return {
            "memory_cursor": "1",
            "memory_ids": [f"mem-{index}" for index in range(25)],
        }

    def invoke_tool(name, args, timeout=90):
        client.calls.append(("invoke_tool", name, args, timeout))
        from runner.agent_client import ToolInvokeResult

        return ToolInvokeResult(
            output=json.dumps({
                "results": [
                    {"id": f"mem-{index}", "memory_scope": "long_term"}
                    for index in range(25)
                ]
            }),
            is_error=False,
            duration_ms=0,
        )

    client.process_notification_memory = process_notification_memory
    client.invoke_tool = invoke_tool
    per_task_setup(client, setup)
    assert client.calls[-1][2] == {"types": ["fact"], "limit": 25}


def test_setup_sequence_runs_existing_primitives_in_order():
    client = RecordingSetupClient()
    memory = {"id": "tmp-1", "content": "Existing conclusion"}
    event = {"title": "Update", "message": "New conclusion"}

    per_task_setup(
        client,
        [
            {"type": "seed_memory", "memories": [memory]},
            {"type": "seed_notification", "events": [event], "consolidate": False},
        ],
    )

    assert client.calls == [
        ("seed_memory", memory, 30),
        ("seed_notification", [event], 30),
    ]


def test_seed_session_chunk_setup_writes_chunk_without_clearing_history_by_default():
    client = RecordingSetupClient()
    setup = {
        "type": "seed_session_chunk",
        "session_id": "benchmark-session",
        "summary": "Seeded chunk summary",
        "messages": [{"role": "user", "content": "old question"}],
        "timeout_sec": 45,
    }

    per_task_setup(client, setup)

    assert client.calls == [
        (
            "seed_session_chunk",
            {
                "session_id": "benchmark-session",
                "summary": "Seeded chunk summary",
                "messages": [{"role": "user", "content": "old question"}],
            },
            45,
        ),
        ("invoke_tool", "recall_session_chunks", {"limit": 100}, 45),
    ]


def test_seed_session_chunk_setup_can_explicitly_clear_history_after_seed():
    client = RecordingSetupClient()
    setup = {
        "type": "seed_session_chunk",
        "session_id": "benchmark-session",
        "summary": "Seeded chunk summary",
        "messages": [{"role": "user", "content": "old question"}],
        "clear_history_after": True,
    }

    per_task_setup(client, setup)

    assert client.calls == [
        (
            "seed_session_chunk",
            {
                "session_id": "benchmark-session",
                "summary": "Seeded chunk summary",
                "messages": [{"role": "user", "content": "old question"}],
            },
            30,
        ),
        ("clear_history",),
        ("invoke_tool", "recall_session_chunks", {"limit": 100}, 30),
    ]


@pytest.mark.parametrize("result", [
    ToolInvokeResult('{"results":null}', False, 0),
    ToolInvokeResult('{"results":[]}', False, 0),
    ToolInvokeResult('{"results":[{"session_id":"other","summary":"expected"}]}', False, 0),
    ToolInvokeResult('{"results":[{"session_id":"seeded","summary":"wrong"}]}', False, 0),
    ToolInvokeResult('invalid json', False, 0),
    ToolInvokeResult('unavailable', True, 0),
])
def test_seed_session_chunk_rejects_unavailable_or_wrong_evidence(result):
    client = RecordingSetupClient()
    client.invoke_tool = lambda *args, **kwargs: result
    with pytest.raises(ResetError, match="seed_session_chunk verification"):
        per_task_setup(client, {
            "type": "seed_session_chunk", "session_id": "seeded", "summary": "expected",
            "messages": [{"role": "user", "content": "old question"}],
        })


def test_seed_session_chunk_setup_wraps_clear_history_timeout_as_reset_error():
    setup = {
        "type": "seed_session_chunk",
        "session_id": "benchmark-session",
        "summary": "Seeded chunk summary",
        "messages": [{"role": "user", "content": "old question"}],
        "clear_history_after": True,
    }

    with pytest.raises(ResetError, match="seed_session_chunk clear_history failed"):
        per_task_setup(TimeoutClearHistoryClient(), setup)


def test_setup_sequence_preserves_consolidation_result_when_later_setup_returns_none():
    client = RecordingSetupClient()
    episode = {"id": "ep-1", "user_goal": "verify a device procedure"}

    result = per_task_setup(
        client,
        [
            {"type": "seed_episode", "episode": episode, "consolidate": True},
            {"type": "agent_prompt", "prompt": "continue"},
        ],
    )

    assert result is not None
    assert result["consolidation"]["memory_ids"] == ["devmem-1"]


def test_setup_sequence_applies_expectation_only_to_matching_seed_episode():
    client = RecordingSetupClient()
    first = {"id": "ep-1", "user_goal": "first episode"}
    second = {"id": "ep-2", "user_goal": "second episode"}

    result = per_task_setup(
        client,
        [
            {"type": "seed_episode", "episode": first, "consolidate": False},
            {
                "type": "seed_episode",
                "episode": second,
                "consolidate": True,
                "consolidation_expectation": {"min_memory_ids": 1},
            },
            {"type": "agent_prompt", "prompt": "continue"},
        ],
        consolidation_expectation=SimpleNamespace(min_memory_ids=1),
    )

    assert result is not None
    assert result["episode_id"] == "ep-2"
    assert client.calls[:3] == [
        ("seed_episode", first, 30),
        ("seed_episode", second, 90),
        ("process_episode_memory", "ep-2", 90),
    ]


def test_assert_memory_mismatch_is_a_setup_assertion_failure():
    class MismatchClient(RecordingSetupClient):
        def invoke_tool(self, name, args, timeout=90):
            from runner.agent_client import ToolInvokeResult

            return ToolInvokeResult(
                output='{"results":[]}',
                is_error=False,
                duration_ms=0,
            )

    with pytest.raises(SetupAssertionError, match="expected 1, got 0"):
        per_task_setup(
            MismatchClient(),
            {"type": "assert_memory", "expected_count": 1},
        )


def test_assert_memory_rejects_malformed_nested_reference_before_recall():
    client = RecordingSetupClient()
    with pytest.raises(ResetError, match="event_ids_contains must be a list of non-empty strings"):
        per_task_setup(
            client,
            {
                "type": "assert_memory",
                "expected": [
                    {
                        "source_refs_contain": [
                            {"type": "notification", "event_ids_contains": "not-a-list"}
                        ]
                    }
                ],
            },
        )
    assert not any(call[0] == "invoke_tool" for call in client.calls)


def test_assert_memory_matches_source_reference_evidence():
    class SourceRefClient(RecordingSetupClient):
        def invoke_tool(self, name, args, timeout=90):
            from runner.agent_client import ToolInvokeResult

            return ToolInvokeResult(
                output=(
                    '{"results":[{"id":"m-1","source_refs":['
                    '{"type":"notification","id":"old","event_ids":["e-old"]},'
                    '{"type":"notification","id":"new","event_ids":["e-new"]}]}]}'
                ),
                is_error=False,
                duration_ms=0,
            )

    per_task_setup(
        SourceRefClient(),
        {
            "type": "assert_memory",
            "expected": [
                {
                    "id": "m-1",
                    "source_refs_contain": [
                        {"type": "notification", "id": "old", "event_ids_contains": ["e-old"]},
                        {"type": "notification", "id": "new", "event_ids_contains": ["e-new"]},
                    ],
                }
            ],
        },
    )


def test_setup_sequence_preserves_setup_assertion_failure_type():
    class MismatchClient(RecordingSetupClient):
        def invoke_tool(self, name, args, timeout=90):
            from runner.agent_client import ToolInvokeResult

            return ToolInvokeResult(
                output='{"results":[]}',
                is_error=False,
                duration_ms=0,
            )

    with pytest.raises(SetupAssertionError, match=r"setup\[1\] failed"):
        per_task_setup(
            MismatchClient(),
            [
                {"type": "seed_memory", "memories": [{"id": "m-1", "content": "x"}]},
                {"type": "assert_memory", "expected_count": 1},
            ],
        )


def test_notification_setup_rejects_non_boolean_consolidate():
    with pytest.raises(ResetError, match="consolidate must be boolean"):
        per_task_setup(
            RecordingSetupClient(),
            {"type": "seed_notification", "events": [{"title": "x"}], "consolidate": "false"},
        )


def test_agent_prompt_setup_wraps_timeouts_as_reset_error():
    setup = {"type": "agent_prompt", "prompt": "remember this", "timeout_sec": 5}

    with pytest.raises(ResetError, match="setup agent_prompt timed out"):
        per_task_setup(TimeoutChatClient(), setup)


def test_agent_prompt_setup_wraps_clear_history_errors_as_reset_error():
    setup = {"type": "agent_prompt", "prompt": "remember this", "timeout_sec": 5}

    with pytest.raises(ResetError, match="setup agent_prompt clear_history failed"):
        per_task_setup(FailingClearHistoryClient(), setup)


def test_agent_prompt_setup_clears_history_by_default():
    client = RecordingSetupClient()

    per_task_setup(client, {"type": "agent_prompt", "prompt": "remember this", "timeout_sec": 5})

    assert client.calls == [
        ("chat", "remember this", 5),
        ("clear_history",),
    ]


def test_agent_prompt_setup_can_make_history_clear_explicit():
    client = RecordingSetupClient()

    per_task_setup(
        client,
        {
            "type": "agent_prompt",
            "prompt": "remember this",
            "timeout_sec": 5,
            "clear_history_after": True,
        },
    )

    assert client.calls[-1] == ("clear_history",)


def test_agent_prompt_setup_includes_prompt_prefix():
    client = RecordingSetupClient()

    per_task_setup(
        client,
        {"type": "agent_prompt", "prompt": "prepare editor", "timeout_sec": 5},
        prompt_prefix="ADB benchmark rules",
    )

    assert client.calls[0] == ("chat", "ADB benchmark rules\n\nprepare editor", 5)


def test_agent_prompt_setup_validates_expected_response():
    client = RecordingSetupClient()

    per_task_setup(
        client,
        {
            "type": "agent_prompt",
            "prompt": "prepare editor",
            "expected_response": "READY",
            "clear_history_after": False,
        },
    )


def test_agent_prompt_setup_rejects_unexpected_response():
    class FailedSetupClient(RecordingSetupClient):
        def chat(self, message, timeout_sec=None):
            return ChatResponse(response="FAILED: capture failed", history=[])

    with pytest.raises(ResetError, match="response mismatch"):
        per_task_setup(
            FailedSetupClient(),
            {
                "type": "agent_prompt",
                "prompt": "prepare editor",
                "expected_response": "READY",
                "clear_history_after": False,
            },
        )


def test_seed_episode_setup_can_leave_episode_unconsolidated():
    client = RecordingSetupClient()
    episode = {"id": "ep-1", "user_goal": "verify a device procedure"}

    per_task_setup(
        client,
        {"type": "seed_episode", "episode": episode, "consolidate": False, "timeout_sec": 45},
    )

    assert client.calls == [("seed_episode", episode, 45)]


def test_seed_episode_setup_rejects_task_expectation_without_consolidation():
    client = RecordingSetupClient()

    with pytest.raises(ResetError, match="requires consolidate=true"):
        per_task_setup(
            client,
            {
                "type": "seed_episode",
                "episode": {"id": "ep-1", "user_goal": "verify a procedure"},
                "consolidate": False,
            },
            consolidation_expectation=object(),
        )

    assert client.calls == []


def test_seed_episode_setup_consolidates_and_requires_memory():
    client = RecordingSetupClient()
    episode = {"id": "ep-1", "user_goal": "verify a device procedure"}

    per_task_setup(
        client,
        {"type": "seed_episode", "episode": episode, "consolidate": True, "timeout_sec": 45},
    )

    assert client.calls == [
        ("seed_episode", episode, 45),
        ("process_episode_memory", "ep-1", 45),
    ]


def test_seed_episode_setup_returns_consolidation_artifact_and_accepts_empty_expected_result():
    class UnknownClient(RecordingSetupClient):
        def process_episode_memory(self, episode_id, timeout=90):
            return {
                "episode_id": episode_id,
                "status": "done",
                "assessment": {"goal_result": "unknown", "reason": "No proof", "evidence_refs": []},
                "memory_ids": [],
            }

    result = per_task_setup(
        UnknownClient(),
        {
            "type": "seed_episode",
            "episode": {"id": "ep-unknown", "user_goal": "verify a page"},
            "consolidate": True,
            "consolidation_expectation": {
                "goal_result": "unknown",
                "allow_empty_memory": True,
            },
        },
    )

    assert result["consolidation"]["assessment"]["goal_result"] == "unknown"
    assert result["consolidation"]["memory_ids"] == []


def test_seed_episode_setup_rejects_forbidden_generated_memory_content():
    class LeakingClient(RecordingSetupClient):
        def invoke_tool(self, name, args, timeout=90):
            self.calls.append(("invoke_tool", name, args, timeout))
            return ToolInvokeResult(
                output='{"results":[{"id":"devmem-1","content":"Saved exact value 913204"}]}',
                is_error=False,
                duration_ms=1,
            )

    with pytest.raises(ResetError, match="persisted forbidden value"):
        per_task_setup(
            LeakingClient(),
            {
                "type": "seed_episode",
                "episode": {"id": "ep-sensitive", "user_goal": "complete verification"},
                "consolidate": True,
                "consolidation_expectation": {
                    "allow_empty_memory": True,
                    "forbidden_memory_substrings": ["913204"],
                },
            },
        )


def test_seed_episode_setup_allows_generic_memory_without_forbidden_value():
    class SafeClient(RecordingSetupClient):
        def invoke_tool(self, name, args, timeout=90):
            self.calls.append(("invoke_tool", name, args, timeout))
            return ToolInvokeResult(
                output='{"results":[{"id":"devmem-1","content":"Verify the destination page after authentication."}]}',
                is_error=False,
                duration_ms=1,
            )

    result = per_task_setup(
        SafeClient(),
        {
            "type": "seed_episode",
            "episode": {"id": "ep-sensitive", "user_goal": "complete verification"},
            "consolidate": True,
            "consolidation_expectation": {
                "allow_empty_memory": True,
                "forbidden_memory_substrings": ["913204"],
            },
        },
    )

    assert result["consolidation"]["memory_ids"] == ["devmem-1"]


def test_seed_episode_setup_validates_generated_memory_content_type_and_scope():
    class InspectableClient(RecordingSetupClient):
        def invoke_tool(self, name, args, timeout=90):
            return ToolInvokeResult(
                output='{"results":[{"id":"devmem-1","type":"procedure","content":"Switch to Preview, save, then reopen to verify.","applicability":{"app_name":"QA Notes","app_version":"7"}}]}',
                is_error=False,
                duration_ms=1,
            )

    result = per_task_setup(
        InspectableClient(),
        {
            "type": "seed_episode",
            "episode": {"id": "ep-procedure", "user_goal": "persist a title"},
            "consolidate": True,
            "consolidation_expectation": {
                "required_memory_substrings": ["Preview", "reopen"],
                "required_memory_types": ["procedure"],
                "required_memory_scope": {"app_name": "QA Notes", "app_version": "7"},
            },
        },
    )

    assert result["consolidation"]["memory_ids"] == ["devmem-1"]


def test_seed_episode_setup_rejects_wrong_generated_memory_type():
    class WrongTypeClient(RecordingSetupClient):
        def invoke_tool(self, name, args, timeout=90):
            return ToolInvokeResult(
                output='{"results":[{"id":"devmem-1","type":"fact","content":"Preview then reopen","applicability":{"app_version":"7"}}]}',
                is_error=False,
                duration_ms=1,
            )

    with pytest.raises(ResetError, match="no single memory matching"):
        per_task_setup(
            WrongTypeClient(),
            {
                "type": "seed_episode",
                "episode": {"id": "ep-procedure", "user_goal": "persist a title"},
                "consolidate": True,
                "consolidation_expectation": {"required_memory_types": ["procedure"]},
            },
        )


def test_seed_episode_setup_requires_type_content_and_scope_on_same_memory():
    class SplitEvidenceClient(RecordingSetupClient):
        def process_episode_memory(self, episode_id, timeout=90):
            return {"episode_id": episode_id, "status": "done", "memory_ids": ["devmem-1", "devmem-2"]}

        def invoke_tool(self, name, args, timeout=90):
            memory_id = args["terms"][0]
            if memory_id == "devmem-1":
                item = {"id": memory_id, "type": "procedure", "content": "unrelated", "applicability": {"app_version": "7"}}
            else:
                item = {"id": memory_id, "type": "fact", "content": "Preview then reopen", "applicability": {"app_version": "8"}}
            return ToolInvokeResult(output=json.dumps({"results": [item]}), is_error=False, duration_ms=1)

    with pytest.raises(ResetError, match="no single memory matching"):
        per_task_setup(
            SplitEvidenceClient(),
            {
                "type": "seed_episode",
                "episode": {"id": "ep-split", "user_goal": "persist a title"},
                "consolidate": True,
                "consolidation_expectation": {
                    "required_memory_substrings": ["Preview", "reopen"],
                    "required_memory_types": ["procedure"],
                    "required_memory_scope": {"app_version": "7"},
                },
            },
        )


def test_seed_episode_setup_rejects_empty_memory_with_positive_contract_even_when_allowed():
    class NoMemoryPositiveContractClient(RecordingSetupClient):
        def process_episode_memory(self, episode_id, timeout=90):
            return {"episode_id": episode_id, "status": "done", "memory_ids": []}

    with pytest.raises(ResetError, match="produced no device memory"):
        per_task_setup(
            NoMemoryPositiveContractClient(),
            {
                "type": "seed_episode",
                "episode": {"id": "ep-empty-positive", "user_goal": "persist a title"},
                "consolidate": True,
                "consolidation_expectation": {
                    "allow_empty_memory": True,
                    "required_memory_types": ["procedure"],
                },
            },
        )


def test_seed_episode_setup_rejects_consolidation_without_memory():
    class NoMemoryClient(RecordingSetupClient):
        def process_episode_memory(self, episode_id, timeout=90):
            return {"episode_id": episode_id, "status": "done", "memory_ids": []}

    with pytest.raises(ResetError, match="produced no device memory"):
        per_task_setup(
            NoMemoryClient(),
            {
                "type": "seed_episode",
                "episode": {"id": "ep-1", "user_goal": "verify a device procedure"},
                "consolidate": True,
            },
        )


def test_seed_episode_setup_reports_ignored_status():
    class IgnoredClient(RecordingSetupClient):
        def process_episode_memory(self, episode_id, timeout=90):
            return {"episode_id": episode_id, "status": "ignored", "memory_ids": []}

    with pytest.raises(ResetError, match="was ignored by the worker"):
        per_task_setup(
            IgnoredClient(),
            {
                "type": "seed_episode",
                "episode": {"id": "ep-1", "user_goal": "verify a device procedure"},
                "consolidate": True,
            },
        )


@pytest.mark.parametrize(
    ("result", "expectation", "message"),
    [
        (
            {"episode_id": "ep-1", "status": "done", "memory_ids": ["devmem-1"]},
            {"expected_status": "ignored"},
            "status mismatch",
        ),
        (
            {"episode_id": "ep-1", "status": "ignored", "memory_ids": []},
            None,
            "was ignored by the worker",
        ),
        (
            {"episode_id": "ep-1", "status": "processing", "memory_ids": []},
            None,
            "did not reach a terminal status",
        ),
    ],
)
def test_seed_episode_setup_attaches_result_to_status_failures(
    result, expectation, message
):
    class StatusClient(RecordingSetupClient):
        def process_episode_memory(self, episode_id, timeout=90):
            return result

    setup = {
        "type": "seed_episode",
        "episode": {"id": "ep-1", "user_goal": "verify a device procedure"},
        "consolidate": True,
    }
    if expectation is not None:
        setup["consolidation_expectation"] = expectation

    with pytest.raises(ResetError, match=message) as exc_info:
        per_task_setup(StatusClient(), setup)

    assert exc_info.value.consolidation == result


def test_per_task_setup_rejects_unknown_keys():
    with pytest.raises(ResetError, match="unsupported seed_episode setup keys: consolodate"):
        per_task_setup(
            RecordingSetupClient(),
            {
                "type": "seed_episode",
                "episode": {"id": "ep-1", "user_goal": "verify a device procedure"},
                "consolodate": True,
            },
        )


@pytest.mark.parametrize("setup_type", [["seed_episode"], {"name": "seed_episode"}, 1, None])
def test_per_task_setup_rejects_non_string_setup_type(setup_type):
    with pytest.raises(ResetError, match="setup type must be a string"):
        per_task_setup(RecordingSetupClient(), {"type": setup_type})


def test_call_environment_setup_posts_to_api_setup(monkeypatch):
    seen = {}

    class FakeResponse:
        status = 200

        def read(self):
            return b'{"ok": true, "data": {"episode_id": "reset-1", "reset": true}}'

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(req, timeout=None):
        seen["url"] = req.full_url
        seen["method"] = req.get_method()
        seen["body"] = req.data
        seen["task_id"] = req.headers.get("Benchmark-task-id")
        seen["timeout"] = timeout
        return FakeResponse()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    result = call_environment_setup(
        "http://127.0.0.1:9090",
        timeout=12,
        app_ids=["settings"],
    )

    assert seen == {
        "url": "http://127.0.0.1:9090/api/setup",
        "method": "POST",
        "body": b'{"app_ids": ["settings"]}',
        "task_id": None,
        "timeout": 12,
    }
    assert result["data"]["episode_id"] == "reset-1"


def test_call_environment_setup_sends_foreground_app_id(monkeypatch):
    class FakeResponse:
        def read(self):
            return b'{"ok": true}'

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    bodies = []

    def fake_urlopen(req, timeout=None):
        bodies.append(json.loads(req.data))
        return FakeResponse()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)
    call_environment_setup("http://127.0.0.1:9090", app_ids=["settings"], foreground_app_id="scroll_lab")
    assert bodies == [{"app_ids": ["settings"], "foreground_app_id": "scroll_lab"}]


def test_call_environment_setup_sends_benchmark_task_id_header(monkeypatch):
    seen = {}

    class FakeResponse:
        status = 200

        def read(self):
            return b'{"ok": true}'

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(req, timeout=None):
        seen["task_id"] = req.headers.get("Benchmark-task-id")
        return FakeResponse()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    call_environment_setup("http://127.0.0.1:9090", task_id="clock.CountAlarms")

    assert seen["task_id"] == "clock.CountAlarms"


def test_call_environment_release_sends_benchmark_task_id_header(monkeypatch):
    seen = {}

    class FakeResponse:
        status = 200

        def read(self):
            return b'{"ok": true, "data": {"released": true}}'

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(req, timeout=None):
        seen["url"] = req.full_url
        seen["method"] = req.get_method()
        seen["body"] = req.data
        seen["task_id"] = req.headers.get("Benchmark-task-id")
        return FakeResponse()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    call_environment_release("http://127.0.0.1:9090", task_id="clock.CountAlarms")

    assert seen == {
        "url": "http://127.0.0.1:9090/api/release",
        "method": "POST",
        "body": b"{}",
        "task_id": "clock.CountAlarms",
    }


def test_clear_stale_adb_android_owner_releases_expired_owner(monkeypatch):
    calls = []

    class FakeResponse:
        def __init__(self, body):
            self.body = body

        def read(self):
            return self.body

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(req, timeout=None):
        calls.append((req.full_url, req.get_method(), req.headers.get("Benchmark-task-id"), timeout))
        if req.full_url.endswith("/health"):
            return FakeResponse(
                b'{"ok": true, "data": {"bridge_type": "adb_android", "active_task_id": "suite:old", "active_task_lease_state": "expired"}}'
            )
        return FakeResponse(b'{"ok": true, "data": {"released": true}}')

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    assert clear_stale_adb_android_owner("http://127.0.0.1:9090") == "suite:old"
    assert calls == [
        ("http://127.0.0.1:9090/health", "GET", None, 2.0),
        ("http://127.0.0.1:9090/api/release", "POST", "suite:old", 2),
    ]


def test_clear_stale_adb_android_owner_preserves_active_owner(monkeypatch):
    calls = []

    class FakeResponse:
        def read(self):
            return (
                b'{"ok": true, "data": {"bridge_type": "adb_android", '
                b'"active_task_id": "suite:running", "active_task_lease_state": "active"}}'
            )

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(req, timeout=None):
        calls.append((req.full_url, req.get_method(), req.headers.get("Benchmark-task-id"), timeout))
        return FakeResponse()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    assert clear_stale_adb_android_owner("http://127.0.0.1:9090") == ""
    assert calls == [("http://127.0.0.1:9090/health", "GET", None, 2.0)]


def test_clear_stale_adb_android_owner_preserves_owner_without_lease_state(monkeypatch):
    calls = []

    class FakeResponse:
        def read(self):
            return b'{"ok": true, "data": {"bridge_type": "adb_android", "active_task_id": "suite:running"}}'

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(req, timeout=None):
        calls.append((req.full_url, req.get_method(), req.headers.get("Benchmark-task-id"), timeout))
        return FakeResponse()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    assert clear_stale_adb_android_owner("http://127.0.0.1:9090") == ""
    assert calls == [("http://127.0.0.1:9090/health", "GET", None, 2.0)]


def test_clear_stale_adb_android_owner_ignores_non_adb_bridge(monkeypatch):
    calls = []

    class FakeResponse:
        def read(self):
            return b'{"ok": true, "data": {"bridge_type": "mobilegym", "active_task_id": "suite:old"}}'

        def __enter__(self):
            return self

        def __exit__(self, *exc):
            return False

    def fake_urlopen(req, timeout=None):
        calls.append((req.full_url, req.get_method()))
        return FakeResponse()

    monkeypatch.setattr("urllib.request.urlopen", fake_urlopen)

    assert clear_stale_adb_android_owner("http://127.0.0.1:9090") == ""
    assert calls == [("http://127.0.0.1:9090/health", "GET")]
