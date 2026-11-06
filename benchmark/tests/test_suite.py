import json
import pytest
from pathlib import Path
from runner.platform import TargetPlatform
from runner.suite import load_suite, SuiteValidationError

FIXTURE = {
    "name": "test_suite",
    "global_reset": {},
    "tasks": [
        {
            "id": "open_settings",
            "category": "single_step",
            "description_for_judge": "Agent should open Settings.",
            "prompt": "请打开系统设置。",
            "rubric": [{"id": "in_settings", "check": "Post-screenshot shows Settings."}],
            "hard_assertions": {"min_tool_calls": 1, "max_tool_calls": 8, "must_complete_within_sec": 90},
        }
    ],
}

def test_load_suite_returns_parsed(tmp_path: Path):
    p = tmp_path / "s.json"
    p.write_text(json.dumps(FIXTURE), encoding="utf-8")
    suite = load_suite(p)
    assert suite.name == "test_suite"
    assert len(suite.tasks) == 1
    assert suite.tasks[0].id == "open_settings"
    assert suite.tasks[0].category == "single_step"
    assert suite.tasks[0].rubric[0].id == "in_settings"


@pytest.mark.parametrize("value", ["mobilegym", None, [""], ["MobileGym"], [1]])
def test_invalid_environment_bridge_allowlist_is_rejected(tmp_path, value):
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["environment_bridge_types"] = value
    path = tmp_path / "suite.json"
    path.write_text(json.dumps(fixture))
    with pytest.raises(SuiteValidationError, match="environment_bridge_types"):
        load_suite(path)


def test_android_bridge_input_text_requires_non_empty_string(tmp_path: Path):
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["android_bridge_input_text"] = "Aiden 输入验证 #42"
    path = tmp_path / "bridge_input.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")
    assert load_suite(path).tasks[0].android_bridge_input_text == "Aiden 输入验证 #42"

    fixture["tasks"][0]["android_bridge_input_text"] = ""
    path.write_text(json.dumps(fixture), encoding="utf-8")
    with pytest.raises(SuiteValidationError, match="android_bridge_input_text"):
        load_suite(path)


@pytest.mark.parametrize("assertions", [
    {}, [], {"expected": []}, {"absent_ids": []}, {"expected_count": None},
    {"expected_count": True}, {"expected_count": -1}, {"expected_count": "1"},
    {"expected_count": 1, "unknown": True}, {"expected": [{"id": 12}]},
])
def test_load_suite_rejects_invalid_memory_assertions(tmp_path: Path, assertions):
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["memory_assertions"] = assertions
    path = tmp_path / "invalid.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")
    with pytest.raises(SuiteValidationError, match="memory_assertions"):
        load_suite(path)


def test_load_suite_parses_environment_assertions(tmp_path: Path):
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["environment_assertions"] = {
        "route.path": "/item/scroll-item-083",
        "apps.scroll_lab.selectedItemId": "scroll-item-083",
    }
    path = tmp_path / "environment_assertions.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    task = load_suite(path).tasks[0]

    assert task.environment_assertions["route.path"] == "/item/scroll-item-083"


def test_load_suite_rejects_duplicate_normalized_assertion_paths(tmp_path: Path):
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["environment_assertions"] = {
        " route.path": "/item/scroll-item-024",
        "route.path": "/item/scroll-item-083",
    }
    path = tmp_path / "duplicates.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")
    with pytest.raises(SuiteValidationError, match="duplicate environment_assertions path"):
        load_suite(path)


@pytest.mark.parametrize("value", [{}, [], {"bad..path": "x"}])
def test_load_suite_rejects_invalid_environment_assertions(tmp_path: Path, value):
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["environment_assertions"] = value
    path = tmp_path / "bad_environment_assertions.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="environment_assertions"):
        load_suite(path)


def test_load_suite_parses_range_environment_assertions(tmp_path: Path):
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["environment_assertions"] = {
        "apps.scroll_lab.firstVisibleOrdinal": {"min": 5, "max": 11},
    }
    path = tmp_path / "range_environment_assertions.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    task = load_suite(path).tasks[0]

    assert task.environment_assertions["apps.scroll_lab.firstVisibleOrdinal"] == {"min": 5, "max": 11}


@pytest.mark.parametrize(
    "spec",
    [
        {"min": "five", "max": 11},
        {"min": 5, "max": "eleven"},
        {"min": 11, "max": 5},
        {"min": True, "max": 11},
    ],
)
def test_load_suite_rejects_invalid_range_environment_assertions(tmp_path: Path, spec):
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["environment_assertions"] = {"apps.scroll_lab.firstVisibleOrdinal": spec}
    path = tmp_path / "bad_range_environment_assertions.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="environment_assertions"):
        load_suite(path)


@pytest.mark.parametrize(
    ("value", "message"),
    [
        ([{"unsupported": True}], "contains unsupported keys"),
        (
            [{"source_refs_contain": [{"event_ids_contains": "bad"}]}],
            "event_ids_contains must be a list of non-empty strings",
        ),
        (
            [{"evidence_refs_contain": [{"id": 123}]}],
            "id must be a non-empty string",
        ),
        ([{"id": 123}], "id must be a non-empty string"),
        ([{"revision": "2"}], "revision must be a positive integer"),
        ([{"sensitive": "true"}], "sensitive must be a boolean"),
        ([{"sensitive": None}], "sensitive must be a boolean"),
        ([{"content_contains_any": []}], "content_contains_any must be a non-empty list"),
        ([{"content_contains_any": [" "]}], "content_contains_any must be a non-empty list"),
        ([{"content_contains_any": "杭州"}], "content_contains_any must be a non-empty list"),
    ],
)
def test_load_suite_rejects_malformed_assert_memory_nested_schema(
    tmp_path: Path, value, message
):
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["setup"] = {
        "type": "assert_memory",
        "expected": value,
    }
    path = tmp_path / "invalid.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")
    with pytest.raises(SuiteValidationError, match=message):
        load_suite(path)


def test_load_suite_accepts_a_device_prepare_recipe(tmp_path: Path):
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["setup"] = {
        "type": "agent_prompt",
        "prompt": "Open the contact.",
        "device_prepare": {"recipe": "contact_detail", "params": {"name": "x"}},
    }
    path = tmp_path / "prepare.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    assert load_suite(path).tasks[0].setup["device_prepare"]["recipe"] == "contact_detail"


@pytest.mark.parametrize(
    "spec, message",
    [
        ("contact_detail", "device_prepare must be an object"),
        ({"recipe": "contact_detail", "recipe2": "x"}, "only recipe and params"),
        ({"params": {}}, "requires a non-empty recipe"),
        ({"recipe": "  "}, "requires a non-empty recipe"),
        ({"recipe": "contact_detail", "params": []}, "params must be an object"),
    ],
)
def test_load_suite_rejects_malformed_device_prepare(tmp_path: Path, spec, message):
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["setup"] = {
        "type": "agent_prompt",
        "prompt": "Open the contact.",
        "device_prepare": spec,
    }
    path = tmp_path / "bad_prepare.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match=message):
        load_suite(path)


