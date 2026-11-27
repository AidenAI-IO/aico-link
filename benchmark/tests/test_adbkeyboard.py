import base64
import hashlib
import io
import json

import pytest

from adbandroid.bridge import adb as adb_module
from adbandroid.bridge.adb import (
    ADBAndroidDevice,
    ADBCommandError,
    ADB_KEYBOARD_IME,
    APPIUM_UNICODE_IME,
    UnicodeIMEPackage,
    _download_verified_ime_apk,
    encode_modified_utf7,
)
from adbandroid.scripts import start_bridge


@pytest.fixture
def keyboard(monkeypatch):
    device = ADBAndroidDevice("myron")
    state = {
        "ime": "example/.LatinIME",
        "installed": [ADB_KEYBOARD_IME],
        "enabled": True,
        "calls": [],
    }
    monkeypatch.setattr(adb_module, "_download_verified_ime_apk", lambda _package: b"test-apk")

    def run(args, **kwargs):
        state["calls"].append(args)
        if args[:2] == ["install", "-r"]:
            if state.get("fail_install"):
                return "Failure [INSTALL_FAILED_INVALID_APK]"
            ime = ADB_KEYBOARD_IME if device.unicode_input_method == "adbkeyboard" else APPIUM_UNICODE_IME
            state["installed"].append(ime)
            return "Success"
        if args == ["shell", "ime", "list", "-a", "-s"]:
            return "\n".join(state["installed"])
        if args == ["shell", "ime", "list", "-s"]:
            return ADB_KEYBOARD_IME if state["enabled"] else "example/.LatinIME"
        if args[:3] == ["shell", "ime", "enable"]:
            if state.get("fail_enable"):
                return "Error: could not enable input method"
            state["enabled"] = True
            return f"Input method {args[3]} successfully enabled"
        if args == ["shell", "settings", "get", "secure", "default_input_method"]:
            return state["ime"]
        if args[:3] == ["shell", "ime", "set"]:
            if state.get("fail_restore") and args[3] == "example/.LatinIME":
                raise ADBCommandError("restore failed")
            if not state.get("ignore_switch"):
                state["ime"] = args[3]
            return "selected"
        if args == ["shell", "dumpsys", "input_method"]:
            return ("Input method service state for com.android.adbkeyboard/.AdbIME:\n"
                    "  mInputStarted=true mStartedInputConnection=RemoteInputConnection{idHash=#123}")
        if args[:3] == ["shell", "am", "broadcast"]:
            if state.get("fail_broadcast"):
                raise ADBCommandError("private payload: " + " ".join(args))
            return state.get("broadcast_output", "Broadcast completed: result=0")
        raise AssertionError(args)

    monkeypatch.setattr(device, "_run_text", run)
    monkeypatch.setattr("adbandroid.bridge.adb.time.sleep", lambda _: None)
    return device, state


def test_base64_input_is_exact_package_scoped_and_restores_keyboard(keyboard):
    device, state = keyboard
    text = "你好，世界 😸 %s ' & $(echo test)\n第二行\t!"
    device.insert_text(text, replace=True)
    broadcasts = [args for args in state["calls"] if args[:3] == ["shell", "am", "broadcast"]]
    assert broadcasts == [
        ["shell", "am", "broadcast", "-p", "com.android.adbkeyboard", "-a", "ADB_CLEAR_TEXT"],
        ["shell", "am", "broadcast", "-p", "com.android.adbkeyboard", "-a", "ADB_INPUT_B64", "--es", "msg", base64.b64encode(text.encode()).decode()],
    ]
    assert state["ime"] == "example/.LatinIME"


def test_disabled_input_method_never_advertises_or_sends_native_text(keyboard):
    device, state = keyboard
    device.unicode_input_method = "none"
    state["installed"] = []

    assert device.supports_text_insertion() is False
    assert device.ensure_unicode_input_method_installed() is False
    with pytest.raises(ADBCommandError, match="disabled"):
        device.insert_text("你好", replace=True)
    assert not any(
        call[:2] == ["install", "-r"] or call[:3] in (["shell", "ime", "set"], ["shell", "am", "broadcast"])
        for call in state["calls"]
    )


