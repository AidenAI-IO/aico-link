import json
import re
from pathlib import Path

import pytest

from ci import run_case as run_case_module
from ci.plan import CatalogError, load_catalog, select_cases
from ci.run_case import (
    _effective_exit_code,
    _incomplete_run_reasons,
    _environment_url,
    _images_prepared,
    _runtime_environment,
)


def _write_generated_reports(run_dir: Path) -> None:
    (run_dir / "metrics.json").write_text("{}", encoding="utf-8")
    (run_dir / "summary.md").write_text("# Summary\n", encoding="utf-8")
    (run_dir / "report.html").write_text("<html></html>", encoding="utf-8")


def _workflow_job(name: str) -> str:
    workflow = (
        Path(__file__).resolve().parents[2] / ".github" / "workflows" / "benchmark.yml"
    ).read_text(encoding="utf-8")
    match = re.search(
        rf"(?ms)^  {re.escape(name)}:\n(.*?)(?=^  [\w-]+:\n|\Z)", workflow
    )
    assert match is not None, f"missing workflow job: {name}"
    return match.group(1)


def _workflow_step(job: str, marker: str) -> str:
    match = re.search(
        rf"(?ms)^      - {re.escape(marker)}\n(.*?)(?=^      - |\Z)", job
    )
    assert match is not None, f"missing workflow step: {marker}"
    return match.group(1)


def test_catalog_rejects_an_unclassified_suite(tmp_path: Path) -> None:
    suites_dir = tmp_path / "suites"
    suites_dir.mkdir()
    (suites_dir / "known.json").write_text('{"name":"known","tasks":[]}', encoding="utf-8")
    (suites_dir / "new.json").write_text('{"name":"new","tasks":[]}', encoding="utf-8")
    catalog_path = tmp_path / "suites.json"
    catalog_path.write_text(
        '{"version":1,"cases":['
        '{"id":"known","suite":"suites/known.json",'
        '"environment":"isolated"}'
        '] }',
        encoding="utf-8",
    )

    with pytest.raises(CatalogError, match=r"unclassified suites: suites/new\.json"):
        load_catalog(catalog_path=catalog_path, suites_dir=suites_dir)


def test_runnable_profile_includes_adb_pairs_but_excludes_external_devices() -> None:
    catalog = load_catalog()

    runnable = select_cases(catalog, profile="runnable")
    hardware = select_cases(catalog, profile="hardware")
    all_cases = select_cases(catalog, profile="all")

    assert runnable
    assert hardware
    assert all_cases == catalog.cases
    assert len(runnable) == 18
    assert len(hardware) == 17
    assert len(all_cases) == 33
    assert {case.environment for case in runnable} <= {"isolated", "mobilegym", "adb"}
    assert {case.environment for case in hardware} <= {"adb", "external"}
    assert {case.id for case in runnable if case.environment == "adb"} == {
        "adb_android_basic", "adb_android_basic-aidenbridge",
        "adb_android_text_editing", "adb_android_text_editing-aidenbridge",
        "adb_android_builtin_apps",
    }
    assert len({case.id for case in all_cases}) == len(all_cases)
    assert {case.suite for case in runnable} | {case.suite for case in hardware} == (
        {case.suite for case in catalog.cases}
        - {"suites/mobilegym_scroll_sweep.json", "suites/tool_recovery_v1.json"}
    )


@pytest.mark.parametrize("case_id,suite", [
    ("mobilegym-scroll-sweep", "suites/mobilegym_scroll_sweep.json"),
    ("tool-recovery", "suites/tool_recovery_v1.json"),
])
def test_manual_suite_stays_out_of_runnable_but_is_explicitly_selectable(case_id, suite) -> None:
    catalog = load_catalog()

    runnable = select_cases(catalog, profile="runnable")
    assert case_id not in {case.id for case in runnable}

    selected = select_cases(
        catalog,
        profile="runnable",
        suite=suite,
    )
    assert [case.id for case in selected] == [case_id]
    assert selected[0].runnable is False


