"""Route the fixed AidenBridge board address to a per-task Agent on a rooted emulator."""

from __future__ import annotations

import json
import re
import shlex
import subprocess
import time
import xml.etree.ElementTree as ET
from contextlib import contextmanager
from typing import Iterator

from runner.agent_client import AgentClient, AgentRequestError, AgentTimeoutError


BOARD_ADDRESS = "192.168.42.1/32"
REVERSE_PORT = "tcp:8080"
INPUT_XML_PATH = "/sdcard/aiden-bridge-input.xml"
NAT_RULE = [
    "-d", BOARD_ADDRESS, "-p", "tcp", "--dport", "8080",
    "-j", "DNAT", "--to-destination", "127.0.0.1:8080",
]


def _adb(serial: str, *args: str) -> str:
    command = ["adb", "-s", serial, *args]
    completed = subprocess.run(command, capture_output=True, text=True, check=False, timeout=30)
    if completed.returncode:
        raise RuntimeError(
            f"{' '.join(command)} failed: {completed.stderr.strip() or completed.stdout.strip()}"
        )
    return completed.stdout.strip()


def _wait_for_connection(
    client: AgentClient, timeout_sec: int = 60, *, require_active: bool = False
) -> None:
    deadline = time.monotonic() + timeout_sec
    last_status: dict = {}
    while time.monotonic() < deadline:
        try:
            _, body = client._get("/api/phone-bridge/status", timeout=5)
            status = json.loads(body)
            if isinstance(status, dict):
                last_status = status
                if (
                    status.get("connected") is True
                    and status.get("platform") == "android"
                    and (not require_active or status.get("app_state") == "active")
                ):
                    return
        except (AgentRequestError, AgentTimeoutError, ValueError):
            pass
        time.sleep(2)
    raise RuntimeError(
        "AidenBridge did not become ready on the task Agent; "
        f"last status: {last_status}"
    )


def foreground_android_phone_bridge(serial: str, client: AgentClient) -> None:
    _adb(serial, "shell", "am", "start", "-n", "com.aidenbridge/.MainActivity")
    _wait_for_connection(client, timeout_sec=30, require_active=True)


def clipboard_write_events(serial: str) -> list[dict]:
    script = (
        'for file in /data/user/0/com.aidenbridge/files/diagnostic-*.jsonl; '
        'do if [ -f "$file" ]; then tail -n 2000 "$file"; fi; done'
    )
    output = _adb(
        serial, "shell", shlex.join(["sh", "-c", script]),
    )
    events = []
    for line in output.splitlines():
        try:
            event = json.loads(line)
        except ValueError:
            continue
        if isinstance(event, dict) and (event.get("component"), event.get("event")) == ("clipboard", "write_completed"):
            events.append(event)
    return events


def clipboard_write_ids(serial: str) -> set[str]:
    return {str(event.get("fields", {}).get("command_id")) for event in clipboard_write_events(serial)}


def verify_android_bridge_input(serial: str, text: str, prior_writes: set[str]) -> dict:
    # Android String.length counts UTF-16 code units, including emoji pairs.
    text_length = len(text.encode("utf-16-le")) // 2
    writes = []
    for _ in range(5):
        writes = [event for event in clipboard_write_events(serial)
                  if event.get("fields", {}).get("command_id")
                  and str(event["fields"]["command_id"]) not in prior_writes]
        if any(event.get("fields", {}).get("text_length") == text_length for event in writes):
            break
        time.sleep(0.2)
    bridge_wrote_text = any(
        event.get("fields", {}).get("text_length") == text_length
        for event in writes
    )
    try:
        _adb(serial, "shell", "uiautomator", "dump", INPUT_XML_PATH)
        raw_hierarchy = _adb(serial, "shell", "cat", INPUT_XML_PATH)
        hierarchy = ET.fromstring(raw_hierarchy)
    finally:
        _adb(serial, "shell", "rm", "-f", INPUT_XML_PATH)
    editable_fields = [
        node.attrib
        for node in hierarchy.iter("node")
        if "EditText" in node.attrib.get("class", "")
        and node.attrib.get("package") != "com.aidenbridge"
    ]
    matching_fields = [field for field in editable_fields if field.get("text") == text]
    return {
        "bridge_clipboard_write": bridge_wrote_text,
        "new_clipboard_writes": len(writes),
        "write_command_ids": [event["fields"]["command_id"] for event in writes],
        "field_contains_exact_text": bool(matching_fields),
        "target_package": matching_fields[0].get("package") if matching_fields else None,
        # Keep raw XML so newline escaping, widget classes, and actual field
        # contents can be inspected when visual and exact checks disagree.
        "ui_hierarchy_xml": raw_hierarchy,
        "editable_fields": editable_fields,
    }


def _content(serial: str, action: str, uri: str, *args: str) -> str:
    result = _adb(serial, "shell", shlex.join(["content", action, "--uri", uri, *args]))
    if "Error" in result or "Exception" in result:
        raise RuntimeError(f"Android content {action} failed: {result}")
    return result


@contextmanager
def android_bridge_fixture(serial: str, fixture: str | None) -> Iterator[None]:
    """Scope writable fixtures to this suite's reserved account/name on the emulator."""
    if fixture is None:
        yield
        return
    if fixture == "calendar":
        uri = ("content://com.android.calendar/calendars?caller_is_syncadapter=true"
               "&account_name=AidenBridgeBenchmark&account_type=LOCAL")
        selection = "account_name='AidenBridgeBenchmark' AND account_type='LOCAL'"

        def cleanup() -> None:
            _content(serial, "delete", uri, "--where", selection)

        cleanup()
        try:
            _content(serial, "insert", uri,
                     "--bind", "account_name:s:AidenBridgeBenchmark", "--bind", "account_type:s:LOCAL",
                     "--bind", "name:s:AidenBridgeBenchmark", "--bind", "calendar_displayName:s:AidenBridgeBenchmark",
                     "--bind", "ownerAccount:s:AidenBridgeBenchmark", "--bind", "calendar_access_level:i:700",
                     "--bind", "visible:i:1", "--bind", "sync_events:i:1", "--bind", "isPrimary:i:1")
            yield
        finally:
            cleanup()
    elif fixture == "contacts":
        def cleanup() -> None:
            # Resolve exact reserved-name rows; never delete unrelated contacts.
            uri = "content://com.android.contacts/raw_contacts"
            rows = _content(serial, "query", uri, "--projection", "_id",
                            "--where", "display_name='AidenBridgeBenchmarkContact'")
            for raw_id in re.findall(r"\b_id=(\d+)", rows):
                _content(serial, "delete", f"{uri}/{raw_id}?caller_is_syncadapter=true")

        cleanup()
        try:
            yield
        finally:
            cleanup()
    else:
        raise ValueError(f"Unsupported Android Bridge fixture: {fixture}")


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
        try:
            if rule_installed:
                _adb(serial, "shell", "iptables", "-t", "nat", "-D", "OUTPUT", *NAT_RULE)
        finally:
            _adb(serial, "reverse", "--remove", REVERSE_PORT)
