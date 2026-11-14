"""Environment bridge types: field specs, probes, and per-type start/stop.

A bridge runs on the host (or in its own container) and the Agent container
reaches it through host.docker.internal. Each type normalizes its user config,
can probe the host for prerequisites, and starts/stops its bridge for one
instance. Runtime facts that must survive a launcher restart (pids, container
names, ports) are persisted through ``BridgeContext.save_runtime``.
"""

from __future__ import annotations

import os
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any, Callable

from runner.environment import (
    DEFAULT_MOBILEGYM_IMAGE,
    MOBILEGYM_COMMIT_LABEL,
    build_mobilegym_environment_command,
)
from runner.platform import TargetPlatform, resolve_environment_platform

from . import LABEL_INSTANCE, LABEL_ROLE, validation
from .docker import Docker
from .net import container_side_url, host_proxy_args, host_side_url
from .system import System

PLANNED_REASON = "Planned for a later Launcher release"
BRIDGE_READY_TIMEOUT_SEC = 120
ADB_READY_TIMEOUT_SEC = 45
MOBILEGYM_WEB_PORT = 4173
MOBILEGYM_BRIDGE_PORT = 9090
MAX_MOBILEGYM_ENVS = 8

DEVICE_TYPE_BY_PLATFORM = {
    TargetPlatform.ANDROID: "Android",
    TargetPlatform.IOS: "iOS",
    TargetPlatform.MAC: "macOS",
    TargetPlatform.WINDOWS: "windows",
    TargetPlatform.LINUX: "linux",
}


class Cancelled(Exception):
    """The user stopped the instance while it was starting."""


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


