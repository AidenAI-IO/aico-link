import base64

import pytest

from adbandroid.bridge.adb import (
    ADBAndroidDevice,
    ADBCommandError,
    ADB_KEYBOARD_IME,
    APPIUM_UNICODE_IME,
    encode_modified_utf7,
)


@pytest.fixture
def keyboard(monkeypatch):
    device = ADBAndroidDevice("myron")
    state = {"ime": "example/.LatinIME", "enabled": True, "calls": []}

    def run(args, **kwargs):
        state["calls"].append(args)
        if args == ["shell", "ime", "list", "-s"]:
            return ADB_KEYBOARD_IME if state["enabled"] else "example/.LatinIME"
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


def test_capability_reflects_new_keyboard_installation(keyboard):
    device, state = keyboard
    state["enabled"] = False
    assert device.supports_text_insertion() is False
    state["enabled"] = True
    assert device.supports_text_insertion() is True


@pytest.mark.parametrize("text,replace", [("", False), ("\x00", False), ("\ud800", False), ("😸" * 2049, False), ("你好", "false")])
@pytest.mark.parametrize("input_method", ["adbkeyboard", "appium"])
def test_invalid_input_has_no_device_side_effects(keyboard, text, replace, input_method):
    device, state = keyboard
    device.unicode_input_method = input_method
    with pytest.raises(ValueError):
        device.insert_text(text, replace)
    assert state["calls"] == []


def test_missing_keyboard_does_not_select_or_clear(keyboard):
    device, state = keyboard
    state["enabled"] = False
    with pytest.raises(ADBCommandError, match="Install ADBKeyBoard"):
        device.insert_text("你好", True)
    assert state["calls"] == [["shell", "ime", "list", "-s"]]


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

    def run_text(args, *, timeout=None):
        state["calls"].append(("text", args))
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


@pytest.mark.parametrize("installed", [["example/.LatinIME"], ["io.appium.settings/.AppiumIME"]])
def test_appium_capability_requires_unicode_ime_and_does_not_send(installed, monkeypatch):
    device, state = _appium_device(monkeypatch, installed=installed, enabled=["example/.LatinIME"])

    assert device.supports_text_insertion() is False
    with pytest.raises(ADBCommandError, match="Appium Settings"):
        device.insert_text("你好")
    assert not any(kind == "run" for kind, _ in state["calls"])


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


def test_selected_method_does_not_fall_back_to_other_installed_keyboard(monkeypatch):
    device = ADBAndroidDevice("serial-1")
    installed = [APPIUM_UNICODE_IME]
    monkeypatch.setattr(device, "list_input_methods", lambda: installed)
    assert device.supports_text_insertion() is False
    with pytest.raises(ADBCommandError, match="Install ADBKeyBoard"):
        device.insert_text("你好")

    device, _ = _appium_device(monkeypatch, installed=[ADB_KEYBOARD_IME], enabled=[ADB_KEYBOARD_IME])
    assert device.supports_text_insertion() is False
    with pytest.raises(ADBCommandError, match="Appium Settings"):
        device.insert_text("你好")
