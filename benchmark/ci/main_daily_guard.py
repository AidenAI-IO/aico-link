"""Select, gate and report the must-pass main daily guard.

The guard is a small hand-picked set of tasks that must pass on every attempt
after each day's merges to main. main_daily_guard.json names each task set and
the catalog case it reuses, so the nightly benchmark and its scoring contract
stay unchanged. A failed, excluded or missing attempt fails the guard: an
environment that cannot run a must-pass task is as much a regression as an
Agent that fails it.
"""

from __future__ import annotations

import argparse
import dataclasses as dc
import json
import os
import sys
import time
import urllib.parse
import urllib.request
from pathlib import Path
from typing import Any

from ci.plan import BENCHMARK_ROOT, CatalogError, SuiteCase, load_catalog


DEFAULT_GUARD_PATH = Path(__file__).with_name("main_daily_guard.json")
# The guard runs unattended on the benchmark runner: only the configured ADB
# emulator and isolated workers are always available there.
GUARD_ENVIRONMENTS = {"adb", "isolated"}
MAX_ALERT_COMMITS = 15


def load_guard(
    *,
    guard_path: Path = DEFAULT_GUARD_PATH,
    catalog_cases: tuple[SuiteCase, ...] | None = None,
    benchmark_root: Path | None = None,
) -> tuple[SuiteCase, ...]:
    """Return one runnable case per guard entry, derived from its catalog case."""
    benchmark_root = benchmark_root or BENCHMARK_ROOT
    try:
        raw = json.loads(guard_path.read_text(encoding="utf-8"))
    except (OSError, json.JSONDecodeError) as exc:
        raise CatalogError(f"cannot read guard {guard_path}: {exc}") from exc
    if not isinstance(raw, dict) or raw.get("version") != 1:
        raise CatalogError("guard version must be 1")
    entries = raw.get("cases")
    if not isinstance(entries, list) or not entries:
        raise CatalogError("guard cases must be a non-empty array")

    by_id = {case.id: case for case in (catalog_cases if catalog_cases is not None else load_catalog().cases)}
    cases: list[SuiteCase] = []
    for index, entry in enumerate(entries):
        if not isinstance(entry, dict) or set(entry) != {"id", "source", "task_ids"}:
            raise CatalogError(f"guard case {index} must have exactly id, source and task_ids")
        case_id, source_id, task_ids = entry["id"], entry["source"], entry["task_ids"]
        if not isinstance(case_id, str) or not case_id.replace("-", "").replace("_", "").isalnum():
            raise CatalogError(f"invalid guard case id: {case_id!r}")
        if case_id in by_id:
            # A shared id would merge the guard into the catalog case's history.
            raise CatalogError(f"guard case {case_id} reuses a catalog case id")
        source = by_id.get(source_id)
        if source is None:
            raise CatalogError(f"guard case {case_id} references unknown catalog case {source_id!r}")
        if source.environment not in GUARD_ENVIRONMENTS or source.android_phone_bridge:
            raise CatalogError(f"guard case {case_id} must reuse a plain ADB or isolated case")
        if source.environment == "adb" and source.target_platform != "android":
            raise CatalogError(f"guard case {case_id} must reuse an Android ADB case")
        if (not isinstance(task_ids, list) or not task_ids
                or not all(isinstance(task_id, str) and task_id for task_id in task_ids)
                or len(set(task_ids)) != len(task_ids)):
            raise CatalogError(f"guard case {case_id} needs unique, non-empty task_ids")
        tasks = _suite_tasks(benchmark_root / source.suite)
        for task_id in task_ids:
            if task_id not in tasks:
                raise CatalogError(f"unknown task {task_id!r} for guard case {case_id}")
            platforms = tasks[task_id].get("platforms") or []
            if platforms and source.target_platform != "auto" and source.target_platform not in platforms:
                raise CatalogError(f"task {task_id!r} does not support {source.target_platform} for {case_id}")
        cases.append(dc.replace(source, id=case_id, task_ids=list(task_ids), runnable=False))

    ids = [case.id for case in cases]
    duplicates = sorted({case_id for case_id in ids if ids.count(case_id) > 1})
    if duplicates:
        raise CatalogError(f"duplicate guard case ids: {', '.join(duplicates)}")
    return tuple(cases)


