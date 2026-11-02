import json
from unittest.mock import patch

import pytest

from runner.assertions import evaluate_environment_state_assertions
from runner.environment_state import EnvironmentStateError, read_environment_state


class FakeResponse:
    def __init__(self, body: dict):
        self._body = json.dumps(body).encode("utf-8")

    def read(self):
        return self._body

    def __enter__(self):
        return self

    def __exit__(self, *exc):
        return False


def test_read_environment_state_combines_snapshot_and_route():
    seen = []
    responses = {
        "http://127.0.0.1:19090/state": {
            "ok": True,
            "data": {"apps": {"scroll_lab": {"selectedItemId": "scroll-item-083"}}},
        },
        "http://127.0.0.1:19090/route": {
            "ok": True,
            "data": {"app": "scroll_lab", "path": "/item/scroll-item-083"},
        },
    }

    def fake_urlopen(request, timeout=None):
        seen.append((request.full_url, request.get_header("Benchmark-task-id"), timeout))
        return FakeResponse(responses[request.full_url])

    with patch("urllib.request.urlopen", fake_urlopen):
        state = read_environment_state(
            "http://127.0.0.1:19090",
            benchmark_task_id="suite.json:deep",
            timeout=7,
        )

    assert state == {
        "apps": {"scroll_lab": {"selectedItemId": "scroll-item-083"}},
        "route": {"app": "scroll_lab", "path": "/item/scroll-item-083"},
    }
    assert seen == [
        ("http://127.0.0.1:19090/state", "suite.json:deep", 7),
        ("http://127.0.0.1:19090/route", "suite.json:deep", 7),
    ]


def test_read_environment_state_rejects_non_object_data():
    with patch(
        "urllib.request.urlopen",
        return_value=FakeResponse({"ok": True, "data": []}),
    ):
        with pytest.raises(EnvironmentStateError, match="no object data"):
            read_environment_state("http://127.0.0.1:19090")


def test_evaluate_environment_state_assertions_reports_exact_path_results():
    state = {
        "route": {"app": "scroll_lab", "path": "/item/scroll-item-024"},
        "apps": {"scroll_lab": {"selectedItemId": "scroll-item-083"}},
    }

    results = evaluate_environment_state_assertions(
        state,
        {
            "route.app": "scroll_lab",
            "route.path": "/item/scroll-item-083",
            "apps.scroll_lab.missing": None,
        },
    )

    assert [(result.path, result.passed, result.actual) for result in results] == [
        ("route.app", True, "scroll_lab"),
        ("route.path", False, "/item/scroll-item-024"),
        ("apps.scroll_lab.missing", False, "<missing>"),
    ]


def test_evaluate_environment_state_assertions_resolves_array_indices():
    state = {"apps": {"notes": {"notes": [{"content": "hello-aiden"}]}}}
    results = evaluate_environment_state_assertions(state, {
        "apps.notes.notes.0.content": "hello-aiden",
        "apps.notes.notes.1.content": "hello-aiden",
    })
    assert [(item.passed, item.actual) for item in results] == [
        (True, "hello-aiden"), (False, "<missing>"),
    ]


def test_evaluate_environment_state_assertions_supports_numeric_ranges():
    state = {
        "apps": {"scroll_lab": {"firstVisibleOrdinal": 8, "scrollTop": 720}},
    }

    results = evaluate_environment_state_assertions(
        state,
        {
            "apps.scroll_lab.firstVisibleOrdinal": {"min": 5, "max": 11},
            "apps.scroll_lab.scrollTop": {"max": 900},
        },
    )

    assert [(result.path, result.passed, result.actual) for result in results] == [
        ("apps.scroll_lab.firstVisibleOrdinal", True, 8),
        ("apps.scroll_lab.scrollTop", True, 720),
    ]


def test_evaluate_environment_state_assertions_range_rejects_overshoot_and_non_number():
    state = {
        "apps": {"scroll_lab": {"firstVisibleOrdinal": 15}},
    }

    results = evaluate_environment_state_assertions(
        state,
        {
            "apps.scroll_lab.firstVisibleOrdinal": {"min": 5, "max": 11},
            "apps.scroll_lab.selectedItemId": {"min": 0, "max": 10},
        },
    )

    assert [(result.path, result.passed, result.actual) for result in results] == [
        ("apps.scroll_lab.firstVisibleOrdinal", False, 15),
        ("apps.scroll_lab.selectedItemId", False, "<missing>"),
    ]
