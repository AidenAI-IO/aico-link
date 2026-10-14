import json
import threading
import time
from datetime import datetime, timezone
from pathlib import Path

import pytest

from runner import environment


MOBILEGYM_COMMIT = "1896e744cc33fcc77ebf645ff54584f83b5c6aec"
MOBILEGYM_REPO = "https://github.com/AidenAI-IO/mobilegym.git"


def _manager_without_discovery(monkeypatch, tmp_path: Path) -> environment.EnvironmentManager:
    monkeypatch.setattr(environment.EnvironmentManager, "_discover_existing_containers", lambda self: None)
    return environment.EnvironmentManager(runs_dir=tmp_path / "runs")


def test_mobilegym_bridge_bind_uses_docker_gateway_on_linux(monkeypatch):
    captured = {}
    monkeypatch.setattr(environment.sys, "platform", "linux")

    def inspect(cmd, text=False, timeout=None):
        captured["cmd"] = cmd
        captured["timeout"] = timeout
        return "172.19.0.1\n"

    monkeypatch.setattr(environment.subprocess, "check_output", inspect)

    assert environment.mobilegym_bridge_bind_host() == "172.19.0.1"
    assert captured["cmd"] == [
        "docker", "network", "inspect", "bridge", "--format",
        "{{(index .IPAM.Config 0).Gateway}}",
    ]
    assert captured["timeout"] == 5


def test_mobilegym_bridge_bind_is_loopback_on_docker_desktop(monkeypatch):
    monkeypatch.setattr(environment.sys, "platform", "darwin")
    monkeypatch.setattr(
        environment.subprocess,
        "check_output",
        lambda *args, **kwargs: pytest.fail("Docker gateway lookup is Linux-only"),
    )

    assert environment.mobilegym_bridge_bind_host() == "127.0.0.1"


def test_mobilegym_bridge_bind_rejects_all_interfaces_override(monkeypatch):
    monkeypatch.setenv("AIDEN_MOBILEGYM_BRIDGE_BIND_HOST", "0.0.0.0")

    with pytest.raises(RuntimeError, match="no usable IPv4 host gateway"):
        environment.mobilegym_bridge_bind_host()


def test_list_all_coalesces_concurrent_docker_sync(monkeypatch, tmp_path: Path):
    manager = _manager_without_discovery(monkeypatch, tmp_path)
    calls = 0
    calls_lock = threading.Lock()
    start = threading.Barrier(6)

    def fake_list_docker_containers():
        nonlocal calls
        with calls_lock:
            calls += 1
        time.sleep(0.05)
        return []

    monkeypatch.setattr(manager, "_list_docker_containers", fake_list_docker_containers)

    def worker():
        start.wait(timeout=1)
        manager.list_all()

    threads = [threading.Thread(target=worker) for _ in range(5)]
    for thread in threads:
        thread.start()
    start.wait(timeout=1)
    for thread in threads:
        thread.join(timeout=1)

    assert calls == 1


def test_format_container_age_accepts_docker_inspect_rfc3339nano():
    created = datetime.now(timezone.utc).replace(microsecond=123456).isoformat().replace("+00:00", "Z")

    assert environment._format_container_age(created).endswith(" old")


def test_format_container_age_returns_unknown_for_future_timestamp():
    assert environment._format_container_age("2999-01-01T00:00:00Z") == "age unknown"


def test_recovered_mobilegym_container_is_not_reusable_for_new_runs(monkeypatch, tmp_path: Path):
    manager = _manager_without_discovery(monkeypatch, tmp_path)
    monkeypatch.setattr(
        environment,
        "_docker_published_port_safe",
        lambda container_name, container_port: 19090 if container_port == 9090 else 18173,
    )
    monkeypatch.setattr(environment, "_check_endpoint_health", lambda *args, **kwargs: True)
    monkeypatch.setattr(environment, "mobilegym_bridge_bind_host", lambda: "172.17.0.1")
    monkeypatch.setattr(
        environment,
        "_docker_inspect_created",
        lambda container_name: "2026-08-14T00:00:00Z",
    )

    recovered = manager._build_recovered_env(
        {
            "name": "aiden-mobilegym-env-mg-old",
            "id": "container-id",
            "created_at": "2026-08-14 08:00:00 +0800 CST",
            "image": "aiden-mobilegym-simulator:test",
        }
    )

    assert recovered is not None
    assert recovered.status == "stale"
    assert "start a fresh environment" in recovered.message


