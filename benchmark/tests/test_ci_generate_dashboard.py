import json
from datetime import datetime, timezone
from pathlib import Path

import pytest

from ci.fetch_langfuse_history import COMPARISON_FIELDS
from ci.generate_dashboard import (
    _group_into_runs,
    _summarize_suites,
    generate_dashboard,
)

REPORT_END = datetime(2026, 10, 13, tzinfo=timezone.utc)


def _record(**overrides):
    record = {
        "workflow_run_id": 1,
        "workflow_run_number": 1,
        "suite": "memory_v1",
        "timestamp": "2026-10-10T00:00:00+00:00",
        "commit_sha": "aaaaaaaa1111",
        "git_branch": "main",
        "agent_model": "some/model",
        "tasks": 4,
        "workflow_run_attempt": 1,
        "suite_sha256": "suite-v1",
        "judge_prompt_version": "v2",
        "workload_sha256": "tasks-v1",
        "metrics_schema_version": "p0-v1",
        "metrics_k": "1",
        "target_platform": "auto",
        "passed": 2,
        "pass_at_1": 0.5,
        "wall_ms_p50": 1000.0,
        "failure_classes": {},
        "by_category": {},
    }
    record.update(overrides)
    record.setdefault("eligible_tasks", record["tasks"])
    return record


def test_group_into_runs_weights_pass_rate_by_task_count() -> None:
    # One workflow run covers every suite, so a 2-task suite must not count as
    # much as a 10-task one: 11/12 is the run's rate, not the mean of 0.5 and 1.0.
    history = [
        _record(suite="small", tasks=2, passed=1, pass_at_1=0.5, workload_task_ids=["a", "b"]),
        _record(suite="large", tasks=10, passed=10, pass_at_1=1.0),
    ]

    runs = _group_into_runs(history)

    assert len(runs) == 1
    assert runs[0]["tasks"] == 12
    assert runs[0]["passed"] == 11
    assert runs[0]["pass_at_1"] == 11 / 12
    assert [suite["suite"] for suite in runs[0]["suites"]] == ["large", "small"]
    assert runs[0]["suites"][1]["workload_task_ids"] == ["a", "b"]


def test_group_into_runs_separates_workflow_runs_and_orders_them_oldest_first() -> None:
    history = [
        _record(workflow_run_id=2, timestamp="2026-10-11T00:00:00+00:00", commit_sha="bbbb"),
        _record(workflow_run_id=1, timestamp="2026-10-10T00:00:00+00:00", commit_sha="aaaa"),
    ]

    runs = _group_into_runs(history)

    assert [run["commit_sha"] for run in runs] == ["aaaa", "bbbb"]


def test_group_into_runs_sums_failure_classes_across_suites() -> None:
    history = [
        _record(suite="a", failure_classes={"agent": 2}),
        _record(suite="b", failure_classes={"agent": 1, "harness": 3}),
    ]

    runs = _group_into_runs(history)

    assert runs[0]["failure_classes"] == {"agent": 3, "harness": 3}


def test_group_into_runs_reports_unknown_rate_when_no_tasks_ran() -> None:
    runs = _group_into_runs([_record(tasks=0, passed=0, pass_at_1=None)])

    assert runs[0]["pass_at_1"] is None


def _k_record(**overrides):
    return _record(**{
        "metrics_k": "2", "pass_at_k": 1, "pass_at_k_successes": 2,
        "pass_at_k_eligible_tasks": 2, "pass_at_k_total_tasks": 4,
        "pass_pow_k": 0.5, "pass_pow_k_successes": 1,
        "pass_pow_k_eligible_tasks": 2, "pass_pow_k_total_tasks": 4,
        **overrides,
    })


def test_k_rates_pool_their_own_eligible_tasks():
    run = _group_into_runs([
        _k_record(suite="small"),
        _k_record(suite="large", tasks=12, eligible_tasks=10, passed=6, pass_at_1=0.6,
                  pass_at_k=0.75, pass_at_k_successes=6, pass_at_k_eligible_tasks=8,
                  pass_at_k_total_tasks=12, pass_pow_k=0.25, pass_pow_k_successes=2,
                  pass_pow_k_eligible_tasks=8, pass_pow_k_total_tasks=12),
    ])[0]
    assert run["pass_at_1"] == 8 / 14
    assert run["pass_at_k"] == 8 / 10
    assert run["pass_pow_k"] == 3 / 10
    assert run["pass_pow_k_eligible_tasks"] == 10
    assert run["metrics_k"] == 2


