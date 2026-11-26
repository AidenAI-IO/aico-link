"""Exercise launcher ownership, first-run configuration, and failure cleanup."""
from __future__ import annotations

import importlib.util
import json
import os
from pathlib import Path
import socket
import sys
import threading
import time
from types import SimpleNamespace

import pytest


spec = importlib.util.spec_from_file_location("local_agent", Path(__file__).resolve().parents[2] / "scripts/local_agent.py")
launcher = importlib.util.module_from_spec(spec)
spec.loader.exec_module(launcher)


@pytest.fixture
def args(tmp_path, monkeypatch):
    monkeypatch.setattr(launcher.sys, "platform", "darwin")
    sockets = [socket.socket() for _ in range(4)]
    for sock in sockets:
        sock.bind(("127.0.0.1", 0))
    ports = [sock.getsockname()[1] for sock in sockets]
    for sock in sockets:
        sock.close()
    return launcher.arguments(["ios", "--data-dir", str(tmp_path / "target"), "--no-open",
        "--agent-port", str(ports[0]), "--config-port", str(ports[1]),
        "--bridge-port", str(ports[2]), "--share-port", str(ports[3])])


@pytest.mark.parametrize("location", ["sibling", "path", "missing", "legacy"])
def test_supplied_agent_checks_device_cli(args, tmp_path, monkeypatch, location):
    args.agent_binary = tmp_path / "agent"
    args.agent_binary.touch(mode=0o755)
    bin_dir = tmp_path / "bin"
    bin_dir.mkdir()
    monkeypatch.setenv("PATH", str(bin_dir))
    monkeypatch.delenv("AICO_LEGACY_DEVICE_TOOLS", raising=False)
    if location in {"sibling", "path"}:
        cli_dir = tmp_path if location == "sibling" else bin_dir
        (cli_dir / "device-cli").touch(mode=0o755)
    if location == "legacy":
        monkeypatch.setenv("AICO_LEGACY_DEVICE_TOOLS", "1")
    if location == "missing":
        with pytest.raises(RuntimeError, match="device-cli"):
            launcher.agent_binary(args)
    else:
        assert launcher.agent_binary(args) == str(args.agent_binary.resolve())


def test_target_config_is_preserved_and_cannot_be_shared_between_modes(args, tmp_path):
    args.config = tmp_path / "import.toml"
    args.config.write_text("original configuration")
    with launcher.target_directory(args) as directory:
        assert (directory / "agent.toml").read_text() == "original configuration"
        assert (directory / "agent.toml").stat().st_mode & 0o777 == 0o600
        (directory / "quick_actions.json").write_text('"custom mapping"')
        with pytest.raises(RuntimeError, match="already owns"):
            with launcher.target_directory(args):
                pytest.fail("second launcher acquired the same directory")
    args.config.write_text("replacement must not be imported")
    with launcher.target_directory(args) as directory:
        assert (directory / "agent.toml").read_text() == "original configuration"
        assert (directory / "quick_actions.json").read_text() == '"custom mapping"'
    args.mode = "iphone-mirroring"
    with pytest.raises(RuntimeError, match="different connection mode"):
        with launcher.target_directory(args):
            pytest.fail("mixed connection modes shared configuration")


@pytest.mark.parametrize("options,expected", [([], "adbkeyboard"), (["--unicode-input-method", "appium"], "appium")])
def test_android_unicode_input_method_option_is_forwarded(tmp_path, monkeypatch, options, expected):
    monkeypatch.setattr(launcher.sys, "platform", "darwin")
    args = launcher.arguments([
        "android", *options, "--data-dir", str(tmp_path / "android"), "--no-open",
    ])
    monkeypatch.setattr(launcher, "android_serial", lambda _args: "emulator-5554")

    command = launcher.bridge_command(args, "/prepared/python")

    assert command[-2:] == ["--unicode-input-method", expected]


def test_unicode_input_method_rejects_non_android_mode(tmp_path, monkeypatch):
    monkeypatch.setattr(launcher.sys, "platform", "darwin")

    with pytest.raises(SystemExit):
        launcher.arguments(["ios", "--unicode-input-method", "appium", "--data-dir", str(tmp_path / "ios")])


