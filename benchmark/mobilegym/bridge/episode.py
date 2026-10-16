from __future__ import annotations

import asyncio
import inspect
import threading
from collections.abc import Awaitable, Callable
from typing import Any

from .input import SimulatorInput

BENCHMARK_TASK_ID_HEADER = "benchmark-task-id"
EPISODE_RESET_TIMEOUT_SEC = 45
EPISODE_RESTART_TIMEOUT_SEC = 30


class BridgeEpisodeError(RuntimeError):
    pass


class StaleEpisodeError(BridgeEpisodeError):
    pass


class MissingBenchmarkTaskIDError(ValueError):
    pass


class NoBridgeEnvAvailableError(BridgeEpisodeError):
    pass


def benchmark_task_id_from_headers(headers: Any) -> str:
    if headers is None:
        return ""
    for name in (BENCHMARK_TASK_ID_HEADER, "x-benchmark-task-id"):
        try:
            value = headers.get(name)
        except AttributeError:
            value = None
        if value:
            return str(value).strip()
    return ""


class BridgeTaskRouter:
    """Maps benchmark task ids to bridge states.

    In multi-env mode every request must carry ``benchmark-task-id``. The
    router uses that explicit id on each request; it does not infer the task id
    from a prior reset or episode call.
    """

    def __init__(self, states: list["BridgeEpisodeState"]):
        if not states:
            raise ValueError("at least one bridge state is required")
        self.states = list(states)
        self.default_state = self.states[0]
        self._lock = threading.Lock()
        self._task_to_state: dict[str, BridgeEpisodeState] = {}
        self._state_to_task: dict[BridgeEpisodeState, str] = {}
        self._next_index = 0

    @classmethod
    def from_state(cls, state: "BridgeEpisodeState | BridgeTaskRouter") -> "BridgeTaskRouter":
        if isinstance(state, BridgeTaskRouter):
            return state
        return cls([state])

    def state_for_headers(self, headers: Any) -> "BridgeEpisodeState":
        return self.state_for_task_id(benchmark_task_id_from_headers(headers))

    def state_for_task_id(self, task_id: str | None) -> "BridgeEpisodeState":
        task_id = str(task_id or "").strip()
        if not task_id:
            if len(self.states) > 1:
                raise MissingBenchmarkTaskIDError(f"{BENCHMARK_TASK_ID_HEADER} header is required")
            return self.default_state
        with self._lock:
            existing = self._task_to_state.get(task_id)
            if existing is not None:
                return existing
            state = self._choose_state_locked()
            self._task_to_state[task_id] = state
            self._state_to_task[state] = task_id
            return state

    def existing_state_for_task_id(self, task_id: str | None) -> "BridgeEpisodeState | None":
        task_id = str(task_id or "").strip()
        if not task_id:
            if len(self.states) > 1:
                raise MissingBenchmarkTaskIDError(f"{BENCHMARK_TASK_ID_HEADER} header is required")
            return self.default_state
        with self._lock:
            return self._task_to_state.get(task_id)

    def release_task_id(self, task_id: str | None) -> bool:
        task_id = str(task_id or "").strip()
        if not task_id:
            if len(self.states) > 1:
                raise MissingBenchmarkTaskIDError(f"{BENCHMARK_TASK_ID_HEADER} header is required")
            return False
        with self._lock:
            state = self._task_to_state.pop(task_id, None)
            if state is None:
                return False
            self._state_to_task.pop(state, None)
            return True

    def task_map(self) -> dict[str, int]:
        with self._lock:
            return {
                task_id: self.states.index(state)
                for task_id, state in self._task_to_state.items()
                if state in self.states
            }

    def _choose_state_locked(self) -> "BridgeEpisodeState":
        for offset in range(len(self.states)):
            index = (self._next_index + offset) % len(self.states)
            state = self.states[index]
            if state not in self._state_to_task:
                self._next_index = (index + 1) % len(self.states)
                return state
        raise NoBridgeEnvAvailableError("no MobileGym bridge environment is available for a new benchmark task")


