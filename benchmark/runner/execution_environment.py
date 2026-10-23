"""Presentation identities for benchmark execution environments.

Suite names describe tasks, not their backend. Only recorded metadata may
identify an environment; older records without it remain Unknown.
"""

from typing import Any, Mapping


ENVIRONMENT_LABELS = {
    "adb": "Android (ADB)",
    "mobilegym": "MobileGym",
    "isolated": "Isolated",
    "scripted": "Scripted",
    "external": "External device",
    "mixed": "Mixed",
}


def environment_key(record: Mapping[str, Any]) -> tuple[str, str]:
    return (record.get("execution_environment") or "unknown",
            record.get("target_platform") or "auto")


def environment_label(record: Mapping[str, Any]) -> str:
    environment, platform = environment_key(record)
    label = ENVIRONMENT_LABELS.get(environment, "Unknown")
    if platform != "auto" and environment != "adb":
        label += f" / {platform}"
    return label
