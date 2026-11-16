#!/usr/bin/env python3
"""Start a native Agent, Config Web, and a real-phone bridge in one terminal."""
from __future__ import annotations

import argparse
import contextlib
import fcntl
import json
import os
from pathlib import Path
import shutil
import signal
import socket
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.request
import webbrowser


ROOT = Path(__file__).resolve().parents[1]
MODES = {
    "android": ("Android", "local-android", 8080, 8000, 8899, 18083),
    "iphone-mirroring": ("iOS", "iphone-mirroring", 8081, 8001, 8897, 18084),
    "ios": ("iOS", "ios-device", 8082, 8002, 8896, 18085),
}


def arguments(argv=None):
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("mode", choices=MODES, help="ios uses USB + pymobiledevice3/WDA")
    parser.add_argument("--data-dir", type=Path, help="Existing Agent directory; default: .cache/native-phone-lab/MODE")
    parser.add_argument("--config", type=Path, help="Import a TOML file only if this target has no agent.toml")
    parser.add_argument("--adb-serial", help="Android device; automatically select the only authorized device")
    parser.add_argument("--udid", help="Select an iPhone when multiple USB devices are connected")
    parser.add_argument("--xctrunner", default="io.aiden.aico.WebDriverAgentRunner.xctrunner", help="Installed signed WDA runner (ios only)")
    parser.add_argument("--permission-policy", type=Path, help="Explicit permission rules for ios")
    parser.add_argument("--request-permissions", action="store_true", help="Request macOS Mirroring permissions and exit; approve them yourself")
    parser.add_argument("--agent-binary", type=Path, help="Use an existing native Agent instead of building")
    parser.add_argument("--python", type=Path, help="Prepared bridge Python; otherwise install isolated dependencies with uv")
    parser.add_argument("--agent-port", type=int)
    parser.add_argument("--config-port", type=int)
    parser.add_argument("--bridge-port", type=int)
    parser.add_argument("--share-port", type=int)
    parser.add_argument("--bridge-url", help="Reuse an existing loopback bridge; leave its process running on exit")
    parser.add_argument("--no-open", action="store_true", help="Print URLs without opening a browser")
    args = parser.parse_args(argv)
    if args.mode != "android" and args.adb_serial:
        parser.error("--adb-serial requires android")
    if args.mode != "ios" and (args.udid or args.permission_policy):
        parser.error("--udid and --permission-policy require ios")
    if args.request_permissions and (args.mode != "iphone-mirroring" or args.bridge_url):
        parser.error("--request-permissions requires iphone-mirroring without --bridge-url")
    if args.mode != "android" and sys.platform != "darwin":
        parser.error("iPhone modes require macOS")
    device, task, agent_port, config_port, bridge_port, share_port = MODES[args.mode]
    args.device, args.task = device, task
    for name, default in zip(("agent_port", "config_port", "bridge_port", "share_port"),
                             (agent_port, config_port, bridge_port, share_port)):
        value = getattr(args, name)
        value = default if value is None else value
        if not 1 <= value <= 65535:
            parser.error(f"--{name.replace('_', '-')} must be in [1, 65535]")
        setattr(args, name, value)
    args.data_dir = (args.data_dir or ROOT / ".cache/native-phone-lab" / args.mode).expanduser().resolve()
    args.agent_url = f"http://127.0.0.1:{args.agent_port}"
    args.config_url = f"http://127.0.0.1:{args.config_port}"
    args.bridge_url = (args.bridge_url or "").rstrip("/")
    if args.bridge_url:
        from urllib.parse import urlsplit
        url = urlsplit(args.bridge_url)
        if url.scheme != "http" or url.hostname != "127.0.0.1" or not url.port or url.username or url.password or url.path or url.query or url.fragment:
            parser.error("--bridge-url must be an http://127.0.0.1:PORT origin")
    return args


def private_file(path, data):
    """Create a private file without replacing user configuration."""
    with path.open("xb") as output:
        path.chmod(0o600)
        output.write(data)


