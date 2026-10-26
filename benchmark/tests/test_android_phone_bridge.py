import pytest

from runner import android_phone_bridge


def test_android_bridge_routes_and_cleans_up(monkeypatch) -> None:
    commands = []

    def fake_adb(serial, *args):
        commands.append((serial, args))
        return "0" if args == ("shell", "id", "-u") else ""

    monkeypatch.setattr(android_phone_bridge, "_adb", fake_adb)
    monkeypatch.setattr(android_phone_bridge, "_wait_for_connection", lambda client: None)

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
    monkeypatch.setattr(android_phone_bridge, "_wait_for_connection", disconnected)

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
        android_phone_bridge, "_wait_for_connection",
        lambda client, **kwargs: calls.append(kwargs),
    )

    android_phone_bridge.foreground_android_phone_bridge("emulator-5554", object())

    assert calls[0] == (
        "emulator-5554",
        ("shell", "am", "start", "-n", "com.aidenbridge/.MainActivity"),
    )
    assert calls[1] == {"timeout_sec": 30, "require_active": True}
