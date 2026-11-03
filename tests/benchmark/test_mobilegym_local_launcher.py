"""Unit tests for the Mac-local MobileGym launcher service."""

from __future__ import annotations

import importlib.util
import json
import os
import subprocess
import threading
import urllib.error
import urllib.request
from http.server import HTTPServer
from pathlib import Path

import pytest


REPO_ROOT = Path(__file__).resolve().parents[2]
LAUNCHER_PATH = REPO_ROOT / "benchmark" / "mobilegym" / "scripts" / "local_launcher.py"
HELPER_PATH = REPO_ROOT / "benchmark" / "mobilegym" / "scripts" / "local_launcher_helper.py"
INSTALLER_PATH = REPO_ROOT / "benchmark" / "mobilegym" / "scripts" / "install_local_launcher.sh"


@pytest.fixture
def launcher_module():
    spec = importlib.util.spec_from_file_location("local_launcher", LAUNCHER_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


@pytest.fixture
def helper_module():
    spec = importlib.util.spec_from_file_location("local_launcher_helper", HELPER_PATH)
    mod = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(mod)
    return mod


def test_scan_suites_includes_nested_aico(launcher_module, tmp_path):
    suites_dir = tmp_path / "suites" / "perception"
    suites_dir.mkdir(parents=True)
    (suites_dir / "perception_v1.json").write_text('{"tasks":[{},{}]}')

    suites = launcher_module.scan_suites(tmp_path)

    by_type = {(item["type"], item["name"]): item for item in suites}
    assert by_type[("aico", "perception_v1")]["path"].endswith("perception/perception_v1.json")
    assert by_type[("aico", "perception_v1")]["task_count"] == 2


def test_build_run_command_reports_mobilegym_runs_moved_to_webui(launcher_module, tmp_path):
    with pytest.raises(launcher_module.LauncherError, match="benchmark WebUI"):
        launcher_module.build_run_command(
            tmp_path,
            {"suite": "perception/perception_v1", "suite_type": "aico", "parallel": 3, "limit": 1},
        )


def test_build_run_command_reports_mobilegym_runs_moved_to_webui_with_analysis_payload(launcher_module, tmp_path):
    with pytest.raises(launcher_module.LauncherError, match="benchmark WebUI"):
        launcher_module.build_run_command(
            tmp_path,
            {
                "suite": "clock",
                "suite_type": "mobilegym_builtin",
                "analysis_model": "anthropic/claude-sonnet-4-6",
            },
        )


def test_build_run_command_rejects_unknown_mode(launcher_module, tmp_path):
    with pytest.raises(launcher_module.LauncherError, match="unsupported local launcher mode"):
        launcher_module.build_run_command(
            tmp_path,
            {"mode": "unknown", "suite": "perception/perception_v1"},
        )


def test_build_run_command_rejects_path_traversal(launcher_module, tmp_path):
    with pytest.raises(launcher_module.LauncherError, match="benchmark WebUI"):
        launcher_module.build_run_command(
            tmp_path,
            {"suite": "..", "suite_type": "aico", "parallel": 1},
        )


def test_validate_model_environment_requires_api_key_for_openrouter(launcher_module, monkeypatch):
    for name in ("MODEL_API_KEY", "OPENROUTER_API_KEY", "AICO_MODEL_API_KEY"):
        monkeypatch.delenv(name, raising=False)
    monkeypatch.setenv("MODEL_PROVIDER", "openrouter")

    with pytest.raises(launcher_module.LauncherError, match="MODEL_API_KEY or OPENROUTER_API_KEY"):
        launcher_module.validate_model_environment()


def test_parse_board_agent_model_config(launcher_module):
    config = launcher_module.parse_agent_model_config(
        '''
[model_settings.providers.seeklab]
type = "openai"
base_url = "https://proxy.seeklab.io/qwen/v1"
api_key = "secret-key"

[model_settings.model]
provider = "seeklab"
model = "qwen3.6-35b"
base_url = "https://stale.example.com/v1"

[voice_settings.classic.tts]
provider = "minimax-cn"
api_key = "tts-key"
'''
    )

    assert config == {
        "MODEL_PROVIDER": "openai",
        "MODEL_NAME": "qwen3.6-35b",
        "MODEL_BASE_URL": "https://proxy.seeklab.io/qwen/v1",
        "MODEL_API_KEY": "secret-key",
    }


def test_parse_board_agent_config_ignores_missing_sections(launcher_module):
    assert launcher_module.parse_agent_model_config(
        '[voice_settings.classic.tts]\nprovider = "minimax-cn"\napi_key = "tts-key"\n'
    ) == {}
    assert launcher_module.parse_agent_benchmark_config(
        '[model_settings.model]\nprovider = "openai"\napi_key = "model-key"\n'
    ) == {}


def test_fetch_board_model_config_uses_shell_tool(launcher_module):
    class Handler(launcher_module.BaseHTTPRequestHandler):
        def do_POST(self):
            length = int(self.headers.get("Content-Length") or "0")
            body = json.loads(self.rfile.read(length).decode())
            assert self.path == "/api/tools/shell"
            assert "/userdata/agent/agent.toml" in body["input"]["command"]
            if "model_settings.providers.seeklab" in body["input"]["command"]:
                output = (
                    'type = "openai"\n'
                    'base_url = "https://proxy.seeklab.io/qwen/v1"\n'
                    'api_key = "secret-key"\n'
                )
            else:
                output = 'provider = "seeklab"\nmodel = "qwen3.6-35b"\n'
            payload = {"output": output}
            raw = json.dumps(payload).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(raw)))
            self.end_headers()
            self.wfile.write(raw)

    server = HTTPServer(("127.0.0.1", 0), Handler)
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        config = launcher_module.fetch_board_model_config(f"http://127.0.0.1:{server.server_port}")
        assert config["MODEL_PROVIDER"] == "openai"
        assert config["MODEL_BASE_URL"] == "https://proxy.seeklab.io/qwen/v1"
        assert config["MODEL_API_KEY"] == "secret-key"
    finally:
        server.shutdown()
        thread.join(timeout=2)