def test_occupied_port_is_rejected_without_starting_a_process(args):
    with socket.socket() as occupied:
        occupied.bind(("127.0.0.1", args.agent_port))
        occupied.listen()
        with pytest.raises(RuntimeError, match="occupied"):
            launcher.check_ports(args)
        assert occupied.fileno() >= 0


@pytest.mark.parametrize("ignore_term", [False, True])
def test_exited_parent_does_not_leave_a_descendant_listener(tmp_path, ignore_term):
    stopped = threading.Event()
    processes = launcher.Processes(tmp_path, stopped)
    port_file = tmp_path / "descendant-port"
    descendant = """
import signal, socket, sys, time
from pathlib import Path
if sys.argv[2] == 'True': signal.signal(signal.SIGTERM, signal.SIG_IGN)
with socket.socket() as listener:
    listener.bind(('127.0.0.1', 0))
    listener.listen()
    Path(sys.argv[1]).write_text(str(listener.getsockname()[1]))
    time.sleep(60)
"""
    parent = """
import subprocess, sys, time
from pathlib import Path
subprocess.Popen([sys.executable, '-c', sys.argv[1], sys.argv[2], sys.argv[3]])
while not Path(sys.argv[2]).exists(): time.sleep(.01)
"""
    try:
        child = processes.start("parent", [sys.executable, "-c", parent, descendant, str(port_file), str(ignore_term)], os.environ.copy())
        child.wait(timeout=10)
        port = int(port_file.read_text())
    finally:
        processes.close()
    with socket.socket() as listener:
        deadline = time.monotonic() + 2
        while True:
            try:
                listener.bind(("127.0.0.1", port))
                break
            except OSError:
                if time.monotonic() >= deadline:
                    raise
                time.sleep(.02)


@pytest.mark.parametrize("existing,force,builds", [(True, False, 0), (False, False, 1), (True, True, 1)])
def test_frontend_build_reuses_assets_unless_missing_or_forced(monkeypatch, tmp_path, existing, force, builds):
    monkeypatch.setattr(launcher, "ROOT", tmp_path)
    web_root = tmp_path / "src/config_web/dist"
    if existing:
        web_root.mkdir(parents=True)
        (web_root / "index.html").write_text("existing frontend")
    calls = []

    def build(*args, **kwargs):
        calls.append(True)
        web_root.mkdir(parents=True, exist_ok=True)
        (web_root / "index.html").write_text("rebuilt frontend")

    monkeypatch.setattr(launcher.subprocess, "run", build)
    result = launcher.build_config_web(force=force)
    assert result == web_root
    assert len(calls) == builds
    assert (result / "index.html").read_text() == ("rebuilt frontend" if builds else "existing frontend")


