"""Publish self-contained CI benchmark reports and link them from Actions."""

from __future__ import annotations

import argparse
import shutil
import time
import urllib.parse
from pathlib import Path


DEFAULT_RETENTION_DAYS = 14


def _prune_expired_reports(
    publish_dir: Path,
    *,
    current_run_id_prefix: str,
    retention_days: int,
) -> None:
    if retention_days < 1:
        raise ValueError("retention_days must be positive")
    if not publish_dir.is_dir():
        return
    cutoff = time.time() - retention_days * 24 * 60 * 60
    for candidate in publish_dir.iterdir():
        if (
            candidate.name == current_run_id_prefix
            or not candidate.name.startswith("ci-")
            or candidate.is_symlink()
            or not candidate.is_dir()
        ):
            continue
        try:
            expired = candidate.stat().st_mtime < cutoff
        except OSError:
            continue
        if expired:
            shutil.rmtree(candidate)


def publish_reports(
    *,
    runs_dir: Path,
    run_id_prefix: str,
    summary_path: Path | None = None,
    artifact_url: str = "",
    publish_dir: Path | None = None,
    base_url: str = "",
    retention_days: int = DEFAULT_RETENTION_DAYS,
) -> int:
    """Copy existing report HTML unchanged and append its links to a summary."""
    if (publish_dir is None) != (not base_url.strip()):
        raise ValueError("publish_dir and base_url must be configured together")

    reports = sorted(runs_dir.glob(f"{run_id_prefix}-*/report.html"))
    links: list[tuple[str, str]] = []
    if publish_dir is not None:
        for report in reports:
            case_id = report.parent.name.removeprefix(f"{run_id_prefix}-")
            destination = publish_dir / run_id_prefix / case_id / "report.html"
            destination.parent.mkdir(parents=True, exist_ok=True)
            staged = destination.with_suffix(".html.tmp")
            shutil.copyfile(report, staged)
            staged.replace(destination)
            encoded_path = "/".join(
                urllib.parse.quote(part, safe="")
                for part in (run_id_prefix, case_id, "report.html")
            )
            links.append((case_id, f"{base_url.rstrip('/')}/{encoded_path}"))
        _prune_expired_reports(
            publish_dir,
            current_run_id_prefix=run_id_prefix,
            retention_days=retention_days,
        )

    if summary_path is not None:
        summary_path.parent.mkdir(parents=True, exist_ok=True)
        with summary_path.open("a", encoding="utf-8") as summary:
            summary.write("\n## Benchmark reports\n\n")
            if not reports:
                summary.write(
                    f"No benchmark reports were generated for `{run_id_prefix}`.\n"
                )
                if artifact_url:
                    summary.write(f"\n[Download run artifacts]({artifact_url})\n")
            elif links:
                for label, url in links:
                    summary.write(f"- [{label}]({url})\n")
                if artifact_url:
                    summary.write(f"\n[Download all artifacts]({artifact_url})\n")
            else:
                summary.write("Direct HTML hosting is not configured.\n")
                if artifact_url:
                    summary.write(
                        f"\n[Download interactive reports]({artifact_url})\n"
                    )
    return len(links)


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--runs-dir", type=Path, default=Path("runs"))
    parser.add_argument("--run-id-prefix", required=True)
    parser.add_argument("--publish-dir", type=Path)
    parser.add_argument("--base-url", default="")
    parser.add_argument("--artifact-url", default="")
    parser.add_argument("--summary-path", type=Path)
    parser.add_argument(
        "--retention-days", type=int, default=DEFAULT_RETENTION_DAYS
    )
    args = parser.parse_args(argv)
    try:
        count = publish_reports(
            runs_dir=args.runs_dir,
            run_id_prefix=args.run_id_prefix,
            publish_dir=args.publish_dir,
            base_url=args.base_url,
            artifact_url=args.artifact_url,
            summary_path=args.summary_path,
            retention_days=args.retention_days,
        )
    except (OSError, ValueError) as exc:
        parser.error(str(exc))
    print(f"Published {count} directly browsable benchmark report(s)")
    return 0


if __name__ == "__main__":
    raise SystemExit(cli())