def test_selecting_a_suite_preserves_platform_specific_cases() -> None:
    catalog = load_catalog()

    selected = select_cases(
        catalog,
        profile="all",
        suite="suites/aico_app/connection_capabilities_v1.json",
    )

    assert {case.target_platform for case in selected} == {"ios", "android"}


def test_comma_separated_suites_select_each_suite_in_request_order() -> None:
    catalog = load_catalog()
    first = "suites/quick_capture_v1.json"
    second = "suites/adb_android_basic.json"

    selected = select_cases(
        catalog,
        profile="runnable",
        suite=f" {first} , {second},{first},",
    )

    expected = select_cases(catalog, profile="runnable", suite=first) + select_cases(
        catalog, profile="runnable", suite=second
    )
    assert selected == expected
    with pytest.raises(CatalogError, match="not in the CI catalog"):
        select_cases(catalog, profile="runnable", suite=f"{first},suites/missing.json")


def test_phone_control_selection_uses_only_the_requested_environment_profile() -> None:
    catalog = load_catalog()
    suite = "suites/phone_control_v1.json"

    mobilegym = select_cases(catalog, profile="runnable", suite=suite)
    hardware = select_cases(catalog, profile="hardware", suite=suite)
    both = select_cases(catalog, profile="all", suite=suite)

    assert [case.id for case in mobilegym] == ["phone-control-mobilegym"]
    assert mobilegym[0].environment == "mobilegym"
    assert mobilegym[0].target_platform == "android"
    assert mobilegym[0].runnable is False
    assert mobilegym[0] not in select_cases(catalog, profile="runnable")
    assert {case.id for case in hardware} == {"phone-control-ios"}
    with pytest.raises(CatalogError, match="outside the adb profile"):
        select_cases(catalog, profile="adb", suite=suite)
    assert {case.id for case in both} == {
        "phone-control-mobilegym", "phone-control-ios"
    }


@pytest.mark.parametrize("suite", ["adb_android_basic", "adb_android_text_editing"])
@pytest.mark.parametrize("profile", ["runnable", "hardware", "adb", "all"])
def test_adb_suites_select_matching_bridge_and_no_bridge_cases(suite, profile):
    catalog = load_catalog()
    selected = select_cases(catalog, profile=profile, suite=f"suites/{suite}.json")

    assert len(selected) == 2
    baseline, connected = selected
    assert baseline.android_phone_bridge is False
    assert connected.android_phone_bridge is True
    assert baseline.suite == connected.suite
    assert baseline.environment == connected.environment == "adb"
    assert baseline.target_platform == connected.target_platform == "android"
    assert baseline.max_concurrency == connected.max_concurrency == 1
    assert baseline.timeout_minutes == connected.timeout_minutes


@pytest.mark.parametrize("bridge_enabled", [False, True])
@pytest.mark.parametrize("app_installed", [False, True])
def test_adb_baseline_stops_retained_companion_app(monkeypatch, bridge_enabled, app_installed):
    case = next(case for case in load_catalog().cases
                if case.suite == "suites/adb_android_basic.json"
                and case.android_phone_bridge == bridge_enabled)
    commands = []
    def run(command, **kwargs):
        commands.append((command, kwargs))
        return run_case_module.subprocess.CompletedProcess(
            command, 0, stdout="package:com.aidenbridge\n" if app_installed else "",
        )
    monkeypatch.setattr(run_case_module.subprocess, "run", run)
    monkeypatch.setattr(run_case_module, "_run_json", lambda *args, **kwargs: {
        "environment_url": "http://bridge.test",
    })

    url, _ = _environment_url(case, run_id="paired", environment={"ANDROID_SERIAL": "emulator-5554"})

    assert url == "http://bridge.test"
    if bridge_enabled:
        assert commands == []
    elif app_installed:
        assert [command[0][-3:] for command in commands] == [
            ["list", "packages", "com.aidenbridge"],
            ["am", "force-stop", "com.aidenbridge"],
        ]
    else:
        assert len(commands) == 1
        assert commands[0][0][-3:] == ["list", "packages", "com.aidenbridge"]
        assert commands[0][1]["check"] is True


