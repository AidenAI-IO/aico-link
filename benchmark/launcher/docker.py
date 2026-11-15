"""Thin wrapper around the docker CLI; every call goes through System.run."""

from __future__ import annotations

import ipaddress
import json
from typing import Any

from .system import System

OCI_VERSION = "org.opencontainers.image.version"
OCI_REVISION = "org.opencontainers.image.revision"


class DockerError(RuntimeError):
    pass


def _check_removal(result, verb: str, name: str) -> None:
    """Raise unless a stop/remove succeeded or the object was already gone."""
    if result.returncode == 0:
        return
    # Docker 29 prints "no such object" in lowercase; older CLIs capitalise it.
    text = f"{result.stderr or ''}\n{result.stdout or ''}".lower()
    if "no such container" in text or "no such volume" in text or "no such object" in text:
        return
    raise DockerError(f"docker {verb} {name} failed: {_detail(result)}")


def _detail(result) -> str:
    text = (result.stderr or result.stdout or "").strip()
    return text.splitlines()[-1] if text else f"exit code {result.returncode}"


class Docker:
    def __init__(self, system: System) -> None:
        self.system = system

    def _run(self, *args: str, timeout: float | None = 30):
        return self.system.run(["docker", *args], timeout=timeout)

    def info(self) -> dict[str, Any]:
        result = self._run("version", "--format", "{{.Server.Version}}", timeout=10)
        if result.returncode == 0 and result.stdout.strip():
            return {"available": True, "version": result.stdout.strip(), "error": ""}
        if result.returncode == 127:
            error = "Docker CLI not found. Install Docker Desktop or Docker Engine."
        else:
            error = "Docker is not running: " + _detail(result)
        return {"available": False, "version": "", "error": error}

    def image_exists(self, ref: str) -> bool:
        return self._run("image", "inspect", "--format", "{{.Id}}", ref).returncode == 0

    def image_labels(self, ref: str) -> dict[str, str] | None:
        result = self._run("image", "inspect", "--format", "{{json .Config.Labels}}", ref)
        if result.returncode != 0:
            return None
        try:
            labels = json.loads(result.stdout or "null")
        except json.JSONDecodeError:
            return {}
        return {str(k): str(v) for k, v in labels.items()} if isinstance(labels, dict) else {}

    def list_images(self, repository: str) -> list[dict[str, Any]]:
        listed = self._run("image", "ls", repository, "--format", "{{.Repository}}:{{.Tag}}")
        if listed.returncode != 0:
            return []
        refs = [
            line.strip()
            for line in listed.stdout.splitlines()
            if line.strip() and not line.strip().endswith(":<none>")
        ]
        if not refs:
            return []
        inspected = self._run("image", "inspect", "--format", "{{json .}}", *refs)
        details: list[dict[str, Any]] = []
        for line in inspected.stdout.splitlines():
            try:
                item = json.loads(line)
            except json.JSONDecodeError:
                continue
            if isinstance(item, dict):
                details.append(item)
        by_ref = {
            str(tag): item for item in details for tag in (item.get("RepoTags") or [])
        }
        images = []
        for ref in refs:
            item = by_ref.get(ref)
            if item is None:
                continue
            labels = (item.get("Config") or {}).get("Labels") or {}
            repo, _, tag = ref.rpartition(":")
            images.append(
                {
                    "ref": ref,
                    "repository": repo,
                    "tag": tag,
                    "id": str(item.get("Id") or ""),
                    "created_at": str(item.get("Created") or ""),
                    "size": int(item.get("Size") or 0),
                    "version": str(labels.get(OCI_VERSION) or ""),
                    "commit": str(labels.get(OCI_REVISION) or ""),
                    "arch": str(item.get("Architecture") or ""),
                }
            )
        images.sort(key=lambda image: image["created_at"], reverse=True)
        return images

    def containers(self, label: str) -> dict[str, dict[str, str]]:
        """Map container name -> {state, instance} for containers carrying label."""
        result = self._run(
            "ps",
            "-a",
            "--filter",
            f"label={label}",
            "--format",
            "{{.Names}}\t{{.State}}\t{{.Label \"" + label + "\"}}",
        )
        if result.returncode != 0:
            raise DockerError("docker ps failed: " + _detail(result))
        containers: dict[str, dict[str, str]] = {}
        for line in result.stdout.splitlines():
            parts = line.split("\t")
            if len(parts) >= 3 and parts[0]:
                containers[parts[0]] = {"state": parts[1].strip(), "instance": parts[2].strip()}
        return containers

    def container_state(self, name: str) -> str:
        """The container's status, or '' when Docker reports that it does not exist."""
        result = self._run("inspect", "--format", "{{.State.Status}}", name)
        if result.returncode == 0:
            return result.stdout.strip()
        text = f"{result.stderr or ''}\n{result.stdout or ''}".lower()
        if "no such object" in text or "no such container" in text:
            return ""
        raise DockerError(f"docker inspect {name} failed: {_detail(result)}")

    def remove(self, name: str) -> None:
        _check_removal(self._run("rm", "-f", name, timeout=60), "rm", name)

    def stop(self, name: str, timeout_sec: int = 20) -> None:
        result = self._run("stop", "-t", str(int(timeout_sec)), name, timeout=timeout_sec + 30)
        _check_removal(result, "stop", name)

    def logs(self, name: str, tail: int) -> str:
        result = self._run("logs", "--tail", str(int(tail)), name, timeout=20)
        if result.returncode != 0:
            return ""
        return (result.stdout or "") + (result.stderr or "")

    def published_port(self, name: str, container_port: int) -> tuple[str, int]:
        result = self._run("port", name, f"{int(container_port)}/tcp")
        if result.returncode != 0:
            raise DockerError(f"could not read the published port of {name}: {_detail(result)}")
        for line in result.stdout.splitlines():
            host, _, port_text = line.strip().rpartition(":")
            host = host.strip("[]")
            try:
                address = ipaddress.ip_address(host)
                port = int(port_text)
            except ValueError:
                continue
            if address.version == 4:
                return ("127.0.0.1" if address.is_unspecified else host), port
        raise DockerError(f"{name} does not publish port {container_port}")

    def remove_volume(self, name: str) -> None:
        _check_removal(self._run("volume", "rm", "-f", name, timeout=60), "volume rm", name)
