"""Tests for deterministic ADB device preparation and its runner fallback."""

from __future__ import annotations

import pytest

from adbandroid.bridge import prepare
from adbandroid.bridge.adb import ADBAndroidDevice, ADBCommandError
from adbandroid.bridge.prepare import PrepareError, run_recipe
from runner import reset
from runner.reset import ResetError, per_task_setup
from tests.test_adbandroid_bridge import _request, bridge  # noqa: F401


class RecipeDevice:
    """Device double whose UI shows the texts a recipe waits for."""

    def __init__(self, *, screen_texts=(), photos=()):
        self.calls: list[tuple] = []
        self.screen_texts = list(screen_texts)
        self.photos = list(photos)
        self.raw_contacts = [{"_id": "1", "contact_id": "1"}]

    def dump_window_xml(self):
        nodes = "".join(f'<node text="{text}" />' for text in self.screen_texts)
        return f"<?xml version='1.0'?><hierarchy>{nodes}</hierarchy>"

    def force_stop(self, package):
        self.calls.append(("force_stop", package))

    def start_intent(self, **kwargs):
        self.calls.append(("start_intent", kwargs))

    def keyevent(self, key):
        self.calls.append(("keyevent", key))

    def expand_notifications(self):
        self.calls.append(("expand_notifications",))

    def input_text(self, text):
        self.calls.append(("input_text", text))

    def reset_home(self):
        self.calls.append(("reset_home",))

    def content_delete(self, uri, *, where):
        self.calls.append(("content_delete", uri, where))

    def content_query(self, uri, *, projection="", where=""):
        if where:
            raw_id = where.split("=", 1)[1]
            return [row for row in self.raw_contacts if row["_id"] == raw_id]
        return list(self.raw_contacts)

    def content_insert(self, uri, bindings):
        self.calls.append(("content_insert", uri, bindings))
        if uri.endswith("raw_contacts"):
            new_id = str(len(self.raw_contacts) + 1)
            self.raw_contacts.append({"_id": new_id, "contact_id": f"9{new_id}"})

    def capture_photo(self):
        self.calls.append(("capture_photo",))
        self.photos.append({"_id": "77", "_display_name": "PXL_new.jpg", "relative_path": "Pictures/", "date_added": "200"})

    def newest_media_image(self):
        return self.photos[-1] if self.photos else None


@pytest.fixture(autouse=True)
def no_sleep(monkeypatch):
    monkeypatch.setattr(prepare.time, "sleep", lambda _seconds: None)


def test_chrome_pages_share_a_tab_by_label_and_confirm_each_page():
    device = RecipeDevice(screen_texts=["IANA-managed Reserved Domains", "Root Zone Database"])

    result = run_recipe(device, "chrome_pages", {"pages": [
        {"url": "https://a.test/1", "tab": "history", "wait_for_text": ["Reserved Domains"]},
        {"url": "https://a.test/2", "tab": "history", "wait_for_text": "Root Zone"},
    ]})

    intents = [call[1] for call in device.calls if call[0] == "start_intent"]
    assert device.calls[0] == ("force_stop", prepare.CHROME_PACKAGE)
    assert [intent["data"] for intent in intents] == ["https://a.test/1", "https://a.test/2"]
    assert {intent["extras"][prepare.CHROME_APPLICATION_ID_EXTRA] for intent in intents} == {"aico.benchmark.tab.history"}
    assert [page["confirmed_by"] for page in result["pages"]] == ["Reserved Domains", "Root Zone"]


def test_recipe_fails_when_the_expected_text_never_appears(monkeypatch):
    clock = iter(range(0, 1000, 10))
    monkeypatch.setattr(prepare.time, "monotonic", lambda: next(clock))
    device = RecipeDevice(screen_texts=["Chrome error page"])

    with pytest.raises(PrepareError, match="Reserved Domains"):
        run_recipe(device, "chrome_pages", {"pages": [{"url": "https://a.test", "wait_for_text": ["Reserved Domains"]}]})