def test_load_suite_rejects_device_prepare_next_to_expected_response(tmp_path: Path):
    """A confirmed recipe never sends the prompt, so the assertion cannot run."""
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["setup"] = {
        "type": "agent_prompt",
        "prompt": "Open the contact.",
        "expected_response": "done",
        "device_prepare": {"recipe": "contact_detail"},
    }
    path = tmp_path / "prepare_expected.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="cannot be combined with expected_response"):
        load_suite(path)


def test_load_suite_rejects_device_prepare_with_retained_history(tmp_path: Path):
    """A recipe leaves no setup conversation, so retained context would vary by path."""
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["setup"] = {
        "type": "agent_prompt",
        "prompt": "Open the contact.",
        "clear_history_after": False,
        "device_prepare": {"recipe": "contact_detail"},
    }
    path = tmp_path / "prepare_history.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="clear_history_after=false"):
        load_suite(path)


def test_perception_v1_scores_tap_coordinates_deterministically():
    from runner.assertions import evaluate_hard_assertions
    from runner.models import Trace, ToolCall

    suite_path = Path(__file__).resolve().parents[1] / "suites" / "perception" / "perception_v1.json"
    suite = load_suite(suite_path)
    for task in suite.tasks:
        # Coordinates are checked by the runner, never by the judge.
        assert task.rubric == []
        assert task.hard_assertions.response_required is False
        assert [call.tool for call in task.hard_assertions.required_tool_calls] == ["touch_gesture"]

    task = next(task for task in suite.tasks if task.id == "find_settings_iphone")
    point = task.hard_assertions.required_tool_calls[0].input_contains["point"]
    assert point == {"x": {"$gte": 746, "$lte": 930}, "y": {"$gte": 441, "$lte": 560}}

    def outcome(tool_input):
        trace = Trace(tool_calls=[ToolCall(step=1, tool="touch_gesture", input=tool_input)],
                      final_response="", total_tool_calls=1, total_duration_ms=0)
        return evaluate_hard_assertions(trace, task.hard_assertions, timed_out=False).all_passed

    assert outcome({"type": "tap", "point": {"x": 845, "y": 477}})
    # Outside the band, 0-1 style values, and an untyped gesture all fail.
    assert not outcome({"type": "tap", "point": {"x": 500, "y": 500}})
    assert not outcome({"type": "tap", "point": {"x": 0.85, "y": 0.48}})
    assert not outcome({"point": {"x": 845, "y": 477}})


def test_perception_v1_uses_fixture_backed_mock_environment_for_every_task():
    suite_path = (
        Path(__file__).resolve().parents[1]
        / "suites"
        / "perception"
        / "perception_v1.json"
    )
    suite = load_suite(suite_path)

    assert suite.mock_environment is None
    assert suite.tasks
    for task in suite.tasks:
        assert task.input_screenshot
        assert task.mock_environment is not None
        assert task.mock_environment.platform is TargetPlatform.IOS
        assert task.mock_environment.single_frame is True
        assert task.mock_environment.screen == task.input_screenshot
        assert task.mock_environment.phone_bridge == {}
        responses = task.mock_environment.tools["touch_gesture"]
        assert len(responses) == 1
        assert responses[0].output == {"ok": True}
        assert responses[0].is_error is False


def test_perception_v1_describes_single_frame_environment_without_coaching():
    suite_path = (
        Path(__file__).resolve().parents[1]
        / "suites"
        / "perception"
        / "perception_v1.json"
    )
    suite = load_suite(suite_path)

    prompt_prefix = suite.prompt_prefix.strip().lower()
    assert "static" in prompt_prefix
    assert "locate" in prompt_prefix
    assert "click" in prompt_prefix
    for leaked_instruction in (
        "screenshot",
        "touch_gesture",
        "width",
        "height",
        "0-1000",
        "normalized",
        "coordinate",
        "retry",
        "verify",
    ):
        assert leaked_instruction not in prompt_prefix


def test_mobilegym_touch_suite_starts_in_target_app_and_checks_outcome():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "mobilegym_touch_v1.json"
    suite = load_suite(suite_path)

    assert suite.name == "mobilegym_touch_v1"
    for task in suite.tasks:
        assert task.environment_assertions["route.app"] in task.app_ids
        if task.foreground_app_id is not None:
            assert task.environment_assertions["route.app"] == task.foreground_app_id
        assert task.hard_assertions.required_tools == ["touch_gesture"]
        assert task.rubric and all(item.check for item in task.rubric)
        if task.foreground_app_id == "calculator2":
            assert task.environment_assertions["apps.calculator2.state"] == "RESULT"
            assert task.environment_assertions["apps.calculator2.formula"]


def test_adb_android_suites_keep_navigation_and_text_editing_separate():
    suites_dir = Path(__file__).resolve().parents[1] / "suites"
    basic = load_suite(suites_dir / "adb_android_basic.json")
    editing = load_suite(suites_dir / "adb_android_text_editing.json")
    assert {task.id for task in basic.tasks}.isdisjoint(task.id for task in editing.tasks)
    for suite in (basic, editing):
        assert suite.prompt_prefix == ""
        for task in suite.tasks:
            assert "shell" in task.hard_assertions.forbidden_tools
            assert "request_user_action" in task.hard_assertions.forbidden_tools
            # Generic ADB pairs must preserve task setup, without foregrounding
            # the companion App only in the connected variant.
            assert task.foreground_app_id is None
    by_id = {task.id: task for task in basic.tasks}
    assert by_id["tap_back"].setup is not None
    assert by_id["scroll_page_down"].setup is not None
    assert by_id["screenshot_home"].hard_assertions.required_tools == ["screenshot"]


@pytest.mark.parametrize("task_id,action", [
    ("quick_action_home", "home"),
    ("quick_action_app_switch", "app_switch"),
    ("quick_action_notification_center", "notification_center"),
])
def test_adb_shortcut_contract_requires_successful_execution(task_id, action):
    from runner.assertions import evaluate_hard_assertions
    from runner.models import Trace, ToolCall

    suite = load_suite(Path(__file__).resolve().parents[1] / "suites" / "adb_android_basic.json")
    task = next(task for task in suite.tasks if task.id == task_id)
    trace = Trace(tool_calls=[ToolCall(step=1, tool="quick_action", input={"action": action})],
                  final_response="Done", total_tool_calls=1, total_duration_ms=100)
    assert evaluate_hard_assertions(trace, task.hard_assertions, timed_out=False).all_passed
    trace.tool_calls[0].is_error = True
    assert not evaluate_hard_assertions(trace, task.hard_assertions, timed_out=False).all_passed
    trace.tool_calls[0].is_error = False
    trace.tool_calls[0].input = {"action": "screenshot"}
    assert not evaluate_hard_assertions(trace, task.hard_assertions, timed_out=False).all_passed


