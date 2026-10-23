from adbandroid.bridge.adb import ADBAndroidDevice, WINDOW_XML_REMOTE_PATH


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

    device.input_text("hello aico")

    assert calls == [
        ("text", ["shell", "settings", "get", "secure", "default_input_method"], 5),
        ("text", ["shell", "ime", "list", "-s"], 5),
        ("text", ["shell", "ime", "set", "org.pocketworkstation.pckeyboard/.LatinIME"], 5),
        ("run", ["shell", "input", "text", "'hello%saico'"], None, False),
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
