from __future__ import annotations

from pathlib import Path
from typing import Protocol

from runner.agent_client import AgentClient
from runner.judge import JudgeConfig
from runner.runtask import run_one_task
from runner.suite import Suite, TaskSpec
from skillopt.score import task_result_to_rollout
from skillopt.skill_override import with_skill_override
from skillopt.types import RolloutResult


class SkillOptRolloutBackend(Protocol):
    def run_rollout(
        self,
        *,
        suite: Suite,
        tasks: list[TaskSpec],
        skill_name: str,
        skill_path: Path,
        skill_text: str,
        phase: str,
        run_id: str,
        run_root: Path,
        judge_cfg: JudgeConfig | None,
    ) -> list[RolloutResult]:
        ...

    def close(self) -> None:
        ...


class AicoDeviceBackend:
    def __init__(self, *, agent_url: str):
        self.client = AgentClient(base_url=agent_url)

    def close(self) -> None:
        self.client.close()

    def run_rollout(
        self,
        *,
        suite: Suite,
        tasks: list[TaskSpec],
        skill_name: str,
        skill_path: Path,
        skill_text: str,
        phase: str,
        run_id: str,
        run_root: Path,
        judge_cfg: JudgeConfig | None,
    ) -> list[RolloutResult]:
        current_disk = skill_path.read_text(encoding="utf-8") if skill_path.exists() else ""
        if skill_text != current_disk:
            with with_skill_override(self.client, skill_path, skill_text):
                return self._run_tasks(suite, tasks, skill_name, phase, run_id, run_root, judge_cfg)
        return self._run_tasks(suite, tasks, skill_name, phase, run_id, run_root, judge_cfg)

    def _run_tasks(
        self,
        suite: Suite,
        tasks: list[TaskSpec],
        skill_name: str,
        phase: str,
        run_id: str,
        run_root: Path,
        judge_cfg: JudgeConfig | None,
    ) -> list[RolloutResult]:
        judge_cache = run_root / "_judge_cache"
        rollouts: list[RolloutResult] = []
        for task in tasks:
            result = run_one_task(
                client=self.client,
                suite=suite,
                task=task,
                attempt=1,
                artifact_dir=run_root / phase / task.id,
                judge_cfg=judge_cfg,
                judge_cache_dir=judge_cache,
                run_id=run_id,
                active_skills=[skill_name] if skill_name else [],
            )
            rollouts.append(task_result_to_rollout(result))
        return rollouts