def test_aidenbridge_capabilities_selects_rooted_android_routing() -> None:
    catalog = load_catalog()
    selected = select_cases(
        catalog,
        profile="hardware",
        suite="suites/aico_app/android_bridge_capabilities_v1.json",
    )

    assert len(selected) == 1
    assert selected[0].android_phone_bridge is True
    assert selected[0].environment == "adb"
    assert selected[0].target_platform == "android"
    assert selected[0].max_concurrency == 1
    assert selected[0] not in select_cases(catalog, profile="runnable")
    assert not any(case.id in {"aidenbridge-data-android", "aidenbridge-input-android"} for case in catalog.cases)


def test_suite_selection_must_match_profile_unless_all() -> None:
    catalog = load_catalog()

    with pytest.raises(CatalogError, match="outside the runnable profile"):
        select_cases(
            catalog,
            profile="runnable",
            suite="suites/vphone_ios_basic.json",
        )


def test_external_case_reads_its_named_environment_variable() -> None:
    case = next(
        item
        for item in load_catalog().cases
        if item.id == "aico-app-ios"
    )

    url, service = _environment_url(
        case,
        run_id="test-run",
        environment={case.environment_variable: "http://bridge.test:9000"},
    )

    assert url == "http://bridge.test:9000"
    assert service is None


def test_ci_maps_unprefixed_action_variables_to_benchmark_runtime() -> None:
    runtime = _runtime_environment(
        {
            "BENCHMARK_AGENT_MODEL": "agent-model",
            "BENCHMARK_JUDGE_API_KEY": "judge-key",
            "DAEMON_IMAGE": "agent-daemon:test",
            "LANGFUSE_PUBLIC_KEY": "langfuse-key",
            "BENCHMARK_TELEMETRY": "1",
            "LANGFUSE_HTTPS_PROXY": "http://proxy.test:8080",
        }
    )

    assert runtime["AICO_BENCHMARK_AGENT_MODEL"] == "agent-model"
    assert runtime["AICO_BENCHMARK_JUDGE_API_KEY"] == "judge-key"
    assert runtime["AICO_DAEMON_IMAGE"] == "agent-daemon:test"
    assert runtime["LANGFUSE_PUBLIC_KEY"] == "langfuse-key"
    assert runtime["AICO_BENCHMARK_TELEMETRY"] == "1"
    assert runtime["AICO_LANGFUSE_HTTPS_PROXY"] == "http://proxy.test:8080"
    assert "AICO_BENCHMARK_TELEMETRY" not in _runtime_environment({})
    assert "AICO_LANGFUSE_HTTPS_PROXY" not in _runtime_environment({})


def test_ci_uses_prepared_images_only_when_explicitly_enabled() -> None:
    assert _images_prepared({"BENCHMARK_CI_IMAGES_PREPARED": "1"}) is True
    assert _images_prepared({}) is False


def test_mobilegym_case_reuses_prepared_image(monkeypatch) -> None:
    case = next(
        item for item in load_catalog().cases if item.environment == "mobilegym"
    )
    captured = {}

    def fake_run_json(command, *, environment, log_file=None, timeout_seconds=None):
        captured["command"] = command
        captured["environment"] = environment
        return {"environment_url": "http://127.0.0.1:19090"}

    monkeypatch.setattr(run_case_module, "_run_json", fake_run_json)

    _environment_url(
        case,
        run_id="ci-test",
        environment={"BENCHMARK_CI_IMAGES_PREPARED": "1"},
    )

    assert "--no-build-mobilegym-image" in captured["command"]


