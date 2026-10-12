"""Shared environment management for Benchmark and SkillOpt WebUIs.

This module provides MobileGym environment management functionality that can be
used by both benchmark/runner/webui.py and skillopt/webui.py.
"""

from __future__ import annotations

import configparser
import dataclasses as dc
import json
import os
import re
import subprocess
import threading
import time
import urllib.parse
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Mapping


DEFAULT_MOBILEGYM_IMAGE = "aiden-mobilegym-simulator:py311"
DEFAULT_MOBILEGYM_READY_TIMEOUT_SEC = 120
DEFAULT_MOBILEGYM_PARALLEL_ENVS = 5
MOBILEGYM_SUBMODULE_PATH = "benchmark/mobilegym/vendor/mobilegym"
MOBILEGYM_SUBMODULE_SECTION = f'submodule "{MOBILEGYM_SUBMODULE_PATH}"'
MOBILEGYM_COMMIT_LABEL = "io.aiden.mobilegym.commit"
LOG_TAIL_BYTES = 96 * 1024
# WebUI polls list_all every 5s; cache the docker-sync result briefly so multiple
# concurrent requests within a single poll window don't each fork docker.
DOCKER_SYNC_CACHE_TTL_SEC = 2.0


@dc.dataclass(frozen=True)
class MobileGymSource:
    repo: str
    commit: str


@dc.dataclass
class MobileGymEnvironment:
    id: str
    name: str
    endpoint: str
    public_endpoint: str
    web_url: str
    status: str = "starting"
    message: str = ""
    created_at: str = ""
    started_at: str = ""
    stopped_at: str = ""
    container_name: str = ""
    container_id: str = ""
    bridge_port: int = 0
    web_port: int = 0
    image: str = ""
    log_path: str = ""
    type: str = "mobilegym"
    parallel_envs: int = DEFAULT_MOBILEGYM_PARALLEL_ENVS


