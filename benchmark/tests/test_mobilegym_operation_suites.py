"""Acceptance-oracle regressions, not evidence of agent/device performance."""
from copy import deepcopy
from pathlib import Path

import pytest

from ci.plan import load_catalog, select_cases
from ci.weekly_report import capability_of
from runner.assertions import evaluate_environment_state_assertions
from runner.suite import load_suite

SUITES = Path(__file__).resolve().parents[1] / 'suites'
NAMES = ('mobilegym_navigation_v1', 'mobilegym_editing_v1', 'mobilegym_controls_v1')
WORKLOADS = [load_suite(SUITES / f'{name}.json') for name in NAMES]
TASKS = {task.id: task for suite in WORKLOADS for task in suite.tasks}


def passes(task, state):
    return all(result.passed for result in evaluate_environment_state_assertions(
        state, task.environment_assertions
    ))


def note_result(task_id, **changes):
    task = TASKS[task_id]
    state = deepcopy(task.initial_environment_state)
    state['route'] = {'app': 'notes', 'path': '/note/note-target'}
    state['apps']['notes']['notes'][0].update(**changes, updatedAt=1770000002000)
    return task, state


@pytest.mark.parametrize(('task_id', 'changes'), [
    ('body_multiline_ascii', {'content': 'Order #204: 2 x tea @ $3.50\nEmail: morgan+bench@example.com\nReady? Yes!'}),
    ('append_line', {'content': 'Owner: Morgan\nCode: AT-204\nStatus: draft\nReview: Friday'}),
    ('replace_middle_line', {'content': 'Owner: Morgan\nCode: AT-205\nStatus: draft'}),
    ('delete_last_character', {'content': 'Aiden-123'}),
    ('clear_body_keep_title', {'content': ''}),
    ('rename_title_keep_body', {'title': 'Project Atlas - approved'}),
    ('body_mixed_chinese_ascii', {'content': '周五 18:30 取快递\nCode: Aiden-204!'}),
])
def test_saved_edit_requires_exact_content_identity_and_guard(task_id, changes):
    task, correct = note_result(task_id, **changes)
    assert passes(task, correct)
    for index, field, value in [
        (0, 'content', 'truncated'), (0, 'title', 'wrong field'),
        (0, 'id', 'recreated'), (0, 'locked', True),
        (1, 'content', 'accidental guard edit'), (1, 'updatedAt', 0),
    ]:
        wrong = deepcopy(correct)
        wrong['apps']['notes']['notes'][index][field] = value
        assert not passes(task, wrong), (task_id, index, field)
    duplicate = deepcopy(correct)
    duplicate['apps']['notes']['notes'].append(deepcopy(duplicate['apps']['notes']['notes'][0]))
    assert not passes(task, duplicate)
    wrong_page = deepcopy(correct)
    wrong_page['route']['path'] = '/'
    assert not passes(task, wrong_page)


def test_chinese_pinyin_and_wrong_newlines_do_not_pass():
    task, state = note_result('body_mixed_chinese_ascii', content='zhou wu 18:30 qu kuai di\nCode: Aiden-204!')
    assert not passes(task, state)
    state['apps']['notes']['notes'][0]['content'] = '周五 18:30 取快递 Code: Aiden-204!'
    assert not passes(task, state)


@pytest.mark.parametrize('task_id', ['similar_titles_target_first', 'similar_titles_target_last', 'switch_from_settings', 'back_and_open_other_record'])
def test_navigation_rejects_wrong_record_and_mutations(task_id):
    task = TASKS[task_id]
    state = deepcopy(task.initial_environment_state)
    target = 'note-guard' if task_id == 'back_and_open_other_record' else 'note-target'
    state['route'] = {'app': 'notes', 'path': f'/note/{target}'}
    assert passes(task, state)
    wrong = deepcopy(state)
    wrong['route']['path'] = '/note/' + ('note-target' if target == 'note-guard' else 'note-guard')
    assert not passes(task, wrong)
    state['apps']['notes']['notes'][0]['content'] = 'accidentally edited'
    assert not passes(task, state)


