"""Tests for the ADB bridge's deterministic open_app launcher."""

from __future__ import annotations

import json

import pytest

from adbandroid.bridge.tools_api import _resolve_app_package

# The bridge fixture and the fake device live beside the other bridge tests.
from tests.test_adbandroid_bridge import bridge  # noqa: F401


def _invoke(base_url, tool_name, payload, task_id="suite:open-app"):
    from tests.test_adbandroid_bridge import _request

    _request(base_url, "/api/setup", method="POST", task_id=task_id)
    return _request(
        base_url,
        f"/api/tools/{tool_name}",
        method="POST",
        task_id=task_id,
        payload={"input": payload},
    )


@pytest.mark.parametrize(
    "app, expected",
    [
        ("Maps", "com.google.android.apps.maps"),
        ("Google Maps", "com.google.android.apps.maps"),
        ("maps", "com.google.android.apps.maps"),
        ("Chrome", "com.android.chrome"),
        ("Settings", "com.android.settings"),
    ],
)
def test_open_app_resolves_known_aliases(bridge, app, expected):
    _, device, base_url = bridge

    status, body = _invoke(base_url, "open_app", {"app": app})

    assert status == 200 and body["is_error"] is False
    payload = json.loads(body["output"])
    assert payload["action_output"] is not None
    output = json.loads(payload["action_output"])
    assert output["ok"] is True
    assert output["package"] == expected
    assert ("launch_app", expected) in device.calls


def test_open_app_accepts_the_name_alias(bridge):
    _, device, base_url = bridge

    status, body = _invoke(base_url, "open_app", {"name": "Settings"})

    assert status == 200 and body["is_error"] is False
    assert ("launch_app", "com.android.settings") in device.calls


def test_open_app_defers_an_unknown_app_instead_of_guessing(bridge):
    _, device, base_url = bridge

    status, body = _invoke(base_url, "open_app", {"app": "Snapchat"})

    assert status == 200 and body["is_error"] is False
    payload = json.loads(body["output"])
    assert payload["ok"] is False
    assert "suggestion" in payload
    assert not any(call[0] == "launch_app" for call in device.calls)


def test_open_app_requires_a_name(bridge):
    _, _, base_url = bridge

    status, body = _invoke(base_url, "open_app", {"app": "   "})

    assert status == 200 and body["is_error"] is True


def test_open_app_rejects_unknown_arguments(bridge):
    _, _, base_url = bridge

    status, body = _invoke(base_url, "open_app", {"app": "Maps", "force_stop": True})

    assert status == 200 and body["is_error"] is True


def test_resolve_app_package_ignores_uninstalled_mappings():
    installed = {"com.example.notes"}

    assert _resolve_app_package("Maps", installed) == ""
    assert _resolve_app_package("notes", installed) == "com.example.notes"


def test_resolve_app_package_matches_a_unique_package_suffix():
    installed = {"com.vendor.scanner", "com.other.scanner", "com.vendor.radio"}

    # Two packages share the suffix, so the name is ambiguous and is not guessed.
    assert _resolve_app_package("scanner", installed) == ""
    assert _resolve_app_package("radio", installed) == "com.vendor.radio"


def test_resolve_app_package_without_an_installed_list_uses_the_table():
    assert _resolve_app_package("Messages", set()) == "com.google.android.apps.messaging"