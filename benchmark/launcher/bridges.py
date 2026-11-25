"""Environment bridge types: field specs, probes, actions, and per-type start/stop.

A bridge runs on the host (or in its own container) and the Agent container
reaches it through host.docker.internal. Each type normalizes its user config,
can probe the host for prerequisites, runs setup actions, and starts/stops its
bridge for one instance. Runtime facts that must survive a launcher restart
(pids, container names, ports) are persisted through ``BridgeContext.remember``.
The shared building blocks live in :mod:`launcher.bridgebase`.
"""

from __future__ import annotations

import json
import os
import stat
from dataclasses import dataclass
from pathlib import Path
from typing import Any, Callable

from adbandroid.bridge.adb import DEFAULT_UNICODE_INPUT_METHOD, SUPPORTED_UNICODE_INPUT_METHODS
from runner.environment import (
    DEFAULT_MOBILEGYM_IMAGE,
    MOBILEGYM_COMMIT_LABEL,
    build_mobilegym_environment_command,
)

from . import BENCHMARK_ROOT, LABEL_INSTANCE, LABEL_ROLE, pyenv, validation
from .bridgebase import (  # noqa: F401 - re-exported for callers of launcher.bridges
    BRIDGE_READY_TIMEOUT_SEC,
    BridgeContext,
    BridgeStart,
    BridgeType,
    Cancelled,
    HostProcessBridge,
    action_result,
    action_spec,
    check,
    config_dict,
    dependency_check,
    device_type_from_health,
    field_spec,
    install_action,
    is_loopback,
    output_tail,
    probe_result,
    route_id,
)
from .docker import Docker
from .net import container_side_url, host_proxy_args, host_side_url
from .system import System

ADB_READY_TIMEOUT_SEC = 180
MOBILEGYM_WEB_PORT = 4173
MOBILEGYM_BRIDGE_PORT = 9090
MAX_MOBILEGYM_ENVS = 8
PROBE_TIMEOUT_SEC = 300
LOOPBACK = "127.0.0.1"
HOST_DOCKER_INTERNAL = "host.docker.internal"
# Host desktop and iPhone Mirroring both drive this computer's real pointer and
# keyboard, so they share one device claim and exclude each other.
HOST_INPUT_CLAIM = "host-input"

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
        config = config_dict(config)
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
        return probe_result(checks, device_type=device_type, ok=bool(device_type))

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


class ADBAndroidBridge(HostProcessBridge):
    type = "adb_android"
    label = "Android (ADB)"
    description = "Control an Android phone, emulator, or VM through adb on this computer."
    module = "adbandroid.scripts.start_bridge"
    process_label = "ADB bridge"
    ready_timeout_sec = ADB_READY_TIMEOUT_SEC

    def default_adb(self, host: "HostFacts") -> str:
        # Prefer the plain name when adb is on PATH; otherwise the SDK location found.
        if not host.adb_path or host.system.which("adb"):
            return "adb"
        return host.adb_path

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
                "unicode_input_method",
                "Unicode input method",
                "select",
                required=True,
                default=DEFAULT_UNICODE_INPUT_METHOD,
                options=[
                    {"value": "adbkeyboard", "label": "ADBKeyBoard (default)"},
                    {"value": "appium", "label": "Appium UnicodeIME"},
                ],
                help="Install the selected input method.",
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

    def notes(self, host: "HostFacts") -> list[str]:
        notes = ["Turn on USB debugging (or wireless debugging) and accept the authorization prompt on the device."]
        if not host.adb_path:
            notes.append(
                "adb was not found on PATH or in the Android SDK. Install Android SDK Platform Tools, "
                "or enter the full adb path."
            )
        return notes

    def normalize(self, config: Any) -> dict[str, Any]:
        config = config_dict(config)
        input_method = config.get("unicode_input_method", DEFAULT_UNICODE_INPUT_METHOD)
        if input_method not in SUPPORTED_UNICODE_INPUT_METHODS:
            raise validation.ValidationError("Unicode input method must be adbkeyboard or appium.")
        return {
            "serial": str(config.get("serial") or "").strip(),
            "unicode_input_method": input_method,
            "adb_path": os.path.expanduser(
                validation.executable(config.get("adb_path"), field="adb path", default="adb")
            ),
        }

    def require_complete(self, config: dict[str, Any], host: "HostFacts") -> None:
        super().require_complete(config, host)
        validation.adb_serial(config.get("serial"))

    def device_claim(self, config: dict[str, Any]) -> tuple[str, str]:
        serial = str(config.get("serial") or "")
        return (f"adb:{serial}", f"ADB device {serial}") if serial else ("", "")

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
            hint = "" if host.adb_path else " Install Android SDK Platform Tools or enter the full adb path."
            return probe_result(
                [check("adb", False, f"`{adb_path} devices` failed: {error}.{hint}")],
                options={"serial": []},
            )
        checks = [check("adb", True, f"`{adb_path} devices` works")]
        options = [
            {"value": device.serial, "label": f"{device.serial} ({'authorized' if device.state == 'device' else device.state})"}
            for device in devices
        ]
        if not devices:
            checks.append(check("Devices", False, "No device is connected. Start an emulator or connect a phone with USB debugging on."))
        in_use = host.devices_in_use()
        held = {device.serial for device in devices if self.device_claim({"serial": device.serial})[0] in in_use}
        for device in devices:
            if device.state != "device":
                checks.append(check(device.serial, False, ADB_STATE_HELP.get(device.state, device.state)))
            elif device.serial in held:
                owner = in_use[self.device_claim({"serial": device.serial})[0]]
                checks.append(check(device.serial, False, f"authorized, but already used by instance {owner}"))
            else:
                checks.append(check(device.serial, True, "authorized"))
        selected = config.get("serial") or ""
        authorized = {d.serial for d in devices if d.state == "device"} - held
        if selected:
            ok = selected in authorized
            if selected not in {d.serial for d in devices}:
                checks.append(check(selected, False, "not listed by `adb devices`"))
        else:
            ok = bool(authorized)
        return probe_result(checks, options={"serial": options}, device_type="Android", ok=ok)

    @staticmethod
    def _check_adb(system: System, adb_path: str) -> None:
        path = os.path.expanduser(adb_path)
        if os.sep in path or (os.altsep and os.altsep in path):
            found = os.path.isfile(path) and os.access(path, os.X_OK)
        else:
            found = bool(system.which(path))
        if not found:
            raise RuntimeError(
                f"adb was not found at {adb_path}. Install Android SDK Platform Tools or correct the adb path."
            )

    def start(self, ctx: BridgeContext) -> BridgeStart:
        serial = validation.adb_serial(ctx.config.get("serial"))
        adb_path = os.path.expanduser(ctx.config.get("adb_path") or "adb")
        self._check_adb(ctx.system, adb_path)
        ctx.step(f"Checking ADB device {serial}")
        state = ctx.system.run([adb_path, "-s", serial, "get-state"], timeout=15)
        device_state = state.stdout.strip()
        if state.returncode != 0 or device_state != "device":
            detail = device_state or (state.stderr or "").strip() or "not connected"
            raise RuntimeError(f"ADB device {serial} is not ready: {ADB_STATE_HELP.get(detail, detail)}.")
        # Reserve the serial atomically, like the other device bridges.
        self.claim_free(ctx, *self.device_claim({"serial": serial}))
        bind_host = ctx.system.bridge_bind_host()
        port = ctx.system.free_port()
        argv = self.python_command(
            self.launcher_python(), self.module,
            "--adb-serial", serial, "--adb-path", adb_path,
            "--bridge-host", bind_host, "--bridge-port", port,
            "--unicode-input-method", ctx.config.get("unicode_input_method", DEFAULT_UNICODE_INPUT_METHOD),
        )
        self.launch(
            ctx,
            argv,
            bind_host=bind_host,
            port=port,
            wait_step="Preparing the selected input method and starting the ADB bridge",
            adb_serial=serial,
        )
        return BridgeStart(endpoint=f"http://{HOST_DOCKER_INTERNAL}:{port}", device_type="Android")