def test_adb_bottom_scroll_accepts_one_successful_scroll():
    from runner.assertions import evaluate_hard_assertions
    from runner.models import Trace, ToolCall

    suite = load_suite(Path(__file__).resolve().parents[1] / "suites" / "adb_android_basic.json")
    task = next(task for task in suite.tasks if task.id == "scroll_to_bottom")
    trace = Trace(tool_calls=[ToolCall(step=1, tool="touch_gesture", input={"type": "swipe"})],
                  final_response="Reached the bottom", total_tool_calls=1, total_duration_ms=100)
    # The visual rubric judges whether the bottom was actually reached.
    assert evaluate_hard_assertions(trace, task.hard_assertions, timed_out=False).all_passed


@pytest.mark.parametrize("task_id,tool,tool_input", [
    ("swipe_scroll_list", "touch_gesture", {"type": "swipe", "start": {"x": 500, "y": 800}, "end": {"x": 500, "y": 300}}),
    ("settings_check_wifi", "open_app", {"app": "Settings"}),
    ("clock_count_alarms", "open_app", {"app": "Clock"}),
])
def test_mobilegym_basic_accepts_equivalent_navigation_tools(task_id, tool, tool_input):
    from runner.assertions import evaluate_hard_assertions
    from runner.models import Trace, ToolCall

    suite = load_suite(Path(__file__).resolve().parents[1] / "suites" / "mobilegym_basic.json")
    task = next(task for task in suite.tasks if task.id == task_id)
    trace = Trace(
        tool_calls=[ToolCall(step=1, tool=tool, input=tool_input)],
        final_response="Task completed", total_tool_calls=1, total_duration_ms=100,
    )
    # Outcome correctness is checked by the suite's unchanged visual rubrics.
    assert evaluate_hard_assertions(trace, task.hard_assertions, timed_out=False).all_passed
    trace.tool_calls = []
    trace.total_tool_calls = 0
    assert not evaluate_hard_assertions(trace, task.hard_assertions, timed_out=False).all_passed


def test_benchmark_suites_do_not_use_tool_sequence():
    suites_root = Path(__file__).resolve().parents[1] / "suites"
    offenders = []

    def walk(path, value):
        if isinstance(value, dict):
            for key, child in value.items():
                child_path = f"{path}.{key}" if path else key
                if key == "tool_sequence":
                    offenders.append(child_path)
                walk(child_path, child)
        elif isinstance(value, list):
            for index, child in enumerate(value):
                walk(f"{path}[{index}]", child)

    for suite_path in sorted(suites_root.rglob("*.json")):
        data = json.loads(suite_path.read_text(encoding="utf-8"))
        walk(str(suite_path.relative_to(suites_root)), data)

    assert offenders == []

def test_load_suite_parses_expected_option_answer(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "expected_answer": "(c)",
                "answer_format": "option_letter",
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(p)

    assert suite.tasks[0].expected_answer == "(c)"
    assert suite.tasks[0].answer_format == "option_letter"

def test_load_suite_parses_expected_recalled_memory_ids(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "expected_recalled_memory_ids": ["mem_expected"],
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(p)

    assert suite.tasks[0].expected_recalled_memory_ids == ["mem_expected"]


def test_load_suite_parses_task_app_ids(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "app_ids": ["settings"],
                "foreground_app_id": "scroll_lab",
            }
        ],
    }
    path = tmp_path / "s.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(path)

    assert suite.tasks[0].app_ids == ["settings"]
    assert suite.tasks[0].foreground_app_id == "scroll_lab"


@pytest.mark.parametrize("value", ["", " scroll_lab ", 123, []])
def test_load_suite_rejects_invalid_foreground_app_id(tmp_path: Path, value):
    fixture = json.loads(json.dumps(FIXTURE))
    fixture["tasks"][0]["foreground_app_id"] = value
    path = tmp_path / "bad_foreground.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")
    with pytest.raises(SuiteValidationError, match="foreground_app_id"):
        load_suite(path)


def test_load_suite_rejects_invalid_task_app_ids(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "app_ids": "settings",
            }
        ],
    }
    path = tmp_path / "s.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="app_ids"):
        load_suite(path)

def test_load_suite_parses_task_platforms(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "platforms": ["Android", "ios", "Windows", "linux", "android"],
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(p)

    assert suite.tasks[0].platforms == ["android", "ios", "windows", "linux"]

def test_load_suite_rejects_invalid_task_platform(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "platforms": ["chromeos"],
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="invalid platform"):
        load_suite(p)

def test_load_suite_parses_required_and_forbidden_tools(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "hard_assertions": {
                    "required_tools": ["enter_plan_mode", "commit_plan", "commit_plan"],
                    "forbidden_tools": ["screenshot"],
                },
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(p)

    assert suite.tasks[0].hard_assertions.required_tools == ["enter_plan_mode", "commit_plan"]
    assert suite.tasks[0].hard_assertions.forbidden_tools == ["screenshot"]

def test_load_suite_rejects_invalid_required_tools(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "hard_assertions": {"required_tools": ["enter_plan_mode", ""]},
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError):
        load_suite(p)


def test_load_suite_rejects_non_object_rubric_items(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "rubric": ["not an object"],
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="rubric items must be objects"):
        load_suite(p)


def test_load_suite_rejects_overlapping_required_and_forbidden_tools(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "hard_assertions": {
                    "required_tools": ["commit_plan", "shell"],
                    "forbidden_tools": ["screenshot", "commit_plan"],
                },
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="overlapping required/forbidden tools"):
        load_suite(p)

def test_load_suite_rejects_invalid_expected_recalled_memory_ids(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "expected_recalled_memory_ids": [""],
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError):
        load_suite(p)

def test_load_suite_rejects_falsy_invalid_expected_recalled_memory_ids(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "expected_recalled_memory_ids": "",
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError):
        load_suite(p)

def test_load_suite_rejects_non_string_prompt_prefix(tmp_path: Path):
    fixture = {**FIXTURE, "prompt_prefix": ["recall_memory"]}
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError):
        load_suite(p)


def test_load_suite_parses_task_level_mock_environment(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "mock_environment": {
                    "platform": "ios",
                    "phone_bridge": {
                        "app_state": "background",
                        "pip_bridge_enabled": False,
                    },
                    "tools": {
                        "bridge_contacts": {
                            "output": {"ok": True, "contacts": []},
                        }
                    },
                },
            }
        ],
    }
    p = tmp_path / "task-mock.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(p)

    assert suite.mock_environment is None
    assert suite.tasks[0].mock_environment is not None
    assert suite.tasks[0].mock_environment.platform is TargetPlatform.IOS
    assert suite.tasks[0].mock_environment.single_frame is False
    assert "platform" not in suite.tasks[0].mock_environment.phone_bridge
    assert "bridge_contacts" in suite.tasks[0].mock_environment.tools


