from __future__ import annotations

import argparse
import json
import platform
import signal
import subprocess
import sys
import threading
from pathlib import Path

from .device import MirroringDevice, parse_insets
from .server import MirroringServer


def build_helper(output: Path) -> Path:
    source = Path(__file__).parent / "native" / "Mirroring.swift"
    if not output.exists() or output.stat().st_mtime < source.stat().st_mtime:
        output.parent.mkdir(parents=True, exist_ok=True)
        subprocess.run(["xcrun", "swiftc", "-parse-as-library", "-O", str(source), "-o", str(output)], check=True)
    return output


def main(argv=None):
    parser = argparse.ArgumentParser(description="Expose macOS iPhone Mirroring through the Agent environment bridge")
    parser.add_argument("--port", type=int, default=8897)
    parser.add_argument("--task-id", default="iphone-mirroring", help="Exclusive route claimed at startup")
    parser.add_argument("--content-insets", type=parse_insets, default=parse_insets("8,38,8,8"), metavar="LEFT,TOP,RIGHT,BOTTOM")
    parser.add_argument("--helper", type=Path, help="Use an already compiled native helper")
    parser.add_argument("--probe", action="store_true", help="Print window/permission status and exit without sending input")
    parser.add_argument("--request-input-access", action="store_true", help="Explicitly ask macOS for Accessibility permission, then exit")
    parser.add_argument("--request-screen-access", action="store_true", help="Explicitly ask macOS for Screen Recording permission, then exit")
    args = parser.parse_args(argv)
    if platform.system() != "Darwin":
        parser.error("iPhone Mirroring requires macOS")
    if not 0 <= args.port <= 65535 or not args.task_id.strip():
        parser.error("port must be in [0, 65535] and task-id must not be empty")
    root = Path(__file__).resolve().parents[2]
    try:
        helper = args.helper.resolve() if args.helper else build_helper(root / ".cache/iphone-mirroring/mirroring-native")
        device = MirroringDevice(helper, args.content_insets)
        if args.request_input_access or args.request_screen_access:
            results = {}
            if args.request_input_access:
                results.update(device._call("request_input_access"))
            if args.request_screen_access:
                results.update(device._call("request_screen_access"))
            print(json.dumps(results, indent=2))
            return 0
        if args.probe:
            print(json.dumps(device.diagnostics(), indent=2))
            return 0
        info = device.check_device()
        bridge = MirroringServer(device, port=args.port)
        bridge.state.acquire(args.task_id)
        endpoint = bridge.start()
        print(json.dumps({"endpoint": endpoint, "task_id": args.task_id, "helper": str(helper), **info}), flush=True)
        print(f"Agent flags: --device-type iOS --environment-bridge-mode --environment-bridge-endpoint {endpoint} --benchmark-task-id {args.task_id}", flush=True)
        stopped = threading.Event()
        for signum in (signal.SIGINT, signal.SIGTERM):
            signal.signal(signum, lambda *_: stopped.set())
        try:
            stopped.wait()
        finally:
            bridge.stop()
        return 0
    except (OSError, ValueError, RuntimeError, subprocess.SubprocessError) as exc:
        print(str(exc), file=sys.stderr)
        return 1


if __name__ == "__main__":
    raise SystemExit(main())
