"""Instance lifecycle: create, start, stop, delete, and reconcile on launcher start.

An instance is one Agent container (Config Web on 80, Agent Web UI on 8080)
plus the environment bridge it is connected to. Start and stop run in
background threads; the UI follows progress through ``status`` and ``step``.
Running instances outlive the launcher process: on the next start
:meth:`InstanceManager.reconcile` re-attaches to them by container label and
stored pid.
"""

from __future__ import annotations

import os
import shutil
import threading
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

from runner.environment import tail_text

from . import LABEL_INSTANCE, LABEL_ROLE, validation
from .bridges import (
    BridgeContext,
    Cancelled,
    HostFacts,
    get_bridge,
)
from .docker import Docker, DockerError
from .net import host_proxy_args, no_proxy_value
from .store import Store, now_iso
from .system import System

DEFAULT_CONFIG_WEB_PORT = 8000
DEFAULT_AGENT_WEB_PORT = 8080
AGENT_READY_TIMEOUT_SEC = 180
AGENT_STOP_TIMEOUT_SEC = 20
ERROR_LOG_LINES = 40
MAX_LOG_TAIL_LINES = 5000
ACTIVE_STATUSES = {"starting", "running", "stopping"}
EDITABLE_STATUSES = {"stopped", "error"}


class NotFound(LookupError):
    pass


class Conflict(RuntimeError):
    pass


def agent_container(instance_id: str) -> str:
    return f"aico-launcher-{instance_id}-agent"


def data_volume(instance_id: str) -> str:
    return f"aico-launcher-{instance_id}-data"


def tail_lines(text: str, lines: int) -> str:
    parts = text.splitlines()
    return "\n".join(parts[-lines:]) + ("\n" if parts else "")


def choose_ports(
    requested: dict[str, int],
    taken: set[int],
    port_free: Callable[[int], bool],
    free_port: Callable[[], int],
) -> dict[str, int]:
    """Pick host ports: requested values win, else 8000/8080 when free, else any free port.

    ``taken`` holds ports already assigned to other instances; automatic
    choices avoid them even when those instances are stopped.
    """
    chosen: dict[str, int] = {}
    defaults = {"config_web": DEFAULT_CONFIG_WEB_PORT, "agent_web": DEFAULT_AGENT_WEB_PORT}
    for key in ("config_web", "agent_web"):
        if requested.get(key):
            chosen[key] = int(requested[key])
    for key in ("config_web", "agent_web"):
        if key in chosen:
            continue
        avoid = taken | set(chosen.values())
        preferred = defaults[key]
        if preferred not in avoid and port_free(preferred):
            chosen[key] = preferred
            continue
        for _ in range(50):
            candidate = free_port()
            if candidate not in avoid:
                chosen[key] = candidate
                break
        else:  # pragma: no cover - the OS keeps returning taken ports
            raise RuntimeError("Could not find a free local port.")
    return chosen


