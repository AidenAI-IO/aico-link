"""Atomically bound the persistent Agent log without stopping its writer."""

from __future__ import annotations

import ctypes
import errno
import os
import stat
import sys
import tomllib
from typing import Any

MEBIBYTE = 1024 * 1024
DEFAULT_NORMAL_MAX_BYTES = 10 * MEBIBYTE
DEFAULT_NORMAL_RETAIN_BYTES = 5 * MEBIBYTE
DEFAULT_DEGRADED_MAX_BYTES = MEBIBYTE
FALLOC_FL_COLLAPSE_RANGE = 0x08


class RetentionError(RuntimeError):
    """Report a retention failure without exposing a traceback to systemd."""


def positive_environment_integer(name: str, default: int) -> int:
    """Read a positive integer environment override or return its default."""
    raw_value = os.environ.get(name, "")
    try:
        value = int(raw_value, 10)
    except ValueError:
        return default
    return value if value > 0 else default


def nested_mapping(value: Any, key: str) -> dict[str, Any]:
    """Return a nested TOML table when it has the expected mapping shape."""
    if not isinstance(value, dict):
        return {}
    nested = value.get(key)
    return nested if isinstance(nested, dict) else {}


def configured_degraded_max_bytes(config_path: str) -> int:
    """Load the degraded Agent log limit from grouped Agent TOML."""
    try:
        with open(config_path, "rb") as config_file:
            config = tomllib.load(config_file)
    except FileNotFoundError:
        return DEFAULT_DEGRADED_MAX_BYTES
    except (OSError, tomllib.TOMLDecodeError) as error:
        raise RetentionError(f"cannot read Agent config {config_path}: {error}") from error

    storage_settings = nested_mapping(config, "storage_settings")
    storage = nested_mapping(storage_settings, "storage")
    if not storage:
        storage = nested_mapping(config, "storage")
    degraded_mode = nested_mapping(storage, "degraded_mode")
    max_mebibytes = degraded_mode.get("max_agent_log_mb")
    if type(max_mebibytes) is not int or max_mebibytes <= 0:
        return DEFAULT_DEGRADED_MAX_BYTES
    return max_mebibytes * MEBIBYTE


def storage_level(path: str) -> str:
    """Read the deployment storage level, defaulting to normal when absent."""
    try:
        with open(path, encoding="ascii") as level_file:
            return level_file.read().strip()
    except FileNotFoundError:
        return "normal"
    except (OSError, UnicodeError) as error:
        raise RetentionError(f"cannot read storage level {path}: {error}") from error


def fallocate_collapse_range(file_descriptor: int, length: int) -> None:
    """Atomically remove an aligned prefix while preserving the open inode."""
    libc = ctypes.CDLL(None, use_errno=True)
    fallocate = getattr(libc, "fallocate64", None) or getattr(libc, "fallocate", None)
    if fallocate is None:
        raise RetentionError("libc does not provide fallocate")
    fallocate.argtypes = [ctypes.c_int, ctypes.c_int, ctypes.c_longlong, ctypes.c_longlong]
    fallocate.restype = ctypes.c_int
    if fallocate(file_descriptor, FALLOC_FL_COLLAPSE_RANGE, 0, length) != 0:
        error_number = ctypes.get_errno()
        raise OSError(error_number, os.strerror(error_number))


def collapse_log(
    file_descriptor: int, current_bytes: int, retain_bytes: int, block_size: int
) -> int:
    """Remove enough aligned leading bytes to leave no more than retain_bytes."""
    required_collapse = current_bytes - retain_bytes
    collapse_bytes = ((required_collapse + block_size - 1) // block_size) * block_size
    largest_valid_collapse = ((current_bytes - 1) // block_size) * block_size
    collapse_bytes = min(collapse_bytes, largest_valid_collapse)

    # Linux serializes collapse-range with append writes on the inode. Appends
    # made before or during this syscall therefore remain at the new EOF.
    fallocate_collapse_range(file_descriptor, collapse_bytes)
    return os.fstat(file_descriptor).st_size


def retain_agent_log() -> None:
    """Apply the active policy through one no-follow Agent log descriptor."""
    log_path = os.environ.get("AICO_AGENT_LOG_PATH", "/userdata/agent/log/agent.log")
    level_path = os.environ.get("AICO_AGENT_STORAGE_LEVEL_PATH", "/run/agent/storage_level")
    config_path = os.environ.get("AICO_AGENT_CONFIG_PATH", "/userdata/agent/agent.toml")
    level = storage_level(level_path)

    max_bytes = positive_environment_integer(
        "AICO_AGENT_LOG_MAX_BYTES", DEFAULT_NORMAL_MAX_BYTES
    )
    retain_bytes = min(
        positive_environment_integer(
            "AICO_AGENT_LOG_RETAIN_BYTES", DEFAULT_NORMAL_RETAIN_BYTES
        ),
        max_bytes,
    )
    if level in {"critical", "emergency"}:
        max_bytes = configured_degraded_max_bytes(config_path)
        retain_bytes = max_bytes

    no_follow = getattr(os, "O_NOFOLLOW", None)
    if no_follow is None:
        raise RetentionError("platform does not provide O_NOFOLLOW")
    flags = os.O_RDWR | getattr(os, "O_CLOEXEC", 0) | no_follow
    try:
        file_descriptor = os.open(log_path, flags)
    except FileNotFoundError:
        return
    except OSError as error:
        if error.errno == errno.ELOOP:
            raise RetentionError(f"refusing symlink Agent log: {log_path}") from error
        raise RetentionError(f"cannot open Agent log {log_path}: {error}") from error

    try:
        file_info = os.fstat(file_descriptor)
        if not stat.S_ISREG(file_info.st_mode):
            raise RetentionError(f"refusing non-regular Agent log: {log_path}")
        current_bytes = file_info.st_size
        filesystem = os.fstatvfs(file_descriptor)
        block_size = filesystem.f_frsize or filesystem.f_bsize
        if block_size <= 0:
            raise RetentionError("cannot determine Agent log filesystem block size")

        # collapse-range requires block-aligned input. A smaller configured
        # byte override cannot be honored atomically, so one block is the
        # minimum effective bound. Production limits are expressed in MiB.
        max_bytes = max(max_bytes, block_size)
        retain_bytes = max(retain_bytes, block_size)
        if current_bytes <= max_bytes:
            return
        final_bytes = collapse_log(
            file_descriptor, current_bytes, retain_bytes, block_size
        )
    finally:
        os.close(file_descriptor)

    print(
        "agent_log_trimmed"
        f" previous_bytes={current_bytes} retained_bytes={final_bytes}"
        f" max_bytes={max_bytes} level={level or 'normal'}"
    )


def main() -> int:
    """Run retention and convert expected operational failures into one line."""
    try:
        retain_agent_log()
    except (OSError, RetentionError) as error:
        print(f"agent_log_retention_failed error={error}", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
