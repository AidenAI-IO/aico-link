"""Unit tests for score.py."""
import pytest

from runner.models import HardAssertionFailure, HardAssertionResults, RubricVerdict, TaskResult
from skillopt.score import (
    aggregate_score,
    rollout_sample_quality,
    task_result_to_rollout,
    validation_gate,
)


def _mk_task_result(task_id: str, status: str, rubric_pass: int = 0, rubric_total: int = 0):
    return TaskResult(
        suite="s", run_id="r", task_id=task_id, category="c", attempt=1,
        status=status,
        rubric=[],
        rubric_pass_count=rubric_pass,
        rubric_total=rubric_total,
        hard_assertions=HardAssertionResults(),
        metrics={"tool_calls": 3},
        artifact_dir="/tmp/t",
        description_for_judge="desc",
    )


def test_task_result_to_rollout_passed():
    tr = _mk_task_result("t1", "passed", 2, 2)
    ro = task_result_to_rollout(tr)
    assert ro.id == "t1"
    assert ro.hard == 1
    assert ro.soft == 1.0


def test_task_result_to_rollout_failed_partial_rubric():
    tr = _mk_task_result("t2", "failed", 1, 2)
    ro = task_result_to_rollout(tr)
    assert ro.hard == 0
    assert ro.soft == 0.5


def test_task_result_to_rollout_timeout():
    tr = _mk_task_result("t3", "timeout")
    ro = task_result_to_rollout(tr)
    assert ro.hard == 0
    assert ro.fail_reason == "timeout"


def test_aggregate_score_mixed():
    results = [
        _mk_task_result("a", "passed", 2, 2),
        _mk_task_result("b", "failed", 1, 2),
        _mk_task_result("c", "passed", 2, 2),
    ]
    agg = aggregate_score(results)
    assert agg.n == 3
    assert agg.n_passed == 2
    # hard = 2/3
    assert abs(agg.hard - 2/3) < 1e-6
    # soft = (1.0 + 0.5 + 1.0) / 3
    assert abs(agg.soft - 2.5/3) < 1e-6


def test_aggregate_score_empty():
    agg = aggregate_score([])
    assert agg.n == 0
    assert agg.hard == 0.0
    assert agg.soft == 0.0


def test_aggregate_score_excludes_polluted_samples_and_downweights_timeouts():
    passed = _mk_task_result("pass", "passed", 2, 2)
    timeout = _mk_task_result("timeout", "timeout", 0, 2)
    agent_error = _mk_task_result("agent_error", "failed", 0, 2)
    agent_error.metrics = {
        "tool_calls": 4,
        "error": "Agent Error: planner/executor role model call timed out after 2m0s HTTP 500",
    }
    zero_tool_plan = _mk_task_result("zero_tool", "failed", 0, 2)
    zero_tool_plan.metrics = {"tool_calls": 0, "final_response": '{"plan":["open app"]}'}
    zero_tool_plan.hard_assertions = HardAssertionResults(min_tool_calls=False, response_exists=True)
    zero_tool_plan.hard_assertion_failures = [
        HardAssertionFailure(
            id="min_tool_calls",
            label="Minimum tool calls",
            requirement="at least 1 tool call",
            actual="0 tool calls",
        )
    ]

    agg = aggregate_score([passed, timeout, agent_error, zero_tool_plan])

    assert agg.n_raw == 4
    assert agg.n == 2
    assert agg.n_excluded == 2
    assert agg.n_downweighted == 1
    assert agg.weight_total == pytest.approx(1.35)
    assert agg.hard == pytest.approx(1 / 1.35)
    assert agg.soft == pytest.approx(1 / 1.35)


def test_task_result_to_rollout_preserves_sample_quality_metadata():
    tr = _mk_task_result("docker", "skipped")
    tr.metrics = {"error": "Docker compose startup failed: container exited"}

    rollout = task_result_to_rollout(tr)
    quality = rollout_sample_quality(rollout)

    assert quality.kind == "system_error"
    assert quality.include_in_score is False
    assert quality.include_in_reflect is False
    assert rollout.extras["sample_quality"] == "system_error"


