from __future__ import annotations
import argparse
import concurrent.futures
import hashlib
import json
import os
import re
import sys
import time
from dataclasses import dataclass
from datetime import datetime, timezone
from pathlib import Path
from runner.agent_client import AgentClient
from runner.analysis import AnalysisConfig, _int_env, analyze_run
from runner.config import BENCHMARK_AGENT_MODEL_ENV
from runner.html_report import generate_report_html, upload_report
from runner.judge import DEFAULT_JUDGE_BASE_URL, JudgeConfig
from runner.platform import (
    TargetPlatform,
    read_environment_health,
    resolve_daemon_platform,
    resolve_environment_platform,
)
from runner.preflight import (
    MOBILEGYM_PREFLIGHT_COMPLETE_ENV,
    preflight_mobilegym_environment,
)
from runner.report import (
    git_sha,
    now_iso,
    write_jsonl,
    write_manifest,
    write_metrics,
    write_summary,
)
from runner.recovery import (
    DEFAULT_ENVIRONMENT_SETUP_TIMEOUT_SEC,
    recover_agent_after_timeout,
    wait_for_agent_ready,
)
from runner.reset import (
    ResetError,
    call_environment_release,
    clear_stale_adb_android_owner,
)
from runner.runtask import run_one_task, skipped_task_result
from runner.suite import (
    Suite,
    TaskSpec,
    effective_mock_environment,
    load_suite,
    resolve_mock_task_platform,
)

REPO_ROOT = Path(__file__).resolve().parents[2]


@dataclass(frozen=True)
class TaskRunUnit:
    index: int
    task: TaskSpec
    attempt: int
    repeats: int
    skip_reason: str
    target_platform: str


def wait_for_agent_clock(
    client: AgentClient,
    min_year: int | None = None,
    timeout_sec: int = 180,
    poll_sec: int = 2,
) -> bool:
    if min_year is None:
        min_year = datetime.now(timezone.utc).year
    deadline = time.monotonic() + max(0, timeout_sec)

    while True:
        try:
            result = client.invoke_tool("shell", {"command": "date +%Y", "timeout": 5})
            if not result.is_error:
                match = re.search(r"\b(19\d{2}|20\d{2})\b", result.output or "")
                if match and int(match.group(1)) >= min_year:
                    return True
        except Exception:
            pass

        now = time.monotonic()
        if now >= deadline:
            return False
        time.sleep(min(max(0, poll_sec), max(0, deadline - now)))


def recover_agent_after_timeout(client: AgentClient, timeout_sec: int = 90) -> bool:
    """Wait for an Agent to become usable after a task timeout.

    Keep this small compatibility seam separate from task execution so callers
    and tests can replace the recovery policy without replacing the runner.
    """
    return wait_for_agent_ready(client, timeout_sec=timeout_sec)


