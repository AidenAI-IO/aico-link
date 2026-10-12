import json
import socket
import time
from unittest.mock import patch

import pytest

from runner.agent_client import (
    AgentClient,
    AgentRequestError,
    AgentSemanticError,
    AgentTimeoutError,
)


class FakeResponse:
    def __init__(self, status: int, body: dict):
        self.status = status
        self._body = json.dumps(body).encode("utf-8")

    def read(self):
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def _captured(seen, status=200, body=None):
    """Return a urlopen replacement that records the request and returns body."""
    def fake_urlopen(req, timeout=None):
        seen["url"] = req.full_url
        seen["method"] = req.get_method()
        seen["headers"] = {k.lower(): v for k, v in req.header_items()}
        try:
            seen["body"] = req.data.decode("utf-8") if req.data else ""
        except Exception:
            seen["body"] = ""
        seen["timeout"] = timeout
        return FakeResponse(status, body or {})
    return fake_urlopen


def test_clear_history_posts_correct_path():
    seen = {}
    client = AgentClient(base_url="http://test")
    with patch("urllib.request.urlopen", _captured(seen, body={"status": "ok"})):
        client.clear_history()
    assert seen["method"] == "POST"
    assert seen["url"].endswith("/api/clear")


def test_chat_returns_response_and_history():
    history = [{"type": "assistant", "content": "done"}]
    seen = {}
    client = AgentClient(base_url="http://test")
    with patch("urllib.request.urlopen",
               _captured(seen, body={"response": "ok", "history": history})):
        resp = client.chat("请打开设置")
    assert seen["method"] == "POST"
    assert seen["url"].endswith("/api/chat")
    assert "请打开" in seen["body"]
    assert resp.response == "ok"
    assert resp.history == history


def test_chat_long_polls_async_result_until_complete():
    history = [
        {"type": "user", "content": "请打开设置"},
        {"type": "assistant", "content": "done"},
    ]
    seen = []
    responses = [
        FakeResponse(200, {"request_id": "req-1"}),
        FakeResponse(200, {
            "status": "complete", "response": "done", "history": history,
            "messages": [{"type": "assistant", "episode_id": "ep-current"}],
        }),
    ]

    def fake_urlopen(req, timeout=None):
        seen.append((req.full_url, req.get_method(), timeout))
        return responses.pop(0)

    client = AgentClient(base_url="http://test")
    with patch("urllib.request.urlopen", fake_urlopen):
        resp = client.chat("请打开设置", timeout_sec=30)

    assert resp.response == "done"
    assert resp.history == history
    assert resp.episode_id == "ep-current"
    assert seen[0][0].endswith("/api/chat")
    assert seen[1][0].endswith("/api/chat/result?request_id=req-1&wait=true")
    assert seen[1][2] == 30


@pytest.mark.parametrize("messages, expected", [
    ([{"episode_id": "ep-current"}], "ep-current"),
    ([{"episode_id": "ep-current"}, {"episode_id": "ep-other"}], None),
    ([], None),
])
def test_chat_correlation_prefers_current_request_over_old_history(messages, expected):
    client = AgentClient(base_url="http://test")
    body = {
        "status": "complete", "response": "done", "messages": messages,
        "history": [{"episode_id": "ep-previous"}],
    }
    with patch("urllib.request.urlopen", _captured({}, body=body)):
        response = client._wait_for_chat_result("req-1")
    assert response.episode_id == expected


def test_chat_error_retains_execution_correlation():
    client = AgentClient(base_url="http://test")
    body = {
        "status": "error", "error": "provider failed",
        "messages": [{"episode_id": "ep-failed"}],
    }
    with patch("urllib.request.urlopen", _captured({}, body=body)), \
         pytest.raises(AgentRequestError) as exc_info:
        client._wait_for_chat_result("req-failed")
    assert exc_info.value.request_id == "req-failed"
    assert exc_info.value.episode_id == "ep-failed"


