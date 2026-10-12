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


def test_notification_memory_suite_uses_benchmark_seed_setup():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "notification_memory_v1.json"
    suite = load_suite(suite_path)
    assert suite_path.read_text(encoding="utf-8").isascii()

    assert suite.name == "notification_memory_v1"
    assert [task.id for task in suite.tasks] == [
        "delivery_notification_recall",
        "notification_noise_is_filtered",
        "notification_user_fact_beats_public_info",
        "notification_batch_cursor_drain",
        "notification_explicit_update",
        "notification_explicit_reinforce",
        "notification_explicit_remove",
        "notification_explicit_promote",
    ]
    assert suite.tasks[0].setup["type"] == "seed_notification"
    assert suite.tasks[0].setup["expected_memory_scope"] == "temporary"
    assert suite.tasks[1].setup["consolidate"] is True
    assert suite.tasks[1].setup["expected_memory_count"] == 0
    assert suite.tasks[2].repeats == 5
    assert suite.tasks[2].setup["expected_memory_count"] == 1
    assert [step["type"] for step in suite.tasks[4].setup] == [
        "seed_memory",
        "seed_notification",
        "assert_memory",
    ]


def test_perception_v1_settings_rubric_uses_0_1000_normalized_coordinates():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "perception" / "perception_v1.json"
    suite = load_suite(suite_path)
    task = next(task for task in suite.tasks if task.id == "find_settings_iphone")
    check = next(item.check for item in task.rubric if item.id == "click_targets_settings")

    assert "(0-1000 normalized space" in check
    assert "normalized x in [746, 930]" in check
    assert "y in [441, 560]" in check
    assert "[0.75, 0.98]" not in check


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
    assert "静态" in prompt_prefix or "static" in prompt_prefix
    assert "找到" in prompt_prefix or "locate" in prompt_prefix
    assert "点击" in prompt_prefix or "click" in prompt_prefix
    for leaked_instruction in (
        "screenshot",
        "touch_gesture",
        "width",
        "height",
        "0-1000",
        "retry",
        "verify",
    ):
        assert leaked_instruction not in prompt_prefix


def test_perception_v1_settings_prompt_requires_clicking_visible_icon():
    suite_path = (
        Path(__file__).resolve().parents[1]
        / "suites"
        / "perception"
        / "perception_v1.json"
    )
    suite = load_suite(suite_path)

    task = next(task for task in suite.tasks if task.id == "find_settings_iphone")
    assert task.prompt == "请点击屏幕上的设置图标。"


def test_mobilegym_basic_suite_loads_device_operation_tasks():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "mobilegym_basic.json"
    suite = load_suite(suite_path)

    assert suite.name == "mobilegym_basic"
    assert {task.category for task in suite.tasks} == {"device_operation"}
    assert all(task.rubric and task.rubric[0].check for task in suite.tasks)


def test_vphone_ios_basic_warmup_requires_screenshot():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "vphone_ios_basic.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    warmup = task_by_id["warmup"]
    assert suite.prompt_prefix == ""
    assert warmup.prompt == "看一眼当前屏幕。"
    assert warmup.hard_assertions.min_tool_calls == 1
    assert warmup.hard_assertions.required_tools == ["screenshot"]


def test_adb_android_basic_suite_loads_device_operation_tasks():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "adb_android_basic.json"
    suite = load_suite(suite_path)

    assert suite.name == "adb_android_basic"
    assert {task.category for task in suite.tasks} == {"device_operation"}
    assert [task.id for task in suite.tasks] == [
        "screenshot_home",
        "go_home",
        "open_settings",
        "swipe_screen",
        "type_english_text",
        "clock_count_alarms",
        "settings_check_wifi",
        "open_app_drawer",
    ]
    # Action tasks must not require any specific tool: every action tool
    # already returns a post-action screenshot, so the agent legitimately
    # completes them without ever invoking the standalone screenshot tool,
    # and it may pick quick_action vs touch_gesture for the same outcome.
    by_id = {task.id: task for task in suite.tasks}
    assert by_id["screenshot_home"].hard_assertions.required_tools == ["screenshot"]
    assert suite.prompt_prefix == ""
    for task_id in (
        "go_home",
        "open_settings",
        "swipe_screen",
        "type_english_text",
        "clock_count_alarms",
        "settings_check_wifi",
        "open_app_drawer",
    ):
        assert by_id[task_id].hard_assertions.required_tools == []
    assert by_id["settings_check_wifi"].hard_assertions.min_tool_calls == 1
    # These ported multi-step tasks genuinely need navigation before observing.
    for task_id in ("clock_count_alarms", "open_app_drawer"):
        assert by_id[task_id].hard_assertions.min_tool_calls >= 2


