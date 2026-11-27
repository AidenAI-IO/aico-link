"""Send a weekly benchmark report card to Lark via custom bot webhook."""

from __future__ import annotations

import argparse
import base64
import hashlib
import hmac
import html
import json
import os
import time
import urllib.request
from datetime import datetime
from pathlib import Path
from typing import Any
from zoneinfo import ZoneInfo


def _sign(secret: str, timestamp: str) -> str:
    """Lark custom bot signature: HMAC-SHA256 keyed by 'timestamp\\nsecret'."""
    key = f"{timestamp}\n{secret}".encode()
    return base64.b64encode(hmac.new(key, digestmod=hashlib.sha256).digest()).decode()


CAPABILITY_LABELS = {
    "Memory": "记忆",
    "Experience reflection": "经验复盘",
    "Visual grounding": "视觉定位",
    "UI operation": "界面操作",
    "Device integration": "设备集成",
    "Shell utilities": "Shell 工具",
    "Realtime collaboration": "实时协作",
    "Other": "其他",
}


def _format_rate(value: float | None) -> str:
    return "暂无" if value is None else f"{value:.1%}"


def _format_count(value: float | None) -> str:
    return "未知" if value is None else f"{value:,.0f}"


def _format_change(change: float | None) -> str:
    if change is None:
        return ""
    points = round(change * 100, 1)
    if points == 0:
        return " <font color='grey'>0.0 pp</font>"
    color = "green" if points > 0 else "red"
    return f" <font color='{color}'>{points:+.1f} pp</font>"


def _date(timestamp: str, fmt: str = "%m/%d %H:%M") -> str:
    return datetime.fromisoformat(timestamp.replace("Z", "+00:00")).astimezone(
        ZoneInfo("Asia/Shanghai")
    ).strftime(fmt)


def _markdown(content: str, **options: Any) -> dict[str, Any]:
    return {"tag": "markdown", "content": content, **options}


def _columns(*groups: list[dict[str, Any]], **options: Any) -> dict[str, Any]:
    return {"tag": "column_set", **options, "columns": [
        {"tag": "column", "width": "weighted", "weight": 1, "vertical_spacing": "4px", "elements": group}
        for group in groups
    ]}


def _change(current: float | None, previous: float | None) -> str:
    return _format_change(current - previous if current is not None and previous is not None else None)


def _metric(label: str, current: float | None, previous: float | None, counts: str) -> list[dict[str, Any]]:
    baseline = f"上周 {_format_rate(previous)}{_change(current, previous)}"
    return [
        _markdown(f"**{label}**"),
        _markdown(f"**{_format_rate(current)}**", text_size="heading-0"),
        _markdown(f"{counts}\n{baseline}", text_size="notation"),
    ]


def _latest_run(run: dict[str, Any]) -> list[dict[str, Any]]:
    """The newest workflow run's pass@1, from the summary the dashboard shows."""
    totals = run["totals"]
    counts = f"{_format_count(totals['passed'])} / {_format_count(totals['eligible_tasks'])} 项通过"
    models = ", ".join(model or "未知模型" for model in run.get("agent_models", []))
    detail = " · ".join(filter(None, [_date(run["start"]), html.escape(models)]))
    return [
        _markdown("**最近一次运行**"),
        _markdown(f"**{_format_rate(totals['pass_at_1'])}**", text_size="heading-0"),
        _markdown(f"{counts}\n{detail}", text_size="notation"),
    ]


def _suite_line(suite: dict[str, Any]) -> str:
    name = html.escape(suite["suite"])
    rate = suite["pass_at_1"]
    if rate is None:
        last = suite.get("last_valid")
        result = "本周暂无有效评测"
        if last:
            result += f" · 上次有效 {_format_rate(last['pass_at_1'])}（{_date(last['timestamp'], '%m/%d')}）"
    else:
        counts = f"{_format_count(suite['passed'])}/{_format_count(suite['eligible_tasks'])}"
        result = f"**{_format_rate(rate)}**（{counts}）{_format_change(suite.get('change'))}"
    if suite.get("new"):
        result += " · 新增"
    return f"- {name} · {result} · 覆盖 {_format_rate(suite.get('coverage'))}"


def _capability_panel(capability: dict[str, Any], suites: list[dict[str, Any]]) -> dict[str, Any]:
    label = CAPABILITY_LABELS.get(capability["capability"], capability["capability"])
    rate = capability["pass_at_1"]
    title = (f"**{html.escape(label)}** · {len(suites)} 个数据集 · **{_format_rate(rate)}**"
             f"{_change(rate, capability.get('previous_pass_at_1'))}"
             f" · 覆盖 {_format_rate(capability.get('coverage'))}")
    return {
        "tag": "collapsible_panel",
        "expanded": False,
        "header": {
            "title": _markdown(title),
            "icon": {"tag": "standard_icon", "token": "down-small-ccm_outlined", "size": "16px 16px"},
            "icon_position": "right",
            "icon_expanded_angle": 180,
        },
        "elements": [_markdown("\n".join(_suite_line(suite) for suite in suites))],
    }