def test_maps_directions_opens_a_route_preview_url():
    device = RecipeDevice(screen_texts=["Eiffel Tower"])

    run_recipe(device, "maps_directions", {"origin": "Louvre Museum, Paris", "destination": "Eiffel Tower, Paris", "wait_for_text": ["Eiffel Tower"]})

    intent = next(call[1] for call in device.calls if call[0] == "start_intent")
    assert intent["package"] == prepare.MAPS_PACKAGE
    assert "travelmode=walking" in intent["data"] and "origin=Louvre+Museum%2C+Paris" in intent["data"]
    assert "navigate" not in intent["data"]


def test_messages_draft_prefills_without_sending():
    device = RecipeDevice(screen_texts=["Aiden benchmark draft 2026"])

    run_recipe(device, "messages_draft", {"recipient": "5550104040", "body": "Aiden benchmark draft 2026"})

    intent = next(call[1] for call in device.calls if call[0] == "start_intent")
    assert intent["action"] == "android.intent.action.SENDTO"
    assert intent["data"] == "smsto:5550104040"
    assert intent["extras"] == {"sms_body": "Aiden benchmark draft 2026"}


def test_contact_detail_replaces_the_contact_and_opens_it():
    device = RecipeDevice(screen_texts=["Aiden Benchmark Contact"])

    result = run_recipe(device, "contact_detail", {
        "name": "Aiden Benchmark Contact", "given_name": "Aiden", "family_name": "Benchmark Contact", "phone": "5550102026",
    })

    kinds = [call[0] for call in device.calls]
    assert kinds.index("content_delete") < kinds.index("content_insert")
    delete = next(call for call in device.calls if call[0] == "content_delete")
    assert "caller_is_syncadapter=true" in delete[1] and "Aiden Benchmark Contact" in delete[2]
    data_rows = [call[2] for call in device.calls if call[0] == "content_insert" and call[1].endswith("/data")]
    assert ("data1", "s", "5550102026") in data_rows[1]
    assert all(("raw_contact_id", "i", result["raw_contact_id"]) in row for row in data_rows)
    intent = next(call[1] for call in device.calls if call[0] == "start_intent")
    assert intent["data"].endswith(f"/contacts/{result['contact_id']}")


def test_contact_detail_delete_cannot_be_widened_by_a_quote_in_the_name():
    device = RecipeDevice(screen_texts=["x' OR '1'='1"])

    run_recipe(device, "contact_detail", {"name": "x' OR '1'='1", "phone": "5550102026"})

    delete = next(call for call in device.calls if call[0] == "content_delete")
    assert delete[2] == "display_name='x'' OR ''1''=''1'"


@pytest.mark.parametrize("then, expect_intent", [("home", False), ("photos", True), ("files", True)])
def test_camera_photo_waits_for_a_new_image(then, expect_intent):
    device = RecipeDevice(
        screen_texts=["Share", "PXL_new.jpg", "Phone"],
        photos=[{"_id": "5", "_display_name": "old.jpg", "relative_path": "Pictures/", "date_added": "100"}],
    )

    result = run_recipe(device, "camera_photo", {"then": then})

    assert result["photo"] == "PXL_new.jpg"
    assert (("reset_home",) if then == "home" else ("keyevent", "KEYCODE_HOME")) in device.calls
    if then == "home":
        assert result["confirmed_by"] == "Phone"
    assert any(call[0] == "start_intent" for call in device.calls) is expect_intent


def test_camera_recipe_rejects_home_action_without_a_confirmed_home_screen(monkeypatch):
    clock = iter(range(0, 1000, 10))
    monkeypatch.setattr(prepare.time, "monotonic", lambda: next(clock))
    device = RecipeDevice(screen_texts=["Camera", "Shutter"])
    # Home input can complete before the foreground camera has disappeared.
    with pytest.raises(PrepareError, match="became visible"):
        run_recipe(device, "camera_photo", {"then": "home", "wait_for_text": ["Search apps"]})