def test_quick_action_suite_marks_non_android_action_tasks():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "quick_action_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    assert "quick_action" in suite.prompt_prefix
    for task_id in (
        "quick_action_switch_left",
        "quick_action_switch_right",
        "quick_action_home_screen_left",
        "quick_action_home_screen_right",
        "quick_action_control_center",
        "quick_action_control_center_dismiss",
        "quick_action_spotlight_search",
        "quick_action_spotlight_search_via_input",
        "quick_action_browser_new_tab",
        "quick_action_browser_close_tab",
        "quick_action_browser_address_and_refresh",
        "quick_action_open_editor_and_type",
        "quick_action_undo",
        "quick_action_select_all",
        "quick_action_copy",
        "quick_action_cut",
        "quick_action_paste",
    ):
        assert "android" not in task_by_id[task_id].platforms


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


def test_phone_control_navigation_tasks_have_setup_pages():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "phone_control_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    for task_id in ["tap_back", "scroll_page_down", "scroll_to_bottom"]:
        setup = task_by_id[task_id].setup
        assert setup is not None
        assert setup["type"] == "agent_prompt"
        assert setup["clear_history_after"] is True


def test_phone_control_text_editing_tasks_have_input_setup():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "phone_control_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    for task_id in ("type_long_mixed_text", "select_all_and_delete", "copy_paste_text"):
        setup = task_by_id[task_id].setup
        assert setup is not None
        assert setup["type"] == "agent_prompt"
        assert setup["clear_history_after"] is True


def test_phone_control_drag_icon_excludes_adb_android():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "phone_control_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    assert "android" not in task_by_id["drag_app_icon"].platforms


def test_phone_control_unicode_search_tasks_exclude_adb_android():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "phone_control_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    assert "android" not in task_by_id["settings_search_bluetooth"].platforms
    assert task_by_id["type_long_mixed_text"].platforms == []
    assert "Aiden test benchmark-2026!" in task_by_id["type_long_mixed_text"].prompt


def test_skill_discovery_mixed_text_task_excludes_adb_android():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "skill_discovery_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    assert "android" not in task_by_id["discover_device_operator_for_mixed_text_entry"].platforms


def test_phone_control_wifi_toggle_is_split_into_on_and_off_tasks():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "phone_control_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    assert "toggle_wifi" not in task_by_id
    off_task = task_by_id["toggle_wifi_off"]
    on_task = task_by_id["toggle_wifi_on"]

    assert off_task.setup is not None
    assert any(item.id == "wifi_off_final" for item in off_task.rubric)

    assert on_task.setup is not None
    assert any(item.id == "wifi_on_final" for item in on_task.rubric)


def test_phone_control_bluetooth_task_uses_chinese_keyword():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "phone_control_v1.json"
    suite = load_suite(suite_path)
    task = next(task for task in suite.tasks if task.id == "settings_search_bluetooth")

    assert "蓝牙" in task.prompt
    assert "Bluetooth" not in task.prompt
    assert any("蓝牙" in item.check for item in task.rubric)

