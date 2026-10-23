"""Device layer for the ADB Android environment bridge.

Wraps all adb interactions with a target Android device (e.g. a Genymotion
emulator reachable at 127.0.0.1:6555). This module deliberately knows nothing
about HTTP or the bridge protocol: it returns plain values and raises
ADBCommandError / ValueError on failure. The bridge layers (tools_api/server)
translate those into protocol responses.
"""

from __future__ import annotations

import io
import re
import subprocess
import time
from typing import Any


DEFAULT_SCREENSHOT_MAX_WIDTH = 720
DEFAULT_JPEG_QUALITY = 75
WINDOW_XML_REMOTE_PATH = "/sdcard/aiden-window.xml"
PREFERRED_ASCII_INPUT_METHODS = (
    "org.pocketworkstation.pckeyboard/.LatinIME",
    "com.android.inputmethod.latin/.LatinIME",
)
DEFAULT_TEXT_RESTORE_WAIT_SEC = 0.08

# Characters adb `input text` can type reliably on a US keyboard layout.
# Deliberately excludes newline/tab (input text cannot type them) and any
# non-ASCII text (needs a clipboard/IME path, out of scope for v1).
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
KEYCODE_ENTER = 66
KEYCODE_DEL = 67
KEYCODE_APP_SWITCH = 187


class ADBCommandError(RuntimeError):
    """An adb command failed or timed out."""


class ADBAndroidDevice:
    """Thin wrapper over the adb CLI for a single device serial."""

    def __init__(
        self,
        serial: str,
        adb_path: str = "adb",
        timeout_sec: float = 10,
        screenshot_max_width: int = DEFAULT_SCREENSHOT_MAX_WIDTH,
        jpeg_quality: int = DEFAULT_JPEG_QUALITY,
    ):
        self.serial = str(serial).strip()
        if not self.serial:
            raise ValueError("adb serial is required")
        self.adb_path = adb_path
        self.timeout_sec = timeout_sec
        self.screenshot_max_width = max(1, int(screenshot_max_width))
        self.jpeg_quality = min(95, max(1, int(jpeg_quality)))
        self._screen_size: tuple[int, int] | None = None
        self._supports_keycombination: bool | None = None

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
        restore_ime = self._switch_to_ascii_input_method()
        try:
            self._run(["shell", "input", "text", escape_adb_text(text)])
        finally:
            self._restore_input_method(restore_ime)

    def dump_window_xml(self) -> str:
        """Return the current Android UI hierarchy XML."""
        try:
            self._run_text(["shell", "uiautomator", "dump", WINDOW_XML_REMOTE_PATH], timeout=5)
            xml = self._run_text(["shell", "cat", WINDOW_XML_REMOTE_PATH], timeout=5)
        finally:
            try:
                self._run(["shell", "rm", "-f", WINDOW_XML_REMOTE_PATH], timeout=2)
            except ADBCommandError:
                pass
        if "<hierarchy" not in xml:
            raise ADBCommandError("uiautomator dump did not produce hierarchy XML")
        return xml

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
        try:
            original_ime = self.current_input_method()
            if original_ime in PREFERRED_ASCII_INPUT_METHODS:
                return ""
            enabled_imes = set(self.list_input_methods())
            preferred = next((ime for ime in PREFERRED_ASCII_INPUT_METHODS if ime in enabled_imes), "")
            if not preferred or preferred == original_ime:
                return ""
            self.set_input_method(preferred)
            return original_ime
        except ADBCommandError:
            return ""

    def _restore_input_method(self, ime_id: str) -> None:
        if not ime_id or ime_id.lower() == "null":
            return
        time.sleep(DEFAULT_TEXT_RESTORE_WAIT_SEC)
        try:
            self.set_input_method(ime_id)
        except ADBCommandError:
            pass

    def start_settings(self) -> None:
        self._run(["shell", "am", "start", "-a", "android.settings.SETTINGS"])

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


def escape_adb_text(text: str) -> str:
    """Escape text for the device-side shell invoked by `adb shell input text`.

    adb concatenates the shell arguments and runs them through the device's
    `sh -c`, so the text must be single-quoted for that shell. Spaces become
    `%s` per `input text` conventions.
    """
    escaped = text.replace(" ", "%s")
    return "'" + escaped.replace("'", "'\\''") + "'"
