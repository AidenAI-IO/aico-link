"""Aico Launcher backend: HTTP security, instance lifecycle, and helpers.

Everything runs against a fake System, so no Docker, adb, or network is used.
"""

from __future__ import annotations

import http.client
import json
import subprocess
import sys
import threading
import time
import urllib.parse
from pathlib import Path
from typing import Any

import pytest

from launcher import LABEL_INSTANCE, pyenv, validation
from launcher.bridges import detect_guest_ip, device_type_from_health, parse_dhcp_leases
from launcher.images import build_command
from launcher.instances import Conflict, agent_container, choose_ports
from launcher.net import container_proxy, container_side_url, host_side_url
from launcher.server import COOKIE_NAME, Launcher, create_server
from launcher.store import Store, lock_state_dir
from launcher.system import System


class FakeSystem(System):
    """Simulates docker, adb, git, bridge processes, and HTTP probes."""

    def __init__(self) -> None:
        super().__init__()
        self.clock = 0.0
        self.calls: list[list[str]] = []
        self.events: list[str] = []
        self.images = {"aico-link:test"}
        self.containers: dict[str, dict[str, str]] = {}
        self.pids: dict[int, str] = {}
        self.next_pid = 4242
        self.agent_ready = True
        self.busy_ports: set[int] = set()
        self.health: dict[str, Any] = {"status": "ok", "platform": "android"}
        self.health_urls: list[str] = []
        self.health_hosts: set[str] | None = None  # None: every address answers
        self.bind_host = "127.0.0.1"
        self.failing: set[str] = set()  # docker subcommands that fail
        self.env: dict[str, str] = {}
        self.os = "darwin"
        self.xcode = True
        self.deps_installed = False
        self.started: list[list[str]] = []
        self.reachable: set[tuple[str, int]] = set()
        self.usbmux = "[]"
        self.missing_tools: set[str] = set()
        self.posts: list[tuple[str, dict[str, str]]] = []

    def run(self, args, *, timeout=60, cwd=None, env=None):
        args = [str(arg) for arg in args]
        self.calls.append(args)

        def done(code: int = 0, out: str = "", err: str = "") -> subprocess.CompletedProcess:
            return subprocess.CompletedProcess(args, code, out, err)

        if args[:2] == ["git", "rev-parse"]:
            return done(out="a" * 40 + "\n")
        if args[:2] == ["git", "status"]:
            return done(out="")
        if args[0].endswith("/uv"):
            return done(self._uv(args))
        if args[0] in {"xcode-select", "xcrun"}:
            return done(out="/usr/bin/swiftc\n") if self.xcode else done(1, err="no developer tools")
        if args[0].endswith("/bin/python") and "-c" in args:  # pyenv import probe
            return done() if self.deps_installed else done(1, err="ModuleNotFoundError")
        if args[2:5] == ["pymobiledevice3", "usbmux", "list"]:
            return done(out=self.usbmux)
        if args[0] != "docker":
            if args[-1] == "get-state":
                return done(out="device\n")
            return done(out="List of devices attached\n")
        sub = args[1]
        if sub in self.failing:
            return done(1, err="Cannot connect to the Docker daemon")
        if sub == "version":
            return done(out="29.0.0\n")
        if sub == "image" and args[2] == "inspect":
            return done(out="sha256:x\n") if args[-1] in self.images else done(1, err="No such image")
        if sub == "image" and args[2] == "ls":
            return done(out="")
        if sub == "inspect":
            container = self.containers.get(args[-1])
            return done(out=container["state"] + "\n") if container else done(1, err=f"error: no such object: {args[-1]}")
        if sub == "run":
            name = args[args.index("--name") + 1]
            label = next(a for a in args if a.startswith(LABEL_INSTANCE + "="))
            self.containers[name] = {"state": "running", "instance": label.split("=", 1)[1]}
            self.events.append(f"run {name}")
            return done(out="cid\n")
        if sub == "stop":
            self.events.append(f"stop {args[-1]}")
            if args[-1] in self.containers:
                self.containers[args[-1]]["state"] = "exited"
            return done()
        if sub == "rm":
            self.events.append(f"rm {args[-1]}")
            self.containers.pop(args[-1], None)
            return done()
        if sub == "ps":
            lines = [f"{name}\t{c['state']}\t{c['instance']}" for name, c in self.containers.items()]
            return done(out="\n".join(lines) + "\n")
        if sub == "logs":
            return done(out="agent: boot failed\n")
        return done()

    def run_logged(self, args, log_path, *, cwd=None, env=None, should_stop=None):
        args = [str(arg) for arg in args]
        self.calls.append(args)
        return self._uv(args) if args[0].endswith("/uv") else 0

    def _uv(self, args: list[str]) -> int:
        if args[1:2] == ["venv"]:  # uv venv --python 3.12 DIR
            python = Path(args[-1]) / "bin" / "python"
            python.parent.mkdir(parents=True)
            python.touch()
        if args[1:3] == ["pip", "install"]:
            self.deps_installed = True
        return 0

    def platform(self) -> str:
        return self.os

    def tcp_reachable(self, host: str, port: int, timeout: float = 2.0) -> bool:
        return (host, port) in self.reachable

    def which(self, name: str) -> str:
        return "" if name in self.missing_tools else f"/usr/bin/{name}"

    def http_status(self, url: str, timeout: float = 2.0) -> int:
        return 200 if self.agent_ready else 0

    def read_health(self, endpoint: str, timeout: float = 5.0) -> dict[str, Any]:
        self.health_urls.append(endpoint)
        if self.health_hosts is not None and urllib.parse.urlsplit(endpoint).hostname not in self.health_hosts:
            raise ConnectionRefusedError("connection refused")
        return dict(self.health)

    def port_free(self, port: int) -> bool:
        return port not in self.busy_ports

    def free_port(self) -> int:
        self.next_pid += 1
        return 20000 + self.next_pid

    def bridge_bind_host(self) -> str:
        return self.bind_host

    def start_process(self, args, *, cwd, log_path, env=None) -> int:
        args = [str(arg) for arg in args]
        pid = self.next_pid
        self.next_pid += 1
        self.pids[pid] = " ".join(args)
        self.started.append(args)
        self.events.append(f"bridge-start {pid}")
        return pid

    def pid_command(self, pid: int) -> str:
        return self.pids.get(pid, "")

    def terminate(self, pid: int, wait_sec: float = 5.0) -> None:
        self.events.append(f"terminate {pid}")
        self.pids.pop(pid, None)

    def post_json(self, url, payload, *, headers=None, timeout=10.0) -> int:
        self.posts.append((url, dict(headers or {})))
        return 200

    def resolve_mobilegym_commit(self, repo_root: Path) -> str:
        return "b" * 40

    def monotonic(self) -> float:
        return self.clock

    def sleep(self, seconds: float) -> None:
        self.clock += max(seconds, 0.01)

    def environ(self):
        return self.env


