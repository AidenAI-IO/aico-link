import base64

import pytest

from adbandroid.bridge.adb import ADBAndroidDevice, ADBCommandError, ADB_KEYBOARD_IME


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
def test_invalid_input_has_no_device_side_effects(keyboard, text, replace):
    device, state = keyboard
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