@pytest.mark.parametrize("overrides", [
    {"metrics_k": "3"}, {"metrics_k": None}, {"metrics_k": "unknown"},
    {"metrics_k": "0"}, {"pass_pow_k": None}, {"pass_pow_k_eligible_tasks": None},
])
def test_k_rates_do_not_hide_missing_data_or_pool_different_k(overrides):
    run = _group_into_runs([_k_record(suite="a"), _k_record(suite="b", **overrides)])[0]
    assert run["pass_pow_k"] is None
    assert run["pass_at_1"] == 0.5
    if "metrics_k" in overrides:
        assert run["pass_at_k"] is None
    else:
        assert run["pass_at_k"] == 1


def test_k_rates_remain_unknown_when_no_eligible_tasks():
    run = _group_into_runs([_k_record(pass_pow_k=None, pass_pow_k_successes=0,
                                     pass_pow_k_eligible_tasks=0)])[0]
    assert run["pass_pow_k"] is None
    assert run["pass_pow_k_eligible_tasks"] == 0


def test_summarize_suites_puts_the_weakest_suite_first_and_keeps_its_series() -> None:
    history = [
        _record(workflow_run_id=1, timestamp="2026-10-10T00:00:00+00:00", suite="strong", pass_at_1=0.95),
        _record(workflow_run_id=1, timestamp="2026-10-10T00:00:00+00:00", suite="weak", pass_at_1=0.30),
        _record(workflow_run_id=2, timestamp="2026-10-11T00:00:00+00:00", suite="strong", pass_at_1=0.97),
        _record(workflow_run_id=2, timestamp="2026-10-11T00:00:00+00:00", suite="weak", pass_at_1=0.20),
    ]

    summaries = _summarize_suites(_group_into_runs(history))

    assert [item["suite"] for item in summaries] == ["weak", "strong"]
    assert summaries[0]["delta"] == pytest.approx(-0.10)
    assert len(summaries[0]["points"]) == 2
    assert summaries[1]["delta"] > 0


def test_summarize_suites_sorts_unknown_rates_last() -> None:
    history = [
        _record(suite="unknown_rate", tasks=0, passed=0, pass_at_1=None),
        _record(suite="known_rate", pass_at_1=0.1),
    ]

    summaries = _summarize_suites(_group_into_runs(history))

    assert [item["suite"] for item in summaries] == ["known_rate", "unknown_rate"]


def test_dataset_series_preserves_own_timestamps_metrics_and_attempts():
    history = [
        _k_record(suite="a", timestamp="2026-10-10T01:00:00+00:00", run_id="a1",
                  distributions={"tool_calls": {"mean": 4.5, "count": 3}}, eligible_attempts=4,
                  failure_stages={"unknown": 2}, eligible_failures=2, failure_stage_coverage=0,
                  failure_details_status="available", failure_details_inspected=4, failure_details_expected=4,
                  failure_details=[{"task_id": '<img src=x onerror=alert(1)>', "attempt": 2,
                    "status": "failed", "agent_eligible": True,
                    "evidence": [{"kind": "Rubric", "label": "answer", "reason": '<script>alert(1)</script>'}],
                    "final_response": '<img src=x>', "trace_url": 'javascript:alert(1)'}]),
        _k_record(suite="b", timestamp="2026-10-10T00:00:00+00:00", run_id="b1"),
        _k_record(suite="a", timestamp="2026-10-11T00:00:00+00:00", run_id="a2",
                  workflow_run_attempt=2, pass_pow_k=None, pass_pow_k_successes=None),
    ]
    datasets = {s["suite"]: s for s in _summarize_suites(_group_into_runs(history))}
    points = datasets["a"]["points"]
    assert len(points) == 2
    assert points[0]["timestamp"] == "2026-10-10T01:00:00+00:00"
    assert [p["workflow_run_attempt"] for p in points] == [1, 2]
    assert points[0]["metrics_k"] == "2"
    assert points[0]["distributions"]["tool_calls"] == {"mean": 4.5, "count": 3}
    assert points[0]["eligible_attempts"] == 4
    assert points[0]["failure_stages"] == {"unknown": 2}
    assert points[0]["failure_stage_coverage"] == 0
    assert points[0]["pass_pow_k"] == 0.5
    assert points[0]["pass_at_k_eligible_tasks"] == 2
    assert points[1]["pass_pow_k"] is None
    assert datasets["a"]["latest"]["run_id"] == "a2"
    assert len(datasets["b"]["points"]) == 1