def test_chat_episode_id_reads_in_progress_request_without_waiting():
    client = AgentClient(base_url="http://test")
    seen = {}
    body = {"status": "running", "messages": [{"episode_id": "ep-running"}]}
    with patch("urllib.request.urlopen", _captured(seen, body=body)):
        assert client.chat_episode_id("req/one") == "ep-running"
    assert seen["url"].endswith("/api/chat/result?request_id=req%2Fone")
    assert seen["timeout"] == 5


def test_get_history_returns_current_history():
    history = [{"type": "tool_call", "tool_name": "screenshot"}]
    seen = {}
    client = AgentClient(base_url="http://test")
    with patch("urllib.request.urlopen", _captured(seen, body=history)):
        result = client.get_history()
    assert seen["method"] == "GET"
    assert seen["url"].endswith("/api/history")
    assert result == history


def test_get_episode_uses_episode_id_in_path_and_unwraps_episode():
    seen = {}
    episode_id = "ep_1786723200000000000_0123abcd"
    episode = {
        "id": episode_id,
        "retrieved_memory_refs": ["personamem_music_expression"],
    }
    client = AgentClient(base_url="http://test")

    with patch(
        "urllib.request.urlopen",
        _captured(seen, body={"episode": episode}),
    ):
        result = client.get_episode(episode_id)

    assert seen["method"] == "GET"
    assert seen["url"].endswith(f"/api/episodes/{episode_id}")
    assert result == episode


def test_device_type_reads_runtime_phone_bridge_status():
    seen = {}
    client = AgentClient(base_url="http://test")
    with patch(
        "urllib.request.urlopen",
        _captured(seen, body={"device_type": "Android"}),
    ):
        result = client.device_type()

    assert seen["method"] == "GET"
    assert seen["url"].endswith("/api/phone-bridge/status")
    assert result == "Android"


def test_chat_includes_skills_when_provided():
    seen = {}
    client = AgentClient(base_url="http://test")
    with patch("urllib.request.urlopen",
               _captured(seen, body={"response": "ok", "history": []})):
        client.chat("请打开设置", skills=["device-operator"])
    body = json.loads(seen["body"])
    assert body["skills"] == ["device-operator"]


def test_chat_non_200_error_preserves_request_id():
    seen = {}
    client = AgentClient(base_url="http://test")

    with patch("urllib.request.urlopen", _captured(seen, status=503)), \
         pytest.raises(AgentRequestError) as exc_info:
        client.chat("prepare device")

    request_id = json.loads(seen["body"])["request_id"]
    assert request_id.startswith("benchmark-")
    assert exc_info.value.request_id == request_id


@pytest.mark.parametrize(
    ("method_name", "transport_name", "expected_message"),
    [
        ("cancel_chat", "_post", "chat/cancel returned invalid JSON"),
        ("chat_result_status", "_get", "chat/result returned invalid JSON"),
    ],
)
def test_chat_recovery_rejects_invalid_json(
    method_name, transport_name, expected_message
):
    client = AgentClient(base_url="http://test")

    with patch.object(client, transport_name, return_value=(200, b"not-json")), \
         pytest.raises(AgentRequestError) as exc_info:
        getattr(client, method_name)("req-1")

    assert expected_message in str(exc_info.value)
    assert exc_info.value.request_id == "req-1"


def test_recover_after_timeout_waits_until_clear_succeeds(monkeypatch):
    sleeps = []
    monkeypatch.setattr(time, "sleep", lambda seconds: sleeps.append(seconds))

    class RecoverClient:
        def __init__(self):
            self.attempts = 0

        def health(self) -> bool:
            return True

        def clear_history(self, timeout: int = 30) -> None:
            self.attempts += 1
            if self.attempts < 2:
                raise AgentTimeoutError("busy")

    client = RecoverClient()
    assert AgentClient.recover_after_timeout(client, timeout_sec=10, poll_sec=1) is True
    assert client.attempts == 2
    assert sleeps == [1]