def test_target_really_changes_position_between_fixtures():
    first = TASKS['similar_titles_target_first'].initial_environment_state['apps']['notes']['notes']
    last = TASKS['similar_titles_target_last'].initial_environment_state['apps']['notes']['notes']
    assert first[0]['id'] == last[-1]['id'] == 'note-target'
    assert first == list(reversed(last))


def test_reverse_scroll_requires_descent_and_correct_final_selection():
    task = TASKS['scroll_down_then_return']
    state = deepcopy(task.initial_environment_state)
    state['route'] = {'app': 'scroll_lab', 'path': '/item/scroll-item-003'}
    data = state['apps']['scroll_lab']
    data.update(selectedItemId='scroll-item-003', maxFirstVisibleOrdinal=21)
    assert passes(task, state)
    for ordinal in (1, 25, 83):
        data['maxFirstVisibleOrdinal'] = ordinal
        assert not passes(task, state)
    data.update(maxFirstVisibleOrdinal=21, selectedItemId='scroll-item-024')
    assert not passes(task, state)


@pytest.mark.parametrize(('task_id', 'enabled'), [('wifi_enable', True), ('wifi_disable', False), ('wifi_already_enabled', True)])
def test_wifi_checks_state_and_preserves_other_radios(task_id, enabled):
    task = TASKS[task_id]
    state = deepcopy(task.initial_environment_state)
    state['route'] = {'app': 'settings', 'path': '/page/wifi_settings'}
    settings = state['os']['settings']['global']
    settings['wifiEnabled'] = enabled
    assert passes(task, state)
    settings['wifiEnabled'] = not enabled
    assert not passes(task, state)
    settings['wifiEnabled'] = enabled
    for field in ('bluetoothEnabled', 'airplaneModeEnabled'):
        settings[field] = True
        assert not passes(task, state)
        settings[field] = False


@pytest.mark.parametrize(('task_id', 'changes'), [
    ('alarm_minute_forward', {'hour': 7, 'minute': 0}),
    ('alarm_minute_backward', {'hour': 6, 'minute': 59}),
    ('alarm_disable', {'enabled': False}),
    ('alarm_repeat_weekdays', {'repeat': 'weekday'}),
])
def test_alarm_checks_full_record_no_duplicates_or_guard_edits(task_id, changes):
    task = TASKS[task_id]
    correct = deepcopy(task.initial_environment_state)
    correct['route'] = {'app': 'clock', 'path': '/alarm'}
    correct['apps']['clock']['alarms'][0].update(**changes)
    assert passes(task, correct)
    for index, field, value in [(0, 'minute', 30), (0, 'repeat', 'holiday'), (0, 'id', 'new'), (0, 'vibrate', False), (1, 'enabled', True)]:
        wrong = deepcopy(correct)
        wrong['apps']['clock']['alarms'][index][field] = value
        assert not passes(task, wrong)
    correct['apps']['clock']['alarms'].append(deepcopy(correct['apps']['clock']['alarms'][0]))
    assert not passes(task, correct)


def test_pin_requires_target_only_and_original_contents():
    task, state = note_result('long_press_pin_note', pinned=True)
    state['route']['path'] = '/'
    assert passes(task, state)
    state['apps']['notes']['notes'][1]['pinned'] = True
    assert not passes(task, state)


@pytest.mark.parametrize('task', list(TASKS.values()), ids=lambda task: task.id)
def test_setup_is_independent_and_cannot_already_satisfy_full_outcome(task):
    state = deepcopy(task.initial_environment_state)
    app = task.foreground_app_id
    state['route'] = {'app': app, 'path': '/alarm' if app == 'clock' else '/'}
    assert not passes(task, state)
    assert not task.setup  # No hidden agent work to prepare the starting UI.
    assert task.mock_environment is None
    assert 'request_user_action' in task.hard_assertions.forbidden_tools


@pytest.mark.parametrize('suite', WORKLOADS, ids=lambda suite: suite.name)
def test_candidates_are_ci_selectable_and_reported_separately(suite):
    catalog = load_catalog()
    case, = select_cases(catalog, profile='runnable', suite=f'suites/{suite.name}.json')
    assert case.environment == 'mobilegym'
    assert case not in select_cases(catalog, profile='runnable')
    assert 'candidates (simulator)' in capability_of(suite.name)