def test_validation_gate_accept():
    cur = aggregate_score([_mk_task_result("a", "failed"), _mk_task_result("b", "passed")])
    cand = aggregate_score([_mk_task_result("a", "passed"), _mk_task_result("b", "passed")])
    decision = validation_gate(cand, cur, min_delta=0.03)
    assert decision.accepted
    assert decision.delta > 0


def test_validation_gate_reject_regression():
    cur = aggregate_score([_mk_task_result("a", "passed"), _mk_task_result("b", "passed")])
    cand = aggregate_score([_mk_task_result("a", "failed"), _mk_task_result("b", "passed")])
    decision = validation_gate(cand, cur, min_delta=0.03)
    assert not decision.accepted
    assert decision.delta < 0


def test_validation_gate_reject_no_change():
    cur = aggregate_score([_mk_task_result("a", "passed")])
    cand = aggregate_score([_mk_task_result("a", "passed")])
    decision = validation_gate(cand, cur, min_delta=0.03)
    assert not decision.accepted
    assert decision.delta == 0


def test_validation_gate_reject_below_min_delta():
    # 4/5 vs 3/5 = +0.20 (clearly above 0.03), but with 100/100 vs 99/100, +0.01 < 0.03
    cur_results = [_mk_task_result(f"t{i}", "passed") for i in range(99)] + [_mk_task_result("t99", "failed")]
    cand_results = [_mk_task_result(f"t{i}", "passed") for i in range(100)]
    cur = aggregate_score(cur_results)
    cand = aggregate_score(cand_results)
    decision = validation_gate(cand, cur, min_delta=0.03)
    # delta = 0.01, below min_delta
    assert not decision.accepted


def test_validation_gate_rejects_negative_min_delta():
    cur = aggregate_score([_mk_task_result("a", "passed")])
    cand = aggregate_score([_mk_task_result("a", "failed")])

    with pytest.raises(ValueError, match="min_delta must be non-negative"):
        validation_gate(cand, cur, min_delta=-0.5)


def test_typesafe_sample_classifier_overrides_rules_only_when_confident(monkeypatch):
    from skillopt import typesafe

    answers = iter([
        {"answers": {"kind": {"type": "choice", "choice": "external_blocker", "confidence": 0.93}}},
        {"answers": {"kind": {"type": "choice", "choice": "external_blocker", "confidence": 0.4}}},
    ])
    monkeypatch.setenv("SKILLOPT_SAMPLE_CLASSIFIER", "typesafe")
    monkeypatch.setenv("TYPESAFE_API_KEY", "ts-key")
    monkeypatch.setattr(typesafe, "ask", lambda *args, **kwargs: next(answers))

    tr = _mk_task_result("login", "failed", 0, 2)
    tr.metrics = {"tool_calls": 6, "final_response": "The app asks me to sign in first."}
    overridden = task_result_to_rollout(tr)
    assert overridden.extras["sample_quality"] == "external_blocker"
    assert overridden.extras["sample_quality_rule"] == "clean"
    assert rollout_sample_quality(overridden).score_weight == pytest.approx(0.5)

    kept = task_result_to_rollout(tr)
    assert kept.extras["sample_quality"] == "clean"
    assert "sample_quality_rule" not in kept.extras


def test_typesafe_sample_classifier_falls_back_to_rules_on_error(monkeypatch):
    from skillopt import typesafe

    def fail(*args, **kwargs):
        raise RuntimeError("typesafe HTTP 451")

    monkeypatch.setenv("SKILLOPT_SAMPLE_CLASSIFIER", "typesafe")
    monkeypatch.setenv("TYPESAFE_API_KEY", "ts-key")
    monkeypatch.setattr(typesafe, "ask", fail)
    tr = _mk_task_result("timeout", "timeout", 0, 2)
    assert task_result_to_rollout(tr).extras["sample_quality"] == "timeout"