def test_recover_after_timeout_cancels_timed_out_chat_before_clear(monkeypatch):
    seen = []
    responses = [
        FakeResponse(200, {"request_id": "req-1"}),
        socket.timeout("read timed out"),
        FakeResponse(200, {"request_id": "req-1", "status": "canceled"}),
        FakeResponse(200, {"status": "running"}),
        FakeResponse(200, {"status": "error", "error": "request canceled"}),
        FakeResponse(200, {"status": "ok"}),
    ]

    def fake_urlopen(req, timeout=None):
        seen.append((req.full_url, req.get_method(), timeout))
        response = responses.pop(0)
        if isinstance(response, BaseException):
            raise response
        return response

    monkeypatch.setattr(time, "sleep", lambda _seconds: None)
    client = AgentClient(base_url="http://test")
    with patch("urllib.request.urlopen", fake_urlopen):
        with pytest.raises(AgentTimeoutError) as exc_info:
            client.chat("prepare device", timeout_sec=1)

        assert exc_info.value.request_id == "req-1"
        assert client.recover_after_timeout(timeout_sec=10, poll_sec=1) is True

    assert [entry[1] for entry in seen] == ["POST", "GET", "POST", "GET", "GET", "POST"]
    assert seen[2][0].endswith("/api/chat/cancel")
    assert seen[3][0].endswith("/api/chat/result?request_id=req-1")
    assert seen[4][0].endswith("/api/chat/result?request_id=req-1")
    assert seen[5][0].endswith("/api/clear")


def test_chat_timeout_raises():
    def fake_urlopen(req, timeout=None):
        raise socket.timeout("read timed out")
    client = AgentClient(base_url="http://test")
    with patch("urllib.request.urlopen", fake_urlopen), \
         pytest.raises(AgentTimeoutError):
        client.chat("hi", timeout_sec=1)


def test_invoke_tool_returns_output():
    seen = {}
    client = AgentClient(base_url="http://test")
    with patch("urllib.request.urlopen",
               _captured(seen, body={"output": "{}", "is_error": False, "duration_ms": 12})):
        out = client.invoke_tool("keyboard_tap", {"keys": ["escape"]})
    assert seen["url"].endswith("/api/tools/keyboard_tap")
    assert "escape" in seen["body"]
    assert seen["timeout"] == 90
    assert out.is_error is False
    assert out.duration_ms == 12


def test_invoke_tool_can_send_benchmark_task_id_header():
    seen = {}
    client = AgentClient(base_url="http://test")
    with patch("urllib.request.urlopen",
               _captured(seen, body={"output": "{}", "is_error": False, "duration_ms": 12})):
        client.invoke_tool("screenshot", {}, benchmark_task_id="suite.json:t1")

    assert seen["headers"]["benchmark-task-id"] == "suite.json:t1"


def test_seed_memory_sends_benchmark_token_header():
    seen = {}
    client = AgentClient(base_url="http://test", benchmark_token="seed-token")
    with patch("urllib.request.urlopen", _captured(seen, body={"status": "seeded", "id": "mem-1"})):
        result = client.seed_memory({"id": "mem-1", "content": "remember this"})

    assert seen["url"].endswith("/api/benchmark/seed_memory")
    assert seen["headers"]["authorization"] == "Bearer seed-token"
    assert result == {"status": "seeded", "id": "mem-1"}


def test_seed_episode_sends_benchmark_token_header():
    seen = {}
    client = AgentClient(base_url="http://test", benchmark_token="episode-token")
    episode = {"id": "ep-1", "user_goal": "verify a device procedure"}
    with patch("urllib.request.urlopen", _captured(seen, body={"status": "seeded", "id": "ep-1"})):
        result = client.seed_episode(episode)

    assert seen["url"].endswith("/api/benchmark/seed_episode")
    assert seen["headers"]["authorization"] == "Bearer episode-token"
    assert json.loads(seen["body"]) == episode
    assert result == {"status": "seeded", "id": "ep-1"}