class EnvironmentManager:
    """Manages MobileGym environments for WebUI applications.

    This class provides thread-safe management of MobileGym Docker containers,
    including starting, stopping, and monitoring environments.
    """

    def __init__(
        self,
        runs_dir: Path,
        mobilegym_image: str = DEFAULT_MOBILEGYM_IMAGE,
        build_mobilegym_image: bool = True,
        ready_timeout_sec: int = DEFAULT_MOBILEGYM_READY_TIMEOUT_SEC,
        repo_root: Path | None = None,
    ):
        self.runs_dir = runs_dir
        self.mobilegym_image = mobilegym_image
        self.build_mobilegym_image = build_mobilegym_image
        self.ready_timeout_sec = ready_timeout_sec
        self.repo_root = repo_root or Path(__file__).resolve().parents[2]

        self._lock = threading.RLock()
        self._environments: dict[str, MobileGymEnvironment] = {}
        self._log_procs: dict[str, subprocess.Popen] = {}
        self._last_sync_monotonic: float = 0.0
        self._sync_in_progress = False
        self._sync_condition = threading.Condition(self._lock)

        # Discover existing MobileGym containers from previous WebUI sessions
        self._discover_existing_containers()

    def _discover_existing_containers(self) -> None:
        """Scan Docker for existing MobileGym containers and register them.

        This handles the case where WebUI was restarted but Docker containers
        from previous sessions are still running. Without this, those containers
        become orphans that can't be stopped or removed via the WebUI.
        """
        for container in self._list_docker_containers():
            if container["name"] in {
                env.container_name for env in self._environments.values()
            }:
                continue
            env = self._build_recovered_env(container)
            if env is not None:
                self._environments[env.id] = env

    def list_all(self) -> list[MobileGymEnvironment]:
        """List all MobileGym environments.

        Syncs with Docker state on each call to handle:
        - Containers removed by other WebUI processes (will be dropped from list)
        - New containers started elsewhere (will be discovered)

        Sync results are cached for DOCKER_SYNC_CACHE_TTL_SEC so that concurrent
        WebUI polls (every 5s) don't each fork docker.
        """
        self._sync_with_docker()
        with self._lock:
            return list(self._environments.values())

    def _sync_with_docker(self, *, force: bool = False) -> None:
        """Reconcile in-memory state with actual Docker container state."""
        with self._lock:
            if not force and (time.monotonic() - self._last_sync_monotonic) < DOCKER_SYNC_CACHE_TTL_SEC:
                return
            while self._sync_in_progress:
                self._sync_condition.wait()
                if not force and (time.monotonic() - self._last_sync_monotonic) < DOCKER_SYNC_CACHE_TTL_SEC:
                    return
            self._sync_in_progress = True
        # Run the docker call outside the lock; it can block for seconds if
        # the daemon is slow, and we don't need to serialize concurrent callers.
        try:
            current_containers = self._list_docker_containers()
            current_names = {c["name"] for c in current_containers}

            with self._lock:
                # Remove environments whose containers no longer exist
                # Skip envs in transient states (building/starting/stopping) to avoid
                # racing with our own start/stop operations.
                stale_ids = []
                for env_id, env in self._environments.items():
                    if env.container_name and env.container_name not in current_names:
                        if env.status in ("building", "starting", "stopping"):
                            continue  # In-flight, leave alone
                        stale_ids.append(env_id)
                for env_id in stale_ids:
                    self._environments.pop(env_id, None)
                    self._log_procs.pop(env_id, None)

                # Add containers that exist in Docker but not in our memory
                known_names = {env.container_name for env in self._environments.values()}
                for container in current_containers:
                    name = container["name"]
                    if name in known_names:
                        continue
                    env = self._build_recovered_env(container)
                    if env is not None:
                        self._environments[env.id] = env

                self._last_sync_monotonic = time.monotonic()
        finally:
            with self._lock:
                self._sync_in_progress = False
                self._sync_condition.notify_all()

    def _list_docker_containers(self) -> list[dict[str, str]]:
        """List existing MobileGym Docker containers."""
        try:
            result = subprocess.run(
                [
                    "docker",
                    "ps",
                    "--filter",
                    "name=aiden-mobilegym-env-mg-",
                    "--format",
                    "{{.Names}}\t{{.ID}}\t{{.CreatedAt}}\t{{.Image}}",
                ],
                capture_output=True,
                text=True,
                timeout=5,
                check=False,
            )
        except (subprocess.SubprocessError, OSError):
            return []
        if result.returncode != 0:
            return []
        containers = []
        for line in result.stdout.strip().splitlines():
            parts = line.split("\t")
            if len(parts) < 4:
                continue
            containers.append({
                "name": parts[0],
                "id": parts[1],
                "created_at": parts[2],
                "image": parts[3],
            })
        return containers

    def _build_recovered_env(self, container: dict[str, str]) -> MobileGymEnvironment | None:
        """Build a recovered MobileGymEnvironment from a docker container record.

        Performs a quick health check to determine if the container is actually
        usable. A recovered container that fails health check is marked as
        "unhealthy" so users won't accidentally select it for new runs.
        """
        container_name = container["name"]
        prefix = "aiden-mobilegym-env-"
        if not container_name.startswith(prefix):
            return None
        env_id = container_name[len(prefix):]

        bridge_port = _docker_published_port_safe(container_name, 9090)
        web_port = _docker_published_port_safe(container_name, 4173)
        if bridge_port == 0:
            return None

        public_endpoint = f"http://127.0.0.1:{bridge_port}"

        # Health check: verify the container is actually responsive
        is_healthy = _check_endpoint_health(f"{public_endpoint}/health", timeout=2.0)
        # `docker ps` --format CreatedAt is locale-dependent (trailing "CST"/"PDT"
        # etc.). Pull the RFC3339Nano created timestamp from `docker inspect`
        # instead, which is stable across hosts and docker versions.
        created_iso = _docker_inspect_created(container_name)
        age_label = _format_container_age(created_iso) if created_iso else "age unknown"

        if is_healthy:
            status = "stale"
            name = f"MobileGym (recovered, {age_label})"
            message = (
                "recovered from a previous WebUI session; remove it and start "
                "a fresh environment before running benchmarks"
            )
        else:
            status = "unhealthy"
            name = f"MobileGym (recovered, {age_label}, unhealthy)"
            message = "container is unresponsive at /health - recommended to remove and start fresh"

        env_dir = self.runs_dir / "environments" / env_id
        return MobileGymEnvironment(
            id=env_id,
            name=name,
            endpoint=f"http://host.docker.internal:{bridge_port}",
            public_endpoint=public_endpoint,
            web_url=f"http://127.0.0.1:{web_port}" if web_port else "",
            status=status,
            message=message,
            created_at=container["created_at"],
            started_at=container["created_at"],
            container_name=container_name,
            container_id=container["id"],
            bridge_port=bridge_port,
            web_port=web_port,
            image=container["image"],
            log_path=str(env_dir / "mobilegym.log") if env_dir.exists() else "",
            parallel_envs=DEFAULT_MOBILEGYM_PARALLEL_ENVS,
        )

    def get(self, env_id: str) -> MobileGymEnvironment | None:
        """Get environment by ID."""
        with self._lock:
            return self._environments.get(env_id)

    def start_mobilegym(
        self,
        name: str,
        parallel_envs: int = DEFAULT_MOBILEGYM_PARALLEL_ENVS,
    ) -> MobileGymEnvironment:
        """Start a new MobileGym environment."""
        env_id = new_environment_id()
        env_dir = self.runs_dir / "environments" / env_id
        env_dir.mkdir(parents=True, exist_ok=True)

        web_port = 0
        bridge_port = 0
        env = MobileGymEnvironment(
            id=env_id,
            name=name,
            endpoint="",
            public_endpoint="",
            web_url="",
            status="building",
            message="ensuring Docker image",
            created_at=now_iso(),
            container_name=f"aiden-mobilegym-env-{env_id}",
            bridge_port=bridge_port,
            web_port=web_port,
            image=self.mobilegym_image,
            log_path=str(env_dir / "mobilegym.log"),
            parallel_envs=parallel_envs,
        )

        with self._lock:
            self._environments[env.id] = env

        try:
            ensure_mobilegym_image(
                self.mobilegym_image,
                self.build_mobilegym_image,
                Path(env.log_path),
                repo_root=self.repo_root,
            )
            self._set_environment(env, status="starting", message="starting container")

            command = build_mobilegym_environment_command(
                image=self.mobilegym_image,
                container_name=env.container_name,
                host_web_port=web_port,
                host_bridge_port=bridge_port,
                benchmark_dir=self.repo_root / "benchmark",
                parallel_envs=parallel_envs,
            )
            append_log(Path(env.log_path), "$ " + " ".join(command))
            container_id = subprocess.check_output(
                command, cwd=self.repo_root, text=True
            ).strip()

            web_port = docker_published_port(env.container_name, 4173)
            bridge_port = docker_published_port(env.container_name, 9090)

            self._set_environment(
                env,
                container_id=container_id,
                bridge_port=bridge_port,
                web_port=web_port,
                endpoint=f"http://host.docker.internal:{bridge_port}",
                public_endpoint=f"http://127.0.0.1:{bridge_port}",
                web_url=f"http://127.0.0.1:{web_port}",
                started_at=now_iso(),
                message="waiting for bridge",
            )

            log_proc = start_docker_logs(env.container_name, Path(env.log_path))
            if log_proc is not None:
                with self._lock:
                    self._log_procs[env.id] = log_proc

            wait_for_http_health(
                f"{env.public_endpoint}/health", self.ready_timeout_sec
            )
            self._set_environment(env, status="running", message="")
            return env

        except Exception as exc:
            append_log(Path(env.log_path), f"ERROR: {exc}")
            subprocess.run(
                ["docker", "rm", "-f", env.container_name],
                stdout=subprocess.DEVNULL,
                stderr=subprocess.DEVNULL,
                check=False,
            )
            self._stop_log_proc(env.id)
            self._set_environment(
                env, status="failed", stopped_at=now_iso(), message=str(exc)
            )
            raise RuntimeError(f"failed to start MobileGym environment: {exc}") from exc

    def stop(self, env_id: str) -> MobileGymEnvironment | None:
        """Stop a MobileGym environment."""
        with self._lock:
            env = self._environments.get(env_id)
            if env is None:
                return None
            if env.status == "stopped":
                return env
            env.status = "stopping"
            env.message = "stopping container"

        subprocess.run(
            ["docker", "rm", "-f", env.container_name],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
            check=False,
        )
        self._stop_log_proc(env_id)
        self._set_environment(env, status="stopped", stopped_at=now_iso(), message="")
        return env

    def delete(self, env_id: str) -> MobileGymEnvironment | None:
        """Delete a MobileGym environment."""
        env = self.stop(env_id)
        if env is None:
            return None
        with self._lock:
            removed = self._environments.pop(env_id, None)
        return removed if removed is not None else env

    def shutdown_all(self) -> None:
        """Shutdown all environments."""
        with self._lock:
            env_ids = list(self._environments.keys())
        for env_id in env_ids:
            self.stop(env_id)

    def environment_payload(self, env: MobileGymEnvironment) -> dict[str, Any]:
        """Convert environment to JSON payload."""
        payload = dc.asdict(env)
        payload["log_tail"] = tail_text(Path(env.log_path), LOG_TAIL_BYTES)
        return payload

    def _set_environment(self, env: MobileGymEnvironment, **updates: Any) -> None:
        """Update environment attributes (thread-safe)."""
        with self._lock:
            for key, value in updates.items():
                setattr(env, key, value)

    def _stop_log_proc(self, env_id: str) -> None:
        """Stop log process for environment."""
        with self._lock:
            proc = self._log_procs.pop(env_id, None)
        if proc is not None and proc.poll() is None:
            proc.terminate()


