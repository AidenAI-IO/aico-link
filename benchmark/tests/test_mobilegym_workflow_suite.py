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


def checks_pass(task, state):
    return all(check.passed for check in evaluate_environment_state_assertions(
        state, task.environment_assertions
    ))


@pytest.mark.parametrize("task", WORKFLOWS.tasks,
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


def test_note_edit_preserves_identity_other_lines_and_unrelated_notes():
    task = next(task for task in WORKFLOWS.tasks if task.id == "note_replace_one_item")
    state = deepcopy(task.initial_environment_state)
    notes = state["apps"]["notes"]["notes"]
    notes[0].update(content="Milk\nTea\nRice", updatedAt=1770000001000)
    assert checks_pass(task, state)
    correct = deepcopy(state)
    notes[0]["content"] = "Tea"
    assert not checks_pass(task, state)
    state = deepcopy(correct)
    state["apps"]["notes"]["notes"][0]["id"] = "recreated-note"
    assert not checks_pass(task, state)
    state = deepcopy(correct)
    state["apps"]["notes"]["notes"][1]["content"] = "Tea"
    assert not checks_pass(task, state)


@pytest.mark.parametrize("task_id", ["contact_complete_form", "note_to_contact"])
def test_contact_creation_rejects_missing_fields_duplicates_and_guard_edits(task_id):
    task = next(task for task in WORKFLOWS.tasks if task.id == task_id)
    state = deepcopy(task.initial_environment_state)
    contacts = state["os"]["providers"]["contacts"]["contacts"]
    expected = task.environment_assertions["os.providers.contacts.contacts"]["$records"][-1]
    contacts.append({**deepcopy(expected), "id": "generated-contact-id"})
    assert checks_pass(task, state)
    contacts[-1]["emails"] = []
    assert not checks_pass(task, state)
    contacts[-1]["emails"] = deepcopy(expected["emails"])
    contacts.append(deepcopy(contacts[-1]))
    assert not checks_pass(task, state)
    contacts.pop()
    contacts[0]["displayName"] = "Changed guard"
    assert not checks_pass(task, state)


def test_workflows_are_manual_experiments():
    catalog = load_catalog()
    case, = select_cases(catalog, profile="runnable", suite="suites/mobilegym_workflow_v1.json")
    assert case not in select_cases(catalog, profile="runnable")
    assert {task.id for task in WORKFLOWS.tasks} == {
        "note_replace_one_item", "contact_complete_form", "note_to_contact",
    }
    assert case.environment == "mobilegym"
    assert case.target_platform == "android"
    assert capability_of(WORKFLOWS.name) == "Experimental workflows (simulator)"
