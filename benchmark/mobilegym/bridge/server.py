from __future__ import annotations

import asyncio
import base64
import json
import logging
import socket
import threading
import time
import urllib.parse
import uuid
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from typing import Any

from mnk_provider import execute_mnk_request
from setup_token_registry import SetupTokenRegistry, setup_token_from_payload

from .actions import action_to_dict, build_action
from .episode import (
    BridgeEpisodeState,
    BridgeTaskRouter,
    MissingBenchmarkTaskIDError,
    NoBridgeEnvAvailableError,
    StaleEpisodeError,
    benchmark_task_id_from_headers,
)
from .protocol import (
    bridge_error,
    bridge_ok,
    encode_image_as_format,
    encode_provider_frame,
    encode_screenshot,
)
from .tools_api import ToolsAPIHandler, future_result


ACTION_ENDPOINTS = {"tap", "swipe", "drag", "type_text", "key", "back", "home", "wait"}

# 180s upper bound on a single *action/setup* bridge request. This is
# deliberately generous:
# - Chromium-backed MobileGym actions occasionally stall on heavy pages (image
#   decodes, page-load chains, animations) for tens of seconds before settling.
# - Episode reset has its own 45s ceiling (EPISODE_RESET_TIMEOUT_SEC); after a
#   reset failure the resilient-reset path may close+recreate the env+page,
#   which can take another minute on a cold image.
# - 180s lets a single slow action complete instead of cascading retries, while
#   still bounding a stuck slot tightly enough that parallel callers can move on.
# Screenshot capture is a separate, shorter budget: clients and this handler
# both fail a hung POST /api/providers/screenshot at 30s.
DEFAULT_BRIDGE_REQUEST_TIMEOUT_SEC = 180
SCREENSHOT_PROVIDER_TIMEOUT_SEC = 30
RELEASE_EPISODE_TIMEOUT_SEC = 5

logger = logging.getLogger(__name__)


def _setup_app_ids(payload: dict[str, Any]) -> list[str]:
    value = payload.get("app_ids")
    if value is None:
        return []
    if not isinstance(value, list) or not all(
        isinstance(item, str) and item.strip() for item in value
    ):
        raise ValueError("app_ids must be a list of non-empty strings")
    return list(dict.fromkeys(item.strip() for item in value))


def _foreground_app_id(payload: dict[str, Any]) -> str | None:
    value = payload.get("foreground_app_id")
    if value is None:
        return None
    if not isinstance(value, str) or not value.strip() or value != value.strip():
        raise ValueError("foreground_app_id must be a non-empty app ID")
    return value


async def _foreground_app(env: Any, app_id: str) -> None:
    page = getattr(env, "page", None)
    if page is None:
        raise RuntimeError("environment does not expose a browser page")
    await page.evaluate(
        """(appId) => {
            const apps = window.__SIM__?.getState?.()?.os?.installedApps || [];
            if (!apps.some(app => app.id === appId)) throw new Error(`app is not installed: ${appId}`);
            if (!window.__OS__?.openApp) throw new Error('window.__OS__.openApp is unavailable');
            window.__OS__.openApp(appId, '/');
        }""",
        app_id,
    )
    await asyncio.sleep(0.8)


class BridgeServer:
    def __init__(
        self,
        state: BridgeEpisodeState | BridgeTaskRouter,
        host: str = "127.0.0.1",
        port: int = 0,
        public_host: str | None = None,
        request_timeout_sec: float = DEFAULT_BRIDGE_REQUEST_TIMEOUT_SEC,
    ):
        self.router = BridgeTaskRouter.from_state(state)
        self.state = self.router.default_state
        self.host = host
        self.port = port
        self.public_host = public_host
        self.request_timeout_sec = request_timeout_sec
        self._httpd: ThreadingHTTPServer | None = None
        self._thread: threading.Thread | None = None
        self.base_url = ""
        self.tools_api = ToolsAPIHandler(self.router, request_timeout_sec)
        self.setup_tokens = SetupTokenRegistry()

    def start(self) -> str:
        if self._httpd is not None:
            return self.base_url
        handler = _handler_for(self)
        self._httpd = ThreadingHTTPServer((self.host, self.port), handler)
        self._httpd.daemon_threads = True
        host, port = self._httpd.server_address[:2]
        self.base_url = f"http://{self._public_host(host)}:{port}"
        self._thread = threading.Thread(target=self._httpd.serve_forever, name="mobilegym-bridge-http", daemon=True)
        self._thread.start()
        return self.base_url

    def _public_host(self, bound_host: str) -> str:
        if self.public_host:
            return self.public_host
        if bound_host in {"", "0.0.0.0", "::"}:
            return _get_container_ip() or socket.gethostname()
        return bound_host

    def stop(self) -> None:
        if self._httpd is None:
            return
        self._httpd.shutdown()
        self._httpd.server_close()
        if self._thread is not None:
            self._thread.join(timeout=2)
        self._httpd = None
        self._thread = None

    def submit(self, coro: Any, *, timeout: float | None = None) -> Any:
        future = asyncio.run_coroutine_threadsafe(coro, self.state.owner_loop)
        return future_result(
            future,
            self.request_timeout_sec if timeout is None else timeout,
        )

    def submit_to_state(self, state: BridgeEpisodeState, coro: Any, *, timeout: float | None = None) -> Any:
        future = asyncio.run_coroutine_threadsafe(coro, state.owner_loop)
        return future_result(
            future,
            self.request_timeout_sec if timeout is None else timeout,
        )


