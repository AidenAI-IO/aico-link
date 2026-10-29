#!/usr/bin/env python3
"""Start MobileGym as a pure simulator with Bridge server.

This script launches MobileGym environment and Bridge HTTP server WITHOUT
using MobileGym's test framework (SerialRunner, agent registration, etc.).
The Bridge exposes device operations via HTTP endpoints that Aico daemon
can call through the device-operator skill.

Usage:
    python start_simulator.py --env-url http://localhost:4173 --bridge-port 8888
"""
from __future__ import annotations

import argparse
import asyncio
import inspect
import logging
import os
import signal
import sys
from collections.abc import Awaitable
from pathlib import Path
from typing import Any

SCRIPT_PATH = Path(__file__).resolve()
MOBILEGYM_PACKAGE_ROOT = SCRIPT_PATH.parents[1]
BENCHMARK_ROOT = SCRIPT_PATH.parents[2]
DEFAULT_MOBILEGYM_ROOT = MOBILEGYM_PACKAGE_ROOT / "vendor" / "mobilegym"
DEFAULT_ENV_URL = "http://localhost:4173"
DEFAULT_BRIDGE_REQUEST_TIMEOUT_SEC = 180
logger = logging.getLogger(__name__)


class SimulatorError(RuntimeError):
    pass


def _supports_app_ids(reset: Any) -> bool:
    try:
        parameters = inspect.signature(reset).parameters.values()
    except (TypeError, ValueError):
        return True
    return any(
        parameter.kind is inspect.Parameter.VAR_KEYWORD
        or (
            parameter.name == "app_ids"
            and parameter.kind
            in (inspect.Parameter.POSITIONAL_OR_KEYWORD, inspect.Parameter.KEYWORD_ONLY)
        )
        for parameter in parameters
    )


async def _await_reset_result(result: Any) -> None:
    if inspect.isawaitable(result):
        await result


async def _reset_mobilegym_env(env: Any, *, app_ids: list[str]) -> None:
    reset = env.reset
    if _supports_app_ids(reset):
        await _await_reset_result(reset(app_ids=app_ids))
    else:
        await _await_reset_result(reset())


def install_resilient_mobilegym_reset(mobilegym_env_cls: type[Any]) -> None:
    """Patch MobileGymEnv.reset to recover from a crashed Playwright page.

    Upstream reset retries navigation on the same page. Once Chromium reports
    `Page crashed`/`Target crashed`, that page cannot be repaired with another
    goto, so recreate the env's owned page/context before one final reset.
    """
    if getattr(mobilegym_env_cls, "_aico_resilient_reset_installed", False):
        return
    original_reset = mobilegym_env_cls.reset
    original_supports_app_ids = _supports_app_ids(original_reset)

    async def call_original_reset(
        self: Any,
        app_ids: list[str] | None,
        reset_kwargs: dict[str, Any],
    ) -> None:
        kwargs = dict(reset_kwargs)
        if original_supports_app_ids:
            kwargs["app_ids"] = app_ids
        await _await_reset_result(original_reset(self, **kwargs))

    async def resilient_reset(
        self: Any,
        app_ids: list[str] | None = None,
        **reset_kwargs: Any,
    ) -> None:
        try:
            await call_original_reset(self, app_ids, reset_kwargs)
            return
        except Exception as exc:
            if not _is_recoverable_page_crash(exc):
                raise
            logger.warning("MobileGym reset recovered by recreating page after: %s", exc)
            await _restart_mobilegym_env(self)
            await call_original_reset(self, app_ids, reset_kwargs)

    mobilegym_env_cls._aico_original_reset = original_reset
    mobilegym_env_cls.reset = resilient_reset
    mobilegym_env_cls._aico_resilient_reset_installed = True


def _is_recoverable_page_crash(exc: Exception) -> bool:
    message = f"{type(exc).__name__}: {exc}".lower()
    return any(
        marker in message
        for marker in (
            "page crashed",
            "target crashed",
            "target closed",
            "browser closed",
            "has been closed",
        )
    )


