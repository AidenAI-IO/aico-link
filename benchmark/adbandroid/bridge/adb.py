"""Device layer for the ADB Android environment bridge.

Wraps all adb interactions with a target Android device (e.g. a Genymotion
emulator reachable at 127.0.0.1:6555). This module deliberately knows nothing
about HTTP or the bridge protocol: it returns plain values and raises
ADBCommandError / ValueError on failure. The bridge layers (tools_api/server)
translate those into protocol responses.
"""

from __future__ import annotations

import base64
import hashlib
import io
import re
import subprocess
import tempfile
import threading
import time
import urllib.request
from collections.abc import Callable
from dataclasses import dataclass
from pathlib import Path
from typing import Any

from mnk_provider import validate_insert_text


DEFAULT_SCREENSHOT_MAX_WIDTH = 720
DEFAULT_JPEG_QUALITY = 75
WINDOW_XML_REMOTE_PATH = "/sdcard/aico-window.xml"
PREFERRED_ASCII_INPUT_METHODS = (
    "org.pocketworkstation.pckeyboard/.LatinIME",
    "com.android.inputmethod.latin/.LatinIME",
)
DEFAULT_TEXT_RESTORE_WAIT_SEC = 0.08
ADB_KEYBOARD_IME = "com.android.adbkeyboard/.AdbIME"
ADB_KEYBOARD_PACKAGE = "com.android.adbkeyboard"
ADB_KEYBOARD_INPUT_METHOD = "adbkeyboard"
APPIUM_INPUT_METHOD = "appium"
# "none" keeps the bridge to ASCII adb input and never advertises insert_text,
# so Unicode reaches the field the way it does on the board: Phone Bridge
# paste or the Agent's local keyboard path.
NO_UNICODE_INPUT_METHOD = "none"
DEFAULT_UNICODE_INPUT_METHOD = ADB_KEYBOARD_INPUT_METHOD
SUPPORTED_UNICODE_INPUT_METHODS = (ADB_KEYBOARD_INPUT_METHOD, APPIUM_INPUT_METHOD, NO_UNICODE_INPUT_METHOD)
APPIUM_UNICODE_IME = "io.appium.settings/.UnicodeIME"
# Keep the APK sources and digests pinned so automatic installation only
# accepts the selected helper from its upstream release.
MAX_UNICODE_IME_APK_BYTES = 16 * 1024 * 1024
UNICODE_IME_DOWNLOAD_TIMEOUT_SEC = 30
UNICODE_IME_INSTALL_TIMEOUT_SEC = 120


@dataclass(frozen=True)
class UnicodeIMEPackage:
    name: str
    ime_id: str
    url: str
    sha256: str


UNICODE_IME_PACKAGES = {
    ADB_KEYBOARD_INPUT_METHOD: UnicodeIMEPackage(
        name="ADBKeyBoard",
        ime_id=ADB_KEYBOARD_IME,
        url="https://github.com/senzhk/ADBKeyBoard/releases/download/v2.5-dev/keyboardservice-debug.apk",
        sha256="41a8a0996d7397a2390d1ca16a75cb66c4a7bdaa89cf4e63600a4d3fb346fbbb",
    ),
    APPIUM_INPUT_METHOD: UnicodeIMEPackage(
        name="Appium Settings",
        ime_id=APPIUM_UNICODE_IME,
        url="https://github.com/appium/io.appium.settings/releases/download/v8.2.1/settings_apk-debug.apk",
        sha256="e4024b3ee348929252bcaf5eb422bcab9e4ace96703499a84f3332281c03375f",
    ),
}

# Appium Settings 3.4.0's AppiumIME does not decode modified UTF-7.
# UnicodeIME needs time to bind after selection and to commit before restore.
APPIUM_IME_BIND_WAIT_SEC = 1.0
APPIUM_TEXT_COMMIT_WAIT_SEC = 0.3

# Characters adb `input text` can type reliably on a US keyboard layout.
# Deliberately excludes newline/tab (input text cannot type them) and any
# non-ASCII text (handled separately by insert_text through the selected IME).
ADB_TEXT_CHARS = set(
    "abcdefghijklmnopqrstuvwxyz"
    "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
    "0123456789"
    " "
    "-=[]\\;'`,./"
    "!@#$%^&*()_+{}|:\"~<>?"
)

