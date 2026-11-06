"""Deterministic per-task device preparation for the ADB Android bridge.

Benchmark tasks often need a starting state, such as a page open in Chrome or a
saved contact on screen. Asking the Agent to build that state costs a full agent
run before every measured attempt and mixes setup failures into the benchmark.
These recipes build the same states directly with intents and content
providers, then confirm the expected UI text is on screen before returning.

A recipe raises PrepareError when it cannot reach or confirm its state; the
runner then falls back to the task's agent setup prompt.
"""

from __future__ import annotations

import html
import re
import time
import urllib.parse
from typing import Any, Callable

from .adb import ADBCommandError


CHROME_PACKAGE = "com.android.chrome"
SETTINGS_PACKAGE = "com.android.settings"
# Pixel images serve Settings search from a separate package.
SETTINGS_SEARCH_PACKAGES = ("com.google.android.settings.intelligence",)
SETTINGS_PAGE_ACTIONS = {
    "wifi": "android.settings.WIFI_SETTINGS",
    "device_info": "android.settings.DEVICE_INFO_SETTINGS",
}
MAPS_PACKAGE = "com.google.android.apps.maps"
MESSAGES_PACKAGE = "com.google.android.apps.messaging"
CONTACTS_PACKAGE = "com.google.android.contacts"
PHOTOS_PACKAGE = "com.google.android.apps.photos"
FILES_PACKAGE = "com.google.android.documentsui"
# Chrome reuses the tab it opened for an application id, so one id per logical
# tab gives same-tab navigation (history kept) or separate tabs on demand.
CHROME_APPLICATION_ID_EXTRA = "com.android.browser.application_id"
CONTACTS_RAW_URI = "content://com.android.contacts/raw_contacts"
CONTACTS_DATA_URI = "content://com.android.contacts/data"
MEDIA_IMAGES_URI = "content://media/external/images/media"
DEFAULT_WAIT_SEC = 20.0
POLL_INTERVAL_SEC = 0.8
# A matched label can precede the rest of the screen; let it finish drawing
# before the runner takes the pre-task screenshot.
POST_MATCH_SETTLE_SEC = 1.5


class PrepareError(RuntimeError):
    """A recipe could not reach or confirm its target state."""


def run_recipe(device: Any, recipe: str, params: dict[str, Any]) -> dict[str, Any]:
    handler = RECIPES.get(recipe)
    if handler is None:
        raise PrepareError(f"unknown prepare recipe: {recipe!r}")
    if not isinstance(params, dict):
        raise PrepareError("prepare params must be an object")
    try:
        details = handler(device, params)
    except ADBCommandError as exc:
        raise PrepareError(f"{recipe}: {exc}") from exc
    return {"recipe": recipe, **(details or {})}


# ---- UI confirmation ------------------------------------------------------

def visible_texts(xml: str) -> list[str]:
    texts = []
    for match in re.finditer(r'\b(?:text|content-desc)="([^"]*)"', xml):
        value = html.unescape(match.group(1)).strip()
        if value:
            texts.append(value)
    return texts


def wait_for_any_text(
    device: Any,
    candidates: list[str],
    *,
    timeout_sec: float = DEFAULT_WAIT_SEC,
    sleep: Callable[[float], None] | None = None,
    clock: Callable[[], float] | None = None,
) -> str:
    """Wait until any candidate is visible (case-insensitive substring)."""
    sleep = sleep or time.sleep
    clock = clock or time.monotonic
    wanted = [candidate.casefold() for candidate in candidates if candidate]
    if not wanted:
        raise PrepareError("no confirmation text given")
    deadline = clock() + timeout_sec
    last_error = ""
    while True:
        try:
            texts = [text.casefold() for text in visible_texts(device.dump_window_xml())]
        except ADBCommandError as exc:
            texts = []
            last_error = str(exc)
        for original, needle in zip(candidates, wanted):
            if any(needle in text for text in texts):
                sleep(POST_MATCH_SETTLE_SEC)
                return original
        if clock() >= deadline:
            detail = f"; last dump error: {last_error}" if last_error else ""
            raise PrepareError(f"none of {candidates!r} became visible within {timeout_sec:.0f}s{detail}")
        sleep(POLL_INTERVAL_SEC)


def _texts_param(params: dict[str, Any], key: str, default: list[str]) -> list[str]:
    value = params.get(key, default)
    if isinstance(value, str):
        value = [value]
    if not isinstance(value, list) or not value or not all(isinstance(item, str) and item.strip() for item in value):
        raise PrepareError(f"{key} must be a non-empty string or list of strings")
    return value