def _suite_tasks(suite_path: Path) -> dict[str, dict[str, Any]]:
    try:
        tasks = json.loads(suite_path.read_text(encoding="utf-8"))["tasks"]
        return {task["id"]: task for task in tasks}
    except (OSError, ValueError, KeyError, TypeError) as exc:
        raise CatalogError(f"cannot read tasks from {suite_path}: {exc}") from exc


def _read_rows(results_path: Path) -> list[dict[str, Any]]:
    rows = []
    for line in results_path.read_text(encoding="utf-8").splitlines():
        if line.strip():
            row = json.loads(line)
            if isinstance(row, dict):
                rows.append(row)
    return rows


def evaluate_case(case: dict[str, Any], run_dir: Path, *, repeats: int | None,
                  benchmark_root: Path | None = None) -> dict[str, Any]:
    """Require every expected attempt of every guard task to have passed."""
    from ci.failure_details import normalize_failure
    from runner.metrics import status_fields

    benchmark_root = benchmark_root or BENCHMARK_ROOT
    tasks = _suite_tasks(benchmark_root / case["suite"])
    expected = {task_id: repeats or max(1, int(tasks[task_id].get("repeats") or 1))
                for task_id in case["task_ids"]}
    verdict: dict[str, Any] = {"case": case["id"], "run_id": run_dir.name,
                               "expected_attempts": sum(expected.values()),
                               "passed_attempts": 0, "failures": []}
    try:
        rows = _read_rows(run_dir / "results.jsonl")
    except (OSError, ValueError) as exc:
        verdict.update(status="fail", reason=f"no readable results.jsonl: {exc}")
        return verdict

    by_attempt: dict[tuple[str, int], dict[str, Any]] = {}
    for row in rows:
        try:
            by_attempt[(str(row.get("task_id") or ""), int(row.get("attempt") or 1))] = row
        except (TypeError, ValueError):
            continue
    for task_id, attempts in expected.items():
        for attempt in range(1, attempts + 1):
            row = by_attempt.get((task_id, attempt))
            if row is None:
                verdict["failures"].append({"task_id": task_id, "attempt": attempt, "status": "missing",
                                            "failure_class": None, "reason": "attempt did not run"})
                continue
            success, failure_class, _ = status_fields(row)
            if success is True:
                verdict["passed_attempts"] += 1
                continue
            detail = normalize_failure(row, row) or {}
            reasons = [item.get("reason") or item.get("label") for item in detail.get("evidence") or []]
            verdict["failures"].append({
                "task_id": task_id, "attempt": attempt, "status": row.get("status"),
                "failure_class": failure_class,
                "reason": "; ".join(reason for reason in reasons if reason)[:300],
            })
    verdict["status"] = "fail" if verdict["failures"] else "pass"
    verdict["reason"] = f"{len(verdict['failures'])} attempt(s) did not pass" if verdict["failures"] else ""
    return verdict


def render_summary(verdicts: list[dict[str, Any]]) -> str:
    lines = ["## Main daily guard", "",
             "Every attempt of every listed task must pass.", "",
             "| Case | Result | Passed attempts | Detail |", "| --- | --- | --- | --- |"]
    for verdict in verdicts:
        icon = "✅ pass" if verdict["status"] == "pass" else "❌ fail"
        lines.append(f"| {verdict['case']} | {icon} | {verdict['passed_attempts']}/"
                     f"{verdict['expected_attempts']} | {verdict['reason']} |")
    failures = [(verdict["case"], failure) for verdict in verdicts for failure in verdict["failures"]]
    if failures:
        lines += ["", "| Case | Task | Attempt | Status | Class | Reason |",
                  "| --- | --- | --- | --- | --- | --- |"]
        for case_id, failure in failures:
            reason = (failure["reason"] or "").replace("|", "\\|").replace("\n", " ")
            lines.append(f"| {case_id} | {failure['task_id']} | {failure['attempt']} | {failure['status']} "
                         f"| {failure['failure_class'] or ''} | {reason} |")
    return "\n".join(lines) + "\n"


def _github_get(api_url: str, path: str, token: str, **query: Any) -> Any:
    url = f"{api_url}{path}" + (f"?{urllib.parse.urlencode(query)}" if query else "")
    request = urllib.request.Request(url, headers={
        "Accept": "application/vnd.github+json",
        "Authorization": f"Bearer {token}",
        "X-GitHub-Api-Version": "2022-11-28",
    })
    with urllib.request.urlopen(request, timeout=20) as response:
        return json.load(response)