def test_current_model_label_ignores_legacy_command_environment(launcher_module):
    assert launcher_module.current_model_label({"MODEL_NAME": "qwen3.6-35b"}) == "aico-go"


def test_handler_options_allows_browser_cors(launcher_module, tmp_path):
    server = HTTPServer(("127.0.0.1", 0), launcher_module.make_handler(tmp_path))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        req = urllib.request.Request(
            f"http://127.0.0.1:{server.server_port}/benchmark/run",
            method="OPTIONS",
            headers={"Access-Control-Request-Method": "POST"},
        )
        with urllib.request.urlopen(req, timeout=2) as resp:
            assert resp.status == 204
            assert resp.headers["Access-Control-Allow-Origin"] == "*"
            assert "POST" in resp.headers["Access-Control-Allow-Methods"]
            assert "Content-Type" in resp.headers["Access-Control-Allow-Headers"]
            assert resp.headers["Access-Control-Allow-Private-Network"] == "true"
    finally:
        server.shutdown()
        thread.join(timeout=2)


def test_handler_serves_mobilegym_report(launcher_module, tmp_path):
    report_dir = tmp_path / "runs" / "mobilegym" / "batch-20260611-010101"
    report_dir.mkdir(parents=True)
    (report_dir / "index.html").write_text("<html>MobileGym report</html>")
    server = HTTPServer(("127.0.0.1", 0), launcher_module.make_handler(tmp_path))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with urllib.request.urlopen(
            f"http://127.0.0.1:{server.server_port}/benchmark/report/batch-20260611-010101",
            timeout=2,
        ) as resp:
            assert resp.status == 200
            assert "text/html" in resp.headers["Content-Type"]
            assert resp.read().decode() == "<html>MobileGym report</html>"
    finally:
        server.shutdown()
        thread.join(timeout=2)


def test_handler_serves_mobilegym_suite_report(launcher_module, tmp_path):
    report_dir = tmp_path / "runs" / "mobilegym" / "batch-20260611-010101" / "perception" / "perception_v1"
    report_dir.mkdir(parents=True)
    (report_dir / "index.html").write_text("<html>Perception report</html>")

    server = HTTPServer(("127.0.0.1", 0), launcher_module.make_handler(tmp_path))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with urllib.request.urlopen(
            f"http://127.0.0.1:{server.server_port}/benchmark/report/batch-20260611-010101/perception/perception_v1",
            timeout=2,
        ) as resp:
            assert resp.status == 200
            assert "text/html" in resp.headers["Content-Type"]
            assert resp.read().decode() == "<html>Perception report</html>"
    finally:
        server.shutdown()
        thread.join(timeout=2)


def test_handler_serves_mobilegym_analysis_artifact(launcher_module, tmp_path):
    report_dir = tmp_path / "runs" / "mobilegym" / "batch-20260611-010101"
    report_dir.mkdir(parents=True)
    (report_dir / "index.html").write_text("<html>MobileGym report</html>")
    (report_dir / "llm_analysis.md").write_text("analysis body")
    server = HTTPServer(("127.0.0.1", 0), launcher_module.make_handler(tmp_path))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with urllib.request.urlopen(
            f"http://127.0.0.1:{server.server_port}/benchmark/report/batch-20260611-010101/llm_analysis.md",
            timeout=2,
        ) as resp:
            assert resp.status == 200
            assert "text/markdown" in resp.headers["Content-Type"]
            assert resp.read().decode() == "analysis body"
    finally:
        server.shutdown()
        thread.join(timeout=2)


