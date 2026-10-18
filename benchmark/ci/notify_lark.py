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

from ci.weekly_report import describe_config_changes


def _sign(secret: str, timestamp: str) -> str:
    """Lark custom bot signature: HMAC-SHA256 keyed by 'timestamp\\nsecret'."""
    key = f"{timestamp}\n{secret}".encode()
    return base64.b64encode(hmac.new(key, digestmod=hashlib.sha256).digest()).decode()


def _format_rate(value: float | None) -> str:
    return "—" if value is None else f"{value * 100:.1f}%"


def _format_cost(value: float | None) -> str:
    return "—" if value is None else f"${value:.3f}"


def _format_change(change: float | None) -> str:
    """Percentage-point change from the previous seven days: green up, red down."""
    if change is None:
        return ""
    points = change * 100
    if round(points, 1) == 0:
        return " <font color='grey'>±0.0pp</font>"
    return f" <font color='{'green' if points > 0 else 'red'}'>{points:+.1f}pp</font>"


def _build_card(data: dict[str, Any], dashboard_url: str) -> dict[str, Any]:
    """Build a Lark card summarizing the last seven days and their change; details live in the dashboard."""
    weekly = data["weekly"]
    suites = weekly["suites"]
    week = weekly["totals"]
    regressions = data.get("regressions", [])

    overview = (f"**Runs:** {len(data.get('runs', []))} workflow runs, {week['suite_runs']} dataset runs\n"
                f"**pass@1:** {_format_rate(week['pass_at_1'])}{_format_change(weekly['change'])}"
                f" ({week['passed']:.0f}/{week['eligible_tasks']:.0f} eligible tasks)")
    if week["cost_usd_sum"] is not None:
        overview += f"\n**Cost:** {_format_cost(week['cost_usd_sum'])}"
    elements = [
        {"tag": "markdown", "content": f"{weekly['start'][:10]} to {weekly['end'][:10]} (UTC)"},
        {"tag": "markdown", "content": overview},
    ]

    # Group suites by capability so related changes surface together.
    capabilities = weekly.get("capabilities", [])
    if capabilities:
        lines = []
        for cap in capabilities:
            header = f"**{cap['capability']}**: {_format_rate(cap['pass_at_1'])}{_format_change(cap['change'])}"
            if cap.get("eligible_tasks"):
                header += f" ({cap['passed']:.0f}/{cap['eligible_tasks']:.0f})"
            lines.append(header)
        elements.append({"tag": "markdown", "content": "\n".join(lines)})

    suites = weekly["suites"]
    if suites:
        # Weakest suites first, so those needing attention lead.
        lines = [f"{s['suite']}: {_format_rate(s['pass_at_1'])}{_format_change(s['change'])}"
                 + (f" ({s['passed']:.0f}/{s['eligible_tasks']:.0f}, {s['suite_runs']} run{'s' if s['suite_runs'] != 1 else ''})"
                    if s["eligible_tasks"] else "")
                 + (f"\n  <font color='grey'>Config changed · {describe_config_changes(s['config_changes'])}</font>"
                    if s.get("config_changes") else "")
                 for s in suites]
        elements += [{"tag": "hr"},
                     {"tag": "markdown", "content": "**pass@1 by dataset** (change vs previous 7 days)\n" + "\n".join(lines)}]

    if regressions:
        lines = [f"<font color='red'>{r['suite']}: {_format_rate(r['previous'])} → {_format_rate(r['current'])}</font>"
                 for r in regressions]
        elements += [{"tag": "hr"},
                     {"tag": "markdown", "content": "**Regressions** (same configuration)\n" + "\n".join(lines)}]

    if data.get("warnings"):
        elements.append({"tag": "markdown", "content": "⚠️ Some data is incomplete; see the dashboard."})

    elements += [{"tag": "hr"}, {
        "tag": "action",
        "actions": [{
            "tag": "button",
            "text": {"tag": "plain_text", "content": "Open Dashboard"},
            "type": "primary",
            "url": dashboard_url,
        }],
    }]

    return {
        "config": {"wide_screen_mode": True},
        "header": {
            "template": "red" if regressions else "blue",
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
    parser.add_argument("--dashboard-url", required=True,
                        help="Public URL of the dashboard.html")
    args = parser.parse_args(argv)

    webhook_url = os.environ.get("BENCHMARK_REPORT_LARK_WEBHOOK_URL")
    webhook_secret = os.environ.get("BENCHMARK_REPORT_LARK_WEBHOOK_SECRET")

    if not webhook_url or not webhook_secret:
        print("BENCHMARK_REPORT_LARK_WEBHOOK_URL or BENCHMARK_REPORT_LARK_WEBHOOK_SECRET not set; skipping notification")
        return 0

    data = json.loads(args.data_json.read_text(encoding="utf-8"))
    weekly = data.get("weekly")
    if not weekly:
        print("No weekly summary in data; skipping notification")
        return 0

    card = _build_card(data, args.dashboard_url)

    try:
        send_lark(card, webhook_url=webhook_url, webhook_secret=webhook_secret)
        print(f"Lark notification sent: {args.dashboard_url}")
    except Exception as exc:
        print(f"Failed to send Lark notification: {exc}")
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(cli())
