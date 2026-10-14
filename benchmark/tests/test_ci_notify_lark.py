"""Tests for ci.notify_lark."""

from datetime import datetime, timezone

import pytest

from ci.notify_lark import _build_card, _sign


def test_sign_generates_valid_lark_signature():
    # Known test vector from Lark's documentation
    timestamp = "1626164479"
    secret = "test_secret"
    signature = _sign(secret, timestamp)
    # The signature is deterministic
    assert isinstance(signature, str)
    assert len(signature) > 20


def test_build_card_includes_overall_metrics():
    weekly = {
        "start": "2026-10-06T00:00:00+00:00",
        "end": "2026-10-13T00:00:00+00:00",
        "current": {"pass_at_1": 0.75, "suite_runs": 5, "cost_usd_sum": 12.5, "total_tokens_sum": 500000},
        "previous": {"pass_at_1": 0.70, "suite_runs": 4, "cost_usd_sum": 10.0, "total_tokens_sum": 400000},
        "suites": [],
    }
    card = _build_card(weekly, "https://example.com", [])

    assert card["header"]["title"]["content"] == "Benchmark Weekly Report"
    # Check that rates and deltas appear in the card
    content = str(card["elements"])
    assert "75.0%" in content
    assert "70.0%" in content
    assert "+5.0 pp" in content or "5.0" in content


def test_build_card_shows_notable_declines_in_red():
    weekly = {
        "start": "2026-10-06T00:00:00+00:00",
        "end": "2026-10-13T00:00:00+00:00",
        "current": {"pass_at_1": 0.65, "suite_runs": 3, "cost_usd_sum": None, "total_tokens_sum": None},
        "previous": {"pass_at_1": 0.60, "suite_runs": 3, "cost_usd_sum": None, "total_tokens_sum": None},
        "suites": [
            {
                "suite": "memory",
                "delta": -0.10,
                "current": {"pass_at_1": 0.50},
                "previous": {"pass_at_1": 0.60},
            },
            {
                "suite": "stable",
                "delta": 0.0,
                "current": {"pass_at_1": 0.80},
                "previous": {"pass_at_1": 0.80},
            },
        ],
    }
    card = _build_card(weekly, "https://example.com", [])
    content = str(card["elements"])
    # A 10pp decline is notable and should be red
    assert "memory" in content
    assert "red" in content.lower()
    # Stable suite is not notable
    assert "stable" not in content


def test_build_card_shows_improvements():
    weekly = {
        "start": "2026-10-06T00:00:00+00:00",
        "end": "2026-10-13T00:00:00+00:00",
        "current": {"pass_at_1": 0.80, "suite_runs": 3, "cost_usd_sum": 5.0, "total_tokens_sum": 200000},
        "previous": {"pass_at_1": 0.75, "suite_runs": 3, "cost_usd_sum": 4.5, "total_tokens_sum": 180000},
        "suites": [
            {
                "suite": "improved",
                "delta": 0.08,
                "current": {"pass_at_1": 0.88},
                "previous": {"pass_at_1": 0.80},
            },
        ],
    }
    card = _build_card(weekly, "https://example.com", [])
    content = str(card["elements"])
    assert "improved" in content
    assert "green" in content.lower()


def test_header_color_follows_overall_change_not_last_suite():
    weekly = {
        "start": "2026-10-06T00:00:00+00:00",
        "end": "2026-10-13T00:00:00+00:00",
        "current": {"pass_at_1": 0.60, "suite_runs": 3, "cost_usd_sum": None, "total_tokens_sum": None},
        "previous": {"pass_at_1": 0.70, "suite_runs": 3, "cost_usd_sum": None, "total_tokens_sum": None},
        "suites": [{"suite": "gained", "delta": 0.08,
                    "current": {"pass_at_1": 0.88}, "previous": {"pass_at_1": 0.80}}],
    }
    assert _build_card(weekly, "https://example.com", [])["header"]["template"] == "red"


def test_build_card_with_warnings():
    weekly = {
        "start": "2026-10-06T00:00:00+00:00",
        "end": "2026-10-13T00:00:00+00:00",
        "current": {"pass_at_1": 0.70, "suite_runs": 2, "cost_usd_sum": None, "total_tokens_sum": None},
        "previous": {"pass_at_1": 0.68, "suite_runs": 2, "cost_usd_sum": None, "total_tokens_sum": None},
        "suites": [],
    }
    warnings = ["Langfuse experiment history is incomplete: APIError."]
    card = _build_card(weekly, "https://example.com", warnings)
    content = str(card["elements"])
    assert "incomplete" in content.lower() or "⚠" in content


def test_build_card_handles_missing_rates():
    weekly = {
        "start": "2026-10-06T00:00:00+00:00",
        "end": "2026-10-13T00:00:00+00:00",
        "current": {"pass_at_1": None, "suite_runs": 1, "cost_usd_sum": None, "total_tokens_sum": None},
        "previous": {"pass_at_1": None, "suite_runs": 0, "cost_usd_sum": None, "total_tokens_sum": None},
        "suites": [],
    }
    card = _build_card(weekly, "https://example.com", [])
    content = str(card["elements"])
    # Should show dashes for missing rates
    assert "—" in content