def test_handler_rejects_unsafe_report_artifact(launcher_module, tmp_path):
    server = HTTPServer(("127.0.0.1", 0), launcher_module.make_handler(tmp_path))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        with pytest.raises(urllib.error.HTTPError) as exc:
            urllib.request.urlopen(
                f"http://127.0.0.1:{server.server_port}/benchmark/report/batch-20260611-010101/../../secret",
                timeout=2,
            )
        assert exc.value.code == 404
    finally:
        server.shutdown()
        thread.join(timeout=2)


def test_list_runs_includes_summary_model_and_progress(launcher_module, tmp_path):
    run_dir = tmp_path / "runs" / "mobilegym" / "batch-20260611-120000"
    run_dir.mkdir(parents=True)
    (run_dir / "summary.json").write_text(
        '{"tasks":17,"passed":12,"failed":5,"model":"google/gemini-3.5-flash","suites":[{"suite":"clock"}]}',
    )

    runs = launcher_module.list_runs(tmp_path)

    assert runs == [
        {
            "run_id": "batch-20260611-120000",
            "suite": "clock",
            "status": "done",
            "progress": "17/17",
            "model": "google/gemini-3.5-flash",
            "totals": {"tasks": 17, "passed": 12, "failed": 5},
        }
    ]


def test_list_runs_expands_multiple_summary_suites(launcher_module, tmp_path):
    run_dir = tmp_path / "runs" / "mobilegym" / "batch-20260611-130000"
    run_dir.mkdir(parents=True)
    (run_dir / "clock").mkdir()
    (run_dir / "phone_control_v1").mkdir()
    (run_dir / "clock" / "index.html").write_text("clock")
    (run_dir / "phone_control_v1" / "index.html").write_text("phone")
    (run_dir / "summary.json").write_text(
        json.dumps(
            {
                "tasks": 6,
                "passed": 4,
                "failed": 2,
                "model": "qwen3.6-35b",
                "suites": [
                    {"suite": "clock", "tasks": 2, "passed": 1, "failed": 1},
                    {"suite": "phone_control_v1", "tasks": 4, "passed": 3, "failed": 1},
                ],
            }
        ),
    )

    runs = launcher_module.list_runs(tmp_path)

    assert [run["suite"] for run in runs] == ["clock", "phone_control_v1"]
    assert [run["progress"] for run in runs] == ["2/2", "4/4"]
    assert runs[0]["totals"] == {"tasks": 2, "passed": 1, "failed": 1}
    assert runs[1]["totals"] == {"tasks": 4, "passed": 3, "failed": 1}
    assert runs[0]["model"] == "qwen3.6-35b"
    assert [run["report_path"] for run in runs] == [
        "/benchmark/report/batch-20260611-130000/clock",
        "/benchmark/report/batch-20260611-130000/phone_control_v1",
    ]


def test_list_runs_marks_current_run_not_done_without_summary(launcher_module, tmp_path):
    run_dir = tmp_path / "runs" / "mobilegym" / "batch-20260611-205808"
    (run_dir / "launcher" / "shard-0").mkdir(parents=True)
    (tmp_path / launcher_module.STATE_NAME).write_text(
        '{"status":"running","run_id":"batch-20260611-205808","suite":"launcher","model":"qwen3.6-35b"}',
    )

    runs = launcher_module.list_runs(tmp_path)

    assert runs == [
        {
            "run_id": "batch-20260611-205808",
            "suite": "launcher",
            "status": "running",
            "progress": "",
            "model": "qwen3.6-35b",
            "totals": {"tasks": 0, "passed": 0, "failed": 0},
        }
    ]


