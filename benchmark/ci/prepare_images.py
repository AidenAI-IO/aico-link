from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from pathlib import Path
from typing import Any

from ci.plan import BENCHMARK_ROOT
from runner.environment import DEFAULT_MOBILEGYM_IMAGE, ensure_mobilegym_image


DOCKER_DIR = BENCHMARK_ROOT / "docker"
DAEMON_COMPOSE_FILE = DOCKER_DIR / "docker-compose.agent-daemon.yml"


def _matrix_environments(matrix_json: str) -> set[str]:
    try:
        matrix: Any = json.loads(matrix_json)
        cases = matrix["include"]
    except (json.JSONDecodeError, KeyError, TypeError) as exc:
        raise ValueError("invalid benchmark matrix JSON") from exc
    if not isinstance(cases, list):
        raise ValueError("benchmark matrix include must be a list")
    return {
        str(case.get("environment") or "")
        for case in cases
        if isinstance(case, dict)
    }


def prepare_images(
    *,
    matrix_json: str,
    daemon_image: str,
    daemon_dockerfile: str,
    mobilegym_image: str,
    mobilegym_log: Path,
) -> None:
    daemon_environment = dict(os.environ)
    daemon_environment["AICO_DAEMON_IMAGE"] = daemon_image
    daemon_environment["DAEMON_DOCKERFILE"] = daemon_dockerfile
    subprocess.run(
        ["docker", "compose", "-f", str(DAEMON_COMPOSE_FILE), "build", "daemon"],
        cwd=DOCKER_DIR,
        env=daemon_environment,
        check=True,
    )

    if "mobilegym" not in _matrix_environments(matrix_json):
        return

    mobilegym_log.parent.mkdir(parents=True, exist_ok=True)
    try:
        ensure_mobilegym_image(
            mobilegym_image,
            True,
            mobilegym_log,
            repo_root=BENCHMARK_ROOT.parent,
        )
    except subprocess.CalledProcessError:
        if mobilegym_log.is_file():
            print(mobilegym_log.read_text(encoding="utf-8", errors="replace"), file=sys.stderr)
        raise


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Prepare benchmark CI Docker images")
    parser.add_argument("--matrix-json", required=True)
    parser.add_argument("--daemon-image", required=True)
    parser.add_argument("--daemon-dockerfile", required=True)
    parser.add_argument("--mobilegym-image", default=DEFAULT_MOBILEGYM_IMAGE)
    parser.add_argument("--mobilegym-log", type=Path, required=True)
    args = parser.parse_args(argv)
    try:
        prepare_images(
            matrix_json=args.matrix_json,
            daemon_image=args.daemon_image,
            daemon_dockerfile=args.daemon_dockerfile,
            mobilegym_image=args.mobilegym_image,
            mobilegym_log=args.mobilegym_log,
        )
    except (OSError, ValueError, subprocess.CalledProcessError, RuntimeError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    raise SystemExit(cli())