def test_generate_dashboard_escapes_markup_from_branch_and_suite_names(tmp_path: Path) -> None:
    # Branch and suite names reach the page as data. A name that closes the
    # script tag must not be able to run anything in the reader's browser.
    hostile = '</script><img src=x onerror=alert(1)>'
    output = tmp_path / "dashboard.html"

    generate_dashboard(
        history=[_record(suite=hostile, git_branch=hostile)],
        output_file=output,
        repo="owner/repo",
        report_end=REPORT_END,
    )

    html = output.read_text(encoding="utf-8")
    assert "</script><img" not in html
    assert "\\u003c" in html


def test_generate_dashboard_writes_data_and_reports_every_metric(tmp_path: Path) -> None:
    output = tmp_path / "dashboard.html"
    data_output = tmp_path / "data" / "dashboard.json"

    generate_dashboard(
        history=[
            _record(workflow_run_id=1, timestamp="2026-10-10T00:00:00+00:00",
                    suite="memory_v1", pass_at_1=0.9, tasks=10, passed=9),
            _record(workflow_run_id=2, timestamp="2026-10-11T00:00:00+00:00",
                    suite="memory_v1", pass_at_1=0.4, tasks=10, passed=4),
        ],
        output_file=output,
        data_output=data_output,
        repo="owner/repo",
        report_end=REPORT_END,
    )

    payload = json.loads(data_output.read_text(encoding="utf-8"))
    assert len(payload["runs"]) == 2
    assert payload["repo"] == "owner/repo"
    assert "regressions" not in payload
    assert payload["suites"][0]["delta"] == pytest.approx(-0.5)

    html = output.read_text(encoding="utf-8")
    assert "owner/repo" in html
    assert "Benchmark Dashboard" in html


def test_generate_dashboard_renders_without_any_history(tmp_path: Path) -> None:
    output = tmp_path / "dashboard.html"

    generate_dashboard(history=[], output_file=output, repo="owner/repo", report_end=REPORT_END)

    html = output.read_text(encoding="utf-8")
    assert "Benchmark Dashboard" in html
    assert '"runs": []' in html or '[]' in html


def test_dashboard_and_weekly_report_share_the_seven_day_window(tmp_path):
    history = [
        _record(workflow_run_id=1, timestamp="2026-10-05T23:59:59+00:00"),
        _record(workflow_run_id=2, timestamp="2026-10-06T00:00:00+00:00"),
        _record(workflow_run_id=3, timestamp="2026-10-12T23:59:59+00:00"),
        _record(workflow_run_id=4, timestamp="2026-10-13T00:00:00+00:00"),
    ]
    data = tmp_path / "data.json"
    output = tmp_path / "dashboard.html"
    generate_dashboard(history=history, output_file=output, data_output=data, report_end=REPORT_END)
    payload = json.loads(data.read_text())
    assert [run["workflow_run_id"] for run in payload["runs"]] == [2, 3]
    # Per-suite points duplicate runs, so the JSON snapshot omits them.
    assert "points" not in payload["suites"][0]
    assert len(payload["history"]) == 4
    assert payload["weekly"]["totals"]["suite_runs"] == 2
    assert "2026-10-06T00:00:00+00:00" in output.read_text()


def test_data_availability_uses_display_window_but_retains_api_errors(tmp_path):
    output = tmp_path / "dashboard.html"
    history = [
        _record(run_id="old", workflow_run_id=1, timestamp="2026-10-05T23:59:59+00:00",
                missing_metrics=["tasks"], tasks=None),
        _record(run_id="current", workflow_run_id=2, missing_metrics=["tasks"], tasks=None),
        _record(run_id="measured", workflow_run_id=3),
    ]
    warnings = ["old: missing or invalid metrics: tasks.", "current: missing or invalid metrics: tasks.",
                "Scores unavailable for 5 experiment(s): APIError."]
    generate_dashboard(history=history, output_file=output, report_end=REPORT_END, warnings=warnings)
    encoded = output.read_text().split("const DATA = JSON.parse(")[1].split(");\n")[0]
    payload = json.loads(json.loads(encoded))
    assert payload["warnings"] == warnings[1:]
    assert payload["dataCompleteness"] == {"measured": 1, "total": 2}


