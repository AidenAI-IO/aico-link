from __future__ import annotations

import os
import signal
import subprocess
import time


def _group_alive(pgid: int, timeout_sec: float) -> bool:
    """Wait for a process group to empty; True if members are still running afterwards."""
    deadline = time.monotonic() + timeout_sec
    while True:
        try:
            os.killpg(pgid, 0)
        except (ProcessLookupError, PermissionError):
            return False
        if time.monotonic() >= deadline:
            return True
        time.sleep(0.05)


def terminate_process_tree(
    proc: subprocess.Popen | None,
    timeout_sec: float = 3.0,
) -> None:
    """Stop a process group, escalating to SIGKILL after a short grace period."""
    if proc is None or proc.poll() is not None:
        return
    try:
        if os.name == "posix":
            os.killpg(proc.pid, signal.SIGTERM)
        else:
            proc.terminate()
    except ProcessLookupError:
        return
    except Exception:
        try:
            proc.terminate()
        except Exception:
            return
    try:
        proc.wait(timeout=timeout_sec)
        # The leader exited, but members of its group that ignored SIGTERM may remain.
        if os.name != "posix" or not _group_alive(proc.pid, timeout_sec):
            return
    except subprocess.TimeoutExpired:
        pass
    except Exception:
        return
    try:
        if os.name == "posix":
            os.killpg(proc.pid, signal.SIGKILL)
        else:
            proc.kill()
    except ProcessLookupError:
        return
    except Exception:
        try:
            proc.kill()
        except Exception:
            return
    try:
        proc.wait(timeout=1)
    except Exception:
        return