def test_benchmark_case_reuses_prepared_daemon_image(monkeypatch, tmp_path: Path) -> None:
    case = next(item for item in load_catalog().cases if item.environment == "isolated")
    captured = {}

    def fake_run(command, **kwargs):
        captured["command"] = command
        return run_case_module.subprocess.CompletedProcess(command, 0)

    monkeypatch.setattr(run_case_module.subprocess, "run", fake_run)
    monkeypatch.setattr(run_case_module, "BENCHMARK_ROOT", tmp_path)

    run_case_module.run_case(
        case,
        run_id="ci-test",
        environment={"BENCHMARK_CI_IMAGES_PREPARED": "1"},
    )

    assert "--no-build-daemon-image" in captured["command"]


def test_ci_allows_a_complete_run_where_every_task_was_skipped(tmp_path: Path) -> None:
    manifest = tmp_path / "manifest.json"
    manifest.write_text(
        '{"totals":{"tasks":3,"passed":0,"failed":0,"skipped":3,'
        '"judge_error":0,"timeout":0}}',
        encoding="utf-8",
    )
    (tmp_path / "results.jsonl").write_text(
        "".join(
            '{"task_id":"task-%s","attempt":1,"status":"skipped","metrics":{"error":"setup failed"}}\n'
            % i
            for i in range(3)
        ),
        encoding="utf-8",
    )
    _write_generated_reports(tmp_path)

    assert _effective_exit_code(0, manifest) == 0


def test_ci_allows_a_complete_run_with_an_infrastructure_skip(tmp_path: Path) -> None:
    manifest = tmp_path / "manifest.json"
    manifest.write_text(
        '{"totals":{"tasks":2,"passed":1,"failed":0,"skipped":1,'
        '"judge_error":0,"timeout":0}}',
        encoding="utf-8",
    )
    (tmp_path / "results.jsonl").write_text(
        '{"task_id":"passed","attempt":1,"status":"passed","metrics":{}}\n'
        '{"task_id":"skipped","attempt":1,"status":"skipped","metrics":{"error":"agent not ready"}}\n',
        encoding="utf-8",
    )
    _write_generated_reports(tmp_path)

    assert _effective_exit_code(0, manifest) == 0


def test_ci_allows_platform_ineligible_skips(tmp_path: Path) -> None:
    manifest = tmp_path / "manifest.json"
    manifest.write_text(
        '{"totals":{"tasks":2,"passed":1,"failed":0,"skipped":1,'
        '"judge_error":0,"timeout":0}}',
        encoding="utf-8",
    )
    (tmp_path / "results.jsonl").write_text(
        '{"task_id":"passed","attempt":1,"status":"passed","metrics":{}}\n'
        '{"task_id":"skipped","attempt":1,"status":"skipped","metrics":{"error":"task platforms ios do not include target platform android"}}\n',
        encoding="utf-8",
    )
    _write_generated_reports(tmp_path)

    assert _effective_exit_code(0, manifest) == 0


def test_ci_allows_completed_run_with_benchmark_failures(tmp_path: Path) -> None:
    manifest = tmp_path / "manifest.json"
    manifest.write_text(
        '{"totals":{"tasks":2,"passed":1,"failed":1,"skipped":0,'
        '"judge_error":0,"timeout":0}}',
        encoding="utf-8",
    )
    (tmp_path / "results.jsonl").write_text(
        '{"task_id":"passed","attempt":1,"status":"passed","metrics":{}}\n'
        '{"task_id":"failed","attempt":1,"status":"failed","metrics":{}}\n',
        encoding="utf-8",
    )
    _write_generated_reports(tmp_path)

    assert _effective_exit_code(0, manifest) == 0


