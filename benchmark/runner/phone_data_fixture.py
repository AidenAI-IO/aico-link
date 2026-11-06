"""Controlled phone effects; only Agent tool invocations can change the state."""
from __future__ import annotations

import copy
import secrets
import threading
import time
from typing import Any, Callable

from runner.matching import dict_contains


class PhoneDataFixture:
    tools = {"bridge_clipboard", "bridge_notification"}

    def __init__(self, spec: dict[str, Any]):
        self._condition = threading.Condition()
        self._closed = False
        self._released: set[str] = set()
        self.spec = spec
        self.state = copy.deepcopy(spec.get("state", {}))
        if self.state.get("clipboard") == {"generate": "token"}:
            self.state["clipboard"] = secrets.token_hex(12)
        self.state.setdefault("clipboard", "")
        self.state.setdefault("notifications", [])
        self.initial_state = copy.deepcopy(self.state)
        self.events: list[dict[str, Any]] = []

    def _record(self, kind: str, **payload: Any) -> None:
        sequence = len(self.events) + 1
        self.events.append({"event_id": f"fixture-{sequence}", "sequence": sequence,
                            "kind": kind, "payload": payload})
        self._condition.notify_all()

    def snapshot(self) -> dict[str, Any]:
        with self._condition:
            return copy.deepcopy({"state": self.state, "initial_state": self.initial_state,
                                  "events": self.events})

    def release(self, gate: str, daemon_watermark: dict[str, int] | None = None) -> None:
        with self._condition:
            if gate not in {item["id"] for item in self.spec.get("gates", [])}:
                raise ValueError(f"unknown fixture gate: {gate}")
            self._released.add(gate)
            self._record("fixture.gate", phase="released", gate=gate, daemon_watermark=daemon_watermark or {})

    def close(self) -> None:
        with self._condition:
            self._closed = True
            self._condition.notify_all()

    def invoke(self, tool: str, args: dict[str, Any], *, task_id: str, call_id: str,
               canceled: Callable[[], bool]) -> dict[str, Any]:
        with self._condition:
            identity = {"task_id": task_id, "call_id": call_id, "operation": tool,
                        "input": copy.deepcopy(args)}
            self._record("fixture.call", phase="accepted", **identity)
            for gate in self.spec.get("gates", []):
                if gate["tool"] != tool or not dict_contains(args, gate.get("input_contains", {})):
                    continue
                if gate["id"] in self._released:
                    continue
                if gate.get("at") == "permission":
                    error = gate["error"]
                    self._record("fixture.gate", phase="denied", gate=gate["id"], **identity)
                    self._record("fixture.call", phase="failed", error=error, **identity)
                    return {"ok": False, "error": error}
                self._record("fixture.gate", phase="entered", gate=gate["id"], **identity)
                deadline = time.monotonic() + gate.get("timeout_sec", 120)
                while gate["id"] not in self._released:
                    if self._closed or canceled():
                        self._record("fixture.call", phase="cancelled", **identity)
                        return {"ok": False, "error": "fixture operation cancelled"}
                    if time.monotonic() >= deadline:
                        self._record("fixture.call", phase="expired", **identity)
                        return {"ok": False, "error": "fixture gate deadline exceeded"}
                    self._condition.wait(timeout=.05)
            if self._closed or canceled():
                self._record("fixture.call", phase="cancelled", **identity)
                return {"ok": False, "error": "fixture operation cancelled"}
            failure = self.spec.get("failures", {}).get(tool)
            if failure:
                self._record("fixture.call", phase="failed", error=failure, **identity)
                return {"ok": False, "error": failure}
            if tool == "bridge_clipboard" and args.get("action") == "read":
                result = {"ok": True, "text": self.state["clipboard"]}
            elif tool == "bridge_clipboard" and args.get("action") == "write" and isinstance(args.get("text"), str):
                self.state["clipboard"] = args["text"]
                self._record("fixture.effect", **identity)
                result = {"ok": True}
            elif (tool == "bridge_notification" and args.get("action", "send") == "send"
                  and not args.get("schedule_at") and isinstance(args.get("title"), str)
                  and isinstance(args.get("body"), str)):
                self.state["notifications"].append(copy.deepcopy(args))
                self._record("fixture.effect", **identity)
                result = {"ok": True, "notification_id": f"notification-{len(self.state['notifications'])}"}
            else:
                self._record("fixture.call", phase="failed", **identity)
                return {"ok": False, "error": "unsupported fixture operation"}
            self._record("fixture.call", phase="completed", result=result, **identity)
            return result