def test_camera_recipe_waits_for_launcher_after_home(monkeypatch):
    clock = iter(range(100))
    monkeypatch.setattr(prepare.time, "monotonic", lambda: next(clock))

    class TransitioningDevice(RecipeDevice):
        def __init__(self):
            super().__init__()
            self.screens = iter(['<node text="Camera"/>', '<node text="Phone"/>'])
            self.dumps = 0

        def dump_window_xml(self):
            self.dumps += 1
            return next(self.screens)

    device = TransitioningDevice()
    result = run_recipe(device, "camera_photo", {"then": "home"})
    assert device.dumps == 2
    assert result["confirmed_by"] == "Phone"


def test_unknown_recipe_is_rejected():
    with pytest.raises(PrepareError, match="unknown prepare recipe"):
        run_recipe(RecipeDevice(), "factory_reset", {})


@pytest.mark.parametrize("params, screen, intents, expands", [
    ({"page": "main"}, ["Network & internet"], ["android.settings.SETTINGS"], False),
    ({"page": "device_info", "wait_for_text": ["Model"]}, ["Network & internet", "Model"],
     ["android.settings.SETTINGS", "android.settings.DEVICE_INFO_SETTINGS"], False),
    ({"panel": "notifications", "wait_for_text": ["Manage"]}, ["Network & internet", "Manage"],
     ["android.settings.SETTINGS"], True),
])
def test_settings_page_restarts_settings_on_its_main_list(params, screen, intents, expands):
    device = RecipeDevice(screen_texts=screen)

    run_recipe(device, "settings_page", params)

    # A stopped Settings reopens at the top of the main list; a subpage is opened
    # from there, so Back returns to the main list.
    assert device.calls[0] == ("force_stop", prepare.SETTINGS_PACKAGE)
    assert [call[1]["action"] for call in device.calls if call[0] == "start_intent"] == intents
    assert (("expand_notifications",) in device.calls) is expands


@pytest.mark.parametrize("params", [{"page": "bluetooth"}, {"page": "wifi", "panel": "notifications"}])
def test_settings_page_rejects_states_it_cannot_build(params):
    with pytest.raises(PrepareError):
        run_recipe(RecipeDevice(screen_texts=["Network & internet"]), "settings_page", params)


@pytest.mark.parametrize("recipe,params", [
    ("settings_page", {"page": "device_info"}),
    ("settings_page", {"panel": "notifications"}),
    ("home", {}),
])
@pytest.mark.parametrize("value", [None, [], "", [" "]])
def test_required_confirmation_is_validated_before_device_actions(recipe, params, value):
    device = RecipeDevice()
    if value is not None:
        params = {**params, "wait_for_text": value}
    with pytest.raises(PrepareError, match="wait_for_text"):
        run_recipe(device, recipe, params)
    assert device.calls == []


def test_settings_search_types_only_into_a_focused_field():
    focused = '<node class="android.widget.EditText" focused="true" text="" />'

    class SearchDevice(RecipeDevice):
        def dump_window_xml(self):
            typed = [call[1] for call in self.calls if call[0] == "input_text"]
            nodes = '<node text="Network &amp; internet" />' + focused + "".join(f'<node text="{t}" />' for t in typed)
            return f"<?xml version='1.0'?><hierarchy>{nodes}</hierarchy>"

    device = SearchDevice()
    result = run_recipe(device, "settings_search", {"query": "alpha beta"})

    assert result["confirmed_by"] == "alpha beta"
    kinds = [call[0] for call in device.calls]
    assert kinds.index("start_intent") < kinds.index("input_text")


def test_settings_search_does_not_type_without_a_focused_field(monkeypatch):
    clock = iter(range(0, 1000, 10))
    monkeypatch.setattr(prepare.time, "monotonic", lambda: next(clock))
    device = RecipeDevice(screen_texts=["Network & internet"])

    with pytest.raises(PrepareError, match="no focused text field"):
        run_recipe(device, "settings_search", {"query": "alpha beta"})
    assert not any(call[0] == "input_text" for call in device.calls)


