#!/usr/bin/env python3
"""Run the repository's Dockerized test/build suites from a manifest.

The quick profile gives local feedback; the full profile is the CI gate. Both
use the same manifest and test image so coverage cannot silently drift.
"""

from __future__ import annotations

import argparse
import json
import os
from pathlib import Path
import signal
import shutil
import subprocess
import sys
import time
from typing import Any

try:
    import yaml
except ImportError as exc:  # pragma: no cover - exercised by the image preflight
    raise SystemExit("PyYAML is required to read tests/test-manifest.yaml") from exc


REPO_ROOT = Path(__file__).resolve().parents[1]
MANIFEST_PATH = REPO_ROOT / "tests" / "test-manifest.yaml"


class ManifestError(RuntimeError):
    pass


def load_manifest() -> list[dict[str, Any]]:
    with MANIFEST_PATH.open(encoding="utf-8") as stream:
        document = yaml.safe_load(stream)
    if not isinstance(document, dict) or document.get("version") != 1:
        raise ManifestError(f"unsupported manifest format: {MANIFEST_PATH}")
    suites = document.get("suites")
    if not isinstance(suites, list) or not suites:
        raise ManifestError("manifest must contain a non-empty suites list")

    seen: set[str] = set()
    validated: list[dict[str, Any]] = []
    for suite in suites:
        if not isinstance(suite, dict):
            raise ManifestError("each suite must be a mapping")
        name = suite.get("name")
        command = suite.get("command")
        paths = suite.get("paths")
        if not isinstance(name, str) or not name:
            raise ManifestError("each suite needs a non-empty name")
        if name in seen:
            raise ManifestError(f"duplicate suite name: {name}")
        seen.add(name)
        if not isinstance(command, str) or not command.strip():
            raise ManifestError(f"suite {name} needs a command")
        if not isinstance(paths, list) or not paths or not all(
            isinstance(path, str) and path and not Path(path).is_absolute()
            for path in paths
        ):
            raise ManifestError(f"suite {name} needs relative paths")
        dependencies = suite.get("dependencies", [])
        if not isinstance(dependencies, list) or not all(
            isinstance(dep, str) and dep for dep in dependencies
        ):
            raise ManifestError(f"suite {name} has invalid dependencies")
        required_inputs = suite.get("required_inputs", [])
        if not isinstance(required_inputs, list) or not all(
            isinstance(path, str) and path and not Path(path).is_absolute()
            for path in required_inputs
        ):
            raise ManifestError(f"suite {name} has invalid required_inputs")
        if not isinstance(suite.get("requires_docker_socket", False), bool):
            raise ManifestError(f"suite {name} has invalid requires_docker_socket")
        if not isinstance(suite.get("requires_host_network", False), bool):
            raise ManifestError(f"suite {name} has invalid requires_host_network")
        if suite.get("requires_host_network") and not suite.get("requires_docker_socket"):
            raise ManifestError(f"suite {name} requires host networking without a Docker socket")
        if not isinstance(suite.get("quick", False), bool):
            raise ManifestError(f"suite {name} has invalid quick flag")
        quick_command = suite.get("quick_command")
        if quick_command is not None and (
            not suite.get("quick") or not isinstance(quick_command, str) or not quick_command.strip()
        ):
            raise ManifestError(f"suite {name} has invalid quick_command")
        timeout = suite.get("timeout_seconds", 600)
        if not isinstance(timeout, int) or timeout <= 0:
            raise ManifestError(f"suite {name} has invalid timeout_seconds")
        suite = dict(suite)
        suite["required"] = bool(suite.get("required", True))
        suite["workdir"] = suite.get("workdir", ".")
        if not isinstance(suite["workdir"], str) or Path(suite["workdir"]).is_absolute():
            raise ManifestError(f"suite {name} has an invalid workdir")
        validated.append(suite)
    return validated


def path_exists(relative: str) -> bool:
    return (REPO_ROOT / relative).exists()


def format_command(command: str) -> str:
    return command.strip().replace("\n", " && ")


def select_suites(
    suites: list[dict[str, Any]],
    *,
    profile: str,
    requested: set[str] | None = None,
    required_names: set[str] | None = None,
    suite_class: str | None = None,
) -> list[dict[str, Any]]:
    if profile not in {"quick", "full"}:
        raise ManifestError(f"unknown profile: {profile}")
    requested = requested or set()
    required_names = required_names or set()
    known = {suite["name"] for suite in suites}
    unknown = (requested | required_names) - known
    if unknown:
        raise ManifestError(f"unknown suite(s): {', '.join(sorted(unknown))}")
    if requested and not required_names <= requested:
        raise ManifestError("required suite(s) excluded by --suite: " +
                            ", ".join(sorted(required_names - requested)))
    if suite_class:
        excluded = {
            suite["name"] for suite in suites
            if suite["name"] in required_names and suite.get("class") != suite_class
        }
        if excluded:
            raise ManifestError("required suite(s) excluded by --class: " +
                                ", ".join(sorted(excluded)))

    selected = []
    for suite in suites:
        if requested and suite["name"] not in requested:
            continue
        if suite_class and suite.get("class") != suite_class:
            continue
        if not requested and not suite_class:
            if profile == "quick" and not suite.get("quick", False) and suite["name"] not in required_names:
                continue
            if profile == "full" and not suite["required"] and suite["name"] not in required_names:
                continue
        if suite["name"] in required_names or requested or suite_class:
            # Explicit selections must never report a successful no-op when
            # their inputs or the Docker socket are unavailable.
            suite = dict(suite)
            suite["required"] = True
        selected.append(suite)
    if not selected:
        raise ManifestError("no suites selected")
    return selected


