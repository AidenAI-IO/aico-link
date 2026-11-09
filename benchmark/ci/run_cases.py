"""Run several catalogued benchmark CI cases concurrently inside one job.

A matrix job per case pays the checkout, uv sync and Docker preflight cost once
per case and, with a single runner serving the label, still runs them one at a
time. Fanning out here collapses that setup to once per job and overlaps the
cases that are genuinely independent: run directories, container names and
published ports are all keyed by run id.

Cases backed by shared hardware cannot overlap, so they are serialised behind a
mutex while the container-backed ones run free.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import dataclasses as dc
import json
import os
import subprocess
import sys
import threading
import time
from pathlib import Path
from typing import Any, Mapping

from ci.plan import (
    BENCHMARK_ROOT,
    HARDWARE_ENVIRONMENTS,
    CatalogError,
    SuiteCase,
)
from ci.run_case import _incomplete_run_reasons, run_case

DEFAULT_MAX_PARALLEL = 2
LOG_TAIL_LINES = 40
# Set to "0" by the workflow when the shared AidenBridge install failed.
AIDENBRIDGE_READY_ENV = "AIDENBRIDGE_READY"


@dc.dataclass(frozen=True)
class CaseOutcome:
    case_id: str
    run_id: str
    returncode: int
    duration_seconds: float
    log_path: Path
    error_summary: str = ""

    @property
    def ok(self) -> bool:
        return self.returncode == 0


def _cases_from_matrix(matrix_json: str) -> tuple[SuiteCase, ...]:
    try:
        payload = json.loads(matrix_json)
    except json.JSONDecodeError as exc:
        raise CatalogError(f"matrix JSON is not valid: {exc}") from exc
    if not isinstance(payload, dict):
        raise CatalogError("matrix JSON must be an object")
    entries = payload.get("include")
    if not isinstance(entries, list) or not entries:
        raise CatalogError("matrix JSON has no include entries")
    cases: list[SuiteCase] = []
    for index, entry in enumerate(entries):
        if not isinstance(entry, dict):
            raise CatalogError(f"matrix entry {index} must be an object")
        try:
            cases.append(SuiteCase(**entry))
        except TypeError as exc:
            raise CatalogError(f"invalid matrix entry {index}: {exc}") from exc
    return tuple(cases)


def _log_tail(log_path: Path, lines: int = LOG_TAIL_LINES) -> str:
    try:
        content = log_path.read_text(encoding="utf-8", errors="replace")
    except OSError:
        return ""
    tail = content.splitlines()[-lines:]
    return "\n".join(tail)


def _record_setup_failure(
    case: SuiteCase, run_id: str, environment: Mapping[str, str], error: str,
) -> Path:
    """Keep a planned case visible using the normal, publishable run artifacts."""
    from runner.main import (
        _build_task_units, _planned_metrics_k, _planned_task_attempts,
        _result_totals, _write_suite_snapshot,
    )
    from runner.html_report import generate_report_html
    from runner.judge import JUDGE_PROMPT_VERSION, JudgeConfig
    from runner.report import git_sha, now_iso, write_jsonl, write_manifest, write_metrics, write_summary
    from runner.runtask import skipped_task_result
    from runner.suite import load_suite
    from ci.run_case import _runtime_environment

    runtime = _runtime_environment(environment)
    suite = load_suite(BENCHMARK_ROOT / case.suite)
    if case.task_ids:
        by_id = {task.id: task for task in suite.tasks}
        suite.tasks = [by_id[task_id] for task_id in case.task_ids]
    repeats = runtime.get("BENCHMARK_REPEATS", "").strip()
    repeat_count = int(repeats) if repeats else None
    if repeat_count is not None and repeat_count <= 0:
        raise ValueError("BENCHMARK_REPEATS must be a positive integer")
    units = _build_task_units(argparse.Namespace(repeats=repeat_count), suite, case.target_platform)
    run_dir = BENCHMARK_ROOT / "runs" / run_id
    # Match the normal runner's refusal to overwrite an existing measurement.
    run_dir.mkdir(parents=True, exist_ok=False)
    results = []
    for unit in units:
        result = skipped_task_result(
            suite, unit.task, unit.attempt,
            run_dir / "tasks" / unit.task.id / f"attempt_{unit.attempt}", run_id, error,
        )
        result.metrics["failure_class"] = "environment"
        results.append(result)
    sha, dirty = git_sha(BENCHMARK_ROOT.parent)
    judge = JudgeConfig()
    manifest = {
        "run_id": run_id, "git_sha": sha, "git_dirty": dirty,
        "git_branch": runtime.get("GITHUB_REF_NAME"),
        "suite_path": str(suite.source_path), "suite_sha256": suite.sha256,
        "suite_snapshot_path": _write_suite_snapshot(run_dir, suite),
        "selected_task_ids": case.task_ids,
        "target_platform": case.target_platform,
        "execution_environment": "adb_aidenbridge",
        "agent_model": runtime.get("AICO_BENCHMARK_AGENT_MODEL"),
        "judge_config": {"provider": judge.provider,
                         "model": runtime.get("AICO_BENCHMARK_JUDGE_MODEL", judge.model),
                         "base_url": runtime.get("AICO_BENCHMARK_JUDGE_BASE_URL", judge.base_url)},
        "judge_prompt_version": JUDGE_PROMPT_VERSION,
        "started_at": now_iso(), "finished_at": now_iso(),
        "totals": _result_totals(results), "metrics_schema_version": "p0-v1",
        "metrics_k": _planned_metrics_k(units),
        "planned_task_attempts": _planned_task_attempts(units),
    }
    write_manifest(run_dir / "manifest.json", manifest)
    write_jsonl(run_dir / "results.jsonl", results)
    write_metrics(run_dir / "metrics.json", suite.name, manifest, results)
    write_summary(run_dir / "summary.md", suite.name, manifest, results)
    (run_dir / "report.html").write_text(generate_report_html(run_dir), encoding="utf-8")
    log_path = run_dir / "ci-case.log"
    log_path.write_text(error + "\n", encoding="utf-8")
    return log_path


def run_cases(
    cases: tuple[SuiteCase, ...],
    *,
    run_id_prefix: str,
    environment: Mapping[str, str],
    max_parallel: int = DEFAULT_MAX_PARALLEL,
) -> list[CaseOutcome]:
    # Shared phones and Macs cannot serve two cases at once, so every
    # hardware-backed case takes this lock; container-backed ones never touch it.
    hardware_lock = threading.Lock()
    print_lock = threading.Lock()
    started = time.monotonic()

    def emit(message: str) -> None:
        with print_lock:
            elapsed = time.monotonic() - started
            print(f"[{elapsed:7.1f}s] {message}", flush=True)

    def execute(case: SuiteCase) -> CaseOutcome:
        run_id = f"{run_id_prefix}-{case.id}"
        if case.android_phone_bridge and environment.get(AIDENBRIDGE_READY_ENV) == "0":
            # The workflow installs AidenBridge once for every case that needs it.
            # Retain invalid setup attempts without attributing them to the Agent.
            summary = "aidenbridge_setup_failed"
            log_path = BENCHMARK_ROOT / "runs" / run_id / "ci-case.log"
            try:
                log_path = _record_setup_failure(case, run_id, environment, summary)
            except (OSError, ValueError, KeyError, RuntimeError) as exc:
                # Reporting errors must not prevent independent cases from running.
                summary += ", setup_report_failed"
                emit(f"ERROR   {case.id}: could not record setup failure: {exc}")
            with print_lock:
                print(f"::error title=Benchmark case {case.id}::{summary}", flush=True)
            emit(f"SKIP    {case.id}: AidenBridge device setup failed")
            return CaseOutcome(
                case_id=case.id,
                run_id=run_id,
                returncode=2,
                duration_seconds=0.0,
                log_path=log_path,
                error_summary=summary,
            )
        # Keep each case's staging directory private.  Cases execute in parallel,
        # so sharing a parent and removing it from each worker creates a race
        # between another worker's mkdir/open and rmdir.
        staged_log_path = (
            BENCHMARK_ROOT / "runs" / ".ci-case-logs" / run_id / "ci-case.log"
        )
        final_log_path = BENCHMARK_ROOT / "runs" / run_id / "ci-case.log"
        needs_hardware = case.environment in HARDWARE_ENVIRONMENTS
        if needs_hardware:
            emit(f"WAIT    {case.id} (hardware is exclusive)")
            hardware_lock.acquire()
        case_started = time.monotonic()
        emit(f"START   {case.id} ({case.environment}, {case.suite})")
        try:
            returncode = run_case(
                case,
                run_id=run_id,
                environment=environment,
                log_path=staged_log_path,
                timeout_seconds=case.timeout_minutes * 60,
            )
        except (RuntimeError, KeyError, OSError, subprocess.SubprocessError) as exc:
            # A case that cannot even start must not abort its siblings.
            returncode = 2
            try:
                staged_log_path.parent.mkdir(parents=True, exist_ok=True)
                with staged_log_path.open("a", encoding="utf-8") as handle:
                    handle.write(f"\nfailed to launch case: {exc}\n")
            except OSError:
                pass
            emit(f"ERROR   {case.id}: {exc}")
        finally:
            if needs_hardware:
                hardware_lock.release()
        outcome_log_path = staged_log_path
        if staged_log_path.is_file():
            try:
                final_log_path.parent.mkdir(parents=True, exist_ok=True)
                staged_log_path.replace(final_log_path)
                outcome_log_path = final_log_path
            except OSError as exc:
                emit(f"WARN    {case.id}: could not archive case log: {exc}")
        try:
            # Only remove this case's private staging directory; never remove
            # the shared `.ci-case-logs` parent while sibling cases run.
            staged_log_path.parent.rmdir()
        except OSError:
            pass
        duration = time.monotonic() - case_started
        error_summary = ""
        if returncode:
            reasons = _incomplete_run_reasons(
                returncode,
                BENCHMARK_ROOT / "runs" / run_id / "manifest.json",
            )
            error_summary = ", ".join(reasons) or f"runner_exit={returncode}"
            with print_lock:
                print(
                    f"::error title=Benchmark case {case.id}::{error_summary}",
                    flush=True,
                )
        verdict = "COMPLETE" if returncode == 0 else f"ERROR {error_summary}"
        emit(f"DONE    {case.id} {verdict} in {duration / 60:.1f}m")
        return CaseOutcome(
            case_id=case.id,
            run_id=run_id,
            returncode=returncode,
            duration_seconds=duration,
            log_path=outcome_log_path,
            error_summary=error_summary,
        )

    workers = max(1, min(max_parallel, len(cases)))
    emit(f"fan-out: {len(cases)} case(s), up to {workers} at a time")
    outcomes: list[CaseOutcome] = []
    with concurrent.futures.ThreadPoolExecutor(
        max_workers=workers, thread_name_prefix="bench-ci"
    ) as executor:
        execution_order = sorted(
            cases,
            key=lambda case: case.environment != "mobilegym",
        )
        futures = {executor.submit(execute, case): case for case in execution_order}
        for future in concurrent.futures.as_completed(futures):
            outcomes.append(future.result())
    order = {case.id: index for index, case in enumerate(cases)}
    outcomes.sort(key=lambda outcome: order[outcome.case_id])
    return outcomes


def _write_step_summary(outcomes: list[CaseOutcome]) -> None:
    summary_path = os.environ.get("GITHUB_STEP_SUMMARY", "")
    if not summary_path:
        return
    lines = ["## Benchmark cases", "", "| Case | Result | Duration |", "| --- | --- | --- |"]
    for outcome in outcomes:
        verdict = "complete" if outcome.ok else f"error ({outcome.error_summary})"
        lines.append(
            f"| {outcome.case_id} | {verdict} | {outcome.duration_seconds / 60:.1f}m |"
        )
    for outcome in outcomes:
        case_summary = BENCHMARK_ROOT / "runs" / outcome.run_id / "summary.md"
        if not case_summary.is_file():
            continue
        try:
            body = case_summary.read_text(encoding="utf-8").strip()
        except OSError:
            continue
        if body:
            lines.extend(["", f"<details><summary>{outcome.case_id}</summary>", "", body, "", "</details>"])
    try:
        with open(summary_path, "a", encoding="utf-8") as handle:
            handle.write("\n".join(lines) + "\n")
    except OSError:
        pass


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Run catalogued benchmark CI cases concurrently in one job"
    )
    parser.add_argument(
        "--matrix-json",
        required=True,
        help="Case selection emitted by plan.py matrix",
    )
    parser.add_argument(
        "--run-id-prefix",
        required=True,
        help="Run ids become <prefix>-<case id>",
    )
    parser.add_argument(
        "--max-parallel",
        type=int,
        default=DEFAULT_MAX_PARALLEL,
        help=f"Cases to run at once (default {DEFAULT_MAX_PARALLEL})",
    )
    args = parser.parse_args(argv)
    if args.max_parallel < 1:
        parser.error("--max-parallel must be positive")
    try:
        cases = _cases_from_matrix(args.matrix_json)
    except CatalogError as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 2

    outcomes = run_cases(
        cases,
        run_id_prefix=args.run_id_prefix,
        environment=os.environ,
        max_parallel=args.max_parallel,
    )
    _write_step_summary(outcomes)

    failed = [outcome for outcome in outcomes if not outcome.ok]
    print("", flush=True)
    print("=== benchmark case results ===", flush=True)
    for outcome in outcomes:
        verdict = "COMPLETE" if outcome.ok else f"ERROR {outcome.error_summary}"
        print(
            f"  {outcome.case_id:<28} {verdict:<12} {outcome.duration_seconds / 60:5.1f}m",
            flush=True,
        )
    for outcome in failed:
        tail = _log_tail(outcome.log_path)
        if not tail:
            continue
        # Surface enough context to triage without downloading the artifact.
        print(f"\n::group::{outcome.case_id} log tail", flush=True)
        print(tail, flush=True)
        print("::endgroup::", flush=True)
    if failed:
        names = ", ".join(outcome.case_id for outcome in failed)
        print(f"\n{len(failed)} of {len(outcomes)} case(s) did not complete: {names}", flush=True)
        return 1
    print(
        f"\nall {len(outcomes)} case(s) produced complete result artifacts",
        flush=True,
    )
    return 0


if __name__ == "__main__":
    raise SystemExit(cli())