# Helper functions


def new_environment_id() -> str:
    """Generate a new environment ID."""
    stamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    return f"mg-{stamp}-{os.urandom(3).hex()}"


def now_iso() -> str:
    """Return current time in ISO format."""
    return datetime.now(timezone.utc).isoformat()


def tail_text(path: Path, max_bytes: int) -> str:
    """Read the tail of a file."""
    try:
        with path.open("rb") as handle:
            handle.seek(0, os.SEEK_END)
            size = handle.tell()
            handle.seek(max(0, size - max_bytes))
            return handle.read().decode("utf-8", errors="replace")
    except Exception:
        return ""


def append_log(path: Path, text: str) -> None:
    """Append text to log file."""
    path.parent.mkdir(parents=True, exist_ok=True)
    with path.open("a", encoding="utf-8") as handle:
        handle.write(text.rstrip() + "\n")


def wait_for_http_health(url: str, timeout_sec: int) -> None:
    """Wait for HTTP health endpoint to respond."""
    deadline = time.monotonic() + max(1, timeout_sec)
    last_error = ""
    while time.monotonic() < deadline:
        try:
            with urllib.request.urlopen(url, timeout=2) as response:
                if 200 <= response.status < 300:
                    return
        except Exception as exc:
            last_error = str(exc)
        time.sleep(1)
    suffix = f": {last_error}" if last_error else ""
    raise RuntimeError(f"service did not become ready at {url}{suffix}")


