"""Orchestrator: main optimization loop.

Ties together rollout, reflect, aggregate, patch, score, gate in the
classic SkillOpt rhythm:

  loop:
    train_rollout(current_skill) → rollouts
    reflect(rollouts) → raw_patches
    aggregate(raw_patches) → patch
    apply(patch) → candidate_skill
    selection_rollout(candidate_skill) → candidate_score
    gate(candidate_score, current_score) → accept / reject
    if no improvement for N steps → early stop
"""
from __future__ import annotations
import dataclasses as dc
import json
from datetime import datetime, timezone
from pathlib import Path

from runner.judge import JudgeConfig
from runner.suite import Suite, TaskSpec
from skillopt.aggregate import aggregate, format_rejected_context
from skillopt.backends import AicoDeviceBackend, SkillOptRolloutBackend
from skillopt.optimizer_client import OptimizerConfig, OptimizerError
from skillopt.patch import apply_patch_with_report
from skillopt.phase_artifacts import write_phase_completed, write_phase_failed, write_phase_started
from skillopt.reflect import run_reflect
from skillopt.score import (
    aggregate_score,
    validation_gate,
    DEFAULT_MIN_DELTA,
)
from skillopt.skill_lint import lint_skill_text
from skillopt.types import Edit, OptimizationResult, PhaseSummary, ScoreSummary, StepDecision


@dc.dataclass
class OptimizationConfig:
    skill_name: str
    skill_path: Path
    suite: Suite
    train_tasks: list[TaskSpec]
    selection_tasks: list[TaskSpec]
    train_suite: Suite | None = None
    selection_suite: Suite | None = None
    budget: int = 10                     # max optimization steps
    edit_budget: int = 4                 # edits per step
    min_delta: float = DEFAULT_MIN_DELTA
    optimizer_cfg: OptimizerConfig = dc.field(default_factory=OptimizerConfig)
    judge_cfg: JudgeConfig | None = None
    agent_url: str = "http://localhost:8080"
    run_id: str = dc.field(default_factory=lambda: datetime.now(timezone.utc).strftime("%Y%m%d_%H%M%S"))
    artifact_root: Path = Path("skillopt/runs")
    early_stop_patience: int = 3         # stop if no improvement for N steps
    rollout_backend: SkillOptRolloutBackend | None = None


