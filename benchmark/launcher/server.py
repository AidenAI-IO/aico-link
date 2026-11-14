"""HTTP server: token auth, Host/Origin checks, static files, and the JSON API.

The server binds 127.0.0.1 only. A random token printed at startup is
exchanged for an HttpOnly cookie; every /api/* request needs that cookie.
Requests with an unexpected Host header (DNS rebinding) and cross-origin
mutating requests are refused before routing.
"""

from __future__ import annotations

import hmac
import json
import mimetypes
import platform
import re
import secrets
import sys
import urllib.parse
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Callable

from . import REPO_ROOT, STATIC_DIR, validation
from .bridges import HostFacts, get_bridge
from .docker import Docker, DockerError
from .images import BuildConflict, ImageManager, repo_commit
from .instances import Conflict, InstanceManager, NotFound
from .store import Store
from .system import System

COOKIE_NAME = "aico_launcher"
MAX_BODY_BYTES = 256 * 1024
MUTATING_METHODS = {"POST", "PUT", "DELETE"}


class ApiError(Exception):
    def __init__(self, status: int, message: str) -> None:
        super().__init__(message)
        self.status = status
        self.message = message


def normalize_settings(body: Any) -> dict[str, Any]:
    if not isinstance(body, dict):
        raise validation.ValidationError("Settings must be a JSON object.")
    build = body.get("build") or {}
    if not isinstance(build, dict):
        raise validation.ValidationError("build must be an object.")
    return {
        "build": {
            "base_image": validation.optional_image_ref(build.get("base_image"), field="Base image"),
            "go_download_base": validation.optional_http_url(build.get("go_download_base"), field="Go download base"),
            "mobilegym_node_image": validation.optional_image_ref(
                build.get("mobilegym_node_image"), field="MobileGym Node image"
            ),
        },
        "pass_host_proxy": validation.boolean(body.get("pass_host_proxy"), field="Pass host proxy"),
    }


class Launcher:
    """Application object wiring the managers together; independent of HTTP."""

    def __init__(self, state_dir: Path, *, system: System | None = None, repo_root: Path = REPO_ROOT) -> None:
        self.state_dir = Path(state_dir)
        self.repo_root = repo_root
        self.system = system or System()
        self.store = Store(self.state_dir)
        self.docker = Docker(self.system)
        self.host = HostFacts(self.system, self.docker, repo_root)
        self.images = ImageManager(
            store=self.store, docker=self.docker, system=self.system,
            state_dir=self.state_dir, repo_root=repo_root,
        )
        self.instances = InstanceManager(
            store=self.store, docker=self.docker, system=self.system, host=self.host,
            state_dir=self.state_dir, repo_root=repo_root,
        )
        self._repo_commit = repo_commit(self.system, repo_root)

    def state(self) -> dict[str, Any]:
        docker_info = self.host.docker_info()
        return {
            "launcher": {"repo_commit": self._repo_commit, "state_dir": str(self.state_dir)},
            "host": {
                "os": {"darwin": "macOS", "linux": "Linux", "win32": "Windows"}.get(sys.platform, sys.platform),
                "arch": platform.machine(),
                "docker": docker_info,
            },
            "settings": self.store.settings(),
            "images": self.images.list_images() if docker_info["available"] else [],
            "builds": self.images.builds(),
            "bridge_types": self.host.bridge_specs(),
            "instances": self.instances.list(),
        }

    def probe(self, type_: str, body: Any) -> dict[str, Any]:
        bridge = get_bridge(type_)
        if bridge.planned:
            raise validation.ValidationError(f"{bridge.label} is not available yet.")
        config = (body or {}).get("config") if isinstance(body, dict) else None
        if bridge.type == "custom" and not str((config or {}).get("endpoint") or "").strip():
            return {
                "ok": False,
                "checks": [{"name": "Bridge endpoint", "ok": False, "detail": "Enter the bridge endpoint first."}],
                "options": {},
                "device_type": "",
            }
        normalized = bridge.normalize(config)
        return bridge.probe(normalized, self.host.refresh(force=True))


Route = tuple[str, "re.Pattern[str]", Callable[..., tuple[int, Any]]]


