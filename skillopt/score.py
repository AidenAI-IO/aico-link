"""Score and validation gate.

Converts benchmark TaskResult into SkillOpt-style RolloutResult, computes
aggregate scores, and applies the validation gate (candidate must beat
current by min_delta to be accepted).
"""
from __future__ import annotations
import dataclasses as dc
import json
import math
import os
import re
import sys
from typing import Any

from runner.models import TaskResult
from skillopt.types import RolloutResult


# Default acceptance threshold (validation_score gain).
DEFAULT_MIN_DELTA = 0.03
TIMEOUT_SCORE_WEIGHT = 0.35
EXTERNAL_BLOCKER_SCORE_WEIGHT = 0.5


@dc.dataclass(frozen=True)
class SampleQuality:
    kind: str = "clean"
    reason: str = ""
    include_in_score: bool = True
    score_weight: float = 1.0
    include_in_reflect: bool = True
    # The rule-based kind when the Jev classifier overrode it.
    rule_kind: str = ""

    @property
    def is_clean(self) -> bool:
        return (
            self.kind == "clean"
            and self.include_in_score
            and self.include_in_reflect
            and self.score_weight == 1.0
        )

    def to_extras(self) -> dict[str, Any]:
        extras = {
            "sample_quality": self.kind,
            "sample_quality_reason": self.reason,
            "score_weight": self.score_weight,
            "score_excluded": not self.include_in_score,
            "reflect_excluded": not self.include_in_reflect,
        }
        if self.rule_kind:
            extras["sample_quality_rule"] = self.rule_kind
        return extras


_SYSTEM_ERROR_MARKERS = (
    "agent error",
    "http 500",
    "http 502",
    "http 503",
    "http 504",
    "planner/executor role model call timed out",
    "role model call timed out",
    "model call timed out",
    "docker",
    "docker compose",
    "container exited",
    "setup:",
    "setup endpoint timed out",
    "setup endpoint failed",
    "no_bridge_env_available",
    "worker failed",
    "page crashed",
    "target crashed",
)

_ZERO_TOOL_MARKERS = (
    "no tool call",
    "0 tool call",
    "zero tool",
    "final response empty",
    "empty final response",
    "missing aiden_last_chat_history",
)

_EXTERNAL_BLOCKER_MARKERS = (
    "locked device",
    "device is locked",
    "lock screen",
    "requires login",
    "authentication required",
    "no result",
    "no results",
    "0 results",
    "no visible result",
    "unavailable",
    "not available",
    "out of stock",
)


def task_result_to_rollout(tr: TaskResult) -> RolloutResult:
    """Convert a TaskResult into a SkillOpt RolloutResult.

      hard = 1 if the task passed end-to-end, else 0.
      soft = rubric_pass_count / rubric_total when rubric is present,
             otherwise mirrors hard.
    """
    hard = 1 if tr.status == "passed" else 0
    if tr.rubric_total > 0:
        soft = tr.rubric_pass_count / tr.rubric_total
    else:
        soft = float(hard)

    fail_reason = ""
    if tr.status != "passed":
        if tr.status == "timeout":
            fail_reason = "timeout"
        elif tr.status == "skipped":
            fail_reason = tr.metrics.get("error", "skipped") if tr.metrics else "skipped"
        elif tr.status == "judge_error":
            fail_reason = tr.metrics.get("judge_error", "judge_error") if tr.metrics else "judge_error"
        else:
            # failed: include first 'no' rubric reason if any
            for v in tr.rubric:
                if v.verdict == "no":
                    fail_reason = f"{v.id}: {v.reason}"
                    break
            if not fail_reason:
                fail_reason = "failed"

    quality = task_result_sample_quality(tr, fail_reason=fail_reason)
    return RolloutResult(
        id=tr.task_id,
        hard=hard,
        soft=soft,
        n_turns=tr.metrics.get("tool_calls", 0) if tr.metrics else 0,
        fail_reason=fail_reason,
        task_description=tr.description_for_judge,
        artifact_dir=tr.artifact_dir,
        extras=quality.to_extras(),
    )


def task_result_sample_quality(tr: TaskResult, *, fail_reason: str = "") -> SampleQuality:
    rule = _rule_sample_quality(tr, fail_reason=fail_reason)
    if os.environ.get("SKILLOPT_SAMPLE_CLASSIFIER", "rules").strip() != "typesafe":
        return rule
    return _typesafe_sample_quality(tr, fail_reason, rule)


