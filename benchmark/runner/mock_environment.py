from __future__ import annotations

import base64
import io
import json
import secrets
import threading
import urllib.parse
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any

from PIL import Image, ImageDraw

from mnk_provider import execute_mnk_request
from runner.matching import dict_contains
from runner.suite import MockEnvironmentSpec, MockToolResponseSpec

MAX_REQUEST_BODY_BYTES = 1024 * 1024
REQUEST_TIMEOUT_SECONDS = 5.0


class MockEnvironmentServer:
    """Scripted bridge protected by an unguessable per-instance URL path."""

    def __init__(
        self,
        spec: MockEnvironmentSpec,
        suite_dir: Path,
        host: str = "127.0.0.1",
        port: int = 0,
        auth_token: str = "",
    ):
        self.spec = spec
        self.suite_dir = Path(suite_dir)
        self.host = host
        self.port = port
        configured_token = str(auth_token or "").strip()
        self.auth_token = configured_token or secrets.token_urlsafe(32)
        self.base_path = f"/_aiden_mock/{urllib.parse.quote(self.auth_token, safe='')}"
        self.base_url = ""
        self.screen_text = spec.screen_text or "Mock phone environment ready."
        self.calls: list[dict[str, Any]] = []
        self._lock = threading.Lock()
        self._screenshot_seq = 0
        self._httpd: ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None

    @property
    def redacted_url(self) -> str:
        if not self.base_url:
            return ""
        parsed = urllib.parse.urlparse(self.base_url)
        return urllib.parse.urlunparse(parsed._replace(path="/_aiden_mock/REDACTED"))

    def start(self) -> str:
        if self._httpd is not None:
            return self.base_url
        self._httpd = ThreadingHTTPServer((self.host, self.port), _handler_for(self))
        self._httpd.daemon_threads = True
        bound_host, bound_port = self._httpd.server_address[:2]
        advertised_host = "127.0.0.1" if bound_host == "0.0.0.0" else bound_host
        self.base_url = f"http://{advertised_host}:{bound_port}{self.base_path}"
        self._thread = threading.Thread(
            target=self._httpd.serve_forever,
            name="benchmark-mock-environment",
            daemon=True,
        )
        self._thread.start()
        return self.base_url

    def stop(self) -> None:
        if self._httpd is None:
            return
        self._httpd.shutdown()
        self._httpd.server_close()
        if self._thread is not None:
            self._thread.join(timeout=2)
        self._httpd = None
        self._thread = None

    def activate(self, spec: MockEnvironmentSpec) -> None:
        """Switch fixtures between tasks while no requests are in flight.

        The mock bridge advertises concurrency one to preserve this invariant.
        """
        with self._lock:
            self.spec = spec
            self.calls.clear()
            self.screen_text = spec.screen_text or "Mock phone environment ready."

    def reset(self) -> None:
        with self._lock:
            self.calls.clear()
            self.screen_text = self.spec.screen_text or "Mock phone environment ready."

    def invoke(self, tool_name: str, tool_input: dict[str, Any]) -> MockToolResponseSpec:
        with self._lock:
            self.calls.append({"tool": tool_name, "input": tool_input})
            for response in self.spec.tools.get(tool_name, []):
                screen_matches = (
                    not response.screen_contains
                    or response.screen_contains in self.screen_text
                )
                if screen_matches and dict_contains(
                    tool_input, response.input_contains
                ):
                    if response.screen_text:
                        self.screen_text = response.screen_text
                    return response
            if self.spec.default_tool_response is not None:
                response = self.spec.default_tool_response
                if response.screen_text:
                    self.screen_text = response.screen_text
                return response
        return MockToolResponseSpec(
            output={"ok": False, "error": f"no mock response configured for {tool_name}"},
            is_error=True,
            error=f"no mock response configured for {tool_name}",
        )

    def screenshot_payload(self) -> dict[str, Any]:
        image = self._screen_image()
        width, height = image.size
        output = io.BytesIO()
        image.save(output, format="JPEG", quality=88)
        jpeg = output.getvalue()
        with self._lock:
            self._screenshot_seq += 1
            seq = self._screenshot_seq
        return {
            "data": base64.b64encode(jpeg).decode("ascii"),
            "width": width,
            "height": height,
            "format": "jpeg",
            "bytes": len(jpeg),
            "seq": seq,
            "description": self.screen_text,
        }

    def snapshot(self) -> dict[str, Any]:
        with self._lock:
            calls = list(self.calls)
            screen_text = self.screen_text
        return {
            "platform": self.spec.platform,
            "phone_bridge": dict(self.spec.phone_bridge),
            "screen_text": screen_text,
            "calls": calls,
        }

    def _screen_image(self) -> Image.Image:
        if self.spec.screen:
            screen_path = self.suite_dir / self.spec.screen
            try:
                with Image.open(screen_path) as image:
                    image.load()
                    return image.convert("RGB")
            except OSError:
                pass
        image = Image.new("RGB", (1170, 2532), "#f5f5f7")
        draw = ImageDraw.Draw(image)
        draw.rounded_rectangle(
            (45, 55, 1125, 2470),
            radius=55,
            fill="white",
            outline="#c7c7cc",
            width=4,
        )
        draw.text((95, 105), "Aiden Benchmark Mock Phone", fill="#111111")
        y = 180
        for line in _wrap_text(self.screen_text, width=72):
            draw.text((95, y), line, fill="#222222")
            y += 38
        return image