@pytest.fixture
def fake() -> FakeSystem:
    return FakeSystem()


@pytest.fixture
def app(tmp_path: Path, fake: FakeSystem) -> Launcher:
    launcher = Launcher(tmp_path / "state", system=fake, repo_root=tmp_path)
    launcher.instances.health_check_interval = 0
    return launcher


def wait_settled(app: Launcher, instance_id: str) -> dict[str, Any]:
    deadline = time.monotonic() + 10
    while time.monotonic() < deadline:
        record = app.store.instance(instance_id)
        worker = app.instances._workers.get(instance_id)
        if record["status"] not in {"starting", "stopping"} and (worker is None or not worker.is_alive()):
            return app.instances.get(instance_id)
        time.sleep(0.01)
    raise AssertionError(f"instance {instance_id} did not settle: {record}")


def body(name: str, bridge_type: str, config: dict[str, Any] | None = None, **extra: Any) -> dict[str, Any]:
    return {"name": name, "image": "aico-link:test", "bridge": {"type": bridge_type, "config": config or {}}, **extra}


def env_of(command: list[str]) -> dict[str, str]:
    return dict(command[i + 1].split("=", 1) for i, arg in enumerate(command) if arg == "-e")


def docker_run(fake: FakeSystem) -> list[str]:
    return next(call for call in fake.calls if call[:2] == ["docker", "run"])


# -- HTTP security -------------------------------------------------------------


@pytest.fixture
def server(app: Launcher):
    srv, token = create_server(app, port=0, token="secret-token")
    thread = threading.Thread(target=srv.serve_forever, daemon=True)
    thread.start()
    yield srv.server_address[1], token
    srv.shutdown()
    srv.server_close()


def request(port: int, method: str, path: str, *, host: str | None = None, headers=None, payload=None, conn=None):
    own = conn is None
    conn = conn or http.client.HTTPConnection("127.0.0.1", port, timeout=5)
    all_headers = {"Host": host or f"127.0.0.1:{port}", **(headers or {})}
    data = None
    if payload is not None:
        data = json.dumps(payload).encode()
        all_headers.setdefault("Content-Type", "application/json")
    conn.request(method, path, body=data, headers=all_headers)
    response = conn.getresponse()
    content = response.read()
    if own:
        conn.close()
    return response, content


def auth(port: int, token: str) -> dict[str, str]:
    return {"Cookie": f"{COOKIE_NAME}_{port}={token}"}


def test_token_exchange_sets_cookie_and_api_requires_it(server):
    port, token = server
    response, _ = request(port, "GET", "/api/state")
    assert response.status == 401

    response, _ = request(port, "GET", "/?token=wrong")
    assert response.status == 200 and response.getheader("Set-Cookie") is None

    response, _ = request(port, "GET", f"/?token={token}")
    assert response.status == 303 and response.getheader("Location") == "/"
    cookie = response.getheader("Set-Cookie")
    # The name carries the port: cookies are shared by every port on 127.0.0.1.
    assert cookie.startswith(f"{COOKIE_NAME}_{port}={token};")
    assert "HttpOnly" in cookie and "SameSite=Strict" in cookie

    # Non-RFC cookies set by other local apps must not hide ours.
    other_apps = 'session={"a":1}; theme=dark mode; '
    response, content = request(
        port, "GET", "/api/state", headers={"Cookie": other_apps + auth(port, token)["Cookie"]}
    )
    assert response.status == 200
    assert json.loads(content)["host"]["docker"]["available"] is True

    for wrong in (f"{COOKIE_NAME}_{port}=nope", f"{COOKIE_NAME}_{port + 1}={token}"):
        response, _ = request(port, "GET", "/api/state", headers={"Cookie": wrong})
        assert response.status == 401


