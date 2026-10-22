import pytest
from runner import preflight


def test_preflight_requires_observed_scroll_state(monkeypatch):
    states = iter([
        {'route': {'app': 'scroll_lab'}, 'apps': {'scroll_lab': {'firstVisibleOrdinal': 1}}},
        {'route': {'app': 'scroll_lab'}, 'apps': {'scroll_lab': {'firstVisibleOrdinal': 6}}},
    ])
    calls = []
    monkeypatch.setattr(preflight, 'read_environment_state', lambda *args, **kwargs: next(states), raising=False)
    monkeypatch.setattr(preflight, '_post_environment', lambda *args, **kwargs: calls.append((args, kwargs)) or {'output': 'ok'}, raising=False)
    preflight._verify_mobilegym_scroll('http://bridge', timeout=10, task_id='probe')
    assert calls[0][0][0] == 'http://bridge/api/tools/touch_gesture'
    assert calls[0][1]['headers']['benchmark-task-id'] == 'probe'


def test_preflight_releases_slot_when_action_does_not_change_state(monkeypatch):
    released = []
    monkeypatch.setattr(preflight, 'call_environment_setup', lambda *args, **kwargs: None)
    monkeypatch.setattr(preflight, 'call_environment_release', lambda *args, **kwargs: released.append(kwargs['task_id']))
    monkeypatch.setattr(preflight, 'read_environment_state', lambda *args, **kwargs: {'route': {'app': 'scroll_lab'}, 'apps': {'scroll_lab': {'firstVisibleOrdinal': 1}}}, raising=False)
    monkeypatch.setattr(preflight, '_post_environment', lambda *args, **kwargs: {'output': 'ok'}, raising=False)
    monkeypatch.setattr('time.sleep', lambda _: None)
    with pytest.raises(RuntimeError, match='preflight'):
        preflight.preflight_mobilegym_environment('http://bridge', health={'bridge_type': 'mobilegym'})
    assert len(released) == 1
