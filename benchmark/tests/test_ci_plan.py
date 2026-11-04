import json
import re
from pathlib import Path

import pytest

from ci import run_case as run_case_module
from ci.plan import Catalog, CatalogError, SuiteCase, load_catalog, select_cases
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


# Selection rules are tested on a synthetic catalog so adding, retiring, or
# reclassifying a real suite does not require editing these tests.
CATALOG = Catalog(cases=(
    SuiteCase(id="isolated", suite="suites/isolated.json", environment="isolated"),
    SuiteCase(id="manual", suite="suites/manual.json", environment="isolated", runnable=False),
    SuiteCase(id="adb", suite="suites/shared.json", environment="adb", target_platform="android"),
    SuiteCase(id="external", suite="suites/shared.json", environment="external",
              environment_variable="DEVICE_URL", target_platform="ios"),
    SuiteCase(id="device", suite="suites/device.json", environment="external",
              environment_variable="DEVICE_URL"),
))


def _ids(cases) -> list[str]:
    return [case.id for case in cases]


@pytest.mark.parametrize("profile,expected", [
    ("runnable", ["isolated", "adb"]),
    ("hardware", ["adb", "external", "device"]),
    ("adb", ["adb"]),
    ("all", ["isolated", "manual", "adb", "external", "device"]),
])
def test_profile_selects_its_environments_and_skips_manual_cases(profile, expected) -> None:
    assert _ids(select_cases(CATALOG, profile=profile)) == expected


@pytest.mark.parametrize("profile,suite,expected", [
    ("runnable", "suites/manual.json", ["manual"]),
    ("runnable", "suites/shared.json", ["adb"]),
    ("hardware", "suites/shared.json", ["adb", "external"]),
    ("all", "suites/shared.json", ["adb", "external"]),
])
def test_explicit_suite_selects_matching_cases_within_profile(profile, suite, expected) -> None:
    assert _ids(select_cases(CATALOG, profile=profile, suite=suite)) == expected


def test_explicit_suite_outside_profile_or_catalog_is_rejected() -> None:
    with pytest.raises(CatalogError, match="outside the runnable profile"):
        select_cases(CATALOG, profile="runnable", suite="suites/device.json")
    with pytest.raises(CatalogError, match="not in the CI catalog"):
        select_cases(CATALOG, profile="all", suite="suites/missing.json")


def test_comma_separated_suites_select_each_suite_in_request_order() -> None:
    selected = select_cases(
        CATALOG,
        profile="all",
        suite=" suites/shared.json , suites/isolated.json,suites/shared.json,",
    )

    assert _ids(selected) == ["adb", "external", "isolated"]
    with pytest.raises(CatalogError, match="not in the CI catalog"):
        select_cases(CATALOG, profile="all", suite="suites/isolated.json,suites/missing.json")
    for empty_list in (",", " , ,"):
        with pytest.raises(CatalogError, match="no suite paths"):
            select_cases(CATALOG, profile="all", suite=empty_list)
    assert select_cases(CATALOG, profile="runnable", suite="") == select_cases(
        CATALOG, profile="runnable"
    )


@pytest.mark.parametrize("bridge_enabled", [False, True])
@pytest.mark.parametrize("app_installed", [False, True])
def test_adb_baseline_stops_retained_companion_app(monkeypatch, bridge_enabled, app_installed):
    case = next(case for case in load_catalog().cases
                if case.suite == "suites/adb_android_text_editing.json"
                and case.android_phone_bridge == bridge_enabled)
    commands = []
    def run(command, **kwargs):
        commands.append((command, kwargs))
        return run_case_module.subprocess.CompletedProcess(
            command, 0, stdout="package:link.aico.app\n" if app_installed else "",
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
            ["list", "packages", "link.aico.app"],
            ["am", "force-stop", "link.aico.app"],
        ]
    else:
        assert len(commands) == 1
        assert commands[0][0][-3:] == ["list", "packages", "link.aico.app"]
        assert commands[0][1]["check"] is True


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


@pytest.mark.parametrize("statuses", [
    ["skipped", "skipped"],
    ["passed", "skipped"],
    ["passed", "failed"],
])
def test_ci_accepts_a_complete_run_regardless_of_task_outcomes(
    tmp_path: Path, statuses: list[str],
) -> None:
    totals = {"tasks": len(statuses), "judge_error": 0, "timeout": 0}
    for status in ("passed", "failed", "skipped"):
        totals[status] = statuses.count(status)
    manifest = tmp_path / "manifest.json"
    manifest.write_text(json.dumps({"totals": totals}), encoding="utf-8")
    (tmp_path / "results.jsonl").write_text(
        "".join(
            json.dumps({"task_id": f"task-{i}", "attempt": 1, "status": status, "metrics": {}}) + "\n"
            for i, status in enumerate(statuses)
        ),
        encoding="utf-8",
    )
    _write_generated_reports(tmp_path)

    assert _effective_exit_code(0, manifest) == 0


@pytest.mark.parametrize("case_id", ["adb_android_basic", "adb_android_text_editing-aidenbridge", "aidenbridge-capabilities-android"])
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
    artifact_paths = "\n".join(
        _workflow_step(run_job, marker).split("          path: |\n", 1)[1].split("          if-no-files-found:", 1)[0]
        for marker in ("name: Upload benchmark artifacts", "name: Retry benchmark artifact upload")
    ).splitlines()

    # agent.toml, control tokens, and CLI service configs contain materialized
    # credentials and must never match an upload glob. Every run path must name
    # a concrete file, so a broad pattern such as runs/** cannot slip in.
    run_paths = [path for path in artifact_paths if "/runs/" in path]
    assert run_paths
    assert all("*" not in path.rsplit("/", 1)[1] for path in run_paths)
    assert not any(
        secret in path for path in artifact_paths
        for secret in ("cli-services", "agent.toml", "control_token")
    )
    assert all(
        path.endswith(("/workers/*/daemon.log", "/workers/*/runner.log"))
        for path in artifact_paths if "/workers/" in path
    )
    allowed_task_artifacts = (
        "/tasks/**/trace.json", "/tasks/**/history.json", "/tasks/**/episode.json",
        "/tasks/**/setup.json", "/tasks/**/setup_failure.json", "/tasks/**/consolidation.json",
    )
    assert all(path.endswith(allowed_task_artifacts) for path in artifact_paths if "/tasks/" in path)


def test_self_hosted_benchmark_job_never_runs_pull_request_code() -> None:
    run_job = _workflow_job("run")

    assert (
        "    if: ${{ startsWith(github.ref, 'refs/heads/') "
        "&& github.event_name != 'pull_request' "
        "&& github.event_name != 'pull_request_target' }}"
    ) in run_job.splitlines()
