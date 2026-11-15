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
import sys
import time
import urllib.error
import urllib.request
from pathlib import Path
from typing import Any, Callable, Mapping, Sequence

from runner import environment as runner_environment
from runner.adb_android_environment import pid_alive, terminate_pid
from runner.environment import append_log
from runner.environment_endpoint import EnvironmentEndpoint

from .net import PROXY_VARIABLES


def runs_module(command_line: str, module: str) -> bool:
    """True when command_line is a ``python ... -m <module>`` invocation."""
    tokens = command_line.split()
    return bool(module) and any(
        token == "-m" and index + 1 < len(tokens) and tokens[index + 1] == module
        for index, token in enumerate(tokens)
    )


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

    def platform(self) -> str:
        """sys.platform of the host: darwin, linux, or win32."""
        return sys.platform

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

    def post_json(
        self, url: str, payload: Any, *, headers: Mapping[str, str] | None = None, timeout: float = 10.0
    ) -> int:
        """POST a JSON body to a local bridge; returns the HTTP status, or 0 when nothing answered."""
        request = urllib.request.Request(
            url,
            data=json.dumps(payload).encode("utf-8"),
            method="POST",
            headers={"Content-Type": "application/json", **dict(headers or {})},
        )
        opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
        try:
            with opener.open(request, timeout=timeout) as response:
                response.read()
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

    def tcp_reachable(self, host: str, port: int, timeout: float = 2.0) -> bool:
        try:
            with socket.create_connection((host, int(port)), timeout=timeout):
                return True
        except OSError:
            return False

    def free_port(self) -> int:
        with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as sock:
            sock.bind(("127.0.0.1", 0))
            return int(sock.getsockname()[1])

    def bridge_bind_host(self) -> str:
        """Host interface that containers reach through host.docker.internal."""
        return runner_environment.mobilegym_bridge_bind_host()

    # -- processes ----------------------------------------------------------

    def start_process(
        self,
        args: Sequence[str],
        *,
        cwd: Path,
        log_path: Path,
        env: Mapping[str, str] | None = None,
    ) -> int:
        """Start a detached host process with stdout and stderr appended to log_path.

        The process gets its own session, so it outlives the launcher and a
        Ctrl+C in the launcher's terminal does not reach it.
        """
        args = [str(arg) for arg in args]
        append_log(log_path, "$ " + " ".join(args))
        popen_kwargs: dict[str, Any] = {}
        if os.name == "posix":
            popen_kwargs["start_new_session"] = True
        with log_path.open("ab") as log:
            proc = subprocess.Popen(
                args,
                cwd=str(cwd),
                stdout=log,
                stderr=subprocess.STDOUT,
                stdin=subprocess.DEVNULL,
                env=dict(env) if env is not None else None,
                **popen_kwargs,
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
        result = self.run(["ps", "-ww", "-o", "command=", "-p", str(pid)], timeout=5)
        return result.stdout.strip() if result.returncode == 0 else ""

    def process_runs_module(self, pid: int, module: str) -> bool:
        """True while pid is still a ``python -m <module>`` process (pids are recycled)."""
        return runs_module(self.pid_command(pid), module)

    def terminate(self, pid: int, wait_sec: float = 5.0) -> None:
        """SIGTERM pid, then SIGKILL it when it has not exited after wait_sec."""
        proc = self._bridge_procs.pop(pid, None)
        if proc is not None:
            proc.terminate()
            try:
                proc.wait(timeout=wait_sec)
            except subprocess.TimeoutExpired:
                proc.kill()
                proc.wait()
            return
        terminate_pid(pid, wait_sec=wait_sec)

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