def _handler_for(bridge: BridgeServer):
    class BridgeRequestHandler(BaseHTTPRequestHandler):
        def do_GET(self) -> None:
            path = urllib.parse.urlparse(self.path).path
            # Handle /api/tools catalog
            if path == "/api/tools":
                bridge.tools_api.handle_request(self, path)
                return
            if path == "/api/concurrent":
                self._send_json(200, bridge_ok(_concurrent_payload(bridge)))
                return
            if path != "/health":
                self._send_error(404, "not_found", "unknown endpoint")
                return
            self._send_json(200, bridge_ok(_health_payload(bridge)))

        def do_POST(self) -> None:
            parsed = urllib.parse.urlparse(self.path)
            request_path = parsed.path
            # Handle /api/tools/{tool_name} invocation
            if request_path.startswith("/api/tools/"):
                bridge.tools_api.handle_request(self, request_path)
                return

            payload = self._read_json()
            if payload is None:
                return
            path = request_path.strip("/")
            try:
                if path == "episode/start":
                    self._handle_episode_start(payload)
                elif path == "episode/end":
                    self._handle_episode_end(payload)
                elif path == "api/setup":
                    self._handle_setup(payload)
                elif path in {"api/release", "release"}:
                    self._handle_release()
                elif path == "state":
                    self._handle_state(payload)
                elif path == "route":
                    self._handle_route()
                elif path == "screenshot":
                    self._handle_screenshot(payload)
                elif path == "api/providers/screenshot":
                    self._handle_provider_screenshot(payload)
                elif path == "api/providers/mnk":
                    self._handle_provider_mnk(payload)
                elif path in ACTION_ENDPOINTS:
                    self._handle_action(path, payload)
                else:
                    self._send_error(404, "not_found", "unknown endpoint")
            except StaleEpisodeError:
                self._send_error(409, "stale_episode", "stale episode_id")
            except MissingBenchmarkTaskIDError as exc:
                self._send_error(400, "missing_benchmark_task_id", str(exc))
            except NoBridgeEnvAvailableError as exc:
                self._send_error(429, "no_bridge_env_available", str(exc))
            except ValueError as exc:
                self._send_error(400, "bad_request", str(exc))
            except TimeoutError as exc:
                self._send_error(504, "timeout", str(exc) or "bridge request timed out")
            except Exception as exc:
                self._send_error(500, "bridge_error", str(exc))

        def log_message(self, format: str, *args: Any) -> None:
            return

        def _handle_episode_start(self, payload: dict[str, Any]) -> None:
            state = self._request_state()
            result = bridge.submit_to_state(state, state.start_episode(str(payload.get("episode_id", ""))))
            self._send_json(200, bridge_ok(result))

        def _handle_episode_end(self, payload: dict[str, Any]) -> None:
            state = self._request_state()
            result = bridge.submit_to_state(state, state.end_episode(str(payload.get("episode_id", ""))))
            self._send_json(200, bridge_ok(result))

        def _handle_setup(self, payload: dict[str, Any]) -> None:
            episode_id = str(payload.get("episode_id") or "").strip()
            if not episode_id:
                episode_id = f"reset-{uuid.uuid4().hex}"
            task_id = benchmark_task_id_from_headers(self.headers)
            setup_token = setup_token_from_payload(payload)
            app_ids = _setup_app_ids(payload)
            foreground_app_id = _foreground_app_id(payload)

            def setup() -> dict[str, Any]:
                state = bridge.router.state_for_task_id(task_id)
                result = bridge.submit_to_state(
                    state,
                    state.reset_episode(episode_id, app_ids=app_ids),
                )
                if foreground_app_id:
                    bridge.submit_to_state(
                        state,
                        state.run_env(lambda env: _foreground_app(env, foreground_app_id)),
                    )
                return result

            if setup_token:
                result = bridge.setup_tokens.run(
                    (task_id, setup_token, tuple(app_ids), foreground_app_id),
                    setup,
                )
            else:
                result = setup()
            self._send_json(200, bridge_ok(result))

        def _handle_release(self) -> None:
            task_id = benchmark_task_id_from_headers(self.headers)
            state = bridge.router.existing_state_for_task_id(task_id)
            if state is not None and state.active_episode_id:
                try:
                    bridge.submit_to_state(
                        state,
                        state.end_episode(state.active_episode_id),
                        timeout=min(bridge.request_timeout_sec, RELEASE_EPISODE_TIMEOUT_SEC),
                    )
                except Exception as exc:
                    logger.warning("Could not end MobileGym episode during release for task %s: %s", task_id, exc)
            released = bridge.router.release_task_id(task_id)
            bridge.setup_tokens.clear_completed_for_task(task_id)
            self._send_json(200, bridge_ok({"released": released}))

        def _handle_state(self, payload: dict[str, Any]) -> None:
            state = self._request_state()
            result = bridge.submit_to_state(state, state.run_env(lambda env: env.get_state(**payload)))
            self._send_json(200, bridge_ok(result))

        def _handle_route(self) -> None:
            state = self._request_state()
            result = bridge.submit_to_state(state, state.run_env(lambda env: env.get_route()))
            self._send_json(200, bridge_ok(result))

        def _handle_screenshot(self, payload: dict[str, Any]) -> None:
            state = self._request_state()
            episode_id = str(payload.get("episode_id", ""))
            state.require_active(episode_id)

            async def get_screenshot(env: Any) -> dict[str, Any]:
                state.require_active(str(payload.get("episode_id", "")))
                observation = await _maybe_await(env.get_observation())
                return _encode_observation_screenshot(observation)

            result = bridge.submit_to_state(state, state.run_env(get_screenshot))
            self._send_json(200, result)

        def _handle_provider_screenshot(self, payload: dict[str, Any]) -> None:
            state = self._request_screen_state()
            if state is None:
                raise NoBridgeEnvAvailableError("no MobileGym env is available")

            async def get_frame(env: Any) -> dict[str, Any]:
                observation = await _maybe_await(env.get_observation())
                screenshot = _encode_observation_screenshot(observation)
                seq = int(getattr(state, "_screenshot_seq", 0)) + 1
                state._screenshot_seq = seq
                image = base64.b64decode(screenshot["data"])
                requested_format = str(payload.get("format") or "jpeg")
                quality = payload.get("quality")
                try:
                    quality_value = int(quality) if quality is not None else 80
                except (TypeError, ValueError):
                    quality_value = 80
                image, pixel_format = encode_image_as_format(
                    image,
                    str(screenshot.get("format") or ""),
                    requested_format,
                    quality_value,
                )
                return encode_provider_frame(
                    image,
                    width=int(screenshot["width"]),
                    height=int(screenshot["height"]),
                    pixel_format=pixel_format,
                    backend="mobilegym",
                    seq=seq,
                )

            result = bridge.submit_to_state(
                state,
                state.run_env(get_frame),
                timeout=SCREENSHOT_PROVIDER_TIMEOUT_SEC,
            )
            self._send_json(200, bridge_ok(result))

        def _handle_provider_mnk(self, payload: dict[str, Any]) -> None:
            state = self._request_state()
            if not state.active_episode_id:
                self._send_json(409, {"error": "no active episode; call /api/setup first"})
                return
            status, response = execute_mnk_request(
                payload,
                lambda tool_name, tool_input: bridge.tools_api._submit_tool_call(
                    state, tool_name, tool_input
                ),
            )
            self._send_json(status, response)

        def _handle_action(self, name: str, payload: dict[str, Any]) -> None:
            state = self._request_state()
            episode_id = str(payload.get("episode_id", ""))
            state.require_active(episode_id)
            tool_input = {key: value for key, value in payload.items() if key != "episode_id"}
            action = build_action(name, tool_input)
            action_payload = action_to_dict(action)

            async def step_env(env: Any) -> dict[str, Any]:
                state.require_active(episode_id)
                started = time.monotonic()
                step_result = await _maybe_await(env.step(action))
                duration_ms = int((time.monotonic() - started) * 1000)
                observation = _observation_value(step_result, "observation")
                if observation is None:
                    observation = await _maybe_await(env.get_observation())
                screenshot = _encode_observation_screenshot(observation)
                log_entry = state.log_action(
                    tool_name=name,
                    tool_input=tool_input,
                    mobilegym_action=action_payload,
                    duration_ms=duration_ms,
                    error=None,
                    episode_id=episode_id,
                    screenshot=screenshot,
                )
                return {
                    "ok": True,
                    "message": "ok",
                    "action_id": log_entry["action_id"],
                    "screenshot": screenshot,
                }

            result = bridge.submit_to_state(state, state.run_env(step_env))
            self._send_json(200, result)

        def _request_state(self) -> BridgeEpisodeState:
            return bridge.router.state_for_headers(self.headers)

        def _request_screen_state(self) -> BridgeEpisodeState | None:
            task_id = benchmark_task_id_from_headers(self.headers)
            if not task_id:
                query = urllib.parse.parse_qs(urllib.parse.urlparse(self.path).query)
                values = query.get("benchmark-task-id") or query.get("task_id") or []
                task_id = str(values[0]).strip() if values else ""
            if task_id:
                return bridge.router.existing_state_for_task_id(task_id)
            return bridge.router.state_for_task_id(task_id)

        def _read_json(self) -> dict[str, Any] | None:
            try:
                length = int(self.headers.get("Content-Length", "0") or "0")
            except ValueError:
                self._send_error(400, "bad_header", "invalid Content-Length")
                return None
            raw = self.rfile.read(length) if length else b"{}"
            try:
                payload = json.loads(raw.decode("utf-8"))
            except json.JSONDecodeError:
                self._send_error(400, "bad_json", "invalid JSON body")
                return None
            if not isinstance(payload, dict):
                self._send_error(400, "bad_json", "JSON body must be an object")
                return None
            return payload

        def _send_error(self, status: int, code: str, message: str) -> None:
            self._send_json(status, bridge_error(code, message, status=status))

        def _send_json(self, status: int, payload: dict[str, Any]) -> None:
            data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            self._send_bytes(status, "application/json", data)

        def _send_bytes(self, status: int, content_type: str, data: bytes) -> None:
            self.send_response(status)
            self.send_header("Content-Type", content_type)
            self.send_header("Cache-Control", "no-store")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)

    return BridgeRequestHandler