def test_load_suite_parses_single_frame_mock_environment(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "mock_environment": {
            "platform": "ios",
            "single_frame": True,
            "tools": {},
        },
    }
    p = tmp_path / "single-frame-mock.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(p)

    assert suite.mock_environment is not None
    assert suite.mock_environment.single_frame is True


@pytest.mark.parametrize("single_frame", [None, 0, 1, "true", [], {}])
def test_load_suite_rejects_non_boolean_single_frame(
    tmp_path: Path,
    single_frame,
):
    fixture = {
        **FIXTURE,
        "mock_environment": {
            "platform": "ios",
            "single_frame": single_frame,
            "tools": {},
        },
    }
    p = tmp_path / "invalid-single-frame-mock.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="single_frame must be boolean"):
        load_suite(p)


def test_load_suite_normalizes_legacy_phone_bridge_platform(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "mock_environment": {
            "phone_bridge": {"platform": "iOS", "connected": True},
            "tools": {},
        },
    }
    p = tmp_path / "legacy-mock.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(p)

    assert suite.mock_environment is not None
    assert suite.mock_environment.platform == "ios"
    assert suite.mock_environment.phone_bridge == {"connected": True}


def test_load_suite_rejects_conflicting_mock_platforms(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "mock_environment": {
            "platform": "ios",
            "phone_bridge": {"platform": "android"},
            "tools": {},
        },
    }
    p = tmp_path / "conflicting-mock.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="conflicts with"):
        load_suite(p)


def test_load_suite_rejects_invalid_top_level_mock_platform(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "mock_environment": {"platform": "chromeos", "tools": {}},
    }
    p = tmp_path / "invalid-mock.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(
        SuiteValidationError,
        match="platform must be ios, android, mac, windows, or linux",
    ):
        load_suite(p)


def test_load_suite_allows_task_mock_to_override_suite_default(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "mock_environment": {
            "platform": "ios",
            "phone_bridge": {},
            "tools": {"bridge_contacts": {"output": {"ok": True}}},
        },
        "tasks": [
            FIXTURE["tasks"][0],
            {
                **FIXTURE["tasks"][0],
                "id": "android_override",
                "mock_environment": {
                    "platform": "android",
                    "phone_bridge": {},
                    "tools": {"bridge_calendar": {"output": {"ok": True}}},
                },
            },
        ],
    }
    p = tmp_path / "task-mock-override.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(p)

    assert suite.mock_environment is not None
    assert suite.mock_environment.platform == "ios"
    assert suite.tasks[0].mock_environment is None
    assert suite.tasks[1].mock_environment is not None
    assert suite.tasks[1].mock_environment.platform == "android"


def test_load_suite_allows_mixed_mock_and_real_environment_tasks(
    tmp_path: Path,
):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "mock_environment": {
                    "phone_bridge": {"platform": "ios"},
                    "tools": {},
                },
            },
            {
                **FIXTURE["tasks"][0],
                "id": "without_mock",
            },
        ],
    }
    p = tmp_path / "partial-task-mock.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(p)

    assert suite.mock_environment is None
    assert suite.tasks[0].mock_environment is not None
    assert suite.tasks[0].mock_environment.platform == "ios"
    assert suite.tasks[1].mock_environment is None


def test_load_suite_rejects_unknown_setup_keys(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "setup": {
                    "type": "seed_episode",
                    "episode": {"id": "ep-1", "user_goal": "verify a procedure"},
                    "consolodate": True,
                },
            }
        ],
    }
    p = tmp_path / "unknown-setup-key.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="unsupported seed_episode setup keys: consolodate"):
        load_suite(p)


def test_load_suite_parses_consolidation_expectation(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [{
            **FIXTURE["tasks"][0],
            "setup": {
                "type": "seed_episode",
                "episode": {"id": "ep-1", "user_goal": "verify a procedure"},
                "consolidate": True,
                "consolidation_expectation": {
                    "goal_result": "unknown",
                    "min_memory_ids": 0,
                    "max_memory_ids": 1,
                    "allow_empty_memory": False,
                    "required_assessment_evidence": False,
                    "forbidden_memory_substrings": ["secret-value"],
                    "required_memory_substrings": ["verify"],
                    "required_memory_types": ["procedure"],
                    "required_memory_scope": {"app_version": "7"},
                },
            },
        }],
    }
    p = tmp_path / "consolidation-expectation.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(p)
    expectation = suite.tasks[0].consolidation_expectation
    assert expectation is not None
    assert expectation.goal_result == "unknown"
    assert expectation.max_memory_ids == 1
    assert expectation.allow_empty_memory is False
    assert expectation.forbidden_memory_substrings == ["secret-value"]
    assert expectation.required_memory_substrings == ["verify"]
    assert expectation.required_memory_types == ["procedure"]
    assert expectation.required_memory_scope == {"app_version": "7"}
    assert suite.tasks[0].setup["consolidation_expectation"]["goal_result"] == "unknown"


def test_load_suite_rejects_invalid_consolidation_expectation(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [{
            **FIXTURE["tasks"][0],
            "setup": {
                "type": "seed_episode",
                "episode": {"id": "ep-1", "user_goal": "verify a procedure"},
                "consolidation_expectation": {
                    "goal_result": "maybe",
                },
            },
        }],
    }
    p = tmp_path / "invalid-consolidation-expectation.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="goal_result"):
        load_suite(p)


def test_load_suite_rejects_consolidation_expectation_without_consolidation(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [{
            **FIXTURE["tasks"][0],
            "setup": {
                "type": "seed_episode",
                "episode": {"id": "ep-1", "user_goal": "verify a procedure"},
                "consolidate": False,
                "consolidation_expectation": {"goal_result": "unknown"},
            },
        }],
    }
    p = tmp_path / "contradictory-consolidation-expectation.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="requires consolidate=true"):
        load_suite(p)


def test_load_suite_rejects_allow_empty_with_positive_memory_contract(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [{
            **FIXTURE["tasks"][0],
            "setup": {
                "type": "seed_episode",
                "episode": {"id": "ep-1", "user_goal": "verify a procedure"},
                "consolidate": True,
                "consolidation_expectation": {
                    "allow_empty_memory": True,
                    "required_memory_types": ["procedure"],
                },
            },
        }],
    }
    p = tmp_path / "empty-positive-consolidation-expectation.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="allow_empty_memory cannot be true"):
        load_suite(p)