def test_unread_bodies_do_not_corrupt_kept_alive_connections(server, app: Launcher):
    port, token = server
    created = app.instances.create(body("Demo", "none"))
    conn = http.client.HTTPConnection("127.0.0.1", port, timeout=5)
    try:
        # Start ignores its body; a refused request never reaches a handler.
        for headers, expected in ((auth(port, token), 202), ({}, 401)):
            response, _ = request(
                port, "POST", f"/api/instances/{created['id']}/start", headers=headers, payload={}, conn=conn
            )
            assert response.status == expected
            response, content = request(port, "GET", "/api/state", headers=auth(port, token), conn=conn)
            assert response.status == 200 and "instances" in json.loads(content)
            wait_settled(app, created["id"])
    finally:
        conn.close()


@pytest.mark.parametrize(
    ("method", "host", "origin", "content_type", "expected"),
    [
        ("GET", "evil.example:{port}", None, None, 403),  # DNS rebinding
        ("GET", "127.0.0.1:1", None, None, 403),
        ("GET", "localhost:{port}", None, None, 200),
        ("POST", "127.0.0.1:{port}", "http://evil.example", "application/json", 403),
        ("POST", "127.0.0.1:{port}", "http://localhost:{port}", "application/json", 403),
        ("POST", "127.0.0.1:{port}", None, "text/plain", 415),
        ("POST", "127.0.0.1:{port}", "http://127.0.0.1:{port}", "application/json", 201),
        ("POST", "localhost:{port}", "http://localhost:{port}", "application/json", 201),
    ],
)
def test_host_origin_and_content_type_rules(server, method, host, origin, content_type, expected):
    port, token = server
    headers = auth(port, token)
    if origin:
        headers["Origin"] = origin.format(port=port)
    path = "/api/state"
    payload = None
    if method == "POST":
        path = "/api/instances"
        payload = body("Demo", "none")
        headers["Content-Type"] = content_type
    response, _ = request(port, method, path, host=host.format(port=port), headers=headers, payload=payload)
    assert response.status == expected


def test_static_files_do_not_escape_the_static_directory(server):
    port, _ = server
    response, _ = request(port, "GET", "/static/../server.py")
    assert response.status == 404
    response, _ = request(port, "GET", "/static/%2e%2e/server.py")
    assert response.status == 404


# -- lifecycle -------------------------------------------------------------------


def test_start_runs_agent_on_loopback_with_bridge_env(app: Launcher, fake: FakeSystem):
    fake.env = {"HTTPS_PROXY": "http://127.0.0.1:7890", "http_proxy": "proxy.corp:3128", "NO_PROXY": "corp.internal"}
    app.store.set_settings({**app.store.settings(), "pass_host_proxy": True})
    created = app.instances.create(
        body("Phone", "custom", {"endpoint": "http://localhost:19090"}, enable_terminal=True)
    )
    app.instances.start(created["id"])
    instance = wait_settled(app, created["id"])

    assert instance["status"] == "running", instance["error"]
    # The host-side probe used the address containers reach; they get host.docker.internal.
    assert fake.health_urls == ["http://127.0.0.1:19090"]
    command = docker_run(fake)
    publish = [command[i + 1] for i, arg in enumerate(command) if arg == "-p"]
    assert publish == ["127.0.0.1:8000:80", "127.0.0.1:8080:8080"]
    assert f"aico-launcher-{created['id']}-data:/userdata" in command
    env = env_of(command)
    route = f"launcher-{created['id']}"
    assert env["AICO_ENVIRONMENT_BRIDGE_ENDPOINT"] == "http://host.docker.internal:19090"
    assert env["AICO_DEVICE_TYPE"] == "Android"
    assert env["AICO_BENCHMARK_TASK_ID"] == env["AICO_BRIDGE_EPISODE_ID"] == route
    assert env["AICO_PUBLIC_AGENT_WEB_PORT"] == "8080"
    assert env["AICO_ENABLE_TERMINAL"] == "1"
    # A proxy on the host's loopback is reached through host.docker.internal.
    assert env["HTTPS_PROXY"] == "http://host.docker.internal:7890"
    assert env["http_proxy"] == "proxy.corp:3128"
    assert env["NO_PROXY"].split(",")[:4] == ["localhost", "127.0.0.1", "host.docker.internal", "corp.internal"]
    assert instance["urls"]["terminal"] == "http://127.0.0.1:8080/webtty/"


def test_custom_bridge_on_loopback_is_refused_when_containers_cannot_reach_it(app: Launcher, fake: FakeSystem):
    # Linux: containers reach the host at the docker0 gateway, not its loopback.
    fake.bind_host = "172.17.0.1"
    fake.health_hosts = {"127.0.0.1"}
    result = app.probe("custom", {"config": {"endpoint": "http://localhost:19090"}})
    assert result["ok"] is False
    assert "listens only on 127.0.0.1" in result["checks"][0]["detail"]

    fake.health_hosts = {"172.17.0.1"}
    assert app.probe("custom", {"config": {"endpoint": "http://localhost:19090"}})["ok"] is True
    assert fake.health_urls[-1] == "http://172.17.0.1:19090"


