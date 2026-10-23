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
    assert suites["memory"]["config_changes"] == []


def test_changes_name_settings_that_differ_between_the_windows():
    report = summarize_weekly([
        _record(passed=2, agent_model="anthropic/new", judge_prompt_version="v3"),
        _record("2026-10-01T00:00:00+00:00", passed=6, judge_prompt_version="v2"),
    ], end=END)
    suite = report["suites"][0]
    assert suite["change"] == pytest.approx(-0.5)
    assert suite["config_changes"] == [
        {"field": "agent model", "previous": "model", "current": "anthropic/new"},
        {"field": "judge prompt", "previous": "v2", "current": "v3"},
    ]
    assert "agent model: model → anthropic/new; judge prompt: v2 → v3" in render_weekly(report, [])


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


def test_config_changes_cover_every_comparison_setting():
    report = summarize_weekly([
        _record(judge_provider="b", active_skills=["device-operator"]),
        _record("2026-10-01T00:00:00+00:00", judge_provider="a", active_skills=[]),
    ], end=END)
    fields = [c["field"] for c in report["suites"][0]["config_changes"]]
    assert fields == ["judge provider", "skills"]


def test_capabilities_group_suites_and_compare_with_the_previous_week():
    from ci.weekly_report import capability_of

    report = summarize_weekly([
        _record(suite="memory_v1", passed=8), _record(suite="quick_capture_v1", passed=4),
        _record(suite="perception_v1", passed=6), _record(suite="unlisted_v1", passed=2),
        _record("2026-10-01T00:00:00+00:00", suite="memory_v1", passed=4),
    ], end=END)
    caps = {c["capability"]: c for c in report["capabilities"]}
    # Order follows CAPABILITY_SUITES, with unmapped suites last.
    assert [c["capability"] for c in report["capabilities"]] == ["Memory", "Visual grounding", "Other"]
    assert caps["Memory"]["suites"] == ["memory_v1", "quick_capture_v1"]
    assert caps["Memory"]["pass_at_1"] == pytest.approx(12 / 16)
    assert caps["Memory"]["change"] == pytest.approx(12 / 16 - 4 / 8)
    assert caps["Visual grounding"]["change"] is None
    assert capability_of("unlisted_v1") == "Other"
    markdown = render_weekly(report, [])
    assert "### By test area" in markdown
    # Tasks: latest eligible count per suite; first attempts: every run summed.
    assert "| Memory | memory_v1, quick_capture_v1 | 16 | 1 | 16 | 75.0% | 50.0% | +25.0 pp |" in markdown


def test_capability_counts_separate_tasks_from_repetitions():
    # Two suites, each run in the same three workflow runs.
    runs = [("2026-10-10T00:00:00+00:00", "1"), ("2026-10-11T00:00:00+00:00", "2"),
            ("2026-10-12T00:00:00+00:00", "3")]
    report = summarize_weekly([
        _record(timestamp, suite=suite, workflow_run_id=run, eligible_tasks=size, passed=size)
        for timestamp, run in runs for suite, size in (("memory_v1", 16), ("quick_capture_v1", 10))
    ], end=END)
    memory = report["capabilities"][0]
    assert memory["task_count"] == 26
    assert memory["run_count"] == 3
    assert memory["eligible_tasks"] == 78
    assert "| Memory | memory_v1, quick_capture_v1 | 26 | 3 | 78 | 100.0% |" in render_weekly(report, [])


def test_capability_run_count_distinguishes_workflow_reruns_and_groups_suites():
    report = summarize_weekly([
        _record(f"2026-10-{10 + attempt}T00:00:00+00:00", suite=suite,
                workflow_run_id="123", workflow_run_attempt=attempt)
        for attempt in (1, 2) for suite in ("memory_v1", "quick_capture_v1")
    ], end=END)
    memory = report["capabilities"][0]
    assert memory["run_count"] == 2
    assert memory["eligible_tasks"] == 32
    assert memory["task_count"] == 16