def test_shell_utility_suite_replaces_removed_time_and_calculator_tools():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "shell_utility_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    assert suite.name == "shell_utility_v1"
    assert "shell" not in suite.prompt_prefix.lower()
    assert set(task_by_id) == {
        "current_time_via_shell",
        "timezone_conversion_via_shell",
        "calculation_via_shell",
    }
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
    assert {task.id for task in suite.tasks} == {
        "discover_device_operator_for_settings",
        "discover_device_operator_for_settings_search",
        "discover_device_operator_for_mixed_text_entry",
        "discover_device_operator_for_scrolling_navigation",
    }
    assert len(suite.tasks) == 4
    assert sum(task.category == "multi_step" for task in suite.tasks) == 3

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

    expected_train_ids = {
        "crossapp_work_calendar_earliest_alarm",
        "crossapp_work_meeting_route_eta_draft_wechat",
        "crossapp_life_weather_filter_non_rainy_notes",
        "crossapp_life_map_place_draft_wechat",
        "crossapp_content_spotify_current_song_notes",
        "crossapp_content_redbook_search_title_draft_wechat",
        "crossapp_commerce_ebay_lowest_price_notes",
        "crossapp_commerce_alipay_recent_transactions_notes",
    }
    expected_verification_ids = {
        "crossapp_work_weather_conditional_meeting_decision",
        "crossapp_life_railway_weather_draft_wechat",
        "crossapp_content_wechat_reading_bookshelf_draft",
        "crossapp_content_bilibili_ranking_draft_wechat",
        "crossapp_commerce_ebay_balance_diff_notes",
        "crossapp_commerce_price_budget_stop",
    }

    train_ids = {task.id for task in train.tasks}
    verification_ids = {task.id for task in verification.tasks}
    assert train_ids == expected_train_ids
    assert verification_ids == expected_verification_ids
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


def test_memory_suite_covers_representative_memory_behaviors():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "memory_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    expected_tasks = {
        "save_explicit_preference",
        "save_user_fact",
        "save_user_rule",
        "save_user_procedure",
        "save_correct_tags",
        "use_rule_to_block_action",
        "use_procedure_steps",
        "recall_saved_fact_after_setup",
        "recall_session_chunk_details",
        "update_changed_preference",
        "use_filtered_subset",
        "forget_on_request",
        "avoid_saving_ephemeral_fact",
        "no_save_for_ephemeral_chat",
        "no_recall_when_in_context",
        "multi_turn_overwrite_and_verify_last_value",
    }
    assert expected_tasks <= set(task_by_id)
    assert len(suite.tasks) >= 15
    assert all(task.category == "memory" for task in suite.tasks)

    overwrite_task = task_by_id["multi_turn_overwrite_and_verify_last_value"]
    assert isinstance(overwrite_task.setup, list)
    assert [item["type"] for item in overwrite_task.setup] == ["agent_prompt"] * 3
    assert all(item["clear_history_after"] is False for item in overwrite_task.setup)
    assert overwrite_task.hard_assertions.min_tool_calls == 3
    assert overwrite_task.hard_assertions.max_tool_calls > 4
    assert overwrite_task.hard_assertions.required_tools == ["save_memory"]
    assert overwrite_task.memory_assertions["expected_count"] == 1


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
    assert len(suite.tasks) >= 16
    assert all(task.category == "memory" for task in suite.tasks)
    assert all(task.expected_answer for task in suite.tasks)
    assert all(task.expected_recalled_memory_ids for task in suite.tasks)
    assert all(task.answer_format == "option_letter" for task in suite.tasks)


def test_episode_memory_suite_guards_against_setup_context_leakage():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "episode_memory_v1.json"
    assert suite_path.read_text(encoding="utf-8").isascii()
    suite = load_suite(suite_path)
    assert suite.status == "legacy"
    task_by_id = {task.id: task for task in suite.tasks}
    task = task_by_id["reuse_success_episode_for_planning"]

    assert task.setup["type"] == "agent_prompt"
    assert task.setup["clear_history_after"] is True
    assert "shell" not in task.setup["prompt"].lower()
    assert "current_time" not in task.setup["prompt"]
    task_text = " ".join(
        [task.prompt, task.description_for_judge, *(item.check for item in task.rubric)]
    )
    assert "recall_device_memory" not in task_text