class BridgeEpisodeState:
    def __init__(self, env: Any, owner_loop: asyncio.AbstractEventLoop | None = None):
        self.env = env
        self.input = SimulatorInput(env)
        self.owner_loop = owner_loop or asyncio.get_event_loop()
        self.active_episode_id: str | None = None
        self.action_log: list[dict[str, Any]] = []
        self._action_counter = 0
        self._env_lock = asyncio.Lock()
        self._pending_sync_reset: asyncio.Task[Any] | None = None
        self._restart_task: asyncio.Task[bool] | None = None

    async def start_episode(self, episode_id: str) -> dict[str, str]:
        episode_id = str(episode_id or "").strip()
        if not episode_id:
            raise ValueError("episode_id is required")
        async with self._env_lock:
            await self._await_pending_restart()
            if self.input.contact_active:
                await self.input.release()
            self.active_episode_id = episode_id
            self.action_log = []
            self._action_counter = 0
        return {"episode_id": episode_id}

    async def reset_episode(
        self,
        episode_id: str,
        app_ids: list[str] | None = None,
    ) -> dict[str, Any]:
        episode_id = str(episode_id or "").strip()
        if not episode_id:
            raise ValueError("episode_id is required")
        async with self._env_lock:
            await self._await_pending_restart()
            if self.input.contact_active:
                await self.input.release()
            reset_ran = False
            for attempt in range(2):
                reset = getattr(self.env, "reset", None)
                if reset is None:
                    break
                try:
                    await self._run_reset(reset, app_ids=app_ids)
                    reset_ran = True
                    break
                except (asyncio.TimeoutError, TimeoutError) as exc:
                    restarted = await self._restart_env_after_timeout(exc)
                    if attempt == 0 and restarted:
                        continue
                    raise TimeoutError(
                        f"environment reset timed out after {EPISODE_RESET_TIMEOUT_SEC}s: {exc}"
                    ) from exc
            self.active_episode_id = episode_id
            self.action_log = []
            self._action_counter = 0
        return {"episode_id": episode_id, "reset": reset_ran}

    async def _run_reset(
        self,
        reset: Callable[..., Any | Awaitable[Any]],
        *,
        app_ids: list[str] | None,
    ) -> None:
        async def invoke_async() -> None:
            result = reset() if app_ids is None else reset(app_ids=app_ids)
            if inspect.isawaitable(result):
                await result

        async def invoke_sync() -> None:
            if app_ids is None:
                reset_task = asyncio.create_task(asyncio.to_thread(reset))
            else:
                reset_task = asyncio.create_task(asyncio.to_thread(reset, app_ids=app_ids))
            self._pending_sync_reset = reset_task
            try:
                result = await asyncio.shield(reset_task)
            finally:
                if reset_task.done() and self._pending_sync_reset is reset_task:
                    self._pending_sync_reset = None
            if inspect.isawaitable(result):
                await result

        invoke = invoke_async if inspect.iscoroutinefunction(reset) else invoke_sync
        await asyncio.wait_for(invoke(), timeout=EPISODE_RESET_TIMEOUT_SEC)

    async def _restart_env_after_timeout(self, reset_error: BaseException) -> bool:
        if self._restart_task is None:
            self._restart_task = asyncio.create_task(self._restart_env_when_idle())
        restart_task = self._restart_task
        try:
            result = await asyncio.wait_for(
                asyncio.shield(restart_task),
                timeout=EPISODE_RESTART_TIMEOUT_SEC,
            )
        except asyncio.TimeoutError as exc:
            if restart_task.done():
                try:
                    result = restart_task.result()
                except Exception as restart_exc:
                    self._restart_task = None
                    raise TimeoutError(
                        "environment restart failed while recovering from reset timeout: "
                        f"{type(restart_exc).__name__}: {restart_exc}; reset error: {reset_error}"
                    ) from restart_exc
                self._restart_task = None
                return result
            raise TimeoutError(
                f"environment restart timed out after {EPISODE_RESTART_TIMEOUT_SEC}s "
                f"while recovering from reset timeout: {reset_error}"
            ) from exc
        except Exception as exc:
            if restart_task.done():
                self._restart_task = None
            raise TimeoutError(
                f"environment restart failed while recovering from reset timeout: "
                f"{type(exc).__name__}: {exc}; reset error: {reset_error}"
            ) from exc
        self._restart_task = None
        return result

    async def _restart_env_when_idle(self) -> bool:
        await self._await_pending_sync_reset()
        return await self._restart_env_if_supported()

    async def _await_pending_sync_reset(self) -> None:
        reset_task = self._pending_sync_reset
        if reset_task is None:
            return
        try:
            await asyncio.gather(asyncio.shield(reset_task), return_exceptions=True)
        finally:
            if reset_task.done() and self._pending_sync_reset is reset_task:
                self._pending_sync_reset = None

    async def _await_pending_restart(self) -> None:
        if self._restart_task is None:
            await self._await_pending_sync_reset()
            return
        await self._restart_env_after_timeout(
            TimeoutError("previous environment restart is still in progress")
        )

    async def _restart_env_if_supported(self) -> bool:
        restart = getattr(self.env, "restart", None)
        if restart is not None:
            started_env = await self._invoke_env_callable(restart)
            if started_env is not None:
                self.env = started_env
            self.input = SimulatorInput(self.env)
            return True

        close = getattr(self.env, "close", None)
        start = getattr(self.env, "start", None)
        if close is None or start is None:
            return False
        await self._invoke_env_callable(close)
        started_env = await self._invoke_env_callable(start)
        if started_env is not None:
            self.env = started_env
        self.input = SimulatorInput(self.env)
        return True

    async def _invoke_env_callable(self, fn: Callable[..., Any], *args: Any, **kwargs: Any) -> Any:
        if inspect.iscoroutinefunction(fn):
            result = fn(*args, **kwargs)
        else:
            result = await asyncio.to_thread(fn, *args, **kwargs)
        if inspect.isawaitable(result):
            return await result
        return result

    async def end_episode(self, episode_id: str) -> dict[str, Any]:
        episode_id = self.require_active(episode_id)
        async with self._env_lock:
            if self.active_episode_id != episode_id:
                raise StaleEpisodeError("stale episode_id")
            if self.input.contact_active:
                await self.input.release()
            logs = list(self.action_log)
            self.active_episode_id = None
        return {"episode_id": episode_id, "action_log": logs}

    def require_active(self, episode_id: str | None) -> str:
        episode_id = str(episode_id or "").strip()
        if not episode_id or episode_id != self.active_episode_id:
            raise StaleEpisodeError("stale episode_id")
        return episode_id

    async def run_env(self, fn: Callable[[Any], Any | Awaitable[Any]]) -> Any:
        running_loop = asyncio.get_running_loop()
        if running_loop is not self.owner_loop:
            raise RuntimeError("env work must run on the MobileGym owner loop")
        async with self._env_lock:
            await self._await_pending_restart()
            result = fn(self.env)
            if asyncio.iscoroutine(result) or isinstance(result, Awaitable):
                return await result
            return result

    def log_action(
        self,
        tool_name: str,
        tool_input: dict[str, Any],
        mobilegym_action: dict[str, Any],
        duration_ms: int,
        error: str | None,
        episode_id: str | None = None,
        screenshot: dict[str, Any] | None = None,
    ) -> dict[str, Any]:
        active_episode_id = self.require_active(episode_id or self.active_episode_id)
        self._action_counter += 1
        entry = {
            "episode_id": active_episode_id,
            "action_id": f"{active_episode_id}:{self._action_counter:04d}",
            "tool_name": tool_name,
            "tool_input": dict(tool_input),
            "mobilegym_action": dict(mobilegym_action),
            "screenshot": screenshot,
            "duration_ms": int(duration_ms),
            "error": error,
        }
        self.action_log.append(entry)
        return entry