class InstanceManager:
    def __init__(
        self,
        *,
        store: Store,
        docker: Docker,
        system: System,
        host: HostFacts,
        state_dir: Path,
        repo_root: Path,
        image_present: Callable[[str], bool] | None = None,
    ) -> None:
        self.store = store
        self.docker = docker
        self.system = system
        self.host = host
        self.state_dir = Path(state_dir)
        self.repo_root = repo_root
        self._lock = threading.RLock()
        self._workers: dict[str, threading.Thread] = {}
        self._cancel: dict[str, threading.Event] = {}
        self._last_health_check = -1e9
        self.health_check_interval = 3.0
        self.ready_timeout_sec = AGENT_READY_TIMEOUT_SEC
        self.poll_interval = 1.0
        host.serials_in_use = self.serials_in_use

    # -- paths and logs -----------------------------------------------------

    def instance_dir(self, instance_id: str) -> Path:
        return self.state_dir / "instances" / instance_id

    def launcher_log(self, instance_id: str) -> Path:
        return self.instance_dir(instance_id) / "launcher.log"

    def bridge_log(self, instance_id: str) -> Path:
        return self.instance_dir(instance_id) / "bridge.log"

    def log(self, instance_id: str, message: str) -> None:
        path = self.launcher_log(instance_id)
        path.parent.mkdir(parents=True, exist_ok=True)
        stamp = datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
        with path.open("a", encoding="utf-8") as handle:
            for line in str(message).rstrip().splitlines() or [""]:
                handle.write(f"{stamp} {line}\n")

    # -- payloads -----------------------------------------------------------

    def payload(self, record: dict[str, Any]) -> dict[str, Any]:
        runtime = record.get("runtime") or {}
        ports = record.get("ports") or {}
        status = record.get("status") or "stopped"
        urls: dict[str, str] = {}
        if status == "running":
            urls["config_web"] = f"http://127.0.0.1:{ports.get('config_web')}/"
            urls["agent_web"] = f"http://127.0.0.1:{ports.get('agent_web')}/"
            if record.get("enable_terminal"):
                urls["terminal"] = f"http://127.0.0.1:{ports.get('agent_web')}/webtty/"
            if runtime.get("environment_web_port"):
                urls["environment_web"] = f"http://127.0.0.1:{runtime['environment_web_port']}/"
        return {
            "id": record["id"],
            "name": record.get("name", ""),
            "image": record.get("image", ""),
            "bridge": {
                "type": (record.get("bridge") or {}).get("type", "none"),
                "config": dict((record.get("bridge") or {}).get("config") or {}),
            },
            "ports": {"config_web": ports.get("config_web"), "agent_web": ports.get("agent_web")},
            "enable_terminal": bool(record.get("enable_terminal")),
            "status": status,
            "step": record.get("step", "") if status in {"starting", "stopping"} else "",
            "error": record.get("error", "") if status == "error" else "",
            "urls": urls,
            "device_type": record.get("device_type", ""),
            "bridge_endpoint": record.get("bridge_endpoint", ""),
            "created_at": record.get("created_at", ""),
            "updated_at": record.get("updated_at", ""),
        }

    def list(self) -> list[dict[str, Any]]:
        self.check_running()
        return [self.payload(record) for record in self.store.instances()]

    def get(self, instance_id: str) -> dict[str, Any]:
        record = self._record(instance_id)
        return self.payload(record)

    def _record(self, instance_id: str) -> dict[str, Any]:
        try:
            instance_id = validation.instance_id(instance_id)
        except validation.ValidationError:
            raise NotFound("Unknown instance.") from None
        record = self.store.instance(instance_id)
        if record is None:
            raise NotFound("Unknown instance.")
        return record

    # -- configuration ------------------------------------------------------

    def _validated(self, body: Any, *, instance_id: str, current: dict[str, Any] | None) -> dict[str, Any]:
        if not isinstance(body, dict):
            raise validation.ValidationError("Request body must be a JSON object.")
        name = validation.instance_name(body.get("name"))
        image = validation.image_ref(body.get("image"))
        bridge_body = body.get("bridge")
        if not isinstance(bridge_body, dict):
            raise validation.ValidationError("Choose an environment bridge.")
        bridge = get_bridge(bridge_body.get("type"))
        if bridge.planned:
            raise validation.ValidationError(f"{bridge.label} is not available yet.")
        config = bridge.normalize(bridge_body.get("config"))
        if bridge.type == "adb_android":
            config["serial"] = validation.adb_serial(config.get("serial"))
        ports_body = body.get("ports") or {}
        if not isinstance(ports_body, dict):
            raise validation.ValidationError("Ports must be an object.")
        requested = {
            "config_web": validation.optional_port(ports_body.get("config_web"), field="Config Web port"),
            "agent_web": validation.optional_port(ports_body.get("agent_web"), field="Agent Web UI port"),
        }
        if requested["config_web"] and requested["config_web"] == requested["agent_web"]:
            raise validation.ValidationError("Config Web and the Agent Web UI need different ports.")
        # A port the UI sends back unchanged from an automatic choice stays automatic.
        if current is not None:
            current_ports = current.get("ports") or {}
            for key in ("config_web", "agent_web"):
                if (current.get("ports_auto") or {}).get(key) and requested[key] == current_ports.get(key):
                    requested[key] = 0
        taken = self._ports_of_others(instance_id)
        ports = choose_ports(requested, taken, self.system.port_free, self.system.free_port)
        return {
            "name": name,
            "image": image,
            "bridge": {"type": bridge.type, "config": config},
            "ports": ports,
            "ports_auto": {key: not requested[key] for key in ports},
            "enable_terminal": validation.boolean(body.get("enable_terminal"), field="Terminal"),
        }

    def _ports_of_others(self, instance_id: str) -> set[int]:
        ports: set[int] = set()
        for record in self.store.instances():
            if record["id"] == instance_id:
                continue
            ports.update(int(value) for value in (record.get("ports") or {}).values() if value)
        return ports

    def create(self, body: Any) -> dict[str, Any]:
        instance_id = os.urandom(4).hex()
        with self._lock:
            fields = self._validated(body, instance_id=instance_id, current=None)
            stamp = now_iso()
            record = {
                "id": instance_id,
                **fields,
                "status": "stopped",
                "step": "",
                "error": "",
                "device_type": "",
                "bridge_endpoint": "",
                "runtime": {},
                "created_at": stamp,
                "updated_at": stamp,
            }
            self.store.put_instance(record)
        self.log(instance_id, f"Created instance {fields['name']!r} with image {fields['image']}")
        return self.payload(record)

    def update(self, instance_id: str, body: Any) -> dict[str, Any]:
        with self._lock:
            current = self._record(instance_id)
            if current.get("status") not in EDITABLE_STATUSES:
                raise Conflict("Stop the instance before editing it.")
            fields = self._validated(body, instance_id=current["id"], current=current)
            record = self.store.patch_instance(current["id"], **fields)
        self.log(current["id"], "Updated the instance configuration")
        return self.payload(record)

    def delete(self, instance_id: str, *, purge_data: bool) -> None:
        with self._lock:
            record = self._record(instance_id)
            if record.get("status") not in EDITABLE_STATUSES:
                raise Conflict("Stop the instance before deleting it.")
            self.store.patch_instance(record["id"], status="stopping", step="Removing the instance")
        try:
            self._teardown(record)
            if purge_data:
                self.docker.remove_volume(data_volume(record["id"]))
        except Exception:
            self.store.patch_instance(record["id"], status=record.get("status") or "error", step="")
            raise
        self.store.delete_instance(record["id"])
        shutil.rmtree(self.instance_dir(record["id"]), ignore_errors=True)

    def serials_in_use(self, exclude: str = "") -> dict[str, str]:
        """Map ADB serial -> instance name for instances that hold a device."""
        used: dict[str, str] = {}
        for record in self.store.instances():
            if record["id"] == exclude or record.get("status") not in ACTIVE_STATUSES:
                continue
            bridge = record.get("bridge") or {}
            if bridge.get("type") == "adb_android":
                serial = (bridge.get("config") or {}).get("serial")
                if serial:
                    used[str(serial)] = record.get("name") or record["id"]
        return used

    # -- start --------------------------------------------------------------

    def start(self, instance_id: str) -> dict[str, Any]:
        with self._lock:
            record = self._record(instance_id)
            if record.get("status") not in EDITABLE_STATUSES:
                raise Conflict(f"The instance is already {record.get('status')}.")
            worker = self._workers.get(record["id"])
            if worker is not None and worker.is_alive():
                raise Conflict("The instance is still busy; try again in a moment.")
            self._check_serial(record)
            record = self.store.patch_instance(
                record["id"], status="starting", step="Preparing", error="",
                device_type="", bridge_endpoint="",
            )
            cancel = threading.Event()
            self._cancel[record["id"]] = cancel
            thread = threading.Thread(
                target=self._run_start, args=(record["id"], cancel), daemon=True,
                name=f"launcher-start-{record['id']}",
            )
            self._workers[record["id"]] = thread
            thread.start()
        return self.payload(record)

    def _check_serial(self, record: dict[str, Any]) -> None:
        bridge = record.get("bridge") or {}
        if bridge.get("type") != "adb_android":
            return
        serial = (bridge.get("config") or {}).get("serial")
        owner = self.serials_in_use(exclude=record["id"]).get(str(serial))
        if owner:
            raise Conflict(f"ADB device {serial} is already used by instance {owner!r}.")

    def _step(self, instance_id: str, text: str) -> None:
        self.store.patch_instance(instance_id, step=text)
        self.log(instance_id, text)

    def _context(self, record: dict[str, Any], cancel: threading.Event | None = None) -> BridgeContext:
        instance_id = record["id"]
        bridge = record.get("bridge") or {}
        return BridgeContext(
            instance_id=instance_id,
            config=dict(bridge.get("config") or {}),
            settings=self.store.settings(),
            system=self.system,
            docker=self.docker,
            repo_root=self.repo_root,
            bridge_log=self.bridge_log(instance_id),
            runtime=dict(record.get("runtime") or {}),
            step=lambda text: self._step(instance_id, text),
            save_runtime=lambda values: self.store.patch_instance(instance_id, runtime=values),
            cancelled=(cancel.is_set if cancel is not None else (lambda: False)),
        )

    def _run_start(self, instance_id: str, cancel: threading.Event) -> None:
        record = self.store.instance(instance_id) or {}
        self.bridge_log(instance_id).parent.mkdir(parents=True, exist_ok=True)
        ctx = self._context(record, cancel)
        try:
            # Leftovers from an earlier failed run would block names and ports.
            self._teardown(record, quiet=True)
            ctx.runtime.clear()
            self.store.patch_instance(instance_id, runtime=None)
            self._step(instance_id, "Checking Docker")
            record = self._preflight(record, ctx, cancel)
            bridge = get_bridge((record.get("bridge") or {}).get("type"))
            result = bridge.start(ctx)
            if cancel.is_set():
                raise Cancelled()
            self.store.patch_instance(
                instance_id, device_type=result.device_type, bridge_endpoint=result.endpoint,
            )
            self._run_agent(record, result.endpoint, result.device_type, ctx)
            self._wait_for_agent(record, cancel)
            with self._lock:
                # stop() takes the same lock, so a Stop now cannot be lost.
                if cancel.is_set():
                    raise Cancelled()
                self.store.patch_instance(instance_id, status="running", step="")
            self.log(instance_id, "Running")
        except Cancelled:
            self.log(instance_id, "Start cancelled")
            self._cleanup_after_start(instance_id, ctx)
            self.store.patch_instance(instance_id, status="stopped", step="", runtime=None)
        except Exception as exc:  # noqa: BLE001 - every failure becomes instance.error
            message = str(exc) or exc.__class__.__name__
            self.log(instance_id, f"ERROR: {message}")
            self._cleanup_after_start(instance_id, ctx)
            self.store.patch_instance(
                instance_id, status="error", step="", error=message, runtime=None,
            )
        finally:
            with self._lock:
                self._workers.pop(instance_id, None)
                self._cancel.pop(instance_id, None)

    def _cleanup_after_start(self, instance_id: str, ctx: BridgeContext) -> None:
        record = self.store.instance(instance_id) or {"id": instance_id}
        record["runtime"] = {**(record.get("runtime") or {}), **ctx.runtime}
        self._teardown(record, quiet=True)

    def _preflight(self, record: dict[str, Any], ctx: BridgeContext, cancel: threading.Event) -> dict[str, Any]:
        instance_id = record["id"]
        info = self.docker.info()
        if not info["available"]:
            raise RuntimeError(info["error"] or "Docker is not available.")
        image = record["image"]
        if not self.docker.image_exists(image):
            if not validation.image_has_namespace(image):
                raise RuntimeError(f"Image {image} is not available locally; build or pull the image first.")
            self._step(instance_id, f"Pulling {image}")
            code = self.system.run_logged(
                ["docker", "pull", image], self.launcher_log(instance_id), should_stop=cancel.is_set
            )
            if cancel.is_set():
                raise Cancelled()
            if code != 0:
                raise RuntimeError(f"docker pull {image} failed; see the launcher log.")
        self._step(instance_id, "Checking ports")
        record = self._ensure_ports(record)
        bridge = get_bridge((record.get("bridge") or {}).get("type"))
        if bridge.type == "adb_android":
            # The configured adb counts, even when none is on PATH.
            self._check_adb(str(((record.get("bridge") or {}).get("config") or {}).get("adb_path") or "adb"))
        else:
            available, reason = bridge.availability(self.host.refresh())
            if not available:
                raise RuntimeError(f"{bridge.label} is not available: {reason}")
        self._check_serial(record)
        return record

    def _check_adb(self, adb_path: str) -> None:
        path = os.path.expanduser(adb_path)
        if os.sep in path or (os.altsep and os.altsep in path):
            found = os.path.isfile(path) and os.access(path, os.X_OK)
        else:
            found = bool(self.system.which(path))
        if not found:
            raise RuntimeError(
                f"adb was not found at {adb_path}. Install Android SDK Platform Tools or correct the adb path."
            )

    def _ensure_ports(self, record: dict[str, Any]) -> dict[str, Any]:
        ports = dict(record.get("ports") or {})
        auto = record.get("ports_auto") or {}
        busy_elsewhere = set()
        for other in self.store.instances():
            if other["id"] != record["id"] and other.get("status") in ACTIVE_STATUSES:
                busy_elsewhere.update(int(v) for v in (other.get("ports") or {}).values() if v)
        changed = False
        for key, label in (("config_web", "Config Web"), ("agent_web", "Agent Web UI")):
            port = int(ports.get(key) or 0)
            if port and port not in busy_elsewhere and self.system.port_free(port):
                continue
            if port and not auto.get(key):
                raise RuntimeError(
                    f"{label} port {port} is already in use on 127.0.0.1. Choose another port or stop what uses it."
                )
            ports.pop(key, None)
            changed = True
        if changed:
            taken = self._ports_of_others(record["id"]) | busy_elsewhere
            ports = choose_ports(ports, taken, self.system.port_free, self.system.free_port)
            record = self.store.patch_instance(record["id"], ports=ports)
            self.log(record["id"], f"Using ports {ports['config_web']} (Config Web) and {ports['agent_web']} (Agent Web UI)")
        return record

    def agent_command(self, record: dict[str, Any], endpoint: str, device_type: str) -> list[str]:
        instance_id = record["id"]
        ports = record["ports"]
        settings = self.store.settings()
        route_id = f"launcher-{instance_id}"
        env: dict[str, str] = {
            "AICO_ENVIRONMENT_BRIDGE_ENDPOINT": endpoint,
            "AICO_BENCHMARK_TASK_ID": route_id,
            "AICO_BRIDGE_EPISODE_ID": route_id,
            "AICO_PUBLIC_AGENT_WEB_PORT": str(ports["agent_web"]),
            "AICO_CONFIG_WEB_PORT": str(ports["config_web"]),
            "AICO_ENABLE_TERMINAL": "1" if record.get("enable_terminal") else "0",
        }
        if device_type:
            env["AICO_DEVICE_TYPE"] = device_type
        if settings.get("pass_host_proxy"):
            env.update(host_proxy_args(self.system.environ()))
        no_proxy = no_proxy_value(env.get("NO_PROXY") or env.get("no_proxy") or "")
        env["NO_PROXY"] = no_proxy
        env["no_proxy"] = no_proxy
        command = [
            "docker", "run", "-d", "--init",
            "--name", agent_container(instance_id),
            "--label", f"{LABEL_INSTANCE}={instance_id}",
            "--label", f"{LABEL_ROLE}=agent",
            "-p", f"127.0.0.1:{ports['config_web']}:80",
            "-p", f"127.0.0.1:{ports['agent_web']}:8080",
            "-v", f"{data_volume(instance_id)}:/userdata",
            "--add-host", "host.docker.internal:host-gateway",
        ]
        for key, value in env.items():
            command.extend(["-e", f"{key}={value}"])
        command.append(record["image"])
        return command

    def _run_agent(self, record: dict[str, Any], endpoint: str, device_type: str, ctx: BridgeContext) -> None:
        instance_id = record["id"]
        self._step(instance_id, "Starting the Agent container")
        command = self.agent_command(record, endpoint, device_type)
        ctx.remember(agent_container=agent_container(instance_id))
        self.log(instance_id, "$ " + " ".join(command))
        result = self.system.run(command, timeout=120)
        if result.returncode != 0:
            detail = (result.stderr or result.stdout or "").strip()
            raise RuntimeError(f"docker run failed: {detail}")

    def _wait_for_agent(self, record: dict[str, Any], cancel: threading.Event) -> None:
        instance_id = record["id"]
        ports = record["ports"]
        container = agent_container(instance_id)
        self._step(instance_id, "Waiting for Config Web and the Agent Web UI")
        targets = {
            "Config Web": f"http://127.0.0.1:{ports['config_web']}/",
            "Agent Web UI": f"http://127.0.0.1:{ports['agent_web']}/api/tools",
        }
        deadline = self.system.monotonic() + self.ready_timeout_sec
        pending = dict(targets)
        while True:
            if cancel.is_set():
                raise Cancelled()
            for label, url in list(pending.items()):
                status = self.system.http_status(url, timeout=2.0)
                if 200 <= status < 400 and (label == "Config Web" or status < 300):
                    pending.pop(label)
            if not pending:
                return
            state = self.docker.container_state(container)
            if state and state != "running":
                raise RuntimeError(self._agent_failure(container, f"The Agent container {state}."))
            if self.system.monotonic() >= deadline:
                waiting = " and ".join(pending)
                raise RuntimeError(
                    self._agent_failure(container, f"{waiting} did not answer within {self.ready_timeout_sec} s.")
                )
            self.system.sleep(self.poll_interval)

    def _agent_failure(self, container: str, message: str) -> str:
        logs = tail_lines(self.docker.logs(container, ERROR_LOG_LINES), ERROR_LOG_LINES).strip()
        return f"{message}\n\nLast container output:\n{logs}" if logs else message

    # -- stop ---------------------------------------------------------------

    def stop(self, instance_id: str) -> dict[str, Any]:
        with self._lock:
            record = self._record(instance_id)
            status = record.get("status")
            if status == "starting":
                cancel = self._cancel.get(record["id"])
                if cancel is not None:
                    cancel.set()
                    record = self.store.patch_instance(record["id"], step="Cancelling the start")
                    return self.payload(record)
            if status == "stopping":
                return self.payload(record)
            if status not in {"running", "starting", "error"}:
                raise Conflict("The instance is not running.")
            record = self.store.patch_instance(record["id"], status="stopping", step="Stopping")
            thread = threading.Thread(
                target=self._run_stop, args=(record["id"],), daemon=True,
                name=f"launcher-stop-{record['id']}",
            )
            self._workers[record["id"]] = thread
            thread.start()
        return self.payload(record)

    def _run_stop(self, instance_id: str) -> None:
        try:
            record = self.store.instance(instance_id) or {"id": instance_id}
            self._teardown(record)
            self.store.patch_instance(
                instance_id, status="stopped", step="", error="",
                runtime=None,
            )
            self.log(instance_id, "Stopped")
        except Exception as exc:  # noqa: BLE001
            self.log(instance_id, f"ERROR while stopping: {exc}")
            self.store.patch_instance(instance_id, status="error", step="", error=f"Stopping failed: {exc}")
        finally:
            with self._lock:
                self._workers.pop(instance_id, None)

    def _teardown(self, record: dict[str, Any], *, quiet: bool = False) -> None:
        """Stop the Agent first (its entrypoint releases the bridge route), then the bridge."""
        instance_id = record["id"]
        container = agent_container(instance_id)

        def attempt(action: Callable[[], None]) -> None:
            # Quiet teardowns clean up after another outcome (a failed or
            # cancelled start, a reconcile); their own failures are only logged
            # so they do not mask that outcome.
            try:
                action()
            except DockerError as exc:
                if not quiet:
                    raise
                self.log(instance_id, f"Cleanup: {exc}")

        def stop_agent() -> None:
            self.docker.stop(container, AGENT_STOP_TIMEOUT_SEC)
            self.docker.remove(container)

        if self.docker.container_state(container):
            if not quiet:
                self._step(instance_id, "Stopping the Agent container")
            attempt(stop_agent)
        bridge = get_bridge((record.get("bridge") or {}).get("type"))
        if not quiet and bridge.type != "none":
            self._step(instance_id, f"Stopping the {bridge.label} bridge")
        attempt(lambda: bridge.stop(self._context(record)))
        if not quiet and self.docker.container_state(container):
            raise DockerError(f"The Agent container {container} is still present after stopping it.")

    # -- health -------------------------------------------------------------

    def check_running(self, *, force: bool = False) -> None:
        """Mark running instances whose container or bridge died as error."""
        now = self.system.monotonic()
        if not force and now - self._last_health_check < self.health_check_interval:
            return
        self._last_health_check = now
        running = [r for r in self.store.instances() if r.get("status") == "running"]
        if not running:
            return
        try:
            containers = self.docker.containers(LABEL_INSTANCE)
        except DockerError:
            return
        for record in running:
            problem = self._problem(record, containers)
            if problem:
                with self._lock:
                    current = self.store.instance(record["id"]) or {}
                    if current.get("status") != "running":
                        continue
                    self.log(record["id"], f"ERROR: {problem}")
                    self.store.patch_instance(record["id"], status="error", error=problem)

    def _problem(self, record: dict[str, Any], containers: dict[str, dict[str, str]]) -> str:
        container = agent_container(record["id"])
        state = (containers.get(container) or {}).get("state", "")
        if state != "running":
            return (
                "The Agent container is gone." if not state
                else f"The Agent container is {state}. Open the agent log for details, then start the instance again."
            )
        bridge = get_bridge((record.get("bridge") or {}).get("type"))
        return bridge.problem(record.get("runtime") or {}, containers, self.system)

    def reconcile(self) -> None:
        """Bring stored statuses in line with what is actually running."""
        info = self.docker.info()
        try:
            containers = self.docker.containers(LABEL_INSTANCE) if info["available"] else None
        except DockerError:
            containers = None
        for record in self.store.instances():
            status = record.get("status") or "stopped"
            instance_id = record["id"]
            if containers is None:
                if status in ACTIVE_STATUSES:
                    self.store.patch_instance(
                        instance_id, status="error", step="",
                        error="Docker was not reachable when the launcher started. Start Docker, then start the instance again.",
                    )
                continue
            if status == "running":
                problem = self._problem(record, containers)
                if problem:
                    self.log(instance_id, f"Reconcile: {problem}")
                    agent_state = (containers.get(agent_container(instance_id)) or {}).get("state", "")
                    bridge = get_bridge((record.get("bridge") or {}).get("type"))
                    leftovers = agent_state or bridge.has_leftovers(record.get("runtime") or {}, containers, self.system)
                    if leftovers:
                        self.store.patch_instance(instance_id, status="error", step="", error=problem)
                    else:
                        self.store.patch_instance(
                            instance_id, status="stopped", step="", error="",
                            runtime=None,
                        )
                else:
                    self.log(instance_id, "Reconcile: still running")
            elif status in {"starting", "stopping"}:
                # The launcher exited mid-operation; finish by cleaning up.
                self.log(instance_id, f"Reconcile: the launcher exited while {status}; cleaning up")
                self._teardown(record, quiet=True)
                if status == "stopping":
                    self.store.patch_instance(
                        instance_id, status="stopped", step="", error="",
                        runtime=None,
                    )
                else:
                    self.store.patch_instance(
                        instance_id, status="error", step="",
                        error="The launcher exited while this instance was starting. Start it again.",
                        runtime=None,
                    )

    # -- logs ---------------------------------------------------------------

    def logs(self, instance_id: str, source: str, tail: int) -> dict[str, Any]:
        record = self._record(instance_id)
        tail = max(1, min(int(tail), MAX_LOG_TAIL_LINES))
        max_bytes = tail * 400
        if source == "launcher":
            text = tail_lines(tail_text(self.launcher_log(record["id"]), max_bytes), tail)
        elif source == "bridge":
            text = tail_text(self.bridge_log(record["id"]), max_bytes)
            container = (record.get("runtime") or {}).get("bridge_container")
            if container and self.docker.container_state(container):
                text += f"\n--- docker logs {container} ---\n" + self.docker.logs(container, tail)
            text = tail_lines(text, tail)
            if not text.strip():
                text = "No bridge output yet." if (record.get("bridge") or {}).get("type") != "none" else "This instance has no bridge."
        elif source == "agent":
            container = agent_container(record["id"])
            state = self.docker.container_state(container)
            if not state:
                text = "The Agent container is not running. Start the instance to see its output."
            else:
                text = tail_lines(self.docker.logs(container, tail), tail)
                if state == "running":
                    agent_log = self.system.run(
                        ["docker", "exec", container, "tail", "-n", str(tail), "/userdata/agent/log/agent.log"],
                        timeout=15,
                    )
                    if agent_log.returncode == 0 and agent_log.stdout.strip():
                        text += "\n--- /userdata/agent/log/agent.log ---\n" + agent_log.stdout
        else:
            raise validation.ValidationError("source must be agent, bridge, or launcher.")
        return {"source": source, "text": text}