def _health_payload(bridge: BridgeServer) -> dict[str, Any]:
    return {
        "status": "ok",
        "bridge_type": "mobilegym",
        "platform": "android",
        "env_count": len(bridge.router.states),
        "concurrent": len(bridge.router.states),
        "active_episode_id": bridge.state.active_episode_id,
        "active_routes": bridge.router.task_map(),
        "interfaces": [
            "/api/tools",
            "/api/providers/screenshot",
            "/api/providers/mnk",
            "/api/setup",
            "/state",
            "/route",
            "/api/release",
            "/api/concurrent",
        ],
    }


def _concurrent_payload(bridge: BridgeServer) -> dict[str, Any]:
    return {
        "bridge_type": "mobilegym",
        "concurrent": len(bridge.router.states),
        "env_count": len(bridge.router.states),
        "active_routes": bridge.router.task_map(),
    }


async def _maybe_await(value: Any) -> Any:
    if asyncio.iscoroutine(value):
        return await value
    return value


def _encode_observation_screenshot(observation: Any) -> dict[str, Any]:
    payload = _observation_value(observation, "screenshot")
    if payload is None:
        payload = _observation_value(observation, "screenshot_bytes")
    if isinstance(payload, str):
        payload = payload.encode("utf-8")
    if not isinstance(payload, bytes):
        raise ValueError("observation does not contain screenshot bytes")
    mime_type = _observation_value(observation, "mime_type") or _infer_screenshot_mime_type(payload)
    width = _observation_int(observation, "width") or _observation_int(observation, "screenshot_width")
    height = _observation_int(observation, "height") or _observation_int(observation, "screenshot_height")
    return encode_screenshot(payload, mime_type=str(mime_type), width=width, height=height)


def _infer_screenshot_mime_type(payload: bytes) -> str:
    if payload.startswith(b"\x89PNG\r\n\x1a\n"):
        return "image/png"
    if payload.startswith(b"\xff\xd8"):
        return "image/jpeg"
    return "application/octet-stream"


def _observation_int(observation: Any, name: str) -> int | None:
    value = _observation_value(observation, name)
    if value is None:
        return None
    return int(value)


def _observation_value(observation: Any, name: str) -> Any:
    if isinstance(observation, dict):
        value = observation.get(name)
    else:
        value = getattr(observation, name, None)
    if callable(value):
        return value()
    return value


def _get_container_ip() -> str | None:
    """Return the container's outbound IP on the docker bridge network.

    Parallel workers run in isolated docker-compose projects with `bind=0.0.0.0`.
    `socket.gethostname()` returns the random container ID, which other services
    on the user-defined network may not always resolve. The UDP-connect trick
    forces the OS to pick the interface that would carry outbound traffic and
    surfaces its address. No packets are sent — `connect` on a SOCK_DGRAM only
    binds the local address.
    """
    try:
        s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
        s.connect(("8.8.8.8", 80))
        ip = s.getsockname()[0]
        s.close()
        return ip
    except OSError:
        return None
