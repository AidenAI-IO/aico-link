from __future__ import annotations

import argparse
import json
import os
import signal
import subprocess
import sys
import time
from pathlib import Path
from typing import Any, Mapping

from ci.plan import BENCHMARK_ROOT, CatalogError, SuiteCase, load_catalog
from runner.process import terminate_process_tree


ACTION_ENVIRONMENT_ALIASES = {
    "BENCHMARK_AGENT_PROVIDER": "AIDEN_BENCHMARK_AGENT_PROVIDER",
    "BENCHMARK_AGENT_MODEL": "AIDEN_BENCHMARK_AGENT_MODEL",
    "BENCHMARK_AGENT_BASE_URL": "AIDEN_BENCHMARK_AGENT_BASE_URL",
    "BENCHMARK_AGENT_API_KEY": "AIDEN_BENCHMARK_AGENT_API_KEY",
    "BENCHMARK_JUDGE_MODEL": "AIDEN_BENCHMARK_JUDGE_MODEL",
    "BENCHMARK_JUDGE_BASE_URL": "AIDEN_BENCHMARK_JUDGE_BASE_URL",
    "BENCHMARK_JUDGE_API_KEY": "AIDEN_BENCHMARK_JUDGE_API_KEY",
    "BENCHMARK_TELEMETRY": "AIDEN_BENCHMARK_TELEMETRY",
    "LANGFUSE_HTTPS_PROXY": "AIDEN_LANGFUSE_HTTPS_PROXY",
    "DAEMON_IMAGE": "AIDEN_DAEMON_IMAGE",
    "BENCHMARK_PHONE_ENVIRONMENT_URL": "AIDEN_BENCHMARK_PHONE_ENVIRONMENT_URL",
    "BENCHMARK_IOS_ENVIRONMENT_URL": "AIDEN_BENCHMARK_IOS_ENVIRONMENT_URL",
    "BENCHMARK_MAC_ENVIRONMENT_URL": "AIDEN_BENCHMARK_MAC_ENVIRONMENT_URL",
    "BENCHMARK_VPHONE_ENVIRONMENT_URL": "AIDEN_BENCHMARK_VPHONE_ENVIRONMENT_URL",
    "BENCHMARK_AIDEN_APP_IOS_ENVIRONMENT_URL": (
        "AIDEN_BENCHMARK_AIDEN_APP_IOS_ENVIRONMENT_URL"
    ),
    "BENCHMARK_AIDEN_APP_ANDROID_ENVIRONMENT_URL": (
        "AIDEN_BENCHMARK_AIDEN_APP_ANDROID_ENVIRONMENT_URL"
    ),
}
IMAGES_PREPARED_ENV = "BENCHMARK_CI_IMAGES_PREPARED"


def _runtime_environment(environment: Mapping[str, str]) -> dict[str, str]:
    runtime = dict(environment)
    for action_name, runtime_name in ACTION_ENVIRONMENT_ALIASES.items():
        if action_name in environment:
            runtime[runtime_name] = environment[action_name]
    return runtime


def _images_prepared(environment: Mapping[str, str]) -> bool:
    return environment.get(IMAGES_PREPARED_ENV, "").strip().lower() in {
        "1",
        "true",
        "yes",
    }


def _run_json(
    command: list[str],
    *,
    environment: Mapping[str, str],
    log_file: Any | None = None,
    timeout_seconds: float | None = None,
) -> dict[str, Any]:
    popen_kwargs: dict[str, Any] = {
        "cwd": BENCHMARK_ROOT,
        "env": environment,
        "text": True,
        "stdout": subprocess.PIPE,
        "stderr": log_file,
    }
    if timeout_seconds is not None:
        popen_kwargs["start_new_session"] = os.name == "posix"
    proc = subprocess.Popen(command, **popen_kwargs)
    try:
        stdout, _ = proc.communicate(timeout=timeout_seconds)
    except subprocess.TimeoutExpired:
        terminate_process_tree(proc)
        raise
    if proc.returncode:
        raise subprocess.CalledProcessError(
            proc.returncode,
            command,
            output=stdout,
        )
    try:
        payload = json.loads(stdout)
    except json.JSONDecodeError as exc:
        raise RuntimeError(
            f"service command returned invalid JSON: {' '.join(command)}"
        ) from exc
    if not isinstance(payload, dict):
        raise RuntimeError("service command did not return a JSON object")
    return payload


