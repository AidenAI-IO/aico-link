"""Persistent launcher state: one JSON file written atomically under a lock."""

from __future__ import annotations

import copy
import json
import os
import tempfile
import threading
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable

STATE_FILE_NAME = "state.json"
LOCK_FILE_NAME = "launcher.lock"
STATE_VERSION = 1
MAX_STORED_BUILDS = 20


def now_iso() -> str:
    return datetime.now(timezone.utc).isoformat(timespec="seconds")


def default_settings() -> dict[str, Any]:
    return {
        "build": {"base_image": "", "go_download_base": "", "mobilegym_node_image": ""},
        "pass_host_proxy": False,
    }


class StateDirBusy(RuntimeError):
    """Another launcher process already owns the state directory."""


def lock_state_dir(state_dir: Path) -> Any:
    """Take an exclusive, non-blocking process lock on ``state_dir``.

    Each launcher caches state.json in memory and rewrites it whole, so two
    launchers sharing a directory would overwrite each other's changes. Keep
    the returned handle open for the life of the process; the OS releases the
    lock when the process exits. Raises :class:`StateDirBusy` if it is held.
    """
    state_dir = Path(state_dir)
    state_dir.mkdir(parents=True, exist_ok=True)
    handle = open(state_dir / LOCK_FILE_NAME, "a+b")
    try:
        if os.name == "nt":
            import msvcrt

            handle.seek(0)
            msvcrt.locking(handle.fileno(), msvcrt.LK_NBLCK, 1)
        else:
            import fcntl

            fcntl.flock(handle.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
    except OSError as exc:
        handle.close()
        raise StateDirBusy(
            f"Another launcher is already using the state directory {state_dir}. "
            "Stop it, or start this one with a different --state-dir."
        ) from exc
    return handle


def _empty_state() -> dict[str, Any]:
    return {
        "version": STATE_VERSION,
        "settings": default_settings(),
        "instances": {},
        "builds": [],
    }


class Store:
    """Thread-safe JSON document; every mutation is persisted before returning.

    Readers get deep copies, so callers can never mutate the stored state
    without going through :meth:`update`.
    """

    def __init__(self, state_dir: Path) -> None:
        self.state_dir = Path(state_dir)
        self.path = self.state_dir / STATE_FILE_NAME
        self._lock = threading.RLock()
        self.state_dir.mkdir(parents=True, exist_ok=True)
        self._data = self._load()

    def _load(self) -> dict[str, Any]:
        try:
            raw = json.loads(self.path.read_text(encoding="utf-8"))
        except FileNotFoundError:
            return _empty_state()
        except (OSError, json.JSONDecodeError) as exc:
            # Keep the unreadable file for inspection instead of overwriting it.
            backup = self.path.with_suffix(f".corrupt-{os.getpid()}")
            try:
                self.path.replace(backup)
            except OSError:
                pass
            print(f"warning: ignoring unreadable launcher state {self.path}: {exc}")
            return _empty_state()
        data = _empty_state()
        if isinstance(raw, dict):
            settings = raw.get("settings")
            if isinstance(settings, dict):
                merged = default_settings()
                build = settings.get("build")
                if isinstance(build, dict):
                    merged["build"].update(
                        {key: str(build.get(key) or "") for key in merged["build"]}
                    )
                merged["pass_host_proxy"] = bool(settings.get("pass_host_proxy"))
                data["settings"] = merged
            instances = raw.get("instances")
            if isinstance(instances, dict):
                data["instances"] = {
                    str(key): value for key, value in instances.items() if isinstance(value, dict)
                }
            builds = raw.get("builds")
            if isinstance(builds, list):
                data["builds"] = [item for item in builds if isinstance(item, dict)]
        return data

    def _save(self, data: dict[str, Any]) -> None:
        payload = json.dumps(data, ensure_ascii=False, indent=2, sort_keys=True)
        fd, tmp_name = tempfile.mkstemp(prefix=".state-", suffix=".json", dir=self.state_dir)
        try:
            with os.fdopen(fd, "w", encoding="utf-8") as handle:
                handle.write(payload)
                handle.flush()
                os.fsync(handle.fileno())
            os.replace(tmp_name, self.path)
        except BaseException:
            try:
                os.unlink(tmp_name)
            except OSError:
                pass
            raise

    # -- generic access -----------------------------------------------------

    def read(self, reader: Callable[[dict[str, Any]], Any]) -> Any:
        with self._lock:
            return copy.deepcopy(reader(self._data))

    def update(self, mutator: Callable[[dict[str, Any]], Any]) -> Any:
        """Apply mutator to a copy of the document, persist it, then publish it.

        If saving fails, the live document is left unchanged.
        """
        with self._lock:
            data = copy.deepcopy(self._data)
            result = mutator(data)
            self._save(data)
            self._data = data
            return copy.deepcopy(result)

    @property
    def lock(self) -> threading.RLock:
        return self._lock

    # -- settings -----------------------------------------------------------

    def settings(self) -> dict[str, Any]:
        return self.read(lambda data: data["settings"])

    def set_settings(self, settings: dict[str, Any]) -> dict[str, Any]:
        def mutate(data: dict[str, Any]) -> dict[str, Any]:
            data["settings"] = settings
            return settings

        return self.update(mutate)

    # -- instances ----------------------------------------------------------

    def instances(self) -> list[dict[str, Any]]:
        return self.read(
            lambda data: sorted(data["instances"].values(), key=lambda item: item.get("created_at", ""))
        )

    def instance(self, instance_id: str) -> dict[str, Any] | None:
        return self.read(lambda data: data["instances"].get(instance_id))

    def put_instance(self, record: dict[str, Any]) -> dict[str, Any]:
        def mutate(data: dict[str, Any]) -> dict[str, Any]:
            data["instances"][record["id"]] = record
            return record

        return self.update(mutate)

    def patch_instance(self, instance_id: str, **changes: Any) -> dict[str, Any] | None:
        def mutate(data: dict[str, Any]) -> dict[str, Any] | None:
            record = data["instances"].get(instance_id)
            if record is None:
                return None
            for key, value in changes.items():
                if key == "runtime":
                    # runtime=None clears it; a dict merges into it.
                    runtime = record.setdefault("runtime", {})
                    if value is None:
                        runtime.clear()
                    else:
                        runtime.update(value)
                else:
                    record[key] = value
            record["updated_at"] = now_iso()
            return record

        return self.update(mutate)

    def delete_instance(self, instance_id: str) -> bool:
        return bool(self.update(lambda data: data["instances"].pop(instance_id, None) is not None))

    # -- builds -------------------------------------------------------------

    def builds(self) -> list[dict[str, Any]]:
        return self.read(lambda data: data["builds"])

    def build(self, build_id: str) -> dict[str, Any] | None:
        return self.read(
            lambda data: next((item for item in data["builds"] if item.get("id") == build_id), None)
        )

    def put_build(self, record: dict[str, Any]) -> dict[str, Any]:
        def mutate(data: dict[str, Any]) -> dict[str, Any]:
            builds = [item for item in data["builds"] if item.get("id") != record["id"]]
            builds.append(record)
            data["builds"] = builds[-MAX_STORED_BUILDS:]
            return record

        return self.update(mutate)

    def patch_build(self, build_id: str, **changes: Any) -> dict[str, Any] | None:
        def mutate(data: dict[str, Any]) -> dict[str, Any] | None:
            for item in data["builds"]:
                if item.get("id") == build_id:
                    item.update(changes)
                    return item
            return None

        return self.update(mutate)