@pytest.mark.parametrize("mode,outcome", [
    (mode, outcome)
    for mode in ("android", "iphone-mirroring", "ios")
    for outcome in ("ready", "bridge-fails", "cancel-after-health")
] + [("android", "slow-ime")])
def test_launch_waits_for_first_config_and_cleans_up_owned_children(args, tmp_path, monkeypatch, mode, outcome):
    args.mode, args.device, args.task = mode, *launcher.MODES[mode][:2]
    args.bridge_url = ""
    args.no_open = False
    ready = threading.Event()
    monkeypatch.setattr(launcher.webbrowser, "open", lambda url: ready.set() if url == args.agent_url else None)
    fixture = tmp_path / "fake-agent"
    fixture.write_text('''#!/usr/bin/env python3
import json, os, sys
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path
role = sys.argv[1] if sys.argv[1] in ('config-web', 'bridge') else 'agent'
def option(name): return sys.argv[sys.argv.index(name) + 1]
directory = Path(os.environ['AICO_SYSTEM_ENV']).parent
(directory / (role + '.pid')).write_text(str(os.getpid()))
(directory / (role + '.args')).write_text(json.dumps(sys.argv[1:]))
if role == 'bridge' and os.environ.get('TEST_BRIDGE_FAIL') == '1': sys.exit(7)
port = int(option('--port')) if role != 'agent' else int(option('-addr').split(':')[1])
class Handler(BaseHTTPRequestHandler):
    def log_message(self, *args): pass
    def do_GET(self):
        self.send_response(200); self.end_headers()
        if role == 'config-web': self.wfile.write(b'<html>Settings</html>'); return
        value = {'bridge_type': os.environ['TEST_BRIDGE_TYPE']} if role == 'bridge' else {'device_type': os.environ['TEST_DEVICE']}
        self.wfile.write(json.dumps({'ok': True, 'data': value}).encode())
    def do_POST(self):
        self.rfile.read(int(self.headers.get('Content-Length', '0')))
        if role == 'bridge':
            (directory / 'setup-task').write_text(self.headers.get('benchmark-task-id', ''))
            value = {'task_id': self.headers.get('benchmark-task-id')}
        else:
            (directory / 'captured').write_text('yes')
            value = {'image': 'aGVsbG8='}
        self.send_response(200); self.end_headers()
        self.wfile.write(json.dumps({'ok': True, 'data': value}).encode())
HTTPServer(('127.0.0.1', port), Handler).serve_forever()
''')
    fixture.chmod(0o755)
    monkeypatch.setenv("TEST_DEVICE", args.device)
    monkeypatch.setenv("TEST_BRIDGE_TYPE", {"android": "adb_android", "iphone-mirroring": "iphone_mirroring", "ios": "ios_device"}[mode])
    monkeypatch.setenv("TEST_BRIDGE_FAIL", "1" if outcome == "bridge-fails" else "0")
    monkeypatch.setattr(launcher, "agent_binary", lambda _: str(fixture))
    monkeypatch.setattr(launcher, "build_config_web", lambda force=False: tmp_path / "web")
    monkeypatch.setattr(launcher, "bridge_python", lambda _: sys.executable)
    monkeypatch.setattr(launcher, "bridge_command", lambda args, _: [str(fixture), "bridge", "--port", str(args.bridge_port)])
    stopped = threading.Event()
    errors = []
    real_request = launcher.request
    install_elapsed = 0
    if outcome == "slow-ime":
        monkeypatch.setattr(launcher, "time", SimpleNamespace(
            monotonic=lambda: time.monotonic() + install_elapsed, sleep=time.sleep,
        ))

    def request(url, *positional, **keywords):
        nonlocal install_elapsed
        if outcome == "slow-ime" and url == f"http://127.0.0.1:{args.bridge_port}/health" and install_elapsed < 100:
            # Advance only the launcher's clock while the initial IME install
            # keeps the bridge unavailable beyond the former 90-second limit.
            install_elapsed += 25
            raise OSError("IME installation is still running")
        result = real_request(url, *positional, **keywords)
        if outcome == "cancel-after-health" and url == f"http://127.0.0.1:{args.bridge_port}/health":
            stopped.set()
        return result

    monkeypatch.setattr(launcher, "request", request)

    def run():
        try:
            launcher.launch(args, stopped)
        except Exception as exc:
            errors.append(exc)

    thread = threading.Thread(target=run, daemon=True)
    thread.start()
    try:
        deadline = time.monotonic() + 10
        while not (args.data_dir / "config-web.pid").exists() and thread.is_alive() and time.monotonic() < deadline:
            time.sleep(.05)
        assert not (args.data_dir / "bridge.pid").exists(), "bridge started before first config save"
        (args.data_dir / "agent.toml").write_text("test configuration")
        while thread.is_alive() and not ready.is_set() and time.monotonic() < deadline:
            time.sleep(.05)
        if outcome == "bridge-fails":
            assert errors and "bridge exited (7)" in str(errors[0])
            assert not (args.data_dir / "agent.pid").exists()
        elif outcome == "cancel-after-health":
            assert errors and isinstance(errors[0], InterruptedError)
            assert not (args.data_dir / "setup-task").exists()
            assert not (args.data_dir / "agent.pid").exists()
        else:
            assert not errors
            assert ready.is_set(), "Agent UI was not opened after completed initialization"
            assert (args.data_dir / "captured").exists(), "no fresh capture before readiness"
            assert (args.data_dir / "setup-task").read_text() == args.task
            agent_args = json.loads((args.data_dir / "agent.args").read_text())
            assert ("--local-phone-bridge" in agent_args) == (mode == "android")
    finally:
        stopped.set()
        thread.join(timeout=15)
    assert not thread.is_alive()
    if outcome in ("ready", "slow-ime"):
        assert not errors
    for pidfile in args.data_dir.glob("*.pid"):
        with pytest.raises(ProcessLookupError):
            os.kill(int(pidfile.read_text()), 0)
