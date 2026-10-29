"""Identical business tools and resettable environments for native harnesses.

The daemon is a tool server for pi, never an agent delegated to by pi. Evaluation
state stays in the runner; it is not included in the task or tool catalog.
"""
from __future__ import annotations

import json
from pathlib import Path
import threading
import uuid

from runner.capture import take_environment_screenshot
from runner.environment_state import read_environment_state
from runner.harness import AidenAdapter, Execution, OpenClawAdapter, PiAdapter
from runner.reset import call_environment_release, call_environment_setup
from runner.suite import TaskSpec
from runner.trace import extract_trace


OPERATION_TOOLS = (
    "screenshot", "touch_gesture", "keyboard_tap", "enter_text",
    "mouse_move", "mouse_scroll", "wait_for_stable_screen",
)


def preparation_steps(setup) -> list[dict]:
    """Only device preparation that discards conversation can be shared fairly."""
    if not setup:
        return []
    steps = setup if isinstance(setup, list) else [setup]
    for step in steps:
        if (step.get("type") != "agent_prompt" or not step.get("prompt")
                or step.get("clear_history_after", True) is not True or step.get("evaluate")
                or set(step) - {"type", "prompt", "timeout_sec", "clear_history_after", "expected_response", "evaluate"}):
            raise ValueError("Comparison preparation supports only unevaluated agent_prompt steps with clear_history_after=true; memory seeding and retained context are agent-specific")
    return steps


class OperationAdapter:
    def __init__(self, *, daemon: AidenAdapter, environment_url: str,
                 pi: PiAdapter | None = None, openclaw: OpenClawAdapter | None = None,
                 global_reset: dict | None = None, prompt_prefix: str = ""):

        self.daemon, self.environment_url, self.pi = daemon, environment_url, pi
        self.openclaw = openclaw
        self.global_reset, self.prompt_prefix = global_reset, prompt_prefix

    def execute_operation(self, prompt: str, timeout: int, artifacts: Path,
                          stop: threading.Event, task: TaskSpec) -> Execution:
        route = "operation-" + uuid.uuid4().hex
        result = None
        artifacts.mkdir(parents=True, exist_ok=True)
        try:
            if stop.is_set():
                return Execution(canceled=True)
            setup = call_environment_setup(
                self.environment_url, timeout=120, task_id=route,
                app_ids=task.app_ids, foreground_app_id=task.foreground_app_id,
            )
            (artifacts / "setup.json").write_text(json.dumps(setup), encoding="utf-8")
            if stop.is_set():
                return Execution(canceled=True)
            # The same frozen preparer runs before either evaluated harness.
            # Separate daemons prevent preparation memory from leaking to Aiden.
            steps = preparation_steps(self.global_reset) + preparation_steps(task.setup)
            for index, step in enumerate(steps):
                directory = artifacts / "preparation" / str(index + 1)
                prepared = self.daemon.execute(
                    f"{self.prompt_prefix}\n\n{step['prompt']}".strip(), int(step.get("timeout_sec", 90)),
                    directory, stop, environment_url=self.environment_url,
                    benchmark_task_id=route, tool_names=OPERATION_TOOLS,
                )
                if prepared.canceled or stop.is_set():
                    return Execution(canceled=True)
                (directory / "history.json").write_text(json.dumps(prepared.history, ensure_ascii=False), encoding="utf-8")
                if prepared.timed_out or prepared.metrics.get("agent_error"):
                    raise ValueError("Shared environment preparation failed; inspect preparation artifacts")
                expected = step.get("expected_response")
                if expected is not None and extract_trace(prepared.history).final_response.strip() != expected:
                    raise ValueError("Shared environment preparation response did not match expected_response")
            pre = artifacts / "pre.jpg"
            take_environment_screenshot(self.environment_url, pre, benchmark_task_id=route)
            if task.environment_assertions:
                initial = read_environment_state(self.environment_url, benchmark_task_id=route)
                (artifacts / "initial_environment_state.json").write_text(json.dumps(initial, ensure_ascii=False), encoding="utf-8")
            result = self.daemon.execute(
                prompt, timeout, artifacts, stop, environment_url=self.environment_url,
                benchmark_task_id=route, pi=self.pi, tool_names=OPERATION_TOOLS,
                openclaw=self.openclaw,
            )
            result.pre_screenshot = pre
            if not result.canceled and task.environment_assertions:
                try:
                    state = read_environment_state(self.environment_url, benchmark_task_id=route)
                    (artifacts / "environment_state.json").write_text(json.dumps(state, ensure_ascii=False), encoding="utf-8")
                    result.environment_state = state
                except Exception as exc:
                    result.environment_state_error = str(exc)
            if not result.canceled:
                try:
                    post = artifacts / "post.jpg"
                    take_environment_screenshot(self.environment_url, post, benchmark_task_id=route)
                    result.post_screenshot = post
                except Exception as exc:
                    result.metrics["post_screenshot_error"] = str(exc)
            return result
        finally:
            # Daemon execution has already stopped before the route is released.
            # Even partially failed setup may have claimed a simulator slot.
            try:
                call_environment_release(self.environment_url, task_id=route)
            except Exception as exc:
                (artifacts / "release-error.txt").write_text(str(exc), encoding="utf-8")
                # Stop this experiment; continuing could reuse a dirty device.
                stop.set()
                if result is not None:
                    result.metrics["environment_release_error"] = str(exc)
