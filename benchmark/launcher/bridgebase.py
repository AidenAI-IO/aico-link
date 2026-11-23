"""Building blocks shared by every bridge type.

A bridge type describes its fields, setup notes, and actions, normalizes and
validates its config, probes the host, and starts and stops its bridge for one
instance. :class:`HostProcessBridge` is the common base for bridges that run as
a Python process on this computer (ADB, desktop, iPhone Mirroring, iOS device,
vphone).
"""

from __future__ import annotations

import ipaddress
import sys
import threading
from dataclasses import dataclass, field
from pathlib import Path
from typing import TYPE_CHECKING, Any, Callable, Sequence

from runner.environment import tail_text
from runner.platform import TargetPlatform, resolve_environment_platform

from . import BENCHMARK_ROOT, validation
from . import pyenv
from .docker import Docker
from .system import System

if TYPE_CHECKING:  # pragma: no cover
    from .bridges import HostFacts

PLANNED_REASON = "Planned for a later Launcher release"
BRIDGE_READY_TIMEOUT_SEC = 120
BRIDGE_STOP_TIMEOUT_SEC = 5.0
ROUTE_RENEW_TIMEOUT_SEC = 30.0
ACTION_TIMEOUT_SEC = 600
MAX_ACTION_OUTPUT_CHARS = 20000

DEVICE_TYPE_BY_PLATFORM = {
    TargetPlatform.ANDROID: "Android",
    TargetPlatform.IOS: "iOS",
    TargetPlatform.MAC: "macOS",
    TargetPlatform.WINDOWS: "windows",
    TargetPlatform.LINUX: "linux",
}


class Cancelled(Exception):
    """The user stopped the instance while it was starting."""


def route_id(instance_id: str) -> str:
    """The bridge route (task and episode id) an instance's Agent claims."""
    return f"launcher-{instance_id}"


def device_type_from_health(health: dict[str, Any]) -> str:
    """Map a bridge /health payload to the Agent's AICO_DEVICE_TYPE value."""
    try:
        return DEVICE_TYPE_BY_PLATFORM[resolve_environment_platform(health)]
    except (ValueError, KeyError):
        return ""


def field_spec(
    name: str,
    label: str,
    kind: str,
    *,
    required: bool = False,
    default: Any = None,
    help: str = "",
    placeholder: str = "",
    options: list[dict[str, str]] | None = None,
    dynamic_options: bool = False,
) -> dict[str, Any]:
    return {
        "name": name,
        "label": label,
        "kind": kind,
        "required": required,
        "default": default,
        "help": help,
        "placeholder": placeholder,
        "options": options or [],
        "dynamic_options": dynamic_options,
    }


def action_spec(name: str, label: str, help: str = "") -> dict[str, str]:
    return {"name": name, "label": label, "help": help}


def check(name: str, ok: bool, detail: str = "") -> dict[str, Any]:
    return {"name": name, "ok": bool(ok), "detail": detail}


def action_result(ok: bool, message: str, checks: list[dict[str, Any]] | None = None, output: str = "") -> dict[str, Any]:
    if len(output) > MAX_ACTION_OUTPUT_CHARS:
        output = "...\n" + output[-MAX_ACTION_OUTPUT_CHARS:]
    return {"ok": bool(ok), "message": message, "checks": checks or [], "output": output}


def probe_result(checks: list[dict[str, Any]], *, device_type: str = "", options: dict[str, Any] | None = None,
                 ok: bool | None = None) -> dict[str, Any]:
    return {
        "ok": all(item["ok"] for item in checks) if ok is None else bool(ok),
        "checks": checks,
        "options": options or {},
        "device_type": device_type,
    }


def output_tail(result: Any, lines: int = 1) -> str:
    """Last lines of a CompletedProcess's stderr (or stdout), for check details."""
    text = (result.stderr or "").strip() or (result.stdout or "").strip()
    return "\n".join(text.splitlines()[-lines:]) if text else f"exit code {result.returncode}"


def is_loopback(host: str) -> bool:
    if host.lower() == "localhost":
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


