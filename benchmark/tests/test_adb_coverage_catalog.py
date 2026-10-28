"""Native Android coverage must not silently run incompatible suite tasks."""

import json
import subprocess
from pathlib import Path

import pytest

from ci import run_case as run_case_module
from ci.plan import BENCHMARK_ROOT, CatalogError, load_catalog, select_cases
from ci.run_cases import _cases_from_matrix
from runner.suite import load_suite


def test_adb_profile_selects_only_serial_native_android_cases():
    catalog = load_catalog()
    cases = select_cases(catalog, profile="adb")
    assert len(cases) == 5
    assert all(case.environment == "adb" and case.target_platform == "android"
               and case.max_concurrency == 1 for case in cases)
    comparisons = [case for case in cases if case.runnable]
    assert len(comparisons) == 4
    assert {case.id for case in comparisons} <= {
        case.id for case in select_cases(catalog, profile="runnable")
    }

    total = 0
    for case in comparisons:
        suite = load_suite(BENCHMARK_ROOT / case.suite)
        selected = [task for task in suite.tasks if not case.task_ids or task.id in case.task_ids]
        total += len(selected)
        for task in selected:
            assert not task.platforms or "android" in task.platforms
            assert not task.app_ids and task.foreground_app_id is None
            assert not task.environment_assertions  # ADB has no MobileGym state API.
            assert task.mock_environment is None and suite.mock_environment is None
            assert task.input_screenshot is None  # Observe the real device.
    assert total == 40


def test_adb_profile_rejects_non_adb_suite():
    with pytest.raises(CatalogError, match="outside the adb profile"):
        select_cases(load_catalog(), profile="adb", suite="suites/mobilegym_basic.json")


@pytest.mark.parametrize("name", ["adb_android_basic", "adb_android_text_editing"])
def test_new_android_tasks_use_outcome_judging_without_required_routes(name):
    suite = load_suite(BENCHMARK_ROOT / "suites" / f"{name}.json")
    navigation_ids = {
        "clock_from_settings", "notification_round_trip", "about_device_and_back",
        "settings_scroll_round_trip", "reopen_settings_context", "return_to_settings_via_recents",
    }
    tasks = [task for task in suite.tasks
             if name == "adb_android_text_editing" or task.id in navigation_ids]
    assert tasks
    for task in tasks:
        assert task.rubric
        assert task.hard_assertions.min_tool_calls == 1
        assert not task.hard_assertions.required_tools
        assert not task.hard_assertions.required_tool_calls
        assert task.platforms == ["android"]


@pytest.mark.parametrize("task_ids", ["open", [""], [None], [" open"], ["open", "open"], ["unknown"], ["ios-only"]])
def test_catalog_rejects_invalid_task_selection(tmp_path: Path, task_ids):
    suites = tmp_path / "suites"
    suites.mkdir()
    (suites / "native.json").write_text(json.dumps({"tasks": [
        {"id": "open", "platforms": ["android"]},
        {"id": "ios-only", "platforms": ["ios"]},
    ]}))
    catalog = tmp_path / "catalog.json"
    catalog.write_text(json.dumps({"version": 1, "cases": [{
        "id": "native", "suite": "suites/native.json", "environment": "adb",
        "target_platform": "android", "task_ids": task_ids,
    }]}))
    with pytest.raises(CatalogError):
        load_catalog(catalog_path=catalog, suites_dir=suites)


def test_catalog_rejects_parallel_tasks_on_one_adb_device(tmp_path: Path):
    catalog = tmp_path / "catalog.json"
    catalog.write_text(json.dumps({"version": 1, "cases": [{
        "id": "native", "suite": "suites/native.json", "environment": "adb",
        "target_platform": "android", "max_concurrency": 2,
    }]}))
    with pytest.raises(CatalogError, match="max_concurrency=1"):
        load_catalog(catalog_path=catalog, suites_dir=tmp_path)


@pytest.mark.parametrize("case_id", ["adb_android_basic", "adb_android_text_editing"])
def test_task_selection_survives_matrix_and_reaches_runner(monkeypatch, tmp_path, case_id):
    original = next(case for case in load_catalog().cases if case.id == case_id)
    matrix = json.dumps({"include": [original.matrix_entry()]})
    case, = _cases_from_matrix(matrix)
    assert case == original
    commands = []

    def fake_run(command, **kwargs):
        commands.append(command)
        return subprocess.CompletedProcess(command, 0)

    monkeypatch.setattr(run_case_module, "BENCHMARK_ROOT", tmp_path)
    monkeypatch.setattr(run_case_module, "_environment_url", lambda *args, **kwargs: ("http://adb.test:8899", None))
    monkeypatch.setattr(run_case_module.subprocess, "run", fake_run)
    run_case_module.run_case(case, run_id="selection-test", environment={})
    command, = commands
    assert command[command.index("--target-platform") + 1] == "android"
    assert command[command.index("--max-concurrency") + 1] == "1"
    selected = [command[index + 1] for index, item in enumerate(command) if item == "--task-id"]
    assert selected == original.task_ids