def test_load_suite_rejects_allow_empty_with_positive_minimum(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [{
            **FIXTURE["tasks"][0],
            "setup": {
                "type": "seed_episode",
                "episode": {"id": "ep-1", "user_goal": "verify a procedure"},
                "consolidate": True,
                "consolidation_expectation": {
                    "allow_empty_memory": True,
                    "min_memory_ids": 1,
                },
            },
        }],
    }
    p = tmp_path / "empty-positive-minimum.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="min_memory_ids is positive"):
        load_suite(p)


def test_load_suite_rejects_multiple_seed_episode_expectations(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [{
            **FIXTURE["tasks"][0],
            "setup": [
                {
                    "type": "seed_episode",
                    "episode": {"id": "ep-1", "user_goal": "first"},
                    "consolidate": True,
                    "consolidation_expectation": {"goal_result": "achieved"},
                },
                {
                    "type": "seed_episode",
                    "episode": {"id": "ep-2", "user_goal": "second"},
                    "consolidate": True,
                    "consolidation_expectation": {"goal_result": "achieved"},
                },
            ],
        }],
    }
    p = tmp_path / "duplicate-seed-episode-expectations.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="only one seed_episode"):
        load_suite(p)


def test_load_suite_preserves_seed_episode_expectation_identity(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [{
            **FIXTURE["tasks"][0],
            "setup": [
                {
                    "type": "seed_episode",
                    "episode": {"id": "ep-1", "user_goal": "first"},
                    "consolidate": False,
                },
                {
                    "type": "seed_episode",
                    "episode": {"id": "ep-2", "user_goal": "second"},
                    "consolidate": True,
                    "consolidation_expectation": {"goal_result": "achieved"},
                },
            ],
        }],
    }
    p = tmp_path / "seed-episode-expectation-identity.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    task = load_suite(p).tasks[0]
    assert task.consolidation_expectation.goal_result == "achieved"
    assert "consolidation_expectation" not in task.setup[0]
    assert task.setup[1]["consolidation_expectation"] == {
        "goal_result": "achieved"
    }


def test_load_suite_accepts_quick_capture_setup_keys(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "setup": {
                    "type": "seed_memory",
                    "memories": [{"id": "mem-screen", "content": "saved screen"}],
                    "clear_history_after": True,
                },
            },
            {
                **FIXTURE["tasks"][0],
                "id": "capture_setup",
                "setup": {
                    "type": "agent_prompt",
                    "prompt": "prepare capture",
                    "expected_response": "READY",
                    "clear_history_after": False,
                },
            },
        ],
    }
    p = tmp_path / "quick-capture-setup-keys.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    suite = load_suite(p)

    assert suite.tasks[0].setup["clear_history_after"] is True
    assert suite.tasks[1].setup["expected_response"] == "READY"


@pytest.mark.parametrize("setup_type", [["seed_episode"], {"name": "seed_episode"}, 1, None])
def test_load_suite_rejects_non_string_setup_type(tmp_path: Path, setup_type):
    fixture = {
        **FIXTURE,
        "tasks": [{**FIXTURE["tasks"][0], "setup": {"type": setup_type}}],
    }
    p = tmp_path / "invalid-setup-type.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError, match="setup type must be a string"):
        load_suite(p)


def test_load_suite_accepts_seed_session_chunk_setup_keys(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "setup": {
                    "type": "seed_session_chunk",
                    "session_id": "benchmark-session",
                    "summary": "Seeded chunk summary",
                    "messages": [{"role": "user", "content": "old question"}],
                    "timeout_sec": 30,
                    "clear_history_after": True,
                },
            }
        ],
    }
    path = tmp_path / "seed-session-chunk-setup-keys.json"
    path.write_text(json.dumps(fixture), encoding="utf-8")

    task = load_suite(path).tasks[0]
    assert task.setup["type"] == "seed_session_chunk"
    assert task.setup["session_id"] == "benchmark-session"


def test_load_suite_rejects_invalid_expected_option_answer(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "expected_answer": "z",
                "answer_format": "option_letter",
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError):
        load_suite(p)

def test_load_suite_rejects_ambiguous_expected_option_answer(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "expected_answer": "(a) or (b)",
                "answer_format": "option_letter",
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError):
        load_suite(p)

def test_load_suite_rejects_answer_format_without_expected_answer(tmp_path: Path):
    fixture = {
        **FIXTURE,
        "tasks": [
            {
                **FIXTURE["tasks"][0],
                "answer_format": "option_letter",
            }
        ],
    }
    p = tmp_path / "s.json"
    p.write_text(json.dumps(fixture), encoding="utf-8")

    with pytest.raises(SuiteValidationError):
        load_suite(p)


def test_load_suite_missing_tasks_raises(tmp_path: Path):
    p = tmp_path / "s.json"
    p.write_text(json.dumps({"name": "x"}), encoding="utf-8")
    with pytest.raises(SuiteValidationError):
        load_suite(p)

def test_load_suite_invalid_category_raises(tmp_path: Path):
    bad = {**FIXTURE, "tasks": [{**FIXTURE["tasks"][0], "category": "weird"}]}
    p = tmp_path / "s.json"
    p.write_text(json.dumps(bad), encoding="utf-8")
    with pytest.raises(SuiteValidationError):
        load_suite(p)

def test_load_suite_duplicate_ids_raise(tmp_path: Path):
    bad = {**FIXTURE, "tasks": [FIXTURE["tasks"][0], FIXTURE["tasks"][0]]}
    p = tmp_path / "s.json"
    p.write_text(json.dumps(bad), encoding="utf-8")
    with pytest.raises(SuiteValidationError):
        load_suite(p)


def test_shell_utility_suite_replaces_removed_time_and_calculator_tools():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "shell_utility_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    assert suite.name == "shell_utility_v1"
    assert "shell" not in suite.prompt_prefix.lower()
    for task in suite.tasks:
        assert task.hard_assertions.required_tools == ["shell"]
        assert "shell" not in task.prompt.lower()
        assert "current_time" in task.hard_assertions.forbidden_tools
        assert "calculator" in task.hard_assertions.forbidden_tools
        assert "screenshot" in task.hard_assertions.forbidden_tools

    calculation = task_by_id["calculation_via_shell"]
    assert calculation.expected_answer == "(b)"
    assert calculation.answer_format == "option_letter"