@pytest.mark.parametrize("case_id", ["adb_android_basic", "adb_android_basic-aidenbridge", "aidenbridge-capabilities-android"])
@pytest.mark.parametrize("complete", [True, False])
def test_android_cases_use_run_completeness_not_task_pass_rate(
    monkeypatch, tmp_path: Path, case_id: str, complete: bool,
) -> None:
    case = next(item for item in load_catalog().cases if item.id == case_id)
    run_dir = tmp_path / "runs" / "ci-test"
    run_dir.mkdir(parents=True)
    (run_dir / "manifest.json").write_text(
        '{"totals":{"tasks":2,"passed":1,"failed":1,"skipped":0}}',
        encoding="utf-8",
    )
    (run_dir / "results.jsonl").write_text(
        '{"task_id":"passed","attempt":1,"status":"passed"}\n'
        + ('{"task_id":"failed","attempt":1,"status":"failed"}\n' if complete else ''),
        encoding="utf-8",
    )
    _write_generated_reports(run_dir)
    monkeypatch.setattr(run_case_module, "BENCHMARK_ROOT", tmp_path)
    monkeypatch.setattr(run_case_module, "_environment_url", lambda *args, **kwargs: ("http://bridge.test", None))
    monkeypatch.setattr(run_case_module, "_stop_daemon_projects_by_run_id", lambda run_id: None)
    monkeypatch.setattr(run_case_module, "_stop_service", lambda service: None)
    commands = []
    def run(command, **kwargs):
        commands.append(command)
        return run_case_module.subprocess.CompletedProcess(command, 0)
    monkeypatch.setattr(run_case_module.subprocess, "run", run)

    assert run_case_module.run_case(
        case, run_id="ci-test", environment={"ANDROID_SERIAL": "emulator-5554"},
    ) == (0 if complete else 1)
    assert ("--android-phone-bridge-serial" in commands[0]) == case.android_phone_bridge


@pytest.mark.parametrize(
    "metrics",
    [
        {"agent_error": "agent request failed", "failure_class": "unknown"},
        {"error": "setup failed", "failure_class": "environment"},
    ],
)
def test_ci_allows_complete_run_with_task_execution_error(
    tmp_path: Path,
    metrics: dict[str, str],
) -> None:
    manifest = tmp_path / "manifest.json"
    manifest.write_text(
        '{"totals":{"tasks":1,"passed":0,"failed":1,"skipped":0,'
        '"judge_error":0,"timeout":0}}',
        encoding="utf-8",
    )
    (tmp_path / "results.jsonl").write_text(
        json.dumps(
            {
                "task_id": "task-1",
                "attempt": 1,
                "status": "failed",
                "metrics": metrics,
            }
        )
        + "\n",
        encoding="utf-8",
    )
    _write_generated_reports(tmp_path)

    assert _effective_exit_code(0, manifest) == 0
    assert _incomplete_run_reasons(0, manifest) == []


def test_ci_rejects_results_that_do_not_match_manifest_totals(tmp_path: Path) -> None:
    manifest = tmp_path / "manifest.json"
    manifest.write_text(
        '{"totals":{"tasks":2,"passed":1,"failed":1,"skipped":0,'
        '"judge_error":0,"timeout":0}}',
        encoding="utf-8",
    )
    (tmp_path / "results.jsonl").write_text(
        '{"task_id":"task-1","attempt":1,"status":"passed","metrics":{}}\n',
        encoding="utf-8",
    )
    _write_generated_reports(tmp_path)

    assert _effective_exit_code(0, manifest) == 1
    assert _incomplete_run_reasons(0, manifest) == [
        "results=1/2",
        "results_totals_mismatch",
    ]


def test_ci_rejects_a_run_without_generated_reports(tmp_path: Path) -> None:
    manifest = tmp_path / "manifest.json"
    manifest.write_text(
        '{"totals":{"tasks":1,"passed":1,"failed":0,"skipped":0,'
        '"judge_error":0,"timeout":0}}',
        encoding="utf-8",
    )
    (tmp_path / "results.jsonl").write_text(
        '{"task_id":"task-1","attempt":1,"status":"passed","metrics":{}}\n',
        encoding="utf-8",
    )

    assert _effective_exit_code(0, manifest) == 1
    assert _incomplete_run_reasons(0, manifest) == [
        "metrics_missing",
        "summary_missing",
        "report_missing",
    ]