def _link_button(label: str, url: str, *, primary: bool = False) -> dict[str, Any]:
    return {
        "tag": "button",
        "text": {"tag": "plain_text", "content": label},
        "type": "primary" if primary else "default",
        "behaviors": [{"type": "open_url", "default_url": url}],
    }


def _build_card(data: dict[str, Any], dashboard_url: str, weekly_report_url: str | None = None) -> dict[str, Any]:
    """Build a Lark card with separate score and coverage trends."""
    weekly = data["weekly"]
    if "coverage" not in weekly["totals"] and "history" in data:
        # Saved dashboards already contain the records needed to rebuild the report.
        from ci.weekly_report import summarize_weekly

        weekly = summarize_weekly(data["history"], end=datetime.fromisoformat(weekly["end"]))
    week = weekly["totals"]
    previous = weekly.get("previous_totals", {})
    suites = weekly["suites"]
    branches = {row.get("git_branch") for row in data.get("history", [])}
    branch = branches.pop() if len(branches) == 1 else None
    period = f"{_date(weekly['start'])} – {_date(weekly['end'])} · 北京时间"
    if branch:
        period += f" · {html.escape(branch)}"

    passed = _format_count(week["passed"])
    eligible = _format_count(week["eligible_tasks"])
    recorded = _format_count(week.get("tasks"))
    headline = [
        _metric("有效评测通过率", week["pass_at_1"], previous.get("pass_at_1"), f"{passed} / {eligible} 项通过"),
        _metric("评测覆盖率", week.get("coverage"), previous.get("coverage"), f"{eligible} / {recorded} 项有效"),
    ]
    if data.get("latest_run"):
        headline.append(_latest_run(data["latest_run"]))
    # Three columns are too narrow on phones; stretch stacks them there.
    elements = [
        _markdown(period, text_size="notation"),
        _columns(*headline, **({"flex_mode": "stretch"} if len(headline) > 2 else {})),
    ]
    failures = week.get("failure_counts", {})
    if failures and any(value is not None for value in failures.values()):
        labels = {"evaluation": "评测错误", "environment": "环境错误", "skipped": "跳过"}
        elements.append(_markdown(" · ".join(f"**{_format_count(failures.get(kind))}** 次{label}"
                                             for kind, label in labels.items())))
    run_count = week.get("run_count", len(data.get("runs", [])))
    runs = f"{run_count} 次工作流运行 · {week['suite_runs']} 次数据集运行"
    if week.get("cost_usd_sum") is not None:
        runs += f" · 成本 ${week['cost_usd_sum']:.3f}"
    elements.append(_markdown(runs, text_size="notation"))

    capabilities = weekly.get("capabilities", [])
    if capabilities:
        elements.extend([{"tag": "hr"}, _markdown("**按能力查看** · 通过率较上周")])
        for capability in capabilities:
            names = set(capability["suites"])
            rows = [suite for suite in suites if suite["suite"] in names]
            elements.append(_capability_panel(capability, rows))

    diagnostics = [row for row in weekly.get("diagnostics", []) if row["rate"] is not None]
    if diagnostics:
        lines = [f"{html.escape(row['suite'])} · 技能读取 {_format_rate(row['rate'])}"
                 f"{_format_change(row.get('change'))}" for row in diagnostics]
        elements.append(_markdown("**诊断观测**\n" + "\n".join(lines), text_size="notation"))

    if any(not warning.endswith("missing or invalid metrics: evaluation_coverage.")
           for warning in data.get("warnings", [])):
        elements.append(_markdown("部分数据缺失，详情见 Dashboard。", text_size="notation"))

    buttons = [_link_button("打开 Dashboard", dashboard_url, primary=True)]
    if weekly_report_url:
        buttons.append(_link_button("查看周报", weekly_report_url))
    elements += [{"tag": "hr"}, _columns(*([button] for button in buttons))]
    return {
        "schema": "2.0",
        "config": {"update_multi": True},
        "header": {
            "template": "blue",
            "title": {"tag": "plain_text", "content": "Benchmark 周报"},
        },
        "body": {"elements": elements},
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
    parser.add_argument("--weekly-report-url", help="URL of the weekly report with window configuration comparisons")
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

    card = _build_card(data, args.dashboard_url, args.weekly_report_url)

    try:
        send_lark(card, webhook_url=webhook_url, webhook_secret=webhook_secret)
        print(f"Lark notification sent: {args.dashboard_url}")
    except Exception as exc:
        print(f"Failed to send Lark notification: {exc}")
        return 1

    return 0


if __name__ == "__main__":
    raise SystemExit(cli())
