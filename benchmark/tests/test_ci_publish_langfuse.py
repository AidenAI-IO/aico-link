"""Tests for the concurrent, process-isolated Langfuse publication step.

These run real child processes: the step deadlocked in CI when runs shared one
process, which an in-process fake publisher could not reveal.
"""

from __future__ import annotations

import sys
import time
from pathlib import Path

from ci.publish_langfuse import (
    find_manifests,
    publish_command,
    publish_runs,
    summarize,
)


def _make_run(runs_dir: Path, name: str) -> Path:
    run_dir = runs_dir / name
    run_dir.mkdir(parents=True)
    (run_dir / "manifest.json").write_text("{}", encoding="utf-8")
    return run_dir


def _child(script: str):
    """A publisher stand-in; the script sees the run directory as argv[1]."""
    return lambda run_dir, _prefix: [sys.executable, "-c", script, str(run_dir)]


def _publish(run_dirs, script, **kwargs):
    lines: list[str] = []
    outcomes = publish_runs(run_dirs, command_for=_child(script), cwd=Path.cwd(), emit=lines.append, **kwargs)
    return outcomes, lines


def test_find_manifests_selects_only_this_attempt(tmp_path: Path):
    _make_run(tmp_path, "ci-100-1-memory")
    _make_run(tmp_path, "ci-100-1-shell")
    _make_run(tmp_path, "ci-101-1-memory")
    (tmp_path / "ci-100-1-broken").mkdir()

    found = find_manifests(tmp_path, "ci-100-1")

    assert [path.name for path in found] == ["ci-100-1-memory", "ci-100-1-shell"]


def test_find_manifests_tolerates_missing_runs_dir(tmp_path: Path):
    assert find_manifests(tmp_path / "absent", "ci-1-1") == []


def test_publish_command_runs_the_single_run_publisher_in_this_interpreter(tmp_path: Path):
    command = publish_command(tmp_path / "ci-1-1-memory", "aico-benchmark")

    assert command[:4] == [sys.executable, "-m", "runner", "publish-langfuse"]
    assert command[command.index("--run-dir") + 1] == str(tmp_path / "ci-1-1-memory")
    assert command[command.index("--dataset-prefix") + 1] == "aico-benchmark"


def test_publish_runs_reports_each_run_and_keeps_its_output_grouped(tmp_path: Path):
    runs = [_make_run(tmp_path, f"ci-1-1-suite{i}") for i in range(3)]

    outcomes, lines = _publish(runs, "import sys, pathlib; print('published', pathlib.Path(sys.argv[1]).name)")

    assert all(outcome.ok and outcome.returncode == 0 for outcome in outcomes)
    for run in runs:
        start = lines.index(next(line for line in lines if line.startswith(f"::group::publish {run.name}")))
        assert lines[start + 1] == f"published {run.name}"
        assert lines[start + 2] == "::endgroup::"


def test_a_failing_run_does_not_affect_its_siblings(tmp_path: Path):
    good = _make_run(tmp_path, "ci-1-1-good")
    bad = _make_run(tmp_path, "ci-1-1-bad")

    outcomes, lines = _publish(
        [good, bad],
        "import sys; bad = sys.argv[1].endswith('bad'); print('Error: unreachable' if bad else 'ok'); sys.exit(2 if bad else 0)",
    )

    assert [outcome.ok for outcome in outcomes] == [True, False]
    assert outcomes[1].returncode == 2 and outcomes[1].detail == "exit status 2"
    assert any("Langfuse publication failed for ci-1-1-bad" in line for line in lines)
    assert summarize(outcomes) == "Langfuse publication: 2 attempted, 1 succeeded, 1 failed"


def test_a_hung_run_is_killed_without_holding_the_others(tmp_path: Path):
    hung = _make_run(tmp_path, "ci-1-1-hung")
    quick = [_make_run(tmp_path, f"ci-1-1-quick{i}") for i in range(3)]

    started = time.monotonic()
    outcomes, lines = _publish(
        [hung, *quick],
        "import sys, time; time.sleep(60 if sys.argv[1].endswith('hung') else 0)",
        run_timeout_sec=1,
    )
    elapsed = time.monotonic() - started

    assert elapsed < 15, f"a hung publisher held the step for {elapsed:.0f}s"
    assert outcomes[0].timed_out and not outcomes[0].ok and outcomes[0].returncode is None
    assert all(outcome.ok for outcome in outcomes[1:])
    assert summarize(outcomes) == "Langfuse publication: 4 attempted, 3 succeeded, 1 failed (1 timed out)"
    assert any("killed after exceeding 1s" in line for line in lines)


def test_publish_runs_overlaps_processes_instead_of_serializing(tmp_path: Path):
    runs = [_make_run(tmp_path, f"ci-2-1-suite{i}") for i in range(4)]

    started = time.monotonic()
    outcomes, _ = _publish(runs, "import time; time.sleep(1.5)", max_parallel=4)
    elapsed = time.monotonic() - started

    assert all(outcome.ok for outcome in outcomes)
    assert elapsed < 4.5, f"four 1.5s publishes took {elapsed:.1f}s, so they ran serially"


def test_publish_runs_with_no_runs_is_a_no_op():
    assert publish_runs([], emit=lambda message: None) == []
    assert summarize([]) == "Langfuse publication: 0 attempted, 0 succeeded, 0 failed"
