"""Host access for the launcher behind one injectable object.

Every subprocess, HTTP probe, port check, and process-signal call the launcher
makes goes through :class:`System`, so tests can substitute a fake and run the
whole lifecycle without Docker, adb, or the network.
"""

from __future__ import annotations

import json
import os
import shutil
import socket
import subprocess
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

from runner import environment as runner_environment
from runner.adb_android_environment import (
    pid_alive,
    start_adb_bridge_process,
    terminate_pid,
)
from runner.environment_endpoint import EnvironmentEndpoint

from .net import PROXY_VARIABLES

ADB_BRIDGE_MODULE = "adbandroid.scripts.start_bridge"


class System:
    """Real host implementation; tests override individual methods."""

    def __init__(self) -> None:
        self._bridge_procs: dict[int, subprocess.Popen] = {}

    # -- commands -----------------------------------------------------------

    def run(
        self,
        args: Sequence[str],
        *,
        timeout: float | None = 60,
        cwd: Path | None = None,
        env: Mapping[str, str] | None = None,
    ) -> subprocess.CompletedProcess:
        """Run a command and capture text output; never raises for exit codes."""
        args = [str(arg) for arg in args]
        try:
            return subprocess.run(
                args,
                capture_output=True,
                text=True,
                errors="replace",
                timeout=timeout,
                cwd=str(cwd) if cwd else None,
                env=dict(env) if env is not None else None,
                check=False,
            )
        except FileNotFoundError:
            return subprocess.CompletedProcess(args, 127, "", f"{args[0]}: command not found")
        except subprocess.TimeoutExpired:
            return subprocess.CompletedProcess(args, 124, "", f"{args[0]}: timed out after {timeout}s")
        except OSError as exc:
            return subprocess.CompletedProcess(args, 126, "", f"{args[0]}: {exc}")

    def run_logged(
        self,
        args: Sequence[str],
        log_path: Path,
        *,
        cwd: Path | None = None,
        env: Mapping[str, str] | None = None,
        should_stop: Callable[[], bool] | None = None,
    ) -> int:
        """Run a long command, streaming stdout and stderr into log_path."""
        args = [str(arg) for arg in args]
        log_path.parent.mkdir(parents=True, exist_ok=True)
        with log_path.open("ab") as log:
            try:
                proc = subprocess.Popen(
                    args,
                    stdout=log,
                    stderr=subprocess.STDOUT,
                    stdin=subprocess.DEVNULL,
                    cwd=str(cwd) if cwd else None,
                    env=dict(env) if env is not None else None,
                )
            except OSError as exc:
                log.write(f"{args[0]}: {exc}\n".encode())
                return 127
            while proc.poll() is None:
                if should_stop is not None and should_stop():
                    proc.terminate()
                    try:
                        proc.wait(timeout=10)
                    except subprocess.TimeoutExpired:
                        proc.kill()
                        proc.wait()
                    return -1
                time.sleep(0.25)
            return int(proc.returncode)

    def which(self, name: str) -> str:
        return shutil.which(name) or ""

    # -- network ------------------------------------------------------------

    def http_status(self, url: str, timeout: float = 2.0) -> int:
        """Return the HTTP status for url, or 0 when nothing answered."""
        request = urllib.request.Request(url, method="GET")
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(request, timeout=timeout) as response:
                return int(response.status)
        except urllib.error.HTTPError as exc:
            return int(exc.code)
        except Exception:
            return 0

    def read_health(self, endpoint: str, timeout: float = 5.0) -> dict[str, Any]:
        """Read an environment bridge /health payload (raises on failure).

        Unlike runner.platform.read_environment_health this ignores host proxy
        variables: bridges are local, and a proxy would answer for them.
        """
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        with opener.open(EnvironmentEndpoint(endpoint).health, timeout=timeout) as response:
            payload = json.loads(response.read().decode("utf-8"))
        if isinstance(payload, dict) and isinstance(payload.get("data"), dict):
            return payload["data"]
        return payload if isinstance(payload, dict) else {}

    def port_free(self, port: int) -> bool:
        """True when nothing listens on 127.0.0.1:port and it can be bound."""
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as probe:
            probe.settimeout(0.3)
            if probe.connect_ex(("127.0.0.1", int(port))) == 0:
                return False
        try:
            with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
                sock.bind(("127.0.0.1", int(port)))
        except OSError:
            return False
        return True

    def free_port(self) -> int:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.bind(("127.0.0.1", 0))
            return int(sock.getsockname()[1])

    def bridge_bind_host(self) -> str:
        """Host interface that containers reach through host.docker.internal."""
        return runner_environment.mobilegym_bridge_bind_host()

    # -- processes ----------------------------------------------------------

    def start_adb_bridge(
        self, *, serial: str, port: int, host: str, adb_path: str, log_path: Path
    ) -> int:
        proc = start_adb_bridge_process(
            serial=serial,
            bridge_port=port,
            log_path=log_path,
            adb_path=adb_path,
            bridge_host=host,
        )
        self._bridge_procs[proc.pid] = proc
        return int(proc.pid)

    def pid_command(self, pid: int) -> str:
        """Return the command line of pid, or '' when unknown or gone."""
        if pid <= 0:
            return ""
        proc = self._bridge_procs.get(pid)
        if proc is not None and proc.poll() is not None:
            self._bridge_procs.pop(pid, None)
            return ""
        if not pid_alive(pid):
            return ""
        proc_cmdline = Path(f"/proc/{pid}/cmdline")
        if proc_cmdline.exists():
            try:
                return proc_cmdline.read_bytes().replace(b"\0", b" ").decode(errors="replace").strip()
            except OSError:
                return ""
        result = self.run(["ps", "-o", "command=", "-p", str(pid)], timeout=5)
        return result.stdout.strip() if result.returncode == 0 else ""

    def is_adb_bridge(self, pid: int) -> bool:
        return ADB_BRIDGE_MODULE in self.pid_command(pid)

    def terminate(self, pid: int) -> None:
        proc = self._bridge_procs.pop(pid, None)
        if proc is not None:
            proc.terminate()
            try:
                proc.wait(timeout=5)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
            return
        terminate_pid(pid)

    # -- MobileGym ----------------------------------------------------------

    def ensure_mobilegym_image(
        self,
        image: str,
        log_path: Path,
        *,
        repo_root: Path,
        build_args: Mapping[str, str],
        should_stop: Callable[[], bool] | None = None,
    ) -> None:
        extra_args: list[str] = []
        if any(name in build_args for name in PROXY_VARIABLES):
            # Proxies on the host's loopback are rewritten to host.docker.internal,
            # which a Linux build only resolves with an explicit host entry.
            extra_args = ["--add-host", "host.docker.internal:host-gateway"]
        runner_environment.ensure_mobilegym_image(
            image,
            True,
            log_path,
            repo_root=repo_root,
            extra_build_args=build_args,
            extra_args=extra_args,
            stop_requested=should_stop,
        )

    def resolve_mobilegym_commit(self, repo_root: Path) -> str:
        return runner_environment.resolve_mobilegym_source(repo_root).commit

    # -- time ---------------------------------------------------------------

    def monotonic(self) -> float:
        return time.monotonic()

    def sleep(self, seconds: float) -> None:
        time.sleep(seconds)

    def environ(self) -> Mapping[str, str]:
        return os.environ
