"""Aico Launcher backend: HTTP security, instance lifecycle, and helpers.

Everything runs against a fake System, so no Docker, adb, or network is used.
"""

from __future__ import annotations

import http.client
import json
import subprocess
import threading
import time
import urllib.parse
from pathlib import Path
from typing import Any

import pytest

from launcher import LABEL_INSTANCE
from launcher.bridges import device_type_from_health
from launcher.images import build_command
from launcher.instances import Conflict, agent_container, choose_ports
from launcher.server import COOKIE_NAME, Launcher, create_server
from launcher.system import ADB_BRIDGE_MODULE, System


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

    def run(self, args, *, timeout=60, cwd=None, env=None):
        args = [str(arg) for arg in args]
        self.calls.append(args)

        def done(code: int = 0, out: str = "", err: str = "") -> subprocess.CompletedProcess:
            return subprocess.CompletedProcess(args, code, out, err)

        if args[:2] == ["git", "rev-parse"]:
            return done(out="a" * 40 + "\n")
        if args[:2] == ["git", "status"]:
            return done(out="")
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
            return done(out=container["state"] + "\n") if container else done(1, err="No such object")
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
        self.calls.append([str(arg) for arg in args])
        return 0

    def which(self, name: str) -> str:
        return f"/usr/bin/{name}"

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

    def start_adb_bridge(self, *, serial, port, host, adb_path, log_path) -> int:
        pid = self.next_pid
        self.next_pid += 1
        self.pids[pid] = f"python -m {ADB_BRIDGE_MODULE} --adb-serial {serial}"
        self.events.append(f"adb-start {serial}")
        return pid

    def pid_command(self, pid: int) -> str:
        return self.pids.get(pid, "")

    def terminate(self, pid: int) -> None:
        self.events.append(f"terminate {pid}")
        self.pids.pop(pid, None)

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
    pid = app.store.instance(created["id"])["runtime"]["adb_pid"]
    fake.events.clear()

    app.instances.stop(created["id"])
    assert wait_settled(app, created["id"])["status"] == "stopped"
    container = agent_container(created["id"])
    assert fake.events == [f"stop {container}", f"rm {container}", f"terminate {pid}"]


def test_stop_reports_docker_failures_instead_of_stopped(app: Launcher, fake: FakeSystem):
    created = app.instances.create(body("Demo", "none"))
    app.instances.start(created["id"])
    assert wait_settled(app, created["id"])["status"] == "running"
    fake.failing = {"stop", "rm"}

    app.instances.stop(created["id"])
    instance = wait_settled(app, created["id"])
    assert instance["status"] == "error"
    assert "Cannot connect to the Docker daemon" in instance["error"]
    assert agent_container(created["id"]) in fake.containers


def test_stop_never_signals_a_recycled_pid(app: Launcher, fake: FakeSystem):
    created = app.instances.create(body("Phone", "adb_android", {"serial": "emulator-5554"}))
    app.instances.start(created["id"])
    wait_settled(app, created["id"])
    pid = app.store.instance(created["id"])["runtime"]["adb_pid"]
    fake.pids[pid] = "/usr/bin/some-other-process"

    app.instances.stop(created["id"])
    wait_settled(app, created["id"])
    assert f"terminate {pid}" not in fake.events


def test_same_adb_serial_cannot_run_twice(app: Launcher):
    first = app.instances.create(body("One", "adb_android", {"serial": "emulator-5554"}))
    second = app.instances.create(body("Two", "adb_android", {"serial": "emulator-5554"}))
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
