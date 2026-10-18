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


def _suite(name, pass_at_1, passed=0, eligible=0, runs=1, change=None):
    return {"suite": name, "pass_at_1": pass_at_1, "passed": passed, "eligible_tasks": eligible,
            "suite_runs": runs, "change": change}


def _data(**overrides):
    data = {
        "weekly": {"start": "2026-10-06T00:00:00+00:00", "end": "2026-10-13T00:00:00+00:00",
                   "totals": _week(), "change": None,
                   "suites": [_suite("memory", 0.5, 2.0, 4.0, 2), _suite("perception", 0.9, 9, 10)]},
        "runs": [{}, {}],
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


def test_card_summarizes_the_last_week():
    text = _text(_build_card(_data(), "https://example.com/d.html"))
    assert "2026-10-06 to 2026-10-13" in text
    assert "2 workflow runs, 6 dataset runs" in text
    assert "75.0% (18/24 eligible tasks)" in text
    assert "Previous" not in text


def test_card_lists_each_datasets_weekly_rate_in_the_given_order():
    text = _text(_build_card(_data(), "https://example.com/d.html"))
    assert "memory: 50.0% (2/4, 2 runs)" in text
    assert "perception: 90.0% (9/10, 1 run)" in text
    assert text.index("memory") < text.index("perception")


def test_changes_from_the_previous_week_are_colored_by_direction():
    data = _data()
    data["weekly"]["change"] = -0.05
    data["weekly"]["suites"] = [_suite("up", 0.9, 9, 10, change=0.1), _suite("down", 0.4, 4, 10, change=-0.2),
                                _suite("flat", 0.5, 5, 10, change=0.0004), _suite("new", 0.5, 5, 10)]
    text = _text(_build_card(data, "https://example.com/d.html"))
    assert "75.0% <font color='red'>-5.0pp</font> (18/24" in text
    assert "up: 90.0% <font color='green'>+10.0pp</font> (9/10, 1 run)" in text
    assert "down: 40.0% <font color='red'>-20.0pp</font>" in text
    assert "flat: 50.0% <font color='grey'>±0.0pp</font>" in text
    # Without a previous-week rate, no change is shown.
    assert "new: 50.0% (5/10, 1 run)" in text
    assert "Config changed" not in text


def test_changed_settings_are_listed_under_the_dataset():
    data = _data()
    suite = _suite("perception", 0.75, 9, 12, change=-0.08)
    suite["config_changes"] = [{"field": "agent model", "previous": "google/a", "current": "anthropic/b"}]
    data["weekly"]["suites"] = [suite]
    text = _text(_build_card(data, "https://example.com/d.html"))
    assert "perception: 75.0% <font color='red'>-8.0pp</font> (9/12, 1 run)\n" \
           "  <font color='grey'>Config changed · agent model: google/a → anthropic/b</font>" in text


def test_unknown_dataset_rate_shows_a_dash_without_counts():
    data = _data()
    data["weekly"]["suites"] = [_suite("new", None)]
    text = _text(_build_card(data, "https://example.com/d.html"))
    assert "new: —" in text and "new: — (" not in text


def test_cost_is_shown_only_when_known():
    assert "Cost" not in _text(_build_card(_data(), "https://example.com/d.html"))
    data = _data()
    data["weekly"]["totals"] = _week(cost_usd_sum=1.25)
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


def test_card_leads_with_capability_rates():
    data = _data()
    data["weekly"]["capabilities"] = [
        {"capability": "Memory", "pass_at_1": 0.75, "passed": 12, "eligible_tasks": 16,
         "task_count": 8, "run_count": 2, "change": 0.25},
        {"capability": "Visual grounding", "pass_at_1": 0.5, "passed": 6, "eligible_tasks": 12,
         "task_count": 12, "run_count": 1, "change": None},
    ]
    text = _text(_build_card(data, "https://example.com/d.html"))
    assert "**Memory**: 75.0% <font color='green'>+25.0pp</font> (8 tasks, 2 runs, 12/16 eligible first attempts passed)" in text
    assert "**Visual grounding**: 50.0% (12 tasks, 1 run, 6/12 eligible first attempts passed)" in text
    assert text.index("**Memory**") < text.index("memory: 50.0%")
