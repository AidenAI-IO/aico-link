"""Validation for user-provided launcher values.

Everything here is applied before a value reaches a subprocess argument list,
a container name, or a file path.
"""

from __future__ import annotations

import re
import urllib.parse
from typing import Any

from runner.environment_endpoint import EnvironmentEndpoint


class ValidationError(ValueError):
    """A user-provided value is not acceptable; the message is shown verbatim."""


_TAG_RE = re.compile(r"^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$")
# Docker references: [registry[:port]/]path[:tag][@digest]. Kept conservative:
# no whitespace, no leading '-', only characters Docker accepts.
_IMAGE_REF_RE = re.compile(
    r"^[a-z0-9][a-z0-9._-]*(?::[0-9]+)?(?:/[a-z0-9][a-z0-9._-]*)*"
    r"(?::[A-Za-z0-9_][A-Za-z0-9_.-]{0,127})?(?:@sha256:[0-9a-f]{64})?$"
)
_SERIAL_RE = re.compile(r"^[A-Za-z0-9._:@/+\[\]-]{1,128}$")
_EXECUTABLE_RE = re.compile(r"^[A-Za-z0-9._/~+ -]{1,512}$")
_INSTANCE_ID_RE = re.compile(r"^[a-z0-9]{6,32}$")
_BUILD_ID_RE = re.compile(r"^[A-Za-z0-9-]{1,64}$")


def image_tag(value: Any) -> str:
    tag = str(value or "").strip()
    if not _TAG_RE.fullmatch(tag):
        raise ValidationError(
            "Tags may contain letters, digits, '_', '.', and '-', start with a letter, "
            "digit, or '_', and be at most 128 characters."
        )
    return tag


def image_ref(value: Any, *, field: str = "Image") -> str:
    ref = str(value or "").strip()
    if not ref:
        raise ValidationError(f"{field} is required.")
    if len(ref) > 255 or not _IMAGE_REF_RE.fullmatch(ref):
        raise ValidationError(f"{field} is not a valid Docker image reference: {ref!r}.")
    return ref


def image_has_namespace(ref: str) -> bool:
    """True when ref names a registry or namespace and can therefore be pulled."""
    name = ref.split("@", 1)[0]
    return "/" in name


def instance_name(value: Any) -> str:
    name = str(value or "").strip()
    if not name:
        raise ValidationError("Name is required.")
    if len(name) > 64:
        raise ValidationError("Name must be at most 64 characters.")
    if any(ord(ch) < 32 or ord(ch) == 127 for ch in name):
        raise ValidationError("Name must not contain control characters.")
    return name


def instance_id(value: Any) -> str:
    text = str(value or "")
    if not _INSTANCE_ID_RE.fullmatch(text):
        raise ValidationError("Unknown instance.")
    return text


def build_id(value: Any) -> str:
    text = str(value or "")
    if not _BUILD_ID_RE.fullmatch(text):
        raise ValidationError("Unknown build.")
    return text


def port(value: Any, *, field: str) -> int:
    if isinstance(value, bool):
        raise ValidationError(f"{field} must be a whole number from 1 to 65535.")
    try:
        number = int(value)
    except (TypeError, ValueError):
        raise ValidationError(f"{field} must be a whole number from 1 to 65535.") from None
    if str(value).strip() not in {str(number), f"{number}.0"} or not 1 <= number <= 65535:
        raise ValidationError(f"{field} must be a whole number from 1 to 65535.")
    return number


def optional_port(value: Any, *, field: str) -> int:
    if value is None or value == "" or value == 0:
        return 0
    return port(value, field=field)


def boolean(value: Any, *, field: str) -> bool:
    if isinstance(value, bool):
        return value
    if value is None:
        return False
    raise ValidationError(f"{field} must be true or false.")


def adb_serial(value: Any) -> str:
    serial = str(value or "").strip()
    if not serial:
        raise ValidationError("Select an ADB device serial.")
    if serial.startswith("-") or not _SERIAL_RE.fullmatch(serial):
        raise ValidationError(f"ADB serial is not valid: {serial!r}.")
    return serial


def executable(value: Any, *, field: str, default: str) -> str:
    text = str(value or "").strip() or default
    if text.startswith("-") or not _EXECUTABLE_RE.fullmatch(text):
        raise ValidationError(f"{field} is not a valid executable name or path.")
    return text


def http_url(value: Any, *, field: str) -> str:
    text = str(value or "").strip()
    try:
        normalized = EnvironmentEndpoint(text).base
    except ValueError:
        raise ValidationError(f"{field} must be an http:// or https:// URL without a query, e.g. http://host.docker.internal:19090.") from None
    parsed = urllib.parse.urlsplit(normalized)
    if parsed.username or parsed.password:
        raise ValidationError(f"{field} must not contain credentials.")
    if not parsed.hostname:
        raise ValidationError(f"{field} must name a host.")
    try:
        parsed.port
    except ValueError:
        raise ValidationError(f"{field} has an invalid port.") from None
    return normalized