@pytest.mark.parametrize("error_status", ["judge_error", "timeout"])
def test_ci_allows_complete_run_with_judge_error_or_timeout(
    tmp_path: Path,
    error_status: str,
) -> None:
    manifest = tmp_path / "manifest.json"
    totals = {
        "tasks": 2,
        "passed": 1,
        "failed": 0,
        "skipped": 0,
        "judge_error": int(error_status == "judge_error"),
        "timeout": int(error_status == "timeout"),
    }
    manifest.write_text(json.dumps({"totals": totals}), encoding="utf-8")
    (tmp_path / "results.jsonl").write_text(
        '{"task_id":"passed","attempt":1,"status":"passed","metrics":{}}\n'
        + json.dumps(
            {
                "task_id": "error",
                "attempt": 1,
                "status": error_status,
                "metrics": {"error": "boom"},
            }
        )
        + "\n",
        encoding="utf-8",
    )
    _write_generated_reports(tmp_path)

    assert _effective_exit_code(1, manifest) == 0
    assert _incomplete_run_reasons(1, manifest) == []


def test_ci_rejects_anomalous_exit_after_complete_artifacts(tmp_path: Path) -> None:
    manifest = tmp_path / "manifest.json"
    manifest.write_text(
        '{"totals":{"tasks":1,"passed":1,"failed":0,"skipped":0,'
        '"judge_error":0,"timeout":0}}',
        encoding="utf-8",
    )
    (tmp_path / "results.jsonl").write_text(
        '{"task_id":"task-1","attempt":1,"status":"passed","metrics":{}}\n',
        encoding="utf-8",
    )
    _write_generated_reports(tmp_path)

    assert _effective_exit_code(1, manifest) == 1
    assert "runner_exit=1,expected=0" in _incomplete_run_reasons(1, manifest)


def test_ci_rejects_duplicate_result_identities(tmp_path: Path) -> None:
    manifest = tmp_path / "manifest.json"
    manifest.write_text(
        '{"totals":{"tasks":2,"passed":1,"failed":1,"skipped":0,'
        '"judge_error":0,"timeout":0}}',
        encoding="utf-8",
    )
    (tmp_path / "results.jsonl").write_text(
        '{"task_id":"task-1","attempt":1,"status":"passed","metrics":{}}\n'
        '{"task_id":"task-1","attempt":1,"status":"failed","metrics":{}}\n',
        encoding="utf-8",
    )
    _write_generated_reports(tmp_path)

    assert "duplicate_result_identity" in _incomplete_run_reasons(0, manifest)


def test_workflow_artifacts_do_not_include_materialized_worker_configs() -> None:
    run_job = _workflow_job("run")
    primary = _workflow_step(run_job, "name: Upload benchmark artifacts")
    retry = _workflow_step(run_job, "name: Retry benchmark artifact upload")
    artifact_paths = "\n".join(
        step.split("          path: |\n", 1)[1].split("          if-no-files-found:", 1)[0]
        for step in (primary, retry)
    )

    assert "cli-services" not in artifact_paths
    # Only diagnostic logs may leave worker directories: agent.toml and control
    # tokens contain materialized credentials and must never match an upload glob.
    worker_paths = [path for path in artifact_paths.splitlines() if "/workers/" in path]
    assert worker_paths
    assert all(path.endswith(("/workers/*/daemon.log", "/workers/*/runner.log")) for path in worker_paths)
    assert "auto-agent-setup.log" in artifact_paths
    task_paths = [path for path in artifact_paths.splitlines() if "/tasks/" in path]
    assert task_paths
    allowed_task_artifacts = (
        "/tasks/**/trace.json", "/tasks/**/history.json", "/tasks/**/episode.json",
        "/tasks/**/setup.json", "/tasks/**/setup_failure.json", "/tasks/**/consolidation.json",
    )
    assert all(path.endswith(allowed_task_artifacts) for path in task_paths)
    for artifact in allowed_task_artifacts:
        assert sum(path.endswith(artifact) for path in task_paths) == 2  # primary and retry upload
    assert "        id: upload_benchmark_artifacts" in primary.splitlines()
    assert "        if: ${{ always() && steps.upload_benchmark_artifacts.outcome == 'failure' }}" in retry.splitlines()
    assert "          overwrite: true" in retry.splitlines()