@pytest.mark.parametrize("overrides, direction, status", [
    ({"pass_at_1": 0.75, "passed": 3}, "improved", "Comparable"),
    ({"pass_at_1": 0.25, "passed": 1}, "declined", "Comparable"),
    ({"pass_at_1": 0.75, "passed": 3, "pass_pow_k": 0}, "mixed", "Comparable"),
    ({}, "unchanged", "Comparable"),
    # A configuration change still shows the score change and names what differs.
    ({"pass_at_1": 0.75, "passed": 3, "git_branch": None}, "improved", "Configuration unverified"),
    ({"pass_pow_k": 0, "agent_model": "different"}, "declined", "Configuration changed"),
])
def test_latest_changes_include_all_metrics_and_label_comparability(overrides, direction, status):
    summary = _summarize_suites(_group_into_runs([
        _k_record(workflow_run_id=1),
        _k_record(workflow_run_id=2, timestamp="2026-10-11T00:00:00+00:00", **overrides),
    ]))[0]
    assert summary["change_direction"] == direction
    assert summary["comparison_status"] == status
    assert summary["previous"]["workflow_run_id"] == 1
    assert summary["changed_fields"] == [field for field in overrides if field in COMPARISON_FIELDS]


def test_latest_changes_compare_the_previous_run_and_name_changed_settings():
    summary = _summarize_suites(_group_into_runs([
        _k_record(workflow_run_id=1, timestamp="2026-10-09T00:00:00+00:00"),
        _k_record(workflow_run_id=2, timestamp="2026-10-10T00:00:00+00:00", agent_model="different",
                  judge_prompt_version="v3", pass_at_1=0.0, passed=0),
    ]))[0]
    assert summary["previous"]["workflow_run_id"] == 1
    assert summary["comparison_status"] == "Configuration changed"
    assert summary["changed_fields"] == ["agent_model", "judge_prompt_version"]
    assert summary["change_direction"] == "declined"
    assert summary["metric_deltas"]["pass_at_1"] == -0.5


def test_latest_changes_do_not_skip_over_a_missing_score():
    summary = _summarize_suites(_group_into_runs([
        _record(workflow_run_id=1, timestamp="2026-10-09T00:00:00+00:00"),
        _record(workflow_run_id=2, pass_at_1=None, passed=None, eligible_tasks=None),
        _record(workflow_run_id=3, timestamp="2026-10-11T00:00:00+00:00"),
    ]))[0]
    assert summary["change_direction"] == "unavailable"
    assert summary["comparison_status"] == "Missing scores"
    assert summary["metric_deltas"]["pass_at_1"] is None


def test_latest_changes_without_a_previous_run_are_not_zero():
    summary = _summarize_suites(_group_into_runs([_k_record()]))[0]
    assert summary["change_direction"] == "unavailable"
    assert summary["comparison_status"] == "No baseline"
    assert all(value is None for value in summary["metric_deltas"].values())


def test_pass_at_1_uses_eligible_first_attempts_instead_of_total_tasks():
    runs = _group_into_runs([_record(tasks=10, eligible_tasks=4, passed=2, pass_at_1=0.5)])
    assert runs[0]["tasks"] == 10
    assert runs[0]["pass_at_1"] == 0.5


def test_missing_scores_leave_overall_rate_unknown():
    runs = _group_into_runs([_record(suite="known"), _record(suite="missing", tasks=None,
        passed=None, eligible_tasks=None, pass_at_1=None, missing_metrics=["tasks"])])
    assert runs[0]["pass_at_1"] is None
    assert runs[0]["tasks"] is None


def test_workflow_attempts_are_separate():
    runs = _group_into_runs([_record(workflow_run_attempt=1), _record(workflow_run_attempt=2)])
    assert len(runs) == 2


@pytest.mark.parametrize("field", ["agent_model", "suite_sha256", "workload_sha256",
    "metrics_k", "judge_model", "git_branch", "target_platform", "active_skills"])
