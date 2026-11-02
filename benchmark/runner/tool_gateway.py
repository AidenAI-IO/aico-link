"""Allowlisted device UI tool endpoint for external agent harnesses.

External agents never receive a backend URL. The gateway serves only the frozen
business tool catalog from one backend: the Aiden daemon tool API, or neutral
raw primitives on the environment bridge. It records one audit stream, with
step screenshots, so every external harness is scored from the same evidence
regardless of how it talks to the tools (extension or MCP).
"""
from __future__ import annotations

import base64
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import json
from pathlib import Path
import threading
import time
from typing import Any, Protocol
import urllib.error
import urllib.request


AUDIT_FILE = "tool-audit.jsonl"
DAEMON_TOOL_TIMEOUT_SEC = 300


def action_failed(output: Any) -> bool:
    """True when the business action reported `ok:false`, even beside a screenshot."""
    if isinstance(output, str):
        try:
            output = json.loads(output)
        except ValueError:
            return False
    if not isinstance(output, dict):
        return False
    action = output.get("action_output", output)
    if isinstance(action, str):
        try:
            action = json.loads(action)
        except ValueError:
            return False
    return isinstance(action, dict) and action.get("ok") is False


def _screenshot(output: Any) -> tuple[bytes, str] | None:
    try:
        decoded = json.loads(output) if isinstance(output, str) else output
    except ValueError:
        return None
    if (isinstance(decoded, dict) and isinstance(decoded.get("data"), str)
            and decoded.get("format") in {"jpeg", "jpg", "png", "webp"}):
        suffix = ".png" if decoded["format"] == "png" else ".jpg"
        try:
            return base64.b64decode(decoded["data"]), suffix
        except ValueError:
            return None
    return None


class ToolBackend(Protocol):
    def invoke(self, name: str, tool_input: dict[str, Any]) -> tuple[int, dict]:
        """Return an HTTP status and a daemon-shaped tool response."""
        ...


class DaemonToolBackend:
    """Forward calls to the Aiden daemon tool API (the Aiden tool layer)."""

    def __init__(self, daemon_url: str):
        self.daemon_url = daemon_url.rstrip("/")

    def invoke(self, name: str, tool_input: dict[str, Any]) -> tuple[int, dict]:
        request = urllib.request.Request(f"{self.daemon_url}/api/tools/{name}",
                                         data=json.dumps({"input": tool_input}).encode(),
                                         headers={"Content-Type": "application/json"}, method="POST")
        try:
            with urllib.request.urlopen(request, timeout=DAEMON_TOOL_TIMEOUT_SEC) as response:
                return response.status, json.load(response)
        except urllib.error.HTTPError as exc:
            return exc.code, {"error": exc.read().decode("utf-8", "replace")[:2000], "is_error": True}
        except (OSError, ValueError) as exc:
            return 502, {"error": str(exc), "is_error": True}


class ToolGateway:
    """Serve `GET /api/tools` and `POST /api/tools/<name>` for one attempt."""

    def __init__(self, backend: ToolBackend, catalog: list[dict], artifacts: Path):
        self.backend = backend
        self.catalog = catalog
        self.tools = {tool["name"]: tool for tool in catalog}
        self.artifacts = artifacts
        self._lock = threading.Lock()
        self._calls = 0
        self._server: ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None

    @property
    def url(self) -> str:
        assert self._server is not None
        return f"http://127.0.0.1:{self._server.server_port}"

    def __enter__(self) -> ToolGateway:
        gateway = self

        class Handler(BaseHTTPRequestHandler):
            def do_GET(self):
                if self.path == "/api/tools":
                    self._send(200, {"tools": gateway.catalog})
                else:
                    self._send(404, {"error": "not found"})

            def do_POST(self):
                prefix = "/api/tools/"
                name = self.path[len(prefix):] if self.path.startswith(prefix) else ""
                if name not in gateway.tools:
                    self._send(404, {"error": f"Unknown business tool: {name}"})
                    return
                body = self.rfile.read(int(self.headers.get("Content-Length") or 0))
                status, payload = gateway.invoke(name, body)
                self._send(status, payload)

            def _send(self, status: int, payload: dict) -> None:
                data = json.dumps(payload, ensure_ascii=False).encode()
                try:
                    self.send_response(status)
                    self.send_header("Content-Type", "application/json")
                    self.send_header("Content-Length", str(len(data)))
                    self.end_headers()
                    self.wfile.write(data)
                except (BrokenPipeError, ConnectionResetError):
                    # The harness was stopped at its deadline while this call ran;
                    # the call is already audited and nobody is left to read the reply.
                    pass

            def log_message(self, *_):
                pass

        self._server = ThreadingHTTPServer(("127.0.0.1", 0), Handler)
        self._thread = threading.Thread(target=self._server.serve_forever, daemon=True)
        self._thread.start()
        return self

    def __exit__(self, *_) -> None:
        if self._server is not None:
            self._server.shutdown()
            self._server.server_close()
        if self._thread is not None:
            self._thread.join(timeout=5)

    def invoke(self, name: str, body: bytes) -> tuple[int, dict]:
        """Forward one call and normalize `is_error` to include business failures."""
        started = time.monotonic()
        try:
            tool_input = json.loads(body or b"{}").get("input", {})
        except (ValueError, AttributeError):
            tool_input = None
        if not isinstance(tool_input, dict):
            status, payload = 400, {"error": "Request body must be an object with an input object", "is_error": True}
            tool_input = {}
        else:
            status, payload = self.backend.invoke(name, tool_input)
        if not isinstance(payload, dict):
            status, payload = 502, {"error": "daemon returned a non-object tool response", "is_error": True}
        output = payload.get("output", payload.get("error", ""))
        payload["is_error"] = (bool(payload.get("is_error")) or payload.get("tool_error") is not None
                               or action_failed(output))
        raw_input = payload.get("raw_input") or json.dumps(tool_input, ensure_ascii=False)
        self._audit(name, raw_input, payload, output, int((time.monotonic() - started) * 1000))
        return status, payload

    def _audit(self, name: str, raw_input: str, payload: dict, output: Any, elapsed_ms: int) -> None:
        shot = _screenshot(output)
        with self._lock:
            self._calls += 1
            record = {"tool_name": name, "raw_input": raw_input, "is_error": payload["is_error"],
                      "tool_error": payload.get("tool_error") or payload.get("error"),
                      "duration_ms": payload.get("duration_ms", elapsed_ms),
                      "observation": "screenshot observation" if shot else
                      (output if isinstance(output, str) else json.dumps(output, ensure_ascii=False)),
                      "has_screenshot": shot is not None}
            if shot:
                path = self.artifacts / f"step_{self._calls}{shot[1]}"
                path.write_bytes(shot[0])
                record["screenshot_file"] = path.name
            with (self.artifacts / AUDIT_FILE).open("a", encoding="utf-8") as stream:
                stream.write(json.dumps(record, ensure_ascii=False) + "\n")
