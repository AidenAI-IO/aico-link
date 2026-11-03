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


@pytest.mark.parametrize("then, expect_intent", [("home", False), ("photos", True), ("files", True)])
def test_camera_photo_waits_for_a_new_image(then, expect_intent):
    device = RecipeDevice(
        screen_texts=["Share", "PXL_new.jpg"],
        photos=[{"_id": "5", "_display_name": "old.jpg", "relative_path": "Pictures/", "date_added": "100"}],
    )

    result = run_recipe(device, "camera_photo", {"then": then})

    assert result["photo"] == "PXL_new.jpg"
    assert ("keyevent", "KEYCODE_HOME") in device.calls
    assert any(call[0] == "start_intent" for call in device.calls) is expect_intent


def test_unknown_recipe_is_rejected():
    with pytest.raises(PrepareError, match="unknown prepare recipe"):
        run_recipe(RecipeDevice(), "factory_reset", {})


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
    assert result is None


def test_device_prepare_without_an_environment_uses_the_agent_prompt(monkeypatch):
    monkeypatch.setattr(reset, "call_environment_prepare", lambda *a, **kw: pytest.fail("no environment"))
    client = ChatClient()

    per_task_setup(client, SETUP)

    assert client.prompts == ["Open the contact."]


def test_builtin_apps_suite_keeps_a_prompt_fallback_for_every_recipe():
    from runner.suite import load_suite
    from pathlib import Path

    suite = load_suite(Path(__file__).resolve().parents[1] / "suites" / "adb_android_builtin_apps.json")
    prepared = [task for task in suite.tasks if isinstance(task.setup, dict) and task.setup.get("device_prepare")]
    assert len(prepared) == 12
    for task in prepared:
        assert task.setup["type"] == "agent_prompt" and task.setup["prompt"].strip()
        assert task.setup["device_prepare"]["recipe"] in prepare.RECIPES