def test_current_status_reports_mobilegym_progress_from_shard_artifacts(launcher_module, tmp_path):
    class RunningProcess:
        def poll(self):
            return None

    run_dir = tmp_path / "runs" / "mobilegym" / "batch-20260613-progress"
    shard0 = run_dir / "personamem_lt_recall_v1" / "shard-0"
    shard1 = run_dir / "personamem_lt_recall_v1" / "shard-1"
    shard0.mkdir(parents=True)
    shard1.mkdir(parents=True)
    (shard0 / "shard.json").write_text(
        json.dumps(
            {
                "selected_task_count": 2,
                "selected_task_ids": ["suite.task_a", "suite.task_b"],
                "exit_code": 0,
            }
        )
    )
    (shard1 / "shard.json").write_text(
        json.dumps(
            {
                "selected_task_count": 2,
                "selected_task_ids": ["suite.task_c", "suite.task_d"],
            }
        )
    )
    (shard0 / "raw" / "run").mkdir(parents=True)
    (shard0 / "raw" / "run" / "results.jsonl").write_text(
        json.dumps({"id": "suite.task_a", "is_success": True}) + "\n"
    )
    (tmp_path / launcher_module.STATE_NAME).write_text(
        json.dumps(
            {
                "status": "running",
                "run_id": "batch-20260613-progress",
                "suite": "personamem_lt_recall_v1",
                "model": "qwen3.6-35b",
            }
        )
    )
    launcher_module._process = RunningProcess()

    try:
        status = launcher_module.current_status(tmp_path)
    finally:
        launcher_module._process = None

    assert status["status"] == "running"
    assert status["total"] == 4
    assert status["completed"] == 1
    assert status["current"] == 2
    assert status["current_task"] == "suite.task_b"
    assert status["progress"] == "1/4"


def test_current_status_keeps_state_progress_before_shards_exist(launcher_module, tmp_path):
    class RunningProcess:
        def poll(self):
            return None

    (tmp_path / "runs" / "mobilegym" / "batch-20260613-starting").mkdir(parents=True)
    (tmp_path / launcher_module.STATE_NAME).write_text(
        json.dumps(
            {
                "status": "running",
                "run_id": "batch-20260613-starting",
                "suite": "clock",
                "total": 2,
                "current": 1,
                "completed": 0,
            }
        )
    )
    launcher_module._process = RunningProcess()

    try:
        status = launcher_module.current_status(tmp_path)
    finally:
        launcher_module._process = None

    assert status["total"] == 2
    assert status["current"] == 1
    assert status["completed"] == 0


def test_list_runs_reports_running_mobilegym_progress_from_shard_artifacts(launcher_module, tmp_path):
    run_dir = tmp_path / "runs" / "mobilegym" / "batch-20260613-progress"
    shard = run_dir / "personamem_lt_recall_v1" / "shard-0"
    (shard / "raw" / "run").mkdir(parents=True)
    (shard / "shard.json").write_text(
        json.dumps(
            {
                "selected_task_count": 3,
                "selected_task_ids": ["suite.task_a", "suite.task_b", "suite.task_c"],
            }
        )
    )
    (shard / "raw" / "run" / "results.jsonl").write_text(
        json.dumps({"id": "suite.task_a", "is_success": True}) + "\n"
    )
    (tmp_path / launcher_module.STATE_NAME).write_text(
        json.dumps(
            {
                "status": "running",
                "run_id": "batch-20260613-progress",
                "suite": "personamem_lt_recall_v1",
                "model": "qwen3.6-35b",
            }
        )
    )

    runs = launcher_module.list_runs(tmp_path)

    assert runs == [
        {
            "run_id": "batch-20260613-progress",
            "suite": "personamem_lt_recall_v1",
            "status": "running",
            "progress": "1/3",
            "model": "qwen3.6-35b",
            "totals": {"tasks": 3, "passed": 0, "failed": 0},
        }
    ]


def test_helper_start_invokes_launchctl_for_local_launcher(helper_module, monkeypatch):
    calls = []

    def fake_run(argv, *, check, capture_output, text, timeout):
        calls.append((argv, timeout))
        return subprocess.CompletedProcess(argv, 0, stdout="started\n", stderr="")

    monkeypatch.setattr(helper_module.subprocess, "run", fake_run)

    result = helper_module.start_local_launcher()

    assert result == {"ok": True, "status": "started", "output": "started"}
    assert calls == [
        (
            [
                "launchctl",
                "kickstart",
                "-k",
                f"gui/{os.getuid()}/com.aico.mobilegym-local-launcher",
            ],
            helper_module.LAUNCHCTL_TIMEOUT_SECONDS,
        )
    ]


