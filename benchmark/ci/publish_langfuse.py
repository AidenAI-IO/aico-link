"""Publish every completed run of one CI attempt to Langfuse.

Publication is a reporting side channel, so each run is independent: a run that
fails to publish must not block its siblings, and the whole step must not fail
the job. Runs are published concurrently because the cost is per-run network
round trips against Langfuse, not local work. Publishing them one at a time made
this step the second longest in a scheduled run after the cases themselves.

Each run is published by its own ``python -m runner publish-langfuse`` process.
The Langfuse SDK keeps one resource manager per public key for the whole
process, and ``publish_run`` shuts its client down when it finishes; with
several runs in one process the first shutdown stops the shared upload
consumers and every other run then waits forever in ``flush()``. Separate
processes also let a hard per-run timeout kill a stuck publish so the step can
never hold the runner.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import dataclasses as dc
import subprocess
import sys
import tempfile
import threading
import time
from pathlib import Path
from typing import Callable

from ci.plan import BENCHMARK_ROOT


DEFAULT_MAX_PARALLEL = 4
# One run normally publishes in well under a minute; anything near this bound
# is a stalled connection or a hung SDK flush, not useful work.
DEFAULT_RUN_TIMEOUT_SEC = 300
MAX_LOGGED_OUTPUT_CHARS = 20_000


@dc.dataclass(frozen=True)
class PublishOutcome:
    run_dir: Path
    ok: bool
    returncode: int | None = None
    timed_out: bool = False
    elapsed_sec: float = 0.0
    detail: str = ""


def find_manifests(runs_dir: Path, run_id_prefix: str) -> list[Path]:
    """Return the run directories of this attempt, sorted for stable output."""
    if not runs_dir.is_dir():
        return []
    manifests = [
        path.parent
        for path in runs_dir.glob(f"{run_id_prefix}-*/manifest.json")
        if path.is_file()
    ]
    return sorted(manifests)


def publish_command(run_dir: Path, dataset_prefix: str) -> list[str]:
    """The single-run publisher, run with this interpreter and environment."""
    return [
        sys.executable, "-m", "runner", "publish-langfuse",
        "--run-dir", str(run_dir),
        "--dataset-prefix", dataset_prefix,
    ]


def _run_isolated(command: list[str], *, timeout_sec: float, cwd: Path) -> tuple[int | None, bool, str]:
    """Run one publisher process; kill it if it outlives ``timeout_sec``.

    Output goes to a temporary file rather than a pipe, so a child that stops
    reading or writing can never block this thread, and concurrent runs do not
    interleave their logs.
    """
    with tempfile.TemporaryFile() as output:
        process = subprocess.Popen(
            command,
            cwd=cwd,
            stdin=subprocess.DEVNULL,
            stdout=output,
            stderr=subprocess.STDOUT,
        )
        timed_out = False
        try:
            returncode: int | None = process.wait(timeout=timeout_sec)
        except subprocess.TimeoutExpired:
            timed_out = True
            process.kill()
            process.wait()
            returncode = None
        output.seek(0)
        text = output.read().decode("utf-8", errors="replace")
    if len(text) > MAX_LOGGED_OUTPUT_CHARS:
        text = "[earlier output truncated]\n" + text[-MAX_LOGGED_OUTPUT_CHARS:]
    return returncode, timed_out, text


def publish_runs(
    run_dirs: list[Path],
    *,
    dataset_prefix: str = "aico-benchmark",
    max_parallel: int = DEFAULT_MAX_PARALLEL,
    run_timeout_sec: float = DEFAULT_RUN_TIMEOUT_SEC,
    command_for: Callable[[Path, str], list[str]] = publish_command,
    cwd: Path = BENCHMARK_ROOT,
    emit: Callable[[str], None],
) -> list[PublishOutcome]:
    """Publish each run in its own process, concurrently and with a time bound."""
    if not run_dirs:
        return []
    print_lock = threading.Lock()

    def publish_one(run_dir: Path) -> PublishOutcome:
        started = time.monotonic()
        try:
            returncode, timed_out, output = _run_isolated(
                command_for(run_dir, dataset_prefix), timeout_sec=run_timeout_sec, cwd=cwd,
            )
        except OSError as exc:  # the process could not even start
            returncode, timed_out, output = None, False, f"could not start publisher: {exc}"
        elapsed = time.monotonic() - started
        ok = returncode == 0 and not timed_out
        if timed_out:
            detail = f"killed after exceeding {run_timeout_sec:.0f}s"
        elif not ok:
            detail = f"exit status {returncode}" if returncode is not None else output.strip()
        else:
            detail = ""
        with print_lock:
            emit(f"::group::publish {run_dir.name} ({elapsed:.0f}s)")
            if output.strip():
                emit(output.rstrip())
            emit("::endgroup::")
            if not ok:
                emit(f"::warning::Langfuse publication failed for {run_dir.name}: {detail}")
        return PublishOutcome(
            run_dir=run_dir,
            ok=ok,
            returncode=returncode,
            timed_out=timed_out,
            elapsed_sec=elapsed,
            detail=detail,
        )

    workers = max(1, min(max_parallel, len(run_dirs)))
    emit(
        f"publishing {len(run_dirs)} run(s), up to {workers} at a time, "
        f"{run_timeout_sec:.0f}s limit per run"
    )
    with concurrent.futures.ThreadPoolExecutor(
        max_workers=workers, thread_name_prefix="langfuse-publish"
    ) as executor:
        return list(executor.map(publish_one, run_dirs))


def summarize(outcomes: list[PublishOutcome]) -> str:
    attempted = len(outcomes)
    succeeded = sum(1 for outcome in outcomes if outcome.ok)
    timed_out = sum(1 for outcome in outcomes if outcome.timed_out)
    failed = attempted - succeeded
    summary = (
        f"Langfuse publication: {attempted} attempted, "
        f"{succeeded} succeeded, {failed} failed"
    )
    if timed_out:
        summary += f" ({timed_out} timed out)"
    return summary


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Publish CI benchmark runs to Langfuse")
    parser.add_argument("--runs-dir", required=True)
    parser.add_argument("--run-id-prefix", required=True)
    parser.add_argument("--dataset-prefix", default="aico-benchmark")
    parser.add_argument("--max-parallel", type=int, default=DEFAULT_MAX_PARALLEL)
    parser.add_argument(
        "--run-timeout-sec",
        type=float,
        default=DEFAULT_RUN_TIMEOUT_SEC,
        help="Kill a single run's publisher after this many seconds.",
    )
    parser.add_argument(
        "--summary-path",
        default="",
        help="Also append the publication summary to this file (GITHUB_STEP_SUMMARY).",
    )
    args = parser.parse_args(argv)
    if args.run_timeout_sec <= 0:
        parser.error("--run-timeout-sec must be positive")

    run_dirs = find_manifests(Path(args.runs_dir).resolve(), args.run_id_prefix)
    outcomes = publish_runs(
        run_dirs,
        dataset_prefix=args.dataset_prefix,
        max_parallel=args.max_parallel,
        run_timeout_sec=args.run_timeout_sec,
        emit=lambda message: print(message, flush=True),
    )
    if not run_dirs:
        print(
            f"::warning::No completed manifest under {args.run_id_prefix}-*; "
            "skipping Langfuse publication",
            flush=True,
        )
    summary = summarize(outcomes)
    print(f"::notice::{summary}", flush=True)
    if args.summary_path:
        try:
            with open(args.summary_path, "a", encoding="utf-8") as handle:
                handle.write(f"{summary}\n")
        except OSError as exc:
            print(f"::warning::could not write step summary: {exc}", flush=True)
    return 0


if __name__ == "__main__":
    raise SystemExit(cli())