def test_episode_reflection_suite_covers_assessment_contracts():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "episode_reflection_v1.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    assert set(task_by_id) == {
        "successful_workaround_is_extracted",
        "recorded_success_without_proof_is_not_trusted",
        "failed_episode_creates_actionable_guard",
        "transient_value_is_not_memorized",
    }
    assert task_by_id["successful_workaround_is_extracted"].consolidation_expectation.goal_result == "achieved"
    assert task_by_id["recorded_success_without_proof_is_not_trusted"].consolidation_expectation.allow_empty_memory is True
    assert task_by_id["failed_episode_creates_actionable_guard"].consolidation_expectation.goal_result == "not_achieved"
    assert task_by_id["transient_value_is_not_memorized"].consolidation_expectation.allow_empty_memory is True


def test_episode_reflection_v2_is_a_release_gate_not_a_smoke_suite():
    suite_path = Path(__file__).resolve().parents[1] / "suites" / "episode_reflection_v2.json"
    suite = load_suite(suite_path)
    task_by_id = {task.id: task for task in suite.tasks}

    assert suite.name == "episode_reflection_v2"
    assert len(suite.tasks) == 12
    assert all(task.category == "memory" for task in suite.tasks)
    assert all(not task.platforms for task in suite.tasks)
    assert {
        "verified_success_procedure_applies",
        "success_claim_without_proof_is_unknown",
        "contradictory_final_evidence_overrides_claim",
        "incomplete_episode_is_not_overclaimed",
        "failure_guard_is_recalled_and_applied",
        "transient_ip_is_not_memorized",
        "one_time_token_is_not_memorized",
        "duplicate_attempts_are_compacted",
        "version_scoped_lesson_requires_fresh_check",
        "missing_result_pair_does_not_create_procedure",
        "final_verification_beats_intermediate_success",
        "successful_lesson_has_assessment_evidence",
    } == set(task_by_id)

    for task_id in (
        "verified_success_procedure_applies",
        "failure_guard_is_recalled_and_applied",
        "version_scoped_lesson_requires_fresh_check",
    ):
        task = task_by_id[task_id]
        assert task.setup["type"] == "seed_episode"
        assert task.consolidation_expectation.min_memory_ids >= 1
        task_text = " ".join(
            [task.prompt, task.description_for_judge, *(item.check for item in task.rubric)]
        )
        assert "recall_device_memory" not in task_text
        assert "recall_device_memory" not in task.hard_assertions.required_tools
        assert task.expected_recall_from_consolidation is True
        assert task.expected_recalled_memory_tool == "recall_device_memory"

    expectation = task_by_id["success_claim_without_proof_is_unknown"].consolidation_expectation
    assert expectation.max_memory_ids == 0
    assert expectation.allow_empty_memory is True

    ip_expectation = task_by_id["transient_ip_is_not_memorized"].consolidation_expectation
    assert ip_expectation.allow_empty_memory is True
    assert ip_expectation.forbidden_memory_substrings == [
        "192.0.2.20",
        "255.255.255.0",
        "192.0.2.1",
    ]

    token_expectation = task_by_id["one_time_token_is_not_memorized"].consolidation_expectation
    assert token_expectation.allow_empty_memory is True
    assert token_expectation.forbidden_memory_substrings == ["913204"]

    assert task_by_id["incomplete_episode_is_not_overclaimed"].consolidation_expectation.goal_result == "not_achieved"
    assert task_by_id["incomplete_episode_is_not_overclaimed"].consolidation_expectation.max_memory_ids == 0

    ignored_expectation = task_by_id["missing_result_pair_does_not_create_procedure"].consolidation_expectation
    assert ignored_expectation.expected_status == "ignored"

    for task_id in (
        "contradictory_final_evidence_overrides_claim",
        "final_verification_beats_intermediate_success",
    ):
        assert task_by_id[task_id].consolidation_expectation.goal_result == "not_achieved"
        assert task_by_id[task_id].consolidation_expectation.required_assessment_evidence is True

    assert task_by_id["duplicate_attempts_are_compacted"].consolidation_expectation.max_memory_ids == 1


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

    assert [task.id for task in before.tasks] == [
        "qa_notes_title_save_procedure",
        "settings_ethernet_ipv4_from_episode",
    ]
    assert [task.id for task in after.tasks] == [
        "qa_notes_title_save_procedure",
        "settings_ethernet_ipv4_from_episode",
    ]
    assert [task.id for task in legacy.tasks] == [
        "qa_notes_title_save_procedure",
        "settings_ethernet_ipv4_from_episode",
    ]

    for suite in (before, after, legacy):
        assert len(suite.tasks) == 2
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
    assert len(suite.tasks) >= 10
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
    assert len(task_by_id["preserve_duplicate_snapshots"].expected_recalled_memory_ids) == 2

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