def _string_param(params: dict[str, Any], key: str) -> str:
    value = params.get(key)
    if not isinstance(value, str) or not value.strip():
        raise PrepareError(f"{key} is required")
    return value.strip()


def _sql_string(value: str) -> str:
    """Quote a value as a SQL string literal for a content provider where clause.

    Shell quoting protects only the adb shell layer; the provider still parses
    the clause as SQL, so an embedded quote would otherwise widen a delete.
    """
    return "'" + value.replace("'", "''") + "'"


def _timeout_param(params: dict[str, Any]) -> float:
    value = params.get("wait_sec", DEFAULT_WAIT_SEC)
    if isinstance(value, bool) or not isinstance(value, (int, float)) or not 0 < value <= 120:
        raise PrepareError("wait_sec must be a number in (0, 120]")
    return float(value)


# ---- recipes --------------------------------------------------------------

def prepare_chrome_pages(device: Any, params: dict[str, Any]) -> dict[str, Any]:
    """Open URLs in Chrome; pages that share a ``tab`` label share one tab.

    Chrome is stopped first so no menu, Find bar or tab switcher survives
    from an earlier task. Its tab list is restored on relaunch.
    """
    pages = params.get("pages")
    if not isinstance(pages, list) or not pages:
        raise PrepareError("pages must be a non-empty list")
    timeout = _timeout_param(params)
    device.force_stop(CHROME_PACKAGE)
    opened = []
    for index, page in enumerate(pages):
        if not isinstance(page, dict):
            raise PrepareError(f"pages[{index}] must be an object")
        url = _string_param(page, "url")
        tab = str(page.get("tab") or "main")
        # A heavy page (IANA's root zone list is >500 KB) can take longer to
        # render than the recipe default, so a page may set its own wait.
        page_timeout = _timeout_param(page) if "wait_sec" in page else timeout
        device.start_intent(
            action="android.intent.action.VIEW",
            data=url,
            package=CHROME_PACKAGE,
            extras={CHROME_APPLICATION_ID_EXTRA: f"aico.benchmark.tab.{tab}"},
        )
        matched = wait_for_any_text(device, _texts_param(page, "wait_for_text", [url]), timeout_sec=page_timeout)
        opened.append({"url": url, "tab": tab, "confirmed_by": matched})
    return {"pages": opened}


def prepare_maps_directions(device: Any, params: dict[str, Any]) -> dict[str, Any]:
    """Open a Google Maps route preview (never turn-by-turn navigation)."""
    origin = _string_param(params, "origin")
    destination = _string_param(params, "destination")
    mode = str(params.get("travelmode") or "walking")
    if mode not in {"walking", "driving", "transit", "bicycling"}:
        raise PrepareError(f"unsupported travelmode {mode!r}")
    query = urllib.parse.urlencode(
        {"api": "1", "origin": origin, "destination": destination, "travelmode": mode}
    )
    device.force_stop(MAPS_PACKAGE)
    device.start_intent(
        action="android.intent.action.VIEW",
        data=f"https://www.google.com/maps/dir/?{query}",
        package=MAPS_PACKAGE,
    )
    matched = wait_for_any_text(
        device,
        _texts_param(params, "wait_for_text", [destination]),
        timeout_sec=_timeout_param(params),
    )
    return {"origin": origin, "destination": destination, "travelmode": mode, "confirmed_by": matched}


def prepare_messages_draft(device: Any, params: dict[str, Any]) -> dict[str, Any]:
    """Open an unsent SMS composer for a recipient with prefilled text."""
    recipient = _string_param(params, "recipient")
    body = _string_param(params, "body")
    device.force_stop(MESSAGES_PACKAGE)
    device.start_intent(
        action="android.intent.action.SENDTO",
        data=f"smsto:{recipient}",
        package=MESSAGES_PACKAGE,
        extras={"sms_body": body},
    )
    matched = wait_for_any_text(device, [body], timeout_sec=_timeout_param(params))
    return {"recipient": recipient, "confirmed_by": matched}


