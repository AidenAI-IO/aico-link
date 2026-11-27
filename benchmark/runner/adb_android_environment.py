"""Shared ADB Android environment management for CLI services and the WebUI.

The ADB Android bridge is a local Python process (adbandroid.scripts.start_bridge)
rather than a Docker container, so lifecycle management is pid-based:
- a pidfile + JSON manifest is written next to the log for each environment;
- recovery after a WebUI restart requires BOTH a live pid and a passing
  /health probe (pids can be recycled, so neither alone is trusted).
"""

from __future__ import annotations

import dataclasses as dc
import json
import os
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.request
from datetime import datetime, timezone
from pathlib import Path
from typing import Any

try:
    from benchmark.runner.environment import append_log, now_iso, tail_text, wait_for_http_health
except ImportError:
    from runner.environment import append_log, now_iso, tail_text, wait_for_http_health


BENCHMARK_ROOT = Path(__file__).resolve().parents[1]
DEFAULT_ADB_SERIAL = "127.0.0.1:6555"
DEFAULT_ADB_BRIDGE_READY_TIMEOUT_SEC = 30
ADB_ENV_MANIFEST_NAME = "adb_android.json"
ADB_ENV_PID_NAME = "adb_android.pid"
ADB_ENV_LOG_NAME = "adb_android.log"
LOG_TAIL_BYTES = 96 * 1024


@dc.dataclass
class ADBAndroidEnvironment:
    id: str
    name: str
    serial: str
    endpoint: str
    public_endpoint: str
    docker_endpoint: str
    status: str = "starting"
    message: str = ""
    created_at: str = ""
    started_at: str = ""
    stopped_at: str = ""
    process_id: int = 0
    bridge_port: int = 0
    log_path: str = ""
    manifest_path: str = ""
    type: str = "adb_android"
    parallel_envs: int = 1


def new_adb_environment_id() -> str:
    stamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
    return f"adb-{stamp}-{os.urandom(3).hex()}"


def endpoint_for_docker_host(endpoint: str) -> str:
    return (
        endpoint.replace("://127.0.0.1", "://host.docker.internal")
        .replace("://localhost", "://host.docker.internal")
    )


def reserve_free_port() -> int:
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
        sock.bind(("127.0.0.1", 0))
        return int(sock.getsockname()[1])


def pid_alive(pid: int) -> bool:
    if pid <= 0:
        return False
    try:
        os.kill(pid, 0)
    except (OSError, ProcessLookupError):
        return False
    return True


def check_endpoint_health(url: str, timeout: float = 2.0) -> bool:
    try:
        with urllib.request.urlopen(url, timeout=timeout) as response:
            return 200 <= response.status < 300
    except Exception:
        return False


def start_adb_bridge_process(
    *,
    serial: str,
    bridge_port: int,
    log_path: Path,
    adb_path: str = "adb",
    bridge_host: str = "127.0.0.1",
    cwd: Path | None = None,
    unicode_input_method: str = "",
) -> subprocess.Popen:
    """Launch adbandroid.scripts.start_bridge detached with output to log_path.

    An empty unicode_input_method keeps the bridge default.
    """
    command = [
        sys.executable,
        "-m",
        "adbandroid.scripts.start_bridge",
        "--adb-serial",
        serial,
        "--adb-path",
        adb_path,
        "--bridge-host",
        bridge_host,
        "--bridge-port",
        str(int(bridge_port)),
    ]
    if unicode_input_method:
        command += ["--unicode-input-method", unicode_input_method]
    append_log(log_path, "$ " + " ".join(command))
    popen_kwargs: dict[str, Any] = {}
    if os.name == "posix":
        popen_kwargs["start_new_session"] = True
    log_file = log_path.open("ab")
    try:
        return subprocess.Popen(
            command,
            cwd=str(cwd or BENCHMARK_ROOT),
            stdout=log_file,
            stderr=subprocess.STDOUT,
            **popen_kwargs,
        )
    finally:
        log_file.close()


def terminate_pid(pid: int, *, wait_sec: float = 5.0) -> None:
    """TERM then KILL a bridge process; no-op when already gone."""
    if not pid_alive(pid):
        return
    try:
        os.kill(pid, signal.SIGTERM)
    except OSError:
        return
    deadline = time.monotonic() + wait_sec
    while time.monotonic() < deadline:
        if not pid_alive(pid):
            return
        time.sleep(0.1)
    try:
        os.kill(pid, signal.SIGKILL)
    except OSError:
        pass