def test_helper_start_bootstraps_launcher_when_service_is_unloaded(helper_module, monkeypatch, tmp_path):
    calls = []
    plist = tmp_path / "com.aico.mobilegym-local-launcher.plist"
    plist.write_text("plist")
    monkeypatch.setattr(helper_module, "LAUNCHER_PLIST", plist)

    def fake_run(argv, *, check, capture_output, text, timeout):
        calls.append(argv)
        if argv[1] == "kickstart" and len(calls) == 1:
            raise subprocess.CalledProcessError(
                113,
                argv,
                output="",
                stderr='Could not find service "com.aico.mobilegym-local-launcher"\n',
            )
        return subprocess.CompletedProcess(argv, 0, stdout="", stderr="")

    monkeypatch.setattr(helper_module.subprocess, "run", fake_run)

    result = helper_module.start_local_launcher()

    assert result == {"ok": True, "status": "started", "output": ""}
    assert calls == [
        ["launchctl", "kickstart", "-k", f"gui/{os.getuid()}/com.aico.mobilegym-local-launcher"],
        ["launchctl", "bootstrap", f"gui/{os.getuid()}", str(plist)],
        ["launchctl", "kickstart", "-k", f"gui/{os.getuid()}/com.aico.mobilegym-local-launcher"],
    ]


def test_helper_start_reports_launchctl_timeout(helper_module, monkeypatch):
    def fake_run(argv, *, check, capture_output, text, timeout):
        raise subprocess.TimeoutExpired(argv, timeout)

    monkeypatch.setattr(helper_module.subprocess, "run", fake_run)

    with pytest.raises(helper_module.HelperError, match="launchctl timed out"):
        helper_module.start_local_launcher()


def test_helper_start_endpoint_allows_private_browser_origin(helper_module):
    server = HTTPServer(("127.0.0.1", 0), helper_module.make_handler(lambda: {"ok": True}))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        req = urllib.request.Request(
            f"http://127.0.0.1:{server.server_port}/start",
            data=b"{}",
            method="POST",
            headers={"Content-Type": "application/json", "Origin": "http://192.168.31.107:8080"},
        )
        with urllib.request.urlopen(req, timeout=2) as resp:
            assert resp.status == 200
            assert resp.headers["Access-Control-Allow-Origin"] == "http://192.168.31.107:8080"
            assert resp.headers["Access-Control-Allow-Private-Network"] == "true"
            assert json.loads(resp.read().decode()) == {"ok": True}
    finally:
        server.shutdown()
        thread.join(timeout=2)


def test_helper_start_endpoint_rejects_public_browser_origin(helper_module):
    server = HTTPServer(("127.0.0.1", 0), helper_module.make_handler(lambda: {"ok": True}))
    thread = threading.Thread(target=server.serve_forever, daemon=True)
    thread.start()
    try:
        req = urllib.request.Request(
            f"http://127.0.0.1:{server.server_port}/start",
            data=b"{}",
            method="POST",
            headers={"Content-Type": "application/json", "Origin": "https://example.com"},
        )
        with pytest.raises(urllib.error.HTTPError) as err:
            urllib.request.urlopen(req, timeout=2)
        assert err.value.code == 403
        assert err.value.headers.get("Access-Control-Allow-Origin") is None
    finally:
        server.shutdown()
        thread.join(timeout=2)


def test_local_launcher_installer_registers_launchd_service():
    script = INSTALLER_PATH.read_text()

    assert "com.aico.mobilegym-local-launcher" in script
    assert "com.aico.mobilegym-local-launcher-helper" in script
    assert "benchmark/mobilegym/scripts/local_launcher.py" in script
    assert "benchmark/mobilegym/scripts/local_launcher_helper.py" in script
    assert "UV_BIN=\"$(command -v uv)\"" in script
    assert 'exec "$UV_BIN" run --project benchmark python' in script
    assert "exec uv run --project benchmark python" not in script
    assert "<string>-c</string>" in script
    assert "<string>-lc</string>" not in script
    assert "--host 127.0.0.1" in script
    assert "--port 4174" in script
    assert "--port 4175" in script
    assert "MOBILEGYM_DOCKER_PROXY" in script
    assert "http://host.docker.internal:7897" not in script
    assert 'if [[ -n "${MOBILEGYM_DOCKER_PROXY:-}" ]]' in script
    assert "MOBILEGYM_PLAYWRIGHT_CHROMIUM_DOWNLOAD_HOST" in script
    assert "<key>PATH</key>" in script
    assert "/Applications/Docker.app/Contents/Resources/bin" in script
    assert "launchctl bootstrap gui/$UID" in script


def test_local_launcher_uses_user_specific_temp_paths(launcher_module):
    paths = [launcher_module.LOG_PATH, launcher_module.PID_PATH]

    assert paths[0].parent == paths[1].parent
    assert "mobilegym" in paths[0].parent.name
    assert str(os.getuid()) in paths[0].parent.name
    assert paths[0] != Path("/tmp/mobilegym_run.log")
    assert paths[1] != Path("/tmp/mobilegym_runner.pid")
    assert paths[0].parent.stat().st_mode & 0o777 == 0o700