def test_workflow_links_exact_interactive_reports_from_static_host_or_artifact() -> None:
    step = _workflow_step(_workflow_job("run"), "name: Link benchmark reports")
    lines = step.splitlines()

    assert "          BENCHMARK_REPORT_PUBLISH_DIR: ${{ vars.BENCHMARK_REPORT_PUBLISH_DIR }}" in lines
    assert "          BENCHMARK_REPORT_BASE_URL: ${{ vars.BENCHMARK_REPORT_BASE_URL }}" in lines
    assert "          PRIMARY_ARTIFACT_URL: ${{ steps.upload_benchmark_artifacts.outputs.artifact-url }}" in lines
    assert "          RETRY_ARTIFACT_URL: ${{ steps.retry_benchmark_artifacts.outputs.artifact-url }}" in lines
    assert '          uv run python -m ci.publish_reports "${args[@]}"' in lines


def test_workflow_checks_docker_and_surfaces_setup_failures() -> None:
    workflow = (
        Path(__file__).resolve().parents[2] / ".github" / "workflows" / "benchmark.yml"
    ).read_text(encoding="utf-8")

    assert "docker version" in workflow
    assert "docker compose version" in workflow
    assert "docker info" in workflow
    assert "runnable) timeout_minutes=1440" in workflow
    assert "hardware) timeout_minutes=4320" in workflow
    assert "all) timeout_minutes=5760" in workflow
    assert "timeout-minutes: ${{ fromJSON(needs.plan.outputs.timeout_minutes) }}" in workflow
    assert "docker network ls" in workflow
    # The fan-out driver replaced the per-case setup-log step: it tails each
    # failing case's log into the job output so a failure is triageable without
    # downloading the artifact.
    assert "ci.run_cases" in workflow
    assert '--max-parallel "$BENCHMARK_CI_MAX_PARALLEL"' in workflow
    driver = (
        Path(__file__).resolve().parents[1] / "ci" / "run_cases.py"
    ).read_text(encoding="utf-8")
    assert "log tail" in driver
    assert "auto-agent-setup.log" in workflow


def test_workflow_schedules_all_runnable_cases_daily() -> None:
    workflow = (
        Path(__file__).resolve().parents[2] / ".github" / "workflows" / "benchmark.yml"
    ).read_text(encoding="utf-8")

    triggers = workflow.split("\njobs:\n", 1)[0]
    assert "  push:" not in triggers
    assert re.search(
        r"(?m)^on:\n  schedule:\n(?:    #.*\n)*    - cron: '7 18 \* \* \*'$",
        triggers,
    )
    self_hosted_guard = (
        "if: ${{ startsWith(github.ref, 'refs/heads/') "
        "&& github.event_name != 'pull_request' "
        "&& github.event_name != 'pull_request_target' }}"
    )
    run_job = _workflow_job("run")
    assert f"    {self_hosted_guard}" in run_job.splitlines()
    assert "github.ref == 'refs/heads/main'" not in run_job
    profile_step = _workflow_step(_workflow_job("plan"), "id: profile")
    assert 'elif [[ "$EVENT_NAME" == "schedule" ]]; then\n            profile="runnable"' in profile_step


