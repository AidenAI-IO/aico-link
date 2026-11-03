import json
import os
from pathlib import Path
import signal
import subprocess
import tempfile
import threading
import time
import unittest
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer


REPO_ROOT = Path(__file__).resolve().parents[2]


def read_repo_file(relative_path: str) -> str:
    return (REPO_ROOT / relative_path).read_text(encoding="utf-8")


class DockerSandboxContractTest(unittest.TestCase):
    def test_root_compose_exposes_both_web_apps_and_persists_runtime_data(self):
        compose = read_repo_file("compose.yml")

        self.assertIn("docker/dev/Dockerfile", compose)
        self.assertIn('"127.0.0.1:${AICO_CONFIG_WEB_PORT:-8000}:80"', compose)
        self.assertIn('"127.0.0.1:${AICO_AGENT_WEB_PORT:-8080}:8080"', compose)
        self.assertIn("aico-data:/userdata", compose)
        self.assertIn("stop_grace_period: 20s", compose)
        self.assertIn("http://127.0.0.1:8080/api/tools", compose)
        self.assertIn("AICO_ENVIRONMENT_BRIDGE_ENDPOINT", compose)
        self.assertIn(
            "ENVIRONMENT_BRIDGE_ENDPOINT: ${ENVIRONMENT_BRIDGE_ENDPOINT:-}",
            compose,
        )
        self.assertIn(
            "AICO_BENCHMARK_TASK_ID: ${AICO_BENCHMARK_TASK_ID:-docker-sandbox}",
            compose,
        )
        self.assertIn(
            "AICO_BRIDGE_EPISODE_ID: ${AICO_BRIDGE_EPISODE_ID:-docker-sandbox}",
            compose,
        )
        self.assertIn("AICO_DEVICE_TYPE: ${AICO_DEVICE_TYPE:-}", compose)
        self.assertIn(
            "AICO_PUBLIC_AGENT_WEB_PORT: ${AICO_AGENT_WEB_PORT:-8080}",
            compose,
        )
        self.assertIn("host.docker.internal:host-gateway", compose)

    def test_smoke_uses_canonical_config_web_routes(self):
        smoke_script = read_repo_file("scripts/test_docker_sandbox.sh")

        self.assertIn('/api/device/status', smoke_script)
        self.assertIn('/api/system/environment', smoke_script)
        self.assertNotIn('/api/system/env"', smoke_script)

    def test_sandbox_image_builds_real_agent_with_config_web_subcommand(self):
        dockerfile = read_repo_file("docker/dev/Dockerfile")

        self.assertIn("go build", dockerfile)
        self.assertIn("./cmd/daemon", dockerfile)
        self.assertNotIn("src/config_web.cpp", dockerfile)
        self.assertIn("src/config_web/web/ /usr/share/aico/config-web/", dockerfile)
        self.assertIn("config-web", read_repo_file("docker/dev/entrypoint.sh"))
        self.assertIn("ttyd-builder", dockerfile)
        self.assertIn('ttyd_version="1.7.3"', dockerfile)
        self.assertNotIn("ARG TTYD_VERSION", dockerfile)
        self.assertNotIn("node:16-bookworm", dockerfile)

    def test_runtime_leaves_voice_unconfigured_without_credentials(self):
        config = read_repo_file("docker/dev/agent.toml")

        # The sandbox configures no STT/TTS provider, and both stt and realtime
        # require their own. An unset input_mode is valid and keeps the agent
        # reachable through the Web UI and HTTP API.
        self.assertNotIn("input_mode =", config)
        self.assertIn('device_type = "iOS"', config)
        self.assertNotIn("api_key", config)

    def test_start_target_has_health_checked_startup(self):
        start_script = read_repo_file("scripts/start_docker_sandbox.sh")
        makefile = read_repo_file("Makefile")

        self.assertIn("docker compose", start_script)
        self.assertIn("--wait", start_script)
        self.assertIn("--wait-timeout", start_script)
        self.assertIn("docker compose up --help", start_script)
        self.assertIn("sandbox-start:", makefile)
        self.assertIn("./scripts/start_docker_sandbox.sh", makefile)
        self.assertNotIn("down -v", start_script)

    def test_starting_the_sandbox_always_rebuilds_the_image(self):
        """The started sandbox must run the working tree, never a stale image."""

        with tempfile.TemporaryDirectory() as temporary_directory:
            temporary_path = Path(temporary_directory)
            invocations = temporary_path / "docker-invocations"
            docker = temporary_path / "docker"
            docker.write_text(
                "#!/bin/sh\n"
                'if [ "$1" = compose ] && [ "$2" = up ] && [ "$3" = --help ]; then\n'
                "    printf '  --build\\n  --wait\\n  --wait-timeout duration\\n'\n"
                "    exit 0\n"
                "fi\n"
                'printf \'%s\\n\' "$*" >>"$AICO_FAKE_DOCKER_LOG"\n'
                "exit 0\n",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            environment = os.environ.copy()
            environment["PATH"] = f"{temporary_path}{os.pathsep}{environment['PATH']}"
            environment["AICO_FAKE_DOCKER_LOG"] = str(invocations)
            completed = subprocess.run(
                [str(REPO_ROOT / "scripts/start_docker_sandbox.sh")],
                env=environment,
                capture_output=True,
                text=True,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)

            up_commands = [
                line
                for line in invocations.read_text(encoding="utf-8").splitlines()
                if line.startswith("compose up")
            ]
            self.assertEqual(len(up_commands), 1, invocations.read_text())
            self.assertIn("--build", up_commands[0].split())

    def test_start_target_resolves_random_ports_before_compose(self):
        with tempfile.TemporaryDirectory() as temporary_directory:
            temporary_path = Path(temporary_directory)
            invocation = temporary_path / "docker-invocation"
            docker = temporary_path / "docker"
            docker.write_text(
                "#!/bin/sh\n"
                'if [ "$1" = compose ] && [ "$2" = version ]; then exit 0; fi\n'
                'if [ "$1" = compose ] && [ "$2" = up ] && [ "$3" = --help ]; then\n'
                "    printf '  --build\\n  --wait\\n  --wait-timeout duration\\n'\n"
                "    exit 0\n"
                "fi\n"
                'printf "%s %s\\n" "$AICO_CONFIG_WEB_PORT" "$AICO_AGENT_WEB_PORT" '
                '>>"$AICO_FAKE_DOCKER_LOG"\n'
                "exit 0\n",
                encoding="utf-8",
            )
            docker.chmod(0o755)

            environment = os.environ.copy()
            environment.update(
                {
                    "PATH": f"{temporary_path}{os.pathsep}{environment['PATH']}",
                    "AICO_FAKE_DOCKER_LOG": str(invocation),
                    "AICO_CONFIG_WEB_PORT": "0",
                    "AICO_AGENT_WEB_PORT": "0",
                }
            )
            completed = subprocess.run(
                [str(REPO_ROOT / "scripts/start_docker_sandbox.sh")],
                env=environment,
                capture_output=True,
                text=True,
            )
            self.assertEqual(completed.returncode, 0, completed.stderr)
            config_port, agent_port = map(int, invocation.read_text().split())
            self.assertNotEqual(config_port, 0)
            self.assertNotEqual(agent_port, 0)
            self.assertNotEqual(config_port, agent_port)

    def test_try_aico_on_pc_is_the_hardware_free_sandbox_entrypoint(self):
        guide = read_repo_file("docs/01-getting-started/try-aico-on-pc.md")
        readme = read_repo_file("README.md")

        self.assertIn("docker compose up --build", guide)
        self.assertIn("make sandbox-start", guide)
        self.assertNotIn("make sandbox-update", guide)
        self.assertIn(
            "AICO_BRIDGE_EPISODE_ID=my-sandbox-session", guide
        )
        self.assertIn(
            "https://docs.astral.sh/uv/getting-started/installation/", guide
        )
        self.assertIn(
            "https://developer.android.com/tools/releases/platform-tools", guide
        )
        self.assertNotIn("python -m runner webui", guide)
        self.assertNotIn("docker-sandbox.md", readme)
        self.assertFalse(
            (REPO_ROOT / "docs/01-getting-started/docker-sandbox.md").exists()
        )

    def test_docker_smoke_requests_and_ci_step_have_finite_timeouts(self):
        smoke_script = read_repo_file("scripts/test_docker_sandbox.sh")
        workflow = read_repo_file(".github/workflows/ci.yml")

        # Asserted requests go through the request() helper, so curl is no
        # longer the first word on its line. Anchor on the option instead of
        # the command position: what matters is that every request the script
        # makes still carries a finite --max-time.
        request_lines = [
            line for line in smoke_script.splitlines() if "-fsS" in line
        ]
        self.assertGreater(len(request_lines), 0)
        for line in request_lines:
            self.assertIn("--max-time", line)

        docker_step = workflow.split(
            "- name: Verify Docker sandbox contract", 1
        )[1].split("- name:", 1)[0]
        self.assertIn("timeout-minutes: 25", docker_step)

    def test_docker_smoke_resolves_random_host_ports_before_startup(self):
        smoke_script = read_repo_file("scripts/test_docker_sandbox.sh")

        selection = (
            'python3 "$script_dir/select_docker_web_ports.py" '
            '"$config_port" "$agent_port"'
        )
        first_start = "compose up -d --build"
        self.assertIn(selection, smoke_script)
        self.assertLess(smoke_script.index(selection), smoke_script.index(first_start))
        self.assertNotIn("published_port()", smoke_script)

        completed = subprocess.run(
            [
                "python3",
                str(REPO_ROOT / "scripts/select_docker_web_ports.py"),
                "0",
                "0",
            ],
            check=True,
            capture_output=True,
            text=True,
        )
        config_port, agent_port = map(int, completed.stdout.split())
        self.assertNotEqual(config_port, 0)
        self.assertNotEqual(agent_port, 0)
        self.assertNotEqual(config_port, agent_port)

    def test_entrypoint_exits_when_either_web_process_stops(self):
        entrypoint = read_repo_file("docker/dev/entrypoint.sh")

        self.assertTrue(entrypoint.startswith("#!/bin/bash\n"))
        self.assertIn(
            'wait -n "$config_web_pid" "$ttyd_pid"',
            entrypoint,
        )

    def test_entrypoint_configures_ttyd_options(self):
        entrypoint = read_repo_file("docker/dev/entrypoint.sh")

        # ttyd 1.7.3 exposes --readonly; writable mode is the default and
        # passing --writable makes the daemon fail option parsing.
        self.assertNotIn("--writable", entrypoint)
        self.assertIn('--max-clients="${TTYD_MAX_CLIENTS:-2}"', entrypoint)
        self.assertIn('rendererType=${TTYD_RENDERER:-canvas}', entrypoint)
        self.assertIn('fontSize=${TTYD_FONT_SIZE:-24}', entrypoint)
        self.assertIn('scrollback=${TTYD_SCROLLBACK:-500}', entrypoint)
        self.assertIn('cursorStyle=${TTYD_CURSOR_STYLE:-bar}', entrypoint)
        self.assertIn(
            'disableResizeOverlay=${TTYD_DISABLE_RESIZE_OVERLAY:-true}',
            entrypoint,
        )

    def test_agent_supervisor_truncates_the_persistent_log(self):
        service = REPO_ROOT / "docker/dev/agent-service.sh"

        with tempfile.TemporaryDirectory() as temporary_directory:
            temporary_path = Path(temporary_directory)
            agent = temporary_path / "fake-agent.sh"
            agent.write_text(
                "#!/usr/bin/env python3\n"
                "import signal\n"
                "import sys\n"
                "import time\n"
                "signal.signal(signal.SIGINT, lambda *_: sys.exit(0))\n"
                "signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))\n"
                "sys.stdout.write('0123456789abcdef' * 512)\n"
                "sys.stdout.flush()\n"
                "time.sleep(30)\n",
                encoding="utf-8",
            )
            agent.chmod(0o755)

            agent_directory = temporary_path / "agent"
            log_file = agent_directory / "log/agent.log"
            environment = os.environ.copy()
            environment.update(
                {
                    "AICO_AGENT_BIN": str(agent),
                    "AICO_AGENT_DIR": str(agent_directory),
                    "AICO_AGENT_RUN_DIR": str(temporary_path / "run"),
                    "AICO_SYSTEM_ENV": str(temporary_path / "system-env"),
                    "AICO_ENV_RUN_BIN": str(
                        REPO_ROOT / "docker/dev/aico-env-run"
                    ),
                    "AICO_AGENT_LOG_MAX_BYTES": "1024",
                    "AICO_AGENT_LOG_RETAIN_BYTES": "256",
                    "AICO_AGENT_LOG_CHECK_INTERVAL": "1",
                }
            )
            supervisor = subprocess.Popen(
                ["sh", str(service), "run"],
                env=environment,
                stdout=subprocess.DEVNULL,
                stderr=subprocess.PIPE,
                text=True,
            )
            try:
                deadline = time.monotonic() + 5
                while time.monotonic() < deadline:
                    if log_file.exists() and "log_rotated" in log_file.read_text(
                        encoding="utf-8"
                    ):
                        break
                    time.sleep(0.1)
                else:
                    self.fail("agent supervisor did not rotate the log")

                self.assertLess(log_file.stat().st_size, 1024)
            finally:
                supervisor.terminate()
                try:
                    supervisor.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    supervisor.kill()
                    supervisor.wait(timeout=5)

            stderr = supervisor.stderr.read()
            supervisor.stderr.close()
            self.assertEqual(supervisor.returncode, 0, stderr)

    def test_bridge_session_outlives_agent_and_supervisor_restarts(self):
        service = REPO_ROOT / "docker/dev/agent-service.sh"

        class BridgeServer:
            def __init__(
                self,
                block_setup=False,
                bridge_type="mobilegym",
                fail_setup_once=False,
            ):
                self.requests = []
                self.routes = {}
                self.reset_count = 0
                self.setup_entries = {}
                self.lock = threading.Lock()
                self.fail_setup_once = fail_setup_once
                self.setup_gate = threading.Event()
                if not block_setup:
                    self.setup_gate.set()
                requests = self.requests
                routes = self.routes
                setup_entries = self.setup_entries
                state_lock = self.lock
                setup_gate = self.setup_gate
                bridge = self

                class Handler(BaseHTTPRequestHandler):
                    def log_message(self, *_args):
                        return

                    def reply(self, payload=b'{"ok":true}', status=200):
                        self.send_response(status)
                        self.send_header("Content-Type", "application/json")
                        self.send_header("Content-Length", str(len(payload)))
                        self.end_headers()
                        try:
                            self.wfile.write(payload)
                        except (BrokenPipeError, ConnectionResetError):
                            pass

                    def do_GET(self):
                        with state_lock:
                            data = {"bridge_type": bridge_type}
                            if bridge_type == "mobilegym":
                                data["active_routes"] = dict(routes)
                            else:
                                data["active_task_id"] = next(iter(routes), "")
                        payload = json.dumps({"ok": True, "data": data}).encode()
                        with state_lock:
                            requests.append(
                                ("GET", self.path, "", payload.decode("utf-8"))
                            )
                        self.reply(payload)

                    def do_POST(self):
                        size = int(self.headers.get("Content-Length", "0"))
                        body = self.rfile.read(size).decode()
                        task_id = self.headers.get("benchmark-task-id", "")
                        with state_lock:
                            requests.append(("POST", self.path, task_id, body))
                        if self.path == "/api/setup":
                            setup_token = str(
                                (json.loads(body) if body else {}).get(
                                    "setup_token", ""
                                )
                            )
                            entry_key = (task_id, setup_token)
                            if setup_token:
                                with state_lock:
                                    entry = setup_entries.get(entry_key)
                                    if entry is None:
                                        entry = {
                                            "completed": threading.Event(),
                                            "status": None,
                                        }
                                        setup_entries[entry_key] = entry
                                        owns_setup = True
                                    else:
                                        owns_setup = False
                            else:
                                entry = None
                                owns_setup = True
                            if owns_setup:
                                with state_lock:
                                    routes[task_id] = 0
                                setup_gate.wait(timeout=10)
                                with state_lock:
                                    if bridge.fail_setup_once:
                                        bridge.fail_setup_once = False
                                        setup_status = 500
                                        if setup_token:
                                            setup_entries.pop(entry_key, None)
                                    else:
                                        bridge.reset_count += 1
                                        setup_status = 200
                                if entry is not None:
                                    entry["status"] = setup_status
                                    entry["completed"].set()
                            else:
                                entry["completed"].wait(timeout=10)
                                setup_status = entry["status"] or 500
                            if setup_status != 200:
                                self.reply(b'{"ok":false}', status=setup_status)
                                return
                        elif self.path == "/api/release":
                            with state_lock:
                                routes.pop(task_id, None)
                                for key, entry in list(setup_entries.items()):
                                    if key[0] == task_id and entry[
                                        "completed"
                                    ].is_set():
                                        setup_entries.pop(key, None)
                        self.reply()

                self.server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
                self.thread = threading.Thread(
                    target=self.server.serve_forever, daemon=True
                )
                self.thread.start()

            @property
            def endpoint(self):
                return f"http://127.0.0.1:{self.server.server_port}"

            def close(self):
                self.setup_gate.set()
                self.server.shutdown()
                self.server.server_close()
                self.thread.join(timeout=5)

            def lose_routes(self):
                with self.lock:
                    self.routes.clear()

            def allow_setup(self):
                self.setup_gate.set()

            def request_snapshot(self):
                with self.lock:
                    return list(self.requests)

            def reset_count_snapshot(self):
                with self.lock:
                    return self.reset_count

        def wait_until(predicate, message):
            deadline = time.monotonic() + 8
            while time.monotonic() < deadline:
                if predicate():
                    return
                time.sleep(0.05)
            self.fail(message)

        with tempfile.TemporaryDirectory() as temporary_directory:
            temporary_path = Path(temporary_directory)
            agent = temporary_path / "fake-agent.sh"
            agent.write_text(
                "#!/bin/sh\n"
                'printf "started\\n" >> "$AICO_FAKE_AGENT_STARTS"\n'
                'if [ "${AICO_FAKE_AGENT_CRASH:-0}" = 1 ]; then exit 7; fi\n'
                "trap 'exit 0' INT TERM\n"
                "while :; do sleep 1; done\n",
                encoding="utf-8",
            )
            agent.chmod(0o755)
            fake_bin = temporary_path / "bin"
            fake_bin.mkdir()
            fake_flock = fake_bin / "flock"
            fake_flock.write_text("#!/bin/sh\nexit 0\n", encoding="utf-8")
            fake_flock.chmod(0o755)

            run_directory = temporary_path / "run"
            agent_directory = temporary_path / "agent"
            starts_file = temporary_path / "agent-starts"
            system_env = temporary_path / "system-env"
            system_env.write_text("", encoding="utf-8")
            base_environment = os.environ.copy()
            base_environment.update(
                {
                    "PATH": f"{fake_bin}:{base_environment['PATH']}",
                    "AICO_AGENT_BIN": str(agent),
                    "AICO_AGENT_DIR": str(agent_directory),
                    "AICO_AGENT_RUN_DIR": str(run_directory),
                    "AICO_SYSTEM_ENV": str(system_env),
                    "AICO_ENV_RUN_BIN": str(
                        REPO_ROOT / "docker/dev/aico-env-run"
                    ),
                    "AICO_AGENT_LOG_CHECK_INTERVAL": "1",
                    "AICO_AGENT_STOP_ATTEMPTS": "1",
                    "AICO_BRIDGE_WAIT_ATTEMPTS": "1",
                    "AICO_ENVIRONMENT_BRIDGE_MODE": "true",
                    "AICO_FAKE_AGENT_STARTS": str(starts_file),
                }
            )
            first_bridge = BridgeServer(fail_setup_once=True)
            second_bridge = BridgeServer(
                block_setup=True, bridge_type="adb_android"
            )
            supervisors = []

            def environment(endpoint, task_id, episode_id, crash=False):
                result = base_environment.copy()
                result.update(
                    {
                        "AICO_ENVIRONMENT_BRIDGE_ENDPOINT": endpoint,
                        "AICO_BENCHMARK_TASK_ID": task_id,
                        "AICO_BRIDGE_EPISODE_ID": episode_id,
                        "AICO_FAKE_AGENT_CRASH": "1" if crash else "0",
                    }
                )
                return result

            def start_supervisor(current_environment):
                supervisor = subprocess.Popen(
                    ["sh", str(service), "run"],
                    env=current_environment,
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.PIPE,
                    text=True,
                    start_new_session=True,
                )
                supervisors.append(supervisor)
                return supervisor

            def stop_supervisor(supervisor):
                if supervisor.poll() is None:
                    try:
                        os.killpg(supervisor.pid, signal.SIGTERM)
                    except ProcessLookupError:
                        pass
                    try:
                        supervisor.wait(timeout=5)
                    except subprocess.TimeoutExpired:
                        try:
                            os.killpg(supervisor.pid, signal.SIGKILL)
                        except ProcessLookupError:
                            pass
                        supervisor.wait(timeout=5)
                stderr = supervisor.stderr.read()
                supervisor.stderr.close()
                self.assertEqual(supervisor.returncode, 0, stderr)

            def setup_requests(bridge):
                return [
                    request
                    for request in bridge.request_snapshot()
                    if request[1] == "/api/setup"
                ]

            try:
                first_environment = environment(
                    first_bridge.endpoint, "task-one", "episode-one", crash=True
                )
                first_supervisor = start_supervisor(first_environment)
                wait_until(
                    lambda: starts_file.exists()
                    and len(starts_file.read_text(encoding="utf-8").splitlines()) >= 2,
                    "watchdog did not restart the fake Agent",
                )
                self.assertEqual(len(setup_requests(first_bridge)), 2)
                first_setup_token = json.loads(
                    setup_requests(first_bridge)[0][3]
                ).get("setup_token")
                self.assertTrue(first_setup_token)
                self.assertEqual(
                    json.loads(setup_requests(first_bridge)[1][3]).get(
                        "setup_token"
                    ),
                    first_setup_token,
                )
                self.assertEqual(first_bridge.reset_count_snapshot(), 1)
                stop_supervisor(first_supervisor)

                first_environment["AICO_FAKE_AGENT_CRASH"] = "0"
                second_supervisor = start_supervisor(first_environment)
                wait_until(
                    lambda: len(starts_file.read_text(encoding="utf-8").splitlines()) >= 3,
                    "replacement supervisor did not start the fake Agent",
                )
                self.assertEqual(len(setup_requests(first_bridge)), 2)
                stop_supervisor(second_supervisor)

                first_bridge.lose_routes()
                starts_before_route_recovery = len(
                    starts_file.read_text(encoding="utf-8").splitlines()
                )
                route_recovery_supervisor = start_supervisor(first_environment)
                wait_until(
                    lambda: len(setup_requests(first_bridge)) == 3,
                    "lost remote bridge route was not set up again",
                )
                self.assertNotEqual(
                    json.loads(setup_requests(first_bridge)[2][3]).get(
                        "setup_token"
                    ),
                    first_setup_token,
                )
                wait_until(
                    lambda: len(starts_file.read_text(encoding="utf-8").splitlines())
                    > starts_before_route_recovery,
                    "recovered bridge route did not reach the fake Agent",
                )
                stop_supervisor(route_recovery_supervisor)

                episode_environment = environment(
                    first_bridge.endpoint, "task-one", "episode-two"
                )
                starts_before_episode_switch = len(
                    starts_file.read_text(encoding="utf-8").splitlines()
                )
                third_supervisor = start_supervisor(episode_environment)
                wait_until(
                    lambda: len(setup_requests(first_bridge)) == 4,
                    "new bridge episode was not set up",
                )
                wait_until(
                    lambda: len(starts_file.read_text(encoding="utf-8").splitlines())
                    > starts_before_episode_switch,
                    "new bridge episode did not reach the fake Agent",
                )
                wait_until(
                    lambda: any(
                        request[1:3] == ("/api/release", "task-one")
                        for request in first_bridge.request_snapshot()
                    ),
                    "old bridge episode was not released",
                )
                self.assertIn("episode-two", setup_requests(first_bridge)[-1][3])
                episode_setup_token = json.loads(
                    setup_requests(first_bridge)[-1][3]
                ).get("setup_token")
                self.assertTrue(episode_setup_token)
                self.assertNotEqual(episode_setup_token, first_setup_token)
                stop_supervisor(third_supervisor)

                task_environment = environment(
                    first_bridge.endpoint, "task-two", "episode-two"
                )
                starts_before_task_switch = len(
                    starts_file.read_text(encoding="utf-8").splitlines()
                )
                fourth_supervisor = start_supervisor(task_environment)
                wait_until(
                    lambda: len(setup_requests(first_bridge)) == 5,
                    "new bridge task was not set up",
                )
                wait_until(
                    lambda: len(starts_file.read_text(encoding="utf-8").splitlines())
                    > starts_before_task_switch,
                    "new bridge task did not reach the fake Agent",
                )
                self.assertEqual(setup_requests(first_bridge)[-1][2], "task-two")
                task_setup_token = json.loads(
                    setup_requests(first_bridge)[-1][3]
                ).get("setup_token")
                self.assertTrue(task_setup_token)
                self.assertNotEqual(task_setup_token, episode_setup_token)
                stop_supervisor(fourth_supervisor)

                second_environment = environment(
                    second_bridge.endpoint, "task-two", "episode-two"
                )
                fifth_supervisor = start_supervisor(second_environment)
                wait_until(
                    lambda: len(setup_requests(second_bridge)) == 1,
                    "blocked bridge setup request was not received",
                )
                wait_until(
                    lambda: (run_directory / "bridge-session").exists(),
                    "pending bridge identity was not persisted",
                )
                os.killpg(fifth_supervisor.pid, signal.SIGTERM)
                try:
                    fifth_supervisor.wait(timeout=5)
                except subprocess.TimeoutExpired:
                    try:
                        os.killpg(fifth_supervisor.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    fifth_supervisor.wait(timeout=5)
                fifth_supervisor.stderr.close()

                starts_before_blocked_restart = len(
                    starts_file.read_text(encoding="utf-8").splitlines()
                )
                sixth_supervisor = start_supervisor(second_environment)
                wait_until(
                    lambda: len(setup_requests(second_bridge)) == 2,
                    "restart did not retry the in-flight setup token",
                )
                blocked_setup_requests = setup_requests(second_bridge)
                setup_tokens = [
                    json.loads(request[3]).get("setup_token")
                    for request in blocked_setup_requests
                ]
                self.assertTrue(setup_tokens[0])
                self.assertEqual(setup_tokens[0], setup_tokens[1])
                time.sleep(0.2)
                self.assertEqual(
                    len(starts_file.read_text(encoding="utf-8").splitlines()),
                    starts_before_blocked_restart,
                )
                second_bridge.allow_setup()
                wait_until(
                    lambda: len(starts_file.read_text(encoding="utf-8").splitlines())
                    > starts_before_blocked_restart,
                    "restart after blocked setup did not reach the fake Agent",
                )
                self.assertEqual(second_bridge.reset_count_snapshot(), 1)
                wait_until(
                    lambda: any(
                        request[1:3] == ("/api/release", "task-two")
                        for request in first_bridge.request_snapshot()
                    ),
                    "old bridge endpoint was not released",
                )

                subprocess.run(
                    ["sh", str(service), "stop"],
                    env=second_environment,
                    check=True,
                    stdout=subprocess.DEVNULL,
                    stderr=subprocess.PIPE,
                    text=True,
                )
                sixth_supervisor.wait(timeout=5)
                sixth_supervisor.stderr.close()
                self.assertTrue(
                    any(
                        request[1:3] == ("/api/release", "task-two")
                        for request in second_bridge.request_snapshot()
                    )
                )

                starts_before_fresh_start = len(
                    starts_file.read_text(encoding="utf-8").splitlines()
                )
                seventh_supervisor = start_supervisor(second_environment)
                wait_until(
                    lambda: len(setup_requests(second_bridge)) == 3,
                    "full stop did not clear the bridge session",
                )
                fresh_setup_token = json.loads(
                    setup_requests(second_bridge)[-1][3]
                ).get("setup_token")
                self.assertTrue(fresh_setup_token)
                self.assertNotEqual(fresh_setup_token, setup_tokens[0])
                wait_until(
                    lambda: len(starts_file.read_text(encoding="utf-8").splitlines())
                    > starts_before_fresh_start,
                    "fresh bridge session did not reach the fake Agent",
                )
                stop_supervisor(seventh_supervisor)
            finally:
                for supervisor in supervisors:
                    try:
                        os.killpg(supervisor.pid, signal.SIGKILL)
                    except ProcessLookupError:
                        pass
                    if supervisor.poll() is None:
                        supervisor.wait(timeout=5)
                    if not supervisor.stderr.closed:
                        supervisor.stderr.close()
                first_bridge.close()
                second_bridge.close()


if __name__ == "__main__":
    unittest.main()
