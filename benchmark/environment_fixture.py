"""Validation shared by suite loading and the MobileGym setup endpoint."""
from __future__ import annotations

import json
from typing import Any


def parse_initial_environment_state(raw: Any, app_ids: list[str]) -> dict[str, Any]:
    if raw is None:
        return {}
    if not isinstance(raw, dict) or not raw or set(raw) - {"apps", "os"}:
        raise ValueError("initial_environment_state must contain only apps and/or os objects")
    if any(not isinstance(value, dict) or not value for value in raw.values()):
        raise ValueError("initial_environment_state sections must be non-empty objects")
    apps = raw.get("apps", {})
    if any(app_id not in app_ids for app_id in apps):
        raise ValueError("initial_environment_state apps must be included in app_ids")
    try:
        json.dumps(raw, allow_nan=False)
    except (TypeError, ValueError) as exc:
        raise ValueError("initial_environment_state must contain finite JSON values") from exc
    return raw


def state_contains_fixture(actual: Any, expected: Any) -> bool:
    """Match supplied fields recursively; arrays must match exactly, in order."""
    if isinstance(expected, dict):
        return isinstance(actual, dict) and all(
            key in actual and state_contains_fixture(actual[key], value)
            for key, value in expected.items()
        )
    return actual == expected
