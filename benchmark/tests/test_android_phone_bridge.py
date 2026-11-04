import json

import pytest

from runner import android_phone_bridge


def test_android_bridge_routes_and_cleans_up(monkeypatch) -> None:
    commands = []

    def fake_adb(serial, *args):
        commands.append((serial, args))
        return "0" if args == ("shell", "id", "-u") else ""

    monkeypatch.setattr(android_phone_bridge, "_adb", fake_adb)
    monkeypatch.setattr(android_phone_bridge, "wait_for_android_phone_bridge", lambda client: None)

    with android_phone_bridge.connected_android_phone_bridge("emulator-5554", 32123, object()):
        assert commands[1][1] == ("reverse", "tcp:8080", "tcp:32123")
        assert commands[2][1][:6] == ("shell", "iptables", "-t", "nat", "-I", "OUTPUT")
        assert commands[3][1] == ("shell", "am", "force-stop", "link.aico.app")
        assert commands[4][1] == ("shell", "am", "start", "-n", "link.aico.app/.MainActivity")

    assert commands[-2][1][:6] == ("shell", "iptables", "-t", "nat", "-D", "OUTPUT")
    assert commands[-1][1] == ("reverse", "--remove", "tcp:8080")


def test_android_bridge_cleans_up_when_app_does_not_connect(monkeypatch) -> None:
    commands = []

    def fake_adb(serial, *args):
        commands.append(args)
        return "0" if args == ("shell", "id", "-u") else ""

    def disconnected(client):
        raise RuntimeError("App did not connect")

    monkeypatch.setattr(android_phone_bridge, "_adb", fake_adb)
    monkeypatch.setattr(android_phone_bridge, "wait_for_android_phone_bridge", disconnected)

    with pytest.raises(RuntimeError, match="App did not connect"):
        with android_phone_bridge.connected_android_phone_bridge("emulator-5554", 32123, object()):
            pass

    assert commands[-2][:6] == ("shell", "iptables", "-t", "nat", "-D", "OUTPUT")
    assert commands[-1] == ("reverse", "--remove", "tcp:8080")


def test_foreground_app_waits_for_active_bridge(monkeypatch) -> None:
    calls = []
    monkeypatch.setattr(
        android_phone_bridge, "_adb",
        lambda serial, *args: calls.append((serial, args)),
    )
    monkeypatch.setattr(
        android_phone_bridge, "wait_for_android_phone_bridge",
        lambda client, **kwargs: calls.append(kwargs),
    )

    android_phone_bridge.foreground_android_phone_bridge("emulator-5554", object())

    assert calls[0] == (
        "emulator-5554",
        ("shell", "am", "start", "-n", "link.aico.app/.MainActivity"),
    )
    assert calls[1] == {"timeout_sec": 30, "require_active": True}


def _status_client(*statuses):
    class Client:
        def __init__(self):
            self.remaining = list(statuses)

        def _get(self, path, timeout):
            assert path == "/api/phone-bridge/status"
            status = self.remaining.pop(0) if len(self.remaining) > 1 else self.remaining[0]
            return 200, json.dumps(status)

    return Client()


def test_background_bridge_wait_requires_fgs_poller(monkeypatch) -> None:
    clock = iter(range(0, 1000, 10))
    monkeypatch.setattr(android_phone_bridge.time, "monotonic", lambda: next(clock))
    monkeypatch.setattr(android_phone_bridge.time, "sleep", lambda seconds: None)
    websocket_only = {"connected": True, "platform": "android", "app_state": "background"}

    with pytest.raises(RuntimeError, match="did not become ready"):
        android_phone_bridge.wait_for_android_phone_bridge(
            _status_client(websocket_only), timeout_sec=30, require_background_bridge=True,
        )
    android_phone_bridge.wait_for_android_phone_bridge(
        _status_client(websocket_only, {**websocket_only, "fgs_bridge_enabled": True}),
        timeout_sec=60, require_background_bridge=True,
    )


def test_ethernet_ipconfig_is_static_board_address() -> None:
    data = android_phone_bridge._ethernet_ipconfig()

    assert data[:4] == b"\x00\x00\x00\x03"
    for token in (b"ipAssignment", b"STATIC", b"192.168.42.2", b"eth1", b"eos"):
        assert token in data
    prefix_at = data.index(b"192.168.42.2") + len(b"192.168.42.2")
    assert data[prefix_at:prefix_at + 4] == b"\x00\x00\x00\x18"


def test_emulator_ethernet_skips_restart_when_feature_present(monkeypatch) -> None:
    calls = []
    monkeypatch.setattr(android_phone_bridge, "_adb", lambda serial, *args: calls.append(args)
                        or "feature:android.hardware.touchscreen\nfeature:android.hardware.ethernet")

    assert android_phone_bridge.ensure_emulator_ethernet("emulator-5554") is False
    assert calls == [("shell", "pm", "list", "features")]


def test_emulator_ethernet_enables_feature_and_restarts_framework(monkeypatch) -> None:
    calls = []
    features = iter(["feature:android.hardware.touchscreen", "feature:android.hardware.ethernet"])

    def fake_adb(serial, *args):
        calls.append(args)
        if args == ("shell", "pm", "list", "features"):
            return next(features)
        return "1" if args[-1] == "sys.boot_completed" else ""

    monkeypatch.setattr(android_phone_bridge, "_adb", fake_adb)
    monkeypatch.setattr(android_phone_bridge.time, "sleep", lambda seconds: None)

    assert android_phone_bridge.ensure_emulator_ethernet("emulator-5554") is True
    assert calls[1][0] == "push" and calls[1][2] == "/data/local/tmp/aico-ipconfig.txt"
    install = calls[2][1]
    assert "mount -o bind /data/local/tmp/aico-ethernet-permissions /vendor/etc/permissions" in install
    assert "android.hardware.ethernet" in install
    assert "/data/misc/apexdata/com.android.tethering/misc/ethernet/ipconfig.txt" in install
    assert calls[3:5] == [("shell", "stop"), ("shell", "start")]


