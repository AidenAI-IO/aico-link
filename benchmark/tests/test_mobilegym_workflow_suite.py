"""Check that workflow assertions reject plausible but incorrect outcomes."""
from copy import deepcopy
from pathlib import Path

import pytest

from ci.plan import load_catalog, select_cases
from ci.weekly_report import capability_of
from runner.assertions import evaluate_environment_state_assertions
from runner.recovery import prepare_task_isolation
from runner.reset import ResetError
from runner.suite import load_suite


SUITES = Path(__file__).resolve().parents[1] / "suites"
WORKFLOWS = load_suite(SUITES / "mobilegym_workflow_v1.json")
PHONE = load_suite(SUITES / "phone_control_v1.json")
NEW_TASKS = WORKFLOWS.tasks + [
    task for task in PHONE.tasks
    if task.id in {"notes_fill_title_and_body", "clock_edit_alarm_label"}
]


def checks_pass(task, state):
    return all(check.passed for check in evaluate_environment_state_assertions(
        state, task.environment_assertions
    ))


@pytest.mark.parametrize("task", [task for task in NEW_TASKS if task.id != "contact_discard_draft"],
                         ids=lambda task: task.id)
def test_mutation_tasks_cannot_pass_without_changing_seeded_state(task):
    assert not checks_pass(task, task.initial_environment_state)


def test_seeded_tasks_fail_before_agent_actions_without_environment():
    with pytest.raises(ResetError, match="requires a live environment_url"):
        prepare_task_isolation(object(), WORKFLOWS, WORKFLOWS.tasks[0])


def test_cross_app_contact_checks_destination_and_preserves_source():
    task = next(task for task in WORKFLOWS.tasks if task.id == "note_to_contact")
    state = deepcopy(task.initial_environment_state)
    state["os"]["providers"]["contacts"]["contacts"].append({
        "id": "generated-contact-id",
        "displayName": "Avery Chen", "company": "Harbor Supply", "title": "Account Manager",
        "phones": [{"id": "p1", "label": "手机", "number": "13700001003", "isPrimary": True}],
        "emails": [{"id": "e1", "label": "工作", "email": "avery.chen@example.com", "isPrimary": True}],
        "starred": False,
    })
    assert checks_pass(task, state)
    state["apps"]["notes"]["notes"][0]["content"] = ""
    assert not checks_pass(task, state)


def test_alarm_edit_rejects_recreation_and_modifying_same_time_decoy():
    task = next(task for task in WORKFLOWS.tasks if task.id == "alarm_edit_matching_record")
    state = deepcopy(task.initial_environment_state)
    alarms = state["apps"]["clock"]["alarms"]
    alarms[0].update(minute=45, repeat="workday")
    assert checks_pass(task, state)
    alarms[0]["id"] = "replacement"
    assert not checks_pass(task, state)
    alarms[0]["id"] = "alarm-target"
    alarms[1]["minute"] = 45
    assert not checks_pass(task, state)


def test_workflows_are_in_default_mobilegym_ci_and_simulator_report_group():
    case = next(case for case in select_cases(load_catalog(), profile="runnable")
                if case.suite == "suites/mobilegym_workflow_v1.json")
    assert case.environment == "mobilegym"
    assert case.target_platform == "android"
    assert capability_of(WORKFLOWS.name) == "UI operation (simulator)"
