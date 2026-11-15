"""Command line entry point: ``uv run python -m launcher`` from benchmark/."""

from __future__ import annotations

import argparse
import sys
import threading
import webbrowser
from pathlib import Path

from . import DEFAULT_PORT, DEFAULT_STATE_DIR
from .server import Launcher, create_server
from .store import StateDirBusy, lock_state_dir


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="python -m launcher",
        description="Aico Launcher: start Aico Link Docker images with an environment bridge.",
    )
    parser.add_argument("--port", type=int, default=DEFAULT_PORT, help=f"port on 127.0.0.1 (default {DEFAULT_PORT})")
    parser.add_argument("--no-open", action="store_true", help="do not open a browser")
    parser.add_argument(
        "--state-dir",
        type=Path,
        default=DEFAULT_STATE_DIR,
        help=f"where instances, builds, and logs are kept (default {DEFAULT_STATE_DIR})",
    )
    args = parser.parse_args(argv)
    if not 0 <= args.port <= 65535:
        parser.error("--port must be between 0 and 65535")

    state_dir = args.state_dir.expanduser().resolve()
    try:
        # Held until the process exits; released by the OS.
        state_lock = lock_state_dir(state_dir)  # noqa: F841
    except StateDirBusy as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 1
    app = Launcher(state_dir)
    try:
        server, token = create_server(app, port=args.port)
    except OSError as exc:
        print(
            f"error: cannot listen on 127.0.0.1:{args.port}: {exc}. "
            "Another launcher may be running; use --port to pick another port.",
            file=sys.stderr,
        )
        return 1
    port = int(server.server_address[1])
    app.instances.reconcile()
    app.instances.start_route_keepalive()
    url = f"http://127.0.0.1:{port}/?token={token}"
    print("Aico Launcher is running.", flush=True)
    print(f"  Open: {url}", flush=True)
    print(f"  State: {state_dir}", flush=True)
    print("  Press Ctrl+C to quit. Running instances keep running.", flush=True)
    if not args.no_open:
        threading.Timer(0.5, lambda: webbrowser.open(url)).start()
    try:
        server.serve_forever()
    except KeyboardInterrupt:
        print("\nLauncher stopped. Running instances were left running.", flush=True)
    finally:
        server.server_close()
    return 0


if __name__ == "__main__":
    sys.exit(main())