# Sample-quality policy per kind: (include_in_score, score_weight, include_in_reflect).
_QUALITY_POLICY = {
    "system_error": (False, 0.0, False),
    "agent_format_error": (False, 0.0, False),
    "timeout": (True, TIMEOUT_SCORE_WEIGHT, False),
    "external_blocker": (True, EXTERNAL_BLOCKER_SCORE_WEIGHT, False),
    "clean": (True, 1.0, True),
}

_TYPESAFE_CRITERIA = {
    "system_error": "the harness, environment, network or service failed; the agent's own behavior is not what went wrong",
    "agent_format_error": "the agent made no tool calls, gave an empty reply, or replied with an internal JSON plan instead of acting",
    "timeout": "the task ran out of time",
    "external_blocker": "the task failed because of something outside the agent: a login wall, missing account, captcha, permission prompt, or app that is unavailable",
    "clean": "an ordinary pass or failure that fairly reflects the agent's own ability",
}


def _typesafe_sample_quality(tr: TaskResult, fail_reason: str, rule: SampleQuality) -> SampleQuality:
    """Ask Jev for the sample-quality class; a confident answer overrides the rules."""
    from skillopt import typesafe

    metrics = tr.metrics or {}
    state = {
        "status": str(tr.status or ""),
        "fail_reason": fail_reason,
        "tool_calls": metrics.get("tool_calls"),
        "final_response": str(metrics.get("final_response") or metrics.get("aiden_last_response") or "")[:2000],
        "evidence": _quality_text(
            str(tr.status or ""), fail_reason, metrics,
            getattr(tr, "hard_assertions", None), getattr(tr, "hard_assertion_failures", []), getattr(tr, "rubric", []),
        )[:8000],
    }
    question = {
        "type": "choice",
        "instructions": "This is one benchmark run of a phone-automation agent. Which kind of training sample is it?",
        "criteria": _TYPESAFE_CRITERIA,
    }
    min_confidence = _safe_float(os.environ.get("SKILLOPT_SAMPLE_CLASSIFIER_MIN_CONFIDENCE"), 0.8)
    if not math.isfinite(min_confidence):
        min_confidence = 0.8
    try:
        response = typesafe.ask(state, {"kind": question}, api_key=typesafe.resolve_api_key())
        got = typesafe.answer(response, "kind", "choice")
    except Exception as exc:  # noqa: BLE001 - classification falls back to the rules
        print(f"[skillopt] typesafe sample classifier failed, using rules: {exc}", file=sys.stderr)
        return rule
    kind, confidence = str(got.get("choice") or ""), _safe_float(got.get("confidence"), 0.0)
    if kind not in _QUALITY_POLICY or not math.isfinite(confidence) or confidence < min_confidence or kind == rule.kind:
        return rule
    include_in_score, weight, include_in_reflect = _QUALITY_POLICY[kind]
    return SampleQuality(
        kind=kind,
        reason=f"jev confidence {confidence:.2f}; rules said {rule.kind}",
        include_in_score=include_in_score,
        score_weight=weight,
        include_in_reflect=include_in_reflect,
        rule_kind=rule.kind,
    )


def _rule_sample_quality(tr: TaskResult, *, fail_reason: str = "") -> SampleQuality:
    metrics = tr.metrics or {}
    status = str(tr.status or "")
    text = _quality_text(
        status,
        fail_reason,
        metrics,
        getattr(tr, "hard_assertions", None),
        getattr(tr, "hard_assertion_failures", []),
        getattr(tr, "rubric", []),
    )
    if _contains_any(text, _SYSTEM_ERROR_MARKERS):
        return SampleQuality(
            kind="system_error",
            reason=_matched_marker(text, _SYSTEM_ERROR_MARKERS),
            include_in_score=False,
            score_weight=0.0,
            include_in_reflect=False,
        )
    tool_calls = _safe_int(metrics.get("tool_calls"), default=-1)
    final_response = str(metrics.get("final_response") or metrics.get("aiden_last_response") or "")
    if _looks_like_zero_tool_or_planner_format(text, tool_calls=tool_calls, final_response=final_response):
        return SampleQuality(
            kind="agent_format_error",
            reason="zero tool calls, empty final response, or internal JSON plan output",
            include_in_score=False,
            score_weight=0.0,
            include_in_reflect=False,
        )
    if status == "timeout" or _timeout_without_system_marker(text):
        return SampleQuality(
            kind="timeout",
            reason="task timed out",
            include_in_score=True,
            score_weight=TIMEOUT_SCORE_WEIGHT,
            include_in_reflect=False,
        )
    if status != "passed" and _contains_any(text, _EXTERNAL_BLOCKER_MARKERS):
        return SampleQuality(
            kind="external_blocker",
            reason=_matched_marker(text, _EXTERNAL_BLOCKER_MARKERS),
            include_in_score=True,
            score_weight=EXTERNAL_BLOCKER_SCORE_WEIGHT,
            include_in_reflect=False,
        )
    return SampleQuality()


