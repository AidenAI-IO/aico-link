"""Exercise launcher ownership, first-run configuration, and failure cleanup."""
from __future__ import annotations

import importlib.util
import os
from pathlib import Path
import socket
import sys
import threading
import time

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


@pytest.mark.parametrize("mode", ["android", "iphone-mirroring", "ios"])
@pytest.mark.parametrize("outcome", ["ready", "bridge-fails", "cancel-after-health"])
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
    monkeypatch.setattr(launcher, "bridge_python", lambda _: sys.executable)
    monkeypatch.setattr(launcher, "bridge_command", lambda args, _: [str(fixture), "bridge", "--port", str(args.bridge_port)])
    stopped = threading.Event()
    errors = []
    real_request = launcher.request

    def request(url, *positional, **keywords):
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
    finally:
        stopped.set()
        thread.join(timeout=15)
    assert not thread.is_alive()
    if outcome == "ready":
        assert not errors
    for pidfile in args.data_dir.glob("*.pid"):
        with pytest.raises(ProcessLookupError):
            os.kill(int(pidfile.read_text()), 0)