def make_handler(app: Launcher, token: str, port_getter: Callable[[], int]):
    def allowed_hosts() -> set[str]:
        port = port_getter()
        return {f"127.0.0.1:{port}", f"localhost:{port}"}

    def cookie_name() -> str:
        # Cookies are not scoped by port: a per-port name keeps launchers on
        # different ports from overwriting each other's session.
        return f"{COOKIE_NAME}_{port_getter()}"

    def body_of(handler: "Handler") -> Any:
        return handler.json_body()

    def r_state(h, m):
        return 200, app.state()

    def r_build(h, m):
        body = body_of(h) or {}
        if not isinstance(body, dict):
            raise validation.ValidationError("Request body must be a JSON object.")
        return 202, app.images.start_build(body.get("tag") or None)

    def r_get_build(h, m):
        try:
            build = app.images.get_build(m["id"])
        except validation.ValidationError:
            build = None
        if build is None:
            raise ApiError(404, "Unknown build.")
        return 200, build

    def r_probe(h, m):
        return 200, app.probe(urllib.parse.unquote(m["type"]), body_of(h))

    def r_create(h, m):
        return 201, app.instances.create(body_of(h))

    def r_update(h, m):
        return 200, app.instances.update(m["id"], body_of(h))

    def r_start(h, m):
        return 202, app.instances.start(m["id"])

    def r_stop(h, m):
        return 202, app.instances.stop(m["id"])

    def r_delete(h, m):
        purge = h.query.get("purge_data", [""])[0] in {"1", "true", "yes"}
        app.instances.delete(m["id"], purge_data=purge)
        return 200, {"deleted": True}

    def r_logs(h, m):
        source = h.query.get("source", ["launcher"])[0]
        try:
            tail = int(h.query.get("tail", ["400"])[0])
        except ValueError:
            raise validation.ValidationError("tail must be a whole number.") from None
        return 200, app.instances.logs(m["id"], source, tail)

    def r_settings(h, m):
        return 200, app.store.set_settings(normalize_settings(body_of(h)))

    routes: list[Route] = [
        ("GET", re.compile(r"^/api/state$"), r_state),
        ("POST", re.compile(r"^/api/images/build$"), r_build),
        ("GET", re.compile(r"^/api/builds/(?P<id>[^/]+)$"), r_get_build),
        ("POST", re.compile(r"^/api/bridge-types/(?P<type>[^/]+)/probe$"), r_probe),
        ("POST", re.compile(r"^/api/instances$"), r_create),
        ("PUT", re.compile(r"^/api/instances/(?P<id>[^/]+)$"), r_update),
        ("DELETE", re.compile(r"^/api/instances/(?P<id>[^/]+)$"), r_delete),
        ("POST", re.compile(r"^/api/instances/(?P<id>[^/]+)/start$"), r_start),
        ("POST", re.compile(r"^/api/instances/(?P<id>[^/]+)/stop$"), r_stop),
        ("GET", re.compile(r"^/api/instances/(?P<id>[^/]+)/logs$"), r_logs),
        ("PUT", re.compile(r"^/api/settings$"), r_settings),
    ]

    class Handler(BaseHTTPRequestHandler):
        server_version = "AicoLauncher"
        sys_version = ""
        protocol_version = "HTTP/1.1"

        # -- plumbing -------------------------------------------------------

        def log_message(self, format: str, *args: Any) -> None:
            # Never log request lines: GET /?token=... would leak the token.
            return

        def _security_headers(self) -> None:
            self.send_header("X-Content-Type-Options", "nosniff")
            self.send_header("Referrer-Policy", "no-referrer")
            self.send_header("X-Frame-Options", "DENY")
            self.send_header("Cache-Control", "no-store")

        def send_json(self, status: int, payload: Any) -> None:
            data = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            if getattr(self, "_raw_body", None) is None:
                # The request body was never consumed: its bytes would be
                # parsed as the next request on a kept-alive connection.
                self.close_connection = True
            self.send_response(status)
            self.send_header("Content-Type", "application/json; charset=utf-8")
            self.send_header("Content-Length", str(len(data)))
            self._security_headers()
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(data)

        def send_error_json(self, status: int, message: str) -> None:
            self.send_json(status, {"error": message})

        def read_body(self) -> bool:
            """Consume the request body before any response is sent.

            Every response, including early refusals, must leave the
            connection positioned at the next request. Returns False (after
            answering and closing the connection) when the body cannot be read.
            """
            self._raw_body = None
            if self.headers.get("Transfer-Encoding"):
                self.send_error_json(411, "Request bodies need a Content-Length.")
                return False
            length_text = (self.headers.get("Content-Length") or "0").strip()
            try:
                length = int(length_text)
            except ValueError:
                length = -1
            if length < 0:
                self.send_error_json(400, "Invalid Content-Length.")
                return False
            if length > MAX_BODY_BYTES:
                self.send_error_json(413, "Request body is too large.")
                return False
            self._raw_body = self.rfile.read(length) if length > 0 else b""
            return True

        def json_body(self) -> Any:
            raw = self._raw_body or b""
            if not raw.strip():
                return None
            try:
                return json.loads(raw.decode("utf-8"))
            except (UnicodeDecodeError, json.JSONDecodeError):
                raise ApiError(400, "Request body is not valid JSON.") from None

        def cookie_token(self) -> str:
            # Parsed by hand: SimpleCookie drops every cookie when another
            # application on 127.0.0.1 sets one that is not RFC-strict.
            wanted = cookie_name()
            for part in (self.headers.get("Cookie") or "").split(";"):
                name, _, value = part.strip().partition("=")
                if name.strip() == wanted:
                    return value.strip().strip('"')
            return ""

        def authenticated(self) -> bool:
            return hmac.compare_digest(self.cookie_token().encode(), token.encode())

        # -- checks ---------------------------------------------------------

        def host_ok(self) -> bool:
            return (self.headers.get("Host") or "").strip().lower() in allowed_hosts()

        def origin_ok(self) -> bool:
            value = self.headers.get("Origin")
            if value is None:
                return True
            # The Origin must be this server as reached through the request's Host.
            host = (self.headers.get("Host") or "").strip().lower()
            return value.strip().lower() == f"http://{host}"

        # -- dispatch -------------------------------------------------------

        def handle_any(self) -> None:
            if not self.read_body():
                return
            if not self.host_ok():
                self.send_error_json(403, "Forbidden host.")
                return
            parsed = urllib.parse.urlsplit(self.path)
            self.query = urllib.parse.parse_qs(parsed.query)
            path = parsed.path
            if self.command in MUTATING_METHODS and not self.origin_ok():
                self.send_error_json(403, "Cross-origin request refused.")
                return
            if path.startswith("/api/"):
                self.handle_api(path)
            elif self.command in {"GET", "HEAD"}:
                self.handle_static(path)
            else:
                self.send_error_json(405, "Method not allowed.")

        def handle_api(self, path: str) -> None:
            if not self.authenticated():
                self.send_error_json(401, "Open the URL printed by the launcher to sign in.")
                return
            if self.command in MUTATING_METHODS:
                # A JSON content type cannot be sent cross-site without a CORS
                # preflight, which this server never grants. DELETE may omit a body.
                content_type = (self.headers.get("Content-Type") or "").split(";", 1)[0].strip().lower()
                has_body = (self.headers.get("Content-Length") or "0").strip() not in {"", "0"}
                if (has_body or self.command != "DELETE") and content_type != "application/json":
                    self.send_error_json(415, "Request bodies must be application/json.")
                    return
            method_allowed = False
            for method, pattern, handler in routes:
                match = pattern.fullmatch(path)
                if not match:
                    continue
                method_allowed = True
                if method != self.command:
                    continue
                try:
                    status, payload = handler(self, match.groupdict())
                except ApiError as exc:
                    self.send_error_json(exc.status, exc.message)
                except validation.ValidationError as exc:
                    self.send_error_json(400, str(exc))
                except NotFound as exc:
                    self.send_error_json(404, str(exc))
                except (Conflict, BuildConflict) as exc:
                    self.send_error_json(409, str(exc))
                except DockerError as exc:
                    self.send_error_json(502, str(exc))
                except Exception as exc:  # noqa: BLE001
                    self.send_error_json(500, f"Internal error: {exc}")
                else:
                    self.send_json(status, payload)
                return
            if method_allowed:
                self.send_error_json(405, "Method not allowed.")
            else:
                self.send_error_json(404, "Not found.")

        def handle_static(self, path: str) -> None:
            if path == "/":
                supplied = self.query.get("token", [""])[0]
                if supplied:
                    if hmac.compare_digest(supplied.encode(), token.encode()):
                        self.send_response(303)
                        self.send_header(
                            "Set-Cookie",
                            f"{cookie_name()}={token}; HttpOnly; SameSite=Strict; Path=/",
                        )
                        self.send_header("Location", "/")
                        self.send_header("Content-Length", "0")
                        self._security_headers()
                        self.end_headers()
                        return
                    # A wrong token falls through to the page, which shows the sign-in hint.
                self.send_file(STATIC_DIR / "index.html")
                return
            if path.startswith("/static/"):
                name = urllib.parse.unquote(path[len("/static/"):])
                target = (STATIC_DIR / name).resolve()
                try:
                    target.relative_to(STATIC_DIR.resolve())
                except ValueError:
                    self.send_error_json(404, "Not found.")
                    return
                self.send_file(target)
                return
            self.send_error_json(404, "Not found.")

        def send_file(self, target: Path) -> None:
            if not target.is_file():
                self.send_error_json(404, "Not found.")
                return
            data = target.read_bytes()
            content_type = mimetypes.guess_type(target.name)[0] or "application/octet-stream"
            if content_type.startswith("text/") or content_type in {"application/javascript"}:
                content_type += "; charset=utf-8"
            self.send_response(200)
            self.send_header("Content-Type", content_type)
            self.send_header("Content-Length", str(len(data)))
            self._security_headers()
            if target.suffix == ".html":
                self.send_header(
                    "Content-Security-Policy",
                    "default-src 'self'; img-src 'self' data:; style-src 'self'; script-src 'self'; "
                    "connect-src 'self'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'",
                )
            self.end_headers()
            if self.command != "HEAD":
                self.wfile.write(data)

        do_GET = handle_any
        do_HEAD = handle_any
        do_POST = handle_any
        do_PUT = handle_any
        do_DELETE = handle_any

        def do_OPTIONS(self) -> None:  # noqa: N802 - no CORS, ever
            if self.read_body():
                self.send_error_json(405, "Method not allowed.")

    return Handler


class LauncherServer(ThreadingHTTPServer):
    daemon_threads = True
    allow_reuse_address = True


def create_server(app: Launcher, *, port: int, token: str | None = None) -> tuple[LauncherServer, str]:
    token = token or secrets.token_urlsafe(32)
    holder: dict[str, int] = {"port": port}
    server = LauncherServer(("127.0.0.1", port), make_handler(app, token, lambda: holder["port"]))
    holder["port"] = int(server.server_address[1])
    return server, token