def cli(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="benchmark.runner")
    sub = parser.add_subparsers(dest="cmd", required=True)
    p_run = sub.add_parser("run")
    p_run.add_argument("--suite", required=True)
    p_run.add_argument("--agent-url", default=os.environ.get("AIDEN_AGENT_URL", "http://localhost:8080"))
    p_run.add_argument("--environment-url", default=os.environ.get("AIDEN_ENVIRONMENT_URL", ""),
                       help="Optional environment bridge endpoint; when set, each task calls /api/setup, POST /api/providers/screenshot, and /api/release")
    p_run.add_argument("--auto-agent-setup", action="store_true",
                       help="Start isolated agent daemons automatically and ignore --agent-url; concurrency is read from environment bridge /api/concurrent")
    p_run.add_argument("--max-concurrency", type=int, default=0,
                       help="Cap auto-agent-setup worker concurrency; 0 means no explicit cap")
    p_run.add_argument("--daemon-image", default=os.environ.get("AIDEN_DAEMON_IMAGE", "aiden-agent-daemon:local"))
    p_run.add_argument("--no-build-daemon-image", action="store_true")
    p_run.add_argument("--base-config-dir", default=str(REPO_ROOT / "benchmark" / "config"))
    p_run.add_argument("--agent-config", default="")
    p_run.add_argument("--benchmark-token-file", default="")
    p_run.add_argument(
        "--judge-model",
        default=os.environ.get("AIDEN_BENCHMARK_JUDGE_MODEL", "claude-sonnet-4-6"),
    )
    p_run.add_argument(
        "--judge-base-url",
        default=os.environ.get("AIDEN_BENCHMARK_JUDGE_BASE_URL", DEFAULT_JUDGE_BASE_URL),
        help="OpenAI-compatible base URL for judge requests",
    )
    p_run.add_argument("--agent-model", default=os.environ.get(BENCHMARK_AGENT_MODEL_ENV, ""))
    p_run.add_argument("--no-judge", action="store_true")
    p_run.add_argument("--repeats", type=int, default=None)
    p_run.add_argument("--out", default=str(REPO_ROOT / "benchmark" / "runs"))
    p_run.add_argument("--state-file", default=os.environ.get("BENCHMARK_STATE_FILE"))
    p_run.add_argument("--task-id", action="append", default=[],
                       help="Run only this task id; can be repeated")
    p_run.add_argument("--task-ids", default="",
                        help="Comma-separated task ids to run")
    p_run.add_argument("--skill", action="append", default=[],
                       help="Activate this skill for every task chat; can be repeated or comma-separated")
    p_run.add_argument("--run-id", default="",
                       help="Optional run directory name under --out")
    p_run.add_argument("--benchmark-task-id", default="",
                       help="Task routing id to use for environment setup, providers/screenshot, and release")
    p_run.add_argument("--target-platform", default=os.environ.get("AIDEN_BENCHMARK_TARGET_PLATFORM", "auto"),
                       choices=["auto", "ios", "android", "mac", "windows", "linux"],
                       help="Filter suite tasks by platform; auto resolves platform from environment bridge health")
    p_run.add_argument("--skip-clock-wait", action="store_true")
    p_run.add_argument("--clock-timeout-sec", type=int, default=180)
    p_run.add_argument("--agent-ready-timeout-sec", type=int, default=120)
    p_run.add_argument("--agent-recovery-timeout-sec", type=int, default=90,
                       help="Extra wait after timeout/skipped before next task")
    p_run.add_argument("--inter-task-cooldown-sec", type=float, default=2.0)
    p_run.add_argument("--llm-analysis", action="store_true", help="Run post-run LLM RCA analysis")
    p_run.add_argument(
        "--analysis-model",
        default=os.environ.get("AIDEN_BENCHMARK_ANALYSIS_MODEL"),
    )
    p_run.add_argument(
        "--analysis-base-url",
        default=os.environ.get("AIDEN_BENCHMARK_ANALYSIS_BASE_URL"),
        help="OpenAI-compatible base URL for post-run LLM analysis; defaults to judge base URL",
    )
    p_run.add_argument(
        "--analysis-max-log-bytes",
        type=int,
        default=_int_env("AIDEN_BENCHMARK_ANALYSIS_MAX_LOG_BYTES", 64 * 1024),
    )
    p_run.add_argument(
        "--analysis-max-code-bytes",
        type=int,
        default=_int_env("AIDEN_BENCHMARK_ANALYSIS_MAX_CODE_BYTES", 128 * 1024),
    )
    p_run.add_argument(
        "--analysis-timeout-sec",
        type=int,
        default=_int_env("AIDEN_BENCHMARK_ANALYSIS_TIMEOUT_SEC", 180),
    )
    p_run.add_argument("--verbose", "-v", action="store_true",
                       help="Show detailed rubric results for each task")
    p_rejudge = sub.add_parser("rejudge")
    p_rejudge.add_argument("--run-dir", required=True)
    p_rejudge.add_argument(
        "--judge-model",
        default=os.environ.get("AIDEN_BENCHMARK_JUDGE_MODEL", "claude-sonnet-4-6"),
    )
    p_rejudge.add_argument(
        "--judge-base-url",
        default=os.environ.get("AIDEN_BENCHMARK_JUDGE_BASE_URL", DEFAULT_JUDGE_BASE_URL),
    )
    p_compare = sub.add_parser("compare")
    p_compare.add_argument("--runs", nargs=2, required=True)
    p_publish = sub.add_parser("publish-langfuse")
    p_publish.add_argument("--run-dir", required=True)
    p_publish.add_argument("--dataset-prefix", default="aiden-benchmark")
    p_webui = sub.add_parser("webui")
    p_webui.add_argument("--host", default="127.0.0.1")
    p_webui.add_argument("--port", type=int, default=8765)
    p_webui.add_argument("--suites-dir", default=str(REPO_ROOT / "benchmark" / "suites"))
    p_webui.add_argument("--runs-dir", default=str(REPO_ROOT / "benchmark" / "runs" / "webui"))
    p_webui.add_argument("--base-config-dir", default=str(REPO_ROOT / "benchmark" / "config"))
    p_webui.add_argument("--agent-config", default="")
    p_webui.add_argument("--daemon-image", default="aiden-agent-daemon:local")
    p_webui.add_argument("--mobilegym-image", default="aiden-mobilegym-simulator:py311")
    p_webui.add_argument("--no-build-daemon-image", action="store_true")
    p_webui.add_argument("--no-build-mobilegym-image", action="store_true")
    from runner.services import add_service_parsers
    add_service_parsers(sub)
    args = parser.parse_args(argv)
    if args.cmd == "run":
        return _cmd_run(args)
    if args.cmd == "rejudge":
        from runner.rejudge import rejudge_run
        return rejudge_run(Path(args.run_dir), args.judge_model, args.judge_base_url)
    if args.cmd == "compare":
        from runner.compare import compare_runs
        return compare_runs(Path(args.runs[0]), Path(args.runs[1]))
    if args.cmd == "publish-langfuse":
        from runner.langfuse_reporter import LangfusePublishError, publish_run
        try:
            published = publish_run(
                Path(args.run_dir),
                dataset_prefix=args.dataset_prefix,
                progress=lambda message: print(
                    f"Langfuse publish: {message}",
                    flush=True,
                ),
            )
        except LangfusePublishError as exc:
            print(f"Error: {exc}", file=sys.stderr)
            return 2
        state = "already published" if published.already_exists else "published"
        print(
            f"Langfuse experiment {state}: dataset={published.dataset_name} "
            f"run={published.run_name} items={published.item_count}",
            flush=True,
        )
        if published.dataset_run_url:
            print(f"View experiment: {published.dataset_run_url}", flush=True)
        return 0
    if args.cmd == "webui":
        from runner.webui import cli as webui_cli
        forwarded = [
            "--host", args.host,
            "--port", str(args.port),
            "--suites-dir", args.suites_dir,
            "--runs-dir", args.runs_dir,
            "--base-config-dir", args.base_config_dir,
            "--daemon-image", args.daemon_image,
            "--mobilegym-image", args.mobilegym_image,
        ]
        if args.agent_config:
            forwarded.extend(["--agent-config", args.agent_config])
        if args.no_build_daemon_image:
            forwarded.append("--no-build-daemon-image")
        if args.no_build_mobilegym_image:
            forwarded.append("--no-build-mobilegym-image")
        return webui_cli(forwarded)
    if args.cmd == "start-agent-daemon":
        from runner.services import cmd_start_agent_daemon
        return cmd_start_agent_daemon(args)
    if args.cmd == "start-mobilegym-env":
        from runner.services import cmd_start_mobilegym_env
        return cmd_start_mobilegym_env(args)
    if args.cmd == "start-adb-android-env":
        from runner.services import cmd_start_adb_android_env
        return cmd_start_adb_android_env(args)
    if args.cmd == "start-desktop-env":
        from runner.services import cmd_start_desktop_env
        return cmd_start_desktop_env(args)
    return 2


