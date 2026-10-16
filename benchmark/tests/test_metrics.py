from datetime import datetime, timezone

import pytest

from runner.metrics import (
    aggregate,
    aggregate_rows,
    derive_episode_metrics,
    derive_history_metrics,
)
from runner.models import TaskResult


@pytest.mark.parametrize("cost", [0.0, 0.0024])
def test_episode_billing_reaches_langfuse_history_distributions(cost):
    from ci.fetch_langfuse_history import _record
    from runner.langfuse_reporter import _run_scores

    metrics = derive_episode_metrics({"extra": {
        "llm_calls": 2, "llm_time_ms": 80.5, "cost_reported_calls": 2,
        "cost_usd": cost, "cost_source": "provider_reported",
    }})
    assert metrics["cost_usd"] == cost
    assert metrics["cost_source"] == "provider_reported"
    assert metrics["llm_calls"] == 2
    assert metrics["llm_time_ms"] == 80.5
    agg = aggregate_rows([{"task_id": "a", "attempt": 1, "status": "passed", "metrics": metrics}])
    scores = {score["name"]: score["value"] for score in _run_scores(agg)}
    record = _record(metadata={"benchmark_suite": "test"}, run_id="test",
                     timestamp=datetime.now(timezone.utc), scores=scores,
                     experiment_id="exp", source="langfuse")
    distribution = record["distributions"]["cost_usd"]
    assert distribution == {"count": 1, "mean": cost, "p90": cost}
    assert record["cost_usd_sum"] == cost
    assert record["distributions"]["llm_calls"]["mean"] == 2
    assert record["distributions"]["llm_time_ms"]["p50"] == 80.5


@pytest.mark.parametrize("cost", [None, -1, True, "0.001", float("nan"), float("inf")])
def test_episode_invalid_or_unreported_cost_is_unknown(cost):
    metrics = derive_episode_metrics({"extra": {"cost_usd": cost, "llm_calls": 3, "cost_reported_calls": 2}})
    assert metrics.get("cost_usd") is None
    assert metrics.get("cost_source") is None
    agg = aggregate_rows([{"task_id": "a", "status": "passed", "metrics": metrics}])
    assert agg["cost_usd"]["count"] == 0
    assert agg["cost_usd"]["sum"] is None


def test_episode_partial_billing_is_not_a_task_total():
    metrics = derive_episode_metrics({"extra": {
        "cost_usd": 0.001, "llm_calls": 3, "cost_reported_calls": 2,
    }})
    assert metrics.get("cost_usd") is None
    assert metrics.get("cost_source") is None


def test_aggregate_counts_all_trace_observation_ids():
    results = [
        TaskResult(
            suite="suite",
            run_id="run",
            task_id="plain",
            category="single_step",
            attempt=1,
            status="passed",
            rubric=[],
            metrics={
                "trace_observations": [
                    {"id": "used_enter_text", "passed": False},
                    {"id": "used_search_launch_app", "passed": False},
                ]
            },
        ),
        TaskResult(
            suite="suite",
            run_id="run",
            task_id="text",
            category="multi_step",
            attempt=1,
            status="passed",
            rubric=[],
            metrics={
                "trace_observations": [
                    {"id": "used_enter_text", "passed": True},
                    {"id": "used_search_launch_app", "passed": True},
                ]
            },
        ),
    ]

    agg = aggregate(results)

    assert agg["trace_observations"] == {
        "used_enter_text": {"tasks_with_observation": 1, "tasks_observed": 2},
        "used_search_launch_app": {"tasks_with_observation": 1, "tasks_observed": 2},
    }


def test_aggregate_rows_computes_fixed_k_pass_metrics_and_eligibility():
    rows = [
        {"task_id": "a", "attempt": 1, "status": "passed", "metrics": {"task_wall_ms": 100}},
        {"task_id": "a", "attempt": 2, "status": "failed", "metrics": {"task_wall_ms": 200}},
        {"task_id": "a", "attempt": 3, "status": "passed", "metrics": {"task_wall_ms": 300}},
        {"task_id": "b", "attempt": 1, "status": "failed", "metrics": {"agent_eligible": False}},
        {"task_id": "b", "attempt": 2, "status": "passed", "metrics": {}},
        {"task_id": "b", "attempt": 3, "status": "passed", "metrics": {}},
    ]

    agg = aggregate_rows(rows, k=3)

    assert agg["metrics_k"] == 3
    assert agg["pass_at_k"]["eligible_tasks"] == 1
    assert agg["pass_at_k"]["successes"] == 1
    assert agg["pass_at_k"]["value"] == 1.0
    assert agg["pass_pow_k"]["value"] == 0.0
    assert agg["pass_at_k"]["coverage"] == 0.5
    assert agg["pass_at_1"]["eligible_tasks"] == 1
    assert agg["pass_at_1"]["value"] == 1.0


