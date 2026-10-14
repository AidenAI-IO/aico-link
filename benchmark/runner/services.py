from __future__ import annotations

import argparse
import json
import os
import re
import subprocess
import sys
import time
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

from runner.agent_client import AgentClient
from runner.platform import (
    normalize_target_platform,
    read_environment_health,
    resolve_daemon_platform,
    resolve_environment_platform,
)
from runner.adb_android_environment import (
    DEFAULT_ADB_BRIDGE_READY_TIMEOUT_SEC,
    DEFAULT_ADB_SERIAL,
    endpoint_for_docker_host,
    reserve_free_port,
    start_adb_bridge_process,
    terminate_pid,
)
from runner.desktop_environment import pid_alive as desktop_pid_alive, start_desktop_bridge_process, terminate_pid as terminate_desktop_pid, wait_for_desktop_bridge
from runner.webui import (
    BENCHMARK_ROOT,
    DEFAULT_BASE_CONFIG_DIR,
    DEFAULT_DAEMON_IMAGE,
    DEFAULT_DAEMON_READY_TIMEOUT_SEC,
    DEFAULT_MOBILEGYM_IMAGE,
    DEFAULT_MOBILEGYM_READY_TIMEOUT_SEC,
    Job,
    append_log,
    build_mobilegym_environment_command,
    daemon_compose_command,
    docker_published_port,
    endpoint_for_docker,
    ensure_daemon_image,
    ensure_mobilegym_image,
    prepare_run_config,
    start_daemon_compose,
    stop_daemon_compose,
    wait_for_http_health,
)
from runner.environment import mobilegym_bridge_bind_host


DEFAULT_CLI_RUNS_DIR = BENCHMARK_ROOT / "runs" / "cli-services"
DEFAULT_CLI_BENCHMARK_TASK_ID = "cli-task"