def test_capability_reflects_new_keyboard_installation(keyboard):
    device, state = keyboard
    state["installed"] = []
    assert device.supports_text_insertion() is True
    assert state["installed"] == [ADB_KEYBOARD_IME]
    assert sum(call[:2] == ["install", "-r"] for call in state["calls"]) == 1


@pytest.mark.parametrize("input_method", ["adbkeyboard", "appium"])
@pytest.mark.parametrize("failure", ["download", "install"])
def test_failed_install_is_optional_for_capability_but_strict_for_input(
    keyboard, monkeypatch, input_method, failure,
):
    device, state = keyboard
    device.unicode_input_method = input_method
    state["installed"] = []
    if failure == "download":
        def fail_download(_package):
            raise ADBCommandError("could not download selected IME")
        monkeypatch.setattr(adb_module, "_download_verified_ime_apk", fail_download)
        error = "could not download"
    else:
        state["fail_install"] = True
        error = "installing .* failed"

    assert device.supports_text_insertion() is False
    with pytest.raises(ADBCommandError, match=error):
        device.ensure_unicode_input_method_installed()
    with pytest.raises(ADBCommandError, match=error):
        device.insert_text("你好", replace=True)

    assert not any(
        call[:3] in (["shell", "ime", "set"], ["shell", "am", "broadcast"])
        for call in state["calls"]
    )


def test_insert_enables_and_selects_installed_adbkeyboard_then_restores(keyboard):
    device, state = keyboard
    state["enabled"] = False

    device.insert_text("你好")

    assert state["enabled"] is True
    assert [call for call in state["calls"] if call[:3] == ["shell", "ime", "enable"]] == [
        ["shell", "ime", "enable", ADB_KEYBOARD_IME]
    ]
    assert [call for call in state["calls"] if call[:3] == ["shell", "ime", "set"]] == [
        ["shell", "ime", "set", ADB_KEYBOARD_IME],
        ["shell", "ime", "set", "example/.LatinIME"],
    ]
    assert state["ime"] == "example/.LatinIME"


def test_failed_enable_does_not_select_or_restore_ime(keyboard):
    device, state = keyboard
    state["enabled"] = False
    state["fail_enable"] = True

    with pytest.raises(ADBCommandError, match="could not enable installed input method"):
        device.insert_text("你好", replace=True)

    assert not any(call[:3] == ["shell", "ime", "set"] for call in state["calls"])
    assert not any(call[:3] == ["shell", "am", "broadcast"] for call in state["calls"])


@pytest.mark.parametrize("text,replace", [("", False), ("\x00", False), ("\ud800", False), ("😸" * 2049, False), ("你好", "false")])
@pytest.mark.parametrize("input_method", ["adbkeyboard", "appium"])
def test_invalid_input_has_no_device_side_effects(keyboard, text, replace, input_method):
    device, state = keyboard
    device.unicode_input_method = input_method
    with pytest.raises(ValueError):
        device.insert_text(text, replace)
    assert state["calls"] == []


def test_missing_keyboard_is_installed_before_input(keyboard):
    device, state = keyboard
    state["installed"] = []
    device.insert_text("你好", True)
    assert state["installed"] == [ADB_KEYBOARD_IME]
    assert sum(call[:2] == ["install", "-r"] for call in state["calls"]) == 1
    assert any(call[:3] == ["shell", "am", "broadcast"] for call in state["calls"])


def test_failed_install_does_not_select_or_send_text(keyboard):
    device, state = keyboard
    state["installed"] = []
    state["fail_install"] = True

    with pytest.raises(ADBCommandError, match="installing ADBKeyBoard failed"):
        device.insert_text("你好", replace=True)

    assert not any(call[:3] == ["shell", "ime", "set"] for call in state["calls"])
    assert not any(call[:3] == ["shell", "am", "broadcast"] for call in state["calls"])