def prepare_contact_detail(device: Any, params: dict[str, Any]) -> dict[str, Any]:
    """Replace any same-named device contact with one fresh contact, then show it."""
    name = _string_param(params, "name")
    phone = _string_param(params, "phone")
    given = str(params.get("given_name") or name.split(" ", 1)[0])
    family = str(params.get("family_name") or (name.split(" ", 1)[1] if " " in name else ""))
    # The sync-adapter flag deletes rows outright instead of marking them for a
    # sync that a local, account-less contact never gets.
    device.content_delete(
        f"{CONTACTS_RAW_URI}?caller_is_syncadapter=true",
        where=f"display_name={_sql_string(name)}",
    )
    before = {row.get("_id") for row in device.content_query(CONTACTS_RAW_URI, projection="_id")}
    device.content_insert(CONTACTS_RAW_URI, [("account_type", "n", ""), ("account_name", "n", "")])
    created = [
        row for row in device.content_query(CONTACTS_RAW_URI, projection="_id:contact_id")
        if row.get("_id") not in before
    ]
    if len(created) != 1 or not created[0].get("_id", "").isdigit():
        raise PrepareError(f"could not identify the inserted raw contact: {created!r}")
    raw_id = created[0]["_id"]
    device.content_insert(CONTACTS_DATA_URI, [
        ("raw_contact_id", "i", raw_id),
        ("mimetype", "s", "vnd.android.cursor.item/name"),
        ("data1", "s", name),
        ("data2", "s", given),
        ("data3", "s", family),
    ])
    device.content_insert(CONTACTS_DATA_URI, [
        ("raw_contact_id", "i", raw_id),
        ("mimetype", "s", "vnd.android.cursor.item/phone_v2"),
        ("data1", "s", phone),
        ("data2", "i", "2"),
    ])
    linked = device.content_query(CONTACTS_RAW_URI, projection="_id:contact_id", where=f"_id={raw_id}")
    contact_id = linked[0].get("contact_id", "") if linked else ""
    if not contact_id.isdigit():
        raise PrepareError(f"raw contact {raw_id} has no aggregate contact")
    device.force_stop(CONTACTS_PACKAGE)
    device.start_intent(
        action="android.intent.action.VIEW",
        data=f"content://com.android.contacts/contacts/{contact_id}",
        package=CONTACTS_PACKAGE,
    )
    matched = wait_for_any_text(device, [name], timeout_sec=_timeout_param(params))
    return {"raw_contact_id": raw_id, "contact_id": contact_id, "confirmed_by": matched}


def prepare_camera_photo(device: Any, params: dict[str, Any]) -> dict[str, Any]:
    """Capture one photo, then leave Home, the photo in Photos, or its Files folder."""
    then = str(params.get("then") or "home")
    if then not in {"home", "photos", "files"}:
        raise PrepareError(f"unsupported then={then!r}")
    timeout = _timeout_param(params)
    previous = device.newest_media_image()
    device.capture_photo()
    photo = _wait_for_new_photo(device, previous, timeout_sec=timeout)
    details: dict[str, Any] = {"photo": photo.get("_display_name", ""), "folder": photo.get("relative_path", "")}
    if then == "home":
        # Sending HOME is not evidence that the transition has finished. Do not
        # hand the task a camera screenshot while claiming a prepared launcher.
        device.reset_home()
        details["confirmed_by"] = wait_for_any_text(
            device, _texts_param(params, "wait_for_text", ["Phone", "Chrome", "Messages", "Gmail"]),
            timeout_sec=timeout,
        )
    elif then == "photos":
        device.keyevent("KEYCODE_HOME")
        device.start_intent(
            action="android.intent.action.VIEW",
            data=f"{MEDIA_IMAGES_URI}/{photo['_id']}",
            package=PHOTOS_PACKAGE,
        )
        details["confirmed_by"] = wait_for_any_text(
            device, _texts_param(params, "wait_for_text", ["Share", "Edit"]), timeout_sec=timeout
        )
    elif then == "files":
        device.keyevent("KEYCODE_HOME")
        folder = str(photo.get("relative_path") or "").strip("/")
        if not folder:
            raise PrepareError("the new photo has no relative_path")
        document = urllib.parse.quote(f"primary:{folder}", safe="")
        # DocumentsUI only opens a folder when the intent carries the
        # directory MIME type; without it Files stays on its default root.
        device.start_intent(
            action="android.intent.action.VIEW",
            data=f"content://com.android.externalstorage.documents/document/{document}",
            package=FILES_PACKAGE,
            mime_type="vnd.android.document/directory",
        )
        name = str(photo.get("_display_name", ""))
        stem = name.rsplit(".", 1)[0]
        details["confirmed_by"] = wait_for_any_text(
            device, [candidate for candidate in (name, stem) if candidate], timeout_sec=timeout
        )
    return details


def _start_settings_main(device: Any, params: dict[str, Any]) -> str:
    """Relaunch Settings on its main list, scrolled to the top.

    Settings is stopped first so no subpage, scroll position or search query
    survives from an earlier task.
    """
    device.force_stop(SETTINGS_PACKAGE)
    device.start_intent(action="android.settings.SETTINGS", package=SETTINGS_PACKAGE)
    return wait_for_any_text(
        device,
        _texts_param(params, "main_text", ["Network & internet", "Search settings"]),
        timeout_sec=_timeout_param(params),
    )