def test_skill_discovery_suite_does_not_prompt_for_skill_read():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "skill_discovery_v1.json"
    suite = load_suite(suite_path)

    assert suite.name == "skill_discovery_v1"
    assert "device-operation skill" in suite.prompt_prefix
    assert "skill_read" not in suite.prompt_prefix
    assert "device-operator" not in suite.prompt_prefix
    assert [obs.skill_name for obs in suite.trace_observations] == ["device-operator"]

    for task in suite.tasks:
        assert "skill_read" not in task.prompt
        assert "device-operator" not in task.prompt
        assert "device-operation skill" in task.description_for_judge
        assert task.hard_assertions.required_tools == []
        assert "shell" in task.hard_assertions.forbidden_tools


def test_skillopt_crossapp_device_operator_suites_target_skill_capabilities():
    suite_root = Path(__file__).resolve().parents[2] / "skillopt" / "suites" / "device-operator"
    train = load_suite(suite_root / "crossapp_train.json")
    verification = load_suite(suite_root / "crossapp_verification.json")

    assert train.name == "crossapp_train"
    assert verification.name == "crossapp_verification"
    assert [obs.skill_name for obs in train.trace_observations] == ["device-operator"]
    assert [obs.skill_name for obs in verification.trace_observations] == ["device-operator"]

    train_ids = {task.id for task in train.tasks}
    verification_ids = {task.id for task in verification.tasks}
    assert train_ids and verification_ids
    assert train_ids.isdisjoint(verification_ids)

    all_tasks = [*train.tasks, *verification.tasks]
    for family in ("work", "life", "content", "commerce"):
        assert any(task.id.startswith(f"crossapp_{family}_") for task in all_tasks)

    precondition_phrases = [
        "当前输入框",
        "当前搜索框",
        "Starting from",
        "In the eBay app",
        "In the eBay search results",
    ]
    app_names = {
        "Alipay",
        "Bilibili",
        "Calendar",
        "Clock",
        "eBay",
        "Map",
        "Notes",
        "Railway 12306",
        "RedNote",
        "Spotify",
        "Tencent Meeting",
        "Weather",
        "WeChat",
        "WeChat Reading",
    }
    sensitive_words = ("send", "post", "buy", "purchase", "checkout", "payment", "order submission", "submit order", "message to")

    def mentioned_apps(prompt: str) -> set[str]:
        names = set()
        prompt_without_compound_names = prompt
        for compound_name in ["WeChat Reading"]:
            if compound_name in prompt:
                names.add(compound_name)
                prompt_without_compound_names = prompt_without_compound_names.replace(compound_name, "")
        for app in app_names - names:
            if app in prompt_without_compound_names:
                names.add(app)
        return names

    for task in all_tasks:
        assert task.category == "multi_step"
        assert not any(phrase in task.prompt for phrase in precondition_phrases), task.id
        assert task.hard_assertions.response_required is True
        assert "screenshot" in task.hard_assertions.required_tools
        assert "shell" in task.hard_assertions.forbidden_tools
        assert len(mentioned_apps(task.prompt)) >= 2, task.id
        prompt_lower = task.prompt.lower()
        if any(word in prompt_lower for word in sensitive_words):
            if "send" in prompt_lower or "message to" in prompt_lower:
                assert "do not send" in prompt_lower and "ask for approval" in prompt_lower, task.id
                assert "send" in task.hard_assertions.prohibited_actions, task.id
            if "post" in prompt_lower:
                assert "do not post" in prompt_lower or "stop before posting" in prompt_lower, task.id
                assert "post" in task.hard_assertions.prohibited_actions, task.id
            if "buy" in prompt_lower or "purchase" in prompt_lower:
                assert "do not buy" in prompt_lower or "stop before any purchase" in prompt_lower, task.id
                assert "purchase" in task.hard_assertions.prohibited_actions, task.id
            if "checkout" in prompt_lower:
                assert "do not enter checkout" in prompt_lower or "stop before any purchase, checkout" in prompt_lower or "stop before checkout" in prompt_lower, task.id
                assert "checkout" in task.hard_assertions.prohibited_actions, task.id
            if "payment" in prompt_lower:
                assert "do not initiate any payment" in prompt_lower or "do not enter checkout/payment" in prompt_lower or "stop before any purchase, checkout, payment" in prompt_lower or "stop before payment" in prompt_lower, task.id
                assert "payment" in task.hard_assertions.prohibited_actions, task.id
            if "order submission" in prompt_lower or "submit order" in prompt_lower:
                assert "stop before any purchase, checkout, payment, or order submission" in prompt_lower or "stop before order" in prompt_lower, task.id
                assert "order_submission" in task.hard_assertions.prohibited_actions, task.id

    map_task = next(task for task in train.tasks if task.id == "crossapp_life_map_place_draft_wechat")
    assert "If no Map result" in map_task.prompt
    assert "instead of inventing" in map_task.prompt
    assert not any("plausible address" in item.check for item in map_task.rubric)
    assert any("visible Map result" in item.check for item in map_task.rubric)


def test_personamem_lt_recall_suite_uses_deterministic_answers():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "personamem_lt_recall_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}
    expected_tasks = {
        "personamem_music_recall_fact",
        "personamem_music_update_reason",
        "personamem_music_preference_evolution",
        "personamem_cooking_generalization",
        "personamem_music_creative_getaway",
        "personamem_music_new_expression",
        "personamem_food_fusion_cuisine",
        "personamem_therapy_yoga_acknowledge",
        "personamem_therapy_games_dislike",
        "personamem_writing_style_discrimination",
        "personamem_book_recommendation_preference",
        "personamem_travel_planning_preference",
        "personamem_online_shopping_preference",
        "personamem_sports_recommendation_preference",
        "personamem_study_consultation_generalization",
        "personamem_family_relations_update_reason",
    }

    assert suite.name == "personamem_lt_recall_v1"
    assert "recall_memory" in suite.prompt_prefix
    assert expected_tasks <= set(task_by_id)
    assert all(task.category == "memory" for task in suite.tasks)
    assert all(task.expected_answer for task in suite.tasks)
    assert all(task.expected_recalled_memory_ids for task in suite.tasks)
    assert all(task.answer_format == "option_letter" for task in suite.tasks)


def test_episode_reflection_v2_preview_workaround_fixtures_show_why_it_is_needed():
    # Device Memory keeps only app-specific knowledge. Without a failed direct
    # Save, the Preview round trip is an ordinary save flow and is discarded.
    suite = load_suite(Path(__file__).resolve().parents[1] / "suites" / "episode_reflection_v2.json")
    task_by_id = {task.id: task for task in suite.tasks}

    for task_id in (
        "verified_success_procedure_applies",
        "duplicate_attempts_are_compacted",
        "version_scoped_lesson_requires_fresh_check",
        "successful_lesson_has_assessment_evidence",
    ):
        events = task_by_id[task_id].setup["episode"]["events"]
        results = [event for event in events if event["type"] == "tool_result"]
        first_preview = next(i for i, event in enumerate(results) if "Preview" in event["observation"])
        assert any(
            "did not persist" in event["observation"] for event in results[:first_preview]
        ), task_id
        calls = [event for event in events if event["type"] == "tool_call"]
        assert len(calls) == len(results), task_id
        # The failed Save reverted the title, so the next step must re-enter it
        # before Preview can show the change.
        failure = next(i for i, event in enumerate(events) if "did not persist" in event.get("observation", ""))
        assert "re-enter the changed title" in events[failure + 1]["content"].lower(), task_id


