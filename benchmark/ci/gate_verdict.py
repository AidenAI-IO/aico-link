"""Judge the baseline gate from the results of one or more benchmark passes.

ci.run_cases fails a job only for incomplete artifacts, never for a task
verdict. The gate adds the verdict: every gated task must pass. Agent and judge
calls are nondeterministic, so a task counts as passed when any pass over it
succeeded; the workflow reruns only the tasks that did not pass, and a task
fails the gate only when it never passes. A task without a result (setup
failure, killed case) counts as not passed.

A failed attempt whose model wrote its tool call as reply text instead of
calling the tool is recorded as "model_format". That is a known model output
defect (about 5% of attempts with google/gemini-3.5-flash-lite), not a change
in what the Agent decided, so a task that only ever fails that way is reported
as a warning instead of failing the gate.
"""

from __future__ import annotations

import argparse
import dataclasses as dc
import json
import re
import sys
from pathlib import Path

from ci.plan import (
    BENCHMARK_ROOT,
    DEFAULT_GATE_PATH,
    CatalogError,
    SuiteCase,
    load_catalog,
    load_gate,
)


# Tool-call markup a model emitted as plain reply text. Seen forms:
# <parcel_call><default_api:touch_gesture>, <call_default_api:recall_memory{...}/>
# <recall_memory>...</recall_memory> and recall_memory(tags=[...]). Tool names
# are snake_case, so a snake_case tag or call catches any tool without listing
# them. <final_answer> is the benchmark's answer format (runner/assertions.py).
TEXT_TOOL_CALL = re.compile(
    r"default_api:\w+"
    r"|<(?!final_answer\b)[a-z]+(?:_[a-z]+)+[\s>/{]"
    r"|\b[a-z]+(?:_[a-z]+)+\((?:\w+=|\))"
)


@dc.dataclass(frozen=True)
class TaskVerdict:
    case_id: str
    task_id: str
    # Statuses in pass order; "missing" when a pass has no result for the task.
    statuses: tuple[str, ...]

    @property
    def passed(self) -> bool:
        return "passed" in self.statuses

    @property
    def flaky(self) -> bool:
        return self.passed and self.statuses[0] != "passed"

    @property
    def kind(self) -> str:
        """Regression when a pass scored the task as failed; model_format when
        every unscored-or-failed pass was a text tool call; otherwise
        unverified (judge error, timeout, skipped or missing)."""
        if "failed" in self.statuses:
            return "regression"
        if self.statuses and all(status == "model_format" for status in self.statuses):
            return "model_format"
        return "unverified"

    @property
    def blocking(self) -> bool:
        return not self.passed and self.kind != "model_format"


def _wrote_tool_call_as_text(run_dir: Path, row: dict) -> bool:
    """True when the attempt made no tool call but its reply holds tool markup."""
    artifact = str(row.get("artifact_dir") or "")
    # artifact_dir is absolute on the runner; keep the part inside the run.
    marker = f"/{run_dir.name}/"
    relative = artifact.split(marker, 1)[1] if marker in artifact else f"tasks/{row['task_id']}"
    try:
        trace = json.loads((run_dir / relative / "trace.json").read_text(encoding="utf-8"))
    except (OSError, ValueError):
        return False
    if not isinstance(trace, dict) or trace.get("tool_calls"):
        return False
    return bool(TEXT_TOOL_CALL.search(str(trace.get("final_response") or "")))


def _task_statuses(run_dir: Path) -> dict[str, list[str]]:
    statuses: dict[str, list[str]] = {}
    try:
        lines = (run_dir / "results.jsonl").read_text(encoding="utf-8").splitlines()
    except OSError:
        return statuses
    for line in lines:
        try:
            row = json.loads(line)
        except json.JSONDecodeError:
            continue
        if not isinstance(row, dict) or not row.get("task_id"):
            continue
        status = str(row.get("status") or "unknown")
        if status == "failed" and _wrote_tool_call_as_text(run_dir, row):
            status = "model_format"
        statuses.setdefault(str(row["task_id"]), []).append(status)
    return statuses