def test_capability_table_labels_first_attempt_denominator_when_k_exceeds_one():
    report = summarize_weekly([
        _record(suite="memory_v1", metrics_k=3, total_attempts=30, eligible_attempts=24)
    ], end=END)
    markdown = render_weekly(report, [])
    assert "| Runs | Eligible first attempts |" in markdown
    assert "| Memory | memory_v1 | 8 | 1 | 8 | 50.0% |" in markdown


def test_every_ci_suite_has_a_capability():
    import json
    from pathlib import Path
    from ci.weekly_report import OTHER_CAPABILITY, capability_of

    root = Path(__file__).resolve().parents[1]
    for case in json.loads((root / "ci" / "suites.json").read_text())["cases"]:
        name = json.loads((root / case["suite"]).read_text())["name"]
        assert capability_of(name) != OTHER_CAPABILITY, case["id"]


@pytest.mark.parametrize("suite,area", [
    ("shell_utility_v1", "Shell utilities"),
    ("episode_memory_before_v1", "Memory"), ("episode_memory_after_v1", "Memory"),
    ("episode_memory_legacy_v1", "Memory"), ("adb_android_basic", "UI operation"),
    ("vphone_ios_basic", "UI operation"), ("app_workflow_v1", "UI operation"),
    ("quick_action_v1", "UI operation"), ("phone_control_v1", "UI operation"),
    ("mobilegym_basic", "UI operation"),
    ("aiden_app_connection_capabilities_v1", "Device integration"),
    ("aiden_app_phone_bridge_data_policy_v1", "Device integration"),
    ("aiden_app_notes_entry_policy_v1", "Device integration"),
    ("skill_discovery_v1", "Diagnostics"),
])
def test_areas_follow_test_objectives_instead_of_tools_or_environments(suite, area):
    from ci.weekly_report import capability_of
    assert capability_of(suite) == area


def test_all_suite_files_are_classified_once():
    import json
    from pathlib import Path
    from ci.weekly_report import CAPABILITY_SUITES, DIAGNOSTIC_SUITES

    mapped = [suite for suites in CAPABILITY_SUITES.values() for suite in suites] + list(DIAGNOSTIC_SUITES)
    names = {json.loads(p.read_text())["name"] for p in (Path(__file__).resolve().parents[1] / "suites").rglob("*.json")}
    assert len(mapped) == len(set(mapped))
    assert set(mapped) == names


def test_diagnostics_use_observations_and_do_not_inflate_scored_results():
    report = summarize_weekly([
        _record(suite="memory_v1", passed=4),
        _record(suite="skill_discovery_v1", passed=8, skill_discovery={"read": 1, "observed": 4}),
        _record("2026-10-01T00:00:00+00:00", suite="skill_discovery_v1",
                skill_discovery={"read": 0, "observed": 4}),
    ], end=END)
    assert report["totals"]["pass_at_1"] == 0.5
    assert report["totals"]["suite_runs"] == 1
    assert [c["capability"] for c in report["capabilities"]] == ["Memory"]
    assert [s["suite"] for s in report["suites"]] == ["memory_v1"]
    assert report["diagnostics"] == [{"suite": "skill_discovery_v1", "read": 1, "observed": 4,
                                      "rate": 0.25, "change": 0.25}]
    markdown = render_weekly(report, [])
    assert "### Diagnostics" in markdown and "skill read 25.0%" in markdown


@pytest.mark.parametrize("observation", [None, {}, {"read": 5, "observed": 4}])
def test_missing_or_invalid_diagnostic_observations_remain_unknown(observation):
    report = summarize_weekly([
        _record(suite="skill_discovery_v1", passed=8, skill_discovery=observation)
    ], end=END)
    assert report["diagnostics"][0]["rate"] is None
    assert "observation data unavailable" in render_weekly(report, [])


def test_environment_is_independent_of_suite_objective():
    from ci.weekly_report import capability_of, environment_of
    assert capability_of("phone_control_v1") == "UI operation"
    assert environment_of({"suite": "phone_control_v1", "execution_environment": "mobilegym"}) == "Simulator"
    assert environment_of({"suite": "phone_control_v1", "execution_environment": "external"}) == "External device"
    assert environment_of({"suite": "mobilegym_basic"}) == "Unknown"