KEYCODE_HOME = 3
KEYCODE_BACK = 4
KEYCODE_TAB = 61
KEYCODE_SPACE = 62
KEYCODE_ENTER = 66
KEYCODE_DEL = 67
KEYCODE_APP_SWITCH = 187


class ADBCommandError(RuntimeError):
    """An adb command failed or timed out."""


def _download_verified_ime_apk(package: UnicodeIMEPackage) -> bytes:
    request = urllib.request.Request(
        package.url,
        headers={"User-Agent": "aico-link-adb-bridge"},
    )
    try:
        with urllib.request.urlopen(request, timeout=UNICODE_IME_DOWNLOAD_TIMEOUT_SEC) as response:
            apk_data = response.read(MAX_UNICODE_IME_APK_BYTES + 1)
    except OSError as exc:
        raise ADBCommandError(f"could not download {package.name}: {exc}") from exc

    if not apk_data or len(apk_data) > MAX_UNICODE_IME_APK_BYTES:
        raise ADBCommandError(f"downloaded {package.name} APK has an invalid size")
    digest = hashlib.sha256(apk_data).hexdigest()
    if digest != package.sha256:
        raise ADBCommandError(f"downloaded {package.name} APK failed SHA-256 verification")
    return apk_data


class ADBAndroidDevice:
    """Thin wrapper over the adb CLI for a single device serial."""

    def __init__(
        self,
        serial: str,
        adb_path: str = "adb",
        timeout_sec: float = 10,
        screenshot_max_width: int = DEFAULT_SCREENSHOT_MAX_WIDTH,
        jpeg_quality: int = DEFAULT_JPEG_QUALITY,
        unicode_input_method: str = DEFAULT_UNICODE_INPUT_METHOD,
    ):
        self.serial = str(serial).strip()
        if not self.serial:
            raise ValueError("adb serial is required")
        if unicode_input_method not in SUPPORTED_UNICODE_INPUT_METHODS:
            choices = ", ".join(SUPPORTED_UNICODE_INPUT_METHODS)
            raise ValueError(f"unicode_input_method must be one of: {choices}")
        self.adb_path = adb_path
        self.timeout_sec = timeout_sec
        self.screenshot_max_width = max(1, int(screenshot_max_width))
        self.jpeg_quality = min(95, max(1, int(jpeg_quality)))
        self.unicode_input_method = unicode_input_method
        self._screen_size: tuple[int, int] | None = None
        self._supports_keycombination: bool | None = None
        self._preferred_ime: str | None = None
        self._unicode_ime_install_lock = threading.Lock()
        self._pending_input_method_restore: str | None = None

    # ---- low-level -------------------------------------------------------

    def _run(self, args: list[str], *, timeout: float | None = None, binary: bool = False) -> bytes:
        command = [self.adb_path, "-s", self.serial, *args]
        try:
            result = subprocess.run(
                command,
                capture_output=True,
                timeout=timeout if timeout is not None else self.timeout_sec,
                check=False,
            )
        except FileNotFoundError as exc:
            raise ADBCommandError(f"adb binary not found: {self.adb_path!r}") from exc
        except subprocess.TimeoutExpired as exc:
            raise ADBCommandError(f"adb command timed out: {' '.join(args)}") from exc
        if result.returncode != 0:
            stderr = result.stderr.decode("utf-8", errors="replace").strip()
            stdout = result.stdout.decode("utf-8", errors="replace").strip()
            detail = stderr or stdout or f"exit code {result.returncode}"
            raise ADBCommandError(f"adb {' '.join(args)} failed: {detail}")
        return result.stdout if binary else result.stdout

    def _run_text(self, args: list[str], *, timeout: float | None = None) -> str:
        return self._run(args, timeout=timeout).decode("utf-8", errors="replace")

    # ---- queries ---------------------------------------------------------

    def check_device(self) -> dict[str, Any]:
        """Return device state; raises ADBCommandError when unreachable."""
        state = self._run_text(["get-state"]).strip()
        if state != "device":
            raise ADBCommandError(f"adb device {self.serial} is not ready: state={state!r}")
        return {"serial": self.serial, "state": state}

    def screen_size(self) -> tuple[int, int]:
        """Return the device screen size in pixels (Override wins over Physical)."""
        if self._screen_size is not None:
            return self._screen_size
        output = self._run_text(["shell", "wm", "size"])
        override: tuple[int, int] | None = None
        physical: tuple[int, int] | None = None
        for line in output.splitlines():
            match = re.search(r"(Override|Physical) size:\s*(\d+)x(\d+)", line)
            if not match:
                continue
            size = (int(match.group(2)), int(match.group(3)))
            if match.group(1) == "Override":
                override = size
            else:
                physical = size
        size = override or physical
        if size is None or size[0] <= 0 or size[1] <= 0:
            raise ADBCommandError(f"could not parse `wm size` output: {output!r}")
        self._screen_size = size
        return size

    def screenshot_jpeg(self) -> tuple[bytes, int, int]:
        """Capture the screen, downscale, and return (jpeg_bytes, width, height).

        Width/height are the scaled JPEG dimensions. Coordinate conversion must
        use screen_size() (raw pixels), never these values.
        """
        try:
            from PIL import Image
        except ImportError as exc:  # pragma: no cover - Pillow is a hard dependency
            raise ADBCommandError("Pillow is required for screenshots") from exc

        png = self._run(["exec-out", "screencap", "-p"], binary=True, timeout=max(self.timeout_sec, 30))
        if not png.startswith(b"\x89PNG\r\n\x1a\n"):
            raise ADBCommandError("screencap did not return PNG data")
        image = Image.open(io.BytesIO(png)).convert("RGB")
        if image.width > self.screenshot_max_width:
            new_height = max(1, round(image.height * self.screenshot_max_width / image.width))
            image = image.resize((self.screenshot_max_width, new_height))
        buffer = io.BytesIO()
        image.save(buffer, format="JPEG", quality=self.jpeg_quality)
        return buffer.getvalue(), image.width, image.height

    # ---- actions ---------------------------------------------------------

    def tap(self, x: int, y: int) -> None:
        self._run(["shell", "input", "tap", str(int(x)), str(int(y))])

    def swipe(self, x1: int, y1: int, x2: int, y2: int, duration_ms: int) -> None:
        self._run(
            [
                "shell",
                "input",
                "swipe",
                str(int(x1)),
                str(int(y1)),
                str(int(x2)),
                str(int(y2)),
                str(max(1, int(duration_ms))),
            ],
            timeout=max(self.timeout_sec, duration_ms / 1000 + 5),
        )

    def keyevent(self, keycode: str | int) -> None:
        self._run(["shell", "input", "keyevent", str(keycode)])

    def keycombination(self, keycodes: list[str]) -> None:
        """Send simultaneous keys only when the device advertises native support."""
        if self._supports_keycombination is None:
            help_text = self._run_text(["shell", "input"])
            self._supports_keycombination = bool(re.search(r"\bkeycombination\b", help_text))
        if not self._supports_keycombination:
            raise ADBCommandError("device does not support input keycombination; cannot execute text-editing shortcuts")
        output = self._run_text(["shell", "input", "keycombination", *keycodes])
        # Some Android input implementations print an error but exit zero.
        if "error" in output.lower() or "unknown command" in output.lower():
            raise ADBCommandError(f"input keycombination failed: {output.strip()}")

    def input_text(self, text: str) -> None:
        """Type ASCII text via `input text`.

        Known limitation: `input text` interprets `%s` as a space, so a literal
        "%s" in the text is typed as a space. Non-ASCII text is rejected.
        """
        unsupported = unsupported_adb_text_chars(text)
        if unsupported:
            raise ValueError(
                f"adb input text supports only US-keyboard ASCII characters; unsupported: {unsupported!r}"
            )
        if text == "":
            raise ValueError("text is required")
        self._recover_pending_input_method_restore()
        restore_ime = self._switch_to_ascii_input_method()

        def send_text() -> None:
            self._run(["shell", "input", "text", escape_adb_text(text)])

        if restore_ime:
            self._run_with_input_method_restore(restore_ime, send_text)
        else:
            send_text()

    def supports_text_insertion(self) -> bool:
        """Ensure the selected Unicode IME is installed before advertising it."""
        if self.unicode_input_method == NO_UNICODE_INPUT_METHOD:
            return False
        try:
            self.ensure_unicode_input_method_installed()
        except ADBCommandError:
            return False
        return True

    def ensure_unicode_input_method_installed(self) -> bool:
        """Install the selected pinned APK if its IME component is missing."""
        if self.unicode_input_method == NO_UNICODE_INPUT_METHOD:
            return False
        package = UNICODE_IME_PACKAGES[self.unicode_input_method]
        if package.ime_id in self._installed_input_methods():
            return False

        with self._unicode_ime_install_lock:
            if package.ime_id in self._installed_input_methods():
                return False
            apk_data = _download_verified_ime_apk(package)
            with tempfile.TemporaryDirectory(prefix="aico-unicode-ime-") as directory:
                apk_path = Path(directory) / "input-method.apk"
                apk_path.write_bytes(apk_data)
                output = self._run_text(
                    ["install", "-r", str(apk_path)],
                    timeout=UNICODE_IME_INSTALL_TIMEOUT_SEC,
                )
            if "failure" in output.lower() or not re.search(r"\bsuccess\b", output, re.I):
                raise ADBCommandError(f"installing {package.name} failed: {output.strip()}")
            if package.ime_id not in self._installed_input_methods():
                raise ADBCommandError(f"{package.name} installed but {package.ime_id} was not registered")
            return True

    def _installed_input_methods(self) -> set[str]:
        output = self._run_text(["shell", "ime", "list", "-a", "-s"], timeout=5)
        return {line.strip() for line in output.splitlines() if line.strip()}

    def insert_text(self, text: str, replace: bool = False) -> None:
        """Commit Unicode once through the configured IME and restore the user's IME.

        Delivery success is not proof that an editor accepted the text; callers
        must verify the field and must not replay an ambiguous failure.
        """
        validate_insert_text(text)
        if not isinstance(replace, bool):
            raise ValueError("replace must be a boolean")
        self._recover_pending_input_method_restore()
        if self.unicode_input_method == NO_UNICODE_INPUT_METHOD:
            raise ADBCommandError("native text insertion is disabled for this bridge; no text was sent")
        if self.unicode_input_method == ADB_KEYBOARD_INPUT_METHOD:
            self._insert_text_with_adbkeyboard(text, replace)
        else:
            self._insert_text_with_appium(text, replace)

    def _insert_text_with_adbkeyboard(self, text: str, replace: bool) -> None:
        self.ensure_unicode_input_method_installed()
        original_ime = self.current_input_method()
        if not original_ime or original_ime.lower() == "null":
            raise ADBCommandError("cannot determine the input method to restore; no text was sent")
        self._ensure_input_method_enabled(ADB_KEYBOARD_IME)

        def send_text() -> None:
            if original_ime != ADB_KEYBOARD_IME:
                self.set_input_method(ADB_KEYBOARD_IME)
            if self.current_input_method() != ADB_KEYBOARD_IME:
                raise ADBCommandError("ADBKeyBoard selection failed; no text was sent")
            self._wait_for_adb_keyboard()
            if replace:
                self._keyboard_broadcast("ADB_CLEAR_TEXT")
            encoded = base64.b64encode(text.encode("utf-8")).decode("ascii")
            self._keyboard_broadcast("ADB_INPUT_B64", ["--es", "msg", encoded])

        if original_ime != ADB_KEYBOARD_IME:
            self._run_with_input_method_restore(original_ime, send_text)
        else:
            send_text()

    def _insert_text_with_appium(self, text: str, replace: bool) -> None:
        if "%s" in text:
            raise ValueError('text must not contain "%s": adb input text types it as a space')
        self.ensure_unicode_input_method_installed()
        original_ime = self.current_input_method()
        if not original_ime or original_ime.lower() == "null":
            raise ADBCommandError("cannot determine the input method to restore; no text was sent")
        self._ensure_input_method_enabled(APPIUM_UNICODE_IME)

        def send_text() -> None:
            if original_ime != APPIUM_UNICODE_IME:
                self.set_input_method(APPIUM_UNICODE_IME)
                if self.current_input_method() != APPIUM_UNICODE_IME:
                    raise ADBCommandError("Appium UnicodeIME selection failed; no text was sent")
                time.sleep(APPIUM_IME_BIND_WAIT_SEC)
            if replace:
                self.keycombination(["KEYCODE_CTRL_LEFT", "KEYCODE_A"])
                self.keyevent("KEYCODE_DEL")
            encoded = encode_modified_utf7(text)
            try:
                self._run(["shell", "input", "text", escape_adb_text(encoded)])
            except ADBCommandError:
                raise ADBCommandError(
                    "Appium UnicodeIME input failed or timed out; inspect the field before retrying"
                ) from None
            time.sleep(APPIUM_TEXT_COMMIT_WAIT_SEC)

        if original_ime != APPIUM_UNICODE_IME:
            self._run_with_input_method_restore(original_ime, send_text)
        else:
            send_text()

    def _run_with_input_method_restore(
        self,
        original_ime: str,
        action: Callable[[], None],
        restore_wait_sec: float = DEFAULT_TEXT_RESTORE_WAIT_SEC,
    ) -> None:
        def restore() -> None:
            if restore_wait_sec > 0:
                time.sleep(restore_wait_sec)
            self._restore_text_input_method(original_ime)

        try:
            action()
        except Exception as input_error:
            try:
                restore()
            except ADBCommandError as restore_error:
                raise ADBCommandError(f"{input_error}; {restore_error}") from input_error
            raise
        restore()

    def _recover_pending_input_method_restore(self) -> None:
        if self._pending_input_method_restore:
            self._restore_text_input_method(self._pending_input_method_restore)

    def _restore_text_input_method(self, original_ime: str) -> None:
        self._pending_input_method_restore = original_ime
        try:
            self.set_input_method(original_ime)
            if self.current_input_method() != original_ime:
                raise ADBCommandError("input method did not change")
        except ADBCommandError:
            raise ADBCommandError(
                "could not restore the original input method; inspect the field before retrying"
            ) from None
        self._pending_input_method_restore = None

    def _ensure_input_method_enabled(self, ime_id: str) -> None:
        if ime_id in self.list_input_methods():
            return
        output = self._run_text(["shell", "ime", "enable", ime_id], timeout=5)
        if "error" in output.lower() or "unknown" in output.lower() or ime_id not in self.list_input_methods():
            raise ADBCommandError(f"could not enable installed input method {ime_id!r}: {output.strip()}")

    def _wait_for_adb_keyboard(self) -> None:
        deadline = time.monotonic() + 3
        while True:
            state = self._run_text(["shell", "dumpsys", "input_method"], timeout=5)
            service = re.search(
                r"Input method service state for [^\n]*com\.android\.adbkeyboard[^\n]*:", state,
            )
            if service:
                details = re.split(r"\n(?=\S)", state[service.end():].lstrip("\n"), maxsplit=1)[0]
                if (re.search(r"\bmInputStarted=true\b", details) and
                        re.search(r"\bmStartedInputConnection=(?!null\b)\S+", details)):
                    return
            if time.monotonic() >= deadline:
                raise ADBCommandError("ADBKeyBoard has no active editor connection; focus an input field first")
            time.sleep(0.1)

    def _keyboard_broadcast(self, action: str, extras: list[str] | None = None) -> None:
        # Package targeting prevents another receiver from seeing the payload.
        # Do not include the Base64 payload in errors: it contains user text.
        try:
            output = self._run_text([
                "shell", "am", "broadcast", "-p", ADB_KEYBOARD_PACKAGE,
                "-a", action, *(extras or []),
            ])
        except ADBCommandError:
            raise ADBCommandError(
                f"{action} failed or timed out; inspect the field before retrying"
            ) from None
        if "Broadcast completed:" not in output or re.search(r"Error|Exception", output, re.I):
            raise ADBCommandError(f"{action} was not confirmed; inspect the field before retrying")

    def dump_window_xml(self) -> str:
        """Return the current Android UI hierarchy XML.

        The dump, the read-back and the cleanup share one adb shell invocation:
        three separate adb round trips were the largest single cost in every
        enter_text call, and the remote file is a scratch artifact of this call
        alone.
        """
        xml = self._run_text(
            [
                "shell",
                "uiautomator", "dump", WINDOW_XML_REMOTE_PATH,
                ";", "cat", WINDOW_XML_REMOTE_PATH,
                ";", "rm", "-f", WINDOW_XML_REMOTE_PATH,
            ],
            timeout=10,
        )
        start = xml.find("<?xml")
        if start < 0 or "<hierarchy" not in xml[start:]:
            raise ADBCommandError("uiautomator dump did not produce hierarchy XML")
        end = xml.rfind("</hierarchy>")
        if end < 0:
            return xml[start:].rstrip()
        return xml[start:end + len("</hierarchy>")]

    def current_input_method(self) -> str:
        return self._run_text(["shell", "settings", "get", "secure", "default_input_method"], timeout=5).strip()

    def list_input_methods(self) -> list[str]:
        output = self._run_text(["shell", "ime", "list", "-s"], timeout=5)
        return [line.strip() for line in output.splitlines() if line.strip()]

    def set_input_method(self, ime_id: str) -> None:
        output = self._run_text(["shell", "ime", "set", ime_id], timeout=5)
        if "error" in output.lower():
            raise ADBCommandError(f"set input method {ime_id!r} failed: {output.strip()}")

    def _switch_to_ascii_input_method(self) -> str:
        """Select an ASCII-capable IME and report the one to restore.

        The IME list is static for the lifetime of a device session, so the
        lookup is cached: probing it on every enter_text cost three adb round
        trips per call.
        """
        try:
            original_ime = self.current_input_method()
            if original_ime in PREFERRED_ASCII_INPUT_METHODS:
                return ""
            preferred = self._preferred_ascii_input_method()
            if not preferred or preferred == original_ime:
                return ""
            self.set_input_method(preferred)
            return original_ime
        except ADBCommandError:
            return ""

    def _preferred_ascii_input_method(self) -> str:
        """Return the ASCII IME to switch to, caching the enabled-IME probe."""
        if self._preferred_ime is not None:
            return self._preferred_ime
        try:
            enabled_imes = set(self.list_input_methods())
        except ADBCommandError:
            return ""
        self._preferred_ime = next(
            (ime for ime in PREFERRED_ASCII_INPUT_METHODS if ime in enabled_imes), ""
        )
        return self._preferred_ime

    def start_settings(self) -> None:
        self._run(["shell", "am", "start", "-a", "android.settings.SETTINGS"])

    def start_intent(
        self,
        *,
        action: str,
        data: str = "",
        package: str = "",
        mime_type: str = "",
        extras: dict[str, str] | None = None,
        new_task: bool = False,
        wait: bool = True,
    ) -> None:
        """Start an activity from an explicit intent (``am start``)."""
        args = ["shell", "am", "start"]
        if wait:
            args.append("-W")
        args += ["-a", action]
        if data:
            args += ["-d", _shell_quote(data)]
        if mime_type:
            args += ["-t", mime_type]
        if package:
            args += ["-p", package]
        for key, value in (extras or {}).items():
            args += ["--es", key, _shell_quote(value)]
        if new_task:
            args += ["-f", "0x10000000"]
        output = self._run_text(args, timeout=20)
        if "Error:" in output or "Exception" in output:
            raise ADBCommandError(f"am start failed: {output.strip()}")

    def force_stop(self, package: str) -> None:
        self._run(["shell", "am", "force-stop", package])

    def content_query(self, uri: str, *, projection: str = "", where: str = "") -> list[dict[str, str]]:
        """Run ``content query`` and parse its ``Row: n k=v, k=v`` output."""
        args = ["shell", "content", "query", "--uri", _shell_quote(uri)]
        if projection:
            args += ["--projection", projection]
        if where:
            args += ["--where", _shell_quote(where)]
        output = self._run_text(args, timeout=15)
        rows: list[dict[str, str]] = []
        for line in output.splitlines():
            match = re.match(r"\s*Row:\s*\d+\s+(.*)$", line)
            if not match:
                continue
            row: dict[str, str] = {}
            for part in re.split(r",\s+(?=[A-Za-z0-9_]+=)", match.group(1)):
                key, _, value = part.partition("=")
                row[key.strip()] = value.strip()
            rows.append(row)
        return rows

    def content_delete(self, uri: str, *, where: str) -> None:
        self._run(["shell", "content", "delete", "--uri", _shell_quote(uri), "--where", _shell_quote(where)], timeout=15)

    def content_insert(self, uri: str, bindings: list[tuple[str, str, str]]) -> None:
        """Insert one row; each binding is (column, type, value), type s/i/l/n."""
        args = ["shell", "content", "insert", "--uri", _shell_quote(uri)]
        for column, kind, value in bindings:
            args += ["--bind", _shell_quote(f"{column}:{kind}:{value}")]
        self._run(args, timeout=15)

    def capture_photo(self) -> None:
        """Take a photo through the Camera app and leave it in Pictures/DCIM."""
        self.start_intent(action="android.media.action.STILL_IMAGE_CAMERA", wait=True)
        time.sleep(1.5)
        self.keyevent("KEYCODE_CAMERA")
        time.sleep(1.5)

    def newest_media_image(self) -> dict[str, str] | None:
        rows = self.content_query(
            "content://media/external/images/media",
            projection="_id:_display_name:relative_path:date_added",
        )
        rows = [row for row in rows if row.get("date_added", "").isdigit()]
        if not rows:
            return None
        return max(rows, key=lambda row: int(row["date_added"]))

    def expand_notifications(self) -> None:
        self._run(["shell", "cmd", "statusbar", "expand-notifications"])

    def expand_settings(self) -> None:
        self._run(["shell", "cmd", "statusbar", "expand-settings"])

    def collapse_statusbar(self) -> None:
        self._run(["shell", "cmd", "statusbar", "collapse"])

    def keyboard_visible(self) -> bool:
        """Read current IME visibility, excluding histories and retained view state."""
        dump = self._run_text(["shell", "dumpsys", "input_method"], timeout=5)
        # The dump appends request histories and client/service state. In
        # particular mIsInputViewShown can stay true after hideWindow() has
        # hidden the IME window, so it is not a visibility signal.
        manager = re.split(
            r"(?m)^\s*(?:m\w*History\b|mImeTrackerService#History\b|Input method (?:client|service) state\b)",
            dump,
            maxsplit=1,
        )[0]
        shown = re.search(r"\bmInputShown=(true|false)\b", manager)
        service = re.search(r"(?ms)^Input method service state for [^\n]+:\n(.*?)(?:^Last computed insets:|\Z)", dump)
        visible = re.search(r"\bmWindowVisible=(true|false)\b", service.group(1)) if service else None
        # Manager and service visibility can disagree while a hide is in flight.
        # Do not accept reset until every available current signal is hidden.
        signals = [match.group(1) == "true" for match in (shown, visible) if match]
        if signals:
            return any(signals)
        raise ADBCommandError("cannot verify current keyboard window visibility during reset")

    def reset_home(self) -> None:
        """Return home and verify that no keyboard leaks into the next task."""
        try:
            self.collapse_statusbar()
        except ADBCommandError:
            pass
        self.keyevent("KEYCODE_HOME")
        time.sleep(0.3)
        for attempt in range(3):
            if not self.keyboard_visible():
                return
            if attempt < 2:
                self.keyevent("KEYCODE_BACK")
                self.keyevent("KEYCODE_HOME")
                time.sleep(0.3)
        raise ADBCommandError("keyboard is still visible after reset; refusing to start a contaminated task")


