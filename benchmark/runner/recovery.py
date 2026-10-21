from __future__ import annotations

import time
from typing import TYPE_CHECKING, Any

from runner.agent_client import AgentClient, AgentRequestError, AgentTimeoutError
from runner.reset import (
    ResetError,
    SetupAssertionError,
    call_environment_setup,
    per_task_setup,
)

if TYPE_CHECKING:
    from runner.suite import Suite, TaskSpec


DEFAULT_ENVIRONMENT_SETUP_TIMEOUT_SEC = 180


def recover_agent_after_timeout(
    client: AgentClient,
    timeout_sec: int = 90,
    poll_sec: float = 3.0,
) -> bool:
    return client.recover_after_timeout(timeout_sec=timeout_sec, poll_sec=poll_sec)


def wait_for_agent_ready(
    client: AgentClient,
    timeout_sec: int = 120,
    poll_sec: int = 3,
) -> bool:
    deadline = time.monotonic() + max(0, timeout_sec)
    while True:
        if client.health():
            return True
        now = time.monotonic()
        if now >= deadline:
            return False
        time.sleep(min(max(0, poll_sec), max(0, deadline - now)))


def prepare_task_isolation(
    client: AgentClient,
    suite: Suite,
    task: TaskSpec,
    *,
    environment_url: str | None = None,
    benchmark_task_id: str | None = None,
    ready_timeout_sec: int = 120,
    setup_attempts: int = 3,
) -> dict[str, Any] | None:
    if task.initial_environment_state and (not environment_url or task.input_screenshot):
        raise ResetError("initial_environment_state requires a live environment_url")
    if not wait_for_agent_ready(client, timeout_sec=ready_timeout_sec):
        raise ResetError(f"agent not ready within {ready_timeout_sec}s")
    if not recover_agent_after_timeout(client, timeout_sec=min(30, ready_timeout_sec)):
        raise ResetError("agent did not recover before task isolation")

    environment_setup_required = bool(environment_url and not task.input_screenshot)
    environment_setup_done = not environment_setup_required
    last_error: Exception | None = None
    setup_result: dict[str, Any] | None = None
    for attempt in range(1, setup_attempts + 1):
        try:
            client.clear_history()
        except (AgentTimeoutError, AgentRequestError) as e:
            last_error = e
            if attempt >= setup_attempts:
                break
            per_attempt_timeout = max(15, ready_timeout_sec // setup_attempts)
            wait_for_agent_ready(client, timeout_sec=per_attempt_timeout)
            time.sleep(1)
            continue

        if environment_setup_required and not environment_setup_done:
            try:
                call_environment_setup(
                    environment_url,
                    task_id=benchmark_task_id or task.id,
                    timeout=DEFAULT_ENVIRONMENT_SETUP_TIMEOUT_SEC,
                    app_ids=task.app_ids,
                    foreground_app_id=task.foreground_app_id,
                    **({"initial_environment_state": task.initial_environment_state}
                       if task.initial_environment_state else {}),
                )
            except (ResetError, AgentTimeoutError, AgentRequestError):
                raise
            environment_setup_done = True

        try:
            if not task.input_screenshot:
                setup_result = per_task_setup(
                    client,
                    task.setup,
                    prompt_prefix=suite.prompt_prefix,
                    consolidation_expectation=task.consolidation_expectation,
                )
            return setup_result
        except (ResetError, AgentTimeoutError, AgentRequestError) as e:
            last_error = e
            if isinstance(e, SetupAssertionError):
                raise
            if getattr(e, "consolidation", None) is not None:
                raise
            if attempt >= setup_attempts:
                break
            per_attempt_timeout = max(15, ready_timeout_sec // setup_attempts)
            if not recover_agent_after_timeout(
                client, timeout_sec=per_attempt_timeout
            ):
                raise ResetError(
                    "agent did not recover after per-task setup failure"
                ) from e
            environment_setup_done = not environment_setup_required
            time.sleep(1)

    if last_error is None:
        raise ResetError("task isolation failed for unknown reason")
    raise last_error
