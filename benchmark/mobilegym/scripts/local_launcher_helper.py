#!/usr/bin/env python3
from __future__ import annotations

import argparse
import json
import os
import subprocess
import sys
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from ipaddress import ip_address
from pathlib import Path
from typing import Any, Callable
from urllib.parse import urlparse


HOST = "127.0.0.1"
PORT = 4175
LAUNCHER_LABEL = "com.aico.mobilegym-local-launcher"
LAUNCHER_PLIST = Path.home() / "Library" / "LaunchAgents" / f"{LAUNCHER_LABEL}.plist"
LAUNCHCTL_TIMEOUT_SECONDS = 10


class HelperError(RuntimeError):
    pass


def start_local_launcher() -> dict[str, Any]:
    target = f"gui/{os.getuid()}/{LAUNCHER_LABEL}"
    domain = f"gui/{os.getuid()}"
    try:
        result = run_launchctl(["launchctl", "kickstart", "-k", target])
    except subprocess.CalledProcessError as exc:
        if "Could not find service" in "\n".join(part for part in (exc.stdout, exc.stderr) if part):
            run_launchctl(["launchctl", "bootstrap", domain, str(LAUNCHER_PLIST)])
            result = run_launchctl(["launchctl", "kickstart", "-k", target])
        else:
            output = "\n".join(part for part in (exc.stdout, exc.stderr) if part).strip()
            raise HelperError(output or f"launchctl exited with {exc.returncode}") from exc
    output = "\n".join(part for part in (result.stdout, result.stderr) if part).strip()
    return {"ok": True, "status": "started", "output": output}


def run_launchctl(argv: list[str]) -> subprocess.CompletedProcess[str]:
    try:
        return subprocess.run(
            argv,
            check=True,
            capture_output=True,
            text=True,
            timeout=LAUNCHCTL_TIMEOUT_SECONDS,
        )
    except subprocess.TimeoutExpired as exc:
        raise HelperError("launchctl timed out") from exc


def default_allowed_origins() -> set[str]:
    raw = os.environ.get("MOBILEGYM_HELPER_ALLOWED_ORIGINS", "")
    return {origin.strip().rstrip("/") for origin in raw.split(",") if origin.strip()}


def origin_allowed(origin: str | None, allowed_origins: set[str]) -> bool:
    if not origin:
        return True
    normalized = origin.rstrip("/")
    if normalized in allowed_origins:
        return True
    parsed = urlparse(normalized)
    if parsed.scheme not in {"http", "https"} or not parsed.hostname:
        return False
    host = parsed.hostname.lower()
    if host in {"localhost", "127.0.0.1", "::1"}:
        return True
    try:
        return ip_address(host).is_private
    except ValueError:
        return False


def make_handler(
    starter: Callable[[], dict[str, Any]] = start_local_launcher,
    allowed_origins: set[str] | None = None,
) -> type[BaseHTTPRequestHandler]:
    allowed = default_allowed_origins() if allowed_origins is None else allowed_origins

    class LocalLauncherHelperHandler(BaseHTTPRequestHandler):
        def do_OPTIONS(self) -> None:
            origin = self.allowed_origin()
            if self.headers.get("Origin") and not origin:
                self.send_response(403)
                self.end_headers()
                return
            self.send_response(204)
            self.send_common_headers("text/plain; charset=utf-8", origin)
            self.end_headers()

        def do_POST(self) -> None:
            path = urlparse(self.path).path
            if path != "/start":
                self.send_error(404, "not found")
                return
            origin = self.allowed_origin()
            if self.headers.get("Origin") and not origin:
                self.send_json({"error": "forbidden"}, status=403)
                return
            try:
                self.send_json(starter(), origin=origin)
            except HelperError as exc:
                self.send_json({"error": str(exc)}, status=500, origin=origin)

        def allowed_origin(self) -> str | None:
            origin = self.headers.get("Origin")
            return origin if origin_allowed(origin, allowed) else None

        def send_json(self, payload: Any, status: int = 200, origin: str | None = None) -> None:
            body = json.dumps(payload, ensure_ascii=False).encode("utf-8")
            self.send_response(status)
            self.send_common_headers("application/json; charset=utf-8", origin)
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)

        def send_common_headers(self, content_type: str, origin: str | None = None) -> None:
            self.send_header("Content-Type", content_type)
            if origin:
                self.send_header("Access-Control-Allow-Origin", origin)
                self.send_header("Vary", "Origin")
            self.send_header("Access-Control-Allow-Methods", "POST,OPTIONS")
            self.send_header("Access-Control-Allow-Headers", "Content-Type")
            self.send_header("Access-Control-Allow-Private-Network", "true")

        def log_message(self, fmt: str, *args: Any) -> None:
            print(f"{self.address_string()} - {fmt % args}", file=sys.stderr)

    return LocalLauncherHelperHandler


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Mac-local helper that starts the MobileGym launcher.")
    parser.add_argument("--host", default=HOST)
    parser.add_argument("--port", type=int, default=PORT)
    args = parser.parse_args(argv)

    server = ThreadingHTTPServer((args.host, args.port), make_handler())
    print(f"MobileGym launcher helper listening on http://{args.host}:{args.port}")
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nStopping MobileGym launcher helper")
        return 130
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
