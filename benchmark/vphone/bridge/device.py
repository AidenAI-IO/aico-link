"""VPhone iOS device operations backed by the host-control Unix socket."""

from __future__ import annotations

import dataclasses as dc
import io
import re
import subprocess
import tempfile
import threading
import time
from pathlib import Path
from typing import Any

from PIL import Image

from .client import VPhoneSocketClient, VPhoneSocketError


DEFAULT_SCREENSHOT_MAX_WIDTH = 900
DEFAULT_JPEG_QUALITY = 80
MAX_TEXT_LENGTH = 1024
KEY_INTERVAL_SEC = 0.02
KEYBOARD_TIMEOUT_MARGIN_SEC = 5.0
VPHONE_TEXT_CHARS = set(
    "abcdefghijklmnopqrstuvwxyz"
    "ABCDEFGHIJKLMNOPQRSTUVWXYZ"
    "0123456789"
    " "
    "-=[]\\;'`,./"
    "!@#$%^&*()_+{}|:\"~<>?"
    "\t\n\r"
)
_BUNDLE_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9.-]{0,254}$")


@dc.dataclass(frozen=True)
class GuestSSHConfig:
    host: str
    port: int = 22222
    user: str = "root"
    identity_file: str = ""
    connect_timeout_sec: float = 5