def _log_task_result(task_id: str, attempt: int, result, verbose: bool = False,
                     progress: str = "") -> None:
    """Print task execution result with optional detailed rubric information."""
    prefix = f"[{progress}] " if progress else ""
    status_line = (
        f"{prefix}{result.status.upper():10s} {task_id} attempt={attempt} "
        f"rubric={result.rubric_pass_count}/{result.rubric_total} "
        f"wall={result.metrics.get('wall_ms')}ms"
    )

    # Add metrics if available
    metrics_info = []
    if "tool_calls" in result.metrics:
        metrics_info.append(f"tools={result.metrics['tool_calls']}")
    if "screenshots_taken" in result.metrics:
        metrics_info.append(f"screenshots={result.metrics['screenshots_taken']}")
    if metrics_info:
        status_line += f" ({', '.join(metrics_info)})"

    print(status_line, flush=True)

    # Show detailed rubric results in verbose mode
    if verbose and result.rubric:
        print("  📋 Rubric Details:", flush=True)
        for i, v in enumerate(result.rubric, 1):
            verdict_symbol = "✅" if v.verdict == "yes" else "❌"
            print(f"    {verdict_symbol} [{i}/{len(result.rubric)}] {v.id}: {v.verdict.upper()}", flush=True)
            if v.reason:
                # Indent reason text for readability
                reason_lines = v.reason.strip().split('\n')
                for line in reason_lines[:3]:  # Limit to first 3 lines
                    print(f"        → {line}", flush=True)
                if len(reason_lines) > 3:
                    print(f"        → ... ({len(reason_lines) - 3} more lines)", flush=True)

    if verbose and result.hard_assertion_failures:
        print("  ⚠️  Hard assertion failures:", flush=True)
        for failure in result.hard_assertion_failures:
            print(f"    - {failure.label}", flush=True)
            print(f"        Requirement: {failure.requirement}", flush=True)
            print(f"        Actual: {failure.actual}", flush=True)

    # Show error messages (verbose mode)
    if verbose and "error" in result.metrics:
        print(f"  ❌ Error: {result.metrics['error']}", flush=True)
    if verbose and "agent_error" in result.metrics:
        print(f"  ❌ Agent Error: {result.metrics['agent_error']}", flush=True)
    if verbose and "judge_error" in result.metrics:
        print(f"  ❌ Judge Error: {result.metrics['judge_error']}", flush=True)


def _write_state(path: str | None, payload: dict) -> None:
    if not path:
        return
    try:
        p = Path(path)
        p.parent.mkdir(parents=True, exist_ok=True)
        tmp = p.with_suffix(p.suffix + ".tmp")
        tmp.write_text(json.dumps(payload, ensure_ascii=False, separators=(",", ":")), encoding="utf-8")
        tmp.replace(p)
    except Exception as e:
        print(f"warning: failed to write benchmark state: {e}", file=sys.stderr, flush=True)


def _result_totals(results: list[object], total_tasks: int | None = None) -> dict[str, int]:
    totals = {
        "tasks": len(results) if total_tasks is None else int(total_tasks),
        "passed": 0,
        "failed": 0,
        "skipped": 0,
        "judge_error": 0,
        "timeout": 0,
    }
    for result in results:
        status = str(getattr(result, "status", "") or "")
        if status in totals and status != "tasks":
            totals[status] += 1
    return totals


def _run_exit_code(totals: dict[str, int]) -> int:
    if totals.get("judge_error", 0) or totals.get("timeout", 0):
        return 1
    accounted = (
        totals.get("passed", 0)
        + totals.get("failed", 0)
        + totals.get("skipped", 0)
    )
    return 0 if accounted == totals.get("tasks", 0) else 1


def _selected_task_ids(args: argparse.Namespace) -> list[str]:
    ids: list[str] = []
    for value in list(args.task_id or []) + [args.task_ids or ""]:
        for item in str(value).split(","):
            task_id = item.strip()
            if task_id and task_id not in ids:
                ids.append(task_id)
    return ids


def _selected_skills(args: argparse.Namespace) -> list[str]:
    skills: list[str] = []
    for value in args.skill or []:
        for item in str(value).split(","):
            name = item.strip()
            if name and name not in skills:
                skills.append(name)
    return skills


def _valid_run_id(run_id: str) -> bool:
    return bool(run_id and "/" not in run_id and "\\" not in run_id and run_id not in {".", ".."})


def _task_route_id(args: argparse.Namespace, suite: Suite, task_id: str, attempt: int, repeats: int) -> str:
    explicit = str(args.benchmark_task_id or "").strip()
    if explicit:
        # Caller (e.g., WebUI) provided the full route id already. Trust it
        # verbatim: it avoids double-concatenation like "suite:task:task", and
        # the caller has already started its daemon with this exact id, so an
        # ":attempt-N" suffix here would stop matching once repeats > 1.
        return explicit
    # No explicit id: build "<suite_filename>:<task_id>" as the route id.
    prefix = Path(str(suite.source_path)).name
    route_id = f"{prefix}:{task_id}"
    if repeats > 1:
        route_id = f"{route_id}:attempt-{attempt}"
    return route_id


def _resolve_target_platform(args: argparse.Namespace, *, required: bool = False) -> str:
    platform = _resolve_target_platform_enum(args, required=required)
    return platform.value if platform is not None else ""


def _resolve_target_platform_enum(
    args: argparse.Namespace,
    *,
    required: bool = False,
    health: dict | None = None,
) -> TargetPlatform | None:
    requested = str(getattr(args, "target_platform", "") or "").strip().lower()
    explicit = requested if requested and requested != "auto" else ""
    environment_url = str(getattr(args, "environment_url", "") or "").strip()
    if not environment_url:
        return None
    if health is None:
        try:
            health = _read_environment_health(environment_url)
        except Exception:
            if required:
                raise
            return None
    try:
        return resolve_environment_platform(
            health,
            constraint=explicit or None,
        )
    except ValueError:
        if "platform" in health or required or explicit:
            raise
        return None


def _read_environment_health(environment_url: str) -> dict:
    return read_environment_health(environment_url)


def _preflight_mobilegym_environment(
    environment_url: str,
    *,
    timeout: int = DEFAULT_ENVIRONMENT_SETUP_TIMEOUT_SEC,
    health: dict | None = None,
) -> None:
    preflight_mobilegym_environment(
        environment_url,
        timeout=timeout,
        health=health,
    )