def test_failed_start_cleans_up_and_reports_container_output(app: Launcher, fake: FakeSystem):
    fake.agent_ready = False
    created = app.instances.create(body("Phone", "adb_android", {"serial": "emulator-5554"}))
    app.instances.start(created["id"])
    instance = wait_settled(app, created["id"])

    assert instance["status"] == "error"
    assert "did not answer" in instance["error"] and "agent: boot failed" in instance["error"]
    assert agent_container(created["id"]) not in fake.containers
    assert not fake.pids, "the ADB bridge must be terminated"
    assert app.store.instance(created["id"])["runtime"] == {}


def test_stop_stops_agent_before_bridge(app: Launcher, fake: FakeSystem):
    created = app.instances.create(body("Phone", "adb_android", {"serial": "emulator-5554"}))
    app.instances.start(created["id"])
    assert wait_settled(app, created["id"])["status"] == "running"
    pid = app.store.instance(created["id"])["runtime"]["bridge_pid"]
    fake.events.clear()

    app.instances.stop(created["id"])
    assert wait_settled(app, created["id"])["status"] == "stopped"
    container = agent_container(created["id"])
    assert fake.events == [f"stop {container}", f"rm {container}", f"terminate {pid}"]


# inspect: a failed inspection must not read as an absent container.
@pytest.mark.parametrize("failing", [{"stop", "rm"}, {"inspect"}])
def test_stop_reports_docker_failures_instead_of_stopped(app: Launcher, fake: FakeSystem, failing: set[str]):
    created = app.instances.create(body("Demo", "none"))
    app.instances.start(created["id"])
    assert wait_settled(app, created["id"])["status"] == "running"
    fake.failing = failing

    app.instances.stop(created["id"])
    instance = wait_settled(app, created["id"])
    assert instance["status"] == "error"
    assert "Cannot connect to the Docker daemon" in instance["error"]
    assert agent_container(created["id"]) in fake.containers


@pytest.mark.parametrize(
    "recycled",
    ["/usr/bin/some-other-process", "python -u -m ios_device --port 1", "vim adbandroid.scripts.start_bridge.py"],
)
def test_stop_never_signals_a_recycled_pid(app: Launcher, fake: FakeSystem, recycled: str):
    created = app.instances.create(body("Phone", "adb_android", {"serial": "emulator-5554"}))
    app.instances.start(created["id"])
    wait_settled(app, created["id"])
    pid = app.store.instance(created["id"])["runtime"]["bridge_pid"]
    fake.pids[pid] = recycled

    app.instances.stop(created["id"])
    wait_settled(app, created["id"])
    assert f"terminate {pid}" not in fake.events


@pytest.mark.parametrize(
    ("first", "second"),
    [
        (("adb_android", {"serial": "emulator-5554"}), ("adb_android", {"serial": "emulator-5554"})),
        (("ios_device", {"udid": "00008110-001A"}), ("ios_device", {"udid": "00008110-001A"})),
        # The first instance picked the only connected iPhone at start.
        (("ios_device", {}), ("ios_device", {"udid": "00008110-001A"})),
        # Both drive this computer's real pointer and keyboard.
        (("desktop", {"confirm_control": True}), ("iphone_mirroring", {})),
    ],
)
def test_same_device_cannot_run_twice(app: Launcher, fake: FakeSystem, first, second):
    fake.usbmux = json.dumps([{"UniqueDeviceID": "00008110-001A", "ConnectionType": "USB"}])
    first = app.instances.create(body("One", *first))
    second = app.instances.create(body("Two", *second))
    app.instances.start(first["id"])
    assert wait_settled(app, first["id"])["status"] == "running"
    with pytest.raises(Conflict, match="already used"):
        app.instances.start(second["id"])


def test_reconcile_reattaches_running_and_clears_vanished(tmp_path: Path, fake: FakeSystem):
    first = Launcher(tmp_path / "state", system=fake, repo_root=tmp_path)
    alive = first.instances.create(body("Alive", "none"))
    gone = first.instances.create(body("Gone", "none"))
    for created in (alive, gone):
        first.instances.start(created["id"])
        assert wait_settled(first, created["id"])["status"] == "running"
    # The launcher exits (instances keep running); one container disappears meanwhile.
    fake.containers.pop(agent_container(gone["id"]))

    second = Launcher(tmp_path / "state", system=fake, repo_root=tmp_path)
    second.instances.reconcile()
    assert second.instances.get(alive["id"])["status"] == "running"
    assert second.instances.get(gone["id"])["status"] == "stopped"


def test_edit_and_delete_require_a_stopped_instance(app: Launcher):
    created = app.instances.create(body("Demo", "none"))
    app.instances.start(created["id"])
    wait_settled(app, created["id"])
    with pytest.raises(Conflict):
        app.instances.update(created["id"], body("Renamed", "none"))
    with pytest.raises(Conflict):
        app.instances.delete(created["id"], purge_data=False)


# -- helpers ---------------------------------------------------------------------