def add_service_parsers(subparsers: argparse._SubParsersAction[argparse.ArgumentParser]) -> None:
    p_agent = subparsers.add_parser(
        "start-agent-daemon",
        help="Start an isolated benchmark agent daemon Docker container.",
    )
    p_agent.add_argument("--port", type=int, default=0, help="Host port for the daemon API (default: auto)")
    p_agent.add_argument("--name", default="", help="Stable name suffix for the service/container")
    p_agent.add_argument("--runs-dir", default=str(DEFAULT_CLI_RUNS_DIR))
    p_agent.add_argument("--base-config-dir", default=str(DEFAULT_BASE_CONFIG_DIR))
    p_agent.add_argument("--agent-config", default="", help="Optional agent.toml to use instead of the base config template")
    p_agent.add_argument("--daemon-image", default=DEFAULT_DAEMON_IMAGE)
    p_agent.add_argument("--no-build-daemon-image", action="store_true")
    p_agent.add_argument("--environment-bridge-endpoint", default="", help="Environment bridge endpoint to forward device tools to")
    p_agent.add_argument(
        "--device-type",
        default="",
        help="Optional daemon basic_settings.device.device_type override; defaults to agent.toml without an environment bridge",
    )
    p_agent.add_argument(
        "--target-platform",
        default="",
        choices=["", "ios", "android", "mac", "windows", "linux"],
        help=argparse.SUPPRESS,
    )
    p_agent.add_argument("--benchmark-task-id", default=DEFAULT_CLI_BENCHMARK_TASK_ID)
    p_agent.add_argument("--ready-timeout-sec", type=int, default=DEFAULT_DAEMON_READY_TIMEOUT_SEC)
    p_agent.add_argument("--json", action="store_true", help="Print machine-readable JSON")

    p_mobilegym = subparsers.add_parser(
        "start-mobilegym-env",
        help="Start a MobileGym simulator and bridge environment Docker container.",
    )
    p_mobilegym.add_argument("--name", default="", help="Stable name suffix for the environment container")
    p_mobilegym.add_argument("--runs-dir", default=str(DEFAULT_CLI_RUNS_DIR))
    p_mobilegym.add_argument("--envs", "--parallel-envs", dest="parallel_envs", type=int, default=1)
    p_mobilegym.add_argument("--web-port", type=int, default=0, help="Host port for the MobileGym web UI (default: auto)")
    p_mobilegym.add_argument("--bridge-port", type=int, default=0, help="Host port for the bridge API (default: auto)")
    p_mobilegym.add_argument("--mobilegym-image", default=DEFAULT_MOBILEGYM_IMAGE)
    p_mobilegym.add_argument("--no-build-mobilegym-image", action="store_true")
    p_mobilegym.add_argument("--ready-timeout-sec", type=int, default=DEFAULT_MOBILEGYM_READY_TIMEOUT_SEC)
    p_mobilegym.add_argument("--json", action="store_true", help="Print machine-readable JSON")

    p_adb = subparsers.add_parser(
        "start-adb-android-env",
        help="Start a local ADB Android environment bridge process.",
    )
    p_adb.add_argument("--adb-serial", default=os.environ.get("ANDROID_SERIAL", DEFAULT_ADB_SERIAL),
                       help="adb device serial, e.g. 127.0.0.1:6555")
    p_adb.add_argument("--adb-path", default="adb")
    p_adb.add_argument("--name", default="", help="Stable name suffix for the environment")
    p_adb.add_argument("--runs-dir", default=str(DEFAULT_CLI_RUNS_DIR))
    p_adb.add_argument("--bridge-host", default="127.0.0.1")
    p_adb.add_argument("--bridge-port", type=int, default=0, help="Host port for the bridge API (default: auto)")
    p_adb.add_argument("--ready-timeout-sec", type=int, default=DEFAULT_ADB_BRIDGE_READY_TIMEOUT_SEC)
    p_adb.add_argument("--json", action="store_true", help="Print machine-readable JSON")

    p_desktop = subparsers.add_parser(
        "start-desktop-env",
        help="Start a host desktop environment bridge process.",
    )
    p_desktop.add_argument("--name", default="", help="Stable name suffix for the environment")
    p_desktop.add_argument("--runs-dir", default=str(DEFAULT_CLI_RUNS_DIR))
    p_desktop.add_argument("--bridge-host", default="127.0.0.1")
    p_desktop.add_argument("--bridge-port", type=int, default=0)
    p_desktop.add_argument("--backend", choices=["auto", "pyautogui"], default="auto")
    p_desktop.add_argument("--screenshot-command", default="")
    p_desktop.add_argument("--ready-timeout-sec", type=int, default=30)
    p_desktop.add_argument("--json", action="store_true", help="Print machine-readable JSON")


