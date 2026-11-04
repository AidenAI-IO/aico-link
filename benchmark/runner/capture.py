from __future__ import annotations
import base64
import binascii
import json
from pathlib import Path
from typing import Any
import urllib.error
import urllib.request

from runner.environment_endpoint import EnvironmentEndpoint


class CaptureError(RuntimeError):
    pass


DEFAULT_JPEG_QUALITY = 80
DEFAULT_SCREENSHOT_TIMEOUT_SEC = 30


def take_environment_screenshot(
    environment_url: str,
    out_path: Path,
    benchmark_task_id: str | None = None,
    timeout: int = DEFAULT_SCREENSHOT_TIMEOUT_SEC,
) -> tuple[int, int]:
    """Read the environment bridge screenshot provider and write bytes to out_path."""
    screenshot = read_environment_frame(environment_url, benchmark_task_id, timeout)
    return _write_screenshot_payload(screenshot, out_path, context="screen")


def read_environment_frame(
    environment_url: str,
    benchmark_task_id: str | None = None,
    timeout: int = DEFAULT_SCREENSHOT_TIMEOUT_SEC,
) -> dict[str, Any]:
    """Return the bridge frame as base64 `data`, `width`, `height`, and `format`."""
    try:
        endpoint = EnvironmentEndpoint(environment_url).screen
    except ValueError as exc:
        raise CaptureError(str(exc)) from exc
    headers = {"Content-Type": "application/json"}
    task_id = str(benchmark_task_id or "").strip()
    if task_id:
        headers["benchmark-task-id"] = task_id
    body = json.dumps(
        {"format": "jpeg", "quality": DEFAULT_JPEG_QUALITY}
    ).encode("utf-8")
    req = urllib.request.Request(endpoint, data=body, headers=headers, method="POST")
    try:
        with urllib.request.urlopen(req, timeout=timeout) as resp:
            raw = resp.read()
    except urllib.error.HTTPError as e:
        try:
            raw = e.read()
        except Exception:
            raw = b""
        raise CaptureError(
            f"screen request failed HTTP {e.code}: {raw[:200]!r}"
        ) from e
    except urllib.error.URLError as e:
        raise CaptureError(f"screen request failed: {e}") from e
    except TimeoutError as e:
        raise CaptureError(f"screen request timed out: {e}") from e

    try:
        payload = json.loads(raw.decode("utf-8")) if raw else {}
    except (UnicodeDecodeError, json.JSONDecodeError) as e:
        raise CaptureError(
            f"screen request returned invalid JSON: {raw[:200]!r}"
        ) from e
    return _extract_provider_frame(payload)


def _extract_provider_frame(payload: Any) -> dict[str, Any]:
    if not isinstance(payload, dict):
        raise CaptureError(f"screen returned unexpected payload: {payload!r}")
    if payload.get("ok") is False:
        raise CaptureError(f"screen failed: {payload.get('error') or payload}")
    data = payload.get("data")
    if not isinstance(data, dict) or not data.get("image"):
        raise CaptureError(f"screen returned no image field: {payload!r}")
    meta = data.get("meta") if isinstance(data.get("meta"), dict) else {}
    return {
        "data": data["image"],
        "width": meta.get("width", 0),
        "height": meta.get("height", 0),
        "format": meta.get("pixel_format") or "jpeg",
    }


def _write_screenshot_payload(
    payload: dict[str, Any],
    out_path: Path,
    *,
    context: str,
) -> tuple[int, int]:
    data = payload.get("data")
    if not data:
        raise CaptureError(f"{context} returned no data field")
    out_path.parent.mkdir(parents=True, exist_ok=True)
    try:
        out_path.write_bytes(base64.b64decode(data))
    except (binascii.Error, ValueError) as e:
        raise CaptureError(f"invalid base64 screenshot data: {e}") from e
    return int(payload.get("width", 0)), int(payload.get("height", 0))
