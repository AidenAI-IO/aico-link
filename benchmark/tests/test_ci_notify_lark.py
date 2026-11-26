"""Tests for the benchmark weekly Lark card."""

import base64
import hashlib
import hmac
from datetime import datetime, timezone

from ci.notify_lark import _build_card, _sign
from ci.weekly_report import summarize_weekly


END = datetime(2026, 10, 13, tzinfo=timezone.utc)
DASHBOARD = "https://example.com/dashboard.html"
REPORT = "https://github.com/owner/repo/actions/runs/123#summary"


def _record(suite, *, timestamp="2026-10-12T00:00:00+00:00", passed=3, eligible=4, tasks=8, **fields):
    return {
        "suite": suite, "timestamp": timestamp, "tasks": tasks, "eligible_tasks": eligible,
        "passed": passed, "git_branch": "main", **fields,
    }


def _card(records, **fields):
    weekly = summarize_weekly(records, end=END)
    return _build_card({"weekly": weekly, "history": records, **fields}, DASHBOARD, REPORT)


def _elements(node):
    yield node
    for key in ("elements", "columns"):
        for child in node.get(key, []):
            yield from _elements(child)
    for key in ("body", "header", "title", "text"):
        if key in node:
            yield from _elements(node[key])


def _text(card):
    return "\n".join(node["content"] for node in _elements(card) if "content" in node)


def test_sign_matches_lark_custom_bot_algorithm():
    key = b"1626164479\ntest_secret"
    expected = base64.b64encode(hmac.new(key, digestmod=hashlib.sha256).digest()).decode()
    assert _sign("test_secret", "1626164479") == expected


def test_card_separates_the_full_window_trend_coverage_and_shared_dataset_trend():
    card = _card([
        _record("memory_v1", timestamp="2026-10-05T00:00:00+00:00", passed=8, eligible=10, tasks=10),
        _record("memory_v1", missing_metrics=["evaluation_coverage"],
                failure_classes={"evaluation": 3, "skipped": 1}),
        _record("mobilegym_aico_app_v1", passed=2, eligible=2, tasks=2),
    ])
    text = _text(card)
    assert "5 / 6 项通过\n上周 80.0% <font color='green'>+3.3 pp</font>" in text
    assert "6 / 10 项有效\n上周 100.0% <font color='red'>-40.0 pp</font>" in text
    assert "**3** 次评测错误" in text and "**1** 次跳过" in text
    assert "相同 1 个数据集的通过率 · 80.0% → 75.0%" in text

    panels = [element for element in _elements(card) if element.get("tag") == "collapsible_panel"]
    assert all(not panel["expanded"] for panel in panels)
    assert "memory_v1 · **75.0%**（3/4） <font color='red'>-5.0 pp</font> · 覆盖 50.0%" in _text(panels[0])
    assert "mobilegym_aico_app_v1 · **100.0%**（2/2） · 新增 · 覆盖 100.0%" in _text(panels[1])
    urls = [element["behaviors"][0]["default_url"]
            for element in _elements(card) if element.get("tag") == "button"]
    assert urls == [DASHBOARD, REPORT]


def test_card_labels_a_fallback_with_its_date_without_adding_it_to_the_headline():
    card = _card([
        _record("memory_v1", timestamp="2026-10-05T00:00:00+00:00"),
        _record("memory_v1", passed=0, eligible=0, missing_metrics=["evaluation_coverage"],
                failure_classes={"environment": 8}),
    ])
    text = _text(card)
    assert "**暂无**\n0 / 0 项通过" in text
    assert "memory_v1 · 本周暂无有效评测 · 上次有效 75.0%（10/05） · 覆盖 0.0%" in text


def test_card_rebuilds_coverage_from_an_archived_dashboard_snapshot():
    records = [_record("perception_v1", missing_metrics=["evaluation_coverage"])]
    old = {"end": END.isoformat(), "totals": {"pass_at_1": None}}
    text = _text(_build_card({"weekly": old, "history": records}, DASHBOARD))
    assert "**75.0%**\n3 / 4 项通过" in text
    assert "**50.0%**\n4 / 8 项有效" in text


def test_card_only_shows_diagnostic_observations_that_were_recorded():
    text = _text(_card([
        _record("skill_discovery_v1", skill_discovery={"read": 1, "observed": 4}),
        _record("tool_recovery_v1"),
    ], warnings=["Langfuse experiment history is incomplete: APIError."]))
    assert "skill_discovery_v1 · 技能读取 25.0%" in text
    assert "tool_recovery_v1" not in text
    assert "部分数据缺失" in text