def cmd_start_agent_daemon(args: argparse.Namespace) -> int:
    if args.port < 0:
        print("Error: --port must be zero or positive", file=sys.stderr)
        return 2
    if args.ready_timeout_sec <= 0:
        print("Error: --ready-timeout-sec must be positive", file=sys.stderr)
        return 2

    host_port = int(args.port or 0)
    service_id = _service_id("agent", args.name)
    project = f"aiden-benchmark-agent-{service_id}"
    service_dir = Path(args.runs_dir) / service_id
    config_dir = service_dir / "config"
    log_path = service_dir / "agent-daemon.log"

    # Check if a service with this ID already exists and is still running.
    # For Docker-based agent daemons, check if the container is still active.
    try:
        result = subprocess.run(
            ["docker", "inspect", "--format", "{{.State.Running}}", project],
            capture_output=True,
            text=True,
            check=False,
        )
        if result.returncode == 0 and result.stdout.strip().lower() == "true":
            print(
                f"Error: service ID '{service_id}' already exists with running container '{project}'.\n"
                f"Stop it first (docker stop {project}) or use a different --name.",
                file=sys.stderr,
            )
            return 2
    except FileNotFoundError:
        pass  # docker not available, proceed

    service_dir.mkdir(parents=True, exist_ok=True)

    environment_bridge_endpoint = str(args.environment_bridge_endpoint or "").strip().rstrip("/")
    requested_device_type = str(getattr(args, "device_type", "") or "").strip()
    requested_platform = str(getattr(args, "target_platform", "") or "").strip()
    try:
        device_type_constraint = _resolve_device_type_constraint(
            requested_device_type,
            requested_platform,
        )
    except ValueError as exc:
        print(f"Error: invalid device type override: {exc}", file=sys.stderr)
        return 2
    target_platform = ""
    if environment_bridge_endpoint:
        try:
            resolution = resolve_environment_platform(
                read_environment_health(environment_bridge_endpoint),
                constraint=device_type_constraint or None,
            )
            target_platform = resolution.value
        except Exception as exc:
            print(f"Error: failed to resolve environment platform: {exc}", file=sys.stderr)
            return 2
    elif device_type_constraint:
        target_platform = device_type_constraint
    try:
        agent_config_text = None
        if args.agent_config:
            agent_config_text = Path(args.agent_config).read_text(encoding="utf-8")
        prepare_run_config(
            Path(args.base_config_dir),
            config_dir,
            agent_config_text=agent_config_text,
        )
    except (OSError, ValueError) as exc:
        print(f"Error: failed to prepare agent config: {exc}", file=sys.stderr)
        return 2
    docker_environment_bridge_endpoint = endpoint_for_docker(environment_bridge_endpoint) if environment_bridge_endpoint else ""
    benchmark_task_id = str(args.benchmark_task_id or "").strip()
    agent_url = f"http://127.0.0.1:{host_port}"
    job = Job(
        id=service_id,
        endpoint=environment_bridge_endpoint,
        docker_endpoint=docker_environment_bridge_endpoint,
        suites=[],
        agent_url=agent_url,
        container_name=project,
        config_dir=str(config_dir),
        runner_log=str(log_path),
    )

    try:
        ensure_daemon_image(args.daemon_image, not args.no_build_daemon_image, log_path)
        container_id = start_daemon_compose(
            job,
            image=args.daemon_image,
            host_port=host_port,
            config_dir=config_dir,
            environment_bridge_endpoint=docker_environment_bridge_endpoint,
            benchmark_task_id=benchmark_task_id if docker_environment_bridge_endpoint else "",
            device_type=target_platform if environment_bridge_endpoint or device_type_constraint else "",
            environment_bridge_mode=bool(docker_environment_bridge_endpoint),
            log_path=log_path,
        )
        if host_port == 0:
            host_port = docker_published_port(container_id, 8080)
            agent_url = f"http://127.0.0.1:{host_port}"
            job.agent_url = agent_url
        _wait_for_agent(agent_url, args.ready_timeout_sec)
        client = AgentClient(agent_url)
        try:
            effective_device_type = client.device_type()
            effective_platform = resolve_daemon_platform(
                effective_device_type,
                constraint=target_platform or None,
            )
        finally:
            client.close()
        target_platform = effective_platform.value
    except Exception as exc:
        stop_daemon_compose(job)
        append_log(log_path, f"ERROR: {exc}")
        print(f"Error: failed to start agent daemon: {exc}", file=sys.stderr)
        return 1

    payload = {
        "type": "agent-daemon",
        "agent_url": agent_url,
        "container_id": container_id,
        "compose_project": project,
        "config_dir": str(config_dir),
        "log_path": str(log_path),
        "environment_bridge_endpoint": environment_bridge_endpoint,
        "docker_environment_bridge_endpoint": docker_environment_bridge_endpoint,
        "benchmark_task_id": benchmark_task_id if docker_environment_bridge_endpoint else "",
        "device_type": effective_device_type,
        "target_platform": target_platform,
        "stop_command": " ".join(
            daemon_compose_command(
                "down",
                "--volumes",
                "--remove-orphans",
                project=project,
            )
        ),
    }
    payload["run_command"] = _agent_run_command(payload)
    _print_agent_payload(payload, json_output=bool(args.json))
    return 0


