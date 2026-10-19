from __future__ import annotations

from typing import Any


def dict_contains(actual: dict[str, Any], expected: dict[str, Any]) -> bool:
    """Return whether actual contains the expected nested matcher subset."""
    for key, expected_value in expected.items():
        if key not in actual or not value_matches(actual[key], expected_value):
            return False
    return True


def value_matches(actual: Any, expected: Any) -> bool:
    if isinstance(expected, dict):
        if set(expected) == {"$contains"}:
            needle = expected["$contains"]
            if isinstance(actual, str) and isinstance(needle, str):
                return needle in actual
            if isinstance(actual, list):
                return any(value_matches(item, needle) for item in actual)
            return False
        if expected and set(expected) <= {"$gte", "$lte"}:
            # Inclusive numeric bounds, e.g. a normalized tap coordinate band.
            if isinstance(actual, bool) or not isinstance(actual, (int, float)):
                return False
            return expected.get("$gte", actual) <= actual <= expected.get("$lte", actual)
        return isinstance(actual, dict) and dict_contains(actual, expected)
    return actual == expected