def _task_repeats(args: argparse.Namespace, task: TaskSpec) -> int:
    repeats = args.repeats if args.repeats is not None else task.repeats
    return repeats if repeats > 0 else 1


def _task_platform_skip_reason(task: TaskSpec, target_platform: str) -> str:
    if not target_platform or not task.platforms or target_platform in task.platforms:
        return ""
    return f"task platforms {', '.join(task.platforms)} do not include target platform {target_platform}"


def _build_task_units(
    args: argparse.Namespace,
    suite: Suite,
    target_platform: str,
) -> list[TaskRunUnit]:
    units: list[TaskRunUnit] = []
    uses_mock_environment = _suite_has_mock_environment(suite)
    for task in suite.tasks:
        repeats = _task_repeats(args, task)
        task_target_platform = target_platform
        skip_reason = ""
        if uses_mock_environment:
            try:
                task_target_platform = resolve_mock_task_platform(
                    suite,
                    task,
                    constraint=target_platform or None,
                ).value
            except ValueError as exc:
                skip_reason = str(exc)
        if not skip_reason:
            skip_reason = _task_platform_skip_reason(task, task_target_platform)
        for attempt in range(1, repeats + 1):
            units.append(
                TaskRunUnit(
                    index=len(units) + 1,
                    task=task,
                    attempt=attempt,
                    repeats=repeats,
                    skip_reason=skip_reason,
                    target_platform=task_target_platform,
                )
            )
    return units


def _run_target_platform(units: list[TaskRunUnit], fallback: str = "") -> str:
    platforms = {unit.target_platform for unit in units if unit.target_platform}
    if not platforms:
        return fallback
    if len(platforms) == 1:
        return next(iter(platforms))
    return "mixed"


def _planned_metrics_k(units: list[TaskRunUnit]) -> int:
    """Determine metrics_k from task units.

    Returns the minimum number of repeats across all tasks, which represents
    the k value for fixed-k evaluation (pass@k, pass^k metrics).
    """
    repeats = [unit.repeats for unit in units if unit.repeats > 0]
    return min(repeats, default=1)


def _planned_task_attempts(units: list[TaskRunUnit]) -> list[dict[str, object]]:
    return [
        {"task_id": unit.task.id, "attempt": unit.attempt}
        for unit in units
    ]


def _write_suite_snapshot(run_dir: Path, suite: Suite) -> str:
    snapshot_name = "suite.json"
    run_dir.mkdir(parents=True, exist_ok=True)
    source_bytes = suite.source_bytes or suite.source_path.read_bytes()
    if hashlib.sha256(source_bytes).hexdigest() != suite.sha256:
        raise ValueError("suite source no longer matches its loaded SHA-256")
    (run_dir / snapshot_name).write_bytes(source_bytes)
    return snapshot_name


def _cmd_run_auto_agent_setup(
    args: argparse.Namespace,
    suite: Suite,
    selected_task_ids: list[str],
    target_platform: str,
    run_id: str,
    run_dir: Path,
) -> int:
    mock_server = None
    if _suite_has_mock_environment(suite):
        if args.environment_url:
            print(
                "Error: suites or tasks with mock_environment manage their own environment; "
                "do not pass --environment-url",
                file=sys.stderr,
            )
            return 2
        from runner.mock_environment import MockEnvironmentServer

        initial_spec = suite.mock_environment
        if initial_spec is None:
            initial_spec = next(
                spec
                for task in suite.tasks
                if (spec := task.mock_environment) is not None
            )
        mock_server = MockEnvironmentServer(
            initial_spec,
            suite.source_path.parent,
            # Docker host-gateway access requires a wildcard listener. The mock
            # server protects it with a high-entropy, per-run capability path.
            host="0.0.0.0",  # noqa: S104
        )
        args.environment_url = mock_server.start()
        print(f"mock environment started: {mock_server.redacted_url}", flush=True)
    try:
        return _cmd_run_auto_agent_setup_inner(
            args,
            suite,
            selected_task_ids,
            target_platform,
            run_id,
            run_dir,
            mock_server=mock_server,
        )
    finally:
        if mock_server is not None:
            mock_server.stop()


