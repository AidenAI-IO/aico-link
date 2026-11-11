from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

from mnk_provider import execute_mnk_request
from setup_token_registry import SetupTokenRegistry, setup_token_from_payload

from .device import DesktopDevice, DesktopDeviceError
from .protocol import bridge_error, bridge_ok, encode_provider_frame
from .state import DesktopBridgeState, NoBridgeEnvAvailableError, benchmark_task_id_from_headers
from .tools_api import DesktopToolsAPIHandler, MAX_REQUEST_BODY_BYTES


class DesktopBridgeServer:
    def __init__(self, device: DesktopDevice | None = None, host: str = "127.0.0.1", port: int = 0, action_settle_sec: float = 0.2, *, tools_factory=DesktopToolsAPIHandler, mnk_executor=execute_mnk_request):
        self.state = DesktopBridgeState(device=device or DesktopDevice())
        self.host, self.port = host, int(port)
        self.tools_api = tools_factory(self.state, action_settle_sec)
        self.mnk_executor = mnk_executor
        self.bridge_type = getattr(self.state.device, "bridge_type", "desktop")
        self.setup_tokens = SetupTokenRegistry()
        self._httpd: ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None
        self.base_url = ""

    def start(self) -> str:
        if self._httpd is not None: return self.base_url
        self._httpd = ThreadingHTTPServer((self.host, self.port), _handler_for(self)); self._httpd.daemon_threads = True
        host, port = self._httpd.server_address[:2]; self.base_url = f"http://{host}:{port}"
        self._thread = threading.Thread(target=self._httpd.serve_forever, name="desktop-environment-bridge", daemon=True); self._thread.start(); return self.base_url

    def stop(self) -> None:
        if self._httpd is None: return
        self._httpd.shutdown(); self._httpd.server_close()
        if self._thread is not None: self._thread.join(timeout=2)
        self._httpd = None; self._thread = None


def _handler_for(bridge: DesktopBridgeServer):
    class Handler(BaseHTTPRequestHandler):
        timeout = 30

        def do_GET(self) -> None:
            path = self.path.split("?", 1)[0]
            if path == "/api/providers/mnk":
                self._send(200, {"capabilities": getattr(bridge.state.device, "mnk_capabilities", {})}); return
            if path == "/api/tools": bridge.tools_api.handle_request(self, path); return
            if path == "/api/concurrent": self._send(200, bridge_ok({"bridge_type": bridge.bridge_type, "concurrent": 1, "env_count": 1, "active_task_id": bridge.state.active_task_id})); return
            if path == "/health":
                try: info = bridge.state.device.check_device()
                except Exception as exc: self._send_error(503, "desktop_unavailable", str(exc)); return
                self._send(200, bridge_ok({"status": "ok", "bridge_type": bridge.bridge_type, "platform": bridge.state.device.platform, "env_count": 1, "concurrent": 1, "active_task_id": bridge.state.active_task_id, "active_episode_id": bridge.state.active_episode_id, "active_task_lease_state": bridge.state.active_task_lease_state(), "interfaces": ["/api/tools", "/api/providers/screenshot", "/api/providers/mnk", "/api/setup", "/api/release", "/api/concurrent"], **info})); return
            self._send_error(404, "not_found", "unknown endpoint")

        def do_POST(self) -> None:
            path = self.path.split("?", 1)[0]
            if path.startswith("/api/tools/"): bridge.tools_api.handle_request(self, path); return
            try: payload = self._read_json()
            except Exception as exc: self._send_error(400, "bad_json", str(exc)); return
            try:
                if path == "/api/setup": self._setup(payload)
                elif path == "/api/release" or path == "/release": self._release()
                elif path == "/api/providers/screenshot": self._screenshot(payload)
                elif path == "/api/providers/mnk": self._mnk(payload)
                else: self._send_error(404, "not_found", "unknown endpoint")
            except NoBridgeEnvAvailableError as exc: self._send_error(429, "no_bridge_env_available", str(exc))
            except DesktopDeviceError as exc: self._send_error(503, "desktop_unavailable", str(exc))
            except ValueError as exc: self._send_error(400, "bad_request", str(exc))
            except Exception as exc: self._send_error(500, "bridge_error", str(exc))

        def log_message(self, format: str, *args: Any) -> None: return

        def _setup(self, payload: dict[str, Any]) -> None:
            task_id = benchmark_task_id_from_headers(self.headers)
            token = setup_token_from_payload(payload)

            def operation() -> dict[str, Any]:
                episode, newly_acquired = bridge.state.acquire(task_id)
                try:
                    bridge.state.device.check_device()
                except Exception:
                    if newly_acquired:
                        bridge.state.release(task_id)
                    raise
                return {"episode_id": episode, "reset": False, "task_id": task_id}

            result = bridge.setup_tokens.run((task_id, token), operation) if token else operation()
            self._send(200, bridge_ok(result))

        def _release(self) -> None:
            task_id = benchmark_task_id_from_headers(self.headers)
            released = bridge.state.release(task_id)
            if released:
                bridge.setup_tokens.clear_completed_for_task(task_id)
            self._send(200, bridge_ok({"released": released}))

        def _screenshot(self, payload: dict[str, Any]) -> None:
            task_id = benchmark_task_id_from_headers(self.headers); bridge.state.check_task_access(task_id)
            quality = int(payload.get("quality", 85) or 85)
            try:
                image, width, height = bridge.state.device.screenshot_jpeg(quality)
            except TypeError:
                image, width, height = bridge.state.device.screenshot_jpeg()
            bridge.state.screenshot_seq += 1
            self._send(200, bridge_ok(encode_provider_frame(image, width=width, height=height, seq=bridge.state.screenshot_seq, backend=getattr(bridge.state.device, "backend", "desktop"))))

        def _mnk(self, payload: dict[str, Any]) -> None:
            task_id = benchmark_task_id_from_headers(self.headers); bridge.state.check_task_access(task_id); status, response = bridge.mnk_executor(payload, bridge.tools_api.invoke); self._send(status, response)

        def _read_json(self) -> dict[str, Any]:
            length = int(self.headers.get("Content-Length", "0") or "0")
            if length < 0:
                raise ValueError("Content-Length must be non-negative")
            if length > MAX_REQUEST_BODY_BYTES: raise ValueError("request body too large")
            raw = self.rfile.read(length) if length else b"{}"; value = json.loads(raw.decode("utf-8"))
            if not isinstance(value, dict): raise ValueError("JSON body must be an object")
            return value

        def _send_error(self, status: int, code: str, message: str) -> None: self._send(status, bridge_error(code, message, status))
        def _send(self, status: int, payload: dict[str, Any]) -> None:
            data = json.dumps(payload, ensure_ascii=False).encode("utf-8"); self.send_response(status); self.send_header("Content-Type", "application/json"); self.send_header("Cache-Control", "no-store"); self.send_header("Content-Length", str(len(data))); self.end_headers(); self.wfile.write(data)

    return Handler