async def _restart_mobilegym_env(env: Any) -> None:
    restart = getattr(env, "restart", None)
    if restart is not None:
        result = restart()
        if asyncio.iscoroutine(result) or isinstance(result, Awaitable):
            await result
        return

    close = getattr(env, "close", None)
    if close is not None:
        result = close()
        if asyncio.iscoroutine(result) or isinstance(result, Awaitable):
            await result
    start = getattr(env, "start", None)
    if start is None:
        raise RuntimeError("MobileGym env cannot restart after page crash")
    result = start()
    if asyncio.iscoroutine(result) or isinstance(result, Awaitable):
        await result


def prepare_import_paths(mobilegym_root: str | Path) -> None:
    """Add MobileGym to Python path."""
    benchmark_entry = str(BENCHMARK_ROOT)
    mobilegym_entry = str(Path(mobilegym_root).expanduser())
    existing = [entry for entry in sys.path if entry not in {benchmark_entry, mobilegym_entry}]
    ordered = [benchmark_entry]
    if mobilegym_entry != benchmark_entry:
        ordered.append(mobilegym_entry)
    sys.path[:] = ordered + existing


def resolve_mobilegym_root(cli_root: str | Path | None) -> tuple[Path, str]:
    """Resolve MobileGym installation path."""
    if cli_root is not None:
        return Path(cli_root).expanduser(), "--mobilegym-root"
    env_root = os.getenv("MOBILEGYM_ROOT")
    if env_root:
        return Path(env_root).expanduser(), "MOBILEGYM_ROOT"
    return DEFAULT_MOBILEGYM_ROOT, "benchmark/mobilegym/vendor/mobilegym"


def validate_mobilegym_root(path: Path, source: str) -> None:
    """Validate that MobileGym installation exists."""
    if not path.exists() or not path.is_dir():
        raise SimulatorError(
            f"MobileGym root not found at {path} (from {source}). "
            "Initialize the submodule with: "
            "`git submodule update --init --recursive benchmark/mobilegym/vendor/mobilegym`"
        )
    bench_env = path / "bench_env"
    if not bench_env.exists():
        raise SimulatorError(
            f"MobileGym root at {path} does not contain bench_env/. "
            "Pass the upstream MobileGym repository root."
        )


async def create_mobilegym_env(env_url: str, headless: bool, device: str = "sim") -> tuple[Any, Any]:
    """Create MobileGym environment without using factory/runner framework.

    Returns:
        Tuple of (env, config) - the environment instance and its config
    """
    try:
        from bench_env.config import EnvConfig
        from bench_env.env.mobile import MobileEnv
    except (ImportError, ModuleNotFoundError):
        try:
            from bench_env.env import MobileGymEnv
        except ModuleNotFoundError as exc:
            raise SimulatorError(
                "Unable to import MobileGym modules. "
                "Install dependencies with: "
                "`pip install -r bench_env/requirements.txt && playwright install chromium`"
            ) from exc

        config = {
            "env_url": env_url,
            "headless": headless,
            "device": device,
            "coord_space": "norm_0_1000",
            "delay_after_action": 1.0,
        }
        env = MobileGymEnv(
            url=env_url,
            headless=headless,
            coord_space="norm_0_1000",
            delay_after_action=1.0,
            verbose=True,
            viewport_size=(360, 800),
            physical_size=(1080, 2400),
            device_scale_factor=3,
        )
        install_resilient_mobilegym_reset(MobileGymEnv)
        await env.start()
        await _reset_mobilegym_env(env, app_ids=[])
        return env, config
    else:
        config = EnvConfig(
            env_url=env_url,
            headless=headless,
            device=device,
            coord_space="norm_0_1000",
            delay_after_action=1.0,
            screenshot_scale=1.0,
        )
        env = MobileEnv(config)
        await env.reset()
        return env, config


