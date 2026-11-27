from __future__ import annotations

from datetime import datetime
from decimal import Decimal
import re
from typing import Any


RFC3339_BOUNDS = {"$rfc3339_gte", "$rfc3339_lte"}
_RFC3339 = re.compile(
    r"(?P<timestamp>[0-9]{4}-[0-9]{2}-[0-9]{2}T[0-9]{2}:[0-9]{2}:[0-9]{2})"
    r"(?:\.(?P<fraction>[0-9]+))?"
    r"(?P<offset>Z|[+-](?:[01][0-9]|2[0-3]):[0-5][0-9])"
)


def parse_rfc3339(value: Any) -> tuple[datetime, Decimal] | None:
    """Return a comparison key with an aware whole second and exact fraction."""
    if not isinstance(value, str):
        return None
    match = _RFC3339.fullmatch(value)
    if match is None:
        return None
    offset = "+00:00" if match["offset"] == "Z" else match["offset"]
    try:
        whole_second = datetime.fromisoformat(match["timestamp"] + offset)
    except ValueError:
        return None
    fraction = Decimal("0." + (match["fraction"] or "0"))
    return whole_second, fraction


def dict_contains(actual: dict[str, Any], expected: dict[str, Any]) -> bool:
    """Return whether actual contains the expected nested matcher subset."""
    for key, expected_value in expected.items():
        if key not in actual or not value_matches(actual[key], expected_value):
            return False
    return True


def value_matches(actual: Any, expected: Any) -> bool:
    if isinstance(expected, dict):
        keys = set(expected)
        if keys == {"$one_of"}:
            return any(value_matches(actual, alternative) for alternative in expected["$one_of"])
        if keys == {"$contains"}:
            needle = expected["$contains"]
            if isinstance(actual, str) and isinstance(needle, str):
                return needle in actual
            if isinstance(actual, list):
                return any(value_matches(item, needle) for item in actual)
            return False
        if keys == {"$digits"}:
            # Phone numbers: "+1 415-555-0132" and "+14155550132" are the same number.
            return isinstance(actual, str) and re.sub(r"\D", "", actual) == expected["$digits"]
        if keys and keys <= {"$gte", "$lte"}:
            # Inclusive numeric bounds, e.g. a normalized tap coordinate band.
            if isinstance(actual, bool) or not isinstance(actual, (int, float)):
                return False
            return expected.get("$gte", actual) <= actual <= expected.get("$lte", actual)
        if keys and keys <= RFC3339_BOUNDS:
            instant = parse_rfc3339(actual)
            if instant is None:
                return False
            lower = parse_rfc3339(expected["$rfc3339_gte"]) if "$rfc3339_gte" in expected else instant
            upper = parse_rfc3339(expected["$rfc3339_lte"]) if "$rfc3339_lte" in expected else instant
            return lower is not None and upper is not None and lower <= instant <= upper
        return isinstance(actual, dict) and dict_contains(actual, expected)
    return actual == expected