def test_ime_download_checks_pinned_sha256(monkeypatch):
    apk_data = b"verified test apk"
    package = UnicodeIMEPackage(
        name="test IME",
        ime_id="example/.TestIME",
        url="https://example.test/ime.apk",
        sha256=hashlib.sha256(apk_data).hexdigest(),
    )
    monkeypatch.setattr(adb_module.urllib.request, "urlopen", lambda _request, timeout: io.BytesIO(apk_data))

    assert _download_verified_ime_apk(package) == apk_data


def test_ime_download_rejects_unexpected_sha256(monkeypatch):
    package = UnicodeIMEPackage(
        name="test IME",
        ime_id="example/.TestIME",
        url="https://example.test/ime.apk",
        sha256="0" * 64,
    )
    monkeypatch.setattr(adb_module.urllib.request, "urlopen", lambda _request, timeout: io.BytesIO(b"tampered"))

    with pytest.raises(ADBCommandError, match="SHA-256 verification"):
        _download_verified_ime_apk(package)


@pytest.mark.parametrize("json_output", [False, True])
def test_start_bridge_defers_ime_provisioning(monkeypatch, capsys, json_output):
    calls = []

    class FakeDevice:
        def __init__(self, **kwargs):
            calls.append(("device", kwargs["unicode_input_method"]))

        def check_device(self):
            calls.append("device-ready")

        def ensure_unicode_input_method_installed(self):
            pytest.fail("IME provisioning must not block bridge startup")

    class FakeServer:
        def __init__(self, device, **kwargs):
            calls.append("server-created")

        def start(self):
            calls.append("server-started")
            return "http://127.0.0.1:12345"

        def stop(self):
            calls.append("server-stopped")

    class FakeEvent:
        def wait(self):
            calls.append("waited")

    class FakeThreading:
        Event = FakeEvent

    monkeypatch.setattr(start_bridge, "ADBAndroidDevice", FakeDevice)
    monkeypatch.setattr(start_bridge, "ADBBridgeServer", FakeServer)
    monkeypatch.setattr(start_bridge, "threading", FakeThreading)
    monkeypatch.setattr(start_bridge.signal, "signal", lambda *_args: None)

    args = ["--adb-serial", "serial-1", "--unicode-input-method", "appium"]
    if json_output:
        args.append("--json")
    result = start_bridge.main(args)
    captured = capsys.readouterr()

    assert result == 0
    assert calls.index("device-ready") < calls.index("server-started")
    assert ("device", "appium") in calls
    if json_output:
        startup = json.loads(captured.out)
        assert startup["environment_url"] == "http://127.0.0.1:12345"
        assert startup["unicode_input_method"] == "appium"
    else:
        assert "ADB Android environment started" in captured.out


def test_failed_broadcast_restores_ime_without_replay_or_payload_leak(keyboard):
    device, state = keyboard
    state["fail_broadcast"] = True
    with pytest.raises(ADBCommandError, match="inspect the field") as exc:
        device.insert_text("你好")
    assert base64.b64encode("你好".encode()).decode() not in str(exc.value)
    assert state["ime"] == "example/.LatinIME"
    assert sum(args[:3] == ["shell", "am", "broadcast"] for args in state["calls"]) == 1


def test_restore_failure_is_reported(keyboard):
    device, state = keyboard
    state["fail_restore"] = True
    with pytest.raises(ADBCommandError, match="restore the original"):
        device.insert_text("你好")
    assert device._pending_input_method_restore == "example/.LatinIME"

    state["fail_restore"] = False
    device.insert_text("再次输入")
    assert device._pending_input_method_restore is None
    assert state["ime"] == "example/.LatinIME"


def test_restore_failure_does_not_hide_input_failure(keyboard):
    device, state = keyboard
    state["fail_broadcast"] = True
    state["fail_restore"] = True

    with pytest.raises(ADBCommandError, match="ADB_INPUT_B64 failed or timed out.*restore the original"):
        device.insert_text("你好")
    assert device._pending_input_method_restore == "example/.LatinIME"


def test_ascii_input_reports_restore_failure(keyboard, monkeypatch):
    device, state = keyboard
    state["fail_restore"] = True
    monkeypatch.setattr(device, "_switch_to_ascii_input_method", lambda: "example/.LatinIME")
    monkeypatch.setattr(device, "_run", lambda args, **kwargs: b"")

    with pytest.raises(ADBCommandError, match="restore the original"):
        device.input_text("hello")
    assert device._pending_input_method_restore == "example/.LatinIME"