def commits_since_last_pass(*, api_url: str, repo: str, run_id: str, head_sha: str,
                            token: str, branch: str = "main") -> dict[str, Any]:
    """List commits between this workflow's last successful main run and head_sha."""
    run = _github_get(api_url, f"/repos/{repo}/actions/runs/{run_id}", token)
    runs = _github_get(api_url, f"/repos/{repo}/actions/workflows/{run['workflow_id']}/runs", token,
                       branch=branch, status="success", per_page=10)
    previous = next((item for item in runs.get("workflow_runs") or [] if str(item.get("id")) != str(run_id)), None)
    if previous is None:
        return {"base_sha": None, "commits": [], "total": 0}
    base_sha = previous["head_sha"]
    if base_sha == head_sha:
        return {"base_sha": base_sha, "commits": [], "total": 0}
    compare = _github_get(api_url, f"/repos/{repo}/compare/{base_sha}...{head_sha}", token)
    commits = [{
        "sha": item["sha"],
        "title": (item.get("commit", {}).get("message") or "").splitlines()[0] if item.get("commit") else "",
        "author": (item.get("author") or {}).get("login") or item.get("commit", {}).get("author", {}).get("name", ""),
    } for item in compare.get("commits") or []]
    # The compare API lists commits oldest first; the newest merges matter most.
    commits.reverse()
    return {"base_sha": base_sha, "commits": commits, "total": compare.get("total_commits", len(commits))}


def build_card(verdict: dict[str, Any], *, run_url: str, cases_outcome: str, head_sha: str,
               server_url: str, repo: str, history: dict[str, Any] | None) -> dict[str, Any]:
    commit_url = f"{server_url}/{repo}/commit"
    lines = [f"Commit: [{head_sha[:8]}]({commit_url}/{head_sha})" if head_sha else "Commit: unknown",
             f"Case runner: **{cases_outcome or 'not run'}**"]
    for case in verdict.get("cases", []):
        if case["status"] == "pass":
            continue
        lines.append(f"**{case['case']}** {case['passed_attempts']}/{case['expected_attempts']} passed"
                     + (f": {case['reason']}" if case["reason"] and not case["failures"] else ""))
        for failure in case["failures"][:10]:
            lines.append(f"- {failure['task_id']} #{failure['attempt']} {failure['status']}"
                         + (f" ({failure['failure_class']})" if failure["failure_class"] else "")
                         + (f": {failure['reason'][:120]}" if failure["reason"] else ""))
    lines.append("")
    if history is None:
        lines.append("Commits since the last passing run: unavailable")
    elif history["base_sha"] is None:
        lines.append("Commits since the last passing run: no earlier passing run on main")
    elif not history["commits"]:
        lines.append(f"No new commits since the last passing run ([{history['base_sha'][:8]}]"
                     f"({commit_url}/{history['base_sha']})); suspect the environment or flakiness")
    else:
        lines.append(f"Commits since the last passing run ([{history['base_sha'][:8]}]"
                     f"({commit_url}/{history['base_sha']})), newest first:")
        for commit in history["commits"][:MAX_ALERT_COMMITS]:
            title = commit["title"].replace("[", "(").replace("]", ")")
            lines.append(f"- [{commit['sha'][:8]}]({commit_url}/{commit['sha']}) {title}"
                         + (f" — {commit['author']}" if commit["author"] else ""))
        if history["total"] > MAX_ALERT_COMMITS:
            lines.append(f"- … and {history['total'] - MAX_ALERT_COMMITS} more")
    return {
        "header": {"template": "red",
                   "title": {"tag": "plain_text", "content": "Main daily guard failed"}},
        "elements": [
            {"tag": "markdown", "content": "\n".join(lines)},
            {"tag": "action", "actions": [{
                "tag": "button", "type": "primary", "url": run_url,
                "text": {"tag": "plain_text", "content": "Open workflow run"},
            }]},
        ],
    }