def write_adb_env_manifest(path: Path, env: ADBAndroidEnvironment) -> None:
    payload = {
        "id": env.id,
        "name": env.name,
        "pid": env.process_id,
        "serial": env.serial,
        "bridge_port": env.bridge_port,
        "public_endpoint": env.public_endpoint,
        "log_path": env.log_path,
        "created_at": env.created_at,
    }
    path.parent.mkdir(parents=True, exist_ok=True)
    path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")


class ADBAndroidEnvironmentManager:
    """Manages local ADB Android bridge processes for the WebUI."""

    def __init__(
        self,
        runs_dir: Path,
        ready_timeout_sec: int = DEFAULT_ADB_BRIDGE_READY_TIMEOUT_SEC,
        adb_path: str = "adb",
    ):
        self.runs_dir = runs_dir
        self.ready_timeout_sec = ready_timeout_sec
        self.adb_path = adb_path
        self._lock = threading.RLock()
        self._environments: dict[str, ADBAndroidEnvironment] = {}
        self._recover_existing()

    def _env_dir(self, env_id: str) -> Path:
        return self.runs_dir / "environments" / env_id

    def _recover_existing(self) -> None:
        environments_dir = self.runs_dir / "environments"
        if not environments_dir.exists():
            return
        for manifest_path in sorted(environments_dir.glob(f"*/{ADB_ENV_MANIFEST_NAME}")):
            try:
                manifest = json.loads(manifest_path.read_text(encoding="utf-8"))
            except (OSError, json.JSONDecodeError):
                continue
            env = self._build_recovered_env(manifest, manifest_path)
            if env is not None:
                self._environments[env.id] = env

    def _build_recovered_env(
        self, manifest: dict[str, Any], manifest_path: Path
    ) -> ADBAndroidEnvironment | None:
        env_id = str(manifest.get("id") or "").strip()
        public_endpoint = str(manifest.get("public_endpoint") or "").strip()
        if not env_id or not public_endpoint:
            return None
        pid = int(manifest.get("pid") or 0)
        # Require both a live pid and a healthy endpoint: pids get recycled and
        # ports get reused, so either check alone can attach to the wrong process.
        is_healthy = pid_alive(pid) and check_endpoint_health(f"{public_endpoint}/health")
        name = str(manifest.get("name") or "ADB Android")
        if is_healthy:
            status = "running"
            name = f"{name} (recovered)"
            message = "recovered from existing bridge process"
            process_id = pid
        else:
            status = "unhealthy"
            name = f"{name} (recovered, unhealthy)"
            message = "bridge process is gone or unresponsive - remove and start fresh"
            # Clear the PID: once unhealthy, we cannot safely terminate it
            # (PID may have been recycled), and stop() must not kill an
            # unrelated process.
            process_id = 0
        return ADBAndroidEnvironment(
            id=env_id,
            name=name,
            serial=str(manifest.get("serial") or ""),
            endpoint=endpoint_for_docker_host(public_endpoint),
            public_endpoint=public_endpoint,
            docker_endpoint=endpoint_for_docker_host(public_endpoint),
            status=status,
            message=message,
            created_at=str(manifest.get("created_at") or ""),
            started_at=str(manifest.get("created_at") or ""),
            process_id=process_id,
            bridge_port=int(manifest.get("bridge_port") or 0),
            log_path=str(manifest.get("log_path") or ""),
            manifest_path=str(manifest_path),
        )

    def list_all(self) -> list[ADBAndroidEnvironment]:
        with self._lock:
            environments = list(self._environments.values())
        # Refresh liveness of running environments so the WebUI reflects
        # bridges that died since the last poll.
        for env in environments:
            if env.status == "running" and not (
                pid_alive(env.process_id) and check_endpoint_health(f"{env.public_endpoint}/health")
            ):
                # Clear the PID when marking unhealthy: the process is either
                # gone or unresponsive, and a recycled PID must not be killed.
                self._set_environment(
                    env,
                    status="unhealthy",
                    message="bridge process is gone or unresponsive",
                    process_id=0,
                )
        return environments

    def get(self, env_id: str) -> ADBAndroidEnvironment | None:
        with self._lock:
            return self._environments.get(env_id)

    def start_adb_android(
        self,
        name: str,
        serial: str,
        bridge_port: int = 0,
    ) -> ADBAndroidEnvironment:
        serial = str(serial or "").strip() or DEFAULT_ADB_SERIAL
        env_id = new_adb_environment_id()
        env_dir = self._env_dir(env_id)
        env_dir.mkdir(parents=True, exist_ok=True)
        log_path = env_dir / ADB_ENV_LOG_NAME
        manifest_path = env_dir / ADB_ENV_MANIFEST_NAME
        pid_path = env_dir / ADB_ENV_PID_NAME

        port = int(bridge_port or 0) or reserve_free_port()
        public_endpoint = f"http://127.0.0.1:{port}"
        env = ADBAndroidEnvironment(
            id=env_id,
            name=str(name or "").strip() or f"ADB Android ({serial})",
            serial=serial,
            endpoint=endpoint_for_docker_host(public_endpoint),
            public_endpoint=public_endpoint,
            docker_endpoint=endpoint_for_docker_host(public_endpoint),
            status="starting",
            message="starting bridge process",
            created_at=now_iso(),
            bridge_port=port,
            log_path=str(log_path),
            manifest_path=str(manifest_path),
        )
        with self._lock:
            self._environments[env.id] = env

        try:
            proc = start_adb_bridge_process(
                serial=serial,
                bridge_port=port,
                log_path=log_path,
                adb_path=self.adb_path,
            )
            self._set_environment(env, process_id=proc.pid, message="waiting for bridge")
            pid_path.write_text(str(proc.pid), encoding="utf-8")
            write_adb_env_manifest(manifest_path, env)
            wait_for_http_health(f"{public_endpoint}/health", self.ready_timeout_sec)
            self._set_environment(env, status="running", started_at=now_iso(), message="")
            return env
        except Exception as exc:
            append_log(log_path, f"ERROR: {exc}")
            terminate_pid(env.process_id)
            # Drop the persisted manifest/pidfile (keep the log for debugging)
            # so a WebUI restart does not resurrect this failed environment as
            # a ghost "(recovered, unhealthy)" entry.
            self._remove_persisted_files(env_id)
            self._set_environment(env, status="failed", stopped_at=now_iso(), message=str(exc))
            raise RuntimeError(f"failed to start ADB Android environment: {exc}") from exc

    def stop(self, env_id: str) -> ADBAndroidEnvironment | None:
        with self._lock:
            env = self._environments.get(env_id)
            if env is None:
                return None
            if env.status == "stopped":
                return env
            env.status = "stopping"
            env.message = "stopping bridge process"
        terminate_pid(env.process_id)
        # A deliberately stopped environment must not resurface after a WebUI
        # restart: the manifest plays the role of MobileGym's docker container,
        # which also ceases to exist on stop.
        self._remove_persisted_files(env_id)
        self._set_environment(env, status="stopped", stopped_at=now_iso(), message="")
        return env

    def delete(self, env_id: str) -> ADBAndroidEnvironment | None:
        env = self.stop(env_id)
        if env is None:
            return None
        with self._lock:
            removed = self._environments.pop(env_id, None)
        self._remove_persisted_files(env_id)
        return removed if removed is not None else env

    def _remove_persisted_files(self, env_id: str) -> None:
        """Remove manifest + pidfile (never the log) for an environment."""
        for file_name in (ADB_ENV_MANIFEST_NAME, ADB_ENV_PID_NAME):
            try:
                (self._env_dir(env_id) / file_name).unlink(missing_ok=True)
            except OSError:
                pass

    def shutdown_all(self) -> None:
        with self._lock:
            env_ids = list(self._environments.keys())
        for env_id in env_ids:
            self.stop(env_id)

    def environment_payload(self, env: ADBAndroidEnvironment) -> dict[str, Any]:
        payload = dc.asdict(env)
        payload["log_tail"] = tail_text(Path(env.log_path), LOG_TAIL_BYTES) if env.log_path else ""
        return payload

    def _set_environment(self, env: ADBAndroidEnvironment, **updates: Any) -> None:
        with self._lock:
            for key, value in updates.items():
                setattr(env, key, value)
