import os
import time
from pathlib import Path

from ci.publish_reports import publish_reports


def _write_report(runs_dir: Path, run_id: str, html: str) -> None:
    run_dir = runs_dir / run_id
    run_dir.mkdir(parents=True)
    (run_dir / "report.html").write_text(html, encoding="utf-8")


def test_publish_reports_preserves_html_and_links_each_suite(tmp_path: Path) -> None:
    runs_dir = tmp_path / "runs"
    publish_dir = tmp_path / "published"
    summary_path = tmp_path / "summary.md"
    prefix = "ci-123-1"
    first_html = "<html><script>window.reportDrawer = true</script></html>"
    second_html = "<html><button>open details</button></html>"
    _write_report(runs_dir, f"{prefix}-memory", first_html)
    _write_report(runs_dir, f"{prefix}-mobilegym-basic", second_html)

    published = publish_reports(
        runs_dir=runs_dir,
        run_id_prefix=prefix,
        publish_dir=publish_dir,
        base_url="https://reports.example.com/benchmarks/",
        summary_path=summary_path,
        artifact_url="https://github.example/artifact/123",
    )

    assert published == 2
    assert (publish_dir / prefix / "memory" / "report.html").read_text(
        encoding="utf-8"
    ) == first_html
    assert (publish_dir / prefix / "mobilegym-basic" / "report.html").read_text(
        encoding="utf-8"
    ) == second_html
    summary = summary_path.read_text(encoding="utf-8")
    assert (
        "[memory](https://reports.example.com/benchmarks/ci-123-1/memory/report.html)"
        in summary
    )
    assert "[Download all artifacts](https://github.example/artifact/123)" in summary


def test_publish_reports_uses_artifact_link_without_static_host(tmp_path: Path) -> None:
    runs_dir = tmp_path / "runs"
    summary_path = tmp_path / "summary.md"
    _write_report(runs_dir, "ci-123-1-memory", "<html></html>")

    published = publish_reports(
        runs_dir=runs_dir,
        run_id_prefix="ci-123-1",
        summary_path=summary_path,
        artifact_url="https://github.example/artifact/123",
    )

    assert published == 0
    summary = summary_path.read_text(encoding="utf-8")
    assert "Direct HTML hosting is not configured." in summary
    assert "[Download interactive reports](https://github.example/artifact/123)" in summary


def test_publish_reports_distinguishes_configured_host_without_reports(tmp_path: Path) -> None:
    summary_path = tmp_path / "summary.md"

    assert publish_reports(
        runs_dir=tmp_path / "runs",
        run_id_prefix="ci-123-1",
        publish_dir=tmp_path / "published",
        base_url="https://reports.example.com/benchmarks",
        summary_path=summary_path,
    ) == 0

    assert "No benchmark reports were generated" in summary_path.read_text(encoding="utf-8")
    assert "hosting is not configured" not in summary_path.read_text(encoding="utf-8")


def test_publish_reports_prunes_only_expired_ci_runs(tmp_path: Path) -> None:
    publish_dir = tmp_path / "published"
    old_run = publish_dir / "ci-old-1"
    current_run = publish_dir / "ci-123-1"
    unrelated = publish_dir / "other-content"
    old_run.mkdir(parents=True)
    current_run.mkdir(parents=True)
    unrelated.mkdir(parents=True)
    old_time = time.time() - 15 * 24 * 60 * 60
    os.utime(old_run, (old_time, old_time))
    os.utime(current_run, (old_time, old_time))

    _write_report(tmp_path / "runs", "ci-123-1-memory", "<html></html>")
    publish_reports(
        runs_dir=tmp_path / "runs",
        run_id_prefix="ci-123-1",
        publish_dir=publish_dir,
        base_url="https://reports.example.com/benchmarks",
    )

    assert not old_run.exists()
    assert current_run.exists()
    assert unrelated.exists()