def docker_published_port(container_name: str, container_port: int) -> int:
    """Get published host port for container port."""
    output = subprocess.check_output(
        ["docker", "port", container_name, f"{int(container_port)}/tcp"],
        text=True,
    ).strip()
    for line in output.splitlines():
        try:
            return int(line.rsplit(":", 1)[1])
        except (IndexError, ValueError):
            continue
    raise RuntimeError(
        f"could not determine published port for {container_name}:{container_port}"
    )


def _docker_published_port_safe(container_name: str, container_port: int) -> int:
    """Safe version of docker_published_port: returns 0 on any error."""
    try:
        return docker_published_port(container_name, container_port)
    except Exception:
        return 0


def _check_endpoint_health(url: str, timeout: float = 2.0) -> bool:
    """Quick health check: returns True only if endpoint returns 2xx."""
    try:
        with urllib.request.urlopen(url, timeout=timeout) as response:
            return 200 <= response.status < 300
    except Exception:
        return False
def _docker_inspect_created(container_name: str) -> str:
    """Return the container's RFC3339Nano creation timestamp, or '' on error."""
    try:
        result = subprocess.run(
            ["docker", "inspect", "--format", "{{.Created}}", container_name],
            capture_output=True,
            text=True,
            timeout=5,
            check=False,
        )
    except (subprocess.SubprocessError, OSError):
        return ""
    if result.returncode != 0:
        return ""
    return result.stdout.strip()


