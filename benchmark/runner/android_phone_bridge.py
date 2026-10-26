"""Route the fixed AidenBridge board address to a per-task Agent on a rooted emulator."""

from __future__ import annotations

import json
import subprocess
import time
from contextlib import contextmanager
from typing import Iterator

from runner.agent_client import AgentClient, AgentRequestError, AgentTimeoutError


BOARD_ADDRESS = "192.168.42.1/32"
REVERSE_PORT = "tcp:8080"
NAT_RULE = [
    "-d", BOARD_ADDRESS, "-p", "tcp", "--dport", "8080",
    "-j", "DNAT", "--to-destination", "127.0.0.1:8080",
]


def _adb(serial: str, *args: str) -> str:
    command = ["adb", "-s", serial, *args]
    completed = subprocess.run(command, capture_output=True, text=True, check=False)
    if completed.returncode:
        raise RuntimeError(
            f"{' '.join(command)} failed: {completed.stderr.strip() or completed.stdout.strip()}"
        )
    return completed.stdout.strip()


def _wait_for_connection(client: AgentClient, timeout_sec: int = 60) -> None:
    deadline = time.monotonic() + timeout_sec
    last_status: dict = {}
    while time.monotonic() < deadline:
        try:
            _, body = client._get("/api/phone-bridge/status", timeout=5)
            status = json.loads(body)
            if isinstance(status, dict):
                last_status = status
                if status.get("connected") is True and status.get("platform") == "android":
                    return
        except (AgentRequestError, AgentTimeoutError, ValueError):
            pass
        time.sleep(2)
    raise RuntimeError(
        "AidenBridge did not connect to the task Agent; "
        f"last status: {last_status}"
    )


@contextmanager
def connected_android_phone_bridge(
    serial: str, host_port: int, client: AgentClient
) -> Iterator[None]:
    if _adb(serial, "shell", "id", "-u") != "0":
        raise RuntimeError("Android Phone Bridge routing requires a rooted emulator")
    _adb(serial, "reverse", REVERSE_PORT, f"tcp:{host_port}")
    rule_installed = False
    try:
        _adb(serial, "shell", "iptables", "-t", "nat", "-I", "OUTPUT", "1", *NAT_RULE)
        rule_installed = True
        _adb(serial, "shell", "am", "force-stop", "com.aidenbridge")
        _adb(serial, "shell", "am", "start", "-n", "com.aidenbridge/.MainActivity")
        _wait_for_connection(client)
        yield
    finally:
        if rule_installed:
            _adb(serial, "shell", "iptables", "-t", "nat", "-D", "OUTPUT", *NAT_RULE)
        _adb(serial, "reverse", "--remove", REVERSE_PORT)