def _cmd_run_auto_agent_setup_inner(
    args: argparse.Namespace,
    suite: Suite,
    selected_task_ids: list[str],
    target_platform: str,
    run_id: str,
    run_dir: Path,
    mock_server=None,
) -> int:
    if args.repeats is not None and args.repeats <= 0:
        print(f"Error: --repeats must be positive, got {args.repeats}", file=sys.stderr)
        return 2
    if args.max_concurrency < 0:
        print("Error: max-concurrency must be non-negative", file=sys.stderr)
        return 2

    from runner.webui import (
        Job,
        append_log,
        container_boot_failure_detail,
        docker_published_port,
        endpoint_for_docker,
        ensure_daemon_image,
        prepare_run_config,
        read_environment_bridge_concurrency,
        start_daemon_compose,
        start_daemon_logs,
        stop_daemon_compose,
        worker_token,
    )

    run_dir.mkdir(parents=True, exist_ok=True)
    workers_dir = run_dir / "workers"
    workers_dir.mkdir(parents=True, exist_ok=True)
    setup_log = run_dir / "auto-agent-setup.log"

    units = _build_task_units(args, suite, target_platform)
    mock_platform_fallback = target_platform
    if not mock_platform_fallback and suite.mock_environment is not None:
        mock_platform_fallback = suite.mock_environment.platform.value
    manifest_target_platform = (
        _run_target_platform(units, mock_platform_fallback)
        if mock_server is not None
        else target_platform
    )
    total_runs = len(units)
    has_runnable_units = any(not unit.skip_reason for unit in units)
    if has_runnable_units:
        if args.environment_url:
            clear_stale_adb_android_owner(args.environment_url)
        ensure_daemon_image(args.daemon_image, not args.no_build_daemon_image, setup_log)
        concurrency = (
            read_environment_bridge_concurrency(args.environment_url) or 1
            if args.environment_url
            else 1
        )
        if args.max_concurrency > 0:
            concurrency = (
                min(concurrency, args.max_concurrency)
                if args.environment_url
                else args.max_concurrency
            )
    else:
        concurrency = 1
    docker_environment_url = (
        endpoint_for_docker(args.environment_url.rstrip("/"))
        if args.environment_url
        else ""
    )
    judge_cfg = None if args.no_judge else JudgeConfig(model=args.judge_model, base_url=args.judge_base_url)
    active_skills = _selected_skills(args)
    judge_cache = run_dir / "_judge_cache"
    sha, dirty = git_sha(REPO_ROOT)
    started = now_iso()
    display_environment_url = (
        mock_server.redacted_url if mock_server is not None else args.environment_url
    )
    base_state = {
        "status": "running",
        "suite": str(suite.source_path),
        "run_id": run_id,
        "total": total_runs,
        "completed": 0,
        "started_at": started,
        "parallel": min(max(1, concurrency), max(1, total_runs)),
        "auto_agent_setup": True,
        "totals": _result_totals([], total_runs),
    }
    _write_state(args.state_file, base_state)

    agent_config_text = None
    if args.agent_config:
        agent_config_text = Path(args.agent_config).read_text(encoding="utf-8")

    def run_unit(unit: TaskRunUnit):
        progress = f"{unit.index}/{total_runs}"
        art_dir = run_dir / "tasks" / unit.task.id / (
            f"attempt_{unit.attempt}" if unit.repeats > 1 else ""
        )
        if unit.skip_reason:
            print(
                f"[{progress}] SKIPPED    {unit.task.id} attempt={unit.attempt} "
                f"({unit.skip_reason})",
                flush=True,
            )
            return skipped_task_result(
                suite,
                unit.task,
                unit.attempt,
                art_dir,
                run_id,
                unit.skip_reason,
            )
        token = worker_token(str(suite.source_path), f"{unit.task.id}-{unit.attempt}")
        worker_dir = workers_dir / token
        config_dir = worker_dir / "config"
        runner_log = worker_dir / "runner.log"
        daemon_log = worker_dir / "daemon.log"
        worker_dir.mkdir(parents=True, exist_ok=True)
        prepare_run_config(
            Path(args.base_config_dir),
            config_dir,
            agent_config_text=agent_config_text,
        )
        benchmark_token = _read_optional_token(config_dir / "control_token")
        host_port = 0
        agent_url = f"http://127.0.0.1:{host_port}"
        route_id = _task_route_id(
            args,
            suite,
            unit.task.id,
            unit.attempt,
            unit.repeats,
        )
        job = Job(
            id=f"{run_id}-{token}",
            endpoint=args.environment_url.rstrip("/") if args.environment_url else "",
            docker_endpoint=docker_environment_url,
            suites=[str(suite.source_path)],
            environment_endpoint=args.environment_url.rstrip("/") if args.environment_url else "",
            agent_url=agent_url,
            container_name=f"aiden-benchmark-agent-{run_id}-{token}",
            config_dir=str(config_dir),
            runner_log=str(runner_log),
            daemon_log=str(daemon_log),
        )
        log_proc = None
        client = None
        try:
            print(
                f"[{progress}] STARTING   {unit.task.id} attempt={unit.attempt}",
                flush=True,
            )
            task_mock_environment = effective_mock_environment(suite, unit.task)
            if mock_server is not None:
                if task_mock_environment is None:
                    return skipped_task_result(
                        suite,
                        unit.task,
                        unit.attempt,
                        art_dir,
                        run_id,
                        "task has no mock_environment and the suite has no default",
                    )
                mock_server.activate(task_mock_environment)
            container_id = start_daemon_compose(
                job,
                image=args.daemon_image,
                host_port=host_port,
                config_dir=config_dir,
                environment_bridge_endpoint=docker_environment_url,
                benchmark_task_id=route_id,
                device_type=unit.target_platform,
                environment_bridge_mode=bool(args.environment_url),
                log_path=runner_log,
            )
            append_log(runner_log, f"container {container_id}")
            # Stream daemon logs before probing the published port so a
            # container that exits at startup (e.g. config validation failure)
            # still leaves its output in daemon.log.
            log_proc = start_daemon_logs(job, daemon_log)
            try:
                published_port = docker_published_port(container_id, 8080)
            except Exception as exc:
                boot_detail = container_boot_failure_detail(container_id)
                append_log(runner_log, f"daemon failed to publish port 8080: {exc}")
                if boot_detail:
                    append_log(runner_log, boot_detail)
                raise RuntimeError(
                    "daemon container is not serving port 8080: "
                    + (boot_detail or str(exc))
                ) from exc
            job.agent_url = f"http://127.0.0.1:{published_port}"
            client = _new_agent_client(job.agent_url, benchmark_token)
            if not wait_for_agent_ready(client, timeout_sec=args.agent_ready_timeout_sec):
                return skipped_task_result(
                    suite,
                    unit.task,
                    unit.attempt,
                    art_dir,
                    run_id,
                    f"agent not ready within {args.agent_ready_timeout_sec}s",
                )
            if task_mock_environment is not None:
                client.set_phone_bridge_state(task_mock_environment.phone_bridge)
            if not args.skip_clock_wait and not wait_for_agent_clock(
                client,
                timeout_sec=args.clock_timeout_sec,
            ):
                return skipped_task_result(
                    suite,
                    unit.task,
                    unit.attempt,
                    art_dir,
                    run_id,
                    "agent board clock did not sync before benchmark start",
                )
            print(
                f"[{progress}] RUNNING    {unit.task.id} attempt={unit.attempt}",
                flush=True,
            )
            return run_one_task(
                client,
                suite,
                unit.task,
                unit.attempt,
                art_dir,
                judge_cfg,
                judge_cache,
                run_id,
                environment_url=args.environment_url or None,
                benchmark_task_id=route_id,
                active_skills=active_skills,
            )
        except Exception as exc:
            append_log(runner_log, f"ERROR: {exc}")
            return skipped_task_result(
                suite,
                unit.task,
                unit.attempt,
                art_dir,
                run_id,
                str(exc),
            )
        finally:
            if args.environment_url:
                try:
                    call_environment_release(args.environment_url, task_id=route_id)
                except ResetError as exc:
                    print(f"warning: failed to release environment task route for {route_id}: {exc}", file=sys.stderr, flush=True)
            if client is not None:
                client.close()
            stop_daemon_compose(job)
            if log_proc is not None:
                log_proc.terminate()

    results = []
    completed = 0
    max_workers = min(max(1, concurrency), max(1, total_runs))
    print(
        f"auto agent setup enabled: concurrency={max_workers} "
        f"environment={display_environment_url}",
        flush=True,
    )
    with concurrent.futures.ThreadPoolExecutor(max_workers=max_workers, thread_name_prefix=f"bench-cli-{run_id}") as executor:
        future_to_unit = {
            executor.submit(run_unit, unit): unit
            for unit in units
        }
        for future in concurrent.futures.as_completed(future_to_unit):
            unit = future_to_unit[future]
            result = future.result()
            _log_task_result(
                unit.task.id,
                unit.attempt,
                result,
                verbose=args.verbose,
                progress=f"{unit.index}/{total_runs}",
            )
            results.append(result)
            completed += 1
            _write_state(args.state_file, {
                **base_state,
                "completed": completed,
                "current": unit.index,
                "current_task": unit.task.id,
                "current_attempt": unit.attempt,
                "last_result": result.status,
                "totals": _result_totals(results, total_runs),
            })

    totals = _result_totals(results, total_runs)
    suite_snapshot_path = _write_suite_snapshot(run_dir, suite)
    manifest = {
        "run_id": run_id, "git_sha": sha, "git_dirty": dirty,
        "suite_path": str(suite.source_path), "suite_sha256": suite.sha256,
        "suite_snapshot_path": suite_snapshot_path,
        "selected_task_ids": selected_task_ids,
        "agent_url": None,
        "environment_url": display_environment_url or None,
        "agent_model": args.agent_model,
        "active_skills": active_skills,
        "judge_config": {"provider": judge_cfg.provider, "model": args.judge_model, "base_url": args.judge_base_url} if judge_cfg else None,
        "judge_prompt_version": "v1",
        "target_platform": manifest_target_platform or None,
        "auto_agent_setup": True,
        "concurrency": max_workers,
        "mock_environment": _mock_environment_manifest(suite),
        "started_at": started, "finished_at": now_iso(),
        "totals": totals,
        "metrics_schema_version": "p0-v1",
        "metrics_k": _planned_metrics_k(units),
        "planned_task_attempts": _planned_task_attempts(units),
    }
    write_manifest(run_dir / "manifest.json", manifest)
    write_jsonl(run_dir / "results.jsonl", results)
    write_metrics(run_dir / "metrics.json", suite.name, manifest, results)
    write_summary(run_dir / "summary.md", suite.name, manifest, results)
    html = generate_report_html(run_dir)
    (run_dir / "report.html").write_text(html, encoding="utf-8")
    _write_state(args.state_file, {
        **base_state,
        "status": "done",
        "completed": completed,
        "totals": totals,
    })

    print("\n" + "="*60, flush=True)
    print(f"Benchmark Summary - {suite.name}", flush=True)
    print("="*60, flush=True)
    print(f"Total Tasks:   {manifest['totals']['tasks']}", flush=True)
    print(f"Passed:        {manifest['totals']['passed']}", flush=True)
    print(f"Failed:        {manifest['totals']['failed']}", flush=True)
    print(f"Skipped:       {manifest['totals']['skipped']}", flush=True)
    if manifest["totals"]["timeout"] > 0:
        print(f"Timeout:        {manifest['totals']['timeout']}", flush=True)
    if manifest["totals"]["judge_error"] > 0:
        print(f"Judge Error:    {manifest['totals']['judge_error']}", flush=True)
    print(f"Results saved to: {run_dir}", flush=True)
    print("="*60 + "\n", flush=True)
    return _run_exit_code(manifest["totals"])