def test_emulator_board_network_adds_and_always_removes_veth(monkeypatch) -> None:
    calls = []
    monkeypatch.setattr(android_phone_bridge, "ensure_emulator_ethernet", lambda serial: False)
    monkeypatch.setattr(android_phone_bridge.time, "sleep", lambda seconds: None)
    monkeypatch.setattr(
        android_phone_bridge, "_adb",
        lambda serial, *args: calls.append(args) or "eth1@aicobd UP 192.168.42.2/24",
    )

    with pytest.raises(RuntimeError, match="task failed"):
        with android_phone_bridge.emulator_board_network("emulator-5554"):
            assert "ip link add name eth1 type veth peer name aicobd" in calls[1][1]
            assert calls[2] == ("shell", "ip", "-4", "-br", "addr", "show", "eth1")
            raise RuntimeError("task failed")

    assert calls[0] == calls[-1] == ("shell", "ip link del eth1 2>/dev/null; true")


def test_clipboard_write_events_ignores_other_diagnostics(monkeypatch) -> None:
    calls = []
    lines = '\n'.join([
        '{"component":"clipboard","event":"write_completed","fields":{"text_length":18}}',
        '{"component":"clipboard","event":"read_completed"}',
        'incomplete json',
    ])
    monkeypatch.setattr(android_phone_bridge, "_adb", lambda serial, *args: calls.append(args) or lines)

    assert android_phone_bridge.clipboard_write_events("emulator-5554") == [{
        "component": "clipboard", "event": "write_completed", "fields": {"text_length": 18},
    }]
    assert "/data/user/0/link.aico.app/files/diagnostic-*.jsonl" in calls[0][-1]


def test_bridge_input_requires_new_app_write_and_exact_other_app_field(monkeypatch) -> None:
    target = "Aiden résumé check #42"
    event = {"component": "clipboard", "event": "write_completed", "fields": {"text_length": len(target), "command_id": "write-current"}}
    xml = (
        '<hierarchy><node class="android.widget.EditText" package="com.android.settings" '
        f'text="{target}" /></hierarchy>'
    )
    monkeypatch.setattr(android_phone_bridge, "clipboard_write_events", lambda serial: [event])
    monkeypatch.setattr(android_phone_bridge.time, "sleep", lambda seconds: None)
    monkeypatch.setattr(
        android_phone_bridge, "_adb",
        lambda serial, *args: xml if args[:2] == ("shell", "cat") else "",
    )

    evidence = android_phone_bridge.verify_android_bridge_input("emulator-5554", target, {"write-old"})
    assert evidence["bridge_clipboard_write"] is True
    assert evidence["field_contains_exact_text"] is True
    assert evidence["target_package"] == "com.android.settings"

    evidence = android_phone_bridge.verify_android_bridge_input("emulator-5554", target, {"write-current"})
    assert evidence["bridge_clipboard_write"] is False
    assert evidence["field_contains_exact_text"] is True


@pytest.mark.parametrize("package,text", [
    ("link.aico.app", "Aiden résumé check #42"),
    ("com.android.settings", "Aiden résumé check #42 wrong"),
])
def test_bridge_write_alone_does_not_pass_input(monkeypatch, package, text):
    target = "Aiden résumé check #42"
    monkeypatch.setattr(android_phone_bridge, "clipboard_write_events", lambda serial: [
        {"fields": {"command_id": "current", "text_length": len(target)}}
    ])
    monkeypatch.setattr(android_phone_bridge, "_adb", lambda serial, *args:
                        f'<hierarchy><node class="android.widget.EditText" package="{package}" text="{text}" /></hierarchy>'
                        if args[:2] == ("shell", "cat") else "")
    evidence = android_phone_bridge.verify_android_bridge_input("emulator-5554", target, set())
    assert evidence["bridge_clipboard_write"] is True
    assert evidence["field_contains_exact_text"] is False


def test_calendar_fixture_cleans_up_after_failure(monkeypatch):
    calls = []
    monkeypatch.setattr(android_phone_bridge, "_content", lambda *args: calls.append(args) or "")
    with pytest.raises(RuntimeError, match="task failed"):
        with android_phone_bridge.android_bridge_fixture("emulator-5554", "calendar"):
            raise RuntimeError("task failed")
    assert [call[1] for call in calls] == ["delete", "insert", "delete"]
    assert calls[0] == calls[-1]
    assert calls[-1][-1] == "account_name='AidenBridgeBenchmark' AND account_type='LOCAL'"


def test_contact_fixture_deletes_only_reserved_raw_contact_ids(monkeypatch):
    calls = []
    def content(*args):
        calls.append(args)
        return "Row: 0 _id=42\nRow: 1 _id=73" if args[1] == "query" else ""
    monkeypatch.setattr(android_phone_bridge, "_content", content)
    with android_phone_bridge.android_bridge_fixture("emulator-5554", "contacts"):
        pass
    assert [call[2] for call in calls if call[1] == "delete"] == [
        "content://com.android.contacts/raw_contacts/42?caller_is_syncadapter=true",
        "content://com.android.contacts/raw_contacts/73?caller_is_syncadapter=true",
    ] * 2
    assert all(call[-1] == "display_name='AidenBridgeBenchmarkContact'" for call in calls if call[1] == "query")