def test_aggregate_rows_reports_percentiles_and_quality_best_score():
    rows = [
        {"task_id": "a", "attempt": 1, "status": "failed", "metrics": {"quality_score": 0.25, "task_wall_ms": 100, "llm_calls": 2}},
        {"task_id": "a", "attempt": 2, "status": "passed", "metrics": {"quality_score": 1.0, "task_wall_ms": 300, "llm_calls": 4}},
    ]

    agg = aggregate_rows(rows, k=2)

    assert agg["task_wall_ms"]["p50"] == 200.0
    assert agg["task_wall_ms"]["p90"] == 280.0
    assert agg["llm_calls"]["p50"] == 3.0
    assert agg["oracle_best_score_at_k"]["value"] == 1.0


def test_aggregate_rows_excludes_ineligible_attempts_from_efficiency_metrics():
    rows = [
        {
            "task_id": "valid",
            "attempt": 1,
            "status": "passed",
            "metrics": {"agent_eligible": True, "task_wall_ms": 100, "llm_calls": 2},
        },
        {
            "task_id": "invalid",
            "attempt": 1,
            "status": "skipped",
            "metrics": {"agent_eligible": False, "task_wall_ms": 0, "llm_calls": 0},
        },
    ]

    agg = aggregate_rows(rows, k=1)

    assert agg["task_wall_ms"]["count"] == 1
    assert agg["task_wall_ms"]["p50"] == 100
    assert agg["llm_calls"]["count"] == 1
    assert agg["llm_calls"]["p50"] == 2


def test_aggregate_rows_maps_legacy_status_without_overriding_explicit_metrics():
    rows = [
        {"task_id": "agent", "attempt": 1, "status": "failed", "metrics": {}},
        {"task_id": "env", "attempt": 1, "status": "skipped", "metrics": {"failure_class": "environment", "agent_eligible": False}},
        {"task_id": "judge", "attempt": 1, "status": "judge_error", "metrics": {}},
    ]

    agg = aggregate_rows(rows, k=1)

    assert agg["failure_classes"] == {"agent": 1, "environment": 1, "evaluation": 1}
    assert agg["agent_eligible_attempts"] == 1


def test_derive_history_metrics_counts_usage_actions_and_initial_image():
    history = [
        {"type": "user", "content": "tap it", "attachments": [{"kind": "image"}]},
        {
            "type": "tool_call",
            "tool_name": "touch_gesture",
            "tool_input": '{"type":"tap"}',
            "usage": {"input_tokens": 10, "output_tokens": 2, "total_tokens": 12},
        },
        {"type": "tool_result", "tool_name": "touch_gesture", "content": "{}"},
        {
            "type": "assistant",
            "content": "done",
            "usage": {"input_tokens": 5, "output_tokens": 1, "total_tokens": 6},
        },
    ]

    metrics = derive_history_metrics(history)

    assert metrics["llm_calls"] == 2
    assert metrics["input_tokens"] == 15
    assert metrics["output_tokens"] == 3
    assert metrics["total_tokens"] == 18
    assert metrics["device_actions"] == 1
    assert metrics["vision_llm_calls"] == 1
    assert metrics["llm_time_ms"] is None
    assert metrics["vision_llm_time_ms"] is None


def test_derive_history_metrics_does_not_infer_retry_or_replan_from_tool_names():
    history = [
        {"type": "tool_call", "tool_name": "retry_action", "tool_input": "{}"},
        {"type": "tool_call", "tool_name": "enter_plan_mode", "tool_input": "{}"},
    ]

    metrics = derive_history_metrics(history)

    assert metrics["retry_count"] is None
    assert metrics["replan_count"] is None
    assert metrics["input_tokens"] is None
    assert metrics["output_tokens"] is None
    assert metrics["total_tokens"] is None


