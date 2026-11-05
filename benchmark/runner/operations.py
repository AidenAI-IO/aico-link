"""Identical business tools and resettable environments for native harnesses.

The daemon is a tool server for external harnesses, never an agent they delegate
to. The business tool catalog is the UI operation surface that Aiden's model
sees on the environment platform, derived from daemon tool metadata. Evaluation
state stays in the runner; it is not included in the task or tool catalog.
"""
from __future__ import annotations

import json
from pathlib import Path
import threading
from typing import Protocol
import uuid

from runner.capture import take_environment_screenshot
from runner.environment_state import read_environment_state
from runner.harness import AidenAdapter, Execution, ExternalHarness, record_catalog, refresh_gateway_evidence
from runner.raw_tools import RAW_TOOL_CATALOG, TOOL_LAYER as RAW_TOOL_LAYER, RawToolBackend
from runner.reset import call_environment_release, call_environment_setup, prepare_environment_tools
from runner.suite import SETUP_KEYS, TaskSpec
from runner.tool_gateway import ToolGateway
from runner.trace import extract_trace


# Shared preparation is an environment fixture run by Aiden, not a scored attempt;
# suite timeouts sized for a warm device are too short for a cold daemon.
PREPARATION_MIN_TIMEOUT_SEC = 120
AIDEN_TOOL_LAYER = "aiden"
TOOL_LAYERS = (AIDEN_TOOL_LAYER, RAW_TOOL_LAYER)
# Every derived catalog must contain these; suites may require only these.
REQUIRED_BUSINESS_TOOLS = (
    "screenshot", "touch_gesture", "keyboard_tap", "enter_text",
    "mouse_move", "mouse_scroll", "wait_for_stable_screen",
)


def business_tool_catalog(descriptors: list[dict], platform: str) -> list[dict]:
    """Select the daemon's model-visible UI operation tools for one platform."""
    if descriptors and not any("ui_operation" in tool for tool in descriptors):
        raise RuntimeError("The daemon image does not report UI tool metadata; rebuild the daemon image")
    catalog = [tool for tool in descriptors
               if tool.get("ui_operation") and tool.get("agent_exposed")
               and (not tool.get("agent_platforms") or platform in tool["agent_platforms"])]
    missing = set(REQUIRED_BUSINESS_TOOLS) - {tool["name"] for tool in catalog}
    if missing:
        raise RuntimeError(f"Shared business tools are missing: {sorted(missing)}")
    return catalog


def preparation_steps(setup) -> list[dict]:
    """Only device preparation that discards conversation can be shared fairly."""
    if not setup:
        return []
    steps = setup if isinstance(setup, list) else [setup]
    for step in steps:
        if step.get("type") == "environment_tools":
            if not step.get("tools") or set(step) - SETUP_KEYS["environment_tools"]:
                raise ValueError("Comparison environment_tools requires tools and optional expected_state")
            continue
        if step.get("type") == "agent_prompt" and step.get("device_prepare"):
            # Every harness here is driven by the daemon, which never calls the
            # bridge's prepare recipe; keeping the key would silently give the
            # comparison a different starting state than a normal run.
            raise ValueError(
                "Comparison preparation does not support device_prepare: it is a bridge "
                "recipe, and comparison preparation runs every harness through the agent"
            )
        if (step.get("type") != "agent_prompt" or not step.get("prompt")
                or step.get("clear_history_after", True) is not True or step.get("evaluate")
                or set(step) - (SETUP_KEYS["agent_prompt"] | {"evaluate"})):
            raise ValueError("Comparison preparation supports only unevaluated agent_prompt steps with clear_history_after=true; memory seeding and retained context are agent-specific")
    return steps


class OperationHarness(Protocol):
    def execute_operation(self, prompt: str, timeout: int, artifacts: Path,
                          stop: threading.Event, task: TaskSpec) -> Execution: ...


class OperationAdapter:
    def __init__(self, *, daemon: AidenAdapter, environment_url: str,
                 external: ExternalHarness | None = None, platform: str = "android",
                 tool_layer: str = AIDEN_TOOL_LAYER,
                 global_reset: dict | None = None, prompt_prefix: str = ""):
        if tool_layer not in TOOL_LAYERS or (tool_layer == RAW_TOOL_LAYER and external is None):
            raise ValueError("Raw tools are available only to external harnesses")
        self.daemon, self.environment_url, self.external = daemon, environment_url, external
        self.platform, self.tool_layer = platform, tool_layer
        self.global_reset, self.prompt_prefix = global_reset, prompt_prefix

    def select_tools(self, descriptors: list[dict]) -> list[dict]:
        return business_tool_catalog(descriptors, self.platform)

    def _execute_raw(self, prompt: str, timeout: int, artifacts: Path, stop: threading.Event,
                     route: str) -> Execution:
        """Serve neutral bridge primitives to the external harness; no daemon starts."""
        assert self.external is not None
        metrics = record_catalog(RAW_TOOL_CATALOG, artifacts, RAW_TOOL_LAYER)
        with ToolGateway(RawToolBackend(self.environment_url, route), RAW_TOOL_CATALOG, artifacts) as gateway:
            execution = self.external.execute(prompt, timeout, artifacts, stop,
                                              tool_endpoint=gateway.url, tool_catalog=RAW_TOOL_CATALOG)
        refresh_gateway_evidence(execution, artifacts)
        execution.metrics.update(metrics)
        return execution

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
                if step["type"] == "environment_tools":
                    directory.mkdir(parents=True, exist_ok=True)
                    prepared = prepare_environment_tools(self.environment_url, route, step)
                    (directory / "actions.json").write_text(json.dumps(prepared, ensure_ascii=False), encoding="utf-8")
                    continue
                prepared = self.daemon.execute(
                    f"{self.prompt_prefix}\n\n{step['prompt']}".strip(), max(int(step.get("timeout_sec", 90)), PREPARATION_MIN_TIMEOUT_SEC),
                    directory, stop, environment_url=self.environment_url,
                    benchmark_task_id=route, select_tools=self.select_tools,
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
            if self.tool_layer == RAW_TOOL_LAYER:
                result = self._execute_raw(prompt, timeout, artifacts, stop, route)
            else:
                result = self.daemon.execute(
                    prompt, timeout, artifacts, stop, environment_url=self.environment_url,
                    benchmark_task_id=route, external=self.external, select_tools=self.select_tools,
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
