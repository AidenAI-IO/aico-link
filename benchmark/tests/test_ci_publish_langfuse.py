"""Tests for the concurrent Langfuse publication step."""

from __future__ import annotations

import dataclasses as dc
import time
from pathlib import Path

import pytest

from ci.publish_langfuse import (
    PublishOutcome,
    find_manifests,
    publish_runs,
    summarize,
)


@dc.dataclass
class _Published:
    item_count: int = 3
    already_exists: bool = False


def _make_run(runs_dir: Path, name: str) -> Path:
    run_dir = runs_dir / name
    run_dir.mkdir(parents=True)
    (run_dir / "manifest.json").write_text("{}", encoding="utf-8")
    return run_dir


def test_find_manifests_selects_only_this_attempt(tmp_path: Path):
    _make_run(tmp_path, "ci-100-1-memory")
    _make_run(tmp_path, "ci-100-1-shell")
    _make_run(tmp_path, "ci-101-1-memory")
    (tmp_path / "ci-100-1-broken").mkdir()

    found = find_manifests(tmp_path, "ci-100-1")

    assert [path.name for path in found] == ["ci-100-1-memory", "ci-100-1-shell"]


def test_find_manifests_tolerates_missing_runs_dir(tmp_path: Path):
    assert find_manifests(tmp_path / "absent", "ci-1-1") == []


def test_publish_runs_isolates_individual_failures(tmp_path: Path):
    good = _make_run(tmp_path, "ci-1-1-good")
    bad = _make_run(tmp_path, "ci-1-1-bad")

    def publisher(run_dir: Path, _prefix: str):
        if run_dir.name.endswith("bad"):
            raise RuntimeError("langfuse unreachable")
        return _Published()

    outcomes = publish_runs(
        [good, bad],
        publisher=publisher,
        emit=lambda message: None,
    )

    assert [outcome.ok for outcome in outcomes] == [True, False]
    assert outcomes[1].detail == "langfuse unreachable"
    assert summarize(outcomes) == "Langfuse publication: 2 attempted, 1 succeeded, 1 failed"


def test_publish_runs_overlaps_instead_of_serializing(tmp_path: Path):
    runs = [_make_run(tmp_path, f"ci-2-1-suite{i}") for i in range(4)]
    shared = {"active": 0, "peak": 0}

    def publisher(_run_dir: Path, _prefix: str):
        shared["active"] += 1
        shared["peak"] = max(shared["peak"], shared["active"])
        time.sleep(0.05)
        shared["active"] -= 1
        return _Published()

    outcomes = publish_runs(
        runs,
        max_parallel=4,
        publisher=publisher,
        emit=lambda message: None,
    )

    assert all(outcome.ok for outcome in outcomes)
    assert shared["peak"] > 1, "publication ran serially"


def test_publish_runs_with_no_runs_is_a_no_op():
    assert publish_runs([], publisher=lambda *_: pytest.fail("no work"), emit=lambda message: None) == []
    assert summarize([]) == "Langfuse publication: 0 attempted, 0 succeeded, 0 failed"