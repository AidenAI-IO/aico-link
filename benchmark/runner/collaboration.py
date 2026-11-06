"""Benchmark-authorized SSE capture. Observation never consumes task notifications."""
from __future__ import annotations

import json
import os
import threading
import time
from pathlib import Path
from typing import Any, Callable

from runner.agent_client import AgentClient


class CollaborationEvidenceError(RuntimeError):
    pass


class CollaborationEventCollector:
    def __init__(self, client: AgentClient, path: Path):
        self.client, self.path = client, path
        self.events: list[dict[str, Any]] = []
        self.by_id: dict[str, dict[str, Any]] = {}
        self.cursor: tuple[int, int] | None = None
        self.error = ""
        self.connected = False
        self.condition = threading.Condition()
        self.stop_event = threading.Event()
        self.thread = threading.Thread(target=self._read, name="collaboration-events", daemon=True)

    def start(self, timeout: float = 15) -> None:
        self.path.parent.mkdir(parents=True, exist_ok=True)
        self.thread.start()
        deadline = time.monotonic() + timeout
        with self.condition:
            while not self.connected:
                self._check()
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise CollaborationEvidenceError("event stream did not connect")
                self.condition.wait(min(.1, remaining))

    def _check(self) -> None:
        if self.error:
            raise CollaborationEvidenceError(self.error)

    def snapshot(self) -> list[dict[str, Any]]:
        with self.condition:
            self._check()
            return list(self.events)

    def _accept(self, item: dict[str, Any], output) -> None:
        with self.condition:
            if item.get("type") == "resync_required":
                raise CollaborationEvidenceError("event replay gap: resync required")
            cursor = (item.get("session_epoch"), item.get("sequence"))
            if any(type(n) is not int or n < 0 for n in cursor) or cursor[0] == 0:
                raise CollaborationEvidenceError("invalid event cursor")
            if self.cursor is not None and cursor[0] != self.cursor[0]:
                raise CollaborationEvidenceError("recorder epoch changed during scenario")
            if item.get("type") == "connected":
                if self.cursor is None:
                    self.cursor = cursor
                self.connected = True
                self.condition.notify_all()
                return
            event_id = item.get("event_id")
            if not event_id or not item.get("kind"):
                raise CollaborationEvidenceError("event omitted identity or kind")
            if event_id in self.by_id:
                if self.by_id[event_id] != item:
                    raise CollaborationEvidenceError("conflicting duplicate event")
                return
            if self.cursor is None or cursor[1] != self.cursor[1] + 1:
                raise CollaborationEvidenceError("event sequence gap")
            if len(self.events) >= 100000:
                raise CollaborationEvidenceError("scenario exceeded event capture limit")
            output.write(json.dumps(item, ensure_ascii=False, separators=(",", ":")) + "\n")
            output.flush()
            os.fsync(output.fileno())
            self.by_id[event_id] = item
            self.events.append(item)
            self.cursor = cursor
            self.condition.notify_all()

    def _read(self) -> None:
        disconnected_at = time.monotonic()
        try:
            with self.path.open("w", encoding="utf-8") as output:
                while not self.stop_event.is_set():
                    with self.condition:
                        cursor = ":".join(map(str, self.cursor)) if self.cursor else ""
                    try:
                        with self.client.open_collaboration_events(cursor, timeout=2) as response:
                            data: list[str] = []
                            size = 0
                            for raw in response:
                                if self.stop_event.is_set():
                                    return
                                line = raw.decode("utf-8").rstrip("\r\n")
                                if line.startswith("data:"):
                                    size += len(raw)
                                    if size > 2 * 1024 * 1024:
                                        raise CollaborationEvidenceError("event exceeds capture limit")
                                    data.append(line[5:].lstrip())
                                elif not line and data:
                                    self._accept(json.loads("\n".join(data)), output)
                                    data, size = [], 0
                                disconnected_at = time.monotonic()
                    except (OSError, EOFError) as exc:
                        if time.monotonic() - disconnected_at > 10:
                            raise CollaborationEvidenceError("event stream reconnect deadline exceeded") from exc
                    self.stop_event.wait(.05)
        except Exception as exc:
            if not self.stop_event.is_set():
                with self.condition:
                    self.error = str(exc)
                    self.condition.notify_all()

    def barrier(self, timeout: float = 10) -> dict[str, Any]:
        status = self.client.collaboration_status(barrier=True)
        if status.get("complete") is not True:
            raise CollaborationEvidenceError("recorder reports incomplete evidence")
        target = status["cursor"]
        deadline = time.monotonic() + timeout
        with self.condition:
            while True:
                self._check()
                if self.cursor and self.cursor[0] != target["session_epoch"]:
                    raise CollaborationEvidenceError("barrier belongs to another recorder")
                if self.cursor and self.cursor[1] >= target["sequence"]:
                    return status
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise CollaborationEvidenceError("event capture did not reach recorder watermark")
                self.condition.wait(min(.1, remaining))

    def wait_for(self, predicate: Callable[[dict[str, Any]], bool], timeout: float) -> dict[str, Any]:
        deadline = time.monotonic() + timeout
        with self.condition:
            while True:
                self._check()
                for event in self.events:
                    if predicate(event):
                        return event
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    raise TimeoutError("collaboration event wait timed out")
                self.condition.wait(min(.1, remaining))

    def close(self) -> None:
        self.stop_event.set()
        if self.thread.ident is not None:
            self.thread.join(timeout=3)
        if self.thread.is_alive():
            raise CollaborationEvidenceError("event collector did not stop")
        self._check()
