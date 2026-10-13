from datetime import datetime, timezone

import pytest

from ci.weekly_report import render_weekly, summarize_weekly

END = datetime(2026, 10, 13, tzinfo=timezone.utc)


def _record(timestamp="2026-10-12T00:00:00+00:00", **overrides):
    r = {"suite": "memory", "timestamp": timestamp, "git_branch": "main",
         "suite_sha256": "suite-v1", "workload_sha256": "tasks-v1",
         "metrics_schema_version": "p0-v1", "metrics_k": "1", "target_platform": "auto",
         "agent_model": "model", "tasks": 10, "eligible_tasks": 8,
         "passed": 4, "cost_usd_sum": 1.0, "total_tokens_sum": 200,
         "eligible_attempts": 8,
         "distributions": {"cost_usd": {"count": 8}, "total_tokens": {"count": 8}},
         "missing_metrics": []}
    r.update(overrides)
    return r


def test_weekly_windows_exclude_end_and_include_each_start_once():
    data = [_record("2026-10-13T00:00:00+00:00"), _record("2026-10-06T00:00:00+00:00"),
            _record("2026-09-29T00:00:00+00:00"), _record("2026-09-28T23:59:59+00:00")]
    report = summarize_weekly(data, end=END)
    assert report["current"]["suite_runs"] == 1
    assert report["previous"]["suite_runs"] == 1


def test_weekly_rates_weight_eligible_counts_and_match_configurations():
    report = summarize_weekly([_record(passed=8), _record(tasks=4, eligible_tasks=2, passed=0),
        _record("2026-10-01T00:00:00+00:00", passed=4)], end=END)
    assert report["current"]["pass_at_1"] == 0.8
    assert report["suites"][0]["delta"] == pytest.approx(0.3)
    assert report["current"]["cost_usd_sum"] == 2.0


def test_different_models_and_unknown_metadata_do_not_produce_deltas():
    report = summarize_weekly([_record(agent_model="new"), _record(workload_sha256=None),
        _record("2026-10-01T00:00:00+00:00", agent_model="old")], end=END)
    assert all(s["delta"] is None for s in report["suites"])
    assert report["uncomparable_suite_runs"] == 1


def test_missing_metrics_or_cost_are_visible_without_false_zero_or_delta():
    report = summarize_weekly([_record(missing_metrics=["passed"], cost_usd_sum=None),
        _record("2026-10-01T00:00:00+00:00")], end=END)
    assert report["current"]["pass_at_1"] is None
    assert report["current"]["cost_usd_sum"] is None
    assert report["suites"][0]["delta"] is None
    markdown = render_weekly(report, ["Missing scores"])
    assert "Missing scores" in markdown
    assert "| Last 7 days | 1 | 0 |" in markdown


def test_weekly_markdown_escapes_untrusted_suite_names():
    report = summarize_weekly([_record(suite="<img src=x>|\\nhello")], end=END)
    markdown = render_weekly(report, [])
    assert "<img" not in markdown
    assert "&#124;" in markdown


@pytest.mark.parametrize("metric", ["cost_usd", "total_tokens"])
def test_weekly_resource_totals_require_every_eligible_attempt(metric):
    distributions = {"cost_usd": {"count": 8}, "total_tokens": {"count": 8}}
    distributions[metric]["count"] = 7
    report = summarize_weekly([_record(distributions=distributions)], end=END)
    assert report["current"][f"{metric}_sum"] is None


def test_weekly_cost_preserves_free_calls_and_small_billed_amounts():
    report = summarize_weekly([_record(cost_usd_sum=0)], end=END)
    assert report["current"]["cost_usd_sum"] == 0
    assert "| 0 | 200 |" in render_weekly(report, [])
    report = summarize_weekly([_record(cost_usd_sum=0.00000012)], end=END)
    assert "1.2e-07" in render_weekly(report, [])