# -- host desktop -------------------------------------------------------------------

DESKTOP_ENV = pyenv.PythonEnv("desktop", ("pillow>=10.2.0", "pyautogui>=0.9.54"))
DESKTOP_CAPTURE_CODE = (
    "import json, sys\n"
    "from desktop.bridge.device import DesktopDevice\n"
    "device = DesktopDevice(screenshot_command=sys.argv[1])\n"
    "data, width, height = device.screenshot_jpeg(60)\n"
    "print(json.dumps({'width': width, 'height': height, 'bytes': len(data),"
    " 'backend': device.backend, 'platform': device.platform}))\n"
)


class DesktopBridge(HostProcessBridge):
    type = "desktop"
    label = "Host desktop"
    description = "Let the Agent operate this computer's desktop: screen, mouse, and keyboard."
    module = "desktop.scripts.start_bridge"
    process_label = "desktop bridge"
    renews_route = True

    def availability(self, host: "HostFacts") -> tuple[bool, str]:
        if host.platform == "win32":
            return False, "The launcher does not support desktop control on Windows yet"
        return True, ""

    def fields(self, host: "HostFacts") -> list[dict[str, Any]]:
        return [
            field_spec(
                "confirm_control",
                "I understand the Agent will move the mouse and type on this computer",
                "checkbox",
                required=True,
                default=False,
                help="While the instance runs, the Agent uses the real pointer and keyboard.",
            ),
            field_spec(
                "screenshot_command",
                "Screenshot command (advanced)",
                "text",
                default="",
                placeholder="leave empty to use pyautogui",
                help=(
                    "Optional command prefix that writes a screenshot; the output path is appended, "
                    "e.g. `gnome-screenshot -f` or `scrot -o`. Leave empty unless the default capture fails."
                ),
            ),
        ]

    def notes(self, host: "HostFacts") -> list[str]:
        notes = []
        if host.platform == "darwin":
            notes += [
                "macOS asks for Screen Recording and Accessibility permissions. Grant them to the app "
                "that runs the launcher (Terminal, iTerm, VS Code, ...), then restart the launcher.",
                "Test screen capture checks Screen Recording without sending input; on the first run "
                "it may show the macOS permission prompt.",
            ]
        elif host.platform.startswith("linux"):
            notes.append(
                "Run the launcher inside a graphical session (X11, or Wayland with XWayland) so the "
                "bridge can capture the screen and send input."
            )
        notes.append("The Agent controls the real pointer and keyboard while the instance runs; avoid using them meanwhile.")
        return notes

    def actions(self) -> list[dict[str, str]]:
        return [
            action_spec("install_dependencies", "Install dependencies",
                        "Create the launcher's desktop Python environment with Pillow and pyautogui."),
            action_spec("test_screenshot", "Test screen capture",
                        "Capture one screenshot through the bridge code without sending input."),
        ]

    def normalize(self, config: Any) -> dict[str, Any]:
        config = config_dict(config)
        return {
            "confirm_control": validation.boolean(config.get("confirm_control"), field="Confirmation"),
            "screenshot_command": validation.command_line(
                config.get("screenshot_command"), field="Screenshot command"
            ),
        }

    def device_claim(self, config: dict[str, Any]) -> tuple[str, str]:
        return HOST_INPUT_CLAIM, "This computer's pointer and keyboard"

    def probe(self, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        checks = [dependency_check(DESKTOP_ENV, host)]
        if host.platform.startswith("linux"):
            environ = host.system.environ()
            session = environ.get("WAYLAND_DISPLAY") or environ.get("DISPLAY") or ""
            checks.append(check(
                "Graphical session", bool(session),
                f"display {session}" if session else "neither DISPLAY nor WAYLAND_DISPLAY is set; start the launcher from a desktop session",
            ))
        try:
            bind_host = host.system.bridge_bind_host()
            checks.append(check("Bind address", True, f"the bridge listens on {bind_host}"))
        except Exception as exc:  # noqa: BLE001
            checks.append(check("Bind address", False, f"could not find the address containers reach this computer at: {exc}"))
        owner = host.devices_in_use().get(HOST_INPUT_CLAIM)
        if owner:
            checks.append(check("Pointer and keyboard", False, f"already used by instance {owner}"))
        device_type = {"darwin": "macOS"}.get(host.platform, "linux")
        return probe_result(checks, device_type=device_type)

    def run_action(self, name: str, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        if name == "install_dependencies":
            return install_action(DESKTOP_ENV, host)
        if name != "test_screenshot":
            raise LookupError(name)
        prepared = install_action(DESKTOP_ENV, host)
        if not prepared["ok"]:
            return prepared
        python = str(DESKTOP_ENV.python(host.state_dir))
        result = host.system.run(
            [python, "-c", DESKTOP_CAPTURE_CODE, config.get("screenshot_command") or ""],
            timeout=60, cwd=BENCHMARK_ROOT,
        )
        output = "\n".join(part for part in (prepared["output"], result.stdout, result.stderr) if part.strip())
        try:
            info = json.loads(result.stdout.strip().splitlines()[-1]) if result.returncode == 0 else None
        except (ValueError, IndexError):
            info = None
        if not isinstance(info, dict):
            hint = (
                " Grant Screen Recording to the app that runs the launcher, then restart the launcher."
                if host.platform == "darwin" else ""
            )
            detail = output_tail(result).split("Error: ", 1)[-1]
            return action_result(False, f"Screen capture failed.{hint}",
                                 [check("Screen capture", False, detail)], output)
        detail = f"{info.get('width')}x{info.get('height')} with {info.get('backend')}"
        message = f"Captured the screen ({detail})."
        if host.platform == "darwin":
            message += (" If the image shows only the desktop background, Screen Recording is not granted yet:"
                        " grant it to the app that runs the launcher and restart the launcher.")
        return action_result(True, message, [check("Screen capture", True, detail)], output)

    def start(self, ctx: BridgeContext) -> BridgeStart:
        self.claim_free(ctx, *self.device_claim(ctx.config))
        python = ctx.ensure_pyenv(DESKTOP_ENV)
        bind_host = ctx.system.bridge_bind_host()
        port = ctx.system.free_port()
        args: list[Any] = ["--bridge-host", bind_host, "--bridge-port", port]
        if not is_loopback(bind_host):
            # Linux: containers reach the host at the Docker bridge gateway.
            args.append("--allow-private-bind")
        if ctx.config.get("screenshot_command"):
            args += ["--screenshot-command", ctx.config["screenshot_command"]]
        health = self.launch(ctx, self.python_command(python, self.module, *args), bind_host=bind_host, port=port,
                             device_claim=HOST_INPUT_CLAIM)
        device_type = device_type_from_health(health)
        if not device_type:
            raise RuntimeError("The desktop bridge does not report a supported platform.")
        return BridgeStart(endpoint=f"http://{HOST_DOCKER_INTERNAL}:{port}", device_type=device_type)


# -- iPhone Mirroring ------------------------------------------------------------------

MIRRORING_MODULE = "iphone_mirroring"
DEFAULT_CONTENT_INSETS = "8,38,8,8"
MIRRORING_READY_TIMEOUT_SEC = 300


def _last_json(text: str) -> dict[str, Any] | None:
    """The JSON object a helper printed last (possibly pretty-printed), or None.

    Earlier output, such as compiler messages, is skipped.
    """
    text = (text or "").strip()
    decoder = json.JSONDecoder()
    start = text.find("{")
    while start != -1:
        try:
            value, end = decoder.raw_decode(text, start)
        except ValueError:
            start = text.find("{", start + 1)
            continue
        if isinstance(value, dict) and not text[end:].strip():
            return value
        start = text.find("{", start + 1)
    return None


def mirroring_helper_stale(repo_root: Path) -> bool:
    """True when the bridge will (re)compile its Swift helper: missing or older than the source."""
    helper = repo_root / ".cache" / "iphone-mirroring" / "mirroring-native"
    source = BENCHMARK_ROOT / "iphone_mirroring" / "native" / "Mirroring.swift"
    try:
        return not helper.exists() or helper.stat().st_mtime < source.stat().st_mtime
    except OSError:
        return True


class IPhoneMirroringBridge(HostProcessBridge):
    type = "iphone_mirroring"
    label = "iPhone Mirroring"
    description = "Control a real iPhone through macOS iPhone Mirroring."
    module = MIRRORING_MODULE
    process_label = "iPhone Mirroring bridge"
    quick_actions_preset = "iphone_mirroring"
    ready_timeout_sec = MIRRORING_READY_TIMEOUT_SEC
    renews_route = True

    def availability(self, host: "HostFacts") -> tuple[bool, str]:
        if host.platform != "darwin":
            return False, "Requires macOS iPhone Mirroring"
        if not host.swiftc:
            return False, "Install Xcode Command Line Tools (xcode-select --install)"
        return True, ""

    def fields(self, host: "HostFacts") -> list[dict[str, Any]]:
        return [
            field_spec(
                "content_insets",
                "Content insets",
                "text",
                default=DEFAULT_CONTENT_INSETS,
                placeholder=DEFAULT_CONTENT_INSETS,
                help=(
                    "left,top,right,bottom in macOS points between the mirror window edge and the phone "
                    "screen. Keep the default unless screenshots include window chrome."
                ),
            )
        ]

    def notes(self, host: "HostFacts") -> list[str]:
        return [
            "Open iPhone Mirroring and connect the phone before Check or Start; keep the iPhone locked.",
            "Screen Recording and Accessibility belong to the app that runs the launcher (Terminal, iTerm, "
            "VS Code, ...). Use Request permissions, approve them in System Settings, then restart the launcher.",
            "Input briefly takes over the Mac's pointer and keyboard and focuses the mirror window, "
            "so a Host desktop instance cannot run at the same time.",
            "The first Check or Start compiles a small Swift helper, which can take a minute.",
        ]

    def actions(self) -> list[dict[str, str]]:
        return [
            action_spec("request_permissions", "Request permissions",
                        "Ask macOS for Accessibility and Screen Recording for the app that runs the launcher."),
        ]

    def normalize(self, config: Any) -> dict[str, Any]:
        config = config_dict(config)
        return {"content_insets": validation.content_insets(config.get("content_insets"), default=DEFAULT_CONTENT_INSETS)}

    def device_claim(self, config: dict[str, Any]) -> tuple[str, str]:
        return HOST_INPUT_CLAIM, "This computer's pointer and keyboard (used by iPhone Mirroring)"

    def _run(self, host: "HostFacts", *args: str) -> Any:
        return host.system.run(
            [self.launcher_python(), "-m", self.module, *args], timeout=PROBE_TIMEOUT_SEC, cwd=BENCHMARK_ROOT
        )

    def probe(self, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        result = self._run(host, "--probe", "--content-insets", config["content_insets"])
        info = _last_json(result.stdout) if result.returncode == 0 else None
        if info is None:
            return probe_result([check("iPhone Mirroring helper", False, output_tail(result, 3))], device_type="iOS")
        checks = [
            check("Mirror window", bool(info.get("window_visible")),
                  "found" if info.get("window_visible") else
                  info.get("window_error") or "no visible window; open iPhone Mirroring and connect the phone"),
            check("Screen capture permission", bool(info.get("screen_recording")),
                  "granted" if info.get("screen_recording") else "missing; use Request permissions, then restart the launcher"),
            check("Input permission", bool(info.get("input_available")),
                  "granted" if info.get("input_available") else "missing (Accessibility); use Request permissions, then restart the launcher"),
        ]
        owner = host.devices_in_use().get(HOST_INPUT_CLAIM)
        if owner:
            checks.append(check("Pointer and keyboard", False, f"already used by instance {owner}"))
        return probe_result(checks, device_type="iOS")

    def run_action(self, name: str, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        if name != "request_permissions":
            raise LookupError(name)
        result = self._run(host, "--request-input-access", "--request-screen-access")
        output = (result.stdout + result.stderr).strip()
        info = _last_json(result.stdout) if result.returncode == 0 else None
        if info is None:
            return action_result(False, f"Requesting permissions failed: {output_tail(result)}", output=output)
        checks = [
            check("Input permission (Accessibility)", bool(info.get("accessibility")),
                  "granted" if info.get("accessibility") else "not granted yet; approve it in System Settings > Privacy & Security"),
            check("Screen Recording", bool(info.get("screen_recording")),
                  "granted" if info.get("screen_recording") else "not granted yet; approve it in System Settings > Privacy & Security"),
        ]
        ok = all(item["ok"] for item in checks)
        message = ("Both permissions are granted." if ok else
                   "Approve the app that runs the launcher in System Settings, then restart the launcher.")
        return action_result(ok, message, checks, output)

    def start(self, ctx: BridgeContext) -> BridgeStart:
        self.claim_free(ctx, *self.device_claim(ctx.config))
        port = ctx.system.free_port()
        argv = self.python_command(
            self.launcher_python(), self.module,
            "--port", port, "--task-id", ctx.route_id, "--content-insets", ctx.config["content_insets"],
        )
        # The bridge compiles its Swift helper before it answers /health.
        wait_step = (
            "Building the iPhone Mirroring helper and starting the bridge (first start only)"
            if mirroring_helper_stale(ctx.repo_root) else ""
        )
        # The bridge binds 127.0.0.1, which Docker Desktop on macOS reaches.
        self.launch(ctx, argv, bind_host=LOOPBACK, port=port, device_claim=HOST_INPUT_CLAIM, wait_step=wait_step)
        return BridgeStart(endpoint=f"http://{HOST_DOCKER_INTERNAL}:{port}", device_type="iOS")


# -- iOS device over USB -------------------------------------------------------------------

IOS_REQUIREMENTS = BENCHMARK_ROOT / "ios_device" / "requirements.txt"
IOS_ENV = pyenv.PythonEnv("ios_device", pyenv.requirements_file(IOS_REQUIREMENTS))
DEFAULT_XCTRUNNER = "io.aiden.aico.WebDriverAgentRunner.xctrunner"
IOS_READY_TIMEOUT_SEC = 180
IOS_STOP_TIMEOUT_SEC = 15.0


def parse_usbmux_devices(text: str) -> list[dict[str, str]]:
    """USB devices from ``pymobiledevice3 usbmux list`` JSON: [{udid, name, version}]."""
    try:
        payload = json.loads(text)
    except ValueError:
        return []
    devices = []
    for item in payload if isinstance(payload, list) else []:
        if not isinstance(item, dict):
            continue
        udid = str(item.get("UniqueDeviceID") or item.get("Identifier") or item.get("SerialNumber") or "")
        connection = str(item.get("ConnectionType") or "USB")
        if not udid or connection.lower() not in {"usb"}:
            continue
        devices.append({
            "udid": udid,
            "name": str(item.get("DeviceName") or ""),
            "version": str(item.get("ProductVersion") or ""),
        })
    return devices


class IOSDeviceBridge(HostProcessBridge):
    type = "ios_device"
    label = "iOS device"
    description = "Control a real iPhone over USB through WebDriverAgent."
    module = "ios_device"
    process_label = "iOS device bridge"
    quick_actions_preset = "ios_device"
    ready_timeout_sec = IOS_READY_TIMEOUT_SEC
    renews_route = True
    # The bridge needs up to ~12 s to cancel the WDA runner and close its tunnel.
    stop_timeout_sec = IOS_STOP_TIMEOUT_SEC

    def availability(self, host: "HostFacts") -> tuple[bool, str]:
        if host.platform != "darwin":
            return False, "Requires macOS with Xcode"
        return True, ""

    def fields(self, host: "HostFacts") -> list[dict[str, Any]]:
        return [
            field_spec(
                "udid",
                "Device",
                "select",
                default="",
                dynamic_options=True,
                help="Run the check to list USB iPhones. Optional when exactly one is connected.",
            ),
            field_spec(
                "xctrunner",
                "WebDriverAgent runner",
                "text",
                default=DEFAULT_XCTRUNNER,
                placeholder=DEFAULT_XCTRUNNER,
                help="Bundle ID of the signed WDA runner installed on the phone (ends in .xctrunner).",
            ),
            field_spec(
                "permission_policy",
                "Permission policy file",
                "text",
                default="",
                placeholder="optional, e.g. ~/aico/permission-policy.json",
                help="Optional rules for answering iOS permission prompts automatically. See the ios_device README.",
            ),
        ]

    def notes(self, host: "HostFacts") -> list[str]:
        return [
            "One-time setup is manual: sign and install WebDriverAgent with your Apple team "
            "(python -m ios_device.setup, see benchmark/ios_device/README.md, Prerequisites and Installation).",
            "Connect the iPhone by USB, enable Developer Mode, unlock it, and accept Trust This Computer.",
            "Only one WebDriverAgent controller can drive a device at a time; stop other WDA sessions first.",
            "Dependencies (pymobiledevice3) go into a launcher-managed Python 3.12 environment; Start installs them when missing.",
        ]

    def actions(self) -> list[dict[str, str]]:
        return [
            action_spec("install_dependencies", "Install dependencies",
                        "Create the launcher's iOS Python environment with pymobiledevice3."),
        ]

    def normalize(self, config: Any) -> dict[str, Any]:
        config = config_dict(config)
        return {
            "udid": validation.ios_udid(config.get("udid")),
            "xctrunner": validation.bundle_id(config.get("xctrunner"), field="WebDriverAgent runner", default=DEFAULT_XCTRUNNER),
            "permission_policy": validation.host_path(
                config.get("permission_policy"), field="Permission policy file", must_exist=True, kind="file"
            ),
        }

    def device_claim(self, config: dict[str, Any]) -> tuple[str, str]:
        udid = str(config.get("udid") or "")
        return (f"ios:{udid}", f"iOS device {udid}") if udid else ("", "")

    def _list(self, system: System, python: str) -> tuple[list[dict[str, str]] | None, str]:
        result = system.run([python, "-m", "pymobiledevice3", "usbmux", "list", "--usb"], timeout=30, cwd=BENCHMARK_ROOT)
        if result.returncode != 0:
            return None, output_tail(result)
        return parse_usbmux_devices(result.stdout), ""

    def probe(self, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        deps = dependency_check(IOS_ENV, host)
        if not pyenv.is_ready(host.system, IOS_ENV, host.state_dir):
            deps = check("Dependencies", False,
                         "not installed; click Install dependencies so Check can list devices"
                         if host.uv else pyenv.UV_MISSING)
            return probe_result([deps], options={"udid": []}, device_type="iOS")
        python = str(IOS_ENV.python(host.state_dir))
        checks = [deps]
        devices, error = self._list(host.system, python)
        if devices is None:
            checks.append(check("USB devices", False, f"usbmux list failed: {error}"))
            return probe_result(checks, options={"udid": []}, device_type="iOS")
        in_use = host.devices_in_use()
        options = []
        for device in devices:
            label = " ".join(part for part in (device["name"], f"iOS {device['version']}" if device["version"] else "") if part)
            options.append({"value": device["udid"], "label": f"{label} ({device['udid']})" if label else device["udid"]})
        udid = config.get("udid") or ""
        if not devices:
            checks.append(check("USB devices", False, "No iPhone is connected by USB. Connect it, unlock it, and trust this computer."))
            return probe_result(checks, options={"udid": options}, device_type="iOS")
        checks.append(check("USB devices", True, f"{len(devices)} connected"))
        if udid and udid not in {device["udid"] for device in devices}:
            checks.append(check(udid, False, "not connected by USB"))
            return probe_result(checks, options={"udid": options}, device_type="iOS")
        if not udid and len(devices) > 1:
            checks.append(check("Device", False, "Several iPhones are connected; select one."))
            return probe_result(checks, options={"udid": options}, device_type="iOS")
        target = udid or devices[0]["udid"]
        owner = in_use.get(f"ios:{target}")
        if owner:
            checks.append(check(target, False, f"already used by instance {owner}"))
            return probe_result(checks, options={"udid": options}, device_type="iOS")
        args = ["--probe", "--udid", target]
        if config.get("xctrunner"):
            args += ["--xctrunner", config["xctrunner"]]
        result = host.system.run([python, "-m", self.module, *args], timeout=150, cwd=BENCHMARK_ROOT)
        info = _last_json(result.stdout) if result.returncode == 0 else None
        if info is None:
            checks.append(check("WebDriverAgent", False, output_tail(result, 2)))
            return probe_result(checks, options={"udid": options}, device_type="iOS")
        checks += [
            check("Connected", bool(info.get("connected")), "WebDriverAgent answers" if info.get("connected") else "not connected"),
            check("Unlocked", not info.get("locked"), "unlocked" if not info.get("locked") else "the iPhone is locked; unlock it"),
            check("Input", bool(info.get("input_available")), "available" if info.get("input_available") else "unavailable while the iPhone is locked"),
        ]
        return probe_result(checks, options={"udid": options}, device_type="iOS")

    def run_action(self, name: str, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        if name != "install_dependencies":
            raise LookupError(name)
        return install_action(IOS_ENV, host)

    def start(self, ctx: BridgeContext) -> BridgeStart:
        python = ctx.ensure_pyenv(IOS_ENV)
        udid = ctx.config.get("udid") or ""
        if not udid:
            ctx.step("Finding the connected iPhone")
            devices, error = self._list(ctx.system, python)
            if devices is None:
                raise RuntimeError(f"Could not list USB iPhones: {error}")
            if len(devices) != 1:
                raise RuntimeError(
                    "Connect one iPhone by USB, or select the device in the instance settings."
                    if not devices else "Several iPhones are connected; select the device in the instance settings."
                )
            udid = devices[0]["udid"]
        key, description = self.device_claim({"udid": udid})
        self.claim_free(ctx, key, description)
        policy = ctx.config.get("permission_policy") or ""
        if policy and not os.path.isfile(policy):
            raise RuntimeError(f"The permission policy file does not exist: {policy}")
        port = ctx.system.free_port()
        args: list[Any] = ["--port", port, "--task-id", ctx.route_id, "--udid", udid]
        if ctx.config.get("xctrunner"):
            args += ["--xctrunner", ctx.config["xctrunner"]]
        if policy:
            args += ["--permission-policy", policy]
        self.launch(ctx, self.python_command(python, self.module, *args), bind_host=LOOPBACK, port=port,
                    device_claim=key, ios_udid=udid)
        return BridgeStart(endpoint=f"http://{HOST_DOCKER_INTERNAL}:{port}", device_type="iOS")


# -- vphone virtual iPhone ------------------------------------------------------------------

DHCP_LEASES = Path("/var/db/dhcpd_leases")
VPHONE_DEFAULT_ENV_FILE = BENCHMARK_ROOT / "vphone" / "vphone.env"
VPHONE_MODULE = "vphone.scripts.start_bridge"
DEFAULT_GUEST_SSH_PORT = 22222
DEFAULT_GUEST_SSH_USER = "root"


def parse_dhcp_leases(text: str, name: str = "iPhone") -> list[str]:
    """IPs leased to hosts called name, newest lease first (as start.sh detect_guest_ip)."""
    leases: list[tuple[int, str]] = []
    entry: dict[str, str] = {}
    for raw in text.splitlines():
        line = raw.strip()
        if line.startswith("{"):
            entry = {}
        elif line.startswith("}"):
            if entry.get("name") == name and entry.get("ip_address"):
                try:
                    stamp = int(entry.get("lease", "0"), 16)
                except ValueError:
                    stamp = 0
                leases.append((stamp, entry["ip_address"]))
            entry = {}
        elif "=" in line:
            key, _, value = line.partition("=")
            entry[key.strip()] = value.strip()
    leases.sort(key=lambda item: item[0], reverse=True)
    return [ip for _, ip in leases]


def detect_guest_ip(system: System, port: int, leases_path: Path = DHCP_LEASES) -> tuple[str, str]:
    """(ip, detail): the newest iPhone lease whose SSH port accepts a connection."""
    try:
        text = Path(leases_path).read_text(encoding="utf-8", errors="replace")
    except OSError:
        return "", f"{leases_path} is not readable; no VM has been leased an address yet"
    candidates = parse_dhcp_leases(text)
    if not candidates:
        return "", f"no iPhone lease in {leases_path}; is the vphone VM running?"
    for ip in candidates:
        if validation.host_name(ip, field="Guest IP") and system.tcp_reachable(ip, port, timeout=2.0):
            return ip, f"detected {ip} from {leases_path}"
    return "", f"none of the {len(candidates)} iPhone leases answers on port {port}"


def read_env_file(path: str) -> dict[str, str]:
    if not path:
        return {}
    from dotenv import dotenv_values

    return {key: str(value) for key, value in dotenv_values(path).items() if value is not None}


class VPhoneBridge(HostProcessBridge):
    type = "vphone"
    label = "Virtual iPhone"
    description = "Control a vphone-cli virtual iPhone running on this Mac."
    module = VPHONE_MODULE
    process_label = "vphone bridge"
    leases_path = DHCP_LEASES

    def availability(self, host: "HostFacts") -> tuple[bool, str]:
        if host.platform != "darwin":
            return False, "Requires macOS with a vphone-cli virtual machine"
        return True, ""

    def fields(self, host: "HostFacts") -> list[dict[str, Any]]:
        return [
            field_spec("env_file", "vphone.env file", "text", default="",
                       placeholder="optional, e.g. ~/hardware-demo/benchmark/vphone/vphone.env",
                       help="Optional. Its values fill the fields you leave empty."),
            field_spec("socket", "VM socket", "text", default="",
                       placeholder="/path/to/vphone-cli/vm/vphone.sock",
                       help="The VM's host-control socket, vm/vphone.sock in the vphone-cli checkout. "
                            "Required unless the env file sets VPHONE_SOCKET."),
            field_spec("use_guest_ssh", "Use guest SSH fallback", "checkbox", default=True,
                       help="Use SSH into the guest for actions the host-control socket cannot do."),
            field_spec("guest_ssh_host", "Guest SSH host", "text", default="",
                       placeholder="auto-detect",
                       help="Leave empty to detect the VM's address from /var/db/dhcpd_leases "
                            "(falls back to VPHONE_GUEST_SSH_HOST in the env file)."),
            field_spec("guest_ssh_port", "Guest SSH port", "number", default="",
                       placeholder=str(DEFAULT_GUEST_SSH_PORT),
                       help=f"Empty: VPHONE_GUEST_SSH_PORT from the env file, otherwise {DEFAULT_GUEST_SSH_PORT}."),
            field_spec("guest_ssh_user", "Guest SSH user", "text", default="",
                       placeholder=DEFAULT_GUEST_SSH_USER,
                       help=f"Empty: VPHONE_GUEST_SSH_USER from the env file, otherwise {DEFAULT_GUEST_SSH_USER}."),
            field_spec("guest_ssh_identity", "Guest SSH key", "text", default="",
                       placeholder="e.g. ~/.ssh/vphone_ecdsa",
                       help="Private key for the guest; leave empty to use the env file or your SSH defaults."),
        ]

    def notes(self, host: "HostFacts") -> list[str]:
        return [
            "Start the vphone VM first with vphone-cli; see docs/09-benchmark/vphone-cli-setup-guide-en.md.",
            "The guest address changes on every VM boot; leave Guest SSH host empty to detect it.",
        ]

    def normalize(self, config: Any) -> dict[str, Any]:
        config = config_dict(config)
        port_value = config.get("guest_ssh_port")
        use_ssh = config.get("use_guest_ssh")
        return {
            "env_file": validation.host_path(config.get("env_file"), field="vphone.env file", must_exist=True, kind="file"),
            "socket": validation.host_path(config.get("socket"), field="VM socket"),
            "use_guest_ssh": True if use_ssh is None else validation.boolean(use_ssh, field="Use guest SSH fallback"),
            "guest_ssh_host": validation.host_name(config.get("guest_ssh_host"), field="Guest SSH host"),
            # Blank port and user come from the env file, then the vphone defaults.
            "guest_ssh_port": validation.optional_port(port_value, field="Guest SSH port"),
            "guest_ssh_user": (
                validation.user_name(config.get("guest_ssh_user"), field="Guest SSH user", default="")
                if str(config.get("guest_ssh_user") or "").strip() else ""
            ),
            "guest_ssh_identity": validation.host_path(
                config.get("guest_ssh_identity"), field="Guest SSH key", must_exist=True, kind="file"
            ),
        }

    def require_complete(self, config: dict[str, Any], host: "HostFacts") -> None:
        super().require_complete(config, host)
        if not self.effective(config)["socket"]:
            raise validation.ValidationError("Enter the VM socket, or an env file that sets VPHONE_SOCKET.")

    def effective(self, config: dict[str, Any]) -> dict[str, Any]:
        """Config with blank fields filled from the env file (as start_bridge --env-file does)."""
        env_file = config.get("env_file") or (str(VPHONE_DEFAULT_ENV_FILE) if VPHONE_DEFAULT_ENV_FILE.is_file() else "")
        try:
            values = read_env_file(env_file)
        except OSError:
            values = {}
        socket_path = config.get("socket") or os.path.expanduser(values.get("VPHONE_SOCKET", ""))
        try:
            env_port = validation.optional_port(values.get("VPHONE_GUEST_SSH_PORT"), field="VPHONE_GUEST_SSH_PORT")
        except validation.ValidationError:
            env_port = 0
        identity = config.get("guest_ssh_identity") or os.path.expanduser(values.get("VPHONE_GUEST_SSH_IDENTITY", ""))
        return {
            "env_file": env_file,
            "socket": os.path.normpath(socket_path) if socket_path else "",
            "env_guest_ssh_host": values.get("VPHONE_GUEST_SSH_HOST", ""),
            "guest_ssh_port": int(config.get("guest_ssh_port") or env_port or DEFAULT_GUEST_SSH_PORT),
            "guest_ssh_user": config.get("guest_ssh_user") or values.get("VPHONE_GUEST_SSH_USER", "") or DEFAULT_GUEST_SSH_USER,
            "guest_ssh_identity": identity,
        }

    def device_claim(self, config: dict[str, Any]) -> tuple[str, str]:
        socket_path = self.effective(config)["socket"]
        return (f"vphone:{socket_path}", f"The vphone VM at {socket_path}") if socket_path else ("", "")

    def guest_host(self, config: dict[str, Any], system: System) -> tuple[str, str]:
        if config.get("guest_ssh_host"):
            return config["guest_ssh_host"], "set in the instance"
        effective = self.effective(config)
        ip, detail = detect_guest_ip(system, effective["guest_ssh_port"], self.leases_path)
        if ip:
            return ip, detail
        if effective["env_guest_ssh_host"]:
            return effective["env_guest_ssh_host"], f"{detail}; using VPHONE_GUEST_SSH_HOST from the env file"
        return "", detail

    def probe(self, config: dict[str, Any], host: "HostFacts") -> dict[str, Any]:
        effective = self.effective(config)
        checks = []
        socket_path = effective["socket"]
        if not socket_path:
            checks.append(check("VM socket", False, "enter the VM socket or an env file that sets VPHONE_SOCKET"))
        else:
            try:
                mode = os.stat(socket_path).st_mode
            except OSError:
                checks.append(check("VM socket", False, f"{socket_path} does not exist; start the vphone VM first"))
            else:
                is_socket = stat.S_ISSOCK(mode)
                checks.append(check("VM socket", is_socket, socket_path if is_socket else f"{socket_path} is not a Unix socket"))
        if config.get("use_guest_ssh", True):
            ip, detail = self.guest_host(config, host.system)
            checks.append(check("Guest address", bool(ip), f"{ip} ({detail})" if ip else detail))
            if ip:
                port = effective["guest_ssh_port"]
                reachable = host.system.tcp_reachable(ip, port, timeout=2.0)
                checks.append(check("Guest SSH", reachable, f"{ip}:{port} accepts connections" if reachable
                                    else f"{ip}:{port} does not answer; is the VM booted?"))
        key, _ = self.device_claim(config)
        owner = host.devices_in_use().get(key) if key else None
        if owner:
            checks.append(check("VM", False, f"already used by instance {owner}"))
        return probe_result(checks, device_type="iOS")

    def start(self, ctx: BridgeContext) -> BridgeStart:
        config = ctx.config
        effective = self.effective(config)
        key, description = self.device_claim(config)
        self.claim_free(ctx, key, description)
        port = ctx.system.free_port()
        # Pass every value the launcher resolved and checked explicitly: the
        # bridge would otherwise prefer $VPHONE_ENV_FILE and VPHONE_* variables
        # from the launcher's environment, and run against another VM.
        args: list[Any] = ["--host", LOOPBACK, "--port", port, "--benchmark-task-id", ctx.route_id, "--json",
                           "--socket", effective["socket"]]
        if effective["env_file"]:
            args += ["--env-file", effective["env_file"]]
        if config.get("use_guest_ssh", True):
            ctx.step("Finding the vphone guest address")
            ip, detail = self.guest_host(config, ctx.system)
            ctx.log(f"guest address: {ip or 'not found'} ({detail})")
            if not ip:
                raise RuntimeError(f"Could not find the vphone guest address: {detail}. Start the VM or set Guest SSH host.")
            args += ["--guest-ssh-host", ip, "--guest-ssh-port", effective["guest_ssh_port"],
                     "--guest-ssh-user", effective["guest_ssh_user"]]
            if effective["guest_ssh_identity"]:
                args += ["--guest-ssh-identity", effective["guest_ssh_identity"]]
        else:
            args.append("--no-guest-ssh-fallback")
        health = self.launch(ctx, self.python_command(self.launcher_python(), self.module, *args),
                             bind_host=LOOPBACK, port=port, device_claim=key)
        return BridgeStart(endpoint=f"http://{HOST_DOCKER_INTERNAL}:{port}",
                           device_type=device_type_from_health(health) or "iOS")


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
        config = config_dict(config)
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


BRIDGE_TYPES: dict[str, BridgeType] = {
    bridge.type: bridge
    for bridge in (
        NoBridge(),
        MobileGymBridge(),
        ADBAndroidBridge(),
        DesktopBridge(),
        IPhoneMirroringBridge(),
        IOSDeviceBridge(),
        VPhoneBridge(),
        CustomBridge(),
    )
}


def get_bridge(type_: str) -> BridgeType:
    bridge = BRIDGE_TYPES.get(str(type_ or ""))
    if bridge is None:
        raise validation.ValidationError(f"Unknown bridge type: {type_!r}.")
    return bridge


class HostFacts:
    """Host prerequisites the bridge specs and probes need, cached briefly.

    Building a spec only reads these cached facts; probes and actions do the
    expensive work (running helpers, installing dependencies) on request.
    """

    TTL_SEC = 10.0

    def __init__(
        self,
        system: System,
        docker: Docker,
        repo_root: Path,
        devices_in_use: Callable[[], dict[str, str]] = dict,
        *,
        state_dir: Path | None = None,
    ) -> None:
        self.system = system
        self.docker = docker
        self.repo_root = repo_root
        self.state_dir = Path(state_dir) if state_dir is not None else repo_root / ".cache" / "launcher"
        self.devices_in_use = devices_in_use
        self._expires = 0.0
        self._docker_info: dict[str, Any] = {}
        self._docker_info_expires = 0.0
        self._swiftc: str | None = None
        self.platform = system.platform()
        self.docker_cli = ""
        self.adb_path = ""
        self.uv = ""
        self.mobilegym_commit = ""
        self.mobilegym_error = ""

    @property
    def swiftc(self) -> str:
        """Path of swiftc from the Xcode tools, '' when missing; computed once per refresh(force)."""
        if self._swiftc is None:
            self._swiftc = ""
            # xcode-select -p never opens the "install the tools" dialog that a
            # bare xcrun shim can; only ask xcrun when the tools are present.
            if self.platform == "darwin" and self.system.run(["xcode-select", "-p"], timeout=10).returncode == 0:
                result = self.system.run(["xcrun", "--find", "swiftc"], timeout=20)
                if result.returncode == 0:
                    self._swiftc = result.stdout.strip()
        return self._swiftc

    def refresh(self, *, force: bool = False) -> "HostFacts":
        now = self.system.monotonic()
        if not force and now < self._expires:
            return self
        if force:
            self._swiftc = None
        self.platform = self.system.platform()
        self.docker_cli = self.system.which("docker")
        self.adb_path = self.system.which("adb") or _android_sdk_adb(self.system.environ())
        self.uv = self.system.which("uv")
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