def _cmd_run(args: argparse.Namespace) -> int:
    suite = load_suite(Path(args.suite))
    selected_task_ids = _selected_task_ids(args)
    if selected_task_ids:
        by_id = {task.id: task for task in suite.tasks}
        missing = [task_id for task_id in selected_task_ids if task_id not in by_id]
        if missing:
            print(f"Error: task id(s) not found in suite: {', '.join(missing)}", file=sys.stderr)
            return 2
        suite.tasks = [by_id[task_id] for task_id in selected_task_ids]

    if _suite_has_mixed_mock_and_real_tasks(suite):
        print(
            "Error: a single run cannot mix mock_environment tasks with real-environment tasks; "
            "select one task type with --task-id",
            file=sys.stderr,
        )
        return 2

    run_id = str(args.run_id or "").strip() or datetime.now(timezone.utc).strftime("%Y-%m-%d_%H%M%S")
    if not _valid_run_id(run_id):
        print(f"Error: invalid --run-id: {run_id!r}", file=sys.stderr)
        return 2
    run_dir = Path(args.out) / run_id
    if args.auto_agent_setup and args.max_concurrency < 0:
        print("Error: max-concurrency must be non-negative", file=sys.stderr)
        return 2
    environment_health = None
    try:
        if args.environment_url:
            environment_health = _read_environment_health(args.environment_url)
        resolved_platform = _resolve_target_platform_enum(
            args,
            required=bool(args.environment_url),
            health=environment_health,
        )
        target_platform = (
            resolved_platform.value if resolved_platform is not None else ""
        )
    except Exception as exc:
        print(f"Error: failed to resolve target platform: {exc}", file=sys.stderr)
        return 2
    if _suite_has_mock_environment(suite) and not args.auto_agent_setup:
        print(
            "Error: suites or tasks with mock_environment require --auto-agent-setup so tool calls "
            "can be routed to the scripted environment",
            file=sys.stderr,
        )
        return 2
    if args.auto_agent_setup and _suite_has_mock_environment(suite):
        requested_platform = str(args.target_platform or "").strip().lower()
        target_platform = "" if requested_platform == "auto" else requested_platform
    if args.repeats is not None and args.repeats <= 0:
        print(f"Error: --repeats must be positive, got {args.repeats}", file=sys.stderr)
        return 2
    if (
        args.environment_url
        and os.environ.get(MOBILEGYM_PREFLIGHT_COMPLETE_ENV) != "1"
    ):
        try:
            _preflight_mobilegym_environment(
                args.environment_url,
                health=environment_health,
            )
        except Exception as exc:
            print(f"Error: environment preflight failed: {exc}", file=sys.stderr)
            return 2
    if run_dir.exists() and (
        not run_dir.is_dir() or any(run_dir.iterdir())
    ):
        print(f"Error: benchmark run directory already exists: {run_dir}", file=sys.stderr)
        return 2
    if args.auto_agent_setup:
        return _cmd_run_auto_agent_setup(
            args,
            suite,
            selected_task_ids,
            target_platform,
            run_id,
            run_dir,
        )
    units = _build_task_units(args, suite, target_platform)
    has_runnable_units = any(not unit.skip_reason for unit in units)
    if args.environment_url:
        clear_stale_adb_android_owner(args.environment_url)
    client = _new_agent_client(args.agent_url, _read_optional_token(args.benchmark_token_file))
    if not client.health():
        print(f"agent at {args.agent_url} is not reachable", file=sys.stderr)
        client.close()
        return 2
    try:
        daemon_platform = resolve_daemon_platform(
            client.device_type(),
            constraint=target_platform or args.target_platform,
        )
    except Exception as exc:
        print(f"failed to validate agent daemon target platform: {exc}", file=sys.stderr)
        client.close()
        return 2
    if args.environment_url and daemon_platform.value != target_platform:
        print(
            f"agent platform {daemon_platform.value!r} does not match "
            f"environment platform {target_platform!r}",
            file=sys.stderr,
        )
        client.close()
        return 2
    if not args.environment_url:
        target_platform = daemon_platform.value
        units = _build_task_units(args, suite, target_platform)
        has_runnable_units = any(not unit.skip_reason for unit in units)
    if (
        has_runnable_units
        and not args.skip_clock_wait
        and not wait_for_agent_clock(client, timeout_sec=args.clock_timeout_sec)
    ):
        print("agent board clock did not sync before benchmark start", file=sys.stderr)
        client.close()
        return 2
    judge_cfg = None if args.no_judge else JudgeConfig(model=args.judge_model, base_url=args.judge_base_url)
    active_skills = _selected_skills(args)
    judge_cache = run_dir / "_judge_cache"
    sha, dirty = git_sha(REPO_ROOT)
    started = now_iso()
    results = []
    # Compute total number of executions (accounting for repeats) for progress display
    total_runs = len(units)
    completed = 0
    base_state = {
        "status": "running",
        "suite": str(suite.source_path),
        "run_id": run_id,
        "total": total_runs,
        "completed": 0,
        "started_at": started,
        "totals": _result_totals(results, total_runs),
    }
    _write_state(args.state_file, base_state)
    try:
        for unit in units:
            current_index = unit.index
            task = unit.task
            attempt = unit.attempt
            n = unit.repeats
            skip_reason = unit.skip_reason
            if skip_reason:
                art_dir = run_dir / "tasks" / task.id / (f"attempt_{attempt}" if n > 1 else "")
                r = skipped_task_result(suite, task, attempt, art_dir, run_id, skip_reason)
                _log_task_result(task.id, attempt, r, verbose=args.verbose, progress=f"{current_index}/{total_runs}")
                results.append(r)
                completed += 1
                _write_state(args.state_file, {
                    **base_state,
                    "completed": completed,
                    "current": current_index,
                    "current_task": task.id,
                    "current_attempt": attempt,
                    "last_result": "skipped",
                    "totals": _result_totals(results, total_runs),
                })
                continue
            task_benchmark_id = _task_route_id(args, suite, task.id, attempt, n)
            try:
                progress = f"{current_index}/{total_runs}"
                _write_state(args.state_file, {
                    **base_state,
                    "completed": completed,
                    "current": current_index,
                    "current_task": task.id,
                    "current_attempt": attempt,
                    "totals": _result_totals(results, total_runs),
                })
                print(f"[{progress}] RUNNING    {task.id} attempt={attempt}", flush=True)
                if not wait_for_agent_ready(
                    client, timeout_sec=args.agent_ready_timeout_sec
                ):
                    print(
                        f"[{progress}] SKIPPED    {task.id} attempt={attempt} "
                        f"rubric=0/{len(task.rubric)} wall=Nonems "
                        f"(agent not ready)",
                        flush=True,
                    )
                    results.append(
                        skipped_task_result(
                            suite, task, attempt,
                            run_dir / "tasks" / task.id
                            / (f"attempt_{attempt}" if n > 1 else ""),
                            run_id,
                            f"agent not ready within {args.agent_ready_timeout_sec}s",
                        )
                    )
                    completed += 1
                    _write_state(args.state_file, {
                        **base_state,
                        "completed": completed,
                        "current": current_index,
                        "current_task": task.id,
                        "current_attempt": attempt,
                        "last_result": "skipped",
                        "totals": _result_totals(results, total_runs),
                    })
                    continue

                art_dir = run_dir / "tasks" / task.id / (f"attempt_{attempt}" if n > 1 else "")
                try:
                    r = run_one_task(client, suite, task, attempt, art_dir,
                                      judge_cfg, judge_cache, run_id,
                                      environment_url=args.environment_url or None,
                                      benchmark_task_id=task_benchmark_id,
                                      active_skills=active_skills)
                except Exception as e:
                    print(f"[{progress}] ERROR      {task.id} attempt={attempt} — {e}", flush=True)
                    r = skipped_task_result(suite, task, attempt, art_dir, run_id, str(e))
                _log_task_result(task.id, attempt, r, verbose=args.verbose, progress=progress)
                results.append(r)
                completed += 1
                _write_state(args.state_file, {
                    **base_state,
                    "completed": completed,
                    "current": current_index,
                    "current_task": task.id,
                    "current_attempt": attempt,
                    "last_result": r.status,
                    "totals": _result_totals(results, total_runs),
                })

                if args.inter_task_cooldown_sec > 0:
                    time.sleep(args.inter_task_cooldown_sec)
            finally:
                if args.environment_url:
                    try:
                        call_environment_release(args.environment_url, task_id=task_benchmark_id)
                    except ResetError as exc:
                        print(f"warning: failed to release environment task route for {task_benchmark_id}: {exc}", file=sys.stderr, flush=True)
    finally:
        if client is not None:
            client.close()
    totals = _result_totals(results, total_runs)
    suite_snapshot_path = _write_suite_snapshot(run_dir, suite)
    manifest = {
        "run_id": run_id, "git_sha": sha, "git_dirty": dirty,
        "suite_path": str(suite.source_path), "suite_sha256": suite.sha256,
        "suite_snapshot_path": suite_snapshot_path,
        "selected_task_ids": selected_task_ids,
        "agent_url": args.agent_url,
        "environment_url": args.environment_url or None,
        "agent_model": args.agent_model,
        "active_skills": active_skills,
        "judge_config": {"provider": judge_cfg.provider, "model": args.judge_model, "base_url": args.judge_base_url} if judge_cfg else None,
        "judge_prompt_version": "v1",
        "target_platform": target_platform or None,
        "mock_environment": _mock_environment_manifest(suite),
        "started_at": started, "finished_at": now_iso(),
        "totals": totals,
        "metrics_schema_version": "p0-v1",
        "metrics_k": _planned_metrics_k(units),
        "planned_task_attempts": _planned_task_attempts(units),
    }
    write_manifest(run_dir / "manifest.json", manifest)
    write_jsonl(run_dir / "results.jsonl", results)
    write_metrics(run_dir / "metrics.json", suite.name, manifest, results)
    write_summary(run_dir / "summary.md", suite.name, manifest, results)
    html = generate_report_html(run_dir)
    (run_dir / "report.html").write_text(html, encoding="utf-8")
    if args.llm_analysis:
        analysis_result = analyze_run(run_dir, REPO_ROOT, AnalysisConfig(
            enabled=True,
            model=args.analysis_model or args.judge_model,
            base_url=args.analysis_base_url or args.judge_base_url,
            max_log_bytes=args.analysis_max_log_bytes,
            max_code_bytes=args.analysis_max_code_bytes,
            timeout_sec=args.analysis_timeout_sec,
            api_key_env=os.environ.get("AIDEN_BENCHMARK_ANALYSIS_API_KEY_ENV") or None,
        ))
        if not analysis_result.ok:
            print(
                f"Warning: benchmark LLM analysis failed: {analysis_result.warning}",
                file=sys.stderr,
                flush=True,
            )
        html = generate_report_html(run_dir)
        (run_dir / "report.html").write_text(html, encoding="utf-8")

    _write_state(args.state_file, {
        **base_state,
        "status": "done",
        "completed": completed,
        "totals": totals,
    })

    # Print final summary
    print("\n" + "="*60, flush=True)
    print(f"📊 Benchmark Summary - {suite.name}", flush=True)
    print("="*60, flush=True)
    print(f"Total Tasks:   {manifest['totals']['tasks']}", flush=True)
    print(f"✅ Passed:     {manifest['totals']['passed']}", flush=True)
    print(f"❌ Failed:     {manifest['totals']['failed']}", flush=True)
    print(f"⏭️  Skipped:    {manifest['totals']['skipped']}", flush=True)
    if manifest['totals']['timeout'] > 0:
        print(f"⏱️  Timeout:    {manifest['totals']['timeout']}", flush=True)
    if manifest['totals']['judge_error'] > 0:
        print(f"⚠️  Judge Error: {manifest['totals']['judge_error']}", flush=True)
    print(f"\n📁 Results saved to: {run_dir}", flush=True)
    print("="*60 + "\n", flush=True)

    if has_runnable_units:
        upload_client = AgentClient(base_url=args.agent_url)
        if upload_report(upload_client, html, run_dir):
            print(f"Report uploaded → http://{args.agent_url.split('//')[1].split(':')[0]}:80/benchmark")
        else:
            print("Warning: failed to upload report to board")
        upload_client.close()
    return _run_exit_code(manifest["totals"])