def prepare_settings_page(device: Any, params: dict[str, Any]) -> dict[str, Any]:
    """Show main Settings, a subpage opened from it, or a panel over it.

    A subpage is started inside the Settings task after the main list, so Back
    returns to the main list as it does after navigating there by hand.
    """
    page = str(params.get("page") or "main")
    if page != "main" and page not in SETTINGS_PAGE_ACTIONS:
        raise PrepareError(f"unsupported settings page {page!r}")
    panel = params.get("panel")
    if panel not in (None, "notifications"):
        raise PrepareError(f"unsupported panel {panel!r}")
    if panel and page != "main":
        raise PrepareError("a panel can open only over the main Settings page")
    confirmation = _texts_param(params, "wait_for_text", []) if page != "main" or panel else None
    details: dict[str, Any] = {"page": page, "main_confirmed_by": _start_settings_main(device, params)}
    if page != "main":
        device.start_intent(action=SETTINGS_PAGE_ACTIONS[page], package=SETTINGS_PACKAGE)
        details["confirmed_by"] = wait_for_any_text(
            device, confirmation, timeout_sec=_timeout_param(params)
        )
    if panel:
        device.expand_notifications()
        details["panel"] = panel
        details["confirmed_by"] = wait_for_any_text(
            device, confirmation, timeout_sec=_timeout_param(params)
        )
    return details


def prepare_settings_search(device: Any, params: dict[str, Any]) -> dict[str, Any]:
    """Open Settings search with an unsubmitted ASCII query in the field.

    The query is typed only once a focused editable field is on screen, so it
    cannot land in another app or in the main list's search entry point.
    """
    query = _string_param(params, "query")
    for package in SETTINGS_SEARCH_PACKAGES:
        device.force_stop(package)
    _start_settings_main(device, params)
    device.start_intent(action="android.settings.APP_SEARCH_SETTINGS")
    _wait_for_focused_edit_text(device, timeout_sec=_timeout_param(params))
    device.input_text(query)
    matched = wait_for_any_text(device, [query], timeout_sec=_timeout_param(params))
    return {"query": query, "confirmed_by": matched}


FOCUSED_EDIT_TEXT = re.compile(
    r'<node\b(?=[^>]*\bclass="android\.widget\.EditText")(?=[^>]*\bfocused="true")[^>]*>'
)


def _wait_for_focused_edit_text(device: Any, *, timeout_sec: float) -> None:
    deadline = time.monotonic() + timeout_sec
    while True:
        try:
            if FOCUSED_EDIT_TEXT.search(device.dump_window_xml()):
                return
        except ADBCommandError:
            pass
        if time.monotonic() >= deadline:
            raise PrepareError(f"no focused text field appeared within {timeout_sec:.0f}s")
        time.sleep(POLL_INTERVAL_SEC)


def prepare_home(device: Any, params: dict[str, Any]) -> dict[str, Any]:
    """Show the launcher Home screen with no panel or keyboard open."""
    confirmation = _texts_param(params, "wait_for_text", [])
    device.reset_home()
    matched = wait_for_any_text(device, confirmation, timeout_sec=_timeout_param(params))
    return {"confirmed_by": matched}


def _wait_for_new_photo(
    device: Any,
    previous: dict[str, str] | None,
    *,
    timeout_sec: float,
    sleep: Callable[[float], None] | None = None,
    clock: Callable[[], float] | None = None,
) -> dict[str, str]:
    sleep = sleep or time.sleep
    clock = clock or time.monotonic
    previous_id = (previous or {}).get("_id")
    deadline = clock() + timeout_sec
    while True:
        newest = device.newest_media_image()
        if newest and newest.get("_id") and newest.get("_id") != previous_id:
            return newest
        if clock() >= deadline:
            raise PrepareError("the camera did not save a new photo")
        sleep(POLL_INTERVAL_SEC)


RECIPES: dict[str, Callable[[Any, dict[str, Any]], dict[str, Any]]] = {
    "chrome_pages": prepare_chrome_pages,
    "maps_directions": prepare_maps_directions,
    "messages_draft": prepare_messages_draft,
    "contact_detail": prepare_contact_detail,
    "camera_photo": prepare_camera_photo,
    "settings_page": prepare_settings_page,
    "settings_search": prepare_settings_search,
    "home": prepare_home,
}