class VPhoneDevice:
    def __init__(
        self,
        socket_path: str | Path,
        *,
        timeout_sec: float = 30,
        screenshot_max_width: int = DEFAULT_SCREENSHOT_MAX_WIDTH,
        jpeg_quality: int = DEFAULT_JPEG_QUALITY,
        guest_ssh: GuestSSHConfig | None = None,
    ):
        self.socket_path = Path(socket_path).expanduser()
        self.client = VPhoneSocketClient(self.socket_path, timeout_sec=timeout_sec)
        self.timeout_sec = max(0.1, float(timeout_sec))
        self.screenshot_max_width = max(1, int(screenshot_max_width))
        self.jpeg_quality = min(95, max(1, int(jpeg_quality)))
        self.guest_ssh = guest_ssh
        self._screen_size: tuple[int, int] | None = None
        self._status: dict[str, Any] | None = None
        self._tempdir = tempfile.TemporaryDirectory(prefix="aico-vphone-bridge-")
        self._capture_lock = threading.Lock()

    def close(self) -> None:
        self._tempdir.cleanup()

    def check_device(self) -> dict[str, Any]:
        """Return live VM status, with a legacy screenshot fallback."""
        try:
            response = self.client.request({"t": "status", "screen": False})
        except VPhoneSocketError as exc:
            if exc.code != "command_failed" or "unknown command" not in str(exc).lower():
                raise
            jpeg, width, height, source_width, source_height = self.screenshot_jpeg()
            del jpeg, width, height
            status = {
                "status": "ok",
                "legacy_host_control": True,
                "screen_width": source_width,
                "screen_height": source_height,
                "display_ready": True,
                "vphoned_connected": None,
                "capabilities": ["screenshot", "touch", "hid", "clipboard"],
            }
            self._status = status
            return status

        status = dict(response)
        width = _positive_int(status.get("screen_width"))
        height = _positive_int(status.get("screen_height"))
        if width and height:
            self._screen_size = (width, height)
        if status.get("display_ready") is False:
            raise VPhoneSocketError("display_unavailable", "VPhone display is not ready")
        if status.get("vphoned_connected") is False:
            raise VPhoneSocketError("guest_unavailable", "vphoned is not connected")
        status.setdefault("status", "ok")
        status.setdefault("legacy_host_control", False)
        self._status = status
        return status

    def capabilities(self) -> set[str]:
        if self._status is None:
            try:
                self.check_device()
            except VPhoneSocketError:
                return set()
        raw = (self._status or {}).get("capabilities") or []
        return {str(item).strip() for item in raw if str(item).strip()}

    def screen_size(self) -> tuple[int, int]:
        if self._screen_size is not None:
            return self._screen_size
        status = self.check_device()
        width = _positive_int(status.get("screen_width"))
        height = _positive_int(status.get("screen_height"))
        if not width or not height:
            raise VPhoneSocketError("screen_size_unavailable", "VPhone screen dimensions are unavailable")
        self._screen_size = (width, height)
        return self._screen_size

    def screenshot_jpeg(self) -> tuple[bytes, int, int, int, int]:
        """Capture full-color PNG, scale it, and return JPEG plus both dimensions."""
        with self._capture_lock:
            png = self._capture_png()
        try:
            image = Image.open(io.BytesIO(png)).convert("RGB")
        except Exception as exc:
            raise VPhoneSocketError("invalid_screenshot", f"cannot decode VPhone screenshot: {exc}") from exc
        source_width, source_height = image.size
        self._screen_size = (source_width, source_height)
        if image.width > self.screenshot_max_width:
            new_height = max(1, round(image.height * self.screenshot_max_width / image.width))
            image = image.resize((self.screenshot_max_width, new_height), Image.Resampling.LANCZOS)
        output = io.BytesIO()
        image.save(output, format="JPEG", quality=self.jpeg_quality)
        return output.getvalue(), image.width, image.height, source_width, source_height

    def _capture_png(self) -> bytes:
        fd, raw_path = tempfile.mkstemp(suffix=".png", dir=self._tempdir.name)
        path = Path(raw_path)
        try:
            # The VPhone recorder creates/replaces the destination itself.
            Path(raw_path).unlink(missing_ok=True)
            response = self.client.request(
                {"t": "screenshot", "path": str(path), "screen": False},
                timeout_sec=max(self.timeout_sec, 30),
            )
            response_path = Path(str(response.get("path") or path))
            if response_path != path:
                raise VPhoneSocketError("invalid_screenshot_path", "VPhone returned an unexpected screenshot path")
            try:
                payload = path.read_bytes()
            except OSError as exc:
                raise VPhoneSocketError("screenshot_missing", f"VPhone did not create screenshot: {exc}") from exc
            if not payload.startswith(b"\x89PNG\r\n\x1a\n"):
                raise VPhoneSocketError("invalid_screenshot", "VPhone screenshot is not a PNG file")
            return payload
        finally:
            try:
                Path(raw_path).unlink(missing_ok=True)
            finally:
                try:
                    # mkstemp's descriptor remains valid after unlink.
                    import os

                    os.close(fd)
                except OSError:
                    pass

    def tap(self, x: int, y: int) -> None:
        self.client.request({"t": "tap", "x": int(x), "y": int(y), "screen": False})
        if "gesture_completion" not in self.capabilities():
            time.sleep(0.1)

    def double_tap(self, x: int, y: int, pause_ms: int = 120) -> None:
        pause_ms = max(20, min(180, int(pause_ms)))
        try:
            self.client.request(
                {"t": "double_tap", "x": int(x), "y": int(y), "pause_ms": pause_ms, "screen": False}
            )
            return
        except VPhoneSocketError as exc:
            if exc.code != "command_failed" or "unknown command" not in str(exc).lower():
                raise
        # Legacy injectTap returns before its 80ms mouse-up. Allow that key-up
        # to run, then keep the second down inside the calibrated double-click window.
        self.client.request({"t": "tap", "x": int(x), "y": int(y), "screen": False})
        time.sleep(max(0.09, pause_ms / 1000))
        self.client.request({"t": "tap", "x": int(x), "y": int(y), "screen": False})
        time.sleep(0.1)

    def swipe(self, x1: int, y1: int, x2: int, y2: int, duration_ms: int) -> None:
        duration_ms = max(1, min(10_000, int(duration_ms)))
        self.client.request(
            {
                "t": "swipe",
                "x1": int(x1),
                "y1": int(y1),
                "x2": int(x2),
                "y2": int(y2),
                "ms": duration_ms,
                "screen": False,
            },
            timeout_sec=max(self.timeout_sec, duration_ms / 1000 + 5),
        )
        if "gesture_completion" not in self.capabilities():
            time.sleep(duration_ms / 1000 + 0.05)

    def hardware_key(self, name: str) -> None:
        self.client.request({"t": "key", "name": str(name), "screen": False})

    def keyboard_key(self, name: str) -> None:
        self.client.request({"t": "keyboard_key", "name": str(name), "screen": False})

    def keyboard_text(self, text: str) -> None:
        if not isinstance(text, str) or not text:
            raise ValueError("text is required")
        if len(text) > MAX_TEXT_LENGTH:
            raise ValueError(f"text exceeds {MAX_TEXT_LENGTH} characters")
        unsupported = unsupported_vphone_text_chars(text)
        if unsupported:
            raise ValueError(
                f"VPhone keyboard supports only US-keyboard ASCII characters; unsupported: {unsupported!r}"
            )
        dynamic_timeout = KEYBOARD_TIMEOUT_MARGIN_SEC + len(text) * KEY_INTERVAL_SEC
        self.client.request(
            {"t": "keyboard_text", "text": text, "screen": False},
            timeout_sec=dynamic_timeout,
        )

    def clipboard_set(self, text: str) -> None:
        if not isinstance(text, str):
            raise ValueError("text is required")
        if len(text) > MAX_TEXT_LENGTH:
            raise ValueError(f"text exceeds {MAX_TEXT_LENGTH} characters")
        self.client.request({"t": "type", "text": text, "screen": False})

    def launch_app(self, bundle_id: str) -> None:
        bundle_id = _validated_bundle_id(bundle_id)
        try:
            self.client.request({"t": "app_launch", "bundle_id": bundle_id, "screen": False})
        except VPhoneSocketError as exc:
            if not self._can_use_ssh_fallback(exc):
                raise
            self._run_guest_command(["/var/jb/usr/bin/uiopen", "-b", bundle_id])

    def reset_home(self) -> None:
        self.hardware_key("home")
        time.sleep(0.4)

    def _can_use_ssh_fallback(self, exc: VPhoneSocketError) -> bool:
        return (
            self.guest_ssh is not None
            and exc.code in {"command_failed", "unknown_command"}
            and "unknown command" in str(exc).lower()
        )

    def _run_guest_command(self, remote_args: list[str]) -> None:
        config = self.guest_ssh
        if config is None:
            raise VPhoneSocketError("unsupported", "guest SSH fallback is not configured")
        # The guest is an ephemeral local VM whose IP is re-detected from the
        # host DHCP leases on every bridge start, so its host key is unknown on
        # a fresh IP and changed on a recycled one. With BatchMode=yes the
        # default StrictHostKeyChecking aborts in both cases ("Host key
        # verification failed"), which breaks every app launch. Skip host key
        # tracking for this host-private virtualization network instead of
        # writing throwaway keys into the operator's known_hosts.
        command = [
            "ssh",
            "-p",
            str(config.port),
            "-o",
            "BatchMode=yes",
            "-o",
            "StrictHostKeyChecking=no",
            "-o",
            "UserKnownHostsFile=/dev/null",
            "-o",
            "LogLevel=ERROR",
            "-o",
            f"ConnectTimeout={max(1, int(config.connect_timeout_sec))}",
        ]
        if config.identity_file:
            command.extend(["-i", str(Path(config.identity_file).expanduser())])
        command.append(f"{config.user}@{config.host}")
        command.extend(remote_args)
        try:
            result = subprocess.run(
                command,
                capture_output=True,
                text=True,
                timeout=max(5.0, config.connect_timeout_sec + 5),
                check=False,
            )
        except (OSError, subprocess.TimeoutExpired) as exc:
            raise VPhoneSocketError("guest_ssh_failed", f"guest SSH command failed: {exc}") from exc
        if result.returncode != 0:
            detail = result.stderr.strip() or result.stdout.strip() or f"exit code {result.returncode}"
            raise VPhoneSocketError("guest_ssh_failed", f"guest SSH command failed: {detail}")


def unsupported_vphone_text_chars(text: str) -> str:
    return "".join(ch for ch in text if ch not in VPHONE_TEXT_CHARS)


def _validated_bundle_id(value: str) -> str:
    bundle_id = str(value).strip()
    if not _BUNDLE_ID_RE.fullmatch(bundle_id):
        raise ValueError("bundle_id has an invalid format")
    return bundle_id


def _positive_int(value: Any) -> int | None:
    try:
        parsed = int(value)
    except (TypeError, ValueError):
        return None
    return parsed if parsed > 0 else None