async def create_mobilegym_env_pool(
    env_url: str,
    headless: bool,
    device: str,
    n: int,
    isolation: str = "pages",
    num_browsers: int = 0,
) -> tuple[Any, list[Any], Any]:
    """Create a pool of MobileGym environments for task-id routing."""
    if n <= 1:
        env, config = await create_mobilegym_env(env_url, headless=headless, device=device)
        return None, [env], config
    if device != "sim":
        raise SimulatorError("--parallel-envs > 1 is only supported with --device sim")

    try:
        from bench_env.env import EnvPool
    except ModuleNotFoundError as exc:
        raise SimulatorError(
            "Unable to import MobileGym EnvPool. "
            "Install dependencies with: "
            "`pip install -r bench_env/requirements.txt && playwright install chromium`"
        ) from exc
    try:
        from bench_env.env import MobileGymEnv
    except (ImportError, AttributeError):
        MobileGymEnv = None
    if MobileGymEnv is not None:
        install_resilient_mobilegym_reset(MobileGymEnv)

    pool = EnvPool(
        url=env_url,
        n=n,
        isolation=isolation,
        num_browsers=num_browsers,
        headless=headless,
        coord_space="norm_0_1000",
        delay_after_action=1.0,
        verbose=True,
    )
    await pool.__aenter__()
    envs = list(pool.envs)
    for env in envs:
        await _reset_mobilegym_env(env, app_ids=[])
    config = {
        "env_url": env_url,
        "headless": headless,
        "device": "sim",
        "coord_space": "norm_0_1000",
        "delay_after_action": 1.0,
        "parallel_envs": n,
        "isolation": isolation,
        "num_browsers": num_browsers,
    }
    return pool, envs, config


async def run_simulator(args: argparse.Namespace) -> int:
    """Run MobileGym simulator with Bridge server."""
    if args.parallel_envs < 1:
        raise SimulatorError("--parallel-envs must be positive")
    if args.bridge_request_timeout_sec <= 0:
        raise SimulatorError("--bridge-request-timeout-sec must be positive")

    # Resolve and validate MobileGym installation
    mobilegym_root, source = resolve_mobilegym_root(args.mobilegym_root)
    validate_mobilegym_root(mobilegym_root, source)
    prepare_import_paths(mobilegym_root)

    # Import bridge components (no agent registration needed)
    from mobilegym.bridge.episode import BridgeEpisodeState, BridgeTaskRouter
    from mobilegym.bridge.server import BridgeServer

    print("Creating MobileGym simulator environment...", flush=True)
    print(f"  Simulator URL: {args.env_url}", flush=True)
    print(f"  Headless: {args.headless}", flush=True)
    print(f"  Parallel envs: {args.parallel_envs}", flush=True)

    # Create environment(s)
    env_pool, envs, env_config = await create_mobilegym_env_pool(
        env_url=args.env_url,
        headless=args.headless,
        device=args.device,
        n=args.parallel_envs,
        isolation=args.env_isolation,
        num_browsers=args.env_browsers,
    )

    print(f"✓ MobileGym environment(s) created: {len(envs)}", flush=True)

    # Create bridge
    owner_loop = asyncio.get_running_loop()
    bridge_states = [BridgeEpisodeState(env, owner_loop) for env in envs]
    bridge_state = bridge_states[0] if len(bridge_states) == 1 else BridgeTaskRouter(bridge_states)
    bridge = BridgeServer(
        bridge_state,
        host=args.bridge_host,
        port=args.bridge_port,
        public_host=args.bridge_public_host or None,
        request_timeout_sec=args.bridge_request_timeout_sec,
    )

    bridge_url = bridge.start()
    print(f"✓ Bridge server started at {bridge_url}", flush=True)

    # Write bridge URL to file if requested
    if args.bridge_url_file:
        url_file = Path(args.bridge_url_file)
        url_file.parent.mkdir(parents=True, exist_ok=True)
        url_file.write_text(bridge_url)
        print(f"✓ Bridge URL written to {args.bridge_url_file}", flush=True)

    print("\n" + "=" * 60, flush=True)
    print("🚀 MobileGym Simulator Ready", flush=True)
    print("=" * 60, flush=True)
    print(f"Bridge URL: {bridge_url}", flush=True)
    print(f"Health check: curl {bridge_url}/health", flush=True)
    print("\nPress Ctrl+C to stop...", flush=True)
    print("=" * 60 + "\n", flush=True)

    # Setup signal handlers
    stop_event = asyncio.Event()

    def signal_handler(sig, frame):
        print("\n\nReceived shutdown signal, stopping...", flush=True)
        stop_event.set()

    signal.signal(signal.SIGINT, signal_handler)
    signal.signal(signal.SIGTERM, signal_handler)

    # Wait for stop signal
    try:
        await stop_event.wait()
    finally:
        print("Shutting down bridge server...", flush=True)
        bridge.stop()
        print("✓ Bridge stopped", flush=True)
        if env_pool is not None:
            print("Shutting down MobileGym env pool...", flush=True)
            await env_pool.__aexit__(None, None, None)
            print("✓ MobileGym env pool stopped", flush=True)
        else:
            print("Shutting down MobileGym environment...", flush=True)
            for env in envs:
                close_fn = getattr(env, "close", None) or getattr(env, "stop", None)
                if close_fn is None:
                    continue
                result = close_fn()
                if asyncio.iscoroutine(result) or isinstance(result, Awaitable):
                    await result
            print("✓ MobileGym environment stopped", flush=True)

    return 0


