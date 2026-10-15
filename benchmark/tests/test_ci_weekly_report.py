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


def test_window_is_the_seven_days_before_end_with_end_exclusive():
    data = [_record("2026-10-13T00:00:00+00:00"), _record("2026-10-06T00:00:00+00:00"),
            _record("2026-10-05T23:59:59+00:00")]
    report = summarize_weekly(data, end=END)
    assert report["start"] == "2026-10-06T00:00:00+00:00"
    assert report["totals"]["suite_runs"] == 1


def test_rates_weight_eligible_counts_overall_and_per_suite():
    report = summarize_weekly([_record(passed=8), _record(tasks=4, eligible_tasks=2, passed=0),
                               _record(suite="perception", passed=2)], end=END)
    assert report["totals"]["pass_at_1"] == pytest.approx(10 / 18)
    memory = next(s for s in report["suites"] if s["suite"] == "memory")
    assert memory["suite_runs"] == 2 and memory["pass_at_1"] == 0.8
    assert report["totals"]["cost_usd_sum"] == 3.0


def test_suites_are_ordered_weakest_first_with_unknown_rates_last():
    report = summarize_weekly([_record(suite="strong", passed=8), _record(suite="weak", passed=1),
                               _record(suite="unknown", missing_metrics=["passed"])], end=END)
    assert [s["suite"] for s in report["suites"]] == ["weak", "strong", "unknown"]


def test_missing_metrics_or_cost_are_visible_without_false_zero():
    report = summarize_weekly([_record(missing_metrics=["passed"], cost_usd_sum=None)], end=END)
    assert report["totals"]["pass_at_1"] is None
    assert report["totals"]["cost_usd_sum"] is None
    markdown = render_weekly(report, ["Missing scores"])
    assert "Missing scores" in markdown
    assert "| 1 | 0 | 0 / 0 | — | — | — |" in markdown


def test_markdown_escapes_untrusted_suite_names():
    report = summarize_weekly([_record(suite="<img src=x>|\\nhello")], end=END)
    markdown = render_weekly(report, [])
    assert "<img" not in markdown
    assert "&#124;" in markdown


def test_changes_compare_each_suite_and_the_total_with_the_previous_seven_days():
    report = summarize_weekly([
        _record(passed=6), _record("2026-10-01T00:00:00+00:00", passed=4),
        _record(suite="new", passed=2),
        _record(suite="gone", timestamp="2026-10-01T00:00:00+00:00"),
        _record(suite="unmeasured", passed=4), _record("2026-10-01T00:00:00+00:00", suite="unmeasured", missing_metrics=["passed"]),
    ], end=END)
    suites = {s["suite"]: s for s in report["suites"]}
    assert suites["memory"]["previous_pass_at_1"] == 0.5
    assert suites["memory"]["change"] == pytest.approx(0.25)
    # No previous run, or no previous measured rate, is unknown rather than zero.
    assert suites["new"]["change"] is None and suites["unmeasured"]["change"] is None
    # A suite that only ran last week is not listed for this week.
    assert "gone" not in suites
    assert report["change"] == pytest.approx(12 / 24 - 8 / 16)
    assert "+25.0 pp" in render_weekly(report, [])


def test_empty_window_is_reported_explicitly():
    assert "No benchmark runs in this window" in render_weekly(summarize_weekly([], end=END), [])


@pytest.mark.parametrize("metric", ["cost_usd", "total_tokens"])
def test_resource_totals_require_every_eligible_attempt(metric):
    distributions = {"cost_usd": {"count": 8}, "total_tokens": {"count": 8}}
    distributions[metric]["count"] = 7
    report = summarize_weekly([_record(distributions=distributions)], end=END)
    assert report["totals"][f"{metric}_sum"] is None


def test_cost_preserves_free_calls_and_small_billed_amounts():
    report = summarize_weekly([_record(cost_usd_sum=0)], end=END)
    assert report["totals"]["cost_usd_sum"] == 0
    assert "| 0 | 200 |" in render_weekly(report, [])
    report = summarize_weekly([_record(cost_usd_sum=0.00000012)], end=END)
    assert "1.2e-07" in render_weekly(report, [])