def unsupported_adb_text_chars(text: str) -> str:
    return "".join(ch for ch in text if ch not in ADB_TEXT_CHARS)


def encode_modified_utf7(text: str) -> str:
    """Encode Unicode for Appium UnicodeIME's modified UTF-7 decoder."""
    out: list[str] = []
    pending: list[str] = []

    def flush() -> None:
        if pending:
            encoded = base64.b64encode("".join(pending).encode("utf-16-be")).decode("ascii")
            out.append("&" + encoded.rstrip("=").replace("/", ",") + "-")
            pending.clear()

    for ch in text:
        if 0x20 <= ord(ch) <= 0x7E:
            flush()
            out.append("&-" if ch == "&" else ch)
        else:
            pending.append(ch)
    flush()
    return "".join(out)


def escape_adb_text(text: str) -> str:
    """Escape text for the device-side shell invoked by `adb shell input text`.

    adb concatenates the shell arguments and runs them through the device's
    `sh -c`, so the text must be single-quoted for that shell. Spaces become
    `%s` per `input text` conventions.
    """
    escaped = text.replace(" ", "%s")
    return "'" + escaped.replace("'", "'\\''") + "'"


def _shell_quote(value: str) -> str:
    """Single-quote a value for the device shell that `adb shell` invokes."""
    return "'" + str(value).replace("'", "'\\''") + "'"