def optional_http_url(value: Any, *, field: str) -> str:
    text = str(value or "").strip()
    return http_url(text, field=field) if text else ""


def optional_image_ref(value: Any, *, field: str) -> str:
    text = str(value or "").strip()
    return image_ref(text, field=field) if text else ""


def integer(value: Any, *, field: str, minimum: int, maximum: int) -> int:
    if isinstance(value, bool):
        raise ValidationError(f"{field} must be a whole number.")
    try:
        number = int(value)
    except (TypeError, ValueError):
        raise ValidationError(f"{field} must be a whole number.") from None
    if float(value) != number or not minimum <= number <= maximum:
        raise ValidationError(f"{field} must be a whole number from {minimum} to {maximum}.")
    return number


_UDID_RE = re.compile(r"^[A-Za-z0-9-]{1,64}$")
_BUNDLE_ID_RE = re.compile(r"^[A-Za-z0-9][A-Za-z0-9.-]{0,254}$")
_HOSTNAME_RE = re.compile(r"^[A-Za-z0-9](?:[A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$")
_USER_RE = re.compile(r"^[A-Za-z_][A-Za-z0-9_.-]{0,31}$")
_INSETS_RE = re.compile(r"^\s*\d+(?:\.\d+)?(?:\s*,\s*\d+(?:\.\d+)?){3}\s*$")
MAX_PATH_LENGTH = 1024
MAX_COMMAND_LENGTH = 1024


def _no_control(text: str, field: str) -> None:
    if any(ord(ch) < 32 or ord(ch) == 127 for ch in text):
        raise ValidationError(f"{field} must not contain control characters.")


def host_path(value: Any, *, field: str, must_exist: bool = False, kind: str = "") -> str:
    """An absolute path on this computer ('~' is expanded); '' when blank.

    kind "file" requires an existing regular file when must_exist is set.
    """
    import os

    text = str(value or "").strip()
    if not text:
        return ""
    _no_control(text, field)
    if len(text) > MAX_PATH_LENGTH:
        raise ValidationError(f"{field} is too long.")
    path = os.path.expanduser(text)
    if not os.path.isabs(path):
        raise ValidationError(f"{field} must be an absolute path, e.g. /Users/me/... or ~/...")
    path = os.path.normpath(path)
    if must_exist:
        if not os.path.exists(path):
            raise ValidationError(f"{field} does not exist: {path}")
        if kind == "file" and not os.path.isfile(path):
            raise ValidationError(f"{field} is not a file: {path}")
    return path


def ios_udid(value: Any) -> str:
    text = str(value or "").strip()
    if text and (text.startswith("-") or not _UDID_RE.fullmatch(text)):
        raise ValidationError(f"Device UDID is not valid: {text!r}.")
    return text


def bundle_id(value: Any, *, field: str, default: str) -> str:
    text = str(value or "").strip() or default
    if not _BUNDLE_ID_RE.fullmatch(text):
        raise ValidationError(f"{field} must be a bundle identifier such as com.example.Runner.xctrunner.")
    return text


def host_name(value: Any, *, field: str) -> str:
    """An IP address or DNS name; '' when blank."""
    text = str(value or "").strip()
    if text and not _HOSTNAME_RE.fullmatch(text):
        raise ValidationError(f"{field} must be an IP address or host name.")
    return text


def user_name(value: Any, *, field: str, default: str) -> str:
    text = str(value or "").strip() or default
    if not _USER_RE.fullmatch(text):
        raise ValidationError(f"{field} is not a valid user name.")
    return text


def content_insets(value: Any, *, default: str) -> str:
    text = str(value or "").strip() or default
    if not _INSETS_RE.fullmatch(text):
        raise ValidationError(
            "Content insets must be four non-negative numbers: left,top,right,bottom (macOS points)."
        )
    return ",".join(part.strip() for part in text.split(","))


def command_line(value: Any, *, field: str) -> str:
    """A command prefix the bridge splits with shlex; '' when blank."""
    import shlex

    text = str(value or "").strip()
    if not text:
        return ""
    _no_control(text, field)
    if len(text) > MAX_COMMAND_LENGTH or text.startswith("-"):
        raise ValidationError(f"{field} is not a valid command.")
    try:
        parts = shlex.split(text)
    except ValueError as exc:
        raise ValidationError(f"{field} is not a valid command: {exc}.") from None
    if not parts:
        raise ValidationError(f"{field} is not a valid command.")
    return text