def test_notes_entry_policy_suite_covers_three_screen_states():
    suites_dir = Path(__file__).resolve().parents[1] / "suites" / "aiden_app"
    suite = load_suite(suites_dir / "notes_entry_policy_v1.json")
    tasks = {task.id: task for task in suite.tasks}

    assert suite.mock_environment is None
    assert set(tasks) == {
        "ios_pip_notes_already_open",
        "ios_pip_notes_icon_visible",
        "ios_pip_notes_icon_missing",
    }
    assert all(task.mock_environment is not None for task in suite.tasks)

    open_task = tasks["ios_pip_notes_already_open"]
    assert "search_launch_app" in open_task.hard_assertions.forbidden_tools
    assert "bridge_open_app" in open_task.hard_assertions.forbidden_tools
    assert "enter_text" in open_task.hard_assertions.required_tools
    assert "enter_text" not in open_task.prompt
    assert "bridge_clipboard" not in open_task.prompt
    assert "search_launch_app" not in open_task.prompt

    icon_task = tasks["ios_pip_notes_icon_visible"]
    assert "touch_gesture" in icon_task.hard_assertions.required_tools
    assert "search_launch_app" in icon_task.hard_assertions.forbidden_tools
    assert icon_task.hard_assertions.required_tool_calls[1].input_contains == {
        "type": "tap",
        "point": {"x": 180, "y": 310},
    }
    assert "point" not in icon_task.prompt
    assert "touch_gesture" not in icon_task.prompt
    assert "enter_text" not in icon_task.prompt
    assert "search_launch_app" not in icon_task.prompt

    missing_task = tasks["ios_pip_notes_icon_missing"]
    assert "search_launch_app" in missing_task.hard_assertions.required_tools
    assert "bridge_open_app" in missing_task.hard_assertions.forbidden_tools
    assert "search_launch_app" not in missing_task.prompt
    assert "enter_text" not in missing_task.prompt
    text_matcher = missing_task.hard_assertions.required_tool_calls[2].input_contains["text"]
    assert text_matcher == {"$contains": "+1 202-555-0147"}


def test_phone_bridge_data_policy_suite_covers_tools_and_routing_modes():
    suites_dir = Path(__file__).resolve().parents[1] / "suites" / "aiden_app"
    suite = load_suite(suites_dir / "phone_bridge_data_policy_v1.json")
    tasks = {task.id: task for task in suite.tasks}

    assert suite.mock_environment is None
    assert suite.prompt_prefix == ""
    assert len(tasks) == 12
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
    assert len(dynamic_island_tasks) == 4
    assert len(pip_tasks) == 4
    assert len(fgs_tasks) == 4

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


def test_aiden_app_connection_capabilities_suite_covers_real_proxy_contract():
    suites_dir = Path(__file__).resolve().parents[1] / "suites" / "aiden_app"
    suite = load_suite(suites_dir / "connection_capabilities_v1.json")
    tasks = {task.id: task for task in suite.tasks}

    assert suite.mock_environment is None
    assert set(tasks) == {
        "ios_forwarded_environment_context",
        "android_forwarded_environment_context",
        "semantic_open_browser",
        "open_https_url",
        "clipboard_round_trip_restore",
        "calendar_create_query_delete_round_trip",
        "contacts_query_update_restore_fixture",
        "send_local_notification",
    }
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
        "title": "AIDEN_BRIDGE_NOTIFICATION_V1",
        "body": "forwarded app connection verified",
        "sound": False,
        "badge": 1,
    }