def _environment_url(
    case: SuiteCase,
    *,
    run_id: str,
    environment: Mapping[str, str],
    log_file: Any | None = None,
    timeout_seconds: float | None = None,
) -> tuple[str, dict[str, Any] | None]:
    python = sys.executable
    if case.environment == "isolated":
        return "", None
    if case.environment == "mobilegym":
        command = [
            python,
            "-m",
            "runner",
            "start-mobilegym-env",
            "--name",
            run_id,
            "--envs",
            str(case.max_concurrency),
            "--json",
        ]
        if _images_prepared(environment):
            command.append("--no-build-mobilegym-image")
        payload = _run_json(
            command,
            environment=environment,
            log_file=log_file,
            timeout_seconds=timeout_seconds,
        )
        return str(payload["environment_url"]), payload
    if case.environment == "adb":
        serial = environment.get("ANDROID_SERIAL", "").strip()
        if not serial:
            raise RuntimeError("ADB case requires ANDROID_SERIAL")
        payload = _run_json(
            [
                python,
                "-m",
                "runner",
                "start-adb-android-env",
                "--name",
                run_id,
                "--adb-serial",
                serial,
                "--json",
            ],
            environment=environment,
            log_file=log_file,
            timeout_seconds=timeout_seconds,
        )
        return str(payload["environment_url"]), payload
    variable = case.environment_variable
    value = environment.get(variable, "").strip()
    if not value:
        raise RuntimeError(f"{case.id} requires {variable}")
    return value, None


def _stop_service(payload: dict[str, Any] | None) -> None:
    if not payload:
        return
    container_name = str(payload.get("container_name") or "").strip()
    if container_name:
        subprocess.run(
            ["docker", "rm", "-f", container_name],
            cwd=BENCHMARK_ROOT,
            check=False,
        )
    pid = payload.get("pid")
    if isinstance(pid, int) and pid > 0:
        try:
            os.kill(pid, signal.SIGTERM)
        except ProcessLookupError:
            pass


def _stop_mobilegym_by_run_id(run_id: str) -> None:
    subprocess.run(
        ["docker", "rm", "-f", f"aiden-mobilegym-env-mobilegym-{run_id}"],
        cwd=BENCHMARK_ROOT,
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
        check=False,
    )


def _stop_daemon_projects_by_run_id(run_id: str) -> None:
    workers_dir = BENCHMARK_ROOT / "runs" / run_id / "workers"
    try:
        worker_dirs = [path for path in workers_dir.iterdir() if path.is_dir()]
    except OSError:
        return
    compose_file = BENCHMARK_ROOT / "docker" / "docker-compose.agent-daemon.yml"
    for worker_dir in worker_dirs:
        project = f"aiden-benchmark-agent-{run_id}-{worker_dir.name}"
        try:
            subprocess.run(
                [
                    "docker",
                    "compose",
                    "-f",
                    str(compose_file),
                    "-p",
                    project,
                    "down",
                    "--volumes",
                    "--remove-orphans",
                ],
                cwd=BENCHMARK_ROOT / "docker",
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
                timeout=30,
            )
        except (OSError, subprocess.SubprocessError):
            pass