def _format_container_age(created_at: str) -> str:
    """Format a container creation timestamp as a human-friendly age.

    Accepts RFC3339Nano from `docker inspect` (e.g. '2026-06-25T01:07:53.123456789Z')
    and the legacy locale-dependent `docker ps --format {{.CreatedAt}}` form
    (e.g. '2026-06-25 09:07:53 +0800 CST') as a fallback.

    Returns labels like '15m old', '2h old', '3d old', or 'age unknown'.
    """
    if not created_at:
        return "age unknown"
    dt = _parse_docker_timestamp(created_at)
    if dt is None:
        return "age unknown"
    if dt.tzinfo is None:
        dt = dt.replace(tzinfo=timezone.utc)
    delta = datetime.now(timezone.utc) - dt
    total_seconds = int(delta.total_seconds())
    if total_seconds < 0:
        return "age unknown"
    if total_seconds < 60:
        return f"{total_seconds}s old"
    minutes = total_seconds // 60
    if minutes < 60:
        return f"{minutes}m old"
    hours = minutes // 60
    if hours < 24:
        return f"{hours}h old"
    days = hours // 24
    return f"{days}d old"


def _parse_docker_timestamp(value: str) -> datetime | None:
    """Parse RFC3339Nano (from `docker inspect`) or `docker ps` CreatedAt."""
    raw = value.strip()
    if not raw:
        return None
    # RFC3339Nano: '2026-06-25T01:07:53.123456789Z' or with +hh:mm offset.
    iso = raw
    if iso.endswith("Z"):
        iso = iso[:-1] + "+00:00"
    # fromisoformat() in 3.11+ tolerates fractional seconds; strip nanos to be safe.
    if "." in iso:
        head, _, tail = iso.partition(".")
        # tail may end with timezone like '123456789+00:00'
        for i, ch in enumerate(tail):
            if ch in "+-":
                frac = tail[:i][:6]
                tz = tail[i:]
                iso = f"{head}.{frac}{tz}"
                break
        else:
            iso = f"{head}.{tail[:6]}"
    try:
        return datetime.fromisoformat(iso)
    except ValueError:
        pass
    # Legacy `docker ps` form: '2026-06-25 09:07:53 +0800 CST': strip trailing
    # locale abbreviation if present, then parse with %z.
    cleaned = raw
    parts = cleaned.rsplit(" ", 1)
    if len(parts) == 2 and not parts[1].startswith(("+", "-")):
        cleaned = parts[0]
    for fmt in ("%Y-%m-%d %H:%M:%S %z", "%Y-%m-%d %H:%M:%S"):
        try:
            return datetime.strptime(cleaned, fmt)
        except ValueError:
            continue
    return None


