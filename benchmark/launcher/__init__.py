"""Aico Launcher: a repository-local web UI that starts Aico Link Docker images.

The launcher builds or selects an ``aico-link`` image, starts an environment
bridge on the host (MobileGym, ADB Android, or a custom endpoint), and runs the
image with Config Web and the Agent Web UI published on 127.0.0.1. It is a
developer tool and is not published as a package.
"""

from __future__ import annotations

from pathlib import Path

BENCHMARK_ROOT = Path(__file__).resolve().parents[1]
REPO_ROOT = BENCHMARK_ROOT.parent
STATIC_DIR = Path(__file__).resolve().parent / "static"
DEFAULT_PORT = 4180
DEFAULT_STATE_DIR = REPO_ROOT / ".cache" / "launcher"
IMAGE_REPOSITORY = "aico-link"
LABEL_INSTANCE = "io.aico.launcher.instance"
LABEL_ROLE = "io.aico.launcher.role"
