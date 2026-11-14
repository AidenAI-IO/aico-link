"""Local aico-link images and background image builds."""

from __future__ import annotations

import os
import threading
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Mapping

from runner.environment import tail_text

from . import IMAGE_REPOSITORY, validation
from .docker import Docker
from .net import PROXY_VARIABLES, host_proxy_args
from .store import Store, now_iso
from .system import System

BUILD_LOG_TAIL_BYTES = 64 * 1024
DOCKERFILE = Path("docker") / "dev" / "Dockerfile"


def repo_commit(system: System, repo_root: Path) -> str:
    result = system.run(["git", "rev-parse", "HEAD"], cwd=repo_root, timeout=10)
    return result.stdout.strip() if result.returncode == 0 else ""


def repo_dirty(system: System, repo_root: Path) -> bool:
    # Untracked files count: docker build sends them in the build context.
    # git status still honours .gitignore, so build output and .cache stay out.
    result = system.run(["git", "status", "--porcelain"], cwd=repo_root, timeout=20)
    return result.returncode == 0 and bool(result.stdout.strip())


def default_tag(system: System, repo_root: Path) -> str:
    commit = repo_commit(system, repo_root)
    tag = f"local-{commit[:7]}" if commit else "local"
    if repo_dirty(system, repo_root):
        tag += "-dirty"
    return tag


def build_command(
    *,
    repo_root: Path,
    image_ref: str,
    version: str,
    commit: str,
    settings: dict[str, Any],
    environ: Mapping[str, str],
) -> list[str]:
    build_settings = settings.get("build") or {}
    args: dict[str, str] = {"AICO_VERSION": version, "AICO_COMMIT": commit or "unknown"}
    # The Dockerfile owns the defaults; only override what the user set.
    if build_settings.get("base_image"):
        args["BASE_IMAGE"] = str(build_settings["base_image"])
    if build_settings.get("go_download_base"):
        args["GO_DOWNLOAD_BASE"] = str(build_settings["go_download_base"])
    # A Go module mirror configured on the host (for example goproxy.cn)
    # applies to the image build as well.
    if environ.get("GOPROXY"):
        args["GOPROXY"] = str(environ["GOPROXY"])
    if settings.get("pass_host_proxy"):
        args.update(host_proxy_args(environ))
    command = ["docker", "build", "-f", str(repo_root / DOCKERFILE)]
    if settings.get("pass_host_proxy") and args.keys() & set(PROXY_VARIABLES):
        # Lets a rewritten loopback proxy resolve during the build on Linux too.
        command.extend(["--add-host", "host.docker.internal:host-gateway"])
    for key, value in args.items():
        command.extend(["--build-arg", f"{key}={value}"])
    command.extend(["-t", image_ref, str(repo_root)])
    return command


class ImageManager:
    def __init__(
        self, *, store: Store, docker: Docker, system: System, state_dir: Path, repo_root: Path
    ) -> None:
        self.store = store
        self.docker = docker
        self.system = system
        self.repo_root = repo_root
        self.builds_dir = Path(state_dir) / "builds"
        self._lock = threading.Lock()
        self._threads: dict[str, threading.Thread] = {}
        self._mark_interrupted_builds()

    def _mark_interrupted_builds(self) -> None:
        for build in self.store.builds():
            if build.get("status") == "running":
                self.store.patch_build(
                    build["id"], status="failed", finished_at=now_iso(),
                    error="The launcher exited while this build was running.",
                )

    def list_images(self) -> list[dict[str, Any]]:
        return self.docker.list_images(IMAGE_REPOSITORY)

    def _log_path(self, build_id: str) -> Path:
        return self.builds_dir / f"{build_id}.log"

    def build_payload(self, record: dict[str, Any], *, with_log: bool = True) -> dict[str, Any]:
        payload = {
            "id": record.get("id", ""),
            "status": record.get("status", ""),
            "image_ref": record.get("image_ref", ""),
            "started_at": record.get("started_at", ""),
            "finished_at": record.get("finished_at", ""),
            "log_tail": "",
        }
        if with_log:
            payload["log_tail"] = tail_text(self._log_path(payload["id"]), BUILD_LOG_TAIL_BYTES)
            if record.get("error") and record.get("status") == "failed" and not payload["log_tail"]:
                payload["log_tail"] = str(record["error"])
        return payload

    def builds(self) -> list[dict[str, Any]]:
        return [self.build_payload(item, with_log=False) for item in self.store.builds()]

    def get_build(self, build_id: str) -> dict[str, Any] | None:
        record = self.store.build(validation.build_id(build_id))
        return self.build_payload(record) if record else None

    def start_build(self, tag: str | None = None) -> dict[str, Any]:
        tag = validation.image_tag(tag) if tag else default_tag(self.system, self.repo_root)
        image_ref = f"{IMAGE_REPOSITORY}:{tag}"
        with self._lock:
            for build in self.store.builds():
                if build.get("status") == "running":
                    raise BuildConflict(
                        f"{build.get('image_ref')} is still building; wait for it to finish."
                    )
            stamp = datetime.now(timezone.utc).strftime("%Y%m%d-%H%M%S")
            build_id = f"{stamp}-{os.urandom(3).hex()}"
            record = {
                "id": build_id,
                "status": "running",
                "image_ref": image_ref,
                "tag": tag,
                "started_at": now_iso(),
                "finished_at": "",
                "error": "",
            }
            self.store.put_build(record)
            thread = threading.Thread(
                target=self._run_build, args=(build_id, image_ref, tag), daemon=True,
                name=f"launcher-build-{build_id}",
            )
            self._threads[build_id] = thread
            thread.start()
        return self.build_payload(record)

    def _run_build(self, build_id: str, image_ref: str, tag: str) -> None:
        log_path = self._log_path(build_id)
        log_path.parent.mkdir(parents=True, exist_ok=True)
        try:
            command = build_command(
                repo_root=self.repo_root,
                image_ref=image_ref,
                version=tag,
                commit=repo_commit(self.system, self.repo_root),
                settings=self.store.settings(),
                environ=self.system.environ(),
            )
            with log_path.open("a", encoding="utf-8") as log:
                log.write("$ " + " ".join(command) + "\n")
            env = dict(self.system.environ())
            # The Dockerfile relies on BuildKit (BUILDPLATFORM/TARGETARCH).
            env["DOCKER_BUILDKIT"] = "1"
            code = self.system.run_logged(command, log_path, cwd=self.repo_root, env=env)
            if code == 0:
                self.store.patch_build(build_id, status="succeeded", finished_at=now_iso())
            else:
                self.store.patch_build(
                    build_id, status="failed", finished_at=now_iso(),
                    error=f"docker build exited with code {code}",
                )
        except Exception as exc:  # pragma: no cover - defensive
            with log_path.open("a", encoding="utf-8") as log:
                log.write(f"ERROR: {exc}\n")
            self.store.patch_build(build_id, status="failed", finished_at=now_iso(), error=str(exc))
        finally:
            with self._lock:
                self._threads.pop(build_id, None)


class BuildConflict(RuntimeError):
    pass