def build_mobilegym_environment_command(
    *,
    image: str,
    container_name: str,
    host_web_port: int,
    host_bridge_port: int,
    benchmark_dir: Path,
    bridge_port: int = 9090,
    parallel_envs: int = DEFAULT_MOBILEGYM_PARALLEL_ENVS,
) -> list[str]:
    """Build docker run command for MobileGym environment."""
    parallel_envs = max(1, int(parallel_envs))
    script = "\n".join(
        [
            "set -eu",
            'export PATH="/opt/venv/bin:$PATH"',
            "cd /mobilegym",
            "npm run preview -- --host 0.0.0.0 --port 4173 >/tmp/mobilegym-preview.log 2>&1 &",
            'preview_pid="$!"',
            'cleanup() { kill "$preview_pid" 2>/dev/null || true; }',
            "trap cleanup INT TERM EXIT",
            "ready=0",
            "for _ in $(seq 1 120); do",
            (
                "  if python3 -c \"import urllib.request; "
                "urllib.request.urlopen('http://127.0.0.1:4173', timeout=1).read(1)\" "
                ">/dev/null 2>&1; then"
            ),
            "    ready=1",
            "    break",
            "  fi",
            '  if ! kill -0 "$preview_pid" 2>/dev/null; then',
            "    cat /tmp/mobilegym-preview.log >&2",
            "    exit 1",
            "  fi",
            "  sleep 1",
            "done",
            'if [ "$ready" != "1" ]; then',
            "  cat /tmp/mobilegym-preview.log >&2",
            "  exit 1",
            "fi",
            (
                "exec python3 /app/benchmark/mobilegym/scripts/start_simulator.py "
                "--mobilegym-root /mobilegym "
                "--env-url http://127.0.0.1:4173 "
                "--bridge-host 0.0.0.0 "
                f"--bridge-port {bridge_port} "
                f"--parallel-envs {parallel_envs} "
                "--headless"
            ),
        ]
    )
    bridge_endpoint = f"http://host.docker.internal:{host_bridge_port}"
    return [
        "docker",
        "run",
        "--rm",
        "-d",
        "--name",
        container_name,
        "--add-host",
        "host.docker.internal:host-gateway",
        "-p",
        docker_publish_arg(host_web_port, 4173, bind_host="127.0.0.1"),
        "-p",
        docker_publish_arg(
            host_bridge_port,
            bridge_port,
            # Agent containers reach this port through host.docker.internal.
            # A loopback-only publish works for the host's readiness checks but
            # refuses connections through the Linux host gateway.
        ),
        "-v",
        f"{benchmark_dir.resolve()}:/app/benchmark:ro",
        "-e",
        "MOBILEGYM_ROOT=/mobilegym",
        "-e",
        f"NO_PROXY={docker_no_proxy(bridge_endpoint)}",
        "-e",
        f"no_proxy={docker_no_proxy(bridge_endpoint)}",
        "--entrypoint",
        "sh",
        image,
        "-c",
        script,
    ]


def docker_publish_arg(host_port: int, container_port: int, *, bind_host: str = "") -> str:
    """Build docker port publish argument."""
    host_port = int(host_port)
    if host_port == 0:
        return f"{bind_host}::{container_port}" if bind_host else str(container_port)
    return (
        f"{bind_host}:{host_port}:{container_port}"
        if bind_host
        else f"{host_port}:{container_port}"
    )


def docker_no_proxy(endpoint: str) -> str:
    """Build NO_PROXY environment variable."""
    base = ["localhost", "127.0.0.1", "host.docker.internal"]
    host = urllib.parse.urlparse(endpoint).hostname
    if host and host not in base:
        base.append(host)
    existing = os.getenv("NO_PROXY") or os.getenv("no_proxy") or ""
    for item in existing.split(","):
        item = item.strip()
        if item and item not in base:
            base.append(item)
    return ",".join(base)


def resolve_mobilegym_source(repo_root: Path) -> MobileGymSource:
    """Resolve the MobileGym repository and pinned commit from the firmware tree."""
    gitmodules = configparser.ConfigParser()
    gitmodules_path = repo_root / ".gitmodules"
    if not gitmodules.read(gitmodules_path, encoding="utf-8"):
        raise RuntimeError(f"Unable to read submodule configuration: {gitmodules_path}")
    try:
        mobilegym_repo = gitmodules[MOBILEGYM_SUBMODULE_SECTION]["url"].strip()
    except KeyError as exc:
        raise RuntimeError(
            f"MobileGym submodule URL not found in {gitmodules_path}"
        ) from exc

    try:
        mobilegym_commit = subprocess.check_output(
            ["git", "rev-parse", f"HEAD:{MOBILEGYM_SUBMODULE_PATH}"],
            cwd=repo_root,
            text=True,
        ).strip()
    except (OSError, subprocess.CalledProcessError) as exc:
        raise RuntimeError("Unable to resolve the MobileGym submodule commit from HEAD") from exc
    if not re.fullmatch(r"[0-9a-f]{40}", mobilegym_commit):
        raise RuntimeError(f"Invalid MobileGym submodule commit in HEAD: {mobilegym_commit!r}")

    return MobileGymSource(repo=mobilegym_repo, commit=mobilegym_commit)