@dataclass
class BridgeContext:
    """Everything a bridge needs while starting or stopping one instance."""

    instance_id: str
    config: dict[str, Any]
    settings: dict[str, Any]
    system: System
    docker: Docker
    repo_root: Path
    bridge_log: Path
    state_dir: Path = Path(".")
    runtime: dict[str, Any] = field(default_factory=dict)
    step: Callable[[str], None] = lambda text: None
    save_runtime: Callable[[dict[str, Any]], None] = lambda runtime: None
    cancelled: Callable[[], bool] = lambda: False
    # Device keys held by other active instances -> their names.
    in_use: Callable[[], dict[str, str]] = dict
    # Serializes device claims across instances (the instance manager's lock).
    claim_lock: Any = field(default_factory=threading.RLock)

    @property
    def route_id(self) -> str:
        return route_id(self.instance_id)

    @property
    def benchmark_dir(self) -> Path:
        return self.repo_root / "benchmark"

    def remember(self, **values: Any) -> None:
        self.runtime.update(values)
        self.save_runtime(dict(values))

    def claim_device(self, key: str, description: str) -> None:
        """Reserve a device key for this instance, or raise when another instance holds it.

        The check and the reservation happen under one lock, so two instances
        that picked the same device at start cannot both reserve it.
        """
        if not key:
            return
        with self.claim_lock:
            owner = self.in_use().get(key)
            if owner:
                raise RuntimeError(f"{description} is already used by instance {owner!r}.")
            self.remember(device_claim=key)

    def log(self, text: str) -> None:
        self.bridge_log.parent.mkdir(parents=True, exist_ok=True)
        with self.bridge_log.open("a", encoding="utf-8") as handle:
            handle.write(text.rstrip() + "\n")

    def wait_for_health(
        self, url: str, timeout_sec: float, *, alive: Callable[[], str] | None = None
    ) -> dict[str, Any]:
        """Poll a bridge /health until it answers; alive() returns a death reason."""
        deadline = self.system.monotonic() + timeout_sec
        last_error = ""
        while True:
            if self.cancelled():
                raise Cancelled()
            try:
                return self.system.read_health(url, timeout=2.0)
            except Exception as exc:  # noqa: BLE001 - any failure means "not ready yet"
                last_error = str(exc)
            if alive is not None:
                reason = alive()
                if reason:
                    raise RuntimeError(reason)
            if self.system.monotonic() >= deadline:
                break
            self.system.sleep(1.0)
        suffix = f" ({last_error})" if last_error else ""
        raise RuntimeError(f"The bridge at {url} did not become ready within {int(timeout_sec)} s{suffix}.")

    def ensure_pyenv(self, env: pyenv.PythonEnv) -> str:
        """Prepare a launcher-managed Python environment, logging to the bridge log."""
        if not pyenv.is_ready(self.system, env, self.state_dir):
            self.step("Preparing bridge dependencies (the first time takes a minute or two)")

        def run(args: Sequence[str]) -> tuple[int, str]:
            code = self.system.run_logged(
                args, self.bridge_log, cwd=self.benchmark_dir, should_stop=self.cancelled,
                env=pyenv.uv_environ(self.system.environ()),
            )
            if code == -1 and self.cancelled():
                raise Cancelled()
            return code, ""

        try:
            python, _ = pyenv.ensure(self.system, env, self.state_dir, run, should_stop=self.cancelled)
        except pyenv.PyEnvCancelled as exc:
            raise Cancelled() from exc
        except pyenv.PyEnvError as exc:
            raise RuntimeError(f"{exc} The bridge log has the details.") from exc
        return python


@dataclass
class BridgeStart:
    """Result of starting a bridge."""

    endpoint: str = ""
    device_type: str = ""
    environment_web: str = ""


