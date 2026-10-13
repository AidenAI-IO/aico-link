from __future__ import annotations

import concurrent.futures
import uuid
from collections.abc import Callable
from typing import Any

from runner.platform import read_environment_health
from runner.recovery import DEFAULT_ENVIRONMENT_SETUP_TIMEOUT_SEC
from runner.reset import call_environment_release, call_environment_setup


MOBILEGYM_PREFLIGHT_COMPLETE_ENV = "AIDEN_BENCHMARK_MOBILEGYM_PREFLIGHT_COMPLETE"


def _environment_count(health: dict[str, Any]) -> int:
    for key in ("env_count", "concurrent"):
        try:
            value = int(health.get(key))
        except (TypeError, ValueError):
            continue
        if value > 0:
            return value
    return 1


def _call_for_task_ids(
    action: Callable[..., Any],
    environment_url: str,
    timeout: int,
    task_ids: list[str],
) -> list[tuple[str, Exception]]:
    errors: list[tuple[str, Exception]] = []
    with concurrent.futures.ThreadPoolExecutor(
        max_workers=len(task_ids),
        thread_name_prefix="mobilegym-preflight",
    ) as executor:
        futures = {
            executor.submit(
                action,
                environment_url,
                timeout=timeout,
                task_id=task_id,
            ): task_id
            for task_id in task_ids
        }
        for future in concurrent.futures.as_completed(futures):
            task_id = futures[future]
            try:
                future.result()
            except Exception as exc:
                errors.append((task_id, exc))
    return errors


def preflight_mobilegym_environment(
    environment_url: str,
    *,
    timeout: int = DEFAULT_ENVIRONMENT_SETUP_TIMEOUT_SEC,
    health: dict[str, Any] | None = None,
) -> None:
    environment_url = str(environment_url or "").strip()
    if not environment_url:
        return

    health = health if health is not None else read_environment_health(environment_url)
    if str(health.get("bridge_type") or "").strip().lower() != "mobilegym":
        return

    task_ids = [
        f"benchmark-preflight:{uuid.uuid4().hex}"
        for _ in range(_environment_count(health))
    ]

    setup_errors = _call_for_task_ids(
        call_environment_setup,
        environment_url,
        timeout,
        task_ids,
    )
    release_errors = _call_for_task_ids(
        call_environment_release,
        environment_url,
        timeout,
        task_ids,
    )

    if setup_errors:
        raise RuntimeError(
            "MobileGym environment failed setup preflight; remove it and start "
            "a fresh MobileGym environment"
        ) from setup_errors[0][1]
    if release_errors:
        raise RuntimeError(
            "MobileGym environment failed release preflight; remove it and start "
            "a fresh MobileGym environment"
        ) from release_errors[0][1]