def test_derive_history_metrics_does_not_report_partial_llm_duration():
    history = [
        {
            "type": "assistant",
            "content": "first",
            "usage": {"input_tokens": 1, "output_tokens": 1},
            "duration_ms": 10,
        },
        {
            "type": "assistant",
            "content": "second",
            "usage": {"input_tokens": 1, "output_tokens": 1},
        },
    ]

    metrics = derive_history_metrics(history)

    assert metrics["llm_time_ms"] is None


def test_derive_history_metrics_treats_missing_usage_as_unknown_call_count():
    metrics = derive_history_metrics([{"type": "assistant", "content": "done"}])

    assert metrics["llm_calls"] is None
    assert metrics["vision_llm_calls"] is None


def test_derive_history_metrics_keeps_partial_usage_fields_unknown():
    metrics = derive_history_metrics([
        {
            "type": "assistant",
            "content": "first",
            "usage": {
                "input_tokens": 4,
                "output_tokens": 1,
                "total_tokens": 5,
                "cached_input_tokens": 2,
            },
        },
        {
            "type": "assistant",
            "content": "second",
            "usage": {"total_tokens": 7},
        },
    ])

    assert metrics["input_tokens"] is None
    assert metrics["output_tokens"] is None
    assert metrics["total_tokens"] == 12
    assert metrics["cached_input_tokens"] is None
    assert metrics["reasoning_tokens"] is None


def test_derive_history_metrics_counts_state_images_and_known_device_actions():
    metrics = derive_history_metrics([
        {"type": "state", "attachments": [{"kind": "image"}]},
        {
            "type": "tool_call",
            "tool_name": "keyboard_tap",
            "tool_input": '{"keys":["enter"]}',
            "usage": {"input_tokens": 2, "output_tokens": 1},
        },
        {"type": "tool_call", "tool_name": "mouse_move", "tool_input": '{"x":1,"y":2}'},
        {"type": "tool_call", "tool_name": "mouse_scroll", "tool_input": '{"delta":-1}'},
        {"type": "tool_call", "tool_name": "quick_action", "tool_input": '{"list":true}'},
        {"type": "tool_call", "tool_name": "bridge_contacts", "tool_input": '{"action":"query"}'},
    ])

    assert metrics["vision_llm_calls"] == 1
    assert metrics["device_actions"] == 3


def test_derive_history_metrics_records_evidence_backed_device_failure():
    metrics = derive_history_metrics([
        {"type": "tool_call", "tool_name": "open_app", "tool_input": '{"app":"Contacts"}'},
        {
            "type": "tool_result",
            "tool_name": "open_app",
            "content": '{"ok":false,"error":"not available"}',
        },
    ])

    assert metrics["tool_errors"] == 1
    assert metrics["first_failure_stage"] == "device_execution"
    assert metrics["failure_event_ref"] == "history.json#/1"


def test_derive_episode_metrics_uses_run_usage_and_device_action_durations():
    episode = {
        "extra": {
            "prompt_tokens": 100,
            "completion_tokens": 20,
            "total_tokens": 120,
            "cached_prompt_tokens": 40,
            "reasoning_tokens": 5,
            "first_token_time_ms": 125.5,
        },
        "events": [
            {
                "type": "tool_result",
                "tool_name": "bridge_contacts",
                "tool_input": '{"action":"query"}',
                "duration_ms": 11,
            },
            {
                "type": "tool_result",
                "tool_name": "touch_gesture",
                "tool_input": '{"type":"tap"}',
                "duration_ms": 23,
                "is_error": True,
                "needs_replan": True,
            },
        ],
    }

    metrics = derive_episode_metrics(episode)

    assert metrics["input_tokens"] == 100
    assert metrics["output_tokens"] == 20
    assert metrics["total_tokens"] == 120
    assert metrics["cached_input_tokens"] == 40
    assert metrics["reasoning_tokens"] == 5
    assert metrics["time_to_first_token_ms"] == 125.5
    assert metrics["device_execution_ms"] == 23
    assert metrics["tool_errors"] == 1
    assert metrics["replan_count"] == 1
    assert metrics["first_failure_stage"] == "device_execution"
    assert metrics["failure_event_ref"] == "episode.json#/events/1"


def test_derive_episode_metrics_keeps_unreported_replans_unknown():
    metrics = derive_episode_metrics({
        "events": [
            {
                "type": "tool_result",
                "tool_name": "touch_gesture",
                "tool_input": '{"type":"tap"}',
                "duration_ms": 10,
            }
        ]
    })

    assert metrics.get("replan_count") is None