class BridgeType:
    type = ""
    label = ""
    description = ""
    planned = False
    # Name of a quick-actions preset baked into the image, or "" for the Agent defaults.
    quick_actions_preset = ""

    def fields(self, host: "HostFacts") -> list[dict[str, Any]]:
        return []

    def notes(self, host: "HostFacts") -> list[str]:
        """Short plain-text setup guidance shown under the fields."""
        return []

    def actions(self) -> list[dict[str, str]]:
        """Buttons shown next to Check; run through :meth:`run_action`."""
        return []

    def availability(self, host: "HostFacts") -> tuple[bool, str]:
        if self.planned:
            return False, PLANNED_REASON
        return True, ""

    def spec(self, host: "HostFacts") -> dict[str, Any]:
        available, reason = self.availability(host)
        return {
            "type": self.type,
            "label": self.label,
            "description": self.description,
            "available": available,
            "unavailable_reason": "" if available else reason,
            "fields": self.fields(host),
            "notes": self.notes(host),
            "actions": self.actions(),
        }

    def normalize(self, config: Any) -> dict[str, Any]:
        return {}

    def require_complete(self, config: dict[str, Any], host: "HostFacts") -> None:
        """Raise ValidationError when a normalized config cannot be saved or started.

        A required checkbox is a confirmation and must be checked.
        """
        for spec in self.fields(host):
            if spec["kind"] == "checkbox" and spec["required"] and config.get(spec["name"]) is not True:
                raise validation.ValidationError(f"Confirm “{spec['label']}” before starting this bridge.")

    def device_claim(self, config: dict[str, Any]) -> tuple[str, str]:
        """(key, description) of the device this config holds exclusively, or ('', '').

        Two active instances never hold the same key.
        """
        return "", ""

    def probe(self, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        return probe_result([])

    def run_action(self, name: str, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        raise LookupError(name)

    def start(self, ctx: BridgeContext) -> BridgeStart:
        return BridgeStart()

    def stop(self, ctx: BridgeContext) -> None:
        return None

    def problem(self, runtime: dict[str, Any], containers: dict[str, dict[str, str]], system: System) -> str:
        """Return why a running bridge is unhealthy, or '' when it looks alive."""
        return ""

    def has_leftovers(self, runtime: dict[str, Any], containers: dict[str, dict[str, str]], system: System) -> bool:
        return False

    def renew(self, instance_id: str, runtime: dict[str, Any], system: System) -> str:
        """Keep a running instance's bridge route claimed; returns a failure reason or ''."""
        return ""


def config_dict(config: Any) -> dict[str, Any]:
    if config is None:
        return {}
    if not isinstance(config, dict):
        raise validation.ValidationError("Bridge config must be an object.")
    return config


def install_action(env: pyenv.PythonEnv, host: "HostFacts") -> dict[str, Any]:
    """The install_dependencies action: prepare env and report what happened."""
    output: list[str] = []
    # Stay inside the action timeout across both uv steps, leaving room for a follow-up check.
    deadline = host.system.monotonic() + ACTION_TIMEOUT_SEC - 90

    def run(args: Sequence[str]) -> tuple[int, str]:
        output.append("$ " + " ".join(str(arg) for arg in args))
        remaining = max(10.0, deadline - host.system.monotonic())
        result = host.system.run(
            args, timeout=remaining, cwd=BENCHMARK_ROOT, env=pyenv.uv_environ(host.system.environ())
        )
        text = (result.stdout or "") + (result.stderr or "")
        output.append(text.rstrip())
        return result.returncode, text

    try:
        python, installed = pyenv.ensure(host.system, env, host.state_dir, run)
    except pyenv.PyEnvError as exc:
        return action_result(False, str(exc), [check("Dependencies", False, str(exc))], "\n".join(output))
    message = "Dependencies installed." if installed else "Dependencies are already installed."
    detail = f"{', '.join(env.requirements)} in {Path(python).parent.parent}"
    return action_result(True, message, [check("Dependencies", True, detail)], "\n".join(output))


def dependency_check(env: pyenv.PythonEnv, host: "HostFacts") -> dict[str, Any]:
    """A Check line for a launcher-managed environment (never installs)."""
    if pyenv.is_ready(host.system, env, host.state_dir):
        return check("Dependencies", True, "installed")
    if not host.uv:
        return check("Dependencies", False, pyenv.UV_MISSING)
    return check(
        "Dependencies", True,
        "not installed yet; Start installs them automatically, or click Install dependencies",
    )


class HostProcessBridge(BridgeType):
    """A bridge that runs as a detached ``python -m <module>`` process on this computer.

    The pid and module are remembered in the instance runtime, so a restarted
    launcher can check and stop the process. A pid is only signalled while its
    command line still runs the module: pids are recycled.
    """

    module = ""
    process_label = "bridge"
    ready_timeout_sec = BRIDGE_READY_TIMEOUT_SEC
    # How long the bridge gets to shut down after SIGTERM before SIGKILL.
    stop_timeout_sec = BRIDGE_STOP_TIMEOUT_SEC
    # The bridge drops its route lease after a period without requests (the
    # desktop-style bridges: 30 minutes), so the launcher claims it again.
    renews_route = False

    def launch(
        self,
        ctx: BridgeContext,
        argv: Sequence[str],
        *,
        bind_host: str,
        port: int,
        timeout_sec: float | None = None,
        wait_step: str = "",
        **remember: Any,
    ) -> dict[str, Any]:
        """Start argv (cwd benchmark/) and wait for its /health; returns the health payload.

        wait_step replaces the generic waiting step, for bridges that do slow
        work (such as compiling a helper) before they answer /health.
        """
        if ctx.cancelled():
            raise Cancelled()
        ctx.step(f"Starting the {self.process_label}")
        pid = ctx.system.start_process(argv, cwd=ctx.benchmark_dir, log_path=ctx.bridge_log)
        ctx.remember(bridge_pid=pid, bridge_module=self.module, bridge_port=port, bridge_host=bind_host, **remember)

        def died() -> str:
            if ctx.system.process_runs_module(pid, self.module):
                return ""
            lines = [line for line in tail_text(ctx.bridge_log, 4000).splitlines() if line.strip()]
            last = f" Last output: {lines[-1].strip()}" if lines else ""
            return f"The {self.process_label} process exited; see the bridge log.{last}"

        ctx.step(wait_step or f"Waiting for the {self.process_label}")
        return ctx.wait_for_health(
            f"http://{bind_host}:{port}", timeout_sec or self.ready_timeout_sec, alive=died
        )

    @staticmethod
    def python_command(python: str, module: str, *args: Any) -> list[str]:
        return [python, "-u", "-m", module, *(str(arg) for arg in args)]

    @staticmethod
    def launcher_python() -> str:
        """The launcher's own interpreter; it has the benchmark lock's packages."""
        return sys.executable

    def _pid(self, runtime: dict[str, Any]) -> tuple[int, str]:
        # adb_pid: instances started by the first launcher release.
        pid = int(runtime.get("bridge_pid") or runtime.get("adb_pid") or 0)
        return pid, str(runtime.get("bridge_module") or self.module)

    def _alive(self, runtime: dict[str, Any], system: System) -> bool:
        pid, module = self._pid(runtime)
        return bool(pid) and system.process_runs_module(pid, module)

    def stop(self, ctx: BridgeContext) -> None:
        pid, _ = self._pid(ctx.runtime)
        if self._alive(ctx.runtime, ctx.system):
            ctx.system.terminate(pid, wait_sec=self.stop_timeout_sec)

    def problem(self, runtime: dict[str, Any], containers: dict[str, dict[str, str]], system: System) -> str:
        if not self._alive(runtime, system):
            return f"The {self.process_label} process is no longer running."
        return ""

    def has_leftovers(self, runtime: dict[str, Any], containers: dict[str, dict[str, str]], system: System) -> bool:
        return self._alive(runtime, system)

    def renew(self, instance_id: str, runtime: dict[str, Any], system: System) -> str:
        """Claim the route again through POST /api/setup.

        Setup on these bridges does not reset the device, and claiming the
        route the instance already holds only renews its lease.
        """
        port = int(runtime.get("bridge_port") or 0)
        bind_host = str(runtime.get("bridge_host") or "")
        if not self.renews_route or not port or not bind_host or not self._alive(runtime, system):
            return ""
        status = system.post_json(
            f"http://{bind_host}:{port}/api/setup", {},
            headers={"benchmark-task-id": route_id(instance_id)}, timeout=ROUTE_RENEW_TIMEOUT_SEC,
        )
        if status == 200:
            return ""
        return f"Renewing the {self.process_label} route failed (HTTP {status or 'no answer'})."

    def claim_free(self, ctx: BridgeContext, key: str, description: str) -> None:
        ctx.claim_device(key, description)
