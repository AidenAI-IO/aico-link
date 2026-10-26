"""Reflect stage: turn scored rollouts into RawPatches.

Vendored & simplified from microsoft/SkillOpt (skillopt/gradient/reflect.py).

Two analyst calls per reflect step:
  - run_error_analyst_minibatch:   over rollouts with hard=0
  - run_success_analyst_minibatch: over rollouts with hard=1

Trajectories are read from each rollout's artifact_dir (history.json),
formatted into a compact text representation, and concatenated into one
prompt with per-trajectory headers.
"""
from __future__ import annotations
import json
from pathlib import Path

from skillopt.optimizer_client import (
    OptimizerConfig,
    OptimizerError,
    chat_optimizer,
    extract_json,
)
from skillopt.score import rollout_sample_quality
from skillopt.types import RawPatch, RolloutResult


PROMPTS_DIR = Path(__file__).parent / "prompts"

_MAX_TRAJ_CHARS = 12_000


def _clip(value, limit: int) -> str:
    if value is None:
        return ""
    return str(value)[:limit]


def _load_prompt(name: str) -> str:
    return (PROMPTS_DIR / f"{name}.md").read_text(encoding="utf-8")


def fmt_trajectory_from_history(history: list[dict], max_chars: int = _MAX_TRAJ_CHARS) -> str:
    """Render Aico's ChatResponse.history list into analyst-readable text.

    Aico's history items are typed: user / assistant / tool_call / tool_result.
    We collapse tool_call + tool_result pairs into a single [action]/[obs]
    block to mirror microsoft/SkillOpt's format.
    """
    lines: list[str] = []
    pending_call: dict | None = None
    for item in history:
        if not isinstance(item, dict):
            lines.append(f"[agent] {_clip(item, 500)}")
            continue
        kind = item.get("type", "")
        if kind == "user":
            lines.append(f"[user] {_clip(item.get('content'), 500)}")
        elif kind == "assistant":
            content = _clip(item.get("content"), 500)
            if content:
                lines.append(f"[assistant] {content}")
        elif kind == "tool_call":
            pending_call = item
            tool = item.get("tool_name", "?")
            args = _clip(item.get("tool_input"), 300)
            lines.append(f"[action] {tool}({args})")
        elif kind == "tool_result":
            obs = _clip(item.get("content"), 600)
            tool = item.get("tool_name", pending_call.get("tool_name", "?") if pending_call else "?")
            lines.append(f"[obs]    {tool}: {obs}")
            pending_call = None
        else:
            lines.append(f"[{kind}] {_clip(item.get('content'), 500)}")

    text = "\n".join(lines)
    if len(text) > max_chars:
        head = text[: max_chars // 2]
        tail = text[-max_chars // 2:]
        text = head + "\n...[middle truncated]...\n" + tail
    return text


def _read_history(artifact_dir: str) -> list[dict] | None:
    path = Path(artifact_dir) / "history.json"
    if not path.exists():
        return None
    try:
        return json.loads(path.read_text(encoding="utf-8"))
    except json.JSONDecodeError:
        return None


def fmt_minibatch_trajectories(items: list[RolloutResult]) -> str:
    """Render a minibatch of rollouts as a single block of text."""
    parts: list[str] = []
    for idx, item in enumerate(items, start=1):
        history = _read_history(item.artifact_dir)
        if not history:
            continue
        traj_text = fmt_trajectory_from_history(history)
        header = (
            f"### Trajectory {idx} (id={item.id})\n"
            f"Task: {item.task_description}\n"
            f"Hard: {item.hard}  Soft: {item.soft:.2f}  Turns: {item.n_turns}\n"
        )
        if item.fail_reason:
            header += f"Fail reason: {_clip(item.fail_reason, 300)}\n"
        parts.append(header + "\n" + traj_text)
    return "\n\n---\n\n".join(parts)


def _build_user_prompt(
    skill_content: str,
    items: list[RolloutResult],
    edit_budget: int,
    rejected_context: str,
    label: str,
) -> str:
    trajectories_text = fmt_minibatch_trajectories(items)
    user = (
        f"## Current Skill\n{skill_content}\n\n"
        f"## Edit Budget\nProduce at most L={edit_budget} edits.\n\n"
    )
    if rejected_context.strip():
        user += f"## Previously Rejected Edits\n{rejected_context}\n\n"
    user += f"## {label} Trajectories ({len(items)} total)\n{trajectories_text}"
    return user


def run_error_analyst_minibatch(
    cfg: OptimizerConfig,
    skill_content: str,
    items: list[RolloutResult],
    edit_budget: int = 4,
    rejected_context: str = "",
) -> RawPatch | None:
    """Analyze a minibatch of failed trajectories. Returns failure-source RawPatch."""
    if not items:
        return None
    system = _load_prompt("analyst_error")
    user = _build_user_prompt(skill_content, items, edit_budget, rejected_context, "Failed")
    raw = chat_optimizer(cfg, system=system, user=user)
    result = extract_json(raw)
    result["source_type"] = "failure"
    result.setdefault("batch_size", len(items))
    return RawPatch.from_dict(result)


def run_success_analyst_minibatch(
    cfg: OptimizerConfig,
    skill_content: str,
    items: list[RolloutResult],
    edit_budget: int = 4,
    rejected_context: str = "",
) -> RawPatch | None:
    """Analyze a minibatch of successful trajectories. Returns success-source RawPatch."""
    if not items:
        return None
    system = _load_prompt("analyst_success")
    user = _build_user_prompt(skill_content, items, edit_budget, rejected_context, "Successful")
    raw = chat_optimizer(cfg, system=system, user=user)
    result = extract_json(raw)
    result["source_type"] = "success"
    result.setdefault("batch_size", len(items))
    return RawPatch.from_dict(result)


def run_reflect(
    cfg: OptimizerConfig,
    skill_content: str,
    rollouts: list[RolloutResult],
    edit_budget: int = 4,
    rejected_context: str = "",
) -> list[RawPatch]:
    """Split rollouts by hard score and run both analysts. Returns non-None patches."""
    failures = [r for r in rollouts if r.hard == 0 and rollout_sample_quality(r).include_in_reflect]
    successes = [r for r in rollouts if r.hard == 1 and rollout_sample_quality(r).include_in_reflect]
    out: list[RawPatch] = []
    fail_patch = run_error_analyst_minibatch(cfg, skill_content, failures, edit_budget, rejected_context)
    if fail_patch is not None:
        out.append(fail_patch)
    succ_patch = run_success_analyst_minibatch(cfg, skill_content, successes, edit_budget, rejected_context)
    if succ_patch is not None:
        out.append(succ_patch)
    return out