def cmd_start_mobilegym_env(args: argparse.Namespace) -> int:
    if args.parallel_envs <= 0:
        print("Error: --envs must be positive", file=sys.stderr)
        return 2
    if args.web_port < 0 or args.bridge_port < 0:
        print("Error: --web-port and --bridge-port must be zero or positive", file=sys.stderr)
        return 2
    if args.ready_timeout_sec <= 0:
        print("Error: --ready-timeout-sec must be positive", file=sys.stderr)
        return 2

    web_port = int(args.web_port or 0)
    bridge_port = int(args.bridge_port or 0)
    service_id = _service_id("mobilegym", args.name)
    container_name = f"aiden-mobilegym-env-{service_id}"
    service_dir = Path(args.runs_dir) / service_id
    log_path = service_dir / "mobilegym-env.log"
    service_dir.mkdir(parents=True, exist_ok=True)

    try:
        ensure_mobilegym_image(args.mobilegym_image, not args.no_build_mobilegym_image, log_path)
        bridge_bind_host = mobilegym_bridge_bind_host()
        command = build_mobilegym_environment_command(
            image=args.mobilegym_image,
            container_name=container_name,
            host_web_port=web_port,
            host_bridge_port=bridge_port,
            bridge_bind_host=bridge_bind_host,
            benchmark_dir=BENCHMARK_ROOT,
            parallel_envs=args.parallel_envs,
        )
        append_log(log_path, "$ " + " ".join(command))
        container_id = subprocess.check_output(command, cwd=BENCHMARK_ROOT.parent, text=True).strip()
        if web_port == 0:
            web_port = docker_published_port(container_name, 4173)
        if bridge_port == 0:
            bridge_port = docker_published_port(container_name, 9090)
        public_endpoint = f"http://{bridge_bind_host}:{bridge_port}"
        docker_endpoint = f"http://host.docker.internal:{bridge_port}"
        wait_for_http_health(f"{public_endpoint}/health", args.ready_timeout_sec)
    except Exception as exc:
        subprocess.run(["docker", "rm", "-f", container_name], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=False)
        append_log(log_path, f"ERROR: {exc}")
        print(f"Error: failed to start MobileGym environment: {exc}", file=sys.stderr)
        return 1

    payload = {
        "type": "mobilegym-env",
        "environment_url": public_endpoint,
        "docker_environment_url": docker_endpoint,
        "web_url": f"http://127.0.0.1:{web_port}",
        "container_id": container_id,
        "container_name": container_name,
        "parallel_envs": args.parallel_envs,
        "log_path": str(log_path),
        "logs_command": f"docker logs -f {container_name}",
        "stop_command": f"docker rm -f {container_name}",
    }
    payload["agent_daemon_command"] = (
        "uv run python -m runner start-agent-daemon "
        f"--environment-bridge-endpoint {public_endpoint}"
    )
    _print_mobilegym_payload(payload, json_output=bool(args.json))
    return 0


