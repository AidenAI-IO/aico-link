from __future__ import annotations

import argparse
import json
import signal
import sys
import threading
from pathlib import Path

from .device import IOSDevice
from .permissions import PermissionPolicy
from .server import IOSServer
from .transport import USBTransport


def main(argv=None):
    parser = argparse.ArgumentParser(description="Connect a real iPhone to the local Agent through USB and WebDriverAgent")
    parser.add_argument("--udid", help="Select a USB device; required if more than one is connected")
    parser.add_argument("--xctrunner", help="Start this installed, signed WDA test runner and stop it with the bridge")
    parser.add_argument("--port", type=int, default=8896)
    parser.add_argument("--task-id", default="ios-device")
    parser.add_argument("--permission-policy", type=Path, help="Explicit prompt/button signatures to handle automatically before screenshots")
    parser.add_argument("--probe", action="store_true", help="Check USB and WDA without sending input or applying permission rules")
    args = parser.parse_args(argv)
    if not 0 <= args.port <= 65535 or not args.task_id.strip():
        parser.error("port must be in [0, 65535] and task-id must not be empty")
    transport = None
    bridge = None
    try:
        policy = PermissionPolicy.load(args.permission_policy) if args.permission_policy else PermissionPolicy()
        transport = USBTransport(args.udid, args.xctrunner)
        device = IOSDevice(transport, policy)
        info = device.check_device()
        if args.probe:
            print(json.dumps(info, indent=2))
            return 0
        bridge = IOSServer(device, port=args.port)
        bridge.state.acquire(args.task_id)
        endpoint = bridge.start()
        print(json.dumps({"endpoint": endpoint, "task_id": args.task_id, **info}), flush=True)
        print(f"Agent flags: --device-type iOS --environment-bridge-mode --environment-bridge-endpoint {endpoint} --benchmark-task-id {args.task_id}", flush=True)
        stopped = threading.Event()
        for signum in (signal.SIGINT, signal.SIGTERM):
            signal.signal(signum, lambda *_: stopped.set())
        stopped.wait()
        return 0
    except (OSError, ValueError, RuntimeError, ImportError) as exc:
        print(str(exc), file=sys.stderr)
        return 1
    finally:
        if bridge:
            bridge.stop()
        if transport:
            transport.close()


if __name__ == "__main__":
    raise SystemExit(main())