def check(name: str, ok: bool, detail: str = "") -> dict[str, Any]:
    return {"name": name, "ok": bool(ok), "detail": detail}


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
    runtime: dict[str, Any] = field(default_factory=dict)
    step: Callable[[str], None] = lambda text: None
    save_runtime: Callable[[dict[str, Any]], None] = lambda runtime: None
    cancelled: Callable[[], bool] = lambda: False

    def remember(self, **values: Any) -> None:
        self.runtime.update(values)
        self.save_runtime(dict(values))

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

    def fields(self, host: "HostFacts") -> list[dict[str, Any]]:
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
        }

    def normalize(self, config: Any) -> dict[str, Any]:
        return {}

    def probe(self, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        return {"ok": True, "checks": [], "options": {}, "device_type": ""}

    def start(self, ctx: BridgeContext) -> BridgeStart:
        return BridgeStart()

    def stop(self, ctx: BridgeContext) -> None:
        return None

    def problem(self, runtime: dict[str, Any], containers: dict[str, dict[str, str]], system: System) -> str:
        """Return why a running bridge is unhealthy, or '' when it looks alive."""
        return ""

    def has_leftovers(self, runtime: dict[str, Any], containers: dict[str, dict[str, str]], system: System) -> bool:
        return False


def _config_dict(config: Any) -> dict[str, Any]:
    if config is None:
        return {}
    if not isinstance(config, dict):
        raise validation.ValidationError("Bridge config must be an object.")
    return config


class NoBridge(BridgeType):
    type = "none"
    label = "No device"
    description = "Chat with the Agent only. Nothing on screen to observe or control."


class CustomBridge(BridgeType):
    type = "custom"
    label = "Custom bridge"
    description = (
        "Connect to an Environment Bridge you already run on this computer or the network."
    )

    def fields(self, host: "HostFacts") -> list[dict[str, Any]]:
        return [
            field_spec(
                "endpoint",
                "Bridge endpoint",
                "text",
                required=True,
                default="",
                placeholder="http://host.docker.internal:19090",
                help=(
                    "URL the Agent container uses. Use host.docker.internal for a bridge on this "
                    "computer; 127.0.0.1 and localhost are rewritten automatically. On Linux, a "
                    "bridge on this computer must listen on 0.0.0.0 or the docker0 gateway, "
                    "because containers cannot reach its 127.0.0.1."
                ),
            )
        ]

    def normalize(self, config: Any) -> dict[str, Any]:
        config = _config_dict(config)
        return {"endpoint": validation.http_url(config.get("endpoint"), field="Bridge endpoint")}

    @staticmethod
    def read_health(system: System, endpoint: str) -> tuple[str, dict[str, Any]]:
        """Read /health at the address containers reach; returns (url, health).

        A bridge on this computer is probed through the address the Agent
        container uses (the docker0 gateway on Linux), so a bridge that only
        listens on 127.0.0.1 is reported instead of appearing healthy.
        """
        try:
            bind_host = system.bridge_bind_host()
        except Exception as exc:  # noqa: BLE001 - docker missing or no bridge network
            raise RuntimeError(f"Could not find the address containers reach this computer at: {exc}") from exc
        url = host_side_url(endpoint, bind_host)
        try:
            return url, system.read_health(url, timeout=5.0)
        except Exception as exc:  # noqa: BLE001
            loopback = host_side_url(endpoint)
            if loopback != url:
                try:
                    system.read_health(loopback, timeout=2.0)
                except Exception:  # noqa: BLE001
                    pass
                else:
                    raise RuntimeError(
                        f"The bridge listens only on 127.0.0.1; containers reach this computer at "
                        f"{bind_host}. Bind the bridge to 0.0.0.0 or {bind_host}."
                    ) from exc
            raise RuntimeError(f"{url}/health did not answer: {exc}") from exc

    def probe(self, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        endpoint = config["endpoint"]
        try:
            url, health = self.read_health(host.system, endpoint)
        except RuntimeError as exc:
            return {
                "ok": False,
                "checks": [check("Bridge /health", False, str(exc))],
                "options": {},
                "device_type": "",
            }
        device_type = device_type_from_health(health)
        checks = [check("Bridge /health", True, f"{url}/health answered")]
        if device_type:
            checks.append(check("Platform", True, f"reports {health.get('platform') or device_type}"))
        else:
            checks.append(check("Platform", False, "the bridge does not report a supported platform"))
        return {"ok": bool(device_type), "checks": checks, "options": {}, "device_type": device_type}

    def start(self, ctx: BridgeContext) -> BridgeStart:
        endpoint = ctx.config["endpoint"]
        ctx.step(f"Checking the bridge at {endpoint}")
        try:
            url, health = self.read_health(ctx.system, endpoint)
        except RuntimeError as exc:
            raise RuntimeError(f"The bridge at {endpoint} is not reachable: {exc}") from exc
        device_type = device_type_from_health(health)
        if not device_type:
            raise RuntimeError(f"The bridge at {url} does not report a supported platform.")
        return BridgeStart(endpoint=container_side_url(endpoint), device_type=device_type)


@dataclass
class ADBDevice:
    serial: str
    state: str


def parse_adb_devices(output: str) -> list[ADBDevice]:
    devices: list[ADBDevice] = []
    for line in output.splitlines():
        line = line.strip()
        if not line or line.startswith(("List of devices", "*")):
            continue
        parts = line.split()
        if len(parts) >= 2:
            devices.append(ADBDevice(parts[0], parts[1]))
    return devices


ADB_STATE_HELP = {
    "unauthorized": "unauthorized: accept the USB debugging prompt on the device",
    "offline": "offline: reconnect the device or restart adb",
    "no": "no permissions: check udev rules or reconnect the device",
}


def _android_sdk_adb(environ: Any) -> str:
    candidates = []
    for name in ("ANDROID_HOME", "ANDROID_SDK_ROOT"):
        if environ.get(name):
            candidates.append(Path(environ[name]) / "platform-tools" / "adb")
    home = Path(os.path.expanduser("~"))
    candidates += [
        home / "Library" / "Android" / "sdk" / "platform-tools" / "adb",
        home / "Android" / "Sdk" / "platform-tools" / "adb",
    ]
    for candidate in candidates:
        if candidate.is_file() and os.access(candidate, os.X_OK):
            return str(candidate)
    return ""


class ADBAndroidBridge(BridgeType):
    type = "adb_android"
    label = "Android (ADB)"
    description = "Control an Android phone, emulator, or VM through adb on this computer."

    def default_adb(self, host: "HostFacts") -> str:
        # Prefer the plain name when adb is on PATH; otherwise the SDK location found.
        if not host.adb_path or host.system.which("adb"):
            return "adb"
        return host.adb_path

    def availability(self, host: "HostFacts") -> tuple[bool, str]:
        if not host.adb_path:
            return False, "adb was not found. Install Android SDK Platform Tools and put adb on PATH."
        return True, ""

    def fields(self, host: "HostFacts") -> list[dict[str, Any]]:
        return [
            field_spec(
                "serial",
                "Device",
                "select",
                required=True,
                default="",
                dynamic_options=True,
                help="Run the check to list devices from `adb devices`. The device must be authorized.",
            ),
            field_spec(
                "adb_path",
                "adb path",
                "text",
                default=self.default_adb(host),
                placeholder="adb",
                help="adb executable used by the bridge.",
            ),
        ]

    def normalize(self, config: Any) -> dict[str, Any]:
        config = _config_dict(config)
        return {
            "serial": str(config.get("serial") or "").strip(),
            "adb_path": os.path.expanduser(
                validation.executable(config.get("adb_path"), field="adb path", default="adb")
            ),
        }

    def _devices(self, system: System, adb_path: str) -> tuple[list[ADBDevice] | None, str]:
        result = system.run([adb_path, "devices"], timeout=15)
        if result.returncode != 0:
            detail = (result.stderr or result.stdout or "").strip() or f"exit code {result.returncode}"
            return None, detail
        return parse_adb_devices(result.stdout), ""

    def probe(self, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        adb_path = config["adb_path"]
        devices, error = self._devices(host.system, adb_path)
        if devices is None:
            return {
                "ok": False,
                "checks": [check("adb", False, f"`{adb_path} devices` failed: {error}")],
                "options": {"serial": []},
                "device_type": "",
            }
        checks = [check("adb", True, f"`{adb_path} devices` works")]
        options = [
            {"value": device.serial, "label": f"{device.serial} ({'authorized' if device.state == 'device' else device.state})"}
            for device in devices
        ]
        if not devices:
            checks.append(check("Devices", False, "No device is connected. Start an emulator or connect a phone with USB debugging on."))
        in_use = host.serials_in_use()
        for device in devices:
            if device.state != "device":
                checks.append(check(device.serial, False, ADB_STATE_HELP.get(device.state, device.state)))
            elif device.serial in in_use:
                checks.append(check(device.serial, False, f"authorized, but already used by instance {in_use[device.serial]}"))
            else:
                checks.append(check(device.serial, True, "authorized"))
        selected = config.get("serial") or ""
        authorized = {d.serial for d in devices if d.state == "device"} - set(in_use)
        if selected:
            ok = selected in authorized
            if selected not in {d.serial for d in devices}:
                checks.append(check(selected, False, "not listed by `adb devices`"))
        else:
            ok = bool(authorized)
        return {"ok": ok, "checks": checks, "options": {"serial": options}, "device_type": "Android"}

    def start(self, ctx: BridgeContext) -> BridgeStart:
        serial = validation.adb_serial(ctx.config.get("serial"))
        adb_path = os.path.expanduser(ctx.config.get("adb_path") or "adb")
        ctx.step(f"Checking ADB device {serial}")
        state = ctx.system.run([adb_path, "-s", serial, "get-state"], timeout=15)
        device_state = state.stdout.strip()
        if state.returncode != 0 or device_state != "device":
            detail = device_state or (state.stderr or "").strip() or "not connected"
            raise RuntimeError(f"ADB device {serial} is not ready: {ADB_STATE_HELP.get(detail, detail)}.")
        bind_host = ctx.system.bridge_bind_host()
        port = ctx.system.free_port()
        ctx.step(f"Starting the ADB bridge for {serial}")
        pid = ctx.system.start_adb_bridge(
            serial=serial, port=port, host=bind_host, adb_path=adb_path, log_path=ctx.bridge_log
        )
        ctx.remember(adb_pid=pid, adb_serial=serial, bridge_port=port, bridge_host=bind_host)

        def died() -> str:
            if ctx.system.pid_command(pid):
                return ""
            return "The ADB bridge process exited; see the bridge log."

        ctx.step("Waiting for the ADB bridge")
        ctx.wait_for_health(f"http://{bind_host}:{port}", ADB_READY_TIMEOUT_SEC, alive=died)
        return BridgeStart(endpoint=f"http://host.docker.internal:{port}", device_type="Android")

    def stop(self, ctx: BridgeContext) -> None:
        pid = int(ctx.runtime.get("adb_pid") or 0)
        # Only signal a pid that is still our bridge: pids are recycled.
        if pid and ctx.system.is_adb_bridge(pid):
            ctx.system.terminate(pid)

    def problem(self, runtime: dict[str, Any], containers: dict[str, dict[str, str]], system: System) -> str:
        pid = int(runtime.get("adb_pid") or 0)
        if not pid or not system.is_adb_bridge(pid):
            return "The ADB bridge process is no longer running."
        return ""

    def has_leftovers(self, runtime: dict[str, Any], containers: dict[str, dict[str, str]], system: System) -> bool:
        pid = int(runtime.get("adb_pid") or 0)
        return bool(pid) and system.is_adb_bridge(pid)


def mobilegym_container(instance_id: str) -> str:
    return f"aico-launcher-{instance_id}-mobilegym"


class MobileGymBridge(BridgeType):
    type = "mobilegym"
    label = "MobileGym simulator"
    description = "A simulated Android phone in Docker, with its own web view."

    def availability(self, host: "HostFacts") -> tuple[bool, str]:
        if not host.docker_cli:
            return False, "Docker CLI not found."
        if not host.mobilegym_commit:
            return False, host.mobilegym_error or "The MobileGym submodule commit cannot be resolved."
        return True, ""

    def fields(self, host: "HostFacts") -> list[dict[str, Any]]:
        return [
            field_spec(
                "envs",
                "Parallel environments",
                "number",
                default=1,
                placeholder="1",
                help=f"Simulated phones inside the container (1 to {MAX_MOBILEGYM_ENVS}). One is enough for chat.",
            )
        ]

    def normalize(self, config: Any) -> dict[str, Any]:
        config = _config_dict(config)
        raw = config.get("envs")
        envs = 1 if raw in (None, "") else validation.integer(
            raw, field="Parallel environments", minimum=1, maximum=MAX_MOBILEGYM_ENVS
        )
        return {"envs": envs}

    def probe(self, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        docker_info = host.docker_info()
        checks = [
            check(
                "Docker",
                docker_info["available"],
                f"Docker {docker_info['version']}" if docker_info["available"] else docker_info["error"],
            )
        ]
        commit = host.mobilegym_commit
        checks.append(
            check(
                "MobileGym source",
                bool(commit),
                f"pinned commit {commit[:12]}" if commit else host.mobilegym_error,
            )
        )
        if docker_info["available"] and commit:
            labels = host.docker.image_labels(DEFAULT_MOBILEGYM_IMAGE)
            if labels is None:
                detail = "not built yet; the first start builds it, which may take 10 minutes or more"
            elif labels.get(MOBILEGYM_COMMIT_LABEL) != commit:
                detail = "built from an older commit; the next start rebuilds it, which may take 10 minutes or more"
            else:
                detail = f"{DEFAULT_MOBILEGYM_IMAGE} is ready"
            checks.append(check("Simulator image", True, detail))
        ok = all(item["ok"] for item in checks)
        return {"ok": ok, "checks": checks, "options": {}, "device_type": "Android"}

    def start(self, ctx: BridgeContext) -> BridgeStart:
        container = mobilegym_container(ctx.instance_id)
        ctx.docker.remove(container)
        build_args: dict[str, str] = {}
        node_image = (ctx.settings.get("build") or {}).get("mobilegym_node_image")
        if node_image:
            build_args["MOBILEGYM_NODE_IMAGE"] = str(node_image)
        if ctx.settings.get("pass_host_proxy"):
            build_args.update(host_proxy_args(ctx.system.environ()))
        ctx.step("Preparing the MobileGym simulator image (the first build may take 10 minutes or more)")
        try:
            ctx.system.ensure_mobilegym_image(
                DEFAULT_MOBILEGYM_IMAGE,
                ctx.bridge_log,
                repo_root=ctx.repo_root,
                build_args=build_args,
                should_stop=ctx.cancelled,
            )
        except Exception as exc:
            if ctx.cancelled():
                raise Cancelled() from exc
            raise
        if ctx.cancelled():
            raise Cancelled()
        bind_host = ctx.system.bridge_bind_host()
        command = build_mobilegym_environment_command(
            image=DEFAULT_MOBILEGYM_IMAGE,
            container_name=container,
            host_web_port=0,
            host_bridge_port=0,
            bridge_bind_host=bind_host,
            benchmark_dir=ctx.repo_root / "benchmark",
            parallel_envs=int(ctx.config.get("envs") or 1),
        )
        # Keep the container when it dies so its output can be read; start()
        # and stop() remove it explicitly.
        command = [arg for arg in command if arg != "--rm"]
        name_index = command.index("--name")
        command[name_index:name_index] = [
            "--label", f"{LABEL_INSTANCE}={ctx.instance_id}",
            "--label", f"{LABEL_ROLE}=mobilegym",
        ]
        ctx.step("Starting the MobileGym simulator")
        ctx.remember(bridge_container=container)
        with ctx.bridge_log.open("a", encoding="utf-8") as log:
            log.write("$ " + " ".join(command) + "\n")
        result = ctx.system.run(command, cwd=ctx.repo_root, timeout=120)
        if result.returncode != 0:
            detail = (result.stderr or result.stdout or "").strip()
            raise RuntimeError(f"Could not start the MobileGym container: {detail}")
        _, bridge_port = ctx.docker.published_port(container, MOBILEGYM_BRIDGE_PORT)
        _, web_port = ctx.docker.published_port(container, MOBILEGYM_WEB_PORT)
        ctx.remember(bridge_port=bridge_port, bridge_host=bind_host, environment_web_port=web_port)

        def died() -> str:
            state = ctx.docker.container_state(container)
            if state == "running":
                return ""
            # Save the output now: the failed start removes the container.
            output = ctx.docker.logs(container, 200)
            if output:
                with ctx.bridge_log.open("a", encoding="utf-8") as log:
                    log.write(f"--- {container} output ---\n{output}")
                    if not output.endswith("\n"):
                        log.write("\n")
            return "The MobileGym container exited; see the bridge log."

        ctx.step("Waiting for the MobileGym bridge")
        ctx.wait_for_health(f"http://{bind_host}:{bridge_port}", BRIDGE_READY_TIMEOUT_SEC, alive=died)
        return BridgeStart(
            endpoint=f"http://host.docker.internal:{bridge_port}",
            device_type="Android",
            environment_web=f"http://127.0.0.1:{web_port}",
        )

    def stop(self, ctx: BridgeContext) -> None:
        container = ctx.runtime.get("bridge_container") or mobilegym_container(ctx.instance_id)
        ctx.docker.remove(container)

    def problem(self, runtime: dict[str, Any], containers: dict[str, dict[str, str]], system: System) -> str:
        container = runtime.get("bridge_container") or ""
        if (containers.get(container) or {}).get("state") != "running":
            return "The MobileGym container is no longer running."
        return ""

    def has_leftovers(self, runtime: dict[str, Any], containers: dict[str, dict[str, str]], system: System) -> bool:
        container = runtime.get("bridge_container") or ""
        return (containers.get(container) or {}).get("state") == "running"


class PlannedBridge(BridgeType):
    planned = True

    def __init__(self, type_: str, label: str, description: str) -> None:
        self.type = type_
        self.label = label
        self.description = description


BRIDGE_TYPES: dict[str, BridgeType] = {
    bridge.type: bridge
    for bridge in (
        NoBridge(),
        MobileGymBridge(),
        ADBAndroidBridge(),
        CustomBridge(),
        PlannedBridge("desktop", "Host desktop", "Let the Agent operate this computer's desktop."),
        PlannedBridge("iphone_mirroring", "iPhone Mirroring", "Control a real iPhone through macOS iPhone Mirroring."),
        PlannedBridge("ios_device", "iOS device", "Control a real iPhone over USB."),
        PlannedBridge("vphone", "Virtual iPhone", "Control a vphone virtual iPhone."),
    )
}


def get_bridge(type_: str) -> BridgeType:
    bridge = BRIDGE_TYPES.get(str(type_ or ""))
    if bridge is None:
        raise validation.ValidationError(f"Unknown bridge type: {type_!r}.")
    return bridge


class HostFacts:
    """Host prerequisites the bridge specs and probes need, cached briefly."""

    TTL_SEC = 10.0

    def __init__(
        self,
        system: System,
        docker: Docker,
        repo_root: Path,
        serials_in_use: Callable[[], dict[str, str]] = dict,
    ) -> None:
        self.system = system
        self.docker = docker
        self.repo_root = repo_root
        self.serials_in_use = serials_in_use
        self._expires = 0.0
        self._docker_info: dict[str, Any] = {}
        self._docker_info_expires = 0.0
        self.docker_cli = ""
        self.adb_path = ""
        self.mobilegym_commit = ""
        self.mobilegym_error = ""

    def refresh(self, *, force: bool = False) -> "HostFacts":
        now = self.system.monotonic()
        if not force and now < self._expires:
            return self
        self.docker_cli = self.system.which("docker")
        self.adb_path = self.system.which("adb") or _android_sdk_adb(self.system.environ())
        try:
            self.mobilegym_commit = self.system.resolve_mobilegym_commit(self.repo_root)
            self.mobilegym_error = ""
        except Exception as exc:  # noqa: BLE001
            self.mobilegym_commit = ""
            self.mobilegym_error = str(exc)
        self._expires = now + self.TTL_SEC
        return self

    def docker_info(self, *, max_age: float = 3.0) -> dict[str, Any]:
        now = self.system.monotonic()
        if not self._docker_info or now >= self._docker_info_expires:
            self._docker_info = self.docker.info()
            self._docker_info_expires = now + max_age
        return dict(self._docker_info)

    def bridge_specs(self) -> list[dict[str, Any]]:
        self.refresh()
        return [bridge.spec(self) for bridge in BRIDGE_TYPES.values()]