def test_seed_session_chunk_sends_benchmark_token_header():
    seen = {}
    client = AgentClient(base_url="http://test", benchmark_token="chunk-token")
    chunk = {
        "session_id": "s-1",
        "summary": "old conversation",
        "messages": [{"role": "user", "content": "remember this"}],
    }
    with patch("urllib.request.urlopen", _captured(seen, body={"status": "seeded", "session_id": "s-1"})):
        result = client.seed_session_chunk(chunk, timeout=45)

    assert seen["url"].endswith("/api/benchmark/seed_session_chunk")
    assert seen["headers"]["authorization"] == "Bearer chunk-token"
    assert json.loads(seen["body"]) == chunk
    assert seen["timeout"] == 45
    assert result == {"status": "seeded", "session_id": "s-1"}


def test_process_episode_memory_sends_benchmark_token_header():
    seen = {}
    client = AgentClient(base_url="http://test", benchmark_token="episode-token")
    response = {"episode_id": "ep-1", "status": "done", "memory_ids": ["devmem-1"]}
    with patch("urllib.request.urlopen", _captured(seen, body=response)):
        result = client.process_episode_memory("ep-1", timeout=90)

    assert seen["url"].endswith("/api/benchmark/episode-memory/process")
    assert seen["headers"]["authorization"] == "Bearer episode-token"
    assert json.loads(seen["body"]) == {"episode_id": "ep-1"}
    assert seen["timeout"] == 90
    assert result == response


def test_seed_notification_sends_benchmark_token_header():
    seen = {}
    client = AgentClient(base_url="http://test", benchmark_token="notification-token")
    events = [{"title": "Package", "message": "Tomorrow"}]
    with patch(
        "urllib.request.urlopen",
        _captured(seen, body={"status": "seeded", "context_ids": ["1"]}),
    ):
        result = client.seed_notification(events)

    assert seen["url"].endswith("/api/benchmark/seed_notification")
    assert seen["headers"]["authorization"] == "Bearer notification-token"
    assert '"events"' in seen["body"]
    assert result == {"status": "seeded", "context_ids": ["1"]}


def test_process_notification_memory_sends_benchmark_token_header():
    seen = {}
    client = AgentClient(base_url="http://test", benchmark_token="notification-token")
    with patch(
        "urllib.request.urlopen",
        _captured(seen, body={"memory_cursor": "1", "memory_ids": ["tmp_notification_1"]}),
    ):
        result = client.process_notification_memory()

    assert seen["url"].endswith("/api/benchmark/notification-memory/process")
    assert seen["headers"]["authorization"] == "Bearer notification-token"
    assert result["memory_cursor"] == "1"


def test_http_422_raises_structured_semantic_error():
    class SemanticHTTPErrorResponse:
        def read(self):
            return b"invalid proposal"

        def close(self):
            pass

    def fail_urlopen(*args, **kwargs):
        import urllib.error

        raise urllib.error.HTTPError(
            "http://test/api/benchmark/notification-memory/process",
            422,
            "Unprocessable Entity",
            {},
            SemanticHTTPErrorResponse(),
        )

    client = AgentClient(base_url="http://test", benchmark_token="token")
    with patch("urllib.request.urlopen", fail_urlopen), pytest.raises(
        AgentSemanticError
    ) as exc_info:
        client.process_notification_memory()

    assert exc_info.value.status_code == 422


def test_set_phone_bridge_state_sends_benchmark_token_header():
    seen = {}
    client = AgentClient(base_url="http://test", benchmark_token="state-token")
    state = {
        "platform": "ios",
        "app_state": "background",
        "pip_bridge_enabled": True,
    }
    with patch(
        "urllib.request.urlopen",
        _captured(seen, body={"ok": True, "status": state}),
    ):
        result = client.set_phone_bridge_state(state)

    assert seen["url"].endswith("/api/benchmark/phone_bridge_state")
    assert seen["headers"]["authorization"] == "Bearer state-token"
    assert json.loads(seen["body"])["pip_bridge_enabled"] is True
    assert result["ok"] is True


def test_health_returns_true_when_tools_endpoint_ok():
    seen = {}
    client = AgentClient(base_url="http://test")
    with patch("urllib.request.urlopen", _captured(seen, body={"tools": []})):
        assert client.health() is True
    assert seen["url"].endswith("/api/tools")
    assert seen["method"] == "GET"