def judge(
    cases: tuple[SuiteCase, ...],
    *,
    runs_dir: Path,
    run_id_prefixes: list[str],
) -> list[TaskVerdict]:
    verdicts: list[TaskVerdict] = []
    for case in cases:
        per_pass = [
            _task_statuses(runs_dir / f"{prefix}-{case.id}")
            for prefix in run_id_prefixes
        ]
        for task_id in case.task_ids:
            statuses: list[str] = []
            for results in per_pass:
                found = results.get(task_id)
                if found:
                    statuses.extend(found)
                elif "passed" not in statuses:
                    # A retry pass only holds the tasks that had not passed yet.
                    statuses.append("missing")
            verdicts.append(TaskVerdict(case.id, task_id, tuple(statuses)))
    return verdicts


def retry_matrix(cases: tuple[SuiteCase, ...], verdicts: list[TaskVerdict]) -> dict:
    failed: dict[str, list[str]] = {}
    for verdict in verdicts:
        if not verdict.passed:
            failed.setdefault(verdict.case_id, []).append(verdict.task_id)
    return {
        "include": [
            dc.replace(case, task_ids=failed[case.id]).matrix_entry()
            for case in cases
            if case.id in failed
        ]
    }


def summary_markdown(verdicts: list[TaskVerdict], *, final: bool) -> str:
    failed = [verdict for verdict in verdicts if not verdict.passed]
    flaky = [verdict for verdict in verdicts if verdict.flaky]
    passed = len(verdicts) - len(failed)
    title = "Baseline gate" if final else "Baseline gate (first pass)"
    lines = [f"## {title}", "", f"{passed}/{len(verdicts)} gated task(s) passed."]
    if failed:
        lines += ["", "| Case | Task | Kind | Results |", "| --- | --- | --- | --- |"]
        lines += [
            f"| {v.case_id} | {v.task_id} | {v.kind} | {', '.join(v.statuses)} |"
            for v in failed
        ]
        if any(v.kind == "model_format" for v in failed):
            lines += [
                "",
                "model_format: the model wrote its tool call as reply text instead of "
                "calling the tool. This is reported but does not fail the gate.",
            ]
        if all(v.kind == "unverified" for v in failed):
            lines += [
                "",
                "No gated task was scored as failed; the unpassed tasks never received "
                "a score. Check the judge and runner environment, then rerun.",
            ]
    if final and flaky:
        lines += ["", "Passed only on retry:", ""]
        lines += [f"- {v.case_id} / {v.task_id} ({', '.join(v.statuses)})" for v in flaky]
    return "\n".join(lines) + "\n"


def _append(path: str, text: str) -> None:
    if not path:
        return
    try:
        with open(path, "a", encoding="utf-8") as handle:
            handle.write(text)
    except OSError:
        pass


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Judge the benchmark baseline gate")
    parser.add_argument("--gate", type=Path, default=DEFAULT_GATE_PATH)
    parser.add_argument("--runs-dir", type=Path, default=BENCHMARK_ROOT / "runs")
    parser.add_argument(
        "--run-id-prefix",
        action="append",
        required=True,
        help="Run id prefix of one pass; repeat in pass order",
    )
    parser.add_argument(
        "--github-output",
        default="",
        help="Append failed=<count> and retry_matrix=<json> to this file",
    )
    parser.add_argument("--summary-path", default="")
    parser.add_argument(
        "--enforce",
        action="store_true",
        help="Exit 1 when any gated task never passed",
    )
    args = parser.parse_args(argv)
    try:
        cases = load_gate(load_catalog(), gate_path=args.gate)
    except CatalogError as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 2

    verdicts = judge(cases, runs_dir=args.runs_dir, run_id_prefixes=args.run_id_prefix)
    failed = [verdict for verdict in verdicts if not verdict.passed]
    blocking = [verdict for verdict in failed if verdict.blocking]
    summary = summary_markdown(verdicts, final=args.enforce)
    print(summary, flush=True)
    _append(args.summary_path, summary)
    matrix = retry_matrix(cases, verdicts)
    _append(
        args.github_output,
        f"failed={len(failed)}\nretry_matrix={json.dumps(matrix, separators=(',', ':'))}\n",
    )
    if args.enforce:
        for verdict in failed:
            level = "error" if verdict.blocking else "warning"
            print(
                f"::{level} title=Baseline gate {verdict.kind}::{verdict.case_id}/{verdict.task_id} "
                f"did not pass ({', '.join(verdict.statuses)})",
                flush=True,
            )
        if blocking:
            return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(cli())