@contextlib.contextmanager
def target_directory(args):
    directory = args.data_dir
    directory.mkdir(parents=True, mode=0o700, exist_ok=True)
    with (directory / ".launcher.lock").open("a") as lock:
        try:
            fcntl.flock(lock, fcntl.LOCK_EX | fcntl.LOCK_NB)
        except BlockingIOError:
            raise RuntimeError(f"A launcher already owns {directory}; stop it with Ctrl-C first") from None
        marker = directory / ".launcher-mode"
        if marker.exists() and marker.read_text().strip() != args.mode:
            raise RuntimeError("This data directory belongs to a different connection mode; select another --data-dir")
        if not marker.exists():
            private_file(marker, args.mode.encode())
        config = directory / "agent.toml"
        if args.config and not config.exists():
            private_file(config, args.config.expanduser().read_bytes())
        quick_actions = directory / "quick_actions.json"
        if args.mode != "android" and not quick_actions.exists():
            package = "ios_device" if args.mode == "ios" else "iphone_mirroring"
            private_file(quick_actions, (ROOT / "benchmark" / package / "quick_actions.json").read_bytes())
        yield directory


def check_ports(args):
    """Refuse occupied listeners instead of attaching to or stopping strangers."""
    ports = [args.agent_port, args.config_port, args.share_port]
    if not args.bridge_url:
        ports.append(args.bridge_port)
    if len(ports) != len(set(ports)):
        raise RuntimeError("Agent, Config Web, shared files, and bridge require different ports")
    sockets = []
    try:
        for port in ports:
            sock = socket.socket()
            sockets.append(sock)
            try:
                sock.bind(("127.0.0.1", port))
            except OSError:
                raise RuntimeError(f"Port {port} is occupied. Stop its launcher or select another --agent/config/bridge/share-port") from None
    finally:
        for sock in sockets:
            sock.close()


def bridge_python(args):
    if args.mode == "iphone-mirroring":
        return str(args.python.expanduser().absolute()) if args.python else sys.executable
    python = args.python.expanduser().absolute() if args.python else args.data_dir / "venv/bin/python"
    probe = "import PIL"
    if args.mode == "ios":
        probe += "; from importlib.metadata import version; assert version('pymobiledevice3') == '11.52.1'"
    if python.exists() and subprocess.run([str(python), "-c", probe], capture_output=True).returncode == 0:
        return str(python)
    if args.python:
        raise RuntimeError("--python needs Pillow and, for ios, pymobiledevice3 11.52.1; see the bridge README")
    if not shutil.which("uv"):
        raise RuntimeError("Install uv for isolated bridge dependencies, or pass a prepared --python")
    print("Preparing isolated bridge dependencies (first run only)...", flush=True)
    if not python.exists():
        subprocess.run(["uv", "venv", "--python", "3.12", str(args.data_dir / "venv")], check=True)
    dependencies = ["-r", str(ROOT / "benchmark/ios_device/requirements.txt")] if args.mode == "ios" else ["pillow>=10.2.0"]
    subprocess.run(["uv", "pip", "install", "--python", str(python), *dependencies], check=True)
    return str(python)


def agent_binary(args):
    if args.agent_binary:
        binary = args.agent_binary.expanduser().resolve()
        if not binary.is_file() or not os.access(binary, os.X_OK):
            raise RuntimeError("--agent-binary must be an executable native Agent")
        return str(binary)
    if not shutil.which("go"):
        raise RuntimeError("Install Go 1.26+, or pass --agent-binary")
    print("Building native Agent (Go reuses its build cache)...", flush=True)
    binary = args.data_dir / "agent"
    env = dict(os.environ, CGO_ENABLED="0")
    # The checkout's module specifies the supported toolchain. Never inherit a
    # cross-compile target from board-development terminals.
    env.pop("GOOS", None)
    env.pop("GOARCH", None)
    subprocess.run(["go", "build", "-o", str(binary), "./cmd/daemon"], cwd=ROOT / "src/agent", env=env, check=True)
    return str(binary)