def cmd_start_adb_android_env(args: argparse.Namespace) -> int:
    if args.bridge_port < 0:
        print("Error: --bridge-port must be zero or positive", file=sys.stderr)
        return 2
    if args.ready_timeout_sec <= 0:
        print("Error: --ready-timeout-sec must be positive", file=sys.stderr)
        return 2
    serial = str(args.adb_serial or "").strip()
    if not serial:
        print("Error: --adb-serial is required (or set ANDROID_SERIAL)", file=sys.stderr)
        return 2

    service_id = _service_id("adb-android", args.name)
    service_dir = Path(args.runs_dir) / service_id
    log_path = service_dir / "adb-android-env.log"
    pid_path = service_dir / "adb-android.pid"
    manifest_path = service_dir / "adb-android.json"

    # Check if a service with this ID already exists and is still running.
    if pid_path.exists():
        try:
            existing_pid = int(pid_path.read_text(encoding="utf-8").strip())
            from runner.adb_android_environment import pid_alive
            if pid_alive(existing_pid):
                print(
                    f"Error: service ID '{service_id}' already exists with running process {existing_pid}.\n"
                    f"Stop it first (kill {existing_pid}) or use a different --name.",
                    file=sys.stderr,
                )
                return 2
        except (ValueError, OSError):
            pass  # Stale or corrupted pid file, proceed with cleanup

    service_dir.mkdir(parents=True, exist_ok=True)

    bridge_port = int(args.bridge_port or 0) or reserve_free_port()
    environment_url = f"http://{args.bridge_host}:{bridge_port}"
    docker_environment_url = endpoint_for_docker_host(environment_url)

    pid = 0
    try:
        proc = start_adb_bridge_process(
            serial=serial,
            bridge_port=bridge_port,
            log_path=log_path,
            adb_path=args.adb_path,
            bridge_host=args.bridge_host,
        )
        pid = proc.pid
        pid_path.write_text(str(pid), encoding="utf-8")
        manifest_path.write_text(
            json.dumps(
                {
                    "id": service_id,
                    "pid": pid,
                    "serial": serial,
                    "bridge_port": bridge_port,
                    "public_endpoint": environment_url,
                    "log_path": str(log_path),
                    "created_at": datetime.now(timezone.utc).isoformat(),
                },
                ensure_ascii=False,
                indent=2,
            ),
            encoding="utf-8",
        )
        wait_for_http_health(f"{environment_url}/health", args.ready_timeout_sec)
    except Exception as exc:
        terminate_pid(pid)
        append_log(log_path, f"ERROR: {exc}")
        print(f"Error: failed to start ADB Android environment: {exc}", file=sys.stderr)
        return 1

    payload = {
        "type": "adb-android-env",
        "environment_url": environment_url,
        "docker_environment_url": docker_environment_url,
        "adb_serial": serial,
        "pid": pid,
        "pid_path": str(pid_path),
        "log_path": str(log_path),
        "manifest_path": str(manifest_path),
        "stop_command": f"kill -TERM {pid}",
        "agent_daemon_command": (
            "uv run python -m runner start-agent-daemon "
            f"--environment-bridge-endpoint {environment_url}"
        ),
    }
    _print_adb_android_payload(payload, json_output=bool(args.json))
    return 0


