"""Send a weekly benchmark report card to Lark via custom bot webhook."""

from __future__ import annotations

import argparse
import base64
import hashlib
import hmac
import json
import os
import time
import urllib.request
from pathlib import Path
from typing import Any


def _sign(secret: str, timestamp: str) -> str:
    """Lark custom bot signature: HMAC-SHA256 keyed by 'timestamp\\nsecret'."""
    key = f"{timestamp}\n{secret}".encode()
    return base64.b64encode(hmac.new(key, digestmod=hashlib.sha256).digest()).decode()


def _format_rate(value: float | None) -> str:
    return "—" if value is None else f"{value * 100:.1f}%"


def _format_cost(value: float | None) -> str:
    return "—" if value is None else f"${value:.3f}"


def _format_tokens(value: float | None) -> str:
    return "—" if value is None else f"{value / 1e6:.2f}M"


def _build_card(weekly: dict[str, Any], run_url: str, warnings: list[str]) -> dict[str, Any]:
    """Build a Lark interactive card from the weekly summary."""
    current, previous = weekly["current"], weekly["previous"]

    # Overall metrics
    current_rate = _format_rate(current["pass_at_1"])
    prev_rate = _format_rate(previous["pass_at_1"])
    delta_pp = None
    if current["pass_at_1"] is not None and previous["pass_at_1"] is not None:
        delta_pp = (current["pass_at_1"] - previous["pass_at_1"]) * 100
    delta_text = f" ({delta_pp:+.1f} pp)" if delta_pp is not None else ""

    elements = [
        {
            "tag": "markdown",
            "content": f"**Benchmark Weekly Report**\n{weekly['start'][:10]} to {weekly['end'][:10]}",
        },
        {"tag": "hr"},
        {
            "tag": "column_set",
            "flex_mode": "none",
            "background_style": "grey",
            "columns": [
                {
                    "tag": "column",
                    "width": "weighted",
                    "weight": 1,
                    "elements": [
                        {"tag": "markdown", "content": f"**Last 7 days**\n{current_rate}{delta_text}"},
                    ],
                },
                {
                    "tag": "column",
                    "width": "weighted",
                    "weight": 1,
                    "elements": [
                        {"tag": "markdown", "content": f"**Previous 7 days**\n{prev_rate}"},
                    ],
                },
            ],
        },
        {
            "tag": "column_set",
            "flex_mode": "none",
            "background_style": "grey",
            "columns": [
                {
                    "tag": "column",
                    "width": "weighted",
                    "weight": 1,
                    "elements": [
                        {"tag": "markdown", "content": f"Runs: {current['suite_runs']}"},
                        {"tag": "markdown", "content": f"Cost: {_format_cost(current['cost_usd_sum'])}"},
                    ],
                },
                {
                    "tag": "column",
                    "width": "weighted",
                    "weight": 1,
                    "elements": [
                        {"tag": "markdown", "content": f"Runs: {previous['suite_runs']}"},
                        {"tag": "markdown", "content": f"Cost: {_format_cost(previous['cost_usd_sum'])}"},
                    ],
                },
            ],
        },
    ]

    # Top changes (up to 3, only if delta >= 5pp decline or any improvement)
    changes = [s for s in weekly["suites"] if s["delta"] is not None and abs(s["delta"]) >= 0.001]
    notable = [s for s in changes if s["delta"] < -0.05 or s["delta"] > 0][:3]

    if notable:
        elements.append({"tag": "hr"})
        elements.append({"tag": "markdown", "content": "**Notable changes**"})
        for suite in notable:
            delta_pp = suite["delta"] * 100
            color = "red" if delta_pp < -5 else "green" if delta_pp > 0 else "grey"
            suite_line = (
                f"<font color='{color}'>{suite['suite']}: {_format_rate(suite['previous']['pass_at_1'])} "
                f"→ {_format_rate(suite['current']['pass_at_1'])} ({delta_pp:+.1f} pp)</font>"
            )
            elements.append({"tag": "markdown", "content": suite_line})

    if warnings:
        elements.append({"tag": "hr"})
        elements.append({"tag": "markdown", "content": "⚠️ Data incomplete (see run for details)"})

    elements.append({"tag": "hr"})
    elements.append({
        "tag": "action",
        "actions": [
            {
                "tag": "button",
                "text": {"tag": "plain_text", "content": "View full report"},
                "type": "primary",
                "url": run_url,
            }
        ],
    })

    return {
        "config": {"wide_screen_mode": True},
        "header": {
            "template": "blue" if delta_pp is None or delta_pp >= 0 else "red",
            "title": {"tag": "plain_text", "content": "Benchmark Weekly Report"},
        },
        "elements": elements,
    }


def send_lark(card: dict[str, Any], *, webhook_url: str, webhook_secret: str) -> None:
    """Send a card to Lark via webhook with signature verification."""
    timestamp = str(int(time.time()))
    body = {
        "timestamp": timestamp,
        "sign": _sign(webhook_secret, timestamp),
        "msg_type": "interactive",
        "card": card,
    }

    request = urllib.request.Request(
        webhook_url,
        data=json.dumps(body).encode("utf-8"),
        headers={"Content-Type": "application/json"},
    )

    with urllib.request.urlopen(request, timeout=10) as response:
        result = json.load(response)

    # Lark returns HTTP 200 with a nonzero code on errors
    if result.get("code", 0) != 0:
        raise RuntimeError(f"Lark rejected the message: code {result.get('code')}, msg {result.get('msg')}")


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--data-json", type=Path, required=True,
                        help="dashboard-data.json with the weekly summary")
    parser.add_argument("--run-url", required=True,
                        help="GitHub Actions run URL for the 'View full report' button")
    args = parser.parse_args(argv)

    webhook_url = os.environ.get("LARK_WEBHOOK_URL")
    webhook_secret = os.environ.get("LARK_WEBHOOK_SECRET")

    if not webhook_url or not webhook_secret:
        print("LARK_WEBHOOK_URL or LARK_WEBHOOK_SECRET not set; skipping notification")
        return 0

    data = json.loads(args.data_json.read_text(encoding="utf-8"))
    weekly = data.get("weekly")
    if not weekly:
        print("No weekly summary in data; skipping notification")
        return 0

    warnings = data.get("warnings", [])
    card = _build_card(weekly, args.run_url, warnings)

    try:
        send_lark(card, webhook_url=webhook_url, webhook_secret=webhook_secret)
        print(f"Lark notification sent: {args.run_url}")
    except Exception as exc:
        print(f"Failed to send Lark notification: {exc}")
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(cli())
