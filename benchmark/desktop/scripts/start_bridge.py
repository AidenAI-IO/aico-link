from __future__ import annotations

import argparse
import ipaddress
import signal
import sys
import time

from desktop.bridge.device import DesktopDevice
from desktop.bridge.server import DesktopBridgeServer


_PRIVATE_BIND_NETWORKS = tuple(
    ipaddress.ip_network(network)
    for network in ("10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16", "100.64.0.0/10", "169.254.0.0/16")
)


def _bridge_host_error(bridge_host: str, *, allow_private_bind: bool) -> str:
    """Return why the bridge may not bind bridge_host, or an empty string."""
    allowed = "localhost or an IPv4 loopback address"
    if allow_private_bind:
        allowed += " or, with --allow-private-bind, a private-use IPv4 address"
    if bridge_host.lower() == "localhost":
        return ""
    try:
        host_address = ipaddress.ip_address(bridge_host)
    except ValueError:
        return f"--bridge-host must be {allowed}"
    if host_address.version != 4:
        return f"--bridge-host must be {allowed}"
    if host_address.is_loopback:
        return ""
    if allow_private_bind and any(host_address in network for network in _PRIVATE_BIND_NETWORKS):
        return ""
    return f"--bridge-host must be {allowed}"


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(description="Start a host desktop environment bridge")
    parser.add_argument("--bridge-host", default="127.0.0.1")
    parser.add_argument("--bridge-port", type=int, default=8898)
    parser.add_argument("--backend", choices=["auto", "pyautogui"], default="auto")
    parser.add_argument(
        "--allow-private-bind",
        action="store_true",
        help="Also accept a private-use IPv4 --bridge-host, such as the Docker bridge gateway on Linux",
    )
    parser.add_argument("--screenshot-command", default="", help="Optional screenshot command prefix (the output path is appended)")
    args = parser.parse_args(argv)
    if args.bridge_port < 0 or args.bridge_port > 65535:
        parser.error("--bridge-port must be between 0 and 65535")
    bridge_host = str(args.bridge_host or "").strip()
    host_error = _bridge_host_error(bridge_host, allow_private_bind=args.allow_private_bind)
    if host_error:
        parser.error(host_error)
    try:
        device = DesktopDevice(backend=args.backend, screenshot_command=args.screenshot_command)
        print(f"permission_notice: {device.permission_hint}", file=sys.stderr, flush=True)
        bridge = DesktopBridgeServer(device=device, host=bridge_host, port=args.bridge_port)
        url = bridge.start()
    except Exception as exc:
        print(f"failed to start desktop bridge: {exc}", file=sys.stderr)
        return 1
    print(f"desktop environment bridge started: {url}", flush=True)
    print(f"platform: {device.platform}", flush=True)
    print(f"stop_command: kill -TERM {__import__('os').getpid()}", flush=True)
    stop = False
    def _stop(signum, frame):
        nonlocal stop
        stop = True
    signal.signal(signal.SIGTERM, _stop)
    signal.signal(signal.SIGINT, _stop)
    try:
        while not stop:
            time.sleep(0.5)
    finally:
        bridge.stop()
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