def test_failed_selection_does_not_clear_field(keyboard):
    device, state = keyboard
    state["ignore_switch"] = True
    with pytest.raises(ADBCommandError, match="selection failed"):
        device.insert_text("你好", True)
    assert not any(args[:3] == ["shell", "am", "broadcast"] for args in state["calls"])


def test_waits_for_active_editor_before_delivery(keyboard, monkeypatch):
    device, state = keyboard
    run = device._run_text
    probes = []
    def delayed(args, **kwargs):
        if args == ["shell", "dumpsys", "input_method"]:
            probes.append(args)
            if len(probes) == 1:
                return "Input method service state for com.android.adbkeyboard/.AdbIME:\n  mInputStarted=false mStartedInputConnection=null"
        return run(args, **kwargs)
    monkeypatch.setattr(device, "_run_text", delayed)
    device.insert_text("你好")
    assert len(probes) == 2


def test_unfocused_editor_is_not_cleared(keyboard, monkeypatch):
    device, state = keyboard
    run = device._run_text
    monkeypatch.setattr(device, "_run_text", lambda args, **kw: "mInputStarted=false" if args == ["shell", "dumpsys", "input_method"] else run(args, **kw))
    times = iter([0, 4])
    monkeypatch.setattr("adbandroid.bridge.adb.time.monotonic", lambda: next(times))
    with pytest.raises(ADBCommandError, match="focus an input field"):
        device.insert_text("你好", True)
    assert state["ime"] == "example/.LatinIME"
    assert not any(args[:3] == ["shell", "am", "broadcast"] for args in state["calls"])


def _appium_device(monkeypatch, *, installed, enabled, current="example/.LatinIME"):
    device = ADBAndroidDevice("serial-1", unicode_input_method="appium")
    state = {"current": current, "enabled": list(enabled), "calls": []}
    monkeypatch.setattr(adb_module, "_download_verified_ime_apk", lambda _package: b"test-apk")

    def run_text(args, *, timeout=None):
        state["calls"].append(("text", args))
        if args[:2] == ["install", "-r"]:
            installed.append(APPIUM_UNICODE_IME)
            return "Success"
        if args == ["shell", "ime", "list", "-a", "-s"]:
            return "\n".join(installed) + "\n"
        if args == ["shell", "ime", "list", "-s"]:
            return "\n".join(state["enabled"]) + "\n"
        if args == ["shell", "settings", "get", "secure", "default_input_method"]:
            return state["current"] + "\n"
        if args[:3] == ["shell", "ime", "enable"]:
            state["enabled"].append(args[3])
            return f"Input method {args[3]} successfully enabled"
        if args[:3] == ["shell", "ime", "set"]:
            if state.get("fail_restore") and args[3] == "example/.LatinIME":
                raise ADBCommandError("restore failed")
            if not state.get("ignore_switch"):
                state["current"] = args[3]
            return f"Input method {args[3]} selected"
        if args == ["shell", "input"]:
            return "Usage: input [<source>] <command> ... keycombination ..."
        if args[:3] == ["shell", "input", "keycombination"]:
            state["calls"].append(("run", args))
            return ""
        raise AssertionError(args)

    def run(args, *, timeout=None, binary=False):
        state["calls"].append(("run", args))
        return b""

    monkeypatch.setattr(device, "_run_text", run_text)
    monkeypatch.setattr(device, "_run", run)
    monkeypatch.setattr("adbandroid.bridge.adb.time.sleep", lambda _seconds: None)
    return device, state


def test_modified_utf7_encoding_matches_appium_unicode_ime():
    assert encode_modified_utf7("hello aico") == "hello aico"
    assert encode_modified_utf7("小红书") == "&XA9+ok5m-"
    assert encode_modified_utf7("Hi 你好 & 😀") == "Hi &T2BZfQ- &- &2D3eAA-"