def rollout_sample_quality(rollout: RolloutResult) -> SampleQuality:
    extras = rollout.extras or {}
    if extras.get("sample_quality"):
        return SampleQuality(
            kind=str(extras.get("sample_quality") or "clean"),
            reason=str(extras.get("sample_quality_reason") or ""),
            include_in_score=not bool(extras.get("score_excluded")),
            score_weight=_safe_float(extras.get("score_weight"), 1.0),
            include_in_reflect=not bool(extras.get("reflect_excluded")),
        )
    text = _quality_text(
        str(extras.get("benchmark_status") or ""),
        rollout.fail_reason,
        extras,
        None,
        [],
        [],
    )
    if _contains_any(text, _SYSTEM_ERROR_MARKERS):
        return SampleQuality("system_error", _matched_marker(text, _SYSTEM_ERROR_MARKERS), False, 0.0, False)
    if _looks_like_zero_tool_or_planner_format(text, tool_calls=rollout.n_turns, final_response=str(extras.get("final_response") or "")):
        return SampleQuality("agent_format_error", "zero tool calls, empty final response, or internal JSON plan output", False, 0.0, False)
    if str(extras.get("benchmark_status") or "") == "timeout" or rollout.fail_reason == "timeout" or _timeout_without_system_marker(text):
        return SampleQuality("timeout", "task timed out", True, TIMEOUT_SCORE_WEIGHT, False)
    if rollout.hard == 0 and _contains_any(text, _EXTERNAL_BLOCKER_MARKERS):
        return SampleQuality("external_blocker", _matched_marker(text, _EXTERNAL_BLOCKER_MARKERS), True, EXTERNAL_BLOCKER_SCORE_WEIGHT, False)
    return SampleQuality()


def _quality_text(*values: Any) -> str:
    parts: list[str] = []
    for value in values:
        if value is None:
            continue
        if isinstance(value, dict):
            parts.extend(str(v) for v in value.values() if v is not None)
        elif isinstance(value, list):
            for item in value:
                if hasattr(item, "reason"):
                    parts.append(str(getattr(item, "reason")))
                elif hasattr(item, "actual"):
                    parts.append(str(getattr(item, "actual")))
                else:
                    parts.append(str(item))
        else:
            added_attrs = False
            for attr in ("min_tool_calls", "response_exists"):
                if hasattr(value, attr):
                    parts.append(f"{attr}={getattr(value, attr)}")
                    added_attrs = True
            if not added_attrs:
                parts.append(str(value))
    return " ".join(parts).lower()


def _contains_any(text: str, markers: tuple[str, ...]) -> bool:
    return any(marker in text for marker in markers)


def _matched_marker(text: str, markers: tuple[str, ...]) -> str:
    for marker in markers:
        if marker in text:
            return marker
    return ""


def _timeout_without_system_marker(text: str) -> bool:
    return bool(re.search(r"\b(timed out|timeout|overdue_termination)\b", text)) and not _contains_any(text, _SYSTEM_ERROR_MARKERS)


def _looks_like_zero_tool_or_planner_format(text: str, *, tool_calls: int, final_response: str) -> bool:
    has_zero_tools = tool_calls == 0 or _contains_any(text, _ZERO_TOOL_MARKERS)
    if not has_zero_tools:
        return False
    return (
        "min_tool_calls=false" in text
        or "response_exists=false" in text
        or _contains_any(text, _ZERO_TOOL_MARKERS)
        or _looks_like_internal_json_plan(final_response)
    )