def send_card(card: dict[str, Any], *, webhook_url: str, webhook_secret: str = "") -> None:
    """Post a card to a Lark custom bot, signing it when the bot requires a secret."""
    if webhook_secret:
        from ci.notify_lark import send_lark

        send_lark(card, webhook_url=webhook_url, webhook_secret=webhook_secret)
        return
    request = urllib.request.Request(
        webhook_url,
        data=json.dumps({"msg_type": "interactive", "card": card}).encode("utf-8"),
        headers={"Content-Type": "application/json"},
    )
    with urllib.request.urlopen(request, timeout=10) as response:
        result = json.load(response)
    if result.get("code", 0) != 0:
        raise RuntimeError(f"Lark rejected the message: code {result.get('code')}, msg {result.get('msg')}")


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Main daily guard")
    subparsers = parser.add_subparsers(dest="command", required=True)
    subparsers.add_parser("matrix", help="Print the guard case matrix")
    verdict_parser = subparsers.add_parser("verdict", help="Check the guard runs")
    verdict_parser.add_argument("--matrix-json", required=True)
    verdict_parser.add_argument("--runs-dir", type=Path, required=True)
    verdict_parser.add_argument("--run-id-prefix", required=True)
    verdict_parser.add_argument("--repeats", type=int, default=None)
    verdict_parser.add_argument("--output", type=Path, required=True)
    notify_parser = subparsers.add_parser("notify", help="Send a failure card to Lark")
    notify_parser.add_argument("--verdict-json", type=Path, required=True)
    notify_parser.add_argument("--cases-outcome", default="")
    args = parser.parse_args(argv)

    if args.command == "matrix":
        try:
            cases = load_guard()
        except CatalogError as exc:
            parser.error(str(exc))
        print(json.dumps({"include": [case.matrix_entry() for case in cases]}, separators=(",", ":")))
        return 0

    if args.command == "verdict":
        cases = json.loads(args.matrix_json)["include"]
        verdicts = [evaluate_case(case, args.runs_dir / f"{args.run_id_prefix}-{case['id']}",
                                  repeats=args.repeats)
                    for case in cases]
        status = "fail" if any(verdict["status"] == "fail" for verdict in verdicts) else "pass"
        args.output.write_text(json.dumps({"status": status, "cases": verdicts}, indent=2), encoding="utf-8")
        summary = render_summary(verdicts)
        print(summary)
        if os.environ.get("GITHUB_STEP_SUMMARY"):
            with open(os.environ["GITHUB_STEP_SUMMARY"], "a", encoding="utf-8") as handle:
                handle.write(summary)
        if os.environ.get("GITHUB_OUTPUT"):
            with open(os.environ["GITHUB_OUTPUT"], "a", encoding="utf-8") as handle:
                handle.write(f"status={status}\n")
        return 0

    webhook_url = os.environ.get("MAIN_DAILY_GUARD_LARK_WEBHOOK_URL")
    if not webhook_url:
        print("MAIN_DAILY_GUARD_LARK_WEBHOOK_URL not set; skipping alert")
        return 0
    try:
        verdict = json.loads(args.verdict_json.read_text(encoding="utf-8"))
    except (OSError, ValueError):
        verdict = {"status": "fail", "cases": []}
    server_url = os.environ.get("GITHUB_SERVER_URL", "https://github.com")
    repo = os.environ.get("GITHUB_REPOSITORY", "")
    run_id = os.environ.get("GITHUB_RUN_ID", "")
    head_sha = os.environ.get("GITHUB_SHA", "")
    history = None
    try:
        history = commits_since_last_pass(
            api_url=os.environ.get("GITHUB_API_URL", "https://api.github.com"), repo=repo,
            run_id=run_id, head_sha=head_sha, token=os.environ["GITHUB_TOKEN"])
    except Exception as exc:
        # The alert must still go out when the commit lookup fails.
        print(f"Cannot list commits since the last passing run: {exc}", file=sys.stderr)
    card = build_card(verdict, run_url=f"{server_url}/{repo}/actions/runs/{run_id}",
                      cases_outcome=args.cases_outcome, head_sha=head_sha,
                      server_url=server_url, repo=repo, history=history)
    for attempt in range(3):
        try:
            send_card(card, webhook_url=webhook_url,
                      webhook_secret=os.environ.get("MAIN_DAILY_GUARD_LARK_WEBHOOK_SECRET", ""))
            print("Lark alert sent")
            return 0
        except Exception as exc:
            print(f"Failed to send Lark alert (attempt {attempt + 1}): {exc}", file=sys.stderr)
            if attempt < 2:
                time.sleep(2 * (attempt + 1))
    return 1


if __name__ == "__main__":
    raise SystemExit(cli())