def optimize_skill(cfg: OptimizationConfig) -> OptimizationResult:
    """Run the full optimization loop. Returns best skill + stats."""
    skill_path = cfg.skill_path
    if not skill_path.exists():
        raise FileNotFoundError(f"skill not found: {skill_path}")

    original = skill_path.read_text(encoding="utf-8")
    current = original
    backend = cfg.rollout_backend or AicoDeviceBackend(agent_url=cfg.agent_url)
    run_root = cfg.artifact_root / cfg.run_id
    run_root.mkdir(parents=True, exist_ok=True)
    train_suite = cfg.train_suite or cfg.suite
    selection_suite = cfg.selection_suite or cfg.suite
    phase_summaries: list[PhaseSummary] = []
    stop_reason = ""
    try:
        # Baseline eval on selection split
        print(f"[baseline] Evaluating original skill on {len(cfg.selection_tasks)} selection tasks...")
        sel_rollouts, current_score = _run_rollout_phase(
            backend,
            cfg,
            suite=selection_suite,
            tasks=cfg.selection_tasks,
            skill_text=current,
            phase="baseline_selection",
            kind="verification",
            run_root=run_root,
        )
        phase_summaries.append(_phase_summary(
            phase="baseline_selection",
            kind="verification",
            suite=selection_suite,
            score=current_score,
        ))
        print(f"[baseline] hard={current_score.hard:.3f} soft={current_score.soft:.3f} ({current_score.n_passed}/{current_score.n})")

        best = current
        best_score = current_score
        steps: list[StepDecision] = []
        rejected: list[Edit] = []
        no_improvement_count = 0

        for step_idx in range(1, cfg.budget + 1):
            print(f"\n[step {step_idx}] Train rollout on {len(cfg.train_tasks)} tasks...")
            train_rollouts, train_score = _run_rollout_phase(
                backend,
                cfg,
                suite=train_suite,
                tasks=cfg.train_tasks,
                skill_text=current,
                phase=f"step_{step_idx:02d}_train",
                kind="train",
                run_root=run_root,
            )
            train_summary = _phase_summary(
                phase=f"step_{step_idx:02d}_train",
                kind="train",
                suite=train_suite,
                score=train_score,
            )
            phase_summaries.append(train_summary)
            print(f"[step {step_idx}] train: hard={train_score.hard:.3f} soft={train_score.soft:.3f}")

            # Reflect
            print(f"[step {step_idx}] Reflect (calling optimizer LLM)...")
            rejected_ctx = format_rejected_context(rejected)
            step_artifact = run_root / f"step_{step_idx:02d}"
            step_artifact.mkdir(parents=True, exist_ok=True)
            try:
                raw_patches = run_reflect(
                    cfg.optimizer_cfg,
                    current,
                    train_rollouts,
                    edit_budget=cfg.edit_budget,
                    rejected_context=rejected_ctx,
                )
            except OptimizerError as exc:
                stop_reason = f"step {step_idx}: reflect failed: {exc}"
                (step_artifact / "reflect_error.json").write_text(
                    json.dumps({"error": str(exc), "type": type(exc).__name__}, ensure_ascii=False, indent=2),
                    encoding="utf-8",
                )
                print(f"[step {step_idx}] reflect failed: {exc}; stopping.")
                break
            if not raw_patches:
                stop_reason = f"step {step_idx}: no patches produced by reflect"
                print(f"[step {step_idx}] no patches produced by reflect; stopping.")
                break

            # Aggregate
            patch = aggregate(raw_patches, edit_budget=cfg.edit_budget)
            if not patch.edits:
                stop_reason = f"step {step_idx}: aggregate produced 0 edits after dedup"
                print(f"[step {step_idx}] aggregate produced 0 edits after dedup; stopping.")
                break
            print(f"[step {step_idx}] {len(patch.edits)} edits: {[e.op for e in patch.edits]}")

            # Apply
            candidate, reports = apply_patch_with_report(current, patch)
            (step_artifact / "candidate.md").write_text(candidate, encoding="utf-8")
            (step_artifact / "patch.json").write_text(
                json.dumps(patch.to_dict(), ensure_ascii=False, indent=2), encoding="utf-8"
            )
            (step_artifact / "patch_reports.json").write_text(
                json.dumps(reports, ensure_ascii=False, indent=2), encoding="utf-8"
            )
            lint_issues = lint_skill_text(candidate)
            if lint_issues:
                (step_artifact / "candidate_lint.json").write_text(
                    json.dumps([issue.to_dict() for issue in lint_issues], ensure_ascii=False, indent=2),
                    encoding="utf-8",
                )
                reason = "skill lint failed: " + ", ".join(issue.code for issue in lint_issues)
                print(f"[step {step_idx}] {reason}")
                rejected.extend(patch.edits)
                no_improvement_count += 1
                steps.append(StepDecision(
                    step=step_idx,
                    candidate_score=current_score.primary,
                    current_score=current_score.primary,
                    accepted=False,
                    reason=reason,
                    edits_rejected=patch.edits,
                    train_score=train_summary.score,
                    patch_reasoning=patch.reasoning,
                    patch_reports=reports,
                    raw_patches=raw_patches,
                ))
                if no_improvement_count >= cfg.early_stop_patience:
                    stop_reason = f"step {step_idx}: no improvement for {cfg.early_stop_patience} steps"
                    print(f"[step {step_idx}] no improvement for {cfg.early_stop_patience} steps; stopping.")
                    break
                continue

            # Selection eval with candidate
            print(f"[step {step_idx}] Selection eval with candidate...")
            cand_rollouts, candidate_score_agg = _run_rollout_phase(
                backend,
                cfg,
                suite=selection_suite,
                tasks=cfg.selection_tasks,
                skill_text=candidate,
                phase=f"step_{step_idx:02d}_selection",
                kind="verification",
                run_root=run_root,
            )
            candidate_summary = _phase_summary(
                phase=f"step_{step_idx:02d}_selection",
                kind="verification",
                suite=selection_suite,
                score=candidate_score_agg,
            )
            phase_summaries.append(candidate_summary)
            print(f"[step {step_idx}] candidate: hard={candidate_score_agg.hard:.3f} soft={candidate_score_agg.soft:.3f}")

            # Gate
            decision = validation_gate(candidate_score_agg, current_score, cfg.min_delta)
            print(f"[step {step_idx}] gate: {decision.reason}")
            (step_artifact / "decision.json").write_text(
                json.dumps(dc.asdict(decision), ensure_ascii=False, indent=2), encoding="utf-8"
            )

            if decision.accepted:
                current = candidate
                current_score = candidate_score_agg
                if current_score.primary > best_score.primary:
                    best = current
                    best_score = current_score
                    (run_root / "best_skill.md").write_text(best, encoding="utf-8")
                no_improvement_count = 0
                steps.append(StepDecision(
                    step=step_idx,
                    candidate_score=decision.candidate_score,
                    current_score=decision.current_score,
                    accepted=True,
                    reason=decision.reason,
                    edits_applied=patch.edits,
                    train_score=train_summary.score,
                    candidate_selection_score=candidate_summary.score,
                    patch_reasoning=patch.reasoning,
                    patch_reports=reports,
                    raw_patches=raw_patches,
                ))
            else:
                rejected.extend(patch.edits)
                no_improvement_count += 1
                steps.append(StepDecision(
                    step=step_idx,
                    candidate_score=decision.candidate_score,
                    current_score=decision.current_score,
                    accepted=False,
                    reason=decision.reason,
                    edits_rejected=patch.edits,
                    train_score=train_summary.score,
                    candidate_selection_score=candidate_summary.score,
                    patch_reasoning=patch.reasoning,
                    patch_reports=reports,
                    raw_patches=raw_patches,
                ))

            # Early stop
            if no_improvement_count >= cfg.early_stop_patience:
                stop_reason = f"step {step_idx}: no improvement for {cfg.early_stop_patience} steps"
                print(f"[step {step_idx}] no improvement for {cfg.early_stop_patience} steps; stopping.")
                break

        return OptimizationResult(
            skill_name=cfg.skill_name,
            initial_score=aggregate_score(sel_rollouts).primary,
            best_score=best_score.primary,
            best_skill=best,
            steps=steps,
            accepted_count=sum(1 for s in steps if s.accepted),
            rejected_count=sum(1 for s in steps if not s.accepted),
            phase_summaries=phase_summaries,
            stop_reason=stop_reason,
        )
    finally:
        backend.close()