@pytest.mark.parametrize(
    ("requested", "taken", "busy", "expected"),
    [
        ({}, set(), set(), {"config_web": 8000, "agent_web": 8080}),
        ({}, {8000}, set(), {"config_web": 30001, "agent_web": 8080}),
        ({}, set(), {8080}, {"config_web": 8000, "agent_web": 30001}),
        ({"config_web": 9000}, set(), set(), {"config_web": 9000, "agent_web": 8080}),
        ({"agent_web": 8000}, set(), set(), {"config_web": 30001, "agent_web": 8000}),
    ],
)
def test_choose_ports(requested, taken, busy, expected):
    counter = iter(range(30001, 30100))
    assert choose_ports(requested, taken, lambda port: port not in busy, lambda: next(counter)) == expected


@pytest.mark.parametrize(
    ("health", "expected"),
    [
        ({"platform": "android"}, "Android"),
        ({"platform": "ios"}, "iOS"),
        ({"platform": "mac"}, "macOS"),
        ({"platform": "macos"}, "macOS"),
        ({"platform": "windows"}, "windows"),
        ({"platform": "linux"}, "linux"),
        ({"bridge_type": "adb_android"}, "Android"),
        ({"platform": "plan9"}, ""),
        ({}, ""),
    ],
)
def test_device_type_from_health(health, expected):
    assert device_type_from_health(health) == expected


@pytest.mark.parametrize(
    ("build_settings", "expected_args"),
    [
        ({}, set()),
        ({"base_image": "mirror/debian:trixie-slim", "go_download_base": "https://golang.google.cn/dl"},
         {"BASE_IMAGE=mirror/debian:trixie-slim", "GO_DOWNLOAD_BASE=https://golang.google.cn/dl"}),
    ],
)
def test_build_overrides_only_configured_sources(tmp_path: Path, build_settings, expected_args):
    command = build_command(
        repo_root=tmp_path, image_ref="aico-link:t", version="t", commit="c" * 40,
        settings={"build": build_settings, "pass_host_proxy": False}, environ={"HTTP_PROXY": "x"},
    )
    args = {command[i + 1] for i, arg in enumerate(command) if arg == "--build-arg"}
    assert args == {"AICO_VERSION=t", f"AICO_COMMIT={'c' * 40}"} | expected_args


# -- host-process bridges ----------------------------------------------------------


@pytest.mark.parametrize(
    ("bridge_type", "config", "os_name", "bind_host", "expected"),
    [
        ("desktop", {"confirm_control": True}, "darwin", "127.0.0.1",
         {"python": "venv", "module": "desktop.scripts.start_bridge", "bind": "127.0.0.1", "route_flag": None,
          "device": "macOS", "preset": None}),
        # Linux: containers reach the host at the Docker gateway, a private non-loopback address.
        ("desktop", {"confirm_control": True, "screenshot_command": "scrot -o"}, "linux", "172.17.0.1",
         {"python": "venv", "module": "desktop.scripts.start_bridge", "bind": "172.17.0.1", "route_flag": None,
          "device": "linux", "preset": None}),
        ("iphone_mirroring", {}, "darwin", "127.0.0.1",
         {"python": "launcher", "module": "iphone_mirroring", "bind": "127.0.0.1", "route_flag": "--task-id",
          "device": "iOS", "preset": "iphone_mirroring"}),
        ("ios_device", {"udid": "00008110-001A"}, "darwin", "127.0.0.1",
         {"python": "venv", "module": "ios_device", "bind": "127.0.0.1", "route_flag": "--task-id",
          "device": "iOS", "preset": "ios_device"}),
        ("vphone", {"socket": "/vm/vphone.sock", "guest_ssh_host": "192.168.64.5"}, "darwin", "127.0.0.1",
         {"python": "launcher", "module": "vphone.scripts.start_bridge", "bind": "127.0.0.1",
          "route_flag": "--benchmark-task-id", "device": "iOS", "preset": None}),
    ],
)
def test_host_bridges_start_with_route_python_and_bind_host(
    app: Launcher, fake: FakeSystem, bridge_type, config, os_name, bind_host, expected
):
    fake.os = os_name
    fake.bind_host = bind_host
    fake.health = {"status": "ok", "platform": {"macOS": "mac", "linux": "linux", "iOS": "ios"}[expected["device"]]}
    created = app.instances.create(body("Device", bridge_type, config))
    app.instances.start(created["id"])
    instance = wait_settled(app, created["id"])
    assert instance["status"] == "running", instance["error"]

    (argv,) = fake.started
    if expected["python"] == "venv":
        assert argv[0] == str(app.state_dir / "pyenv" / bridge_type / "bin" / "python")
        assert any(call[1:3] == ["pip", "install"] for call in fake.calls), "missing dependencies are installed"
    else:
        assert argv[0] == sys.executable
    assert argv[1:4] == ["-u", "-m", expected["module"]]
    port = argv[argv.index("--bridge-port" if bridge_type == "desktop" else "--port") + 1]
    assert fake.health_urls[-1] == f"http://{expected['bind']}:{port}"
    route = f"launcher-{created['id']}"
    if expected["route_flag"]:
        assert argv[argv.index(expected["route_flag"]) + 1] == route
    if bridge_type == "desktop":
        assert argv[argv.index("--bridge-host") + 1] == expected["bind"]
        assert ("--allow-private-bind" in argv) == (bind_host != "127.0.0.1")
    env = env_of(docker_run(fake))
    assert env["AICO_BENCHMARK_TASK_ID"] == route
    assert env["AICO_ENVIRONMENT_BRIDGE_ENDPOINT"] == f"http://host.docker.internal:{port}"
    assert env["AICO_DEVICE_TYPE"] == expected["device"]
    assert env.get("AICO_QUICK_ACTIONS_PRESET") == expected["preset"]

    # Bridges whose route lease lapses after 30 idle minutes get it claimed again.
    app.instances.renew_routes()
    renews = bridge_type in {"desktop", "iphone_mirroring", "ios_device"}
    expected_posts = [(f"http://{expected['bind']}:{port}/api/setup", {"benchmark-task-id": route})]
    assert fake.posts == (expected_posts if renews else [])


