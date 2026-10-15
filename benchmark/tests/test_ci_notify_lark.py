"""Tests for ci.notify_lark."""

import base64
import hashlib
import hmac

from ci.notify_lark import _build_card, _sign


def _week(**overrides):
    week = {"suite_runs": 6, "measured_suite_runs": 6, "tasks": 30, "eligible_tasks": 24,
            "passed": 18, "pass_at_1": 0.75, "cost_usd_sum": None, "total_tokens_sum": None}
    week.update(overrides)
    return week


def _suite(name, pass_at_1, passed=None, eligible=None):
    return {"suite": name, "latest": {"pass_at_1": pass_at_1, "passed": passed, "eligible_tasks": eligible}}


def _data(**overrides):
    data = {
        "weekly": {"start": "2026-10-06T00:00:00+00:00", "end": "2026-10-13T00:00:00+00:00",
                   "current": _week(), "previous": _week(pass_at_1=0.10), "suites": []},
        "runs": [{}, {}],
        "suites": [_suite("memory", 0.5, 2.0, 4.0), _suite("perception", 0.9, 9, 10)],
        "regressions": [],
        "warnings": [],
    }
    data.update(overrides)
    return data


def _text(card):
    return "\n".join(e["content"] for e in card["elements"] if e.get("tag") == "markdown")


def test_sign_matches_lark_custom_bot_algorithm():
    key = b"1626164479\ntest_secret"
    expected = base64.b64encode(hmac.new(key, digestmod=hashlib.sha256).digest()).decode()
    assert _sign("test_secret", "1626164479") == expected


def test_card_summarizes_the_last_week_without_a_previous_week_comparison():
    text = _text(_build_card(_data(), "https://example.com/d.html"))
    assert "2026-10-06 to 2026-10-13" in text
    assert "2 workflow runs, 6 dataset runs" in text
    assert "75.0% (18/24 eligible tasks)" in text
    # The previous window's 10% rate must not appear.
    assert "10.0%" not in text and "Previous" not in text


def test_card_lists_every_dataset_in_the_given_order():
    text = _text(_build_card(_data(), "https://example.com/d.html"))
    assert "memory: 50.0% (2/4)" in text
    assert "perception: 90.0% (9/10)" in text
    assert text.index("memory") < text.index("perception")


def test_unknown_dataset_rate_shows_a_dash_without_counts():
    text = _text(_build_card(_data(suites=[_suite("new", None)]), "https://example.com/d.html"))
    assert "new: —" in text and "new: — (" not in text


def test_cost_is_shown_only_when_known():
    assert "Cost" not in _text(_build_card(_data(), "https://example.com/d.html"))
    data = _data()
    data["weekly"]["current"] = _week(cost_usd_sum=1.25)
    assert "$1.250" in _text(_build_card(data, "https://example.com/d.html"))


def test_regressions_are_listed_and_turn_the_header_red():
    data = _data(regressions=[{"suite": "memory", "previous": 0.8, "current": 0.5}])
    card = _build_card(data, "https://example.com/d.html")
    assert "memory: 80.0% → 50.0%" in _text(card)
    assert card["header"]["template"] == "red"
    assert _build_card(_data(), "https://example.com/d.html")["header"]["template"] == "blue"


def test_warnings_add_a_data_incomplete_note():
    assert "incomplete" not in _text(_build_card(_data(), "https://example.com/d.html"))
    data = _data(warnings=["Langfuse experiment history is incomplete: APIError."])
    assert "incomplete" in _text(_build_card(data, "https://example.com/d.html"))


def test_button_opens_the_dashboard():
    card = _build_card(_data(), "https://example.com/d.html")
    action = next(e for e in card["elements"] if e.get("tag") == "action")
    assert action["actions"][0]["url"] == "https://example.com/d.html"