def build_parser() -> argparse.ArgumentParser:
    parser = argparse.ArgumentParser(
        description="Start MobileGym as a pure simulator with Bridge HTTP server.",
    )

    mobilegym = parser.add_argument_group("MobileGym simulator")
    mobilegym.add_argument(
        "--mobilegym-root",
        type=Path,
        help="Path to MobileGym installation (default: benchmark/mobilegym/vendor/mobilegym)",
    )
    mobilegym.add_argument(
        "--env-url",
        default=os.getenv("MOBILEGYM_ENV_URL", DEFAULT_ENV_URL),
        help="MobileGym web simulator URL (default: http://localhost:4173)",
    )
    mobilegym.add_argument(
        "--headless",
        action="store_true",
        help="Run browser in headless mode",
    )
    mobilegym.add_argument(
        "--device",
        default="sim",
        choices=["sim", "real"],
        help="Device type (default: sim)",
    )
    mobilegym.add_argument(
        "--parallel-envs",
        type=int,
        default=int(os.getenv("MOBILEGYM_PARALLEL_ENVS", "1")),
        help="Number of MobileGym envs behind one bridge (default: 1)",
    )
    mobilegym.add_argument(
        "--env-isolation",
        default=os.getenv("MOBILEGYM_ENV_ISOLATION", "pages"),
        choices=["pages", "contexts", "browsers"],
        help="MobileGym EnvPool isolation mode when --parallel-envs > 1 (default: pages)",
    )
    mobilegym.add_argument(
        "--env-browsers",
        type=int,
        default=int(os.getenv("MOBILEGYM_ENV_BROWSERS", "0")),
        help="Browser process count for EnvPool when --parallel-envs > 1 (default: auto)",
    )

    bridge = parser.add_argument_group("Bridge server")
    bridge.add_argument(
        "--bridge-host",
        default=os.getenv("AICO_BRIDGE_BIND_HOST", "127.0.0.1"),
        help="Bridge server bind host (default: 127.0.0.1)",
    )
    bridge.add_argument(
        "--bridge-port",
        type=int,
        default=int(os.getenv("AICO_BRIDGE_PORT", "0")),
        help="Bridge server port (default: auto-assign)",
    )
    bridge.add_argument(
        "--bridge-public-host",
        default=os.getenv("AICO_BRIDGE_PUBLIC_HOST"),
        help="Bridge server public hostname (for Docker networking)",
    )
    bridge.add_argument(
        "--bridge-url-file",
        type=Path,
        help="Write bridge URL to this file",
    )
    bridge.add_argument(
        "--bridge-request-timeout-sec",
        type=float,
        default=DEFAULT_BRIDGE_REQUEST_TIMEOUT_SEC,
        help=f"Bridge request timeout in seconds (default: {DEFAULT_BRIDGE_REQUEST_TIMEOUT_SEC})",
    )

    return parser


def main(argv: list[str] | None = None) -> int:
    parser = build_parser()
    args = parser.parse_args(argv)

    try:
        return asyncio.run(run_simulator(args))
    except SimulatorError as exc:
        print(f"error: {exc}", file=sys.stderr)
        return 2
    except KeyboardInterrupt:
        print("\nInterrupted", file=sys.stderr)
        return 130


if __name__ == "__main__":
    sys.exit(main())
