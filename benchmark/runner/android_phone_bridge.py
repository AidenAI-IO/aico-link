"""Route the fixed AidenBridge board address to a per-task Agent on a rooted emulator."""

from __future__ import annotations

import json
import re
import shlex
import struct
import subprocess
import tempfile
import time
import xml.etree.ElementTree as ET
from contextlib import contextmanager
from typing import Iterator

from runner.agent_client import AgentClient, AgentRequestError, AgentTimeoutError


BOARD_ADDRESS = "192.168.42.1/32"
REVERSE_PORT = "tcp:8080"
INPUT_XML_PATH = "/sdcard/aiden-bridge-input.xml"
ANDROID_BRIDGE_PACKAGE = "link.aico.app"
ANDROID_BRIDGE_COMPONENT = f"{ANDROID_BRIDGE_PACKAGE}/.MainActivity"
NAT_RULE = [
    "-d", BOARD_ADDRESS, "-p", "tcp", "--dport", "8080",
    "-j", "DNAT", "--to-destination", "127.0.0.1:8080",
]

# The App's background (FGS) bridge polls only through a USB or Ethernet
# Network that routes to the board subnet. The emulator has neither, so a veth
# pair stands in for the board's USB ECM link: EthernetService provisions the
# App-facing end with the address a phone gets from the board.
BOARD_NET_INTERFACE = "eth1"
BOARD_NET_PEER = "aicobd"
BOARD_NET_PHONE_ADDRESS = "192.168.42.2"
BOARD_NET_BOARD_ADDRESS = "192.168.42.1/24"
ETHERNET_FEATURE = "android.hardware.ethernet"
ETHERNET_PERMISSIONS_DIR = "/vendor/etc/permissions"
ETHERNET_PERMISSIONS_COPY = "/data/local/tmp/aico-ethernet-permissions"
# Android 13+ keeps Ethernet IP configuration in the Tethering APEX data dir.
ETHERNET_IPCONFIG_DIRS = (
    "/data/misc/apexdata/com.android.tethering/misc/ethernet",
    "/data/misc/ethernet",
)


def _adb(serial: str, *args: str) -> str:
    command = ["adb", "-s", serial, *args]
    completed = subprocess.run(command, capture_output=True, text=True, check=False, timeout=30)
    if completed.returncode:
        raise RuntimeError(
            f"{' '.join(command)} failed: {completed.stderr.strip() or completed.stdout.strip()}"
        )
    return completed.stdout.strip()


