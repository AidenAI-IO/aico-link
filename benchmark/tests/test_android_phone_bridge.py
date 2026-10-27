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
        ("shell", "am", "start", "-n", "com.aidenbridge/.MainActivity"),
    )
    assert calls[1] == {"timeout_sec": 30, "require_active": True}


def test_clipboard_write_events_ignores_other_diagnostics(monkeypatch) -> None:
    lines = '\n'.join([
        '{"component":"clipboard","event":"write_completed","fields":{"text_length":18}}',
        '{"component":"clipboard","event":"read_completed"}',
        'incomplete json',
    ])
    monkeypatch.setattr(android_phone_bridge, "_adb", lambda serial, *args: lines)

    assert android_phone_bridge.clipboard_write_events("emulator-5554") == [{
        "component": "clipboard", "event": "write_completed", "fields": {"text_length": 18},
    }]


def test_bridge_input_requires_new_app_write_and_exact_other_app_field(monkeypatch) -> None:
    target = "Aiden 输入验证 #42"
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
    ("com.aidenbridge", "Aiden 输入验证 #42"),
    ("com.android.settings", "Aiden 输入验证 #42 wrong"),
])
def test_bridge_write_alone_does_not_pass_input(monkeypatch, package, text):
    target = "Aiden 输入验证 #42"
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