def cmd_start_desktop_env(args: argparse.Namespace) -> int:
    if args.bridge_port < 0 or args.bridge_port > 65535:
        print("Error: --bridge-port must be between 0 and 65535", file=sys.stderr); return 2
    if args.ready_timeout_sec <= 0:
        print("Error: --ready-timeout-sec must be positive", file=sys.stderr); return 2
    service_id = _service_id("desktop", args.name)
    service_dir = Path(args.runs_dir) / service_id
    log_path = service_dir / "desktop-env.log"; pid_path = service_dir / "desktop.pid"
    if pid_path.exists():
        try:
            existing_pid = int(pid_path.read_text(encoding="utf-8").strip())
        except (ValueError, OSError):
            existing_pid = 0
        if existing_pid > 0 and desktop_pid_alive(existing_pid):
            print(
                f"Error: service ID '{service_id}' already exists with running process {existing_pid}.\n"
                f"Stop it first (kill {existing_pid}) or use a different --name.",
                file=sys.stderr,
            )
            return 2
        try:
            pid_path.unlink()
        except FileNotFoundError:
            pass
        except OSError as exc:
            print(f"Error: failed to remove stale desktop pid file {pid_path}: {exc}", file=sys.stderr)
            return 2
    service_dir.mkdir(parents=True, exist_ok=True)
    bridge_port = int(args.bridge_port or 0)
    if bridge_port == 0:
        bridge_port = reserve_free_port()
    endpoint = f"http://{args.bridge_host}:{bridge_port}"
    pid = 0
    try:
        proc = start_desktop_bridge_process(bridge_host=args.bridge_host, bridge_port=bridge_port, backend=args.backend, screenshot_command=args.screenshot_command, log_path=log_path)
        pid = proc.pid; pid_path.write_text(str(pid), encoding="utf-8")
        wait_for_desktop_bridge(endpoint, args.ready_timeout_sec)
    except Exception as exc:
        terminate_desktop_pid(pid); append_log(log_path, f"ERROR: {exc}")
        print(f"Error: failed to start desktop environment: {exc}", file=sys.stderr); return 1
    docker_endpoint = endpoint_for_docker_host(endpoint)
    payload = {"type": "desktop-env", "environment_url": endpoint, "docker_environment_url": docker_endpoint, "platform": _desktop_platform(), "backend": args.backend, "pid": pid, "pid_path": str(pid_path), "log_path": str(log_path), "stop_command": f"kill -TERM {pid}", "agent_daemon_command": f"uv run python -m runner start-agent-daemon --environment-bridge-endpoint {endpoint} --device-type {_desktop_device_type()}"}
    if args.json: print(json.dumps(payload, ensure_ascii=False, indent=2), flush=True)
    else:
        print("Desktop environment started", flush=True)
        for key in ("environment_url", "platform", "backend", "pid", "log_path", "stop_command", "agent_daemon_command"): _print_kv(key, payload[key])
        print(f"\nexport AIDEN_ENVIRONMENT_URL={endpoint}", flush=True)
    return 0


def _desktop_platform() -> str:
    import platform
    system = platform.system().lower()
    return "mac" if system == "darwin" else "windows" if system == "windows" else "linux"


def _desktop_device_type() -> str:
    platform_name = _desktop_platform()
    return "macOS" if platform_name == "mac" else platform_name


def _print_adb_android_payload(payload: dict[str, Any], *, json_output: bool) -> None:
    if json_output:
        print(json.dumps(payload, ensure_ascii=False, indent=2), flush=True)
        return
    print("ADB Android environment started", flush=True)
    _print_kv("environment_url", payload["environment_url"])
    _print_kv("docker_environment_url", payload["docker_environment_url"])
    _print_kv("adb_serial", payload["adb_serial"])
    _print_kv("pid", payload["pid"])
    _print_kv("pid_path", payload["pid_path"])
    _print_kv("log_path", payload["log_path"])
    _print_kv("stop_command", payload["stop_command"])
    _print_kv("agent_daemon_command", payload["agent_daemon_command"])
    print("", flush=True)
    print(f"export AIDEN_ENVIRONMENT_URL={payload['environment_url']}", flush=True)


def _wait_for_agent(agent_url: str, timeout_sec: int) -> None:
    client = AgentClient(agent_url)
    deadline = time.monotonic() + max(1, timeout_sec)
    try:
        while time.monotonic() < deadline:
            if client.health():
                return
            time.sleep(1)
    finally:
        client.close()
    raise RuntimeError(f"agent daemon did not become ready at {agent_url}")


def _service_id(prefix: str, name: str) -> str:
    raw = str(name or "").strip() or datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    slug = re.sub(r"[^a-z0-9_-]+", "-", raw.lower()).strip("-_")
    if not slug:
        slug = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    if slug.startswith(f"{prefix}-"):
        return slug
    return f"{prefix}-{slug}"