def test_episode_reflection_v2_option_tasks_request_a_tagged_answer():
    # Agents often restate every option before choosing. Without the tag, the
    # restated options make the choice ambiguous to the answer parser.
    suite = load_suite(Path(__file__).resolve().parents[1] / "suites" / "episode_reflection_v2.json")
    for task in suite.tasks:
        if task.answer_format == "option_letter":
            assert "<final_answer>(x)</final_answer>" in task.prompt, task.id


def test_episode_memory_conditions_share_natural_and_iphone_tasks():
    suites_dir = Path(__file__).resolve().parents[1] / "suites"
    for name in (
        "episode_memory_before_v1.json",
        "episode_memory_after_v1.json",
        "episode_memory_legacy_v1.json",
    ):
        assert (suites_dir / name).read_text(encoding="utf-8").isascii()
    before = load_suite(suites_dir / "episode_memory_before_v1.json")
    after = load_suite(suites_dir / "episode_memory_after_v1.json")
    legacy = load_suite(suites_dir / "episode_memory_legacy_v1.json")

    # The conditions differ only in setup, so they must run the same tasks.
    assert [task.id for task in before.tasks] == [task.id for task in after.tasks]
    assert [task.id for task in before.tasks] == [task.id for task in legacy.tasks]

    for suite in (before, after, legacy):
        assert all(task.mock_environment is None for task in suite.tasks)
        assert suite.tasks[1].platforms == ["ios"]
        assert suite.tasks[1].setup["type"] in {"seed_episode", "seed_memory"}

    for before_task, after_task in zip(before.tasks, after.tasks, strict=True):
        assert before_task.id == after_task.id
        assert before_task.prompt == after_task.prompt
        assert before_task.description_for_judge == after_task.description_for_judge
        assert before_task.rubric == after_task.rubric
        if before_task.id == "qa_notes_title_save_procedure":
            assert "recall_device_memory" not in before_task.hard_assertions.required_tools
            assert "recall_device_memory" not in after_task.hard_assertions.required_tools
            assert after_task.expected_recall_from_consolidation is True
            assert after_task.expected_recalled_memory_tool == "recall_device_memory"
        else:
            assert before_task.hard_assertions == after_task.hard_assertions

    for suite in (before, after, legacy):
        natural_task = suite.tasks[0]
        natural_text = " ".join(
            [
                natural_task.prompt,
                natural_task.description_for_judge,
                *(item.check for item in natural_task.rubric),
            ]
        )
        assert "recall_device_memory" not in natural_text

    assert before.tasks[0].setup["consolidate"] is False
    assert after.tasks[0].setup["consolidate"] is True
    assert legacy.tasks[0].setup["type"] == "seed_memory"
    assert legacy.tasks[0].prompt == before.tasks[0].prompt
    assert legacy.tasks[0].expected_recalled_memory_ids == [
        "legacy_qa_notes_title_save_procedure"
    ]
    assert legacy.tasks[0].expected_recalled_memory_tool == "recall_device_memory"
    assert legacy.tasks[1].setup["type"] == "seed_memory"
    assert legacy.tasks[1].setup["memories"][0]["store"] == "device"

    assert before.tasks[1].setup["consolidate"] is False
    assert after.tasks[1].setup["consolidate"] is True
    assert before.tasks[1].setup["episode"] == after.tasks[1].setup["episode"]


def test_quick_capture_suite_covers_screen_memory_upper_bound():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "quick_capture_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    assert {
        "recall_exact_key_text",
        "recall_dense_multilingual_values",
        "prefer_latest_snapshot",
        "preserve_duplicate_snapshots",
        "route_screen_memory_away_from_session",
        "compare_multiple_snapshots",
        "find_old_snapshot_in_large_history",
        "do_not_hallucinate_missing_snapshot",
        "treat_snapshot_as_historical_evidence",
        "forget_one_of_two_similar_snapshots",
    } <= set(task_by_id)
    assert all(task.category == "memory" for task in suite.tasks)

    for task in suite.tasks:
        assert task.setup is not None
        assert task.setup["type"] == "seed_memory"
        assert task.setup["clear_history_after"] is True
        assert task.hard_assertions.must_complete_within_sec <= 120
        assert task.expected_answer is None

    for task in suite.tasks:
        requirements = task.hard_assertions.required_tool_calls
        assert any(item.tool == "recall_memory" for item in requirements)
        assert "recall_session_chunks" in task.hard_assertions.forbidden_tools

    assert len(task_by_id["compare_multiple_snapshots"].expected_recalled_memory_ids) == 3
    assert len(task_by_id["find_old_snapshot_in_large_history"].setup["memories"]) >= 8
    duplicate_task = task_by_id["preserve_duplicate_snapshots"]
    assert len(duplicate_task.expected_recalled_memory_ids) == 2
    # Deletion is covered by forget_one_of_two_similar_snapshots; this task
    # checks only that identical captures stay independent.
    assert "forget_memory" in duplicate_task.hard_assertions.forbidden_tools

    # Routing is validated by observed evidence and answer grounding, not by
    # forcing one exact recall query shape on the agent.
    route_task = task_by_id["route_screen_memory_away_from_session"]
    assert route_task.expected_recalled_memory_ids == [
        "mem_1700000000000000500_qc_approval_screen"
    ]
    route_memories = {item["id"]: item for item in route_task.setup["memories"]}
    assert route_memories["mem_1700000000000000500_qc_approval_screen"]["type"] == "screen_snapshot"
    assert route_memories["mem_1700000000000000501_qc_approval_fact"]["type"] == "fact"

    forget_task = task_by_id["forget_one_of_two_similar_snapshots"]
    assert any(
        item.tool == "forget_memory"
        and item.input_contains.get("id") == "mem_1700000000000001100_qc_old_order"
        for item in forget_task.hard_assertions.required_tool_calls
    )