def _incomplete_run_reasons(returncode: int, manifest_path: Path) -> list[str]:
    """Describe incomplete run artifacts that should fail benchmark CI."""
    if not manifest_path.is_file():
        return ["manifest_missing", f"runner_exit={returncode}"] if returncode else [
            "manifest_missing"
        ]
    try:
        manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
        totals = manifest["totals"]
        tasks = int(totals["tasks"])
        passed = int(totals["passed"])
        failed = int(totals["failed"])
        skipped = int(totals["skipped"])
        judge_errors = int(totals.get("judge_error", 0))
        timeouts = int(totals.get("timeout", 0))
    except (
        AttributeError,
        OSError,
        KeyError,
        TypeError,
        ValueError,
        json.JSONDecodeError,
    ):
        return ["manifest_invalid", f"runner_exit={returncode}"] if returncode else [
            "manifest_invalid"
        ]

    reasons: list[str] = []
    completed = passed + failed + skipped + judge_errors + timeouts
    if tasks < 1:
        reasons.append(f"tasks={tasks}")
    if completed != tasks:
        reasons.append(f"completed={completed}/{tasks}")
    try:
        results_path = manifest_path.with_name("results.jsonl")
        result_rows = 0
        unknown_statuses = 0
        result_identities: list[tuple[str, int]] = []
        invalid_identities = 0
        result_counts = {
            "passed": 0,
            "failed": 0,
            "skipped": 0,
            "judge_error": 0,
            "timeout": 0,
        }
        for line in results_path.read_text(encoding="utf-8").splitlines():
            if not line.strip():
                continue
            row = json.loads(line)
            if not isinstance(row, dict):
                raise TypeError("result row must be an object")
            result_rows += 1
            task_id = str(row.get("task_id") or "").strip()
            try:
                attempt = int(row.get("attempt"))
            except (TypeError, ValueError):
                attempt = 0
            if task_id and attempt > 0:
                result_identities.append((task_id, attempt))
            else:
                invalid_identities += 1
            status = str(row.get("status") or "")
            if status in result_counts:
                result_counts[status] += 1
            else:
                unknown_statuses += 1
            metrics = row.get("metrics") or {}
            if not isinstance(metrics, dict):
                raise TypeError("result metrics must be an object")
    except (
        AttributeError,
        OSError,
        KeyError,
        TypeError,
        ValueError,
        json.JSONDecodeError,
    ):
        reasons.append("results_invalid")
    else:
        manifest_counts = {
            "passed": passed,
            "failed": failed,
            "skipped": skipped,
            "judge_error": judge_errors,
            "timeout": timeouts,
        }
        if result_rows != tasks:
            reasons.append(f"results={result_rows}/{tasks}")
        if result_counts != manifest_counts:
            reasons.append("results_totals_mismatch")
        if unknown_statuses:
            reasons.append(f"unknown_status={unknown_statuses}")
        if invalid_identities:
            reasons.append(f"invalid_result_identity={invalid_identities}")
        if len(set(result_identities)) != len(result_identities):
            reasons.append("duplicate_result_identity")
        planned = manifest.get("planned_task_attempts")
        if planned is not None:
            try:
                planned_identities = {
                    (str(item["task_id"]).strip(), int(item["attempt"]))
                    for item in planned
                }
                planned_valid = (
                    isinstance(planned, list)
                    and len(planned_identities) == len(planned)
                    and all(
                        task_id and attempt > 0
                        for task_id, attempt in planned_identities
                    )
                )
            except (KeyError, TypeError, ValueError):
                planned_valid = False
                planned_identities = set()
            if not planned_valid:
                reasons.append("planned_task_attempts_invalid")
            elif set(result_identities) != planned_identities:
                reasons.append("planned_results_mismatch")
    for filename, label in (
        ("metrics.json", "metrics"),
        ("summary.md", "summary"),
        ("report.html", "report"),
    ):
        path = manifest_path.with_name(filename)
        try:
            available = path.is_file() and path.stat().st_size > 0
        except OSError:
            available = False
        if not available:
            reasons.append(f"{label}_missing")
    expected_returncode = (
        1
        if judge_errors or timeouts or passed + failed + skipped != tasks
        else 0
    )
    if returncode != expected_returncode:
        reasons.append(f"runner_exit={returncode},expected={expected_returncode}")
    return reasons


def _effective_exit_code(returncode: int, manifest_path: Path) -> int:
    """Fail CI only when the benchmark did not produce a complete result set."""
    return 1 if _incomplete_run_reasons(returncode, manifest_path) else 0


