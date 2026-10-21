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


@pytest.mark.parametrize("records,passed", [
    ([{"id": "random", "name": "New", "timestamp": 10},
      {"id": "guard", "name": "Old"}], True),
    ([{"name": "New"}, {"id": "guard", "name": "Changed"}], False),
    ([{"name": "New"}], False),
    ([{"name": "New"}, {"id": "guard", "name": "Old"}, {"name": "New"}], False),
    ({}, False),
])
def test_record_assertions_accept_generated_ids_but_reject_collateral_changes(records, passed):
    checks = evaluate_environment_state_assertions(
        {"records": records},
        {"records": {"$records": [{"id": "guard", "name": "Old"}, {"name": "New"}]}},
    )
    assert checks[0].passed is passed


def test_record_assertions_assign_distinct_matches_even_for_overlapping_subsets():
    spec = {"records": {"$records": [{"name": "Same"}, {"id": "specific", "name": "Same"}]}}
    for ids, passed in [(["specific", "other"], True), (["specific", "wrong"], True),
                        (["other", "wrong"], False)]:
        checks = evaluate_environment_state_assertions(
            {"records": [{"id": id, "name": "Same"} for id in ids]}, spec
        )
        assert checks[0].passed is passed


def test_record_assertions_do_not_reuse_one_matching_record():
    checks = evaluate_environment_state_assertions(
        {"records": [{"name": "Same"}, {"name": "Other"}]},
        {"records": {"$records": [{"name": "Same"}, {"name": "Same"}]}},
    )
    assert checks[0].passed is False


def test_record_matcher_preserves_exact_object_equality_and_empty_collection_semantics():
    checks = evaluate_environment_state_assertions(
        {"object": {"name": "New", "extra": True}, "empty": []},
        {"object": {"name": "New"}, "empty": {"$records": []}},
    )
    assert [check.passed for check in checks] == [False, True]
