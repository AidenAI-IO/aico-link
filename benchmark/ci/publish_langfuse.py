"""Publish every completed run of one CI attempt to Langfuse.

Publication is a reporting side channel, so each run is independent: a run that
fails to publish must not block its siblings, and the whole step must not fail
the job. Runs are published concurrently because the cost is per-run network
round trips against Langfuse, not local work. Publishing them one at a time made
this step the second longest in a scheduled run after the cases themselves.
"""

from __future__ import annotations

import argparse
import concurrent.futures
import dataclasses as dc
import threading
import time
from pathlib import Path


DEFAULT_MAX_PARALLEL = 4


@dc.dataclass(frozen=True)
class PublishOutcome:
    run_dir: Path
    ok: bool
    already_exists: bool = False
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


def publish_runs(
    run_dirs: list[Path],
    *,
    dataset_prefix: str = "aico-benchmark",
    max_parallel: int = DEFAULT_MAX_PARALLEL,
    publisher,
    emit,
) -> list[PublishOutcome]:
    """Publish each run concurrently, isolating failures per run."""
    if not run_dirs:
        return []
    print_lock = threading.Lock()

    def publish_one(run_dir: Path) -> PublishOutcome:
        started = time.monotonic()
        with print_lock:
            emit(f"::group::publish {run_dir.name}")
        try:
            published = publisher(run_dir, dataset_prefix)
        except Exception as exc:  # one run's failure must not cancel the rest
            elapsed = time.monotonic() - started
            with print_lock:
                emit(
                    f"::warning::Langfuse publication failed for {run_dir.name} "
                    f"after {elapsed:.0f}s: {exc}"
                )
                emit("::endgroup::")
            return PublishOutcome(run_dir=run_dir, ok=False, detail=str(exc))
        elapsed = time.monotonic() - started
        with print_lock:
            emit(
                f"published {run_dir.name} items={published.item_count} "
                f"in {elapsed:.0f}s"
            )
            emit("::endgroup::")
        return PublishOutcome(
            run_dir=run_dir,
            ok=True,
            already_exists=bool(published.already_exists),
        )

    workers = max(1, min(max_parallel, len(run_dirs)))
    emit(f"publishing {len(run_dirs)} run(s), up to {workers} at a time")
    with concurrent.futures.ThreadPoolExecutor(
        max_workers=workers, thread_name_prefix="langfuse-publish"
    ) as executor:
        outcomes = list(executor.map(publish_one, run_dirs))
    return outcomes


def summarize(outcomes: list[PublishOutcome]) -> str:
    attempted = len(outcomes)
    succeeded = sum(1 for outcome in outcomes if outcome.ok)
    failed = attempted - succeeded
    return (
        f"Langfuse publication: {attempted} attempted, "
        f"{succeeded} succeeded, {failed} failed"
    )


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Publish CI benchmark runs to Langfuse")
    parser.add_argument("--runs-dir", required=True)
    parser.add_argument("--run-id-prefix", required=True)
    parser.add_argument("--dataset-prefix", default="aico-benchmark")
    parser.add_argument("--max-parallel", type=int, default=DEFAULT_MAX_PARALLEL)
    parser.add_argument(
        "--summary-path",
        default="",
        help="Also append the publication summary to this file (GITHUB_STEP_SUMMARY).",
    )
    args = parser.parse_args(argv)

    from runner.langfuse_reporter import publish_run

    def publisher(run_dir: Path, dataset_prefix: str):
        return publish_run(run_dir, dataset_prefix=dataset_prefix)

    run_dirs = find_manifests(Path(args.runs_dir), args.run_id_prefix)
    outcomes = publish_runs(
        run_dirs,
        dataset_prefix=args.dataset_prefix,
        max_parallel=args.max_parallel,
        publisher=publisher,
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