def android_serial(args):
    adb = shutil.which("adb")
    if not adb:
        raise RuntimeError("Install Android SDK Platform Tools (adb), authorize USB debugging or pair wireless debugging")
    lines = subprocess.check_output([adb, "devices"], text=True).splitlines()[1:]
    devices = {parts[0]: parts[1] for line in lines if len(parts := line.split()) >= 2}
    serial = args.adb_serial or os.environ.get("ANDROID_SERIAL")
    if not serial:
        ready = [serial for serial, state in devices.items() if state == "device"]
        if len(ready) == 1:
            serial = ready[0]
        else:
            raise RuntimeError("Connect one authorized Android device, or select it with --adb-serial (see adb devices)")
    if devices.get(serial) != "device":
        raise RuntimeError("Selected Android device is offline or unauthorized; check adb devices and the phone")
    return serial


def bridge_command(args, python):
    if args.mode == "android":
        return [python, "-u", "-m", "adbandroid.scripts.start_bridge", "--adb-serial", android_serial(args),
                "--bridge-host", "127.0.0.1", "--bridge-port", str(args.bridge_port)]
    command = [python, "-u", "-m", "ios_device" if args.mode == "ios" else "iphone_mirroring",
               "--port", str(args.bridge_port), "--task-id", args.task]
    if args.mode == "ios":
        command += ["--xctrunner", args.xctrunner]
        if args.udid:
            command += ["--udid", args.udid]
        if args.permission_policy:
            command += ["--permission-policy", str(args.permission_policy.expanduser().resolve())]
    return command


def request(url, payload=None, task="", timeout=5, decode_json=True):
    """Use loopback directly, retaining lease routing and never replaying POSTs."""
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}))
    req = urllib.request.Request(url, data=None if payload is None else json.dumps(payload).encode(),
                                 headers={"Content-Type": "application/json", "benchmark-task-id": task})
    with opener.open(req, timeout=timeout) as response:
        return json.load(response) if decode_json else response.read(1)


class Processes:
    """Own only children created by this launcher, including their descendants."""

    def __init__(self, directory, stopped):
        self.directory, self.stopped, self.children = directory, stopped, []

    def start(self, name, command, env):
        log = self.directory / f"{name}.log"
        with log.open("ab") as output:
            log.chmod(0o600)
            child = subprocess.Popen(command, cwd=ROOT / "benchmark", env=env, stdout=output,
                                     stderr=subprocess.STDOUT, start_new_session=True)
        self.children.append((name, child))
        return child

    def check(self):
        for name, child in self.children:
            if child.poll() is not None:
                raise RuntimeError(f"{name} exited ({child.returncode}); see {self.directory / (name + '.log')}")
        if self.stopped.is_set():
            raise InterruptedError

    def wait_http(self, url, timeout=90, decode_json=True):
        deadline = time.monotonic() + timeout
        while True:
            self.check()
            try:
                return request(url, decode_json=decode_json)
            except (OSError, ValueError):
                if time.monotonic() >= deadline:
                    raise RuntimeError(f"Startup timed out at {url}; inspect logs in {self.directory}") from None
                self.stopped.wait(0.25)

    def close(self):
        groups = {child.pid for _, child in self.children}
        for _, child in reversed(self.children):
            # A failed child can leave descendants in its process group.
            with contextlib.suppress(ProcessLookupError):
                os.killpg(child.pid, signal.SIGTERM)
        deadline = time.monotonic() + 12
        while groups and time.monotonic() < deadline:
            for _, child in self.children:
                child.poll()  # Reap exited parents before checking their groups.
            for group in tuple(groups):
                try:
                    os.killpg(group, 0)
                except ProcessLookupError:
                    groups.remove(group)
            if groups:
                time.sleep(0.05)
        for group in groups:
            with contextlib.suppress(ProcessLookupError):
                os.killpg(group, signal.SIGKILL)
        for _, child in self.children:
            child.wait()


