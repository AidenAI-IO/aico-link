import pytest

from adbandroid.bridge.adb import ADBAndroidDevice, ADBCommandError, WINDOW_XML_REMOTE_PATH


def test_text_editing_chords_use_native_keycombination(monkeypatch):
    device = ADBAndroidDevice("serial-1")
    calls = []

    def run_text(args, **kwargs):
        calls.append(args)
        return "keycombination [-t duration] <key code 1> <key code 2> ..." if args == ["shell", "input"] else ""

    monkeypatch.setattr(device, "_run_text", run_text)
    device.keycombination(["KEYCODE_CTRL_LEFT", "KEYCODE_A"])
    device.keycombination(["KEYCODE_CTRL_LEFT", "KEYCODE_Z"])
    assert calls == [
        ["shell", "input"],
        ["shell", "input", "keycombination", "KEYCODE_CTRL_LEFT", "KEYCODE_A"],
        ["shell", "input", "keycombination", "KEYCODE_CTRL_LEFT", "KEYCODE_Z"],
    ]


def test_old_android_does_not_silently_execute_plain_keys(monkeypatch):
    device = ADBAndroidDevice("serial-1")
    calls = []

    def run_text(args, **kwargs):
        calls.append(args)
        return "Available commands: text keyevent tap swipe"

    monkeypatch.setattr(device, "_run_text", run_text)
    with pytest.raises(ADBCommandError, match="keycombination"):
        device.keycombination(["KEYCODE_CTRL_LEFT", "KEYCODE_A"])
    assert calls == [["shell", "input"]]


@pytest.mark.parametrize("output", ["Error: Unknown command: keycombination", "Error: Invalid arguments for command: keycombination"])
def test_keycombination_checks_errors_even_with_zero_exit_status(monkeypatch, output):
    device = ADBAndroidDevice("serial-1")
    monkeypatch.setattr(device, "_run_text", lambda args, **kw: "keycombination" if args == ["shell", "input"] else output)
    with pytest.raises(ADBCommandError):
        device.keycombination(["KEYCODE_CTRL_LEFT", "KEYCODE_A"])


@pytest.mark.parametrize("field", ["mInputShown", "mIsInputViewShown"])
def test_reset_home_dismisses_and_checks_keyboard(monkeypatch, field):
    device = ADBAndroidDevice("serial-1")
    calls = []
    states = iter([f"{field}=true", f"{field}=false"])

    def run_text(args, **kwargs):
        calls.append(args)
        assert args == ["shell", "dumpsys", "input_method"]
        return next(states)

    monkeypatch.setattr(device, "_run_text", run_text)
    monkeypatch.setattr(device, "_run", lambda args, **kw: calls.append(args))
    monkeypatch.setattr("adbandroid.bridge.adb.time.sleep", lambda _: None)
    device.reset_home()
    assert calls == [
        ["shell", "cmd", "statusbar", "collapse"],
        ["shell", "input", "keyevent", "KEYCODE_HOME"],
        ["shell", "dumpsys", "input_method"],
        ["shell", "input", "keyevent", "KEYCODE_BACK"],
        ["shell", "input", "keyevent", "KEYCODE_HOME"],
        ["shell", "dumpsys", "input_method"],
    ]


@pytest.mark.parametrize("state", ["mInputShown=true", "unrecognized input method dump"])
def test_reset_home_rejects_stuck_or_unverifiable_keyboard(monkeypatch, state):
    device = ADBAndroidDevice("serial-1")
    monkeypatch.setattr(device, "_run_text", lambda *args, **kw: state)
    monkeypatch.setattr(device, "_run", lambda *args, **kw: b"")
    monkeypatch.setattr("adbandroid.bridge.adb.time.sleep", lambda _: None)
    with pytest.raises(ADBCommandError, match="keyboard"):
        device.reset_home()


def test_dump_window_xml_reads_dumped_temp_file_and_cleans_up(monkeypatch):
    device = ADBAndroidDevice("serial-1")
    calls = []

    def fake_run_text(args, *, timeout=None):
        calls.append(("text", args, timeout))
        if args == ["shell", "uiautomator", "dump", WINDOW_XML_REMOTE_PATH]:
            return f"UI hierchary dumped to: {WINDOW_XML_REMOTE_PATH}"
        if args == ["shell", "cat", WINDOW_XML_REMOTE_PATH]:
            return "<?xml version='1.0'?><hierarchy />"
        raise AssertionError(f"unexpected _run_text args: {args!r}")

    def fake_run(args, *, timeout=None, binary=False):
        calls.append(("run", args, timeout, binary))
        assert args == ["shell", "rm", "-f", WINDOW_XML_REMOTE_PATH]
        return b""

    monkeypatch.setattr(device, "_run_text", fake_run_text)
    monkeypatch.setattr(device, "_run", fake_run)

    assert device.dump_window_xml() == "<?xml version='1.0'?><hierarchy />"
    assert calls == [
        ("text", ["shell", "uiautomator", "dump", WINDOW_XML_REMOTE_PATH], 5),
        ("text", ["shell", "cat", WINDOW_XML_REMOTE_PATH], 5),
        ("run", ["shell", "rm", "-f", WINDOW_XML_REMOTE_PATH], 2, False),
    ]


def test_input_text_switches_to_preferred_ascii_ime_and_restores(monkeypatch):
    device = ADBAndroidDevice("serial-1")
    calls = []

    def fake_run_text(args, *, timeout=None):
        calls.append(("text", args, timeout))
        if args == ["shell", "settings", "get", "secure", "default_input_method"]:
            return "com.google.android.inputmethod.latin/com.android.inputmethod.latin.LatinIME\n"
        if args == ["shell", "ime", "list", "-s"]:
            return (
                "org.pocketworkstation.pckeyboard/.LatinIME\n"
                "com.google.android.inputmethod.latin/com.android.inputmethod.latin.LatinIME\n"
            )
        if args[0:3] == ["shell", "ime", "set"]:
            return f"Input method {args[3]} selected"
        raise AssertionError(f"unexpected _run_text args: {args!r}")

    def fake_run(args, *, timeout=None, binary=False):
        calls.append(("run", args, timeout, binary))
        return b""

    monkeypatch.setattr(device, "_run_text", fake_run_text)
    monkeypatch.setattr(device, "_run", fake_run)
    monkeypatch.setattr("adbandroid.bridge.adb.time.sleep", lambda _seconds: None)

    device.input_text("hello aiden")

    assert calls == [
        ("text", ["shell", "settings", "get", "secure", "default_input_method"], 5),
        ("text", ["shell", "ime", "list", "-s"], 5),
        ("text", ["shell", "ime", "set", "org.pocketworkstation.pckeyboard/.LatinIME"], 5),
        ("run", ["shell", "input", "text", "'hello%saiden'"], None, False),
        (
            "text",
            [
                "shell",
                "ime",
                "set",
                "com.google.android.inputmethod.latin/com.android.inputmethod.latin.LatinIME",
            ],
            5,
        ),
    ]