def unavailable_result(suite: dict[str, Any], reason: str, *, always_fail: bool = False) -> dict[str, Any]:
    return {
        "name": suite["name"],
        "class": suite.get("class", "unknown"),
        "status": "failed" if always_fail or suite["required"] else "skipped",
        "required": suite["required"],
        "reason": reason,
        "duration_seconds": 0.0,
        "executed": False,
    }


def run_suite(suite: dict[str, Any], *, profile: str = "full") -> dict[str, Any]:
    name = suite["name"]
    required = suite["required"]
    missing_paths = [path for path in suite["paths"] if not path_exists(path)]
    if missing_paths:
        return unavailable_result(suite, f"missing manifest paths: {', '.join(missing_paths)}")

    missing_inputs = [
        path for path in suite.get("required_inputs", []) if not path_exists(path)
    ]
    if missing_inputs:
        return unavailable_result(suite, f"missing required inputs: {', '.join(missing_inputs)}")

    if suite.get("requires_docker_socket") and not Path("/var/run/docker.sock").exists():
        return unavailable_result(suite, "Docker socket is not mounted; rerun with --docker-socket")
    if suite.get("requires_host_network") and os.environ.get("AICO_TEST_HOST_NETWORK") != "1":
        return unavailable_result(suite, "host network is required; rerun with --host-network")

    missing_dependencies = [
        dependency
        for dependency in suite.get("dependencies", [])
        if shutil.which(dependency) is None
    ]
    if missing_dependencies:
        return unavailable_result(suite, f"missing required commands: {', '.join(missing_dependencies)}")

    workdir = REPO_ROOT / suite["workdir"]
    if not workdir.is_dir():
        return unavailable_result(suite, f"workdir does not exist: {suite['workdir']}", always_fail=True)

    command = suite.get("quick_command", suite["command"]) if profile == "quick" else suite["command"]
    print(f"\n=== {name} ({suite.get('class', 'unknown')}, {profile}) ===", flush=True)
    print(f"$ (cd {suite['workdir']} && {format_command(command)})", flush=True)
    started = time.monotonic()
    env = os.environ.copy()
    env.setdefault("AICO_CONTRACT_FIXTURES", str(REPO_ROOT / "tests" / "contracts"))
    command = "set -euo pipefail\n" + command
    process = subprocess.Popen(
        command,
        shell=True,
        cwd=workdir,
        env=env,
        executable="/bin/bash",
        start_new_session=True,
    )
    try:
        returncode = process.wait(timeout=suite["timeout_seconds"])
        status = "passed" if returncode == 0 else "failed"
        reason = "" if status == "passed" else f"exit code {returncode}"
    except subprocess.TimeoutExpired:
        try:
            os.killpg(process.pid, signal.SIGKILL)
        except ProcessLookupError:
            pass
        process.wait()
        status = "failed"
        reason = f"timeout after {suite['timeout_seconds']} seconds"
    duration = time.monotonic() - started
    print(f"=== {name}: {status} ({duration:.1f}s) ===", flush=True)
    return {
        "name": name,
        "class": suite.get("class", "unknown"),
        "status": status,
        "required": required,
        "reason": reason,
        "duration_seconds": round(duration, 3),
        "executed": True,
    }


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--suite", action="append", dest="suites", help="run only this suite; repeatable")
    parser.add_argument("--class", dest="suite_class", help="run only suites of this class")
    parser.add_argument("--profile", choices=("quick", "full"), default="full",
                        help="quick local feedback or full CI gate (default: full)")
    parser.add_argument(
        "--require-suite",
        action="append",
        default=[],
        help="promote an optional suite to required; repeatable",
    )
    parser.add_argument("--list", action="store_true", help="list suites and exit")
    parser.add_argument("--summary", type=Path, help="write a JSON summary")
    args = parser.parse_args()

    try:
        suites = load_manifest()
    except ManifestError as exc:
        print(f"manifest error: {exc}", file=sys.stderr)
        return 2

    if args.list:
        for suite in suites:
            profiles = "full+quick" if suite.get("quick", False) else "full"
            print(f"{suite['name']}\t{suite.get('class', 'unknown')}\t{profiles}\t{'required' if suite['required'] else 'optional'}")
        return 0

    try:
        selected = select_suites(
            suites,
            profile=args.profile,
            requested=set(args.suites or []),
            required_names=set(args.require_suite),
            suite_class=args.suite_class,
        )
    except ManifestError as exc:
        print(f"manifest error: {exc}", file=sys.stderr)
        return 2

    results = [run_suite(suite, profile=args.profile) for suite in selected]
    summary = {
        "manifest": str(MANIFEST_PATH.relative_to(REPO_ROOT)),
        "profile": args.profile,
        "selected": [suite["name"] for suite in selected],
        "results": results,
        "passed": sum(result["status"] == "passed" for result in results),
        "failed": sum(result["status"] == "failed" for result in results),
        "skipped": sum(result["status"] == "skipped" for result in results),
    }
    if args.summary:
        args.summary.parent.mkdir(parents=True, exist_ok=True)
        args.summary.write_text(json.dumps(summary, indent=2) + "\n", encoding="utf-8")

    print(
        f"\nManifest summary ({args.profile}): "
        f"{summary['passed']} passed, {summary['failed']} failed, "
        f"{summary['skipped']} skipped",
        flush=True,
    )
    if args.profile == "quick" and not args.suites and not args.suite_class:
        print("Quick feedback only; run make check-full for the complete CI gate.", flush=True)
    return 1 if any(
        result["status"] == "failed" and (result["required"] or result["executed"])
        for result in results
    ) else 0


if __name__ == "__main__":
    raise SystemExit(main())