def _score_summary(score) -> ScoreSummary:
    return ScoreSummary(
        hard=score.hard,
        soft=score.soft,
        n=score.n,
        n_passed=score.n_passed,
        n_raw=getattr(score, "n_raw", score.n),
        n_excluded=getattr(score, "n_excluded", 0),
        n_downweighted=getattr(score, "n_downweighted", 0),
        weight_total=getattr(score, "weight_total", float(score.n)),
    )


def _run_rollout_phase(
    backend: SkillOptRolloutBackend,
    cfg: OptimizationConfig,
    *,
    suite: Suite,
    tasks: list[TaskSpec],
    skill_text: str,
    phase: str,
    kind: str,
    run_root: Path,
):
    write_phase_started(run_root, phase=phase, kind=kind, suite=suite, tasks=tasks)
    try:
        rollouts = backend.run_rollout(
            suite=suite,
            tasks=tasks,
            skill_name=cfg.skill_name,
            skill_path=cfg.skill_path,
            skill_text=skill_text,
            phase=phase,
            run_id=cfg.run_id,
            run_root=run_root,
            judge_cfg=cfg.judge_cfg,
        )
    except Exception as exc:
        partial_rollouts = getattr(exc, "rollouts", None)
        if not isinstance(partial_rollouts, list):
            partial_rollouts = None
        write_phase_failed(
            run_root,
            phase=phase,
            kind=kind,
            suite=suite,
            tasks=tasks,
            error=str(exc),
            rollouts=partial_rollouts,
        )
        raise
    score = aggregate_score(rollouts)
    write_phase_completed(run_root, phase=phase, kind=kind, suite=suite, tasks=tasks, rollouts=rollouts, score=score)
    return rollouts, score


def _phase_summary(*, phase: str, kind: str, suite: Suite, score) -> PhaseSummary:
    return PhaseSummary(
        phase=phase,
        kind=kind,
        suite_name=suite.name,
        score=_score_summary(score),
    )