def test_required_checkbox_must_be_confirmed(app: Launcher, fake: FakeSystem):
    with pytest.raises(validation.ValidationError, match="Confirm"):
        app.instances.create(body("Desk", "desktop", {"confirm_control": False}))
    created = app.instances.create(body("Desk", "desktop", {"confirm_control": True}))
    # A record saved without the confirmation (e.g. by an older launcher) does not start.
    app.store.patch_instance(created["id"], bridge={"type": "desktop", "config": {"confirm_control": False}})
    with pytest.raises(validation.ValidationError, match="Confirm"):
        app.instances.start(created["id"])
    assert not fake.started


@pytest.mark.parametrize(
    ("os_name", "xcode", "missing", "expected"),
    [
        ("darwin", True, {"adb"}, {"desktop", "iphone_mirroring", "ios_device", "vphone", "adb_android"}),
        ("darwin", False, set(), {"desktop", "ios_device", "vphone", "adb_android"}),
        ("linux", True, set(), {"desktop", "adb_android"}),
        ("win32", True, set(), {"adb_android"}),
    ],
)
def test_availability_comes_from_cached_host_facts(app: Launcher, fake: FakeSystem, os_name, xcode, missing, expected):
    fake.os, fake.xcode, fake.missing_tools = os_name, xcode, missing
    host_types = {"desktop", "iphone_mirroring", "ios_device", "vphone", "adb_android"}
    for _ in range(2):
        specs = {spec["type"]: spec for spec in app.state()["bridge_types"]}
    assert {name for name in host_types if specs[name]["available"]} == expected
    assert all(specs[name]["unavailable_reason"] for name in host_types - expected)
    # Building /api/state never installs or probes, and asks xcrun at most once.
    assert not any(call[0].endswith("uv") or "-m" in call for call in fake.calls)
    assert sum(call[0] == "xcrun" for call in fake.calls) <= 1


def test_bridge_action_endpoint(server, app: Launcher, fake: FakeSystem):
    port, token = server
    path = "/api/bridge-types/{}/actions/{}"
    response, _ = request(port, "POST", path.format("desktop", "install_dependencies"), payload={"config": {}})
    assert response.status == 401
    response, _ = request(port, "POST", path.format("desktop", "install_dependencies"), payload={"config": {}},
                          headers={**auth(port, token), "Origin": "http://evil.example"})
    assert response.status == 403
    for type_, name in (("nope", "install_dependencies"), ("desktop", "nope"), ("vphone", "install_dependencies")):
        response, _ = request(port, "POST", path.format(type_, name), payload={"config": {}}, headers=auth(port, token))
        assert response.status == 404
    response, content = request(port, "POST", path.format("ios_device", "install_dependencies"),
                                payload={"config": {"udid": "-rf"}}, headers=auth(port, token))
    assert response.status == 400 and "UDID" in json.loads(content)["error"]

    fake.missing_tools = {"uv"}
    response, content = request(port, "POST", path.format("desktop", "install_dependencies"),
                                payload={"config": {}}, headers=auth(port, token))
    assert response.status == 200
    result = json.loads(content)
    assert result["ok"] is False and "Install uv" in result["message"]

    fake.missing_tools = set()
    messages = []
    for _ in range(2):
        response, content = request(port, "POST", path.format("desktop", "install_dependencies"),
                                    payload={"config": {}}, headers=auth(port, token))
        result = json.loads(content)
        assert response.status == 200 and result["ok"] is True
        assert set(result) == {"ok", "message", "checks", "output"}
        messages.append(result["message"])
    assert messages == ["Dependencies installed.", "Dependencies are already installed."]


@pytest.mark.parametrize(
    ("python_exists", "importable", "uv", "expected_steps"),
    [
        (True, True, True, []),  # reuse: no uv call at all
        (True, False, True, ["pip"]),  # incomplete env: install into it
        (False, False, True, ["venv", "pip"]),
        (False, False, False, "uv"),
    ],
)
def test_pyenv_reuses_or_installs(tmp_path: Path, fake: FakeSystem, python_exists, importable, uv, expected_steps):
    env = pyenv.PythonEnv("demo", ("pillow>=10.2.0", "pymobiledevice3==11.52.1"))
    if python_exists:
        env.python(tmp_path).parent.mkdir(parents=True)
        env.python(tmp_path).touch()
    fake.deps_installed = importable
    if not uv:
        fake.missing_tools = {"uv"}
    steps: list[str] = []

    def run(args):
        steps.append(args[1])
        return fake.run_logged(args, tmp_path / "log"), ""

    if expected_steps == "uv":
        with pytest.raises(pyenv.PyEnvError, match="Install uv"):
            pyenv.ensure(fake, env, tmp_path, run)
        return
    python, installed = pyenv.ensure(fake, env, tmp_path, run)
    assert python == str(tmp_path / "pyenv" / "demo" / "bin" / "python")
    assert steps == expected_steps and installed == bool(expected_steps)