def run_case(
    case: SuiteCase,
    *,
    run_id: str,
    environment: Mapping[str, str],
    log_path: Path | None = None,
    timeout_seconds: float | None = None,
) -> int:
    runtime_environment = _runtime_environment(environment)
    environment_url = ""
    service: dict[str, Any] | None = None
    deadline = (
        time.monotonic() + timeout_seconds
        if timeout_seconds is not None
        else None
    )

    def remaining_timeout() -> float | None:
        if deadline is None:
            return None
        return max(0.001, deadline - time.monotonic())

    # Concurrent cases interleave unreadably on a shared stdout, so each one can
    # capture into its own file instead. Without log_path the output stays inherited,
    # which is what the single-case CLI wants.
    log_file = None
    if log_path is not None:
        log_path.parent.mkdir(parents=True, exist_ok=True)
        log_file = log_path.open("w", encoding="utf-8")
    try:
        try:
            environment_url, service = _environment_url(
                case,
                run_id=run_id,
                environment=runtime_environment,
                log_file=log_file,
                timeout_seconds=remaining_timeout(),
            )
        except subprocess.TimeoutExpired:
            message = f"case {case.id} exceeded {timeout_seconds:.0f}s during environment startup"
            if log_file is not None:
                log_file.write(f"\n{message}\n")
            print(f"Error: {message}", file=sys.stderr, flush=True)
            return 1
        command = [
            sys.executable,
            "-m",
            "runner",
            "run",
            "--suite",
            case.suite,
            "--run-id",
            run_id,
            "--auto-agent-setup",
            "--max-concurrency",
            str(case.max_concurrency),
            "--verbose",
        ]
        repeats = runtime_environment.get("BENCHMARK_REPEATS", "").strip()
        if repeats:
            try:
                repeat_count = int(repeats)
            except ValueError as exc:
                raise RuntimeError(
                    "BENCHMARK_REPEATS must be a positive integer, "
                    f"got {repeats!r}"
                ) from exc
            if repeat_count <= 0:
                raise RuntimeError(
                    "BENCHMARK_REPEATS must be a positive integer, "
                    f"got {repeats!r}"
                )
            command.extend(["--repeats", str(repeat_count)])
        if environment_url:
            command.extend(["--environment-url", environment_url])
        if case.target_platform != "auto":
            command.extend(["--target-platform", case.target_platform])
        if _images_prepared(runtime_environment):
            command.append("--no-build-daemon-image")
        if timeout_seconds is None:
            returncode = subprocess.run(
                command,
                cwd=BENCHMARK_ROOT,
                env=runtime_environment,
                check=False,
                stdout=log_file,
                stderr=subprocess.STDOUT if log_file is not None else None,
            ).returncode
        else:
            popen_kwargs: dict[str, Any] = {
                "cwd": BENCHMARK_ROOT,
                "env": runtime_environment,
                "stdout": log_file,
                "stderr": subprocess.STDOUT,
                "start_new_session": os.name == "posix",
            }
            proc = subprocess.Popen(command, **popen_kwargs)
            try:
                returncode = proc.wait(timeout=remaining_timeout())
            except subprocess.TimeoutExpired:
                # One overrunning case must not consume the whole job's budget;
                # terminate the runner and every daemon it spawned.
                terminate_process_tree(proc)
                message = f"case {case.id} exceeded {timeout_seconds:.0f}s and was killed"
                if log_file is not None:
                    log_file.write(f"\n{message}\n")
                print(f"Error: {message}", file=sys.stderr, flush=True)
                return 1
        return _effective_exit_code(returncode, BENCHMARK_ROOT / "runs" / run_id / "manifest.json")
    finally:
        _stop_daemon_projects_by_run_id(run_id)
        _stop_service(service)
        if case.environment == "mobilegym" and service is None:
            _stop_mobilegym_by_run_id(run_id)
        if log_file is not None:
            log_file.close()


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Run one catalogued benchmark CI case")
    parser.add_argument("--case-id", required=True)
    parser.add_argument("--run-id", required=True)
    args = parser.parse_args(argv)
    try:
        catalog = load_catalog()
        case = next((item for item in catalog.cases if item.id == args.case_id), None)
        if case is None:
            raise CatalogError(f"unknown benchmark CI case: {args.case_id}")
        return run_case(case, run_id=args.run_id, environment=os.environ)
    except (CatalogError, RuntimeError, KeyError, subprocess.CalledProcessError) as exc:
        print(f"Error: {exc}", file=sys.stderr)
        return 2


if __name__ == "__main__":
    raise SystemExit(cli())