def test_appium_input_enables_unicode_ime_sends_utf7_and_restores(monkeypatch):
    device, state = _appium_device(
        monkeypatch,
        installed=["example/.LatinIME", "io.appium.settings/.AppiumIME", APPIUM_UNICODE_IME],
        enabled=["example/.LatinIME"],
    )

    assert device.supports_text_insertion() is True
    device.insert_text("小红书 ok")

    assert ("text", ["shell", "ime", "enable", APPIUM_UNICODE_IME]) in state["calls"]
    assert ("run", ["shell", "input", "text", "'&XA9+ok5m-%sok'"]) in state["calls"]
    sets = [args[3] for kind, args in state["calls"] if kind == "text" and args[:3] == ["shell", "ime", "set"]]
    assert sets == [APPIUM_UNICODE_IME, "example/.LatinIME"]
    assert state["current"] == "example/.LatinIME"


def test_appium_input_replace_clears_field_before_typing(monkeypatch):
    device, state = _appium_device(
        monkeypatch,
        installed=[APPIUM_UNICODE_IME],
        enabled=[APPIUM_UNICODE_IME, "example/.LatinIME"],
    )

    device.insert_text("你好", replace=True)

    sent = [args for kind, args in state["calls"] if kind == "run"]
    assert sent == [
        ["shell", "input", "keycombination", "KEYCODE_CTRL_LEFT", "KEYCODE_A"],
        ["shell", "input", "keyevent", "KEYCODE_DEL"],
        ["shell", "input", "text", "'&T2BZfQ-'"],
    ]


@pytest.mark.parametrize("installed", [["example/.LatinIME"], [ADB_KEYBOARD_IME]])
def test_appium_capability_installs_selected_ime_without_falling_back(installed, monkeypatch):
    device, state = _appium_device(monkeypatch, installed=installed, enabled=["example/.LatinIME"])

    assert device.supports_text_insertion() is True
    assert APPIUM_UNICODE_IME in installed
    assert sum(args[:2] == ["install", "-r"] for kind, args in state["calls"] if kind == "text") == 1


def test_appium_rejects_literal_percent_s_before_device_commands(monkeypatch):
    device, state = _appium_device(monkeypatch, installed=[APPIUM_UNICODE_IME], enabled=[])

    with pytest.raises(ValueError, match="%s"):
        device.insert_text("50%s off")
    assert state["calls"] == []


@pytest.mark.parametrize("failure", ["fail_restore", "ignore_switch"])
def test_appium_input_reports_ime_failure(monkeypatch, failure):
    device, state = _appium_device(
        monkeypatch,
        installed=[APPIUM_UNICODE_IME],
        enabled=[APPIUM_UNICODE_IME, "example/.LatinIME"],
    )
    state[failure] = True

    with pytest.raises(ADBCommandError, match="restore the original|selection failed"):
        device.insert_text("你好", replace=True)
    if failure == "ignore_switch":
        assert not any(kind == "run" for kind, _ in state["calls"])


def test_appium_failure_restores_keyboard_without_replay_or_payload_leak(monkeypatch):
    device, state = _appium_device(monkeypatch, installed=[APPIUM_UNICODE_IME], enabled=[APPIUM_UNICODE_IME])
    sent = []

    def fail(args, **kwargs):
        sent.append(args)
        raise ADBCommandError("private payload: " + " ".join(args))

    monkeypatch.setattr(device, "_run", fail)
    with pytest.raises(ADBCommandError, match="inspect the field") as exc:
        device.insert_text("你好")
    assert encode_modified_utf7("你好") not in str(exc.value)
    assert state["current"] == "example/.LatinIME"
    assert len(sent) == 1


def test_selected_method_installs_without_falling_back_to_other_ime(keyboard, monkeypatch):
    device, state = keyboard
    state["installed"] = [APPIUM_UNICODE_IME]

    assert device.supports_text_insertion() is True
    assert state["installed"] == [APPIUM_UNICODE_IME, ADB_KEYBOARD_IME]

    device, _ = _appium_device(monkeypatch, installed=[ADB_KEYBOARD_IME], enabled=[ADB_KEYBOARD_IME])
    assert device.supports_text_insertion() is True