def _resolve_device_type_constraint(device_type: str, legacy_target_platform: str) -> str:
    requested_device_type = str(device_type or "").strip()
    requested_legacy_platform = str(legacy_target_platform or "").strip()
    if not requested_device_type and not requested_legacy_platform:
        return ""

    resolved_device_type = (
        normalize_target_platform(requested_device_type, field="device type")
        if requested_device_type
        else None
    )
    resolved_legacy_platform = (
        normalize_target_platform(requested_legacy_platform)
        if requested_legacy_platform
        else None
    )
    if (
        resolved_device_type is not None
        and resolved_legacy_platform is not None
        and resolved_device_type != resolved_legacy_platform
    ):
        raise ValueError(
            "--device-type and deprecated --target-platform disagree: "
            f"{resolved_device_type.value} != {resolved_legacy_platform.value}"
        )
    return (resolved_device_type or resolved_legacy_platform).value


def _print_agent_payload(payload: dict[str, Any], *, json_output: bool) -> None:
    if json_output:
        print(json.dumps(payload, ensure_ascii=False, indent=2), flush=True)
        return
    print("Agent daemon started", flush=True)
    _print_kv("agent_url", payload["agent_url"])
    if payload.get("environment_bridge_endpoint"):
        _print_kv("environment_bridge_endpoint", payload["environment_bridge_endpoint"])
        _print_kv("docker_environment_bridge_endpoint", payload["docker_environment_bridge_endpoint"])
        _print_kv("benchmark_task_id", payload["benchmark_task_id"])
    _print_kv("container_id", payload["container_id"])
    _print_kv("device_type", payload["device_type"])
    _print_kv("compose_project", payload["compose_project"])
    _print_kv("config_dir", payload["config_dir"])
    _print_kv("log_path", payload["log_path"])
    _print_kv("stop_command", payload["stop_command"])
    _print_kv("run_command", payload["run_command"])
    print("", flush=True)
    print(f"export AIDEN_AGENT_URL={payload['agent_url']}", flush=True)
    if payload.get("benchmark_task_id"):
        print(f"export AIDEN_BENCHMARK_TASK_ID={payload['benchmark_task_id']}", flush=True)


def _print_mobilegym_payload(payload: dict[str, Any], *, json_output: bool) -> None:
    if json_output:
        print(json.dumps(payload, ensure_ascii=False, indent=2), flush=True)
        return
    print("MobileGym environment started", flush=True)
    _print_kv("environment_url", payload["environment_url"])
    _print_kv("docker_environment_url", payload["docker_environment_url"])
    _print_kv("web_url", payload["web_url"])
    _print_kv("parallel_envs", payload["parallel_envs"])
    _print_kv("container_id", payload["container_id"])
    _print_kv("container_name", payload["container_name"])
    _print_kv("log_path", payload["log_path"])
    _print_kv("logs_command", payload["logs_command"])
    _print_kv("stop_command", payload["stop_command"])
    _print_kv("agent_daemon_command", payload["agent_daemon_command"])
    print("", flush=True)
    print(f"export AIDEN_ENVIRONMENT_URL={payload['environment_url']}", flush=True)
    if payload.get("benchmark_task_id"):
        print(f"export AIDEN_BENCHMARK_TASK_ID={payload['benchmark_task_id']}", flush=True)


def _print_kv(key: str, value: Any) -> None:
    print(f"{key}={value}", flush=True)


def _agent_run_command(payload: dict[str, Any]) -> str:
    parts = [
        "uv run python -m runner run",
        "--suite <suite.json>",
        f"--agent-url {payload['agent_url']}",
    ]
    if payload.get("config_dir"):
        parts.append(f"--benchmark-token-file {Path(payload['config_dir']) / 'control_token'}")
    if payload.get("environment_bridge_endpoint"):
        parts.append(f"--environment-url {payload['environment_bridge_endpoint']}")
    if payload.get("benchmark_task_id"):
        parts.append(f"--benchmark-task-id {payload['benchmark_task_id']}")
    if payload.get("target_platform") and not payload.get("environment_bridge_endpoint"):
        parts.append(f"--target-platform {payload['target_platform']}")
    return " ".join(parts)