def test_resolve_mobilegym_source_uses_head_gitlink_and_gitmodules(monkeypatch, tmp_path: Path):
    (tmp_path / ".gitmodules").write_text(
        """[submodule \"benchmark/mobilegym/vendor/mobilegym\"]
\tpath = benchmark/mobilegym/vendor/mobilegym
\turl = https://github.com/AidenAI-IO/mobilegym.git
""",
        encoding="utf-8",
    )
    captured = {}

    def fake_check_output(cmd, **kwargs):
        captured["cmd"] = cmd
        captured["cwd"] = kwargs.get("cwd")
        return f"{MOBILEGYM_COMMIT}\n"

    monkeypatch.setattr(environment.subprocess, "check_output", fake_check_output)

    source = environment.resolve_mobilegym_source(tmp_path)

    assert source.repo == MOBILEGYM_REPO
    assert source.commit == MOBILEGYM_COMMIT
    assert captured["cmd"] == [
        "git",
        "rev-parse",
        "HEAD:benchmark/mobilegym/vendor/mobilegym",
    ]
    assert captured["cwd"] == tmp_path


def test_ensure_mobilegym_image_reuses_matching_commit(monkeypatch, tmp_path: Path):
    monkeypatch.setattr(
        environment,
        "resolve_mobilegym_source",
        lambda repo_root: environment.MobileGymSource(MOBILEGYM_REPO, MOBILEGYM_COMMIT),
    )

    def fake_run(cmd, **kwargs):
        return environment.subprocess.CompletedProcess(
            cmd,
            0,
            stdout=json.dumps({environment.MOBILEGYM_COMMIT_LABEL: MOBILEGYM_COMMIT}),
            stderr="",
        )

    monkeypatch.setattr(environment.subprocess, "run", fake_run)
    monkeypatch.setattr(
        environment.subprocess,
        "Popen",
        lambda *args, **kwargs: pytest.fail("matching image should not be rebuilt"),
    )

    environment.ensure_mobilegym_image(
        "aiden-mobilegym-simulator:test",
        True,
        tmp_path / "build.log",
        repo_root=tmp_path,
    )


@pytest.mark.parametrize(
    ("inspect_returncode", "labels"),
    [
        (1, None),
        (0, {"io.aiden.mobilegym.commit": "0" * 40}),
    ],
)
def test_ensure_mobilegym_image_builds_missing_or_stale_image(
    monkeypatch,
    tmp_path: Path,
    inspect_returncode: int,
    labels: dict[str, str] | None,
):
    monkeypatch.setattr(
        environment,
        "resolve_mobilegym_source",
        lambda repo_root: environment.MobileGymSource(MOBILEGYM_REPO, MOBILEGYM_COMMIT),
    )
    monkeypatch.setenv("MOBILEGYM_NODE_IMAGE", "mirror.example/node:22-bookworm")
    monkeypatch.setenv("MOBILEGYM_DEBIAN_MIRROR", "mirror.example")

    def fake_run(cmd, **kwargs):
        return environment.subprocess.CompletedProcess(
            cmd,
            inspect_returncode,
            stdout=json.dumps(labels) if labels is not None else "",
            stderr="",
        )

    class FakeProc:
        returncode = 0

        def poll(self):
            return self.returncode

    captured = {}
    monkeypatch.setattr(environment.subprocess, "run", fake_run)

    def fake_popen(cmd, **kwargs):
        captured["cmd"] = cmd
        return FakeProc()

    monkeypatch.setattr(environment.subprocess, "Popen", fake_popen)

    environment.ensure_mobilegym_image(
        "aiden-mobilegym-simulator:test",
        True,
        tmp_path / "build.log",
        repo_root=tmp_path,
    )

    assert captured["cmd"] == [
        "docker",
        "build",
        "-f",
        str(tmp_path / "benchmark" / "mobilegym" / "docker" / "Dockerfile"),
        "--target",
        "mobilegym-base",
        "--build-arg",
        f"MOBILEGYM_REPO={MOBILEGYM_REPO}",
        "--build-arg",
        f"MOBILEGYM_COMMIT={MOBILEGYM_COMMIT}",
        "--build-arg",
        "MOBILEGYM_NODE_IMAGE=mirror.example/node:22-bookworm",
        "--build-arg",
        "MOBILEGYM_DEBIAN_MIRROR=mirror.example",
        "-t",
        "aiden-mobilegym-simulator:test",
        str(tmp_path),
    ]


def test_ensure_mobilegym_image_rejects_stale_image_when_build_disabled(monkeypatch, tmp_path: Path):
    monkeypatch.setattr(
        environment,
        "resolve_mobilegym_source",
        lambda repo_root: environment.MobileGymSource(MOBILEGYM_REPO, MOBILEGYM_COMMIT),
    )
    monkeypatch.setattr(
        environment.subprocess,
        "run",
        lambda cmd, **kwargs: environment.subprocess.CompletedProcess(
            cmd,
            0,
            stdout=json.dumps({environment.MOBILEGYM_COMMIT_LABEL: "0" * 40}),
            stderr="",
        ),
    )

    with pytest.raises(RuntimeError, match=f"expected {MOBILEGYM_COMMIT}"):
        environment.ensure_mobilegym_image(
            "aiden-mobilegym-simulator:test",
            False,
            tmp_path / "build.log",
            repo_root=tmp_path,
        )