def launch(args, stopped):
    with target_directory(args) as directory:
        if args.request_permissions:
            python = bridge_python(args)
            subprocess.run([python, "-m", "iphone_mirroring", "--request-input-access", "--request-screen-access"],
                           cwd=ROOT / "benchmark", check=True)
            return
        check_ports(args)
        binary = agent_binary(args)
        python = None if args.bridge_url else bridge_python(args)
        env = dict(os.environ, AICO_SHARE_DIR=str(directory / "shared"),
                   AICO_SHARE_ADDR=f"127.0.0.1:{args.share_port}", AICO_SYSTEM_ENV=str(directory / "system.env"))
        processes = Processes(directory, stopped)
        try:
            processes.start("config-web", [binary, "config-web", "--local-agent", "--port", str(args.config_port),
                            "--config", str(directory / "agent.toml"), "--web-root", str(ROOT / "src/config_web/web"),
                            "--agent-url", args.agent_url], env)
            processes.wait_http(args.config_url + "/", decode_json=False)
            print(f"Mode: {args.mode}\nConfig Web: {args.config_url}\nData/logs: {directory}\nCtrl-C stops this launch.", flush=True)
            if not args.no_open:
                webbrowser.open(args.config_url)
            config = directory / "agent.toml"
            if not config.is_file():
                print("First run: save your model and credentials in Config Web; startup continues automatically.", flush=True)
            while not config.is_file():
                processes.check()
                stopped.wait(0.25)
            if not args.bridge_url:
                processes.start("bridge", bridge_command(args, python), env)
            endpoint = args.bridge_url or f"http://127.0.0.1:{args.bridge_port}"
            if args.bridge_url:
                try:
                    health = request(endpoint + "/health").get("data", {})
                except (OSError, ValueError):
                    raise RuntimeError("Existing bridge is unavailable; restore it before using --bridge-url") from None
            else:
                health = processes.wait_http(endpoint + "/health").get("data", {})
            expected = {"android": "adb_android", "iphone-mirroring": "iphone_mirroring", "ios": "ios_device"}[args.mode]
            if health.get("bridge_type") != expected:
                raise RuntimeError("Bridge identity does not match the selected mode")
            if health.get("input_available") is False or health.get("locked"):
                raise RuntimeError("The phone is locked or input is unavailable; restore the connection before starting")
            # Acquire the route once, including external iOS bridges whose idle
            # lease may have expired. Android setup goes Home, without erasing data.
            processes.check()
            request(endpoint + "/api/setup", {}, args.task, timeout=30)
            processes.check()
            processes.start("agent", [binary, "-dir", str(directory), "-addr", f"127.0.0.1:{args.agent_port}",
                            "--device-type", args.device, "--environment-bridge-mode",
                            "--environment-bridge-endpoint", endpoint, "--benchmark-task-id", args.task], env)
            health = processes.wait_http(args.agent_url + "/health").get("data", {})
            if health.get("device_type") != args.device:
                raise RuntimeError("Agent identity does not match the selected mode")
            frame = request(args.agent_url + "/api/providers/screenshot", {}, timeout=35)
            if not frame.get("ok") or not frame.get("data", {}).get("image"):
                raise RuntimeError("Agent capture verification failed; inspect the bridge log before interacting")
            print(f"Ready: {args.agent_url} (fresh phone screenshot verified)", flush=True)
            if not args.no_open:
                webbrowser.open(args.agent_url)
            while not stopped.wait(0.5):
                processes.check()
        finally:
            processes.close()


def main(argv=None):
    args = arguments(argv)
    stopped = threading.Event()
    previous = {sig: signal.signal(sig, lambda *_: stopped.set()) for sig in (signal.SIGINT, signal.SIGTERM)}
    try:
        launch(args, stopped)
        return 0
    except (KeyboardInterrupt, InterruptedError):
        return 0
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
        print(f"local-agent: {exc}", file=sys.stderr)
        return 1
    finally:
        for sig, handler in previous.items():
            signal.signal(sig, handler)


if __name__ == "__main__":
    raise SystemExit(main())