def test_pyenv_import_probe_checks_presence_pins_and_minimums():
    import importlib.metadata

    pinned = importlib.metadata.version("pytest")
    major = int(pinned.split(".")[0])

    def probe(*requirements: str) -> int:
        code = pyenv.PythonEnv("x", requirements).probe_code()
        return subprocess.run([sys.executable, "-c", code], capture_output=True).returncode

    assert probe("pytest>=1", f"pytest=={pinned}", f"pytest>={pinned}") == 0
    assert probe("pytest==0.0.1") != 0
    assert probe(f"pytest>={major + 1}.0.0") != 0  # installed but older than required: not reused
    assert probe("surely-not-installed-package") != 0
    with pytest.raises(ValueError, match="unsupported"):
        pyenv.PythonEnv("x", ("pytest~=8.0",))


LEASES = """{
\tname=iPhone
\tip_address=192.168.64.3
\tlease=0x69000001
}
{
\tname=Linux
\tip_address=192.168.64.9
\tlease=0x6a000000
}
{
\tip_address=192.168.64.7
\tname=iPhone
\tlease=0x69000005
}
"""


def test_vphone_start_passes_the_values_the_launcher_resolved(
    app: Launcher, fake: FakeSystem, tmp_path: Path, monkeypatch: pytest.MonkeyPatch
):
    env_file = tmp_path / "vphone.env"
    env_file.write_text("VPHONE_SOCKET=/vm/from-file.sock\nVPHONE_GUEST_SSH_PORT=2222\nVPHONE_GUEST_SSH_USER=mobile\n")
    # The bridge would prefer these over the env file unless the launcher passes its values.
    monkeypatch.setenv("VPHONE_ENV_FILE", str(tmp_path / "other.env"))
    monkeypatch.setenv("VPHONE_SOCKET", "/vm/other.sock")
    fake.health = {"status": "ok", "platform": "ios"}
    # Blank port and user (the form default) fall back to the env file.
    config = {"env_file": str(env_file), "guest_ssh_host": "192.168.64.5", "guest_ssh_port": "", "guest_ssh_user": ""}
    created = app.instances.create(body("VM", "vphone", config))
    app.instances.start(created["id"])
    assert wait_settled(app, created["id"])["status"] == "running"

    (argv,) = fake.started
    flags = {argv[i]: argv[i + 1] for i in range(len(argv) - 1) if argv[i].startswith("--")}
    assert flags["--socket"] == "/vm/from-file.sock"
    assert flags["--env-file"] == str(env_file)
    assert (flags["--guest-ssh-port"], flags["--guest-ssh-user"]) == ("2222", "mobile")


def test_pyenv_wait_for_another_install_can_be_cancelled(tmp_path: Path, fake: FakeSystem):
    env = pyenv.PythonEnv("busy", ("pillow",))
    lock = pyenv._lock(env.directory(tmp_path))
    with lock:  # an Install dependencies action holds the environment
        with pytest.raises(pyenv.PyEnvCancelled):
            pyenv.ensure(fake, env, tmp_path, lambda args: (0, ""), should_stop=lambda: True)


def test_vphone_guest_ip_is_newest_reachable_iphone_lease(tmp_path: Path, fake: FakeSystem):
    leases = tmp_path / "dhcpd_leases"
    leases.write_text(LEASES)
    assert parse_dhcp_leases(LEASES) == ["192.168.64.7", "192.168.64.3"]

    assert detect_guest_ip(fake, 22222, leases)[0] == ""
    fake.reachable = {("192.168.64.3", 22222)}  # the newest lease is stale
    assert detect_guest_ip(fake, 22222, leases)[0] == "192.168.64.3"
    assert detect_guest_ip(fake, 22222, tmp_path / "missing")[0] == ""


# -- store, net, and pyenv review fixes ---------------------------------------