def test_configuration_changes_keep_inline_deltas(field):
    history = [_record(workflow_run_id=1, pass_at_1=1.0),
               _record(workflow_run_id=2, pass_at_1=0.1, **{field: "changed"})]
    # The score change stays visible, labelled with the setting that differs.
    summary = _summarize_suites(_group_into_runs(history))[0]
    assert summary["delta"] == pytest.approx(-0.9)
    assert summary["changed_fields"] == [field]
    assert summary["comparison_status"] == "Configuration changed"


def test_unknown_configuration_keeps_inline_delta():
    history = [_record(workflow_run_id=1, pass_at_1=1.0, workload_sha256=None),
               _record(workflow_run_id=2, pass_at_1=0.1, workload_sha256=None)]
    summary = _summarize_suites(_group_into_runs(history))[0]
    assert summary["comparison_status"] == "Configuration unverified"
    assert summary["delta"] == pytest.approx(-0.9)


def test_runs_reporting_git_changes_remain_comparable():
    # CI checks out fresh, so the runner's git_dirty flag does not change the configuration.
    history = [_record(workflow_run_id=1, pass_at_1=1.0, git_dirty=True),
               _record(workflow_run_id=2, timestamp="2026-10-11T00:00:00+00:00", pass_at_1=0.5, git_dirty=True)]
    summary = _summarize_suites(_group_into_runs(history))[0]
    assert summary["comparison_status"] == "Comparable"
    assert summary["delta"] == -0.5


def test_data_snapshot_can_be_reused_by_cli(tmp_path):
    from ci.generate_dashboard import cli
    data = tmp_path / "data.json"
    generate_dashboard(history=[_record()], output_file=tmp_path / "a.html", data_output=data,
                       warnings=["Scores unavailable."], report_end=REPORT_END)
    assert cli(["--history-json", str(data), "--output", str(tmp_path / "b.html"),
                "--weekly-output", str(tmp_path / "week.md")]) == 0
    assert "Scores unavailable." in (tmp_path / "week.md").read_text()