def _mock_environment_manifest(suite: Suite) -> dict[str, object] | None:
    task_specs = {
        task.id: _serialize_mock_environment(task.mock_environment)
        for task in suite.tasks
        if task.mock_environment is not None
    }
    if suite.mock_environment is None and not task_specs:
        return None
    if suite.mock_environment is not None and not task_specs:
        return _serialize_mock_environment(suite.mock_environment)
    return {
        "default": _serialize_mock_environment(suite.mock_environment),
        "tasks": task_specs,
    }


def _serialize_mock_environment(spec) -> dict[str, object] | None:
    if spec is None:
        return None
    return {
        "platform": spec.platform,
        "single_frame": spec.single_frame,
        "phone_bridge": dict(spec.phone_bridge),
        "tools": sorted(spec.tools),
        "screen": spec.screen,
        "screen_text": spec.screen_text,
    }


def _suite_has_mock_environment(suite: Suite) -> bool:
    return suite.mock_environment is not None or any(
        task.mock_environment is not None for task in suite.tasks
    )


def _suite_has_mixed_mock_and_real_tasks(suite: Suite) -> bool:
    if suite.mock_environment is not None:
        return False
    has_mock = any(task.mock_environment is not None for task in suite.tasks)
    has_real = any(task.mock_environment is None for task in suite.tasks)
    return has_mock and has_real


def _read_optional_token(path: str | Path | None) -> str:
    raw = str(path or "").strip()
    if not raw:
        return ""
    try:
        token = Path(raw).read_text(encoding="utf-8").strip()
    except OSError as exc:
        raise ValueError(f"unable to read benchmark token file {raw!r}: {exc}") from exc
    if not token:
        raise ValueError(f"benchmark token file {raw!r} is empty")
    return token


def _new_agent_client(base_url: str, benchmark_token: str = "") -> AgentClient:
    if benchmark_token:
        return AgentClient(base_url=base_url, benchmark_token=benchmark_token)
    return AgentClient(base_url=base_url)


if __name__ == "__main__":
    sys.exit(cli())