def test_derive_episode_metrics_links_result_to_read_only_bridge_call():
    metrics = derive_episode_metrics({
        "events": [
            {
                "type": "tool_call",
                "tool_name": "bridge_contacts",
                "tool_input": '{"action":"query","query":"Biden"}',
            },
            {
                "type": "tool_result",
                "tool_name": "bridge_contacts",
                "duration_ms": 10,
                "is_error": True,
            },
        ]
    })

    assert metrics["device_execution_ms"] == 0
    assert metrics["first_failure_stage"] == "unknown"
    assert metrics["failure_event_ref"] == "episode.json#/events/1"


def test_aggregate_rows_reports_cumulative_cost_to_first_success():
    rows = [
        {
            "task_id": "a",
            "attempt": 1,
            "status": "failed",
            "metrics": {"total_tokens": 10, "cost_usd": 0.1, "task_wall_ms": 100},
        },
        {
            "task_id": "a",
            "attempt": 2,
            "status": "passed",
            "metrics": {"total_tokens": 20, "cost_usd": 0.2, "task_wall_ms": 200},
        },
        {
            "task_id": "b",
            "attempt": 1,
            "status": "passed",
            "metrics": {"total_tokens": 40, "cost_usd": 0.4, "task_wall_ms": 400},
        },
        {
            "task_id": "b",
            "attempt": 2,
            "status": "failed",
            "metrics": {"total_tokens": 80, "cost_usd": 0.8, "task_wall_ms": 800},
        },
    ]

    agg = aggregate_rows(rows, k=2)

    assert agg["cost_to_first_success"]["total_tokens"]["count"] == 2
    assert agg["cost_to_first_success"]["total_tokens"]["p50"] == 35
    assert agg["cost_to_first_success"]["cost_usd"]["p50"] == pytest.approx(0.35)
    assert agg["cost_to_first_success"]["task_wall_ms"]["p50"] == 350


def test_first_success_survives_a_later_ineligible_attempt():
    rows = [
        {
            "task_id": "a",
            "attempt": 1,
            "status": "passed",
            "metrics": {
                "agent_eligible": True,
                "success": True,
                "total_tokens": 10,
                "task_wall_ms": 100,
            },
        },
        {
            "task_id": "a",
            "attempt": 2,
            "status": "skipped",
            "metrics": {"agent_eligible": False, "success": None},
        },
    ]

    agg = aggregate_rows(rows, k=2)

    assert agg["pass_at_k"]["eligible_tasks"] == 0
    assert agg["first_success_attempt"]["count"] == 1
    assert agg["first_success_attempt"]["p50"] == 1
    assert agg["cost_to_first_success"]["total_tokens"]["p50"] == 10
    assert agg["cost_to_first_success"]["task_wall_ms"]["p50"] == 100


def test_first_success_uses_attempts_beyond_common_k():
    rows = [
        {
            "task_id": "long",
            "attempt": 1,
            "status": "failed",
            "metrics": {"total_tokens": 10},
        },
        {
            "task_id": "long",
            "attempt": 2,
            "status": "passed",
            "metrics": {"total_tokens": 20},
        },
        {
            "task_id": "short",
            "attempt": 1,
            "status": "failed",
            "metrics": {},
        },
    ]

    agg = aggregate_rows(rows, k=1)

    assert agg["metrics_k"] == 1
    assert agg["first_success_attempt"]["p50"] == 2
    assert agg["cost_to_first_success"]["total_tokens"]["p50"] == 30


def test_aggregate_preserves_legacy_attempt_count_with_repeats():
    results = [
        TaskResult("suite", "run", "task", "single_step", 1, "passed", [], metrics={}),
        TaskResult("suite", "run", "task", "single_step", 2, "failed", [], metrics={}),
    ]

    agg = aggregate(results)

    assert agg["tasks"] == 2
    assert agg["attempts"] == 2
    assert agg["unique_tasks"] == 1


def test_aggregate_rows_has_one_canonical_metric_shape():
    agg = aggregate_rows(
        [{"task_id": "a", "attempt": 1, "status": "passed", "metrics": {"task_wall_ms": 100}}],
        k=1,
    )

    assert "metrics" not in agg
    assert "task_wall_ms_p50" not in agg
    assert "failure_class_rates" not in agg