def ensure_mobilegym_image(
    image: str,
    build_missing: bool,
    log_path: Path,
    *,
    repo_root: Path | None = None,
) -> None:
    """Ensure the MobileGym image matches the commit pinned by the firmware tree."""
    repo_root = repo_root or Path(__file__).resolve().parents[2]
    source = resolve_mobilegym_source(repo_root)
    build_args = {
        "MOBILEGYM_REPO": source.repo,
        "MOBILEGYM_COMMIT": source.commit,
    }
    for name in ("MOBILEGYM_NODE_IMAGE", "MOBILEGYM_DEBIAN_MIRROR"):
        if value := os.environ.get(name):
            build_args[name] = value
    ensure_docker_image(
        image,
        build_missing,
        log_path,
        "mobilegym-base",
        repo_root=repo_root,
        build_args=build_args,
        required_labels={MOBILEGYM_COMMIT_LABEL: source.commit},
    )


def ensure_docker_image(
    image: str,
    build_missing: bool,
    log_path: Path,
    target: str,
    *,
    stop_requested: Callable[[], bool] | None = None,
    repo_root: Path | None = None,
    build_args: Mapping[str, str] | None = None,
    required_labels: Mapping[str, str] | None = None,
) -> None:
    """Ensure Docker image exists, optionally building it."""
    repo_root = repo_root or Path(__file__).resolve().parents[2]

    inspect = subprocess.run(
        ["docker", "image", "inspect", "--format", "{{json .Config.Labels}}", image],
        capture_output=True,
        text=True,
        check=False,
    )
    image_exists = inspect.returncode == 0
    labels: dict[str, str] = {}
    if image_exists:
        try:
            parsed_labels = json.loads(inspect.stdout) if inspect.stdout.strip() else None
        except json.JSONDecodeError:
            parsed_labels = None
        if isinstance(parsed_labels, dict):
            labels = {str(key): str(value) for key, value in parsed_labels.items()}

        mismatched_labels = {
            key: (labels.get(key), expected)
            for key, expected in (required_labels or {}).items()
            if labels.get(key) != expected
        }
        if not mismatched_labels:
            return
    else:
        mismatched_labels = {}

    if not build_missing:
        if not image_exists:
            raise RuntimeError(f"Docker image not found: {image}")
        details = ", ".join(
            f"{key} is {actual or 'unset'}, expected {expected}"
            for key, (actual, expected) in mismatched_labels.items()
        )
        raise RuntimeError(f"Docker image {image} is stale: {details}")

    cmd = [
        "docker",
        "build",
        "-f",
        str(repo_root / "benchmark" / "mobilegym" / "docker" / "Dockerfile"),
        "--target",
        target,
    ]
    for key, value in (build_args or {}).items():
        cmd.extend(["--build-arg", f"{key}={value}"])
    cmd.extend(["-t", image, str(repo_root)])
    append_log(log_path, "$ " + " ".join(cmd))

    popen_kwargs: dict[str, Any] = {}
    if os.name == "posix":
        popen_kwargs["start_new_session"] = True

    with log_path.open("ab") as log:
        proc = subprocess.Popen(cmd, stdout=log, stderr=subprocess.STDOUT, **popen_kwargs)
        while proc.poll() is None:
            if stop_requested is not None and stop_requested():
                # Import here to avoid circular dependency
                from benchmark.runner.webui import terminate_process_tree, JobStopped

                terminate_process_tree(proc)
                raise JobStopped("job stop requested")
            time.sleep(0.25)

    if proc.returncode != 0:
        raise subprocess.CalledProcessError(proc.returncode, cmd)


def start_docker_logs(container_name: str, log_path: Path) -> subprocess.Popen | None:
    """Start docker logs process."""
    try:
        log_file = log_path.open("ab")
        try:
            return subprocess.Popen(
                ["docker", "logs", "-f", container_name],
                stdout=log_file,
                stderr=subprocess.STDOUT,
            )
        finally:
            log_file.close()
    except Exception:
        return None