def test_phone_bridge_data_policy_suite_covers_tools_and_routing_modes():
    suites_dir = Path(__file__).resolve().parents[1] / "suites" / "aico_app"
    suite = load_suite(suites_dir / "phone_bridge_data_policy_v1.json")
    tasks = {task.id: task for task in suite.tasks}

    assert suite.mock_environment is None
    assert suite.prompt_prefix == ""
    assert all(task.mock_environment is not None for task in suite.tasks)
    assert {
        task.mock_environment.platform
        for task in suite.tasks
        if task.mock_environment is not None
    } == {TargetPlatform.ANDROID, TargetPlatform.IOS}
    assert {
        tool
        for task in suite.tasks
        for tool in task.hard_assertions.required_tools
    } == {
        "bridge_contacts",
        "bridge_calendar",
        "bridge_clipboard",
        "bridge_notification",
    }
    required_calls = [
        call
        for task in suite.tasks
        for call in task.hard_assertions.required_tool_calls
    ]
    assert {
        call.input_contains.get("action")
        for call in required_calls
        if call.tool == "bridge_calendar"
    } == {"query", "create"}
    assert {
        call.input_contains.get("action")
        for call in required_calls
        if call.tool == "bridge_clipboard"
    } == {"read", "write"}

    dynamic_island_tasks = [
        task for task_id, task in tasks.items() if task_id.startswith("ios_dynamic_island_")
    ]
    pip_tasks = [task for task_id, task in tasks.items() if task_id.startswith("ios_pip_")]
    fgs_tasks = [task for task_id, task in tasks.items() if task_id.startswith("android_fgs_")]
    assert dynamic_island_tasks and pip_tasks and fgs_tasks

    for task in dynamic_island_tasks:
        state = task.mock_environment.phone_bridge
        assert task.mock_environment.platform == "ios"
        assert "platform" not in state
        assert state["pip_bridge_enabled"] is False
        assert state["return_entry"] == "dynamic_island"
        assert state["return_entry_available"] is True
        assert "bridge_open_app" in task.hard_assertions.forbidden_tools

    for task in pip_tasks:
        state = task.mock_environment.phone_bridge
        assert task.mock_environment.platform == "ios"
        assert "platform" not in state
        assert state["pip_bridge_enabled"] is True
        assert "bridge_open_app" in task.hard_assertions.forbidden_tools

    for task in fgs_tasks:
        state = task.mock_environment.phone_bridge
        assert task.mock_environment.platform == "android"
        assert "platform" not in state
        assert state["fgs_bridge_enabled"] is True
        assert "bridge_open_app" in task.hard_assertions.forbidden_tools


def test_aico_app_connection_capabilities_suite_covers_real_proxy_contract():
    suites_dir = Path(__file__).resolve().parents[1] / "suites" / "aico_app"
    suite = load_suite(suites_dir / "connection_capabilities_v1.json")
    tasks = {task.id: task for task in suite.tasks}

    assert suite.mock_environment is None
    assert all(task.mock_environment is None for task in suite.tasks)
    assert all(task.setup and task.setup["type"] == "agent_prompt" for task in suite.tasks)
    assert all(task.setup["clear_history_after"] is True for task in suite.tasks)
    assert all(set(task.platforms) <= {"ios", "android"} for task in suite.tasks)

    ios_context = tasks["ios_forwarded_environment_context"]
    android_context = tasks["android_forwarded_environment_context"]
    assert ios_context.platforms == ["ios"]
    assert android_context.platforms == ["android"]
    assert ios_context.hard_assertions.max_tool_calls == 0
    assert android_context.hard_assertions.max_tool_calls == 0

    assert tasks["semantic_open_browser"].hard_assertions.required_tool_calls[0].input_contains == {
        "app": "browser"
    }
    assert tasks["open_https_url"].hard_assertions.required_tool_calls[0].input_contains == {
        "url": "https://example.com/"
    }

    clipboard_actions = [
        requirement.input_contains["action"]
        for requirement in tasks[
            "clipboard_round_trip_restore"
        ].hard_assertions.required_tool_calls
    ]
    assert clipboard_actions == ["read", "write", "read", "write", "read"]

    calendar_actions = [
        requirement.input_contains["action"]
        for requirement in tasks[
            "calendar_create_query_delete_round_trip"
        ].hard_assertions.required_tool_calls
    ]
    assert calendar_actions == ["query", "create", "query", "delete", "query"]

    contacts_task = tasks["contacts_query_update_restore_fixture"]
    contacts_actions = [
        requirement.input_contains["action"]
        for requirement in contacts_task.hard_assertions.required_tool_calls
    ]
    assert contacts_actions == ["query", "update", "query", "update", "query"]
    assert all(
        requirement.input_contains["action"] != "create"
        for requirement in contacts_task.hard_assertions.required_tool_calls
    )

    notification_call = tasks[
        "send_local_notification"
    ].hard_assertions.required_tool_calls[0]
    assert notification_call.tool == "bridge_notification"
    assert notification_call.input_contains == {
        "action": "send",
        "title": "AICO_BRIDGE_NOTIFICATION_V1",
        "body": "forwarded app connection verified",
        "sound": False,
        "badge": 1,
    }


@pytest.mark.parametrize("bounds, message", [
    ({"$gte": "746"}, "must be a finite number"),
    ({"$lte": None}, "must be a finite number"),
    ({"$gte": True}, "must be a finite number"),
    ({"$gte": 10, "$lte": 5}, "must not exceed"),
])
def test_numeric_matcher_bounds_are_validated_when_the_suite_loads(tmp_path, bounds, message):
    task = {
        "id": "t", "category": "perception", "prompt": "Tap", "rubric": [],
        "hard_assertions": {"required_tool_calls": [
            {"tool": "touch_gesture", "input_contains": {"point": {"x": bounds}}},
        ]},
    }
    path = tmp_path / "suite.json"
    path.write_text(json.dumps({"name": "s", "tasks": [task]}))
    with pytest.raises(SuiteValidationError, match=message):
        load_suite(path)
    # Mock fixture matchers use the same validation.
    task["hard_assertions"]["required_tool_calls"][0]["input_contains"] = {"type": "tap"}
    task["mock_environment"] = {"platform": "ios", "phone_bridge": {}, "tools": {
        "touch_gesture": {"input_contains": {"point": {"y": bounds}}, "output": {"ok": True}},
    }}
    path.write_text(json.dumps({"name": "s", "tasks": [task]}))
    with pytest.raises(SuiteValidationError, match=message):
        load_suite(path)


def test_consolidation_procedure_steps_are_a_positive_contract():
    from runner.suite import _parse_consolidation_expectation

    expected = ["Open preferences", "Apply", "Verify"]
    parsed = _parse_consolidation_expectation({"required_procedure_steps": expected}, "path")
    assert parsed.required_procedure_steps == expected
    for invalid in ("Apply", [""], [1]):
        with pytest.raises(SuiteValidationError, match="required_procedure_steps"):
            _parse_consolidation_expectation({"required_procedure_steps": invalid}, "path")
    for empty in ({"allow_empty_memory": True}, {"max_memory_ids": 0}):
        with pytest.raises(SuiteValidationError):
            _parse_consolidation_expectation({"required_procedure_steps": expected, **empty}, "path")