def _looks_like_internal_json_plan(value: str) -> bool:
    text = str(value or "").strip()
    if not text.startswith("{"):
        return False
    try:
        payload = json.loads(text)
    except json.JSONDecodeError:
        return False
    if not isinstance(payload, dict):
        return False
    keys = {str(k).lower() for k in payload}
    if "plan" in keys and keys.intersection({"objective", "completion_criteria", "next_step", "reason"}):
        return True
    mode = str(payload.get("mode") or "").strip().lower()
    if mode in {"plan", "simple"} and not str(payload.get("final_answer") or "").strip():
        return True
    return False


def _safe_int(value: Any, default: int = 0) -> int:
    try:
        return int(value)
    except (TypeError, ValueError):
        return default


def _safe_float(value: Any, default: float = 0.0) -> float:
    try:
        return float(value)
    except (TypeError, ValueError):
        return default


@dc.dataclass
class AggregateScore:
    hard: float        # mean(hard)  — fraction of tasks fully passing
    soft: float        # mean(soft)  — rubric pass-rate
    n: int             # number of tasks
    n_passed: int
    n_raw: int = 0
    n_excluded: int = 0
    n_downweighted: int = 0
    weight_total: float = 0.0

    @property
    def primary(self) -> float:
        """Primary score used by the gate. Hard is more reliable on real-device runs."""
        return self.hard


def aggregate_score(results: list[TaskResult] | list[RolloutResult]) -> AggregateScore:
    """Compute (hard, soft) means over a list of results."""
    if not results:
        return AggregateScore(hard=0.0, soft=0.0, n=0, n_passed=0, n_raw=0, weight_total=0.0)
    hard_weighted = 0.0
    soft_weighted = 0.0
    weight_total = 0.0
    n = 0
    n_passed = 0
    n_excluded = 0
    n_downweighted = 0
    for r in results:
        if isinstance(r, TaskResult):
            ro = task_result_to_rollout(r)
        else:
            ro = r
        quality = rollout_sample_quality(ro)
        if not quality.include_in_score or quality.score_weight <= 0:
            n_excluded += 1
            continue
        weight = quality.score_weight
        n += 1
        if weight < 1.0:
            n_downweighted += 1
        n_passed += ro.hard
        hard_weighted += ro.hard * weight
        soft_weighted += ro.soft * weight
        weight_total += weight
    if weight_total <= 0:
        return AggregateScore(hard=0.0, soft=0.0, n=0, n_passed=0, n_raw=len(results), n_excluded=n_excluded, n_downweighted=n_downweighted, weight_total=0.0)
    return AggregateScore(
        hard=hard_weighted / weight_total,
        soft=soft_weighted / weight_total,
        n=n,
        n_passed=n_passed,
        n_raw=len(results),
        n_excluded=n_excluded,
        n_downweighted=n_downweighted,
        weight_total=weight_total,
    )


@dc.dataclass
class GateDecision:
    accepted: bool
    reason: str
    candidate_score: float
    current_score: float
    delta: float


def validation_gate(
    candidate: AggregateScore,
    current: AggregateScore,
    min_delta: float = DEFAULT_MIN_DELTA,
) -> GateDecision:
    """Decide whether to accept a candidate.

    Rule:
      candidate.primary > current.primary + min_delta  -> accept
      candidate.primary == current.primary             -> reject (we don't
        compress in v1; future: accept if candidate skill is shorter)
      candidate.primary <  current.primary             -> reject (regression)
    """
    if min_delta < 0:
        raise ValueError(f"min_delta must be non-negative, got {min_delta}")

    delta = candidate.primary - current.primary
    if delta > min_delta:
        return GateDecision(
            accepted=True,
            reason=f"candidate hard {candidate.primary:.3f} > current {current.primary:.3f} (+{delta:.3f}, min_delta={min_delta})",
            candidate_score=candidate.primary,
            current_score=current.primary,
            delta=delta,
        )
    return GateDecision(
        accepted=False,
        reason=f"candidate hard {candidate.primary:.3f} not better than current {current.primary:.3f} (delta={delta:+.3f}, min_delta={min_delta})",
        candidate_score=candidate.primary,
        current_score=current.primary,
        delta=delta,
    )