def wait_for_android_phone_bridge(
    client: AgentClient,
    timeout_sec: int = 60,
    *,
    require_active: bool = False,
    require_background_bridge: bool = False,
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
                    and (not require_background_bridge
                         or status.get("fgs_bridge_enabled") is True)
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
    _adb(serial, "shell", "am", "start", "-n", ANDROID_BRIDGE_COMPONENT)
    wait_for_android_phone_bridge(client, timeout_sec=30, require_active=True)


def clipboard_write_events(serial: str) -> list[dict]:
    script = (
        f'for file in /data/user/0/{ANDROID_BRIDGE_PACKAGE}/files/diagnostic-*.jsonl; '
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
        and node.attrib.get("package") != ANDROID_BRIDGE_PACKAGE
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


def _ethernet_ipconfig() -> bytes:
    """Serialize a static eth1 configuration in Android's IpConfigStore v3 format."""
    def utf(value: str) -> bytes:
        encoded = value.encode()
        return struct.pack(">H", len(encoded)) + encoded

    return b"".join([
        struct.pack(">i", 3),
        utf("ipAssignment"), utf("STATIC"),
        utf("linkAddress"), utf(BOARD_NET_PHONE_ADDRESS), struct.pack(">i", 24),
        utf("proxySettings"), utf("NONE"),
        utf("id"), utf(BOARD_NET_INTERFACE),
        utf("eos"),
    ])


def _wait_for_boot(serial: str, timeout_sec: int = 180) -> None:
    deadline = time.monotonic() + timeout_sec
    while time.monotonic() < deadline:
        try:
            if _adb(serial, "shell", "getprop", "sys.boot_completed") == "1":
                return
        except (RuntimeError, subprocess.TimeoutExpired):
            pass
        time.sleep(2)
    raise RuntimeError("Android framework did not finish restarting")


def ensure_emulator_ethernet(serial: str) -> bool:
    """Enable EthernetService with a static eth1 config; returns True if it restarted.

    Emulator images omit the Ethernet feature, so system_server never starts
    EthernetService. The feature file is added through a bind mount (lost on
    reboot, so this check repeats every run), and the framework restarts once
    to load it together with the static configuration.
    """
    features = _adb(serial, "shell", "pm", "list", "features")
    feature_present = f"feature:{ETHERNET_FEATURE}" in features.splitlines()
    with tempfile.NamedTemporaryFile(suffix="ipconfig.txt") as ipconfig:
        ipconfig.write(_ethernet_ipconfig())
        ipconfig.flush()
        _adb(serial, "push", ipconfig.name, "/data/local/tmp/aico-ipconfig.txt")
    config_present = _adb(serial, "shell", " && ".join(
        f"cmp -s /data/local/tmp/aico-ipconfig.txt {path}/ipconfig.txt"
        for path in ETHERNET_IPCONFIG_DIRS
    ) + " && echo present || echo missing") == "present"
    if feature_present and config_present:
        return False
    install = []
    if not feature_present:
        feature_xml = (
            '<?xml version="1.0" encoding="utf-8"?>\n'
            f'<permissions><feature name="{ETHERNET_FEATURE}" /></permissions>\n'
        )
        install.extend([
            f"rm -rf {ETHERNET_PERMISSIONS_COPY}",
            f"mkdir -p {ETHERNET_PERMISSIONS_COPY}",
            f"cp -a {ETHERNET_PERMISSIONS_DIR}/. {ETHERNET_PERMISSIONS_COPY}/",
            f"printf %s {shlex.quote(feature_xml)} > {ETHERNET_PERMISSIONS_COPY}/{ETHERNET_FEATURE}.xml",
            # Images may ship dangling symlinks here (e.g. extra_feature.xml), so
            # touch only the new file's mode and never follow links when relabeling.
            f"chmod 644 {ETHERNET_PERMISSIONS_COPY}/{ETHERNET_FEATURE}.xml",
            f"chcon -hR u:object_r:vendor_configs_file:s0 {ETHERNET_PERMISSIONS_COPY}",
            f"mount -o bind {ETHERNET_PERMISSIONS_COPY} {ETHERNET_PERMISSIONS_DIR}",
        ])
    if not config_present:
        install.extend([
            f"mkdir -p {path} && cp /data/local/tmp/aico-ipconfig.txt {path}/ipconfig.txt"
            f" && chown system:system {path} {path}/ipconfig.txt && chmod 600 {path}/ipconfig.txt"
            for path in ETHERNET_IPCONFIG_DIRS
        ])
        install.append(f"restorecon -R {ETHERNET_IPCONFIG_DIRS[0]}")
    _adb(serial, "shell", " && ".join(install))
    _adb(serial, "shell", "stop")
    _adb(serial, "shell", "start")
    time.sleep(10)
    _wait_for_boot(serial)
    features = _adb(serial, "shell", "pm", "list", "features")
    if f"feature:{ETHERNET_FEATURE}" not in features.splitlines():
        raise RuntimeError("Android EthernetService did not start after enabling its feature")
    return True


def remove_board_network(serial: str) -> None:
    """Delete the board veth; deleting one end removes its peer. Missing is fine."""
    _adb(serial, "shell", f"ip link del {BOARD_NET_INTERFACE} 2>/dev/null; true")


@contextmanager
def emulator_board_network(serial: str, timeout_sec: int = 90) -> Iterator[None]:
    """Give the emulator a board-like Ethernet link for the App's FGS Bridge.

    While the link is up, Android prefers it as the default network, which
    has no internet route, so the emulator is offline until it is removed.
    """
    ensure_emulator_ethernet(serial)
    remove_board_network(serial)
    try:
        _adb(serial, "shell", " && ".join([
            f"ip link add name {BOARD_NET_INTERFACE} type veth peer name {BOARD_NET_PEER}",
            f"ip addr add {BOARD_NET_BOARD_ADDRESS} dev {BOARD_NET_PEER}",
            f"ip link set {BOARD_NET_PEER} up",
            f"ip link set {BOARD_NET_INTERFACE} up",
        ]))
        deadline = time.monotonic() + timeout_sec
        while BOARD_NET_PHONE_ADDRESS + "/" not in _adb(
            serial, "shell", "ip", "-4", "-br", "addr", "show", BOARD_NET_INTERFACE,
        ):
            if time.monotonic() > deadline:
                raise RuntimeError("Android did not provision the emulator board network")
            time.sleep(2)
        yield
    finally:
        remove_board_network(serial)


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
        _adb(serial, "shell", "am", "force-stop", ANDROID_BRIDGE_PACKAGE)
        _adb(serial, "shell", "am", "start", "-n", ANDROID_BRIDGE_COMPONENT)
        wait_for_android_phone_bridge(client)
        yield
    finally:
        try:
            if rule_installed:
                _adb(serial, "shell", "iptables", "-t", "nat", "-D", "OUTPUT", *NAT_RULE)
        finally:
            _adb(serial, "reverse", "--remove", REVERSE_PORT)
