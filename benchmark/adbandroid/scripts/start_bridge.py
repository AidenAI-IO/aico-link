#!/usr/bin/env python3
"""Start the ADB Android environment bridge for a single device.

Usage (from the benchmark/ directory):

    uv run python -m adbandroid.scripts.start_bridge \
      --adb-serial 127.0.0.1:6555 \
      --bridge-host 127.0.0.1 \
      --bridge-port 8899
"""

from __future__ import annotations

import argparse
import json
import signal
import sys
import threading

from adbandroid.bridge.adb import (
    ADBAndroidDevice,
    ADBCommandError,
    DEFAULT_JPEG_QUALITY,
    DEFAULT_SCREENSHOT_MAX_WIDTH,
    DEFAULT_UNICODE_INPUT_METHOD,
    SUPPORTED_UNICODE_INPUT_METHODS,
)
from adbandroid.bridge.server import ADBBridgeServer


def endpoint_for_docker(endpoint: str) -> str:
    return (
        endpoint.replace("://127.0.0.1", "://host.docker.internal")
        .replace("://localhost", "://host.docker.internal")
    )


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(prog="adbandroid.scripts.start_bridge")
    parser.add_argument("--adb-serial", required=True, help="adb device serial, e.g. 127.0.0.1:6555")
    parser.add_argument("--adb-path", default="adb")
    parser.add_argument("--bridge-host", default="127.0.0.1")
    parser.add_argument("--bridge-port", type=int, default=0)
    parser.add_argument("--screenshot-max-width", type=int, default=DEFAULT_SCREENSHOT_MAX_WIDTH)
    parser.add_argument("--jpeg-quality", type=int, default=DEFAULT_JPEG_QUALITY)
    parser.add_argument(
        "--unicode-input-method",
        choices=SUPPORTED_UNICODE_INPUT_METHODS,
        default=DEFAULT_UNICODE_INPUT_METHOD,
        help="Unicode text input path (default: adbkeyboard); none disables native insert_text",
    )
    parser.add_argument("--json", action="store_true", help="Print machine-readable JSON")
    args = parser.parse_args(argv)

    device = ADBAndroidDevice(
        serial=args.adb_serial,
        adb_path=args.adb_path,
        screenshot_max_width=args.screenshot_max_width,
        jpeg_quality=args.jpeg_quality,
        unicode_input_method=args.unicode_input_method,
    )
    try:
        device.check_device()
    except ADBCommandError as exc:
        print(f"warning: adb device not ready yet: {exc}", file=sys.stderr, flush=True)
    else:
        try:
            installed = device.ensure_unicode_input_method_installed()
        except ADBCommandError as exc:
            print(f"error: could not install the selected input method: {exc}", file=sys.stderr, flush=True)
            return 1
        if installed:
            print(f"Installed selected input method: {args.unicode_input_method}", file=sys.stderr, flush=True)

    server = ADBBridgeServer(device, host=args.bridge_host, port=args.bridge_port)
    environment_url = server.start()
    docker_environment_url = endpoint_for_docker(environment_url)
    agent_daemon_command = (
        "uv run python -m runner start-agent-daemon "
        f"--environment-bridge-endpoint {environment_url}"
    )

    if args.json:
        print(
            json.dumps(
                {
                    "type": "adb-android-env",
                    "environment_url": environment_url,
                    "docker_environment_url": docker_environment_url,
                    "adb_serial": args.adb_serial,
                    "unicode_input_method": args.unicode_input_method,
                    "agent_daemon_command": agent_daemon_command,
                },
                ensure_ascii=False,
            ),
            flush=True,
        )
    else:
        print("ADB Android environment started", flush=True)
        print(f"environment_url={environment_url}", flush=True)
        print(f"docker_environment_url={docker_environment_url}", flush=True)
        print(f"adb_serial={args.adb_serial}", flush=True)
        print(f"unicode_input_method={args.unicode_input_method}", flush=True)
        print(f"agent_daemon_command={agent_daemon_command}", flush=True)

    stop_event = threading.Event()

    def handle_signal(signum: int, frame: object) -> None:
        stop_event.set()

    signal.signal(signal.SIGINT, handle_signal)
    signal.signal(signal.SIGTERM, handle_signal)
    stop_event.wait()
    server.stop()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
