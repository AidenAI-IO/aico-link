from __future__ import annotations

import base64
import io
from typing import Any


def bridge_ok(data: Any | None = None) -> dict[str, Any]:
    return {"ok": True, "data": {} if data is None else data}


def bridge_error(code: str, message: str, status: int = 400) -> dict[str, Any]:
    return {"ok": False, "error": {"code": code, "message": message}, "status": status}


def parse_initial_state(raw: Any) -> dict[str, Any] | None:
    """Validate a setup ``initial_state`` patch for ``window.__SIM__.setState``."""
    if raw is None:
        return None
    if not isinstance(raw, dict) or not raw or set(raw) - {"apps", "os"}:
        raise ValueError("initial_state must be a non-empty object with apps and/or os patches")
    for key, patch in raw.items():
        if not isinstance(patch, dict):
            raise ValueError(f"initial_state.{key} must be an object")
    return raw


def encode_screenshot(
    payload: bytes,
    mime_type: str = "application/octet-stream",
    width: int | None = None,
    height: int | None = None,
) -> dict[str, Any]:
    if width is None or height is None:
        inferred = _image_dimensions(payload)
        if inferred is not None:
            width = inferred[0] if width is None else width
            height = inferred[1] if height is None else height
    if width is None or height is None:
        raise ValueError("screenshot width and height are required")
    fmt = mime_type.split("/", 1)[-1] if "/" in mime_type else mime_type
    if fmt == "jpg":
        fmt = "jpeg"
    return {
        "width": int(width),
        "height": int(height),
        "format": fmt,
        "size": len(payload),
        "data": base64.b64encode(payload).decode("ascii"),
    }


def encode_provider_frame(
    payload: bytes,
    *,
    width: int,
    height: int,
    pixel_format: str = "jpeg",
    backend: str = "mobilegym",
    seq: int = 1,
    source_width: int | None = None,
    source_height: int | None = None,
) -> dict[str, Any]:
    source_width = int(source_width or width)
    source_height = int(source_height or height)
    return {
        "meta": {
            "seq": int(seq),
            "width": int(width),
            "height": int(height),
            "source_width": source_width,
            "source_height": source_height,
            "crop_x": 0,
            "crop_y": 0,
            "crop_width": int(width),
            "crop_height": int(height),
            "pixel_format": pixel_format,
            "stride": 0,
            "bytes": len(payload),
            "stale": False,
        },
        "capture_info": {"capture_backend": backend},
        "image": base64.b64encode(payload).decode("ascii"),
    }


def encode_image_as_format(
    payload: bytes,
    current_format: str,
    requested_format: str,
    quality: int,
) -> tuple[bytes, str]:
    """Convert capture bytes to the requested provider format when possible."""
    current = _normalize_pixel_format(current_format) or _infer_pixel_format(payload)
    requested = _normalize_pixel_format(requested_format) or "jpeg"
    if requested != "jpeg" or current == "jpeg":
        return payload, current or requested
    if quality <= 0:
        quality = 80
    try:
        from PIL import Image
    except ImportError:
        return payload, current or "png"

    try:
        image = Image.open(io.BytesIO(payload)).convert("RGB")
        encoded = io.BytesIO()
        image.save(encoded, format="JPEG", quality=min(100, quality))
        return encoded.getvalue(), "jpeg"
    except Exception:
        return payload, current or "png"


def _normalize_pixel_format(value: str) -> str:
    fmt = str(value or "").strip().lower()
    if fmt == "jpg":
        return "jpeg"
    if fmt in {"jpeg", "png", "raw"}:
        return fmt
    return ""


def _infer_pixel_format(payload: bytes) -> str:
    if payload.startswith(b"\x89PNG\r\n\x1a\n"):
        return "png"
    if payload.startswith(b"\xff\xd8"):
        return "jpeg"
    return ""

def _image_dimensions(payload: bytes) -> tuple[int, int] | None:
    if payload.startswith(b"\x89PNG\r\n\x1a\n") and len(payload) >= 24:
        return int.from_bytes(payload[16:20], "big"), int.from_bytes(payload[20:24], "big")
    if not payload.startswith(b"\xff\xd8"):
        return None

    offset = 2
    while offset + 9 < len(payload):
        if payload[offset] != 0xFF:
            offset += 1
            continue
        marker = payload[offset + 1]
        offset += 2
        while marker == 0xFF and offset < len(payload):
            marker = payload[offset]
            offset += 1
        if marker in {0x01, *range(0xD0, 0xD9)}:
            continue
        if offset + 2 > len(payload):
            return None
        segment_length = int.from_bytes(payload[offset : offset + 2], "big")
        if segment_length < 2 or offset + segment_length > len(payload):
            return None
        if marker in {0xC0, 0xC1, 0xC2, 0xC3, 0xC5, 0xC6, 0xC7, 0xC9, 0xCA, 0xCB, 0xCD, 0xCE, 0xCF}:
            return int.from_bytes(payload[offset + 5 : offset + 7], "big"), int.from_bytes(
                payload[offset + 3 : offset + 5], "big"
            )
        offset += segment_length
    return None