def _handler_for(server: MockEnvironmentServer):
    class Handler(BaseHTTPRequestHandler):
        def setup(self) -> None:
            super().setup()
            self.connection.settimeout(REQUEST_TIMEOUT_SECONDS)

        def do_GET(self) -> None:
            path = self._authorized_path()
            if path is None:
                self._json(404, {"ok": False, "error": {"code": "not_found"}})
                return
            if path == "/health":
                self._json(
                    200,
                    {
                        "ok": True,
                        "data": {
                            "bridge_type": "mock",
                            "platform": server.spec.platform,
                        },
                    },
                )
                return
            if path == "/api/phone-bridge/status":
                # The Agent proxy decodes the native status object, without an envelope.
                with server._lock:
                    status = {
                        "connected": False,
                        **server.spec.phone_bridge,
                        "platform": server.spec.platform,
                    }
                    # An enabled scripted FGS models an actively polling service.
                    # Preserve explicit timestamps (including stale/null fixtures).
                    if status.get("fgs_bridge_enabled"):
                        status.setdefault(
                            "fgs_bridge_updated_at",
                            datetime.now(timezone.utc).isoformat(),
                        )
                self._json(200, status)
                return
            if path == "/api/concurrent":
                self._json(
                    200,
                    {
                        "ok": True,
                        "data": {"bridge_type": "mock", "concurrent": 1},
                    },
                )
                return
            if path == "/api/tools":
                names = sorted(set(server.spec.tools))
                self._json(
                    200,
                    {
                        "tools": [
                            {
                                "name": name,
                                "category": "mock",
                                "description": f"Scripted mock response for {name}.",
                                "input_mode": "json",
                                "example_input": "{}",
                                "args_schema": {"type": "object"},
                                "http": {
                                    "method": "POST",
                                    "path": f"{server.base_path}/api/tools/{name}",
                                },
                            }
                            for name in names
                        ]
                    },
                )
                return
            if path == "/api/state":
                self._json(200, {"ok": True, "data": server.snapshot()})
                return
            self._json(
                404,
                {"ok": False, "error": {"code": "not_found", "message": path}},
            )

        def do_POST(self) -> None:
            path = self._authorized_path()
            if path is None:
                self._json(404, {"ok": False, "error": {"code": "not_found"}})
                return
            if path == "/api/setup":
                server.reset()
                self._json(200, {"ok": True, "data": {"setup": True}})
                return
            if path == "/api/release":
                self._json(200, {"ok": True, "data": {"released": True}})
                return
            if path == "/api/providers/screenshot":
                payload = self._read_json()
                if payload is None:
                    return
                shot = server.screenshot_payload()
                self._json(
                    200,
                    {
                        "ok": True,
                        "data": {
                            "meta": {
                                "seq": shot["seq"],
                                "width": shot["width"],
                                "height": shot["height"],
                                "source_width": shot["width"],
                                "source_height": shot["height"],
                                "crop_x": 0,
                                "crop_y": 0,
                                "crop_width": shot["width"],
                                "crop_height": shot["height"],
                                "pixel_format": shot.get("format") or "jpeg",
                                "stride": 0,
                                "bytes": shot["bytes"],
                                "stale": False,
                            },
                            "capture_info": {"capture_backend": "mock"},
                            "image": shot["data"],
                        },
                    },
                )
                return
            if path == "/api/providers/mnk":
                payload = self._read_json()
                if payload is None:
                    return
                def invoke(tool_name: str, tool_input: dict[str, Any]) -> dict[str, Any]:
                    result = server.invoke(tool_name, tool_input)
                    return {
                        "output": result.output,
                        "is_error": result.is_error,
                        "error": result.error,
                    }

                status, response = execute_mnk_request(payload, invoke)
                self._json(status, response)
                return
            if path.startswith("/api/tools/"):
                tool_name = path.removeprefix("/api/tools/").strip()
                payload = self._read_json()
                if payload is None:
                    return
                tool_input = _decode_tool_input(payload.get("input"))
                response = server.invoke(tool_name, tool_input)
                output = (
                    response.output
                    if isinstance(response.output, str)
                    else json.dumps(response.output, ensure_ascii=False)
                )
                self._json(
                    200,
                    {
                        "output": output,
                        "is_error": response.is_error,
                        "error": response.error,
                        "duration_ms": 0,
                    },
                )
                return
            self._json(
                404,
                {"ok": False, "error": {"code": "not_found", "message": path}},
            )

        def _authorized_path(self) -> str | None:
            path = urllib.parse.urlparse(self.path).path
            if path == server.base_path:
                return "/"
            prefix = server.base_path + "/"
            if path.startswith(prefix):
                return path[len(server.base_path) :]
            return None

        def _read_json(self) -> dict[str, Any] | None:
            try:
                length = int(self.headers.get("Content-Length", "0"))
                if length < 0 or length > MAX_REQUEST_BODY_BYTES:
                    raise ValueError(
                        f"Content-Length must be between 0 and {MAX_REQUEST_BODY_BYTES}"
                    )
                raw = self.rfile.read(length) if length else b"{}"
                payload = json.loads(raw.decode("utf-8"))
            except (
                OSError,
                ValueError,
                UnicodeDecodeError,
                json.JSONDecodeError,
            ) as exc:
                self._json(
                    400,
                    {
                        "ok": False,
                        "error": {"code": "bad_request", "message": str(exc)},
                    },
                )
                return None
            if not isinstance(payload, dict):
                self._json(
                    400,
                    {
                        "ok": False,
                        "error": {
                            "code": "bad_request",
                            "message": "object required",
                        },
                    },
                )
                return None
            return payload

        def _json(self, status: int, payload: dict[str, Any]) -> None:
            data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

        def log_message(self, format: str, *args: Any) -> None:
            return

    return Handler


def _decode_tool_input(raw: Any) -> dict[str, Any]:
    if isinstance(raw, dict):
        return raw
    if not isinstance(raw, str) or not raw.strip():
        return {}
    try:
        decoded = json.loads(raw)
    except json.JSONDecodeError:
        return {"raw_input": raw}
    return decoded if isinstance(decoded, dict) else {"value": decoded}


def _wrap_text(text: str, width: int) -> list[str]:
    lines: list[str] = []
    for paragraph in str(text or "").splitlines() or [""]:
        remaining = paragraph
        if not remaining:
            lines.append("")
            continue
        while len(remaining) > width:
            lines.append(remaining[:width])
            remaining = remaining[width:]
        lines.append(remaining)
    return lines
