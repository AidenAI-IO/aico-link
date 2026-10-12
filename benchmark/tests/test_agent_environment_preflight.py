import pytest

from runner.agent_client import AgentRequestError, ToolInvokeResult
from runner.preflight import AgentEnvironmentPreflightError, preflight_agent_environment
import runner.preflight as preflight


@pytest.mark.parametrize("failure", [None, "provider", "request", "setup", "release"])
def test_worker_screenshot_probe_releases_route_and_preserves_failure(monkeypatch, failure):
    calls = []
    endpoint = "http://127.0.0.1:19090"

    def setup(url, *, task_id, timeout):
        calls.append(("setup", url, task_id, timeout))
        if failure == "setup":
            raise RuntimeError("setup unavailable")

    def release(url, *, task_id, timeout):
        calls.append(("release", url, task_id, timeout))
        if failure == "release":
            raise RuntimeError("release unavailable")

    class Client:
        def invoke_tool(self, name, args, *, timeout, benchmark_task_id):
            calls.append((name, args, benchmark_task_id, timeout))
            if failure == "request":
                raise AgentRequestError("request unavailable")
            return ToolInvokeResult(
                "provider connection refused" if failure == "provider" else "captured",
                failure == "provider", 1,
            )

    monkeypatch.setattr(preflight, "call_environment_setup", setup)
    monkeypatch.setattr(preflight, "call_environment_release", release)
    if failure:
        with pytest.raises(AgentEnvironmentPreflightError, match=failure):
            preflight_agent_environment(Client(), endpoint, task_id="task-1", timeout=7)
    else:
        preflight_agent_environment(Client(), endpoint, task_id="task-1", timeout=7)

    assert calls[0] == ("setup", endpoint, "task-1", 7)
    assert calls[-1] == ("release", endpoint, "task-1", 7)
    if failure != "setup":
        assert calls[1] == ("screenshot", {}, "task-1", 7)
    else:
        assert len(calls) == 2