def test_a_page_can_extend_its_own_wait(monkeypatch):
    waits = []
    monkeypatch.setattr(prepare, "wait_for_any_text", lambda device, texts, timeout_sec: waits.append(timeout_sec) or texts[0])

    run_recipe(RecipeDevice(), "chrome_pages", {"pages": [
        {"url": "https://a.test/light", "wait_for_text": ["Light"]},
        {"url": "https://a.test/heavy", "wait_for_text": ["Heavy"], "wait_sec": 40},
    ]})

    assert waits == [prepare.DEFAULT_WAIT_SEC, 40]


def test_files_opens_the_photo_folder_as_a_directory_and_accepts_the_stem():
    device = RecipeDevice(
        screen_texts=["PXL_new"],
        photos=[{"_id": "5", "_display_name": "old.jpg", "relative_path": "Pictures/", "date_added": "100"}],
    )

    result = run_recipe(device, "camera_photo", {"then": "files"})

    intent = next(call[1] for call in device.calls if call[0] == "start_intent")
    assert intent["mime_type"] == "vnd.android.document/directory"
    assert intent["data"].endswith("/document/primary%3APictures")
    assert result["confirmed_by"] == "PXL_new"


def test_start_intent_passes_the_mime_type(monkeypatch):
    device = ADBAndroidDevice("serial-1")
    calls = []
    monkeypatch.setattr(device, "_run_text", lambda args, **kw: calls.append(args) or "Status: ok")

    device.start_intent(action="android.intent.action.VIEW", data="content://x/y", mime_type="vnd.android.document/directory")

    args = calls[0]
    assert args[args.index("-t") + 1] == "vnd.android.document/directory"
    assert args[args.index("-d") + 1] == "'content://x/y'"


def test_adb_errors_become_prepare_errors():
    class Broken(RecipeDevice):
        def force_stop(self, package):
            raise ADBCommandError("device offline")

    with pytest.raises(PrepareError, match="device offline"):
        run_recipe(Broken(), "messages_draft", {"recipient": "1", "body": "x"})


def test_content_query_parses_rows(monkeypatch):
    device = ADBAndroidDevice("serial-1")
    monkeypatch.setattr(device, "_run_text", lambda args, **kw: (
        "Row: 0 _id=12, _display_name=PXL 1.jpg, relative_path=Pictures/\n"
        "Row: 1 _id=13, _display_name=b.jpg, relative_path=DCIM/Camera/\n"
    ))

    assert device.content_query("content://media/external/images/media") == [
        {"_id": "12", "_display_name": "PXL 1.jpg", "relative_path": "Pictures/"},
        {"_id": "13", "_display_name": "b.jpg", "relative_path": "DCIM/Camera/"},
    ]


# ---- bridge endpoint ---------------------------------------------------------

def test_prepare_endpoint_requires_setup_first(bridge):
    _, _, base_url = bridge
    status, body = _request(base_url, "/api/prepare", method="POST", payload={"recipe": "messages_draft"})
    assert status == 409
    assert body["error"]["code"] == "no_active_episode"


def test_prepare_endpoint_reports_recipe_failures_as_422(bridge, monkeypatch):
    _, _, base_url = bridge
    _request(base_url, "/api/setup", method="POST", task_id="suite:prep")

    def fail(device, recipe, params):
        raise PrepareError("state not confirmed")

    monkeypatch.setattr("adbandroid.bridge.server.run_recipe", fail)
    status, body = _request(
        base_url, "/api/prepare", method="POST", task_id="suite:prep",
        payload={"recipe": "messages_draft", "params": {}},
    )
    assert status == 422
    assert body["error"]["code"] == "prepare_failed"


