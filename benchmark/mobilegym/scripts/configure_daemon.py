#!/usr/bin/env python3
"""Configure Aico daemon to use MobileGym bridge.

This script configures the Aico daemon to connect to a MobileGym bridge
by setting the bridge URL and device token as environment variables or
via the /api/mobilegym/bridge/configure endpoint.
"""
from __future__ import annotations

import argparse
import json
import sys
import urllib.request
from pathlib import Path


class ConfigureError(RuntimeError):
    pass


def configure_daemon_bridge(
    daemon_url: str,
    bridge_url: str,
    bridge_device_token: str,
    control_token: str | None = None,
) -> dict:
    """Configure Aico daemon to use MobileGym bridge.

    Args:
        daemon_url: Aico daemon base URL
        bridge_url: MobileGym bridge URL
        bridge_device_token: Device token for bridge authentication
        control_token: Optional control token for daemon API

    Returns:
        Response from daemon configuration endpoint
    """
    endpoint = f"{daemon_url.rstrip('/')}/api/mobilegym/bridge/configure"
    payload = {
        "bridge_url": bridge_url,
        "bridge_device_token": bridge_device_token,
    }

    headers = {"Content-Type": "application/json"}
    if control_token:
        headers["Authorization"] = f"Bearer {control_token}"

    data = json.dumps(payload).encode("utf-8")
    request = urllib.request.Request(endpoint, data=data, headers=headers, method="POST")

    try:
        with urllib.request.urlopen(request, timeout=10) as response:  # noqa: S310
            raw = response.read().decode("utf-8")
            return json.loads(raw) if raw else {}
    except urllib.error.HTTPError as exc:
        body = exc.read().decode("utf-8", errors="replace")
        raise ConfigureError(f"HTTP {exc.code}: {body}") from exc
    except urllib.error.URLError as exc:
        raise ConfigureError(f"Failed to connect to daemon: {exc}") from exc
    except Exception as exc:
        raise ConfigureError(f"Configuration failed: {exc}") from exc


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description="Configure Aico daemon to use MobileGym bridge"
    )
    parser.add_argument(
        "--daemon-url",
        required=True,
        help="Aico daemon base URL (e.g., http://localhost:8080)",
    )
    parser.add_argument(
        "--bridge-url",
        required=True,
        help="MobileGym bridge URL (e.g., http://localhost:8888)",
    )
    parser.add_argument(
        "--bridge-device-token",
        help="Bridge device token (or use --bridge-token-file)",
    )
    parser.add_argument(
        "--bridge-token-file",
        type=Path,
        help="Path to file containing bridge device token",
    )
    parser.add_argument(
        "--control-token",
        help="Daemon control token for authentication",
    )
    parser.add_argument(
        "--control-token-file",
        type=Path,
        help="Path to file containing daemon control token",
    )

    args = parser.parse_args(argv)

    # Read bridge device token
    if args.bridge_device_token:
        bridge_token = args.bridge_device_token
    elif args.bridge_token_file:
        try:
            bridge_token = args.bridge_token_file.read_text().strip()
        except OSError as exc:
            print(f"error: failed to read bridge token file: {exc}", file=sys.stderr)
            return 2
    else:
        print("error: either --bridge-device-token or --bridge-token-file is required", file=sys.stderr)
        return 2

    # Read control token if provided
    control_token = None
    if args.control_token:
        control_token = args.control_token
    elif args.control_token_file:
        try:
            control_token = args.control_token_file.read_text().strip()
        except OSError as exc:
            print(f"warning: failed to read control token file: {exc}", file=sys.stderr)

    try:
        result = configure_daemon_bridge(
            daemon_url=args.daemon_url,
            bridge_url=args.bridge_url,
            bridge_device_token=bridge_token,
            control_token=control_token,
        )
        print("✓ Daemon configured successfully", flush=True)
        print(f"  Bridge URL: {args.bridge_url}", flush=True)
        if result.get("status") == "configured":
            print(f"  Status: {result.get('message', 'OK')}", flush=True)
        return 0
    except ConfigureError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1


if __name__ == "__main__":
    sys.exit(main())