def test_generated_javascript_escapes_html_at_the_insertion_point(tmp_path):
    import shutil
    import subprocess
    if not shutil.which("node"):
        pytest.skip("Node.js is unavailable")
    output = tmp_path / "dashboard.html"
    generate_dashboard(history=[
        _k_record(suite='<img src=x onerror=alert(1)>', eligible_attempts=4,
                  distributions={"task_wall_ms": {"p50": 14200, "p90": 31500, "count": 3},
                                 "tool_calls": {"mean": 4.5, "count": 3},
                                 "llm_calls": {"mean": 0, "count": 4},
                                 "tool_errors": {"mean": 0, "count": 4},
                                 "retry_count": {"mean": 0, "count": 0}},
                  failure_stages={"unknown": 2}, eligible_failures=2, failure_stage_coverage=0,
                  failure_details_status="available", failure_details_inspected=4, failure_details_expected=4,
                  failure_details=[{"task_id": '<img src=x onerror=alert(1)>', "attempt": 2,
                    "status": "failed", "agent_eligible": True,
                    "evidence": [{"kind": "Rubric", "label": "answer", "reason": '<script>alert(1)</script>'}],
                    "final_response": '<img src=x>', "trace_url": 'javascript:alert(1)'}]),
        _k_record(suite="other", workflow_run_id=2, pass_at_1=0.25, passed=1),
        _k_record(suite="other", workflow_run_id=3, timestamp="2026-10-12T00:00:00+00:00",
                  pass_at_1=None, passed=None, eligible_tasks=None),
        _k_record(suite="no_failures", eligible_failures=0, failure_stages={},
                  failure_details_status="available", failure_details=[], failure_details_inspected=4,
                  failure_details_expected=4),
        _k_record(suite="partial_stages", eligible_failures=2,
                  failure_stages={"tool_execution": 1}, failure_stage_coverage=0.5),
        _k_record(suite="memory_v1"),
        _k_record(suite="memory_v1", workflow_run_id=4, timestamp="2026-10-11T00:00:00+00:00",
                  judge_prompt_version="v3", git_branch='<img src=x>', suite_sha256="memory-v2"),
        _k_record(suite="episode_memory_v1"),
        _k_record(suite="episode_memory_v1", workflow_run_id=4, timestamp="2026-10-11T00:00:00+00:00",
                  judge_prompt_version="v3", git_branch='<img src=x>'),
        _k_record(suite="shell_utility_v1", execution_environment="isolated"),
        _k_record(suite="skill_discovery_v1", execution_environment="external",
                  skill_discovery={"read": 1, "observed": 4}),
    ], output_file=output, report_end=REPORT_END, warnings=["SDK unavailable <script>"],
       repo="owner/repo")
    html = output.read_text()
    assert 'Configuration context' not in html
    assert 'How to read these results' not in html
    assert 'Metric definitions' in html
    assert '<h2>Data availability</h2>' not in html
    assert 'Latest pass@1 by dataset' not in html
    assert 'Latest failure stages by dataset' not in html
    script = html.split('<script>')[1].split('</script>')[0]
    prefix = """
const elements = new Map();
global.document = {
  documentElement: {},
  querySelectorAll: () => [],
  getElementById: id => {
    if (!elements.has(id)) elements.set(id, {style: {}, listeners: {},
      addEventListener(event, callback) { this.listeners[event] = callback; },
      setAttribute(name, value) { this[name] = value; },
      focus() {}, scrollIntoView() {}, querySelector() { return {focus() {}}; }});
    return elements.get(id);
  }
};
global.getComputedStyle = () => ({ getPropertyValue: () => '#fff' });
global.Chart = function(element, config) {
  this.data = config.data;
  this.options = config.options;
  this.update = () => {};
  this.resize = () => {};
};
"""
    verification = """
if (elements.get('suites').innerHTML.includes('<img')) process.exit(1);
if (!elements.get('suites').innerHTML.includes('&lt;img')) process.exit(2);
if (selected.suite !== 'other') process.exit(3);
if (JSON.stringify(chart.data.datasets[0].data) !== '[25,null]') process.exit(4);
selectDataset(datasets.findIndex(s => s.suite !== 'other'));
if (JSON.stringify(chart.data.datasets[0].data) !== '[50]') process.exit(5);
if (JSON.stringify(chart.data.datasets[1].data) !== '[100]') process.exit(6);
if (JSON.stringify(chart.data.datasets[2].data) !== '[50]') process.exit(7);
if (!elements.get('history-title').textContent.startsWith(selected.suite)) process.exit(8);
if (elements.get('dataset-select').innerHTML.includes('<img')) process.exit(9);
const latency = measurementCharts.find(item => item.id === 'latency').chart;
if (Math.abs(latency.data.datasets[0].data[0] - 14.2) > 1e-9 ||
    latency.data.datasets[1].data[0] !== 31.5) process.exit(12);
if (!elements.get('measurement-summary-latency').innerHTML.includes('3 / 4')) process.exit(13);
const reliability = measurementCharts.find(item => item.id === 'reliability').chart;
if (reliability.data.datasets[0].data[0] !== 0) process.exit(14);
if (reliability.data.datasets.length !== 1) process.exit(15);
if (document.getElementById('measurement-panel-cost').hidden || elements.get('measurement-empty-cost').hidden) process.exit(16);
if (document.getElementById('measurement-panel-reliability').hidden) process.exit(21);
if (reliability.data.datasets[0].label !== 'Tool errors') process.exit(22);
if (!elements.get('measurement-summary-reliability').innerHTML.includes('Tool errors') ||
    elements.get('measurement-summary-reliability').innerHTML.includes('Retries')) process.exit(23);
const chartMetrics = DATA.metricCharts.flatMap(spec => spec.series.map(series => series.metric));
if (['retry_count', 'replan_count', 'time_to_first_token_ms', 'vision_llm_time_ms'].some(metric => chartMetrics.includes(metric))) process.exit(47);
if (!['llm_time_ms', 'cost_usd', 'screenshot_capture_ms'].every(metric => chartMetrics.includes(metric))) process.exit(48);
if (!elements.get('latest').innerHTML.includes('Latest commit')) process.exit(27);
const overviewHTML = elements.get('suites').innerHTML;
if (!elements.get('latest-run-note').innerHTML.includes('https://github.com/owner/repo/commit/aaaaaaaa1111') ||
    overviewHTML.includes('/commit/')) process.exit(36);
if (!overviewHTML.includes('Tool calls · mean') || !overviewHTML.includes('LLM calls · mean') ||
    !overviewHTML.includes('Task time · p50') || !overviewHTML.includes('4.5 calls') ||
    !overviewHTML.includes('0.0 calls') || !overviewHTML.includes('14.2 s') ||
    !overviewHTML.includes('n=3 / 4') || !overviewHTML.includes('Unavailable')) process.exit(17);
if (!overviewMeasure(selected, 'retry_count', 'mean', 1, 'calls').includes('Unavailable')) process.exit(18);
if (!elements.get('availability').innerHTML.includes('1 data warnings') ||
    !elements.get('availability').innerHTML.includes('&lt;script&gt;')) process.exit(30);
selectDataset(datasets.findIndex(s => s.suite === 'other'));
if (latency.data.datasets[0].data.some(v => v !== null)) process.exit(19);
if (elements.get('suites').innerHTML !== overviewHTML) process.exit(20);
if (document.getElementById('measurement-panel-latency').hidden || elements.get('measurement-empty-latency').hidden) process.exit(24);
if (!elements.get('measurement-note').textContent.startsWith('0 / 6')) process.exit(25);
selectDataset(datasets.findIndex(s => s.suite !== 'other'));
if (!elements.get('measurement-empty-latency').hidden || latency.data.datasets[0].label !== 'p50') process.exit(26);
const costSpec = DATA.metricCharts.find(spec => spec.id === 'cost');
if (!measurementText(0.00000012, costSpec).includes('0.00000012')) process.exit(28);
if (measurementText(0, costSpec) !== '0 USD') process.exit(29);
const detailHTML = elements.get('failure-details').innerHTML;
if (detailHTML.includes('/commit/') || detailHTML.includes('<code>')) process.exit(37);
if (detailHTML.includes('<img') || detailHTML.includes('<script>') || detailHTML.includes('href="javascript:')) process.exit(32);
if (!detailHTML.includes('&lt;script&gt;') || !detailHTML.includes('Attempt 2')) process.exit(33);
if (!detailHTML.includes('Failure details unavailable') || !detailHTML.includes('No failed or excluded attempts')) process.exit(34);
if (detailHTML.includes('Failure details unavailable · Details unavailable')) process.exit(38);
if (!safeLink('https://langfuse.test/traces/a?x=1&y=2', 'Open trace').includes('&amp;')) process.exit(35);
const namesForFilter = filter => matchingDatasetIndices(filter).map(i => datasets[i].suite);
if (namesForFilter('all').length !== datasets.length) process.exit(39);
if (!el('measurement-wrap-cost').hidden || el('measurement-panel-cost').hidden) process.exit(45);
if (el('measurement-wrap-latency').hidden) process.exit(46);
const assert = require('node:assert/strict');
assert(!overviewHTML.includes('Config changed') && !overviewHTML.includes('Config unverified'));
assert(!elements.has('configuration-context'));
assert(!elements.has('comparison-note'));
const memoryComparison = datasets.find(s => s.suite === 'memory_v1');
assert(comparisonExplanation(memoryComparison).includes('Test definition file changed'));
assert(!comparisonExplanation(memoryComparison).includes('Branch:'));
const repeated = structuredClone(memoryComparison);
repeated.changed_fields = ['metrics_k', 'workload_sha256'];
Object.assign(repeated.previous, {workload_task_ids: '["a","b"]', tasks: 2, metrics_k: 1, total_attempts: 2});
Object.assign(repeated.latest, {workload_task_ids: ['b','a'], tasks: 2, metrics_k: 3, total_attempts: 6});
assert.equal(comparisonExplanation(repeated), 'Attempts per task: 1 → 3.');
repeated.latest.workload_task_ids = ['a','c'];
assert(comparisonExplanation(repeated).includes('Task selection or attempt coverage changed'));
repeated.latest.workload_task_ids = ['a','b'];
repeated.latest.total_attempts = 5;
assert(comparisonExplanation(repeated).includes('attempt coverage changed'));
repeated.latest.workload_task_ids = null;
assert(comparisonExplanation(repeated).includes('attempt coverage changed'));
const branchOnly = {...memoryComparison, changed_fields: ['git_branch']};
assert.equal(comparisonExplanation(branchOnly), 'No evaluation setting changes recorded.');
assert.deepEqual(DATA.capabilities, ['Memory', 'Shell utilities', 'Diagnostics', 'Other']);
assert(!overviewHTML.includes('skill_discovery_v1'));
assert(el('diagnostic-results').innerHTML.includes('Skill read: 25.0%'));
assert(datasets.find(s => s.suite === 'skill_discovery_v1').latest.environment_label === 'External device');
assert.deepEqual(namesForFilter('Memory').sort(), ['episode_memory_v1', 'memory_v1']);
assert.deepEqual(namesForFilter('Shell utilities'), ['shell_utility_v1']);
assert(namesForFilter('Other').includes('other'));
assert.equal(el('change-summary').innerHTML, el('trend-capabilities').innerHTML);
const rows = datasets.flatMap((s, i) => [
  {dataset: {filterDataset: String(i)}}, {dataset: {filterDataset: String(i)}},
]);
const buttons = capabilityControls.flatMap(() => datasetFilters.map(f => ({
  dataset: {filter: f.id}, setAttribute(name, value) { this[name] = value; },
})));
document.querySelectorAll = selector => selector === '[data-filter-dataset]' ? rows
  : selector === 'button[data-filter]' ? buttons : [];
const clickFilter = (control, capability) => el(control).listeners.click({
  target: {closest: () => ({dataset: {filter: capability}})},
});
clickFilter('change-summary', 'Memory');
assert.equal(selected.capability, 'Memory');
assert.equal((el('dataset-select').innerHTML.match(/<option /g) || []).length, 2);
assert(!el('dataset-select').innerHTML.includes('shell_utility_v1'));
assert(rows.every(row => row.hidden === (datasets[Number(row.dataset.filterDataset)].capability !== 'Memory')));
assert(buttons.every(button => button['aria-pressed'] === String(button.dataset.filter === 'Memory')));
const memoryIndex = datasets.findIndex(s => s.suite === 'memory_v1');
openDataset(memoryIndex);
assert(el('comparison-details').textContent.includes('Judge prompt: v2 → v3'));
assert(!el('run-details').innerHTML.includes('memory-v2'));
assert(el('technical-details').innerHTML.includes('memory-v2'));
assert(el('run-details').innerHTML.includes('Test definition'));
assert(el('run-details').innerHTML.includes('&lt;img src=x&gt;'));
assert(!el('run-details').innerHTML.includes('<img'));
assert(el('run-details').innerHTML.includes('Attempts per task'));
assert(!el('run-details').innerHTML.includes('Judge provider'));
assert(sourceDiffLink({...memoryComparison, latest: {...memoryComparison.latest, commit_sha: 'bbbb2222'}}).includes('/compare/aaaaaaaa1111..bbbb2222'));
assert.equal(sourceDiffLink(memoryComparison), '');
assert.equal(activeFilter, 'Memory');
assert.equal(el('view-trends').hidden, false);
openFailures(memoryIndex);
assert.equal(activeFilter, 'Memory');
assert.equal(el('view-latest').hidden, false);
assert.equal(el('failure-dataset-' + memoryIndex).open, true);
clickFilter('trend-capabilities', 'all');
assert.equal(selected.suite, 'memory_v1');
assert(rows.every(row => !row.hidden));
clickFilter('trend-capabilities', 'Shell utilities');
assert.equal(selected.suite, 'shell_utility_v1');
assert(el('dataset-context').textContent.includes('Isolated'));
assert.equal(el('dataset-select').value, String(datasets.indexOf(selected)));
assert.equal((el('dataset-select').innerHTML.match(/<option /g) || []).length, 1);
clickFilter('change-summary', 'Other');
assert.equal(selected.suite, 'other');
assert(!el('dataset-select').innerHTML.includes('<img'));
assert(el('dataset-select').innerHTML.includes('&lt;img'));
assert.equal(el('suites').innerHTML, overviewHTML);
clickFilter('change-summary', 'Diagnostics');
assert.equal(selected.suite, 'skill_discovery_v1');
assert(el('dataset-context').textContent.includes('not skill discovery'));
assert(el('latest').innerHTML.includes('Skill read observation'));
assert.equal(skillReadRate({skill_discovery:{read:0,observed:4}}),0);
assert.equal(skillReadRate({skill_discovery:{read:2,observed:1}}),null);
assert.equal(skillReadRate({}),null);
"""
    subprocess.run(["node", "-e", prefix + script + verification], check=True)