def test_prepare_endpoint_runs_the_recipe(bridge, monkeypatch):
    _, device, base_url = bridge
    _request(base_url, "/api/setup", method="POST", task_id="suite:prep")
    seen = []

    def recipe(dev, name, params):
        seen.append((dev, name, params))
        return {"recipe": name}

    monkeypatch.setattr("adbandroid.bridge.server.run_recipe", recipe)
    status, body = _request(
        base_url, "/api/prepare", method="POST", task_id="suite:prep",
        payload={"recipe": "contact_detail", "params": {"name": "x"}},
    )
    assert status == 200 and body["data"]["recipe"] == "contact_detail"
    assert seen == [(device, "contact_detail", {"name": "x"})]


# ---- runner fallback ---------------------------------------------------------

class ChatClient:
    def __init__(self):
        self.prompts = []

    def chat(self, prompt, timeout_sec=None):
        self.prompts.append(prompt)
        return type("Chat", (), {"response": "done"})()

    def clear_history(self):
        pass


SETUP = {
    "type": "agent_prompt",
    "prompt": "Open the contact.",
    "device_prepare": {"recipe": "contact_detail", "params": {"name": "x", "phone": "1"}},
}


def test_device_prepare_replaces_the_agent_prompt(monkeypatch):
    calls = []
    monkeypatch.setattr(reset, "call_environment_prepare", lambda url, **kw: calls.append((url, kw)) or {"ok": True, "data": {"recipe": "contact_detail"}})
    client = ChatClient()

    result = per_task_setup(client, SETUP, environment_url="http://bridge", benchmark_task_id="suite:task")

    assert client.prompts == []
    assert result == {"device_prepare": {"recipe": "contact_detail"}}
    assert calls[0][0] == "http://bridge" and calls[0][1]["task_id"] == "suite:task"


def test_device_prepare_failure_falls_back_to_the_agent_prompt(monkeypatch):
    def fail(url, **kw):
        raise ResetError("prepare endpoint failed HTTP 422")

    monkeypatch.setattr(reset, "call_environment_prepare", fail)
    client = ChatClient()

    result = per_task_setup(client, SETUP, environment_url="http://bridge", benchmark_task_id="suite:task")

    assert client.prompts == ["Open the contact."]
    # The fallback is recorded, so results can tell an Agent-built start state
    # from a recipe-built one.
    assert result == {"device_prepare_fallback": {"recipe": "contact_detail", "error": "prepare endpoint failed HTTP 422"}}


def test_device_prepare_without_an_environment_uses_the_agent_prompt(monkeypatch):
    monkeypatch.setattr(reset, "call_environment_prepare", lambda *a, **kw: pytest.fail("no environment"))
    client = ChatClient()

    result = per_task_setup(client, SETUP)

    assert client.prompts == ["Open the contact."]
    assert result == {"device_prepare_fallback": {"recipe": "contact_detail", "error": "no environment_url"}}


def test_device_prepare_rejects_a_setup_built_at_runtime(monkeypatch):
    """Suites are validated on load; a hand-built setup gets the same checks."""
    monkeypatch.setattr(reset, "call_environment_prepare", lambda *a, **kw: pytest.fail("no prepare call"))
    client = ChatClient()

    with pytest.raises(ResetError, match="cannot be combined with expected_response"):
        per_task_setup(
            client,
            {**SETUP, "expected_response": "done"},
            environment_url="http://bridge",
        )

    assert client.prompts == []


@pytest.mark.parametrize("suite_file", ["adb_android_builtin_apps.json", "adb_android_basic.json", "adb_android_text_editing.json"])
def test_every_recipe_keeps_a_prompt_fallback(suite_file):
    from runner.suite import load_suite
    from pathlib import Path

    suite = load_suite(Path(__file__).resolve().parents[1] / "suites" / suite_file)
    prepared = [task for task in suite.tasks if isinstance(task.setup, dict) and task.setup.get("device_prepare")]
    assert prepared
    for task in prepared:
        assert task.setup["type"] == "agent_prompt" and task.setup["prompt"].strip()
        assert task.setup["device_prepare"]["recipe"] in prepare.RECIPES