def test_workflow_maps_unprefixed_github_configuration_to_runner_environment() -> None:
    workflow = (
        Path(__file__).resolve().parents[2] / ".github" / "workflows" / "benchmark.yml"
    ).read_text(encoding="utf-8")

    assert re.search(r"(?m)^\s+AICO_[A-Z0-9_]+:", workflow) is None
    assert "BENCHMARK_TELEMETRY: '1'" in workflow
    expected_mappings = {
        "BENCHMARK_AGENT_PROVIDER": "vars.BENCHMARK_AGENT_PROVIDER",
        "BENCHMARK_AGENT_MODEL": "vars.BENCHMARK_AGENT_MODEL",
        "BENCHMARK_AGENT_BASE_URL": "vars.BENCHMARK_AGENT_BASE_URL",
        "BENCHMARK_AGENT_API_KEY": "secrets.BENCHMARK_AGENT_API_KEY",
        "BENCHMARK_JUDGE_MODEL": "vars.BENCHMARK_JUDGE_MODEL",
        "BENCHMARK_JUDGE_BASE_URL": "vars.BENCHMARK_JUDGE_BASE_URL",
        "BENCHMARK_JUDGE_API_KEY": "secrets.BENCHMARK_JUDGE_API_KEY",
        "LANGFUSE_HTTPS_PROXY": "secrets.BENCHMARK_HTTPS_PROXY",
        "DAEMON_IMAGE": "vars.DAEMON_IMAGE",
        "BENCHMARK_PHONE_ENVIRONMENT_URL": "vars.BENCHMARK_PHONE_ENVIRONMENT_URL",
        "BENCHMARK_IOS_ENVIRONMENT_URL": "vars.BENCHMARK_IOS_ENVIRONMENT_URL",
        "BENCHMARK_MAC_ENVIRONMENT_URL": "vars.BENCHMARK_MAC_ENVIRONMENT_URL",
        "BENCHMARK_VPHONE_ENVIRONMENT_URL": "vars.BENCHMARK_VPHONE_ENVIRONMENT_URL",
        "BENCHMARK_AICO_APP_IOS_ENVIRONMENT_URL": (
            "vars.BENCHMARK_AICO_APP_IOS_ENVIRONMENT_URL"
        ),
        "BENCHMARK_AICO_APP_ANDROID_ENVIRONMENT_URL": (
            "vars.BENCHMARK_AICO_APP_ANDROID_ENVIRONMENT_URL"
        ),
    }
    for environment_name, github_expression in expected_mappings.items():
        assert f"{environment_name}: ${{{{ {github_expression}" in workflow


def test_workflow_uses_mirrored_container_sources() -> None:
    workflow = (
        Path(__file__).resolve().parents[2] / ".github" / "workflows" / "benchmark.yml"
    ).read_text(encoding="utf-8")
    compose = (
        Path(__file__).resolve().parents[1]
        / "docker"
        / "docker-compose.agent-daemon.yml"
    ).read_text(encoding="utf-8")

    assert (
        "DAEMON_DOCKERFILE: benchmark/docker/Dockerfile.agent-daemon.cn" in workflow
    )
    assert "MOBILEGYM_NODE_IMAGE: swr.cn-north-4.myhuaweicloud.com/" in workflow
    assert (
        "dockerfile: ${DAEMON_DOCKERFILE:-benchmark/docker/Dockerfile.agent-daemon}"
        in compose
    )
    assert "  prepare-images:" not in workflow
    assert "- prepare-images" not in workflow
    assert workflow.count("astral-sh/setup-uv@v6") == 1
    assert workflow.count("Install Python and benchmark dependencies") == 1
    assert workflow.count("Install Docker Compose V2") == 1
    assert workflow.count("Set up Docker Buildx") == 1
    assert workflow.count("Validate Docker runtime") == 1
    assert workflow.count("uv run python -m ci.prepare_images") == 1
    assert "BENCHMARK_CI_IMAGES_PREPARED: '1'" in workflow