def test_store_keeps_state_unchanged_when_saving_fails(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    store = Store(tmp_path)
    store.put_instance({"id": "a", "image": "old"})

    def fail(data):
        raise OSError("disk full")

    monkeypatch.setattr(store, "_save", fail)
    with pytest.raises(OSError):
        store.patch_instance("a", image="new")
    assert store.instance("a")["image"] == "old"


def test_state_dir_lock_admits_one_launcher(tmp_path: Path):
    code = (
        "import sys\n"
        "from launcher.store import StateDirBusy, lock_state_dir\n"
        "try:\n"
        "    lock_state_dir(sys.argv[1])\n"
        "except StateDirBusy as exc:\n"
        "    sys.exit(str(exc))\n"
    )

    def other_process() -> subprocess.CompletedProcess:
        return subprocess.run(
            [sys.executable, "-c", code, str(tmp_path)],
            capture_output=True, text=True, cwd=Path(__file__).resolve().parents[1],
        )

    handle = lock_state_dir(tmp_path)
    try:
        busy = other_process()
        assert busy.returncode != 0 and str(tmp_path) in busy.stderr and "--state-dir" in busy.stderr
    finally:
        handle.close()
    assert other_process().returncode == 0


@pytest.mark.parametrize(
    ("endpoint", "container", "host_probe"),
    [
        ("http://127.0.0.1:19090", "http://host.docker.internal:19090", "http://10.0.0.1:19090"),
        ("http://127.0.0.2:19090/x", "http://host.docker.internal:19090/x", "http://10.0.0.1:19090/x"),
        ("http://[::1]:19090", "http://host.docker.internal:19090", "http://10.0.0.1:19090"),
        ("http://user:pw@LOCALHOST:8080", "http://user:pw@host.docker.internal:8080", "http://10.0.0.1:8080"),
        ("http://host.docker.internal:19090", "http://host.docker.internal:19090", "http://10.0.0.1:19090"),
        ("http://[fe80::1]:19090", "http://[fe80::1]:19090", "http://[fe80::1]:19090"),
        ("http://192.168.1.5:19090", "http://192.168.1.5:19090", "http://192.168.1.5:19090"),
    ],
)
def test_loopback_urls_are_rewritten_for_containers_and_host_probes(endpoint, container, host_probe):
    assert container_side_url(endpoint) == container
    assert host_side_url(endpoint, "10.0.0.1") == host_probe


@pytest.mark.parametrize(
    ("proxy", "expected"),
    [
        ("127.0.0.5:7890", "host.docker.internal:7890"),
        ("http://[::1]:7890", "http://host.docker.internal:7890"),
        ("http://proxy.example:7890", "http://proxy.example:7890"),
    ],
)
def test_container_proxy_rewrites_any_loopback(proxy, expected):
    assert container_proxy(proxy) == expected


# -- lifecycle: device claims, failed inspections, and failed builds ------------------


def test_two_starts_cannot_reserve_the_same_automatically_selected_iphone(app: Launcher, fake: FakeSystem):
    fake.usbmux = json.dumps([{"UniqueDeviceID": "00008110-001A", "ConnectionType": "USB"}])
    # Both workers list the one connected iPhone before either reserves it.
    listed = threading.Barrier(2, timeout=5)
    run = fake.run

    def run_in_step(args, **kwargs):
        result = run(args, **kwargs)
        if [str(arg) for arg in args][2:5] == ["pymobiledevice3", "usbmux", "list"]:
            listed.wait()
        return result

    fake.run = run_in_step
    patch_instance = app.store.patch_instance

    def slow_reservation(instance_id, **changes):
        if "device_claim" in (changes.get("runtime") or {}):
            time.sleep(0.2)  # widen the gap between the ownership check and the reservation
        return patch_instance(instance_id, **changes)

    app.store.patch_instance = slow_reservation
    ids = [app.instances.create(body(name, "ios_device"))["id"] for name in ("One", "Two")]
    for instance_id in ids:
        app.instances.start(instance_id)
    results = [wait_settled(app, instance_id) for instance_id in ids]

    assert sorted(result["status"] for result in results) == ["error", "running"]
    assert "already used" in next(result["error"] for result in results if result["status"] == "error")
    assert sum("ios_device" in " ".join(args) for args in fake.started) == 1


@pytest.mark.parametrize("edit", [
    body("One", "none"),
    body("One", "adb_android", {"serial": "emulator-5556"}),
])
def test_an_errored_instance_keeps_its_device_and_bridge_until_its_bridge_stops(
    app: Launcher, fake: FakeSystem, edit: dict
):
    first = app.instances.create(body("One", "adb_android", {"serial": "emulator-5554"}))
    second = app.instances.create(body("Two", "adb_android", {"serial": "emulator-5554"}))
    app.instances.start(first["id"])
    assert wait_settled(app, first["id"])["status"] == "running"
    # The Agent exits; the ADB bridge process keeps running.
    fake.containers.pop(agent_container(first["id"]))
    app.instances.check_running(force=True)
    assert app.instances.get(first["id"])["status"] == "error"

    with pytest.raises(Conflict, match="already used"):
        app.instances.start(second["id"])
    with pytest.raises(Conflict, match="still running"):
        app.instances.update(first["id"], edit)
    with pytest.raises(Conflict, match="already used"):
        app.instances.start(second["id"])

    app.instances.stop(first["id"])
    assert wait_settled(app, first["id"])["status"] == "stopped"
    assert not fake.pids
    app.instances.update(first["id"], edit)
    app.instances.start(second["id"])
    assert wait_settled(app, second["id"])["status"] == "running"


def test_a_build_whose_log_cannot_be_written_still_ends_failed(app: Launcher, tmp_path: Path):
    app.images.builds_dir = tmp_path / "not-a-directory"
    app.images.builds_dir.write_text("")
    build = app.images.start_build("t1")
    deadline = time.monotonic() + 10
    while app.store.build(build["id"])["status"] == "running":
        assert time.monotonic() < deadline, "the build never finished"
        time.sleep(0.01)
    assert app.store.build(build["id"])["status"] == "failed"
    app.images.start_build("t2")  # no BuildConflict from the failed build
